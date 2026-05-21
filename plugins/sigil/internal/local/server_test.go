package local

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"
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
	writeGen(t, storage, "conv-A", "g1", sigil.Generation{
		AgentName:   "pi",
		Model:       sigil.ModelRef{Name: "claude-opus-4-7"},
		StartedAt:   mustParse(t, "2026-05-21T10:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T10:00:03Z"),
		Usage:       sigil.TokenUsage{InputTokens: 100, OutputTokens: 50},
	}, "2026-05-21T10:00:03Z")
	writeGen(t, storage, "conv-B", "g2", sigil.Generation{
		AgentName:   "claude-code",
		Model:       sigil.ModelRef{Name: "claude-sonnet-4"},
		StartedAt:   mustParse(t, "2026-05-21T11:00:00Z"),
		CompletedAt: mustParse(t, "2026-05-21T11:00:01Z"),
		Usage:       sigil.TokenUsage{InputTokens: 10, OutputTokens: 5},
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
				"metadata":{"sigil.conversation.title":"Local mode smoke test"}
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
				assert.Equal(t, sigil.RoleUser, gen.Input[0].Role)
				assert.Equal(t, sigil.RoleAssistant, gen.Output[0].Role)
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
				assert.Equal(t, sigil.RoleTool, input[0].Role)
				require.Len(t, input[0].Parts, 1)
				part := input[0].Parts[0]
				require.NotNil(t, part.ToolResult)
				assert.Equal(t, sigil.PartKindToolResult, part.Kind)
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
	return NewServer(storage, nil), dir
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
