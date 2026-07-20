package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
)

func TestPostAgentTurn_EndToEnd(t *testing.T) {
	// Drive the handler through a httptest server playing the role of
	// the Sigil export endpoint, then assert the offset and token
	// snapshot in state were advanced.
	type captured struct {
		path string
		body []byte
	}
	var (
		mu   sync.Mutex
		recs []captured
	)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		recs = append(recs, captured{path: r.URL.Path, body: body})
		mu.Unlock()
		writeAcceptedGenerationResponse(t, w, body)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_ENDPOINT", srv.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")

	// Copy the fixture into a tempdir so the meta.json sibling is
	// resolved off a real on-disk path.
	dir := t.TempDir()
	must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
	must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
	tp := filepath.Join(dir, "messages.jsonl")

	logger := log.New(io.Discard, "", 0)
	payload := Payload{HookEventName: "post_agent_turn", SessionID: "sess-end-to-end", TranscriptPath: tp}

	PostAgentTurn(context.Background(), payload, logger)

	mu.Lock()
	got := recs
	mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("expected at least one export request")
	}
	if got[0].path != "/api/v1/generations:export" {
		t.Errorf("path = %q, want /api/v1/generations:export", got[0].path)
	}
	// One generation per turn; the request body must carry exactly one
	// generation with our agent name.
	var req struct {
		Generations []struct {
			AgentName      string `json:"agent_name"`
			ConversationID string `json:"conversation_id"`
			Model          struct {
				Provider string `json:"provider"`
				Name     string `json:"name"`
			} `json:"model"`
		} `json:"generations"`
	}
	if err := json.Unmarshal(got[0].body, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if n := len(req.Generations); n != 1 {
		t.Fatalf("got %d generations, want 1", n)
	}
	g := req.Generations[0]
	if g.AgentName != "mistral-vibe" {
		t.Errorf("AgentName = %q, want mistral-vibe", g.AgentName)
	}
	if g.ConversationID != "sess-end-to-end" {
		t.Errorf("ConversationID = %q, want sess-end-to-end", g.ConversationID)
	}
	if g.Model.Provider != "mistral" {
		t.Errorf("provider = %q, want mistral", g.Model.Provider)
	}

	// State must reflect the new offset and session-token snapshot.
	st, _ := state.Load("sess-end-to-end")
	if st.Offset == 0 {
		t.Errorf("state.Offset not advanced (still 0)")
	}
	if st.SessionPromptTokens == 0 {
		t.Errorf("SessionPromptTokens not stored")
	}
}

func TestPostAgentTurn_UsesMetaParentSessionID(t *testing.T) {
	// A subagent session carries its parent only in meta.json, not on the
	// thin hook payload. The handler must still resolve the parent edge.
	var (
		mu   sync.Mutex
		body []byte
	)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body = b
		mu.Unlock()
		writeAcceptedGenerationResponse(t, w, b)
	}))
	t.Cleanup(srv.Close)

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_ENDPOINT", srv.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	must(t, state.Save("parent-session", state.Session{LastGenerationID: "parent-generation"}))

	dir := t.TempDir()
	must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
	metaPath := filepath.Join(dir, "meta.json")
	must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), metaPath))
	must(t, replaceInFile(metaPath, `"parent_session_id": null`, `"parent_session_id": "parent-session"`))
	tp := filepath.Join(dir, "messages.jsonl")

	logger := log.New(io.Discard, "", 0)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn", SessionID: "child-session", TranscriptPath: tp}, logger)

	mu.Lock()
	got := body
	mu.Unlock()
	if len(got) == 0 {
		t.Fatalf("expected export request")
	}
	var req struct {
		Generations []struct {
			ConversationID      string            `json:"conversation_id"`
			ParentGenerationIDs []string          `json:"parent_generation_ids"`
			Tags                map[string]string `json:"tags"`
		} `json:"generations"`
	}
	if err := json.Unmarshal(got, &req); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(req.Generations) != 1 {
		t.Fatalf("got %d generations, want 1", len(req.Generations))
	}
	g := req.Generations[0]
	if g.ConversationID != "parent-session" {
		t.Errorf("ConversationID = %q, want parent-session (reparented from meta.json)", g.ConversationID)
	}
	if len(g.ParentGenerationIDs) != 1 || g.ParentGenerationIDs[0] != "parent-generation" {
		t.Errorf("ParentGenerationIDs = %v, want [parent-generation]", g.ParentGenerationIDs)
	}
	if g.Tags["vibe.parent_session_id"] != "parent-session" {
		t.Errorf("parent tag = %q, want parent-session", g.Tags["vibe.parent_session_id"])
	}
}

