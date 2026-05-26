package guard

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
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
		clearCreds       bool
		toolName         string
		wantAction       sigil.HookAction
		wantReasonSub    string
		wantServerCalled bool
	}{
		{
			name:       "disabled returns allow without contacting sigil",
			cfg:        envconfig.GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			toolName:   "bash",
			wantAction: sigil.HookActionAllow,
		},
		{
			name:       "empty tool name returns allow",
			cfg:        envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			toolName:   "",
			wantAction: sigil.HookActionAllow,
		},
		{
			name:             "allow response from sigil",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow"}`,
			toolName:         "bash",
			wantAction:       sigil.HookActionAllow,
			wantServerCalled: true,
		},
		{
			name:             "deny response from sigil",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","reason":"blocked tool","rule_id":"r-1"}`,
			toolName:         "bash",
			wantAction:       sigil.HookActionDeny,
			wantReasonSub:    "blocked tool",
			wantServerCalled: true,
		},
		{
			name:            "transport error fails open",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			useClosedServer: true,
			toolName:        "bash",
			wantAction:      sigil.HookActionAllow,
		},
		{
			name:            "transport error fails closed",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			useClosedServer: true,
			toolName:        "bash",
			wantAction:      sigil.HookActionDeny,
			wantReasonSub:   "sigil guard evaluation failed",
		},
		{
			name:          "missing credentials fail open",
			cfg:           envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			clearCreds:    true,
			toolName:      "bash",
			wantAction:    sigil.HookActionAllow,
			wantReasonSub: "",
		},
		{
			name:          "missing credentials fail closed",
			cfg:           envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			clearCreds:    true,
			toolName:      "bash",
			wantAction:    sigil.HookActionDeny,
			wantReasonSub: "missing SIGIL_ENDPOINT",
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
			if tt.clearCreds {
				t.Setenv("SIGIL_ENDPOINT", "")
				t.Setenv("SIGIL_AUTH_TENANT_ID", "")
				t.Setenv("SIGIL_AUTH_TOKEN", "")
			} else {
				t.Setenv("SIGIL_ENDPOINT", endpoint)
				t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
				t.Setenv("SIGIL_AUTH_TOKEN", "token")
			}

			res := EvaluateToolCall(context.Background(), tt.cfg, ToolCallInput{
				AgentName:     "copilot",
				AgentVersion:  "dev",
				ModelProvider: "openai",
				ModelName:     "gpt-4",
				ToolName:      tt.toolName,
				ToolCallID:    "tu_1",
				ToolInputJSON: json.RawMessage(`{"cmd":"echo hi"}`),
			}, log.New(io.Discard, "", 0))

			if res.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", res.Action, tt.wantAction)
			}
			if tt.wantReasonSub != "" && !strings.Contains(res.Reason, tt.wantReasonSub) {
				t.Fatalf("Reason = %q, want substring %q", res.Reason, tt.wantReasonSub)
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
