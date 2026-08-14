// Package emit holds the agento11y emission mechanics shared by the agent
// adapters: OTel provider setup, client construction, the generation
// recorder lifecycle, and tool-span helpers.
//
// The per-turn adapters (codex, copilot, cursor) use all of it. claudecode
// uses only the recorder lifecycle (Record): it keeps its own client
// construction because it resolves endpoint and auth explicitly, substituting
// local-mode placeholder credentials, rather than relying on the SDK's
// SIGIL_* env resolution.
//
// The adapters pass per-agent options such as instrumentation scope, logger,
// and content-capture mode. Content handling that is genuinely agent-specific
// (redaction, capture-mode clamping) stays at the call site; this package
// only owns the mechanics that are identical.
package emit

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/grafana/agento11y/plugins/agento11y/internal/timeutil"
)

// ExportEndpoint returns the generations export URL derived from the branded
// ENDPOINT family (trailing slash trimmed) plus the SDK's
// `/api/v1/generations:export` path.
func ExportEndpoint() string {
	return strings.TrimRight(envconfig.Getenv("ENDPOINT"), "/") + "/api/v1/generations:export"
}

// ClientOptions configures NewClient.
type ClientOptions struct {
	// InstrumentationName is the OTel scope name attached to spans/metrics.
	InstrumentationName string
	// ContentCapture is the resolved capture mode for the client.
	ContentCapture agento11y.ContentCaptureMode
	// Logger is forwarded to the SDK client. Every adapter passes its own
	// logger, which writes to the plugin log file; nil leaves the SDK on its
	// default log.Default(), which writes to the stderr a hook shares with the
	// host agent.
	Logger *log.Logger
	// Providers wires the tracer/meter when non-nil.
	Providers *otel.Providers
	// UserAgent overrides the generation-export User-Agent header when
	// non-empty. Each agent passes its own token via useragent.For; empty
	// leaves the SDK default in place.
	UserAgent string
	// Tags are client tags: merged into every generation and emitted as
	// `agento11y.tag.<key>` on spans and metrics. Adapters pass the values
	// autotag.FromEnv resolved for AGENTO11Y_AUTO_CODING_AGENT_TAGS. Keys set in
	// AGENTO11Y_TAGS must not appear here: the SDK lets caller tags win over
	// env tags, and the explicit tag is the one the user asked for.
	Tags map[string]string
}

// exportConfig builds the shared basic-auth generation export config.
// A non-empty userAgent sets the export User-Agent header (each agent passes
// its own token via useragent.For); empty leaves the SDK default in place.
func exportConfig(userAgent string, protocol agento11y.GenerationExportProtocol) agento11y.GenerationExportConfig {
	export := agento11y.GenerationExportConfig{
		Protocol: protocol,
		Endpoint: ExportEndpoint(),
		Auth:     agento11y.AuthConfig{Mode: agento11y.ExportAuthModeBasic},
	}
	if userAgent != "" {
		export.Headers = map[string]string{"User-Agent": userAgent}
	}
	return export
}

// ExportProtocol resolves the generation export protocol from the branded
// PROTOCOL family. The value `otel` is the only one that changes anything: it
// exports each generation as a GenAI span on the OTLP pipeline instead of
// calling the export endpoint.
//
// The mode needs both an OTLP pipeline and the SDK's experimental gate. With
// either one missing it would drop every generation, so ExportProtocol falls
// back to HTTP and logs which one is missing. Callers pass the result to the
// SDK explicitly instead of leaving the SDK to read the env var itself,
// because the fallback has to win over AGENTO11Y_PROTOCOL.
//
// The fallback notice goes to the plugin log file, where a user who set the
// variable looks for it. Writing the notice to stderr would put plugin output
// inside the host agent's hook protocol.
func ExportProtocol(providers *otel.Providers, logger *log.Logger) agento11y.GenerationExportProtocol {
	value, key, ok := envconfig.LookupEnv("PROTOCOL")
	if !ok || !strings.EqualFold(value, string(agento11y.GenerationExportProtocolOTel)) {
		return agento11y.GenerationExportProtocolHTTP
	}

	fallback := ""
	switch {
	case providers == nil:
		fallback = "no OTLP endpoint is configured"
	case !agento11y.ExperimentalFeaturesEnabled():
		fallback = agento11y.EnvEnableExperimentalFeatures + " is not enabled"
	}
	if fallback != "" {
		if logger != nil {
			logger.Printf("otel: %s=otel but %s; exporting generations over HTTP", key, fallback)
		}
		return agento11y.GenerationExportProtocolHTTP
	}
	return agento11y.GenerationExportProtocolOTel
}

