package local

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/model"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

// httptest.NewRequest defaults to example.com, which the server refuses.
func newLocalRequest(method, target string, body io.Reader) *http.Request {
	req := httptest.NewRequest(method, target, body)
	req.Host = "127.0.0.1:8765"
	return req
}

func TestServer_GenerationsExport_RecordsAndAccepts(t *testing.T) {
	s, dir := newTestServer(t)
	body := `{"generations":[
		{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m1"}},
		{"id":"gen-2","conversation_id":"conv-A"}
	]}`
	resp := post(t, s, "/api/v1/generations:export", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out generationsResponse
	decodeJSON(t, resp.Body, &out)
	require.Len(t, out.Results, 2)
	assert.True(t, out.Results[0].Accepted)
	assert.True(t, out.Results[1].Accepted)
	assert.Equal(t, "gen-1", out.Results[0].GenerationID)

	// A runtime may send Fetch Metadata without an Origin. Ingest skips that
	// check so a runtime update cannot stop local capture.
	req := newLocalRequest(http.MethodPost, "/api/v1/generations:export", strings.NewReader(
		`{"generations":[{"id":"gen-3","conversation_id":"conv-A"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	// All generations belong to conv-A so they share one file.
	lines := readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl"))
	require.Len(t, lines, 3)
	var rec generationRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	assert.Equal(t, "gen-1", rec.GenerationID)
	assert.Equal(t, "conv-A", rec.ConversationID)
	assert.NotEmpty(t, rec.ReceivedAt)
	assert.JSONEq(t, `{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m1"}}`, string(rec.Generation))
	assert.Contains(t, lines[2], `"id":"gen-3"`)
}

// TestServer_GenerationsExport_StampsLastActivity covers the ordering key
// ingest writes: a conversation file's modification time is the newest
// activity the batch carried for it, and a later import of older turns
// leaves it where it was, so the conversation keeps its place among
// today's.
func TestServer_GenerationsExport_StampsLastActivity(t *testing.T) {
	s, dir := newTestServer(t)
	live := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	earlier := live.Add(-30 * time.Minute)
	backfill := live.Add(-90 * 24 * time.Hour)
	stamp := func() time.Time {
		t.Helper()
		info, err := os.Stat(filepath.Join(dir, ConversationsDir, "conv-A.jsonl"))
		require.NoError(t, err)
		return info.ModTime()
	}
	export := func(body string) {
		t.Helper()
		resp := post(t, s, "/api/v1/generations:export", "application/json", body)
		defer resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	// One request, two turns of the same conversation, out of order.
	export(fmt.Sprintf(`{"generations":[
		{"id":"gen-2","conversation_id":"conv-A","started_at":%q,"completed_at":%q},
		{"id":"gen-1","conversation_id":"conv-A","started_at":%q,"completed_at":%q}
	]}`,
		live.Add(-time.Minute).Format(time.RFC3339Nano), live.Format(time.RFC3339Nano),
		earlier.Add(-time.Minute).Format(time.RFC3339Nano), earlier.Format(time.RFC3339Nano)))
	assert.WithinDuration(t, live, stamp(), time.Second, "the newest activity in the batch")

	// A history import appending months-old turns to the same conversation.
	export(fmt.Sprintf(`{"generations":[
		{"id":"gen-0","conversation_id":"conv-A","started_at":%q,"completed_at":%q}
	]}`, backfill.Add(-time.Minute).Format(time.RFC3339Nano), backfill.Format(time.RFC3339Nano)))
	assert.WithinDuration(t, live, stamp(), time.Second, "a backfill must not sink a live conversation")

	// The list bounds on that stamp, so a conversation that started before
	// the range but finished inside it remains visible.
	since := live.Add(-30 * time.Second).Format(time.RFC3339Nano)
	req := newLocalRequest(http.MethodGet, "/api/v1/conversations?since="+url.QueryEscape(since), nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Contains(t, rr.Body.String(), `"id":"conv-A"`)
}

func TestServer_GenerationsExport_RejectsMissingAndUnsafeConversationID(t *testing.T) {
	s, dir := newTestServer(t)
	body := `{"generations":[
		{"id":"missing-conv"},
		{"id":"bad-path","conversation_id":"../runs"}
	]}`
	resp := post(t, s, "/api/v1/generations:export", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var out generationsResponse
	decodeJSON(t, resp.Body, &out)
	require.Len(t, out.Results, 2)
	for _, r := range out.Results {
		assert.False(t, r.Accepted)
		assert.NotEmpty(t, r.Error)
	}

	assertConversationDirEmpty(t, &Storage{dir: dir})
}

func TestServer_GenerationsExport_AppendsByConversation(t *testing.T) {
	s, dir := newTestServer(t)
	postDiscard(t, s, "/api/v1/generations:export", "application/json", `{"generations":[{"id":"gen-a","conversation_id":"conv-shared"}]}`)
	postDiscard(t, s, "/api/v1/generations:export", "application/json", `{"generations":[{"id":"gen-b","conversation_id":"conv-shared"}]}`)

	lines := readLines(t, filepath.Join(dir, ConversationsDir, "conv-shared.jsonl"))
	require.Len(t, lines, 2)
	var first, second generationRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	require.NoError(t, json.Unmarshal([]byte(lines[1]), &second))
	assert.Equal(t, "gen-a", first.GenerationID)
	assert.Equal(t, "gen-b", second.GenerationID)
}

// TestServer_GenerationsExport_BatchesPerConversation covers the batched
// ingest path: one request opens each conversation file once, per-record
// results stay in request order, a mid-batch write failure keeps the
// records written before it accepted, and a conversations directory removed
// under the running server (a cleanup script, a synced state directory) is
// recreated.
func TestServer_GenerationsExport_BatchesPerConversation(t *testing.T) {
	cases := []struct {
		name           string
		convIDs        []string // one generation per entry, in request order
		failWriteAfter int
		removeDir      bool
		wantAccepted   []bool
		wantOpens      int
		wantLines      map[string]int
	}{
		{
			name:         "five generations one conversation",
			convIDs:      []string{"conv-A", "conv-A", "conv-A", "conv-A", "conv-A"},
			wantAccepted: []bool{true, true, true, true, true},
			wantOpens:    1,
			wantLines:    map[string]int{"conv-A": 5},
		},
		{
			name:         "interleaved conversations open once each",
			convIDs:      []string{"conv-A", "conv-B", "conv-A", "conv-B", "conv-A"},
			wantAccepted: []bool{true, true, true, true, true},
			wantOpens:    2,
			wantLines:    map[string]int{"conv-A": 3, "conv-B": 2},
		},
		{
			name:           "third append fails",
			convIDs:        []string{"conv-A", "conv-A", "conv-A", "conv-A", "conv-A"},
			failWriteAfter: 2,
			wantAccepted:   []bool{true, true, false, false, false},
			wantOpens:      1,
			wantLines:      map[string]int{"conv-A": 2},
		},
		{
			name:         "missing conversations dir is recreated",
			convIDs:      []string{"conv-A", "conv-A"},
			removeDir:    true,
			wantAccepted: []bool{true, true},
			wantOpens:    1,
			wantLines:    map[string]int{"conv-A": 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, storage, dir := newTestServerStorage(t)
			opener := &countingOpener{failWriteAfter: tc.failWriteAfter}
			storage.openAppend = opener.open
			if tc.removeDir {
				require.NoError(t, os.RemoveAll(filepath.Join(dir, ConversationsDir)))
			}

			gens := make([]string, 0, len(tc.convIDs))
			for i, convID := range tc.convIDs {
				gens = append(gens, fmt.Sprintf(`{"id":"gen-%d","conversation_id":%q}`, i, convID))
			}
			resp := post(t, srv, "/api/v1/generations:export", "application/json",
				`{"generations":[`+strings.Join(gens, ",")+`]}`)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			var out generationsResponse
			decodeJSON(t, resp.Body, &out)
			require.Len(t, out.Results, len(tc.convIDs))
			for i, want := range tc.wantAccepted {
				assert.Equal(t, fmt.Sprintf("gen-%d", i), out.Results[i].GenerationID, "result %d out of request order", i)
				assert.Equal(t, want, out.Results[i].Accepted, "result %d accepted", i)
				if want {
					assert.Empty(t, out.Results[i].Error, "result %d error", i)
				} else {
					assert.NotEmpty(t, out.Results[i].Error, "result %d error", i)
				}
			}

			for convID, wantLines := range tc.wantLines {
				assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, convID+".jsonl")), wantLines, convID)
			}
			opens, closes := opener.counts()
			assert.Equal(t, tc.wantOpens, opens, "file opens")
			assert.Equal(t, opens, closes, "every open must be closed")
		})
	}
}

func TestServer_OTLPDrainsAndReturns200(t *testing.T) {
	s, dir := newTestServer(t)
	for _, tc := range []struct {
		name        string
		path        string
		contentType string
		body        []byte
	}{
		{name: "traces json", path: "/otlp/v1/traces", contentType: "application/json", body: []byte(`{"resourceSpans":[{"resource":{"attributes":[]}}]}`)},
		{name: "metrics protobuf", path: "/otlp/v1/metrics", contentType: "application/x-protobuf", body: []byte("\x00\x01\x02\x03binary-protobuf-body")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postBytes(t, s, tc.path, tc.contentType, tc.body)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d", resp.StatusCode)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "otlp-traces.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("otlp traces should not be persisted, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "otlp-metrics.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("otlp metrics should not be persisted, stat err = %v", err)
	}
}

// A daemon with no guards.toml has no rules to enforce, so every call is
// allowed and nothing about the call is written to the store.
func TestServer_HookEvaluate_AllowsWithEmptyRuleset(t *testing.T) {
	s, dir := newTestServer(t)
	body := `{"phase":"postflight","context":{"agent_name":"x"}}`

	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out agento11y.HookEvaluateResponse
	decodeJSON(t, resp.Body, &out)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)
	assert.NotNil(t, out.Evaluations)
	assert.Empty(t, out.Evaluations)

	_, err := os.Stat(filepath.Join(dir, "hooks.jsonl"))
	assert.True(t, os.IsNotExist(err), "hooks should not be persisted")
}

func TestServer_HookEvaluate_InvalidJSONReturns400(t *testing.T) {
	s, dir := newTestServer(t)
	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", `{not valid json`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	_, err := os.Stat(filepath.Join(dir, "hooks.jsonl"))
	assert.True(t, os.IsNotExist(err), "hooks should not be persisted")
}

func TestServer_ConversationMetrics(t *testing.T) {
	srv, storage, _ := newTestServerStorage(t)
	for _, seed := range []struct {
		conv, when, workspace string
		tokens                int64
	}{
		{conv: "conv-repo", when: "2026-08-21T10:00:00Z", workspace: "/repo", tokens: 5},
		{conv: "conv-repo", when: "2026-08-21T11:00:00Z", workspace: "/repo", tokens: 50},
		{conv: "conv-unknown", when: "2026-08-21T10:30:00Z", tokens: 7},
	} {
		writeGen(t, storage, seed.conv, seed.when, agento11y.Generation{
			StartedAt: mustParse(t, seed.when),
			Usage:     agento11y.TokenUsage{InputTokens: seed.tokens},
			Tags:      map[string]string{"cwd": seed.workspace},
		}, seed.when)
	}

	t.Run("matched count precedes limit and before is exclusive", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet,
			"/api/v1/metrics/conversations?limit=1&since=2026-08-21T10:00:00Z&before=2026-08-21T11:00:00Z", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var got struct {
			Aggregate            ConversationMetricsAggregate `json:"aggregate"`
			Conversations        []ConversationSummary        `json:"conversations"`
			MatchedConversations int                          `json:"matched_conversations"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
		assert.Equal(t, 2, got.MatchedConversations)
		assert.Equal(t, 2, got.Aggregate.Calls)
		assert.Equal(t, 0, got.Aggregate.Agents)
		assert.Equal(t, 2, got.Aggregate.Workspaces)
		assert.Equal(t, TokenBuckets{FreshInput: 12}, got.Aggregate.TokenBuckets)
		require.Len(t, got.Conversations, 1)
		assert.Equal(t, "conv-unknown", got.Conversations[0].ID)
	})

	t.Run("present blank workspace selects unknown", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet,
			"/api/v1/metrics/conversations?workspace=&since=2026-08-21T10:00:00Z&before=2026-08-21T11:00:00Z", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), `"id":"conv-unknown"`)
		assert.NotContains(t, rr.Body.String(), `"id":"conv-repo"`)
	})

	t.Run("invalid period and limit are rejected", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/metrics/conversations?limit=0",
			"/api/v1/metrics/conversations?since=",
			"/api/v1/metrics/conversations?before=tomorrow",
		} {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusBadRequest, rr.Code, path)
		}
	})
}

func TestServer_ToolMetrics(t *testing.T) {
	srv, storage, _ := newTestServerStorage(t)
	writeGen(t, storage, "conv-tools", "g1", agento11y.Generation{
		Tags: map[string]string{"cwd": "/repo"},
		Output: []agento11y.Message{{
			Role: agento11y.RoleAssistant,
			Parts: []agento11y.Part{{
				Kind:     agento11y.PartKindToolCall,
				ToolCall: &agento11y.ToolCall{ID: "call-1", Name: "Bash"},
			}},
		}},
	}, "2026-08-21T10:00:00Z")
	failedResult := agento11y.Generation{Input: []agento11y.Message{{
		Role: agento11y.RoleTool,
		Parts: []agento11y.Part{{
			Kind:       agento11y.PartKindToolResult,
			ToolResult: &agento11y.ToolResult{ToolCallID: "call-1", Name: "Bash", IsError: true},
		}},
	}}}
	writeGen(t, storage, "conv-tools", "g2", failedResult, "2026-08-21T10:01:00Z")
	writeGen(t, storage, "conv-tools", "g3", failedResult, "2026-08-21T10:02:00Z")

	req := newLocalRequest(http.MethodGet, "/api/v1/metrics/tools?limit=2000&since=2026-08-21T09:00:00Z&before=2026-08-21T10:02:00Z&workspace=%2Frepo", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var got struct {
		Conversations []ConversationToolUsage `json:"conversations"`
	}
	decodeJSON(t, io.NopCloser(rr.Body), &got)
	require.Len(t, got.Conversations, 1)
	assert.Equal(t, "conv-tools", got.Conversations[0].ID)
	assert.Equal(t, []ToolUsage{{Name: "Bash", Calls: 1, Failures: 1}}, got.Conversations[0].Tools)
}

func TestServer_SkillsToolsMetricsAndSessionDrilldown(t *testing.T) {
	srv, storage, _ := newTestServerStorage(t)
	lower := mustParse(t, "2026-08-21T10:00:00Z")
	upper := mustParse(t, "2026-08-21T11:00:00Z")
	writeToolGeneration(t, storage, "conv-match", "g1", "/repo", lower, "call-1", "Bash", true)
	writeToolGeneration(t, storage, "conv-new", "g1", "/repo", upper, "call-2", "Bash", false)
	_, err := storage.appendToolSpans([]toolSpanRecord{
		analyticsSpan("conv-match", "trace-1", "span-1", "call-1", "Bash", lower.Add(time.Second), 2*time.Second, false),
	})
	require.NoError(t, err)

	t.Run("tools-only exact response", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet,
			"/api/v1/metrics/skills-tools?since=2026-08-21T10:00:00Z&before=2026-08-21T11:00:00Z&workspace=%2Frepo&interval=300", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.NotContains(t, rr.Body.String(), `"skills"`)
		var got SkillsToolsMetricsResponse
		decodeJSON(t, io.NopCloser(rr.Body), &got)
		assert.Equal(t, ToolAnalyticsTotals{Calls: 1, Failures: 1, Tools: 1, Sessions: 1, DurationSamples: 1}, got.Tools.Totals)
		assert.Equal(t, ToolAnalyticsCoverage{GenerationCalls: 1, ProjectedSpans: 1, MatchedCalls: 1}, got.Tools.Coverage)
		assert.Equal(t, int64(300), got.Tools.IntervalSeconds)
		require.Len(t, got.Tools.Rows, 1)
		assert.Equal(t, "Bash", got.Tools.Rows[0].Name)
	})

	t.Run("sessions filters before limit", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet,
			"/api/v1/conversations?limit=1&tool=Bash&workspace=%2Frepo&since=2026-08-21T10:00:00Z&before=2026-08-21T11:00:00Z", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var got struct {
			Conversations []ConversationSummary `json:"conversations"`
		}
		decodeJSON(t, io.NopCloser(rr.Body), &got)
		require.Len(t, got.Conversations, 1)
		assert.Equal(t, "conv-match", got.Conversations[0].ID)
	})

	t.Run("invalid exact parameters are rejected", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/metrics/skills-tools?interval=0",
			"/api/v1/metrics/skills-tools?before=tomorrow",
			"/api/v1/conversations?tool=Bash&since=",
			"/api/v1/conversations?tool=Bash&before=tomorrow",
		} {
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, path, nil))
			assert.Equal(t, http.StatusBadRequest, rr.Code, path)
		}
	})
}

func assertFixedSecurityHeaders(t *testing.T, header http.Header) {
	t.Helper()
	assert.Equal(t, "nosniff", header.Get("X-Content-Type-Options"))
	assert.Equal(t, "no-referrer", header.Get("Referrer-Policy"))
	assert.Equal(t, "DENY", header.Get("X-Frame-Options"))
	assert.Equal(t, "same-origin", header.Get("Cross-Origin-Resource-Policy"))
}

// TestServer_Routing covers the small router-level status responses.
// The richer per-endpoint behaviour lives in the generations / OTLP /
// hook tests above.
func TestServer_Routing(t *testing.T) {
	s, dir := newTestServer(t)
	cases := []struct {
		name                string
		method              string
		path                string
		contentType         string
		body                string
		want                int
		wantContentType     string // prefix-matched; "" skips the check
		wantBodyHas         string // substring check; "" skips
		wantBodyNotHas      string // substring check; "" skips
		wantNoConversations bool
	}{
		{name: "root serves viewer HTML", method: http.MethodGet, path: "/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "conversation path serves viewer HTML", method: http.MethodGet, path: "/conversations/conv-123", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "settings path serves viewer HTML", method: http.MethodGet, path: "/settings", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "settings trailing slash serves viewer HTML", method: http.MethodGet, path: "/settings/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "analytics path serves viewer HTML", method: http.MethodGet, path: "/analytics", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "analytics trailing slash serves viewer HTML", method: http.MethodGet, path: "/analytics/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `src="/assets/app.js"`},
		{name: "CSS asset", method: http.MethodGet, path: "/assets/app.css", want: http.StatusOK, wantContentType: "text/css", wantBodyHas: ":root"},
		{name: "app bundle asset", method: http.MethodGet, path: "/assets/app.js", want: http.StatusOK, wantContentType: "application/javascript", wantBodyHas: "function App()"},
		{name: "healthz serves JSON", method: http.MethodGet, path: "/healthz", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"status":"ok"`},
		{name: "empty conversation metrics serves an array", method: http.MethodGet, path: "/api/v1/metrics/conversations", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"conversations":[]`},
		{name: "empty tool metrics serves an array", method: http.MethodGet, path: "/api/v1/metrics/tools", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"conversations":[]`},
		{name: "empty skills-tools metrics serves tools only", method: http.MethodGet, path: "/api/v1/metrics/skills-tools", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"tools":{"totals":{"calls":0`},
		{name: "unknown route", method: http.MethodPost, path: "/api/v1/unknown", contentType: wire.ContentTypeJSON, body: "{}", want: http.StatusNotFound},
		{name: "wrong method on generations export", method: http.MethodPut, path: "/api/v1/generations:export", contentType: wire.ContentTypeJSON, body: "{}", want: http.StatusMethodNotAllowed},
		{name: "hook evaluate serves JSON", method: http.MethodPost, path: "/api/v1/hooks:evaluate", contentType: wire.ContentTypeJSON, body: `{"phase":"postflight"}`, want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"action":"allow"`},
		{name: "wrong method on hook evaluate", method: http.MethodGet, path: "/api/v1/hooks:evaluate", want: http.StatusMethodNotAllowed},
		{name: "form type refused", method: http.MethodPost, path: "/api/v1/generations:export", contentType: "application/x-www-form-urlencoded", body: "generations=[]", want: http.StatusUnsupportedMediaType, wantNoConversations: true},
		{name: "text type refused", method: http.MethodPost, path: "/api/v1/hooks:evaluate", contentType: "text/plain", body: `{"phase":"postflight"}`, want: http.StatusUnsupportedMediaType, wantBodyNotHas: `"action"`},
		{name: "absent type refused", method: http.MethodPost, path: "/api/v1/history/runs/r1:cancel", want: http.StatusUnsupportedMediaType},
		{name: "charset parameter accepted", method: http.MethodPost, path: "/api/v1/hooks:evaluate", contentType: "application/json; charset=utf-8", body: `{"phase":"postflight"}`, want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"action":"allow"`},
		{name: "protobuf accepted on OTLP", method: http.MethodPost, path: otlpTracesPath, contentType: wire.ContentTypeProto, body: "protobuf", want: http.StatusOK},
		{name: "protobuf refused outside OTLP", method: http.MethodPost, path: "/api/v1/generations:export", contentType: wire.ContentTypeProto, body: "protobuf", want: http.StatusUnsupportedMediaType},
		{name: "GET is not gated", method: http.MethodGet, path: "/api/v1/conversations", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"conversations":[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newLocalRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			assertFixedSecurityHeaders(t, rr.Header())
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d", rr.Code, tc.want)
			}
			if tc.wantContentType != "" {
				if got := rr.Header().Get("Content-Type"); !strings.HasPrefix(got, tc.wantContentType) {
					t.Fatalf("Content-Type = %q, want prefix %q", got, tc.wantContentType)
				}
			}
			if tc.wantBodyHas != "" && !strings.Contains(rr.Body.String(), tc.wantBodyHas) {
				t.Fatalf("body missing %q\n--- body ---\n%s", tc.wantBodyHas, rr.Body.String())
			}
			if tc.wantBodyNotHas != "" && strings.Contains(rr.Body.String(), tc.wantBodyNotHas) {
				t.Fatalf("body contains %q\n--- body ---\n%s", tc.wantBodyNotHas, rr.Body.String())
			}
			if tc.wantNoConversations {
				entries, err := os.ReadDir(filepath.Join(dir, ConversationsDir))
				require.NoError(t, err)
				assert.Empty(t, entries)
			}
		})
	}
}

func TestServer_MediaTypeRefusalIsLogged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		wantLog string
	}{
		{
			name:    "request details",
			path:    "/api/v1/history:import",
			wantLog: "local: refused POST \"/api/v1/history:import\": Content-Type \"text/plain\"\n",
		},
		{
			name:    "escaped control character",
			path:    "/x%0aforged",
			wantLog: "local: refused POST \"/x\\nforged\": Content-Type \"text/plain\"\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			var logs strings.Builder
			s.logger.SetOutput(&logs)

			resp := post(t, s, tc.path, "text/plain", "{}")
			resp.Body.Close()
			require.Equal(t, http.StatusUnsupportedMediaType, resp.StatusCode)
			assert.Equal(t, tc.wantLog, logs.String())
		})
	}
}

func TestServer_DocumentCSPNonce(t *testing.T) {
	s, _ := newTestServer(t)

	fetch := func(path string) (string, string) {
		t.Helper()
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, newLocalRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, rr.Code)

		csp := rr.Header().Get("Content-Security-Policy")
		const prefix = "script-src 'self' 'nonce-"
		start := strings.Index(csp, prefix)
		require.NotEqual(t, -1, start, "CSP missing %q", prefix)
		remainder := csp[start+len(prefix):]
		end := strings.IndexByte(remainder, '\'')
		require.Greater(t, end, 0, "CSP has no closing quote after nonce")
		nonce := remainder[:end]
		assert.Equal(t, 1, strings.Count(rr.Body.String(), nonce))
		assert.Contains(t, rr.Body.String(), `nonce="`+nonce+`"`)
		assert.NotContains(t, rr.Body.String(), noncePlaceholder)
		return nonce, csp
	}

	firstNonce, _ := fetch("/")
	secondNonce, _ := fetch("/")
	assert.NotEqual(t, firstNonce, secondNonce)

	settingsNonce, settingsCSP := fetch("/settings")
	assert.Equal(t, "default-src 'self'; "+
		"script-src 'self' 'nonce-"+settingsNonce+"'; "+
		"style-src 'self'; "+
		"img-src 'self' data:; "+
		"font-src 'self'; "+
		"connect-src 'self' https://models.dev; "+
		"object-src 'none'; "+
		"frame-ancestors 'none'; "+
		"base-uri 'none'; "+
		"form-action 'none'", settingsCSP)
}

func TestServer_NonDocumentSecurityHeaders(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name         string
		path         string
		wantStatus   int
		wantRedirect bool
	}{
		{name: "JSON response", path: "/api/v1/conversations", wantStatus: http.StatusOK},
		{name: "mux redirect", path: "//settings", wantRedirect: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, newLocalRequest(http.MethodGet, tc.path, nil))

			if tc.wantRedirect {
				require.GreaterOrEqual(t, rr.Code, http.StatusMultipleChoices)
				require.Less(t, rr.Code, http.StatusBadRequest)
			} else {
				require.Equal(t, tc.wantStatus, rr.Code)
			}
			assertFixedSecurityHeaders(t, rr.Header())
			assert.Equal(t, "default-src 'none'; frame-ancestors 'none'; base-uri 'none'", rr.Header().Get("Content-Security-Policy"))
		})
	}
}

// TestServer_APIConversations exercises the read endpoints the viewer
// UI calls. The empty-list, seeded-list, limit, detail, and not-found
// paths share enough structure to belong in one table; richer
// aggregation semantics (token sums, dedup, etc.) live in query_test.
func TestServer_APIConversations(t *testing.T) {
	srv, dir := newTestServer(t)
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	writeGen(t, storage, "conv-A", "g1", agento11y.Generation{
		AgentName:   "pi",
		Model:       agento11y.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50},
	}, "2026-05-21T10:00:03Z")
	writeGen(t, storage, "conv-B", "g2", agento11y.Generation{
		AgentName:   "claude-code",
		Model:       agento11y.ModelRef{Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T11:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T11:00:01Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, "2026-05-21T11:00:01Z")

	cases := []struct {
		name          string
		method        string
		path          string
		want          int
		wantBodyHas   []string // all substrings must appear
		wantBodyLacks []string // none of these may appear
	}{
		{
			name:   "list returns both conversations newest first",
			method: http.MethodGet, path: "/api/v1/conversations",
			want:        http.StatusOK,
			wantBodyHas: []string{`"conversations"`, `"id":"conv-B"`, `"id":"conv-A"`, `"calls":1`, `"total_tokens":15`},
		},
		{
			name:   "list honours limit query param",
			method: http.MethodGet, path: "/api/v1/conversations?limit=1",
			want: http.StatusOK,
			// newest-first: only conv-B survives the cap.
			wantBodyHas:   []string{`"id":"conv-B"`},
			wantBodyLacks: []string{`"id":"conv-A"`},
		},
		{
			name:   "list honours since query param",
			method: http.MethodGet, path: "/api/v1/conversations?since=2026-05-21T10:30:00Z",
			want:          http.StatusOK,
			wantBodyHas:   []string{`"id":"conv-B"`},
			wantBodyLacks: []string{`"id":"conv-A"`},
		},
		{
			name:   "list rejects a non-RFC3339 since",
			method: http.MethodGet, path: "/api/v1/conversations?since=yesterday",
			want: http.StatusBadRequest,
		},
		{
			// The viewer tells an empty store from an empty range by this
			// count, so a range that holds nothing must still report the
			// files the store has.
			name:   "a range holding nothing still counts the store",
			method: http.MethodGet, path: "/api/v1/conversations?since=2027-01-01T00:00:00Z",
			want:          http.StatusOK,
			wantBodyHas:   []string{`"conversations":[]`, `"total_conversations":2`},
			wantBodyLacks: []string{`"id":"conv-`},
		},
		{
			name:   "list rejects a non-numeric limit",
			method: http.MethodGet, path: "/api/v1/conversations?limit=abc",
			want: http.StatusBadRequest,
		},
		{
			// A client that means "everything" passes a large number; zero
			// and negative values are rejected rather than read as one.
			name:   "list rejects a non-positive limit",
			method: http.MethodGet, path: "/api/v1/conversations?limit=0",
			want: http.StatusBadRequest,
		},
		{
			name:   "detail returns one conversation",
			method: http.MethodGet, path: "/api/v1/conversations/conv-A",
			want:        http.StatusOK,
			wantBodyHas: []string{`"id":"conv-A"`, `"generation_id":"g1"`, `"total_tokens":150`},
		},
		{
			name:   "detail 404s on unknown conversation",
			method: http.MethodGet, path: "/api/v1/conversations/does-not-exist",
			want: http.StatusNotFound,
		},
		{
			name:   "detail 404s on empty id (trailing slash)",
			method: http.MethodGet, path: "/api/v1/conversations/",
			want: http.StatusNotFound,
		},
		{
			name:   "detail 404s on slash-containing id",
			method: http.MethodGet, path: "/api/v1/conversations/a/b",
			want: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := newLocalRequest(tc.method, tc.path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d\nbody=%s", rr.Code, tc.want, rr.Body.String())
			}
			for _, want := range tc.wantBodyHas {
				if !strings.Contains(rr.Body.String(), want) {
					t.Errorf("body missing %q\n--- body ---\n%s", want, rr.Body.String())
				}
			}
			for _, unwanted := range tc.wantBodyLacks {
				if strings.Contains(rr.Body.String(), unwanted) {
					t.Errorf("body contains %q\n--- body ---\n%s", unwanted, rr.Body.String())
				}
			}
		})
	}
}

// TestServer_GenerationsExport_ProtoJSON exercises the wire-format path.
// The SDK's HTTP exporter sends proto-JSON: roles are protobuf enum names
// and int64 fields are JSON strings. The receiver stores the raw generation
// and the query layer normalises only the fields the viewer needs.
func TestServer_GenerationsExport_ProtoJSON(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		wantAccepted []bool
		wantConvID   string
		wantListHas  []string
		check        func(t *testing.T, detail *ConversationDetail)
	}{
		{
			name: "full proto-json with enums and int64-as-string",
			body: `{"generations":[{
				"id":"gen-pj",
				"conversation_id":"conv-pj",
				"agent_name":"claude-code",
				"mode":"GENERATION_MODE_SYNC",
				"model":{"provider":"anthropic","name":"claude-opus-4-7"},
				"input":[{"role":"MESSAGE_ROLE_USER","parts":[{"text":"hi"}]}],
				"output":[{"role":"MESSAGE_ROLE_ASSISTANT","parts":[{"text":"hello"}]}],
				"usage":{"input_tokens":"6","output_tokens":"14","total_tokens":"20"},
				"stop_reason":"end_turn",
				"started_at":"2026-05-21T13:01:50.922Z",
				"completed_at":"2026-05-21T13:01:50.922Z",
				"metadata":{"agento11y.conversation.title":"Local mode smoke test"}
			}]}`,
			wantConvID:  "conv-pj",
			wantListHas: []string{`"title":"Local mode smoke test"`},
			check: func(t *testing.T, detail *ConversationDetail) {
				require.Len(t, detail.Generations, 1)
				gen := detail.Generations[0]
				assert.Equal(t, int64(6), gen.InputTokens)
				assert.Equal(t, int64(14), gen.OutputTokens)
				assert.Equal(t, int64(20), gen.TotalTokens)
				require.Len(t, gen.Input, 1)
				require.Len(t, gen.Output, 1)
				assert.Equal(t, agento11y.RoleUser, gen.Input[0].Role)
				assert.Equal(t, agento11y.RoleAssistant, gen.Output[0].Role)
				assert.Equal(t, "end_turn", gen.StopReason)
				assert.Equal(t, "Local mode smoke test", detail.Title)
			},
		},
		{
			name: "tool data is normalised for the detail view",
			body: `{"generations":[{"id":"g","conversation_id":"conv-tool",
				"input":[{"role":"MESSAGE_ROLE_TOOL","parts":[{"tool_result":{"tool_call_id":"tc1","content":"ok"}}]}],
				"output":[{"role":"MESSAGE_ROLE_ASSISTANT","parts":[{"tool_call":{"id":"tc1","name":"bash","input_json":"eyJjb21tYW5kIjoibHMifQ=="}}]}]
			}]}`,
			wantConvID: "conv-tool",
			check: func(t *testing.T, detail *ConversationDetail) {
				require.Len(t, detail.Generations, 1)
				gen := detail.Generations[0]
				input := gen.Input
				require.Len(t, input, 1)
				assert.Equal(t, agento11y.RoleTool, input[0].Role)
				require.Len(t, input[0].Parts, 1)
				part := input[0].Parts[0]
				require.NotNil(t, part.ToolResult)
				assert.Equal(t, agento11y.PartKindToolResult, part.Kind)
				assert.Equal(t, "tc1", part.ToolResult.ToolCallID)
				assert.Equal(t, "ok", part.ToolResult.Content)
				assert.Equal(t, []string{"bash"}, gen.Tools)
				assert.Equal(t, "ls", gen.ToolPreview)
			},
		},
		{
			name:         "malformed entry rejected, valid entry in same batch still accepted",
			body:         `{"generations":[{"id":"ok","conversation_id":"c1"},{"id":"bad","conversation_id":"c1","usage":"not-an-object"}]}`,
			wantAccepted: []bool{true, false},
			wantConvID:   "c1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			resp := post(t, s, "/api/v1/generations:export", "application/json", tc.body)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)

			var out generationsResponse
			decodeJSON(t, resp.Body, &out)

			wantAccepted := tc.wantAccepted
			if wantAccepted == nil {
				wantAccepted = make([]bool, len(out.Results))
				for i := range wantAccepted {
					wantAccepted[i] = true
				}
			}
			require.Len(t, out.Results, len(wantAccepted))
			for i, want := range wantAccepted {
				assert.Equal(t, want, out.Results[i].Accepted, "result[%d].error=%q", i, out.Results[i].Error)
				if !want {
					assert.NotEmpty(t, out.Results[i].Error)
				}
			}

			if tc.wantConvID == "" {
				return
			}

			detail, err := s.storage.ConversationDetail(tc.wantConvID)
			require.NoError(t, err)
			require.NotNil(t, detail)
			if tc.check != nil {
				tc.check(t, detail)
			}

			req := newLocalRequest(http.MethodGet, "/api/v1/conversations", nil)
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			body := rr.Body.String()
			assert.Contains(t, body, `"`+tc.wantConvID+`"`)
			for _, want := range tc.wantListHas {
				assert.Contains(t, body, want)
			}
		})
	}
}

// TestServer_APIConversations_EmptyStorage covers the path the user
// will hit most often — opening the UI with no generations recorded
// yet. The endpoint must return an array, never null, and report the
// empty store so the viewer shows its first-launch notice.
func TestServer_APIConversations_EmptyStorage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := newLocalRequest(http.MethodGet, "/api/v1/conversations", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"conversations":[],"total_conversations":0}`, strings.TrimSpace(rr.Body.String()))
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	srv, _, dir := newTestServerStorage(t)
	return srv, dir
}

// newTestServerStorage is newTestServer for tests that also instrument the
// storage the server writes through.
func newTestServerStorage(t *testing.T) (*Server, *Storage, string) {
	t.Helper()
	// The forward loader falls back to the process environment, so a developer
	// with forwarding and real credentials exported would otherwise have this
	// suite POST to their live tenant.
	clearForwardEnv(t)
	// LOCAL_WEB_DIR is deliberately not an alias family, so PinAliasEnvBlank
	// leaves it alone. A developer with it exported would otherwise have the
	// asset tests compile their working tree instead of the embedded viewer.
	t.Setenv(envconfig.PreferredKey("LOCAL_WEB_DIR"), "")
	t.Setenv(envconfig.LegacyKey("LOCAL_WEB_DIR"), "")
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return NewServer(storage, nil, filepath.Join(dir, "config.env")), storage, dir
}

func post(t *testing.T, s *Server, path, contentType, body string) *http.Response {
	t.Helper()
	return postBytes(t, s, path, contentType, []byte(body))
}

// postDiscard issues a POST and discards the response. Use it when the test
// only cares that the request was accepted, not the body content.
func postDiscard(t *testing.T, s *Server, path, contentType, body string) {
	t.Helper()
	resp := postBytes(t, s, path, contentType, []byte(body))
	resp.Body.Close()
}

func postBytes(t *testing.T, s *Server, path, contentType string, body []byte) *http.Response {
	t.Helper()
	req := newLocalRequest(http.MethodPost, path, bytes.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr.Result()
}

func decodeJSON(t *testing.T, body interface {
	Read(p []byte) (int, error)
	Close() error
}, dst any) {
	t.Helper()
	defer body.Close()
	if err := json.NewDecoder(body).Decode(dst); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

// TestServer_APITokenMetrics checks the token-usage endpoint the viewer
// charts: a seeded store returns points with provider-aware disjoint
// buckets, and a wrong method is rejected. Bucket math and sorting are
// covered in query_test; this asserts the wire shape.
func TestServer_APITokenMetrics(t *testing.T) {
	srv, dir := newTestServer(t)
	storage, err := NewStorage(dir)
	require.NoError(t, err)

	writeGen(t, storage, "conv-A", "g1", agento11y.Generation{
		Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:02Z"),
		Usage:       agento11y.TokenUsage{InputTokens: 100, OutputTokens: 50, CacheReadInputTokens: 30, CacheWriteInputTokens: 20},
	}, "2026-05-21T10:00:02Z")

	t.Run("seeded store returns disjoint points", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet, "/api/v1/metrics/tokens", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)

		var body struct {
			Points []TokenUsagePoint `json:"points"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Len(t, body.Points, 1)
		// The embedded TokenBuckets must flatten into the point object;
		// the viewer reads these keys at the top level.
		assert.Contains(t, rr.Body.String(), `"fresh_input":100`)
		assert.Contains(t, rr.Body.String(), `"cache_read":30`)
		assert.Contains(t, rr.Body.String(), `"calls":1`)
		assert.Equal(t, TokenUsagePoint{
			Timestamp:    mustParse(t, "2026-05-21T10:00:00Z"),
			Model:        "claude-sonnet-4",
			Provider:     "anthropic",
			Calls:        1,
			TokenBuckets: TokenBuckets{FreshInput: 100, CacheRead: 30, CacheWrite: 20, Output: 50},
		}, body.Points[0])
	})

	t.Run("wrong method rejected", func(t *testing.T) {
		req := newLocalRequest(http.MethodPost, "/api/v1/metrics/tokens", strings.NewReader("{}"))
		req.Header.Set("Content-Type", wire.ContentTypeJSON)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	})

	t.Run("range and interval bound the response", func(t *testing.T) {
		writeGen(t, storage, "conv-B", "g2", agento11y.Generation{
			Model:     agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
			StartedAt: mustParse(t, "2026-05-21T10:40:00Z"),
			Usage:     agento11y.TokenUsage{InputTokens: 7},
		}, "2026-05-21T10:40:00Z")

		req := newLocalRequest(http.MethodGet, "/api/v1/metrics/tokens?since=2026-05-21T09:00:00Z&interval=3600", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)

		var body struct {
			Points          []TokenUsagePoint `json:"points"`
			IntervalSeconds int64             `json:"interval_seconds"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, int64(3600), body.IntervalSeconds, "the response echoes the interval used")
		// Both generations share the 10:00 bucket and the same model, so
		// they collapse into one point.
		require.Len(t, body.Points, 1)
		assert.Equal(t, mustParse(t, "2026-05-21T10:00:00Z"), body.Points[0].Timestamp)
		assert.Equal(t, int64(107), body.Points[0].FreshInput)
		assert.Equal(t, 2, body.Points[0].Calls)
	})

	t.Run("since skips older files without decoding them", func(t *testing.T) {
		requireUnreadableFilesSupported(t)
		writeGen(t, storage, "conv-old", "g3", agento11y.Generation{
			Model:       agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4"},
			StartedAt:   mustParse(t, "2026-05-01T10:00:00Z"),
			CompletedAt: mustParse(t, "2026-05-01T10:00:01Z"),
			Usage:       agento11y.TokenUsage{InputTokens: 999},
		}, "2026-05-01T10:00:01Z")
		// Unreadable, so a request that opens it fails instead of quietly
		// paying for the decode the modification-time break exists to avoid.
		blockConversationFile(t, storage, "conv-old")

		req := newLocalRequest(http.MethodGet, "/api/v1/metrics/tokens?since=2026-05-21T09:00:00Z&interval=3600", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)

		var body struct {
			Points []TokenUsagePoint `json:"points"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		require.Len(t, body.Points, 1)
		assert.Equal(t, int64(107), body.Points[0].FreshInput, "the older file contributes nothing")
	})

	t.Run("invalid parameters rejected", func(t *testing.T) {
		for _, path := range []string{
			"/api/v1/metrics/tokens?since=last-tuesday",
			"/api/v1/metrics/tokens?interval=0",
			"/api/v1/metrics/tokens?interval=hourly",
		} {
			req := newLocalRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusBadRequest, rr.Code, path)
		}
	})
}

func TestServer_APITokenMetricsPeriodFilters(t *testing.T) {
	srv, storage, _ := newTestServerStorage(t)
	for _, seed := range []struct {
		conv, when, workspace string
		tokens                int64
	}{
		{conv: "repo-inside", when: "2026-05-21T10:00:00Z", workspace: "/repo", tokens: 11},
		{conv: "repo-upper", when: "2026-05-21T11:00:00Z", workspace: "/repo", tokens: 100},
		{conv: "unknown", when: "2026-05-21T10:15:00Z", tokens: 7},
	} {
		writeGen(t, storage, seed.conv, "g", agento11y.Generation{
			StartedAt: mustParse(t, seed.when),
			Usage:     agento11y.TokenUsage{InputTokens: seed.tokens},
			Tags:      map[string]string{"cwd": seed.workspace},
		}, seed.when)
	}

	cases := []struct {
		name, workspace string
		want            int64
	}{
		{name: "named workspace", workspace: "%2Frepo", want: 11},
		{name: "blank workspace means unknown", workspace: "", want: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/v1/metrics/tokens?since=2026-05-21T10:00:00Z&before=2026-05-21T11:00:00Z&interval=3600&workspace=" + tc.workspace
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, path, nil))
			require.Equal(t, http.StatusOK, rr.Code)
			var got struct {
				Points []TokenUsagePoint `json:"points"`
			}
			require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
			require.Len(t, got.Points, 1)
			assert.Equal(t, tc.want, got.Points[0].FreshInput)
		})
	}
}

