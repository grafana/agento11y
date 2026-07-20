package mapper

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

func makeAssistantLine(model string, tokens int64, content []transcript.ContentBlock, stopReason string) transcript.Line {
	msg := transcript.AssistantMessage{
		Model:      model,
		Content:    content,
		StopReason: stopReason,
		Usage:      transcript.Usage{InputTokens: 100, OutputTokens: tokens, CacheReadInputTokens: 50},
	}
	raw, _ := json.Marshal(msg)
	return transcript.Line{
		Type:      "assistant",
		SessionID: "sess-1",
		Timestamp: "2025-06-01T12:00:00Z",
		Version:   "1.0.0",
		GitBranch: "main",
		CWD:       "/projects/test",
		RequestID: "req-1",
		Message:   raw,
	}
}

func makeAssistantFragment(requestID string, tokens int64, content []transcript.ContentBlock, stopReason string) transcript.Line {
	msg := transcript.AssistantMessage{
		Model:      "claude-sonnet-4-20250514",
		Content:    content,
		StopReason: stopReason,
		Usage:      transcript.Usage{InputTokens: 100, OutputTokens: tokens, CacheReadInputTokens: 50},
	}
	raw, _ := json.Marshal(msg)
	return transcript.Line{
		Type:      "assistant",
		SessionID: "sess-1",
		Timestamp: "2025-06-01T12:00:00Z",
		Version:   "1.0.0",
		RequestID: requestID,
		EndOffset: 100, // placeholder
		Message:   raw,
	}
}

func makeUserLine(content string) transcript.Line {
	msg := transcript.UserMessage{Role: "user", Content: json.RawMessage(`"` + content + `"`)}
	raw, _ := json.Marshal(msg)
	return transcript.Line{
		Type:      "user",
		SessionID: "sess-1",
		Timestamp: "2025-06-01T11:59:00Z",
		EndOffset: 50,
		Message:   raw,
	}
}

func makeToolResultLine(toolUseID, content string) transcript.Line {
	return makeMultiToolResultLine(map[string]string{toolUseID: content})
}

func makeMultiToolResultLine(results map[string]string) transcript.Line {
	var blocks []transcript.UserContentBlock
	for id, content := range results {
		contentJSON, _ := json.Marshal(content)
		blocks = append(blocks, transcript.UserContentBlock{Type: "tool_result", ToolUseID: id, RawContent: contentJSON})
	}
	blocksJSON, _ := json.Marshal(blocks)
	msg := transcript.UserMessage{Role: "user", Content: blocksJSON}
	raw, _ := json.Marshal(msg)
	return transcript.Line{
		Type:      "user",
		SessionID: "sess-1",
		EndOffset: 100,
		Message:   raw,
	}
}

func TestProcess_SinglePromptResponse(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("What is Go?"),
		makeAssistantLine("claude-sonnet-4-20250514", 50, []transcript.ContentBlock{
			{Type: "text", Text: "Go is a programming language."},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, nil)

	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}

	gen := gens[0]
	if gen.ConversationID != "sess-1" {
		t.Errorf("ConversationID = %q", gen.ConversationID)
	}
	if gen.Model.Provider != "anthropic" || gen.Model.Name != "claude-sonnet-4-20250514" {
		t.Errorf("Model = %+v", gen.Model)
	}
	if gen.ResponseID != "req-1" {
		t.Errorf("ResponseID = %q", gen.ResponseID)
	}
	if gen.ResponseModel != "claude-sonnet-4-20250514" {
		t.Errorf("ResponseModel = %q", gen.ResponseModel)
	}
	if gen.Usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d", gen.Usage.OutputTokens)
	}
	if gen.Usage.TotalTokens != gen.Usage.InputTokens+gen.Usage.OutputTokens {
		t.Errorf("TotalTokens = %d, want %d", gen.Usage.TotalTokens, gen.Usage.InputTokens+gen.Usage.OutputTokens)
	}
	if gen.StopReason != "end_turn" {
		t.Errorf("StopReason = %q", gen.StopReason)
	}
	if gen.AgentName != "claude-code" {
		t.Errorf("AgentName = %q", gen.AgentName)
	}
	if gen.AgentVersion != "1.0.0" {
		t.Errorf("AgentVersion = %q", gen.AgentVersion)
	}
	if gen.Mode != agento11y.GenerationModeSync {
		t.Errorf("Mode = %q", gen.Mode)
	}
}

