package local

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		if a.TokenBuckets != (TokenBuckets{FreshInput: 300, Output: 130}) {
			t.Errorf("conv-A token_buckets = %+v, want fresh=300 output=130", a.TokenBuckets)
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
	if first.TokenBuckets != (TokenBuckets{FreshInput: 10, Output: 5}) {
		t.Errorf("first.token_buckets = %+v, want fresh=10 output=5", first.TokenBuckets)
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

// TestDisjointTokenUsage covers the provider-aware split into
// non-overlapping buckets. Anthropic keeps cache tokens separate from
// input; OpenAI, Gemini, and codex fold cache_read into input; OpenAI
// and codex also nest reasoning in output while Gemini keeps thoughts
// additive; unknown providers default to "separate" on both axes.
func TestDisjointTokenUsage(t *testing.T) {
	cases := []struct {
		name                                                 string
		provider                                             string
		usage                                                sigil.TokenUsage
		freshInput, cacheRead, cacheWrite, output, reasoning int64
	}{
		{
			name:       "anthropic keeps cache additive",
			provider:   "anthropic",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
			freshInput: 100, cacheRead: 30, cacheWrite: 20, output: 50, reasoning: 0,
		},
		{
			name:       "openai carves cache_read out of input and reasoning out of output",
			provider:   "openai",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			name:       "gemini fully cached prompt leaves zero fresh input",
			provider:   "gemini",
			usage:      sigil.TokenUsage{InputTokens: 80, OutputTokens: 20, CacheReadInputTokens: 80},
			freshInput: 0, cacheRead: 80, cacheWrite: 0, output: 20, reasoning: 0,
		},
		{
			// Gemini carves cache_read out of input but keeps thoughts
			// additive: output stays at the candidate count.
			name:       "gemini keeps reasoning additive to output",
			provider:   "gemini",
			usage:      sigil.TokenUsage{InputTokens: 80, OutputTokens: 40, CacheReadInputTokens: 20, ReasoningTokens: 10},
			freshInput: 60, cacheRead: 20, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// Azure OpenAI shares OpenAI's subset semantics on both axes.
			name:       "azure carves cache_read and reasoning out",
			provider:   "azure",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// The codex agent falls back to provider "codex" for model
			// names it can't attribute; its usage comes from the
			// Responses API, so OpenAI subset semantics apply.
			name:       "codex shares openai subset semantics",
			provider:   "codex",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// Unknown provider keeps reasoning additive (never hide output).
			name:       "unknown provider keeps reasoning additive",
			provider:   "openrouter",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10},
			freshInput: 100, cacheRead: 0, cacheWrite: 0, output: 50, reasoning: 10,
		},
		{
			name:       "unknown provider defaults to separate (no subtraction)",
			provider:   "mystery-llm",
			usage:      sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30},
			freshInput: 100, cacheRead: 30, cacheWrite: 0, output: 50, reasoning: 0,
		},
		{
			name:       "empty provider defaults to separate",
			provider:   "",
			usage:      sigil.TokenUsage{InputTokens: 100, CacheReadInputTokens: 30},
			freshInput: 100, cacheRead: 30, cacheWrite: 0, output: 0, reasoning: 0,
		},
		{
			name:       "subset cache_read larger than input clamps fresh input to zero",
			provider:   "openai",
			usage:      sigil.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 30},
			freshInput: 0, cacheRead: 30, cacheWrite: 0, output: 5, reasoning: 0,
		},
		{
			name:       "reasoning larger than output clamps output to zero",
			provider:   "openai",
			usage:      sigil.TokenUsage{InputTokens: 20, OutputTokens: 5, ReasoningTokens: 10},
			freshInput: 20, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 10,
		},
		{
			name:       "negative values clamp to zero",
			provider:   "anthropic",
			usage:      sigil.TokenUsage{InputTokens: -5, OutputTokens: -1, CacheReadInputTokens: -3},
			freshInput: 0, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := disjointTokenUsage(tc.usage, tc.provider)
			assert.Equal(t, TokenBuckets{
				FreshInput: tc.freshInput,
				CacheRead:  tc.cacheRead,
				CacheWrite: tc.cacheWrite,
				Output:     tc.output,
				Reasoning:  tc.reasoning,
			}, b)
		})
	}
}

