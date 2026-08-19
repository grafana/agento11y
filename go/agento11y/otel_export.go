package agento11y

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/agento11y/otelhook"
	"github.com/grafana/agento11y/go/otelgenai"
)

// FeatureOTelGenerationExport names the experimental feature that exports
// generations as GenAI-semconv spans instead of calling the generation-export
// endpoint. RequireExperimental writes the name into a sentence, so the value
// is prose rather than an identifier.
const FeatureOTelGenerationExport = "otel generation export"

// otelPartExtension keys carry the parts of the agento11y data model that the
// conventions' message schema has no field for. They match the extension keys
// that the backend's wire-format decoder reads.
const (
	otelPartExtProviderType = "agento11y.provider_type"
	otelPartExtToolName     = "agento11y.tool_name"
	otelPartExtIsError      = "agento11y.is_error"
	otelPartExtArgumentsB64 = "agento11y.arguments_b64"
	otelPartExtResponseB64  = "agento11y.response_b64"
	otelPartExtMediaName    = "agento11y.media_name"
	otelToolExtDeferred     = "agento11y.deferred"
	otelToolExtSchemaB64    = "agento11y.input_schema_b64"
)

// otelProviderStoredToWire maps the SDK's stored provider values to the OTel
// GenAI registry spellings. The backend applies the inverse mapping, so the
// round trip leaves a stored record unchanged.
var otelProviderStoredToWire = map[string]string{
	"gemini":             "gcp.gemini",
	"mistral":            "mistral_ai",
	"moonshotai":         "moonshot_ai",
	"vertex":             "gcp.vertex_ai",
	"bedrock":            "aws.bedrock",
	"azure-openai":       "azure.ai.openai",
	"azure-ai-inference": "azure.ai.inference",
	"watsonx":            "ibm.watsonx.ai",
	"x-ai":               "x_ai",
}

// otelExportEnabled reports whether generations leave this client as GenAI
// spans instead of proprietary export payloads.
func (c *Client) otelExportEnabled() bool {
	return c != nil && c.otelHandler != nil
}

// Flusher force-flushes whatever still holds finished spans, which in otel mode
// is where a generation goes. *go.opentelemetry.io/otel/sdk/trace.TracerProvider
// satisfies it.
//
// The SDK does not reach this method by asserting Config.TracerProvider to it.
// ForceFlush lives on the OTel SDK's provider and not on the API interface the
// SDK is handed, and a provider's lifecycle belongs to the application, so the
// application names the flush target itself. This follows otellambda, which
// takes the same interface as an explicit option.
type Flusher interface {
	ForceFlush(ctx context.Context) error
}

// flushOTel force-flushes Config.Flusher, which is where a generation goes in
// otel mode. Without it the SDK cannot tell whether the span processor still
// holds the batch, and a nil return would tell the caller that a batch is
// delivered when it might not be.
func (c *Client) flushOTel(ctx context.Context) error {
	if c.config.Flusher == nil {
		return fmt.Errorf("%w: set Config.Flusher to your tracer provider", ErrFlushNotVerifiable)
	}
	if err := c.config.Flusher.ForceFlush(ctx); err != nil {
		return fmt.Errorf("agento11y otel generation export flush: %w", err)
	}
	return nil
}

// newOTelHandler builds the otelgenai handler for otel-mode export. When the
// experimental gate is closed it returns a nil handler and an error, and the
// caller then falls back to the noop exporter.
//
// The handler gets its tracer and spec meter from Config.TracerProvider and
// Config.MeterProvider, or from the corresponding global providers.
// Config.Tracer and Config.Meter never reach it; see their Config docs.
//
// The handler logger comes from the global OTel logger provider, and
// otelHandlerOptions disables operation-details records. A process-wide OTel
// environment variable must not add a signal the client did not configure.
func newOTelHandler(cfg Config) (*otelgenai.Handler, error) {
	if err := RequireExperimental(FeatureOTelGenerationExport); err != nil {
		return nil, err
	}
	return otelgenai.NewHandler(otelHandlerOptions(cfg)...), nil
}

func otelHandlerOptions(cfg Config) []otelgenai.Option {
	return []otelgenai.Option{
		otelgenai.WithEndHook(otelhook.New()),
		otelgenai.WithCaptureMode(otelCaptureMode(cfg.ContentCapture)),
		otelgenai.WithEmitEvent(false),
		otelgenai.WithExtendedTokenTypes(),
		otelgenai.WithConformantMetrics(),
		otelgenai.WithTracerProvider(cfg.TracerProvider),
		otelgenai.WithMeterProvider(cfg.MeterProvider),
	}
}

