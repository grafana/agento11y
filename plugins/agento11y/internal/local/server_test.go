package local

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var out hookResponse
	decodeJSON(t, resp.Body, &out)
	assert.Equal(t, "allow", out.Action)
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
	s, _ := newTestServer(t)
	cases := []struct {
		name            string
		method          string
		path            string
		body            string
		want            int
		wantContentType string // prefix-matched; "" skips the check
		wantBodyHas     string // substring check; "" skips
	}{
		{name: "root serves viewer HTML", method: http.MethodGet, path: "/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "conversation path serves viewer HTML", method: http.MethodGet, path: "/conversations/conv-123", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "settings path serves viewer HTML", method: http.MethodGet, path: "/settings", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "settings trailing slash serves viewer HTML", method: http.MethodGet, path: "/settings/", want: http.StatusOK, wantContentType: "text/html", wantBodyHas: `<script type="text/babel" src="/assets/app.jsx">`},
		{name: "CSS asset", method: http.MethodGet, path: "/assets/app.css", want: http.StatusOK, wantContentType: "text/css", wantBodyHas: ":root"},
		{name: "JSX asset", method: http.MethodGet, path: "/assets/app.jsx", want: http.StatusOK, wantContentType: "text/babel", wantBodyHas: "function App()"},
		{name: "healthz serves JSON", method: http.MethodGet, path: "/healthz", want: http.StatusOK, wantContentType: "application/json", wantBodyHas: `"status":"ok"`},
		{name: "unknown route", method: http.MethodPost, path: "/api/v1/unknown", body: "{}", want: http.StatusNotFound},
		{name: "wrong method on generations export", method: http.MethodPut, path: "/api/v1/generations:export", body: "{}", want: http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
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
		name        string
		method      string
		path        string
		want        int
		wantBodyHas []string // all substrings must appear
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
			wantBodyHas: []string{`"id":"conv-B"`},
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
// yet. The endpoint must return an array, never null.
func TestServer_APIConversations_EmptyStorage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/conversations", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"conversations":[]}`, strings.TrimSpace(rr.Body.String()))
}

func newTestServer(t *testing.T) (*Server, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "local")
	storage, err := NewStorage(dir)
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return NewServer(storage, nil, filepath.Join(dir, "config.env")), dir
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
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
	})
}

func TestServer_APITokenMetrics_EmptyStorage(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/metrics/tokens", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	assert.Equal(t, `{"points":[]}`, strings.TrimSpace(rr.Body.String()))
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

	// Save a non-default configuration.
	resp := putConfig(t, srv, Settings{
		Capture:      "metadata_only",
		Tags:         []Tag{{Key: "team", Value: "ai"}},
		Guards:       guardsFailClosed,
		GuardTimeout: "2000",
		Debug:        true,
		AutoUpdate:   false,
		UserID:       "alice",
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

	// Preview and on-disk file agree, sorted with the managed header.
	onDisk, err := os.ReadFile(configPathFor(dir))
	require.NoError(t, err)
	assert.Contains(t, string(onDisk), "SIGIL_CONTENT_CAPTURE_MODE=metadata_only")
	assert.Contains(t, string(onDisk), "SIGIL_GUARDS_TIMEOUT_MS=2000")
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
