package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSkillsToolsMetricsReconcilesBeforeSelectingSkills(t *testing.T) {
	storage := newStorage(t)
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := lower.Add(time.Hour)
	writeToolGeneration(t, storage, "conv-repo", "g1", "/repo", lower.Add(-time.Hour), "same", "Bash", true)
	writeToolGeneration(t, storage, "conv-repo", "g2", "/repo", lower.Add(5*time.Minute), "same", "Bash", false)
	writeToolGeneration(t, storage, "conv-repo", "g3", "/repo", lower.Add(10*time.Minute), "unmarked", "org:Review", false)
	writeToolGeneration(t, storage, "conv-repo", "g4", "/repo", lower.Add(11*time.Minute), "blank", "Read", false)
	writeToolGeneration(t, storage, "conv-other", "g1", "/other", lower.Add(30*time.Minute), "other", "Read", false)

	_, err := storage.appendToolSpans([]toolSpanRecord{
		skillSpan("conv-repo", "1", "same", "Bash", " org:Review ", lower, time.Second, false),
		skillSpan("conv-repo", "2", "same", "Bash", "org:Review", lower.Add(5*time.Minute), 9*time.Second, true),
		skillSpan("conv-repo", "3", "same", "Bash", "org:Review", lower.Add(6*time.Minute), 20*time.Second, false),
		skillSpan("conv-repo", "4", "inverted", "Edit", "org:Review", lower.Add(7*time.Minute), -time.Second, false),
		{TraceID: "trace", SpanID: "5", ConversationID: "conv-repo", ToolName: "Edit", SkillName: "org:Review", StartedAt: lower.Add(8 * time.Minute)},
		skillSpan("conv-standalone", "6", "load", "Skill", "org:Review", lower.Add(40*time.Minute), 4*time.Second, false),
		skillSpan("conv-other", "7", "other", "Read", "org:Review", lower.Add(30*time.Minute), 0, false),
		skillSpan("conv-repo", "8", "case", "Read", "org:review", lower.Add(12*time.Minute), 2*time.Second, false),
		skillSpan("conv-repo", "9", "namespace", "Bash", "other:Review", lower.Add(13*time.Minute), 3*time.Second, false),
		skillSpan("conv-repo", "10", "blank", "Read", " \t\n", lower.Add(11*time.Minute), time.Second, false),
		skillSpan("conv-repo", "11", "upper", "Bash", "org:Review", upper, time.Second, true),
		skillSpan("conv-repo", "12", "before", "Bash", "org:Review", lower.Add(-time.Nanosecond), time.Second, true),
	})
	require.NoError(t, err)
	opts := ToolAnalyticsOptions{Since: lower, Before: upper, Interval: 5 * time.Minute}
	got, err := storage.SkillsToolsMetrics(opts)
	require.NoError(t, err)
	tools, err := storage.ToolAnalytics(opts)
	require.NoError(t, err)
	assert.Equal(t, tools, got.Tools)
	assert.Equal(t, ToolAnalyticsTotals{Calls: 11, Failures: 2, Tools: 5, Sessions: 2, DurationSamples: 8}, got.Tools.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 5, ProjectedSpans: 10, MatchedCalls: 4}, got.Tools.Coverage)
	assert.Equal(t, SkillAnalyticsTotals{Calls: 9, Failures: 2, Skills: 3, Sessions: 2, DurationSamples: 7}, got.Skills.Totals)
	assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 3, ProjectedSpans: 9, MatchedCalls: 3}, got.Skills.Coverage)
	assert.Equal(t, int64(300), got.Skills.IntervalSeconds)
	require.Len(t, got.Skills.Rows, 3)
	assert.Equal(t, []string{"org:Review", "org:review", "other:Review"}, toolRowNames(got.Skills.Rows))
	row := got.Skills.Rows[0]
	assert.Equal(t, 7, row.Calls)
	assert.Equal(t, 2, row.Failures)
	assert.Equal(t, 2, row.Sessions)
	assert.Equal(t, 5, row.DurationSamples)
	require.NotNil(t, row.P50DurationSeconds)
	require.NotNil(t, row.P95DurationSeconds)
	assert.Equal(t, 4.0, *row.P50DurationSeconds)
	assert.Equal(t, 20.0, *row.P95DurationSeconds)
	assert.Equal(t, 9, sumToolBucketCalls(got.Skills.Buckets))
	for _, bucket := range got.Skills.Buckets {
		assert.Contains(t, toolRowNames(got.Skills.Rows), bucket.Name)
		assert.False(t, bucket.Timestamp.Before(lower))
		assert.True(t, bucket.Timestamp.Before(upper))
	}
	assert.Equal(t, []ToolWorkspaceFacet{
		{Path: "/repo", Calls: 7, Sessions: 1},
		{Path: "/other", Calls: 1, Sessions: 1},
		{Path: "", Calls: 1},
	}, got.Skills.Workspaces)

	for _, tc := range []struct {
		workspace               string
		calls, sessions, skills int
	}{
		{workspace: "/repo", calls: 7, sessions: 1, skills: 3},
		{workspace: "/other", calls: 1, sessions: 1, skills: 1},
		{workspace: "", calls: 1, skills: 1},
		{workspace: "/missing"},
	} {
		t.Run("workspace="+tc.workspace, func(t *testing.T) {
			filteredOpts := opts
			filteredOpts.Workspace = &tc.workspace
			filtered, err := storage.SkillsToolsMetrics(filteredOpts)
			require.NoError(t, err)
			assert.Equal(t, tc.calls, filtered.Skills.Totals.Calls)
			assert.Equal(t, tc.sessions, filtered.Skills.Totals.Sessions)
			assert.Equal(t, tc.skills, filtered.Skills.Totals.Skills)
			assert.Equal(t, got.Skills.Workspaces, filtered.Skills.Workspaces)
		})
	}
}

