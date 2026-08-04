// Package otel sets up OTLP HTTP trace + metric providers for the agento11y
// plugin binary.
//
// Configuration precedence (high to low):
//   - AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT (SIGIL_* fallback) — agento11y-specific override
//   - OTEL_EXPORTER_OTLP_ENDPOINT — standard OTel env var
//
// When OTEL_EXPORTER_OTLP_HEADERS lacks an Authorization entry, the package
// synthesizes `Authorization=Basic base64(tenant:token)` from
// AGENTO11Y_AUTH_TENANT_ID + (AGENTO11Y_OTEL_AUTH_TOKEN or
// AGENTO11Y_AUTH_TOKEN), each with the SIGIL_* spelling as fallback. Users
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
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// DefaultServiceName goes on the resource as service.name when
// OTEL_SERVICE_NAME is unset. Agents share this name so traces from any
// dispatched agent end up under a single service in the backend. Renamed from
// "sigil"; dashboards filtering on the old service name need to update.
const DefaultServiceName = "agento11y"

// Providers holds initialized OTel providers. All methods are nil-safe.
type Providers struct {
	tp *sdktrace.TracerProvider
	mp *sdkmetric.MeterProvider
}

type exporterConfig struct {
	endpoint string
	headers  map[string]string
	insecure bool
	// headersExplicit records that the caller supplied the header set, so
	// an empty set must be passed to the exporter rather than left out.
	// The SDK reads OTEL_EXPORTER_OTLP_HEADERS itself, and leaving the
	// option out lets those ambient headers through.
	headersExplicit bool
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

// Options overrides the exporter configuration Setup otherwise reads from
// the environment. A zero field keeps the environment value, so a caller
// that needs one explicit destination does not have to reproduce the rest.
//
// Headers replaces the environment set only when non-nil. A caller that must
// not leak the ambient Authorization header to its own endpoint passes an
// explicit map, which may be empty.
type Options struct {
	// Endpoint replaces the environment OTLP endpoint when non-empty. Its
	// scheme also decides the transport, so AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE
	// cannot downgrade an https endpoint named here to cleartext.
	Endpoint string
	// Headers replaces the environment-derived header set when non-nil. An
	// empty non-nil map sends no headers.
	Headers map[string]string
}

// Setup creates OTLP HTTP trace + metric providers from the environment.
// Returns nil providers (no error) when no OTLP endpoint is configured.
//
// instanceID is written to the resource as service.instance.id so concurrent
// agent sessions on the same host produce distinct OTel resource identities
// (otherwise cumulative metric series collide). Empty falls back to a UUID.
func Setup(ctx context.Context, instanceID string) (*Providers, error) {
	return SetupWithOptions(ctx, instanceID, Options{})
}

// SetupWithOptions is Setup with an explicit endpoint and header set, and it
// writes no process environment. A long-running daemon resolves its Cloud
// forwarding from the same variables, so a caller that needs to export
// somewhere else passes the destination here instead of changing them under
// the daemon.
func SetupWithOptions(ctx context.Context, instanceID string, opts Options) (*Providers, error) {
	cfg, ok := resolveExporterConfig(opts)
	if !ok {
		return nil, nil
	}
	if instanceID == "" {
		instanceID = uuid.NewString()
	}
	attrs := []attribute.KeyValue{semconv.ServiceInstanceID(instanceID)}
	if os.Getenv("OTEL_SERVICE_NAME") == "" {
		// sdkresource.Default() reads OTEL_SERVICE_NAME once per process and
		// caches the result, so the name goes on the resource directly.
		attrs = append(attrs, semconv.ServiceName(DefaultServiceName))
	}
	res, err := sdkresource.Merge(
		sdkresource.Default(),
		sdkresource.NewWithAttributes(semconv.SchemaURL, attrs...),
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
// when no OTLP endpoint is configured. Used by `agento11y doctor --probe` to send
// a lightweight request to each signal and report the HTTP status without
// standing up the full exporter pipeline.
func ProbeConfig() (metrics, traces ProbeTarget, ok bool) {
	cfg, ok := resolveExporterConfig(Options{})
	if !ok {
		return ProbeTarget{}, ProbeTarget{}, false
	}
	return ProbeTarget{URL: probeSignalURL(cfg, "metrics"), Headers: cloneHeaders(cfg.headers)},
		ProbeTarget{URL: probeSignalURL(cfg, "traces"), Headers: cloneHeaders(cfg.headers)},
		true
}

// probeSignalURL is SignalEndpointURL adjusted for the insecure setting. When
// the exporter is configured insecure, the SDK's WithInsecure() forces
// cleartext to the same host:port regardless of the endpoint scheme, so a
// probe over https would exercise a different transport than real export. Drop
// to http here to match what export actually does.
func probeSignalURL(cfg exporterConfig, signal string) string {
	u := SignalEndpointURL(cfg.endpoint, signal)
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

// resolveExporterConfig applies opts over the environment-derived
// configuration. ok is false when no endpoint is configured at all, which
// is how Setup reports "nothing to export to" without an error.
func resolveExporterConfig(opts Options) (exporterConfig, bool) {
	endpoint := firstNonBlank(opts.Endpoint, EndpointFromEnv())
	if endpoint == "" {
		return exporterConfig{}, false
	}
	cfg := exporterConfigFromEnv(endpoint)
	if opts.Headers != nil {
		cfg.headers = cloneHeaders(opts.Headers)
		cfg.headersExplicit = true
	}
	if strings.TrimSpace(opts.Endpoint) != "" {
		// The caller named the destination, so its scheme decides the
		// transport. Reading the insecure env var here would let an
		// ambient value ship an https endpoint's spans in the clear.
		cfg.insecure = !isHTTPSEndpoint(opts.Endpoint)
	}
	return cfg, true
}

func isHTTPSEndpoint(endpoint string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "https://")
}

func exporterConfigFromEnv(endpoint string) exporterConfig {
	headers := ExporterHeaders(
		os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"),
		envconfig.Getenv("AUTH_TENANT_ID"),
		envconfig.Getenv("OTEL_AUTH_TOKEN"),
		envconfig.Getenv("AUTH_TOKEN"),
	)
	return exporterConfig{
		endpoint: endpoint,
		headers:  headers,
		insecure: envconfig.ParseBool(firstNonBlank(envconfig.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"))),
	}
}

// traceOptions and metricOptions both start from WithEndpointURL, which the
// SDK applies after its own environment pass and which sets the transport from
// the URL scheme. That is why cfg.insecure alone decides cleartext: an ambient
// OTEL_EXPORTER_OTLP_INSECURE cannot reach past it.
func traceOptions(cfg exporterConfig) []otlptracehttp.Option {
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(SignalEndpointURL(cfg.endpoint, "traces"))}
	if len(cfg.headers) > 0 || cfg.headersExplicit {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.headers))
	}
	if cfg.insecure {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	return opts
}

func metricOptions(cfg exporterConfig) []otlpmetrichttp.Option {
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(SignalEndpointURL(cfg.endpoint, "metrics"))}
	if len(cfg.headers) > 0 || cfg.headersExplicit {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.headers))
	}
	if cfg.insecure {
		opts = append(opts, otlpmetrichttp.WithInsecure())
	}
	return opts
}

