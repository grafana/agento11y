package guard

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

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestEvaluatePrompt(t *testing.T) {
	tests := []struct {
		name           string
		cfg            envconfig.GuardsConfig
		serverResponds string
		// useClosedServer forces a transport failure.
		useClosedServer  bool
		clearCreds       bool
		prompt           string
		wantAction       agento11y.HookAction
		wantReasonSub    string
		wantNoReasonSub  string
		wantRuleID       string
		wantServerCalled bool
	}{
		{
			name:       "disabled returns allow without contacting the server",
			cfg:        envconfig.GuardsConfig{Enabled: false, TimeoutMs: 1500, FailOpen: true},
			prompt:     "hello",
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "blank prompt returns allow",
			cfg:        envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			prompt:     "   ",
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:             "allow response",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow"}`,
			prompt:           "hello",
			wantAction:       agento11y.HookActionAllow,
			wantServerCalled: true,
		},
		{
			// These hosts cannot apply prompt transforms.
			name:             "allow with transformed input is ignored",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"messages":[{"role":"user","parts":[{"kind":"text","text":"[REDACTED]"}]}]}}`,
			prompt:           "token glc_x",
			wantAction:       agento11y.HookActionAllow,
			wantServerCalled: true,
		},
		{
			name:             "deny response",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","reason":"secret in prompt","rule_id":"r-1"}`,
			prompt:           "hello",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "secret in prompt",
			wantRuleID:       "r-1",
			wantServerCalled: true,
		},
		{
			name:             "deny with empty reason keeps the prefix",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny"}`,
			prompt:           "hello",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "policy blocked this message",
			wantNoReasonSub:  "Reason:",
			wantServerCalled: true,
		},
		{
			name:             "evaluation-failure deny is not reported as a policy deny",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","rule_id":"` + EvaluationFailureRuleID + `","reason":"agento11y could not evaluate the guard"}`,
			prompt:           "hello",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "could not evaluate",
			wantNoReasonSub:  "policy blocked",
			wantRuleID:       EvaluationFailureRuleID,
			wantServerCalled: true,
		},
		{
			name:            "transport error fails open",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			useClosedServer: true,
			prompt:          "hello",
			wantAction:      agento11y.HookActionAllow,
		},
		{
			name:            "transport error fails closed",
			cfg:             envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			useClosedServer: true,
			prompt:          "hello",
			wantAction:      agento11y.HookActionDeny,
			wantReasonSub:   "could not evaluate",
			wantRuleID:      EvaluationFailureRuleID,
		},
		{
			name:       "missing credentials fail open",
			cfg:        envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			clearCreds: true,
			prompt:     "hello",
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:          "missing credentials fail closed",
			cfg:           envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: false},
			clearCreds:    true,
			prompt:        "hello",
			wantAction:    agento11y.HookActionDeny,
			wantReasonSub: "missing AGENTO11Y_ENDPOINT",
			wantRuleID:    EvaluationFailureRuleID,
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

			// Clear primary variables to avoid posting to an exported Cloud endpoint.
			envconfig.PinAliasEnvBlank(t)

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

			var logBuf bytes.Buffer
			res := EvaluatePrompt(context.Background(), tt.cfg, PromptInput{
				AgentName:     "codex",
				AgentVersion:  "dev",
				ModelProvider: "openai",
				ModelName:     "gpt-5",
				Prompt:        tt.prompt,
			}, log.New(&logBuf, "", 0))

			if res.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", res.Action, tt.wantAction)
			}
			if res.Blocked() != (tt.wantAction == agento11y.HookActionDeny) {
				t.Errorf("Blocked() = %t for action %q", res.Blocked(), res.Action)
			}
			if tt.wantReasonSub != "" && !strings.Contains(res.Reason, tt.wantReasonSub) {
				t.Errorf("Reason = %q, want substring %q", res.Reason, tt.wantReasonSub)
			}
			if tt.wantNoReasonSub != "" && strings.Contains(res.Reason, tt.wantNoReasonSub) {
				t.Errorf("Reason = %q, must not contain %q", res.Reason, tt.wantNoReasonSub)
			}
			if res.RuleID != tt.wantRuleID {
				t.Errorf("RuleID = %q, want %q", res.RuleID, tt.wantRuleID)
			}
			if tt.wantServerCalled && calls.Load() == 0 {
				t.Errorf("expected hook server to be called, got 0 calls")
			}
			if !tt.wantServerCalled && !tt.useClosedServer && calls.Load() != 0 {
				t.Errorf("did not expect a hook server call, got %d", calls.Load())
			}
		})
	}
}

