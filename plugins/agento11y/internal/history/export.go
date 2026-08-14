package history

import (
	"context"
	"errors"
	"log"
	"maps"
	"strings"

	"github.com/google/uuid"
	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

// Metadata keys carried with each backfilled generation. They are the minimal
// remote markers: enough for a dashboard to recognise backfilled and
// approximate data, and nothing more. The detailed per-turn audit stays in the
// local ledger, which is content-free by construction.
const (
	MetaBackfill        = "agento11y.import.backfill"            // always true for an imported turn
	MetaApproximate     = "agento11y.import.approximate"         // any flag below is set
	MetaApproxStartedAt = "agento11y.import.approx_started_at"   // StartedAt was synthesized
	MetaApproxCompleted = "agento11y.import.approx_completed_at" // CompletedAt was synthesized
	MetaApproxUsage     = "agento11y.import.approx_usage"        // token usage was missing or estimated
	MetaMissingModel    = "agento11y.import.missing_model"       // no model name was recoverable
	MetaTruncated       = "agento11y.import.truncated"           // the sanitizer truncated a field
)

// MetaConversationTitle mirrors the conversation title into generation
// metadata, which is where the local viewer reads it from.
//
// The literal is duplicated: internal/local declares the same key for its read
// path and imports this package, so this package cannot import it back.
// TestHistoryConversationTitleKeyMatches in internal/local, which can see both,
// fails if the two ever drift.
const MetaConversationTitle = "agento11y.conversation.title"

// placeholderModelName fills Model.Name when the source recorded none, so the
// export clears SDK validation, which requires both a provider and a name. The
// agent ID fills the provider. A turn with a placeholder model therefore still
// reads as the tool it came from rather than as a generic unknown.
const placeholderModelName = "unknown"

// ForwardMarkerHeader tells the local daemon that a request came from the
// daemon's own machine and must be stored without being relayed to Grafana
// Cloud. [NewTargetExporter] sets it on both export legs of a loopback import.
// That header is why an import stays on the machine whatever the forwarding
// setting says.
//
// The literal is duplicated: internal/local declares the same header for the
// receiving side and imports this package, so this package cannot import it
// back. TestHistoryForwardMarkerHeaderMatches in internal/local, which can see
// both, fails if the two ever drift.
const ForwardMarkerHeader = "X-Agento11y-Local-Forwarded"

// otelInstancePrefix names an import in OTel resource attributes. The suffix is
// a UUID per import: service.instance.id exists to keep concurrent producers on
// one host apart, and a fixed value would collide their cumulative metric
// series.
const otelInstancePrefix = "agento11y-history-import-"

// LiveAgentName returns the product name a live adapter emits for an agent. The
// registered IDs are already those names, so an imported generation carries the
// same identity as a live run that did not rename itself. A live run started
// with AGENTO11Y_AGENT_NAME exports under that name instead, and a backfill
// cannot reconstruct it. An importer that produces a more specific name, such
// as "codex/subagent", keeps it; this is only the default.
func LiveAgentName(id AgentID) string { return string(id) }

// Exporter turns normalized historical generations into backdated generation
// exports and, through the SDK recorder's tracer and meter, backdated OTel
// traces. [RunImport] owns ledger bookkeeping, so the exporter does none.
//
// Recording and delivery are separate: [Exporter.ExportHistoricalGeneration]
// hands a turn to the SDK's export queue, and [Exporter.Flush] confirms the
// queue reached the endpoint. RunImport flushes in batches. A large import
// therefore makes one request per hundred turns rather than one per turn.
type Exporter struct {
	// Record validates and enqueues one prepared generation through the SDK
	// recorder. callErr is non-nil for a turn whose source recorded a call
	// failure, so the span and metrics carry an error type. NewExporter wires
	// it to emit.Record; tests substitute a stub.
	Record func(ctx context.Context, start agento11y.GenerationStart, gen agento11y.Generation, callErr error) error
	// Confirm blocks until every generation enqueued so far reached the
	// endpoint. nil skips the confirmation, for a test stub that records into
	// memory and has nothing to deliver.
	Confirm func(ctx context.Context) error
}

// NewExporter builds an Exporter that records through client and confirms
// delivery with client.Flush. The recorder backdates the generation span to the
// generation's StartedAt and CompletedAt, so a client with OTel providers wired
// produces backdated traces.
func NewExporter(client *agento11y.Client) *Exporter {
	return &Exporter{
		Record: func(ctx context.Context, start agento11y.GenerationStart, gen agento11y.Generation, callErr error) error {
			// emitTools is nil: tool activity lives in the generation's message
			// parts rather than in separate live tool spans.
			return emit.Record(ctx, client, start, gen, callErr, nil)
		},
		Confirm: client.Flush,
	}
}

// NewTargetExporter builds the production exporter for an import target.
//
// Endpoint, OTLP endpoint, and headers are passed explicitly rather than
// through the process environment. The local daemon resolves its own Cloud
// forwarding from those same variables, so setting them for the duration of an
// in-process import would change what the daemon forwards while the import
// runs.
func NewTargetExporter(ctx context.Context, target Target, logger *log.Logger) (*Exporter, func(context.Context) error, error) {
	endpoint := strings.TrimSpace(target.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(envconfig.Getenv("ENDPOINT"))
	}
	if endpoint == "" {
		return nil, nil, errors.New("history: no endpoint configured for import (set AGENTO11Y_ENDPOINT or use --local)")
	}
	tenantID, authToken := envconfig.LocalAuthPlaceholders(
		endpoint,
		envconfig.Getenv("AUTH_TENANT_ID"),
		envconfig.Getenv("AUTH_TOKEN"),
	)
	// The SDK panics on a basic-auth config with no password, and a Cloud
	// import with no credentials is a setup mistake rather than a bug. Say so.
	if strings.TrimSpace(tenantID) == "" || strings.TrimSpace(authToken) == "" {
		return nil, nil, errors.New("history: no credentials configured for import (run `agento11y login` or use --local)")
	}

	// The endpoint decides the loopback rules, not the caller. A configured
	// AGENTO11Y_ENDPOINT can be the local daemon even when the user asked for a
	// Cloud import, and a backfill posted there without the marker is one the
	// daemon relays to Grafana Cloud.
	localTarget := envconfig.IsLocalEndpoint(endpoint)
	headers := maps.Clone(target.Headers)
	if localTarget {
		if headers == nil {
			headers = map[string]string{}
		}
		headers[ForwardMarkerHeader] = "1"
	}

	var providers *otel.Providers
	if otlp := strings.TrimSpace(target.OTLPEndpoint); otlp != "" {
		var err error
		providers, err = otel.SetupWithOptions(ctx, otelInstancePrefix+uuid.NewString(), otel.Options{
			Endpoint: otlp,
			Headers:  otlpHeaders(headers),
		})
		if err != nil {
			return nil, nil, err
		}
	}

	contentMode := envconfig.ResolveContentMode(logger)
	if strings.TrimSpace(target.ContentMode) != "" {
		contentMode = envconfig.ResolveContentModeValue(logger, "", target.ContentMode)
	}
	if localTarget {
		// Local capture is always full: CONTENT_CAPTURE_MODE governs the copy
		// forwarded to Cloud, and an import forwards nothing.
		contentMode = agento11y.ContentCaptureModeFull
	}
	cfg := agento11y.Config{
		ContentCapture: contentMode,
		Logger:         logger,
		GenerationExport: agento11y.GenerationExportConfig{
			Protocol: targetExportProtocol(localTarget, providers, logger),
			Endpoint: strings.TrimRight(endpoint, "/") + "/api/v1/generations:export",
			Headers:  headers,
			Auth: agento11y.AuthConfig{
				Mode:          agento11y.ExportAuthModeBasic,
				TenantID:      tenantID,
				BasicPassword: authToken,
			},
		},
	}
	emit.ApplyProviders(&cfg, providers, "agento11y.history")
	client := agento11y.NewClient(cfg)
	cleanup := func(ctx context.Context) error {
		err := client.Shutdown(ctx)
		if providers != nil {
			if shutdownErr := providers.Shutdown(ctx); err == nil {
				err = shutdownErr
			}
		}
		return err
	}
	return NewExporter(client), cleanup, nil
}

// targetExportProtocol resolves the generation export protocol for an import
// target. A local target always gets HTTP: the local daemon reads generations
// from the proto ingest path and drains OTLP spans without storing them, so an
// otel-mode import would mark every turn exported and persist no turn at all.
// Any other target honours the branded PROTOCOL family.
func targetExportProtocol(localTarget bool, providers *otel.Providers, logger *log.Logger) agento11y.GenerationExportProtocol {
	if localTarget {
		return agento11y.GenerationExportProtocolHTTP
	}
	return emit.ExportProtocol(providers, logger)
}

// otlpHeaders returns the header set for the OTLP leg. A non-nil result
// replaces the environment-derived headers and so keeps a Cloud Authorization
// header out of a loopback import. A nil result keeps them, as a Cloud import
// needs.
func otlpHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	return maps.Clone(headers)
}

