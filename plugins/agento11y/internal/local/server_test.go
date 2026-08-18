package local

import (
	"bytes"
	"context"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

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

	// Both generations belong to conv-A so they share one file.
	lines := readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl"))
	require.Len(t, lines, 2)
	var rec generationRecord
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &rec))
	assert.Equal(t, "gen-1", rec.GenerationID)
	assert.Equal(t, "conv-A", rec.ConversationID)
	assert.NotEmpty(t, rec.ReceivedAt)
	assert.JSONEq(t, `{"id":"gen-1","conversation_id":"conv-A","model":{"name":"m1"}}`, string(rec.Generation))
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

	// The list bounds on that stamp, so the conversation stays in a range
	// that covers its live turn.
	since := live.Add(-time.Minute).Format(time.RFC3339Nano)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations?since="+url.QueryEscape(since), nil)
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

func TestServer_HookEvaluate_Allow(t *testing.T) {
	s, dir := newTestServer(t)
	body := `{"phase":"postflight","context":{"agent_name":"x"}}`

	resp := post(t, s, "/api/v1/hooks:evaluate", "application/json", body)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var out agento11y.HookEvaluateResponse
	decodeJSON(t, resp.Body, &out)
	assert.Equal(t, agento11y.HookActionAllow, out.Action)
	assert.NotNil(t, out.Evaluations)

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
		{name: "root serves viewer HTML", method: http.MethodGet, path: "/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "conversation path serves viewer HTML", method: http.MethodGet, path: "/conversations/conv-123", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "settings path serves viewer HTML", method: http.MethodGet, path: "/settings", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "settings trailing slash serves viewer HTML", method: http.MethodGet, path: "/settings/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "CSS asset", method: http.MethodGet, path: "/assets/app.css", want: http.StatusOK, wantContentType: "text/css", wantBodyHas: ":root"},
		{name: "JSX asset", method: http.MethodGet, path: "/assets/app.jsx", want: http.StatusOK, wantContentType: "text/babel", wantBodyHas: "function App()"},
		{name: "healthz serves JSON", method: http.MethodGet, path: "/healthz", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"status":"ok"`},
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
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.contentType != "" {
				req.Header.Set("Content-Type", tc.contentType)
			}
			rr := httptest.NewRecorder()
			s.ServeHTTP(rr, req)
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
			req := httptest.NewRequest(tc.method, tc.path, nil)
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

			req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
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
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/tokens", nil)
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
		assert.Equal(t, TokenUsagePoint{
			Timestamp:    mustParse(t, "2026-05-21T10:00:00Z"),
			Model:        "claude-sonnet-4",
			Provider:     "anthropic",
			TokenBuckets: TokenBuckets{FreshInput: 100, CacheRead: 30, CacheWrite: 20, Output: 50},
		}, body.Points[0])
	})

	t.Run("wrong method rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/metrics/tokens", strings.NewReader("{}"))
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

		req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/tokens?since=2026-05-21T09:00:00Z&interval=3600", nil)
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

		req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/tokens?since=2026-05-21T09:00:00Z&interval=3600", nil)
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
			req := httptest.NewRequest(http.MethodGet, path, nil)
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			assert.Equal(t, http.StatusBadRequest, rr.Code, path)
		}
	})
}

func TestServer_APITokenMetrics_EmptyStorage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/tokens", nil)
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
	req := httptest.NewRequest(http.MethodPut, "/api/v1/config", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr.Result()
}

// TestServer_Config_RoundTrip saves settings and reads them back, asserting
// the GET reflects the normalised on-disk state and the file is written.
func TestServer_Config_RoundTrip(t *testing.T) {
	srv, dir := newTestServer(t)

	// GET on an absent file returns the local defaults.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
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
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
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

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/config", nil)
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
// It serves TLS because resolveForwardConfig refuses an http://127.0.0.1
// endpoint as a hook target, so an https test server is what lets the server
// tests go through the real gate instead of around it. newForwardingTestServer
// trusts its cert, and the relay-level tests inject srv.Client() themselves.
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
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks:evaluate", strings.NewReader(body))
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
		decodeJSON(t, resp.Body, &out)
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
			name:       "transform",
			respond:    `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash","input_json":{"command":"echo safe"}}}]}]}}`,
			wantAction: agento11y.HookActionAllow,
			assertMore: func(t *testing.T, out agento11y.HookEvaluateResponse) {
				require.NotNil(t, out.TransformedInput)
				require.Len(t, out.TransformedInput.Output, 1)
				parts := out.TransformedInput.Output[0].Parts
				require.Len(t, parts, 1)
				require.NotNil(t, parts[0].ToolCall)
				assert.JSONEq(t, `{"command":"echo safe"}`, string(parts[0].ToolCall.InputJSON))
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

// TestServer_HookEvaluate_FailModes covers what the agent is told when the
// Cloud call does not produce a verdict. Fail-open keeps the local allow;
// fail-closed denies, and that deny has to be labelled as an evaluation
// failure or every consumer renders it as "a policy blocked this call".
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

// TestServer_HookEvaluate_RelayShape covers what the daemon puts on the wire:
// the received bytes unchanged, the loop marker, Cloud auth, and the budget
// derived from the calling agent's own deadline.
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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=", nil)
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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=rate+limit", nil)
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
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=zzz-impossible", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		assert.Contains(t, rr.Body.String(), `"hits":[]`)
	})

	t.Run("limit=0 falls back to the default cap", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/search?q=rate&limit=0", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/api/v1/search/capabilities", nil)
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
func TestServer_DevAsset_EnvPrecedence(t *testing.T) {
	srv, _ := newTestServer(t)

	preferred := t.TempDir()
	legacy := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(preferred, "app.jsx"), []byte("// preferred"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(legacy, "app.jsx"), []byte("// legacy"), 0o600))

	fetchJSX := func() string {
		req := httptest.NewRequest(http.MethodGet, "/assets/app.jsx", nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
		return rr.Body.String()
	}

	t.Run("preferred wins over legacy", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", preferred)
		t.Setenv("SIGIL_LOCAL_WEB_DIR", legacy)
		assert.Equal(t, "// preferred", fetchJSX())
	})

	t.Run("legacy is used as a fallback", func(t *testing.T) {
		t.Setenv("AGENTO11Y_LOCAL_WEB_DIR", "")
		t.Setenv("SIGIL_LOCAL_WEB_DIR", legacy)
		assert.Equal(t, "// legacy", fetchJSX())
	})
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
		req := httptest.NewRequest(http.MethodPost, path,
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