// SignalEndpointURL appends the OTLP signal path (/v1/traces or /v1/metrics)
// to a base endpoint, replacing any existing signal suffix. Exported so other
// packages — notably the local daemon's Cloud forwarder — build the exact URLs
// the exporter targets instead of duplicating the path logic.
func SignalEndpointURL(endpoint, signal string) string {
	base := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	for _, suffix := range []string{"/v1/traces", "/v1/metrics"} {
		base = strings.TrimSuffix(base, suffix)
	}
	return base + "/v1/" + signal
}

// BasicAuthHeaderValue returns the Authorization header value the OTLP and
// generation exporters synthesize from a tenant/token pair:
// "Basic base64(tenant:token)". ok is false when either credential is blank
// (after trimming), so callers can decide whether to attach the header.
func BasicAuthHeaderValue(tenant, token string) (value string, ok bool) {
	tenant = strings.TrimSpace(tenant)
	token = strings.TrimSpace(token)
	if tenant == "" || token == "" {
		return "", false
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(tenant+":"+token)), true
}

// ExporterHeaders builds the OTLP header set from a raw
// OTEL_EXPORTER_OTLP_HEADERS value and the tenant/token credentials: the
// explicit headers, plus Basic auth synthesized from the tenant and the first
// non-blank of otelToken (OTEL_AUTH_TOKEN) and authToken (AUTH_TOKEN). An
// explicit Authorization header always wins over the synthesized one.
//
// Exported so the local daemon's Cloud forwarder authenticates its OTLP relay
// exactly the way the real exporter does; passing the values in keeps this
// usable from a config.env snapshot rather than only the process environment.
func ExporterHeaders(rawHeaders, tenant, otelToken, authToken string) map[string]string {
	headers := parseHeaders(rawHeaders)
	if hasAuthorizationHeader(headers) {
		return headers
	}
	if value, ok := BasicAuthHeaderValue(tenant, firstNonBlank(otelToken, authToken)); ok {
		headers["Authorization"] = value
	}
	return headers
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
