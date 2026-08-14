package otelgenai_test

import (
	"context"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/otelgenai"
)

// encodeContent runs one invocation through a span-content handler and
// returns its content attributes.
func encodeContent(t *testing.T, inv *otelgenai.Invocation) map[string]string {
	t.Helper()
	return captureContent(t, inv, otelgenai.CaptureSpanOnly)
}

func captureContent(t *testing.T, inv *otelgenai.Invocation, mode otelgenai.CaptureMode) map[string]string {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	handler := otelgenai.NewHandler(
		otelgenai.WithTracerProvider(provider),
		otelgenai.WithCaptureMode(mode),
	)

	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	out := map[string]string{}
	for key, value := range spanAttrs(ended[0]) {
		out[key] = value.AsString()
	}
	return out
}

func strptr(s string) *string { return &s }

func TestContentEncoding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		inv  otelgenai.Invocation
		key  string
		want string
	}{
		{
			name: "text messages",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role:  otelgenai.RoleUser,
					Parts: []otelgenai.Part{otelgenai.TextPart("What is the weather in Paris?")},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"What is the weather in Paris?"}]}]`,
		},
		{
			name: "output message carries finish reason",
			inv: otelgenai.Invocation{
				OutputMessages: []otelgenai.Message{{
					Role:         otelgenai.RoleAssistant,
					FinishReason: strptr("stop"),
					Parts:        []otelgenai.Part{otelgenai.TextPart("It is 18C.")},
				}},
			},
			key:  "gen_ai.output.messages",
			want: `[{"role":"assistant","parts":[{"type":"text","content":"It is 18C."}],"finish_reason":"stop"}]`,
		},
		{
			name: "tool call carries raw json arguments",
			inv: otelgenai.Invocation{
				OutputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleAssistant,
					Parts: []otelgenai.Part{{
						Type:      otelgenai.PartTypeToolCall,
						ID:        "call_weather",
						Name:      "weather",
						Arguments: []byte(`{"city":"Paris"}`),
					}},
				}},
			},
			key:  "gen_ai.output.messages",
			want: `[{"role":"assistant","parts":[{"type":"tool_call","id":"call_weather","name":"weather","arguments":{"city":"Paris"}}],"finish_reason":""}]`,
		},
		{
			name: "tool response carries a json string document",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleTool,
					Parts: []otelgenai.Part{{
						Type:     otelgenai.PartTypeToolCallResponse,
						ID:       "call_weather",
						Response: []byte(`"{\"temp_c\":18}"`),
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_weather","response":"{\"temp_c\":18}"}]}]`,
		},
		{
			name: "part extension keys are appended in sorted order",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleTool,
					Parts: []otelgenai.Part{{
						Type: otelgenai.PartTypeToolCallResponse,
						ID:   "call_weather",
						Extensions: map[string]any{
							"vendor.tool_name": "weather",
							"vendor.is_error":  true,
						},
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"tool","parts":[{"type":"tool_call_response","id":"call_weather","response":null,"vendor.is_error":true,"vendor.tool_name":"weather"}]}]`,
		},
		{
			name: "system instructions",
			inv: otelgenai.Invocation{
				SystemInstructions: otelgenai.SystemInstructionsFromText("Be concise."),
			},
			key:  "gen_ai.system_instructions",
			want: `[{"type":"text","content":"Be concise."}]`,
		},
		{
			name: "non-text system instructions use the generic shape",
			inv: otelgenai.Invocation{
				SystemInstructions: []otelgenai.Part{{
					Type:       otelgenai.PartTypeToolCall,
					Name:       "weather",
					Extensions: map[string]any{"vendor.source": "policy"},
				}},
			},
			key:  "gen_ai.system_instructions",
			want: `[{"type":"tool_call","vendor.source":"policy"}]`,
		},
		{
			name: "tool definitions",
			inv: otelgenai.Invocation{
				ToolDefinitions: []otelgenai.ToolDefinition{{
					Type:        "function",
					Name:        "weather",
					Description: "Look up the weather",
					Parameters:  []byte(`{"type":"object"}`),
					Extensions:  map[string]any{"vendor.deferred": true},
				}},
			},
			key:  "gen_ai.tool.definitions",
			want: `[{"type":"function","name":"weather","description":"Look up the weather","parameters":{"type":"object"},"vendor.deferred":true}]`,
		},
		{
			name: "part fields outside its type are not encoded",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{{
						Type:      otelgenai.PartTypeText,
						Content:   strptr("hello"),
						ID:        "call_1",
						Name:      "weather",
						Arguments: []byte(`{"city":"Paris"}`),
						URI:       "https://example.invalid/x",
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"hello"}]}]`,
		},
		{
			name: "extension key cannot shadow a schema key",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{{
						Type:    otelgenai.PartTypeText,
						Content: strptr("real text"),
						Extensions: map[string]any{
							"content":    "SHADOW",
							"type":       "OVERRIDE",
							"vendor.tag": "kept",
						},
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"real text","vendor.tag":"kept"}]}]`,
		},
		{
			name: "an extension key the schema omitted is still usable",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{{
						Type:       otelgenai.PartTypeText,
						Content:    strptr("hello"),
						Extensions: map[string]any{"uri": "https://example.invalid/x"},
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"hello","uri":"https://example.invalid/x"}]}]`,
		},
		{
			name: "an unencodable extension keeps the rest of the message",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{
						otelgenai.TextPart("kept"),
						{Type: otelgenai.PartTypeText, Content: strptr("also kept"), Extensions: map[string]any{"vendor.bad": make(chan int)}},
					},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"kept"},{"type":"text","content":"also kept"}]}]`,
		},
		{
			name: "compaction carries its content",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{{
						Type:       otelgenai.PartTypeCompaction,
						ID:         "c1",
						Content:    strptr("summary"),
						Extensions: map[string]any{"vendor.turns": 12},
					}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"compaction","content":"summary","id":"c1","vendor.turns":12}]}]`,
		},
		{
			name: "compaction omits unset optional fields",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role:  otelgenai.RoleUser,
					Parts: []otelgenai.Part{{Type: otelgenai.PartTypeCompaction}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"compaction"}]}]`,
		},
		{
			name: "a dropped part leaves the parts beside it alone",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleUser,
					Parts: []otelgenai.Part{
						otelgenai.TextPart("kept"),
						{Type: ""},
						otelgenai.TextPart("also kept"),
					},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[{"type":"text","content":"kept"},{"type":"text","content":"also kept"}]}]`,
		},
		{
			name: "a message whose parts all drop still carries the required key",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role:  otelgenai.RoleUser,
					Parts: []otelgenai.Part{{Type: ""}},
				}},
			},
			key:  "gen_ai.input.messages",
			want: `[{"role":"user","parts":[]}]`,
		},
		{
			name: "schema-required part keys are emitted when the caller left them empty",
			inv: otelgenai.Invocation{
				InputMessages: []otelgenai.Message{{
					Role: otelgenai.RoleTool,
					Parts: []otelgenai.Part{
						{Type: otelgenai.PartTypeText},
						{Type: otelgenai.PartTypeToolCallResponse, ID: "call_1"},
						{Type: otelgenai.PartTypeBlob},
						{Type: otelgenai.PartTypeFile},
						{Type: otelgenai.PartTypeURI},
					},
				}},
			},
			key: "gen_ai.input.messages",
			want: `[{"role":"tool","parts":[` +
				`{"type":"text","content":""},` +
				`{"type":"tool_call_response","id":"call_1","response":null},` +
				`{"type":"blob","content":""},` +
				`{"type":"file","file_id":""},` +
				`{"type":"uri","uri":""}` +
				`]}]`,
		},
		{
			name: "a server tool call uses the server payload key",
			inv: otelgenai.Invocation{
				OutputMessages: []otelgenai.Message{{
					Role:         otelgenai.RoleAssistant,
					FinishReason: strptr("stop"),
					Parts: []otelgenai.Part{{
						Type:      otelgenai.PartTypeServerToolCall,
						ID:        "srv_1",
						Name:      "web_search",
						Arguments: []byte(`{"q":"weather"}`),
					}},
				}},
			},
			key:  "gen_ai.output.messages",
			want: `[{"role":"assistant","parts":[{"type":"server_tool_call","id":"srv_1","name":"web_search","server_tool_call":{"q":"weather"}}],"finish_reason":"stop"}]`,
		},
		{
			name: "an empty server tool response uses the required server payload key",
			inv: otelgenai.Invocation{
				OutputMessages: []otelgenai.Message{{
					Role:         otelgenai.RoleAssistant,
					FinishReason: strptr("stop"),
					Parts: []otelgenai.Part{{
						Type: otelgenai.PartTypeServerToolCallResponse,
						ID:   "srv_1",
					}},
				}},
			},
			key:  "gen_ai.output.messages",
			want: `[{"role":"assistant","parts":[{"type":"server_tool_call_response","id":"srv_1","server_tool_call_response":null}],"finish_reason":"stop"}]`,
		},
		{
			name: "an output message with no finish reason still carries the required key",
			inv: otelgenai.Invocation{
				OutputMessages: []otelgenai.Message{{
					Role:  otelgenai.RoleAssistant,
					Parts: []otelgenai.Part{otelgenai.TextPart("hi")},
				}},
			},
			key:  "gen_ai.output.messages",
			want: `[{"role":"assistant","parts":[{"type":"text","content":"hi"}],"finish_reason":""}]`,
		},
		{
			name: "tool definitions carry the schema's required type and name",
			inv: otelgenai.Invocation{
				ToolDefinitions: []otelgenai.ToolDefinition{
					{Name: "weather", Parameters: []byte(`{"type":"object"}`)},
					{},
				},
			},
			key: "gen_ai.tool.definitions",
			want: `[{"type":"function","name":"weather","parameters":{"type":"object"}},` +
				`{"type":"function","name":""}]`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := tc.inv
			inv.RequestModel = "test-model"
			inv.StartedAt = startedAt
			inv.CompletedAt = completedAt
			attrs := encodeContent(t, &inv)
			if got := attrs[tc.key]; got != tc.want {
				t.Errorf("%s =\n%s\nwant\n%s", tc.key, got, tc.want)
			}
		})
	}
}

