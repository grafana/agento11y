package guard

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
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
		// useClosedServer forces a transport failure.
		useClosedServer bool
		// clearCreds blanks the SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/
		// SIGIL_AUTH_TOKEN env vars before the call.
		clearCreds bool
		// clearCredsKeepEndpoint clears tenant/token but keeps the (local)
		// endpoint set, to exercise the LocalAuthPlaceholders path.
		clearCredsKeepEndpoint    bool
		toolName                  string
		toolCallID                string
		conversationID            string
		wantConversationID        string
		wantConversationIDOmitted bool
		wantAction                agento11y.HookAction
		wantReasonSub             string
		wantNoReasonSub           string
		wantRuleID                string
		wantUpdatedInput          string
		wantLogSub                string
		wantNoLogSub              string
		wantServerCalled          bool
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
			name:               "allow response from sigil",
			cfg:                envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:     `{"action":"allow"}`,
			toolName:           "bash",
			conversationID:     " session-1 ",
			wantConversationID: "session-1",
			wantAction:         agento11y.HookActionAllow,
			wantLogSub:         "transform_applied=false",
			wantServerCalled:   true,
		},
		{
			// An empty output is treated the same as no transform at all, so it
			// must stay silent rather than emit the no-match line. Mirrors pi.
			name:                      "empty transform output is ignored",
			cfg:                       envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:            `{"action":"allow","transformed_input":{"output":[]}}`,
			toolName:                  "bash",
			conversationID:            "   ",
			wantConversationIDOmitted: true,
			wantAction:                agento11y.HookActionAllow,
			wantNoLogSub:              "tool-call transform present but no part matched",
			wantServerCalled:          true,
		},
		{
			name:             "deny response from sigil",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","reason":"blocked tool","rule_id":"r-1"}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "blocked tool",
			wantRuleID:       "r-1",
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
			wantRuleID:      EvaluationFailureRuleID,
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
			wantRuleID:    EvaluationFailureRuleID,
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
			name:             "mismatched id is ignored when it is the only rewritten call",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"bash","input_json":{"cmd":"echo [REDACTED]"}}}]}]}}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionAllow,
			wantLogSub:       "tool-call transform present but no part matched tu_1",
			wantServerCalled: true,
		},
		{
			name:             "empty id still applies when it is the only rewritten call",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"bash","input_json":{"cmd":"echo [REDACTED]"}}}]}]}}`,
			toolName:         "bash",
			toolCallID:       " ",
			wantAction:       agento11y.HookActionAllow,
			wantUpdatedInput: `{"cmd":"echo [REDACTED]"}`,
			wantLogSub:       "transform_applied=true",
			wantServerCalled: true,
		},
		{
			name:             "transform among several same-name calls is not guessed by id miss",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"tu_other","name":"bash","input_json":{"cmd":"echo X"}}},{"kind":"tool_call","tool_call":{"id":"tu_also","name":"bash","input_json":{"cmd":"echo Y"}}}]}]}}`,
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
			wantRuleID:       "r-1",
			wantServerCalled: true,
		},
		{
			// No rule ran, so the reason must reach the user as written.
			name:             "evaluation-failure deny is not reported as a policy deny",
			cfg:              envconfig.GuardsConfig{Enabled: true, TimeoutMs: 1500, FailOpen: true},
			serverResponds:   `{"action":"deny","rule_id":"` + EvaluationFailureRuleID + `","reason":"` + "agento11y could not evaluate the guard" + `"}`,
			toolName:         "bash",
			wantAction:       agento11y.HookActionDeny,
			wantReasonSub:    "could not evaluate",
			wantNoReasonSub:  "policy blocked",
			wantRuleID:       EvaluationFailureRuleID,
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
			var capturedBody atomic.Value
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				requestBody, _ := io.ReadAll(r.Body)
				capturedBody.Store(requestBody)
				responseBody := tt.serverResponds
				if responseBody == "" {
					responseBody = `{"action":"allow"}`
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(responseBody))
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
			toolCallID := tt.toolCallID
			if toolCallID == "" {
				toolCallID = "tu_1"
			}
			res := EvaluateToolCall(context.Background(), tt.cfg, ToolCallInput{
				AgentName:      "copilot",
				AgentVersion:   "dev",
				ModelProvider:  "openai",
				ModelName:      "gpt-4",
				ConversationID: tt.conversationID,
				ToolName:       tt.toolName,
				ToolCallID:     toolCallID,
				ToolInputJSON:  json.RawMessage(`{"cmd":"echo hi"}`),
			}, log.New(&logBuf, "", 0))

			if res.Action != tt.wantAction {
				t.Fatalf("Action = %q, want %q", res.Action, tt.wantAction)
			}
			if tt.wantReasonSub != "" && !strings.Contains(res.Reason, tt.wantReasonSub) {
				t.Fatalf("Reason = %q, want substring %q", res.Reason, tt.wantReasonSub)
			}
			if tt.wantNoReasonSub != "" && strings.Contains(res.Reason, tt.wantNoReasonSub) {
				t.Fatalf("Reason = %q, must not contain %q", res.Reason, tt.wantNoReasonSub)
			}
			if res.RuleID != tt.wantRuleID {
				t.Errorf("RuleID = %q, want %q", res.RuleID, tt.wantRuleID)
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
			if tt.wantConversationID != "" || tt.wantConversationIDOmitted {
				raw, _ := capturedBody.Load().([]byte)
				var req struct {
					Context map[string]json.RawMessage `json:"context"`
				}
				if err := json.Unmarshal(raw, &req); err != nil {
					t.Fatalf("decode request body: %v\n%s", err, raw)
				}
				conversationID, ok := req.Context["conversation_id"]
				if tt.wantConversationIDOmitted {
					if ok {
						t.Errorf("blank conversation ID serialized as %s", conversationID)
					}
				} else if !ok || string(conversationID) != strconv.Quote(tt.wantConversationID) {
					t.Errorf("conversation_id = %s, want %q", conversationID, tt.wantConversationID)
				}
			}
		})
	}
}

