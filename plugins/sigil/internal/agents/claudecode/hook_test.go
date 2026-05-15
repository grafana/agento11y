package claudecode

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestParseHookInput_PreToolUse(t *testing.T) {
	in := `{"hook_event_name":"PreToolUse","session_id":"s1","transcript_path":"/tmp/t.jsonl","tool_name":"Bash","tool_input":{"command":"echo hi"},"tool_use_id":"tu_1"}`
	got, err := parseHookInput(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parseHookInput: %v", err)
	}
	if got.HookEventName != "PreToolUse" {
		t.Fatalf("HookEventName=%q", got.HookEventName)
	}
	if got.ToolName != "Bash" || got.ToolUseID != "tu_1" {
		t.Fatalf("tool fields = %#v", got)
	}
	if len(got.ToolInput) == 0 || !strings.Contains(string(got.ToolInput), "echo hi") {
		t.Fatalf("ToolInput=%s", string(got.ToolInput))
	}
}

func TestParseHookInput_RejectsEmpty(t *testing.T) {
	if _, err := parseHookInput(strings.NewReader("")); err == nil {
		t.Fatal("expected error on empty stdin")
	}
	if _, err := parseHookInput(strings.NewReader(`{}`)); err == nil {
		t.Fatal("expected error on missing fields")
	}
}

func TestBuildToolResultMap(t *testing.T) {
	tests := []struct {
		name     string
		gens     []sigil.Generation
		wantIDs  []string
		wantErrs map[string]bool
	}{
		{name: "empty generations", gens: nil, wantIDs: nil},
		{
			name: "no tool results in input",
			gens: []sigil.Generation{{
				Input: []sigil.Message{{
					Role:  sigil.RoleUser,
					Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "hello"}},
				}},
			}},
			wantIDs: nil,
		},
		{
			name: "single tool result",
			gens: []sigil.Generation{{
				Input: []sigil.Message{{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{{
						Kind: sigil.PartKindToolResult,
						ToolResult: &sigil.ToolResult{
							ToolCallID: "tc_1",
							Content:    "file contents",
						},
					}},
				}},
			}},
			wantIDs: []string{"tc_1"},
		},
		{
			name: "multiple tool results across generations",
			gens: []sigil.Generation{
				{Input: []sigil.Message{{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{
						{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "tc_1", Content: "result1"}},
						{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "tc_2", Content: "result2"}},
					},
				}}},
				{Input: []sigil.Message{{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{
						{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "tc_3", Content: "result3", IsError: true}},
					},
				}}},
			},
			wantIDs:  []string{"tc_1", "tc_2", "tc_3"},
			wantErrs: map[string]bool{"tc_3": true},
		},
		{
			name: "skips parts without tool call ID",
			gens: []sigil.Generation{{
				Input: []sigil.Message{{
					Role: sigil.RoleTool,
					Parts: []sigil.Part{
						{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "", Content: "orphan"}},
						{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "tc_1", Content: "ok"}},
					},
				}},
			}},
			wantIDs: []string{"tc_1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := buildToolResultMap(tt.gens)

			if len(m) != len(tt.wantIDs) {
				t.Fatalf("got %d entries, want %d", len(m), len(tt.wantIDs))
			}
			for _, id := range tt.wantIDs {
				tr, ok := m[id]
				if !ok {
					t.Errorf("missing key %q", id)
					continue
				}
				if tt.wantErrs[id] && !tr.IsError {
					t.Errorf("key %q: IsError = false, want true", id)
				}
			}
		})
	}
}