func TestContentCaptureGating(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		mode        otelgenai.CaptureMode
		key         string
		value       string
		wantPresent bool
		set         func(*otelgenai.Invocation)
	}{
		{
			name:        "tool call arguments on",
			mode:        otelgenai.CaptureSpanOnly,
			key:         "gen_ai.tool.call.arguments",
			value:       `{"city":"Paris"}`,
			wantPresent: true,
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallArguments = []byte(`{"city":"Paris"}`)
			},
		},
		{
			name: "tool call arguments off",
			mode: otelgenai.CaptureNoContent,
			key:  "gen_ai.tool.call.arguments",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallArguments = []byte(`{"city":"Paris"}`)
			},
		},
		{
			name:        "tool call result on",
			mode:        otelgenai.CaptureSpanOnly,
			key:         "gen_ai.tool.call.result",
			value:       `{"temperature":18}`,
			wantPresent: true,
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallResult = []byte(`{"temperature":18}`)
			},
		},
		{
			name: "tool call result off",
			mode: otelgenai.CaptureNoContent,
			key:  "gen_ai.tool.call.result",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallResult = []byte(`{"temperature":18}`)
			},
		},
		{
			name:        "retrieval query on",
			mode:        otelgenai.CaptureSpanOnly,
			key:         "gen_ai.retrieval.query.text",
			value:       "weather in Paris",
			wantPresent: true,
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalQueryText = "weather in Paris"
			},
		},
		{
			name: "retrieval query off",
			mode: otelgenai.CaptureNoContent,
			key:  "gen_ai.retrieval.query.text",
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalQueryText = "weather in Paris"
			},
		},
		{
			name:        "retrieval documents on",
			mode:        otelgenai.CaptureSpanOnly,
			key:         "gen_ai.retrieval.documents",
			value:       `[{"id":"doc-1","score":0.9}]`,
			wantPresent: true,
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalDocuments = []byte(`[{"id":"doc-1","score":0.9}]`)
			},
		},
		{
			name: "retrieval documents off",
			mode: otelgenai.CaptureNoContent,
			key:  "gen_ai.retrieval.documents",
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalDocuments = []byte(`[{"id":"doc-1","score":0.9}]`)
			},
		},
		{
			name: "invalid tool call arguments omitted",
			mode: otelgenai.CaptureSpanOnly,
			key:  "gen_ai.tool.call.arguments",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallArguments = []byte(`{"city":`)
			},
		},
		{
			name: "invalid tool call result omitted",
			mode: otelgenai.CaptureSpanOnly,
			key:  "gen_ai.tool.call.result",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallResult = []byte(`{"temperature":`)
			},
		},
		{
			name: "invalid retrieval documents omitted",
			mode: otelgenai.CaptureSpanOnly,
			key:  "gen_ai.retrieval.documents",
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalDocuments = []byte(`[{"id":`)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := chatInvocation()
			tc.set(inv)
			attrs := captureContent(t, inv, tc.mode)
			got, present := attrs[tc.key]
			if present != tc.wantPresent {
				t.Errorf("%s present = %v, want %v", tc.key, present, tc.wantPresent)
				return
			}
			if present && got != tc.value {
				t.Errorf("%s = %q, want %q", tc.key, got, tc.value)
			}
		})
	}
}

