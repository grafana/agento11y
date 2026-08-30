package local

import (
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolAnalyticsReconcilesCallsSpansAndCoverage(t *testing.T) {
	storage := newStorage(t)
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := mustParse(t, "2026-08-21T11:00:00Z")

	writeToolGeneration(t, storage, "conv-repo", "g1", "/repo", lower, "same", "Bash", true)
	writeToolGeneration(t, storage, "conv-repo", "g2", "/repo", lower.Add(5*time.Minute), "same", "Bash", false)
	writeToolGeneration(t, storage, "conv-repo", "g3", "/repo", lower.Add(10*time.Minute), "read", "Read", true)
	writeToolGeneration(t, storage, "conv-other", "g1", "/other", lower.Add(30*time.Minute), "other", "Bash", false)
	writeToolGeneration(t, storage, "conv-other", "upper", "/other", upper, "excluded", "Bash", true)

	_, err := storage.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-repo", "trace-1", "span-1", "same", "Bash", lower.Add(time.Second), time.Second, false),
		analyticsSpan("conv-repo", "trace-2", "span-2", "same", "Bash", lower.Add(5*time.Minute+time.Second), 9*time.Second, true),
		// One generation call cannot absorb two distinct spans with the reused
		// call ID. This third span remains its own observation.
		analyticsSpan("conv-repo", "trace-3", "span-3", "same", "Bash", lower.Add(6*time.Minute), 20*time.Second, false),
		{TraceID: "trace-4", SpanID: "span-4", ConversationID: "conv-repo", ToolName: "Edit", StartedAt: lower.Add(7 * time.Minute), CompletedAt: lower.Add(7*time.Minute - time.Second)},
		{TraceID: "trace-5", SpanID: "span-5", ConversationID: "conv-repo", ToolName: "Edit", StartedAt: lower.Add(8 * time.Minute)},
		analyticsSpan("conv-standalone", "trace-6", "span-6", "standalone", "Task", lower.Add(40*time.Minute), 4*time.Second, false),
	})
	require.NoError(t, err)

	got, err := storage.ToolAnalytics(ToolAnalyticsOptions{Since: lower, Before: upper, Interval: 5 * time.Minute})
	require.NoError(t, err)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 8, Failures: 3, Tools: 4, Sessions: 2, DurationSamples: 4}, got.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 4, ProjectedSpans: 6, MatchedCalls: 2}, got.Coverage)
	assert.Equal(t, int64(300), got.IntervalSeconds)
	require.Len(t, got.Rows, 4)
	assert.Equal(t, []string{"Bash", "Edit", "Read", "Task"}, toolRowNames(got.Rows))

	bash := got.Rows[0]
	assert.Equal(t, 4, bash.Calls)
	assert.Equal(t, 2, bash.Failures, "generation and span failures are ORed without double counting")
	assert.Equal(t, 2, bash.Sessions)
	assert.Equal(t, 3, bash.DurationSamples)
	require.NotNil(t, bash.P50DurationSeconds)
	require.NotNil(t, bash.P95DurationSeconds)
	assert.Equal(t, 9.0, *bash.P50DurationSeconds, "nearest-rank p50")
	assert.Equal(t, 20.0, *bash.P95DurationSeconds, "nearest-rank p95")

	edit := got.Rows[1]
	assert.Equal(t, 2, edit.Calls)
	assert.Zero(t, edit.DurationSamples, "negative and incomplete spans are missing, not zero")
	assert.Nil(t, edit.P50DurationSeconds)
	assert.Nil(t, edit.P95DurationSeconds)
	assert.Equal(t, 1, got.Rows[2].Failures, "a historical generation result survives without a span")
	assert.Equal(t, 0, got.Rows[3].Sessions, "a standalone span does not fabricate a session")
	assert.Equal(t, 1, got.Rows[3].DurationSamples, "a standalone span still contributes coverage")

	assert.Equal(t, []ToolWorkspaceFacet{
		{Path: "/repo", Calls: 6, Sessions: 1},
		{Path: "/other", Calls: 1, Sessions: 1},
		{Path: "", Calls: 1, Sessions: 0},
	}, got.Workspaces)
	assert.Equal(t, 8, sumToolBucketCalls(got.Buckets))
	assert.NotContains(t, toolBucketTimes(got.Buckets), upper, "the upper bound is exclusive")

	workspace := "/repo"
	filtered, err := storage.ToolAnalytics(ToolAnalyticsOptions{
		Since: lower, Before: upper, Workspace: &workspace, Interval: 5 * time.Minute,
	})
	require.NoError(t, err)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 6, Failures: 3, Tools: 3, Sessions: 1, DurationSamples: 3}, filtered.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 3, ProjectedSpans: 5, MatchedCalls: 2}, filtered.Coverage)
	assert.Equal(t, got.Workspaces, filtered.Workspaces, "facets retain all period workspaces for a one-request picker")
}