// otelCaptureMode translates the SDK's content-capture mode onto the otelgenai
// setting. AGENTO11Y_CONTENT_CAPTURE_MODE alone decides that setting: in otel
// mode the span is the export, so the conventions'
// OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT would otherwise let a
// traces-side setting overrule the SDK's own content policy.
//
// FullWithMetadataSpans keeps content off the OTel destination by contract,
// and in otel mode there is no second destination, so that mode exports
// metadata only. NewClient logs that restriction when it builds the handler.
func otelCaptureMode(mode ContentCaptureMode) otelgenai.CaptureMode {
	switch resolveClientContentCaptureMode(mode) {
	case ContentCaptureModeFull, ContentCaptureModeNoToolContent:
		return otelgenai.CaptureSpanOnly
	case ContentCaptureModeMetadataOnly, ContentCaptureModeFullWithMetadataSpans:
		return otelgenai.CaptureNoContent
	default:
		return otelgenai.CaptureNoContent
	}
}

// startOTelGeneration opens the generation's span through otelgenai. Only the
// request fields are known here, and they are the fields a sampler can act on.
// endOTelGeneration overwrites all of them from the finished generation.
func (c *Client) startOTelGeneration(
	ctx context.Context,
	seed Generation,
	startedAt time.Time,
) (context.Context, trace.Span, *otelgenai.Invocation) {
	invocation := &otelgenai.Invocation{
		Operation:      otelOperation(seed.OperationName),
		Provider:       otelProviderName(seed.Model.Provider),
		RequestModel:   seed.Model.Name,
		ConversationID: seed.ConversationID,
		AgentName:      seed.AgentName,
		AgentVersion:   seed.AgentVersion,
		Stream:         seed.Mode == GenerationModeStream,
		MaxTokens:      cloneInt64Ptr(seed.MaxTokens),
		Temperature:    cloneFloat64Ptr(seed.Temperature),
		TopP:           cloneFloat64Ptr(seed.TopP),
		StartedAt:      startedAt,
	}
	ctx = c.otelHandler.Start(ctx, invocation)
	return ctx, invocation.Span(), invocation
}

// generationError is the classified outcome of one generation: the
// low-cardinality error.type, the SDK's error category, and the text that goes
// on the span status. All three are empty on success.
type generationError struct {
	errorType string
	category  string
	message   string
	// exportError is a failure of this SDK's own export of the record, which
	// leaves error.type and the span status alone: the generation itself
	// succeeded, and the conventions reserve both fields for an operation that
	// failed. It goes on agento11y.export.error so the drop stays visible in
	// the trace without reading as a failed model call.
	exportError string
	// rejected marks a record the SDK's own validator refused. The span still
	// closes, so the failure stays visible, but the span carries no generation
	// payload: every other protocol drops the record before the queue, and a
	// span that carries the generation id is what makes the backend store one.
	rejected bool
}

// endOTelGeneration fills the invocation from the finished generation and
// closes it through otelgenai, which writes the span attributes, the status,
// and the spec metrics.
//
// capture is the generation's resolved content-capture mode, which can differ
// from the client's when the call or the resolver overrode it.
func (c *Client) endOTelGeneration(
	ctx context.Context,
	invocation *otelgenai.Invocation,
	generation Generation,
	failure generationError,
	firstTokenAt time.Time,
	capture ContentCaptureMode,
) {
	if invocation == nil {
		return
	}
	applyGenerationToInvocation(invocation, generation, failure, firstTokenAt, capture)
	invocation.MetricAttributes = c.otelMetricAttributes(ctx, generation, failure)
	// metricExemplarContext strips the context down to its span context, which
	// is what an exemplar needs, because an exemplar reservoir retains whatever
	// the context holds for the life of the series. End reads the span from the
	// invocation rather than from the context, so the narrowed context costs
	// nothing here and the exemplar still points at this generation's span.
	c.otelHandler.End(metricExemplarContext(ctx), invocation)
}

// otelMetricAttributes are the dimensions the SDK's own generation metrics
// carry and the spec instruments do not add. Without them, otel mode loses
// agent identity, every agento11y.tag.* series, the error category, and the
// token semantics marker.
func (c *Client) otelMetricAttributes(ctx context.Context, generation Generation, failure generationError) []attribute.KeyValue {
	attrs := metricTagAttributes(mergeTags(c.config.Tags, TagsFromContext(ctx)))
	if failure.category != "" {
		attrs = append(attrs, metricStringAttribute(spanAttrErrorCategory, failure.category))
	}
	if generation.Usage.InputSemantics == TokenInputSemanticsInclusive {
		attrs = append(attrs, metricStringAttribute(attrTokenSemantics, tokenSemanticsInclusive))
	}
	if generation.AgentName != "" {
		attrs = append(attrs, metricStringAttribute(spanAttrAgentName, generation.AgentName))
	}
	if generation.AgentVersion != "" {
		attrs = append(attrs, metricStringAttribute(spanAttrAgentVersion, generation.AgentVersion))
	}
	return attrs
}

