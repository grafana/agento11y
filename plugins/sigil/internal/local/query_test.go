package local

import (
	"encoding/json"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grafana/sigil-sdk/go/sigil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGen writes one generation record the way handleGenerations would.
// Tests don't need to go through HTTP to validate the aggregator.
func writeGen(t *testing.T, s *Storage, convID, genID string, gen sigil.Generation, receivedAt string) {
	t.Helper()
	if gen.ID == "" {
		gen.ID = genID
	}
	if gen.ConversationID == "" {
		gen.ConversationID = convID
	}
	raw, err := json.Marshal(gen)
	if err != nil {
		t.Fatalf("marshal generation: %v", err)
	}
	rec := generationRecord{
		ReceivedAt:     receivedAt,
		GenerationID:   gen.ID,
		ConversationID: gen.ConversationID,
		Generation:     raw,
	}
	if err := s.AppendGeneration(rec); err != nil {
		t.Fatalf("append: %v", err)
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

func TestTruncateUTF8Safe(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{name: "short ascii unchanged", input: "hello", max: 10, want: "hello"},
		{name: "ascii truncates at max bytes", input: "hello", max: 3, want: "hel…"},
		{name: "does not split two byte rune", input: "abcédef", max: 4, want: "abc…"},
		{name: "keeps full two byte rune at boundary", input: "abcédef", max: 5, want: "abcé…"},
		{name: "does not split emoji", input: "go🙂lang", max: 4, want: "go…"},
		{name: "keeps emoji at boundary", input: "go🙂lang", max: 6, want: "go🙂…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.input, tc.max)
			assert.Equal(t, tc.want, got)
			assert.True(t, utf8.ValidString(got))
		})
	}
}

// TestListConversations_Aggregates seeds the storage with generations
// across three conversations and asserts the per-conversation rollups:
// token sums, call counts, distinct agents/models, status derivation,
// and sort order.
func TestListConversations_Aggregates(t *testing.T) {
	s := newStorage(t)

	// conv-A: two generations, two models, error on the second.
	writeGen(t, s, "conv-A", "g1", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50},
	}, "2026-05-21T10:00:03Z")
	writeGen(t, s, "conv-A", "g2", sigil.Generation{
		AgentName:     "pi",
		Model:         sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		ResponseModel: "claude-opus-4-7-20250901", // distinct from request name
		StartedAt:     mustParse(t, "2026-05-21T10:00:10Z"),
		CompletedAt:   mustParse(t, "2026-05-21T10:00:13Z"),
		Usage:         sigil.TokenUsage{InputTokens: 200, OutputTokens: 80},
		CallError:     "rate limited",
	}, "2026-05-21T10:00:13Z")

	// conv-B: single generation, distinct agent.
	writeGen(t, s, "conv-B", "g3", sigil.Generation{
		AgentName:   "claude-code",
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T11:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T11:00:01Z"),
		Usage:       sigil.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, "2026-05-21T11:00:01Z")

	// conv-C: only a received_at timestamp (no started/completed); the
	// list should still surface it via the received_at fallback.
	writeGen(t, s, "conv-C", "g5", sigil.Generation{AgentName: "vistra"}, "2026-05-21T11:10:00Z")

	got, err := s.ListConversations(0)
	if err != nil {
		t.Fatalf("ListConversations: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d conversations, want 3; got=%+v", len(got), got)
	}

	// Sort order: conv-C (11:10) → conv-B (11:00:01) → conv-A (10:00:13).
	wantOrder := []string{"conv-C", "conv-B", "conv-A"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("position %d: id = %q, want %q", i, got[i].ID, w)
		}
	}

	byID := map[string]ConversationSummary{}
	for _, c := range got {
		byID[c.ID] = c
	}

	if a := byID["conv-A"]; true {
		if a.Calls != 2 {
			t.Errorf("conv-A calls = %d, want 2", a.Calls)
		}
		if a.InputTokens != 300 || a.OutputTokens != 130 || a.TotalTokens != 430 {
			t.Errorf("conv-A tokens = in=%d out=%d total=%d, want 300/130/430", a.InputTokens, a.OutputTokens, a.TotalTokens)
		}
		if len(a.Agents) != 1 || a.Agents[0] != "pi" {
			t.Errorf("conv-A agents = %v, want [pi]", a.Agents)
		}
		// response_model on g2 must surface alongside the request model.
		wantModels := map[string]bool{"claude-opus-4-7": true, "claude-opus-4-7-20250901": true}
		if len(a.Models) != 2 || !wantModels[a.Models[0]] || !wantModels[a.Models[1]] {
			t.Errorf("conv-A models = %v, want both opus variants", a.Models)
		}
		if a.Status != "err" {
			t.Errorf("conv-A status = %q, want err (g2 has call_error)", a.Status)
		}
		if !a.StartedAt.Equal(mustParse(t, "2026-05-21T10:00:00Z")) {
			t.Errorf("conv-A started_at = %v, want 10:00:00 (earliest g1.started_at)", a.StartedAt)
		}
		if !a.LastActivity.Equal(mustParse(t, "2026-05-21T10:00:13Z")) {
			t.Errorf("conv-A last_activity = %v, want 10:00:13 (latest g2.completed_at)", a.LastActivity)
		}
	}

	if c := byID["conv-C"]; true {
		if c.Status != "ok" {
			t.Errorf("conv-C status = %q, want ok", c.Status)
		}
		// received_at fallback drives last_activity when started/completed are zero.
		if !c.LastActivity.Equal(mustParse(t, "2026-05-21T11:10:00Z")) {
			t.Errorf("conv-C last_activity = %v, want 11:10:00 (received_at fallback)", c.LastActivity)
		}
	}
}

