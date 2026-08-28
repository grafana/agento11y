package local

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeGen writes one generation record the way handleGenerations would.
// Tests don't need to go through HTTP to validate the aggregator.
func writeGen(t *testing.T, s *Storage, convID, genID string, gen agento11y.Generation, receivedAt string) {
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
	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50},
	}, "2026-05-21T10:00:03Z")
	writeGen(t, s, "conv-A", "g2", agento11y.Generation{
		AgentName:     "pi",
		Model:         agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		ResponseModel: "claude-opus-4-7-20250901", // distinct from request name
		StartedAt:     mustParse(t, "2026-05-21T10:00:10Z"),
		CompletedAt:   mustParse(t, "2026-05-21T10:00:13Z"),
		Usage:         agento11y.TokenUsage{InputTokens: 200, OutputTokens: 80},
		CallError:     "rate limited",
	}, "2026-05-21T10:00:13Z")

	// conv-B: single generation, distinct agent.
	writeGen(t, s, "conv-B", "g3", agento11y.Generation{
		AgentName:   "claude-code",
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T11:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T11:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, "2026-05-21T11:00:01Z")

	// conv-C: only a received_at timestamp (no started/completed); the
	// list should still surface it via the received_at fallback.
	writeGen(t, s, "conv-C", "g5", agento11y.Generation{AgentName: "vistra"}, "2026-05-21T11:10:00Z")

	// The list orders conversations by their decoded latest activity.
	got, _, err := s.ListConversations(ConversationListOptions{})
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

func TestToolUsagePairsCallsAndResults(t *testing.T) {
	call := func(id, name string) agento11y.Generation {
		return agento11y.Generation{Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{{
				Kind:     agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{ID: id, Name: name},
			}},
		}}}
	}
	result := func(id, name string, failed bool) agento11y.Generation {
		return agento11y.Generation{Input: []agento11y.Message{{
			Role: agento11y.RoleTool,
			Parts: []agento11y.Part{{
				Kind:       agento11y.PartKindToolResult,
				ToolResult: &agento11y.ToolResult{ToolCallID: id, Name: name, IsError: failed},
			}},
		}}}
	}
	cases := []struct {
		name        string
		generations []agento11y.Generation
		want        []ToolUsage
	}{
		{
			name:        "pairs a failed result across generations",
			generations: []agento11y.Generation{call("call-1", "Bash"), result("call-1", "Bash", true)},
			want:        []ToolUsage{{Name: "Bash", Calls: 1, Failures: 1}},
		},
		{
			name: "dedupes a cumulative result by call id",
			generations: []agento11y.Generation{
				call("call-1", "Bash"),
				result("call-1", "Bash", true),
				result("call-1", "Bash", true),
			},
			want: []ToolUsage{{Name: "Bash", Calls: 1, Failures: 1}},
		},
		{
			name:        "duplicate call ids remain distinct without results",
			generations: []agento11y.Generation{call("call-1", "Bash"), call("call-1", "Bash")},
			want:        []ToolUsage{{Name: "Bash", Calls: 2}},
		},
		{
			name: "duplicate Cursor ids pair results FIFO",
			generations: []agento11y.Generation{
				call("call-1", "Bash"),
				call("call-1", "Bash"),
				result("call-1", "Bash", true),
				result("call-1", "Bash", false),
			},
			want: []ToolUsage{{Name: "Bash", Calls: 2, Failures: 1}},
		},
		{
			name: "reused Cursor id ignores old cumulative result",
			generations: []agento11y.Generation{
				call("call-1", "Bash"),
				result("call-1", "Bash", true),
				call("call-1", "Bash"),
				result("call-1", "Bash", true),
			},
			want: []ToolUsage{{Name: "Bash", Calls: 2, Failures: 1}},
		},
		{
			name: "reused Cursor id ignores a mixed-case old cumulative result",
			generations: []agento11y.Generation{
				call("call-1", "Bash"),
				result("call-1", "Bash", false),
				call("call-1", "Bash"),
				{Input: []agento11y.Message{{Parts: []agento11y.Part{
					{ToolResult: &agento11y.ToolResult{ToolCallID: "call-1", Name: "bash"}},
					{ToolResult: &agento11y.ToolResult{ToolCallID: "call-1", Name: "Bash", IsError: true}},
				}}}},
			},
			want: []ToolUsage{{Name: "Bash", Calls: 2, Failures: 1}},
		},
		{
			name: "Pi output result pairs in part order",
			generations: []agento11y.Generation{{Output: []agento11y.Message{{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{
					{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "pi-1", Name: "Read"}},
					{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "pi-1", Name: "Read", IsError: true}},
				},
			}}}},
			want: []ToolUsage{{Name: "Read", Calls: 1, Failures: 1}},
		},
		{
			name: "anonymous mixed-case result pairs with its call",
			generations: []agento11y.Generation{
				call("", "Bash"),
				result("", "bash", true),
			},
			want: []ToolUsage{{Name: "Bash", Calls: 1, Failures: 1}},
		},
		{
			name: "case-folded usage uses the lexical spelling on a tie",
			generations: []agento11y.Generation{
				call("upper", "Bash"),
				call("lower", "bash"),
			},
			want: []ToolUsage{{Name: "Bash", Calls: 2}},
		},
		{
			name: "anonymous same-name failures all count",
			generations: []agento11y.Generation{
				{Output: []agento11y.Message{{Parts: []agento11y.Part{
					{ToolCall: &agento11y.ToolCall{Name: "Bash"}},
					{ToolCall: &agento11y.ToolCall{Name: "Bash"}},
				}}}},
				{Input: []agento11y.Message{{Parts: []agento11y.Part{
					{ToolResult: &agento11y.ToolResult{Name: "Bash", IsError: true}},
					{ToolResult: &agento11y.ToolResult{Name: "Bash", IsError: true}},
				}}}},
			},
			want: []ToolUsage{{Name: "Bash", Calls: 2, Failures: 2}},
		},
		{
			name:        "uses the result name when the call is absent",
			generations: []agento11y.Generation{result("missing-call", "Read", true)},
			want:        []ToolUsage{{Name: "Read", Calls: 1, Failures: 1}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			for i, generation := range tc.generations {
				writeGen(t, s, "conv-tools", "g"+strconv.Itoa(i+1), generation, "2026-05-21T10:00:00Z")
			}
			rows, err := s.ToolUsage(ConversationListOptions{})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			assert.Equal(t, "conv-tools", rows[0].ID)
			assert.Equal(t, tc.want, rows[0].Tools)
		})
	}
}