// ExportHistoricalGeneration prepares one turn and hands it to the export
// queue. The turn is not on the wire when this returns; [Exporter.Flush]
// confirms that.
func (e *Exporter) ExportHistoricalGeneration(ctx context.Context, gen HistoricalGeneration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	g := gen // local copy; prepare mutates g.Gen
	e.prepare(&g)
	return e.Record(ctx, generationStart(g.Gen), g.Gen, historicalCallError(g.Gen))
}

// Flush confirms that every turn handed over since the last Flush reached the
// destination. The SDK batches a hundred generations per request and retries a
// failed one with backoff, so this is where a transport failure surfaces.
func (e *Exporter) Flush(ctx context.Context) error {
	if e.Confirm == nil {
		return nil
	}
	return e.Confirm(ctx)
}

// historicalCallError returns a coarse, non-leaking error for a turn whose
// source recorded a call failure, so the recorder stamps an error type on the
// span and the metrics. The sanitized message stays in gen.CallError for the
// exported payload; the span path uses only this sentinel.
func historicalCallError(gen agento11y.Generation) error {
	if strings.TrimSpace(gen.CallError) == "" {
		return nil
	}
	return errHistoricalCall
}

var errHistoricalCall = errors.New("historical_call_error")

// prepare normalizes one historical generation for export in place: it enforces
// live-like agent identity and span shape, fills a placeholder model when the
// source had none, anchors a half-missing timestamp pair, and stamps the
// backfill and quality metadata. After prepare, g.Gen passes SDK validation and
// its span is backdated.
func (e *Exporter) prepare(g *HistoricalGeneration) {
	gen := &g.Gen
	q := &g.Quality

	if gen.AgentName == "" {
		gen.AgentName = LiveAgentName(g.Source.Agent)
	}
	if gen.Mode == "" {
		gen.Mode = agento11y.GenerationModeSync
	}
	if gen.OperationName == "" {
		gen.OperationName = "generateText"
	}
	if gen.ID == "" {
		gen.ID = g.Source.GenerationID()
	}

	// Validation requires both a model provider and a model name. A missing
	// model is filled with a placeholder and flagged.
	if gen.Model.Name == "" {
		gen.Model.Name = placeholderModelName
		q.MissingModel = true
	}
	if gen.Model.Provider == "" {
		gen.Model.Provider = string(g.Source.Agent)
	}

	// Usage with no recorded tokens means the source carried none.
	if gen.Usage == (agento11y.TokenUsage{}) {
		q.ApproxUsage = true
	}

	e.applyTimestamps(gen, q)
	stampQuality(gen, *q)
}

