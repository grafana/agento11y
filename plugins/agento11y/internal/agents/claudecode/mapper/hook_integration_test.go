package mapper_test

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
	"sync"
	"testing"

	claudecode "github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
)

// TestHookSessionEndProcessesAssistantFlushedAfterStop checks that a Stop hook
// fired after a complete assistant turn but before the next assistant response
// keeps the saved offset at the last paired assistant line, so the next batch
// still sees the trailing user prompt as input context.
func TestHookSessionEndProcessesAssistantFlushedAfterStop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	capture := &exportCapture{}
	server := httptest.NewServer(http.HandlerFunc(capture.handle))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	sessionID := "delayed-flush-session"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")

	user1 := buildUserJSONL(sessionID, "user1 prompt") + "\n"
	asst1 := buildAssistantJSONL(sessionID, "req-1", "claude-sonnet-4-20250514", 25, "first reply") + "\n"
	user2 := buildUserJSONL(sessionID, "user2 prompt") + "\n"

	asst1End := int64(len(user1) + len(asst1))

	initial := user1 + asst1 + user2
	if err := os.WriteFile(transcriptPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	stopLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(stopLogs, "produced 1 generations") {
		t.Fatalf("Stop did not produce one generation:\n%s", stopLogs)
	}

	capture.assert(t, []int{1})

	if st := state.Load(sessionID); st.Offset != asst1End {
		t.Fatalf("Offset after Stop = %d, want %d (end of asst1, not end-of-file)", st.Offset, asst1End)
	}
	if st := state.Load(sessionID); st.Offset == int64(len(initial)) {
		t.Fatalf("Offset after Stop should not be end-of-file (%d)", st.Offset)
	}

	firstGen := capture.generation(t, 0, 0)
	requireInputContains(t, firstGen, "user1 prompt")

	asst2 := buildAssistantJSONL(sessionID, "req-2", "claude-sonnet-4-20250514", 30, "second reply") + "\n"
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(asst2); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sessionEndLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "SessionEnd",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(sessionEndLogs, "produced 1 generations") {
		t.Fatalf("SessionEnd did not produce one generation:\n%s", sessionEndLogs)
	}

	capture.assert(t, []int{1, 1})

	finalSize := int64(len(initial) + len(asst2))
	if st := state.Load(sessionID); st.Offset != finalSize {
		t.Fatalf("Offset after SessionEnd = %d, want %d", st.Offset, finalSize)
	}

	secondGen := capture.generation(t, 1, 0)
	requireInputContains(t, secondGen, "user2 prompt")
}