func TestServer_APITokenMetrics_EmptyStorage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := newLocalRequest(http.MethodGet, "/api/v1/metrics/tokens", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"interval_seconds":10,"points":[]}`, strings.TrimSpace(rr.Body.String()))
}

// configPathFor returns the dotenv path newTestServer wired into the server,
// so config-endpoint tests can inspect what was written to disk.
func configPathFor(dir string) string { return filepath.Join(dir, "config.env") }

func putConfig(t *testing.T, s *Server, settings Settings) *http.Response {
	t.Helper()
	body, err := json.Marshal(configRequest{Settings: settings})
	require.NoError(t, err)
	req := newLocalRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://127.0.0.1:8765")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr.Result()
}

// TestServer_Config_RoundTrip saves settings and reads them back, asserting
// the GET reflects the normalised on-disk state and the file is written.
func TestServer_Config_RoundTrip(t *testing.T) {
	srv, dir := newTestServer(t)

	// GET on an absent file returns the local defaults.
	req := newLocalRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var got configResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Empty(t, got.Settings.Capture) // unset until the user picks a mode
	assert.True(t, got.Settings.AutoUpdate)
	assert.Equal(t, guardsOff, got.Settings.Guards)
	assert.False(t, got.Settings.LocalForward)
	assert.Equal(t, forwardStatus{Mode: forwardModeOff}, got.ForwardStatus)
	assert.Empty(t, got.ForwardStatus.Reason, "forwarding off by default is not a paused state")

	// Save a non-default configuration.
	resp := putConfig(t, srv, Settings{
		Endpoint:     "https://cloud.example.test",
		TenantID:     "12345",
		Token:        "glc_token",
		Capture:      "metadata_only",
		Tags:         []Tag{{Key: "team", Value: "ai"}},
		Guards:       guardsFailClosed,
		GuardTimeout: "2000",
		Debug:        true,
		AutoUpdate:   false,
		UserID:       "alice",
		LocalForward: true,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var saved configResponse
	decodeJSON(t, resp.Body, &saved)
	assert.Equal(t, "metadata_only", saved.Settings.Capture)
	assert.Equal(t, guardsFailClosed, saved.Settings.Guards)
	assert.Equal(t, "2000", saved.Settings.GuardTimeout)
	assert.True(t, saved.Settings.Debug)
	assert.False(t, saved.Settings.AutoUpdate)
	assert.Equal(t, "alice", saved.Settings.UserID)
	assert.True(t, saved.Settings.LocalForward)
	// The saved toggle plus usable credentials resolve to a live forwarding
	// posture, reduced to metadata_only by the saved capture mode. No OTLP
	// endpoint was configured, so that leg reports why it stays off. Guards
	// were saved on, so the hook leg is live as well.
	assert.Equal(t, forwardStatus{
		Enabled:     true,
		Mode:        forwardModeMetadataOnly,
		Generations: true,
		Hooks:       true,
		OTLPReason:  "no OTLP endpoint configured, so traces and metrics are not forwarded",
	}, saved.ForwardStatus)

	// Preview and on-disk file agree, sorted with the managed header.
	onDisk, err := os.ReadFile(configPathFor(dir))
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "SIGIL_CONTENT_CAPTURE_MODE=metadata_only")
	assert.Contains(t, string(onDisk), "SIGIL_GUARDS_TIMEOUT_MS=2000")
	// Both spellings are written so an older binary still reads the toggle.
	assert.Contains(t, string(onDisk), "AGENTO11Y_LOCAL_FORWARD=true")
	assert.Contains(t, string(onDisk), "SIGIL_LOCAL_FORWARD=true")
	assert.Contains(t, saved.Preview, "SIGIL_USER_ID=alice")
	assert.True(t, strings.HasPrefix(saved.Preview, "# Managed by `agento11y login`."))

	// A fresh GET returns the same saved snapshot.
	req2 := newLocalRequest(http.MethodGet, "/api/v1/config", nil)
	rr2 := httptest.NewRecorder()
	srv.ServeHTTP(rr2, req2)
	require.Equal(t, http.StatusOK, rr2.Code)
	var reread configResponse
	require.NoError(t, json.Unmarshal(rr2.Body.Bytes(), &reread))
	assert.Equal(t, saved.Settings, reread.Settings)
}

// TestServer_Config_StackURL covers the read-only stack URL the connect flow
// prefills its setup-page link from. It is not a Settings field, so a save must
// neither return it as editable state nor delete it from the file.
func TestServer_Config_StackURL(t *testing.T) {
	srv, dir := newTestServer(t)
	path := configPathFor(dir)
	require.NoError(t, dotenv.WriteDotenv(path, map[string]string{
		"AGENTO11Y_STACK_URL": "https://mystack.grafana.net",
	}, nil))

	req := newLocalRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	var got configResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "https://mystack.grafana.net", got.StackURL)

	resp := putConfig(t, srv, Settings{Guards: guardsOff, AutoUpdate: true})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var saved configResponse
	decodeJSON(t, resp.Body, &saved)
	assert.Equal(t, "https://mystack.grafana.net", saved.StackURL)
	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "AGENTO11Y_STACK_URL=https://mystack.grafana.net")
}

// TestServer_Config_OtlpHeaders covers the write-only OTLP headers over the
// wire: an unrelated save preserves the value on disk, the connect flow replaces
// it, disconnect deletes it, and no response ever carries it back.
func TestServer_Config_OtlpHeaders(t *testing.T) {
	srv, dir := newTestServer(t)
	path := configPathFor(dir)
	require.NoError(t, dotenv.WriteDotenv(path, map[string]string{
		"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Basic old",
	}, nil))

	keep := putConfig(t, srv, Settings{Guards: guardsOff, AutoUpdate: true})
	defer keep.Body.Close()
	require.Equal(t, http.StatusOK, keep.StatusCode)
	var kept configResponse
	decodeJSON(t, keep.Body, &kept)
	assert.True(t, kept.Settings.OtlpHeadersSet)
	assert.Empty(t, kept.Settings.OtlpHeaders, "the headers carry a credential and are never read back")
	assert.NotContains(t, kept.Preview, "Basic old")
	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic old"`)

	replace := putConfig(t, srv, Settings{Guards: guardsOff, AutoUpdate: true,
		OtlpHeadersSet: true, OtlpHeaders: "Authorization=Basic new"})
	defer replace.Body.Close()
	require.Equal(t, http.StatusOK, replace.StatusCode)
	onDisk, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), `OTEL_EXPORTER_OTLP_HEADERS="Authorization=Basic new"`)

	clear := putConfig(t, srv, Settings{Guards: guardsOff, AutoUpdate: true,
		OtlpHeadersSet: true, OtlpHeadersCleared: true})
	defer clear.Body.Close()
	require.Equal(t, http.StatusOK, clear.StatusCode)
	var cleared configResponse
	decodeJSON(t, clear.Body, &cleared)
	assert.False(t, cleared.Settings.OtlpHeadersSet)
	onDisk, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(onDisk), "OTEL_EXPORTER_OTLP_HEADERS")
}