// TestListConversations_LimitAndEmpty covers the limit knob and the
// empty-store case in one table.
func TestListConversations_LimitAndEmpty(t *testing.T) {
	cases := []struct {
		name    string
		seed    int // how many conversations to write (oldest first)
		limit   int
		wantLen int
		wantIDs []string // expected ids in returned order; nil to skip
	}{
		{name: "missing dir returns empty", seed: 0, limit: 0, wantLen: 0},
		{name: "limit caps result, newest first", seed: 5, limit: 2, wantLen: 2, wantIDs: []string{"conv-E", "conv-D"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			for i := 0; i < tc.seed; i++ {
				writeGen(t, s, "conv-"+string(rune('A'+i)), "g"+string(rune('0'+i)), sigil.Generation{
					AgentName:   "pi",
					Model:       sigil.ModelRef{Name: "m"},
					StartedAt:   mustParse(t, "2026-05-21T10:00:00Z").Add(time.Duration(i) * time.Minute),
					CompletedAt: mustParse(t, "2026-05-21T10:00:01Z").Add(time.Duration(i) * time.Minute),
				}, "2026-05-21T10:00:01Z")
			}
			got, err := s.ListConversations(tc.limit)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			for i, id := range tc.wantIDs {
				if got[i].ID != id {
					t.Errorf("got[%d].id = %q, want %q", i, got[i].ID, id)
				}
			}
		})
	}
}

// TestConversationDetail covers the per-conversation view: chronological
// ordering, duration math, tool extraction with preview unwrapping, and
// the not-found path.
func TestConversationDetail(t *testing.T) {
	s := newStorage(t)

	// Two generations, written out-of-order so the chronological sort
	// in ConversationDetail actually does work.
	bashInput, _ := json.Marshal(map[string]any{"command": "ls -la /var/log"})
	readInput, _ := json.Marshal(map[string]any{"file_path": "/etc/hosts"})

	writeGen(t, s, "conv-X", "g-second", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:01:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:01:06.5Z"),
		Usage:       sigil.TokenUsage{InputTokens: 20, OutputTokens: 10},
		Output: []sigil.Message{{Role: sigil.RoleAssistant, Parts: []sigil.Part{
			{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{Name: "read", InputJSON: readInput}},
		}}},
	}, "2026-05-21T10:01:06.5Z")

	writeGen(t, s, "conv-X", "g-first", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03.19Z"),
		Usage:       sigil.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Output: []sigil.Message{{Role: sigil.RoleAssistant, Parts: []sigil.Part{
			{Kind: sigil.PartKindText, Text: "thinking..."},
			{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{Name: "bash", InputJSON: bashInput}},
			// Duplicate name to confirm dedup.
			{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{Name: "bash", InputJSON: bashInput}},
		}}},
	}, "2026-05-21T10:00:03.19Z")

	got, err := s.ConversationDetail("conv-X")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("got nil, want detail")
	}
	if got.ID != "conv-X" {
		t.Errorf("id = %q", got.ID)
	}
	if len(got.Generations) != 2 {
		t.Fatalf("len = %d, want 2", len(got.Generations))
	}

	first := got.Generations[0]
	if first.GenerationID != "g-first" {
		t.Errorf("first.generation_id = %q, want g-first (chronological order)", first.GenerationID)
	}
	if first.DurationSeconds < 3.18 || first.DurationSeconds > 3.20 {
		t.Errorf("first.duration_seconds = %v, want ~3.19", first.DurationSeconds)
	}
	if first.TotalTokens != 15 {
		t.Errorf("first.total_tokens = %d, want 15 (input+output via Normalize)", first.TotalTokens)
	}
	// Dedup keeps a single "bash" tool; preview unwraps `command`.
	if len(first.Tools) != 1 || first.Tools[0] != "bash" {
		t.Errorf("first.tools = %v, want [bash]", first.Tools)
	}
	if first.ToolPreview != "ls -la /var/log" {
		t.Errorf("first.tool_preview = %q, want command unwrap", first.ToolPreview)
	}

	second := got.Generations[1]
	if second.GenerationID != "g-second" {
		t.Errorf("second.generation_id = %q, want g-second", second.GenerationID)
	}
	if second.ToolPreview != "/etc/hosts" {
		t.Errorf("second.tool_preview = %q, want file_path unwrap", second.ToolPreview)
	}

	t.Run("not found returns nil", func(t *testing.T) {
		got, err := s.ConversationDetail("does-not-exist")
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if got != nil {
			t.Fatalf("got = %+v, want nil", got)
		}
	})

	t.Run("empty id returns error", func(t *testing.T) {
		if _, err := s.ConversationDetail(""); err == nil {
			t.Fatal("want error for empty id")
		}
	})
}