func TestSkillAnalyticsIntervalUsesRenderedExtent(t *testing.T) {
	marked := mustParse(t, "2026-08-21T10:00:00Z")
	before := mustParse(t, "2026-09-21T10:00:00Z")
	srv, storage, _ := newTestServerStorage(t)
	_, err := storage.appendToolSpans([]toolSpanRecord{
		skillSpan("conv-old", "old", "old", "Read", "skill", marked, time.Second, true),
		skillSpan("conv-recent", "recent", "recent", "Read", "", before.Add(-time.Minute), time.Second, false),
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name                 string
		opts                 ToolAnalyticsOptions
		skillIntervalSeconds int64
		toolIntervalSeconds  int64
	}{
		{name: "old marked recent unmarked All", opts: ToolAnalyticsOptions{Before: before}, skillIntervalSeconds: 7200, toolIntervalSeconds: 7200},
		{name: "both bounds extend beyond marked data", opts: ToolAnalyticsOptions{Since: marked.Add(-31 * 24 * time.Hour), Before: before}, skillIntervalSeconds: 14400, toolIntervalSeconds: 7200},
		{name: "since only extends before marked data", opts: ToolAnalyticsOptions{Since: marked.Add(-31 * 24 * time.Hour)}, skillIntervalSeconds: 7200, toolIntervalSeconds: 7200},
		{name: "no bounds uses marked extent only", skillIntervalSeconds: 10, toolIntervalSeconds: 7200},
		{name: "short bounds exclude recent unmarked", opts: ToolAnalyticsOptions{Since: marked.Add(-time.Minute), Before: marked.Add(time.Minute)}, skillIntervalSeconds: 10, toolIntervalSeconds: 10},
		{name: "explicit fine interval", opts: ToolAnalyticsOptions{Before: before, Interval: 10 * time.Second}, skillIntervalSeconds: 10, toolIntervalSeconds: 10},
		{name: "explicit coarse interval and partial bucket", opts: ToolAnalyticsOptions{Since: marked.Add(-time.Minute), Before: marked.Add(time.Minute), Interval: 4 * time.Hour}, skillIntervalSeconds: 14400, toolIntervalSeconds: 14400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := storage.SkillsToolsMetrics(tc.opts)
			require.NoError(t, err)
			params := url.Values{}
			if !tc.opts.Since.IsZero() {
				params.Set("since", tc.opts.Since.Format(time.RFC3339Nano))
			}
			if !tc.opts.Before.IsZero() {
				params.Set("before", tc.opts.Before.Format(time.RFC3339Nano))
			}
			if tc.opts.Interval > 0 {
				params.Set("interval", fmt.Sprint(int64(tc.opts.Interval/time.Second)))
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/api/v1/metrics/skills-tools?"+params.Encode(), nil))
			require.Equal(t, http.StatusOK, rr.Code)
			var served SkillsToolsMetricsResponse
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &served))
			assert.Equal(t, got, served)
			assert.Equal(t, tc.skillIntervalSeconds, got.Skills.IntervalSeconds)
			assert.Equal(t, tc.toolIntervalSeconds, got.Tools.IntervalSeconds, "Tools retains observation-based automatic intervals")
			assert.Equal(t, SkillAnalyticsTotals{Calls: 1, Failures: 1, Skills: 1, DurationSamples: 1}, got.Skills.Totals)
			assert.Equal(t, ToolAnalyticsCoverage{ProjectedSpans: 1}, got.Skills.Coverage)
			require.Len(t, got.Skills.Buckets, 1, "empty chart columns remain implicit")
			assert.Equal(t, ToolAnalyticsBucket{
				Timestamp: intervalStart(marked, time.Duration(tc.skillIntervalSeconds)*time.Second),
				Name:      "skill", Calls: 1, Failures: 1,
			}, got.Skills.Buckets[0])
			if tc.opts.Interval <= 0 && !tc.opts.Before.IsZero() {
				interval := time.Duration(got.Skills.IntervalSeconds) * time.Second
				first := got.Skills.Buckets[0].Timestamp
				if !tc.opts.Since.IsZero() {
					first = intervalStart(tc.opts.Since, interval)
				}
				columns := (tc.opts.Before.Sub(first) + interval - 1) / interval
				assert.LessOrEqual(t, int(columns), maxTokenUsageBuckets+1)
			}
		})
	}
}