func TestExtractToolCallTransform(t *testing.T) {
	const redacted = `{"command":"echo [REDACTED]"}`
	const other = `{"command":"other call"}`
	call := func(id, name, raw string) agento11y.ToolCall {
		return agento11y.ToolCall{ID: id, Name: name, InputJSON: json.RawMessage(raw)}
	}
	tests := []struct {
		name       string
		id         string
		tool       string
		calls      []agento11y.ToolCall
		want       string
		wantLogSub string
	}{
		{"no calls", "c1", "bash", nil, "", ""},
		{"conflicting ID with unrelated call", "c1", "bash", []agento11y.ToolCall{call("c2", "bash", redacted), call("c3", "read", other)}, "", "no part matched"},
		{"wrong name with unrelated call", "c1", "bash", []agento11y.ToolCall{call("", "write", redacted), call("c3", "read", other)}, "", "no part matched"},
		{"ID-less match with unrelated call", "c1", "bash", []agento11y.ToolCall{call("", "bash", redacted), call("c3", "read", other)}, redacted, ""},
		{"name-less match with unrelated call", "c1", "bash", []agento11y.ToolCall{call("", "", redacted), call("c3", "read", other)}, redacted, ""},
		{"caller ID absent with unrelated call", "", "bash", []agento11y.ToolCall{call("c2", "bash", redacted), call("c3", "read", other)}, redacted, ""},
		{"caller name absent with unrelated ID", "c1", "", []agento11y.ToolCall{call("", "write", redacted), call("c3", "read", other)}, redacted, ""},
		{"whitespace name cannot override conflicting ID", "c1", "bash", []agento11y.ToolCall{call("c2", " Bash ", redacted), call("c3", "read", other)}, "", "no part matched"},
		{"whitespace name matches without conflicting ID", "c1", "bash", []agento11y.ToolCall{call("", " Bash ", redacted), call("c3", "read", other)}, redacted, ""},
		{"duplicate fallback", "c1", "bash", []agento11y.ToolCall{call("", "bash", redacted), call("", "bash", redacted)}, "", "no part matched"},
		{"missing name makes fallback ambiguous", "c1", "bash", []agento11y.ToolCall{call("", "bash", redacted), call("", "", other)}, "", "no part matched"},
		{"missing caller ID makes same names ambiguous", "", "bash", []agento11y.ToolCall{call("c2", "bash", redacted), call("c3", "bash", other)}, "", "no part matched"},
		{"missing caller identity is ambiguous", "", "", []agento11y.ToolCall{call("c2", "bash", redacted), call("c3", "read", other)}, "", "no part matched"},
		{"conflicting ID does not compete with fallback", "c1", "bash", []agento11y.ToolCall{call("c2", "bash", other), call("", "bash", redacted)}, redacted, ""},
		{"exact ID precedes fallback and name", "c1", "bash", []agento11y.ToolCall{call("", "bash", other), call("c1", "write", redacted)}, redacted, ""},
		{"trimmed exact ID precedes fallback", " c1 ", "bash", []agento11y.ToolCall{call("", "bash", other), call(" c1 ", "", redacted)}, redacted, ""},
		{"duplicate exact ID", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", redacted), call("c1", "bash", redacted)}, "", "no part matched"},
		{"duplicate trimmed ID with different names", "c1", "bash", []agento11y.ToolCall{call(" c1 ", "read", other), call("c1", "bash", redacted), call("", "bash", other)}, "", "no part matched"},
		{"invalid exact arguments cannot use fallback", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "not json"), call("", "bash", redacted)}, "", "invalid JSON arguments"},
		{"invalid duplicate exact arguments remain ambiguous", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "not json"), call("c1", "bash", redacted)}, "", "no part matched"},
		{"invalid fallback arguments remain ambiguous", "c1", "bash", []agento11y.ToolCall{call("", "bash", "not json"), call("", "bash", redacted)}, "", "no part matched"},
		{"invalid unrelated arguments are ignored", "c1", "bash", []agento11y.ToolCall{call("c2", "bash", "not json"), call("", "bash", redacted)}, redacted, ""},
		{"malformed JSON", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "not json")}, "", "invalid JSON arguments"},
		{"empty arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "")}, "", "empty arguments"},
		{"array arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "[]")}, "", "not a JSON object"},
		{"null arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "null")}, "", "empty arguments"},
		{"number arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "42")}, "", "not a JSON object"},
		{"boolean arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "true")}, "", "not a JSON object"},
		{"empty object arguments", "c1", "bash", []agento11y.ToolCall{call("c1", "bash", "{}")}, "{}", ""},
	}
	ids := []struct {
		name, caller, response string
		exact, compatible      bool
	}{
		{"exact ID", "c1", "c1", true, true},
		{"trimmed exact ID", " c1 ", "\tc1 ", true, true},
		{"conflicting ID", "c1", "c2", false, false},
		{"trimmed conflicting ID", " c1 ", " c2 ", false, false},
		{"case-sensitive ID", "c1", "C1", false, false},
		{"response ID absent", "c1", "", false, true},
		{"response ID whitespace", "c1", " \t", false, true},
		{"caller ID absent", "", "c2", false, true},
		{"caller ID whitespace", " \t", "c2", false, true},
		{"both IDs absent", "", "", false, true},
		{"both IDs whitespace", " \t", " \t", false, true},
	}
	names := []struct {
		name, caller, response string
		compatible             bool
	}{
		{"same name", "bash", "bash", true},
		{"trimmed case-insensitive name", " bash ", "\tBash ", true},
		{"different name", "bash", "write", false},
		{"response name absent", "bash", "", true},
		{"response name whitespace", "bash", " \t", true},
		{"caller name absent", "", "write", true},
		{"caller name whitespace", " \t", "write", true},
		{"both names absent", "", "", true},
		{"both names whitespace", " \t", " \t", true},
	}
	for _, id := range ids {
		for _, name := range names {
			t.Run(id.name+"/"+name.name, func(t *testing.T) {
				resp := &agento11y.HookEvaluateResponse{TransformedInput: &agento11y.HookInput{Output: []agento11y.Message{{
					Role: agento11y.RoleAssistant,
					Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{
						ID: id.response, Name: name.response, InputJSON: json.RawMessage(redacted),
					}}},
				}}}}
				want := ""
				if id.exact || (id.compatible && name.compatible) {
					want = redacted
				}
				if got := ExtractToolCallTransform(resp, id.caller, name.caller, nil); string(got) != want {
					t.Errorf("transform = %s, want %s", got, want)
				}
			})
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, reverse := range []bool{false, true} {
				var output []agento11y.Message
				for i := range tt.calls {
					if reverse {
						i = len(tt.calls) - 1 - i
					}
					output = append(output, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{
						{Kind: agento11y.PartKindText, Text: "unrelated text"},
						{Kind: agento11y.PartKindToolCall, ToolCall: &tt.calls[i]},
					}})
				}
				resp := &agento11y.HookEvaluateResponse{TransformedInput: &agento11y.HookInput{Output: output}}
				var logs bytes.Buffer
				got := ExtractToolCallTransform(resp, tt.id, tt.tool, log.New(&logs, "", 0))
				if string(got) != tt.want {
					t.Errorf("reverse=%t: transform = %s, want %s", reverse, got, tt.want)
				}
				if tt.wantLogSub != "" && !strings.Contains(logs.String(), tt.wantLogSub) {
					t.Errorf("logs missing %q: %s", tt.wantLogSub, logs.String())
				}
				if tt.want == "" && strings.Contains(logs.String(), "applied") {
					t.Errorf("rejected transform logged as applied: %s", logs.String())
				}
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
