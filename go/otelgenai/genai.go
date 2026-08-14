package otelgenai

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// ScopeName is the instrumentation scope name reported on spans and metrics.
// It equals the package's import path, which is what an instrumentation
// library reports.
const ScopeName = "github.com/grafana/agento11y/go/otelgenai"

// SchemaURL is the semantic-convention schema the emitted telemetry follows.
const SchemaURL = semconv.SchemaURL

// These attributes postdate the semconv version this package is pinned to.
// If the package moves to a release that defines them, replace these keys
// with the generated helpers.
const (
	genAIRequestStreamCursorKey attribute.Key = "gen_ai.request.stream_cursor"
	genAIResponseStatusKey      attribute.Key = "gen_ai.response.status"
)

// CompletionHook observes a finished invocation before its span closes. It
// returns extra span attributes, and may transform the invocation itself,
// which is how content redaction plugs in.
//
// The hook runs before content is encoded, so a change it makes to messages
// reaches the span. It must not retain the invocation past the call.
//
// capture is the invocation's resolved capture mode. The handler cannot
// inspect what a hook returns, so a hook that has content of its own to add
// must check capture.SpanContent() and withhold that content itself.
//
// The package reports a hook panic to the OTel error handler. A panic in
// any hook drops the attributes from every hook and does not reach the
// instrumented application. The span still closes, and no content goes on
// it: a hook that died partway through may have redacted some of the content
// and left the rest raw.
//
// A hook may set the invocation's exported fields, including replacing the
// whole struct: End reads the span handle and the timestamps it needs before
// the hooks run, and restores them afterwards.
type CompletionHook interface {
	OnCompletion(inv *Invocation, capture CaptureMode) []attribute.KeyValue
}

// CompletionHookFunc adapts a function to CompletionHook.
type CompletionHookFunc func(inv *Invocation, capture CaptureMode) []attribute.KeyValue

// OnCompletion calls f with the finished invocation and resolved capture mode.
func (f CompletionHookFunc) OnCompletion(inv *Invocation, capture CaptureMode) []attribute.KeyValue {
	return f(inv, capture)
}

// Handler emits spans and metrics for GenAI invocations. It can also emit
// inference operation-details log records. The capture mode decides whether
// spans and records carry message content. A zero Handler is not usable;
// construct one with NewHandler.
type Handler struct {
	tracer             trace.Tracer
	logger             log.Logger
	instruments        instruments
	hooks              []CompletionHook
	capture            CaptureMode
	emitEventOverride  *bool
	extendedTokenTypes bool
}

type config struct {
	tracerProvider     trace.TracerProvider
	meterProvider      metric.MeterProvider
	loggerProvider     log.LoggerProvider
	hooks              []CompletionHook
	capture            CaptureMode
	emitEventOverride  *bool
	extendedTokenTypes bool
}

// Option configures a Handler.
type Option interface {
	apply(config) config
}

type optionFunc func(config) config

func (f optionFunc) apply(c config) config { return f(c) }

// WithTracerProvider sets the provider the handler takes its tracer from.
// Without this option, the handler uses the global tracer provider.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return optionFunc(func(c config) config {
		c.tracerProvider = provider
		return c
	})
}

// WithMeterProvider sets the provider the handler takes its meter from.
// Without this option, the handler uses the global meter provider.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return optionFunc(func(c config) config {
		c.meterProvider = provider
		return c
	})
}

// WithLoggerProvider sets the provider the handler takes its logger from.
// Without this option, the handler uses the global logger provider.
func WithLoggerProvider(provider log.LoggerProvider) Option {
	return optionFunc(func(c config) config {
		c.loggerProvider = provider
		return c
	})
}

// WithCompletionHook installs a hook. End calls the installed hooks in
// installation order.
func WithCompletionHook(hook CompletionHook) Option {
	return optionFunc(func(c config) config {
		if hook != nil {
			c.hooks = append(c.hooks, hook)
		}
		return c
	})
}

// WithExtendedTokenTypes makes the handler record the cache_read,
// cache_write, and reasoning token series. By default, a handler records only
// the registry's input and output token types, because a provider can already
// count cache tokens in InputTokens, and summing every token type then
// double-counts input.
func WithExtendedTokenTypes() Option {
	return optionFunc(func(c config) config {
		c.extendedTokenTypes = true
		return c
	})
}