// TestServer_Config_Preview renders without writing to disk.
func TestServer_Config_Preview(t *testing.T) {
	srv, dir := newTestServer(t)
	body, err := json.Marshal(configRequest{Settings: Settings{
		Capture: "full", Guards: guardsFailOpen, GuardTimeout: "2500", AutoUpdate: true,
	}})
	require.NoError(t, err)
	resp := post(t, srv, "/api/v1/config:preview", "application/json", string(body))
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Preview string `json:"preview"`
	}
	decodeJSON(t, resp.Body, &got)
	assert.Contains(t, got.Preview, "SIGIL_GUARDS_FAIL_OPEN=true")
	assert.Contains(t, got.Preview, "SIGIL_GUARDS_TIMEOUT_MS=2500")
	// Opt-out/opt-in keys at their defaults must not appear.
	assert.NotContains(t, got.Preview, "SIGIL_AUTO_UPDATE")
	assert.NotContains(t, got.Preview, "SIGIL_DEBUG")
	// LOCAL_FORWARD is the exception: off is written as an explicit false,
	// because the daemon prefers config.env and a missing key cannot override
	// the value it inherited into its own environment at boot.
	assert.Contains(t, got.Preview, "AGENTO11Y_LOCAL_FORWARD=false")
	assert.Contains(t, got.Preview, "SIGIL_LOCAL_FORWARD=false")
	// Preview is read-only: no file should have been created.
	_, statErr := os.Stat(configPathFor(dir))
	assert.True(t, os.IsNotExist(statErr))
}

