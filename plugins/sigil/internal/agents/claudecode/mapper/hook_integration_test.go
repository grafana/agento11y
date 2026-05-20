package mapper_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	claudecode "github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode/state"
)

func TestHookSessionEndProcessesAssistantFlushedAfterStop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	capture := &exportCapture{}
	server := httptest.NewServer(http.HandlerFunc(capture.handle))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	sessionID := "delayed-flush-session"
	transcriptPath := filepath.Join(dir, "transcript.jsonl")
	userPart := buildUserJSONL(sessionID, "hey") + "\n"
	assistantPart := buildAssistantJSONL(sessionID, "req-1", "claude-sonnet-4-20250514", 25, "hello") + "\n"
	if err := os.WriteFile(transcriptPath, []byte(userPart), 0o644); err != nil {
		t.Fatal(err)
	}

	stopLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "Stop",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(stopLogs, "no generations produced; keeping offset=0 for next event") {
		t.Fatalf("Stop did not preserve offset after user-only transcript:\n%s", stopLogs)
	}
	if st := state.Load(sessionID); st.Offset != 0 {
		t.Fatalf("Offset after Stop = %d, want 0", st.Offset)
	}
	capture.assert(t, nil)

	f, err := os.OpenFile(transcriptPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(assistantPart); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	sessionEndLogs := runClaudeHook(t, hookPayload{
		HookEventName:  "SessionEnd",
		SessionID:      sessionID,
		TranscriptPath: transcriptPath,
	})
	if !strings.Contains(sessionEndLogs, "produced 1 generations") {
		t.Fatalf("SessionEnd did not produce one generation:\n%s", sessionEndLogs)
	}

	capture.assert(t, []int{1})
	st := state.Load(sessionID)
	wantOffset := int64(len([]byte(userPart + assistantPart)))
	if st.Offset != wantOffset {
		t.Fatalf("Offset after SessionEnd = %d, want %d", st.Offset, wantOffset)
	}
}

type hookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Model          string `json:"model,omitempty"`
}

type exportCapture struct {
	mu               sync.Mutex
	paths            []string
	generationCounts []int
	readErrs         []string
	decodeErrs       []string
}

func (c *exportCapture) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	var req struct {
		Generations []json.RawMessage `json:"generations"`
	}
	decodeErr := json.Unmarshal(body, &req)

	c.mu.Lock()
	defer c.mu.Unlock()
	c.paths = append(c.paths, r.URL.Path)
	c.generationCounts = append(c.generationCounts, len(req.Generations))
	if err != nil {
		c.readErrs = append(c.readErrs, err.Error())
	}
	if decodeErr != nil {
		c.decodeErrs = append(c.decodeErrs, decodeErr.Error())
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"results":[]}`))
}

func (c *exportCapture) assert(t *testing.T, wantCounts []int) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.readErrs) > 0 {
		t.Fatalf("read export body errors: %v", c.readErrs)
	}
	if len(c.decodeErrs) > 0 {
		t.Fatalf("decode export body errors: %v", c.decodeErrs)
	}
	if len(c.generationCounts) != len(wantCounts) {
		t.Fatalf("export request count = %d, want %d (counts=%v)", len(c.generationCounts), len(wantCounts), c.generationCounts)
	}
	for i, want := range wantCounts {
		if c.paths[i] != "/api/v1/generations:export" {
			t.Fatalf("export path[%d] = %q, want /api/v1/generations:export", i, c.paths[i])
		}
		if c.generationCounts[i] != want {
			t.Fatalf("export generations[%d] = %d, want %d", i, c.generationCounts[i], want)
		}
	}
}

func setHookExportEnv(t *testing.T, endpoint string) {
	t.Helper()
	t.Setenv("SIGIL_ENDPOINT", endpoint)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("SIGIL_AUTH_MODE", "basic")
	t.Setenv("SIGIL_PROTOCOL", "http")
	t.Setenv("SIGIL_USER_ID", "test-user")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
}

func runClaudeHook(t *testing.T, input hookPayload) string {
	t.Helper()
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	if err := claudecode.Hook(context.Background(), bytes.NewReader(payload), io.Discard, log.New(&logs, "", 0)); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	return logs.String()
}

func buildUserJSONL(sessionID, text string) string {
	line := map[string]any{
		"type":      "user",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:00:00Z",
		"version":   "1.0.0",
		"message": map[string]any{
			"role":    "user",
			"content": text,
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}

func buildAssistantJSONL(sessionID, requestID, model string, outputTokens int, text string) string {
	line := map[string]any{
		"type":      "assistant",
		"sessionId": sessionID,
		"timestamp": "2025-06-01T12:01:00Z",
		"version":   "1.0.0",
		"gitBranch": "main",
		"cwd":       "/projects/test",
		"requestId": requestID,
		"message": map[string]any{
			"model": model,
			"content": []map[string]any{
				{"type": "text", "text": text},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  50,
				"output_tokens": outputTokens,
			},
		},
	}
	data, _ := json.Marshal(line)
	return string(data)
}