func newSpanRecordingClient(t *testing.T, mode sigil.ContentCaptureMode) (*sigil.Client, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	client := sigil.NewClient(sigil.Config{
		Tracer:         tp.Tracer("test"),
		ContentCapture: mode,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client, recorder
}

func spansByName(spans []sdktrace.ReadOnlySpan, prefix string) []sdktrace.ReadOnlySpan {
	var matched []sdktrace.ReadOnlySpan
	for _, s := range spans {
		if len(s.Name()) >= len(prefix) && s.Name()[:len(prefix)] == prefix {
			matched = append(matched, s)
		}
	}
	return matched
}

func spanAttr(s sdktrace.ReadOnlySpan, key string) string {
	for _, a := range s.Attributes() {
		if string(a.Key) == key {
			return a.Value.AsString()
		}
	}
	return ""
}

func TestEmitToolSpans(t *testing.T) {
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name        string
		gen         sigil.Generation
		results     map[string]*sigil.ToolResult
		contentMode sigil.ContentCaptureMode
		wantSpans   int
		wantNames   []string
		wantArgs    map[string]string
		wantResults map[string]string
	}{
		{
			name: "no tool calls",
			gen: sigil.Generation{
				Output: []sigil.Message{{
					Role:  sigil.RoleAssistant,
					Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "hello"}},
				}},
				CompletedAt: ts,
			},
			wantSpans: 0,
		},
		{
			name: "single tool call metadata only",
			gen: sigil.Generation{
				ConversationID: "conv-1",
				AgentName:      "claude-code",
				AgentVersion:   "1.0.0",
				Model:          sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
				CompletedAt:    ts,
				Output: []sigil.Message{{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{{
						Kind: sigil.PartKindToolCall,
						ToolCall: &sigil.ToolCall{
							ID:        "tc_1",
							Name:      "Read",
							InputJSON: json.RawMessage(`{"path":"main.go"}`),
						},
					}},
				}},
			},
			contentMode: sigil.ContentCaptureModeMetadataOnly,
			wantSpans:   1,
			wantNames:   []string{"Read"},
		},
		{
			name: "multiple tool calls with full content and results",
			gen: sigil.Generation{
				ConversationID: "conv-1",
				AgentName:      "claude-code",
				Model:          sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
				CompletedAt:    ts,
				Output: []sigil.Message{{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{
						{Kind: sigil.PartKindText, Text: "Let me check."},
						{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{
							ID: "tc_1", Name: "Read", InputJSON: json.RawMessage(`{"path":"a.go"}`),
						}},
						{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{
							ID: "tc_2", Name: "Grep", InputJSON: json.RawMessage(`{"pattern":"TODO"}`),
						}},
					},
				}},
			},
			results: map[string]*sigil.ToolResult{
				"tc_1": {ToolCallID: "tc_1", Content: "package main"},
				"tc_2": {ToolCallID: "tc_2", Content: "found 3 matches"},
			},
			contentMode: sigil.ContentCaptureModeFull,
			wantSpans:   2,
			wantNames:   []string{"Read", "Grep"},
			wantArgs: map[string]string{
				"Read": `{"path":"a.go"}`,
				"Grep": `{"pattern":"TODO"}`,
			},
			wantResults: map[string]string{
				"Read": `"package main"`,
				"Grep": `"found 3 matches"`,
			},
		},
		{
			name: "tool result with ContentJSON",
			gen: sigil.Generation{
				CompletedAt: ts,
				Output: []sigil.Message{{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{{
						Kind: sigil.PartKindToolCall,
						ToolCall: &sigil.ToolCall{
							ID: "tc_1", Name: "Bash", InputJSON: json.RawMessage(`{"cmd":"ls"}`),
						},
					}},
				}},
			},
			results: map[string]*sigil.ToolResult{
				"tc_1": {ToolCallID: "tc_1", ContentJSON: json.RawMessage(`{"files":["a","b"]}`)},
			},
			contentMode: sigil.ContentCaptureModeFull,
			wantSpans:   1,
			wantNames:   []string{"Bash"},
			wantResults: map[string]string{
				"Bash": `{"files":["a","b"]}`,
			},
		},
		{
			name: "error tool result sets error status",
			gen: sigil.Generation{
				CompletedAt: ts,
				Output: []sigil.Message{{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{{
						Kind: sigil.PartKindToolCall,
						ToolCall: &sigil.ToolCall{
							ID: "tc_1", Name: "Write", InputJSON: json.RawMessage(`{}`),
						},
					}},
				}},
			},
			results: map[string]*sigil.ToolResult{
				"tc_1": {ToolCallID: "tc_1", Content: "permission denied", IsError: true},
			},
			contentMode: sigil.ContentCaptureModeMetadataOnly,
			wantSpans:   1,
			wantNames:   []string{"Write"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, recorder := newSpanRecordingClient(t, tt.contentMode)
			results := tt.results
			if results == nil {
				results = map[string]*sigil.ToolResult{}
			}

			emitToolSpans(context.Background(), client, tt.gen, results)
			_ = client.Shutdown(context.Background())

			toolSpans := spansByName(recorder.Ended(), "execute_tool")
			if len(toolSpans) != tt.wantSpans {
				t.Fatalf("got %d tool spans, want %d", len(toolSpans), tt.wantSpans)
			}

			for i, wantName := range tt.wantNames {
				s := toolSpans[i]
				gotName := spanAttr(s, "gen_ai.tool.name")
				if gotName != wantName {
					t.Errorf("span[%d] gen_ai.tool.name = %q, want %q", i, gotName, wantName)
				}
				if gotType := spanAttr(s, "gen_ai.tool.type"); gotType != "function" {
					t.Errorf("span[%d] gen_ai.tool.type = %q, want function", i, gotType)
				}
				if tt.wantArgs != nil {
					if want, ok := tt.wantArgs[wantName]; ok {
						got := spanAttr(s, "gen_ai.tool.call.arguments")
						if got != want {
							t.Errorf("span[%d] arguments = %q, want %q", i, got, want)
						}
					}
				}
				if tt.wantResults != nil {
					if want, ok := tt.wantResults[wantName]; ok {
						got := spanAttr(s, "gen_ai.tool.call.result")
						if got != want {
							t.Errorf("span[%d] result = %q, want %q", i, got, want)
						}
					}
				}
			}
		})
	}
}

func TestEmitToolSpans_ErrorStatus(t *testing.T) {
	client, recorder := newSpanRecordingClient(t, sigil.ContentCaptureModeMetadataOnly)
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	gen := sigil.Generation{
		CompletedAt: ts,
		Output: []sigil.Message{{
			Role: sigil.RoleAssistant,
			Parts: []sigil.Part{{
				Kind:     sigil.PartKindToolCall,
				ToolCall: &sigil.ToolCall{ID: "tc_err", Name: "Write"},
			}},
		}},
	}
	results := map[string]*sigil.ToolResult{
		"tc_err": {ToolCallID: "tc_err", Content: "denied", IsError: true},
	}

	emitToolSpans(context.Background(), client, gen, results)
	_ = client.Shutdown(context.Background())

	spans := spansByName(recorder.Ended(), "execute_tool")
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status code = %v, want Error", spans[0].Status().Code)
	}
}