// TestServer_Config_DoesNotLeakSecrets confirms the auth token never crosses
// to the client (endpoint and tenant id may, they are not secrets), that a
// blank token is kept, and that an explicit reset removes it.
func TestServer_Config_DoesNotLeakSecrets(t *testing.T) {
	srv, dir := newTestServer(t)
	path := configPathFor(dir)
	require.NoError(t, dotenv.WriteDotenv(path, map[string]string{
		"SIGIL_ENDPOINT":       "https://sigil.example.net",
		"SIGIL_AUTH_TENANT_ID": "12345",
		"SIGIL_AUTH_TOKEN":     "glc_supersecret",
		"SIGIL_USER_ID_SOURCE": "accountUuid",
	}, nil))

	// GET surfaces endpoint/tenant and reports the token is set, but never
	// returns the token value; the preview shows it masked.
	req := newLocalRequest(http.MethodGet, "/api/v1/config", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.NotContains(t, rr.Body.String(), "glc_supersecret")
	var got configResponse
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &got))
	assert.Equal(t, "https://sigil.example.net", got.Settings.Endpoint)
	assert.Equal(t, "12345", got.Settings.TenantID)
	assert.True(t, got.Settings.TokenSet)
	assert.Empty(t, got.Settings.Token)
	assert.Contains(t, got.Preview, "SIGIL_AUTH_TOKEN=<set>")

	// Saving with a blank token keeps it; endpoint/tenant round-trip; unmanaged
	// keys survive the merge.
	resp := putConfig(t, srv, Settings{
		Endpoint: "https://sigil.example.net", TenantID: "12345", TokenSet: true,
		Capture: "full", Guards: guardsOff, AutoUpdate: true, UserID: "alice",
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(body), "glc_supersecret")

	onDisk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "SIGIL_AUTH_TOKEN=glc_supersecret")
	assert.Contains(t, string(onDisk), "SIGIL_USER_ID_SOURCE=accountUuid")
	assert.Contains(t, string(onDisk), "SIGIL_USER_ID=alice")

	// Resetting the token removes it from disk.
	resp2 := putConfig(t, srv, Settings{
		Endpoint: "https://sigil.example.net", TenantID: "12345",
		TokenSet: true, TokenCleared: true,
		Capture: "full", Guards: guardsOff, AutoUpdate: true,
	})
	defer resp2.Body.Close()
	require.Equal(t, http.StatusOK, resp2.StatusCode)
	onDisk2, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.NotContains(t, string(onDisk2), "SIGIL_AUTH_TOKEN")
}

// TestServer_Config_RejectsBadBody covers malformed input handling.
func TestServer_Config_RejectsBadBody(t *testing.T) {
	srv, _ := newTestServer(t)
	resp := post(t, srv, "/api/v1/config:preview", "application/json", "{not json")
	assert.Equal(t, http.StatusBadRequest, resp.StatusCode)
	resp.Body.Close()
}

// newForwardingTestServer builds a server whose config.env enables (or not)
// Cloud forwarding to the given fake Cloud server. The forward client is
// pointed at the fake server so its self-signed TLS cert is trusted.
func newForwardingTestServer(t *testing.T, cloud *httptest.Server, env map[string]string) (*Server, string) {
	t.Helper()
	clearForwardEnv(t)
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	s := NewServer(storage, nil, writeConfigEnvFile(t, env))
	s.forward.client = cloud.Client()
	return s, dir
}

// TestServer_Forwarding_OffByDefault covers the opt-in guarantee: without the
// LOCAL_FORWARD toggle the daemon stores locally and never contacts Cloud.
func TestServer_Forwarding_OffByDefault(t *testing.T) {
	hits := make(chan struct{}, 8)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, dir := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_ENDPOINT": cloud.URL, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})

	postDiscard(t, s, "/api/v1/generations:export", "application/json",
		`{"generations":[{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m"}}]}`)
	postDiscard(t, s, "/otlp/v1/traces", "application/x-protobuf", "")
	s.forward.wait()

	assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl")), 1)
	assert.Empty(t, hits, "no Cloud request should be made when forwarding is off")
}

