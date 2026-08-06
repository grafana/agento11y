package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// TestPreToolUse covers the Cursor-specific wiring around the shared guard
// helper: the handler always writes exactly one flat preToolUse response —
// permissive on disabled/allow/unusable-transform/fail-open, deny with the
// policy reason in agent_message, allow+updated_input on a usable Transform.
// Deep behaviour around fail-open/closed, missing credentials, transport
// errors, and transform extraction lives in the guard package tests; this
// test only verifies the integration shape.
func TestPreToolUse(t *testing.T) {
	const permissive = `{"permission":"allow"}` + "\n"

	tests := []struct {
		name string
		env  map[string]string
		// serverResponds is the JSON the fake Agent Observability hook server returns;
		// empty means allow.
		serverResponds string
		// useClosedServer points SIGIL_ENDPOINT at a closed listener so the
		// evaluation fails at transport.
		useClosedServer    bool
		expectServerCall   bool
		wantStdout         string
		wantStdoutContains []string
	}{
		{
			name:       "disabled_by_default_responds_permissive",
			wantStdout: permissive,
		},
		{
			name:             "enabled_allow_responds_permissive",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow"}`,
			expectServerCall: true,
			wantStdout:       permissive,
		},
		{
			name:             "enabled_deny_writes_deny_with_agent_message",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"deny","reason":"blocked tool"}`,
			expectServerCall: true,
			wantStdoutContains: []string{
				`"permission":"deny"`,
				`"agent_message"`,
				`A Grafana Agent Observability policy`,
				`blocked tool`,
			},
		},
		{
			name:             "enabled_allow_transform_writes_updated_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Shell","input_json":{"command":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall: true,
			wantStdout:       `{"permission":"allow","updated_input":{"command":"echo [REDACTED]"}}` + "\n",
		},
		{
			name:             "enabled_allow_transform_dropping_command_keeps_original_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Shell","input_json":{"args":"echo [REDACTED]"}}}]}]}}`,
			expectServerCall: true,
			wantStdout:       permissive,
		},
		{
			name:             "enabled_allow_transform_with_null_command_keeps_original_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_1","name":"Shell","input_json":{"command":null}}}]}]}}`,
			expectServerCall: true,
			wantStdout:       permissive,
		},
		{
			name:             "enabled_allow_unusable_transform_keeps_original_input",
			env:              map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"Shell","input_json":{"command":"echo X"}}}]}]}}`,
			expectServerCall: true,
			wantStdout:       permissive,
		},
		{
			name:            "transport_error_fail_open_responds_permissive",
			env:             map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			useClosedServer: true,
			wantStdout:      permissive,
		},
		{
			name:            "transport_error_fail_closed_denies",
			env:             map[string]string{"SIGIL_GUARDS_ENABLED": "true", "SIGIL_GUARDS_FAIL_OPEN": "false"},
			useClosedServer: true,
			wantStdoutContains: []string{
				`"permission":"deny"`,
				`"agent_message"`,
				`could not evaluate`,
			},
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

			// Blank every branded variable first: config.Load below reads the
			// process env, and an ambient endpoint would send this guard call to
			// a real backend.
			envconfig.PinAliasEnvBlank(t)
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			t.Setenv("SIGIL_ENDPOINT", endpoint)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			var stdout bytes.Buffer
			PreToolUse(context.Background(), Payload{
				HookEventName:  "preToolUse",
				ConversationID: "conv",
				GenerationID:   "gen",
				CursorVersion:  "1.2.3",
				Model:          "claude-sonnet-4",
				ToolName:       "Shell",
				ToolUseID:      "tu_1",
				ToolInput:      []byte(`{"command":"echo hi"}`),
			}, config.Load(log.New(&bytes.Buffer{}, "", 0)), &stdout, log.New(&bytes.Buffer{}, "", 0))

			if tt.expectServerCall && calls.Load() == 0 {
				t.Errorf("expected sigil hook server call, got 0")
			}
			if !tt.expectServerCall && !tt.useClosedServer && calls.Load() != 0 {
				t.Errorf("expected no sigil hook server call, got %d", calls.Load())
			}
			if tt.wantStdout != "" && stdout.String() != tt.wantStdout {
				t.Errorf("stdout = %q, want %q", stdout.String(), tt.wantStdout)
			}
			for _, want := range tt.wantStdoutContains {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("stdout missing %q\nfull output: %s", want, stdout.String())
				}
			}
		})
	}
}

// TestAgentNameOverrideGuardAndExport pins the guard request and the exported
// generation to one resolved name. A rule scoped to a per-run name only matches
// that run when the two paths agree.
func TestAgentNameOverrideGuardAndExport(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{name: "unset keeps the product name", want: mapper.AgentName},
		{name: "preferred spelling", env: map[string]string{"AGENTO11Y_AGENT_NAME": "cursor-e2e"}, want: "cursor-e2e"},
		{name: "legacy spelling", env: map[string]string{"SIGIL_AGENT_NAME": "legacy-name"}, want: "legacy-name"},
		{
			name: "preferred wins over legacy",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": "preferred-name",
				"SIGIL_AGENT_NAME":     "legacy-name",
			},
			want: "preferred-name",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			logger := log.New(&bytes.Buffer{}, "", 0)

			var guardAgents, exportAgents []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if strings.Contains(r.URL.Path, "hooks:evaluate") {
					var req struct {
						Context struct {
							AgentName string `json:"agent_name"`
						} `json:"context"`
					}
					_ = json.Unmarshal(body, &req)
					guardAgents = append(guardAgents, req.Context.AgentName)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"action":"allow"}`))
					return
				}
				var req struct {
					Generations []struct {
						ID        string `json:"id"`
						AgentName string `json:"agent_name"`
					} `json:"generations"`
				}
				_ = json.Unmarshal(body, &req)
				results := make([]map[string]any, 0, len(req.Generations))
				for _, g := range req.Generations {
					exportAgents = append(exportAgents, g.AgentName)
					results = append(results, map[string]any{"generation_id": g.ID, "accepted": true})
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
			}))
			defer server.Close()

			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			t.Setenv("SIGIL_GUARDS_ENABLED", "true")
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			cfg := config.Load(logger)
			BeforeSubmit(Payload{
				HookEventName:  "beforeSubmitPrompt",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:00Z",
				Prompt:         "hello",
			}, cfg, logger)
			var stdout bytes.Buffer
			PreToolUse(context.Background(), Payload{
				HookEventName:  "preToolUse",
				ConversationID: "conv",
				GenerationID:   "gen",
				ToolName:       "Shell",
				ToolUseID:      "tu_1",
				ToolInput:      []byte(`{"command":"echo hi"}`),
			}, cfg, &stdout, logger)
			Stop(Payload{
				HookEventName:  "stop",
				ConversationID: "conv",
				GenerationID:   "gen",
				Timestamp:      "2026-04-28T12:00:05Z",
				Status:         "completed",
			}, cfg, logger)

			if len(guardAgents) != 1 || guardAgents[0] != tt.want {
				t.Fatalf("guard agent names = %v, want [%q]", guardAgents, tt.want)
			}
			if len(exportAgents) != 1 || exportAgents[0] != tt.want {
				t.Fatalf("exported agent names = %v, want [%q]", exportAgents, tt.want)
			}
		})
	}
}
