package local

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type readPathSnapshot struct {
	List        []ConversationSummary
	Total       int
	Detail      *ConversationDetail
	Points      []TokenUsagePoint
	Interval    time.Duration
	Search      []SearchHit
	ListJSON    []byte
	DetailJSON  []byte
	MetricsJSON []byte
	SearchJSON  []byte
}

func TestSQLiteReadPathsMatchJSONLResponses(t *testing.T) {
	storage := newStorage(t)
	first := mustParse(t, "2026-05-21T10:00:00Z")
	second := mustParse(t, "2026-05-21T10:01:00Z")
	writeGen(t, storage, "conv-A", "g1", agento11y.Generation{
		ConversationTitle: "Concurrency issue",
		AgentName:         "pi",
		Model:             agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:         first,
		CompletedAt:       first.Add(2 * time.Second),
		Usage: agento11y.TokenUsage{
			InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 20, CacheWriteInputTokens: 10,
		},
		Tags:   map[string]string{"cwd": "/work/agento11y", "git.branch": "sqlite"},
		Input:  []agento11y.Message{textMsg(agento11y.RoleUser, "Find the blocked\ngoroutine")},
		Output: []agento11y.Message{textMsg(agento11y.RoleAssistant, "I will inspect the stack.")},
	}, first.Format(time.RFC3339Nano))
	writeGen(t, storage, "conv-A", "g2", agento11y.Generation{
		ConversationTitle: "Concurrency issue",
		AgentName:         "pi/reviewer",
		Model:             agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
		StartedAt:         second,
		CompletedAt:       second.Add(-time.Second),
		Usage: agento11y.TokenUsage{
			InputTokens: 80, OutputTokens: 30, CacheReadInputTokens: 15, ReasoningTokens: 5,
		},
		Tags:   map[string]string{"cwd": "/work/agento11y", "git.branch": "sqlite"},
		Output: []agento11y.Message{textMsg(agento11y.RoleAssistant, "The deadline expires while it waits.")},
	}, second.Format(time.RFC3339Nano))
	writeGen(t, storage, "conv-B", "g3", agento11y.Generation{
		ConversationTitle: "Unrelated stderr output",
		AgentName:         "pi",
		Model:             agento11y.ModelRef{Provider: "anthropic", Name: "claude-opus-4-7"},
		StartedAt:         second.Add(time.Hour),
		CompletedAt:       second.Add(time.Hour + time.Second),
		Usage:             agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Output:            []agento11y.Message{textMsg(agento11y.RoleAssistant, "stderr has no matching prefix token")},
	}, second.Add(time.Hour).Format(time.RFC3339Nano))

	want := captureReadPaths(t, storage)
	require.NotEmpty(t, want.Search[0].Snippet)
	assert.Positive(t, want.Search[0].MatchCount)
	store, err := openSQLStore(storage.Dir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	storage.sql = store

	assertReadPathsEqual(t, want, captureReadPaths(t, storage))

	migrator := newStoreMigrator(storage)
	migrator.sleep = noMigrationSleep
	_, err = migrator.run(context.Background())
	require.NoError(t, err)
	ready, err := storage.sqliteReadsReady()
	require.NoError(t, err)
	require.True(t, ready)

	assertReadPathsEqual(t, want, captureReadPaths(t, storage))
	_, err = storage.ConversationDetail("../conv-A")
	require.Error(t, err, "SQL reads check the id like JSONL reads")
	infix, err := storage.SearchConversations("err", 0)
	require.NoError(t, err)
	assert.Empty(t, infix, "unicode61 prefix search must not match err inside stderr")
	for _, query := range []string{`"`, "!!!", "a:b", "*", "-"} {
		_, err := storage.SearchConversations(query, 0)
		require.NoError(t, err, "search query %q", query)
	}
}

func TestSQLiteFilteredQueriesMatchRetiredJSONL(t *testing.T) {
	storage := newStorage(t)
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := lower.Add(time.Hour)
	workspace, blank, tool, skill := "/repo", "", "bash", "workflow:review"
	for _, seed := range []struct {
		conv, id, workspace, agent, model string
		at, completed                     time.Time
	}{
		{"match", "match-old", workspace, "pi", "old", lower.Add(-time.Hour), lower.Add(-time.Hour)},
		{"match", "match-now", "", "pi/reviewer", "new", lower, lower.Add(-time.Second)},
		{"other", "other", "/other", "pi", "other", lower.Add(time.Minute), lower.Add(2 * time.Minute)},
		{"unknown", "unknown", "", "pi", "unknown", lower.Add(2 * time.Minute), lower.Add(3 * time.Minute)},
		{"newest", "newest", workspace, "pi", "new", upper, upper.Add(time.Minute)},
		{"span", "span", workspace, "pi", "old", lower.Add(-time.Hour), lower.Add(-time.Hour)},
	} {
		writeGen(t, storage, seed.conv, seed.id, agento11y.Generation{
			StartedAt: seed.at, CompletedAt: seed.completed, AgentName: seed.agent,
			Model: agento11y.ModelRef{Provider: "anthropic", Name: seed.model},
			Usage: agento11y.TokenUsage{InputTokens: 10, OutputTokens: 3},
			Tags:  map[string]string{"cwd": seed.workspace},
			Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
				{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: seed.id, Name: "Bash", InputJSON: json.RawMessage(`{"file_path":"/repo/main.go"}`)}},
				{Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: seed.id, Name: "Bash", IsError: true}},
				{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: seed.id + "-read", Name: "Read", InputJSON: json.RawMessage(`{"file_path":"/repo/main.go"}`)}},
			}}},
		}, seed.at.Format(time.RFC3339Nano))
	}
	call := agento11y.Generation{
		StartedAt: lower.Add(3 * time.Minute), Tags: map[string]string{"cwd": workspace},
		Usage: agento11y.TokenUsage{InputTokens: 1},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{{
			Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: "reexport-call", Name: "Bash"},
		}}}},
	}
	writeGen(t, storage, "reexport", "reexport-call-gen", call, lower.Add(3*time.Minute).Format(time.RFC3339Nano))
	writeGen(t, storage, "reexport", "reexport-result-gen", agento11y.Generation{
		StartedAt: lower.Add(4 * time.Minute), Usage: agento11y.TokenUsage{InputTokens: 1},
		Input: []agento11y.Message{{Role: agento11y.RoleTool, Parts: []agento11y.Part{{
			Kind: agento11y.PartKindToolResult, ToolResult: &agento11y.ToolResult{ToolCallID: "reexport-call", Name: "Bash", IsError: true},
		}}}},
	}, lower.Add(4*time.Minute).Format(time.RFC3339Nano))
	writeGen(t, storage, "reexport", "reexport-call-gen", call, lower.Add(5*time.Minute).Format(time.RFC3339Nano))
	writeGen(t, storage, "zero", "zero", agento11y.Generation{
		StartedAt: lower.Add(90 * time.Second), Tags: map[string]string{"cwd": workspace},
		Model: agento11y.ModelRef{Provider: "anthropic", Name: "new"},
	}, lower.Add(90*time.Second).Format(time.RFC3339Nano))
	// An input that is not an array fails the storedGeneration decode, so only
	// this record's summaryGenerationCore fields are read.
	scalarAt := lower.Add(150 * time.Second)
	scalar, err := json.Marshal(generationRecord{
		ReceivedAt: scalarAt.Format(time.RFC3339Nano), GenerationID: "scalar", ConversationID: "scalar",
		Generation: json.RawMessage(`{"id":"scalar","conversation_id":"scalar","started_at":"` + scalarAt.Format(time.RFC3339Nano) +
			`","model":{"provider":"anthropic","name":"old"},"usage":{"input_tokens":2},"tags":{"cwd":"/repo"},"input":"plain text"}`),
	})
	require.NoError(t, err)
	appendRaw(t, filepath.Join(storage.Dir(), ConversationsDir, "scalar.jsonl"), string(scalar)+"\n")
	setConversationModTime(t, storage, "scalar", scalarAt)
	span := analyticsSpan("span", "trace", "span", "span-only", "Bash", lower.Add(30*time.Minute), time.Second, false)
	span.SkillName = skill
	_, err = storage.appendToolSpans([]toolSpanRecord{span})
	require.NoError(t, err)

	tests := []struct {
		name string
		opts ConversationListOptions
	}{
		{"all", ConversationListOptions{}},
		{"period", ConversationListOptions{Since: lower, Before: upper}},
		{"since with an offset", ConversationListOptions{Since: lower.Add(30 * time.Minute).In(time.FixedZone("+02:00", 2*60*60))}},
		{"workspace", ConversationListOptions{Since: lower, Before: upper, Workspace: &workspace}},
		{"unknown workspace", ConversationListOptions{Workspace: &blank}},
		{"before only", ConversationListOptions{Before: upper}},
		{"tool before limit", ConversationListOptions{Limit: 1, Since: lower, Before: upper, Workspace: &workspace, Tool: &tool}},
		{"skill observation", ConversationListOptions{Since: lower, Before: upper, Skill: &skill}},
		{"blank tool", ConversationListOptions{Tool: &blank}},
		{"blank skill", ConversationListOptions{Skill: &blank}},
		{"agent facet", ConversationListOptions{Limit: 1, Since: lower, Before: upper, Agent: "pi", MinSubagents: 1}},
		{"model facet", ConversationListOptions{Since: lower, Before: upper, Model: "old"}},
		{"token order", ConversationListOptions{Limit: 1, Order: "tokens"}},
	}
	capture := func(t *testing.T, opts ConversationListOptions) []byte {
		t.Helper()
		listOpts := opts
		listOpts.Exact = true
		list, total, err := storage.ListConversations(listOpts)
		require.NoError(t, err)
		plainList, _, err := storage.ListConversations(opts)
		require.NoError(t, err)
		metrics, matched, aggregate, err := storage.ConversationMetrics(opts)
		require.NoError(t, err)
		tools, err := storage.ToolUsage(opts)
		require.NoError(t, err)
		points, interval, err := storage.TokenUsagePoints(TokenUsageOptions{Since: opts.Since, Before: opts.Before, Workspace: opts.Workspace})
		require.NoError(t, err)
		analytics, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{Since: opts.Since, Before: opts.Before, Workspace: opts.Workspace})
		require.NoError(t, err)
		return mustJSON(t, []any{list, total, plainList, metrics, matched, aggregate, tools, points, interval, analytics})
	}
	want := make([][]byte, len(tests))
	for i, tt := range tests {
		want[i] = capture(t, tt.opts)
	}
	for _, seed := range []string{`"zero"`, `"scalar"`} {
		require.Contains(t, string(want[0]), seed, "the JSONL read must include the seed for the comparison to cover it")
	}
	store, err := openSQLStore(storage.Dir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	storage.sql = store
	runTestMigration(t, storage)
	require.NoError(t, os.RemoveAll(filepath.Join(storage.Dir(), ConversationsDir)))
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.JSONEq(t, string(want[i]), string(capture(t, tt.opts)))
		})
	}
}

