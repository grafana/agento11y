package history

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
)

type opencodeFixture struct {
	Sessions []struct {
		ID        string `json:"id"`
		ParentID  string `json:"parentID"`
		Title     string `json:"title"`
		Directory string `json:"directory"`
		Created   int64  `json:"created"`
		Updated   int64  `json:"updated"`
	} `json:"sessions"`
	Messages []struct {
		ID        string          `json:"id"`
		SessionID string          `json:"sessionID"`
		Created   int64           `json:"created"`
		Data      json.RawMessage `json:"data"`
	} `json:"messages"`
	Parts []struct {
		ID        string          `json:"id"`
		MessageID string          `json:"messageID"`
		SessionID string          `json:"sessionID"`
		Data      json.RawMessage `json:"data"`
	} `json:"parts"`
}

func loadOpenCodeFixture(t *testing.T, name string) (*opencodeImporter, string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "opencode", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fixture opencodeFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	db := sessiondbtest.New(t)
	for _, row := range fixture.Sessions {
		db.AddSession(row.ID, row.ParentID, row.Title, row.Directory, row.Created, row.Updated)
	}
	for _, row := range fixture.Messages {
		db.AddMessage(row.ID, row.SessionID, row.Created, row.Created+1, string(row.Data))
	}
	for _, row := range fixture.Parts {
		db.AddPart(row.ID, row.MessageID, row.SessionID, 1, 2, string(row.Data))
	}
	return &opencodeImporter{}, db.Path
}

func openCodeFixtureSession(t *testing.T, imp *opencodeImporter, path, sessionID string) SessionPreview {
	t.Helper()
	previews, err := imp.Previews(context.Background(), path)
	if err != nil {
		t.Fatalf("Previews: %v", err)
	}
	for _, preview := range previews {
		if preview.SessionID == sessionID {
			return preview
		}
	}
	t.Fatalf("fixture has no preview for %s", sessionID)
	return SessionPreview{}
}