func TestToolAnalyticsFoldsToolNamesByCase(t *testing.T) {
	storage := newStorage(t)
	started := mustParse(t, "2026-08-21T10:00:00Z")
	writeToolGeneration(t, storage, "conv-lower", "g1", "/repo", started, "lower-1", "bash", false)
	writeToolGeneration(t, storage, "conv-lower", "g2", "/repo", started.Add(time.Minute), "lower-2", "bash", false)
	writeToolGeneration(t, storage, "conv-lower", "g3", "/repo", started.Add(2*time.Minute), "lower-3", "bash", false)
	writeToolGeneration(t, storage, "conv-upper", "g1", "/repo", started.Add(3*time.Minute), "upper", "Bash", false)
	_, err := storage.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-upper", "trace", "span", "upper", "Bash", started.Add(3*time.Minute), 2*time.Second, false),
	})
	require.NoError(t, err)

	got, err := storage.ToolAnalytics(ToolAnalyticsOptions{Interval: 5 * time.Minute})
	require.NoError(t, err)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 4, Tools: 1, Sessions: 2, DurationSamples: 1}, got.Totals)
	require.Len(t, got.Rows, 1)
	assert.Equal(t, "bash", got.Rows[0].Name, "the spelling with the most calls is displayed")
	assert.Equal(t, 4, got.Rows[0].Calls)
	assert.Equal(t, 1, got.Rows[0].DurationSamples)
	require.NotNil(t, got.Rows[0].P50DurationSeconds)
	require.NotNil(t, got.Rows[0].P95DurationSeconds)
	assert.Equal(t, 2.0, *got.Rows[0].P50DurationSeconds)
	assert.Equal(t, 2.0, *got.Rows[0].P95DurationSeconds)
	require.Len(t, got.Buckets, 1)
	assert.Equal(t, "bash", got.Buckets[0].Name)
	assert.Equal(t, 4, got.Buckets[0].Calls)
	assert.Equal(t, "Bash", preferredToolSpelling(map[string]int{"bash": 1, "Bash": 1}), "ties are lexical")
}

func TestToolAnalyticsDropsUnixNanoUnrepresentableTimestampsEverywhere(t *testing.T) {
	storage := newStorage(t)
	valid := mustParse(t, "2026-08-21T10:00:00Z")
	_, err := storage.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-valid", "trace-valid", "span-valid", "valid", "Read", valid, time.Second, false),
		analyticsSpan("conv-early", "trace-early", "span-early", "early", "Bash", time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), time.Second, true),
		analyticsSpan("conv-late", "trace-late", "span-late", "late", "Edit", time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC), time.Second, true),
	})
	require.NoError(t, err)

	got, err := storage.ToolAnalytics(ToolAnalyticsOptions{Interval: time.Hour})
	require.NoError(t, err)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 1, Tools: 1, DurationSamples: 1}, got.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{ProjectedSpans: 1}, got.Coverage)
	assert.Equal(t, []string{"Read"}, toolRowNames(got.Rows))
	assert.Equal(t, []ToolWorkspaceFacet{{Path: "", Calls: 1}}, got.Workspaces)
	require.Len(t, got.Buckets, 1)
	assert.Equal(t, valid.Truncate(time.Hour), got.Buckets[0].Timestamp)
	assert.Equal(t, 1, got.Buckets[0].Calls)
}

func TestReconcileToolSourceMatchesOnlyNonblankCallIDs(t *testing.T) {
	source := &conversationToolSource{
		id: "conv-anonymous",
		summary: &fileSummary{toolOccurrences: []toolOccurrence{{
			Timestamp: mustParse(t, "2026-08-21T10:00:00Z"), Name: "Bash",
		}}},
		spans: []toolSpanRecord{{
			TraceID: "trace", SpanID: "span", ConversationID: "conv-anonymous", ToolName: "Bash",
			StartedAt: mustParse(t, "2026-08-21T10:00:01Z"), CompletedAt: mustParse(t, "2026-08-21T10:00:02Z"),
		}},
	}
	got := reconcileToolSource(source)
	require.Len(t, got, 2)
	assert.True(t, got[0].HasGeneration)
	assert.False(t, got[0].HasSpan)
	assert.False(t, got[1].HasGeneration)
	assert.True(t, got[1].HasSpan)
}

