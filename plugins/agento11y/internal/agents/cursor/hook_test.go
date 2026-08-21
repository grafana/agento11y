package cursor

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHookDispatch verifies the dispatcher-level response contract: the event
// reaches the right handler, every terminating path writes exactly one JSON
// response, and the permissive defer fallback never stacks a second line on
// top of a handler-written verdict or a real beforeSubmitPrompt response.
func TestHookDispatch(t *testing.T) {
	denyServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"deny","reason":"blocked tool"}`))
	}))
	defer denyServer.Close()

	allowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer allowServer.Close()

	tests := []struct {
		name string
		env  map[string]string
		// allowVerdict selects the allowing server; the default denies.
		allowVerdict       bool
		stdin              string
		wantStdout         string
		wantStdoutContains []string
	}{
		{
			name:       "routed_event_guards_disabled_responds_permissive",
			stdin:      `{"hook_event_name":"preToolUse","conversation_id":"conv","generation_id":"gen","tool_name":"Shell","tool_use_id":"tu_1","tool_input":{"command":"echo hi"}}`,
			wantStdout: `{"permission":"allow"}` + "\n",
		},
		{
			name:       "malformed_payload_falls_back_permissive",
			stdin:      `{"hook_event_name":"preToolUse","tool_name":`,
			wantStdout: `{"continue":true,"permission":"allow"}` + "\n",
		},
		{
			// A beforeSubmitPrompt payload that embeds the literal preToolUse
			// marker (here in a nested object) must not make the defer fallback
			// write a second permissive line.
			name:       "before_submit_prompt_nested_pretooluse_marker_single_line",
			stdin:      `{"hook_event_name":"beforeSubmitPrompt","prompt":"describe hooks","extra":{"hook_event_name":"preToolUse"}}`,
			wantStdout: permissiveResponse,
		},
		{
			name: "deny_verdict_not_overridden_by_fallback",
			env:  map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			stdin: `{"hook_event_name":"preToolUse","conversation_id":"conv","generation_id":"gen",` +
				`"tool_name":"Shell","tool_use_id":"tu_1","tool_input":{"command":"echo hi"}}`,
			wantStdoutContains: []string{`"permission":"deny"`, `blocked tool`},
		},
		{
			name:               "prompt_deny_replaces_the_permissive_line",
			env:                map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			stdin:              `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv","generation_id":"gen","prompt":"my token is glc_secret"}`,
			wantStdoutContains: []string{`"continue":false`, `"user_message":`, "blocked this message"},
		},
		{
			name:         "prompt_allow_keeps_the_permissive_line",
			env:          map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			allowVerdict: true,
			stdin:        `{"hook_event_name":"beforeSubmitPrompt","conversation_id":"conv","generation_id":"gen","prompt":"hello"}`,
			wantStdout:   permissiveResponse,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("SIGIL_GUARDS_ENABLED", "")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			endpoint := denyServer.URL
			if tt.allowVerdict {
				endpoint = allowServer.URL
			}
			t.Setenv("SIGIL_ENDPOINT", endpoint)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			var stdout bytes.Buffer
			err := Hook(context.Background(), strings.NewReader(tt.stdin), &stdout, log.New(&bytes.Buffer{}, "", 0))
			if err != nil {
				t.Fatalf("Hook returned error: %v", err)
			}

			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q\nfull output: %s", want, stdout.String())
				}
			}
			if got := strings.Count(stdout.String(), "\n"); got != 1 {
				t.Errorf("expected exactly one response line, got %d: %q", got, stdout.String())
			}
		})
	}
}