func TestConversationMetricsPeriodClipping(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-span", "old", agento11y.Generation{
		ConversationTitle: "Lifetime title",
		AgentName:         "old-agent",
		Model:             agento11y.ModelRef{Name: "old-model"},
		StartedAt:         mustParse(t, "2026-05-21T09:30:00Z"),
		Usage:             agento11y.TokenUsage{InputTokens: 9},
		CallError:         "old error",
		Tags:              map[string]string{"cwd": "/repo", "git.branch": "main"},
	}, "2026-05-21T09:30:00Z")
	// Exact lower boundary and zero tokens: it still counts as a call.
	writeGen(t, s, "conv-span", "lower", agento11y.Generation{
		AgentName: "pi/child",
		Model:     agento11y.ModelRef{Name: "zero-model"},
		StartedAt: mustParse(t, "2026-05-21T10:00:00Z"),
	}, "2026-05-21T10:00:00Z")
	writeGen(t, s, "conv-span", "inside", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "current-model"},
		StartedAt: mustParse(t, "2026-05-21T10:30:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 20, OutputTokens: 3},
	}, "2026-05-21T10:30:00Z")
	// Exact upper boundary is excluded.
	writeGen(t, s, "conv-span", "upper", agento11y.Generation{
		AgentName: "future-agent",
		Model:     agento11y.ModelRef{Name: "future-model"},
		StartedAt: mustParse(t, "2026-05-21T11:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 1000},
	}, "2026-05-21T11:00:00Z")
	writeGen(t, s, "conv-other", "other", agento11y.Generation{
		AgentName:   "pi",
		CompletedAt: mustParse(t, "2026-05-21T10:45:00Z"),
		Tags:        map[string]string{"cwd": "/other"},
	}, "2026-05-21T10:45:00Z")
	writeGen(t, s, "conv-unknown", "unknown", agento11y.Generation{
		AgentName: "pi",
		StartedAt: mustParse(t, "2026-05-21T10:40:00Z"),
	}, "2026-05-21T10:40:00Z")

	since := mustParse(t, "2026-05-21T10:00:00Z")
	before := mustParse(t, "2026-05-21T11:00:00Z")
	rows, matched, aggregate, err := s.ConversationMetrics(ConversationListOptions{Limit: 1, Since: since, Before: before})
	require.NoError(t, err)
	assert.Equal(t, 3, matched, "coverage is counted before the limit")
	assert.Equal(t, 4, aggregate.Calls, "aggregate is calculated before the limit")
	assert.Equal(t, 1, aggregate.Agents, "subagents collapse into their host")
	assert.Equal(t, 3, aggregate.Workspaces, "unknown workspace has its own group")
	assert.Equal(t, TokenBuckets{FreshInput: 20, Output: 3}, aggregate.TokenBuckets)
	assert.Equal(t, []string{"current-model", "zero-model"}, aggregate.Models)
	assert.Equal(t, []WorkspaceAggregate{
		{
			Path:                "/other",
			Sessions:            1,
			TokenBucketsByModel: map[string]TokenBuckets{"": {}},
			LastActivity:        mustParse(t, "2026-05-21T10:45:00Z"),
		},
		{
			Sessions:            1,
			TokenBucketsByModel: map[string]TokenBuckets{"": {}},
			LastActivity:        mustParse(t, "2026-05-21T10:40:00Z"),
		},
		{
			Path:         "/repo",
			Sessions:     1,
			TokenBuckets: TokenBuckets{FreshInput: 20, Output: 3},
			TokenBucketsByModel: map[string]TokenBuckets{
				"current-model": {FreshInput: 20, Output: 3},
				"zero-model":    {},
			},
			DurationSeconds: 30 * 60,
			LastActivity:    mustParse(t, "2026-05-21T10:30:00Z"),
		},
	}, aggregate.WorkspaceRows, "workspace rows cover matches outside the returned page in stable order")
	require.Len(t, rows, 1)
	assert.Equal(t, "conv-other", rows[0].ID, "clipped newest activity controls order")
	assert.Equal(t, mustParse(t, "2026-05-21T10:45:00Z"), rows[0].StartedAt, "missing start uses generation time")
	assert.Equal(t, rows[0].StartedAt, rows[0].LastActivity)

	workspace := "/repo"
	rows, matched, aggregate, err = s.ConversationMetrics(ConversationListOptions{Since: since, Before: before, Workspace: &workspace})
	require.NoError(t, err)
	assert.Equal(t, 1, matched)
	require.Len(t, rows, 1)
	got := rows[0]
	assert.Equal(t, "Lifetime title", got.Title)
	assert.Equal(t, "/repo", got.Workspace)
	assert.Equal(t, "main", got.Branch)
	assert.Equal(t, 2, got.Calls)
	assert.Equal(t, int64(20), got.InputTokens)
	assert.Equal(t, int64(3), got.OutputTokens)
	assert.Equal(t, int64(23), got.TotalTokens)
	assert.Equal(t, TokenBuckets{FreshInput: 20, Output: 3}, got.TokenBuckets)
	assert.Equal(t, map[string]TokenBuckets{
		"current-model": {FreshInput: 20, Output: 3},
		"zero-model":    {},
	}, got.TokenBucketsByModel)
	assert.Equal(t, []string{"pi", "pi/child"}, got.Agents)
	assert.Equal(t, []string{"current-model", "zero-model"}, got.Models)
	assert.Equal(t, "ok", got.Status, "an error outside the period does not leak into status")
	assert.Equal(t, 1, got.Subagents)
	assert.Equal(t, since, got.StartedAt)
	assert.Equal(t, mustParse(t, "2026-05-21T10:30:00Z"), got.LastActivity)

	assert.Equal(t, 2, aggregate.Calls)
	assert.Equal(t, map[string]TokenBuckets{
		"current-model": {FreshInput: 20, Output: 3},
		"zero-model":    {},
	}, aggregate.TokenBucketsByModel)
	assert.Equal(t, []WorkspaceAggregate{{
		Path:         "/repo",
		Sessions:     1,
		TokenBuckets: TokenBuckets{FreshInput: 20, Output: 3},
		TokenBucketsByModel: map[string]TokenBuckets{
			"current-model": {FreshInput: 20, Output: 3},
			"zero-model":    {},
		},
		DurationSeconds: 30 * 60,
		LastActivity:    mustParse(t, "2026-05-21T10:30:00Z"),
	}}, aggregate.WorkspaceRows)

	blank := ""
	rows, matched, _, err = s.ConversationMetrics(ConversationListOptions{Since: since, Before: before, Workspace: &blank})
	require.NoError(t, err)
	assert.Equal(t, 1, matched)
	require.Len(t, rows, 1)
	assert.Equal(t, "conv-unknown", rows[0].ID)

	previous, matched, previousAggregate, err := s.ConversationMetrics(ConversationListOptions{
		Since: mustParse(t, "2026-05-21T09:00:00Z"), Before: since, Workspace: &workspace,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, matched, "a spanning conversation independently matches the previous period")
	require.Len(t, previous, 1)
	assert.Equal(t, 1, previous[0].Calls)
	assert.Equal(t, 1, previousAggregate.Calls)
	assert.Equal(t, 1, previousAggregate.Errored)
	assert.Equal(t, int64(9), previous[0].InputTokens)
	assert.Equal(t, []string{"old-agent"}, previous[0].Agents)
	assert.Equal(t, "err", previous[0].Status)

	tied := aggregateConversationMetrics([]ConversationSummary{
		{Workspace: "/zeta", LastActivity: before},
		{Workspace: "/alpha", LastActivity: before},
	})
	require.Len(t, tied.WorkspaceRows, 2)
	assert.Equal(t, "/alpha", tied.WorkspaceRows[0].Path)
	assert.Equal(t, "/zeta", tied.WorkspaceRows[1].Path)
}

func TestConversationFacetFilters(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-pi", "g1", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "claude-opus-5"},
		StartedAt: mustParse(t, "2026-08-21T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10},
	}, "2026-08-21T10:00:00Z")
	writeGen(t, s, "conv-pi-sub", "g2", agento11y.Generation{
		AgentName: "pi/explore",
		Model:     agento11y.ModelRef{Name: "claude-haiku-4-5"},
		StartedAt: mustParse(t, "2026-08-21T10:10:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 20},
	}, "2026-08-21T10:10:00Z")
	writeGen(t, s, "conv-cc", "g3", agento11y.Generation{
		AgentName: "claude-code",
		Model:     agento11y.ModelRef{Name: "claude-opus-5"},
		StartedAt: mustParse(t, "2026-08-21T10:20:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 30},
		CallError: "rate limited",
	}, "2026-08-21T10:20:00Z")

	since := mustParse(t, "2026-08-21T09:00:00Z")
	before := mustParse(t, "2026-08-21T11:00:00Z")
	for _, tc := range []struct {
		name string
		opts ConversationListOptions
		want []string
	}{
		{name: "no facet keeps every conversation", want: []string{"conv-cc", "conv-pi-sub", "conv-pi"}},
		{name: "agent matches the host part", opts: ConversationListOptions{Agent: "pi"}, want: []string{"conv-pi-sub", "conv-pi"}},
		{name: "agent matches a plain name", opts: ConversationListOptions{Agent: "claude-code"}, want: []string{"conv-cc"}},
		{name: "agent does not match a subagent leaf", opts: ConversationListOptions{Agent: "explore"}, want: nil},
		{name: "model is exact", opts: ConversationListOptions{Model: "claude-opus-5"}, want: []string{"conv-cc", "conv-pi"}},
		{name: "status err", opts: ConversationListOptions{Status: "err"}, want: []string{"conv-cc"}},
		{name: "status ok", opts: ConversationListOptions{Status: "ok"}, want: []string{"conv-pi-sub", "conv-pi"}},
		{name: "subagents", opts: ConversationListOptions{MinSubagents: 1}, want: []string{"conv-pi-sub"}},
		{name: "facets compose", opts: ConversationListOptions{Agent: "pi", Model: "claude-opus-5"}, want: []string{"conv-pi"}},
		{name: "facets that share no conversation match none", opts: ConversationListOptions{Agent: "claude-code", Model: "claude-haiku-4-5"}, want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := tc.opts
			opts.Since, opts.Before, opts.Exact = since, before, true
			listed, _, err := s.ListConversations(opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, summaryIDs(listed), "list")

			rows, matched, aggregate, err := s.ConversationMetrics(opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, summaryIDs(rows), "metrics")
			assert.Equal(t, len(tc.want), matched, "matched count")
			assert.Equal(t, len(tc.want), aggregate.Calls, "aggregate covers the matched set only")
		})
	}

	rows, matched, aggregate, err := s.ConversationMetrics(ConversationListOptions{
		Limit: 1, Since: since, Before: before,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-cc"}, summaryIDs(rows))
	assert.Equal(t, 3, matched)
	assert.Equal(t, []string{"claude-code", "pi"}, aggregate.AgentHosts)
	assert.Equal(t, []string{"claude-haiku-4-5", "claude-opus-5"}, aggregate.Models)

	// A facet cuts a candidate before it counts toward the limit, so the page
	// reaches conversations the unfiltered page would have truncated away.
	listed, _, err := s.ListConversations(ConversationListOptions{Limit: 1, MinSubagents: 1})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-pi-sub"}, summaryIDs(listed))
}

// Sessions reads rows from ListConversations and totals from
// ConversationMetrics, so an error facet that matches only outside the period
// must exclude the conversation from both.
func TestConversationFacetsSelectOneSetAcrossQueries(t *testing.T) {
	s := newStorage(t)
	writeGen(t, s, "conv-err-old", "g1", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "claude-opus-5"},
		StartedAt: mustParse(t, "2026-08-20T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10},
		CallError: "rate limited",
	}, "2026-08-20T10:00:00Z")
	writeGen(t, s, "conv-err-old", "g2", agento11y.Generation{
		AgentName: "pi",
		Model:     agento11y.ModelRef{Name: "claude-opus-5"},
		StartedAt: mustParse(t, "2026-08-26T10:00:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 10},
	}, "2026-08-26T10:00:00Z")

	opts := ConversationListOptions{
		Since:  mustParse(t, "2026-08-26T09:00:00Z"),
		Before: mustParse(t, "2026-08-26T11:00:00Z"),
		Status: "err",
		Exact:  true,
	}
	listed, _, err := s.ListConversations(opts)
	require.NoError(t, err)
	assert.Empty(t, summaryIDs(listed), "the errored generation is outside the period")

	rows, matched, _, err := s.ConversationMetrics(opts)
	require.NoError(t, err)
	assert.Empty(t, summaryIDs(rows))
	assert.Equal(t, 0, matched)

	opts.Status = ""
	listed, _, err = s.ListConversations(opts)
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-err-old"}, summaryIDs(listed))
	rows, matched, _, err = s.ConversationMetrics(opts)
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-err-old"}, summaryIDs(rows))
	assert.Equal(t, 1, matched)
}

// A skills-and-tools drill-down can select a reconciled tool call inside the
// period while its generation is outside. Both queries must keep the
// conversation, and ConversationMetrics must report zero in-period generation
// usage.
func TestConversationMetricsToolCallOutsideItsGeneration(t *testing.T) {
	s := newStorage(t)
	generatedAt := mustParse(t, "2026-08-21T10:00:00Z")
	writeToolGeneration(t, s, "conv-late-span", "g1", "/repo", generatedAt, "call-1", "Bash", false)
	spanAt := mustParse(t, "2026-08-21T11:30:00Z")
	_, err := s.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-late-span", "trace-1", "span-1", "call-1", "Bash", spanAt, 2*time.Second, false),
	})
	require.NoError(t, err)

	bash := "Bash"
	for _, tc := range []struct {
		name   string
		status string
		want   []string
	}{
		{name: "no status facet", want: []string{"conv-late-span"}},
		{name: "ok status keeps observation-only session", status: "ok", want: []string{"conv-late-span"}},
		{name: "error status excludes observation-only session", status: "err"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := ConversationListOptions{
				Since:  mustParse(t, "2026-08-21T11:00:00Z"),
				Before: mustParse(t, "2026-08-21T12:00:00Z"),
				Tool:   &bash,
				Status: tc.status,
				Exact:  true,
			}
			listed, _, err := s.ListConversations(opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, summaryIDs(listed), "list")

			rows, matched, aggregate, err := s.ConversationMetrics(opts)
			require.NoError(t, err)
			assert.Equal(t, tc.want, summaryIDs(rows), "metrics")
			assert.Equal(t, len(tc.want), matched)
			assert.Equal(t, 0, aggregate.Calls, "no generation of its own falls in the period")
			assert.Equal(t, TokenBuckets{}, aggregate.TokenBuckets)
			if len(tc.want) > 0 {
				require.Len(t, rows, 1)
				assert.Equal(t, spanAt, rows[0].LastActivity, "the call time bounds a row with no in-period generation")
				assert.Equal(t, "ok", rows[0].Status)
			}
		})
	}
}