// applyGenerationToInvocation maps a normalized generation onto the
// invocation otelgenai emits. Content arrives already sanitized and, under
// metadata_only, already stripped.
func applyGenerationToInvocation(
	invocation *otelgenai.Invocation,
	generation Generation,
	failure generationError,
	firstTokenAt time.Time,
	capture ContentCaptureMode,
) {
	// The client's mode set the handler's default. capture is the mode after
	// the per-call field and the resolver applied their overrides.
	invocation.Capture = otelCaptureMode(capture)
	invocation.Operation = otelOperation(generation.OperationName)
	invocation.Provider = otelProviderName(generation.Model.Provider)
	invocation.RequestModel = generation.Model.Name
	invocation.ResponseModel = generation.ResponseModel
	invocation.ResponseID = generation.ResponseID
	invocation.ConversationID = generation.ConversationID
	invocation.AgentName = generation.AgentName
	invocation.AgentVersion = generation.AgentVersion
	invocation.Stream = generation.Mode == GenerationModeStream
	invocation.SystemInstructions = otelgenai.SystemInstructionsFromText(generation.SystemPrompt)
	invocation.InputMessages = otelMessages(generation.Input, nil)
	invocation.OutputMessages = otelMessages(generation.Output, &generation.StopReason)
	invocation.ToolDefinitions = otelToolDefinitions(generation.Tools)
	if generation.StopReason != "" {
		invocation.FinishReasons = []string{generation.StopReason}
	}
	// Reported stays unset: the SDK cannot distinguish an all-zero usage that a
	// provider returned from a usage the SDK never received, and otelgenai
	// counts any non-zero count as reported. Setting Reported here would export
	// input and output tokens of 0 for a call that never reached a provider.
	invocation.Usage = otelgenai.Usage{
		InputTokens:           generation.Usage.InputTokens,
		OutputTokens:          generation.Usage.OutputTokens,
		CacheReadInputTokens:  generation.Usage.CacheReadInputTokens,
		CacheWriteInputTokens: generation.Usage.CacheWriteInputTokens,
		ReasoningTokens:       generation.Usage.ReasoningTokens,
	}
	invocation.MaxTokens = cloneInt64Ptr(generation.MaxTokens)
	invocation.Temperature = cloneFloat64Ptr(generation.Temperature)
	invocation.TopP = cloneFloat64Ptr(generation.TopP)
	// This function does not reassign StartedAt. Start fixed the span's start
	// instant and the metrics measure from it, so a generation's own start would
	// disagree with both. A mapper that reports a provider-side start therefore
	// changes the duration on the proprietary path and leaves the duration here
	// alone.
	invocation.CompletedAt = generation.CompletedAt
	if invocation.Stream {
		invocation.FirstChunkAt = firstTokenAt
	}
	invocation.ErrorType = failure.errorType
	invocation.ErrorMessage = failure.message
	if failure.category != "" {
		// The category is what the SDK's error dashboards group by; the
		// conventions have no equivalent.
		invocation.Attributes = append(invocation.Attributes,
			attribute.String(spanAttrErrorCategory, failure.category))
	}
	if failure.exportError != "" {
		invocation.Attributes = append(invocation.Attributes,
			attribute.String(spanAttrExportError, failure.exportError))
	}
	if failure.rejected {
		invocation.SystemInstructions = nil
		invocation.InputMessages = nil
		invocation.OutputMessages = nil
		invocation.ToolDefinitions = nil
		invocation.Vendor = nil
		return
	}

	invocation.Vendor = otelhook.Generation{
		ID:                  generation.ID,
		UserID:              generation.UserID,
		ConversationTitle:   generation.ConversationTitle,
		Tags:                generation.Tags,
		Metadata:            generation.Metadata,
		Artifacts:           otelArtifacts(generation.Artifacts),
		ParentGenerationIDs: generation.ParentGenerationIDs,
		// The raw version, not the digest: the hook hashes it, as codec.ToProto
		// does on the proprietary path.
		EffectiveVersion:        generation.EffectiveVersion,
		ToolChoice:              cloneStringPtr(generation.ToolChoice),
		ThinkingEnabled:         cloneBoolPtr(generation.ThinkingEnabled),
		TotalTokens:             generation.Usage.TotalTokens,
		InclusiveTokenSemantics: generation.Usage.InputSemantics == TokenInputSemanticsInclusive,
	}
}