// TestServer_Forwarding_OnForwardsGeneration covers the toggle-on path through
// the handler: a stored generation is also POSTed to Cloud, stripped to the
// configured (default metadata_only) content level.
func TestServer_Forwarding_OnForwardsGeneration(t *testing.T) {
	received := make(chan []byte, 1)
	var gotPath string
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		received <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, dir := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})

	postDiscard(t, s, "/api/v1/generations:export", "application/json",
		`{"generations":[{"id":"gen-1","conversation_id":"conv-A","model":{"provider":"p","name":"m"},"system_prompt":"secret"}]}`)
	s.forward.wait()

	// The local store keeps the full content the forwarded copy dropped.
	lines := readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl"))
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "secret")

	body := <-received
	assert.Equal(t, wire.GenerationExportHTTPPath, gotPath)
	req, err := wire.UnmarshalExportGenerationsJSON(body)
	require.NoError(t, err)
	require.Len(t, req.GetGenerations(), 1)
	g := req.GetGenerations()[0]
	assert.Equal(t, "gen-1", g.GetId())
	assert.Empty(t, g.GetSystemPrompt(), "default content mode strips the forwarded copy")
	assert.Equal(t, "metadata_only", g.GetMetadata().GetFields()[model.MetadataKeyContentCaptureMode].GetStringValue())
}

// TestServer_Forwarding_FailureKeepsLocalStore covers the best-effort
// guarantee: a Cloud failure leaves the local store and the child's ack intact.
func TestServer_Forwarding_FailureKeepsLocalStore(t *testing.T) {
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cloud.Close()

	s, dir := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})

	resp := post(t, s, "/api/v1/generations:export", "application/json",
		`{"generations":[{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m"}}]}`)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out generationsResponse
	decodeJSON(t, resp.Body, &out)
	require.Len(t, out.Results, 1)
	assert.True(t, out.Results[0].Accepted, "child ack succeeds despite Cloud failure")

	s.forward.wait()
	assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl")), 1)
}

// TestServer_Forwarding_RejectedGenerationNotForwarded covers the ordering
// contract: only generations the local store accepted are relayed.
func TestServer_Forwarding_RejectedGenerationNotForwarded(t *testing.T) {
	received := make(chan []byte, 1)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- b
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, _ := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})

	postDiscard(t, s, "/api/v1/generations:export", "application/json",
		`{"generations":["not-an-object",{"id":"gen-ok","conversation_id":"conv-A","model":{"name":"m"}}]}`)
	s.forward.wait()

	req, err := wire.UnmarshalExportGenerationsJSON(<-received)
	require.NoError(t, err)
	require.Len(t, req.GetGenerations(), 1)
	assert.Equal(t, "gen-ok", req.GetGenerations()[0].GetId())
}

// TestServer_Forwarding_RelaysOTLP covers the OTLP leg through the handler:
// traces are relayed to the configured Cloud OTLP endpoint (stripped, because
// the default content mode is reduced) while the local ack stays a 200.
func TestServer_Forwarding_RelaysOTLP(t *testing.T) {
	type capture struct {
		path string
		body []byte
	}
	received := make(chan capture, 2)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- capture{path: r.URL.Path, body: b}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, _ := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL,
	})

	traces, err := proto.Marshal(traceRequestWithContent())
	require.NoError(t, err)
	resp := postBytes(t, s, "/otlp/v1/traces", "application/x-protobuf", traces)
	resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	s.forward.wait()

	c := <-received
	assert.Equal(t, "/v1/traces", c.path)
	assertTraceStripped(t, c.body, wire.ContentTypeProto)
}

// hookCloud is a fake Cloud hook endpoint: it records what the daemon relayed
// and answers with a canned status and body, both settable between calls.
//
// TLS keeps this server outside IsLocalEndpoint. The test client trusts its cert.
type hookCloud struct {
	srv *httptest.Server

	// Set before or between calls, read by the handler.
	status  int
	respond string
	delay   time.Duration

	// The hook path is synchronous, so unlike the generations and OTLP legs
	// no wait() is needed before reading these — but the other legs in this
	// file are async against the same server, so recording is still guarded.
	mu      sync.Mutex
	n       int
	path    string
	body    string
	headers http.Header
}

func newHookCloud(t *testing.T) *hookCloud {
	t.Helper()
	c := &hookCloud{status: http.StatusOK, respond: `{"action":"allow","evaluations":[]}`}
	c.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		c.record(r.URL.Path, body, r.Header.Clone())
		if c.delay > 0 {
			time.Sleep(c.delay)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c.status)
		_, _ = io.WriteString(w, c.respond)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *hookCloud) record(path string, body []byte, headers http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	c.path, c.body, c.headers = path, string(body), headers
}

func (c *hookCloud) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *hookCloud) lastCall() (path, body string, headers http.Header) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.path, c.body, c.headers
}

// hookEnv is the config.env a chaining daemon needs: forwarding on, guards on,
// and usable Cloud credentials pointed at the fake Cloud.
// An override with an empty value removes the key, so a case can express "this
// one gate is not satisfied" without restating the other four.
func hookEnv(cloudURL string, extra map[string]string) map[string]string {
	env := map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloudURL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
		"AGENTO11Y_GUARDS_ENABLED": "true",
	}
	maps.Copy(env, extra)
	maps.DeleteFunc(env, func(_, v string) bool { return v == "" })
	return env
}

// postHook issues a hook evaluation and returns the status plus the verdict
// the calling agent would act on. Each opt rewrites the request before it is
// served.
func postHook(t *testing.T, s *Server, body string, headers map[string]string, opts ...func(*http.Request) *http.Request) (int, agento11y.HookEvaluateResponse) {
	t.Helper()
	req := newLocalRequest(http.MethodPost, "/api/v1/hooks:evaluate", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, opt := range opts {
		req = opt(req)
	}
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	resp := rr.Result()
	defer func() { _ = resp.Body.Close() }()
	var out agento11y.HookEvaluateResponse
	if resp.StatusCode == http.StatusOK {
		raw, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		out, err = decodeHookEvaluateResponse(raw)
		require.NoError(t, err)
	}
	return resp.StatusCode, out
}

const hookToolCallBody = `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash"}}]}]}}`

// abortDuringCall is a postHook option that cancels the request context shortly
// after the request is issued, the way an agent that hit its own hook deadline
// (or a user who interrupted) drops the call while Cloud is still stalling.
// Pair it with a Cloud delay longer than the cancel.
func abortDuringCall(t *testing.T) func(*http.Request) *http.Request {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	return func(r *http.Request) *http.Request { return r.WithContext(ctx) }
}

// TestServer_HookEvaluate_Gates covers when a --local hook evaluation reaches
// Cloud. Chaining needs both the forwarding opt-in and guards: telemetry
// forwarding is what the user consented to, and a guard check ships the tool
// call itself.
func TestServer_HookEvaluate_Gates(t *testing.T) {
	cases := []struct {
		name      string
		override  map[string]string // applied on top of hookEnv
		wantChain bool
		wantWhy   string // substring of forwardStatus.HookReason
	}{
		{
			name:     "forwarding_off",
			override: map[string]string{"AGENTO11Y_LOCAL_FORWARD": ""},
			// Forwarding off at all is not a paused state, so no leg has
			// anything to explain.
			wantWhy: "",
		},
		{
			name:     "guards_off",
			override: map[string]string{"AGENTO11Y_GUARDS_ENABLED": ""},
			wantWhy:  "GUARDS_ENABLED",
		},
		{
			name:     "placeholder_credentials",
			override: map[string]string{"AGENTO11Y_AUTH_TOKEN": envconfig.LocalAuthPlaceholder},
			wantWhy:  "placeholder",
		},
		{name: "both_on", wantChain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			cloud.respond = `{"action":"deny","rule_id":"r1","reason":"nope"}`
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, tc.override))

			status, out := postHook(t, s, hookToolCallBody, nil)
			require.Equal(t, http.StatusOK, status)
			st := s.forward.status()
			assert.Equal(t, tc.wantChain, st.Hooks)
			if tc.wantChain {
				assert.Equal(t, 1, cloud.count())
				assert.Equal(t, agento11y.HookActionDeny, out.Action)
				assert.Empty(t, st.HookReason, "a live hook leg has nothing to explain")
				return
			}
			assert.Zero(t, cloud.count(), "a refused hook leg must make no outbound request")
			assert.Equal(t, agento11y.HookActionAllow, out.Action)
			if tc.wantWhy == "" {
				assert.Empty(t, st.HookReason)
			} else {
				assert.Contains(t, st.HookReason, tc.wantWhy)
			}
		})
	}
}

func TestServer_HookEvaluate_RefusesLocalRelay(t *testing.T) {
	peer, _ := newTestServer(t)
	hits := make(chan struct{}, 1)
	peerHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits <- struct{}{}
		peer.ServeHTTP(w, r)
	}))
	defer peerHTTP.Close()

	source, _ := newForwardingTestServer(t, peerHTTP, hookEnv(peerHTTP.URL, nil))
	status, out := postHook(t, source, hookToolCallBody, nil)

	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)
	st := source.forward.status()
	assert.False(t, st.Hooks)
	assert.Contains(t, st.HookReason, "is local")
	assert.Len(t, hits, 0, "a local hook target must make no outbound request")
	assert.Nil(t, st.Legs, "a refused relay must not record a Cloud delivery")
}

// TestServer_HookEvaluate_CloudVerdict covers the verdicts a chained call
// returns to the calling agent: the action, the rule that produced it, a
// transform, and the per-rule evaluations.
func TestServer_HookEvaluate_CloudVerdict(t *testing.T) {
	cases := []struct {
		name       string
		respond    string
		wantAction agento11y.HookAction
		wantRuleID string
		wantReason string
		assertMore func(t *testing.T, out agento11y.HookEvaluateResponse)
	}{
		{
			name:       "allow",
			respond:    `{"action":"allow","evaluations":[]}`,
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "deny",
			respond:    `{"action":"deny","rule_id":"r1","reason":"blocked by policy"}`,
			wantAction: agento11y.HookActionDeny,
			wantRuleID: "r1",
			wantReason: "blocked by policy",
		},
		{
			name: "transform",
			respond: `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[` +
				`{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":"eyJjb21tYW5kIjoiZWNobyBzYWZlIn0="}},` +
				`{"kind":"tool_result","tool_result":{"tool_call_id":"c1","content_json":"eyJvayI6dHJ1ZX0="}}]}],` +
				`"tools":[{"name":"Bash","input_schema_json":"eyJ0eXBlIjoib2JqZWN0In0="}]}}`,
			wantAction: agento11y.HookActionAllow,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.NotNil(t, out.TransformedInput)
				require.Len(t, out.TransformedInput.Output, 1)
				parts := out.TransformedInput.Output[0].Parts
				require.Len(t, parts, 2)
				require.NotNil(t, parts[0].ToolCall)
				assert.JSONEq(t, `{"command":"echo safe"}`, string(parts[0].ToolCall.InputJSON))
				require.NotNil(t, parts[1].ToolResult)
				assert.JSONEq(t, `{"ok":true}`, string(parts[1].ToolResult.ContentJSON))
				require.Len(t, out.TransformedInput.Tools, 1)
				assert.JSONEq(t, `{"type":"object"}`, string(out.TransformedInput.Tools[0].InputSchema))
			},
		},
		{
			name:       "evaluations_preserved_in_order",
			respond:    `{"action":"allow","evaluations":[{"rule_id":"first","passed":true},{"rule_id":"second","passed":false}]}`,
			wantAction: agento11y.HookActionAllow,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.Len(t, out.Evaluations, 2)
				assert.Equal(t, "first", out.Evaluations[0].RuleID)
				assert.Equal(t, "second", out.Evaluations[1].RuleID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			cloud.respond = tc.respond
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))

			status, out := postHook(t, s, hookToolCallBody, nil)
			require.Equal(t, http.StatusOK, status)
			require.Equal(t, 1, cloud.count())
			path, _, _ := cloud.lastCall()
			assert.Equal(t, hookEvaluatePath, path)
			assert.Equal(t, tc.wantAction, out.Action)
			assert.Equal(t, tc.wantRuleID, out.RuleID)
			assert.Equal(t, tc.wantReason, out.Reason)
			if tc.assertMore != nil {
				tc.assertMore(t, out)
			}
			st := s.forward.status()
			assert.Empty(t, st.Failures)
			assert.Zero(t, st.HookFailOpens)
		})
	}
}

// writeGuardsFileFor installs a ruleset the given server will read on its next
// hook request.
func writeGuardsFileFor(t *testing.T, s *Server, rulesTOML string) {
	t.Helper()
	require.NotEmpty(t, s.guards.RulesPath)
	require.NoError(t, os.WriteFile(s.guards.RulesPath, []byte(rulesTOML), 0o600))
}

func TestServer_HookEvaluate_ReadsRulesOnEachRequest(t *testing.T) {
	s, _ := newTestServer(t)

	status, out := postHook(t, s, `{"phase":"postflight","input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Bash","input_json":{"command":"rm -rf /tmp/x"}}}]}]}}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)

	writeGuardsFileFor(t, s, blockRmRules)
	status, out = postHook(t, s, `{"phase":"postflight","input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Bash","input_json":{"command":"rm -rf /tmp/x"}}}]}]}}`, nil)
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agento11y.HookActionDeny, out.Action)
	assert.Equal(t, "block.rm", out.RuleID)
}

// blockRmRules denies a Bash call whose serialized arguments contain rm -rf.
const blockRmRules = `
[[rules]]
rule_id = "block.rm"
phase = "postflight"
action_on_fail = "deny"
tool_filter.blocked_names = ["Bash(*rm -rf*)"]
`

// redactSecretRules rewrites an API key out of the tool arguments and allows.
const redactSecretRules = `
[[rules]]
rule_id = "redact.key"
phase = "postflight"
transform.patterns = [{ id = "api_key", regex = "sk-[A-Za-z0-9]+" }]
`

// redactPairRules removes an API key and, with a second pattern, a whole JSON
// key/value pair. The pair pattern cannot rewrite input_json without leaving it
// unmarshalable, so every redaction of that field is dropped once the payload
// carries a password.
const redactPairRules = `
[[rules]]
rule_id = "redact.pair"
phase = "postflight"
transform.patterns = [
  { id = "api_key", regex = "sk-[A-Za-z0-9]+" },
  { id = "pw", regex = '"password":\s*"[^"]*"' },
]
`

// passingRegexRules records one passing local evaluation without denying.
const passingRegexRules = `
[[rules]]
rule_id = "check.reset"
phase = "postflight"
action_on_fail = "deny"

  [[rules.evaluators]]
  kind = "regex"
  config.target = "response"
  config.reject = true
  config.patterns = ["git reset --hard"]