// TestConversationDetail_ThreadMessages verifies the display-order thread
// used by the local viewer. The raw generation split is still preserved in
// Input/Output, but the viewer should not render tool results before their
// matching assistant tool calls.
func TestConversationDetail_ThreadMessages(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]any{"command": "ls"})
	toolOutput, _ := json.Marshal([]string{"README.md"})
	type wantMessage struct {
		role       sigil.Role
		partKind   sigil.PartKind
		toolCallID string
		text       string
	}
	for _, tc := range []struct {
		name string
		gen  sigil.Generation
		want []wantMessage
	}{
		{
			name: "tool result follows matching tool call",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []sigil.Message{
					{Role: sigil.RoleUser, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "list files"}}},
					{Role: sigil.RoleTool, Parts: []sigil.Part{{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "call-1", Name: "Bash", ContentJSON: toolOutput}}}},
				},
				Output: []sigil.Message{
					{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{ID: "call-1", Name: "Bash", InputJSON: toolInput}}}},
					{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "README.md"}}},
				},
			},
			want: []wantMessage{
				{role: sigil.RoleUser, partKind: sigil.PartKindText, text: "list files"},
				{role: sigil.RoleAssistant, partKind: sigil.PartKindToolCall, toolCallID: "call-1"},
				{role: sigil.RoleTool, partKind: sigil.PartKindToolResult, toolCallID: "call-1"},
				{role: sigil.RoleAssistant, partKind: sigil.PartKindText, text: "README.md"},
			},
		},
		{
			name: "assistant text before tool call stays before tool call",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []sigil.Message{
					{Role: sigil.RoleUser, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "list files"}}},
					{Role: sigil.RoleTool, Parts: []sigil.Part{{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "call-1", Name: "Bash", ContentJSON: toolOutput}}}},
				},
				Output: []sigil.Message{
					{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "checking"}}},
					{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{ID: "call-1", Name: "Bash", InputJSON: toolInput}}}},
					{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "README.md"}}},
				},
			},
			want: []wantMessage{
				{role: sigil.RoleUser, partKind: sigil.PartKindText, text: "list files"},
				{role: sigil.RoleAssistant, partKind: sigil.PartKindText, text: "checking"},
				{role: sigil.RoleAssistant, partKind: sigil.PartKindToolCall, toolCallID: "call-1"},
				{role: sigil.RoleTool, partKind: sigil.PartKindToolResult, toolCallID: "call-1"},
				{role: sigil.RoleAssistant, partKind: sigil.PartKindText, text: "README.md"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			writeGen(t, s, "conv-tools", "g-tools", tc.gen, "2026-05-21T10:00:01Z")

			got, err := s.ConversationDetail("conv-tools")
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.Generations, 1)
			messages := got.Generations[0].Messages
			require.Len(t, messages, len(tc.want))
			for i, want := range tc.want {
				msg := messages[i]
				require.Len(t, msg.Parts, 1, "message %d", i)
				part := msg.Parts[0]
				assert.Equal(t, want.role, msg.Role, "message %d role", i)
				assert.Equal(t, want.partKind, part.Kind, "message %d part kind", i)
				switch want.partKind {
				case sigil.PartKindToolCall:
					require.NotNil(t, part.ToolCall, "message %d tool call", i)
					assert.Equal(t, want.toolCallID, part.ToolCall.ID, "message %d tool call id", i)
				case sigil.PartKindToolResult:
					require.NotNil(t, part.ToolResult, "message %d tool result", i)
					assert.Equal(t, want.toolCallID, part.ToolResult.ToolCallID, "message %d tool result id", i)
				case sigil.PartKindText:
					assert.Equal(t, want.text, part.Text, "message %d text", i)
				case sigil.PartKindThinking:
					// No thinking parts are used in this table; case included for exhaustiveness.
				}
			}
		})
	}
}

