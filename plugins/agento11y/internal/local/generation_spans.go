package local

// This decoder inverts the public SDK's otel_export/otelhook wire format. It
// intentionally does not use a recorder or codec.ToProto: these are already
// normalized transport records (in particular, effective_version is a digest).
import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	generationpb "github.com/grafana/agento11y/go/proto/agento11y/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// The local storage reader expects snake_case proto field names.
// Rejected spans are removed before tool projection and forwarding.
func generationSpans(request *coltracepb.ExportTraceServiceRequest) ([]json.RawMessage, *coltracepb.ExportTracePartialSuccess) {
	var out []json.RawMessage
	var rejected int64
	for _, resource := range request.GetResourceSpans() {
		for _, scope := range resource.GetScopeSpans() {
			if scope == nil {
				continue
			}
			accepted := scope.Spans[:0]
			for _, span := range scope.GetSpans() {
				marked := false
				for _, attr := range span.GetAttributes() {
					if attr.GetKey() == "agento11y.record" {
						v, ok := attr.GetValue().GetValue().(*commonpb.AnyValue_StringValue)
						marked = marked || (ok && v.StringValue == "true")
					}
				}
				if !marked {
					accepted = append(accepted, span)
					continue
				}
				generation, err := generationSpan(span)
				if err != nil {
					rejected++
					continue
				}
				raw, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(generation)
				if err != nil {
					rejected++
					continue
				}
				out = append(out, raw)
				accepted = append(accepted, span)
			}
			clear(scope.Spans[len(accepted):])
			scope.Spans = accepted
		}
	}
	if rejected > 0 {
		return out, &coltracepb.ExportTracePartialSuccess{
			RejectedSpans: rejected,
			ErrorMessage:  "Invalid generation spans were rejected.",
		}
	}
	return out, nil
}

var generationSpanProviders = map[string]string{
	"gcp.gemini": "gemini", "mistral_ai": "mistral", "moonshot_ai": "moonshotai",
	"gcp.vertex_ai": "vertex", "aws.bedrock": "bedrock", "azure.ai.openai": "azure-openai",
	"azure.ai.inference": "azure-ai-inference", "ibm.watsonx.ai": "watsonx", "x_ai": "x-ai",
}

