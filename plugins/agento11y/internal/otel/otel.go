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
	"fmt"
	"maps"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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
//
// instanceID is written to the resource as service.instance.id so concurrent
// agent sessions on the same host produce distinct OTel resource identities
// (otherwise cumulative metric series collide). Empty falls back to a UUID.
func Setup(ctx context.Context, instanceID string) (*Providers, error) {
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
	if instanceID == "" {
		instanceID = uuid.NewString()
	}
	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceInstanceID(instanceID),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("build resource: %w", err)
	}
	setupCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	traceExp, err := otlptracehttp.New(setupCtx, traceOptions(cfg)...)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExp, sdktrace.WithBatchTimeout(time.Second)),
		sdktrace.WithResource(res),
	)
	metricExp, err := otlpmetrichttp.New(setupCtx, metricOptions(cfg)...)
	if err != nil {
		_ = tp.Shutdown(ctx)
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp, sdkmetric.WithInterval(time.Second))),
		sdkmetric.WithResource(res),
	)
	return &Providers{tp: tp, mp: mp}, nil
}

// EndpointFromEnv returns the configured OTLP endpoint, preferring the
// branded AGENTO11Y_/SIGIL_ OTEL_EXPORTER_OTLP_ENDPOINT spellings over the
// standard OTEL_EXPORTER_OTLP_ENDPOINT. Blank branded values fall through.
func EndpointFromEnv() string {
	return firstNonBlank(envconfig.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
}

// ProbeTarget is one OTLP signal's resolved probe destination: the full
// signal URL plus the auth headers a real export would carry.
type ProbeTarget struct {
	URL     string
	Headers map[string]string
}

// ProbeConfig resolves the OTLP endpoint and returns the per-signal probe
// targets for metrics and traces, reusing the same endpoint resolution,
// signal-URL construction, and auth-header synthesis as Setup. ok is false
// when no OTLP endpoint is configured. Used by `sigil doctor --probe` to send
// a lightweight request to each signal and report the HTTP status without
// standing up the full exporter pipeline.
func ProbeConfig() (metrics, traces ProbeTarget, ok bool) {
	endpoint := EndpointFromEnv()
	if endpoint == "" {
		return ProbeTarget{}, ProbeTarget{}, false
	}
	cfg := exporterConfigFromEnv(endpoint)
	return ProbeTarget{URL: probeSignalURL(cfg, "metrics"), Headers: cloneHeaders(cfg.headers)},
		ProbeTarget{URL: probeSignalURL(cfg, "traces"), Headers: cloneHeaders(cfg.headers)},
		true
}

// probeSignalURL is signalEndpointURL adjusted for the insecure setting. When
// the exporter is configured insecure, the SDK's WithInsecure() forces
// cleartext to the same host:port regardless of the endpoint scheme, so a
// probe over https would exercise a different transport than real export. Drop
// to http here to match what export actually does.
func probeSignalURL(cfg exporterConfig, signal string) string {
	u := signalEndpointURL(cfg.endpoint, signal)
	if cfg.insecure {
		if rest, ok := strings.CutPrefix(u, "https://"); ok {
			return "http://" + rest
		}
	}
	return u
}

func cloneHeaders(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func exporterConfigFromEnv(endpoint string) exporterConfig {
	headers := parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	addAuthHeaderIfMissing(headers)
	return exporterConfig{
		endpoint: endpoint,
		headers:  headers,
		insecure: envconfig.ParseBool(firstNonBlank(envconfig.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"))),
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

// addAuthHeaderIfMissing synthesizes Basic auth from the branded tenant and
// token families. Token order: AGENTO11Y_OTEL_AUTH_TOKEN > SIGIL_OTEL_AUTH_TOKEN
// > AGENTO11Y_AUTH_TOKEN > SIGIL_AUTH_TOKEN. An explicit Authorization value
// in OTEL_EXPORTER_OTLP_HEADERS always wins over the synthesized header.
func addAuthHeaderIfMissing(headers map[string]string) {
	tenant := envconfig.Getenv("AUTH_TENANT_ID")
	token := firstNonBlank(envconfig.Getenv("OTEL_AUTH_TOKEN"), envconfig.Getenv("AUTH_TOKEN"))
	if tenant == "" || token == "" {
		return
	}
	if hasAuthorizationHeader(headers) {
		return
	}
	headers["Authorization"] = "Basic " + base64.StdEncoding.EncodeToString([]byte(tenant+":"+token))
}

func firstNonBlank(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
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
