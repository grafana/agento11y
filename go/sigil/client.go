package sigil

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Version is the released version of the Sigil Go SDK. It is stamped into the
// default generation-export User-Agent (see UserAgent). Released via the
// go/vX.Y.Z git tag; bump this on release.
const Version = "0.8.0"

// UserAgent returns the SDK's default generation-export User-Agent product
// token, "sigil-sdk-go/<Version>". Coding-agent plugins prepend their own token
// (most-specific first), e.g.
//
//	"sigil-plugin-claude-code/" + pluginVersion + " " + sigil.UserAgent()
func UserAgent() string {
	return sdkUserAgentProduct + "/" + Version
}

// Config controls Sigil client behavior.
type Config struct {
	GenerationExport GenerationExportConfig
	API              APIConfig
	EmbeddingCapture EmbeddingCaptureConfig
	// Hooks controls synchronous hook evaluation against the Sigil API
	// (preflight/postflight guardrails). Disabled by default; callers must
	// explicitly opt in by setting Hooks.Enabled = true.
	Hooks HooksConfig
	// ContentCapture controls the default content capture mode for all
	// generations and tool executions. Per-recording overrides take precedence.
	ContentCapture ContentCaptureMode
	// ContentCaptureResolver, when set, is called before each generation,
	// embedding, tool execution, and rating submission to dynamically resolve
	// the content capture mode. It receives the request context and the
	// recording's metadata (nil when the recording type has no metadata, e.g.
	// tool executions).
	//
	// Resolution precedence (highest → lowest):
	//   1. Per-recording ContentCapture field (explicit override)
	//   2. ContentCaptureResolver return value
	//   3. Config.ContentCapture (static default)
	//
	// Returning ContentCaptureModeDefault defers to Config.ContentCapture.
	// Panics are recovered and treated as ContentCaptureModeMetadataOnly
	// (fail-closed).
	ContentCaptureResolver func(ctx context.Context, metadata map[string]any) ContentCaptureMode
	// GenerationSanitizer, when set, is invoked on each generation in
	// GenerationRecorder.End after normalization and trace-context application,
	// but before content-capture stripping and metadata stamping. The sanitizer
	// may mutate strings or payloads (e.g. redact secrets) but must preserve
	// message and part structure.
	//
	// If the sanitizer panics the SDK downgrades the generation's content
	// capture mode to ContentCaptureModeMetadataOnly so that no partially
	// redacted payload is shipped, and logs a warning via Config.Logger.
	//
	// Use NewSecretRedactionSanitizer for the built-in regex-based redactor.
	GenerationSanitizer GenerationSanitizer
	// Tracer is optional and mainly used for tests. If nil, the client uses the global OpenTelemetry tracer.
	Tracer trace.Tracer
	// Meter is optional and mainly used for tests. If nil, the client uses the global OpenTelemetry meter.
	Meter metric.Meter
	// Logger receives async export failures. Defaults to log.Default().
	Logger *log.Logger
	// Now controls clock behavior (useful for tests).
	Now func() time.Time

	// AgentName is applied to GenerationStart.AgentName / EmbeddingStart.AgentName
	// / ToolExecutionStart.AgentName when the per-call value is empty. Read from
	// SIGIL_AGENT_NAME by ConfigFromEnv. Per-call args always win.
	AgentName string
	// AgentVersion is applied to *Start.AgentVersion when the per-call value is
	// empty. Read from SIGIL_AGENT_VERSION by ConfigFromEnv.
	AgentVersion string
	// UserID is applied to GenerationStart.UserID when the per-call value is
	// empty and no user_id has been threaded through the request context.
	// Read from SIGIL_USER_ID by ConfigFromEnv.
	UserID string
	// Tags are merged into every GenerationStart.Tags (per-call tags win on
	// key conflict) and emitted on OTel spans/metrics as sigil.tag.<key>.
	// Read from SIGIL_TAGS (CSV) by ConfigFromEnv.
	Tags map[string]string
	// Debug, when set, signals downstream consumers (plugins, telemetry) that
	// the SDK is running in verbose mode. Read from SIGIL_DEBUG by ConfigFromEnv.
	// Pointer semantics distinguish "not set" (use SIGIL_DEBUG env or default)
	// from explicit false (force off even if env says otherwise). Use BoolPtr.
	// The SDK does not currently change its own logger based on this flag —
	// plugins layer their own log-file plumbing on top of it.
	Debug *bool

	// testGenerationExporter overrides transport for in-package tests.
	testGenerationExporter generationExporter
	// testDisableWorker keeps queue synchronous for in-package tests.
	testDisableWorker bool
}

type ExportAuthMode string

const (
	ExportAuthModeNone   ExportAuthMode = "none"
	ExportAuthModeTenant ExportAuthMode = "tenant"
	ExportAuthModeBearer ExportAuthMode = "bearer"
	ExportAuthModeBasic  ExportAuthMode = "basic"
)

type AuthConfig struct {
	Mode        ExportAuthMode
	TenantID    string
	BearerToken string
	// BasicUser is the username for basic auth. When empty, TenantID is used.
	BasicUser string
	// BasicPassword is the password/token for basic auth.
	BasicPassword string
}

type GenerationExportProtocol string

const (
	GenerationExportProtocolGRPC GenerationExportProtocol = "grpc"
	GenerationExportProtocolHTTP GenerationExportProtocol = "http"
	GenerationExportProtocolNone GenerationExportProtocol = "none"
)

type GenerationExportConfig struct {
	Protocol GenerationExportProtocol
	Endpoint string
	Headers  map[string]string
	Auth     AuthConfig
	// Insecure controls whether the exporter accepts cleartext (HTTP / non-TLS gRPC).
	// Pointer semantics distinguish "not set" (use SIGIL_INSECURE env or default)
	// from explicit false (force TLS even if env says otherwise). Use BoolPtr.
	Insecure *bool
	// GRPCMaxSendMessageBytes controls the gRPC per-message send cap used by
	// the SDK generation exporter.
	GRPCMaxSendMessageBytes int
	// GRPCMaxReceiveMessageBytes controls the gRPC per-message receive cap used
	// by the SDK generation exporter.
	GRPCMaxReceiveMessageBytes int
	BatchSize                  int
	FlushInterval              time.Duration
	QueueSize                  int
	MaxRetries                 int
	InitialBackoff             time.Duration
	MaxBackoff                 time.Duration
	PayloadMaxBytes            int
}

type APIConfig struct {
	Endpoint string
}

const instrumentationName = "github.com/grafana/sigil-sdk/go/sigil"
const (
	defaultGRPCMaxSendMessageBytes    = 16 << 20
	defaultGRPCMaxReceiveMessageBytes = 16 << 20
	defaultGenerationPayloadMaxBytes  = 16 << 20
	minRecordedGenerationIDs          = 1024

	sdkMetadataKeyName  = "sigil.sdk.name"
	metadataUserIDKey   = "sigil.user.id"
	sdkName             = "sdk-go"
	sdkUserAgentProduct = "sigil-sdk-go"

	spanAttrGenerationID           = "sigil.generation.id"
	spanAttrConversationID         = "gen_ai.conversation.id"
	spanAttrConversationTitle      = "sigil.conversation.title"
	spanAttrUserID                 = "user.id"
	spanAttrAgentName              = "gen_ai.agent.name"
	spanAttrAgentVersion           = "gen_ai.agent.version"
	spanAttrErrorType              = "error.type"
	spanAttrErrorCategory          = "error.category"
	spanAttrOperationName          = "gen_ai.operation.name"
	spanAttrProviderName           = "gen_ai.provider.name"
	spanAttrRequestModel           = "gen_ai.request.model"
	spanAttrRequestMaxTokens       = "gen_ai.request.max_tokens"
	spanAttrRequestTemperature     = "gen_ai.request.temperature"
	spanAttrRequestTopP            = "gen_ai.request.top_p"
	spanAttrRequestToolChoice      = "sigil.gen_ai.request.tool_choice"
	spanAttrRequestThinkingEnabled = "sigil.gen_ai.request.thinking.enabled"
	spanAttrRequestThinkingBudget  = "sigil.gen_ai.request.thinking.budget_tokens"
	spanAttrResponseID             = "gen_ai.response.id"
	spanAttrResponseModel          = "gen_ai.response.model"
	spanAttrFinishReasons          = "gen_ai.response.finish_reasons"
	spanAttrInputTokens            = "gen_ai.usage.input_tokens"
	spanAttrOutputTokens           = "gen_ai.usage.output_tokens"
	spanAttrEmbeddingInputCount    = "gen_ai.embeddings.input_count"
	spanAttrEmbeddingInputTexts    = "gen_ai.embeddings.input_texts"
	spanAttrEmbeddingDimCount      = "gen_ai.embeddings.dimension.count"
	spanAttrRequestEncodingFormats = "gen_ai.request.encoding_formats"
	spanAttrCacheReadTokens        = "gen_ai.usage.cache_read_input_tokens"
	spanAttrCacheWriteTokens       = "gen_ai.usage.cache_write_input_tokens"
	spanAttrReasoningTokens        = "gen_ai.usage.reasoning_tokens"
	spanAttrToolName               = "gen_ai.tool.name"
	spanAttrToolCallID             = "gen_ai.tool.call.id"
	spanAttrToolType               = "gen_ai.tool.type"
	spanAttrToolDescription        = "gen_ai.tool.description"
	spanAttrToolCallArguments      = "gen_ai.tool.call.arguments"
	spanAttrToolCallResult         = "gen_ai.tool.call.result"
	spanAttrTagPrefix              = "sigil.tag."

	metricOperationDuration     = "gen_ai.client.operation.duration"
	metricTokenUsage            = "gen_ai.client.token.usage"
	metricTimeToFirstToken      = "gen_ai.client.time_to_first_token"
	metricToolCallsPerOperation = "gen_ai.client.tool_calls_per_operation"
	metricAttrTokenType         = "gen_ai.token.type"
	metricTokenTypeInput        = "input"
	metricTokenTypeOutput       = "output"
	metricTokenTypeCacheRead    = "cache_read"
	metricTokenTypeCacheWrite   = "cache_write"
	metricTokenTypeReasoning    = "reasoning"
)

