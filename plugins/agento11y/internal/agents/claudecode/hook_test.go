package claudecode

import (
	"bytes"
	"context"
	"encoding/base64"
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

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestMain(m *testing.M) {
	// The transcript flush-race wait is unnecessary against the synthetic,
	// fully-written transcripts these tests use; zero it so incomplete
	// fixtures return immediately instead of blocking the settle window.
	transcriptSettleWindow = 0
	os.Exit(m.Run())
}

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
			name: "UserPromptSubmit does not process transcript",
			setup: func(t *testing.T, dir string) hookInput {
				t.Helper()
				return hookInput{
					HookEventName:  "UserPromptSubmit",
					SessionID:      "prompt-route",
					TranscriptPath: filepath.Join(dir, "missing.jsonl"),
					Prompt:         "hello",
				}
			},
			wantMissingLogs: []string{"read transcript:", "raw lines"},
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
				if st.Offset != 0 || st.Title != "" || st.AITitle != "" || st.CustomTitle != "" || st.Model != "" || len(st.SubagentOffsets) != 0 {
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

// TestHandlePreToolUse covers Claude-Code-specific wiring around the shared
// guard helper: ensuring the helper is consulted only when guards are
// enabled, that allow verdicts produce empty stdout, that deny verdicts
// produce the Claude Code PreToolUse envelope (hookSpecificOutput +
// hookEventName=PreToolUse), and that Transform verdicts produce the
// allow+updatedInput envelope. Deep behaviour around fail-open/closed,
// missing credentials, transport errors, and transform extraction lives in
// the guard package tests; this test only verifies the integration shape.
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

	tests := []struct {
		name               string
		env                map[string]string
		serverResponds     string
		expectServerCall   bool
		wantStdoutContains []string
		wantStdoutEmpty    bool
	}{
		{
			name:            "disabled_by_default",
			wantStdoutEmpty: true,
		},
		{
			name:             "enabled_allow",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow"}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
		{
			name:             "enabled_deny_writes_claude_envelope",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"deny","reason":"blocked tool"}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"hookSpecificOutput"`,
				`"hookEventName":"PreToolUse"`,
				`"permissionDecision":"deny"`,
				`A Grafana Agent Observability policy`,
				`blocked tool`,
			},
		},
		{
			name:             "enabled_allow_transform_writes_updated_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"command":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"hookSpecificOutput"`,
				`"hookEventName":"PreToolUse"`,
				`"permissionDecision":"allow"`,
				`"updatedInput":{"command":"echo [REDACTED]"}`,
			},
		},
		{
			name:             "enabled_allow_mismatched_id_keeps_original_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"Bash","input_json":{"command":"echo X"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Blank every branded variable first: an ambient endpoint would send
			// this guard call to a real backend.
			envconfig.PinAliasEnvBlank(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			calls.Store(0)
			responseBody.Store(tt.serverResponds)

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

			handlePreToolUse(context.Background(), &stdout, input, st, AgentName, log.New(&logs, "", 0))

			if tt.expectServerCall && calls.Load() == 0 {
				t.Errorf("expected server call, got 0")
			}
			if !tt.expectServerCall && calls.Load() != 0 {
				t.Errorf("expected no server call, got %d", calls.Load())
			}
			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q\nfull output: %s", want, stdout.String())
				}
			}
		})
	}
}

func TestHandleUserPromptSubmit(t *testing.T) {
	var calls atomic.Int32
	var responseBody atomic.Value
	responseBody.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		body, _ := responseBody.Load().(string)
		if body == "" {
			body = `{"action":"allow"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	tests := []struct {
		name               string
		env                map[string]string
		serverResponds     string
		expectServerCall   bool
		wantStdoutContains []string
		wantStdoutEmpty    bool
	}{
		{
			name:            "disabled_by_default",
			wantStdoutEmpty: true,
		},
		{
			name:             "enabled_allow",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow"}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
		{
			name:             "enabled_deny_writes_block_envelope",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"deny","reason":"secret in prompt"}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"decision":"block"`,
				`A Grafana Agent Observability policy`,
				`secret in prompt`,
			},
		},
		{
			// Claude Code cannot apply a prompt transform.
			name:             "enabled_allow_transform_stays_silent",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"messages":[{"role":"user","parts":[{"kind":"text","text":"[REDACTED]"}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			calls.Store(0)
			responseBody.Store(tt.serverResponds)

			var stdout bytes.Buffer
			var logs bytes.Buffer
			input := &hookInput{
				HookEventName:  "UserPromptSubmit",
				SessionID:      "s1",
				TranscriptPath: "/tmp/t.jsonl",
				Prompt:         "my token is glc_secret",
			}
			st := state.Session{Model: "claude-sonnet-4"}

			handleUserPromptSubmit(context.Background(), &stdout, input, st, AgentName, log.New(&logs, "", 0))

			if tt.expectServerCall && calls.Load() == 0 {
				t.Errorf("expected server call, got 0")
			}
			if !tt.expectServerCall && calls.Load() != 0 {
				t.Errorf("expected no server call, got %d", calls.Load())
			}
			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q\nfull output: %s", want, stdout.String())
				}
			}
		})
	}
}

// Hook must return nil because a non-zero exit runs the `sigil` fallback and
// sends the prompt.
func TestHook_UserPromptSubmitDenyReturnsNil(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"deny","reason":"secret in prompt"}`))
	}))
	defer server.Close()

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	stdin := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"s1","transcript_path":"/tmp/t.jsonl","prompt":"my token is glc_secret"}`)
	var stdout bytes.Buffer
	if err := Hook(context.Background(), stdin, &stdout, log.New(io.Discard, "", 0)); err != nil {
		t.Fatalf("Hook returned %v; a non-zero exit would run the sigil fallback", err)
	}
	if !strings.Contains(stdout.String(), `"decision":"block"`) {
		t.Errorf("stdout = %q, want a block envelope", stdout.String())
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

// hookFixtureTimestamp is the timestamp every fixture line shares unless a
// test needs distinct times to assert span windows.
const hookFixtureTimestamp = "2025-06-01T12:00:00Z"

func buildHookUserJSONL(sessionID, text string) string {
	return buildHookUserJSONLAt(sessionID, text, hookFixtureTimestamp)
}

func buildHookUserJSONLAt(sessionID, text, ts string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": ts,
		"version":   "1.0.0",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func buildHookAssistantJSONL(sessionID, requestID, stopReason, text string, outputTokens int64) string {
	return buildHookAssistantJSONLAt(sessionID, requestID, stopReason, text, outputTokens, hookFixtureTimestamp)
}

func buildHookAssistantJSONLAt(sessionID, requestID, stopReason, text string, outputTokens int64, ts string) string {
	return buildHookAssistantLineAt(sessionID, requestID, stopReason, ts,
		[]any{map[string]any{"type": "text", "text": text}}, outputTokens)
}

// buildHookAssistantToolUseJSONLAt builds an assistant turn that calls one tool.
func buildHookAssistantToolUseJSONLAt(sessionID, requestID, toolUseID, toolName, ts string) string {
	return buildHookAssistantLineAt(sessionID, requestID, "tool_use", ts, []any{map[string]any{
		"type":  "tool_use",
		"id":    toolUseID,
		"name":  toolName,
		"input": map[string]any{"command": "ls"},
	}}, 7)
}

func buildHookAssistantLineAt(sessionID, requestID, stopReason, ts string, content []any, outputTokens int64) string {
	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": ts,
		"version":   "1.0.0",
		"requestId": requestID,
		"message": map[string]any{
			"model":       "claude-opus-4",
			"stop_reason": stopReason,
			"usage":       map[string]any{"input_tokens": 3, "output_tokens": outputTokens},
			"content":     content,
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func buildHookToolResultJSONL(sessionID, toolUseID, content string) string {
	return buildHookToolResultJSONLAt(sessionID, toolUseID, content, hookFixtureTimestamp)
}

func buildHookToolResultJSONLAt(sessionID, toolUseID, content, ts string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": ts,
		"version":   "1.0.0",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": toolUseID,
				"content":     content,
			}},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

// TestReadTranscriptSettled_CapturesLateFinalTurn reproduces the Claude Code
// flush race: the Stop hook reads a transcript whose tail is still a dangling
// tool_result, then the closing assistant turn lands on disk a moment later.
// Without the settle wait the final turn would be dropped (and never recovered,
// since export only happens on Stop/SessionEnd).
func TestReadTranscriptSettled_CapturesLateFinalTurn(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 2 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	sessionID := "late-final-turn"

	// Initial on-disk state at Stop time: the tool-use turn is complete, its
	// result has landed, but the closing assistant turn has not been flushed.
	initial := buildHookAssistantJSONL(sessionID, "req_a", "tool_use", "calling tool", 10) + "\n" +
		buildHookToolResultJSONL(sessionID, "tu_1", "tool ok") + "\n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// Append the closing assistant turn shortly after the first read.
	go func() {
		time.Sleep(150 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString(buildHookAssistantJSONL(sessionID, "req_b", "end_turn", "all done", 5) + "\n")
	}()

	logs := log.New(io.Discard, "", 0)
	lines, safeOffset, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, false, logs)

	if rawCount != 3 {
		t.Fatalf("rawCount = %d, want 3 (tool-use turn, tool_result, final turn)", rawCount)
	}
	last := lines[len(lines)-1]
	if last.RequestID != "req_b" {
		t.Fatalf("last coalesced line RequestID = %q, want req_b (the late final turn)", last.RequestID)
	}
	if safeOffset != last.EndOffset {
		t.Fatalf("safeOffset = %d, want %d (end of final turn)", safeOffset, last.EndOffset)
	}
}

// TestReadTranscriptSettled_StopWaitsForTranscriptCreatedLate reproduces a
// headless `claude -p` run whose Stop fires before Claude Code has created the
// transcript: the read waits for the file instead of failing on it.
func TestReadTranscriptSettled_StopWaitsForTranscriptCreatedLate(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 2 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(path, []byte(buildHookAssistantJSONL("late-file", "req_a", "end_turn", "done", 5)+"\n"), 0o644)
	}()

	lines, _, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, true, log.New(io.Discard, "", 0))
	if rawCount != 1 || len(lines) != 1 {
		t.Fatalf("rawCount=%d len(lines)=%d, want 1/1 (the turn written after Stop)", rawCount, len(lines))
	}
}

// TestReadTranscriptSettled_StopWaitsPastToolUseTail reproduces the same race
// one step later: at Stop the transcript ends at the turn that called a tool,
// complete but not the end of the turn, and the tool result and closing reply
// land a moment later.
func TestReadTranscriptSettled_StopWaitsPastToolUseTail(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 2 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	sessionID := "tool-use-tail"
	if err := os.WriteFile(path, []byte(buildHookAssistantJSONL(sessionID, "req_a", "tool_use", "calling tool", 10)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(150 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString(buildHookToolResultJSONL(sessionID, "tu_1", "tool ok") + "\n" +
			buildHookAssistantJSONL(sessionID, "req_b", "end_turn", "all done", 5) + "\n")
	}()

	lines, _, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, true, log.New(io.Discard, "", 0))
	if rawCount != 3 {
		t.Fatalf("rawCount = %d, want 3 (tool-use turn, tool_result, final turn)", rawCount)
	}
	if last := lines[len(lines)-1]; last.RequestID != "req_b" {
		t.Fatalf("last coalesced line RequestID = %q, want req_b (the closing turn)", last.RequestID)
	}
}

// TestHook_OnlyStopWaitsForMissingTranscript pins which event waits for a
// transcript Claude Code has not created yet. A Stop always follows a turn, so
// it waits out the settle window. A SessionEnd can close a session that never
// wrote a transcript, so it returns at once instead of delaying the exit.
func TestHook_OnlyStopWaitsForMissingTranscript(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })
	t.Setenv("SIGIL_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	missing := filepath.Join(t.TempDir(), "never-written.jsonl")
	for _, tc := range []struct {
		event    string
		wantWait bool
	}{{"Stop", true}, {"SessionEnd", false}} {
		start := time.Now()
		runHookForTest(t, hookInput{HookEventName: tc.event, SessionID: "missing-transcript", TranscriptPath: missing})
		if waited := time.Since(start) >= transcriptSettleWindow; waited != tc.wantWait {
			t.Errorf("%s waited out the settle window = %v, want %v", tc.event, waited, tc.wantWait)
		}
	}
}

// TestReadTranscriptSettled_ReturnsImmediatelyWhenTerminal confirms the common
// case adds no latency: when the tail is already a complete assistant turn the
// function returns on the first read without waiting out the settle window.
func TestReadTranscriptSettled_ReturnsImmediatelyWhenTerminal(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 5 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	sessionID := "terminal-tail"
	content := buildHookAssistantJSONL(sessionID, "req_a", "end_turn", "done", 5) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	lines, safeOffset, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, true, log.New(io.Discard, "", 0))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("settled read took %s; expected near-immediate return", elapsed)
	}
	if rawCount != 1 || len(lines) != 1 {
		t.Fatalf("rawCount=%d len(lines)=%d, want 1/1", rawCount, len(lines))
	}
	if safeOffset != lines[0].EndOffset {
		t.Fatalf("safeOffset = %d, want %d", safeOffset, lines[0].EndOffset)
	}
}