// WithCaptureMode pins the content capture mode, overriding
// EnvCaptureMessageContent. ParseCaptureMode normalizes the value, so a
// lowercase spelling works here too. CaptureUnset leaves the environment in
// charge. An unrecognized mode goes to the OTel error handler and leaves
// content off.
func WithCaptureMode(mode CaptureMode) Option {
	return optionFunc(func(c config) config {
		if mode == CaptureUnset {
			return c
		}
		parsed, ok := ParseCaptureMode(string(mode))
		if !ok {
			otel.Handle(fmt.Errorf("otelgenai: unrecognized capture mode %q, emitting no content", string(mode)))
		}
		c.capture = parsed
		return c
	})
}

// WithEmitEvent pins whether inference operation-details records are emitted.
// It overrides EnvEmitEvent and the capture mode's default. Even when emit is
// true, a known non-inference operation emits no record.
func WithEmitEvent(emit bool) Option {
	return optionFunc(func(c config) config {
		c.emitEventOverride = &emit
		return c
	})
}

func newConfig(opts ...Option) config {
	cfg := config{capture: CaptureModeFromEnv()}
	for _, opt := range opts {
		cfg = opt.apply(cfg)
	}
	if cfg.emitEventOverride == nil {
		cfg.emitEventOverride = emitEventOverrideFromEnv()
	}
	return cfg
}

// NewHandler returns a Handler. Instrument creation errors are reported to the
// OpenTelemetry error handler; the corresponding no-op instruments keep the
// handler usable.
func NewHandler(opts ...Option) *Handler {
	cfg := newConfig(opts...)

	tracerProvider := cfg.tracerProvider
	if tracerProvider == nil {
		tracerProvider = otel.GetTracerProvider()
	}
	meterProvider := cfg.meterProvider
	if meterProvider == nil {
		meterProvider = otel.GetMeterProvider()
	}
	loggerProvider := cfg.loggerProvider
	if loggerProvider == nil {
		loggerProvider = logglobal.GetLoggerProvider()
	}

	meter := meterProvider.Meter(
		ScopeName,
		metric.WithInstrumentationVersion(version),
		metric.WithSchemaURL(SchemaURL),
	)
	inst, err := newInstruments(meter)
	if err != nil {
		otel.Handle(fmt.Errorf("otelgenai: create metric instruments: %w", err))
	}

	tracer := tracerProvider.Tracer(
		ScopeName,
		trace.WithInstrumentationVersion(version),
		trace.WithSchemaURL(SchemaURL),
	)
	logger := loggerProvider.Logger(
		ScopeName,
		log.WithInstrumentationVersion(version),
		log.WithSchemaURL(SchemaURL),
	)

	handler := &Handler{
		tracer:             tracer,
		logger:             logger,
		hooks:              cfg.hooks,
		capture:            cfg.capture,
		emitEventOverride:  cfg.emitEventOverride,
		extendedTokenTypes: cfg.extendedTokenTypes,
	}
	inst.extendedTokenTypes = handler.extendedTokenTypes
	handler.instruments = inst
	return handler
}

func (h *Handler) captureFor(inv *Invocation) CaptureMode {
	if inv == nil || inv.Capture == CaptureUnset {
		return h.capture
	}
	capture, ok := ParseCaptureMode(string(inv.Capture))
	if !ok {
		otel.Handle(fmt.Errorf("otelgenai: unrecognized invocation capture mode %q, using the handler default", inv.Capture))
		return h.capture
	}
	return capture
}

