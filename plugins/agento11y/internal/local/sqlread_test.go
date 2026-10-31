package local

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Merely opening SQLite does not flip reads before migration completes.
	assertReadPathsEqual(t, want, captureReadPaths(t, storage))

	migrator := newStoreMigrator(storage)
	migrator.sleep = noMigrationSleep
	_, err = migrator.run(context.Background())
	require.NoError(t, err)
	ready, err := storage.sqliteReadsReady()
	require.NoError(t, err)
	require.True(t, ready)

	assertReadPathsEqual(t, want, captureReadPaths(t, storage))
	infix, err := storage.SearchConversations("err", 0)
	require.NoError(t, err)
	assert.Empty(t, infix, "unicode61 prefix search must not match err inside stderr")
	for _, query := range []string{`"`, "!!!", "a:b", "*", "-"} {
		_, err := storage.SearchConversations(query, 0)
		require.NoError(t, err, "search query %q", query)
	}
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