// TestReadTranscriptSettled_EmptyReadReturnsImmediately guards against blocking
// the hook for the full settle window on a redundant Stop/SessionEnd: when the
// read at the saved offset yields nothing (EOF / already exported), there is no
// pending fragment to wait for, so the function must return at once.
func TestReadTranscriptSettled_EmptyReadReturnsImmediately(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 5 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	sessionID := "already-exported"
	content := buildHookAssistantJSONL(sessionID, "req_a", "end_turn", "done", 5) + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	// Read from end-of-file: a prior export already consumed everything.
	eof := int64(len(content))
	start := time.Now()
	lines, safeOffset, rawCount, _ := readTranscriptSettled(context.Background(), path, eof, false, log.New(io.Discard, "", 0))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("empty read took %s; expected immediate return without waiting out the settle window", elapsed)
	}
	if rawCount != 0 || len(lines) != 0 || safeOffset != 0 {
		t.Fatalf("rawCount=%d len(lines)=%d safeOffset=%d, want 0/0/0", rawCount, len(lines), safeOffset)
	}
}

// TestReadTranscriptSettled_TrailingPromptAfterCompleteTurn guards against
// blocking the settle window on a Stop whose tail is an already-flushed next
// user prompt sitting after a complete assistant turn. The completed turn is
// what the event reports; the prompt belongs to a future turn, so the read must
// settle at once (returning the complete turn, leaving the prompt for later).
func TestReadTranscriptSettled_TrailingPromptAfterCompleteTurn(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 5 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	sessionID := "trailing-prompt"
	complete := buildHookAssistantJSONL(sessionID, "req_a", "end_turn", "done", 5) + "\n"
	content := complete + buildHookUserJSONL(sessionID, "next question") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	lines, safeOffset, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, false, log.New(io.Discard, "", 0))
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("trailing-prompt read took %s; expected immediate return (completed turn already present)", elapsed)
	}
	if rawCount != 2 {
		t.Fatalf("rawCount = %d, want 2 (assistant turn + trailing prompt)", rawCount)
	}
	// safeOffset must stop at the end of the completed assistant turn, leaving
	// the trailing prompt to be consumed by a later event.
	wantSafe := int64(len(complete))
	if safeOffset != wantSafe {
		t.Fatalf("safeOffset = %d, want %d (end of completed assistant turn)", safeOffset, wantSafe)
	}
	if lines[len(lines)-1].RequestID != "req_a" {
		t.Fatalf("last coalesced line = %q, want req_a", lines[len(lines)-1].RequestID)
	}
}