func TestProcess_SkippedLines(t *testing.T) {
	tests := []struct {
		name  string
		lines []transcript.Line
	}{
		{
			name: "zero output tokens",
			lines: []transcript.Line{
				makeAssistantLine("claude-sonnet-4-20250514", 0, []transcript.ContentBlock{}, "end_turn"),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &state.Session{}
			gens := Process(tt.lines, st, Options{SessionID: "sess-1"}, nil)
			if len(gens) != 0 {
				t.Errorf("got %d generations, want 0", len(gens))
			}
		})
	}
}

func TestProcess_SubagentTag(t *testing.T) {
	line := makeAssistantLine("claude-sonnet-4-20250514", 50, []transcript.ContentBlock{
		{Type: "text", Text: "subagent response"},
	}, "end_turn")
	line.IsSidechain = true

	st := &state.Session{}
	gens := Process([]transcript.Line{line}, st, Options{SessionID: "sess-1"}, nil)

	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1 (sidechain should not be skipped)", len(gens))
	}
	if gens[0].Tags["subagent"] != "true" {
		t.Errorf("missing subagent tag, tags = %v", gens[0].Tags)
	}
}

func TestProcess_ContentModes(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("explain concurrency"),
		makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
			{Type: "text", Text: "Concurrency is..."},
		}, "end_turn"),
	}

	tests := []struct {
		name       string
		redactor   *redact.Redactor
		wantOutput string
	}{
		{"without redactor", nil, "Concurrency is..."},
		{"with redactor", redact.New(), "Concurrency is..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &state.Session{}
			gens := Process(lines, st, Options{SessionID: "sess-1"}, tt.redactor)
			if len(gens) != 1 {
				t.Fatal("expected 1 generation")
			}
			if gens[0].Input == nil {
				t.Error("expected Input to be present")
			}
			if gens[0].Output == nil {
				t.Fatal("expected Output to be present")
			}
			if gens[0].Output[0].Parts[0].Text != tt.wantOutput {
				t.Errorf("Output text = %q, want %q", gens[0].Output[0].Parts[0].Text, tt.wantOutput)
			}
		})
	}
}

func TestProcess_ConversationTitle(t *testing.T) {
	tests := []struct {
		name          string
		state         state.Session
		lines         []transcript.Line
		wantTitle     string
		wantConvTitle string
		wantGenCnt    int
	}{
		{
			name:  "title from first prompt",
			state: state.Session{},
			lines: []transcript.Line{
				makeUserLine("fix the auth bug"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "text", Text: "ok"},
				}, "end_turn"),
				makeUserLine("also update the tests"),
				makeAssistantLine("claude-sonnet-4-20250514", 20, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantTitle:     "fix the auth bug",
			wantConvTitle: "fix the auth bug",
			wantGenCnt:    2,
		},
		{
			name:  "preserves existing title",
			state: state.Session{Title: "old title"},
			lines: []transcript.Line{
				makeUserLine("new prompt"),
				makeAssistantLine("claude-sonnet-4-20250514", 10, []transcript.ContentBlock{
					{Type: "text", Text: "ok"},
				}, "end_turn"),
			},
			wantTitle:     "old title",
			wantConvTitle: "old title",
			wantGenCnt:    1,
		},
		{
			name:  "falls back to session ID without user lines",
			state: state.Session{},
			lines: []transcript.Line{
				makeAssistantLine("claude-sonnet-4-20250514", 10, []transcript.ContentBlock{
					{Type: "text", Text: "hi"},
				}, "end_turn"),
			},
			wantTitle:     "",
			wantConvTitle: "sess-1",
			wantGenCnt:    1,
		},
		{
			name:  "truncates long prompt to 100 chars",
			state: state.Session{},
			lines: []transcript.Line{
				makeUserLine(strings.Repeat("a", 200)),
				makeAssistantLine("claude-sonnet-4-20250514", 10, []transcript.ContentBlock{
					{Type: "text", Text: "ok"},
				}, "end_turn"),
			},
			wantTitle:     strings.Repeat("a", 200),
			wantConvTitle: strings.Repeat("a", 100),
			wantGenCnt:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := tt.state
			gens := Process(tt.lines, &st, Options{SessionID: "sess-1"}, nil)

			if st.Title != tt.wantTitle {
				t.Errorf("state.Title = %q, want %q", st.Title, tt.wantTitle)
			}
			if len(gens) != tt.wantGenCnt {
				t.Fatalf("got %d generations, want %d", len(gens), tt.wantGenCnt)
			}
			for i, gen := range gens {
				if gen.ConversationTitle != tt.wantConvTitle {
					t.Errorf("gen[%d].ConversationTitle = %q, want %q", i, gen.ConversationTitle, tt.wantConvTitle)
				}
			}
		})
	}
}

