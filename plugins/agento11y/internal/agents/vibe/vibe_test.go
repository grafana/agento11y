package vibe

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
)

func TestHook_Dispatch(t *testing.T) {
	// Malformed payloads and the non-stdout events must return nil and
	// write nothing to stdout (vibe reads a non-empty stdout on
	// post_agent / post_tool as a deny/retry signal). The handlers
	// themselves are exercised in hook/*_test.go; here we confirm the
	// dispatcher does not crash and keeps stdout clean for these branches.
	// Guards default to off, so pre_tool is a pass-through too.
	tests := []struct {
		name  string
		stdin string
	}{
		{name: "empty stdin", stdin: ""},
		{name: "whitespace", stdin: "   \n"},
		{name: "invalid json", stdin: "not json"},
		{name: "missing event name", stdin: `{"session_id":"s","transcript_path":"p"}`},
		{name: "unknown event", stdin: `{"hook_event_name":"session_start","session_id":"s"}`},
		{name: "pre_tool with guards off", stdin: `{"hook_event_name":"pre_tool","session_id":"s","tool_name":"bash"}`},
		{name: "post_tool", stdin: `{"hook_event_name":"post_tool","session_id":"s","tool_call_id":"tc1","tool_status":"success"}`},
		// Pre-2.21.0 vibe sends the same events under their old names.
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

func TestHook_PostToolRecordsTiming(t *testing.T) {
	// The other two dispatch labels are pinned by an assertion downstream of
	// them (post_agent by the golden export body, pre_tool by the deny below),
	// while post_tool writes nothing to stdout, so only a recorded event tells
	// a correct label from a typo. Both spellings run: the install path picks
	// one from the installed vibe, and a hooks.toml written before an upgrade
	// (or by hand) can send the other.
	for _, event := range []string{"post_tool", "after_tool"} {
		t.Run(event, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
			session := "s-" + event

			var stdout bytes.Buffer
			logger := log.New(io.Discard, "", 0)
			err := Hook(context.Background(),
				strings.NewReader(`{"hook_event_name":"`+event+`","session_id":"`+session+`","tool_call_id":"tc1","tool_name":"bash","tool_status":"success","duration_ms":42.0}`),
				&stdout, logger)
			if err != nil {
				t.Fatalf("Hook returned err=%v, want nil", err)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout = %q, want empty (vibe reads it on this event as a failure)", stdout.String())
			}
			ev, ok := toolevents.Load(session)["tc1"]
			if !ok {
				t.Fatalf("no tool event recorded for tc1, so %s did not reach its handler", event)
			}
			if ev.DurationMs != 42 {
				t.Errorf("duration = %v, want 42", ev.DurationMs)
			}
		})
	}
}

func TestHook_PreToolStdoutIsPlumbed(t *testing.T) {
	// The dispatcher must hand its stdout to the pre-tool handler so a
	// guard deny reaches vibe. Force a deny via fail-closed guards with no
	// credentials and assert the decision shows up on stdout, under either
	// spelling of the event.
	for _, event := range []string{"pre_tool", "before_tool"} {
		t.Run(event, func(t *testing.T) {
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "false")
			t.Setenv("SIGIL_ENDPOINT", "")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")

			var stdout bytes.Buffer
			logger := log.New(io.Discard, "", 0)
			err := Hook(context.Background(),
				strings.NewReader(`{"hook_event_name":"`+event+`","session_id":"s","tool_name":"bash","tool_input":{"command":"ls"}}`),
				&stdout, logger)
			if err != nil {
				t.Fatalf("Hook returned err=%v, want nil", err)
			}
			if !strings.Contains(stdout.String(), `"decision":"deny"`) {
				t.Errorf("stdout = %q, want a deny decision plumbed through from %s", stdout.String(), event)
			}
		})
	}
}