// Start opens the invocation's span and returns a context carrying it. The
// span is named for the operation and its subject, and takes the kind the
// conventions give that operation. It starts at inv.StartedAt, which Start
// fills with the current time when that field is zero.
//
// Start passes the request attributes to the tracer, so a sampler sees them.
// End writes them again from the finished invocation, and that second set is
// what an exporter reads.
//
// Starting an invocation twice would orphan the first span, so the second
// call returns the context unchanged.
func (h *Handler) Start(ctx context.Context, inv *Invocation) context.Context {
	if h == nil || inv == nil {
		return ctx
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if inv.span != nil || inv.ended {
		return ctx
	}
	if inv.StartedAt.IsZero() {
		inv.StartedAt = now()
	}

	ctx, span := h.tracer.Start(ctx, inv.spanName(),
		trace.WithSpanKind(inv.spanKind()),
		trace.WithTimestamp(inv.StartedAt),
		trace.WithAttributes(h.requestAttributes(inv)...),
	)
	inv.span = span
	inv.spanStartedAt = inv.StartedAt
	return ctx
}

// Span returns the invocation's span, or a non-recording span before Start.
func (inv *Invocation) Span() trace.Span {
	if inv == nil || inv.span == nil {
		return noopSpan
	}
	return inv.span
}

// RecordChunk records streaming timing when an output chunk arrives. It marks
// the invocation as streaming. The first call records the time to first
// chunk; each later call records the interval from the previous chunk on
// gen_ai.client.operation.time_per_output_chunk.
//
// If FirstChunkAt is already set on the first call, RecordChunk uses that
// timestamp for both the span attribute and the time-to-first-chunk metric.
func (h *Handler) RecordChunk(ctx context.Context, inv *Invocation) {
	if h == nil || inv == nil || inv.ended {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	inv.Stream = true
	chunkAt := now()
	if inv.lastChunkAt.IsZero() {
		start := inv.startTime()
		if start.IsZero() {
			return
		}
		if inv.FirstChunkAt.IsZero() {
			inv.FirstChunkAt = chunkAt
		} else {
			chunkAt = inv.FirstChunkAt
		}
		inv.lastChunkAt = chunkAt
		seconds := chunkAt.Sub(start).Seconds()
		if seconds < 0 {
			seconds = 0
		}
		inv.ttfcRecorded = true
		h.instruments.recordTimeToFirstChunk(ctx, inv, seconds)
		return
	}

	seconds := chunkAt.Sub(inv.lastChunkAt).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	inv.lastChunkAt = chunkAt
	h.instruments.recordTimePerOutputChunk(ctx, inv, seconds)
}

// End runs the completion hooks, writes the response attributes and status,
// records the metrics, emits the operation-details log record when enabled,
// and closes the span at inv.CompletedAt. End fills that timestamp with the
// current time when that field is zero. If inv.CompletedAt precedes the span
// start, End reports the clock skew and closes the span at the span start.
//
// The record carries request and response attributes. It carries message
// content only when the capture mode allows event content. A hook panic keeps
// content off both the span and the record.
//
// Calling End without Start can still record metrics when the invocation has
// the required timestamps or usage, and it can emit a log record. There is no
// span to close. Ending an invocation twice does nothing the second time, so a
// caller with a defer and an explicit call does not double-count.
func (h *Handler) End(ctx context.Context, inv *Invocation) {
	if h == nil || inv == nil || inv.ended {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	inv.ended = true
	if inv.CompletedAt.IsZero() {
		inv.CompletedAt = now()
	}
	capture := h.captureFor(inv)
	emitEvent := h.shouldEmitEvent(capture)

	// End reads the span handle and the timestamps before the hooks run, and
	// puts them back afterwards. A hook that rebuilds the invocation would
	// otherwise leave the span unclosed, its metrics unrecorded, or a second
	// End free to record everything twice.
	span := inv.span
	startedAt := inv.StartedAt
	spanStartedAt := inv.spanStartedAt
	completedAt := inv.CompletedAt
	firstChunkAt := inv.FirstChunkAt
	lastChunkAt := inv.lastChunkAt
	ttfcRecorded := inv.ttfcRecorded

	hookAttrs, panicked := h.runHooks(inv, capture)
	inv.ended = true
	inv.StartedAt = startedAt
	inv.spanStartedAt = spanStartedAt
	inv.CompletedAt = completedAt
	inv.FirstChunkAt = firstChunkAt
	inv.lastChunkAt = lastChunkAt
	inv.ttfcRecorded = ttfcRecorded
	if panicked {
		// The hook is the redaction step, and it died partway through. Which
		// fields are still raw is unknowable, so no content goes on the span.
		capture = CaptureNoContent
	}

	if span != nil {
		span.SetName(inv.spanName())
		attrs := h.requestAttributes(inv)
		attrs = append(attrs, h.responseAttributes(inv)...)
		attrs = append(attrs, h.contentAttributes(inv, capture)...)
		attrs = append(attrs, inv.Attributes...)
		attrs = append(attrs, hookAttrs...)
		span.SetAttributes(attrs...)
		// A successful operation leaves the status unset, which is what an
		// instrumentation library reports; Ok is the application's to set.
		if inv.errorType() != "" {
			span.SetStatus(codes.Error, inv.ErrorMessage)
		}
	}

	h.instruments.record(ctx, inv, metricAttributes(inv))
	emitEvent = emitEvent && isInferenceOperation(inv.operation())
	h.emitOperationDetails(ctx, span, inv, capture, emitEvent)

	if span != nil {
		if !spanStartedAt.IsZero() && completedAt.Before(spanStartedAt) {
			otel.Handle(fmt.Errorf("otelgenai: invocation completed %s before its span started, ending the span at its start",
				spanStartedAt.Sub(completedAt)))
			completedAt = spanStartedAt
		}
		span.End(trace.WithTimestamp(completedAt))
		inv.span = nil
	}
}

// runHooks calls every hook in installation order and reports whether any of
// them panicked.
func (h *Handler) runHooks(inv *Invocation, capture CaptureMode) ([]attribute.KeyValue, bool) {
	attrs := make([]attribute.KeyValue, 0, len(h.hooks))
	panicked := false
	for _, hook := range h.hooks {
		hookAttrs, hookPanicked := runHook(hook, inv, capture)
		attrs = append(attrs, hookAttrs...)
		panicked = panicked || hookPanicked
	}
	if panicked {
		return nil, true
	}
	return attrs, false
}

// runHook calls one hook and contains its panic. Instrumentation must not
// bring down the application it observes, so the panic is reported and the
// hook contributes nothing.
func runHook(hook CompletionHook, inv *Invocation, capture CaptureMode) (attrs []attribute.KeyValue, panicked bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			attrs = nil
			panicked = true
			otel.Handle(fmt.Errorf("otelgenai: completion hook %T panicked: %v", hook, recovered))
		}
	}()
	return hook.OnCompletion(inv, capture), false
}

// requestAttributes are the attributes known before the provider replies.
func (h *Handler) requestAttributes(inv *Invocation) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		semconv.GenAIOperationNameKey.String(string(inv.operation())),
	}
	if inv.Provider != "" {
		attrs = append(attrs, semconv.GenAIProviderNameKey.String(inv.Provider))
	}
	if inv.RequestModel != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(inv.RequestModel))
	}
	if inv.Stream {
		attrs = append(attrs, semconv.GenAIRequestStream(true))
	}
	if inv.ConversationID != "" {
		attrs = append(attrs, semconv.GenAIConversationID(inv.ConversationID))
	}
	if inv.AgentName != "" {
		attrs = append(attrs, semconv.GenAIAgentName(inv.AgentName))
	}
	if inv.AgentVersion != "" {
		attrs = append(attrs, semconv.GenAIAgentVersion(inv.AgentVersion))
	}
	if inv.AgentID != "" {
		attrs = append(attrs, semconv.GenAIAgentID(inv.AgentID))
	}
	if inv.AgentDescription != "" {
		attrs = append(attrs, semconv.GenAIAgentDescription(inv.AgentDescription))
	}
	if inv.DataSourceID != "" {
		attrs = append(attrs, semconv.GenAIDataSourceID(inv.DataSourceID))
	}
	if inv.WorkflowName != "" {
		attrs = append(attrs, semconv.GenAIWorkflowName(inv.WorkflowName))
	}
	if inv.StreamCursor != "" {
		attrs = append(attrs, genAIRequestStreamCursorKey.String(inv.StreamCursor))
	}
	if inv.MaxTokens != nil {
		attrs = append(attrs, semconv.GenAIRequestMaxTokens(int(*inv.MaxTokens)))
	}
	if inv.Temperature != nil {
		attrs = append(attrs, semconv.GenAIRequestTemperature(*inv.Temperature))
	}
	if inv.TopP != nil {
		attrs = append(attrs, semconv.GenAIRequestTopP(*inv.TopP))
	}
	if inv.TopK != nil {
		attrs = append(attrs, semconv.GenAIRequestTopK(float64(*inv.TopK)))
	}
	if inv.FrequencyPenalty != nil {
		attrs = append(attrs, semconv.GenAIRequestFrequencyPenalty(*inv.FrequencyPenalty))
	}
	if inv.PresencePenalty != nil {
		attrs = append(attrs, semconv.GenAIRequestPresencePenalty(*inv.PresencePenalty))
	}
	if len(inv.StopSequences) > 0 {
		attrs = append(attrs, semconv.GenAIRequestStopSequences(inv.StopSequences...))
	}
	if inv.Seed != nil {
		attrs = append(attrs, semconv.GenAIRequestSeed(int(*inv.Seed)))
	}
	if inv.ChoiceCount != nil {
		attrs = append(attrs, semconv.GenAIRequestChoiceCount(int(*inv.ChoiceCount)))
	}
	if inv.OutputType != "" {
		attrs = append(attrs, semconv.GenAIOutputTypeKey.String(inv.OutputType))
	}
	if len(inv.EncodingFormats) > 0 {
		attrs = append(attrs, semconv.GenAIRequestEncodingFormats(inv.EncodingFormats...))
	}
	if inv.DimensionCount != nil {
		attrs = append(attrs, semconv.GenAIEmbeddingsDimensionCount(int(*inv.DimensionCount)))
	}
	if inv.ToolName != "" {
		attrs = append(attrs, semconv.GenAIToolName(inv.ToolName))
	}
	if inv.ToolCallID != "" {
		attrs = append(attrs, semconv.GenAIToolCallID(inv.ToolCallID))
	}
	if inv.operation() == OperationExecuteTool && inv.ToolType != "" {
		attrs = append(attrs, semconv.GenAIToolTypeKey.String(inv.ToolType))
	}
	if inv.ToolDescription != "" {
		attrs = append(attrs, semconv.GenAIToolDescription(inv.ToolDescription))
	}
	if inv.ServerAddress != "" {
		attrs = append(attrs, semconv.ServerAddress(inv.ServerAddress))
	}
	if inv.ServerPort != 0 {
		attrs = append(attrs, semconv.ServerPort(inv.ServerPort))
	}
	return attrs
}