`

// hookRmToolCallBody is a postflight tool call the block.rm rule denies.
const hookRmToolCallBody = `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"output":[{"role":"assistant",` +
	`"parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"rm -rf /tmp/x"}}}]}]}}`

// hookSecretToolCallBody is a postflight tool call the redact.key rule rewrites.
const hookSecretToolCallBody = `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"output":[{"role":"assistant",` +
	`"parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"curl -H sk-abc123"}}}]}]}}`

// TestServer_HookEvaluate_LocalRules covers the local engine as the first
// verdict source, with Cloud chaining configured throughout: a local deny must
// answer without the relay, so a payload this machine already rejected never
// leaves it, and a local transform must survive a Cloud response that carries
// none of its own.
func TestServer_HookEvaluate_LocalRules(t *testing.T) {
	cases := []struct {
		name          string
		rules         string
		body          string
		cloudRespond  string
		wantCloudCall bool
		wantAction    agento11y.HookAction
		wantRuleID    string
		assertMore    func(t *testing.T, out agento11y.HookEvaluateResponse)
	}{
		{
			name:          "local_deny_skips_cloud",
			rules:         blockRmRules,
			body:          hookRmToolCallBody,
			wantAction:    agento11y.HookActionDeny,
			wantRuleID:    "block.rm",
			wantCloudCall: false,
		},
		{
			name:          "local_allow_reaches_cloud",
			rules:         blockRmRules,
			body:          hookToolCallBody,
			cloudRespond:  `{"action":"deny","rule_id":"cloud.r1","reason":"blocked by policy"}`,
			wantAction:    agento11y.HookActionDeny,
			wantRuleID:    "cloud.r1",
			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				assert.Equal(t, "blocked by policy", out.Reason)
			},
		},
		{
			name:          "local_transform_survives_cloud_allow",
			rules:         redactSecretRules,
			body:          hookSecretToolCallBody,
			cloudRespond:  `{"action":"allow","evaluations":[]}`,
			wantAction:    agento11y.HookActionAllow,
			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.NotNil(t, out.TransformedInput)
				encoded, err := json.Marshal(out.TransformedInput)
				require.NoError(t, err)
				assert.Contains(t, string(encoded), "[REDACTED:api_key]")
				assert.NotContains(t, string(encoded), "sk-abc123")
			},
		},
		{
			// Cloud does not say whether transformed_input started from the
			// redacted relay, so local patterns run over Cloud's copy again.
			name:         "cloud_transform_is_re_redacted",
			rules:        redactSecretRules,
			body:         hookSecretToolCallBody,
			cloudRespond: `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"curl -H sk-abc123 https://cloud.example"}}}]}]}}`,
			wantAction:   agento11y.HookActionAllow,

			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.NotNil(t, out.TransformedInput)
				encoded, err := json.Marshal(out.TransformedInput)
				require.NoError(t, err)
				assert.NotContains(t, string(encoded), "sk-abc123", "the local redaction must survive Cloud's own rewrite")
				assert.Contains(t, string(encoded), "[REDACTED:api_key]")
				assert.Contains(t, string(encoded), "https://cloud.example", "Cloud's rewrite is kept")
			},
		},
		{
			// A Cloud rewrite that local patterns cannot safely re-redact must
			// not replace the already-redacted local input.
			name:          "invalid_cloud_re_redaction_keeps_local_transform",
			rules:         redactPairRules,
			body:          hookSecretToolCallBody,
			cloudRespond:  `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":"eyJwYXNzd29yZCI6Imh1bnRlcjIiLCJjb21tYW5kIjoiY3VybCAtSCBzay1hYmMxMjMgaHR0cHM6Ly9jbG91ZC5leGFtcGxlIn0="}}]}]}}`,
			wantAction:    agento11y.HookActionAllow,
			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.Len(t, out.Evaluations, 1)
				assert.Equal(t, "redact.pair", out.Evaluations[0].RuleID)
				assert.Equal(t, "transform", out.Evaluations[0].EvaluatorKind)
				assert.False(t, out.Evaluations[0].Passed)
				assert.Contains(t, out.Evaluations[0].Reason, "tool_call.input_json")
				assert.Contains(t, out.Evaluations[0].Reason, "Cloud's rewrite was discarded")
				require.NotNil(t, out.TransformedInput)
				encoded, err := json.Marshal(out.TransformedInput)
				require.NoError(t, err)
				assert.Contains(t, string(encoded), "[REDACTED:api_key]")
				assert.NotContains(t, string(encoded), "sk-abc123")
				assert.NotContains(t, string(encoded), "hunter2")
				assert.NotContains(t, string(encoded), "https://cloud.example", "Cloud's unsafe rewrite must be discarded")
			},
		},
		{
			// The call does not run, so there is nothing to rewrite. Re-attaching
			// the local transform would break the invariant that a deny carries
			// none.
			name:          "cloud_deny_drops_the_local_transform",
			rules:         redactSecretRules,
			body:          hookSecretToolCallBody,
			cloudRespond:  `{"action":"deny","rule_id":"cloud.r1","reason":"blocked by policy"}`,
			wantAction:    agento11y.HookActionDeny,
			wantRuleID:    "cloud.r1",
			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				assert.Nil(t, out.TransformedInput)
			},
		},
		{
			name:          "local_evaluations_precede_cloud",
			rules:         passingRegexRules,
			body:          hookToolCallBody,
			cloudRespond:  `{"action":"allow","evaluations":[{"rule_id":"cloud.r1","passed":true}]}`,
			wantAction:    agento11y.HookActionAllow,
			wantCloudCall: true,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.Len(t, out.Evaluations, 2)
				assert.Equal(t, "check.reset", out.Evaluations[0].RuleID)
				assert.Equal(t, "cloud.r1", out.Evaluations[1].RuleID)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			if tc.cloudRespond != "" {
				cloud.respond = tc.cloudRespond
			}
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))
			writeGuardsFileFor(t, s, tc.rules)

			status, out := postHook(t, s, tc.body, nil)
			require.Equal(t, http.StatusOK, status)
			assert.Equal(t, tc.wantAction, out.Action)
			assert.Equal(t, tc.wantRuleID, out.RuleID)
			if tc.wantCloudCall {
				assert.Equal(t, 1, cloud.count())
			} else {
				assert.Zero(t, cloud.count(), "a locally denied call must not be relayed to Cloud")
			}
			if tc.assertMore != nil {
				tc.assertMore(t, out)
			}
		})
	}
}

func TestServer_HookEvaluate_CamelRequestGetsCanonicalTransform(t *testing.T) {
	s, _ := newTestServer(t)
	writeGuardsFileFor(t, s, redactSecretRules)

	body := `{"phase":"postflight","context":{"agentName":"pi"},"input":{"output":[{"role":"assistant",` +
		`"parts":[{"type":"tool_call","toolCall":{"id":"c1","name":"Bash","inputJSON":"{\"command\":\"curl -H sk-abc123\"}"}}]}]}}`
	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var out struct {
		TransformedInput struct {
			Output []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"output"`
		} `json:"transformed_input"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.TransformedInput.Output, 1)
	require.Len(t, out.TransformedInput.Output[0].Parts, 1)
	part := out.TransformedInput.Output[0].Parts[0]
	assert.Equal(t, "tool_call", part["kind"])
	assert.NotContains(t, part, "type")
	assert.NotContains(t, part, "toolCall")
	toolCall, ok := part["tool_call"].(map[string]any)
	require.True(t, ok)
	encoded, ok := toolCall["input_json"].(string)
	require.True(t, ok)
	assertBase64JSONEq(t, `{"command":"curl -H [REDACTED:api_key]"}`, encoded)
}

// A transform returns the whole input, so a part the decode/encode round trip
// drops is a part the host stops sending. No SDK builds a media part today (the
// server has no media kind and only Go's model.Part has the field), so this
// covers a hand-written or proto-JSON body rather than a shipped client.
func TestServer_HookEvaluate_TransformedInputKeepsMediaParts(t *testing.T) {
	s, _ := newTestServer(t)
	writeGuardsFileFor(t, s, redactSecretRules)

	body := `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"output":[{"role":"assistant","parts":[` +
		`{"kind":"text","text":"here is sk-abc123"},` +
		`{"kind":"media","media":{"kind":"image","url":"https://example.test/a.png","mime_type":"image/png","name":"a.png"}}]}]}}`
	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "sk-abc123")

	var out struct {
		TransformedInput struct {
			Output []struct {
				Parts []map[string]any `json:"parts"`
			} `json:"output"`
		} `json:"transformed_input"`
	}
	require.NoError(t, json.Unmarshal(raw, &out))
	require.Len(t, out.TransformedInput.Output, 1)
	parts := out.TransformedInput.Output[0].Parts
	require.Len(t, parts, 2, "the media part must not be dropped from the replacement input")
	assert.Contains(t, parts[0]["text"], "[REDACTED:api_key]")

	media, ok := parts[1]["media"].(map[string]any)
	require.True(t, ok, "the media payload has to survive, not only the part")
	assert.Equal(t, "https://example.test/a.png", media["url"], "the host transform leaves media fields alone")
	assert.Equal(t, "image", media["kind"])
	assert.Equal(t, "a.png", media["name"])
	assert.Equal(t, "image/png", media["mime_type"])
}

// transformed_input replaces the whole input, tools included, so a tool the
// host cannot read the schema of is a tool it calls the model with untyped
// arguments. The response contract puts that schema under input_schema_json as
// base64; every SDK parser reads that key and ignores the input_schema spelling
// the Go ToolDefinition marshals (conformance/hooks/README.md).
func TestServer_HookEvaluate_TransformedToolsCarrySchemas(t *testing.T) {
	const schema = `{"type":"object","properties":{"command":{"type":"string"}}}`
	cases := []struct {
		name string
		tool string
	}{
		{
			name: "request_schema_is_raw_json",
			tool: `{"name":"Bash","description":"runs sk-abc123","input_schema":` + schema + `}`,
		},
		{
			name: "request_schema_is_base64",
			tool: `{"name":"Bash","description":"runs sk-abc123","input_schema_json":"` +
				base64.StdEncoding.EncodeToString([]byte(schema)) + `"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			writeGuardsFileFor(t, s, redactSecretRules)

			body := `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"tools":[` + tc.tool + `],` +
				`"output":[{"role":"assistant","parts":[` +
				`{"kind":"tool_call","tool_call":{"name":"Bash","input_json":{"command":"sk-abc123"}}},` +
				`{"kind":"tool_result","tool_result":{"tool_call_id":"c1","content_json":{"value":"sk-abc123"}}}]}]}}`

			resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", body)
			defer resp.Body.Close()
			require.Equal(t, http.StatusOK, resp.StatusCode)
			raw, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			assert.NotContains(t, string(raw), "sk-abc123")

			var out struct {
				TransformedInput struct {
					Tools  []map[string]any `json:"tools"`
					Output []struct {
						Parts []struct {
							ToolCall *struct {
								InputJSON string `json:"input_json"`
							} `json:"tool_call"`
							ToolResult *struct {
								ContentJSON string `json:"content_json"`
							} `json:"tool_result"`
						} `json:"parts"`
					} `json:"output"`
				} `json:"transformed_input"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			require.Len(t, out.TransformedInput.Tools, 1)
			tool := out.TransformedInput.Tools[0]
			assert.Equal(t, "Bash", tool["name"])
			assert.NotContains(t, tool, "input_schema", "the key no hook client reads")

			encoded, ok := tool["input_schema_json"].(string)
			require.True(t, ok, "the schema has to travel as a base64 string")
			decoded, err := base64.StdEncoding.DecodeString(encoded)
			require.NoError(t, err)
			assert.JSONEq(t, schema, string(decoded))

			require.Len(t, out.TransformedInput.Output, 1)
			parts := out.TransformedInput.Output[0].Parts
			require.Len(t, parts, 2)
			require.NotNil(t, parts[0].ToolCall)
			assertBase64JSONEq(t, `{"command":"[REDACTED:api_key]"}`, parts[0].ToolCall.InputJSON)
			require.NotNil(t, parts[1].ToolResult)
			assertBase64JSONEq(t, `{"value":"[REDACTED:api_key]"}`, parts[1].ToolResult.ContentJSON)
		})
	}
}