func TestOpenCodeFixtureMapsLikeTheLivePlugin(t *testing.T) {
	imp, path := loadOpenCodeFixture(t, "mapping.json")
	root := collectTurns(t, imp, openCodeFixtureSession(t, imp, path, "root-session"))
	if len(root) != 3 {
		t.Fatalf("root turns = %d, want 3", len(root))
	}

	first := root[0]
	if first.Gen.ID != first.Source.GenerationID() || first.Gen.ConversationID != "root-session" {
		t.Fatalf("first identity = %+v", first.Gen)
	}
	if first.Gen.ConversationTitle != "Root work" || first.Gen.AgentName != "opencode:build" {
		t.Fatalf("first title/agent = %q/%q", first.Gen.ConversationTitle, first.Gen.AgentName)
	}
	if first.Gen.Mode != agento11y.GenerationModeStream || first.Gen.OperationName != "streamText" {
		t.Fatalf("first span shape = %q/%q", first.Gen.Mode, first.Gen.OperationName)
	}
	if first.Gen.Model != (agento11y.ModelRef{Provider: "anthropic", Name: "claude-test"}) || first.Gen.ResponseModel != "claude-test" {
		t.Fatalf("first models = %+v/%q", first.Gen.Model, first.Gen.ResponseModel)
	}
	if first.Gen.StopReason != "tool-calls" || first.Gen.Metadata["cost_usd"] != 0.12 {
		t.Fatalf("first stop/cost = %q/%v", first.Gen.StopReason, first.Gen.Metadata["cost_usd"])
	}
	if !reflect.DeepEqual(first.Gen.Tags, map[string]string{"cwd": "/work/root"}) {
		t.Fatalf("first tags = %v", first.Gen.Tags)
	}
	if got := openCodeTextParts(first.Gen.Input); !slices.Equal(got, []string{"Map this prompt"}) {
		t.Fatalf("first input text = %v", got)
	}
	if got := openCodePartKinds(first.Gen.Output); !slices.Equal(got, []agento11y.PartKind{
		agento11y.PartKindText,
		agento11y.PartKindThinking,
		agento11y.PartKindToolCall,
		agento11y.PartKindToolResult,
		agento11y.PartKindToolCall,
		agento11y.PartKindToolResult,
	}) {
		t.Fatalf("first output kinds = %v", got)
	}
	if got := first.Gen.Output[2].Parts[0].ToolCall.InputJSON; string(got) != `{"cmd":"echo hi"}` {
		t.Fatalf("completed tool input = %s", got)
	}
	if got := first.Gen.Output[5].Parts[0].ToolResult; !got.IsError || got.Content != "" {
		t.Fatalf("failed tool result = %+v", got)
	}
	if got := first.Gen.Tools; !reflect.DeepEqual(got, []agento11y.ToolDefinition{
		{Name: "bash", Type: "function"},
		{Name: "read", Type: "function"},
	}) {
		t.Fatalf("first tools = %+v", got)
	}
	if got := first.Gen.Usage; got != (agento11y.TokenUsage{
		InputTokens: 80, OutputTokens: 25, TotalTokens: 105,
		ReasoningTokens: 7, CacheReadInputTokens: 20, CacheWriteInputTokens: 10,
	}) {
		t.Fatalf("fallback usage = %+v", got)
	}
	if !first.Quality.ApproxUsage {
		t.Fatal("message-token fallback was not marked approximate")
	}

	second := root[1]
	if len(second.Gen.Input) != 0 {
		t.Fatalf("later assistant input = %+v, want none", second.Gen.Input)
	}
	if got := second.Gen.Usage; got != (agento11y.TokenUsage{
		InputTokens: 310, OutputTokens: 60, TotalTokens: 370,
		ReasoningTokens: 15, CacheReadInputTokens: 70, CacheWriteInputTokens: 14,
	}) {
		t.Fatalf("step usage = %+v", got)
	}
	if second.Quality.ApproxUsage {
		t.Fatal("step usage was marked approximate")
	}
	if cost, ok := second.Gen.Metadata["cost_usd"]; !ok || cost != float64(0) {
		t.Fatalf("zero cost = %v, present %v", cost, ok)
	}
	if !slices.Equal(second.Gen.ParentGenerationIDs, []string{first.Gen.ID}) {
		t.Fatalf("second parents = %v", second.Gen.ParentGenerationIDs)
	}
	if !slices.Equal(root[2].Gen.ParentGenerationIDs, []string{second.Gen.ID}) {
		t.Fatalf("third parents = %v", root[2].Gen.ParentGenerationIDs)
	}

	child := collectTurns(t, imp, openCodeFixtureSession(t, imp, path, "child-session"))
	if len(child) != 1 {
		t.Fatalf("child turns = %d, want 1", len(child))
	}
	turn := child[0]
	if turn.Gen.ConversationID != "root-session" || turn.Gen.ConversationTitle != "" {
		t.Fatalf("child conversation = %q title %q", turn.Gen.ConversationID, turn.Gen.ConversationTitle)
	}
	if turn.Gen.AgentName != "opencode:explore" || turn.Gen.CallError != "aborted" {
		t.Fatalf("child agent/error = %q/%q", turn.Gen.AgentName, turn.Gen.CallError)
	}
	if !reflect.DeepEqual(turn.Gen.Tags, map[string]string{"cwd": "/work/child", "subagent": "true"}) {
		t.Fatalf("child tags = %v", turn.Gen.Tags)
	}
	if turn.Gen.Metadata["opencode.parent_session_id"] != "root-session" || turn.Gen.Metadata["opencode.child_session_id"] != "child-session" {
		t.Fatalf("child metadata = %v", turn.Gen.Metadata)
	}
	if !slices.Equal(turn.Gen.ParentGenerationIDs, []string{root[2].Gen.ID}) {
		t.Fatalf("child parents = %v, want spawning turn %s", turn.Gen.ParentGenerationIDs, root[2].Gen.ID)
	}

	encoded, err := json.Marshal(append(root, child...))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"SUMMARY-DIFF-MUST-NOT-EXPORT", "FILE-PART-MUST-NOT-EXPORT", "SIGNATURE-MUST-NOT-EXPORT", "call-running"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("mapped generations contain excluded value %q", forbidden)
		}
	}
}