func summaryIDs(rows []ConversationSummary) []string {
	var out []string
	for _, row := range rows {
		out = append(out, row.ID)
	}
	return out
}

func TestToolUsagePeriodClippingAttributesResultsToCalls(t *testing.T) {
	s := newStorage(t)
	callAt := mustParse(t, "2026-05-21T10:00:00Z")
	resultAt := mustParse(t, "2026-05-21T11:00:00Z")
	writeGen(t, s, "conv-tools", "call", agento11y.Generation{
		StartedAt: callAt,
		Tags:      map[string]string{"cwd": "/repo"},
		Output: []agento11y.Message{{Parts: []agento11y.Part{{
			ToolCall: &agento11y.ToolCall{ID: "id", Name: "Bash"},
		}}}},
	}, callAt.Format(time.RFC3339))
	writeGen(t, s, "conv-tools", "result", agento11y.Generation{
		StartedAt: resultAt,
		Input: []agento11y.Message{{Parts: []agento11y.Part{{
			ToolResult: &agento11y.ToolResult{ToolCallID: "id", Name: "Bash", IsError: true},
		}}}},
	}, resultAt.Format(time.RFC3339))

	workspace := "/repo"
	rows, err := s.ToolUsage(ConversationListOptions{Since: callAt, Before: resultAt, Workspace: &workspace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []ToolUsage{{Name: "Bash", Calls: 1, Failures: 1}}, rows[0].Tools)

	rows, err = s.ToolUsage(ConversationListOptions{Since: resultAt, Before: resultAt.Add(time.Hour), Workspace: &workspace})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the result generation still matches the conversation period")
	assert.Empty(t, rows[0].Tools, "the matched result is attributed to the earlier call timestamp")

	writeGen(t, s, "conv-orphan", "result", agento11y.Generation{
		StartedAt: resultAt,
		Tags:      map[string]string{"cwd": "/orphan"},
		Input: []agento11y.Message{{Parts: []agento11y.Part{{
			ToolResult: &agento11y.ToolResult{ToolCallID: "missing", Name: "Read", IsError: true},
		}}}},
	}, resultAt.Format(time.RFC3339))
	orphanWorkspace := "/orphan"
	rows, err = s.ToolUsage(ConversationListOptions{
		Since: resultAt, Before: resultAt.Add(time.Hour), Workspace: &orphanWorkspace,
	})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, []ToolUsage{{Name: "Read", Calls: 1, Failures: 1}}, rows[0].Tools,
		"an orphan result uses its own generation timestamp")
}

func TestConversationQueriesUseLatestGenerationRecord(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-retry", "g1", agento11y.Generation{
		AgentName:   "cursor",
		Model:       agento11y.ModelRef{Provider: "openai", Name: "gpt-5.5"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
		Usage:       agento11y.TokenUsage{},
	}, "2026-05-21T10:00:01Z")
	writeGen(t, s, "conv-retry", "g1", agento11y.Generation{
		AgentName:   "cursor",
		Model:       agento11y.ModelRef{Provider: "openai", Name: "gpt-5.5"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 12, OutputTokens: 8, TotalTokens: 20},
	}, "2026-05-21T10:00:02Z")

	summaries, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, 1, summaries[0].Calls)
	assert.Equal(t, int64(20), summaries[0].TotalTokens)

	detail, err := s.ConversationDetail("conv-retry")
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Len(t, detail.Generations, 1)
	assert.Equal(t, "g1", detail.Generations[0].GenerationID)
	assert.Equal(t, int64(20), detail.Generations[0].TotalTokens)
}