func generationSpan(span *tracepb.Span) (*generationpb.Generation, error) {
	if !generationSpanIDBytes(span.TraceId, 16) || !generationSpanIDBytes(span.SpanId, 8) {
		return nil, fmt.Errorf("invalid trace/span identity")
	}
	a := &generationSpanFields{values: map[string]any{}}
	for _, attr := range span.Attributes {
		if attr == nil {
			continue
		}
		if _, exists := a.values[attr.Key]; exists {
			return nil, fmt.Errorf("duplicate attribute %q", attr.Key)
		}
		a.values[attr.Key] = generationSpanAttribute(attr.Value)
	}
	g := &generationpb.Generation{
		Id:             a.text("agento11y.generation.id", true),
		ConversationId: a.text("gen_ai.conversation.id", true),
		TraceId:        hex.EncodeToString(span.TraceId), SpanId: hex.EncodeToString(span.SpanId),
		OperationName: a.text("gen_ai.operation.name", false),
		AgentName:     a.text("gen_ai.agent.name", false), AgentVersion: a.text("gen_ai.agent.version", false),
		ResponseId: a.text("gen_ai.response.id", false), ResponseModel: a.text("gen_ai.response.model", false),
		Mode:  generationpb.GenerationMode_GENERATION_MODE_SYNC,
		Model: &generationpb.ModelRef{Provider: a.text("gen_ai.provider.name", false), Name: a.text("gen_ai.request.model", false)},
		Usage: &generationpb.TokenUsage{},
	}
	if !generationSpanIdentity(g.Id) || !validConversationID(g.ConversationId) || !generationSpanIdentity(g.ConversationId) {
		a.failf("invalid generation/conversation identity")
	}
	if provider, ok := generationSpanProviders[g.Model.Provider]; ok {
		g.Model.Provider = provider
	}
	if stream := a.boolean("gen_ai.request.stream"); stream != nil && *stream {
		g.Mode = generationpb.GenerationMode_GENERATION_MODE_STREAM
	}
	g.ParentGenerationIds = a.strings("agento11y.generation.parent_generation_ids")
	for _, id := range g.ParentGenerationIds {
		if !generationSpanIdentity(id) {
			a.failf("invalid parent generation identity")
		}
	}
	if digest := a.text("agento11y.agent.effective_version", false); a.has("agento11y.agent.effective_version") {
		decoded, err := hex.DecodeString(strings.TrimPrefix(digest, "sha256:"))
		if err != nil || !strings.HasPrefix(digest, "sha256:") || len(decoded) != 32 || digest != strings.ToLower(digest) {
			a.failf("invalid effective_version digest")
		}
		g.EffectiveVersion = &digest
	}
	g.StartedAt = generationSpanTime(span.StartTimeUnixNano)
	g.CompletedAt = generationSpanTime(span.EndTimeUnixNano)
	if g.StartedAt == nil || g.CompletedAt == nil || span.EndTimeUnixNano < span.StartTimeUnixNano {
		a.failf("invalid generation timestamps")
	}
	g.MaxTokens = a.integer("gen_ai.request.max_tokens")
	g.Temperature = a.number("gen_ai.request.temperature")
	g.TopP = a.number("gen_ai.request.top_p")
	if a.has("agento11y.gen_ai.request.tool_choice") {
		g.ToolChoice = new(a.text("agento11y.gen_ai.request.tool_choice", false))
	}
	g.ThinkingEnabled = a.boolean("agento11y.gen_ai.request.thinking.enabled")
	for key, target := range map[string]*int64{
		"gen_ai.usage.input_tokens": &g.Usage.InputTokens, "gen_ai.usage.output_tokens": &g.Usage.OutputTokens,
		"gen_ai.usage.cache_read.input_tokens":     &g.Usage.CacheReadInputTokens,
		"gen_ai.usage.cache_creation.input_tokens": &g.Usage.CacheWriteInputTokens,
		"gen_ai.usage.reasoning.output_tokens":     &g.Usage.ReasoningTokens,
		"agento11y.gen_ai.usage.total_tokens":      &g.Usage.TotalTokens,
	} {
		if value := a.integer(key); value != nil {
			*target = *value
		}
	}
	if semantics := a.text("gen_ai.token.semantics", false); a.has("gen_ai.token.semantics") {
		if semantics != "inclusive" {
			a.failf("unknown token semantics")
		}
		g.Usage.InputSemantics = generationpb.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE
	}
	if reasons := a.strings("gen_ai.response.finish_reasons"); len(reasons) > 0 {
		g.StopReason = reasons[0]
	}
	if parts := a.documentArray("gen_ai.system_instructions"); parts != nil {
		var text []string
		for _, item := range parts {
			p := a.object(item)
			if p.text("type", true) != "text" {
				p.failf("unsupported system instruction part")
			}
			text = append(text, p.text("content", true))
		}
		g.SystemPrompt = strings.Join(text, "\n")
	}
	g.Input = generationSpanMessages(a, "gen_ai.input.messages")
	g.Output = generationSpanMessages(a, "gen_ai.output.messages")
	for _, item := range a.documentArray("gen_ai.tool.definitions") {
		t := a.object(item)
		g.Tools = append(g.Tools, &generationpb.ToolDefinition{
			Name: t.text("name", true), Type: t.text("type", true), Description: t.text("description", false),
			InputSchemaJson: t.payload("parameters", "agento11y.input_schema_b64"), Deferred: t.flag("agento11y.deferred"),
		})
	}
	g.Tags = map[string]string{}
	if a.has("agento11y.generation.tags") {
		tags := a.documentObject("agento11y.generation.tags")
		for key := range tags.values {
			g.Tags[key] = tags.text(key, true)
		}
	}
	metadata := map[string]any{}
	if a.has("agento11y.generation.metadata") {
		metadata = generationSpanPlain(a.documentObject("agento11y.generation.metadata").values).(map[string]any)
	}
	if title := a.text("agento11y.conversation.title", false); title != "" {
		metadata["agento11y.conversation.title"] = title
	}
	if user := a.text("user.id", false); user != "" {
		metadata["agento11y.user.id"] = user
	}
	if span.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR {
		g.CallError = span.GetStatus().GetMessage()
		if g.CallError == "" {
			// Local summaries use nonempty call_error as the failure indicator.
			g.CallError = "generation failed"
		}
		metadata["call_error"] = g.CallError
	}
	var err error
	if len(metadata) > 0 {
		// Struct's public wire schema uses doubles. UseNumber keeps numbers
		// intact until this final, schema-required conversion (and in payloads).
		g.Metadata, err = structpb.NewStruct(metadata)
		if err != nil {
			a.failf("invalid metadata: %v", err)
		}
	}
	kinds := map[string]generationpb.ArtifactKind{
		"request": generationpb.ArtifactKind_ARTIFACT_KIND_REQUEST, "response": generationpb.ArtifactKind_ARTIFACT_KIND_RESPONSE,
		"tools": generationpb.ArtifactKind_ARTIFACT_KIND_TOOLS, "provider_event": generationpb.ArtifactKind_ARTIFACT_KIND_PROVIDER_EVENT,
	}
	for _, item := range a.documentArray("agento11y.generation.raw_artifacts") {
		v := a.object(item)
		kind, ok := kinds[v.text("kind", true)]
		if !ok {
			v.failf("invalid artifact kind")
		}
		g.RawArtifacts = append(g.RawArtifacts, &generationpb.Artifact{
			Kind: kind, Name: v.text("name", false), ContentType: v.text("content_type", false),
			Payload: v.base64("payload"), RecordId: v.text("record_id", false), Uri: v.text("uri", false),
		})
	}
	return g, a.error()
}

