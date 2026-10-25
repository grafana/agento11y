package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	generationpb "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// Use the real public SDK, OTel processor and OTLP exporter transformation.
// Only the transport is in memory; no capability check or network is involved.
type generationSpanTestTransport struct {
	mu      sync.Mutex
	request coltracepb.ExportTraceServiceRequest
}

func (*generationSpanTestTransport) Start(context.Context) error { return nil }
func (*generationSpanTestTransport) Stop(context.Context) error  { return nil }
func (c *generationSpanTestTransport) UploadTraces(_ context.Context, spans []*tracepb.ResourceSpans) error {
	wire, err := proto.Marshal(&coltracepb.ExportTraceServiceRequest{ResourceSpans: spans})
	if err != nil {
		return err
	}
	var decoded coltracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(wire, &decoded); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.request.ResourceSpans = append(c.request.ResourceSpans, decoded.ResourceSpans...)
	return nil
}

func generationSpanTestClient(t *testing.T, capture agento11y.ContentCaptureMode) (*agento11y.Client, *sdktrace.TracerProvider, *tracetest.SpanRecorder, *generationSpanTestTransport) {
	t.Helper()
	transport := &generationSpanTestTransport{}
	exporter, err := otlptrace.New(context.Background(), transport)
	require.NoError(t, err)
	spans := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter), sdktrace.WithSpanProcessor(spans))
	cfg := agento11y.DefaultConfig()
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolOTel
	cfg.EnableExperimentalFeatures = new(true)
	cfg.TracerProvider, cfg.Flusher = provider, provider
	cfg.ContentCapture = capture
	cfg.Tags = map[string]string{"client": "client-tag"}
	client := agento11y.NewClient(cfg)
	t.Cleanup(func() {
		require.NoError(t, client.Shutdown(context.Background()))
		require.NoError(t, provider.Shutdown(context.Background()))
	})
	return client, provider, spans, transport
}

func generationSpanTestDecode(t *testing.T, request *coltracepb.ExportTraceServiceRequest) []*generationpb.Generation {
	t.Helper()
	raw, partial := generationSpans(request)
	require.Nil(t, partial)
	out := make([]*generationpb.Generation, 0, len(raw))
	for _, document := range raw {
		var storageFields map[string]json.RawMessage
		require.NoError(t, json.Unmarshal(document, &storageFields))
		require.Contains(t, storageFields, "conversation_id", "local storage uses proto field names")
		require.Contains(t, storageFields, "trace_id")
		g := &generationpb.Generation{}
		require.NoError(t, protojson.Unmarshal(document, g))
		out = append(out, g)
	}
	return out
}