func TestServer_HookEvaluate_FailModes(t *testing.T) {
	cases := []struct {
		name         string
		status       int
		respond      string
		closeCloud   bool
		abortMidCall bool // the agent stops waiting while Cloud stalls
		failOpen     bool
		wantAction   agento11y.HookAction
		wantFailure  string // substring of the recorded failure; "" means none
	}{
		{name: "unreachable_fail_open", closeCloud: true, failOpen: true, wantAction: agento11y.HookActionAllow, wantFailure: "POST"},
		{name: "unreachable_fail_closed", closeCloud: true, wantAction: agento11y.HookActionDeny, wantFailure: "POST"},
		{name: "status_503_fail_closed", status: http.StatusServiceUnavailable, respond: `{"error":"down"}`, wantAction: agento11y.HookActionDeny, wantFailure: "status 503"},
		{name: "bad_json_fail_open", respond: `{"action":`, failOpen: true, wantAction: agento11y.HookActionAllow, wantFailure: "decode response"},
		{name: "bad_json_fail_closed", respond: `{"action":`, wantAction: agento11y.HookActionDeny, wantFailure: "decode response"},
		// An abandoned wait is neither: the agent already applied its own fail
		// mode, so no verdict this handler writes is acted on. Counting it
		// would report an unchecked allow that never reached an agent.
		{name: "caller_abort_fail_open", abortMidCall: true, failOpen: true},
		{name: "caller_abort_fail_closed", abortMidCall: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			if tc.status != 0 {
				cloud.status = tc.status
			}
			if tc.respond != "" {
				cloud.respond = tc.respond
			}
			var opts []func(*http.Request) *http.Request
			if tc.abortMidCall {
				cloud.delay = 500 * time.Millisecond
				opts = append(opts, abortDuringCall(t))
			}
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, map[string]string{
				"AGENTO11Y_GUARDS_FAIL_OPEN": strconv.FormatBool(tc.failOpen),
			}))
			if tc.closeCloud {
				cloud.srv.Close()
			}

			code, out := postHook(t, s, hookToolCallBody, nil, opts...)
			st := s.forward.status()

			if tc.abortMidCall {
				assert.Empty(t, st.Failures, "a caller-side abort is not a Cloud delivery failure")
				assert.Zero(t, st.HookFailOpens, "nor a tool call allowed without a verdict")
				return
			}

			require.Equal(t, http.StatusOK, code, "a failed evaluation is still a verdict, not an HTTP error")
			assert.Equal(t, tc.wantAction, out.Action)

			if tc.wantAction == agento11y.HookActionDeny {
				assert.Equal(t, guard.EvaluationFailureRuleID, out.RuleID)
				assert.Contains(t, out.Reason, "could not evaluate")
				assert.Contains(t, out.Reason, `"Bash"`, "the reason names the blocked call")
				assert.NotContains(t, out.Reason, "policy blocked")
			}

			require.Len(t, st.Failures, 1)
			assert.Equal(t, forwardLabelHooks, st.Failures[0].Label)
			assert.Contains(t, st.Failures[0].Detail, tc.wantFailure)
			// A fail-open allow is byte-identical to a Cloud allow, so the
			// sticky counter is the only durable trace that no guard ran.
			if tc.failOpen {
				assert.Equal(t, 1, st.HookFailOpens)
			} else {
				assert.Zero(t, st.HookFailOpens)
			}
		})
	}
}

// TestServer_HookEvaluate_FailOpenCountSurvivesRecovery covers why the
// fail-open count is not part of the failure ring: the ring is cleared by the
// next delivery, and a guard that stopped enforcing for a while has to stay
// visible after Cloud comes back.
func TestServer_HookEvaluate_FailOpenCountSurvivesRecovery(t *testing.T) {
	cloud := newHookCloud(t)
	cloud.status = http.StatusServiceUnavailable
	s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))

	_, out := postHook(t, s, hookToolCallBody, nil)
	require.Equal(t, agento11y.HookActionAllow, out.Action)
	require.Equal(t, 1, s.forward.status().HookFailOpens)

	cloud.status = http.StatusOK
	_, out = postHook(t, s, hookToolCallBody, nil)
	require.Equal(t, agento11y.HookActionAllow, out.Action)

	st := s.forward.status()
	assert.Empty(t, st.Failures, "a delivered evaluation clears the ring")
	assert.Equal(t, 1, st.HookFailOpens, "but not the record that one call went unchecked")
}

func TestServer_HookEvaluate_RelayIsRedacted(t *testing.T) {
	cloud := newHookCloud(t)
	s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))
	writeGuardsFileFor(t, s, redactSecretRules)

	const schema = `{"type":"object","description":"sk-abc123"}`
	body := `{"phase":"postflight","context":{"agent_name":"sk-abc123","tags":{"route":"sk-abc123"}},"input":{` +
		`"tools":[{"name":"Bash","description":"runs sk-abc123","input_schema_json":"` + base64.StdEncoding.EncodeToString([]byte(schema)) + `","deferred":true}],` +
		`"output":[{"role":"assistant","parts":[` +
		`{"kind":"text","text":"use sk-abc123"},` +
		`{"kind":"thinking","thinking":"keep sk-abc123 for the host"},` +
		`{"kind":"media","media":{"kind":"image","url":"https://example.test/sk-abc123.png","mime_type":"image/png","name":"sk-abc123.png"}}]}]}}`

	status, out := postHook(t, s, body, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, cloud.count())

	_, sent, _ := cloud.lastCall()
	var relay struct {
		Context agento11y.HookContext `json:"context"`
		Input   struct {
			Output []agento11y.Message `json:"output"`
			Tools  []struct {
				InputSchemaJSON string `json:"input_schema_json"`
				Deferred        bool   `json:"deferred"`
			} `json:"tools"`
		} `json:"input"`
	}
	require.NoError(t, json.Unmarshal([]byte(sent), &relay))
	assert.Equal(t, "sk-abc123", relay.Context.AgentName, "routing context must not be rewritten")
	assert.Equal(t, "sk-abc123", relay.Context.Tags["route"], "routing tags must not be rewritten")
	require.Len(t, relay.Input.Output, 1)
	require.Len(t, relay.Input.Output[0].Parts, 3)
	parts := relay.Input.Output[0].Parts
	assert.Equal(t, "use [REDACTED:api_key]", parts[0].Text)
	assert.Equal(t, "keep [REDACTED:api_key] for the host", parts[1].Thinking)
	require.NotNil(t, parts[2].Media)
	assert.Equal(t, "https://example.test/[REDACTED:api_key].png", parts[2].Media.URL)
	assert.Equal(t, "[REDACTED:api_key].png", parts[2].Media.Name)
	assert.Equal(t, "image/png", parts[2].Media.MIMEType)
	require.Len(t, relay.Input.Tools, 1)
	assert.True(t, relay.Input.Tools[0].Deferred)
	assertBase64JSONEq(t, `{"type":"object","description":"[REDACTED:api_key]"}`, relay.Input.Tools[0].InputSchemaJSON)

	require.NotNil(t, out.TransformedInput)
	hostParts := out.TransformedInput.Output[0].Parts
	assert.Equal(t, "use [REDACTED:api_key]", hostParts[0].Text)
	assert.Equal(t, "keep sk-abc123 for the host", hostParts[1].Thinking)
	require.NotNil(t, hostParts[2].Media)
	assert.Equal(t, "https://example.test/sk-abc123.png", hostParts[2].Media.URL)
	assert.Equal(t, "sk-abc123.png", hostParts[2].Media.Name)
}

func TestServer_HookEvaluate_RelayPreparationFailureUsesFailMode(t *testing.T) {
	cases := []struct {
		name       string
		failOpen   bool
		textPart   string
		wantAction agento11y.HookAction
	}{
		{
			name:       "fail_open_when_only_JSON_rewrite_is_dropped",
			failOpen:   true,
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "fail_closed_when_another_field_was_redacted",
			textPart:   `{"kind":"text","text":"also sk-abc123"},`,
			wantAction: agento11y.HookActionDeny,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, map[string]string{
				"AGENTO11Y_GUARDS_FAIL_OPEN": strconv.FormatBool(tc.failOpen),
			}))
			writeGuardsFileFor(t, s, redactPairRules)

			body := `{"phase":"postflight","input":{"output":[{"role":"assistant","parts":[` + tc.textPart +
				`{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":` +
				`{"password":"hunter2","command":"curl -H sk-abc123"}}}]}]}}`
			status, out := postHook(t, s, body, nil)

			require.Equal(t, http.StatusOK, status)
			assert.Equal(t, tc.wantAction, out.Action)
			assert.Zero(t, cloud.count(), "a relay with an unresolved redaction must stay on this machine")
			st := s.forward.status()
			require.Len(t, st.Failures, 1)
			assert.Contains(t, st.Failures[0].Detail, "tool_call.input_json")
			assert.NotContains(t, st.Failures[0].Detail, "hunter2")
			assert.NotContains(t, st.Failures[0].Detail, "sk-abc123")
			if tc.failOpen {
				assert.Equal(t, 1, st.HookFailOpens)
				return
			}
			assert.Zero(t, st.HookFailOpens)
			assert.Equal(t, guard.EvaluationFailureRuleID, out.RuleID)
			assert.Contains(t, out.Reason, "could not evaluate")
		})
	}
}

// A rule that rewrote nothing must leave the relay alone, so the common case
// keeps the agent's bytes on the wire.
func TestServer_HookEvaluate_RelayKeepsBytesWhenNothingMatched(t *testing.T) {
	cloud := newHookCloud(t)
	s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))
	writeGuardsFileFor(t, s, redactSecretRules)

	body := `{"phase":"postflight","context":{"agent_name":"claude-code"},"input":{"output":[{"role":"assistant",` +
		`"parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"ls"}}}]}]}}`
	status, _ := postHook(t, s, body, nil)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, cloud.count())
	_, sent, _ := cloud.lastCall()
	assert.Equal(t, body, sent)
}

// TestServer_HookEvaluate_RelayShape covers what the daemon puts on the wire
// when no local rule rewrote anything: the received bytes unchanged, the loop
// marker, Cloud auth, and the budget derived from the calling agent's own
// deadline.
func TestServer_HookEvaluate_RelayShape(t *testing.T) {
	cloud := newHookCloud(t)
	cloud.respond = `{"action":"allow"}`
	s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))

	// A preflight request carrying a field this daemon's SDK version does not
	// know about. Relaying the bytes is what keeps that field intact.
	body := `{"phase":"preflight","context":{"agent_name":"pi"},"future_field":{"trace_id":"t1"}}`
	status, out := postHook(t, s, body, map[string]string{legacyHookTimeoutHeader: "5000"})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)

	require.Equal(t, 1, cloud.count())
	_, sent, headers := cloud.lastCall()
	assert.Equal(t, body, sent)
	assert.NotEmpty(t, headers.Get(ForwardMarkerHeader))
	assert.Equal(t, "t", headers.Get(wire.TenantHeaderName))
	assert.True(t, strings.HasPrefix(headers.Get("Authorization"), "Basic "))
	// The legacy spelling was propagated under the branded one, minus the
	// margin that keeps the Cloud call ahead of the agent's own deadline.
	assert.Equal(t, "4750", headers.Get(hookTimeoutHeader))
}

// TestServer_HookEvaluate_DoesNotChainRelayedRequest covers the loop guard: a
// daemon whose ENDPOINT was hand-set to another daemon must answer a relayed
// request from its own verdict.
func TestServer_HookEvaluate_DoesNotChainRelayedRequest(t *testing.T) {
	cloud := newHookCloud(t)
	cloud.respond = `{"action":"deny","rule_id":"r1"}`
	s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))

	status, out := postHook(t, s, hookToolCallBody, map[string]string{ForwardMarkerHeader: "1"})
	require.Equal(t, http.StatusOK, status)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)
	assert.Zero(t, cloud.count())
}

// TestServer_HookEvaluate_RejectsBadRequestsBeforeChaining covers the
// validation the chaining must not weaken: neither a malformed body nor an
// oversized one may reach Cloud.
func TestServer_HookEvaluate_RejectsBadRequestsBeforeChaining(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "invalid_json", body: `{"phase":`, wantStatus: http.StatusBadRequest},
		{name: "oversized_body", body: `{"phase":"postflight","pad":"` + strings.Repeat("x", maxHookBodyBytes) + `"}`, wantStatus: http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cloud := newHookCloud(t)
			s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))

			status, _ := postHook(t, s, tc.body, nil)
			assert.Equal(t, tc.wantStatus, status)
			assert.Zero(t, cloud.count(), "a request the daemon rejects never reaches Cloud")
		})
	}
}

// TestServer_HookEvaluate_ConfigChangeAppliesWithoutRestart covers the
// no-restart contract for the guard knobs: they are resolved through the
// file-first reader, not envconfig.ResolveGuards, so flipping guards in
// config.env reaches a running daemon.
func TestServer_HookEvaluate_ConfigChangeAppliesWithoutRestart(t *testing.T) {
	cloud := newHookCloud(t)
	cloud.respond = `{"action":"deny","rule_id":"r1","reason":"nope"}`
	env := hookEnv(cloud.srv.URL, map[string]string{"AGENTO11Y_GUARDS_ENABLED": "false"})
	s, _ := newForwardingTestServer(t, cloud.srv, env)

	_, out := postHook(t, s, hookToolCallBody, nil)
	require.Equal(t, agento11y.HookActionAllow, out.Action)
	require.Zero(t, cloud.count())

	env["AGENTO11Y_GUARDS_ENABLED"] = "true"
	var b []byte
	for k, v := range env {
		b = fmt.Appendf(b, "%s=%s\n", k, v)
	}
	require.NoError(t, os.WriteFile(s.configPath, b, 0o600))
	// Force a distinct mtime so the loader's size+mtime cache notices an edit
	// that landed inside the filesystem's timestamp granularity.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(s.configPath, future, future))

	_, out = postHook(t, s, hookToolCallBody, nil)
	assert.Equal(t, agento11y.HookActionDeny, out.Action)
	assert.Equal(t, 1, cloud.count())
}