func TestOpenCodeMappingRules(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		messages := opencodeInputMessages([]sessiondb.Part{
			{Type: "text", Text: "sent"},
			{Type: "text", Text: "user only", Ignored: true},
		})
		if got := openCodeTextParts(messages); !slices.Equal(got, []string{"sent"}) {
			t.Fatalf("input text = %v", got)
		}
	})

	t.Run("usage", func(t *testing.T) {
		tests := []struct {
			name     string
			fallback sessiondb.TokenCounts
			parts    []sessiondb.Part
			want     agento11y.TokenUsage
			approx   bool
		}{
			{
				name:     "message fallback",
				fallback: sessiondb.TokenCounts{Input: 4, Output: 5, Reasoning: 2, Cache: sessiondb.CacheCounts{Read: 3, Write: 1}},
				want:     agento11y.TokenUsage{InputTokens: 4, OutputTokens: 5, TotalTokens: 9, ReasoningTokens: 2, CacheReadInputTokens: 3, CacheWriteInputTokens: 1},
				approx:   true,
			},
			{
				name:     "empty step falls back",
				fallback: sessiondb.TokenCounts{Input: 4, Output: 5},
				parts:    []sessiondb.Part{openCodeStepPart(t, `{}`)},
				want:     agento11y.TokenUsage{InputTokens: 4, OutputTokens: 5, TotalTokens: 9},
				approx:   true,
			},
			{
				name: "sum steps",
				parts: []sessiondb.Part{
					openCodeStepPart(t, `{"input":4,"output":5,"reasoning":2,"cache":{"read":3,"write":1}}`),
					openCodeStepPart(t, `{"input":6,"output":7,"reasoning":4,"cache":{"read":5,"write":2}}`),
				},
				want: agento11y.TokenUsage{InputTokens: 10, OutputTokens: 12, TotalTokens: 22, ReasoningTokens: 6, CacheReadInputTokens: 8, CacheWriteInputTokens: 3},
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				got, approx := opencodeMapUsage(tt.fallback, tt.parts)
				if got != tt.want || approx != tt.approx {
					t.Fatalf("opencodeMapUsage = (%+v, %v), want (%+v, %v)", got, approx, tt.want, tt.approx)
				}
			})
		}
	})

	t.Run("errors", func(t *testing.T) {
		status := 429
		tests := []struct {
			name string
			err  *sessiondb.MessageError
			want string
		}{
			{name: "none", want: ""},
			{name: "provider auth", err: &sessiondb.MessageError{Name: "ProviderAuthError"}, want: "provider_auth"},
			{name: "api status", err: &sessiondb.MessageError{Name: "APIError", Data: sessiondb.MessageErrorData{StatusCode: &status}}, want: "api_error: 429"},
			{name: "api unknown", err: &sessiondb.MessageError{Name: "APIError"}, want: "api_error: unknown"},
			{name: "length", err: &sessiondb.MessageError{Name: "MessageOutputLengthError"}, want: "output_length_exceeded"},
			{name: "aborted", err: &sessiondb.MessageError{Name: "MessageAbortedError"}, want: "aborted"},
			{name: "unknown", err: &sessiondb.MessageError{Name: "UnknownError"}, want: "unknown_error"},
			{name: "new host error", err: &sessiondb.MessageError{Name: "ContextOverflowError"}, want: "unknown_error"},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := opencodeError(tt.err); got != tt.want {
					t.Fatalf("opencodeError = %q, want %q", got, tt.want)
				}
			})
		}
	})

	t.Run("tool results", func(t *testing.T) {
		empty := ""
		failure := "failed"
		exitZero := 0
		exitOne := 1
		tests := []struct {
			name      string
			part      sessiondb.Part
			wantError bool
			want      string
		}{
			{
				name:      "missing error",
				part:      sessiondb.Part{Type: "tool", State: sessiondb.ToolState{Status: "error"}},
				wantError: true,
				want:      "unknown error",
			},
			{
				name:      "empty error",
				part:      sessiondb.Part{Type: "tool", State: sessiondb.ToolState{Status: "error", Error: &empty}},
				wantError: true,
			},
			{
				name:      "error message",
				part:      sessiondb.Part{Type: "tool", State: sessiondb.ToolState{Status: "error", Error: &failure}},
				wantError: true,
				want:      "failed",
			},
			{
				name:      "invalid tool",
				part:      sessiondb.Part{Type: "tool", Tool: "invalid", State: sessiondb.ToolState{Status: "completed", Output: "invalid arguments"}},
				wantError: true,
				want:      "invalid arguments",
			},
			{
				name:      "bash failure",
				part:      sessiondb.Part{Type: "tool", Tool: "bash", State: sessiondb.ToolState{Status: "completed", Output: "failed", Metadata: sessiondb.ToolMetadata{Exit: &exitOne}}},
				wantError: true,
				want:      "failed",
			},
			{
				name: "bash success",
				part: sessiondb.Part{Type: "tool", Tool: "bash", State: sessiondb.ToolState{Status: "completed", Output: "done", Metadata: sessiondb.ToolMetadata{Exit: &exitZero}}},
				want: "done",
			},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				messages := opencodeOutputMessages([]sessiondb.Part{tt.part})
				if len(messages) != 2 {
					t.Fatalf("messages = %+v", messages)
				}
				got := messages[1].Parts[0].ToolResult
				if got.IsError != tt.wantError || got.Content != tt.want {
					t.Fatalf("tool result = %+v, want error=%v content=%q", got, tt.wantError, tt.want)
				}
			})
		}
	})

	t.Run("agent and tags", func(t *testing.T) {
		tests := []struct {
			name     string
			mode     string
			cwd      string
			subagent bool
			wantName string
			wantTags map[string]string
		}{
			{name: "defaults", wantName: "opencode"},
			{name: "mode and cwd", mode: "plan", cwd: "/work", wantName: "opencode:plan", wantTags: map[string]string{"cwd": "/work"}},
			{name: "subagent", mode: "build", subagent: true, wantName: "opencode:build", wantTags: map[string]string{"subagent": "true"}},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				if got := opencodeAgentName(tt.mode); got != tt.wantName {
					t.Errorf("agent name = %q, want %q", got, tt.wantName)
				}
				if got := opencodeTags(tt.cwd, tt.subagent); !reflect.DeepEqual(got, tt.wantTags) {
					t.Errorf("tags = %v, want %v", got, tt.wantTags)
				}
			})
		}
	})
}