// TestReadTranscriptSettled_LonePromptWaitsForReply confirms the symmetric race
// is still covered: a tool-free final turn whose only flushed line is the user
// prompt (no completed assistant turn yet) must still poll so the assistant
// reply is captured when it lands.
func TestReadTranscriptSettled_LonePromptWaitsForReply(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 2 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	sessionID := "lone-prompt"
	if err := os.WriteFile(path, []byte(buildHookUserJSONL(sessionID, "hi there")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString(buildHookAssistantJSONL(sessionID, "req_a", "end_turn", "hello", 4) + "\n")
	}()

	lines, safeOffset, rawCount, _ := readTranscriptSettled(context.Background(), path, 0, false, log.New(io.Discard, "", 0))
	if rawCount != 2 {
		t.Fatalf("rawCount = %d, want 2 (prompt + late assistant reply)", rawCount)
	}
	last := lines[len(lines)-1]
	if last.RequestID != "req_a" {
		t.Fatalf("last coalesced line = %q, want req_a (the late reply)", last.RequestID)
	}
	if safeOffset != last.EndOffset {
		t.Fatalf("safeOffset = %d, want %d (end of late reply)", safeOffset, last.EndOffset)
	}
}

func TestBuildToolResultMap(t *testing.T) {
	tests := []struct {
		name     string
		gens     []agento11y.Generation
		wantIDs  []string
		wantErrs map[string]bool
	}{
		{name: "empty generations", gens: nil, wantIDs: nil},
		{
			name: "no tool results in input",
			gens: []agento11y.Generation{{
				Input: []agento11y.Message{{
					Role:  agento11y.RoleUser,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "hello"}},
				}},
			}},
			wantIDs: nil,
		},
		{
			name: "single tool result",
			gens: []agento11y.Generation{{
				Input: []agento11y.Message{{
					Role: agento11y.RoleTool,
					Parts: []agento11y.Part{{
						Kind: agento11y.PartKindToolResult,
						ToolResult: &agento11y.ToolResult{
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
			gens: []agento11y.Generation{
				{Input: []agento11y.Message{{
					Role: agento11y.RoleTool,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "tc_1", Content: "result1"}},
						{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "tc_2", Content: "result2"}},
					},
				}}},
				{Input: []agento11y.Message{{
					Role: agento11y.RoleTool,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "tc_3", Content: "result3", IsError: true}},
					},
				}}},
			},
			wantIDs:  []string{"tc_1", "tc_2", "tc_3"},
			wantErrs: map[string]bool{"tc_3": true},
		},
		{
			name: "skips parts without tool call ID",
			gens: []agento11y.Generation{{
				Input: []agento11y.Message{{
					Role: agento11y.RoleTool,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "", Content: "orphan"}},
						{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "tc_1", Content: "ok"}},
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

func newSpanRecordingClient(t *testing.T, mode agento11y.ContentCaptureMode) (*agento11y.Client, *tracetest.SpanRecorder) {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	client := agento11y.NewClient(agento11y.Config{
		Tracer:         tp.Tracer("test"),
		ContentCapture: mode,
		// Spans are the subject here; keep the export transport out of it so
		// shutdown does not try to reach a real endpoint.
		GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone},
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

// TestGenerationStart_CarriesStartedAt pins the field the span window depends
// on: the projection must carry the mapped StartedAt, not drop it.
func TestGenerationStart_CarriesStartedAt(t *testing.T) {
	completed := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		gen  agento11y.Generation
	}{
		{
			name: "normal turn",
			gen: agento11y.Generation{
				ID:             "gen-1",
				ConversationID: "sess-1",
				AgentName:      AgentName,
				StartedAt:      completed.Add(-2 * time.Second),
				CompletedAt:    completed,
			},
		},
		{
			name: "zero start",
			gen: agento11y.Generation{
				ID:          "gen-2",
				AgentName:   AgentName,
				CompletedAt: completed,
			},
		},
		{
			name: "synthesized subagent",
			gen: agento11y.Generation{
				ID:                  "gen-3",
				AgentName:           AgentName + "/explorer",
				ParentGenerationIDs: []string{"gen-1"},
				StartedAt:           completed,
				CompletedAt:         completed.Add(1200 * time.Millisecond),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := generationStart(tt.gen)
			if !got.StartedAt.Equal(tt.gen.StartedAt) {
				t.Errorf("StartedAt = %v, want %v", got.StartedAt, tt.gen.StartedAt)
			}
			if got.ID != tt.gen.ID || got.AgentName != tt.gen.AgentName {
				t.Errorf("identity fields not carried: %+v", got)
			}
		})
	}
}

// TestRecord_GenerationSpanWindowNotInverted pins the reported symptom: a Stop
// hook replays a turn that finished 30 seconds earlier, so a span start taken
// from wall clock falls after the span end.
func TestRecord_GenerationSpanWindowNotInverted(t *testing.T) {
	client, recorder := newSpanRecordingClient(t, agento11y.ContentCaptureModeMetadataOnly)

	completed := time.Now().Add(-30 * time.Second)
	gen := agento11y.Generation{
		ID:             "gen-late-export",
		ConversationID: "sess-1",
		AgentName:      AgentName,
		Mode:           agento11y.GenerationModeSync,
		OperationName:  "generateText",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
		StartedAt:      completed.Add(-2 * time.Second),
		CompletedAt:    completed,
	}

	if err := emit.Record(context.Background(), client, generationStart(gen), gen, nil, nil); err != nil {
		t.Fatalf("emit.Record: %v", err)
	}
	_ = client.Shutdown(context.Background())

	spans := spansByName(recorder.Ended(), "generateText")
	if len(spans) != 1 {
		t.Fatalf("got %d generateText spans, want 1", len(spans))
	}
	got := spans[0].EndTime().Sub(spans[0].StartTime())
	if got < 0 {
		t.Fatalf("span duration = %v, want non-negative (start %v after end %v)",
			got, spans[0].StartTime(), spans[0].EndTime())
	}
	if got != 2*time.Second {
		t.Errorf("span duration = %v, want 2s from the mapped window", got)
	}
}

// TestHook_SpanWindowsFromTranscript runs Hook itself over a transcript with
// distinct timestamps and asserts the recorded span windows. It covers the
// wiring the helper-level tests cannot: re-inlining the GenerationStart literal
// in Hook, or dropping the tool-result timestamps on the way to emitToolSpans,
// fails here.
func TestHook_SpanWindowsFromTranscript(t *testing.T) {
	const (
		sessionID = "span-window-session"
		promptAt  = "2025-06-01T12:00:00Z"
		callAt    = "2025-06-01T12:00:01Z"
		resultAt  = "2025-06-01T12:00:02.2Z"
		replyAt   = "2025-06-01T12:00:03Z"
	)

	jsonl := strings.Join([]string{
		buildHookUserJSONLAt(sessionID, "list the files", promptAt),
		buildHookAssistantToolUseJSONLAt(sessionID, "req_a", "tu_1", "Bash", callAt),
		buildHookToolResultJSONLAt(sessionID, "tu_1", "a.go\nb.go", resultAt),
		buildHookAssistantJSONLAt(sessionID, "req_b", "end_turn", "two files", 5, replyAt),
	}, "\n") + "\n"

	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(jsonl), 0o644); err != nil {
		t.Fatal(err)
	}

	recorder := installGlobalSpanRecorder(t)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	// AGENTO11Y_* wins over SIGIL_*, so pin both spellings before pointing the
	// export at the stub: an ambient endpoint would send this fixture to a real
	// backend.
	envconfig.PinAliasEnvBlank(t)
	server := httptest.NewServer(http.HandlerFunc(acceptGenerationExport))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	logs := runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: path,
	})
	if !strings.Contains(logs, "produced 2 generations") {
		t.Fatalf("hook did not produce two generations:\n%s", logs)
	}

	genSpans := spansByName(recorder.Ended(), "generateText")
	if len(genSpans) != 2 {
		t.Fatalf("got %d generateText spans, want 2", len(genSpans))
	}
	// The tool-use turn runs from the prompt to its own completion, the closing
	// turn from the tool result to its completion.
	wantGenDurations := []time.Duration{time.Second, 800 * time.Millisecond}
	for i, want := range wantGenDurations {
		if got := genSpans[i].EndTime().Sub(genSpans[i].StartTime()); got != want {
			t.Errorf("generateText span[%d] duration = %v, want %v", i, got, want)
		}
	}

	toolSpans := spansByName(recorder.Ended(), "execute_tool")
	if len(toolSpans) != 1 {
		t.Fatalf("got %d execute_tool spans, want 1", len(toolSpans))
	}
	if got := toolSpans[0].EndTime().Sub(toolSpans[0].StartTime()); got != 1200*time.Millisecond {
		t.Errorf("execute_tool span duration = %v, want 1.2s (call to tool_result)", got)
	}
}

// installGlobalSpanRecorder records the spans Hook produces. Hook builds its own
// client and only sets a tracer when an OTLP endpoint is configured, so with the
// endpoint blank the SDK falls back to the global provider.
func installGlobalSpanRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previous := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(previous)
		_ = tp.Shutdown(context.Background())
	})
	return recorder
}