// TestTokenUsagePoints seeds generations across conversations and checks
// the flattened, time-sorted points: provider-aware buckets, model and
// provider tagging, the received_at timestamp fallback, and that
// zero-token generations are dropped.
func TestTokenUsagePoints(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-A", "g1", sigil.Generation{
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:10Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:12Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
	}, "2026-05-21T10:00:12Z")

	// Earlier than g1 so it must sort first; OpenAI subset semantics.
	writeGen(t, s, "conv-B", "g2", sigil.Generation{
		Model:       sigil.ModelRef{Provider: "openai", Name: "gpt-5-omni"},
		StartedAt:   mustParse(t, "2026-05-21T09:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T09:00:01Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
	}, "2026-05-21T09:00:01Z")

	// No started/completed: timestamp must fall back to received_at.
	writeGen(t, s, "conv-C", "g3", sigil.Generation{
		Model: sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		Usage: sigil.TokenUsage{InputTokens: 5, OutputTokens: 3},
	}, "2026-05-21T12:00:00Z")

	// Zero tokens: must be dropped entirely.
	writeGen(t, s, "conv-D", "g4", sigil.Generation{
		Model:     sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt: mustParse(t, "2026-05-21T08:00:00Z"),
	}, "2026-05-21T08:00:00Z")

	points, err := s.TokenUsagePoints()
	require.NoError(t, err)
	require.Len(t, points, 3, "zero-token generation should be dropped")

	// Sorted oldest-first: g2 (09:00) → g1 (10:00) → g3 (12:00 received_at).
	assert.Equal(t, "gpt-5-omni", points[0].Model)
	assert.Equal(t, "claude-sonnet-4", points[1].Model)
	assert.Equal(t, "claude-opus-4-7", points[2].Model)

	// g2 OpenAI: cache_read carved out of input, reasoning out of output.
	assert.Equal(t, TokenUsagePoint{
		Timestamp:    mustParse(t, "2026-05-21T09:00:00Z"),
		Model:        "gpt-5-omni",
		Provider:     "openai",
		TokenBuckets: TokenBuckets{FreshInput: 70, CacheRead: 30, Output: 40, Reasoning: 10},
	}, points[0])

	// g1 Anthropic: cache stays additive.
	assert.Equal(t, TokenUsagePoint{
		Timestamp:    mustParse(t, "2026-05-21T10:00:10Z"),
		Model:        "claude-sonnet-4",
		Provider:     "anthropic",
		TokenBuckets: TokenBuckets{FreshInput: 100, CacheRead: 30, CacheWrite: 20, Output: 50},
	}, points[1])

	// g3 timestamp falls back to received_at.
	assert.Equal(t, mustParse(t, "2026-05-21T12:00:00Z"), points[2].Timestamp)
}

// TestTokenUsagePoints_EmptyStore checks that TokenUsagePoints returns
// no points and no error before any conversations exist.
func TestTokenUsagePoints_EmptyStore(t *testing.T) {
	s := newStorage(t)
	points, err := s.TokenUsagePoints()
	require.NoError(t, err)
	assert.Empty(t, points)
}