func TestParseCaptureMode(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value string
		want  otelgenai.CaptureMode
		ok    bool
	}{
		{value: "NO_CONTENT", want: otelgenai.CaptureNoContent, ok: true},
		{value: "span_only", want: otelgenai.CaptureSpanOnly, ok: true},
		{value: " EVENT_ONLY ", want: otelgenai.CaptureEventOnly, ok: true},
		{value: "SPAN_AND_EVENT", want: otelgenai.CaptureSpanAndEvent, ok: true},
		{value: "", want: otelgenai.CaptureNoContent, ok: false},
		{value: "yes", want: otelgenai.CaptureNoContent, ok: false},
	}
	for _, tc := range cases {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()

			got, ok := otelgenai.ParseCaptureMode(tc.value)
			if got != tc.want || ok != tc.ok {
				t.Errorf("ParseCaptureMode(%q) = (%q, %v), want (%q, %v)", tc.value, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestCaptureModeFromEnv(t *testing.T) {
	t.Setenv(otelgenai.EnvCaptureMessageContent, "SPAN_ONLY")

	if got := otelgenai.CaptureModeFromEnv(); got != otelgenai.CaptureSpanOnly {
		t.Fatalf("CaptureModeFromEnv() = %q, want SPAN_ONLY", got)
	}
}