// acceptGenerationExport accepts every generation in the request so the SDK's
// cardinality check passes and the exporter does not retry.
func acceptGenerationExport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Generations []struct {
			ID string `json:"id"`
		} `json:"generations"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	resp := struct {
		Results []map[string]any `json:"results"`
	}{Results: make([]map[string]any, 0, len(req.Generations))}
	for _, g := range req.Generations {
		resp.Results = append(resp.Results, map[string]any{"generation_id": g.ID, "accepted": true})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func TestEmitToolSpans(t *testing.T) {
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name          string
		gen           agento11y.Generation
		results       map[string]*agento11y.ToolResult
		resultAt      map[string]time.Time
		contentMode   agento11y.ContentCaptureMode
		wantSpans     int
		wantNames     []string
		wantArgs      map[string]string
		wantResults   map[string]string
		wantStarts    map[string]time.Time
		wantDurations map[string]time.Duration
	}{
		{
			name: "no tool calls",
			gen: agento11y.Generation{
				Output: []agento11y.Message{{
					Role:  agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "hello"}},
				}},
				CompletedAt: ts,
			},
			wantSpans: 0,
		},
		{
			name: "single tool call metadata only",
			gen: agento11y.Generation{
				ConversationID: "conv-1",
				AgentName:      "claude-code",
				AgentVersion:   "1.0.0",
				Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
				CompletedAt:    ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind: agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{
							ID:        "tc_1",
							Name:      "Read",
							InputJSON: json.RawMessage(`{"path":"main.go"}`),
						},
					}},
				}},
			},
			contentMode:   agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:     1,
			wantNames:     []string{"Read"},
			wantDurations: map[string]time.Duration{"Read": 0},
		},
		{
			name: "multiple tool calls with full content and results",
			gen: agento11y.Generation{
				ConversationID: "conv-1",
				AgentName:      "claude-code",
				Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-20250514"},
				CompletedAt:    ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindText, Text: "Let me check."},
						{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{
							ID: "tc_1", Name: "Read", InputJSON: json.RawMessage(`{"path":"a.go"}`),
						}},
						{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{
							ID: "tc_2", Name: "Grep", InputJSON: json.RawMessage(`{"pattern":"TODO"}`),
						}},
					},
				}},
			},
			results: map[string]*agento11y.ToolResult{
				"tc_1": {ToolCallID: "tc_1", Content: "package main"},
				"tc_2": {ToolCallID: "tc_2", Content: "found 3 matches"},
			},
			contentMode: agento11y.ContentCaptureModeFull,
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
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind: agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{
							ID: "tc_1", Name: "Bash", InputJSON: json.RawMessage(`{"cmd":"ls"}`),
						},
					}},
				}},
			},
			results: map[string]*agento11y.ToolResult{
				"tc_1": {ToolCallID: "tc_1", ContentJSON: json.RawMessage(`{"files":["a","b"]}`)},
			},
			contentMode: agento11y.ContentCaptureModeFull,
			wantSpans:   1,
			wantNames:   []string{"Bash"},
			wantResults: map[string]string{
				"Bash": `{"files":["a","b"]}`,
			},
		},
		{
			name: "error tool result sets error status",
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind: agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{
							ID: "tc_1", Name: "Write", InputJSON: json.RawMessage(`{}`),
						},
					}},
				}},
			},
			results: map[string]*agento11y.ToolResult{
				"tc_1": {ToolCallID: "tc_1", Content: "permission denied", IsError: true},
			},
			contentMode: agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:   1,
			wantNames:   []string{"Write"},
		},
		{
			name: "tool result timestamp drives duration",
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind:     agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{ID: "tc_1", Name: "Bash"},
					}},
				}},
			},
			resultAt:      map[string]time.Time{"tc_1": ts.Add(1200 * time.Millisecond)},
			contentMode:   agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:     1,
			wantNames:     []string{"Bash"},
			wantDurations: map[string]time.Duration{"Bash": 1200 * time.Millisecond},
		},
		{
			name: "zero result timestamp collapses to zero width",
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind:     agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{ID: "tc_1", Name: "Bash"},
					}},
				}},
			},
			// The mapper drops unparseable timestamps rather than storing the
			// zero value, so this pins the window against a caller that does not.
			resultAt:      map[string]time.Time{"tc_1": {}},
			contentMode:   agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:     1,
			wantNames:     []string{"Bash"},
			wantDurations: map[string]time.Duration{"Bash": 0},
		},
		{
			// A zero generation completion is the case that used to invert the
			// span: the SDK stamps the start with the current time, which is
			// after every transcript timestamp. Both ends move to the result.
			name: "zero generation completion anchors on the result",
			gen: agento11y.Generation{
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind:     agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{ID: "tc_1", Name: "Bash"},
					}},
				}},
			},
			resultAt:      map[string]time.Time{"tc_1": ts},
			contentMode:   agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:     1,
			wantNames:     []string{"Bash"},
			wantStarts:    map[string]time.Time{"Bash": ts},
			wantDurations: map[string]time.Duration{"Bash": 0},
		},
		{
			name: "result before the call clamps to zero width",
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{
						Kind:     agento11y.PartKindToolCall,
						ToolCall: &agento11y.ToolCall{ID: "tc_1", Name: "Bash"},
					}},
				}},
			},
			resultAt:      map[string]time.Time{"tc_1": ts.Add(-time.Second)},
			contentMode:   agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:     1,
			wantNames:     []string{"Bash"},
			wantDurations: map[string]time.Duration{"Bash": 0},
		},
		{
			name: "parallel calls keep distinct ends",
			gen: agento11y.Generation{
				CompletedAt: ts,
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "tc_1", Name: "Read"}},
						{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "tc_2", Name: "Bash"}},
					},
				}},
			},
			resultAt: map[string]time.Time{
				"tc_1": ts.Add(500 * time.Millisecond),
				"tc_2": ts.Add(1200 * time.Millisecond),
			},
			contentMode: agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:   2,
			wantNames:   []string{"Read", "Bash"},
			wantDurations: map[string]time.Duration{
				"Read": 500 * time.Millisecond,
				"Bash": 1200 * time.Millisecond,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, recorder := newSpanRecordingClient(t, tt.contentMode)
			results := tt.results
			if results == nil {
				results = map[string]*agento11y.ToolResult{}
			}

			emitToolSpans(context.Background(), client, tt.gen, results, tt.resultAt)
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
				if want, ok := tt.wantStarts[wantName]; ok {
					if got := s.StartTime(); !got.Equal(want) {
						t.Errorf("span[%d] start = %v, want %v", i, got, want)
					}
				}
				if want, ok := tt.wantDurations[wantName]; ok {
					if got := s.EndTime().Sub(s.StartTime()); got != want {
						t.Errorf("span[%d] duration = %v, want %v", i, got, want)
					}
				}
			}
		})
	}
}

func TestEmitToolSpans_ErrorStatus(t *testing.T) {
	client, recorder := newSpanRecordingClient(t, agento11y.ContentCaptureModeMetadataOnly)
	ts := time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)

	gen := agento11y.Generation{
		CompletedAt: ts,
		Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{{
				Kind:     agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{ID: "tc_err", Name: "Write"},
			}},
		}},
	}
	results := map[string]*agento11y.ToolResult{
		"tc_err": {ToolCallID: "tc_err", Content: "denied", IsError: true},
	}

	emitToolSpans(context.Background(), client, gen, results, nil)
	_ = client.Shutdown(context.Background())

	spans := spansByName(recorder.Ended(), "execute_tool")
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Status().Code != codes.Error {
		t.Errorf("span status code = %v, want Error", spans[0].Status().Code)
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:9000":        true,
		"http://127.0.0.1:9000/custom": true,
		"http://localhost:9000":        true,
		"http://localhost:9000/custom": true,
		"http://[::1]:9000":            true,
		"HTTP://127.0.0.1:9000":        true,
		// Spellings of the receiver's address that only matched exactly before.
		"http://LOCALHOST:9000":          true,
		"http://[0:0:0:0:0:0:0:1]:9000":  true,
		"http://[::ffff:127.0.0.1]:9000": true,
		"http://attacker.com@localhost":  true, // userinfo is not the host
		// Not the receiver, or not certainly: a fully qualified name goes to
		// DNS with Go's own resolver.
		"http://localhost.:9000":         false,
		"http://127.0.0.1.:9000":         false,
		"https://LOCALHOST:9000":         false, // https, whatever the host's case
		"http://localhost..:9000":        false,
		"http://127.0.0.2:9000":          false,
		"http://[::ffff:127.0.0.2]:9000": false,
		"http://[::127.0.0.1]:9000":      false,
		"http://[::1%25lo0]:9000":        false,
		"http://localhost.attacker.com.": false,
		"https://cloud.example.com":      false,
		"https://127.0.0.1":              false, // https → not the local receiver
		"":                               false,
		"http://example.com":             false,
		// Hostname-confusion attacks: HasPrefix matched these; URL parse
		// rejects them.
		"http://localhost.attacker.com": false,
		"http://127.0.0.1.attacker.com": false,
		"http://127.0.0.1@attacker.com": false,
	}
	for endpoint, want := range cases {
		t.Run(endpoint, func(t *testing.T) {
			if got := envconfig.IsLocalEndpoint(endpoint); got != want {
				t.Fatalf("IsLocalEndpoint(%q) = %v, want %v", endpoint, got, want)
			}
		})
	}
}

func TestHook_LocalEndpointSkipsCloudAuthCheck(t *testing.T) {
	dir := t.TempDir()
	sessionID := "local-export-session"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(transcriptPath, []byte(buildHookUserJSONL(sessionID, "hey")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIGIL_ENDPOINT", "http://127.0.0.1:9000")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")

	logs := runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if strings.Contains(logs, "not exporting: missing") {
		t.Fatalf("local endpoint should bypass cloud auth check; got logs:\n%s", logs)
	}
}

// TestHook_AgentNameOverride pins the two paths that stamp the agent identity
// to one value. The guard request and the exported generations are built by
// separate code (guard.ToolCallInput and the mapper), so a rule scoped to a
// per-run name only matches that run's generations when both read the same
// resolved name.
//
// It also pins what must not move with the name: the export User-Agent and the
// OTel instrumentation scope identify the build, not the session.
func TestHook_AgentNameOverride(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		// guardEnv, when set, replaces env for the PreToolUse invocation, so a
		// name changed between two invocations of one session can be checked.
		guardEnv  map[string]string
		wantGuard string
		wantAgent string
		// wantSubagent is the name on the synthesised subagent generation.
		wantSubagent string
	}{
		{
			name:      "unset keeps the product name",
			wantAgent: "claude-code", wantSubagent: "claude-code/subagent",
		},
		{
			name:      "preferred spelling",
			env:       map[string]string{"AGENTO11Y_AGENT_NAME": "claude-code-e2e"},
			wantAgent: "claude-code-e2e", wantSubagent: "claude-code-e2e/subagent",
		},
		{
			name:      "legacy spelling",
			env:       map[string]string{"SIGIL_AGENT_NAME": "legacy-name"},
			wantAgent: "legacy-name", wantSubagent: "legacy-name/subagent",
		},
		{
			name: "preferred wins over legacy",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": "preferred-name",
				"SIGIL_AGENT_NAME":     "legacy-name",
			},
			wantAgent: "preferred-name", wantSubagent: "preferred-name/subagent",
		},
		{
			// The name is resolved per hook invocation, not pinned into session
			// state, so a value changed mid-session splits across two names.
			name:      "name changed between invocations of one session",
			guardEnv:  map[string]string{"AGENTO11Y_AGENT_NAME": "run-a"},
			env:       map[string]string{"AGENTO11Y_AGENT_NAME": "run-b"},
			wantGuard: "run-a",
			wantAgent: "run-b", wantSubagent: "run-b/subagent",
		},
	}

	// The export User-Agent and the OTel scope name the build, not the session,
	// so both stay fixed under every override. No OTLP endpoint is configured
	// here, so Hook passes no tracer and the SDK spans carry the SDK's own scope
	// rather than the plugin's otelInstrumentationName.
	wantUserAgent := useragent.For(AgentName)
	const wantScope = "github.com/grafana/sigil-sdk/go/sigil"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var guardAgents, guardConversationIDs, exportAgents, exportConversationIDs, exportUserAgents []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				switch {
				case strings.Contains(r.URL.Path, "hooks:evaluate"):
					var req struct {
						Context struct {
							AgentName      string `json:"agent_name"`
							ConversationID string `json:"conversation_id"`
						} `json:"context"`
					}
					_ = json.Unmarshal(body, &req)
					guardAgents = append(guardAgents, req.Context.AgentName)
					guardConversationIDs = append(guardConversationIDs, req.Context.ConversationID)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"action":"allow"}`))
				default:
					var req struct {
						Generations []struct {
							ID             string `json:"id"`
							AgentName      string `json:"agent_name"`
							ConversationID string `json:"conversation_id"`
						} `json:"generations"`
					}
					_ = json.Unmarshal(body, &req)
					exportUserAgents = append(exportUserAgents, r.Header.Get("User-Agent"))
					resp := struct {
						Results []map[string]any `json:"results"`
					}{Results: make([]map[string]any, 0, len(req.Generations))}
					for _, g := range req.Generations {
						exportAgents = append(exportAgents, g.AgentName)
						exportConversationIDs = append(exportConversationIDs, g.ConversationID)
						resp.Results = append(resp.Results, map[string]any{"generation_id": g.ID, "accepted": true})
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(resp)
				}
			}))
			defer server.Close()

			dir := t.TempDir()
			sessionID := "agent-name-session"
			// An Agent tool call plus its result: the synthesised subagent
			// generation is a second copy of the name literal in the mapper,
			// so the fixture has to produce one.
			transcript := strings.Join([]string{
				buildHookUserJSONL(sessionID, "research this"),
				buildHookAssistantLineAt(sessionID, "req-1", "tool_use", hookFixtureTimestamp, []any{map[string]any{
					"type": "tool_use", "id": "agent_1", "name": "Agent",
					"input": map[string]any{"description": "look around"},
				}}, 7),
				buildHookToolResultJSONL(sessionID, "agent_1", "agent result"),
				buildHookAssistantJSONL(sessionID, "req-2", "end_turn", "done", 5),
			}, "\n") + "\n"
			transcriptPath := filepath.Join(dir, "transcript.jsonl")
			if err := os.WriteFile(transcriptPath, []byte(transcript), 0o644); err != nil {
				t.Fatal(err)
			}

			recorder := installGlobalSpanRecorder(t)
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
			envconfig.PinAliasEnvBlank(t)
			setHookExportEnv(t, server.URL)
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")

			guardEnv := tt.env
			if tt.guardEnv != nil {
				guardEnv = tt.guardEnv
			}
			for k, v := range guardEnv {
				t.Setenv(k, v)
			}
			runHookForTest(t, hookInput{
				HookEventName:  "PreToolUse",
				SessionID:      sessionID,
				TranscriptPath: transcriptPath,
				ToolName:       "Bash",
				ToolInput:      json.RawMessage(`{"command":"echo hi"}`),
				ToolUseID:      "tu_1",
			})

			// Re-applied so a scenario that changes the name between the two
			// invocations exports under the second value.
			envconfig.PinAliasEnvBlank(t)
			setHookExportEnv(t, server.URL)
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			logs := runHookForTest(t, hookInput{
				HookEventName:  "Stop",
				SessionID:      sessionID,
				TranscriptPath: transcriptPath,
			})

			wantGuard := tt.wantGuard
			if wantGuard == "" {
				wantGuard = tt.wantAgent
			}
			if len(guardAgents) != 1 || guardAgents[0] != wantGuard {
				t.Fatalf("guard agent names = %v, want [%q]", guardAgents, wantGuard)
			}
			if len(guardConversationIDs) != 1 || guardConversationIDs[0] != sessionID {
				t.Fatalf("guard conversation IDs = %v, want [%q]", guardConversationIDs, sessionID)
			}
			wantExport := []string{tt.wantAgent, tt.wantSubagent, tt.wantAgent}
			if len(exportAgents) != len(wantExport) {
				t.Fatalf("exported agent names = %v, want %v (logs:\n%s)", exportAgents, wantExport, logs)
			}
			for i, want := range wantExport {
				if exportAgents[i] != want {
					t.Fatalf("exported agent name[%d] = %q, want %q (all: %v)", i, exportAgents[i], want, exportAgents)
				}
			}
			for i, got := range exportConversationIDs {
				if got != sessionID {
					t.Fatalf("exported conversation ID[%d] = %q, want %q", i, got, sessionID)
				}
			}
			if len(exportUserAgents) == 0 {
				t.Fatal("no export request captured")
			}
			spans := spansByName(recorder.Ended(), "generateText")
			if len(spans) == 0 {
				t.Fatalf("no generateText spans recorded (logs:\n%s)", logs)
			}
			for i, got := range exportUserAgents {
				if got != wantUserAgent {
					t.Fatalf("export User-Agent[%d] = %q, want %q", i, got, wantUserAgent)
				}
			}
			for i, s := range spans {
				if got := s.InstrumentationScope().Name; got != wantScope {
					t.Fatalf("span[%d] scope = %q, want %q", i, got, wantScope)
				}
			}
		})
	}
}

