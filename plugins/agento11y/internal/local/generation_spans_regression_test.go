package local

import (
	"context"
	"encoding/base64"
	"fmt"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	generationpb "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

func generationSpanRecordPayloads(t *testing.T, client *agento11y.Client, id, conversation string, arguments, response, schema []byte) {
	t.Helper()
	_, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ID: id, ConversationID: conversation,
		Model: agento11y.ModelRef{Provider: "custom", Name: "model"},
		Tools: []agento11y.ToolDefinition{{Name: "tool", InputSchema: schema}, {Name: "sibling", InputSchema: []byte(`{"type":"object"}`)}},
	})
	rec.SetResult(agento11y.Generation{
		Input: []agento11y.Message{{Role: agento11y.RoleTool, Parts: []agento11y.Part{
			agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "call", Name: "tool", ContentJSON: response}),
			agento11y.ToolResultPart(agento11y.ToolResult{ToolCallID: "sibling", Content: "plain sibling"}),
		}}},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			agento11y.ToolCallPart(agento11y.ToolCall{ID: "call", Name: "tool", InputJSON: arguments}),
			agento11y.TextPart("text sibling"),
		}}},
	}, nil)
	rec.End()
	require.NoError(t, rec.Err())
}

func generationSpanRequirePayloads(t *testing.T, g *generationpb.Generation, arguments, response, schema []byte) {
	t.Helper()
	require.Equal(t, arguments, g.Output[0].Parts[0].GetToolCall().InputJson)
	require.Equal(t, response, g.Input[0].Parts[0].GetToolResult().ContentJson)
	require.Equal(t, schema, g.Tools[0].InputSchemaJson)
	require.Equal(t, "text sibling", g.Output[0].Parts[1].GetText())
	require.Equal(t, "plain sibling", g.Input[0].Parts[1].GetToolResult().Content)
	require.Equal(t, []byte(`{"type":"object"}`), g.Tools[1].InputSchemaJson)
}

func TestGenerationSpanSDKDuplicatePayloads(t *testing.T) {
	for _, field := range []string{"arguments", "response", "parameters"} {
		for _, tc := range []struct {
			name    string
			payload string
			base64  bool
		}{
			{"duplicate", `{"x":1,"x":2}`, false},
			{"nested", `{"z":9007199254740993,"a":[{"x":1e+02,"x":-0}],"a":{"n":{"x":1,"x":2}}}`, false},
			{"nested array", `[{"x":1,"x":2},{"nested":[{"x":3,"x":4}]}]`, false},
			{"base64 whitespace", " \n{\"x\":1,\"x\":2,\"nested\":{\"x\":3,\"x\":4}}\t ", true},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
				plain := []byte(`{"ok":true}`)
				payloads := map[string][]byte{"arguments": plain, "response": plain, "parameters": plain}
				payloads[field] = []byte(tc.payload)
				generationSpanRecordPayloads(t, client, "before", "mixed", plain, plain, plain)
				generationSpanRecordPayloads(t, client, "payload", "mixed", payloads["arguments"], payloads["response"], payloads["parameters"])
				generationSpanRecordPayloads(t, client, "after", "mixed", plain, plain, plain)
				require.NoError(t, client.Flush(context.Background()))
				extension, attr := "agento11y.arguments_b64", "gen_ai.output.messages"
				if field == "response" {
					extension, attr = "agento11y.response_b64", "gen_ai.input.messages"
				} else if field == "parameters" {
					extension, attr = "agento11y.input_schema_b64", "gen_ai.tool.definitions"
				}
				var document string
				for _, a := range generationSpanFind(t, &transport.request, "payload").Attributes {
					if a.Key == attr {
						document = a.Value.GetStringValue()
					}
				}
				require.NotEmpty(t, document)
				if tc.base64 {
					require.Contains(t, document, fmt.Sprintf(`"%s":"%s"`, extension, base64.StdEncoding.EncodeToString([]byte(tc.payload))))
				} else {
					require.NotContains(t, document, extension)
					require.Contains(t, document, fmt.Sprintf(`"%s":%s`, field, tc.payload))
				}
				decoded := generationSpanTestDecode(t, &transport.request)
				require.Len(t, decoded, 3)
				require.Equal(t, []string{"before", "payload", "after"}, []string{decoded[0].Id, decoded[1].Id, decoded[2].Id})
				generationSpanRequirePayloads(t, decoded[0], plain, plain, plain)
				generationSpanRequirePayloads(t, decoded[1], payloads["arguments"], payloads["response"], payloads["parameters"])
				generationSpanRequirePayloads(t, decoded[2], plain, plain, plain)
			})
		}
	}
}

func TestGenerationSpanOuterDocumentsRemainStrict(t *testing.T) {
	for _, tc := range []struct{ key, document string }{
		{"gen_ai.input.messages", `[{"role":"user","role":"tool","parts":[]}]`},
		{"gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"x","arguments":{"x":1,"x":2},"arguments":{}}]}]`},
		{"gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"x","arguments":{"x":1,"x":2},"name":"y"}]}]`},
		{"gen_ai.input.messages", `[{"role":"tool","parts":[{"type":"tool_call_response","response":{"x":1,"x":2},"response":{}}]}]`},
		{"gen_ai.input.messages", `[{"role":"tool","parts":[{"type":"tool_call_response","response":{"x":1,"x":2}},{"type":"text","content":"a","content":"b"}]}]`},
		{"gen_ai.tool.definitions", `[{"name":"x","type":"function","parameters":{"x":1,"x":2},"parameters":{}}]`},
		{"gen_ai.tool.definitions", `[{"name":"x","type":"function","parameters":{"x":1,"x":2}},{"name":"y","name":"z","type":"function"}]`},
		{"agento11y.generation.metadata", `{"arguments":{"x":1,"x":2}}`},
		{"agento11y.generation.metadata", `{"response":{"x":1,"x":2}}`},
		{"agento11y.generation.metadata", `{"parameters":{"x":1,"x":2}}`},
		{"gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"x","arguments":{"x":1,}}]}]`},
		{"gen_ai.output.messages", `[{"role":"assistant","parts":[{"type":"tool_call","name":"x","arguments":{"x":1,"x":2}}]}] []`},
	} {
		t.Run(tc.key+"/"+tc.document, func(t *testing.T) {
			s := generationSpanTestSpan()
			s.Attributes = append(s.Attributes, generationSpanTestAttr(tc.key, tc.document))
			_, err := generationSpan(s)
			require.Error(t, err)
			out, partial := generationSpans(generationSpanTestRequest(generationSpanTestSpan(), s))
			require.Len(t, out, 1)
			require.EqualValues(t, 1, partial.GetRejectedSpans())
		})
	}
}

func generationSpanFind(t *testing.T, request *coltracepb.ExportTraceServiceRequest, id string) *tracepb.Span {
	t.Helper()
	for _, resource := range request.ResourceSpans {
		for _, scope := range resource.ScopeSpans {
			for _, span := range scope.Spans {
				for _, a := range span.Attributes {
					if a.Key == "agento11y.generation.id" && a.Value.GetStringValue() == id {
						return span
					}
				}
			}
		}
	}
	t.Fatalf("generation %q not exported", id)
	return nil
}