// durationBucketsSeconds is the OTel GenAI semantic-convention bucket advice
// applied to gen_ai.client.operation.duration and gen_ai.client.time_to_first_token.
var durationBucketsSeconds = []float64{
	0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
	2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
}

// tokenUsageBuckets is the OTel GenAI semantic-convention bucket advice
// applied to gen_ai.client.token.usage (powers of 4 from 1 to ~67M tokens).
var tokenUsageBuckets = []float64{
	1, 4, 16, 64, 256, 1024, 4096, 16384,
	65536, 262144, 1048576, 4194304, 16777216, 67108864,
}

var statusCodePattern = regexp.MustCompile(`\b([1-5][0-9][0-9])\b`)

// Keep unexported aliases for backward-compatible fmt.Errorf wrapping.
var (
	errGenerationValidation = ErrValidationFailed
	errGenerationEnqueue    = ErrEnqueueFailed
)

// BoolPtr returns a pointer to the supplied bool value. Used to distinguish
// "explicitly set to false" from "not set" on GenerationExportConfig.Insecure.
func BoolPtr(b bool) *bool { return &b }

// DefaultConfig returns a production-ready baseline configuration.
func DefaultConfig() Config {
	return Config{
		GenerationExport: GenerationExportConfig{
			Protocol:                   GenerationExportProtocolGRPC,
			Endpoint:                   "localhost:4317",
			Auth:                       AuthConfig{Mode: ExportAuthModeNone},
			Insecure:                   BoolPtr(false),
			GRPCMaxSendMessageBytes:    defaultGRPCMaxSendMessageBytes,
			GRPCMaxReceiveMessageBytes: defaultGRPCMaxReceiveMessageBytes,
			BatchSize:                  100,
			FlushInterval:              time.Second,
			QueueSize:                  2000,
			MaxRetries:                 5,
			InitialBackoff:             100 * time.Millisecond,
			MaxBackoff:                 5 * time.Second,
			PayloadMaxBytes:            defaultGenerationPayloadMaxBytes,
		},
		API: APIConfig{
			Endpoint: "http://localhost:8080",
		},
		EmbeddingCapture: EmbeddingCaptureConfig{
			CaptureInput:  false,
			MaxInputItems: 20,
			MaxTextLength: 1024,
		},
		Hooks:  defaultHooksConfig(),
		Tracer: nil,
		Logger: log.Default(),
		Now:    time.Now,
	}
}

// Client records normalized generation data and GenAI spans.
type Client struct {
	config      Config
	tracer      trace.Tracer
	meter       metric.Meter
	instruments telemetryInstruments
	exporter    generationExporter

	queue    chan queuedGeneration
	flushReq chan chan error

	queueMu      sync.RWMutex
	shutdown     bool
	workerOnce   sync.Once
	shutdownOnce sync.Once
	workerDone   chan struct{}

	generationMu      sync.RWMutex
	generationIDs     map[string]struct{}
	generationIO      map[string]string
	generationIDOrder []string
}

// GenerationRecorder records and closes one in-flight generation span.
//
// The typical usage pattern is:
//
//	ctx, rec := client.StartGeneration(ctx, start)
//	defer rec.End()
//	resp, err := provider.Call(ctx, req)
//	if err != nil { rec.SetCallError(err); return err }
//	rec.SetResult(mapper.FromRequestResponse(req, resp))
//
// For streaming calls, use StartStreamingGeneration and set the final stitched
// generation result before End.
//
// All methods are safe to call on a nil or no-op recorder.
type GenerationRecorder struct {
	client             *Client
	ctx                context.Context
	span               trace.Span
	seed               GenerationStart
	startedAt          time.Time
	contentCaptureMode ContentCaptureMode

	mu             sync.Mutex
	ended          bool
	callErr        error
	mapErr         error
	generation     Generation
	hasResult      bool
	lastGeneration Generation
	finalErr       error
	firstTokenAt   time.Time
	// extraMetadata is merged last in normalizeGeneration (e.g. cache diagnostics).
	extraMetadata map[string]any
}

// EmbeddingRecorder records and closes one in-flight embeddings span.
//
// All methods are safe to call on a nil or no-op recorder.
type EmbeddingRecorder struct {
	client             *Client
	ctx                context.Context
	span               trace.Span
	seed               EmbeddingStart
	startedAt          time.Time
	contentCaptureMode ContentCaptureMode

	mu        sync.Mutex
	ended     bool
	callErr   error
	result    EmbeddingResult
	hasResult bool
	finalErr  error
}

// ToolExecutionRecorder records and closes one in-flight execute_tool span.
//
// All methods are safe to call on a nil or no-op recorder.
type ToolExecutionRecorder struct {
	client             *Client
	ctx                context.Context
	span               trace.Span
	seed               ToolExecutionStart
	startedAt          time.Time
	includeContent     bool
	contentCaptureMode ContentCaptureMode

	mu        sync.Mutex
	ended     bool
	execErr   error
	result    ToolExecutionEnd
	hasResult bool
	finalErr  error
}

type telemetryInstruments struct {
	operationDuration     metric.Float64Histogram
	tokenUsage            metric.Int64Histogram
	timeToFirstToken      metric.Float64Histogram
	toolCallsPerOperation metric.Int64Histogram
}

// NewClient creates a Client, applying defaults for empty config values.
//
// Resolution order: explicit fields on the supplied Config win, then canonical
// SIGIL_* env vars, then DefaultConfig values. Env reading is automatic.
//
// Malformed env values are logged and skipped; other valid env values still
// apply. Use ConfigFromEnv() if strict validation is required.
func NewClient(config Config) *Client {
	cfg := config
	defaults, err := resolveFromEnv(defaultLookup, DefaultConfig())
	if err != nil {
		log.Default().Printf("sigil: skipping invalid env values: %v", err)
	}

	cfg.GenerationExport = mergeGenerationExportConfig(defaults.GenerationExport, cfg.GenerationExport)
	cfg.API = mergeAPIConfig(defaults.API, cfg.API)
	cfg.EmbeddingCapture = mergeEmbeddingCaptureConfig(defaults.EmbeddingCapture, cfg.EmbeddingCapture)
	cfg.Hooks = mergeHooksConfig(defaults.Hooks, cfg.Hooks)

	if cfg.AgentName == "" {
		cfg.AgentName = defaults.AgentName
	}
	if cfg.AgentVersion == "" {
		cfg.AgentVersion = defaults.AgentVersion
	}
	if cfg.UserID == "" {
		cfg.UserID = defaults.UserID
	}
	// Merge env-derived tags as a base layer; caller tags win on key collision.
	// mergeTags returns a fresh map, so later caller mutations don't reach the client.
	cfg.Tags = mergeTags(defaults.Tags, cfg.Tags)
	if cfg.ContentCapture == ContentCaptureModeDefault {
		cfg.ContentCapture = defaults.ContentCapture
	}
	if cfg.Debug == nil {
		cfg.Debug = defaults.Debug
	}
	if cfg.GenerationExport.Headers != nil {
		// Defensive copy to insulate the client from later mutations of the
		// caller's headers map.
		cfg.GenerationExport.Headers = cloneTags(cfg.GenerationExport.Headers)
	}

	generationHeaders, err := resolveHeadersWithAuth(cfg.GenerationExport.Headers, cfg.GenerationExport.Auth)
	if err != nil {
		// Mode-clearing in mergeAuthConfig drops cross-mode credentials before
		// reaching here, so this only fires on caller programming errors
		// (mode set without its required credential). Panic so the caller
		// notices their bug; env-induced panics no longer reach this path.
		panic(fmt.Sprintf("invalid generation auth config: %v", err))
	}
	cfg.GenerationExport.Headers = generationHeaders

	if cfg.Now == nil {
		cfg.Now = defaults.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = defaults.Logger
	}

	client := &Client{
		config:        cfg,
		flushReq:      make(chan chan error),
		workerDone:    make(chan struct{}),
		generationIDs: make(map[string]struct{}),
		generationIO:  make(map[string]string),
	}

	if cfg.Tracer != nil {
		client.tracer = cfg.Tracer
	} else {
		client.tracer = otel.Tracer(instrumentationName)
	}
	if cfg.Meter != nil {
		client.meter = cfg.Meter
	} else {
		client.meter = otel.Meter(instrumentationName)
	}
	instruments, err := newTelemetryInstruments(client.meter)
	if err != nil {
		cfg.Logger.Printf("sigil metric instrument init failed: %v", err)
	} else {
		client.instruments = instruments
	}

	exporter := cfg.testGenerationExporter
	if exporter == nil {
		var err error
		exporter, err = newGenerationExporter(cfg.GenerationExport)
		if err != nil {
			cfg.Logger.Printf("sigil generation exporter init failed: %v", err)
			exporter = newNoopGenerationExporter(err)
		}
	}
	client.exporter = exporter
	client.queue = make(chan queuedGeneration, cfg.GenerationExport.QueueSize)

	if !cfg.testDisableWorker {
		client.startWorker()
	} else {
		close(client.workerDone)
	}
	return client
}