// otelOperation maps the SDK's operation name onto the conventions' operation.
// The two mode defaults become the spec's chat operation, which the span name
// and the metric dimension carry. Any other name is the caller's own, and
// passes through unchanged.
func otelOperation(operationName string) otelgenai.Operation {
	switch operationName {
	case "", defaultOperationNameSync, defaultOperationNameStream:
		return otelgenai.OperationChat
	default:
		return otelgenai.Operation(operationName)
	}
}

// otelArtifacts maps raw artifacts onto the hook's wire shape. Artifacts carry
// provider payloads, so the hook emits them only when content capture allows
// it.
func otelArtifacts(artifacts []Artifact) []otelhook.Artifact {
	if len(artifacts) == 0 {
		return nil
	}
	out := make([]otelhook.Artifact, 0, len(artifacts))
	for _, artifact := range artifacts {
		out = append(out, otelhook.Artifact{
			Kind:        artifact.Kind,
			Name:        artifact.Name,
			ContentType: artifact.ContentType,
			Payload:     artifact.Payload,
			RecordID:    artifact.RecordID,
			URI:         artifact.URI,
		})
	}
	return out
}

// otelProviderName returns the registry spelling of a stored provider value.
func otelProviderName(provider string) string {
	if wire, ok := otelProviderStoredToWire[provider]; ok {
		return wire
	}
	return provider
}

// otelMessages maps SDK messages onto the conventions' message schema.
// finishReason is non-nil for output messages, where the schema requires the
// key even when the value is empty.
func otelMessages(messages []Message, finishReason *string) []otelgenai.Message {
	if len(messages) == 0 {
		return nil
	}
	out := make([]otelgenai.Message, 0, len(messages))
	for _, message := range messages {
		converted := otelgenai.Message{
			Role:         otelgenai.Role(message.Role),
			Name:         message.Name,
			FinishReason: finishReason,
		}
		for _, part := range message.Parts {
			mapped, ok := otelPart(part)
			if !ok {
				continue
			}
			converted.Parts = append(converted.Parts, mapped)
		}
		out = append(out, converted)
	}
	return out
}

// otelPart maps one SDK part onto a message part. Parts with no payload have
// no shape in the schema, so otelPart drops them.
func otelPart(part Part) (otelgenai.Part, bool) {
	out := otelgenai.Part{}
	if providerType := part.Metadata.ProviderType; providerType != "" {
		out.Extensions = map[string]any{otelPartExtProviderType: providerType}
	}
	switch part.Kind {
	case PartKindText:
		text := part.Text
		out.Type = otelgenai.PartTypeText
		out.Content = &text
	case PartKindThinking:
		thinking := part.Thinking
		out.Type = otelgenai.PartTypeReasoning
		out.Content = &thinking
	case PartKindToolCall:
		if part.ToolCall == nil {
			return otelgenai.Part{}, false
		}
		out.Type = otelgenai.PartTypeToolCall
		out.ID = part.ToolCall.ID
		out.Name = part.ToolCall.Name
		arguments, b64 := otelJSONBytes(part.ToolCall.InputJSON)
		out.Arguments = arguments
		if b64 != "" {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtArgumentsB64, b64)
		}
	case PartKindToolResult:
		if part.ToolResult == nil {
			return otelgenai.Part{}, false
		}
		out.Type = otelgenai.PartTypeToolCallResponse
		out.ID = part.ToolResult.ToolCallID
		if part.ToolResult.Name != "" {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtToolName, part.ToolResult.Name)
		}
		if part.ToolResult.IsError {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtIsError, true)
		}
		response, b64 := otelToolResponse(part.ToolResult.Content, part.ToolResult.ContentJSON)
		out.Response = response
		if b64 != "" {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtResponseB64, b64)
		}
	case PartKindMedia:
		if part.Media == nil {
			return otelgenai.Part{}, false
		}
		otelMediaPart(&out, part.Media)
	default:
		return otelgenai.Part{}, false
	}
	return out, true
}