// responseAttributes are the non-content attributes known once the
// invocation finishes.
func (h *Handler) responseAttributes(inv *Invocation) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if inv.ResponseID != "" {
		attrs = append(attrs, semconv.GenAIResponseID(inv.ResponseID))
	}
	if inv.ResponseModel != "" {
		attrs = append(attrs, semconv.GenAIResponseModel(inv.ResponseModel))
	}
	if inv.ResponseStatus != "" {
		attrs = append(attrs, genAIResponseStatusKey.String(inv.ResponseStatus))
	}
	if len(inv.FinishReasons) > 0 {
		attrs = append(attrs, semconv.GenAIResponseFinishReasons(inv.FinishReasons...))
	}
	if ttfc, ok := inv.timeToFirstChunk(); ok {
		attrs = append(attrs, semconv.GenAIResponseTimeToFirstChunk(ttfc))
	}
	if inv.Usage.reported() {
		// Always emit the input and output counts so a zero-valued usage still
		// decodes as present.
		attrs = append(attrs,
			semconv.GenAIUsageInputTokens(int(inv.Usage.InputTokens)),
			semconv.GenAIUsageOutputTokens(int(inv.Usage.OutputTokens)),
		)
		if inv.Usage.CacheReadInputTokens != 0 {
			attrs = append(attrs, semconv.GenAIUsageCacheReadInputTokens(int(inv.Usage.CacheReadInputTokens)))
		}
		if inv.Usage.CacheWriteInputTokens != 0 {
			attrs = append(attrs, semconv.GenAIUsageCacheCreationInputTokens(int(inv.Usage.CacheWriteInputTokens)))
		}
		if inv.Usage.ReasoningTokens != 0 {
			attrs = append(attrs, semconv.GenAIUsageReasoningOutputTokens(int(inv.Usage.ReasoningTokens)))
		}
	}
	if errorType := inv.errorType(); errorType != "" {
		attrs = append(attrs, semconv.ErrorTypeKey.String(errorType))
	}
	return attrs
}