func TestGenerationSpanSDKRoundTrip(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", stream), func(t *testing.T) {
			client, _, recorded, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
			start := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.UTC)
			end := start.Add(3*time.Second + 42*time.Nanosecond)
			seed := agento11y.GenerationStart{
				ID: "generation-original", ConversationID: "conversation-original", StartedAt: start,
				Model:     agento11y.ModelRef{Provider: "gemini", Name: "model-request"},
				AgentName: "agent", AgentVersion: "v1", EffectiveVersion: "raw build version",
				ParentGenerationIDs: []string{"parent-b", "parent-a"},
				ConversationTitle:   "A title", UserID: "user-1", SystemPrompt: "System\nexact text",
				MaxTokens: new(int64(0)), Temperature: new(0.0), TopP: new(0.8),
				ToolChoice: new("auto"), ThinkingEnabled: new(false),
				Tools: []agento11y.ToolDefinition{
					{Name: "lookup", Type: "function", Description: "Find things", Deferred: true, InputSchema: []byte(`{"z":9007199254740993,"a":1}`)},
					{Name: "opaque", InputSchema: []byte(" { \"type\": \"object\" } ")},
				},
				Tags:     map[string]string{"record": "tag"},
				Metadata: map[string]any{"nested": map[string]any{"numbers": []any{json.Number("1.25"), true, nil}}, "count": json.Number("9007199254740991")},
			}
			var rec *agento11y.GenerationRecorder
			if stream {
				_, rec = client.StartStreamingGeneration(context.Background(), seed)
			} else {
				_, rec = client.StartGeneration(context.Background(), seed)
			}
			opaque := []byte{0, 255, 'x', '\n'}
			arguments := []byte(`{"z":9007199254740993,"a":[1,true]}`)
			response := []byte(`{"z":9007199254740993,"a":false}`)
			result := agento11y.Generation{
				CompletedAt: end, ResponseID: "response-id", ResponseModel: "model-response", StopReason: "stop",
				Usage: agento11y.TokenUsage{InputTokens: 40, OutputTokens: 10, TotalTokens: 99, CacheReadInputTokens: 7, CacheWriteInputTokens: 8, ReasoningTokens: 3, InputSemantics: agento11y.TokenInputSemanticsInclusive},
				Input: []agento11y.Message{
					{Role: agento11y.RoleUser, Name: "named-user", Parts: []agento11y.Part{
						{Kind: agento11y.PartKindText, Text: "hello", Metadata: agento11y.PartMetadata{ProviderType: "provider.text"}},
						agento11y.MediaPart(agento11y.Media{Kind: "image", URL: "data:image/png;base64,AP94Cg==", MIMEType: "image/png", Name: "inline"}),
						agento11y.MediaPart(agento11y.Media{Kind: "audio", URL: "https://example.test/audio", MIMEType: "audio/wav", Name: "remote"}),
					}},
					{Role: agento11y.RoleTool, Parts: []agento11y.Part{
						agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "c1", Name: "lookup", ContentJSON: response}),
						agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "c2", Name: "opaque", Content: "text wins", ContentJSON: opaque, IsError: true}),
						agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "c3", Content: "plain text"}),
						agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "c4", ContentJSON: []byte(`"json string"`)}),
					}},
				},
				Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Name: "named-agent", Parts: []agento11y.Part{
					agento11y.ThinkingPart("reasoning"), agento11y.TextPart("answer"),
					agento11y.ToolCallPart(agento11y.ToolCall{ID: "c1", Name: "lookup", InputJSON: arguments}),
					agento11y.ToolCallPart(agento11y.ToolCall{ID: "c2", Name: "opaque", InputJSON: opaque}),
				}}},
				Artifacts: []agento11y.Artifact{{Kind: agento11y.ArtifactKindResponse, Name: "raw", ContentType: "application/octet-stream", Payload: opaque, RecordID: "record-id", URI: "artifact://response"}},
			}
			rec.SetResult(result, errors.New("provider failed"))
			rec.End()
			require.NoError(t, rec.Err())
			require.NoError(t, client.Flush(context.Background()))
			decoded := generationSpanTestDecode(t, &transport.request)
			require.Len(t, decoded, 1)
			g := decoded[0]
			require.Equal(t, seed.ID, g.Id)
			require.Equal(t, seed.ConversationID, g.ConversationId)
			require.Equal(t, seed.ParentGenerationIDs, g.ParentGenerationIds)
			require.Len(t, recorded.Ended(), 1)
			span := recorded.Ended()[0]
			require.Equal(t, span.SpanContext().TraceID().String(), g.TraceId)
			require.Equal(t, span.SpanContext().SpanID().String(), g.SpanId)
			require.True(t, start.Equal(g.StartedAt.AsTime()))
			require.True(t, end.Equal(g.CompletedAt.AsTime()))
			wantMode := generationpb.GenerationMode_GENERATION_MODE_SYNC
			if stream {
				wantMode = generationpb.GenerationMode_GENERATION_MODE_STREAM
			}
			require.Equal(t, wantMode, g.Mode)
			require.Equal(t, "chat", g.OperationName)
			require.Equal(t, "gemini", g.Model.Provider)
			require.Equal(t, seed.Model.Name, g.Model.Name)
			require.Equal(t, "response-id", g.ResponseId)
			require.Equal(t, "model-response", g.ResponseModel)
			require.Equal(t, seed.AgentName, g.AgentName)
			require.Equal(t, seed.AgentVersion, g.AgentVersion)
			digest := sha256.Sum256([]byte(seed.EffectiveVersion))
			require.Equal(t, "sha256:"+hex.EncodeToString(digest[:]), g.GetEffectiveVersion()) // not hashed twice
			require.Equal(t, seed.SystemPrompt, g.SystemPrompt)
			require.Equal(t, seed.MaxTokens, g.MaxTokens)
			require.Equal(t, seed.Temperature, g.Temperature)
			require.Equal(t, seed.TopP, g.TopP)
			require.Equal(t, seed.ToolChoice, g.ToolChoice)
			require.Equal(t, seed.ThinkingEnabled, g.ThinkingEnabled)
			require.Equal(t, "stop", g.StopReason)
			require.True(t, proto.Equal(&generationpb.TokenUsage{InputTokens: 40, OutputTokens: 10, TotalTokens: 99, CacheReadInputTokens: 7, CacheWriteInputTokens: 8, ReasoningTokens: 3, InputSemantics: generationpb.TokenInputSemantics_TOKEN_INPUT_SEMANTICS_INCLUSIVE}, g.Usage), "%v", g.Usage)
			require.Equal(t, map[string]string{"client": "client-tag", "record": "tag"}, g.Tags)
			require.Equal(t, "A title", g.Metadata.Fields["agento11y.conversation.title"].GetStringValue())
			require.Equal(t, "user-1", g.Metadata.Fields["agento11y.user.id"].GetStringValue())
			require.Equal(t, "provider failed", g.CallError)
			require.Equal(t, g.CallError, g.Metadata.Fields["call_error"].GetStringValue())
			require.Equal(t, float64(9007199254740991), g.Metadata.Fields["count"].GetNumberValue())
			require.Equal(t, 1.25, g.Metadata.Fields["nested"].GetStructValue().Fields["numbers"].GetListValue().Values[0].GetNumberValue())
			require.Equal(t, "named-user", g.Input[0].Name)
			require.Equal(t, "hello", g.Input[0].Parts[0].GetText())
			require.Equal(t, "provider.text", g.Input[0].Parts[0].Metadata.ProviderType)
			for i := 1; i <= 2; i++ {
				want := result.Input[0].Parts[i].Media
				require.True(t, proto.Equal(&generationpb.Media{Kind: want.Kind, Url: want.URL, MimeType: want.MIMEType, Name: want.Name}, g.Input[0].Parts[i].GetMedia()))
			}
			require.Equal(t, response, g.Input[1].Parts[0].GetToolResult().ContentJson)
			require.True(t, proto.Equal(&generationpb.ToolResult{ToolCallId: "c2", Name: "opaque", Content: "text wins", ContentJson: opaque, IsError: true}, g.Input[1].Parts[1].GetToolResult()))
			require.Equal(t, "plain text", g.Input[1].Parts[2].GetToolResult().Content)
			require.Empty(t, g.Input[1].Parts[2].GetToolResult().ContentJson)
			require.Equal(t, []byte(`"json string"`), g.Input[1].Parts[3].GetToolResult().ContentJson)
			require.Equal(t, "reasoning", g.Output[0].Parts[0].GetThinking())
			require.Equal(t, "answer", g.Output[0].Parts[1].GetText())
			require.Equal(t, arguments, g.Output[0].Parts[2].GetToolCall().InputJson)
			require.Equal(t, opaque, g.Output[0].Parts[3].GetToolCall().InputJson)
			require.Equal(t, seed.Tools[0].InputSchema, json.RawMessage(g.Tools[0].InputSchemaJson))
			require.True(t, g.Tools[0].Deferred)
			require.Equal(t, seed.Tools[1].InputSchema, json.RawMessage(g.Tools[1].InputSchemaJson))
			require.Equal(t, "function", g.Tools[1].Type) // semconv default, not reconstructed SDK state
			require.True(t, proto.Equal(&generationpb.Artifact{Kind: generationpb.ArtifactKind_ARTIFACT_KIND_RESPONSE, Name: "raw", ContentType: "application/octet-stream", Payload: opaque, RecordId: "record-id", Uri: "artifact://response"}, g.RawArtifacts[0]))
		})
	}
}

