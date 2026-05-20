package main

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/copilot/internal/fragment"
)

func TestRunUnknownEventDoesNotCrash(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	logger := log.New(io.Discard, "", 0)
	run(logger, strings.NewReader(`{"hook_event_name":"Unknown","session_id":"sess"}`))
}

func TestRunUserPromptCreatesTurn(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	logger := log.New(io.Discard, "", 0)
	run(logger, strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"sess","prompt":"hello","timestamp":"2026-05-18T12:00:00Z"}`))
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt, got %+v", got)
	}
}

func TestRunUsesEventFromEnvFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
	t.Setenv("SIGIL_COPILOT_HOOK_EVENT", "userPromptSubmitted")
	logger := log.New(io.Discard, "", 0)
	run(logger, strings.NewReader(`{"sessionId":"sess","prompt":"hello","timestamp":1747579200000}`))
	got := fragment.LoadTolerant("sess", "turn-000001", logger)
	if got == nil || got.Prompt != "hello" {
		t.Fatalf("expected fragment with prompt from env-dispatched event, got %+v", got)
	}
}