// NewClient builds an agento11y client with the shared basic-auth generation
// export defaults, the protocol ExportProtocol resolves, and optional OTel
// providers.
func NewClient(opts ClientOptions) *agento11y.Client {
	c := agento11y.Config{
		ContentCapture:   opts.ContentCapture,
		Logger:           opts.Logger,
		GenerationExport: exportConfig(opts.UserAgent, ExportProtocol(opts.Providers, opts.Logger)),
		Tags:             opts.Tags,
	}
	ApplyProviders(&c, opts.Providers, opts.InstrumentationName)
	return agento11y.NewClient(c)
}

// ApplyProviders wires the OTel providers into an SDK config. The tracer and
// meter carry the plugin's own instrumentation scope. The providers go
// alongside them because otel-mode export flushes the tracer provider to
// confirm delivery: an adapter that deletes its on-disk fragment after Flush
// has no other delivery signal.
func ApplyProviders(cfg *agento11y.Config, providers *otel.Providers, instrumentationName string) {
	if cfg == nil || providers == nil {
		return
	}
	cfg.Tracer = providers.Tracer(instrumentationName)
	cfg.Meter = providers.Meter(instrumentationName)
	cfg.TracerProvider = providers.TracerProvider()
	cfg.MeterProvider = providers.MeterProvider()
}

// SetupOTel builds OTel providers when an OTLP endpoint is configured
// (SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT or OTEL_EXPORTER_OTLP_ENDPOINT) and
// returns nil otherwise. instanceID is forwarded as service.instance.id so
// concurrent agent sessions on the same host don't collide on cumulative
// metric series; pass the agent's session/conversation id. Setup failures are
// logged and swallowed so a hook keeps exporting generations even when
// tracing/metrics can't start.
func SetupOTel(ctx context.Context, instanceID string, logger *log.Logger) *otel.Providers {
	endpoint := otel.EndpointFromEnv()
	if endpoint == "" {
		return nil
	}
	providers, err := otel.Setup(ctx, instanceID)
	if err != nil {
		logger.Printf("otel: setup: %v", err)
		return nil
	}
	if providers != nil {
		logger.Printf("otel: endpoint=%s", endpoint)
	}
	return providers
}

// Record runs the recorder lifecycle for one mapped generation:
// StartGeneration → SetResult → (SetCallError when callErr != nil) → tool spans
// → End → Err. emitTools is invoked with the generation context so per-tool
// spans nest under the generation span; pass nil when there are no tool spans.
// The recorder's Err is wrapped and returned so callers don't silently drop SDK
// validation/enqueue errors.
func Record(
	ctx context.Context,
	client *agento11y.Client,
	start agento11y.GenerationStart,
	gen agento11y.Generation,
	callErr error,
	emitTools func(genCtx context.Context),
) error {
	genCtx, rec := client.StartGeneration(ctx, start)
	rec.SetResult(gen, nil)
	if callErr != nil {
		rec.SetCallError(callErr)
	}
	if emitTools != nil {
		emitTools(genCtx)
	}
	rec.End()
	if err := rec.Err(); err != nil {
		return fmt.Errorf("recorder: %w", err)
	}
	return nil
}

// ToolSpanWindow computes the (startedAt, completedAt) wall-clock window for a
// tool execution span. completedAt is parsed from completedAtRaw, falling back
// to genCompletedAt; startedAt equals completedAt unless a positive duration is
// known, in which case it is backdated by durationMs so the span has real width
// on the timeline.
//
// copilot needs a different window (it records a per-tool StartedAt and uses it
// as the base) and keeps its own helper.
func ToolSpanWindow(completedAtRaw string, durationMs *float64, genCompletedAt time.Time) (startedAt, completedAt time.Time) {
	completedAt = timeutil.ParseTimestamp(completedAtRaw, genCompletedAt)
	startedAt = completedAt
	if durationMs != nil && !completedAt.IsZero() {
		startedAt = completedAt.Add(-time.Duration(*durationMs) * time.Millisecond)
	}
	return startedAt, completedAt
}

var errToolReturned = errors.New("tool returned error")

// ToolError converts a tool error message into an error value, substituting a
// generic sentinel when msg is empty. copilot trims whitespace before the empty
// check and keeps its own helper.
func ToolError(msg string) error {
	if msg == "" {
		return errToolReturned
	}
	return errors.New(msg)
}