func TestSkillAnalyticsTimestampAndDurationEligibility(t *testing.T) {
	valid := mustParse(t, "2026-08-21T10:00:00Z")
	for _, tc := range []struct {
		name           string
		start, end     time.Time
		calls, samples int
	}{
		{name: "zero timestamp", end: valid},
		{name: "early timestamp", start: time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "late timestamp", start: time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC)},
		{name: "missing duration", start: valid, calls: 1},
		{name: "inverted duration", start: valid, end: valid.Add(-time.Second), calls: 1},
		{name: "zero duration", start: valid, end: valid, calls: 1, samples: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newStorage(t)
			_, err := storage.appendToolSpans([]toolSpanRecord{{
				TraceID: "trace", SpanID: "span", ConversationID: "standalone", ToolName: "Read", SkillName: "skill",
				StartedAt: tc.start, CompletedAt: tc.end,
			}})
			require.NoError(t, err)
			got, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{Interval: time.Hour})
			require.NoError(t, err)
			assert.Equal(t, tc.calls, got.Skills.Totals.Calls)
			assert.Equal(t, tc.samples, got.Skills.Totals.DurationSamples)
			assert.Zero(t, got.Skills.Totals.Sessions)
			assert.Equal(t, tc.calls, sumToolBucketCalls(got.Skills.Buckets))
			assert.Equal(t, ToolAnalyticsCoverage{ProjectedSpans: tc.calls}, got.Skills.Coverage)
			if tc.calls == 0 {
				assert.Empty(t, got.Skills.Rows)
				assert.Empty(t, got.Skills.Workspaces)
				return
			}
			require.Len(t, got.Skills.Rows, 1)
			assert.Equal(t, []ToolWorkspaceFacet{{Path: "", Calls: 1}}, got.Skills.Workspaces)
			if tc.samples == 0 {
				assert.Nil(t, got.Skills.Rows[0].P50DurationSeconds)
				assert.Nil(t, got.Skills.Rows[0].P95DurationSeconds)
			} else {
				require.NotNil(t, got.Skills.Rows[0].P50DurationSeconds)
				assert.Zero(t, *got.Skills.Rows[0].P50DurationSeconds)
			}
		})
	}
}