func TestProcess_ToolUses(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("read file.go"),
		makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
			{Type: "text", Text: "Let me read that."},
			{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"file.go"}`)},
		}, "tool_use"),
		makeToolResultLine("tu_1", "package main\nfunc main() {}"),
		makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
			{Type: "text", Text: "The file contains a main package."},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, nil)

	if len(gens) != 2 {
		t.Fatalf("got %d generations, want 2", len(gens))
	}
	if len(gens[0].Tools) != 1 || gens[0].Tools[0].Name != "Read" {
		t.Errorf("gen[0].Tools = %+v", gens[0].Tools)
	}
}

func TestProcess_DeduplicatedTools(t *testing.T) {
	lines := []transcript.Line{
		makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
			{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "tu_2", Name: "Read", Input: json.RawMessage(`{}`)},
			{Type: "tool_use", ID: "tu_3", Name: "Write", Input: json.RawMessage(`{}`)},
		}, "tool_use"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, nil)

	if len(gens[0].Tools) != 2 {
		t.Fatalf("got %d tools, want 2 (deduplicated)", len(gens[0].Tools))
	}
	if gens[0].Tools[0].Name != "Read" || gens[0].Tools[1].Name != "Write" {
		t.Errorf("tools = %v", gens[0].Tools)
	}
}

func TestProcess_ThinkingEnabled(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("think about this"),
		makeAssistantLine("claude-sonnet-4-20250514", 50, []transcript.ContentBlock{
			{Type: "thinking", Text: "Let me think..."},
			{Type: "text", Text: "Here's my answer."},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, nil)

	if len(gens) != 1 {
		t.Fatal("expected 1 generation")
	}
	if gens[0].ThinkingEnabled == nil || *gens[0].ThinkingEnabled != true {
		t.Error("expected ThinkingEnabled to be true")
	}
}

func TestProcess_ContentCaptureRedaction(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("use token glc_abcdefghijklmnopqrstuvwx"),
		makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
			{Type: "text", Text: "Found token glc_abcdefghijklmnopqrstuvwx in the code"},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, redact.New())

	gen := gens[0]
	// User prompt gets Tier 1 redaction
	if !strings.Contains(gen.Input[0].Parts[0].Text, "[REDACTED:grafana-cloud-token]") {
		t.Errorf("user prompt was NOT redacted: %q", gen.Input[0].Parts[0].Text)
	}
	// Assistant text also has Tier 1 redaction
	if !strings.Contains(gen.Output[0].Parts[0].Text, "[REDACTED:grafana-cloud-token]") {
		t.Error("assistant text was NOT redacted")
	}
}

func TestProcess_Tags(t *testing.T) {
	tests := []struct {
		name      string
		branch    string
		cwd       string
		entry     string
		extras    map[string]string
		wantNil   bool
		wantCount int
		wantTags  map[string]string // optional: assert specific key/value pairs
	}{
		{name: "all set", branch: "feature/auth", cwd: "/project", entry: "cli", wantCount: 3},
		{name: "all empty", wantNil: true},
		{name: "partial", branch: "main", wantCount: 1},
		{
			name:      "extras merged with built-ins",
			branch:    "main",
			extras:    map[string]string{"account": "work", "env": "dev"},
			wantCount: 3,
			wantTags:  map[string]string{"git.branch": "main", "account": "work", "env": "dev"},
		},
		{
			name:      "extras only, no built-ins",
			extras:    map[string]string{"account": "personal"},
			wantCount: 1,
			wantTags:  map[string]string{"account": "personal"},
		},
		{
			name:      "built-in wins on collision",
			branch:    "main",
			extras:    map[string]string{"git.branch": "user-override", "account": "work"},
			wantCount: 2,
			wantTags:  map[string]string{"git.branch": "main", "account": "work"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			line := makeAssistantLine("claude-sonnet-4-20250514", 10, []transcript.ContentBlock{
				{Type: "text", Text: "hi"},
			}, "end_turn")
			line.GitBranch = tt.branch
			line.CWD = tt.cwd
			line.Entrypoint = tt.entry

			st := &state.Session{}
			gens := Process([]transcript.Line{line}, st, Options{SessionID: "sess-1", ExtraTags: tt.extras}, nil)

			tags := gens[0].Tags
			if (tags == nil) != tt.wantNil {
				t.Errorf("tags nil = %v, want %v", tags == nil, tt.wantNil)
			}
			if len(tags) != tt.wantCount {
				t.Errorf("tags count = %d, want %d (got %v)", len(tags), tt.wantCount, tags)
			}
			for k, v := range tt.wantTags {
				if tags[k] != v {
					t.Errorf("tags[%q] = %q, want %q", k, tags[k], v)
				}
			}
		})
	}
}

func TestProcess_ToolResultsInInput(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("read file.go"),
		makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
			{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{"path":"file.go"}`)},
		}, "tool_use"),
		makeToolResultLine("tu_1", "package main"),
		makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
			{Type: "text", Text: "The file has a main package."},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, redact.New())

	if len(gens) != 2 {
		t.Fatalf("got %d gens, want 2", len(gens))
	}

	// First gen: input is user prompt (redacted)
	if gens[0].Input[0].Parts[0].Kind != agento11y.PartKindText {
		t.Errorf("gen[0] input kind = %q, want text", gens[0].Input[0].Parts[0].Kind)
	}

	// Second gen: input should be tool results
	if len(gens[1].Input) == 0 {
		t.Fatal("gen[1] has no input")
	}
	if gens[1].Input[0].Parts[0].Kind != agento11y.PartKindToolResult {
		t.Errorf("input kind = %q, want tool_result", gens[1].Input[0].Parts[0].Kind)
	}
}

