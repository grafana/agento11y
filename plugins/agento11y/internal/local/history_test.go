package local

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHistoryConversationTitleKeyMatches pins the one duplicated string in the
// import path. history declares the conversation-title metadata key for its
// export path and cannot import this package, because this package imports it.
// If the two spellings ever drift, an imported conversation loses its title in
// the viewer with nothing failing.
func TestHistoryConversationTitleKeyMatches(t *testing.T) {
	assert.Equal(t, metadataKeyConversationTitle, history.MetaConversationTitle)
}

// TestHistoryForwardMarkerHeaderMatches pins the other duplicated string. The
// importer sets the marker, this package reads it, and a drift would send every
// imported turn to Grafana Cloud with nothing failing.
func TestHistoryForwardMarkerHeaderMatches(t *testing.T) {
	assert.Equal(t, ForwardMarkerHeader, history.ForwardMarkerHeader)
}

// isolateHistoryState points XDG_STATE_HOME at a temp dir so ledger and prompt
// writes never touch the developer's real state root, and clears the agent
// roots so discovery sees only what a test writes.
func isolateHistoryState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(dir, "claude"))
	return dir
}

// writeHistoryTranscript writes a Claude transcript with turns assistant turns,
// under the isolated CLAUDE_CONFIG_DIR, and returns its path.
func writeHistoryTranscript(t *testing.T, sessionID string, turns int, lastActivity time.Time) string {
	t.Helper()
	line := func(fields map[string]any) string {
		data, err := json.Marshal(fields)
		require.NoError(t, err)
		return string(data) + "\n"
	}
	var body strings.Builder
	start := lastActivity.Add(-time.Duration(turns) * time.Minute)
	for i := range turns {
		ts := start.Add(time.Duration(i) * time.Minute)
		body.WriteString(line(map[string]any{
			"type": "user", "sessionId": sessionID, "cwd": "/work/repo",
			"timestamp": ts.Format(time.RFC3339),
			"message":   map[string]any{"role": "user", "content": fmt.Sprintf("question %d", i)},
		}))
		body.WriteString(line(map[string]any{
			"type": "assistant", "sessionId": sessionID, "cwd": "/work/repo",
			"timestamp": ts.Add(30 * time.Second).Format(time.RFC3339),
			"requestId": fmt.Sprintf("req-%d", i),
			"message": map[string]any{
				"model": "claude-sonnet-4-20250514", "stop_reason": "end_turn",
				"usage":   map[string]any{"input_tokens": 10, "output_tokens": 5},
				"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("answer %d", i)}},
			},
		}))
	}

	path := filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects", "-work-repo", sessionID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(body.String()), 0o600))
	require.NoError(t, os.Chtimes(path, lastActivity, lastActivity))
	return path
}

// newHistoryServer boots a daemon over a real listener with its own address
// wired in. An import exports through that address.
func newHistoryServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	t.Cleanup(srv.Close)
	srv.SetLocalEndpoint(ts.URL)
	return srv, ts
}

func getJSON(t *testing.T, s *Server, path string, dst any) int {
	t.Helper()
	req := newLocalRequest(http.MethodGet, path, nil)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if dst != nil && rr.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), dst))
	}
	return rr.Code
}