func TestSkillAnalyticsRedeliveryReplacesOrRemovesMarker(t *testing.T) {
	for _, marker := range []string{"other:Skill", "", " \t"} {
		for _, move := range []bool{false, true} {
			t.Run(fmt.Sprintf("marker=%q/move=%t", marker, move), func(t *testing.T) {
				storage := newStorage(t)
				at := mustParse(t, "2026-08-21T10:00:00Z")
				writeToolGeneration(t, storage, "conv-old", "g1", "/old", at, "call", "Read", false)
				writeToolGeneration(t, storage, "conv-new", "g1", "/new", at, "call", "Read", false)
				first := skillSpan("conv-old", "span", "call", "Read", "old:Skill", at, time.Second, false)
				_, err := storage.appendToolSpans([]toolSpanRecord{first})
				require.NoError(t, err)
				before, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{})
				require.NoError(t, err)
				require.Equal(t, 1, before.Skills.Totals.Calls)
				last := first
				last.SkillName = marker
				if move {
					last.ConversationID = "conv-new"
				}
				_, err = storage.appendToolSpans([]toolSpanRecord{first, last})
				require.NoError(t, err)
				restarted, err := NewStorage(storage.Dir())
				require.NoError(t, err)
				for _, reader := range []*Storage{storage, restarted} {
					got, err := reader.SkillsToolsMetrics(ToolAnalyticsOptions{})
					require.NoError(t, err)
					assert.Equal(t, ToolAnalyticsTotals{Calls: 2, Tools: 1, Sessions: 2, DurationSamples: 1}, got.Tools.Totals)
					if marker != "other:Skill" {
						assert.Zero(t, got.Skills.Totals.Calls)
						assert.Empty(t, got.Skills.Workspaces)
						continue
					}
					assert.Equal(t, SkillAnalyticsTotals{Calls: 1, Skills: 1, Sessions: 1, DurationSamples: 1}, got.Skills.Totals)
					require.Len(t, got.Skills.Rows, 1)
					assert.Equal(t, marker, got.Skills.Rows[0].Name)
					workspace := "/old"
					if move {
						workspace = "/new"
					}
					assert.Equal(t, []ToolWorkspaceFacet{{Path: workspace, Calls: 1, Sessions: 1}}, got.Skills.Workspaces)
				}
			})
		}
	}
}

func TestSkillsToolsMetricsIncludesMoreThanSessionPage(t *testing.T) {
	storage := newStorage(t)
	at := mustParse(t, "2026-08-21T10:00:00Z")
	spans := make([]toolSpanRecord, 205)
	for i := range spans {
		id := fmt.Sprintf("conv-%03d", i)
		writeToolGeneration(t, storage, id, "g1", "/repo", at.Add(-time.Hour), "call", "Read", false)
		spans[i] = skillSpan(id, id, "call", "Read", "skill", at, time.Second, false)
	}
	_, err := storage.appendToolSpans(spans)
	require.NoError(t, err)
	got, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{Since: at, Before: at.Add(time.Hour)})
	require.NoError(t, err)
	assert.Equal(t, SkillAnalyticsTotals{Calls: 205, Skills: 1, Sessions: 205, DurationSamples: 205}, got.Skills.Totals)
	assert.Equal(t, 205, got.Tools.Totals.Calls)
	assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 205, ProjectedSpans: 205, MatchedCalls: 205}, got.Skills.Coverage)
	require.Len(t, got.Skills.Rows, 1)
	assert.Equal(t, 205, got.Skills.Rows[0].Sessions)
	assert.Equal(t, []ToolWorkspaceFacet{{Path: "/repo", Calls: 205, Sessions: 205}}, got.Skills.Workspaces)
}