// contentAttributes encodes the message content. It returns nothing when the
// capture mode keeps content off the span.
//
// An encoder that cannot represent one field leaves that field out and keeps
// the rest of the attribute, and the reason goes to the OTel error handler.
func (h *Handler) contentAttributes(inv *Invocation, capture CaptureMode) []attribute.KeyValue {
	if !capture.SpanContent() {
		return nil
	}
	var attrs []attribute.KeyValue
	encode := func(key attribute.Key, encoder func() (string, error), populated bool) {
		if !populated {
			return
		}
		payload, err := encoder()
		if err != nil {
			otel.Handle(fmt.Errorf("otelgenai: encoding %s: %w", key, err))
		}
		if payload == "" {
			return
		}
		attrs = append(attrs, key.String(payload))
	}
	encode(semconv.GenAISystemInstructionsKey, func() (string, error) {
		return encodeSystemInstructions(inv.SystemInstructions)
	}, len(inv.SystemInstructions) > 0)
	encode(semconv.GenAIInputMessagesKey, func() (string, error) {
		return encodeMessages(inv.InputMessages, false)
	}, len(inv.InputMessages) > 0)
	encode(semconv.GenAIOutputMessagesKey, func() (string, error) {
		return encodeMessages(inv.OutputMessages, true)
	}, len(inv.OutputMessages) > 0)
	// Tool definitions are opt-in in the registry because schemas and
	// descriptions can contain sensitive application data. Keep them behind
	// the same explicit content-capture gate as messages.
	encode(semconv.GenAIToolDefinitionsKey, func() (string, error) {
		return encodeToolDefinitions(inv.ToolDefinitions)
	}, len(inv.ToolDefinitions) > 0)
	encode(semconv.GenAIToolCallArgumentsKey, func() (string, error) {
		payload, err := rawJSONField(inv.ToolCallArguments, "tool call arguments")
		return string(payload), err
	}, len(inv.ToolCallArguments) > 0)
	encode(semconv.GenAIToolCallResultKey, func() (string, error) {
		payload, err := rawJSONField(inv.ToolCallResult, "tool call result")
		return string(payload), err
	}, len(inv.ToolCallResult) > 0)
	if inv.RetrievalQueryText != "" {
		attrs = append(attrs, semconv.GenAIRetrievalQueryText(inv.RetrievalQueryText))
	}
	encode(semconv.GenAIRetrievalDocumentsKey, func() (string, error) {
		payload, err := rawJSONField(inv.RetrievalDocuments, "retrieval documents")
		return string(payload), err
	}, len(inv.RetrievalDocuments) > 0)
	return attrs
}

// metricAttributes are the invocation dimensions the spec instruments do not
// add themselves. The attribute set for the client metrics excludes agent
// identity, which belongs on the gen_ai.invoke_agent.* metrics.
// MetricAttributes carries any instrumentation-specific dimensions the caller
// adds.
func metricAttributes(inv *Invocation) []attribute.KeyValue {
	var attrs []attribute.KeyValue
	if inv.RequestModel != "" {
		attrs = append(attrs, semconv.GenAIRequestModel(inv.RequestModel))
	}
	if inv.ResponseModel != "" {
		attrs = append(attrs, semconv.GenAIResponseModel(inv.ResponseModel))
	}
	if inv.ServerAddress != "" {
		attrs = append(attrs, semconv.ServerAddress(inv.ServerAddress))
	}
	if inv.ServerPort != 0 {
		attrs = append(attrs, semconv.ServerPort(inv.ServerPort))
	}
	return append(attrs, inv.MetricAttributes...)
}

// SystemInstructionsFromText returns the system-instruction parts for a plain
// text prompt, which is the shape most providers take.
func SystemInstructionsFromText(text string) []Part {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return []Part{TextPart(text)}
}

var noopSpan trace.Span = tracenoop.Span{}