func TestHistoryAgentsEndpointComesFromTheRegistry(t *testing.T) {
	srv, _ := newTestServer(t)
	var got struct {
		Agents []struct {
			ID          string   `json:"id"`
			DisplayName string   `json:"display_name"`
			Aliases     []string `json:"aliases"`
		} `json:"agents"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/agents", &got))

	specs := history.Specs()
	require.Len(t, got.Agents, len(specs))
	require.NotEmpty(t, specs, "no importers registered; this test proves nothing")
	for i, spec := range specs {
		assert.Equal(t, string(spec.ID), got.Agents[i].ID)
		assert.Equal(t, spec.DisplayName, got.Agents[i].DisplayName)
		assert.Equal(t, spec.Aliases, got.Agents[i].Aliases)
	}
}

func TestHistoryOfferAndDismiss(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newTestServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	type offerResponse struct {
		Offers []historyOffer `json:"offers"`
	}
	claudeOffer := func() historyOffer {
		t.Helper()
		var got offerResponse
		require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/offer", &got))
		for _, o := range got.Offers {
			if o.Agent == string(history.AgentClaudeCode) {
				return o
			}
		}
		t.Fatal("no claude-code offer in the response")
		return historyOffer{}
	}

	offer := claudeOffer()
	assert.True(t, offer.Show, "an offer with discoverable history should show")
	assert.Equal(t, 1, offer.Sessions)
	assert.Positive(t, offer.Turns)

	resp := post(t, srv, "/api/v1/history/offer:dismiss", "application/json", `{"agent":"claude-code"}`)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp.Body.Close()

	assert.False(t, claudeOffer().Show, "a dismissed offer must not come back")
}

func TestHistoryOfferStopsAfterACompletedImport(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	runID := startImport(t, srv, `{"agent":"claude-code"}`)
	run := waitForTerminalRun(t, srv, runID)
	require.Equal(t, importStatusCompleted, run.Status)

	var got struct {
		Offers []historyOffer `json:"offers"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/offer", &got))
	for _, o := range got.Offers {
		assert.False(t, o.Show, "agent %s still offers an import after one completed", o.Agent)
	}
}

// TestHistoryImportFailsWhenNothingLands covers a run whose every turn failed.
// Reporting it as completed would hide the failure and answer the one-time
// offer, so the banner would never come back.
func TestHistoryImportFailsWhenNothingLands(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	// Point the import at an endpoint that refuses every export.
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusInternalServerError)
	}))
	defer broken.Close()
	srv.SetLocalEndpoint(broken.URL)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	run := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	require.Equal(t, importStatusFailed, run.Status)
	assert.Equal(t, 2, run.Failed)
	assert.Zero(t, run.Imported)
	assert.NotEmpty(t, run.Error)

	var got struct {
		Offers []historyOffer `json:"offers"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/offer", &got))
	shown := false
	for _, o := range got.Offers {
		if o.Agent == "claude-code" {
			shown = o.Show
		}
	}
	assert.True(t, shown, "a failed import answered the offer, so the banner never comes back")
}

// TestHistoryImportKeepsTheOfferWhenNothingIsSelected covers a run whose
// source paths discovery no longer finds. It imports nothing, so it answers
// nothing, and the one-time offer has to survive it.
func TestHistoryImportKeepsTheOfferWhenNothingIsSelected(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	body := `{"agent":"claude-code","source_paths":["/elsewhere/sess-gone.jsonl"]}`
	run := waitForTerminalRun(t, srv, startImport(t, srv, body))
	require.Equal(t, importStatusCompleted, run.Status)
	assert.Zero(t, run.Selected)
	assert.Zero(t, run.Imported)

	var got struct {
		Offers []historyOffer `json:"offers"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/offer", &got))
	shown := false
	for _, o := range got.Offers {
		if o.Agent == "claude-code" {
			shown = o.Show
		}
	}
	assert.True(t, shown, "an import that selected nothing answered the offer, so the banner never comes back")
}

