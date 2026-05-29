package copilot

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/fragment"
)

func TestHookUnknownEventDoesNotCrash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"hook_event_name":"Unknown","session_id":"sess"}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
}

func TestHookUserPromptCreatesTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"sess","prompt":"hello","timestamp":"2026-05-18T12:00:00Z"}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt, got %+v", got)
	}
}

func TestHookPreToolUseDeniedPropagatesStdout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "true")
	t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "1500")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"deny","reason":"blocked"}`))
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	var stdout bytes.Buffer
	payload := `{"hook_event_name":"preToolUse","session_id":"sess","tool_name":"bash","toolArgs":{"cmd":"rm -rf /"}}`
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(payload), &stdout, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if !strings.Contains(stdout.String(), `"permissionDecision":"deny"`) {
		t.Fatalf("stdout = %q, want deny decision", stdout.String())
	}
	if !strings.Contains(stdout.String(), `A Grafana AI Observability policy`) ||
		!strings.Contains(stdout.String(), `Reason: blocked`) {
		t.Fatalf("stdout = %q, want wrapped deny reason", stdout.String())
	}
}

func TestHookPreToolUseAllowKeepsStdoutEmpty(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "true")
	t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "1500")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer server.Close()
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	var stdout bytes.Buffer
	payload := `{"hook_event_name":"preToolUse","session_id":"sess","tool_name":"bash","toolArgs":{"cmd":"echo hi"}}`
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(payload), &stdout, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

func TestHookUsesEventFromEnvFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	t.Setenv("SIGIL_COPILOT_HOOK_EVENT", "userPromptSubmitted")
	logger := log.New(io.Discard, "", 0)
	if err := Hook(context.Background(), strings.NewReader(`{"sessionId":"sess","prompt":"hello","timestamp":1747579200000}`), io.Discard, logger); err != nil {
		t.Fatalf("Hook: %v", err)
	}
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt from env-dispatched event, got %+v", got)
	}
}