// TestHook_PromptRedactionWiring pins AGENTO11Y_REDACT_INPUT_MESSAGES from the
// environment through to the exported body. The mapper tests cover the flag
// itself; this one covers the wire between them.
func TestHook_PromptRedactionWiring(t *testing.T) {
	const secret = "glc_abcdefghijklmnopqrstuvwxyz"

	tests := []struct {
		name            string
		raw             string
		wantPromptInRaw bool
	}{
		{name: "redacts by default", raw: "", wantPromptInRaw: false},
		{name: "opt-out exports the prompt unredacted", raw: "false", wantPromptInRaw: true},
		{name: "typo keeps redaction on", raw: "flase", wantPromptInRaw: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
			envconfig.PinAliasEnvBlank(t)

			var gotBody atomic.Value
			gotBody.Store("")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body", http.StatusBadRequest)
					return
				}
				gotBody.Store(string(body))
				var request map[string]any
				if err := json.Unmarshal(body, &request); err != nil {
					http.Error(w, "invalid json", http.StatusBadRequest)
					return
				}
				generations, _ := request["generations"].([]any)
				results := make([]map[string]any, 0, len(generations))
				for _, entry := range generations {
					generation, _ := entry.(map[string]any)
					id, _ := generation["id"].(string)
					results = append(results, map[string]any{"generation_id": id, "accepted": true})
				}
				w.Header().Set("Content-Type", "application/json")
				if err := json.NewEncoder(w).Encode(map[string]any{"results": results}); err != nil {
					http.Error(w, "encode response", http.StatusInternalServerError)
				}
			}))
			defer server.Close()

			setHookExportEnv(t, server.URL)
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
			t.Setenv("AGENTO11Y_REDACT_INPUT_MESSAGES", tt.raw)

			sessionID := "redaction-wiring-session"
			transcriptPath := filepath.Join(dir, "transcript.jsonl")
			content := buildHookUserJSONL(sessionID, "my token is "+secret) + "\n" +
				buildHookAssistantJSONL(sessionID, "req-1", "end_turn", "assistant saw "+secret, 5) + "\n"
			if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}

			runHookForTest(t, hookInput{
				HookEventName:  "Stop",
				SessionID:      sessionID,
				TranscriptPath: transcriptPath,
			})

			body, _ := gotBody.Load().(string)
			if body == "" {
				t.Fatal("expected an export request")
			}
			if got := strings.Contains(body, "my token is "+secret); got != tt.wantPromptInRaw {
				t.Errorf("prompt unredacted in export = %v, want %v: %s", got, tt.wantPromptInRaw, body)
			}
			// The flag covers the prompt only.
			if strings.Contains(body, "assistant saw "+secret) {
				t.Errorf("assistant text lost its redaction: %s", body)
			}
		})
	}
}

// TestHook_ExportsClaudeCodeTitle pins the wire from the transcript's title
// lines to the exported generation. The mapper tests cover the precedence;
// this one fails if the hook stops handing the titles it read to the mapper.
func TestHook_ExportsClaudeCodeTitle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)

	var gotBody atomic.Value
	gotBody.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		gotBody.Store(string(body))
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		acceptGenerationExport(w, r)
	}))
	defer server.Close()

	setHookExportEnv(t, server.URL)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")

	sessionID := "title-wiring-session"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	content := `{"type":"ai-title","aiTitle":"Flaky login test","sessionId":"title-wiring-session"}` + "\n" +
		buildHookUserJSONL(sessionID, "the login test fails") + "\n" +
		buildHookAssistantJSONL(sessionID, "req-1", "end_turn", "It races the token refresh.", 5) + "\n"
	if err := os.WriteFile(transcriptPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})

	body, _ := gotBody.Load().(string)
	if body == "" {
		t.Fatal("expected an export request")
	}
	// The ai-title appears nowhere in the transcript's turns, so it reaches
	// the body only as the conversation title.
	if !strings.Contains(body, "Flaky login test") {
		t.Errorf("export does not carry the ai-title: %s", body)
	}
}