// StartGeneration starts a non-stream GenAI span and returns a context for the provider call.
//
// Start fields are seeds: End fills zero-valued generation fields from start.
// If the client is nil a no-op recorder is returned (instrumentation never crashes business logic).
//
// Linking is two-way after End:
//   - Generation.TraceID and Generation.SpanID are set from the created span context.
//   - The span includes sigil.generation.id as an attribute.
func (c *Client) StartGeneration(ctx context.Context, start GenerationStart) (context.Context, *GenerationRecorder) {
	return c.startGeneration(ctx, start, GenerationModeSync)
}

// StartStreamingGeneration starts a streaming GenAI span and returns a context for the provider call.
//
// It applies STREAM defaults when start fields are zero-valued.
// If the client is nil a no-op recorder is returned (instrumentation never crashes business logic).
func (c *Client) StartStreamingGeneration(ctx context.Context, start GenerationStart) (context.Context, *GenerationRecorder) {
	return c.startGeneration(ctx, start, GenerationModeStream)
}

func (c *Client) startGeneration(ctx context.Context, start GenerationStart, defaultMode GenerationMode) (context.Context, *GenerationRecorder) {
	if c == nil {
		return ctx, &GenerationRecorder{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	seed := cloneGenerationStart(start)
	if seed.Mode == "" {
		seed.Mode = defaultMode
	}
	if seed.OperationName == "" {
		seed.OperationName = defaultOperationNameForMode(seed.Mode)
	}
	// Read conversation ID from context when explicit field is empty.
	if seed.ConversationID == "" {
		if id, ok := ConversationIDFromContext(ctx); ok {
			seed.ConversationID = id
		}
	}
	if seed.ConversationTitle == "" {
		if title, ok := ConversationTitleFromContext(ctx); ok {
			seed.ConversationTitle = title
		}
	}
	if seed.UserID == "" {
		if userID, ok := UserIDFromContext(ctx); ok {
			seed.UserID = userID
		}
	}
	if seed.AgentName == "" {
		if name, ok := AgentNameFromContext(ctx); ok {
			seed.AgentName = name
		}
	}
	if seed.AgentVersion == "" {
		if version, ok := AgentVersionFromContext(ctx); ok {
			seed.AgentVersion = version
		}
	}
	experimentRun, hasExperimentRun := experimentRunFromContext(ctx)
	if hasExperimentRun {
		seed = experimentRun.prepareGeneration(seed)
	} else if experimentRunID, ok := ExperimentRunIDFromContext(ctx); ok {
		seed = prepareGenerationForExperimentRunID(seed, experimentRunID)
	}
	if seed.UserID == "" && c.config.UserID != "" {
		seed.UserID = c.config.UserID
	}
	if seed.AgentName == "" && c.config.AgentName != "" {
		seed.AgentName = c.config.AgentName
	}
	if seed.AgentVersion == "" && c.config.AgentVersion != "" {
		seed.AgentVersion = c.config.AgentVersion
	}
	if len(c.config.Tags) > 0 {
		merged := cloneTags(c.config.Tags)
		maps.Copy(merged, seed.Tags)
		seed.Tags = merged
	}

	startedAt := seed.StartedAt
	if startedAt.IsZero() {
		startedAt = c.now().UTC()
	} else {
		startedAt = startedAt.UTC()
	}
	seed.StartedAt = startedAt

	// Resolve content capture mode before the span starts so MetadataOnly never
	// attaches sensitive content to live span attributes.
	resolverMode := callContentCaptureResolver(ctx, c.config.ContentCaptureResolver, seed.Metadata)
	clientMode := resolveClientContentCaptureMode(resolveContentCaptureMode(resolverMode, c.config.ContentCapture))
	ccMode := resolveContentCaptureMode(seed.ContentCapture, clientMode)

	spanGeneration := Generation{
		ID:                seed.ID,
		ConversationID:    seed.ConversationID,
		ConversationTitle: seed.ConversationTitle,
		UserID:            seed.UserID,
		AgentName:         seed.AgentName,
		AgentVersion:      seed.AgentVersion,
		Mode:              seed.Mode,
		OperationName:     seed.OperationName,
		Model:             seed.Model,
		MaxTokens:         cloneInt64Ptr(seed.MaxTokens),
		Temperature:       cloneFloat64Ptr(seed.Temperature),
		TopP:              cloneFloat64Ptr(seed.TopP),
		ToolChoice:        cloneStringPtr(seed.ToolChoice),
		ThinkingEnabled:   cloneBoolPtr(seed.ThinkingEnabled),
	}
	if ccMode == ContentCaptureModeMetadataOnly || ccMode == ContentCaptureModeFullWithMetadataSpans {
		spanGeneration.ConversationTitle = ""
	}

	callCtx, span := c.startSpan(ctx, spanGeneration, trace.SpanKindClient, startedAt)
	span.SetAttributes(generationSpanAttributes(spanGeneration)...)
	setSpanTagAttributes(span, c.config.Tags)

	callCtx = withContentCaptureMode(callCtx, ccMode)

	recorder := &GenerationRecorder{
		client:             c,
		ctx:                callCtx,
		span:               span,
		seed:               seed,
		startedAt:          startedAt,
		contentCaptureMode: ccMode,
	}
	if hasExperimentRun {
		experimentRun.captureRecorder(recorder)
	}
	return callCtx, recorder
}

// StartEmbedding starts an embeddings GenAI span and returns a context for the provider call.
//
// If the client is nil a no-op recorder is returned (instrumentation never crashes business logic).
func (c *Client) StartEmbedding(ctx context.Context, start EmbeddingStart) (context.Context, *EmbeddingRecorder) {
	if c == nil {
		return ctx, &EmbeddingRecorder{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	seed := cloneEmbeddingStart(start)
	if seed.AgentName == "" {
		if name, ok := AgentNameFromContext(ctx); ok {
			seed.AgentName = name
		}
	}
	if seed.AgentName == "" && c.config.AgentName != "" {
		seed.AgentName = c.config.AgentName
	}
	if seed.AgentVersion == "" {
		if version, ok := AgentVersionFromContext(ctx); ok {
			seed.AgentVersion = version
		}
	}
	if seed.AgentVersion == "" && c.config.AgentVersion != "" {
		seed.AgentVersion = c.config.AgentVersion
	}

	startedAt := seed.StartedAt
	if startedAt.IsZero() {
		startedAt = c.now().UTC()
	} else {
		startedAt = startedAt.UTC()
	}
	seed.StartedAt = startedAt

	resolverMode := callContentCaptureResolver(ctx, c.config.ContentCaptureResolver, seed.Metadata)
	effectiveMode := resolveClientContentCaptureMode(resolveContentCaptureMode(resolverMode, c.config.ContentCapture))

	tracer := c.tracer
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}
	callCtx, span := tracer.Start(
		ctx,
		embeddingSpanName(seed.Model.Name),
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithTimestamp(startedAt),
	)
	span.SetAttributes(embeddingSpanStartAttributes(seed)...)
	setSpanTagAttributes(span, c.config.Tags)

	return callCtx, &EmbeddingRecorder{
		client:             c,
		ctx:                callCtx,
		span:               span,
		seed:               seed,
		startedAt:          startedAt,
		contentCaptureMode: effectiveMode,
	}
}

// StartToolExecution starts an execute_tool span and returns a context for the tool call.
// If the client is nil or tool name is empty a no-op recorder is returned.
func (c *Client) StartToolExecution(ctx context.Context, start ToolExecutionStart) (context.Context, *ToolExecutionRecorder) {
	if c == nil {
		return ctx, &ToolExecutionRecorder{}
	}
	if ctx == nil {
		ctx = context.Background()
	}

	seed := start
	seed.ToolName = strings.TrimSpace(seed.ToolName)
	if seed.ToolName == "" {
		return ctx, &ToolExecutionRecorder{}
	}

	// Read conversation ID from context when explicit field is empty.
	if seed.ConversationID == "" {
		if id, ok := ConversationIDFromContext(ctx); ok {
			seed.ConversationID = id
		}
	}
	if seed.ConversationTitle == "" {
		if title, ok := ConversationTitleFromContext(ctx); ok {
			seed.ConversationTitle = title
		}
	}
	if seed.AgentName == "" {
		if name, ok := AgentNameFromContext(ctx); ok {
			seed.AgentName = name
		}
	}
	if seed.AgentName == "" && c.config.AgentName != "" {
		seed.AgentName = c.config.AgentName
	}
	if seed.AgentVersion == "" {
		if version, ok := AgentVersionFromContext(ctx); ok {
			seed.AgentVersion = version
		}
	}
	if seed.AgentVersion == "" && c.config.AgentVersion != "" {
		seed.AgentVersion = c.config.AgentVersion
	}

	startedAt := seed.StartedAt
	if startedAt.IsZero() {
		startedAt = c.now().UTC()
	} else {
		startedAt = startedAt.UTC()
	}
	seed.StartedAt = startedAt

	// Resolve content capture before the span starts so MetadataOnly never
	// attaches sensitive content to live span attributes.
	resolverMode := callContentCaptureResolver(ctx, c.config.ContentCaptureResolver, nil)
	effectiveClientDefault := resolveContentCaptureMode(resolverMode, c.config.ContentCapture)
	ctxMode, ctxSet := contentCaptureModeFromContext(ctx)
	toolMode := resolveToolContentCaptureMode(seed.ContentCapture, ctxMode, ctxSet, effectiveClientDefault)

	var includeContent bool
	switch toolMode {
	case ContentCaptureModeMetadataOnly, ContentCaptureModeFullWithMetadataSpans:
		// Tools have no separate gRPC export — FullWithMetadataSpans behaves
		// like MetadataOnly for tool spans. Drop every content-bearing seed
		// field before the span attributes pass.
		seed.ConversationTitle = ""
		seed.ToolDescription = ""
	case ContentCaptureModeFull:
		includeContent = true
	default:
		// NoToolContent / Default: honor legacy IncludeContent opt-in.
		includeContent = seed.IncludeContent
	}

	tracer := c.tracer
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}
	callCtx, span := tracer.Start(
		ctx,
		toolSpanName(seed.ToolName),
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(startedAt),
	)
	span.SetAttributes(toolSpanAttributes(seed)...)
	setSpanTagAttributes(span, c.config.Tags)

	return callCtx, &ToolExecutionRecorder{
		client:             c,
		ctx:                callCtx,
		span:               span,
		seed:               seed,
		startedAt:          startedAt,
		includeContent:     includeContent,
		contentCaptureMode: toolMode,
	}
}

// ---------------------------------------------------------------------------
// GenerationRecorder builder methods
// ---------------------------------------------------------------------------

// SetCallError records a provider/network call error.
// It is safe to call on a nil recorder.
func (r *GenerationRecorder) SetCallError(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	r.callErr = err
	r.mu.Unlock()
}

// SetFirstTokenAt records when the first streaming chunk was received.
// It is safe to call on a nil recorder.
func (r *GenerationRecorder) SetFirstTokenAt(firstTokenAt time.Time) {
	if r == nil || firstTokenAt.IsZero() {
		return
	}
	r.mu.Lock()
	r.firstTokenAt = firstTokenAt.UTC()
	r.mu.Unlock()
}

// SetResult stores the mapped generation and/or a mapping error.
// It directly accepts the (Generation, error) return of provider mappers,
// so calls like rec.SetResult(openai.FromRequestResponse(req, resp)) chain naturally.
// It is safe to call on a nil recorder.
func (r *GenerationRecorder) SetResult(g Generation, err error) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.generation = g
	r.mapErr = err
	r.hasResult = true
	r.mu.Unlock()
}

// End finalizes generation recording, sets span status, and closes the span.
//
// End takes no arguments and returns nothing, so it is safe for use with defer:
//
//	ctx, rec := client.StartGeneration(ctx, start)
//	defer rec.End()
//
// End is idempotent; subsequent calls are no-ops.
// It is safe to call on a nil or no-op recorder.
func (r *GenerationRecorder) End() {
	if r == nil {
		return
	}

	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return
	}
	r.ended = true
	callErr := r.callErr
	mapErr := r.mapErr
	generation := r.generation
	firstTokenAt := r.firstTokenAt
	extraMeta := cloneMetadata(r.extraMetadata)
	r.mu.Unlock()

	// No-op recorder: no client/span means nothing to finalize.
	if r.client == nil || r.span == nil {
		return
	}

	completedAt := r.client.now().UTC()
	normalized := r.normalizeGeneration(generation, completedAt, callErr, extraMeta)
	applyTraceContextFromSpan(r.span, &normalized)

	initialContentCaptureMode := r.contentCaptureMode
	sanitizerRan := false
	if r.client.config.GenerationSanitizer != nil && r.contentCaptureMode != ContentCaptureModeMetadataOnly {
		sanitizerRan = true
		sanitized, panicked := r.applyGenerationSanitizer(normalized)
		if panicked {
			r.contentCaptureMode = ContentCaptureModeMetadataOnly
		} else {
			normalized = sanitized
		}
	}

	// Mirror the (possibly redacted) canonical fields back into the metadata
	// keys that downstream readers and the span attribute layer consume, so a
	// successful sanitizer pass cannot leak via Metadata.
	syncCanonicalMetadataMirrors(&normalized)

	// Classify the call error category once and reuse it for stripContent
	// (under MetadataOnly) and the span error text (under MetadataOnly or
	// FullWithMetadataSpans). Under FullWithMetadataSpans the proto export
	// must keep the raw error, but the span path must not echo it.
	callErrorCategory := ""
	if callErr != nil {
		callErrorCategory = classifyErrorCategory(callErr, false)
		if callErrorCategory == "" {
			callErrorCategory = "sdk_error"
		}
	}

	stampContentCaptureMetadata(&normalized, r.contentCaptureMode)
	if r.contentCaptureMode == ContentCaptureModeMetadataOnly {
		stripContent(&normalized, callErrorCategory)
	}

	// spanCallError is the call-error text recorded on the span. Under
	// MetadataOnly stripContent already replaced normalized.CallError with the
	// category. Under FullWithMetadataSpans normalized.CallError stays raw for
	// the gRPC export, so substitute the category for the span path here.
	spanCallError := normalized.CallError
	if r.contentCaptureMode == ContentCaptureModeFullWithMetadataSpans && callErr != nil {
		spanCallError = callErrorCategory
	}
	// spanMapError redacts mapping errors on the span under
	// FullWithMetadataSpans (parity with the other SDKs which already skip
	// recording mapping errors on spans under both stripped modes). The
	// MetadataOnly path keeps the existing pre-feature behavior to stay
	// scoped to this change.
	redactMapErr := r.contentCaptureMode == ContentCaptureModeFullWithMetadataSpans

	r.span.SetName(generationSpanName(normalized))
	// FullWithMetadataSpans: proto export keeps full content (normalized stays
	// untouched), but the span path must drop content-bearing attributes. The
	// only content-bearing attribute generationSpanAttributes emits is
	// sigil.conversation.title, so a shallow copy with the title zeroed is
	// enough; no deep clone needed.
	spanView := normalized
	if r.contentCaptureMode == ContentCaptureModeFullWithMetadataSpans {
		spanView.ConversationTitle = ""
	}
	r.span.SetAttributes(generationSpanAttributes(spanView)...)
	if sanitizerRan && initialContentCaptureMode != ContentCaptureModeFullWithMetadataSpans {
		// The start span published the raw seed title; overwrite with the
		// post-sanitizer value so a sanitizer that empties or rewrites the
		// title doesn't leave the raw value on the span. Skipped under
		// FullWithMetadataSpans: the start span already omitted the title and
		// re-emitting it as "" would violate the absent guarantee.
		r.span.SetAttributes(attribute.String(spanAttrConversationTitle, spanView.ConversationTitle))
	}

	r.mu.Lock()
	r.lastGeneration = cloneGeneration(normalized)
	r.mu.Unlock()

	enqueueErr := r.client.persistGeneration(normalized)

	// Record errors on span. spanCallError is the post-sanitization,
	// post-redaction text: under MetadataOnly and FullWithMetadataSpans the
	// raw provider message is replaced with the error category so secrets,
	// prompt fragments, or response text in the provider error don't leak
	// via exception events or status.
	if callErr != nil {
		spanErr := callErr
		if spanCallError != callErr.Error() {
			spanErr = errors.New(spanCallError)
		}
		r.span.RecordError(spanErr)
	}
	if mapErr != nil {
		spanMapErr := mapErr
		if redactMapErr {
			spanMapErr = errors.New("sdk_error")
		}
		r.span.RecordError(spanMapErr)
	}
	if enqueueErr != nil {
		r.span.RecordError(enqueueErr)
	}

	errorType := generationErrorType(callErr, mapErr, enqueueErr)
	errorCategory := generationErrorCategory(callErr, mapErr, enqueueErr)
	if errorType != "" {
		r.span.SetAttributes(attribute.String(spanAttrErrorType, errorType))
		if errorCategory != "" {
			r.span.SetAttributes(attribute.String(spanAttrErrorCategory, errorCategory))
		}
	}

	switch {
	case callErr != nil:
		r.span.SetStatus(codes.Error, spanCallError)
	case mapErr != nil:
		if redactMapErr {
			r.span.SetStatus(codes.Error, "sdk_error")
		} else {
			r.span.SetStatus(codes.Error, mapErr.Error())
		}
	case enqueueErr != nil:
		r.span.SetStatus(codes.Error, enqueueErr.Error())
	default:
		r.span.SetStatus(codes.Ok, "")
	}
	r.client.recordGenerationMetrics(r.ctx, normalized, errorType, errorCategory, firstTokenAt)
	r.span.End(trace.WithTimestamp(normalized.CompletedAt))

	// rec.Err reports local validation/enqueue failures only.
	r.mu.Lock()
	r.finalErr = combineAllErrors(enqueueErr)
	r.mu.Unlock()
}

