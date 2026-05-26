// Package otel sets up OTLP HTTP trace + metric providers for the sigil
// plugin binary.
//
// Configuration precedence (high to low):
//   - SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT — sigil-specific override
//   - OTEL_EXPORTER_OTLP_ENDPOINT — standard OTel env var
//
// When OTEL_EXPORTER_OTLP_HEADERS lacks an Authorization entry, the package
// synthesizes `Authorization=Basic base64(tenant:token)` from
// SIGIL_AUTH_TENANT_ID + (SIGIL_OTEL_AUTH_TOKEN or SIGIL_AUTH_TOKEN). Users
// who want a different scheme can set the header themselves and the plugin
// won't touch it.
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

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
)

// DefaultServiceName is written to OTEL_SERVICE_NAME if unset before exporter
// construction. Agents share this name so traces from any dispatched agent
// land under a single service in the backend.
const DefaultServiceName = "sigil"

// Providers holds initialized OTel providers. All methods are nil-safe.
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

// ForceFlush exports pending traces and metrics concurrently.
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

// Shutdown flushes and shuts down both providers concurrently. The
// caller's context is honoured; when it has no deadline we apply a 3 s
// budget so a stuck exporter can't pin the process.
func (p *Providers) Shutdown(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
	}
	errc := make(chan error, 2)
	go func() { errc <- p.mp.Shutdown(ctx) }()
	go func() { errc <- p.tp.Shutdown(ctx) }()
	var first error
	for range 2 {
		if err := <-errc; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// Setup creates OTLP HTTP trace + metric providers.
// Returns nil providers (no error) when no OTLP endpoint is configured.
func Setup(ctx context.Context) (*Providers, error) {
	endpoint := EndpointFromEnv()
	if endpoint == "" {
		return nil, nil
	}
	cfg := exporterConfigFromEnv(endpoint)
	// otlphttp has WithInsecure() but no WithSecure(), so any of the
	// inherited insecure env vars below would let the SDK exporter
	// override cfg.insecure=false and silently ship cleartext. We're a
	// subprocess; unsetting locally has no effect on the parent env.
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_INSECURE",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE",
		"OTEL_EXPORTER_OTLP_METRICS_INSECURE",
	} {
		_ = os.Unsetenv(k)
	}
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		_ = os.Setenv("OTEL_SERVICE_NAME", DefaultServiceName)
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

// EndpointFromEnv returns the configured OTLP endpoint, preferring
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT over OTEL_EXPORTER_OTLP_ENDPOINT.
func EndpointFromEnv() string {
	return envOr("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

func exporterConfigFromEnv(endpoint string) exporterConfig {
	headers := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	addAuthHeaderIfMissing(headers)
	return exporterConfig{
		endpoint: endpoint,
		headers:  headers,
		insecure: envconfig.ParseBool(envOr("SIGIL_OTEL_EXPORTER_OTLP_INSECURE", os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"))),
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
	for pair := range strings.SplitSeq(raw, ",") {
		before, after, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(before)
		value := strings.TrimSpace(after)
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