// TestHookExportsToolResultInputAfterDelayedAssistantFlush is the
// tool-result variant of the trailing-context bug.
func TestHookExportsToolResultInputAfterDelayedAssistantFlush(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	capture := &exportCapture{}
	server := httptest.NewServer(http.HandlerFunc(capture.handle))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	sessionID := "tool-result-delayed-session"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")

	user := buildUserJSONL(sessionID, "list files") + "\n"
	asstToolUse := buildAssistantToolUseJSONL(sessionID, "req-1", "tu_1", "Bash", `{"command":"ls"}`) + "\n"
	toolResult := buildToolResultJSONL(sessionID, "tu_1", "file1.go\nfile2.go") + "\n"

	toolUseEnd := int64(len(user) + len(asstToolUse))

	initial := user + asstToolUse + toolResult
	if err := os.WriteFile(transcriptPath, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	stopLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(stopLogs, "produced 1 generations") {
		t.Fatalf("Stop did not produce one generation:\n%s", stopLogs)
	}

	capture.assert(t, []int{1})

	if st := state.Load(sessionID); st.Offset != toolUseEnd {
		t.Fatalf("Offset after Stop = %d, want %d (end of tool_use assistant turn)", st.Offset, toolUseEnd)
	}

	firstGen := capture.generation(t, 0, 0)
	requireInputContains(t, firstGen, "list files")

	asstSummary := buildAssistantJSONL(sessionID, "req-2", "claude-sonnet-4-20250514", 30, "Two files.") + "\n"
	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(asstSummary); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sessionEndLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "SessionEnd",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(sessionEndLogs, "produced 1 generations") {
		t.Fatalf("SessionEnd did not produce one generation:\n%s", sessionEndLogs)
	}

	capture.assert(t, []int{1, 1})

	finalSize := int64(len(initial) + len(asstSummary))
	if st := state.Load(sessionID); st.Offset != finalSize {
		t.Fatalf("Offset after SessionEnd = %d, want %d", st.Offset, finalSize)
	}

	secondGen := capture.generation(t, 1, 0)
	requireToolResultInputContains(t, secondGen, "tu_1", "file1.go")
}

// TestHookKeepsOffsetWhenNoAssistantTurnYet exercises the early-return
// log site: a Stop hook fires while the transcript contains only a user
// prompt with no completed assistant response yet.
func TestHookKeepsOffsetWhenNoAssistantTurnYet(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	capture := &exportCapture{}
	server := httptest.NewServer(http.HandlerFunc(capture.handle))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	sessionID := "no-assistant-yet"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	userPart := buildUserJSONL(sessionID, "hello") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(userPart), 0o644); err != nil {
		t.Fatal(err)
	}

	logs := runClaudeHook(t, hookPayload{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(logs, "no completed assistant turn yet") {
		t.Fatalf("expected early-return log message, got:\n%s", logs)
	}
	if st := state.Load(sessionID); st.Offset != 0 {
		t.Fatalf("Offset after Stop = %d, want 0", st.Offset)
	}
	capture.assert(t, nil)
}

type hookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
}

type exportCapture struct {
	mu               sync.Mutex
	paths            []string
	generationCounts []int
	// generations holds the decoded payloads for each export request, so
	// tests can assert on Input/Output content beyond just counts.
	generations [][]json.RawMessage
	readErrs    []string
	decodeErrs  []string
}

type exportResult struct {
	GenerationID string `json:"generation_id"`
	Accepted     bool   `json:"accepted"`
}

type exportResponse struct {
	Results []exportResult `json:"results"`
}

type exportRequest struct {
	Generations []json.RawMessage `json:"generations"`
}

func (c *exportCapture) handle(w http.ResponseWriter, r *http.Request) {
	body, readErr := io.ReadAll(r.Body)
	var req exportRequest
	decodeErr := json.Unmarshal(body, &req)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	c.generationCounts = append(c.generationCounts, len(req.Generations))
	c.generations = append(c.generations, req.Generations)
	if readErr != nil {
		c.readErrs = append(c.readErrs, readErr.Error())
	}
	if decodeErr != nil {
		c.decodeErrs = append(c.decodeErrs, decodeErr.Error())
	}

	// Return one accepted result per generation so the SDK's cardinality
	// check passes and the exporter does not retry, which would hide the
	// behaviour under test.
	resp := exportResponse{Results: make([]exportResult, 0, len(req.Generations))}
	for _, raw := range req.Generations {
		var g struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &g)
		resp.Results = append(resp.Results, exportResult{GenerationID: g.ID, Accepted: true})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (c *exportCapture) assert(t *testing.T, wantCounts []int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.readErrs) > 0 {
		t.Fatalf("read export body errors: %v", c.readErrs)
	}
	if len(c.decodeErrs) > 0 {
		t.Fatalf("decode export body errors: %v", c.decodeErrs)
	}
	if len(c.generationCounts) != len(wantCounts) {
		t.Fatalf("export request count = %d, want %d (counts=%v)", len(c.generationCounts), len(wantCounts), c.generationCounts)
	}
	for i, want := range wantCounts {
		if c.paths[i] != "/api/v1/generations:export" {
			t.Fatalf("export path[%d] = %q, want /api/v1/generations:export", i, c.paths[i])
		}
		if c.generationCounts[i] != want {
			t.Fatalf("export generations[%d] = %d, want %d", i, c.generationCounts[i], want)
		}
	}
}

// generation returns the decoded payload for the given request and
// generation index. Fails the test if either is out of range.
func (c *exportCapture) generation(t *testing.T, requestIdx, genIdx int) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if requestIdx >= len(c.generations) {
		t.Fatalf("request index %d out of range; have %d requests", requestIdx, len(c.generations))
	}
	gens := c.generations[requestIdx]
	if genIdx >= len(gens) {
		t.Fatalf("generation index %d out of range; have %d generations in request %d", genIdx, len(gens), requestIdx)
	}
	var decoded map[string]any
	if err := json.Unmarshal(gens[genIdx], &decoded); err != nil {
		t.Fatalf("decode generation[%d][%d]: %v", requestIdx, genIdx, err)
	}
	return decoded
}

