package vibe

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
)

func TestHook_Dispatch(t *testing.T) {
	// Malformed payloads and the non-stdout events must return nil and
	// write nothing to stdout (vibe reads a non-empty stdout on
	// post_agent_turn / after_tool as a deny/retry signal). The handlers
	// themselves are exercised in hook/*_test.go; here we confirm the
	// dispatcher does not crash and keeps stdout clean for these branches.
	// Guards default to off, so before_tool is a pass-through too.
	tests := []struct {
		name  string
		stdin string
	}{
		{name: "empty stdin", stdin: ""},
		{name: "whitespace", stdin: "   \n"},
		{name: "invalid json", stdin: "not json"},
		{name: "missing event name", stdin: `{"session_id":"s","transcript_path":"p"}`},
		{name: "unknown event", stdin: `{"hook_event_name":"session_start","session_id":"s"}`},
		{name: "before_tool with guards off", stdin: `{"hook_event_name":"before_tool","session_id":"s","tool_name":"bash"}`},
		{name: "after_tool", stdin: `{"hook_event_name":"after_tool","session_id":"s","tool_call_id":"tc1","tool_status":"success"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "false")
			var stdout bytes.Buffer
			logger := log.New(io.Discard, "", 0)
			err := Hook(context.Background(), strings.NewReader(tt.stdin), &stdout, logger)
			if err != nil {
				t.Errorf("Hook returned err=%v, want nil", err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty (vibe reads it as deny/allow)", stdout.String())
			}
		})
	}
}

func TestHook_BeforeToolStdoutIsPlumbed(t *testing.T) {
	// The dispatcher must hand its stdout to the before_tool handler so a
	// guard deny reaches vibe. Force a deny via fail-closed guards with no
	// credentials and assert the decision shows up on stdout.
	t.Setenv("SIGIL_GUARDS_ENABLED", "true")
	t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "false")
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	var stdout bytes.Buffer
	logger := log.New(io.Discard, "", 0)
	err := Hook(context.Background(),
		strings.NewReader(`{"hook_event_name":"before_tool","session_id":"s","tool_name":"bash","tool_input":{"command":"ls"}}`),
		&stdout, logger)
	if err != nil {
		t.Fatalf("Hook returned err=%v, want nil", err)
	}
	if !strings.Contains(stdout.String(), `"decision":"deny"`) {
		t.Errorf("stdout = %q, want a deny decision plumbed through from before_tool", stdout.String())
	}
}