func TestPostAgentTurn_SaveStateFailureSkipsExport(t *testing.T) {
	// State is saved before the export. If the save fails, nothing is
	// exported, so a successful export can never be stranded by a lost
	// offset. Force a save failure by pointing XDG_STATE_HOME at a file.
	var (
		mu      sync.Mutex
		exports int
	)
	srv := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		exports++
		mu.Unlock()
		writeAcceptedGenerationResponse(t, w, b)
	}))
	t.Cleanup(srv.Close)

	stateFile := filepath.Join(t.TempDir(), "not-a-dir")
	must(t, os.WriteFile(stateFile, []byte("x"), 0o600))
	t.Setenv("XDG_STATE_HOME", stateFile)
	t.Setenv("SIGIL_ENDPOINT", srv.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	dir := t.TempDir()
	must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
	must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
	tp := filepath.Join(dir, "messages.jsonl")

	logger := log.New(io.Discard, "", 0)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn", SessionID: "sess-save-fails", TranscriptPath: tp}, logger)

	mu.Lock()
	got := exports
	mu.Unlock()
	if got != 0 {
		t.Fatalf("exports = %d, want 0 (save failure must abort before export)", got)
	}
}

func replaceInFile(path, old, replacement string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.ReplaceAll(string(data), old, replacement)), 0o600)
}

func TestPostAgentTurn_NoNewMessagesNoExport(t *testing.T) {
	// Pre-populate state with the file's full byte length so the next
	// read returns zero lines; PostAgentTurn must skip silently.
	dir := t.TempDir()
	must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
	must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
	tp := filepath.Join(dir, "messages.jsonl")
	info, err := os.Stat(tp)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_ENDPOINT", "http://example.invalid")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "t")
	t.Setenv("SIGIL_AUTH_TOKEN", "t")
	prior := state.Session{
		Offset:                  info.Size(),
		SessionPromptTokens:     1,
		SessionCompletionTokens: 1,
	}
	if err := state.Save("sess-empty", prior); err != nil {
		t.Fatalf("save: %v", err)
	}
	logger := log.New(io.Discard, "", 0)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn", SessionID: "sess-empty", TranscriptPath: tp}, logger)

	// State must be unchanged; if export had run it would have
	// overwritten our token snapshot.
	got, _ := state.Load("sess-empty")
	if got != prior {
		t.Errorf("state changed: got %+v want %+v", got, prior)
	}
}

func TestPostAgentTurn_MissingCredsSkipsExport(t *testing.T) {
	// No SIGIL_* env: the handler must log and bail without touching
	// state, so a re-run later with credentials still picks up from
	// the same offset.
	dir := t.TempDir()
	must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
	must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
	tp := filepath.Join(dir, "messages.jsonl")

	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	logger := log.New(io.Discard, "", 0)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn", SessionID: "sess-noauth", TranscriptPath: tp}, logger)
	if got, found := state.Load("sess-noauth"); found || (got != state.Session{}) {
		t.Errorf("state was written despite missing creds: found=%v got=%+v", found, got)
	}
}

func TestPostAgentTurn_MissingPayloadFields(t *testing.T) {
	logger := log.New(io.Discard, "", 0)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn"}, logger)
	PostAgentTurn(context.Background(), Payload{HookEventName: "post_agent_turn", SessionID: "s"}, logger)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}

func writeAcceptedGenerationResponse(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	generations, _ := request["generations"].([]any)
	results := make([]map[string]any, 0, len(generations))
	for _, raw := range generations {
		gen, _ := raw.(map[string]any)
		id, _ := gen["id"].(string)
		results = append(results, map[string]any{"generation_id": id, "accepted": true})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
}