func TestCoalesce(t *testing.T) {
	tests := []struct {
		name       string
		lines      func() []transcript.Line
		wantLines  int
		wantOffset int64
	}{
		{
			name: "single complete line",
			lines: func() []transcript.Line {
				l := makeAssistantFragment("req-1", 50, []transcript.ContentBlock{
					{Type: "text", Text: "hello"},
				}, "end_turn")
				l.EndOffset = 200
				return []transcript.Line{l}
			},
			wantLines:  1,
			wantOffset: 200,
		},
		{
			name: "excludes incomplete trailing group",
			lines: func() []transcript.Line {
				user := makeUserLine("hello")
				user.EndOffset = 50
				complete := makeAssistantFragment("req-1", 50, []transcript.ContentBlock{
					{Type: "text", Text: "hi"},
				}, "end_turn")
				complete.EndOffset = 150
				incomplete := makeAssistantFragment("req-2", 10, []transcript.ContentBlock{
					{Type: "thinking", Text: "..."},
				}, "")
				incomplete.EndOffset = 250
				return []transcript.Line{user, complete, incomplete}
			},
			wantLines:  2,
			wantOffset: 150,
		},
		{
			name: "multiple requests with interleaved user lines",
			lines: func() []transcript.Line {
				user := makeUserLine("hello")
				user.EndOffset = 50
				f1a := makeAssistantFragment("req-1", 10, []transcript.ContentBlock{
					{Type: "thinking", Text: "..."},
				}, "")
				f1a.EndOffset = 150
				f1b := makeAssistantFragment("req-1", 100, []transcript.ContentBlock{
					{Type: "text", Text: "response"},
				}, "end_turn")
				f1b.EndOffset = 250
				tr := makeToolResultLine("tu_1", "ok")
				tr.EndOffset = 350
				f2 := makeAssistantFragment("req-2", 50, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn")
				f2.EndOffset = 450
				return []transcript.Line{user, f1a, f1b, tr, f2}
			},
			wantLines:  4,
			wantOffset: 450,
		},
		{
			name: "does not consume trailing user prompt",
			lines: func() []transcript.Line {
				user1 := makeUserLine("first")
				user1.EndOffset = 50
				asst := makeAssistantFragment("req-1", 50, []transcript.ContentBlock{
					{Type: "text", Text: "hi"},
				}, "end_turn")
				asst.EndOffset = 150
				user2 := makeUserLine("second")
				user2.EndOffset = 200
				return []transcript.Line{user1, asst, user2}
			},
			wantLines:  2,
			wantOffset: 150,
		},
		{
			name: "does not consume trailing tool result",
			lines: func() []transcript.Line {
				user := makeUserLine("read file")
				user.EndOffset = 50
				asst := makeAssistantFragment("req-1", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{}`)},
				}, "tool_use")
				asst.EndOffset = 150
				tr := makeToolResultLine("tu_1", "ok")
				tr.EndOffset = 250
				return []transcript.Line{user, asst, tr}
			},
			wantLines:  2,
			wantOffset: 150,
		},
		{
			name: "complete assistant without request id is safe before trailing context",
			lines: func() []transcript.Line {
				user1 := makeUserLine("first")
				user1.EndOffset = 50
				asst := makeAssistantFragment("", 50, []transcript.ContentBlock{
					{Type: "text", Text: "hi"},
				}, "end_turn")
				asst.UUID = "assistant-without-request-id"
				asst.EndOffset = 150
				user2 := makeUserLine("second")
				user2.EndOffset = 200
				return []transcript.Line{user1, asst, user2}
			},
			wantLines:  2,
			wantOffset: 150,
		},
		{
			name: "incomplete assistant without request id is not safe",
			lines: func() []transcript.Line {
				user := makeUserLine("hello")
				user.EndOffset = 50
				asst := makeAssistantFragment("", 10, []transcript.ContentBlock{
					{Type: "thinking", Text: "..."},
				}, "")
				asst.UUID = "incomplete-assistant-without-request-id"
				asst.EndOffset = 150
				return []transcript.Line{user, asst}
			},
			wantLines:  0,
			wantOffset: 0,
		},
		{
			name: "trailing user prompt with no prior assistant",
			lines: func() []transcript.Line {
				user := makeUserLine("hello")
				user.EndOffset = 100
				return []transcript.Line{user}
			},
			wantLines:  0,
			wantOffset: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, offset := Coalesce(tt.lines())
			if len(lines) != tt.wantLines {
				t.Fatalf("got %d lines, want %d", len(lines), tt.wantLines)
			}
			if offset != tt.wantOffset {
				t.Errorf("offset = %d, want %d", offset, tt.wantOffset)
			}
		})
	}
}

func TestCoalesce_MergesFragmentContent(t *testing.T) {
	frag1 := makeAssistantFragment("req-1", 26, []transcript.ContentBlock{
		{Type: "thinking", Text: "Let me think..."},
	}, "")
	frag1.EndOffset = 100
	frag2 := makeAssistantFragment("req-1", 26, []transcript.ContentBlock{
		{Type: "tool_use", ID: "tu_1", Name: "Read", Input: json.RawMessage(`{}`)},
	}, "")
	frag2.EndOffset = 200
	frag3 := makeAssistantFragment("req-1", 611, []transcript.ContentBlock{
		{Type: "tool_use", ID: "tu_2", Name: "Write", Input: json.RawMessage(`{}`)},
	}, "tool_use")
	frag3.EndOffset = 300

	lines, _ := Coalesce([]transcript.Line{frag1, frag2, frag3})
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}

	var msg transcript.AssistantMessage
	if err := json.Unmarshal(lines[0].Message, &msg); err != nil {
		t.Fatal(err)
	}
	if len(msg.Content) != 3 {
		t.Fatalf("got %d content blocks, want 3", len(msg.Content))
	}
	if msg.Content[0].Type != "thinking" {
		t.Errorf("block[0].Type = %q, want thinking", msg.Content[0].Type)
	}
	if msg.Usage.OutputTokens != 611 {
		t.Errorf("OutputTokens = %d, want 611 (from final fragment)", msg.Usage.OutputTokens)
	}
	if msg.StopReason != "tool_use" {
		t.Errorf("StopReason = %q, want tool_use", msg.StopReason)
	}
}

func TestCoalesce_PreservesContextWhenAssistantArrivesInLaterBatch(t *testing.T) {
	user1 := makeUserLine("user1 prompt")
	user1.EndOffset = 50
	asst1 := makeAssistantFragment("req-1", 30, []transcript.ContentBlock{
		{Type: "text", Text: "hi"},
	}, "end_turn")
	asst1.EndOffset = 150
	user2 := makeUserLine("user2 prompt")
	user2.EndOffset = 200

	// First batch: trailing user2 must not advance the safe offset past asst1.
	firstLines, firstOffset := Coalesce([]transcript.Line{user1, asst1, user2})
	if len(firstLines) != 2 {
		t.Fatalf("first Coalesce returned %d lines, want 2", len(firstLines))
	}
	if firstOffset != 150 {
		t.Fatalf("first Coalesce offset = %d, want 150 (end of asst1)", firstOffset)
	}

	// Second batch: starts at firstOffset, so user2 is re-read with the new
	// completed assistant response.
	asst2 := makeAssistantFragment("req-2", 30, []transcript.ContentBlock{
		{Type: "text", Text: "ok"},
	}, "end_turn")
	asst2.EndOffset = 300

	secondLines, secondOffset := Coalesce([]transcript.Line{user2, asst2})
	if len(secondLines) != 2 {
		t.Fatalf("second Coalesce returned %d lines, want 2", len(secondLines))
	}
	if secondOffset != 300 {
		t.Fatalf("second Coalesce offset = %d, want 300", secondOffset)
	}

	gens := Process(secondLines, &state.Session{}, Options{SessionID: "sess-1"}, nil)
	if len(gens) != 1 {
		t.Fatalf("got %d generations, want 1", len(gens))
	}
	if len(gens[0].Input) == 0 || len(gens[0].Input[0].Parts) == 0 {
		t.Fatalf("gen[0].Input is empty: %+v", gens[0].Input)
	}
	if gens[0].Input[0].Parts[0].Text != "user2 prompt" {
		t.Errorf("gen[0].Input text = %q, want %q", gens[0].Input[0].Parts[0].Text, "user2 prompt")
	}
}

// --- Polymorphic tool_result content tests ---

func TestProcess_ToolResultContentFormats(t *testing.T) {
	tests := []struct {
		name        string
		rawContent  json.RawMessage
		wantContain string
	}{
		{
			name:        "string content",
			rawContent:  json.RawMessage(`"package main"`),
			wantContain: "package main",
		},
		{
			name:        "array content blocks",
			rawContent:  json.RawMessage(`[{"type":"text","text":"## Result\nFound 3 files"}]`),
			wantContain: "Found 3 files",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blocks := []transcript.UserContentBlock{{
				Type:       "tool_result",
				ToolUseID:  "tu_1",
				RawContent: tt.rawContent,
			}}
			blocksJSON, _ := json.Marshal(blocks)
			msg := transcript.UserMessage{Role: "user", Content: blocksJSON}
			raw, _ := json.Marshal(msg)
			toolResultLine := transcript.Line{
				Type:      "user",
				SessionID: "sess-1",
				Message:   raw,
			}

			lines := []transcript.Line{
				makeUserLine("do something"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "tu_1", Name: "Grep", Input: json.RawMessage(`{}`)},
				}, "tool_use"),
				toolResultLine,
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "Done."},
				}, "end_turn"),
			}

			st := &state.Session{}
			gens := Process(lines, st, Options{SessionID: "sess-1"}, redact.New())

			if len(gens) != 2 {
				t.Fatalf("got %d gens, want 2", len(gens))
			}
			if len(gens[1].Input) == 0 {
				t.Fatal("gen[1] has no input")
			}
			if gens[1].Input[0].Parts[0].Kind != agento11y.PartKindToolResult {
				t.Errorf("gen[1] input kind = %q, want tool_result", gens[1].Input[0].Parts[0].Kind)
			}
			content := gens[1].Input[0].Parts[0].ToolResult.Content
			if !strings.Contains(content, tt.wantContain) {
				t.Errorf("tool result content = %q, want to contain %q", content, tt.wantContain)
			}
		})
	}
}

func TestTruncateJSON(t *testing.T) {
	tests := []struct {
		name          string
		input         json.RawMessage
		maxLen        int
		wantUnchanged bool
		wantTruncated bool
		wantValidJSON bool
	}{
		{
			name:          "no truncation needed",
			input:         json.RawMessage(`{"path":"file.go"}`),
			maxLen:        4096,
			wantUnchanged: true,
			wantValidJSON: true,
		},
		{
			name:          "truncates large input",
			input:         json.RawMessage(`"` + strings.Repeat("a", 5000) + `"`),
			maxLen:        4096,
			wantTruncated: true,
			wantValidJSON: true,
		},
		{
			name:          "UTF-8 safety on truncation boundary",
			input:         json.RawMessage(`"` + strings.Repeat("é", 2048) + strings.Repeat("x", 2048) + `"`),
			maxLen:        4096,
			wantTruncated: true,
			wantValidJSON: true,
		},
		{
			name:          "empty input",
			input:         json.RawMessage(``),
			maxLen:        4096,
			wantUnchanged: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateJSON(tt.input, tt.maxLen, nil)
			if tt.wantUnchanged && string(result) != string(tt.input) {
				t.Errorf("input was modified: %s", result)
			}
			if tt.wantTruncated && !strings.Contains(string(result), "truncated") {
				t.Error("expected [truncated] marker")
			}
			if tt.wantValidJSON {
				var v any
				if err := json.Unmarshal(result, &v); err != nil {
					t.Errorf("result is not valid JSON: %v", err)
				}
			}
		})
	}
}

func TestProcess_ParentGenerationIDs(t *testing.T) {
	tests := []struct {
		name         string
		lines        []transcript.Line
		wantGenCount int
		// wantParents maps gen index → parent gen indices. Absent or nil means no parents.
		wantParents map[int][]int
		// wantAgentNames optionally checks AgentName per gen index.
		wantAgentNames map[int]string
	}{
		{
			name: "agent call synthesises subagent generation",
			lines: []transcript.Line{
				makeUserLine("research this"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_1", Name: "Agent", Input: json.RawMessage(`{"description":"test"}`)},
				}, "tool_use"),
				makeToolResultLine("agent_1", "agent result here"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "Based on the agent result..."},
				}, "end_turn"),
			},
			// gen[0] = parent, gen[1] = synthetic subagent, gen[2] = continuation
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/subagent"},
		},
		{
			name: "parallel agent calls produce multiple subagent generations",
			lines: []transcript.Line{
				makeUserLine("run two agents"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_a", Name: "Agent", Input: json.RawMessage(`{"description":"A"}`)},
					{Type: "tool_use", ID: "agent_b", Name: "Agent", Input: json.RawMessage(`{"description":"B"}`)},
				}, "tool_use"),
				makeMultiToolResultLine(map[string]string{"agent_a": "result A", "agent_b": "result B"}),
				makeAssistantLine("claude-sonnet-4-20250514", 50, []transcript.ContentBlock{
					{Type: "text", Text: "Both agents done."},
				}, "end_turn"),
			},
			// gen[0] = parent, gen[1..2] = synthetic subagents, gen[3] = continuation
			wantGenCount: 4,
			wantParents:  map[int][]int{1: {0}, 2: {0}},
		},
		{
			name: "non-agent tool calls do not synthesise",
			lines: []transcript.Line{
				makeUserLine("read a file"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "tu_read", Name: "Read", Input: json.RawMessage(`{"path":"f.go"}`)},
				}, "tool_use"),
				makeToolResultLine("tu_read", "file contents"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "The file says..."},
				}, "end_turn"),
			},
			wantGenCount: 2,
			wantParents:  nil,
		},
		{
			name: "subagent_type lowercase",
			lines: []transcript.Line{
				makeUserLine("explore"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_gp", Name: "Agent", Input: json.RawMessage(`{"subagent_type":"general-purpose","description":"test"}`)},
				}, "tool_use"),
				makeToolResultLine("agent_gp", "result"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/general-purpose"},
		},
		{
			name: "subagent_type PascalCase lowercased",
			lines: []transcript.Line{
				makeUserLine("explore"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_ex", Name: "Agent", Input: json.RawMessage(`{"subagent_type":"Explore","description":"…"}`)},
				}, "tool_use"),
				makeToolResultLine("agent_ex", "result"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/explore"},
		},
		{
			name: "plugin-namespaced subagent_type preserved verbatim",
			lines: []transcript.Line{
				makeUserLine("create agent"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_plug", Name: "Agent", Input: json.RawMessage(`{"subagent_type":"plugin-dev:agent-creator","description":"…"}`)},
				}, "tool_use"),
				makeToolResultLine("agent_plug", "result"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/plugin-dev:agent-creator"},
		},
		{
			name: "empty subagent_type falls back",
			lines: []transcript.Line{
				makeUserLine("call agent"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_empty", Name: "Agent", Input: json.RawMessage(`{"subagent_type":"","description":"test"}`)},
				}, "tool_use"),
				makeToolResultLine("agent_empty", "result"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/subagent"},
		},
		{
			// json.Marshal validates RawMessage syntactically, so genuinely
			// malformed bytes can't survive the assistant-message round-trip.
			// Use a valid-JSON-but-wrong-shape input (array) to exercise the
			// json.Unmarshal error fallback in mapper.go.
			name: "unparseable subagent_type input falls back",
			lines: []transcript.Line{
				makeUserLine("call agent"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "tool_use", ID: "agent_bad", Name: "Agent", Input: json.RawMessage(`[]`)},
				}, "tool_use"),
				makeToolResultLine("agent_bad", "result"),
				makeAssistantLine("claude-sonnet-4-20250514", 40, []transcript.ContentBlock{
					{Type: "text", Text: "done"},
				}, "end_turn"),
			},
			wantGenCount:   3,
			wantParents:    map[int][]int{1: {0}},
			wantAgentNames: map[int]string{1: "claude-code/subagent"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := &state.Session{}
			gens := Process(tt.lines, st, Options{SessionID: "sess-1"}, nil)

			if len(gens) != tt.wantGenCount {
				t.Fatalf("got %d generations, want %d", len(gens), tt.wantGenCount)
			}
			for i, gen := range gens {
				wantIdxs, hasEntry := tt.wantParents[i]
				if !hasEntry {
					if gen.ParentGenerationIDs != nil {
						t.Errorf("gen[%d]: unexpected parent_generation_ids = %v", i, gen.ParentGenerationIDs)
					}
					continue
				}
				if len(gen.ParentGenerationIDs) != len(wantIdxs) {
					t.Fatalf("gen[%d]: got %d parents, want %d", i, len(gen.ParentGenerationIDs), len(wantIdxs))
				}
				for j, idx := range wantIdxs {
					if gen.ParentGenerationIDs[j] != gens[idx].ID {
						t.Errorf("gen[%d].ParentGenerationIDs[%d] = %q, want gen[%d].ID = %q",
							i, j, gen.ParentGenerationIDs[j], idx, gens[idx].ID)
					}
				}
				if wantName, ok := tt.wantAgentNames[i]; ok && gen.AgentName != wantName {
					t.Errorf("gen[%d].AgentName = %q, want %q", i, gen.AgentName, wantName)
				}
			}
		})
	}
}

func TestProcess_EffectiveVersionStableAcrossToolSubsets(t *testing.T) {
	lines := []transcript.Line{
		makeUserLine("first"),
		makeAssistantLine("claude-sonnet-4-20250514", 20, []transcript.ContentBlock{
			{Type: "tool_use", ID: "tu_a", Name: "Read", Input: json.RawMessage(`{"path":"a"}`)},
		}, "end_turn"),
		makeUserLine("second"),
		makeAssistantLine("claude-sonnet-4-20250514", 20, []transcript.ContentBlock{
			{Type: "tool_use", ID: "tu_b", Name: "Bash", Input: json.RawMessage(`{"command":"ls"}`)},
		}, "end_turn"),
	}

	st := &state.Session{}
	gens := Process(lines, st, Options{SessionID: "sess-1"}, nil)

	if len(gens) != 2 {
		t.Fatalf("got %d generations, want 2", len(gens))
	}
	if gens[0].EffectiveVersion == "" {
		t.Fatalf("gen[0].EffectiveVersion is empty; expected raw line.Version")
	}
	if gens[0].EffectiveVersion != gens[1].EffectiveVersion {
		t.Fatalf("EffectiveVersion mismatch across turns: %q vs %q", gens[0].EffectiveVersion, gens[1].EffectiveVersion)
	}
	if gens[0].EffectiveVersion != gens[0].AgentVersion {
		t.Fatalf("EffectiveVersion %q should equal AgentVersion %q", gens[0].EffectiveVersion, gens[0].AgentVersion)
	}
}

func TestProcess_UserPromptRedaction(t *testing.T) {
	tests := []struct {
		name       string
		redactor   *redact.Redactor
		wantRedact bool
	}{
		{"with redactor", redact.New(), true},
		{"without redactor", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines := []transcript.Line{
				makeUserLine("my token is glc_abcdefghijklmnopqrstuvwx"),
				makeAssistantLine("claude-sonnet-4-20250514", 30, []transcript.ContentBlock{
					{Type: "text", Text: "ok"},
				}, "end_turn"),
			}

			st := &state.Session{}
			gens := Process(lines, st, Options{SessionID: "sess-1"}, tt.redactor)

			if gens[0].Input == nil {
				t.Fatal("expected Input to be present")
			}

			input := gens[0].Input[0].Parts[0].Text
			if tt.wantRedact {
				if strings.Contains(input, "glc_abcdefghijklmnopqrstuvwx") {
					t.Errorf("unredacted token in prompt: %q", input)
				}
				if !strings.Contains(input, "[REDACTED:grafana-cloud-token]") {
					t.Errorf("missing redaction marker: %q", input)
				}
			} else {
				if !strings.Contains(input, "glc_abcdefghijklmnopqrstuvwx") {
					t.Errorf("expected raw token in prompt: %q", input)
				}
			}
		})
	}
}