func TestHook_AccountIdentity(t *testing.T) {
	const secret = "synthetic-account-secret"
	for _, tc := range []struct {
		name, account, wantID string
	}{
		{name: "selected profile", account: `{"oauthAccount":{"emailAddress":"selected@example.com"},"token":"` + secret + `"}`, wantID: "selected@example.com"},
		{name: "malformed profile", account: `{"token":"` + secret + `","oauthAccount":`},
		{name: "invalid field type", account: `{"oauthAccount":{"emailAddress":{"` + secret + `":"value"}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("HOME", dir)
			t.Setenv("USERPROFILE", dir)
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
			t.Setenv("CLAUDE_CONFIG_DIR", dir)
			t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "")
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(tc.account), 0o600); err != nil {
				t.Fatal(err)
			}

			var gotBody atomic.Value
			gotBody.Store("")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, "read body", http.StatusBadRequest)
					return
				}
				gotBody.Store(string(body))
				r.Body = io.NopCloser(bytes.NewReader(body))
				acceptGenerationExport(w, r)
			}))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			t.Setenv("SIGIL_USER_ID", "")

			const sessionID = "account-identity-session"
			transcriptPath := filepath.Join(dir, "transcript.jsonl")
			content := buildHookUserJSONL(sessionID, "hello") + "\n" +
				buildHookAssistantJSONL(sessionID, "req-1", "end_turn", "hello", 5) + "\n"
			if err := os.WriteFile(transcriptPath, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}

			stderr, err := os.CreateTemp(dir, "stderr")
			if err != nil {
				t.Fatal(err)
			}
			previousStderr := os.Stderr
			os.Stderr = stderr
			t.Cleanup(func() {
				os.Stderr = previousStderr
				_ = stderr.Close()
			})
			logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
			os.Stderr = previousStderr
			diagnostics, err := os.ReadFile(stderr.Name())
			if err != nil {
				t.Fatal(err)
			}
			body, _ := gotBody.Load().(string)
			for _, output := range []string{body, logs, string(diagnostics)} {
				if strings.Contains(output, secret) || strings.Contains(output, tc.account) {
					t.Fatal("account contents leaked into diagnostics or export")
				}
			}
			if tc.wantID == "" && !strings.Contains(string(diagnostics), "malformed selected account file") {
				t.Error("missing malformed-account diagnostic")
			}
			var request struct {
				Generations []struct {
					Metadata map[string]any `json:"metadata"`
				} `json:"generations"`
			}
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatalf("decode export: %v", err)
			}
			if len(request.Generations) != 1 {
				t.Fatalf("exported %d generations, want 1", len(request.Generations))
			}
			metadata := request.Generations[0].Metadata
			if gotID, _ := metadata["agento11y.user.id"].(string); gotID != tc.wantID {
				t.Errorf("identity = %q, want %q", gotID, tc.wantID)
			}
		})
	}
}

// TestHook_ExportsSubagentTranscriptUsage is the live-capture counterpart of
// history import: Claude Code writes the subagent's real turns (and token
// usage) to a sibling file, and the parent transcript only has an Agent
// tool_use/tool_result pair. Before this path existed the hook synthesised a
// zero-token placeholder, which is what priced those agents at $0.
func TestHook_ExportsSubagentTranscriptUsage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)

	var (
		exports   [][]exportedGeneration
		rawBodies []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		rawBodies = append(rawBodies, string(body))
		var req struct {
			Generations []exportedGeneration `json:"generations"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			exports = append(exports, req.Generations)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		acceptGenerationExport(w, r)
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "084e2f13-b8e9-4971-801e-4b2d6334ceec"
	agentID := "a18f9a9d9f1f3d28e"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "investigate the cost gap"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_1", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose", "description": "look around"},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_1", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nfound the mapper"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	sub := strings.Join([]string{
		buildHookUserJSONL(sessionID, "look around"),
		buildHookSidechainAssistantJSONL(sessionID, "req-side", agentID, "general-purpose", "the mapper drops usage", 40),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(sub), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if len(exports) != 1 {
		t.Fatalf("export requests = %d, want 1 (logs:\n%s\nbodies:\n%s)", len(exports), logs, strings.Join(rawBodies, "\n---\n"))
	}

	var (
		parentNames []string
		subGens     []exportedGeneration
	)
	for _, g := range exports[0] {
		if g.ConversationID != sessionID {
			t.Errorf("conversation %q, want %q", g.ConversationID, sessionID)
		}
		switch g.AgentName {
		case "claude-code":
			parentNames = append(parentNames, g.AgentName)
		case "claude-code/general-purpose", "claude-code/subagent":
			subGens = append(subGens, g)
		default:
			t.Errorf("unexpected agent %q", g.AgentName)
		}
	}
	if len(parentNames) != 2 {
		t.Fatalf("parent generations = %d, want 2 (spawn + wrap-up); all=%v logs:\n%s",
			len(parentNames), agentNames(exports[0]), logs)
	}
	if len(subGens) != 1 {
		t.Fatalf("subagent generations = %d, want 1 with real usage; all=%v logs:\n%s",
			len(subGens), agentNames(exports[0]), logs)
	}
	if subGens[0].AgentName != "claude-code/general-purpose" {
		t.Errorf("subagent name = %q, want claude-code/general-purpose", subGens[0].AgentName)
	}
	if in, out := subGens[0].usageTokens(); in != 80 || out != 40 {
		t.Errorf("subagent usage = input %d output %d, want 80/40", in, out)
	}
	if len(subGens[0].ParentGenerationIDs) != 1 {
		t.Errorf("subagent parents = %v, want the spawning generation", subGens[0].ParentGenerationIDs)
	}

	st := state.Load(sessionID)
	if st.SubagentOffsets[agentID] == 0 {
		t.Fatalf("SubagentOffsets[%s] = 0, want the exported byte offset", agentID)
	}

	// A later Stop that only wraps up a new parent turn must not re-export the
	// subagent file, or cost would double-count.
	extra := buildHookUserJSONL(sessionID, "thanks") + "\n" +
		buildHookAssistantJSONL(sessionID, "req-3", "end_turn", "you are welcome", 2) + "\n"
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(extra); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if len(exports) != 2 {
		t.Fatalf("second Stop export requests = %d, want 2 total", len(exports))
	}
	for _, g := range exports[1] {
		if strings.HasPrefix(g.AgentName, "claude-code/") {
			t.Fatalf("second Stop re-exported subagent %q", g.AgentName)
		}
	}
}

type exportedGeneration struct {
	ID                  string   `json:"id"`
	AgentName           string   `json:"agent_name"`
	ConversationID      string   `json:"conversation_id"`
	ParentGenerationIDs []string `json:"parent_generation_ids"`
	Usage               struct {
		InputTokens  json.Number `json:"input_tokens"`
		OutputTokens json.Number `json:"output_tokens"`
	} `json:"usage"`
}

func (g exportedGeneration) usageTokens() (input, output int64) {
	input, _ = g.Usage.InputTokens.Int64()
	output, _ = g.Usage.OutputTokens.Int64()
	return input, output
}

func agentNames(gens []exportedGeneration) []string {
	out := make([]string, len(gens))
	for i, g := range gens {
		out[i] = g.AgentName
	}
	return out
}

func buildHookSidechainAssistantJSONL(sessionID, requestID, agentID, attribution, text string, outputTokens int64) string {
	return buildHookSidechainAssistantLine(sessionID, requestID, agentID, attribution, "",
		[]any{map[string]any{"type": "text", "text": text}}, outputTokens)
}

func buildHookSidechainAssistantLine(sessionID, requestID, agentID, attribution, stopReason string, content []any, outputTokens int64) string {
	msg := map[string]any{
		"model":   "claude-opus-4",
		"usage":   map[string]any{"input_tokens": 80, "output_tokens": outputTokens},
		"content": content,
	}
	if stopReason != "" {
		msg["stop_reason"] = stopReason
	}
	line := map[string]any{
		"type":             "assistant",
		"sessionId":        sessionID,
		"isSidechain":      true,
		"agentId":          agentID,
		"attributionAgent": attribution,
		"timestamp":        hookFixtureTimestamp,
		"version":          "1.0.0",
		"requestId":        requestID,
		"message":          msg,
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func startGenerationExportServer(t *testing.T) (url string, exports *[][]exportedGeneration) {
	t.Helper()
	var got [][]exportedGeneration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		var req struct {
			Generations []exportedGeneration `json:"generations"`
		}
		if err := json.Unmarshal(body, &req); err == nil {
			got = append(got, req.Generations)
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		acceptGenerationExport(w, r)
	}))
	t.Cleanup(server.Close)
	return server.URL, &got
}

func subagentExports(gens []exportedGeneration) []exportedGeneration {
	var out []exportedGeneration
	for _, g := range gens {
		if strings.HasPrefix(g.AgentName, "claude-code/") {
			out = append(out, g)
		}
	}
	return out
}

// TestHook_ExportsBackgroundSubagentLaterTurns covers a run_in_background
// Agent call: the parent tool_result arrives immediately, but the sibling
// transcript keeps growing. The first Stop must not freeze the file as done.
func TestHook_ExportsBackgroundSubagentLaterTurns(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "bg-session"
	agentID := "bgagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subDir, "agent-"+agentID+".jsonl")

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "research in the background"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_bg", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose", "run_in_background": true},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_bg", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nstarted"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "kicked off", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	first := strings.Join([]string{
		buildHookUserJSONL(sessionID, "look around"),
		buildHookSidechainAssistantLine(sessionID, "req-side-1", agentID, "general-purpose", "", []any{map[string]any{
			"type": "tool_use", "id": "tu_read", "name": "Read",
			"input": map[string]any{"file_path": "x.go"},
		}}, 12),
		buildHookToolResultJSONL(sessionID, "tu_read", "package x"),
	}, "\n") + "\n"
	if err := os.WriteFile(subPath, []byte(first), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 1 {
		t.Fatalf("first Stop exports = %d, want 1", len(*exports))
	}
	if got := subagentExports((*exports)[0]); len(got) != 0 {
		t.Fatalf("first Stop exported in-progress subagent gens = %d, want 0 held; all=%v", len(got), agentNames((*exports)[0]))
	}

	second := buildHookSidechainAssistantJSONL(sessionID, "req-side-2", agentID, "general-purpose", "later findings", 40) + "\n"
	f, err := os.OpenFile(subPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(second); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 2 {
		t.Fatalf("SessionEnd exports = %d, want 2 total", len(*exports))
	}
	later := subagentExports((*exports)[1])
	if len(later) != 2 {
		t.Fatalf("SessionEnd subagent gens = %d, want the held first turn and the later one; all=%v", len(later), agentNames((*exports)[1]))
	}
	var outs []int64
	for _, g := range later {
		_, out := g.usageTokens()
		outs = append(outs, out)
	}
	if !((outs[0] == 12 && outs[1] == 40) || (outs[0] == 40 && outs[1] == 12)) {
		t.Fatalf("SessionEnd output tokens = %v, want 12 and 40", outs)
	}
}

// TestHook_ExportsNestedSubagentTranscript covers an Agent call made from
// inside another subagent. The child's tool_result never appears in the main
// session transcript, so a parent-only completed-key gate would skip it.
func TestHook_ExportsNestedSubagentTranscript(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "nest-session"
	midID := "midagent1"
	childID := "childagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "delegate"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_mid", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose"},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_mid", "agentId: "+midID+"\nTranscript dir: "+subDir+"\nmid done"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	mid := strings.Join([]string{
		buildHookUserJSONL(sessionID, "spawn a child"),
		buildHookSidechainAssistantLine(sessionID, "req-mid", midID, "general-purpose", "", []any{map[string]any{
			"type": "tool_use", "id": "agent_child", "name": "Agent",
			"input": map[string]any{"subagent_type": "explore"},
		}}, 9),
		buildHookToolResultJSONL(sessionID, "agent_child", "agentId: "+childID+"\nTranscript dir: "+subDir+"\nchild done"),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+midID+".jsonl"), []byte(mid), 0o644); err != nil {
		t.Fatal(err)
	}

	child := strings.Join([]string{
		buildHookUserJSONL(sessionID, "explore the tree"),
		buildHookSidechainAssistantJSONL(sessionID, "req-child", childID, "explore", "found it", 33),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+childID+".jsonl"), []byte(child), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 1 {
		t.Fatalf("exports = %d, want 1 (logs:\n%s)", len(*exports), logs)
	}
	var (
		midGens   int
		childGens []exportedGeneration
	)
	for _, g := range (*exports)[0] {
		switch g.AgentName {
		case "claude-code/general-purpose":
			midGens++
		case "claude-code/explore":
			childGens = append(childGens, g)
		}
	}
	if midGens == 0 {
		t.Fatalf("missing mid subagent; all=%v", agentNames((*exports)[0]))
	}
	if len(childGens) != 1 {
		t.Fatalf("nested subagent gens = %d, want 1; all=%v", len(childGens), agentNames((*exports)[0]))
	}
	if _, out := childGens[0].usageTokens(); out != 33 {
		t.Fatalf("nested usage output = %d, want 33", out)
	}
}

// TestHook_DropsParentSidechainWhenFileExists pins the double-count: Claude
// Code sometimes leaves a sidechain copy in the parent transcript and also
// writes the same request to the sibling file.
func TestHook_DropsParentSidechainWhenFileExists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "dup-session"
	agentID := "dupagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "investigate"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_1", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose"},
		}}, 7),
		buildHookSidechainAssistantJSONL(sessionID, "req-side", agentID, "general-purpose", "copied into the parent", 40),
		buildHookToolResultJSONL(sessionID, "agent_1", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nfound"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	sub := strings.Join([]string{
		buildHookUserJSONL(sessionID, "look around"),
		buildHookSidechainAssistantJSONL(sessionID, "req-side", agentID, "general-purpose", "copied into the parent", 40),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(sub), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 1 {
		t.Fatalf("exports = %d, want 1", len(*exports))
	}
	subs := subagentExports((*exports)[0])
	if len(subs) != 1 {
		t.Fatalf("subagent gens = %d, want 1 (no parent sidechain copy); all=%v", len(subs), agentNames((*exports)[0]))
	}
	if _, out := subs[0].usageTokens(); out != 40 {
		t.Fatalf("output tokens = %d, want 40 once", out)
	}
}

// TestHook_MergesSplitSubagentResponse covers one API response written as
// several assistant lines with a parallel tool_result between them. Those
// fragments share a requestId (and message id) and must export as one
// generation that keeps the last line's usage.
func TestHook_MergesSplitSubagentResponse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "split-session"
	agentID := "splitagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "investigate"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_1", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose"},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_1", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nfound"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	const messageID = "msg_split"
	sub := strings.Join([]string{
		buildHookUserJSONL(sessionID, "inspect the repo"),
		buildHookSidechainAssistantLineID(sessionID, "req-split", agentID, "general-purpose", messageID, "", []any{
			map[string]any{"type": "thinking", "thinking": "plan the reads"},
			map[string]any{"type": "tool_use", "id": "tu_a", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}, 2),
		buildHookToolResultJSONL(sessionID, "tu_a", "package a"),
		buildHookSidechainAssistantLineID(sessionID, "req-split", agentID, "general-purpose", messageID, "", []any{
			map[string]any{"type": "tool_use", "id": "tu_b", "name": "Read", "input": map[string]any{"file_path": "b.go"}},
		}, 40),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(sub), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 1 {
		t.Fatalf("exports = %d, want 1", len(*exports))
	}
	subs := subagentExports((*exports)[0])
	if len(subs) != 1 {
		t.Fatalf("subagent gens = %d, want 1 merged response; all=%v", len(subs), agentNames((*exports)[0]))
	}
	if in, out := subs[0].usageTokens(); in != 80 || out != 40 {
		t.Fatalf("usage = input %d output %d, want 80/40 (last line, not 80+80 / 2+40)", in, out)
	}
	if strings.Contains(subs[0].ID, "#") {
		t.Fatalf("generation id %q was uniqued; the split parts were exported separately", subs[0].ID)
	}
}

// TestHook_IgnoresTranscriptDirOutsideSession pins discovery to the session's
// own subagents/ folder. A tool result that names some other conversation's
// Transcript dir must not export that folder's turns as this session's.
func TestHook_IgnoresTranscriptDirOutsideSession(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "this-session"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	otherDir := filepath.Join(dir, "other-session", "subagents")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}

	other := strings.Join([]string{
		buildHookUserJSONL("other-session", "from another conversation"),
		buildHookSidechainAssistantJSONL("other-session", "req-other", "otheragent", "explore", "should not export", 777),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(otherDir, "agent-otheragent.jsonl"), []byte(other), 0o644); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "run a command"),
		buildHookAssistantLineAt(sessionID, "req-bash", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "bash_1", "name": "Bash",
			"input": map[string]any{"command": "echo hi"},
		}}, 4),
		buildHookToolResultJSONL(sessionID, "bash_1", "Transcript dir: "+otherDir+"\nhello from bash"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) != 1 {
		t.Fatalf("exports = %d, want 1", len(*exports))
	}
	for _, g := range (*exports)[0] {
		if _, out := g.usageTokens(); out == 777 {
			t.Fatalf("exported a turn from Transcript dir %s outside the session", otherDir)
		}
		if strings.HasPrefix(g.AgentName, "claude-code/") {
			t.Fatalf("exported foreign subagent %q", g.AgentName)
		}
	}
}

// TestHook_HoldsIncompleteBackgroundResponse covers Bugbot's duplicate-ID
// case: a parallel tool_result lands before the rest of the same Anthropic
// message. The prefix must not export on the first Stop, or the finished
// message later reuses the ID and the first write (output 3) wins.
func TestHook_HoldsIncompleteBackgroundResponse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "hold-session"
	agentID := "holdagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	subPath := filepath.Join(subDir, "agent-"+agentID+".jsonl")

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "research in the background"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_bg", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose", "run_in_background": true},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_bg", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nstarted"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "kicked off", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	const messageID = "msg_open"
	prefix := strings.Join([]string{
		buildHookUserJSONL(sessionID, "inspect the repo"),
		buildHookSidechainAssistantLineID(sessionID, "req-split", agentID, "general-purpose", messageID, "", []any{
			map[string]any{"type": "thinking", "thinking": "plan"},
			map[string]any{"type": "tool_use", "id": "tu_a", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}, 3),
		buildHookToolResultJSONL(sessionID, "tu_a", "package a"),
	}, "\n") + "\n"
	if err := os.WriteFile(subPath, []byte(prefix), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if got := subagentExports((*exports)[0]); len(got) != 0 {
		t.Fatalf("first Stop exported the open message: %v", agentNames((*exports)[0]))
	}

	rest := buildHookSidechainAssistantLineID(sessionID, "req-split", agentID, "general-purpose", messageID, "", []any{
		map[string]any{"type": "tool_use", "id": "tu_b", "name": "Read", "input": map[string]any{"file_path": "b.go"}},
	}, 660) + "\n"
	f, err := os.OpenFile(subPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(rest); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	if len(*exports) > 1 {
		if got := subagentExports((*exports)[len(*exports)-1]); len(got) != 0 {
			t.Fatalf("second Stop exported before the subagent finished: %v", agentNames(got))
		}
	}

	runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: sessionID, TranscriptPath: transcriptPath})
	var subs []exportedGeneration
	for _, batch := range *exports {
		subs = append(subs, subagentExports(batch)...)
	}
	if len(subs) != 1 {
		t.Fatalf("subagent gens = %d, want 1 finished message; all=%v", len(subs), agentNames(subs))
	}
	if in, out := subs[0].usageTokens(); in != 80 || out != 660 {
		t.Fatalf("usage = input %d output %d, want 80/660 (last line, not the prefix)", in, out)
	}
	if strings.Contains(subs[0].ID, "#") {
		t.Fatalf("generation id %q was uniqued; the prefix and tail were exported separately", subs[0].ID)
	}
}