// TestConversationDetail_InputOutputPassThrough verifies the detail
// endpoint exposes the captured input/output messages. The viewer uses
// Messages for display order, but Input/Output must stay intact for callers
// that inspect the raw SDK generation split.
func TestConversationDetail_InputOutputPassThrough(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]any{"command": "ls"})
	toolOutput, _ := json.Marshal([]string{"README.md"})
	cases := []struct {
		name  string
		gen   sigil.Generation
		check func(t *testing.T, view GenerationView)
	}{
		{
			name: "full capture—both sides preserved verbatim",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []sigil.Message{{
					Role:  sigil.RoleUser,
					Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "hey"}},
				}},
				Output: []sigil.Message{{
					Role:  sigil.RoleAssistant,
					Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "Hey! What are you working on?"}},
				}},
			},
			check: func(t *testing.T, v GenerationView) {
				require.Len(t, v.Input, 1)
				assert.Equal(t, sigil.RoleUser, v.Input[0].Role)
				assert.Equal(t, "hey", v.Input[0].Parts[0].Text)
				require.Len(t, v.Output, 1)
				assert.Equal(t, sigil.RoleAssistant, v.Output[0].Role)
				assert.Equal(t, "Hey! What are you working on?", v.Output[0].Parts[0].Text)
			},
		},
		{
			name: "metadata-only capture—empty messages don't synthesize content",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				// Input/Output left nil — the metadata-only mode.
			},
			check: func(t *testing.T, v GenerationView) {
				assert.Empty(t, v.Input)
				assert.Empty(t, v.Output)
			},
		},
		{
			name: "tool call in output kept alongside text",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Output: []sigil.Message{{
					Role: sigil.RoleAssistant,
					Parts: []sigil.Part{
						{Kind: sigil.PartKindText, Text: "running ls"},
						{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{Name: "bash", InputJSON: toolInput}},
					},
				}},
			},
			check: func(t *testing.T, v GenerationView) {
				require.Len(t, v.Output, 1)
				parts := v.Output[0].Parts
				require.Len(t, parts, 2)
				assert.Equal(t, sigil.PartKindText, parts[0].Kind)
				assert.Equal(t, "running ls", parts[0].Text)
				assert.Equal(t, sigil.PartKindToolCall, parts[1].Kind)
				require.NotNil(t, parts[1].ToolCall)
				assert.Equal(t, "bash", parts[1].ToolCall.Name)
			},
		},
		{
			name: "tool result stays in input and tool call stays in output",
			gen: sigil.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []sigil.Message{
					{Role: sigil.RoleUser, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: "list files"}}},
					{Role: sigil.RoleTool, Parts: []sigil.Part{{Kind: sigil.PartKindToolResult, ToolResult: &sigil.ToolResult{ToolCallID: "call-1", Name: "bash", ContentJSON: toolOutput}}}},
				},
				Output: []sigil.Message{{
					Role:  sigil.RoleAssistant,
					Parts: []sigil.Part{{Kind: sigil.PartKindToolCall, ToolCall: &sigil.ToolCall{ID: "call-1", Name: "bash", InputJSON: toolInput}}},
				}},
			},
			check: func(t *testing.T, v GenerationView) {
				require.Len(t, v.Input, 2)
				gotResult := v.Input[1].Parts[0].ToolResult
				require.NotNil(t, gotResult)
				assert.Equal(t, "call-1", gotResult.ToolCallID)
				require.Len(t, v.Output, 1)
				gotCall := v.Output[0].Parts[0].ToolCall
				require.NotNil(t, gotCall)
				assert.Equal(t, "call-1", gotCall.ID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			writeGen(t, s, "conv-io", "g", tc.gen, "2026-05-21T10:00:01Z")
			got, err := s.ConversationDetail("conv-io")
			require.NoError(t, err)
			require.NotNil(t, got)
			require.Len(t, got.Generations, 1)
			tc.check(t, got.Generations[0])
		})
	}
}