func TestGenerationSpanSDKCaptureAndCompanions(t *testing.T) {
	for _, capture := range []agento11y.ContentCaptureMode{agento11y.ContentCaptureModeMetadataOnly, agento11y.ContentCaptureModeNoToolContent, agento11y.ContentCaptureModeFullWithMetadataSpans} {
		t.Run(fmt.Sprint(capture), func(t *testing.T) {
			client, provider, _, transport := generationSpanTestClient(t, capture)
			_, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{ID: "g", ConversationID: "c", Model: agento11y.ModelRef{Provider: "custom", Name: "m"}, ConversationTitle: "title"})
			rec.SetResult(agento11y.Generation{Input: []agento11y.Message{agento11y.UserTextMessage("hello")}, Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.ToolCallPart(agento11y.ToolCall{ID: "call", Name: "tool", InputJSON: []byte(`{"x":1}`)})}}}}, nil)
			rec.End()
			require.NoError(t, rec.Err())
			for _, attrs := range [][]attribute.KeyValue{
				{attribute.String("agento11y.generation.id", "companion")},
				{attribute.Bool("agento11y.record", true), attribute.String("agento11y.generation.id", "boolean-marker")},
				{attribute.String("gen_ai.operation.name", "execute_tool"), attribute.String("agento11y.generation.id", "tool"), attribute.String("gen_ai.conversation.id", "c")},
			} {
				_, companion := provider.Tracer("companions").Start(context.Background(), "companion")
				companion.SetAttributes(attrs...)
				companion.End()
			}
			require.NoError(t, client.Flush(context.Background()))
			decoded := generationSpanTestDecode(t, &transport.request)
			require.Len(t, decoded, 1)
			g := decoded[0]
			require.Equal(t, "g", g.Id)
			require.Equal(t, "custom", g.Model.Provider)
			require.Empty(t, g.CallError)
			if capture == agento11y.ContentCaptureModeMetadataOnly {
				require.Empty(t, g.Input)
				require.Empty(t, g.Output)
				require.NotContains(t, g.Metadata.Fields, "agento11y.conversation.title")
			} else {
				require.Equal(t, "hello", g.Input[0].Parts[0].GetText())
				require.Equal(t, []byte(`{"x":1}`), g.Output[0].Parts[0].GetToolCall().InputJson)
			}
		})
	}
}