// Err returns the accumulated error after End has been called, like sql.Rows.Err().
// It is safe to call on a nil recorder.
func (r *GenerationRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finalErr
}

// applyGenerationSanitizer invokes Config.GenerationSanitizer with panic
// recovery. On panic it logs a warning and returns panicked=true so the caller
// can downgrade content capture mode.
func (r *GenerationRecorder) applyGenerationSanitizer(g Generation) (sanitized Generation, panicked bool) {
	defer func() {
		if rec := recover(); rec != nil {
			panicked = true
			if r.client.config.Logger != nil {
				r.client.config.Logger.Printf("sigil: generation sanitization failed, falling back to metadata_only: %v", rec)
			}
		}
	}()
	return r.client.config.GenerationSanitizer(g), false
}

// syncCanonicalMetadataMirrors propagates the canonical ConversationTitle and
// CallError values into the metadata keys that span attribute extraction and
// downstream readers consume. Run after the sanitizer so redactions on the
// canonical fields are reflected in the mirrors before export.
func syncCanonicalMetadataMirrors(g *Generation) {
	if g.Metadata == nil {
		return
	}
	if g.ConversationTitle != "" {
		g.Metadata[spanAttrConversationTitle] = g.ConversationTitle
	} else {
		delete(g.Metadata, spanAttrConversationTitle)
	}
	if g.CallError != "" {
		g.Metadata["call_error"] = g.CallError
	} else {
		delete(g.Metadata, "call_error")
	}
}