func TestSQLiteListBatchesModelBuckets(t *testing.T) {
	storage, store := newMigrationTestStore(t)
	storage.sql = store
	require.NoError(t, store.setJSONLRetired())
	storage.setSQLiteOnly()
	recs := make([]generationRecord, sqlInBatchSize+1)
	for i := range recs {
		id := fmt.Sprintf("c%04d", i)
		recs[i] = generationRecord{
			ReceivedAt: "2026-08-20T08:00:00Z", GenerationID: id, ConversationID: id,
			Generation: json.RawMessage(fmt.Sprintf(`{"id":%q,"model":{"name":"m"},"usage":{"input_tokens":%d}}`, id, i+1)),
		}
	}
	require.NoError(t, store.writeGenerations(recs))

	list, _, err := storage.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Len(t, list, len(recs))
	for _, conv := range list {
		var i int
		_, err := fmt.Sscanf(conv.ID, "c%d", &i)
		require.NoError(t, err)
		assert.Equal(t, map[string]TokenBuckets{"m": {FreshInput: int64(i + 1)}}, conv.TokenBucketsByModel, conv.ID)
	}
}

func TestSQLiteListDoesNotReadRaw(t *testing.T) {
	storage := newStorage(t)
	when := mustParse(t, "2026-08-20T08:00:00Z")
	writeGen(t, storage, "conv", "gen", agento11y.Generation{
		StartedAt: when, Usage: agento11y.TokenUsage{InputTokens: 10},
		Model: agento11y.ModelRef{Name: "model"},
	}, when.Format(time.RFC3339Nano))
	want, _, err := storage.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	store, err := openSQLStore(storage.Dir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	storage.sql = store
	runTestMigration(t, storage)
	require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "conv").Update("raw", []byte("not-json")).Error)
	got, _, err := storage.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestSQLiteSearchReadsRawOnlyForLimitedHits(t *testing.T) {
	storage := newStorage(t)
	when := mustParse(t, "2026-08-20T08:00:00Z")
	writeGen(t, storage, "top", "g-top", agento11y.Generation{
		StartedAt: when,
		Output:    []agento11y.Message{textMsg(agento11y.RoleAssistant, "needle needle needle")},
	}, when.Format(time.RFC3339Nano))
	writeGen(t, storage, "lower", "g-lower", agento11y.Generation{
		StartedAt: when.Add(time.Minute),
		Output:    []agento11y.Message{textMsg(agento11y.RoleAssistant, "needle")},
	}, when.Add(time.Minute).Format(time.RFC3339Nano))
	store, err := openSQLStore(storage.Dir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	storage.sql = store
	runTestMigration(t, storage)
	require.NoError(t, store.db.Model(&sqlGeneration{}).Where("conv_id = ?", "lower").Update("raw", []byte("not-json")).Error)

	hits, err := storage.SearchConversations("needle", 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "top", hits[0].ID)
	assert.Equal(t, 3, hits[0].MatchCount)
	assert.NotEmpty(t, hits[0].Snippet)
}

func TestSQLiteSearchPropagatesRowErrors(t *testing.T) {
	for _, tt := range []struct {
		name            string
		rowsBeforeError int
	}{
		{"before any rows", 0},
		{"after one row", 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			storage := newStorage(t)
			when := mustParse(t, "2026-08-20T08:00:00Z")
			for _, id := range []string{"first", "second"} {
				writeGen(t, storage, "conv", id, agento11y.Generation{
					StartedAt: when,
					Output:    []agento11y.Message{textMsg(agento11y.RoleAssistant, "needle")},
				}, when.Format(time.RFC3339Nano))
			}
			store, err := openSQLStore(storage.Dir(), nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			storage.sql = store
			runTestMigration(t, storage)
			require.NoError(t, store.db.Model(&sqlGeneration{}).Where("1 = 1").Update("body", "invalid-json").Error)
			if tt.rowsBeforeError > 0 {
				require.NoError(t, store.db.Model(&sqlGeneration{}).Where("gen_id = ?", "first").Update("body", `"needle"`).Error)
			}

			const selection = "rowid, conv_id, json_extract(body, '$') AS body"
			rows, err := store.db.Model(&sqlGeneration{}).Select(selection).Where("conv_id = ?", "conv").Rows()
			require.NoError(t, err)
			var scanned int
			for rows.Next() {
				scanned++
			}
			require.NoError(t, rows.Close())
			require.ErrorContains(t, rows.Err(), "malformed JSON")
			require.Equal(t, tt.rowsBeforeError, scanned)

			require.NoError(t, store.db.Callback().Row().Before("gorm:row").Register("test:search_row_error", func(tx *gorm.DB) {
				if len(tx.Statement.Selects) == 1 && tx.Statement.Selects[0] == "rowid, conv_id, body" {
					tx.Statement.Selects[0] = selection
					// Sorting can evaluate the failing expression before streaming any rows.
					delete(tx.Statement.Clauses, "ORDER BY")
				}
			}))
			hits, err := storage.SearchConversations("needle", 0)
			require.ErrorContains(t, err, "malformed JSON")
			assert.Nil(t, hits)
		})
	}
}

func TestSQLiteNormalizesTimestampOffsets(t *testing.T) {
	storage := newStorage(t)
	earlierWithOffset := mustParse(t, "2026-08-20T12:30:00+02:00")
	laterUTC := mustParse(t, "2026-08-20T11:00:00Z")
	for _, seed := range []struct {
		conv string
		gen  string
		when time.Time
	}{
		{conv: "earlier", gen: "g-earlier", when: earlierWithOffset},
		{conv: "later", gen: "g-later", when: laterUTC},
	} {
		writeGen(t, storage, seed.conv, seed.gen, agento11y.Generation{
			StartedAt: seed.when,
			Usage:     agento11y.TokenUsage{InputTokens: 1},
		}, seed.when.Format(time.RFC3339Nano))
	}
	store, err := openSQLStore(storage.Dir(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	storage.sql = store
	runTestMigration(t, storage)

	since := mustParse(t, "2026-08-20T10:45:00Z")
	list, _, err := storage.ListConversations(ConversationListOptions{Since: since})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "later", list[0].ID)
	points, _, err := storage.TokenUsagePoints(TokenUsageOptions{Since: since, Interval: time.Minute})
	require.NoError(t, err)
	require.Len(t, points, 1)
	assert.Equal(t, laterUTC, points[0].Timestamp)

	var row sqlGeneration
	require.NoError(t, store.db.Where("conv_id = ?", "earlier").Take(&row).Error)
	assert.Equal(t, time.UTC, row.StartedAt.Location())
	assert.Equal(t, "2026-08-20T10:30:00Z", row.ReceivedAt)
}

func captureReadPaths(t *testing.T, storage *Storage) readPathSnapshot {
	t.Helper()
	list, total, err := storage.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	detail, err := storage.ConversationDetail("conv-A")
	require.NoError(t, err)
	points, interval, err := storage.TokenUsagePoints(TokenUsageOptions{Interval: time.Minute})
	require.NoError(t, err)
	search, err := storage.SearchConversations("goroutine deadline", 0)
	require.NoError(t, err)
	require.Len(t, search, 1, "terms may occur in different generations of one conversation")
	snapshot := readPathSnapshot{
		List: list, Total: total, Detail: detail, Points: points, Interval: interval, Search: search,
	}
	snapshot.ListJSON = mustJSON(t, struct {
		Conversations []ConversationSummary `json:"conversations"`
		Total         int                   `json:"total_conversations"`
	}{list, total})
	snapshot.DetailJSON = mustJSON(t, detail)
	snapshot.MetricsJSON = mustJSON(t, struct {
		Points     []TokenUsagePoint `json:"points"`
		IntervalMS int64             `json:"interval_ms"`
	}{points, interval.Milliseconds()})
	snapshot.SearchJSON = mustJSON(t, struct {
		Hits []SearchHit `json:"hits"`
	}{search})
	return snapshot
}

func assertReadPathsEqual(t *testing.T, want, got readPathSnapshot) {
	t.Helper()
	assert.Equal(t, want.List, got.List)
	assert.Equal(t, want.Total, got.Total)
	assert.Equal(t, want.Detail, got.Detail)
	assert.Equal(t, want.Points, got.Points)
	assert.Equal(t, want.Interval, got.Interval)
	assert.Equal(t, want.Search, got.Search)
	assert.JSONEq(t, string(want.ListJSON), string(got.ListJSON))
	assert.JSONEq(t, string(want.DetailJSON), string(got.DetailJSON))
	assert.JSONEq(t, string(want.MetricsJSON), string(got.MetricsJSON))
	assert.JSONEq(t, string(want.SearchJSON), string(got.SearchJSON))
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return data
}
