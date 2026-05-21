package copilot

import (
	"context"
	"io"
	"log"
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