// TestListConversations_LimitAndEmpty covers the limit knob and the
// empty-store case in one table. The five fixtures share one received_at
// and stagger completed_at a minute apart, so the returned order comes
// from the activity each append stamped and not from the arrival time.
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
		{name: "unbounded returns every conversation", seed: 5, limit: 0, wantLen: 5, wantIDs: []string{"conv-E", "conv-D", "conv-C", "conv-B", "conv-A"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			for i := 0; i < tc.seed; i++ {
				id := "conv-" + string(rune('A'+i))
				writeGen(t, s, id, "g"+string(rune('0'+i)), agento11y.Generation{
					AgentName:   "pi",
					Model:       agento11y.ModelRef{Name: "m"},
					StartedAt:   mustParse(t, "2026-05-21T10:00:00Z").Add(time.Duration(i) * time.Minute),
					CompletedAt: mustParse(t, "2026-05-21T10:00:01Z").Add(time.Duration(i) * time.Minute),
				}, "2026-05-21T10:00:01Z")
			}
			got, _, err := s.ListConversations(ConversationListOptions{Limit: tc.limit})
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

// TestListConversations_SkipsMessageTrees proves the summary decoder never
// materialises the stored input and output trees. The second case stores
// trees that are not message-shaped at all: the summary reads them, and so
// does the detail view, which falls back to the same projection and shows
// the turn without messages instead of dropping it.
func TestListConversations_SkipsMessageTrees(t *testing.T) {
	big := strings.Repeat("x", 1<<20)
	cases := []struct {
		name         string
		inputOutput  string
		wantMessages bool
	}{
		{
			name:         "one megabyte message trees",
			inputOutput:  `"input":[{"role":"user","parts":[{"text":"` + big + `"}]}],"output":[{"role":"assistant","parts":[{"text":"` + big + `"}]}]`,
			wantMessages: true,
		},
		{
			name:        "trees that are not message shaped",
			inputOutput: `"input":42,"output":"not-a-message"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			gen := `{"id":"g1","conversation_id":"conv-big","conversation_title":"big session",` +
				`"agent_name":"pi","model":{"provider":"anthropic","name":"claude-sonnet-4"},` +
				`"usage":{"input_tokens":10,"output_tokens":5},` +
				`"started_at":"2026-08-03T10:00:00Z","completed_at":"2026-08-03T10:00:02Z",` +
				`"tags":{"cwd":"/repo"},` + tc.inputOutput + `}`
			require.NoError(t, s.AppendGeneration(generationRecord{
				ReceivedAt:     "2026-08-03T10:00:02Z",
				GenerationID:   "g1",
				ConversationID: "conv-big",
				Generation:     json.RawMessage(gen),
			}))

			got, _, err := s.ListConversations(ConversationListOptions{})
			require.NoError(t, err)
			require.Len(t, got, 1)
			assert.Equal(t, "conv-big", got[0].ID)
			assert.Equal(t, "big session", got[0].Title)
			assert.Equal(t, 1, got[0].Calls)
			assert.Equal(t, int64(15), got[0].TotalTokens)
			assert.Equal(t, []string{"claude-sonnet-4"}, got[0].Models)
			assert.Equal(t, "/repo", got[0].Workspace)

			period, matched, _, err := s.ConversationMetrics(ConversationListOptions{
				Since:  mustParse(t, "2026-08-03T10:00:00Z"),
				Before: mustParse(t, "2026-08-03T11:00:00Z"),
			})
			require.NoError(t, err)
			assert.Equal(t, 1, matched)
			require.Len(t, period, 1)
			assert.Equal(t, 1, period[0].Calls, "malformed message trees retain the period projection")

			// The detail view accepts every line the list counted, so a row
			// in the list always opens.
			detail, err := s.ConversationDetail("conv-big")
			require.NoError(t, err)
			require.NotNil(t, detail)
			require.Len(t, detail.Generations, 1)
			assert.Equal(t, int64(15), detail.Generations[0].TotalTokens)
			if tc.wantMessages {
				assert.NotEmpty(t, detail.Generations[0].Messages)
			} else {
				assert.Empty(t, detail.Generations[0].Messages, "an unreadable tree contributes no messages")
			}
		})
	}
}

func TestListAndDetailUseProviderAwareTotalFallback(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-cache", "g-cache", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:02Z"),
		Usage: agento11y.TokenUsage{
			InputTokens:           21,
			OutputTokens:          10077,
			CacheReadInputTokens:  297770,
			CacheWriteInputTokens: 57497,
		},
	}, "2026-05-21T10:00:02Z")

	summaries, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, summaries, 1)
	assert.Equal(t, int64(365365), summaries[0].TotalTokens)
	assert.Equal(t, TokenBuckets{
		FreshInput: 21,
		CacheRead:  297770,
		CacheWrite: 57497,
		Output:     10077,
	}, summaries[0].TokenBuckets)

	detail, err := s.ConversationDetail("conv-cache")
	require.NoError(t, err)
	require.NotNil(t, detail)
	require.Len(t, detail.Generations, 1)
	assert.Equal(t, int64(365365), detail.Generations[0].TotalTokens)
}

func TestTotalTokensForViewProviderAwareFallback(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		usage    agento11y.TokenUsage
		want     int64
	}{
		{
			name:     "explicit total wins",
			provider: "anthropic",
			usage:    agento11y.TokenUsage{InputTokens: 1, OutputTokens: 2, TotalTokens: 42, CacheReadInputTokens: 100},
			want:     42,
		},
		{
			name:     "anthropic cache is additive",
			provider: "anthropic",
			usage: agento11y.TokenUsage{
				InputTokens:           21,
				OutputTokens:          10077,
				CacheReadInputTokens:  297770,
				CacheWriteInputTokens: 57497,
			},
			want: 365365,
		},
		{
			name:     "openai cache read is inside input",
			provider: "openai",
			usage:    agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			want:     150,
		},
		{
			name:     "gemini reasoning stays additive",
			provider: "gemini",
			usage:    agento11y.TokenUsage{InputTokens: 80, OutputTokens: 40, CacheReadInputTokens: 20, ReasoningTokens: 10},
			want:     130,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, totalTokensForView(tc.usage, tc.provider))
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

	writeGen(t, s, "conv-X", "g-second", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:01:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:01:06.5Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 20, OutputTokens: 10},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "read", InputJSON: readInput}},
		}}},
	}, "2026-05-21T10:01:06.5Z")

	writeGen(t, s, "conv-X", "g-first", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03.19Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindText, Text: "thinking..."},
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "bash", InputJSON: bashInput}},
			// Case-only spelling differences are one tool.
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "Bash", InputJSON: bashInput}},
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
	// Dedup keeps the first spelling; preview unwraps `command`.
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

// TestConversationDetail_ThreadMessages verifies the display-order thread used
// by the local viewer: each step shows its own input followed by its own
// output, nothing moved or dropped. A tool result is rendered on the step that
// received it as input — not folded back into the step that issued the call.
func TestConversationDetail_ThreadMessages(t *testing.T) {
	toolInput, _ := json.Marshal(map[string]any{"command": "ls"})
	toolOutput, _ := json.Marshal([]string{"README.md"})
	type wantMessage struct {
		role       agento11y.Role
		partKind   agento11y.PartKind
		toolCallID string
		text       string
	}
	for _, tc := range []struct {
		name string
		gen  agento11y.Generation
		want []wantMessage
	}{
		{
			name: "step shows its input then its output",
			gen: agento11y.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []agento11y.Message{
					{Role: agento11y.RoleUser, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "list files"}}},
				},
				Output: []agento11y.Message{
					{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "checking"}}},
					{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "call-1", Name: "Bash", InputJSON: toolInput}}}},
				},
			},
			want: []wantMessage{
				{role: agento11y.RoleUser, partKind: agento11y.PartKindText, text: "list files"},
				{role: agento11y.RoleAssistant, partKind: agento11y.PartKindText, text: "checking"},
				{role: agento11y.RoleAssistant, partKind: agento11y.PartKindToolCall, toolCallID: "call-1"},
			},
		},
		{
			name: "tool result renders on the step that received it",
			gen: agento11y.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []agento11y.Message{
					{Role: agento11y.RoleTool, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "call-1", Name: "Bash", ContentJSON: toolOutput}}}},
				},
				Output: []agento11y.Message{
					{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "README.md"}}},
				},
			},
			want: []wantMessage{
				{role: agento11y.RoleTool, partKind: agento11y.PartKindToolResult, toolCallID: "call-1"},
				{role: agento11y.RoleAssistant, partKind: agento11y.PartKindText, text: "README.md"},
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
				case agento11y.PartKindToolCall:
					require.NotNil(t, part.ToolCall, "message %d tool call", i)
					assert.Equal(t, want.toolCallID, part.ToolCall.ID, "message %d tool call id", i)
				case agento11y.PartKindToolResult:
					require.NotNil(t, part.ToolResult, "message %d tool result", i)
					assert.Equal(t, want.toolCallID, part.ToolResult.ToolCallID, "message %d tool result id", i)
				case agento11y.PartKindText:
					assert.Equal(t, want.text, part.Text, "message %d text", i)
				case agento11y.PartKindThinking:
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
		gen   agento11y.Generation
		check func(t *testing.T, view GenerationView)
	}{
		{
			name: "full capture—both sides preserved verbatim",
			gen: agento11y.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []agento11y.Message{{
					Role:  agento11y.RoleUser,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "hey"}},
				}},
				Output: []agento11y.Message{{
					Role:  agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "Hey! What are you working on?"}},
				}},
			},
			check: func(t *testing.T, v GenerationView) {
				require.Len(t, v.Input, 1)
				assert.Equal(t, agento11y.RoleUser, v.Input[0].Role)
				assert.Equal(t, "hey", v.Input[0].Parts[0].Text)
				require.Len(t, v.Output, 1)
				assert.Equal(t, agento11y.RoleAssistant, v.Output[0].Role)
				assert.Equal(t, "Hey! What are you working on?", v.Output[0].Parts[0].Text)
			},
		},
		{
			name: "metadata-only capture—empty messages don't synthesize content",
			gen: agento11y.Generation{
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
			gen: agento11y.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{
						{Kind: agento11y.PartKindText, Text: "running ls"},
						{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "bash", InputJSON: toolInput}},
					},
				}},
			},
			check: func(t *testing.T, v GenerationView) {
				require.Len(t, v.Output, 1)
				parts := v.Output[0].Parts
				require.Len(t, parts, 2)
				assert.Equal(t, agento11y.PartKindText, parts[0].Kind)
				assert.Equal(t, "running ls", parts[0].Text)
				assert.Equal(t, agento11y.PartKindToolCall, parts[1].Kind)
				require.NotNil(t, parts[1].ToolCall)
				assert.Equal(t, "bash", parts[1].ToolCall.Name)
			},
		},
		{
			name: "tool result stays in input and tool call stays in output",
			gen: agento11y.Generation{
				StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
				CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
				Input: []agento11y.Message{
					{Role: agento11y.RoleUser, Parts: []agento11y.Part{{Kind: agento11y.PartKindText, Text: "list files"}}},
					{Role: agento11y.RoleTool, Parts: []agento11y.Part{{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "call-1", Name: "bash", ContentJSON: toolOutput}}}},
				},
				Output: []agento11y.Message{{
					Role:  agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "call-1", Name: "bash", InputJSON: toolInput}}},
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

// TestDisjointTokenUsage covers the split into non-overlapping buckets.
// Usage marked TokenInputSemanticsInclusive carries both cache buckets
// inside input_tokens, so both are carved back out whatever the provider
// is named. Without the marker the provider-name heuristic decides:
// Anthropic keeps cache tokens separate from input; OpenAI, Gemini, and
// codex fold cache_read into input; OpenAI and codex also nest reasoning
// in output while Gemini keeps thoughts additive; unknown providers
// default to "separate" on both axes.
func TestDisjointTokenUsage(t *testing.T) {
	cases := []struct {
		name                                                 string
		provider                                             string
		usage                                                agento11y.TokenUsage
		freshInput, cacheRead, cacheWrite, output, reasoning int64
	}{
		{
			name:       "anthropic keeps cache additive",
			provider:   "anthropic",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
			freshInput: 100, cacheRead: 30, cacheWrite: 20, output: 50, reasoning: 0,
		},
		{
			name:       "openai carves cache_read out of input and reasoning out of output",
			provider:   "openai",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			name:       "gemini fully cached prompt leaves zero fresh input",
			provider:   "gemini",
			usage:      agento11y.TokenUsage{InputTokens: 80, OutputTokens: 20, CacheReadInputTokens: 80},
			freshInput: 0, cacheRead: 80, cacheWrite: 0, output: 20, reasoning: 0,
		},
		{
			// Gemini carves cache_read out of input but keeps thoughts
			// additive: output stays at the candidate count.
			name:       "gemini keeps reasoning additive to output",
			provider:   "gemini",
			usage:      agento11y.TokenUsage{InputTokens: 80, OutputTokens: 40, CacheReadInputTokens: 20, ReasoningTokens: 10},
			freshInput: 60, cacheRead: 20, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// Azure OpenAI shares OpenAI's subset semantics on both axes.
			name:       "azure carves cache_read and reasoning out",
			provider:   "azure",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// The codex agent falls back to provider "codex" for model
			// names it can't attribute; its usage comes from the
			// Responses API, so OpenAI subset semantics apply.
			name:       "codex shares openai subset semantics",
			provider:   "codex",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 70, cacheRead: 30, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// pi calls its Codex backend "openai-codex", but pi normalizes
			// usage in its own client: cache_read is disjoint from input, so
			// subtracting it would hide fresh input the model was charged for.
			// The shape below is the ordinary one in pi data: cache_read far
			// above input, which subset semantics cannot produce.
			name:       "pi openai-codex keeps cache_read additive",
			provider:   "openai-codex",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30_000, ReasoningTokens: 10},
			freshInput: 100, cacheRead: 30_000, cacheWrite: 0, output: 50, reasoning: 10,
		},
		{
			// The same holds when cache_read happens to sit below input, which
			// is where a subset rule would look plausible and still be wrong.
			name:       "pi openai-codex keeps cache_read additive below input",
			provider:   "openai-codex",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
			freshInput: 100, cacheRead: 30, cacheWrite: 0, output: 50, reasoning: 10,
		},
		{
			// pi's Gemini CLI backend reports the same disjoint counts.
			name:       "pi google-antigravity keeps cache_read additive",
			provider:   "google-antigravity",
			usage:      agento11y.TokenUsage{InputTokens: 80, OutputTokens: 40, CacheReadInputTokens: 20_000, ReasoningTokens: 10},
			freshInput: 80, cacheRead: 20_000, cacheWrite: 0, output: 40, reasoning: 10,
		},
		{
			// pi routes some models through a "grafana" provider that speaks the
			// Anthropic messages API, where both buckets are additive.
			name:       "pi grafana keeps anthropic additive semantics",
			provider:   "grafana",
			usage:      agento11y.TokenUsage{InputTokens: 2, OutputTokens: 311, CacheReadInputTokens: 0, CacheWriteInputTokens: 46226},
			freshInput: 2, cacheRead: 0, cacheWrite: 46226, output: 311, reasoning: 0,
		},
		{
			// Unknown provider keeps reasoning additive (never hide output).
			name:       "unknown provider keeps reasoning additive",
			provider:   "openrouter",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 10},
			freshInput: 100, cacheRead: 0, cacheWrite: 0, output: 50, reasoning: 10,
		},
		{
			name:       "unknown provider defaults to separate (no subtraction)",
			provider:   "mystery-llm",
			usage:      agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30},
			freshInput: 100, cacheRead: 30, cacheWrite: 0, output: 50, reasoning: 0,
		},
		{
			name:       "empty provider defaults to separate",
			provider:   "",
			usage:      agento11y.TokenUsage{InputTokens: 100, CacheReadInputTokens: 30},
			freshInput: 100, cacheRead: 30, cacheWrite: 0, output: 0, reasoning: 0,
		},
		{
			name:       "subset cache_read larger than input clamps fresh input to zero",
			provider:   "openai",
			usage:      agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5, CacheReadInputTokens: 30},
			freshInput: 0, cacheRead: 30, cacheWrite: 0, output: 5, reasoning: 0,
		},
		{
			name:       "reasoning larger than output clamps output to zero",
			provider:   "openai",
			usage:      agento11y.TokenUsage{InputTokens: 20, OutputTokens: 5, ReasoningTokens: 10},
			freshInput: 20, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 10,
		},
		{
			name:       "negative values clamp to zero",
			provider:   "anthropic",
			usage:      agento11y.TokenUsage{InputTokens: -5, OutputTokens: -1, CacheReadInputTokens: -3},
			freshInput: 0, cacheRead: 0, cacheWrite: 0, output: 0, reasoning: 0,
		},
		{
			// The claude-code mapper's golden turn: input_tokens is the
			// OTel-inclusive count (160 fresh + 15 cache read), so the
			// buckets must sum to the reported total of 193, not 208.
			name:     "inclusive anthropic carves cache_read out of input",
			provider: "anthropic",
			usage: agento11y.TokenUsage{
				InputTokens:          175,
				OutputTokens:         18,
				TotalTokens:          193,
				CacheReadInputTokens: 15,
				InputSemantics:       agento11y.TokenInputSemanticsInclusive,
			},
			freshInput: 160, cacheRead: 15, cacheWrite: 0, output: 18, reasoning: 0,
		},
		{
			// cache_write is inside input under the inclusive contract too,
			// unlike the provider-raw Anthropic case above it.
			name:     "inclusive anthropic carves both cache buckets out of input",
			provider: "anthropic",
			usage: agento11y.TokenUsage{
				InputTokens:           170,
				OutputTokens:          50,
				TotalTokens:           220,
				CacheReadInputTokens:  30,
				CacheWriteInputTokens: 20,
				InputSemantics:        agento11y.TokenInputSemanticsInclusive,
			},
			freshInput: 120, cacheRead: 30, cacheWrite: 20, output: 50, reasoning: 0,
		},
		{
			// The marker, not the provider name, decides the input axis; the
			// reasoning carve-out stays provider-driven.
			name:     "inclusive marker also applies to openai and leaves reasoning provider-driven",
			provider: "openai",
			usage: agento11y.TokenUsage{
				InputTokens:           100,
				OutputTokens:          50,
				CacheReadInputTokens:  30,
				CacheWriteInputTokens: 10,
				ReasoningTokens:       10,
				InputSemantics:        agento11y.TokenInputSemanticsInclusive,
			},
			freshInput: 60, cacheRead: 30, cacheWrite: 10, output: 40, reasoning: 10,
		},
		{
			// A fully cached prompt leaves no fresh input, and an
			// over-reported cache never turns fresh input negative.
			name:     "inclusive fully cached prompt leaves zero fresh input",
			provider: "anthropic",
			usage: agento11y.TokenUsage{
				InputTokens:          15,
				OutputTokens:         18,
				CacheReadInputTokens: 20,
				InputSemantics:       agento11y.TokenInputSemanticsInclusive,
			},
			freshInput: 0, cacheRead: 20, cacheWrite: 0, output: 18, reasoning: 0,
		},
		{
			// Same numbers as the golden turn without the marker: legacy
			// records keep the additive provider heuristic exactly as before.
			name:     "unspecified anthropic keeps the additive heuristic",
			provider: "anthropic",
			usage: agento11y.TokenUsage{
				InputTokens:          175,
				OutputTokens:         18,
				TotalTokens:          193,
				CacheReadInputTokens: 15,
			},
			freshInput: 175, cacheRead: 15, cacheWrite: 0, output: 18, reasoning: 0,
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

// TestInputSemanticsMarkerSurvivesTheStore pins the whole local path for
// the marker the claude-code mapper sets. An inclusive Anthropic turn
// must reach every reader (detail view, conversation list, token chart,
// search) with disjoint buckets that sum to the reported total, and a
// record without the marker must keep the old additive numbers. The
// golden turn below is the one in
// internal/entry/testdata/golden/claude-code-subagent: input 175
// (inclusive of a 15-token cache read), output 18, total 193.
func TestInputSemanticsMarkerSurvivesTheStore(t *testing.T) {
	cases := []struct {
		name        string
		usageJSON   string
		wantBuckets TokenBuckets
		wantTotal   int64
		// wantBucketSum is what the five buckets add up to. For
		// inclusive usage it equals the reported total; for a legacy
		// record the additive heuristic still overshoots it, which is
		// exactly the behaviour this test freezes.
		wantBucketSum int64
	}{
		{
			name:          "inclusive marker as the proto-json enum name",
			usageJSON:     `{"input_tokens":"175","output_tokens":"18","total_tokens":"193","cache_read_input_tokens":"15","input_semantics":"TOKEN_INPUT_SEMANTICS_INCLUSIVE"}`,
			wantBuckets:   TokenBuckets{FreshInput: 160, CacheRead: 15, Output: 18},
			wantTotal:     193,
			wantBucketSum: 193,
		},
		{
			name:          "inclusive marker as the enum number",
			usageJSON:     `{"input_tokens":175,"output_tokens":18,"total_tokens":193,"cache_read_input_tokens":15,"input_semantics":1}`,
			wantBuckets:   TokenBuckets{FreshInput: 160, CacheRead: 15, Output: 18},
			wantTotal:     193,
			wantBucketSum: 193,
		},
		{
			name:          "record without the marker keeps the additive heuristic",
			usageJSON:     `{"input_tokens":"175","output_tokens":"18","total_tokens":"193","cache_read_input_tokens":"15"}`,
			wantBuckets:   TokenBuckets{FreshInput: 175, CacheRead: 15, Output: 18},
			wantTotal:     193,
			wantBucketSum: 208,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			// Written as raw JSON, not through agento11y.Generation, so the
			// test also covers the encoding the HTTP receiver stores.
			raw := json.RawMessage(`{"id":"g-sem","conversation_id":"conv-sem","agent_name":"claude-code",` +
				`"model":{"provider":"anthropic","name":"claude-sonnet-4"},` +
				`"started_at":"2026-05-21T10:00:00Z","completed_at":"2026-05-21T10:00:02Z",` +
				`"output":[{"role":"MESSAGE_ROLE_ASSISTANT","parts":[{"kind":"text","text":"semantics"}]}],` +
				`"usage":` + tc.usageJSON + `}`)
			require.NoError(t, s.AppendGeneration(generationRecord{
				ReceivedAt:     "2026-05-21T10:00:02Z",
				GenerationID:   "g-sem",
				ConversationID: "conv-sem",
				Generation:     raw,
			}))

			detail, err := s.ConversationDetail("conv-sem")
			require.NoError(t, err)
			require.NotNil(t, detail)
			require.Len(t, detail.Generations, 1)
			assert.Equal(t, tc.wantBuckets, detail.Generations[0].TokenBuckets, "detail buckets")
			assert.Equal(t, tc.wantTotal, detail.Generations[0].TotalTokens, "detail total")

			summaries, _, err := s.ListConversations(ConversationListOptions{})
			require.NoError(t, err)
			require.Len(t, summaries, 1)
			assert.Equal(t, tc.wantBuckets, summaries[0].TokenBuckets, "list buckets")
			assert.Equal(t, tc.wantTotal, summaries[0].TotalTokens, "list total")

			points, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Hour})
			require.NoError(t, err)
			require.Len(t, points, 1)
			assert.Equal(t, tc.wantBuckets, points[0].TokenBuckets, "chart buckets")

			hits, err := s.SearchConversations("semantics", 10)
			require.NoError(t, err)
			require.Len(t, hits, 1)
			assert.Equal(t, tc.wantBuckets, hits[0].TokenBuckets, "search buckets")

			b := detail.Generations[0].TokenBuckets
			sum := b.FreshInput + b.CacheRead + b.CacheWrite + b.Output + b.Reasoning
			assert.Equal(t, tc.wantBucketSum, sum, "buckets must stay disjoint")
		})
	}
}

// TestTokenUsagePoints seeds generations across conversations and checks
// the flattened, time-sorted points: provider-aware buckets, model and
// provider tagging, call counts, the received_at timestamp fallback, and that
// timestamps bucketing cannot place are dropped.
func TestTokenUsagePoints(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:10Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:12Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
	}, "2026-05-21T10:00:12Z")

	// Earlier than g1 so it must sort first; OpenAI subset semantics.
	writeGen(t, s, "conv-B", "g2", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "openai", Name: "gpt-5-omni"},
		StartedAt:   mustParse(t, "2026-05-21T09:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T09:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, ReasoningTokens: 10},
	}, "2026-05-21T09:00:01Z")

	// No started/completed: timestamp must fall back to received_at.
	writeGen(t, s, "conv-C", "g3", agento11y.Generation{
		Model: agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		Usage: agento11y.TokenUsage{InputTokens: 5, OutputTokens: 3},
	}, "2026-05-21T12:00:00Z")

	// Zero tokens: retained because the generation is still a model call.
	writeGen(t, s, "conv-D", "g4", agento11y.Generation{
		Model:     agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt: mustParse(t, "2026-05-21T08:00:00Z"),
	}, "2026-05-21T08:00:00Z")

	// Past the year UnixNano covers: bucketing would wrap this into 1715 and
	// add phantom tokens to a real bar, so the point is dropped.
	writeGen(t, s, "conv-E", "g5", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2300-01-01T00:00:00Z"),
		CompletedAt: mustParse(t, "2300-01-01T00:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 7, OutputTokens: 3},
	}, "2300-01-01T00:00:01Z")

	// One-minute buckets keep every seeded generation in its own bucket.
	points, interval, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, time.Minute, interval)
	require.Len(t, points, 4, "only the unplaceable generation should be dropped")

	// Sorted oldest-first: g4 (08:00) → g2 (09:00) → g1 (10:00) → g3 (12:00 received_at).
	assert.Equal(t, "claude-opus-4-7", points[0].Model)
	assert.Equal(t, "gpt-5-omni", points[1].Model)
	assert.Equal(t, "claude-sonnet-4", points[2].Model)
	assert.Equal(t, "claude-opus-4-7", points[3].Model)
	assert.Equal(t, 1, points[0].Calls)
	assert.Equal(t, TokenBuckets{}, points[0].TokenBuckets)

	// g2 OpenAI: cache_read carved out of input, reasoning out of output.
	assert.Equal(t, TokenUsagePoint{
		Timestamp:    mustParse(t, "2026-05-21T09:00:00Z"),
		Model:        "gpt-5-omni",
		Provider:     "openai",
		Calls:        1,
		TokenBuckets: TokenBuckets{FreshInput: 70, CacheRead: 30, Output: 40, Reasoning: 10},
	}, points[1])

	// g1 Anthropic: cache stays additive; the point carries its bucket start.
	assert.Equal(t, TokenUsagePoint{
		Timestamp:    mustParse(t, "2026-05-21T10:00:00Z"),
		Model:        "claude-sonnet-4",
		Provider:     "anthropic",
		Calls:        1,
		TokenBuckets: TokenBuckets{FreshInput: 100, CacheRead: 30, CacheWrite: 20, Output: 50},
	}, points[2])

	// g3 timestamp falls back to received_at.
	assert.Equal(t, mustParse(t, "2026-05-21T12:00:00Z"), points[3].Timestamp)
	assert.Equal(t, 1, points[3].Calls)
}

func TestTokenUsagePointsBeforeAndWorkspace(t *testing.T) {
	s := newStorage(t)
	for _, seed := range []struct {
		conv, when, workspace string
		tokens                int64
	}{
		{conv: "repo-inside", when: "2026-05-21T10:00:00Z", workspace: "/repo", tokens: 10},
		{conv: "repo-upper", when: "2026-05-21T11:00:00Z", workspace: "/repo", tokens: 100},
		{conv: "other", when: "2026-05-21T10:30:00Z", workspace: "/other", tokens: 1000},
		{conv: "unknown", when: "2026-05-21T10:15:00Z", tokens: 7},
	} {
		writeGen(t, s, seed.conv, "g", agento11y.Generation{
			StartedAt: mustParse(t, seed.when),
			Usage:     agento11y.TokenUsage{InputTokens: seed.tokens},
			Tags:      map[string]string{"cwd": seed.workspace},
		}, seed.when)
	}
	since := mustParse(t, "2026-05-21T10:00:00Z")
	before := mustParse(t, "2026-05-21T11:00:00Z")
	repo := "/repo"
	points, _, err := s.TokenUsagePoints(TokenUsageOptions{
		Since: since, Before: before, Workspace: &repo, Interval: time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, int64(10), points[0].FreshInput, "before is exclusive and other workspaces are excluded")

	blank := ""
	points, _, err = s.TokenUsagePoints(TokenUsageOptions{
		Since: since, Before: before, Workspace: &blank, Interval: time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, int64(7), points[0].FreshInput, "present blank selects unknown workspace")
}

// TestTokenUsagePoints_InvertedTimestampsStayInRange covers completed_at
// preceding started_at. Metrics prefer started_at; list activity uses the
// later of started_at and completed_at.
func TestTokenUsagePoints_InvertedTimestampsStayInRange(t *testing.T) {
	s := newStorage(t)
	started := mustParse(t, "2026-05-21T10:00:00Z")
	writeGen(t, s, "conv-inverted", "g1", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   started,
		CompletedAt: started.Add(-30 * time.Minute),
		Usage:       agento11y.TokenUsage{InputTokens: 5, OutputTokens: 3},
	}, started.Format(time.RFC3339Nano))

	points, _, err := s.TokenUsagePoints(TokenUsageOptions{
		Since:    started.Add(-time.Minute),
		Interval: time.Hour,
	})
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, int64(5), points[0].FreshInput)

	summaries, _, err := s.ListConversations(ConversationListOptions{Since: started.Add(-time.Minute)})
	require.NoError(t, err)
	require.Len(t, summaries, 1, "the list keeps the conversation its own bound covers")
}

// TestTokenUsagePoints_Buckets covers the aggregation the chart reads: one
// point per bucket and model, ordered by timestamp then model, plus the
// range bound and the derived interval.
func TestTokenUsagePoints_Buckets(t *testing.T) {
	type seed struct {
		conv  string
		gen   string
		model string
		when  string
		in    int64
		out   int64
	}
	hourSeeds := []seed{
		{conv: "conv-1", gen: "g1", model: "model-a", when: "2026-08-03T10:05:00Z", in: 10},
		{conv: "conv-1", gen: "g2", model: "model-a", when: "2026-08-03T10:55:00Z", in: 20},
		{conv: "conv-2", gen: "g3", model: "model-b", when: "2026-08-03T10:30:00Z", in: 7},
		{conv: "conv-1", gen: "g4", model: "model-a", when: "2026-08-03T11:05:00Z", in: 5},
	}

	cases := []struct {
		name         string
		seeds        []seed
		opts         TokenUsageOptions
		wantInterval time.Duration
		wantPoints   []TokenUsagePoint
	}{
		{
			name:         "hourly buckets group by model",
			seeds:        hourSeeds,
			opts:         TokenUsageOptions{Interval: time.Hour},
			wantInterval: time.Hour,
			wantPoints: []TokenUsagePoint{
				{Timestamp: mustParse(t, "2026-08-03T10:00:00Z"), Model: "model-a", Calls: 2, TokenBuckets: TokenBuckets{FreshInput: 30}},
				{Timestamp: mustParse(t, "2026-08-03T10:00:00Z"), Model: "model-b", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 7}},
				{Timestamp: mustParse(t, "2026-08-03T11:00:00Z"), Model: "model-a", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 5}},
			},
		},
		{
			name:         "since drops earlier buckets",
			seeds:        hourSeeds,
			opts:         TokenUsageOptions{Interval: time.Hour, Since: mustParse(t, "2026-08-03T11:00:00Z")},
			wantInterval: time.Hour,
			wantPoints: []TokenUsagePoint{
				{Timestamp: mustParse(t, "2026-08-03T11:00:00Z"), Model: "model-a", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 5}},
			},
		},
		{
			// A conversation an import wrote carries a modification time of
			// now and generations that are months old, so the range bound has
			// to cut inside the file. The interval follows the points that
			// survive it, not the whole file's span.
			name: "a file straddling the bound is read from the bound",
			seeds: []seed{
				{conv: "conv-1", gen: "g1", model: "model-a", when: "2026-05-05T00:00:00Z", in: 3},
				{conv: "conv-1", gen: "g2", model: "model-a", when: "2026-08-03T10:00:00Z", in: 5},
			},
			opts:         TokenUsageOptions{Since: mustParse(t, "2026-08-03T09:00:00Z")},
			wantInterval: 10 * time.Second,
			wantPoints: []TokenUsagePoint{
				{Timestamp: mustParse(t, "2026-08-03T10:00:00Z"), Model: "model-a", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 5}},
			},
		},
		{
			name: "derived interval keeps the bucket count bounded",
			seeds: []seed{
				{conv: "conv-1", gen: "g1", model: "model-a", when: "2026-07-04T00:00:00Z", in: 1},
				{conv: "conv-2", gen: "g2", model: "model-a", when: "2026-08-03T00:00:00Z", in: 2},
			},
			// 30 days at one-hour buckets is 720, above the 500 cap, so the
			// next ladder step is used.
			wantInterval: 2 * time.Hour,
			wantPoints: []TokenUsagePoint{
				{Timestamp: mustParse(t, "2026-07-04T00:00:00Z"), Model: "model-a", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 1}},
				{Timestamp: mustParse(t, "2026-08-03T00:00:00Z"), Model: "model-a", Calls: 1, TokenBuckets: TokenBuckets{FreshInput: 2}},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			for _, sd := range tc.seeds {
				writeGen(t, s, sd.conv, sd.gen, agento11y.Generation{
					Model:     agento11y.ModelRef{Name: sd.model},
					StartedAt: mustParse(t, sd.when),
					Usage:     agento11y.TokenUsage{InputTokens: sd.in, OutputTokens: sd.out},
				}, sd.when)
			}
			points, interval, err := s.TokenUsagePoints(tc.opts)
			require.NoError(t, err)
			assert.Equal(t, tc.wantInterval, interval)
			assert.Equal(t, tc.wantPoints, points)
		})
	}
}

// TestTokenUsagePoints_ScaleFollowsBucketsNotGenerations seeds a store the
// size an import produces and asserts the response follows the bucket and
// model count: six hourly buckets across three models stay at 18 points
// and well under 100 KB, whatever the generation count is.
func TestTokenUsagePoints_ScaleFollowsBucketsNotGenerations(t *testing.T) {
	s := newStorage(t)
	const (
		generations = 60_000
		perConv     = 1_000
	)
	models := []string{"model-a", "model-b", "model-c"}
	start := mustParse(t, "2026-08-03T06:00:00Z")

	for conv := range generations / perConv {
		convID := "conv-" + strconv.Itoa(conv)
		recs := make([]generationRecord, 0, perConv)
		for i := range perConv {
			n := conv*perConv + i
			genID := "g-" + strconv.Itoa(n)
			// Spread the generations across a six-hour window and three
			// models; each carries one input token.
			raw, err := json.Marshal(agento11y.Generation{
				ID:             genID,
				ConversationID: convID,
				Model:          agento11y.ModelRef{Name: models[n%len(models)]},
				StartedAt:      start.Add(time.Duration(n%(6*60)) * time.Minute),
				Usage:          agento11y.TokenUsage{InputTokens: 1},
			})
			require.NoError(t, err)
			recs = append(recs, generationRecord{
				ReceivedAt:     start.Format(time.RFC3339Nano),
				GenerationID:   genID,
				ConversationID: convID,
				Generation:     raw,
			})
		}
		written, err := s.AppendGenerations(convID, recs)
		require.NoError(t, err)
		require.Equal(t, perConv, written)
	}

	points, interval, err := s.TokenUsagePoints(TokenUsageOptions{
		Since:    start,
		Interval: time.Hour,
	})
	require.NoError(t, err)
	assert.Equal(t, time.Hour, interval)
	assert.LessOrEqual(t, len(points), 6*len(models), "points follow buckets times models")

	var total int64
	var calls int
	for _, p := range points {
		total += p.FreshInput
		calls += p.Calls
	}
	assert.Equal(t, int64(generations), total, "bucketing must preserve every token")
	assert.Equal(t, generations, calls, "bucketing must preserve every call")

	body, err := json.Marshal(map[string]any{"points": points, "interval_seconds": 3600})
	require.NoError(t, err)
	assert.Less(t, len(body), 100*1024, "token chart payload must stay under 100 KB")
}

// TestTokenUsagePoints_EmptyStore checks that TokenUsagePoints returns
// no points and no error before any conversations exist.
func TestTokenUsagePoints_EmptyStore(t *testing.T) {
	s := newStorage(t)
	points, interval, err := s.TokenUsagePoints(TokenUsageOptions{})
	require.NoError(t, err)
	assert.Empty(t, points)
	assert.Equal(t, 10*time.Second, interval, "an empty range derives the finest bucket")
}

// TestListConversations_WorkspaceFacets checks that the list summary
// derives the workspace and branch from the generation's cwd/git.branch
// tags (first non-empty across the conversation) and counts subagent
// steps by the "parent/child" agent_name suffix. These back the Sessions
// view's workspace sidebar and orchestration signal.
func TestListConversations_WorkspaceFacets(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		AgentName:   "claude-code",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Tags:        map[string]string{"cwd": "/some/repo", "git.branch": "main"},
	}, "2026-05-21T10:00:01Z")
	// A spawned subagent step: agent_name carries a "parent/child" suffix.
	// cwd is left blank here to prove the first non-empty wins.
	writeGen(t, s, "conv-A", "g2", agento11y.Generation{
		AgentName:   "claude-code/general-purpose",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:02Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 4, OutputTokens: 2},
	}, "2026-05-21T10:00:03Z")

	writeGen(t, s, "conv-B", "g3", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T11:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T11:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 3, OutputTokens: 1},
		Tags:        map[string]string{"cwd": "/other/repo"},
	}, "2026-05-21T11:00:01Z")

	got, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, got, 2)

	byID := map[string]ConversationSummary{}
	for _, c := range got {
		byID[c.ID] = c
	}

	a := byID["conv-A"]
	assert.Equal(t, "/some/repo", a.Workspace)
	assert.Equal(t, "main", a.Branch)
	assert.Equal(t, 1, a.Subagents, "one generation carries a parent/child agent_name suffix")

	b := byID["conv-B"]
	assert.Equal(t, "/other/repo", b.Workspace)
	assert.Empty(t, b.Branch)
	assert.Equal(t, 0, b.Subagents)
}

// TestConversationDetail_SubagentTreeAndThinking checks that the detail
// view carries the ParentGenerationIDs edges (used to build the subagent
// tree) and the thinking flag through to each step.
func TestConversationDetail_SubagentTreeAndThinking(t *testing.T) {
	s := newStorage(t)

	thinking := true
	writeGen(t, s, "conv-T", "parent-1", agento11y.Generation{
		AgentName:       "claude-code",
		Model:           agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:       mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt:     mustParse(t, "2026-05-21T10:00:01Z"),
		Usage:           agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
		ThinkingEnabled: &thinking,
	}, "2026-05-21T10:00:01Z")
	writeGen(t, s, "conv-T", "child-1", agento11y.Generation{
		AgentName:           "claude-code/general-purpose",
		Model:               agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:           mustParse(t, "2026-05-21T10:00:02Z"),
		CompletedAt:         mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:               agento11y.TokenUsage{InputTokens: 4, OutputTokens: 2},
		ParentGenerationIDs: []string{"parent-1"},
	}, "2026-05-21T10:00:03Z")

	got, err := s.ConversationDetail("conv-T")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Generations, 2)

	byID := map[string]GenerationView{}
	for _, g := range got.Generations {
		byID[g.GenerationID] = g
	}

	parent := byID["parent-1"]
	assert.True(t, parent.ThinkingEnabled, "parent step reasoned")
	assert.Empty(t, parent.ParentGenerationIDs)

	child := byID["child-1"]
	assert.False(t, child.ThinkingEnabled)
	assert.Equal(t, []string{"parent-1"}, child.ParentGenerationIDs, "child links back to its spawning generation")
}

// TestConversationDetail_MediaPartsPreserved checks that media message
// parts (upstream #386878f) survive the store round-trip and come back
// through the query layer with their kind and ordering intact.
func TestConversationDetail_MediaPartsPreserved(t *testing.T) {
	s := newStorage(t)

	writeGen(t, s, "conv-M", "g-media", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Input: []agento11y.Message{{Role: agento11y.RoleUser, Parts: []agento11y.Part{
			{Kind: agento11y.PartKindText, Text: "look at this"},
			{Kind: agento11y.PartKindMedia, Media: &agento11y.Media{Kind: "image", URL: "https://example.com/a.png", MIMEType: "image/png", Name: "a.png"}},
		}}},
	}, "2026-05-21T10:00:01Z")

	got, err := s.ConversationDetail("conv-M")
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Len(t, got.Generations, 1)

	parts := got.Generations[0].Input[0].Parts
	require.Len(t, parts, 2)
	assert.Equal(t, agento11y.PartKindText, parts[0].Kind)
	require.Equal(t, agento11y.PartKindMedia, parts[1].Kind)
	require.NotNil(t, parts[1].Media)
	assert.Equal(t, "https://example.com/a.png", parts[1].Media.URL)
	assert.Equal(t, "image/png", parts[1].Media.MIMEType)
}

// TestReadsReportSkippedLines covers a line no projection can decode: the
// truncated tail an interrupted append leaves behind. Every read drops it,
// keeps the records around it, and says so once per request.
func TestReadsReportSkippedLines(t *testing.T) {
	s := newStorage(t)
	var logs strings.Builder
	s.SetLogger(log.New(&logs, "", 0))

	writeGen(t, s, "conv-A", "g1", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-08-03T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-08-03T10:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, "2026-08-03T10:00:01Z")
	path := filepath.Join(s.Dir(), ConversationsDir, "conv-A.jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"received_at":"2026-08-03T10:00:02Z","generation` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	convs, total, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, convs, 1)
	assert.Equal(t, 1, total)
	assert.Equal(t, 1, convs[0].Calls)

	detail, err := s.ConversationDetail("conv-A")
	require.NoError(t, err)
	require.NotNil(t, detail)
	assert.Len(t, detail.Generations, 1)

	points, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Minute})
	require.NoError(t, err)
	assert.Len(t, points, 1)

	// Each fallback read scans the legacy file and reports its skipped tail.
	again, _, err := s.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	assert.Equal(t, convs, again)
	againDetail, err := s.ConversationDetail("conv-A")
	require.NoError(t, err)
	assert.Equal(t, detail, againDetail)
	againPoints, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Minute})
	require.NoError(t, err)
	assert.Equal(t, points, againPoints)

	for _, want := range []string{
		"local: list conversations: skipped 1 unparseable lines",
		"local: conversation conv-A: skipped 1 unparseable lines",
		"local: token metrics: skipped 1 unparseable lines",
	} {
		assert.Equal(t, 2, strings.Count(logs.String(), want), "%q reported once per request", want)
	}
}

