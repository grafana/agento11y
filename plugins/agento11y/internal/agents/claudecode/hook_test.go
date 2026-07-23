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

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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
			name:             "enabled_allow_unusable_transform_stays_silent",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"Bash","input_json":{"command":"echo X"}}}]}]}}`,
			expectServerCall: true,
			wantStdoutEmpty:  true,
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

			handlePreToolUse(context.Background(), &stdout, input, st, log.New(&logs, "", 0))

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

func buildHookAssistantJSONL(sessionID, requestID, stopReason, text string, outputTokens int64) string {
	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:00:00Z",
		"version":   "1.0.0",
		"requestId": requestID,
		"message": map[string]any{
			"model":       "claude-opus-4",
			"stop_reason": stopReason,
			"usage":       map[string]any{"input_tokens": 3, "output_tokens": outputTokens},
			"content":     []any{map[string]any{"type": "text", "text": text}},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func buildHookToolResultJSONL(sessionID, toolUseID, content string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:00:00Z",
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
	lines, safeOffset, rawCount := readTranscriptSettled(context.Background(), path, 0, logs)

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
	lines, safeOffset, rawCount := readTranscriptSettled(context.Background(), path, 0, log.New(io.Discard, "", 0))
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
	lines, safeOffset, rawCount := readTranscriptSettled(context.Background(), path, eof, log.New(io.Discard, "", 0))
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
	lines, safeOffset, rawCount := readTranscriptSettled(context.Background(), path, 0, log.New(io.Discard, "", 0))
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

	lines, safeOffset, rawCount := readTranscriptSettled(context.Background(), path, 0, log.New(io.Discard, "", 0))
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
		gen         agento11y.Generation
		results     map[string]*agento11y.ToolResult
		contentMode agento11y.ContentCaptureMode
		wantSpans   int
		wantNames   []string
		wantArgs    map[string]string
		wantResults map[string]string
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
			contentMode: agento11y.ContentCaptureModeMetadataOnly,
			wantSpans:   1,
			wantNames:   []string{"Read"},
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, recorder := newSpanRecordingClient(t, tt.contentMode)
			results := tt.results
			if results == nil {
				results = map[string]*agento11y.ToolResult{}
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

func TestIsLocalEndpoint(t *testing.T) {
	cases := map[string]bool{
		"http://127.0.0.1:9000":        true,
		"http://127.0.0.1:9000/custom": true,
		"http://localhost:9000":        true,
		"http://localhost:9000/custom": true,
		"http://[::1]:9000":            true,
		"https://cloud.example.com":    false,
		"https://127.0.0.1":            false, // https → not the local receiver
		"":                             false,
		"http://example.com":           false,
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