func generationSpanTestAttr(key string, value any) *commonpb.KeyValue {
	v := &commonpb.AnyValue{}
	switch x := value.(type) {
	case string:
		v.Value = &commonpb.AnyValue_StringValue{StringValue: x}
	case bool:
		v.Value = &commonpb.AnyValue_BoolValue{BoolValue: x}
	case int64:
		v.Value = &commonpb.AnyValue_IntValue{IntValue: x}
	case float64:
		v.Value = &commonpb.AnyValue_DoubleValue{DoubleValue: x}
	case []any:
		array := &commonpb.ArrayValue{}
		for _, item := range x {
			array.Values = append(array.Values, generationSpanTestAttr("", item).Value)
		}
		v.Value = &commonpb.AnyValue_ArrayValue{ArrayValue: array}
	}
	return &commonpb.KeyValue{Key: key, Value: v}
}
func generationSpanTestSpan() *tracepb.Span {
	return &tracepb.Span{TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8), StartTimeUnixNano: 1_000_000_001, EndTimeUnixNano: 2_000_000_002,
		Attributes: []*commonpb.KeyValue{generationSpanTestAttr("agento11y.record", "true"), generationSpanTestAttr("agento11y.generation.id", "original"), generationSpanTestAttr("gen_ai.conversation.id", "conversation")}}
}
func generationSpanTestRequest(spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
}