func TestOpenCodeLineageWalksToTheRootAndGuardsCycles(t *testing.T) {
	db := sessiondbtest.New(t)
	db.AddSession("root", "", "root", "/work", 1, 1)
	db.AddSession("child", "root", "child", "/work", 1, 1)
	db.AddSession("grandchild", "child", "grandchild", "/work", 1, 1)
	db.AddSession("cycle-a", "cycle-b", "a", "/work", 1, 1)
	db.AddSession("cycle-b", "cycle-a", "b", "/work", 1, 1)

	store, err := sessiondb.Open(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	lineage, err := resolveOpenCodeLineage(context.Background(), store, "grandchild", db.Path)
	if err != nil {
		t.Fatal(err)
	}
	if lineage.parentSessionID != "child" || lineage.conversationID != "root" {
		t.Fatalf("grandchild lineage = %+v", lineage)
	}
	cycle, err := resolveOpenCodeLineage(context.Background(), store, "cycle-a", db.Path)
	if err != nil {
		t.Fatal(err)
	}
	if cycle.conversationID != "cycle-b" {
		t.Fatalf("cycle lineage = %+v, want guard to stop at cycle-b", cycle)
	}
}

func openCodeStepPart(t *testing.T, tokens string) sessiondb.Part {
	t.Helper()
	var part sessiondb.Part
	if err := json.Unmarshal([]byte(`{"type":"step-finish","tokens":`+tokens+`}`), &part); err != nil {
		t.Fatalf("decode step part: %v", err)
	}
	return part
}

func openCodeTextParts(messages []agento11y.Message) []string {
	var out []string
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Kind == agento11y.PartKindText {
				out = append(out, part.Text)
			}
		}
	}
	return out
}

func openCodePartKinds(messages []agento11y.Message) []agento11y.PartKind {
	var out []agento11y.PartKind
	for _, message := range messages {
		for _, part := range message.Parts {
			out = append(out, part.Kind)
		}
	}
	return out
}

func TestOpenCodeFixtureIsDeterministic(t *testing.T) {
	imp, path := loadOpenCodeFixture(t, "mapping.json")
	sess := openCodeFixtureSession(t, imp, path, "root-session")
	first := collectTurns(t, imp, sess)
	second := collectTurns(t, imp, sess)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated fixture import changed:\nfirst=%s\nsecond=%s", fmt.Sprint(first), fmt.Sprint(second))
	}
}
