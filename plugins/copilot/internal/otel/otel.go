package otel

import (
	"context"
	"encoding/base64"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/sigil-sdk/plugins/copilot/internal/util"
)

type Providers struct {
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
}

type exporterConfig struct {
	endpoint string
	headers  map[string]string
	insecure bool
}

func (p *Providers) Tracer(name string) trace.Tracer {
	if p == nil {
		return nil
	}
	return p.tp.Tracer(name)
}

func (p *Providers) Meter(name string) metric.Meter {
	if p == nil {
		return nil
	}
	return p.mp.Meter(name)
}

func (p *Providers) ForceFlush() error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	errc := make(chan error, 2)
	go func() { errc <- p.tp.ForceFlush(ctx) }()
	go func() { errc <- p.mp.ForceFlush(ctx) }()
	var first error
	for range 2 {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (p *Providers) Shutdown(_ context.Context) error {
	if p == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := p.mp.Shutdown(ctx); err != nil {
		_ = p.tp.Shutdown(ctx)
		return err
	}
	return p.tp.Shutdown(ctx)
}

func Setup(ctx context.Context) (*Providers, error) {
	endpoint := EndpointFromEnv()
	if endpoint == "" {
		return nil, nil
	}
	cfg := exporterConfigFromEnv(endpoint)
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		_ = os.Setenv("OTEL_SERVICE_NAME", "sigil-copilot")
	}
	setupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	traceExp, err := otlptracehttp.New(setupCtx, traceOptions(cfg)...)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)))
	metricExp, err := otlpmetrichttp.New(setupCtx, metricOptions(cfg)...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(time.Second))))
	return &Providers{tp: tp, mp: mp}, nil
}

func EndpointFromEnv() string {
	return envOr("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

func exporterConfigFromEnv(endpoint string) exporterConfig {
	headers := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	addAuthHeaderIfMissing(headers)
	return exporterConfig{
		endpoint: endpoint,
		headers:  headers,
		insecure: util.ParseBool(envOr("SIGIL_OTEL_EXPORTER_OTLP_INSECURE", os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"))),
	}
}

func traceOptions(cfg exporterConfig) []otlptracehttp.Option {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(signalEndpointURL(cfg.endpoint, "traces"))}
	if len(cfg.headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.headers))
	}
	if cfg.insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts
}

func metricOptions(cfg exporterConfig) []otlpmetrichttp.Option {
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(signalEndpointURL(cfg.endpoint, "metrics"))}
	if len(cfg.headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.headers))
	}
	if cfg.insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	return opts
}

func signalEndpointURL(endpoint, signal string) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	for _, suffix := range []string{"/v1/traces", "/v1/metrics"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base + "/v1/" + signal
}

func addAuthHeaderIfMissing(headers map[string]string) {
	tenant := strings.TrimSpace(os.Getenv("SIGIL_AUTH_TENANT_ID"))
	token := envOr("SIGIL_OTEL_AUTH_TOKEN", os.Getenv("SIGIL_AUTH_TOKEN"))
	if tenant == "" || token == "" {
		return
	}
	if hasAuthorizationHeader(headers) {
		return
	}
	headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(tenant+":"+token))
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return strings.TrimSpace(fallback)
}

func parseHeaders(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key := strings.TrimSpace(pair[:eq])
		value := strings.TrimSpace(pair[eq+1:])
		if key != "" && value != "" {
			out[key] = value
		}
	}
	return out
}

func hasAuthorizationHeader(headers map[string]string) bool {
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "Authorization") {
			return true
		}
	}
	return false
}