func TestGenerationSpanMalformed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*tracepb.Span)
	}{
		{"missing generation id", func(s *tracepb.Span) { s.Attributes = append(s.Attributes[:1], s.Attributes[2:]...) }},
		{"blank generation id", func(s *tracepb.Span) { s.Attributes[1] = generationSpanTestAttr("agento11y.generation.id", " \t") }},
		{"nonstring generation id", func(s *tracepb.Span) { s.Attributes[1] = generationSpanTestAttr("agento11y.generation.id", int64(1)) }},
		{"control generation id", func(s *tracepb.Span) {
			s.Attributes[1] = generationSpanTestAttr("agento11y.generation.id", "bad\x00id")
		}},
		{"missing conversation", func(s *tracepb.Span) { s.Attributes = s.Attributes[:2] }},
		{"unsafe conversation", func(s *tracepb.Span) { s.Attributes[2] = generationSpanTestAttr("gen_ai.conversation.id", "../escape") }},
		{"short trace", func(s *tracepb.Span) { s.TraceId = s.TraceId[:15] }},
		{"zero trace", func(s *tracepb.Span) { s.TraceId = make([]byte, 16) }},
		{"short span", func(s *tracepb.Span) { s.SpanId = s.SpanId[:7] }},
		{"zero span", func(s *tracepb.Span) { s.SpanId = make([]byte, 8) }},
		{"zero start", func(s *tracepb.Span) { s.StartTimeUnixNano = 0 }},
		{"time overflow", func(s *tracepb.Span) { s.EndTimeUnixNano = math.MaxUint64 }},
		{"reversed times", func(s *tracepb.Span) { s.EndTimeUnixNano = 1 }},
		{"duplicate identity", func(s *tracepb.Span) { s.Attributes = append(s.Attributes, s.Attributes[1]) }},
	}
	badAttrs := []struct {
		key   string
		value any
	}{
		{"agento11y.agent.effective_version", "raw-version"},
		{"gen_ai.request.stream", "true"}, {"gen_ai.request.max_tokens", 1.0},
		{"gen_ai.request.temperature", math.Inf(1)}, {"gen_ai.usage.input_tokens", "1"},
		{"gen_ai.response.finish_reasons", []any{"stop", int64(1)}},
		{"agento11y.generation.parent_generation_ids", []any{""}},
		{"agento11y.generation.parent_generation_ids", "parent"},
		{"agento11y.gen_ai.request.thinking.enabled", "false"},
		{"gen_ai.token.semantics", "unknown"},
		{"agento11y.generation.tags", `{"tag":null}`},
		{"gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"ok","agento11y.arguments_b64":false}]}]`},
		{"gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":"ok","agento11y.is_error":"true"}]}]`},
		{"agento11y.generation.tags", `{"tag":123}`},
		{"agento11y.generation.metadata", `null`},
		{"agento11y.generation.metadata", `[]`},
		{"agento11y.generation.metadata", `{"a":1,"a":2}`},
		{"agento11y.generation.metadata", `{"n":1e1000}`},
		{"gen_ai.input.messages", int64(1)},
		{"gen_ai.input.messages", `[{"role":"user","parts":[{"type":"text","content":1}]}]`},
		{"gen_ai.input.messages", `[{"role":"system","parts":[]}]`},
		{"gen_ai.input.messages", `[null]`},
		{"gen_ai.input.messages", `[{"role":"user","parts":null}]`},
		{"gen_ai.input.messages", `[{"role":"user","parts":[{"type":"unknown"}]}]`},
		{"gen_ai.input.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"x","agento11y.arguments_b64":"!"}]}]`},
		{"gen_ai.input.messages", `[{"role":"tool","parts":[{"type":"tool_call_response","agento11y.response_b64":5}]}]`},
		{"gen_ai.input.messages", `[{"role":"tool","parts":[{"type":"tool_call_response","agento11y.is_error":"true"}]}]`},
		{"gen_ai.input.messages", `[{"role":"user","parts":[{"type":"blob","content":"AB=="}]}]`},
		{"gen_ai.tool.definitions", `[{"name":"x","type":"function","agento11y.input_schema_b64":"aA==\n"}]`},
		{"gen_ai.tool.definitions", `[{"name":"x","type":"function","agento11y.deferred":"true"}]`},
		{"gen_ai.system_instructions", `[{"type":"text","content":false}]`},
		{"agento11y.generation.raw_artifacts", `[{"kind":"response","payload":"???"}]`},
		{"agento11y.generation.raw_artifacts", `[{"kind":"unknown","payload":"eA=="}]`},
		{"agento11y.generation.raw_artifacts", `[{"kind":"request","record_id":1}]`},
	}
	for i, attr := range badAttrs {
		cases = append(cases, struct {
			name   string
			mutate func(*tracepb.Span)
		}{fmt.Sprintf("attribute-%d-%s", i, attr.key), func(s *tracepb.Span) {
			s.Attributes = append(s.Attributes, generationSpanTestAttr(attr.key, attr.value))
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := generationSpanTestSpan()
			tc.mutate(s)
			_, err := generationSpan(s)
			require.Error(t, err)
			request := generationSpanTestRequest(generationSpanTestSpan(), s)
			out, partial := generationSpans(request)
			require.Len(t, out, 1)
			require.EqualValues(t, 1, partial.GetRejectedSpans())
			require.Len(t, request.ResourceSpans[0].ScopeSpans[0].Spans, 1)
		})
	}
	for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.system_instructions", "gen_ai.tool.definitions", "agento11y.generation.tags", "agento11y.generation.metadata", "agento11y.generation.raw_artifacts"} {
		for _, document := range []string{"", "{", "null", "{} []", "[{}] trailing"} {
			t.Run(key+"/"+document, func(t *testing.T) {
				s := generationSpanTestSpan()
				s.Attributes = append(s.Attributes, generationSpanTestAttr(key, document))
				_, err := generationSpan(s)
				require.Error(t, err)
				out, partial := generationSpans(generationSpanTestRequest(s))
				require.Empty(t, out)
				require.EqualValues(t, 1, partial.GetRejectedSpans())
			})
		}
	}
}