// SetCallError records a provider/network call error.
// It is safe to call on a nil recorder.
func (r *EmbeddingRecorder) SetCallError(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	r.callErr = err
	r.mu.Unlock()
}

// SetResult stores the mapped embedding result.
// It is safe to call on a nil recorder.
func (r *EmbeddingRecorder) SetResult(result EmbeddingResult) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.result = cloneEmbeddingResult(result)
	r.hasResult = true
	r.mu.Unlock()
}

// End finalizes embedding recording, sets span status, and closes the span.
//
// End is idempotent; subsequent calls are no-ops.
// It is safe to call on a nil or no-op recorder.
func (r *EmbeddingRecorder) End() {
	if r == nil {
		return
	}

	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return
	}
	r.ended = true
	callErr := r.callErr
	result := cloneEmbeddingResult(r.result)
	hasResult := r.hasResult
	r.mu.Unlock()

	if r.client == nil || r.span == nil {
		return
	}

	completedAt := r.client.now().UTC()
	normalized := r.normalizeEmbeddingResult(result)

	r.span.SetName(embeddingSpanName(r.seed.Model.Name))
	r.span.SetAttributes(embeddingSpanEndAttributes(normalized, hasResult, r.client.config.EmbeddingCapture, r.contentCaptureMode)...)

	var localErr error
	if err := ValidateEmbeddingStart(r.seed); err != nil {
		localErr = fmt.Errorf("%w: %v", ErrEmbeddingValidationFailed, err)
	} else if err := ValidateEmbeddingResult(normalized); err != nil {
		localErr = fmt.Errorf("%w: %v", ErrEmbeddingValidationFailed, err)
	}

	// Redact span-side error text under both stripped modes. Embeddings
	// have no proto export, so the raw provider error never escapes the
	// span path; matches the generation FullWithMetadataSpans contract.
	redactSpanErrors := r.contentCaptureMode == ContentCaptureModeMetadataOnly ||
		r.contentCaptureMode == ContentCaptureModeFullWithMetadataSpans

	if !redactSpanErrors {
		if callErr != nil {
			r.span.RecordError(callErr)
		}
		if localErr != nil {
			r.span.RecordError(localErr)
		}
	}

	errorType := ""
	errorCategory := ""
	switch {
	case callErr != nil:
		errorType = "provider_call_error"
		errorCategory = classifyErrorCategory(callErr, false)
		if errorCategory == "" {
			errorCategory = "sdk_error"
		}
		if redactSpanErrors {
			r.span.SetStatus(codes.Error, errorCategory)
		} else {
			r.span.SetStatus(codes.Error, callErr.Error())
		}
	case localErr != nil:
		errorType = "validation_error"
		errorCategory = "sdk_error"
		if redactSpanErrors {
			r.span.SetStatus(codes.Error, errorCategory)
		} else {
			r.span.SetStatus(codes.Error, localErr.Error())
		}
	default:
		r.span.SetStatus(codes.Ok, "")
	}

	if errorType != "" {
		r.span.SetAttributes(attribute.String(spanAttrErrorType, errorType))
		r.span.SetAttributes(attribute.String(spanAttrErrorCategory, errorCategory))
	}

	r.client.recordEmbeddingMetrics(r.ctx, r.seed, normalized, r.startedAt, completedAt, errorType, errorCategory)
	r.span.End(trace.WithTimestamp(completedAt))

	r.mu.Lock()
	r.finalErr = localErr
	r.mu.Unlock()
}

// Err returns the accumulated local error after End has been called.
// It is safe to call on a nil recorder.
func (r *EmbeddingRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finalErr
}

// ---------------------------------------------------------------------------
// ToolExecutionRecorder builder methods
// ---------------------------------------------------------------------------

// SetExecError records a tool execution error.
// It is safe to call on a nil recorder.
func (r *ToolExecutionRecorder) SetExecError(err error) {
	if r == nil || err == nil {
		return
	}
	r.mu.Lock()
	r.execErr = err
	r.mu.Unlock()
}

// SetResult stores the tool execution end data.
// It is safe to call on a nil recorder.
func (r *ToolExecutionRecorder) SetResult(end ToolExecutionEnd) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.result = end
	r.hasResult = true
	r.mu.Unlock()
}

// End finalizes tool execution span attributes, status, and end timestamp.
//
// End is idempotent; subsequent calls are no-ops.
// It is safe to call on a nil or no-op recorder.
func (r *ToolExecutionRecorder) End() {
	if r == nil {
		return
	}

	r.mu.Lock()
	if r.ended {
		r.mu.Unlock()
		return
	}
	r.ended = true
	execErr := r.execErr
	end := r.result
	r.mu.Unlock()

	// No-op recorder.
	if r.client == nil || r.span == nil {
		return
	}

	completedAt := end.CompletedAt
	if completedAt.IsZero() {
		completedAt = r.client.now().UTC()
	} else {
		completedAt = completedAt.UTC()
	}

	r.span.SetName(toolSpanName(r.seed.ToolName))
	r.span.SetAttributes(toolSpanAttributes(r.seed)...)

	var contentErr error
	if r.includeContent {
		arguments, err := serializeToolContent(end.Arguments)
		if err != nil {
			contentErr = fmt.Errorf("serialize tool arguments: %w", err)
		} else if arguments != "" {
			r.span.SetAttributes(attribute.String(spanAttrToolCallArguments, arguments))
		}

		result, err := serializeToolContent(end.Result)
		if err != nil && contentErr == nil {
			contentErr = fmt.Errorf("serialize tool result: %w", err)
		} else if err == nil && result != "" {
			r.span.SetAttributes(attribute.String(spanAttrToolCallResult, result))
		}
	}

	var finalErr error
	switch {
	case execErr != nil && contentErr != nil:
		finalErr = errors.Join(execErr, contentErr)
	case execErr != nil:
		finalErr = execErr
	case contentErr != nil:
		finalErr = contentErr
	}
	if finalErr != nil {
		// Tools have no proto export; under both stripped modes the span
		// must not echo raw provider exception text via record-error
		// events or the status description.
		redactSpanErrors := r.contentCaptureMode == ContentCaptureModeMetadataOnly ||
			r.contentCaptureMode == ContentCaptureModeFullWithMetadataSpans
		errorCategory := toolErrorCategory(finalErr)
		if !redactSpanErrors {
			r.span.RecordError(finalErr)
		}
		r.span.SetAttributes(attribute.String(spanAttrErrorType, "tool_execution_error"))
		r.span.SetAttributes(attribute.String(spanAttrErrorCategory, errorCategory))
		if redactSpanErrors {
			r.span.SetStatus(codes.Error, errorCategory)
		} else {
			r.span.SetStatus(codes.Error, finalErr.Error())
		}
	} else {
		r.span.SetStatus(codes.Ok, "")
	}
	r.client.recordToolExecutionMetrics(r.ctx, r.seed, r.startedAt, completedAt, finalErr)
	r.span.End(trace.WithTimestamp(completedAt))

	r.mu.Lock()
	r.finalErr = finalErr
	r.mu.Unlock()
}

// Err returns the accumulated error after End has been called, like sql.Rows.Err().
// It is safe to call on a nil recorder.
func (r *ToolExecutionRecorder) Err() error {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.finalErr
}

// ---------------------------------------------------------------------------
// internal helpers
// ---------------------------------------------------------------------------

func (r *GenerationRecorder) normalizeGeneration(raw Generation, completedAt time.Time, callErr error, extraMetadata map[string]any) Generation {
	g := cloneGeneration(raw)

	if g.ID == "" {
		g.ID = r.seed.ID
	}
	if g.ID == "" {
		g.ID = newRandomID("gen")
	}
	if g.ConversationID == "" {
		g.ConversationID = r.seed.ConversationID
	}
	if g.ConversationTitle == "" {
		g.ConversationTitle = r.seed.ConversationTitle
	}
	if g.UserID == "" {
		g.UserID = r.seed.UserID
	}
	if g.AgentName == "" {
		g.AgentName = r.seed.AgentName
	}
	if g.AgentVersion == "" {
		g.AgentVersion = r.seed.AgentVersion
	}
	if g.Mode == "" {
		g.Mode = r.seed.Mode
	}
	if g.Mode == "" {
		g.Mode = GenerationModeSync
	}
	if g.OperationName == "" {
		g.OperationName = r.seed.OperationName
	}
	if g.OperationName == "" {
		g.OperationName = defaultOperationNameForMode(g.Mode)
	}
	if g.Model.Provider == "" {
		g.Model.Provider = r.seed.Model.Provider
	}
	if g.Model.Name == "" {
		g.Model.Name = r.seed.Model.Name
	}
	if g.SystemPrompt == "" {
		g.SystemPrompt = r.seed.SystemPrompt
	}
	if len(g.Tools) == 0 {
		g.Tools = cloneTools(r.seed.Tools)
	}
	if g.MaxTokens == nil {
		g.MaxTokens = cloneInt64Ptr(r.seed.MaxTokens)
	}
	if g.Temperature == nil {
		g.Temperature = cloneFloat64Ptr(r.seed.Temperature)
	}
	if g.TopP == nil {
		g.TopP = cloneFloat64Ptr(r.seed.TopP)
	}
	if g.ToolChoice == nil {
		g.ToolChoice = cloneStringPtr(r.seed.ToolChoice)
	}
	if g.ThinkingEnabled == nil {
		g.ThinkingEnabled = cloneBoolPtr(r.seed.ThinkingEnabled)
	}
	if len(g.ParentGenerationIDs) == 0 {
		g.ParentGenerationIDs = cloneStringSlice(r.seed.ParentGenerationIDs)
	}
	if strings.TrimSpace(g.EffectiveVersion) == "" {
		g.EffectiveVersion = r.seed.EffectiveVersion
	}
	g.Tags = mergeTags(r.seed.Tags, g.Tags)
	g.Metadata = mergeMetadata(r.seed.Metadata, g.Metadata)
	g.Metadata = mergeMetadata(g.Metadata, extraMetadata)
	if g.Metadata == nil {
		g.Metadata = map[string]any{}
	}
	conversationTitle := strings.TrimSpace(g.ConversationTitle)
	if conversationTitle == "" {
		conversationTitle = metadataString(g.Metadata, spanAttrConversationTitle)
	}
	g.ConversationTitle = conversationTitle
	if conversationTitle != "" {
		g.Metadata[spanAttrConversationTitle] = conversationTitle
	}
	if g.UserID == "" {
		g.UserID = metadataString(g.Metadata, metadataUserIDKey)
	}
	if g.UserID == "" {
		// Backward-compatibility with older builds that mirrored user id under the span key.
		g.UserID = metadataString(g.Metadata, spanAttrUserID)
	}
	if userID := strings.TrimSpace(g.UserID); userID != "" {
		g.UserID = userID
		g.Metadata[metadataUserIDKey] = userID
	}
	g.Metadata[sdkMetadataKeyName] = sdkName

	if g.StartedAt.IsZero() {
		g.StartedAt = r.startedAt
	} else {
		g.StartedAt = g.StartedAt.UTC()
	}
	if g.CompletedAt.IsZero() {
		g.CompletedAt = completedAt
	} else {
		g.CompletedAt = g.CompletedAt.UTC()
	}

	if callErr != nil {
		if g.CallError == "" {
			g.CallError = callErr.Error()
		}
		if g.Metadata == nil {
			g.Metadata = map[string]any{}
		}
		g.Metadata["call_error"] = callErr.Error()
	}

	g.Usage = g.Usage.Normalize()
	return g
}