// TestServer_APISearch covers the /api/v1/search endpoint: empty query,
// ranked hits with the design shape, unknown query, and the default
// limit. Semantic search is not wired up, so the mode is always "fts".
func TestServer_APISearch(t *testing.T) {
	srv, dir := newTestServer(t)
	storage, err := NewStorage(dir)
	require.NoError(t, err)

	writeGenWithMessages(t, storage, "conv-A", "g1",
		[]agento11y.Message{textMsg(agento11y.RoleUser, "hit the rate limit")},
		[]agento11y.Message{textMsg(agento11y.RoleAssistant, "backoff once the rate limit clears")},
		"2026-05-21T10:00:00Z")
	writeGenWithMessages(t, storage, "conv-B", "g2",
		nil,
		[]agento11y.Message{textMsg(agento11y.RoleAssistant, "nothing useful here")},
		"2026-05-21T11:00:00Z")

	t.Run("empty query returns empty hits with fts mode", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet, "/api/v1/search?q=", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var body struct {
			Hits []SearchHit `json:"hits"`
			Mode string      `json:"mode"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Empty(t, body.Hits)
		assert.Equal(t, "fts", body.Mode)
	})

	t.Run("populated store returns ranked hits with the design shape", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet, "/api/v1/search?q=rate+limit", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var body struct {
			Hits []SearchHit `json:"hits"`
			Mode string      `json:"mode"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.Equal(t, "fts", body.Mode)
		require.Len(t, body.Hits, 1, "only conv-A contains both terms")
		hit := body.Hits[0]
		assert.Equal(t, "conv-A", hit.ID)
		assert.Greater(t, hit.MatchCount, 0)
		assert.NotEmpty(t, hit.Snippet)
		assert.Contains(t, strings.ToLower(hit.Snippet), "limit")
		assert.Equal(t, "g1", hit.GenerationID)
		assert.NotEmpty(t, hit.LastActivity)
	})

	t.Run("unknown query returns empty hits, not error", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet, "/api/v1/search?q=zzz-impossible", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), `"hits":[]`)
	})

	t.Run("limit=0 falls back to the default cap", func(t *testing.T) {
		req := newLocalRequest(http.MethodGet, "/api/v1/search?q=rate&limit=0", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		var body struct {
			Hits []SearchHit `json:"hits"`
		}
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
		assert.NotEmpty(t, body.Hits)
	})
}

// TestServer_APISearchCapabilities checks that the capabilities endpoint
// reports full-text search available and semantic search unavailable in
// this build.
func TestServer_APISearchCapabilities(t *testing.T) {
	srv, _ := newTestServer(t)
	req := newLocalRequest(http.MethodGet, "/api/v1/search/capabilities", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	var caps SearchCapabilities
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &caps))
	assert.True(t, caps.FullText, "full-text search is available")
	assert.False(t, caps.Semantic, "semantic search is not wired up in this build")
}

// TestServer_DevAsset_EnvPrecedence checks that the preferred
// AGENTO11Y_LOCAL_WEB_DIR wins over the legacy SIGIL_LOCAL_WEB_DIR, and
// that the legacy spelling still works on its own.
//
// The viewer fixtures are statements rather than comments: the bundle is
// compiled, and esbuild strips comments, so a fixture written as a comment
// would compile to the same bytes whichever directory it came from, and the
// marker each subtest looks for would never be in the response.
func TestServer_DevAsset_EnvPrecedence(t *testing.T) {
	srv, _ := newTestServer(t)

	preferred := writeDevViewer(t, "preferred")
	legacy := writeDevViewer(t, "legacy")

	fetchBundle := func() string {
		req := newLocalRequest(http.MethodGet, "/assets/app.js", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}

	t.Run("preferred wins over legacy", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", preferred)
		t.Setenv("SIGIL_LOCAL_WEB_DIR", legacy)
		bundle := fetchBundle()
		assert.Contains(t, bundle, `"preferred"`)
		assert.NotContains(t, bundle, `"legacy"`)
	})

	t.Run("legacy is used as a fallback", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", "")
		t.Setenv("SIGIL_LOCAL_WEB_DIR", legacy)
		assert.Contains(t, fetchBundle(), `"legacy"`)
	})

	// An edit reaches the browser with no Go rebuild, which is the whole point
	// of the variable: the daemon compiles the viewer, so without a rebuild per
	// request the browser would keep getting the bundle built at startup.
	//
	// This writes into a directory of its own. Editing one of the fixtures above
	// would make the two subtests before it depend on running first.
	t.Run("an edited module is rebuilt per request", func(t *testing.T) {
		edited := writeDevViewer(t, "before")
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", edited)
		require.Contains(t, fetchBundle(), `"before"`)

		require.NoError(t, os.WriteFile(filepath.Join(edited, "src", viewerEntry),
			[]byte("globalThis.devMarker = \"edited\";\n"), 0o600))
		assert.Contains(t, fetchBundle(), `"edited"`)
	})

	t.Run("a broken module answers 500 and names the file", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", writeDevViewerSource(t, "const broken = (;\n"))
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/assets/app.js", nil))
		require.Equal(t, http.StatusInternalServerError, rr.Code)
		assert.Contains(t, rr.Body.String(), viewerEntry)
	})

	// Every way of pointing the variable at the wrong place ends in the same
	// esbuild message about a missing entry point, so the response has to say
	// which variable it read and where it looked.
	t.Run("a directory with no src answers 500 and names the variable", func(t *testing.T) {
		empty := t.TempDir()
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", empty)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/assets/app.js", nil))
		require.Equal(t, http.StatusInternalServerError, rr.Code)
		assert.Contains(t, rr.Body.String(), "AGENTO11Y_LOCAL_WEB_DIR")
		assert.Contains(t, rr.Body.String(), filepath.Join(empty, "src"))
	})

	t.Run("missing index nonce placeholder is logged", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", preferred)
		require.NoError(t, os.WriteFile(filepath.Join(preferred, "index.html"), []byte("<!doctype html>"), 0o600))
		var logs bytes.Buffer
		srv.logger.SetOutput(&logs)

		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, newLocalRequest(http.MethodGet, "/", nil))
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, logs.String(), noncePlaceholder)
	})
}

// writeDevViewer lays out a LOCAL_WEB_DIR whose bundle carries marker as a
// string literal, and returns the directory.
func writeDevViewer(t *testing.T, marker string) string {
	t.Helper()
	return writeDevViewerSource(t, "globalThis.devMarker = \""+marker+"\";\n")
}

func writeDevViewerSource(t *testing.T, source string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "src"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src", viewerEntry), []byte(source), 0o600))
	return dir
}

// TestServer_Forwarding_ToggleOffStopsForwarding covers the config.env write
// path end to end for the case the daemon's own environment disagrees: `local
// serve` materializes config.env into its process env at boot, so a deleted key
// would leave forwarding on. Saving Off must write an explicit false and stop
// the relay without a daemon restart.
func TestServer_Forwarding_ToggleOffStopsForwarding(t *testing.T) {
	hits := make(chan struct{}, 4)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, _ := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})
	// What dotenv.ApplyEnv(nil) does at daemon start: both spellings of every
	// config.env key land in the daemon's own environment.
	t.Setenv(envconfig.PreferredKey("LOCAL_FORWARD"), "true")
	t.Setenv(envconfig.LegacyKey("LOCAL_FORWARD"), "true")
	require.True(t, s.forward.load().enabled)

	resp := putConfig(t, s, Settings{
		Endpoint: cloud.URL,
		TenantID: "t",
		Guards:   guardsOff,
	})
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var saved configResponse
	decodeJSON(t, resp.Body, &saved)
	assert.False(t, saved.Settings.LocalForward)
	assert.Equal(t, forwardStatus{Mode: forwardModeOff}, saved.ForwardStatus)

	onDisk, err := os.ReadFile(s.configPath)
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "AGENTO11Y_LOCAL_FORWARD=false")

	postDiscard(t, s, "/api/v1/generations:export", "application/json",
		`{"generations":[{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m"}}]}`)
	s.forward.wait()
	assert.Empty(t, hits, "forwarding must stop as soon as config.env says false")
}

// TestServer_Forwarding_DoesNotRelayForwardedPayload covers the loop guard: a
// payload that already carries another daemon's forward marker is stored but
// not forwarded again, so two daemons pointed at each other (or one pointed
// at itself) exchange one copy instead of looping.
func TestServer_Forwarding_DoesNotRelayForwardedPayload(t *testing.T) {
	hits := make(chan struct{}, 4)
	cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	s, dir := newForwardingTestServer(t, cloud, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL,
	})

	for _, path := range []string{"/api/v1/generations:export", "/otlp/v1/traces"} {
		req := newLocalRequest(http.MethodPost, path,
			strings.NewReader(`{"generations":[{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m"}}]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(ForwardMarkerHeader, "1")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
	}
	s.forward.wait()

	// Stored locally, never relayed.
	assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl")), 1)
	assert.Empty(t, hits)
}

// The guards-management endpoints (GET/PUT /api/v1/guards, POST
// /api/v1/guards:test). The engine they call is unit-tested in
// internal/guardeval; what follows covers the HTTP layer over it.

// newGuardsServer builds a server with explicit guards and config.env paths the
// guards-management tests can inspect and rewrite. Both GUARDS_ENABLED
// spellings are cleared from the OS env so the enabled flag resolves from
// config.env, not the developer's shell.
func newGuardsServer(t *testing.T) (s *Server, guardsPath, configPath string) {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	require.NoError(t, err)
	guardsPath = filepath.Join(t.TempDir(), guardeval.ConfigFile)
	configPath = filepath.Join(t.TempDir(), "config.env")
	return newServer(storage, nil, configPath, guardsPath), guardsPath, configPath
}

// newTestServerWithGuards builds a server whose on-disk ruleset is rulesJSON.
func newTestServerWithGuards(t *testing.T, rulesJSON string) *Server {
	t.Helper()
	s, guardsPath, _ := newGuardsServer(t)
	require.NoError(t, os.WriteFile(guardsPath, []byte(rulesJSON), 0o600))
	return s
}

func doReq(t *testing.T, s *Server, method, path, body string) *http.Response {
	t.Helper()
	req := newLocalRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr.Result()
}

func TestNewServer_DerivesGuardsPathFromConfigDir(t *testing.T) {
	dir := t.TempDir()
	storage, err := NewStorage(filepath.Join(dir, "local"))
	require.NoError(t, err)

	configPath := filepath.Join(dir, "config.env")
	s := NewServer(storage, nil, configPath)
	assert.Equal(t, filepath.Join(dir, guardeval.ConfigFile), s.guards.RulesPath)

	// An empty config path leaves no rules file to read, which the engine takes
	// as an empty ruleset rather than a fault.
	s = NewServer(storage, nil, "")
	assert.Empty(t, s.guards.RulesPath)
	status := guardeval.NewEngine(s.guards).Status()
	assert.Zero(t, status.Enforcing)
	assert.Empty(t, status.Errors)
}

func TestServer_Guards_UncompilableRuleSkippedSiblingsEnforce(t *testing.T) {
	s := newTestServerWithGuards(t, `
[[rules]]
rule_id = "broken"
phase = "postflight"
transform.patterns = [{ regex = "(" }]

[[rules]]
rule_id = "block.rm"
phase = "postflight"
action_on_fail = "deny"
tool_filter.blocked_names = ["Bash(*rm -rf*)"]
`)

	evalBody := `{"phase":"postflight","context":{},"input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call",` +
		`"tool_call":{"id":"t1","name":"Bash","input_json":{"command":"rm -rf /tmp/x"}}}]}]}}`
	er := post(t, s, "/api/v1/hooks:evaluate", "application/json", evalBody)
	defer er.Body.Close()
	var verdict agento11y.HookEvaluateResponse
	decodeJSON(t, er.Body, &verdict)
	assert.Equal(t, agento11y.HookActionDeny, verdict.Action)
	assert.Equal(t, "block.rm", verdict.RuleID)

	resp := doReq(t, s, http.MethodGet, "/api/v1/config", "")
	defer resp.Body.Close()
	var out configResponse
	decodeJSON(t, resp.Body, &out)
	assert.Contains(t, out.LocalGuards.Error, "broken", "the skipped rule is reported, not hidden")
	assert.Equal(t, 1, out.LocalGuards.Rules, "its sibling still enforces")
}

// A ruleset the daemon cannot parse enforces nothing rather than blocking every
// tool call.
func TestServer_Guards_MalformedFileAllows(t *testing.T) {
	s := newTestServerWithGuards(t, "[[rules]\nrule_id = ")
	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json",
		`{"phase":"postflight","context":{},"input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call",`+
			`"tool_call":{"id":"t1","name":"Bash","input_json":{"command":"rm -rf /tmp/x"}}}]}]}}`)
	defer resp.Body.Close()
	var verdict agento11y.HookEvaluateResponse
	decodeJSON(t, resp.Body, &verdict)
	assert.Equal(t, agento11y.HookActionAllow, verdict.Action)
}

// The launcher banner reads its posture from GET /api/v1/config, so the local
// rule count has to travel with the forwarding status rather than only through
// the guards endpoint.
func TestServer_Config_ReportsLocalGuardCount(t *testing.T) {
	s := newTestServerWithGuards(t, `
[[rules]]
rule_id = "block.rm"
phase = "postflight"
tool_filter.blocked_names = ["Bash(*rm -rf*)"]

[[rules]]
rule_id = "off"
enabled = false
tool_filter.blocked_names = ["x"]
`)
	resp := doReq(t, s, http.MethodGet, "/api/v1/config", "")
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out configResponse
	decodeJSON(t, resp.Body, &out)

	assert.Equal(t, 1, out.LocalGuards.Rules, "the disabled rule is not counted")
	assert.Contains(t, out.LocalGuards.Path, guardeval.ConfigFile)
	assert.Empty(t, out.LocalGuards.Error)
	assert.False(t, out.LocalGuards.Enabled, "GUARDS_ENABLED is off, so no host agent asks")

	// The shared posture is embedded, so its keys sit next to "enabled" rather
	// than under an object of their own. Decoding into the same struct cannot
	// see that, so check the encoded shape.
	encoded, err := json.Marshal(out.LocalGuards)
	require.NoError(t, err)
	var keys map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(encoded, &keys))
	assert.Contains(t, keys, "rules")
	assert.Contains(t, keys, "path")
	assert.Contains(t, keys, "enabled")
	assert.NotContains(t, keys, "Posture")
}