// TestTokenUsagePointsFollowAppends checks that usage appended between two
// requests lands in the totals.
func TestTokenUsagePointsFollowAppends(t *testing.T) {
	s := newStorage(t)
	for _, id := range []string{"conv-A", "conv-B"} {
		writeGen(t, s, id, "g-"+id, agento11y.Generation{
			Model:     agento11y.ModelRef{Name: "m"},
			StartedAt: mustParse(t, "2026-08-03T10:00:00Z"),
			Usage:     agento11y.TokenUsage{InputTokens: 4, OutputTokens: 1},
		}, "2026-08-03T10:00:01Z")
	}
	before, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Hour})
	require.NoError(t, err)
	require.Len(t, before, 1)
	assert.Equal(t, TokenBuckets{FreshInput: 8, Output: 2}, before[0].TokenBuckets)

	writeGen(t, s, "conv-A", "g-extra", agento11y.Generation{
		Model:     agento11y.ModelRef{Name: "m"},
		StartedAt: mustParse(t, "2026-08-03T10:30:00Z"),
		Usage:     agento11y.TokenUsage{InputTokens: 13, OutputTokens: 2},
	}, "2026-08-03T10:30:01Z")

	after, _, err := s.TokenUsagePoints(TokenUsageOptions{Interval: time.Hour})
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Equal(t, TokenBuckets{FreshInput: 21, Output: 4}, after[0].TokenBuckets)
}