// requireInputContains asserts that the generation's Input contains a
// text part whose text contains substr.
func requireInputContains(t *testing.T, gen map[string]any, substr string) {
	t.Helper()
	messages, ok := gen["input"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("generation Input is empty or wrong shape: %v", gen["input"])
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			if text, _ := part["text"].(string); strings.Contains(text, substr) {
				return
			}
		}
	}
	t.Fatalf("generation Input does not contain text %q; input=%v", substr, gen["input"])
}

// requireToolResultInputContains asserts that the generation's Input
// contains a tool_result part for the given toolCallID whose content
// contains substr.
func requireToolResultInputContains(t *testing.T, gen map[string]any, toolCallID, substr string) {
	t.Helper()
	messages, ok := gen["input"].([]any)
	if !ok || len(messages) == 0 {
		t.Fatalf("generation Input is empty or wrong shape: %v", gen["input"])
	}
	for _, m := range messages {
		msg, ok := m.(map[string]any)
		if !ok {
			continue
		}
		parts, ok := msg["parts"].([]any)
		if !ok {
			continue
		}
		for _, p := range parts {
			part, ok := p.(map[string]any)
			if !ok {
				continue
			}
			tr, ok := part["tool_result"].(map[string]any)
			if !ok {
				continue
			}
			if id, _ := tr["tool_call_id"].(string); id != toolCallID {
				continue
			}
			if content, _ := tr["content"].(string); strings.Contains(content, substr) {
				return
			}
		}
	}
	t.Fatalf("generation Input does not contain tool_result %q with %q; input=%v", toolCallID, substr, gen["input"])
}

func setHookExportEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("SIGIL_ENDPOINT", endpoint)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("SIGIL_AUTH_MODE", "basic")
	t.Setenv("SIGIL_PROTOCOL", "http")
	t.Setenv("SIGIL_USER_ID", "test-user")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
}

func runClaudeHook(t *testing.T, input hookPayload) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	if err := claudecode.Hook(context.Background(), bytes.NewReader(payload), io.Discard, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	return logs.String()
}

func buildUserJSONL(sessionID, text string) string {
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

func buildAssistantJSONL(sessionID, requestID, model string, outputTokens int, text string) string {
	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:01:00Z",
		"version":   "1.0.0",
		"gitBranch": "main",
		"cwd":       "/projects/test",
		"requestId": requestID,
		"message": map[string]any{
			"model": model,
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  50,
				"output_tokens": outputTokens,
			},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

// buildAssistantToolUseJSONL produces a completed assistant tool_use
// transcript line (stop_reason "tool_use").
func buildAssistantToolUseJSONL(sessionID, requestID, toolCallID, toolName, inputJSON string) string {
	var input any
	if err := json.Unmarshal([]byte(inputJSON), &input); err != nil {
		input = map[string]any{}
	}
	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:01:00Z",
		"version":   "1.0.0",
		"gitBranch": "main",
		"cwd":       "/projects/test",
		"requestId": requestID,
		"message": map[string]any{
			"model": "claude-sonnet-4-20250514",
			"content": []map[string]any{
				{"type": "tool_use", "id": toolCallID, "name": toolName, "input": input},
			},
			"stop_reason": "tool_use",
			"usage": map[string]any{
				"input_tokens":  50,
				"output_tokens": 25,
			},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

// buildToolResultJSONL produces a user tool_result transcript line.
func buildToolResultJSONL(sessionID, toolCallID, content string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:02:00Z",
		"version":   "1.0.0",
		"message": map[string]any{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": toolCallID, "content": content},
			},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}