func generationSpanMessages(a *generationSpanFields, key string) []*generationpb.Message {
	items := a.documentArray(key)
	out := make([]*generationpb.Message, 0, len(items))
	roles := map[string]generationpb.MessageRole{"user": generationpb.MessageRole_MESSAGE_ROLE_USER,
		"assistant": generationpb.MessageRole_MESSAGE_ROLE_ASSISTANT, "tool": generationpb.MessageRole_MESSAGE_ROLE_TOOL}
	for _, item := range items {
		m := a.object(item)
		role, ok := roles[m.text("role", true)]
		if !ok {
			m.failf("unsupported message role")
		}
		message := &generationpb.Message{Role: role, Name: m.text("name", false)}
		m.text("finish_reason", false) // Validate even though stop_reason lives on Generation.
		for _, item := range m.array("parts", true) {
			p := m.object(item)
			part := &generationpb.Part{}
			// Validate extension types even if misplaced on a different part.
			p.text("agento11y.tool_name", false)
			p.text("agento11y.media_name", false)
			p.boolean("agento11y.is_error")
			for _, key := range []string{"agento11y.arguments_b64", "agento11y.response_b64"} {
				if p.has(key) {
					p.base64(key)
				}
			}
			if provider := p.text("agento11y.provider_type", false); provider != "" {
				part.Metadata = &generationpb.PartMetadata{ProviderType: provider}
			}
			switch kind := p.text("type", true); kind {
			case "text":
				part.Payload = &generationpb.Part_Text{Text: p.text("content", true)}
			case "reasoning":
				part.Payload = &generationpb.Part_Thinking{Thinking: p.text("content", true)}
			case "tool_call":
				part.Payload = &generationpb.Part_ToolCall{ToolCall: &generationpb.ToolCall{
					Id: p.text("id", false), Name: p.text("name", true), InputJson: p.payload("arguments", "agento11y.arguments_b64"),
				}}
			case "tool_call_response":
				result := &generationpb.ToolResult{ToolCallId: p.text("id", false), Name: p.text("agento11y.tool_name", false), IsError: p.flag("agento11y.is_error")}
				if response, ok := p.values["response"].(string); ok {
					result.Content = response
				} else {
					result.ContentJson = p.payload("response", "")
				}
				if p.has("agento11y.response_b64") {
					result.ContentJson = p.base64("agento11y.response_b64")
				}
				part.Payload = &generationpb.Part_ToolResult{ToolResult: result}
			case "blob", "uri", "file":
				media := &generationpb.Media{Kind: p.text("modality", false), MimeType: p.text("mime_type", false), Name: p.text("agento11y.media_name", false)}
				switch kind {
				case "blob":
					p.base64("content")
					media.Url = "data:" + media.MimeType + ";base64," + p.text("content", true)
				case "uri":
					media.Url = p.text("uri", true)
				case "file":
					media.Name = p.text("file_id", true)
				}
				part.Payload = &generationpb.Part_Media{Media: media}
			default:
				p.failf("unsupported message part type %q", kind)
			}
			message.Parts = append(message.Parts, part)
		}
		out = append(out, message)
	}
	return out
}