func TestHistoryPlanIsMetadataOnly(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newTestServer(t)
	writeHistoryTranscript(t, "sess-1", 3, time.Now().Add(-2*time.Hour))

	var plan struct {
		Agent    string               `json:"agent"`
		Since    string               `json:"since"`
		Sessions []historySessionJSON `json:"sessions"`
		Skipped  []map[string]any     `json:"skipped"`
		Warnings []string             `json:"warnings"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/plan?agent=claude-code", &plan))
	require.Len(t, plan.Sessions, 1)
	sess := plan.Sessions[0]
	assert.Equal(t, "sess-1", sess.SessionID)
	assert.Equal(t, "sess-1", sess.Title, "the title must be the session ID, never prompt text")
	assert.Equal(t, "/work/repo", sess.Workspace)
	assert.Equal(t, 3, sess.TurnCount)

	// Nothing in the response may echo transcript content.
	req := newLocalRequest(http.MethodGet, "/api/v1/history/plan?agent=claude-code", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	for _, forbidden := range []string{"question 0", "answer 0"} {
		assert.NotContains(t, rr.Body.String(), forbidden)
	}
}

// TestHistoryPlanDefaultsToNinetyDays pins the lower bound the viewer opens
// with, which is also what keeps a first import from filling a linear-scan
// store with every turn ever recorded.
func TestHistoryPlanDefaultsToNinetyDays(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newTestServer(t)
	now := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	srv.now = func() time.Time { return now }
	writeHistoryTranscript(t, "sess-old", 2, now.Add(-120*24*time.Hour))
	writeHistoryTranscript(t, "sess-new", 2, now.Add(-2*24*time.Hour))

	var plan struct {
		Since    string               `json:"since"`
		Sessions []historySessionJSON `json:"sessions"`
		Skipped  []map[string]any     `json:"skipped"`
	}
	require.Equal(t, http.StatusOK, getJSON(t, srv, "/api/v1/history/plan?agent=claude-code", &plan))
	assert.Equal(t, now.Add(-history.DefaultSinceWindow).Format(time.RFC3339), plan.Since)
	require.Len(t, plan.Sessions, 1)
	assert.Equal(t, "sess-new", plan.Sessions[0].SessionID)
	require.Len(t, plan.Skipped, 1)
	assert.Equal(t, string(history.SkipOutOfRange), plan.Skipped[0]["reason"])

	// An explicit lower bound overrides the default.
	plan.Sessions = nil
	path := "/api/v1/history/plan?agent=claude-code&since=" + now.Add(-200*24*time.Hour).Format(time.RFC3339)
	require.Equal(t, http.StatusOK, getJSON(t, srv, path, &plan))
	assert.Len(t, plan.Sessions, 2)
}

func TestHistoryPlanRejectsBadInput(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newTestServer(t)
	tests := []struct {
		name string
		path string
	}{
		{name: "no agent", path: "/api/v1/history/plan"},
		{name: "unknown agent", path: "/api/v1/history/plan?agent=aider"},
		{name: "bad since", path: "/api/v1/history/plan?agent=claude-code&since=yesterday"},
		{name: "bad max_sessions", path: "/api/v1/history/plan?agent=claude-code&max_sessions=-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, http.StatusBadRequest, getJSON(t, srv, tt.path, nil))
		})
	}
}

func TestHistoryImportRunsAsynchronously(t *testing.T) {
	type sessionFixture struct {
		id           string
		turns        int
		lastActivity time.Duration
	}
	tests := []struct {
		name        string
		since       time.Duration
		sessions    []sessionFixture
		want        map[string]int
		notImported []string
	}{
		{
			name: "default bound",
			sessions: []sessionFixture{
				{id: "sess-before-default", turns: 2, lastActivity: -120 * 24 * time.Hour},
				{id: "sess-default", turns: 3, lastActivity: -2 * time.Hour},
			},
			want:        map[string]int{"sess-default": 3},
			notImported: []string{"sess-before-default"},
		},
		{
			name:  "explicit bound",
			since: -24 * time.Hour,
			sessions: []sessionFixture{
				{id: "sess-before-bound", turns: 2, lastActivity: -25 * time.Hour},
				// The first turn predates the bound; the last follows it.
				{id: "sess-overlaps-bound", turns: 3, lastActivity: -24*time.Hour + time.Minute},
				{id: "sess-after-bound", turns: 2, lastActivity: -2 * time.Hour},
			},
			want: map[string]int{
				"sess-overlaps-bound": 3,
				"sess-after-bound":    2,
			},
			notImported: []string{"sess-before-bound"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolateHistoryState(t)
			srv, _ := newHistoryServer(t)
			now := time.Now().UTC().Truncate(time.Second)
			srv.now = func() time.Time { return now }
			for _, session := range tt.sessions {
				writeHistoryTranscript(t, session.id, session.turns, now.Add(session.lastActivity))
			}

			body := `{"agent":"claude-code"}`
			if tt.since != 0 {
				body = fmt.Sprintf(`{"agent":"claude-code","since":%q}`, now.Add(tt.since).Format(time.RFC3339))
			}
			run := waitForTerminalRun(t, srv, startImport(t, srv, body))
			assert.Equal(t, importStatusCompleted, run.Status)
			assert.Equal(t, len(tt.want), run.Selected)
			assert.Zero(t, run.Failed)
			assert.NotEmpty(t, run.FinishedAt)

			wantTurns := 0
			for sessionID, turns := range tt.want {
				wantTurns += turns
				detail, err := srv.storage.ConversationDetail(sessionID)
				require.NoError(t, err)
				require.NotNil(t, detail)
				assert.Len(t, detail.Generations, turns)
			}
			assert.Equal(t, wantTurns, run.Imported)
			for _, sessionID := range tt.notImported {
				detail, err := srv.storage.ConversationDetail(sessionID)
				require.NoError(t, err)
				assert.Nil(t, detail)
			}
		})
	}
}

// TestHistoryImportIsIdempotent covers the ledger: a second run of the same
// selection re-exports nothing.
func TestHistoryImportIsIdempotent(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	first := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	require.Equal(t, 2, first.Imported)

	second := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	assert.Equal(t, 0, second.Imported)
	assert.Equal(t, 2, second.Skipped)

	detail, err := srv.storage.ConversationDetail("sess-1")
	require.NoError(t, err)
	assert.Len(t, detail.Generations, 2, "a skipped rerun must not add entries")
}

func TestHistoryImportRejectsASecondRun(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	// Occupy the slot with a run that will not finish on its own.
	blocked := ImportRun{RunID: "blocking-run", Agent: "claude-code", Status: importStatusRunning}
	require.True(t, srv.startImportRun(blocked, func() {}))

	resp := post(t, srv, "/api/v1/history:import", "application/json", `{"agent":"claude-code"}`)
	assert.Equal(t, http.StatusConflict, resp.StatusCode)
	resp.Body.Close()

	state, ok := srv.importRunState("blocking-run")
	require.True(t, ok, "the active run must survive a rejected second import")
	assert.Equal(t, importStatusRunning, state.Status)
}

// TestHistoryImportMatchesSelectionAgainstFreshDiscovery covers a stale plan:
// the daemon imports the sessions discovery still finds and counts the rest as
// missing rather than opening a path the request named.
func TestHistoryImportMatchesSelectionAgainstFreshDiscovery(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	kept := writeHistoryTranscript(t, "sess-kept", 2, time.Now().Add(-2*time.Hour))
	writeHistoryTranscript(t, "sess-gone", 2, time.Now().Add(-3*time.Hour))

	body := fmt.Sprintf(`{"agent":"claude-code","source_paths":[%q,%q]}`, kept, "/elsewhere/sess-gone.jsonl")
	run := waitForTerminalRun(t, srv, startImport(t, srv, body))
	assert.Equal(t, importStatusCompleted, run.Status)
	assert.Equal(t, 1, run.Selected)
	assert.Equal(t, 1, run.Missing, "a source path discovery no longer finds must be reported, not read")
	assert.Equal(t, 2, run.Imported)

	gone, err := srv.storage.ConversationDetail("sess-gone")
	require.NoError(t, err)
	assert.Nil(t, gone, "the unmatched session must not be imported")
}

func TestHistoryImportPublishesProgressOverSSE(t *testing.T) {
	isolateHistoryState(t)
	srv, ts := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 3, time.Now().Add(-2*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := openEventStream(t, ctx, ts.URL)
	defer stream.close()
	require.NoError(t, stream.waitForComment(2*time.Second))

	runID := startImport(t, srv, `{"agent":"claude-code"}`)

	var last *ImportRun
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		ev, err := stream.nextEvent(5 * time.Second)
		require.NoError(t, err)
		if ev.Import == nil {
			continue
		}
		assert.Equal(t, runID, ev.Import.RunID)
		last = ev.Import
		if ev.Import.Status == importStatusCompleted || ev.Import.Status == importStatusFailed {
			break
		}
	}
	require.NotNil(t, last, "no import event arrived on the stream")
	assert.Equal(t, importStatusCompleted, last.Status)
	assert.Equal(t, 3, last.Imported)
}

func TestHistoryImportCancelKeepsPartialProgress(t *testing.T) {
	isolateHistoryState(t)
	srv, ts := newHistoryServer(t)
	// An in-process import of a small fixture finishes in milliseconds, so the
	// cancel would race the run. Exporting through a delaying proxy makes each
	// batch take long enough for the cancel to arrive mid-run; the proxy is
	// otherwise transparent, marker header included. Turns are spread over
	// many sessions because a batch is confirmed per session, so a single
	// session would be one request however many turns it holds.
	srv.SetLocalEndpoint(newSlowIngestProxy(t, ts.URL, 30*time.Millisecond))
	const (
		sessions     = 20
		turnsEach    = 2
		expectedRuns = sessions * turnsEach
	)
	for i := range sessions {
		writeHistoryTranscript(t, fmt.Sprintf("sess-%02d", i), turnsEach, time.Now().Add(-2*time.Hour))
	}

	runID := startImport(t, srv, `{"agent":"claude-code"}`)
	// Cancel as soon as the run reports its first exported turn, so it stops
	// with sessions still to go.
	waitFor(t, 10*time.Second, func() bool {
		run, ok := srv.importRunState(runID)
		return ok && run.Imported > 0
	})
	resp := post(t, srv, "/api/v1/history/runs/"+runID+":cancel", "application/json", "")
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	resp.Body.Close()

	run := waitForTerminalRun(t, srv, runID)
	require.Equal(t, importStatusCancelled, run.Status)
	assert.Positive(t, run.Imported, "a cancelled run keeps the turns it already exported")
	require.Less(t, run.Imported, expectedRuns, "the cancel arrived after the run had finished")

	// A rerun resumes: the ledger already holds the exported turns, so they
	// come back as skipped rather than as duplicates.
	rerun := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	require.Equal(t, importStatusCompleted, rerun.Status)
	assert.Equal(t, run.Imported, rerun.Skipped)
	assert.Equal(t, expectedRuns, rerun.Imported+rerun.Skipped)

	for i := range sessions {
		detail, err := srv.storage.ConversationDetail(fmt.Sprintf("sess-%02d", i))
		require.NoError(t, err)
		assert.Len(t, detail.Generations, turnsEach, "one entry per source turn after a cancel and a rerun")
	}
}

func TestHistoryRunStatusUnknownRun(t *testing.T) {
	srv, _ := newTestServer(t)
	assert.Equal(t, http.StatusNotFound, getJSON(t, srv, "/api/v1/history/runs/nope", nil))

	resp := post(t, srv, "/api/v1/history/runs/nope:cancel", "application/json", "")
	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	resp.Body.Close()
}

func TestHistoryRunListIsBounded(t *testing.T) {
	srv, _ := newTestServer(t)
	for i := range historyRunHistory + 5 {
		run := ImportRun{RunID: fmt.Sprintf("run-%d", i), Agent: "claude-code", Status: importStatusRunning}
		require.True(t, srv.startImportRun(run, func() {}))
		srv.updateImportRun(run.RunID, func(r *ImportRun) { r.Status = importStatusCompleted })
	}
	srv.importMu.Lock()
	kept := len(srv.importRuns)
	srv.importMu.Unlock()
	assert.Equal(t, historyRunHistory, kept)

	_, ok := srv.importRunState("run-0")
	assert.False(t, ok, "the oldest run should have been dropped")
	_, ok = srv.importRunState(fmt.Sprintf("run-%d", historyRunHistory+4))
	assert.True(t, ok, "the newest run must be retained")
}

// TestHistoryImportNeverForwardsToCloud is the privacy guarantee. With
// forwarding enabled and credentials configured, an import must put nothing on
// the wire while the local store keeps the full content.
func TestHistoryImportNeverForwardsToCloud(t *testing.T) {
	isolateHistoryState(t)

	var cloudRequests atomic.Int64
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cloudRequests.Add(1)
		t.Errorf("the fake Cloud endpoint received %s %s during an import", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	defer cloud.Close()

	configPath := writeConfigEnvFile(t, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD":               "true",
		"AGENTO11Y_ENDPOINT":                    cloud.URL,
		"AGENTO11Y_AUTH_TENANT_ID":              "12345",
		"AGENTO11Y_AUTH_TOKEN":                  "glc_forwarding_token",
		"AGENTO11Y_CONTENT_CAPTURE_MODE":        "full",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL + "/otlp",
	})
	clearForwardEnv(t)
	storage, err := NewStorage(filepath.Join(t.TempDir(), "local"))
	require.NoError(t, err)
	srv := NewServer(storage, nil, configPath)
	t.Cleanup(srv.Close)
	ts := httptest.NewServer(srv)
	defer ts.Close()
	srv.SetLocalEndpoint(ts.URL)

	// The daemon must be forwarding, or this test would pass for the wrong
	// reason.
	fwd, err := srv.forward.resolve()
	require.NoError(t, err)
	require.True(t, fwd.enabled, "forwarding is off, so this test proves nothing")

	writeHistoryTranscript(t, "sess-private", 3, time.Now().Add(-2*time.Hour))
	run := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	require.Equal(t, importStatusCompleted, run.Status)
	require.Equal(t, 3, run.Imported)

	// Forwarding is asynchronous, so give a leaked request time to arrive.
	time.Sleep(300 * time.Millisecond)
	assert.Zero(t, cloudRequests.Load(), "an import must send nothing to Grafana Cloud")

	detail, err := storage.ConversationDetail("sess-private")
	require.NoError(t, err)
	require.Len(t, detail.Generations, 3)
	raw, err := json.Marshal(detail)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "answer 0", "the local store must keep the full imported content")
}

// TestHistoryImportRecreatesTheConversationsDirectory covers the store
// recovering from its directory being removed while the daemon runs.
func TestHistoryImportRecreatesTheConversationsDirectory(t *testing.T) {
	isolateHistoryState(t)
	srv, _ := newHistoryServer(t)
	writeHistoryTranscript(t, "sess-1", 2, time.Now().Add(-2*time.Hour))

	require.NoError(t, os.RemoveAll(filepath.Join(srv.storage.Dir(), ConversationsDir)))

	run := waitForTerminalRun(t, srv, startImport(t, srv, `{"agent":"claude-code"}`))
	require.Equal(t, importStatusCompleted, run.Status)
	assert.Equal(t, 2, run.Imported)

	detail, err := srv.storage.ConversationDetail("sess-1")
	require.NoError(t, err)
	assert.Len(t, detail.Generations, 2)
}

// startImport posts an import request and returns the run ID.
func startImport(t *testing.T, s *Server, body string) string {
	t.Helper()
	resp := post(t, s, "/api/v1/history:import", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusAccepted, resp.StatusCode)
	var got struct {
		RunID  string `json:"run_id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.NotEmpty(t, got.RunID)
	assert.Equal(t, importStatusPending, got.Status)
	return got.RunID
}

func waitForTerminalRun(t *testing.T, s *Server, runID string) ImportRun {
	t.Helper()
	var run ImportRun
	waitFor(t, 30*time.Second, func() bool {
		state, ok := s.importRunState(runID)
		if !ok {
			return false
		}
		run = state
		return state.terminal()
	})
	return run
}

// newSlowIngestProxy returns the URL of a reverse proxy in front of target
// that delays every request by delay.
func newSlowIngestProxy(t *testing.T, target string, delay time.Duration) string {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(delay)
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}
