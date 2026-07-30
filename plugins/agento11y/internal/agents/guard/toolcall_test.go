package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestEvaluateToolCall(t *testing.T) {
	tests := []struct {
		name           string
		cfg            envconfig.GuardsConfig
		serverResponds string
		// useClosedServer makes the helper connect to a closed listener so
		// the request fails at transport.
		useClosedServer bool
		// clearCreds blanks the SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/
		// SIGIL_AUTH_TOKEN env vars before the call.
		clearCreds bool
		// clearCredsKeepEndpoint clears tenant/token but keeps the (local)
		// endpoint set, to exercise the LocalAuthPlaceholders path.
		clearCredsKeepEndpoint bool
		toolName               string
		wantAction             agento11y.HookAction
		wantReasonSub          string
		wantUpdatedInput       string
		wantLogSub             string
		wantNoLogSub           string
		wantServerCalled       bool
	}{
		{
			name:       "disabled returns allow without contacting sigil",
			cfg:        envconfig.GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			toolName:   "bash",
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "empty tool name returns allow",
			cfg:        envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			toolName:   "",
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:             "allow response from sigil",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow"}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "transform_applied=false",
			wantServerCalled: true,
		},
		{
			// An empty output is treated the same as no transform at all, so it
			// must stay silent rather than emit the no-match line. Mirrors pi.
			name:             "empty transform output is ignored",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantNoLogSub:     "tool-call transform present but no part matched",
			wantServerCalled: true,
		},
		{
			name:             "deny response from sigil",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","reason":"blocked tool","rule_id":"r-1"}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "blocked tool",
			wantServerCalled: true,
		},
		{
			name:            "transport error fails open",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			useClosedServer: true,
			toolName:        "bash",
			wantAction:      agento11y.HookActionAllow,
		},
		{
			name:            "transport error fails closed",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			useClosedServer: true,
			toolName:        "bash",
			wantAction:      agento11y.HookActionDeny,
			wantReasonSub:   "could not evaluate",
		},
		{
			name:          "missing credentials fail open",
			cfg:           envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			clearCreds:    true,
			toolName:      "bash",
			wantAction:    agento11y.HookActionAllow,
			wantReasonSub: "",
		},
		{
			name:          "missing credentials fail closed",
			cfg:           envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			clearCreds:    true,
			toolName:      "bash",
			wantAction:    agento11y.HookActionDeny,
			wantReasonSub: "missing AGENTO11Y_ENDPOINT",
		},
		{
			name:             "allow with transform returns redacted args",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":{"cmd":"echo [REDACTED]"}}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantUpdatedInput: `{"cmd":"echo [REDACTED]"}`,
			wantLogSub:       "transform_applied=true",
			wantServerCalled: true,
		},
		{
			// The Agent Observability server marshals the proto bytes input_json field with
			// encoding/json, so the real wire value is a base64 JSON string.
			name:             "allow with base64 string transform decodes server wire format",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":"eyJjb21tYW5kIjoiZWNobyBbUkVEQUNURURdIn0="}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantUpdatedInput: `{"command":"echo [REDACTED]"}`,
			wantServerCalled: true,
		},
		{
			name:             "transform for different tool call id is ignored",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"bash","input_json":{"cmd":"echo X"}}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "tool-call transform present but no part matched tu_1",
			wantServerCalled: true,
		},
		{
			name:             "invalid transform JSON is dropped",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":"not json"}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "tool-call transform for tu_1 dropped: invalid JSON arguments",
			wantServerCalled: true,
		},
		{
			name:             "non-object transform arguments are dropped",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":[1,2]}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "tool-call transform for tu_1 dropped: arguments were not a JSON object",
			wantServerCalled: true,
		},
		{
			name:             "empty transform arguments are dropped",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":""}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "tool-call transform for tu_1 dropped: empty arguments",
			wantServerCalled: true,
		},
		{
			name:             "deny with transform stays deny without transform",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","reason":"blocked tool","rule_id":"r-1","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"bash","input_json":{"cmd":"echo [REDACTED]"}}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "blocked tool",
			wantServerCalled: true,
		},
		{
			// httptest servers bind to a loopback address, which IsLocalEndpoint
			// classifies as local. With empty creds we expect placeholder auth so
			// the request proceeds instead of being rejected as missing-creds.
			name:                   "local endpoint with empty creds applies placeholders",
			cfg:                    envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			clearCredsKeepEndpoint: true,
			serverResponds:         `{"action":"allow"}`,
			toolName:               "bash",
			wantAction:             agento11y.HookActionAllow,
			wantServerCalled:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				body := tt.serverResponds
				if body == "" {
					body = `{"action":"allow"}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()

			closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			closed.Close()

			endpoint := server.URL
			if tt.useClosedServer {
				endpoint = closed.URL
			}
			switch {
			case tt.clearCreds:
				t.Setenv("SIGIL_ENDPOINT", "")
				t.Setenv("SIGIL_AUTH_TENANT_ID", "")
				t.Setenv("SIGIL_AUTH_TOKEN", "")
			case tt.clearCredsKeepEndpoint:
				t.Setenv("SIGIL_ENDPOINT", endpoint)
				t.Setenv("SIGIL_AUTH_TENANT_ID", "")
				t.Setenv("SIGIL_AUTH_TOKEN", "")
			default:
				t.Setenv("SIGIL_ENDPOINT", endpoint)
				t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
				t.Setenv("SIGIL_AUTH_TOKEN", "token")
			}

			var logBuf bytes.Buffer
			res := EvaluateToolCall(context.Background(), tt.cfg, ToolCallInput{
				AgentName:     "copilot",
				AgentVersion:  "dev",
				ModelProvider: "openai",
				ModelName:     "gpt-4",
				ToolName:      tt.toolName,
				ToolCallID:    "tu_1",
				ToolInputJSON: json.RawMessage(`{"cmd":"echo hi"}`),
			}, log.New(&logBuf, "", 0))

			if res.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", res.Action, tt.wantAction)
			}
			if tt.wantReasonSub != "" && !strings.Contains(res.Reason, tt.wantReasonSub) {
				t.Fatalf("Reason = %q, want substring %q", res.Reason, tt.wantReasonSub)
			}
			if string(res.UpdatedInputJSON) != tt.wantUpdatedInput {
				t.Errorf("UpdatedInputJSON = %q, want %q", res.UpdatedInputJSON, tt.wantUpdatedInput)
			}
			if tt.wantLogSub != "" && !strings.Contains(logBuf.String(), tt.wantLogSub) {
				t.Errorf("logs missing %q:\n%s", tt.wantLogSub, logBuf.String())
			}
			if tt.wantNoLogSub != "" && strings.Contains(logBuf.String(), tt.wantNoLogSub) {
				t.Errorf("logs contain %q but should not:\n%s", tt.wantNoLogSub, logBuf.String())
			}
			if tt.wantServerCalled && calls.Load() == 0 {
				t.Errorf("expected sigil hook server to be called, got 0 calls")
			}
			if !tt.wantServerCalled && !tt.useClosedServer && calls.Load() != 0 {
				t.Errorf("did not expect sigil hook server call, got %d", calls.Load())
			}
		})
	}
}

func TestWriteHookSpecificOutputDeny(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		wantSub []string
	}{
		{
			name:    "explicit reason",
			reason:  "blocked by rule r-1",
			wantSub: []string{`"hookEventName":"PreToolUse"`, `"permissionDecision":"deny"`, `"permissionDecisionReason":"blocked by rule r-1"`},
		},
		{
			name:    "blank reason falls back to generic",
			reason:  "",
			wantSub: []string{`"permissionDecisionReason":"tool call denied by agento11y guard"`},
		},
		{
			name:    "whitespace reason falls back to generic",
			reason:  "   ",
			wantSub: []string{`"permissionDecisionReason":"tool call denied by agento11y guard"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			WriteHookSpecificOutputDeny(&buf, tt.reason)
			got := buf.String()
			for _, want := range tt.wantSub {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\nfull output: %s", want, got)
				}
			}
		})
	}

	// nil writer must not panic.
	WriteHookSpecificOutputDeny(nil, "x")
}

func TestWriteHookSpecificOutputUpdatedInput(t *testing.T) {
	tests := []struct {
		name         string
		updatedInput json.RawMessage
		want         string
	}{
		{
			name:         "object arguments",
			updatedInput: json.RawMessage(`{"command":"echo [REDACTED]"}`),
			want:         `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"allow","updatedInput":{"command":"echo [REDACTED]"}}}` + "\n",
		},
		{
			name:         "empty input writes nothing",
			updatedInput: nil,
			want:         "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			WriteHookSpecificOutputUpdatedInput(&buf, tt.updatedInput)
			if got := buf.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}

	// nil writer must not panic.
	WriteHookSpecificOutputUpdatedInput(nil, json.RawMessage(`{}`))
}