func generationSpanIdentity(id string) bool {
	return strings.TrimSpace(id) != "" && utf8.ValidString(id) && !strings.ContainsFunc(id, unicode.IsControl)
}

func generationSpanIDBytes(id []byte, size int) bool {
	return len(id) == size && !bytes.Equal(id, make([]byte, size))
}

func generationSpanTime(ns uint64) *timestamppb.Timestamp {
	if ns == 0 || ns > math.MaxInt64 {
		return nil
	}
	return timestamppb.New(time.Unix(0, int64(ns)).UTC())
}

func generationSpanAttribute(value *commonpb.AnyValue) any {
	switch v := value.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_BoolValue:
		return v.BoolValue
	case *commonpb.AnyValue_IntValue:
		return v.IntValue
	case *commonpb.AnyValue_DoubleValue:
		return v.DoubleValue
	case *commonpb.AnyValue_ArrayValue:
		out := make([]any, 0, len(v.ArrayValue.GetValues()))
		for _, item := range v.ArrayValue.GetValues() {
			out = append(out, generationSpanAttribute(item))
		}
		return out
	default:
		return nil
	}
}

// Child objects share the first error with their parent; no bad field can be
// silently converted to a zero value, even in a document with valid siblings.
type generationSpanFields struct {
	values  map[string]any
	raw     map[string]json.RawMessage
	problem *error
}

func (f *generationSpanFields) failf(format string, args ...any) {
	if f.problem == nil {
		f.problem = new(error)
	}
	if *f.problem == nil {
		*f.problem = fmt.Errorf(format, args...)
	}
}
func (f *generationSpanFields) error() error {
	if f.problem == nil {
		return nil
	}
	return *f.problem
}
func (f *generationSpanFields) has(key string) bool { _, ok := f.values[key]; return ok }
func (f *generationSpanFields) text(key string, required bool) string {
	v, exists := f.values[key]
	if !exists && !required {
		return ""
	}
	s, ok := v.(string)
	if !ok {
		f.failf("%s: expected string", key)
	}
	return s
}
func (f *generationSpanFields) boolean(key string) *bool {
	if !f.has(key) {
		return nil
	}
	v, ok := f.values[key].(bool)
	if !ok {
		f.failf("%s: expected boolean", key)
	}
	return &v
}
func (f *generationSpanFields) flag(key string) bool {
	v := f.boolean(key)
	return v != nil && *v
}
func (f *generationSpanFields) integer(key string) *int64 {
	if !f.has(key) {
		return nil
	}
	v, ok := f.values[key].(int64)
	if !ok {
		f.failf("%s: expected OTLP integer", key)
	}
	return &v
}
func (f *generationSpanFields) number(key string) *float64 {
	if !f.has(key) {
		return nil
	}
	v, ok := f.values[key].(float64)
	if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
		f.failf("%s: expected finite OTLP double", key)
	}
	return &v
}
func (f *generationSpanFields) array(key string, required bool) []any {
	if !f.has(key) && !required {
		return nil
	}
	v, ok := f.values[key].([]any)
	if !ok {
		f.failf("%s: expected array", key)
	}
	return v
}
func (f *generationSpanFields) strings(key string) []string {
	values := f.array(key, false)
	out := make([]string, 0, len(values))
	for _, value := range values {
		v, ok := value.(string)
		if !ok {
			f.failf("%s: expected string array", key)
		}
		out = append(out, v)
	}
	return out
}
func (f *generationSpanFields) object(value any) *generationSpanFields {
	if f.problem == nil {
		f.problem = new(error)
	}
	v, ok := value.(generationSpanJSONObject)
	if !ok {
		f.failf("expected JSON object")
	}
	if v.values == nil {
		v.values = map[string]any{}
	}
	return &generationSpanFields{values: v.values, raw: v.raw, problem: f.problem}
}
func (f *generationSpanFields) document(key string) any {
	value, err := generationSpanJSON(f.text(key, true), key)
	if err != nil {
		f.failf("%s: %v", key, err)
	}
	return value
}
func (f *generationSpanFields) documentArray(key string) []any {
	if !f.has(key) {
		return nil
	}
	v, ok := f.document(key).([]any)
	if !ok {
		f.failf("%s: expected JSON array", key)
	}
	return v
}
func (f *generationSpanFields) documentObject(key string) *generationSpanFields {
	return f.object(f.document(key))
}
func (f *generationSpanFields) base64(key string) []byte {
	if !f.has(key) {
		return nil
	}
	s := f.text(key, true)
	v, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || strings.ContainsAny(s, "\r\n") {
		f.failf("%s: invalid base64", key)
	}
	return v
}
func (f *generationSpanFields) payload(key, extension string) []byte {
	if extension != "" && f.has(extension) {
		return f.base64(extension)
	}
	if !f.has(key) || f.values[key] == nil {
		return nil
	}
	// Keep embedded JSON as emitted, including key order and numeric lexemes.
	// The SDK uses the base64 extension when the original bytes cannot be
	// embedded (whitespace, null, invalid JSON, or dual text/JSON responses).
	return append([]byte(nil), f.raw[key]...)
}