// TestHook_TranscriptDirDoesNotFinishRunningSibling covers a blocking Agent
// result whose Transcript dir is the shared subagents/ folder. That must not
// mark a still-running background sibling done, or CoalesceSession would
// export an open message under an ID the finished response later reuses.
func TestHook_TranscriptDirDoesNotFinishRunningSibling(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)
	url, exports := startGenerationExportServer(t)
	setHookExportEnv(t, url)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")

	sessionID := "sib-session"
	doneID := "doneagent1"
	bgID := "bgagent2"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "delegate"),
		buildHookAssistantLineAt(sessionID, "req-done", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_done", "name": "Agent",
			"input": map[string]any{"subagent_type": "explore"},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_done", "agentId: "+doneID+"\nTranscript dir: "+subDir+"\ndone"),
		buildHookAssistantLineAt(sessionID, "req-bg", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_bg", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose", "run_in_background": true},
		}}, 6),
		buildHookToolResultJSONL(sessionID, "agent_bg", "agentId: "+bgID+"\nTranscript dir: "+subDir+"\nstarted"),
		buildHookAssistantJSONL(sessionID, "req-wrap", "end_turn", "kicked off", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	done := strings.Join([]string{
		buildHookUserJSONL(sessionID, "explore"),
		buildHookSidechainAssistantJSONL(sessionID, "req-done-sub", doneID, "explore", "finished", 33),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+doneID+".jsonl"), []byte(done), 0o644); err != nil {
		t.Fatal(err)
	}

	const messageID = "msg_open"
	bg := strings.Join([]string{
		buildHookUserJSONL(sessionID, "still working"),
		buildHookSidechainAssistantLineID(sessionID, "req-bg-sub", bgID, "general-purpose", messageID, "", []any{
			map[string]any{"type": "thinking", "thinking": "plan"},
			map[string]any{"type": "tool_use", "id": "tu_a", "name": "Read", "input": map[string]any{"file_path": "a.go"}},
		}, 3),
		buildHookToolResultJSONL(sessionID, "tu_a", "package a"),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+bgID+".jsonl"), []byte(bg), 0o644); err != nil {
		t.Fatal(err)
	}

	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: transcriptPath})
	var (
		doneOut int64
		bgOuts  []int64
	)
	for _, g := range subagentExports((*exports)[0]) {
		_, out := g.usageTokens()
		switch g.AgentName {
		case "claude-code/explore":
			doneOut = out
		case "claude-code/general-purpose":
			bgOuts = append(bgOuts, out)
		}
	}
	if doneOut != 33 {
		t.Fatalf("finished sibling output = %d, want 33", doneOut)
	}
	if len(bgOuts) != 0 {
		t.Fatalf("running sibling exported %v, want the open message held", bgOuts)
	}
}