func (r *EmbeddingRecorder) normalizeEmbeddingResult(raw EmbeddingResult) EmbeddingResult {
	return cloneEmbeddingResult(raw)
}

func combineAllErrors(errs ...error) error {
	filtered := make([]error, 0, len(errs))
	for i := range errs {
		if errs[i] != nil {
			filtered = append(filtered, errs[i])
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return errors.Join(filtered...)
}

func (c *Client) persistGeneration(generation Generation) error {
	if err := ValidateGeneration(generation); err != nil {
		return fmt.Errorf("%w: %v", errGenerationValidation, err)
	}
	if err := c.enqueueGeneration(generation); err != nil {
		return fmt.Errorf("%w: %w", errGenerationEnqueue, err)
	}
	c.recordGeneration(generation)
	return nil
}

func (c *Client) recordGeneration(generation Generation) {
	if c == nil || generation.ID == "" {
		return
	}
	c.generationMu.Lock()
	defer c.generationMu.Unlock()
	if c.generationIDs == nil {
		c.generationIDs = make(map[string]struct{})
	}
	if c.generationIO == nil {
		c.generationIO = make(map[string]string)
	}
	c.generationIO[generation.ID] = generationIOFingerprint(generation)
	if _, ok := c.generationIDs[generation.ID]; ok {
		return
	}
	c.generationIDs[generation.ID] = struct{}{}
	c.generationIDOrder = append(c.generationIDOrder, generation.ID)
	for len(c.generationIDOrder) > c.recordedGenerationLimit() {
		oldest := c.generationIDOrder[0]
		c.generationIDOrder[0] = ""
		c.generationIDOrder = c.generationIDOrder[1:]
		delete(c.generationIDs, oldest)
		delete(c.generationIO, oldest)
	}
}

func (c *Client) hasRecordedGenerationID(generationID string) bool {
	if c == nil || generationID == "" {
		return false
	}
	c.generationMu.RLock()
	defer c.generationMu.RUnlock()
	_, ok := c.generationIDs[generationID]
	return ok
}

func (c *Client) hasRecordedGenerationIO(generationID string, fingerprint string) bool {
	if c == nil || generationID == "" || fingerprint == "" {
		return false
	}
	c.generationMu.RLock()
	defer c.generationMu.RUnlock()
	return c.generationIO[generationID] == fingerprint
}

func (c *Client) recordedGenerationLimit() int {
	if c == nil {
		return minRecordedGenerationIDs
	}
	queueSize := c.config.GenerationExport.QueueSize
	batchSize := c.config.GenerationExport.BatchSize
	return max(minRecordedGenerationIDs, queueSize*2, queueSize+batchSize)
}

func (c *Client) now() time.Time {
	return c.config.Now()
}

func (c *Client) startSpan(ctx context.Context, generation Generation, kind trace.SpanKind, startedAt time.Time) (context.Context, trace.Span) {
	if ctx == nil {
		ctx = context.Background()
	}
	if kind == 0 {
		kind = trace.SpanKindClient
	}

	opts := []trace.SpanStartOption{
		trace.WithSpanKind(kind),
		trace.WithTimestamp(startedAt),
	}

	tracer := c.tracer
	if tracer == nil {
		tracer = otel.Tracer(instrumentationName)
	}

	return tracer.Start(ctx, generationSpanName(generation), opts...)
}

func generationSpanName(g Generation) string {
	operation := strings.TrimSpace(g.OperationName)
	if operation == "" {
		operation = defaultOperationNameForMode(g.Mode)
	}

	model := strings.TrimSpace(g.Model.Name)
	if model == "" {
		return operation
	}
	return operation + " " + model
}

func embeddingSpanName(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return defaultEmbeddingOperationName
	}
	return defaultEmbeddingOperationName + " " + model
}

func embeddingSpanStartAttributes(start EmbeddingStart) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(spanAttrOperationName, defaultEmbeddingOperationName),
		attribute.String(sdkMetadataKeyName, sdkName),
	}
	if provider := strings.TrimSpace(start.Model.Provider); provider != "" {
		attrs = append(attrs, attribute.String(spanAttrProviderName, provider))
	}
	if model := strings.TrimSpace(start.Model.Name); model != "" {
		attrs = append(attrs, attribute.String(spanAttrRequestModel, model))
	}
	if agentName := strings.TrimSpace(start.AgentName); agentName != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentName, agentName))
	}
	if agentVersion := strings.TrimSpace(start.AgentVersion); agentVersion != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentVersion, agentVersion))
	}
	if start.Dimensions != nil {
		attrs = append(attrs, attribute.Int64(spanAttrEmbeddingDimCount, *start.Dimensions))
	}
	if encodingFormat := strings.TrimSpace(start.EncodingFormat); encodingFormat != "" {
		attrs = append(attrs, attribute.StringSlice(spanAttrRequestEncodingFormats, []string{encodingFormat}))
	}
	return attrs
}

func embeddingSpanEndAttributes(result EmbeddingResult, hasResult bool, captureCfg EmbeddingCaptureConfig, contentCapture ContentCaptureMode) []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 8)
	if hasResult {
		attrs = append(attrs, attribute.Int64(spanAttrEmbeddingInputCount, int64(result.InputCount)))
	}
	if result.InputTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrInputTokens, result.InputTokens))
	}
	if responseModel := strings.TrimSpace(result.ResponseModel); responseModel != "" {
		attrs = append(attrs, attribute.String(spanAttrResponseModel, responseModel))
	}
	if result.Dimensions != nil {
		attrs = append(attrs, attribute.Int64(spanAttrEmbeddingDimCount, *result.Dimensions))
	}
	// See ContentCaptureModeFullWithMetadataSpans godoc for the embedding
	// gating rationale.
	if captureCfg.CaptureInput && contentCapture != ContentCaptureModeMetadataOnly && contentCapture != ContentCaptureModeFullWithMetadataSpans {
		if texts := captureEmbeddingInputTexts(result.InputTexts, captureCfg); len(texts) > 0 {
			attrs = append(attrs, attribute.StringSlice(spanAttrEmbeddingInputTexts, texts))
		}
	}
	return attrs
}