func TestToolAnalyticsUsesLastDuplicateSpanDelivery(t *testing.T) {
	storage := newStorage(t)
	started := mustParse(t, "2026-08-21T10:00:00Z")
	first := analyticsSpan("conv-only", "trace", "span", "call", "Read", started, time.Second, false)
	last := first
	last.ToolName = "Bash"
	last.Failed = true
	last.CompletedAt = started.Add(12 * time.Second)
	_, err := storage.appendToolSpans([]toolSpanRecord{first, last})
	require.NoError(t, err)

	got, err := storage.ToolAnalytics(ToolAnalyticsOptions{})
	require.NoError(t, err)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 1, Failures: 1, Tools: 1, DurationSamples: 1}, got.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{ProjectedSpans: 1}, got.Coverage)
	require.Len(t, got.Rows, 1)
	assert.Equal(t, "Bash", got.Rows[0].Name)
	require.NotNil(t, got.Rows[0].P50DurationSeconds)
	assert.Equal(t, 12.0, *got.Rows[0].P50DurationSeconds)
}

func TestListConversationsAppliesExactFiltersBeforeLimit(t *testing.T) {
	storage := newStorage(t)
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := mustParse(t, "2026-08-21T11:00:00Z")
	writeToolGeneration(t, storage, "conv-match", "g1", "/repo", lower, "target", "Bash", false)
	writeToolGeneration(t, storage, "conv-case", "g1", "/repo", lower.Add(10*time.Minute), "case", "bash", false)
	writeToolGeneration(t, storage, "conv-other", "g1", "/other", lower.Add(20*time.Minute), "other", "Bash", false)
	writeToolGeneration(t, storage, "conv-newest", "g1", "/repo", upper, "newest", "Bash", false)
	// This readable session has no generation-side Bash call in the range,
	// but its projected span is enough to make it an exact drilldown match.
	writeToolGeneration(t, storage, "conv-span", "g1", "/repo", lower.Add(-time.Hour), "old", "Read", false)
	_, err := storage.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-span", "trace-span", "span-span", "span-only", "Bash", lower.Add(30*time.Minute), time.Second, false),
		analyticsSpan("conv-standalone", "trace-only", "span-only", "only", "Bash", lower.Add(40*time.Minute), time.Second, false),
	})
	require.NoError(t, err)

	workspace := "/repo"
	tool := "Bash"
	rows, total, err := storage.ListConversations(ConversationListOptions{
		Limit: 2, Since: lower, Before: upper, Workspace: &workspace, Tool: &tool, Exact: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 5, total, "the existing store total contract is unchanged")
	assert.Equal(t, []string{"conv-case", "conv-match"}, conversationIDs(rows),
		"case-folded tool, workspace, and half-open bounds apply before the limit")

	rows, _, err = storage.ListConversations(ConversationListOptions{
		Limit: 1, Since: lower, Before: upper, Workspace: &workspace, Exact: true,
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-case"}, conversationIDs(rows),
		"the newest upper-bound row is removed before pagination")

	blankTool := ""
	rows, _, err = storage.ListConversations(ConversationListOptions{Tool: &blankTool, Exact: true})
	require.NoError(t, err)
	assert.Empty(t, rows)
}

func analyticsSpan(conversationID, traceID, spanID, callID, name string, started time.Time, duration time.Duration, failed bool) toolSpanRecord {
	return toolSpanRecord{
		TraceID: traceID, SpanID: spanID, ConversationID: conversationID, ToolCallID: callID, ToolName: name,
		StartedAt: started, CompletedAt: started.Add(duration), Failed: failed,
	}
}

func writeToolGeneration(t *testing.T, storage *Storage, conversationID, generationID, workspace string, at time.Time, callID, name string, failed bool) {
	t.Helper()
	parts := []agento11y.Part{{
		Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{ID: callID, Name: name},
	}}
	if failed {
		parts = append(parts, agento11y.Part{
			Kind:       agento11y.PartKindToolResult,
			ToolResult: &agento11y.ToolResult{ToolCallID: callID, Name: name, IsError: true},
		})
	}
	writeGen(t, storage, conversationID, generationID, agento11y.Generation{
		StartedAt: at, Tags: map[string]string{"cwd": workspace},
		Output: []agento11y.Message{{Role: agento11y.RoleAssistant, Parts: parts}},
	}, at.Format(time.RFC3339Nano))
}

func toolRowNames(rows []ToolAnalyticsRow) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].Name
	}
	return out
}

func sumToolBucketCalls(buckets []ToolAnalyticsBucket) int {
	total := 0
	for _, bucket := range buckets {
		total += bucket.Calls
	}
	return total
}

func toolBucketTimes(buckets []ToolAnalyticsBucket) []time.Time {
	out := make([]time.Time, len(buckets))
	for i := range buckets {
		out[i] = buckets[i].Timestamp
	}
	return out
}

func conversationIDs(rows []ConversationSummary) []string {
	out := make([]string, len(rows))
	for i := range rows {
		out[i] = rows[i].ID
	}
	return out
}
