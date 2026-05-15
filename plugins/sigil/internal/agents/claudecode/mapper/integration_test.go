package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/transcript"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/redact"
)

func TestIntegration_EndToEnd(t *testing.T) {
	tr := buildTestTranscript()
	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "test-session.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(tr), 0o644); err != nil {
		t.Fatal(err)
	}

	stateDir := filepath.Join(dir, "state")
	t.Setenv("XDG_STATE_HOME", stateDir)

	sessionID := "integration-test-session"

	// First run: read everything
	st := state.Load(sessionID)
	if st.Offset != 0 {
		t.Fatal("expected zero offset for new session")
	}

	lines, _, err := transcript.Read(jsonlPath, st.Offset)
	if err != nil {
		t.Fatal(err)
	}

	coalesced, safeOffset := Coalesce(lines)
	gens := Process(coalesced, &st, Options{SessionID: "sess-integration"}, redact.New())
	if len(gens) == 0 {
		t.Fatal("expected at least 1 generation")
	}

	gen := gens[0]
	if gen.ConversationID != "sess-integration" {
		t.Errorf("ConversationID = %q", gen.ConversationID)
	}
	if gen.Model.Provider != "anthropic" {
		t.Errorf("Model.Provider = %q", gen.Model.Provider)
	}
	if gen.AgentName != "claude-code" {
		t.Errorf("AgentName = %q", gen.AgentName)
	}
	if gen.Usage.OutputTokens <= 0 {
		t.Error("expected non-zero output tokens")
	}
	if gen.Usage.TotalTokens != gen.Usage.InputTokens+gen.Usage.OutputTokens {
		t.Errorf("TotalTokens = %d, want %d", gen.Usage.TotalTokens, gen.Usage.InputTokens+gen.Usage.OutputTokens)
	}

	st.Offset = safeOffset
	if err := state.Save(sessionID, st); err != nil {
		t.Fatal(err)
	}

	// Second run: should get no new lines
	st2 := state.Load(sessionID)
	if st2.Offset != safeOffset {
		t.Errorf("offset not persisted: got %d, want %d", st2.Offset, safeOffset)
	}

	lines2, _, err := transcript.Read(jsonlPath, st2.Offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines2) != 0 {
		t.Errorf("second run got %d lines, want 0", len(lines2))
	}

	// Third run after appending: should only get new lines
	f, err := os.OpenFile(jsonlPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	appendLine := buildAssistantJSONL("sess-integration", "req-new", "claude-opus-4-20250514", 200, "second response")
	if _, err := f.WriteString(appendLine + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close() //nolint:errcheck

	lines3, _, err := transcript.Read(jsonlPath, st2.Offset)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines3) != 1 {
		t.Fatalf("third run got %d lines, want 1", len(lines3))
	}

	coalesced3, _ := Coalesce(lines3)
	gens3 := Process(coalesced3, &st2, Options{SessionID: "sess-integration"}, nil)
	if len(gens3) != 1 {
		t.Fatalf("third run produced %d generations, want 1", len(gens3))
	}
	if gens3[0].Model.Name != "claude-opus-4-20250514" {
		t.Errorf("model = %q", gens3[0].Model.Name)
	}
}

func TestIntegration_ConversationTitlePersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	sessionID := "title-test"

	tr := buildUserJSONL("sess-title", "explain generics") + "\n" +
		buildAssistantJSONL("sess-title", "req-1", "claude-sonnet-4-20250514", 50, "Generics allow...") + "\n"
	jsonlPath := filepath.Join(dir, "title.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(tr), 0o644); err != nil {
		t.Fatal(err)
	}

	st := state.Load(sessionID)
	lines, _, _ := transcript.Read(jsonlPath, 0)
	coalesced, offset := Coalesce(lines)
	Process(coalesced, &st, Options{SessionID: "sess-title"}, nil)
	st.Offset = offset
	if err := state.Save(sessionID, st); err != nil {
		t.Fatal(err)
	}

	st2 := state.Load(sessionID)
	if st2.Title != "explain generics" {
		t.Errorf("Title not persisted: got %q", st2.Title)
	}
}

func TestIntegration_StreamingFragments(t *testing.T) {
	// Simulate a streaming response with 3 fragments
	tr := buildUserJSONL("sess-stream", "help me") + "\n" +
		buildAssistantFragmentJSONL("sess-stream", "req-frag", 26, []map[string]any{
			{"type": "thinking", "text": "Let me think..."},
		}, "") + "\n" +
		buildAssistantFragmentJSONL("sess-stream", "req-frag", 26, []map[string]any{
			{"type": "tool_use", "id": "tu_1", "name": "Read", "input": map[string]any{}},
		}, "") + "\n" +
		buildAssistantFragmentJSONL("sess-stream", "req-frag", 500, []map[string]any{
			{"type": "tool_use", "id": "tu_2", "name": "Write", "input": map[string]any{}},
		}, "tool_use") + "\n"

	dir := t.TempDir()
	jsonlPath := filepath.Join(dir, "stream.jsonl")
	if err := os.WriteFile(jsonlPath, []byte(tr), 0o644); err != nil {
		t.Fatal(err)
	}

	st := &state.Session{}
	lines, _, err := transcript.Read(jsonlPath, 0)
	if err != nil {
		t.Fatal(err)
	}

	// Should have 4 raw lines (1 user + 3 assistant fragments)
	if len(lines) != 4 {
		t.Fatalf("got %d raw lines, want 4", len(lines))
	}

	coalesced, _ := Coalesce(lines)
	// Should coalesce to 2 lines (1 user + 1 merged assistant)
	if len(coalesced) != 2 {
		t.Fatalf("got %d coalesced lines, want 2", len(coalesced))
	}

	gens := Process(coalesced, st, Options{SessionID: "sess-stream"}, nil)
	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}

	// Should have correct usage from final fragment
	if gens[0].Usage.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", gens[0].Usage.OutputTokens)
	}

	// Should have tools from all fragments
	if len(gens[0].Tools) != 2 {
		t.Errorf("got %d tools, want 2 (Read + Write)", len(gens[0].Tools))
	}

	// Should detect thinking
	if gens[0].ThinkingEnabled == nil || !*gens[0].ThinkingEnabled {
		t.Error("expected ThinkingEnabled to be true")
	}
}

func buildTestTranscript() string {
	return buildUserJSONL("sess-integration", "What is Go?") + "\n" +
		buildAssistantJSONL("sess-integration", "req-1", "claude-sonnet-4-20250514", 100, "Go is a statically typed language.") + "\n"
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
				"input_tokens":  500,
				"output_tokens": outputTokens,
			},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func buildAssistantFragmentJSONL(sessionID, requestID string, outputTokens int, content []map[string]any, stopReason string) string {
	msg := map[string]any{
		"model":   "claude-sonnet-4-20250514",
		"content": content,
		"usage": map[string]any{
			"input_tokens":  100,
			"output_tokens": outputTokens,
		},
	}
	if stopReason != "" {
		msg["stop_reason"] = stopReason
	}

	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:01:00Z",
		"version":   "1.0.0",
		"requestId": requestID,
		"message":   msg,
	}
	data, _ := json.Marshal(line)
	return string(data)
}