// TestAggregateCache_HitsAndInvalidates covers the per-file aggregate
// cache: identical results on re-call, invalidation when a generation is
// appended (size+mtime change), a cache hit served without re-reading the
// file (proved by corrupting content under an unchanged mtime+size), and
// a TTL-driven re-scan once the entry ages out.
func TestAggregateCache_HitsAndInvalidates(t *testing.T) {
	s := newStorage(t)
	clock := mustParse(t, "2026-05-21T10:00:00Z")
	s.now = func() time.Time { return clock }

	writeGen(t, s, "conv-A", "g1", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50},
	}, "2026-05-21T10:00:03Z")

	list1, err := s.ListConversations(0)
	require.NoError(t, err)
	pts1, err := s.TokenUsagePoints()
	require.NoError(t, err)
	require.Len(t, list1, 1)
	require.Equal(t, 1, list1[0].Calls)
	require.Len(t, pts1, 1)

	// Re-call with no change: identical results, served from cache.
	list2, err := s.ListConversations(0)
	require.NoError(t, err)
	pts2, err := s.TokenUsagePoints()
	require.NoError(t, err)
	assert.Equal(t, list1, list2)
	assert.Equal(t, pts1, pts2)

	// Append a second generation: size grows, so the cache invalidates and
	// the new generation shows up in both the summary and the points.
	writeGen(t, s, "conv-A", "g2", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:10Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:13Z"),
		Usage:       sigil.TokenUsage{InputTokens: 200, OutputTokens: 80},
	}, "2026-05-21T10:00:13Z")

	list3, err := s.ListConversations(0)
	require.NoError(t, err)
	pts3, err := s.TokenUsagePoints()
	require.NoError(t, err)
	require.Len(t, list3, 1)
	assert.Equal(t, 2, list3[0].Calls)
	assert.Equal(t, int64(300), list3[0].InputTokens)
	assert.Equal(t, int64(130), list3[0].OutputTokens)
	assert.Len(t, pts3, 2)

	// Corrupt the file while preserving its size and mtime. A cache hit
	// must serve the prior aggregate without re-reading the now-garbage
	// bytes.
	path := filepath.Join(s.dir, ConversationsDir, "conv-A.jsonl")
	info, err := os.Stat(path)
	require.NoError(t, err)
	orig, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), len(orig)), 0o600))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))

	list4, err := s.ListConversations(0)
	require.NoError(t, err)
	assert.Equal(t, list3, list4, "unchanged mtime+size must serve the cached summary")

	// Advance past the TTL: the entry ages out and the file is re-scanned.
	// The garbage has no decodable records, so the conversation drops out.
	clock = clock.Add(aggregateCacheTTL + time.Minute)
	list5, err := s.ListConversations(0)
	require.NoError(t, err)
	assert.Empty(t, list5, "TTL expiry forces a re-scan of the corrupted file")
	pts5, err := s.TokenUsagePoints()
	require.NoError(t, err)
	assert.Empty(t, pts5)
}