// Do not add prompt, message, tool, artifact, or raw metadata content here
// without updating the FullWithMetadataSpans span view.
func generationSpanAttributes(g Generation) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(spanAttrOperationName, operationName(g)),
		attribute.String(sdkMetadataKeyName, sdkName),
	}
	if g.ID != "" {
		attrs = append(attrs, attribute.String(spanAttrGenerationID, g.ID))
	}
	if conversationID := strings.TrimSpace(g.ConversationID); conversationID != "" {
		attrs = append(attrs, attribute.String(spanAttrConversationID, conversationID))
	}
	if conversationTitle := strings.TrimSpace(g.ConversationTitle); conversationTitle != "" {
		attrs = append(attrs, attribute.String(spanAttrConversationTitle, conversationTitle))
	}
	if userID := strings.TrimSpace(g.UserID); userID != "" {
		attrs = append(attrs, attribute.String(spanAttrUserID, userID))
	}
	if agentName := strings.TrimSpace(g.AgentName); agentName != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentName, agentName))
	}
	if agentVersion := strings.TrimSpace(g.AgentVersion); agentVersion != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentVersion, agentVersion))
	}
	if provider := strings.TrimSpace(g.Model.Provider); provider != "" {
		attrs = append(attrs, attribute.String(spanAttrProviderName, provider))
	}
	if model := strings.TrimSpace(g.Model.Name); model != "" {
		attrs = append(attrs, attribute.String(spanAttrRequestModel, model))
	}
	if responseID := strings.TrimSpace(g.ResponseID); responseID != "" {
		attrs = append(attrs, attribute.String(spanAttrResponseID, responseID))
	}
	if responseModel := strings.TrimSpace(g.ResponseModel); responseModel != "" {
		attrs = append(attrs, attribute.String(spanAttrResponseModel, responseModel))
	}
	if g.MaxTokens != nil {
		attrs = append(attrs, attribute.Int64(spanAttrRequestMaxTokens, *g.MaxTokens))
	}
	if g.Temperature != nil {
		attrs = append(attrs, attribute.Float64(spanAttrRequestTemperature, *g.Temperature))
	}
	if g.TopP != nil {
		attrs = append(attrs, attribute.Float64(spanAttrRequestTopP, *g.TopP))
	}
	if g.ToolChoice != nil {
		if toolChoice := strings.TrimSpace(*g.ToolChoice); toolChoice != "" {
			attrs = append(attrs, attribute.String(spanAttrRequestToolChoice, toolChoice))
		}
	}
	if g.ThinkingEnabled != nil {
		attrs = append(attrs, attribute.Bool(spanAttrRequestThinkingEnabled, *g.ThinkingEnabled))
	}
	if thinkingBudget, ok := thinkingBudgetFromMetadata(g.Metadata); ok {
		attrs = append(attrs, attribute.Int64(spanAttrRequestThinkingBudget, thinkingBudget))
	}
	if g.StopReason != "" {
		attrs = append(attrs,
			attribute.StringSlice(spanAttrFinishReasons, []string{g.StopReason}),
		)
	}
	if g.Usage.InputTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrInputTokens, g.Usage.InputTokens))
	}
	if g.Usage.OutputTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrOutputTokens, g.Usage.OutputTokens))
	}
	if g.Usage.CacheReadInputTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrCacheReadTokens, g.Usage.CacheReadInputTokens))
	}
	if g.Usage.CacheWriteInputTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrCacheWriteTokens, g.Usage.CacheWriteInputTokens))
	}
	if g.Usage.ReasoningTokens != 0 {
		attrs = append(attrs, attribute.Int64(spanAttrReasoningTokens, g.Usage.ReasoningTokens))
	}

	return attrs
}

func operationName(g Generation) string {
	operation := strings.TrimSpace(g.OperationName)
	if operation == "" {
		return defaultOperationNameForMode(g.Mode)
	}

	return operation
}

func thinkingBudgetFromMetadata(metadata map[string]any) (int64, bool) {
	if len(metadata) == 0 {
		return 0, false
	}

	value, ok := metadata[spanAttrRequestThinkingBudget]
	if !ok || value == nil {
		return 0, false
	}

	coerced, ok := coerceInt64(value)
	if !ok {
		return 0, false
	}

	return coerced, true
}

func metadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok {
		return ""
	}
	asString, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(asString)
}

func coerceInt64(value any) (int64, bool) {
	switch typed := value.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		if typed > ^uint64(0)>>1 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		asInt := int64(typed)
		if float32(asInt) != typed {
			return 0, false
		}
		return asInt, true
	case float64:
		asInt := int64(typed)
		if float64(asInt) != typed {
			return 0, false
		}
		return asInt, true
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func generationErrorType(callErr, mapErr, enqueueErr error) string {
	switch {
	case callErr != nil:
		return "provider_call_error"
	case mapErr != nil:
		return "mapping_error"
	case errors.Is(enqueueErr, errGenerationValidation):
		return "validation_error"
	case errors.Is(enqueueErr, errGenerationEnqueue), errors.Is(enqueueErr, ErrQueueFull):
		return "enqueue_error"
	case enqueueErr != nil:
		return "enqueue_error"
	default:
		return ""
	}
}

func generationErrorCategory(callErr, mapErr, enqueueErr error) string {
	switch {
	case callErr != nil:
		return classifyErrorCategory(callErr, false)
	case mapErr != nil:
		return "sdk_error"
	case enqueueErr != nil:
		return "sdk_error"
	default:
		return ""
	}
}

func toolErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	return classifyErrorCategory(err, true)
}

func classifyErrorCategory(err error, fallbackSDK bool) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "timeout"
	}
	statusCode := httpStatusCodeFromError(err)
	switch {
	case statusCode == 429:
		return "rate_limit"
	case statusCode == 401 || statusCode == 403:
		return "auth_error"
	case statusCode == 408:
		return "timeout"
	case statusCode >= 500 && statusCode <= 599:
		return "server_error"
	case statusCode >= 400 && statusCode <= 499:
		return "client_error"
	case fallbackSDK:
		return "sdk_error"
	default:
		return "sdk_error"
	}
}

func httpStatusCodeFromError(err error) int {
	if err == nil {
		return 0
	}
	type statusCoder interface {
		StatusCode() int
	}
	var byMethod statusCoder
	if errors.As(err, &byMethod) {
		code := byMethod.StatusCode()
		if code >= 100 && code <= 599 {
			return code
		}
	}

	value := reflectIntField(err, "StatusCode")
	if value >= 100 && value <= 599 {
		return value
	}
	value = reflectIntField(err, "Status")
	if value >= 100 && value <= 599 {
		return value
	}

	matches := statusCodePattern.FindAllString(err.Error(), -1)
	for i := range matches {
		parsed, parseErr := strconv.Atoi(matches[i])
		if parseErr == nil && parsed >= 100 && parsed <= 599 {
			return parsed
		}
	}
	return 0
}

func reflectIntField(err error, field string) int {
	value := reflect.ValueOf(err)
	if !value.IsValid() {
		return 0
	}
	for value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return 0
	}
	member := value.FieldByName(field)
	if !member.IsValid() {
		return 0
	}
	switch member.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(member.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return int(member.Uint())
	default:
		return 0
	}
}

func newTelemetryInstruments(meter metric.Meter) (telemetryInstruments, error) {
	out := telemetryInstruments{}
	var err error
	out.operationDuration, err = meter.Float64Histogram(
		metricOperationDuration,
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
	if err != nil {
		return telemetryInstruments{}, err
	}
	out.tokenUsage, err = meter.Int64Histogram(
		metricTokenUsage,
		metric.WithUnit("token"),
		metric.WithExplicitBucketBoundaries(tokenUsageBuckets...),
	)
	if err != nil {
		return telemetryInstruments{}, err
	}
	out.timeToFirstToken, err = meter.Float64Histogram(
		metricTimeToFirstToken,
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
	if err != nil {
		return telemetryInstruments{}, err
	}
	out.toolCallsPerOperation, err = meter.Int64Histogram(metricToolCallsPerOperation, metric.WithUnit("count"))
	if err != nil {
		return telemetryInstruments{}, err
	}
	return out, nil
}

func (c *Client) recordGenerationMetrics(ctx context.Context, generation Generation, errorType string, errorCategory string, firstTokenAt time.Time) {
	if c == nil {
		return
	}
	if c.instruments.operationDuration == nil || c.instruments.tokenUsage == nil ||
		c.instruments.timeToFirstToken == nil || c.instruments.toolCallsPerOperation == nil {
		return
	}
	if generation.CompletedAt.IsZero() || generation.StartedAt.IsZero() {
		return
	}
	duration := generation.CompletedAt.Sub(generation.StartedAt).Seconds()
	if duration < 0 {
		duration = 0
	}

	identityAttrs := metricIdentityAttributes(
		generation.Model.Provider,
		generation.Model.Name,
		generation.AgentName,
		generation.AgentVersion,
	)
	tagAttrs := c.clientTagMetricAttributes()
	durationAttrs := append(
		[]attribute.KeyValue{attribute.String(spanAttrOperationName, operationName(generation))},
		identityAttrs...,
	)
	durationAttrs = append(durationAttrs, tagAttrs...)
	durationAttrs = append(durationAttrs,
		attribute.String(spanAttrErrorType, errorType),
		attribute.String(spanAttrErrorCategory, errorCategory),
	)
	c.instruments.operationDuration.Record(ctx, duration, metric.WithAttributes(durationAttrs...))

	recordToken := func(tokenType string, value int64) {
		if value == 0 {
			return
		}
		tokenAttrs := append(
			[]attribute.KeyValue{attribute.String(spanAttrOperationName, operationName(generation))},
			identityAttrs...,
		)
		tokenAttrs = append(tokenAttrs, tagAttrs...)
		tokenAttrs = append(tokenAttrs, attribute.String(metricAttrTokenType, tokenType))
		c.instruments.tokenUsage.Record(ctx, value, metric.WithAttributes(tokenAttrs...))
	}

	recordToken(metricTokenTypeInput, generation.Usage.InputTokens)
	recordToken(metricTokenTypeOutput, generation.Usage.OutputTokens)
	recordToken(metricTokenTypeCacheRead, generation.Usage.CacheReadInputTokens)
	recordToken(metricTokenTypeCacheWrite, generation.Usage.CacheWriteInputTokens)
	recordToken(metricTokenTypeReasoning, generation.Usage.ReasoningTokens)

	toolCalls := countToolCalls(generation.Output)
	toolCallAttrs := append(append([]attribute.KeyValue(nil), identityAttrs...), tagAttrs...)
	c.instruments.toolCallsPerOperation.Record(
		ctx,
		int64(toolCalls),
		metric.WithAttributes(toolCallAttrs...),
	)

	if operationName(generation) == defaultOperationNameStream && !firstTokenAt.IsZero() {
		ttft := firstTokenAt.Sub(generation.StartedAt).Seconds()
		if ttft >= 0 {
			ttftAttrs := append(append([]attribute.KeyValue(nil), identityAttrs...), tagAttrs...)
			c.instruments.timeToFirstToken.Record(
				ctx,
				ttft,
				metric.WithAttributes(ttftAttrs...),
			)
		}
	}
}

func (c *Client) recordEmbeddingMetrics(
	ctx context.Context,
	seed EmbeddingStart,
	result EmbeddingResult,
	startedAt time.Time,
	completedAt time.Time,
	errorType string,
	errorCategory string,
) {
	if c == nil {
		return
	}
	if c.instruments.operationDuration == nil || c.instruments.tokenUsage == nil {
		return
	}
	if startedAt.IsZero() || completedAt.IsZero() {
		return
	}

	duration := completedAt.Sub(startedAt).Seconds()
	if duration < 0 {
		duration = 0
	}

	provider := strings.TrimSpace(seed.Model.Provider)
	model := strings.TrimSpace(seed.Model.Name)
	agentName := strings.TrimSpace(seed.AgentName)
	identityAttrs := metricIdentityAttributes(provider, model, agentName, seed.AgentVersion)
	tagAttrs := c.clientTagMetricAttributes()
	durationAttrs := append(
		[]attribute.KeyValue{attribute.String(spanAttrOperationName, defaultEmbeddingOperationName)},
		identityAttrs...,
	)
	durationAttrs = append(durationAttrs, tagAttrs...)
	durationAttrs = append(durationAttrs,
		attribute.String(spanAttrErrorType, errorType),
		attribute.String(spanAttrErrorCategory, errorCategory),
	)
	c.instruments.operationDuration.Record(
		ctx,
		duration,
		metric.WithAttributes(durationAttrs...),
	)

	if result.InputTokens != 0 {
		tokenAttrs := append(
			[]attribute.KeyValue{attribute.String(spanAttrOperationName, defaultEmbeddingOperationName)},
			identityAttrs...,
		)
		tokenAttrs = append(tokenAttrs, tagAttrs...)
		tokenAttrs = append(tokenAttrs, attribute.String(metricAttrTokenType, metricTokenTypeInput))
		c.instruments.tokenUsage.Record(
			ctx,
			result.InputTokens,
			metric.WithAttributes(tokenAttrs...),
		)
	}
}

func (c *Client) recordToolExecutionMetrics(ctx context.Context, seed ToolExecutionStart, startedAt time.Time, completedAt time.Time, finalErr error) {
	if c == nil {
		return
	}
	if c.instruments.operationDuration == nil {
		return
	}
	if startedAt.IsZero() || completedAt.IsZero() {
		return
	}
	duration := completedAt.Sub(startedAt).Seconds()
	if duration < 0 {
		duration = 0
	}
	errorType := ""
	errorCategory := ""
	if finalErr != nil {
		errorType = "tool_execution_error"
		errorCategory = toolErrorCategory(finalErr)
	}
	attrs := metricIdentityAttributes(seed.RequestProvider, seed.RequestModel, seed.AgentName, seed.AgentVersion)
	attrs = append([]attribute.KeyValue{
		attribute.String(spanAttrOperationName, "execute_tool"),
		attribute.String(spanAttrToolName, strings.TrimSpace(seed.ToolName)),
	}, attrs...)
	attrs = append(attrs, c.clientTagMetricAttributes()...)
	attrs = append(attrs,
		attribute.String(spanAttrErrorType, errorType),
		attribute.String(spanAttrErrorCategory, errorCategory),
	)
	c.instruments.operationDuration.Record(
		ctx,
		duration,
		metric.WithAttributes(attrs...),
	)
}

func tagAttributes(tags map[string]string) []attribute.KeyValue {
	if len(tags) == 0 {
		return nil
	}
	type tagPair struct {
		key   string
		value string
	}
	pairs := make([]tagPair, 0, len(tags))
	for k, v := range tags {
		key := strings.TrimSpace(k)
		if key == "" {
			continue
		}
		pairs = append(pairs, tagPair{key: key, value: strings.TrimSpace(v)})
	}
	if len(pairs) == 0 {
		return nil
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].key < pairs[j].key })
	attrs := make([]attribute.KeyValue, 0, len(pairs))
	for _, p := range pairs {
		attrs = append(attrs, attribute.String(spanAttrTagPrefix+p.key, p.value))
	}
	return attrs
}