func TestGenerationSpanSelectionAndProviderMapping(t *testing.T) {
	require.Empty(t, generationSpanTestDecode(t, nil))
	require.Empty(t, generationSpanTestDecode(t, generationSpanTestRequest(nil, &tracepb.Span{})))
	for _, marker := range []any{true, "false", "TRUE", int64(1), " true "} {
		s := generationSpanTestSpan()
		s.Attributes[0] = generationSpanTestAttr("agento11y.record", marker)
		s.TraceId = nil // Ignored spans are not validated.
		require.Empty(t, generationSpanTestDecode(t, generationSpanTestRequest(s)))
	}
	for wire, stored := range map[string]string{"gcp.gemini": "gemini", "mistral_ai": "mistral", "moonshot_ai": "moonshotai", "gcp.vertex_ai": "vertex", "aws.bedrock": "bedrock", "azure.ai.openai": "azure-openai", "azure.ai.inference": "azure-ai-inference", "ibm.watsonx.ai": "watsonx", "x_ai": "x-ai", "custom": "custom"} {
		s := generationSpanTestSpan()
		s.Attributes = append(s.Attributes, generationSpanTestAttr("gen_ai.provider.name", wire))
		require.Equal(t, stored, generationSpanTestDecode(t, generationSpanTestRequest(s))[0].Model.Provider)
	}
}

func TestGenerationSpanFileMediaAndOpaquePayloads(t *testing.T) {
	s := generationSpanTestSpan()
	s.Attributes = append(s.Attributes,
		generationSpanTestAttr("gen_ai.input.messages", `[{"role":"user","parts":[{"type":"file","file_id":"file-name","modality":"image","mime_type":"image/png"}]}]`),
		generationSpanTestAttr("gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"null-json","arguments":null,"agento11y.arguments_b64":"bnVsbA=="}]}]`),
		generationSpanTestAttr("gen_ai.tool.definitions", `[{"type":"function","name":"empty","parameters":null}]`),
		generationSpanTestAttr("agento11y.tag.dimension_only", "not-a-generation-tag"),
		generationSpanTestAttr("gen_ai.usage.input_tokens", int64(9007199254740993)),
	)
	g := generationSpanTestDecode(t, generationSpanTestRequest(s))[0]
	require.Equal(t, "file-name", g.Input[0].Parts[0].GetMedia().Name)
	require.Equal(t, []byte("null"), g.Output[0].Parts[0].GetToolCall().InputJson)
	require.Empty(t, g.Tools[0].InputSchemaJson)
	require.NotContains(t, g.Tags, "dimension_only")
	require.Equal(t, int64(9007199254740993), g.Usage.InputTokens)
}

func TestGenerationSpanBatchOrder(t *testing.T) {
	for _, sameScope := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			t.Run(fmt.Sprintf("same_scope=%t/reject=%t", sameScope, reject), func(t *testing.T) {
				first, second := generationSpanTestSpan(), generationSpanTestSpan()
				second.Attributes[1] = generationSpanTestAttr("agento11y.generation.id", "second")
				unmarked := &tracepb.Span{Attributes: []*commonpb.KeyValue{generationSpanTestAttr("agento11y.generation.metadata", "malformed")}}
				spans := []*tracepb.Span{nil, first, unmarked}
				if reject {
					invalid := generationSpanTestSpan()
					invalid.SpanId = nil
					spans = append(spans, invalid)
				}
				request := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{
					nil,
					{ScopeSpans: []*tracepb.ScopeSpans{nil, {Spans: spans}}},
				}}
				if sameScope {
					request.ResourceSpans[1].ScopeSpans[1].Spans = append(spans, second)
				} else {
					request.ResourceSpans = append(request.ResourceSpans, &tracepb.ResourceSpans{ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{second}}}})
				}
				raw, partial := generationSpans(request)
				require.Len(t, raw, 2)
				if reject {
					require.EqualValues(t, 1, partial.GetRejectedSpans())
				} else {
					require.Nil(t, partial)
				}
				decoded := generationSpanTestDecode(t, request)
				require.Len(t, decoded, 2)
				require.Equal(t, "original", decoded[0].Id)
				require.Equal(t, "second", decoded[1].Id)
			})
		}
	}
}