// applyTimestamps anchors a turn with one missing timestamp and flags the gaps
// in quality.
//
// When exactly one end is missing it is anchored to the known one, producing a
// zero-width span at that instant: leaving one end zero would let the SDK fill
// it with wall-clock now while the other stays historical, giving a span with a
// negative duration.
//
// When both ends are missing, both are left zero for the SDK to fill with now,
// which is a valid, non-backdated span.
func (e *Exporter) applyTimestamps(gen *agento11y.Generation, q *QualityReport) {
	sZero := gen.StartedAt.IsZero()
	cZero := gen.CompletedAt.IsZero()
	if !sZero && !cZero {
		return
	}

	if sZero && cZero {
		q.ApproxStartedAt = true
		q.ApproxCompletedAt = true
		return
	}

	if sZero {
		q.ApproxStartedAt = true
		gen.StartedAt = gen.CompletedAt
	} else {
		q.ApproxCompletedAt = true
		gen.CompletedAt = gen.StartedAt
	}
}

// stampQuality writes the backfill and quality markers into the generation
// metadata. The backfill marker is always set; an approximation flag is set
// only when true, which keeps the payload lean.
func stampQuality(gen *agento11y.Generation, q QualityReport) {
	if gen.Metadata == nil {
		gen.Metadata = map[string]any{}
	}
	if title := strings.TrimSpace(gen.ConversationTitle); title != "" {
		gen.Metadata[MetaConversationTitle] = title
	}
	gen.Metadata[MetaBackfill] = true

	approx := false
	set := func(key string, on bool) {
		if on {
			gen.Metadata[key] = true
			approx = true
		}
	}
	set(MetaApproxStartedAt, q.ApproxStartedAt)
	set(MetaApproxCompleted, q.ApproxCompletedAt)
	set(MetaApproxUsage, q.ApproxUsage)
	set(MetaMissingModel, q.MissingModel)
	set(MetaTruncated, q.Truncated)
	if approx {
		gen.Metadata[MetaApproximate] = true
	}
}

// generationStart projects the request-shaped fields of a normalized generation
// into the recorder seed. The recorder uses StartedAt as the span start time,
// so the seed must carry it for backdating to work.
func generationStart(g agento11y.Generation) agento11y.GenerationStart {
	return agento11y.GenerationStart{
		ID:                  g.ID,
		ConversationID:      g.ConversationID,
		ConversationTitle:   g.ConversationTitle,
		UserID:              g.UserID,
		AgentName:           g.AgentName,
		AgentVersion:        g.AgentVersion,
		Mode:                g.Mode,
		OperationName:       g.OperationName,
		Model:               g.Model,
		SystemPrompt:        g.SystemPrompt,
		Tools:               g.Tools,
		MaxTokens:           g.MaxTokens,
		Temperature:         g.Temperature,
		TopP:                g.TopP,
		ToolChoice:          g.ToolChoice,
		ThinkingEnabled:     g.ThinkingEnabled,
		ParentGenerationIDs: g.ParentGenerationIDs,
		EffectiveVersion:    g.EffectiveVersion,
		Tags:                g.Tags,
		Metadata:            g.Metadata,
		StartedAt:           g.StartedAt,
	}
}