// TestAggregateCache_ScanUsesFdStatNotCallerInfo pins that the cache key
// describes the bytes actually scanned, not the caller's pre-scan stat. An
// append that lands after ListConversations/TokenUsagePoints snapshot the
// DirEntry info but before the scan runs must not leave the entry keyed by
// the stale (smaller) size, which would let a later poll at the real size
// serve content that omits the appended generation.
func TestAggregateCache_ScanUsesFdStatNotCallerInfo(t *testing.T) {
	// info picks which os.FileInfo a caller hands to fileAggregateFor,
	// given the pre-append snapshot and the current on-disk stat. Whatever
	// it returns, a cold scan must cache the real on-disk content keyed by
	// the real size from the fd stat — a stale, smaller info must not key
	// the entry and let a later poll serve a generation-short aggregate.
	cases := []struct {
		name string
		info func(stale, current os.FileInfo) os.FileInfo
	}{
		{
			name: "stale smaller info from before the append",
			info: func(stale, _ os.FileInfo) os.FileInfo { return stale },
		},
		{
			name: "current info matching disk",
			info: func(_, current os.FileInfo) os.FileInfo { return current },
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			clock := mustParse(t, "2026-05-21T10:00:00Z")
			s.now = func() time.Time { return clock }

			writeGen(t, s, "conv-A", "g1", sigil.Generation{
				Model:     sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
				StartedAt: mustParse(t, "2026-05-21T10:00:00Z"),
				Usage:     sigil.TokenUsage{InputTokens: 100, OutputTokens: 50},
			}, "2026-05-21T10:00:00Z")

			path := filepath.Join(s.dir, ConversationsDir, "conv-A.jsonl")
			stale, err := os.Stat(path)
			require.NoError(t, err)

			// Append after capturing stale, mimicking a write that lands
			// between the caller's ReadDir stat and the scan.
			writeGen(t, s, "conv-A", "g2", sigil.Generation{
				Model:     sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
				StartedAt: mustParse(t, "2026-05-21T10:00:10Z"),
				Usage:     sigil.TokenUsage{InputTokens: 200, OutputTokens: 80},
			}, "2026-05-21T10:00:10Z")

			current, err := os.Stat(path)
			require.NoError(t, err)
			require.Greater(t, current.Size(), stale.Size())

			// The aggregate must reflect both generations on disk, and the
			// cached entry must carry the real size from the fd stat, not the
			// info passed in.
			agg, err := s.fileAggregateFor(path, tc.info(stale, current))
			require.NoError(t, err)
			assert.Equal(t, 2, agg.summary.Calls)
			assert.Equal(t, current.Size(), agg.size)
			assert.True(t, agg.mtime.Equal(current.ModTime()))

			// The entry is keyed by the real size, so a poll at that size is
			// a genuine cache hit: overwrite with same-size garbage, keep the
			// mtime, and confirm the cached summary is served, not the garbage.
			orig, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), len(orig)), 0o600))
			require.NoError(t, os.Chtimes(path, current.ModTime(), current.ModTime()))

			hit, err := s.fileAggregateFor(path, current)
			require.NoError(t, err)
			assert.Equal(t, 2, hit.summary.Calls, "real size must hit the cache, not re-scan garbage")
		})
	}
}

// TestAggregates_IgnoreInputOutput is the lean-decode equivalence guard:
// a generation carrying large input/output message trees must produce the
// same summary and token points as one with the scalar fields alone, since
// the aggregate scans never read input/output.
func TestAggregates_IgnoreInputOutput(t *testing.T) {
	s := newStorage(t)

	big := strings.Repeat("x", 4096)
	withContent := sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
		Input:       []sigil.Message{{Role: sigil.RoleUser, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: big}}}},
		Output:      []sigil.Message{{Role: sigil.RoleAssistant, Parts: []sigil.Part{{Kind: sigil.PartKindText, Text: big}}}},
	}
	writeGen(t, s, "conv-A", "g1", withContent, "2026-05-21T10:00:03Z")

	// Same scalars, no message content.
	noContent := withContent
	noContent.Input = nil
	noContent.Output = nil
	writeGen(t, s, "conv-B", "g2", noContent, "2026-05-21T10:00:03Z")

	list, err := s.ListConversations(0)
	require.NoError(t, err)
	require.Len(t, list, 2)

	byID := map[string]ConversationSummary{}
	for _, c := range list {
		byID[c.ID] = c
	}
	a, b := byID["conv-A"], byID["conv-B"]
	// Normalise the fields that legitimately differ between the two.
	a.ID, b.ID = "", ""
	assert.Equal(t, b, a, "input/output must not change the conversation summary")

	pts, err := s.TokenUsagePoints()
	require.NoError(t, err)
	require.Len(t, pts, 2)
	assert.Equal(t, pts[0].TokenBuckets, pts[1].TokenBuckets, "input/output must not change token points")

	// The full-decode detail path still surfaces the message content.
	detail, err := s.ConversationDetail("conv-A")
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Len(t, detail.Generations, 1)
	assert.NotEmpty(t, detail.Generations[0].Messages, "detail path still decodes input/output")
}