// TestEvaluatePrompt_SendsOneUserMessage pins the preflight request to one
// user text part. The server flattens all messages and roles when applying rules.
func TestEvaluatePrompt_SendsOneUserMessage(t *testing.T) {
	var capturedPath, capturedBody atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedPath.Store(r.URL.Path)
		body, _ := io.ReadAll(r.Body)
		capturedBody.Store(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"action":"allow"}`))
	}))
	defer server.Close()

	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_ENDPOINT", server.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")

	EvaluatePrompt(context.Background(), envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true}, PromptInput{
		AgentName:      "cursor",
		ConversationID: " session-1 ",
		Prompt:         "my token is glc_secret",
	}, log.New(bytes.NewBuffer(nil), "", 0))

	if path, _ := capturedPath.Load().(string); path != "/api/v1/hooks:evaluate" {
		t.Errorf("path = %q, want /api/v1/hooks:evaluate", path)
	}
	raw, _ := capturedBody.Load().([]byte)
	if len(raw) == 0 {
		t.Fatal("captured body is empty")
	}
	var req struct {
		Phase   string `json:"phase"`
		Context struct {
			AgentName      string `json:"agent_name"`
			ConversationID string `json:"conversation_id"`
			Model          *struct {
				Provider string `json:"provider"`
				Name     string `json:"name"`
			} `json:"model"`
		} `json:"context"`
		Input struct {
			Messages []struct {
				Role  string `json:"role"`
				Parts []struct {
					Kind string `json:"kind"`
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"messages"`
			Output []json.RawMessage `json:"output"`
		} `json:"input"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal body: %v\n%s", err, raw)
	}
	if req.Phase != "preflight" {
		t.Errorf("phase = %q, want preflight", req.Phase)
	}
	if req.Context.AgentName != "cursor" {
		t.Errorf("agent_name = %q, want cursor", req.Context.AgentName)
	}
	if req.Context.ConversationID != "session-1" {
		t.Errorf("conversation_id = %q, want session-1", req.Context.ConversationID)
	}
	if req.Context.Model == nil || req.Context.Model.Provider != "unknown" || req.Context.Model.Name != "unknown" {
		t.Errorf("model = %+v, want unknown/unknown", req.Context.Model)
	}
	if len(req.Input.Output) != 0 {
		t.Errorf("input.output = %v, want absent", req.Input.Output)
	}
	if len(req.Input.Messages) != 1 {
		t.Fatalf("input.messages has %d entries, want 1", len(req.Input.Messages))
	}
	msg := req.Input.Messages[0]
	if msg.Role != "user" {
		t.Errorf("role = %q, want user", msg.Role)
	}
	if len(msg.Parts) != 1 {
		t.Fatalf("message has %d parts, want 1", len(msg.Parts))
	}
	if msg.Parts[0].Kind != "text" || msg.Parts[0].Text != "my token is glc_secret" {
		t.Errorf("part = %+v, want the submitted prompt as one text part", msg.Parts[0])
	}

	EvaluatePrompt(context.Background(), envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true}, PromptInput{
		AgentName:      "cursor",
		ConversationID: "   ",
		Prompt:         "another prompt",
	}, log.New(bytes.NewBuffer(nil), "", 0))
	raw, _ = capturedBody.Load().([]byte)
	var blankIDReq struct {
		Context map[string]json.RawMessage `json:"context"`
	}
	if err := json.Unmarshal(raw, &blankIDReq); err != nil {
		t.Fatalf("unmarshal blank-id body: %v\n%s", err, raw)
	}
	if _, ok := blankIDReq.Context["conversation_id"]; ok {
		t.Errorf("blank conversation ID serialized in context: %s", raw)
	}
}

func TestWritePromptBlock(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		want   string
	}{
		{
			name:   "explicit reason",
			reason: "blocked by rule r-1",
			want:   `{"decision":"block","reason":"blocked by rule r-1"}` + "\n",
		},
		{
			name:   "blank reason falls back to generic",
			reason: "  ",
			want:   `{"decision":"block","reason":"message blocked by agento11y guard"}` + "\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			WritePromptBlock(&buf, tt.reason)
			if got := buf.String(); got != tt.want {
				t.Errorf("output = %q, want %q", got, tt.want)
			}
		})
	}

	// nil writer must not panic.
	WritePromptBlock(nil, "x")
}