// otelMediaPart renders media as one of the conventions' media part shapes: a
// base64 data URL becomes an inline blob, any other URL a uri reference, and
// an empty URL a file reference keyed by name.
//
// A declared mime type that disagrees with the data URL's own counts as a
// mislabeled payload and becomes a uri instead. Two cases keep the blob shape:
// the comparison ignores case, and an undeclared mime type takes the data
// URL's mime type.
func otelMediaPart(out *otelgenai.Part, media *Media) {
	if media.Kind != "" {
		kind := media.Kind
		out.Modality = &kind
	}
	out.MimeType = media.MIMEType

	if mimeType, payload, ok := splitBase64DataURL(media.URL); ok &&
		(media.MIMEType == "" || strings.EqualFold(mimeType, media.MIMEType)) {
		out.Type = otelgenai.PartTypeBlob
		out.Content = &payload
		if out.MimeType == "" {
			out.MimeType = mimeType
		}
		if media.Name != "" {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtMediaName, media.Name)
		}
		return
	}
	if media.URL != "" {
		out.Type = otelgenai.PartTypeURI
		out.URI = media.URL
		if media.Name != "" {
			out.Extensions = otelSetExtension(out.Extensions, otelPartExtMediaName, media.Name)
		}
		return
	}
	name := media.Name
	out.Type = otelgenai.PartTypeFile
	out.FileID = &name
}

func splitBase64DataURL(url string) (mimeType string, payload string, ok bool) {
	const prefix = "data:"
	const marker = ";base64,"
	if !strings.HasPrefix(url, prefix) {
		return "", "", false
	}
	mimeType, payload, ok = strings.Cut(url[len(prefix):], marker)
	if !ok {
		return "", "", false
	}
	return mimeType, payload, true
}

func otelToolDefinitions(tools []ToolDefinition) []otelgenai.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]otelgenai.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		converted := otelgenai.ToolDefinition{
			Type:        tool.Type,
			Name:        tool.Name,
			Description: tool.Description,
		}
		parameters, b64 := otelJSONBytes(tool.InputSchema)
		converted.Parameters = parameters
		if tool.Deferred {
			converted.Extensions = otelSetExtension(converted.Extensions, otelToolExtDeferred, true)
		}
		if b64 != "" {
			converted.Extensions = otelSetExtension(converted.Extensions, otelToolExtSchemaB64, b64)
		}
		out = append(out, converted)
	}
	return out
}

// otelToolResponse renders a tool result's content onto the schema's
// `response` key. Structured results go on the wire raw. Anything else goes as
// a JSON string, with the exact bytes on the base64 extension key, which is the
// shape the backend decoder inverts.
func otelToolResponse(content string, contentJSON []byte) (json.RawMessage, string) {
	if len(contentJSON) == 0 {
		return otelJSONString(content), ""
	}
	// Text content wins: when the result carries text, otelToolResponse never
	// inspects the JSON.
	if content == "" {
		if raw, ok := otelEmbeddableJSON(contentJSON); ok && !otelIsJSONString(raw) {
			return raw, ""
		}
	}
	return otelJSONString(content), base64.StdEncoding.EncodeToString(contentJSON)
}

// otelJSONBytes places a JSON document on the wire raw when it survives a
// round trip and falls back to the base64 extension key otherwise.
func otelJSONBytes(raw []byte) (json.RawMessage, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	if embeddable, ok := otelEmbeddableJSON(raw); ok {
		return embeddable, ""
	}
	return nil, base64.StdEncoding.EncodeToString(raw)
}

// otelEmbeddableJSON reports whether raw can go on the wire as a JSON document
// and decode back to the same value. The bytes must already be compact, because
// the enclosing marshal compacts what it embeds.
//
// The enclosing marshal also escapes <, > and & as \u003c, \u003e and \u0026,
// which every JSON decoder reads back as the original character. Coding-agent
// tool arguments are full of those three bytes, so the comparison uses
// json.Compact, which leaves the three bytes alone. Demanding the escaped form
// would push most tool calls onto the base64 extension key and leave the
// semconv `arguments` field empty for every generic OTel consumer.
//
// otelEmbeddableJSON also rejects a JSON null, because the decoder reads a
// null as an absent payload.
func otelEmbeddableJSON(raw []byte) (json.RawMessage, bool) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false
	}
	var compact bytes.Buffer
	// Compact rejects invalid JSON, so this call is the validity check as well.
	if err := json.Compact(&compact, raw); err != nil {
		return nil, false
	}
	if !bytes.Equal(compact.Bytes(), raw) {
		return nil, false
	}
	return json.RawMessage(raw), true
}

func otelJSONString(value string) json.RawMessage {
	payload, _ := json.Marshal(value)
	return json.RawMessage(payload)
}

func otelIsJSONString(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '"'
}

func otelSetExtension(extensions map[string]any, key string, value any) map[string]any {
	if extensions == nil {
		extensions = map[string]any{}
	}
	extensions[key] = value
	return extensions
}