func TestSkillsToolsMetricsEmptyAndReadErrors(t *testing.T) {
	storage := newStorage(t)
	got, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{})
	require.NoError(t, err)
	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	var body map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &body))
	for _, section := range []string{"tools", "skills"} {
		for _, field := range []string{"rows", "buckets", "workspaces"} {
			assert.JSONEq(t, "[]", string(body[section][field]))
		}
	}
	assert.Positive(t, got.Tools.IntervalSeconds)
	assert.Positive(t, got.Skills.IntervalSeconds)
	assert.NotContains(t, string(body["skills"]["totals"]), `"tools"`)
	assert.Contains(t, string(body["skills"]["totals"]), `"skills":0`)
	for _, directory := range []string{ConversationsDir, ToolSpansDir} {
		t.Run(directory, func(t *testing.T) {
			broken := newStorage(t)
			path := filepath.Join(broken.Dir(), directory)
			require.NoError(t, os.RemoveAll(path))
			require.NoError(t, os.WriteFile(path, nil, 0o600))
			_, err := broken.SkillsToolsMetrics(ToolAnalyticsOptions{})
			assert.Error(t, err)
		})
	}
}

func TestSkillsToolsMetricsReadsObservationsOnce(t *testing.T) {
	storage := newStorage(t)
	at := mustParse(t, "2026-08-21T10:00:00Z")
	writeToolGeneration(t, storage, "conv", "g1", "/repo", at, "call", "Read", false)
	_, err := storage.appendToolSpans([]toolSpanRecord{skillSpan("conv", "span", "call", "Read", "skill", at, time.Second, false)})
	require.NoError(t, err)
	for _, directory := range []string{ConversationsDir, ToolSpansDir} {
		path := filepath.Join(storage.Dir(), directory, "conv.jsonl")
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = file.WriteString("not json\n")
		require.NoError(t, err)
		require.NoError(t, file.Close())
	}
	var logs bytes.Buffer
	storage.SetLogger(log.New(&logs, "", 0))
	got, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, got.Tools.Totals.Calls)
	assert.Equal(t, 1, got.Skills.Totals.Calls)
	for _, source := range []string{"generation", "span"} {
		message := "skills-tools " + source + " metrics: skipped 1 unparseable lines"
		assert.Equal(t, 1, strings.Count(logs.String(), message), logs.String())
	}
}

func TestSkillAnalyticsUsesReconciledTimestamp(t *testing.T) {
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := lower.Add(time.Hour)
	for _, tc := range []struct {
		name             string
		generation, span time.Time
		calls            int
	}{
		{name: "missing start uses generation without duration", generation: lower, calls: 1},
		{name: "span moves call into range", generation: lower.Add(-time.Hour), span: lower, calls: 1},
		{name: "span moves call to excluded upper bound", generation: lower, span: upper},
		{name: "invalid span start does not fall back", generation: lower, span: time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newStorage(t)
			writeToolGeneration(t, storage, "conv", "g1", "/repo", tc.generation, "call", "Read", false)
			_, err := storage.appendToolSpans([]toolSpanRecord{{
				TraceID: "trace", SpanID: "span", ConversationID: "conv", ToolCallID: "call", ToolName: "Read", SkillName: "skill", StartedAt: tc.span,
			}})
			require.NoError(t, err)
			got, err := storage.SkillsToolsMetrics(ToolAnalyticsOptions{Since: lower, Before: upper})
			require.NoError(t, err)
			assert.Equal(t, SkillAnalyticsTotals{Calls: tc.calls, Skills: tc.calls, Sessions: tc.calls}, got.Skills.Totals)
			assert.Equal(t, tc.calls, sumToolBucketCalls(got.Skills.Buckets))
			if tc.calls == 1 {
				require.Len(t, got.Skills.Rows, 1)
				assert.Nil(t, got.Skills.Rows[0].P50DurationSeconds)
				assert.Equal(t, []ToolWorkspaceFacet{{Path: "/repo", Calls: 1, Sessions: 1}}, got.Skills.Workspaces)
			} else {
				assert.Empty(t, got.Skills.Workspaces)
			}
		})
	}
}

func skillSpan(conversationID, spanID, callID, toolName, skillName string, started time.Time, duration time.Duration, failed bool) toolSpanRecord {
	span := analyticsSpan(conversationID, "trace", spanID, callID, toolName, started, duration, failed)
	span.SkillName = skillName
	return span
}
