package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
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
	gens, _ := Process(coalesced, &st, Options{SessionID: "sess-integration"}, redact.New())
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
	gens3, _ := Process(coalesced3, &st2, Options{SessionID: "sess-integration"}, nil)
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
	_, _ = Process(coalesced, &st, Options{SessionID: "sess-title"}, nil)
	st.Offset = offset
	if err := state.Save(sessionID, st); err != nil {
		t.Fatal(err)
	}

	st2 := state.Load(sessionID)
	if st2.Title != "explain generics" {
		t.Errorf("Title not persisted: got %q", st2.Title)
	}
}

// The user lines below are copied from real Claude Code 2.1 transcripts, with
// paths and prompts shortened. Each one is text Claude Code writes ahead of, or
// instead of, the prompt the user typed.
const (
	localCommandCaveatJSONL = `{"type":"user","isMeta":true,"sessionId":"sess-title","timestamp":"2025-06-01T12:00:00Z","message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"}}`
	modelCommandJSONL       = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:01Z","message":{"role":"user","content":"<command-name>/model</command-name>\n            <command-message>model</command-message>\n            <command-args>opus</command-args>"}}`
	modelCommandStdoutJSONL = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:02Z","message":{"role":"user","content":"<local-command-stdout>Set model to Opus 5.5 and saved as your default for new sessions</local-command-stdout>"}}`
	doctorCommandJSONL      = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:00Z","message":{"role":"user","content":"<command-message>doctor</command-message>\n<command-name>/doctor</command-name>"}}`
	doctorExpansionJSONL    = `{"type":"user","isMeta":true,"sessionId":"sess-title","timestamp":"2025-06-01T12:00:01Z","message":{"role":"user","content":[{"type":"text","text":"# Claude Code Doctor\n\nHealth-check my Claude Code setup and fix what's wrong."}]}}`
	ideOpenedFileJSONL      = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:00Z","message":{"role":"user","content":[{"type":"text","text":"<ide_opened_file>The user opened the file /projects/test/main.go in the IDE. This may or may not be related to the current task.</ide_opened_file>"},{"type":"text","text":"explain this file"}]}}`
	ideSelectionJSONL       = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:00Z","message":{"role":"user","content":[{"type":"text","text":"<ide_selection>The user selected the lines 31 to 31 from /projects/test/main.go:\nreturn nil\n\nThis may or may not be related to the current task.</ide_selection>"},{"type":"text","text":"why does this return nil?"}]}}`
	interruptedJSONL        = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:02Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}}`
	apiErrorJSONL           = `{"type":"assistant","isApiErrorMessage":true,"error":"server_error","requestId":"req-err","sessionId":"sess-title","timestamp":"2025-06-01T12:00:02Z","message":{"model":"<synthetic>","role":"assistant","type":"message","content":[{"type":"text","text":"API Error: 529 Overloaded. This is a server-side issue, usually temporary."}],"stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}}}`

	// Bash mode lines are not in a captured transcript. Their tags come from
	// the Claude Code 2.1 binary, which parses them with
	// ^<bash-input>([\s\S]*?)<\/bash-input>.
	bashInputJSONL  = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:00Z","message":{"role":"user","content":"<bash-input>git status</bash-input>"}}`
	bashOutputJSONL = `{"type":"user","sessionId":"sess-title","timestamp":"2025-06-01T12:00:01Z","message":{"role":"user","content":"<bash-stdout>On branch main</bash-stdout><bash-stderr></bash-stderr>"}}`
)

// Regression: the title was the first user text in the transcript, so a
// session opened with /model, a slash command, or an IDE file or selection was
// listed under Claude Code's own markup instead of the prompt.
func TestIntegration_ConversationTitleSkipsClaudeCodeText(t *testing.T) {
	answer := buildAssistantJSONL("sess-title", "req-1", "claude-sonnet-4-20250514", 10, "ok")
	tests := []struct {
		name  string
		lines []string
		want  string
	}{
		{
			name:  "local command before the first prompt",
			lines: []string{localCommandCaveatJSONL, modelCommandJSONL, modelCommandStdoutJSONL, buildUserJSONL("sess-title", "hello"), answer},
			want:  "hello",
		},
		{
			name:  "slash command the model answers",
			lines: []string{doctorCommandJSONL, doctorExpansionJSONL, answer},
			want:  "/doctor",
		},
		{
			name:  "IDE opened file ahead of the prompt",
			lines: []string{ideOpenedFileJSONL, answer},
			want:  "explain this file",
		},
		{
			name:  "IDE selection ahead of the prompt",
			lines: []string{ideSelectionJSONL, answer},
			want:  "why does this return nil?",
		},
		{
			name:  "IDE context and prompt in one text",
			lines: []string{buildUserJSONL("sess-title", "<ide_opened_file>The user opened the file /projects/test/main.go in the IDE.</ide_opened_file>\nexplain this file"), answer},
			want:  "explain this file",
		},
		{
			name:  "slash command interrupted before the answer",
			lines: []string{doctorCommandJSONL, doctorExpansionJSONL, interruptedJSONL, buildUserJSONL("sess-title", "hello"), answer},
			want:  "hello",
		},
		{
			name:  "slash command answered only by an API error",
			lines: []string{doctorCommandJSONL, doctorExpansionJSONL, apiErrorJSONL, buildUserJSONL("sess-title", "hello"), answer},
			want:  "hello",
		},
		{
			name:  "shell command before the first prompt",
			lines: []string{localCommandCaveatJSONL, bashInputJSONL, bashOutputJSONL, buildUserJSONL("sess-title", "hello"), answer},
			want:  "hello",
		},
		{
			name:  "whitespace-only text before the prompt",
			lines: []string{buildUserJSONL("sess-title", " \n"), buildUserJSONL("sess-title", "fix the bug"), answer},
			want:  "fix the bug",
		},
	}
	// Live capture and history import coalesce the same lines differently;
	// the title must not depend on which one ran.
	coalescers := []struct {
		name     string
		coalesce func([]transcript.Line) []transcript.Line
	}{
		{"live", func(lines []transcript.Line) []transcript.Line { out, _ := Coalesce(lines); return out }},
		{"import", CoalesceSession},
	}
	for _, tt := range tests {
		for _, c := range coalescers {
			t.Run(tt.name+"/"+c.name, func(t *testing.T) {
				jsonlPath := filepath.Join(t.TempDir(), "title.jsonl")
				if err := os.WriteFile(jsonlPath, []byte(strings.Join(tt.lines, "\n")+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				lines, _, err := transcript.Read(jsonlPath, 0)
				if err != nil {
					t.Fatal(err)
				}
				st := state.Session{}
				gens, _ := Process(c.coalesce(lines), &st, Options{SessionID: "sess-title"}, nil)
				if len(gens) != 1 {
					t.Fatalf("got %d generations, want 1", len(gens))
				}
				if gens[0].ConversationTitle != tt.want {
					t.Errorf("ConversationTitle = %q, want %q", gens[0].ConversationTitle, tt.want)
				}
			})
		}
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

	gens, _ := Process(coalesced, st, Options{SessionID: "sess-stream"}, nil)
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