func setSpanTagAttributes(span trace.Span, tags map[string]string) {
	if span == nil {
		return
	}
	attrs := tagAttributes(tags)
	if len(attrs) == 0 {
		return
	}
	span.SetAttributes(attrs...)
}

func (c *Client) clientTagMetricAttributes() []attribute.KeyValue {
	if c == nil {
		return nil
	}
	return tagAttributes(c.config.Tags)
}

func metricIdentityAttributes(provider, model, agentName, agentVersion string) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(spanAttrProviderName, strings.TrimSpace(provider)),
		attribute.String(spanAttrRequestModel, strings.TrimSpace(model)),
		attribute.String(spanAttrAgentName, strings.TrimSpace(agentName)),
	}
	if version := strings.TrimSpace(agentVersion); version != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentVersion, version))
	}
	return attrs
}

func countToolCalls(messages []Message) int {
	total := 0
	for i := range messages {
		for j := range messages[i].Parts {
			if messages[i].Parts[j].Kind == PartKindToolCall {
				total++
			}
		}
	}
	return total
}

func toolSpanName(toolName string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		name = "unknown"
	}
	return "execute_tool " + name
}

func toolSpanAttributes(start ToolExecutionStart) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String(spanAttrOperationName, "execute_tool"),
		attribute.String(spanAttrToolName, start.ToolName),
		attribute.String(sdkMetadataKeyName, sdkName),
	}

	if callID := strings.TrimSpace(start.ToolCallID); callID != "" {
		attrs = append(attrs, attribute.String(spanAttrToolCallID, callID))
	}
	if toolType := strings.TrimSpace(start.ToolType); toolType != "" {
		attrs = append(attrs, attribute.String(spanAttrToolType, toolType))
	}
	if toolDescription := strings.TrimSpace(start.ToolDescription); toolDescription != "" {
		attrs = append(attrs, attribute.String(spanAttrToolDescription, toolDescription))
	}
	if conversationID := strings.TrimSpace(start.ConversationID); conversationID != "" {
		attrs = append(attrs, attribute.String(spanAttrConversationID, conversationID))
	}
	if conversationTitle := strings.TrimSpace(start.ConversationTitle); conversationTitle != "" {
		attrs = append(attrs, attribute.String(spanAttrConversationTitle, conversationTitle))
	}
	if agentName := strings.TrimSpace(start.AgentName); agentName != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentName, agentName))
	}
	if agentVersion := strings.TrimSpace(start.AgentVersion); agentVersion != "" {
		attrs = append(attrs, attribute.String(spanAttrAgentVersion, agentVersion))
	}
	if provider := strings.TrimSpace(start.RequestProvider); provider != "" {
		attrs = append(attrs, attribute.String(spanAttrProviderName, provider))
	}
	if model := strings.TrimSpace(start.RequestModel); model != "" {
		attrs = append(attrs, attribute.String(spanAttrRequestModel, model))
	}

	return attrs
}

func serializeToolContent(value any) (string, error) {
	if value == nil {
		return "", nil
	}

	switch v := value.(type) {
	case string:
		trimmed := strings.TrimSpace(v)
		if trimmed == "" {
			return "", nil
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed, nil
		}
		data, err := json.Marshal(trimmed)
		if err != nil {
			return "", err
		}
		return string(data), nil
	case []byte:
		trimmed := strings.TrimSpace(string(v))
		if trimmed == "" {
			return "", nil
		}
		if json.Valid([]byte(trimmed)) {
			return trimmed, nil
		}
		data, err := json.Marshal(trimmed)
		if err != nil {
			return "", err
		}
		return string(data), nil
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		trimmed := strings.TrimSpace(string(data))
		if trimmed == "null" {
			return "", nil
		}
		return trimmed, nil
	}
}

func applyTraceContextFromSpan(span trace.Span, generation *Generation) {
	if generation == nil || span == nil {
		return
	}

	spanContext := span.SpanContext()
	if !spanContext.IsValid() {
		return
	}

	generation.TraceID = spanContext.TraceID().String()
	generation.SpanID = spanContext.SpanID().String()
}

func mergeTags(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	out := cloneTags(base)
	if out == nil {
		out = map[string]string{}
	}
	maps.Copy(out, override)
	return out
}

func mergeMetadata(base, override map[string]any) map[string]any {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	out := cloneMetadata(base)
	if out == nil {
		out = map[string]any{}
	}
	maps.Copy(out, override)
	return out
}

func captureEmbeddingInputTexts(inputTexts []string, cfg EmbeddingCaptureConfig) []string {
	if len(inputTexts) == 0 {
		return nil
	}
	maxItems := cfg.MaxInputItems
	if maxItems <= 0 {
		maxItems = 20
	}
	maxTextLen := cfg.MaxTextLength
	if maxTextLen <= 0 {
		maxTextLen = 1024
	}
	if maxItems > len(inputTexts) {
		maxItems = len(inputTexts)
	}
	out := make([]string, 0, maxItems)
	for i := 0; i < maxItems; i++ {
		out = append(out, truncateEmbeddingText(inputTexts[i], maxTextLen))
	}
	return out
}

func truncateEmbeddingText(text string, maxLen int) string {
	if maxLen <= 0 || utf8.RuneCountInString(text) <= maxLen {
		return text
	}
	if maxLen <= len("...") {
		return truncateRunePrefix(text, maxLen)
	}
	return truncateRunePrefix(text, maxLen-len("...")) + "..."
}

func truncateRunePrefix(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runeCount := utf8.RuneCountInString(text)
	if runeCount <= maxRunes {
		return text
	}

	// Find the byte index after maxRunes runes
	byteIndex := 0
	for i := 0; i < maxRunes && byteIndex < len(text); i++ {
		_, size := utf8.DecodeRuneInString(text[byteIndex:])
		byteIndex += size
	}
	return text[:byteIndex]
}