func buildHookSidechainAssistantLineID(sessionID, requestID, agentID, attribution, messageID, stopReason string, content []any, outputTokens int64) string {
	msg := map[string]any{
		"model":   "claude-opus-4",
		"usage":   map[string]any{"input_tokens": 80, "output_tokens": outputTokens},
		"content": content,
	}
	if messageID != "" {
		msg["id"] = messageID
	}
	if stopReason != "" {
		msg["stop_reason"] = stopReason
	}
	line := map[string]any{
		"type":             "assistant",
		"sessionId":        sessionID,
		"isSidechain":      true,
		"agentId":          agentID,
		"attributionAgent": attribution,
		"timestamp":        hookFixtureTimestamp,
		"version":          "1.0.0",
		"requestId":        requestID,
		"message":          msg,
	}
	data, _ := json.Marshal(line)
	return string(data)
}

// TestHook_RedactsSubagentTranscriptSecrets guards full-mode redaction on the
// sibling-file path. A GitHub PAT in the subagent prompt, a tool input, and
// the text reply must become [REDACTED:github-pat] on the wire; the parent
// transcript does not carry the secret, so a miss here would export it.
func TestHook_RedactsSubagentTranscriptSecrets(t *testing.T) {
	const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij"
	const marker = "[REDACTED:github-pat]"

	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	envconfig.PinAliasEnvBlank(t)

	var gotBody atomic.Value
	gotBody.Store("")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}
		gotBody.Store(string(body))
		r.Body = io.NopCloser(bytes.NewReader(body))
		acceptGenerationExport(w, r)
	}))
	defer server.Close()

	setHookExportEnv(t, server.URL)
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")

	sessionID := "subagent-redact-session"
	agentID := "redactagent1"
	transcriptPath := filepath.Join(dir, sessionID+".jsonl")
	subDir := filepath.Join(dir, sessionID, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	parent := strings.Join([]string{
		buildHookUserJSONL(sessionID, "investigate the repo"),
		buildHookAssistantLineAt(sessionID, "req-parent", "tool_use", hookFixtureTimestamp, []any{map[string]any{
			"type": "tool_use", "id": "agent_1", "name": "Agent",
			"input": map[string]any{"subagent_type": "general-purpose", "description": "look around"},
		}}, 7),
		buildHookToolResultJSONL(sessionID, "agent_1", "agentId: "+agentID+"\nTranscript dir: "+subDir+"\nfound"),
		buildHookAssistantJSONL(sessionID, "req-done", "end_turn", "done", 5),
	}, "\n") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(parent), 0o644); err != nil {
		t.Fatal(err)
	}

	sub := strings.Join([]string{
		buildHookUserJSONL(sessionID, "use token "+secret),
		buildHookSidechainAssistantLine(sessionID, "req-side-1", agentID, "general-purpose", "tool_use", []any{
			map[string]any{"type": "tool_use", "id": "tu_bash", "name": "Bash",
				"input": map[string]any{"command": "echo " + secret}},
		}, 8),
		buildHookToolResultJSONL(sessionID, "tu_bash", "ok"),
		buildHookSidechainAssistantJSONL(sessionID, "req-side-2", agentID, "general-purpose", "got "+secret, 12),
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(subDir, "agent-"+agentID+".jsonl"), []byte(sub), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := runHookForTest(t, hookInput{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	body, _ := gotBody.Load().(string)
	if body == "" {
		t.Fatalf("expected an export request (logs:\n%s)", logs)
	}
	if strings.Contains(body, secret) {
		t.Fatalf("export leaked the subagent GitHub PAT:\n%s", body)
	}

	var req struct {
		Generations []struct {
			AgentName string `json:"agent_name"`
			Input     []struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"input"`
			Output []struct {
				Parts []struct {
					Text     string `json:"text"`
					ToolCall *struct {
						InputJSON string `json:"input_json"`
					} `json:"tool_call"`
				} `json:"parts"`
			} `json:"output"`
		} `json:"generations"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode export: %v\n%s", err, body)
	}

	var sawPrompt, sawTool, sawReply bool
	for _, g := range req.Generations {
		if !strings.HasPrefix(g.AgentName, "claude-code/") {
			continue
		}
		for _, msg := range g.Input {
			for _, p := range msg.Parts {
				if strings.Contains(p.Text, secret) {
					t.Errorf("subagent prompt leaked the PAT: %q", p.Text)
				}
				if strings.Contains(p.Text, marker) {
					sawPrompt = true
				}
			}
		}
		for _, msg := range g.Output {
			for _, p := range msg.Parts {
				if strings.Contains(p.Text, secret) {
					t.Errorf("subagent reply leaked the PAT: %q", p.Text)
				}
				if strings.Contains(p.Text, marker) {
					sawReply = true
				}
				if p.ToolCall == nil {
					continue
				}
				raw, err := base64.StdEncoding.DecodeString(p.ToolCall.InputJSON)
				if err != nil {
					t.Fatalf("decode tool input: %v", err)
				}
				args := string(raw)
				if strings.Contains(args, secret) {
					t.Errorf("subagent tool input leaked the PAT: %s", args)
				}
				if strings.Contains(args, marker) {
					sawTool = true
				}
			}
		}
	}
	if !sawPrompt || !sawTool || !sawReply {
		t.Fatalf("redacted fields: prompt=%v tool=%v reply=%v (logs:\n%s\nbody:\n%s)", sawPrompt, sawTool, sawReply, logs, body)
	}
}