func generationSpanJSON(document, key string) (any, error) {
	if !utf8.ValidString(document) {
		return nil, fmt.Errorf("invalid UTF-8")
	}
	d := json.NewDecoder(strings.NewReader(document))
	d.UseNumber()
	value, err := generationSpanJSONValue(d, []byte(document), []string{key}, 0)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return value, nil
}

type generationSpanJSONObject struct {
	values map[string]any
	raw    map[string]json.RawMessage
}

func generationSpanPlain(value any) any {
	switch v := value.(type) {
	case generationSpanJSONObject:
		return generationSpanPlain(v.values)
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = generationSpanPlain(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = generationSpanPlain(item)
		}
		return out
	default:
		return value
	}
}

func generationSpanOpaquePayload(path []string, key string) bool {
	if len(path) == 2 && path[0] == "gen_ai.tool.definitions" && path[1] == "" {
		return key == "parameters"
	}
	if len(path) == 4 && (path[0] == "gen_ai.input.messages" || path[0] == "gen_ai.output.messages") &&
		path[1] == "" && path[2] == "parts" && path[3] == "" {
		return key == "arguments" || key == "response"
	}
	return false
}

func generationSpanJSONValue(d *json.Decoder, document []byte, path []string, depth int) (any, error) {
	if depth > 10000 {
		return nil, fmt.Errorf("JSON nesting limit exceeded")
	}
	token, err := d.Token()
	if err != nil {
		return nil, err
	}
	switch token {
	case json.Delim('{'):
		out := generationSpanJSONObject{values: map[string]any{}, raw: map[string]json.RawMessage{}}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return nil, err
			}
			name, ok := key.(string)
			if !ok {
				return nil, fmt.Errorf("expected object key")
			}
			if _, exists := out.values[name]; exists {
				return nil, fmt.Errorf("duplicate JSON key %q", name)
			}
			start := d.InputOffset()
			if generationSpanOpaquePayload(path, name) {
				// The SDK accepts duplicate keys inside opaque JSON. Only the raw
				// bytes below are persisted; decoded values distinguish text/null.
				var value any
				err = d.Decode(&value)
				out.values[name] = value
			} else {
				out.values[name], err = generationSpanJSONValue(d, document, append(path, name), depth+1)
			}
			if err != nil {
				return nil, err
			}
			raw := bytes.TrimSpace(document[start:d.InputOffset()])
			raw = bytes.TrimSpace(bytes.TrimPrefix(raw, []byte(":")))
			out.raw[name] = json.RawMessage(raw)
		}
		_, err := d.Token()
		return out, err
	case json.Delim('['):
		out := []any{}
		for d.More() {
			value, err := generationSpanJSONValue(d, document, append(path, ""), depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
		}
		_, err := d.Token()
		return out, err
	default:
		return token, nil
	}
}
