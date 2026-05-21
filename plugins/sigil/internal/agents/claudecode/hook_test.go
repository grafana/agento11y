package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
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

func TestHookEventRouting(t *testing.T) {
	tests := []struct {
		name            string
		setup           func(t *testing.T, dir string) hookInput
		wantLogs        []string
		wantMissingLogs []string
		assertState     func(t *testing.T)
	}{
		{
			name: "Stop keeps offset when no generations are produced",
			setup: func(t *testing.T, dir string) hookInput {
				t.Helper()
				sessionID := "zero-generation-session"
				transcriptPath := filepath.Join(dir, "transcript.jsonl")
				processedPart := buildHookUserJSONL(sessionID, "already processed") + "\n"
				userPart := buildHookUserJSONL(sessionID, "hey") + "\n"
				if err := os.WriteFile(transcriptPath, []byte(processedPart+userPart), 0o644); err != nil {
					t.Fatal(err)
				}
				startOffset := int64(len([]byte(processedPart)))
				if err := state.Save(sessionID, state.Session{Offset: startOffset, Title: "existing title", Model: "claude-sonnet-4"}); err != nil {
					t.Fatal(err)
				}
				return hookInput{
					HookEventName:  "Stop",
					SessionID:      sessionID,
					TranscriptPath: transcriptPath,
				}
			},
			wantLogs: []string{"no completed assistant turn yet; keeping offset="},
			assertState: func(t *testing.T) {
				t.Helper()
				sessionID := "zero-generation-session"
				wantOffset := int64(len([]byte(buildHookUserJSONL(sessionID, "already processed") + "\n")))
				st := state.Load(sessionID)
				if st.Offset != wantOffset {
					t.Fatalf("Offset = %d, want %d", st.Offset, wantOffset)
				}
				if st.Title != "existing title" {
					t.Fatalf("Title = %q, want existing title", st.Title)
				}
			},
		},
		{
			name: "SessionEnd processes transcript",
			setup: func(t *testing.T, dir string) hookInput {
				t.Helper()
				sessionID := "sessionend-route"
				transcriptPath := filepath.Join(dir, "transcript.jsonl")
				userLine := buildHookUserJSONL(sessionID, "hey")
				if err := os.WriteFile(transcriptPath, []byte(userLine+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				return hookInput{
					HookEventName:  "SessionEnd",
					SessionID:      sessionID,
					TranscriptPath: transcriptPath,
				}
			},
			wantLogs: []string{
				"read 1 raw lines",
				"no completed assistant turn yet; keeping offset=0",
			},
		},
		{
			name: "SessionStart does not process transcript",
			setup: func(t *testing.T, dir string) hookInput {
				t.Helper()
				return hookInput{
					HookEventName:  "SessionStart",
					SessionID:      "sessionstart-route",
					TranscriptPath: filepath.Join(dir, "missing.jsonl"),
					Model:          "claude-opus-4-20250514",
				}
			},
			wantMissingLogs: []string{"read transcript:", "raw lines"},
			assertState: func(t *testing.T) {
				t.Helper()
				st := state.Load("sessionstart-route")
				if st.Model != "claude-opus-4-20250514" {
					t.Fatalf("Model = %q, want claude-opus-4-20250514", st.Model)
				}
			},
		},
		{
			name: "unknown event does not process transcript",
			setup: func(t *testing.T, dir string) hookInput {
				t.Helper()
				return hookInput{
					HookEventName:  "UnknownEvent",
					SessionID:      "unknown-route",
					TranscriptPath: filepath.Join(dir, "missing.jsonl"),
				}
			},
			wantMissingLogs: []string{"read transcript:", "raw lines"},
			assertState: func(t *testing.T) {
				t.Helper()
				st := state.Load("unknown-route")
				if st != (state.Session{}) {
					t.Fatalf("state = %#v, want zero value", st)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
			setHookExportEnv(t, "http://example.invalid")

			logs := runHookForTest(t, tt.setup(t, dir))
			for _, want := range tt.wantLogs {
				if !strings.Contains(logs, want) {
					t.Fatalf("logs do not contain %q:\n%s", want, logs)
				}
			}
			for _, missing := range tt.wantMissingLogs {
				if strings.Contains(logs, missing) {
					t.Fatalf("logs contain unexpected %q:\n%s", missing, logs)
				}
			}
			if tt.assertState != nil {
				tt.assertState(t)
			}
		})
	}
}

func TestHandlePreToolUse(t *testing.T) {
	var calls atomic.Int32
	var responseBody atomic.Value
	responseBody.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := responseBody.Load().(string)
		if body == "" {
			body = `{"action":"allow"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closed.Close()

	tests := []struct {
		name               string
		env                map[string]string
		useClosedEndpoint  bool
		serverResponds     string
		expectServerCall   bool
		wantStdoutContains string
		wantStdoutEmpty    bool
	}{
		{
			name:            "disabled_by_default_no_env",
			wantStdoutEmpty: true,
		},
		{
			name:            "disabled_explicit_false",
			env:             map[string]string{"SIGIL_GUARDS_ENABLED": "false"},
			wantStdoutEmpty: true,
		},
		{
			name:             "enabled_allow_response",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow"}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
		{
			name:               "enabled_deny_response",
			env:                map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:     `{"action":"deny","reason":"blocked tool"}`,
			expectServerCall:   true,
			wantStdoutContains: `"permissionDecision":"deny"`,
		},
		{
			name:              "enabled_fail_open_on_transport_error",
			env:               map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			useClosedEndpoint: true,
			wantStdoutEmpty:   true,
		},
		{
			name: "enabled_fail_closed_on_transport_error",
			env: map[string]string{
				"SIGIL_GUARDS_ENABLED":   "true",
				"SIGIL_GUARDS_FAIL_OPEN": "false",
			},
			useClosedEndpoint:  true,
			wantStdoutContains: `"permissionDecision":"deny"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			calls.Store(0)
			responseBody.Store(tt.serverResponds)

			endpoint := server.URL
			if tt.useClosedEndpoint {
				endpoint = closed.URL
			}

			var stdout bytes.Buffer
			var logs bytes.Buffer
			input := &hookInput{
				HookEventName:  "PreToolUse",
				SessionID:      "s1",
				TranscriptPath: "/tmp/t.jsonl",
				ToolName:       "Bash",
				ToolInput:      json.RawMessage(`{"command":"echo hi"}`),
				ToolUseID:      "tu_1",
			}
			st := state.Session{Model: "claude-sonnet-4"}

			handlePreToolUse(context.Background(), &stdout, input, st, endpoint, "tenant", "token", log.New(&logs, "", 0))

			if tt.expectServerCall && calls.Load() == 0 {
				t.Errorf("expected server call, got 0")
			}
			if !tt.expectServerCall && !tt.useClosedEndpoint && calls.Load() != 0 {
				t.Errorf("expected no server call, got %d", calls.Load())
			}
			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			if tt.wantStdoutContains != "" && !strings.Contains(stdout.String(), tt.wantStdoutContains) {
				t.Errorf("stdout = %q, want substring %q", stdout.String(), tt.wantStdoutContains)
			}
		})
	}
}

func setHookExportEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("SIGIL_ENDPOINT", endpoint)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("SIGIL_AUTH_MODE", "basic")
	t.Setenv("SIGIL_PROTOCOL", "http")
	t.Setenv("SIGIL_USER_ID", "test-user")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
}

func runHookForTest(t *testing.T, input hookInput) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	if err := Hook(context.Background(), bytes.NewReader(payload), io.Discard, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	return logs.String()
}

func buildHookUserJSONL(sessionID, text string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:00:00Z",
		"version":   "1.0.0",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
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
