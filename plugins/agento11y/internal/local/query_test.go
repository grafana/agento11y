package local

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
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

	// The order comes from the file modification time, and on Linux the three
	// writes above land in one filesystem timestamp tick, which leaves them
	// tied. Pin each file to its last received_at so the expected order is the
	// one the records describe.
	setConversationModTime(t, s, "conv-A", mustParse(t, "2026-05-21T10:00:13Z"))
	setConversationModTime(t, s, "conv-B", mustParse(t, "2026-05-21T11:00:01Z"))
	setConversationModTime(t, s, "conv-C", mustParse(t, "2026-05-21T11:10:00Z"))

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
				// One tick holds every write on Linux, so the modification
				// times tie and the newest-first order falls back to the id.
				// Pin them a minute apart, matching the seeded activity.
				setConversationModTime(t, s, id, mustParse(t, "2026-05-21T10:00:01Z").Add(time.Duration(i)*time.Minute))
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

// TestListConversations_BoundedPage proves the page is cut before any
// conversation file is decoded: entries are ordered by modification time,
// the limit ends the walk, and ?since= drops older files. Files past the
// page are made unreadable, so a request that would open one fails and a
// bounded request cannot pass by accident.
func TestListConversations_BoundedPage(t *testing.T) {
	modTimes := map[string]string{
		"conv-a": "2026-08-03T12:00:00Z",
		"conv-b": "2026-08-03T11:00:00Z",
		"conv-c": "2026-08-03T10:00:00Z",
	}
	oldestFirst := []string{"conv-c", "conv-b", "conv-a"}

	cases := []struct {
		name string
		opts ConversationListOptions
		// blockFrom makes the files older than position blockFrom (in the
		// returned newest-first order) unreadable. -1 blocks nothing.
		blockFrom int
		wantIDs   []string
		wantErr   bool
	}{
		{name: "newest modification time first", blockFrom: -1, wantIDs: []string{"conv-a", "conv-b", "conv-c"}},
		{name: "limit stops before the older files", opts: ConversationListOptions{Limit: 2}, blockFrom: 2, wantIDs: []string{"conv-a", "conv-b"}},
		{name: "since excludes older files before decoding", opts: ConversationListOptions{Since: mustParse(t, "2026-08-03T11:30:00Z")}, blockFrom: 1, wantIDs: []string{"conv-a"}},
		{name: "since bound is inclusive", opts: ConversationListOptions{Since: mustParse(t, "2026-08-03T11:00:00Z")}, blockFrom: 2, wantIDs: []string{"conv-a", "conv-b"}},
		{name: "unbounded request reads every file", blockFrom: 2, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.blockFrom >= 0 {
				requireUnreadableFilesSupported(t)
			}
			s := newStorage(t)
			// Write oldest-first so the modification times below are the
			// only thing the order can come from.
			for _, id := range oldestFirst {
				writeGen(t, s, id, "g-"+id, agento11y.Generation{
					AgentName:   "pi",
					Model:       agento11y.ModelRef{Name: "m"},
					StartedAt:   mustParse(t, modTimes[id]),
					CompletedAt: mustParse(t, modTimes[id]),
					Usage:       agento11y.TokenUsage{InputTokens: 1, OutputTokens: 1},
				}, modTimes[id])
				setConversationModTime(t, s, id, mustParse(t, modTimes[id]))
			}
			if tc.blockFrom >= 0 {
				for _, id := range []string{"conv-a", "conv-b", "conv-c"}[tc.blockFrom:] {
					blockConversationFile(t, s, id)
				}
			}

			got, _, err := s.ListConversations(tc.opts)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			ids := make([]string, 0, len(got))
			for _, c := range got {
				ids = append(ids, c.ID)
			}
			assert.Equal(t, tc.wantIDs, ids)
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

// setConversationModTime pins one conversation file's modification time.
// The list and metrics endpoints order and filter on that timestamp.
func setConversationModTime(t *testing.T, s *Storage, convID string, when time.Time) {
	t.Helper()
	path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
	require.NoError(t, os.Chtimes(path, when, when))
}

// blockConversationFile makes one conversation file unreadable, so any
// request that opens it fails. Tests use it to prove a bounded request
// never touches the files past its page.
func blockConversationFile(t *testing.T, s *Storage, convID string) {
	t.Helper()
	path := filepath.Join(s.Dir(), ConversationsDir, convID+".jsonl")
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

// requireUnreadableFilesSupported skips tests that rely on a file being
// unreadable, which neither Windows ACLs nor a root user honour.
func requireUnreadableFilesSupported(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("posix-only permission check")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

func TestListAndDetailUseCacheAwareTotalFallback(t *testing.T) {
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
			// Duplicate name to confirm dedup.
			{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{Name: "bash", InputJSON: bashInput}},
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

// TestDisjointTokenUsage covers the provider-aware split into
// non-overlapping buckets. Anthropic keeps cache tokens separate from
// input; OpenAI, Gemini, and codex fold cache_read into input; OpenAI
// and codex also nest reasoning in output while Gemini keeps thoughts
// additive; unknown providers default to "separate" on both axes.
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
// zero-token generations and timestamps bucketing cannot place are dropped.
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

	// Zero tokens: must be dropped entirely.
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
	require.Len(t, points, 3, "zero-token and unplaceable generations should be dropped")

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

	// g1 Anthropic: cache stays additive; the point carries its bucket start.
	assert.Equal(t, TokenUsagePoint{
		Timestamp:    mustParse(t, "2026-05-21T10:00:00Z"),
		Model:        "claude-sonnet-4",
		Provider:     "anthropic",
		TokenBuckets: TokenBuckets{FreshInput: 100, CacheRead: 30, CacheWrite: 20, Output: 50},
	}, points[1])

	// g3 timestamp falls back to received_at.
	assert.Equal(t, mustParse(t, "2026-05-21T12:00:00Z"), points[2].Timestamp)
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
				{Timestamp: mustParse(t, "2026-08-03T10:00:00Z"), Model: "model-a", TokenBuckets: TokenBuckets{FreshInput: 30}},
				{Timestamp: mustParse(t, "2026-08-03T10:00:00Z"), Model: "model-b", TokenBuckets: TokenBuckets{FreshInput: 7}},
				{Timestamp: mustParse(t, "2026-08-03T11:00:00Z"), Model: "model-a", TokenBuckets: TokenBuckets{FreshInput: 5}},
			},
		},
		{
			name:         "since drops earlier buckets",
			seeds:        hourSeeds,
			opts:         TokenUsageOptions{Interval: time.Hour, Since: mustParse(t, "2026-08-03T11:00:00Z")},
			wantInterval: time.Hour,
			wantPoints: []TokenUsagePoint{
				{Timestamp: mustParse(t, "2026-08-03T11:00:00Z"), Model: "model-a", TokenBuckets: TokenBuckets{FreshInput: 5}},
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
				{Timestamp: mustParse(t, "2026-07-04T00:00:00Z"), Model: "model-a", TokenBuckets: TokenBuckets{FreshInput: 1}},
				{Timestamp: mustParse(t, "2026-08-03T00:00:00Z"), Model: "model-a", TokenBuckets: TokenBuckets{FreshInput: 2}},
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
	for _, p := range points {
		total += p.FreshInput
	}
	assert.Equal(t, int64(generations), total, "bucketing must preserve every token")

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

	for _, want := range []string{
		"local: list conversations: skipped 1 unparseable lines",
		"local: conversation conv-A: skipped 1 unparseable lines",
		"local: token metrics: skipped 1 unparseable lines",
	} {
		assert.Contains(t, logs.String(), want)
	}
}
