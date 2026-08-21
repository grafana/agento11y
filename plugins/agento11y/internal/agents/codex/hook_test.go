package codex

import (
	"bytes"
	"context"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestHookRoutesPreToolUse(t *testing.T) {
	var responseBody atomic.Value
	responseBody.Store("")
	server := newDispatcherTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := responseBody.Load().(string)
		if body == "" {
			body = `{"action":"allow"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	tests := []struct {
		name               string
		serverResponds     string
		wantStdoutEmpty    bool
		wantStdoutContains []string
	}{
		{
			name:            "allow_response_produces_empty_stdout",
			serverResponds:  `{"action":"allow"}`,
			wantStdoutEmpty: true,
		},
		{
			name:               "deny_response_emits_codex_deny_json",
			serverResponds:     `{"action":"deny","reason":"blocked"}`,
			wantStdoutContains: []string{`"permissionDecision":"deny"`},
		},
		{
			name:           "transform_response_emits_allow_updated_input_json",
			serverResponds: `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Bash","input_json":{"command":"echo [REDACTED]"}}}]}]}}`,
			wantStdoutContains: []string{
				`"hookSpecificOutput"`,
				`"hookEventName":"PreToolUse"`,
				`"permissionDecision":"allow"`,
				`"updatedInput":{"command":"echo [REDACTED]"}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", "")
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", "")
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			responseBody.Store(tt.serverResponds)

			stdin := strings.NewReader(`{"hook_event_name":"PreToolUse","session_id":"sess","tool_name":"Bash","tool_use_id":"tu_1","tool_input":{"command":"rm -rf /"},"model":"gpt-5"}`)
			var stdout bytes.Buffer
			if err := Hook(context.Background(), stdin, &stdout, log.New(io.Discard, "", 0)); err != nil {
				t.Fatalf("Hook: %v", err)
			}

			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, want substring %q", stdout.String(), want)
				}
			}
		})
	}
}

// Hook must return nil because a non-zero exit runs the `sigil` fallback and
// sends the prompt.
func TestHookRoutesUserPromptSubmit(t *testing.T) {
	var responseBody atomic.Value
	responseBody.Store("")
	server := newDispatcherTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := responseBody.Load().(string)
		if body == "" {
			body = `{"action":"allow"}`
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	tests := []struct {
		name               string
		endpoint           string
		failOpen           string
		serverResponds     string
		wantStdoutEmpty    bool
		wantStdoutContains []string
	}{
		{
			name:            "allow_response_produces_empty_stdout",
			serverResponds:  `{"action":"allow"}`,
			wantStdoutEmpty: true,
		},
		{
			name:               "deny_response_emits_block_envelope",
			serverResponds:     `{"action":"deny","reason":"secret in prompt"}`,
			wantStdoutContains: []string{`"decision":"block"`, "secret in prompt"},
		},
		{
			// Fail-closed denials must not run the fallback.
			name:               "fail_closed_transport_error_emits_block_envelope",
			endpoint:           "http://127.0.0.1:1",
			failOpen:           "false",
			wantStdoutContains: []string{`"decision":"block"`, "could not evaluate"},
		},
		{
			name:            "fail_open_transport_error_produces_empty_stdout",
			endpoint:        "http://127.0.0.1:1",
			wantStdoutEmpty: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			// Clear primary variables to avoid posting to an exported Cloud endpoint.
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", tt.failOpen)
			endpoint := tt.endpoint
			if endpoint == "" {
				endpoint = server.URL
			}
			t.Setenv("SIGIL_ENDPOINT", endpoint)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			responseBody.Store(tt.serverResponds)

			stdin := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"sess","turn_id":"turn","prompt":"my token is glc_secret","model":"gpt-5"}`)
			var stdout bytes.Buffer
			if err := Hook(context.Background(), stdin, &stdout, log.New(io.Discard, "", 0)); err != nil {
				t.Fatalf("Hook returned %v; a non-zero exit would run the sigil fallback", err)
			}

			if tt.wantStdoutEmpty && stdout.Len() != 0 {
				t.Errorf("stdout not empty: %q", stdout.String())
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout = %q, want substring %q", stdout.String(), want)
				}
			}
		})
	}
}

func newDispatcherTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable in this sandbox: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}
