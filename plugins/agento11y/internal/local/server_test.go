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
	"github.com/grafana/agento11y/go/agento11y/model"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
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
	// The forward loader falls back to the process environment, so a developer
	// with forwarding and real credentials exported would otherwise have this
	// suite POST to their live tenant.
	clearForwardEnv(t)
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
	// endpoint was configured, so that leg reports why it stays off.
	assert.Equal(t, forwardStatus{
		Enabled:     true,
		Mode:        forwardModeMetadataOnly,
		Generations: true,
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
// not forwarded again, so two daemons pointed at each other (or one pointed at
// itself) exchange one copy instead of looping.
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
		req.Header.Set(forwardMarkerHeader, "1")
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		require.Equal(t, http.StatusOK, rr.Code)
	}
	s.forward.wait()

	// Stored locally, never relayed.
	assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, "conv-A.jsonl")), 1)
	assert.Empty(t, hits)
}
