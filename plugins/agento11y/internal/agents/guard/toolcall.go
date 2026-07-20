// Package guard evaluates tool calls against Sigil guard policy and
// returns a host-neutral result. Callers translate the result into their
// own stdout response shape; convenience writers are provided for shapes
// shared by more than one host agent (e.g. WriteHookSpecificOutputDeny
// for the Claude Code / Codex PreToolUse envelope).
package guard

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// guardBehaviorHint instructs the model on how to react to a deny verdict so
// the surfaced reason is not mistaken for a generic tool failure to retry or
// work around. It is appended by both the policy-deny and fail-closed
// formatters.
const guardBehaviorHint = "Stop and tell the user this tool call was blocked, then wait for their direction before taking any other action."

// formatPolicyDeny wraps a rule-authored reason (which may be empty) into a
// self-explanatory message naming the Grafana AI Observability source, the
// blocked tool, and the expected agent behavior.
func formatPolicyDeny(toolName, reason string) string {
	msg := fmt.Sprintf("A Grafana AI Observability policy blocked the %q tool call, so it was not run.", toolName)
	if r := strings.TrimSpace(reason); r != "" {
		msg += " Reason: " + r
	}
	return msg + "\n\n" + guardBehaviorHint
}

// formatEvalFailure is the fail-closed message used when the guard could not
// be evaluated (missing credentials or transport failure). It explicitly
// distinguishes the infrastructure failure from a policy decision.
func formatEvalFailure(toolName, detail string) string {
	msg := fmt.Sprintf("Sigil could not evaluate the Grafana AI Observability guard for the %q tool call, so it was blocked as a safety measure.", toolName)
	if d := strings.TrimSpace(detail); d != "" {
		msg += " Details: " + d
	}
	return msg + "\n\n" + guardBehaviorHint
}

// ToolCallInput is the host-neutral set of fields needed to evaluate a
// tool call against Sigil guards.
type ToolCallInput struct {
	// AgentName identifies the host agent (e.g. "copilot", "claude-code").
	AgentName string
	// AgentVersion is the host agent build version, when known.
	AgentVersion string
	// ModelProvider and ModelName describe the upstream model, when known.
	ModelProvider string
	ModelName     string
	// ToolName is required.
	ToolName string
	// ToolCallID correlates the tool call with downstream telemetry, when known.
	ToolCallID string
	// ToolInputJSON is the raw JSON of the tool arguments, when available.
	ToolInputJSON json.RawMessage
}

// Result is the host-neutral outcome of a guard evaluation.
type Result struct {
	// Action is allow or deny.
	Action agento11y.HookAction
	// Reason is the deny reason from Sigil or the host-friendly description
	// of a fail-closed transport/config error.
	Reason string
	// RuleID identifies the rule that produced a deny verdict, when known.
	RuleID string
	// UpdatedInputJSON carries the transformed (redacted) tool arguments
	// when Sigil returned a usable Transform verdict for this tool call.
	// Nil when no transform applies; always nil on deny.
	UpdatedInputJSON json.RawMessage
}

// Blocked reports whether the host should refuse to execute the tool call.
func (r Result) Blocked() bool {
	return r.Action == agento11y.HookActionDeny
}

// EvaluateToolCall asks Sigil whether the tool call should proceed. It
// reads the branded ENDPOINT / AUTH_TENANT_ID / AUTH_TOKEN families (either
// spelling) from the process environment and honours envconfig.GuardsConfig
// for the enabled/timeout/fail-open knobs. Callers are expected to gate this
// helper behind cfg.Enabled — when disabled it short-circuits to allow.
//
// Behaviour:
//   - guards disabled or tool name empty: returns allow.
//   - credentials missing and fail-open: returns allow.
//   - credentials missing and fail-closed: returns deny with a credentials reason.
//   - Sigil returns allow: returns allow. When the response carries a
//     transformed_input with redacted arguments for this tool call,
//     UpdatedInputJSON is set so the host can rewrite the tool input.
//   - Sigil returns deny: returns deny with the rule reason.
//   - transport error and fail-open: returns allow (matches SDK behaviour).
//   - transport error and fail-closed: returns deny with a transport reason.
//
// A local-mode endpoint (http://127.0.0.1, http://localhost, http://[::1])
// uses stand-in placeholder credentials for the credential check so that
// running against a local Sigil instance does not require real cloud creds.
func EvaluateToolCall(ctx context.Context, cfg envconfig.GuardsConfig, in ToolCallInput, logger *log.Logger) Result {
	if !cfg.Enabled {
		return Result{Action: agento11y.HookActionAllow}
	}
	if strings.TrimSpace(in.ToolName) == "" {
		return Result{Action: agento11y.HookActionAllow}
	}

	endpoint := envconfig.Getenv("ENDPOINT")
	tenantID := envconfig.Getenv("AUTH_TENANT_ID")
	authToken := envconfig.Getenv("AUTH_TOKEN")
	tenantID, authToken = envconfig.LocalAuthPlaceholders(endpoint, tenantID, authToken)
	if endpoint == "" || tenantID == "" || authToken == "" {
		if cfg.FailOpen {
			if logger != nil {
				logger.Printf("guard: missing AGENTO11Y_*/SIGIL_* credentials; failing open")
			}
			return Result{Action: agento11y.HookActionAllow}
		}
		if logger != nil {
			logger.Printf("guard: missing AGENTO11Y_*/SIGIL_* credentials; failing closed")
		}
		return Result{
			Action: agento11y.HookActionDeny,
			Reason: formatEvalFailure(in.ToolName, "missing AGENTO11Y_ENDPOINT/AGENTO11Y_AUTH_TENANT_ID/AGENTO11Y_AUTH_TOKEN"),
		}
	}

	failOpen := cfg.FailOpen
	clientCfg := agento11y.Config{
		API: agento11y.APIConfig{
			Endpoint: endpoint,
		},
		Hooks: agento11y.HooksConfig{
			Enabled:  true,
			Phases:   []agento11y.HookPhase{agento11y.HookPhasePostflight},
			Timeout:  time.Duration(cfg.TimeoutMs) * time.Millisecond,
			FailOpen: &failOpen,
		},
		GenerationExport: agento11y.GenerationExportConfig{
			Auth: agento11y.AuthConfig{
				Mode:          agento11y.ExportAuthModeBasic,
				BasicUser:     tenantID,
				BasicPassword: authToken,
				TenantID:      tenantID,
			},
		},
	}

	client := agento11y.NewClient(clientCfg)
	defer func() { _ = client.Shutdown(ctx) }()

	provider := strings.TrimSpace(in.ModelProvider)
	if provider == "" {
		provider = "unknown"
	}
	modelName := strings.TrimSpace(in.ModelName)
	if modelName == "" {
		modelName = "unknown"
	}
	hookCtx := agento11y.HookContext{
		AgentName:    in.AgentName,
		AgentVersion: in.AgentVersion,
		Model: &agento11y.HookModel{
			Provider: provider,
			Name:     modelName,
		},
	}

	req := agento11y.HookEvaluateRequest{
		Phase:   agento11y.HookPhasePostflight,
		Context: hookCtx,
		Input: agento11y.HookInput{
			Output: []agento11y.Message{{
				Role: agento11y.RoleAssistant,
				Parts: []agento11y.Part{{
					Kind: agento11y.PartKindToolCall,
					ToolCall: &agento11y.ToolCall{
						ID:        strings.TrimSpace(in.ToolCallID),
						Name:      in.ToolName,
						InputJSON: in.ToolInputJSON,
					},
				}},
			}},
		},
	}

	resp, err := client.EvaluateHook(ctx, req)
	if err != nil {
		// The SDK only returns an error when FailOpen=false; the fail-open
		// path yields an allow response with nil err. So an error here
		// always means strict mode and we should report deny.
		if logger != nil {
			logger.Printf("guard: tool=%q endpoint=%q evaluate err=%v", in.ToolName, endpoint, err)
		}
		return Result{
			Action: agento11y.HookActionDeny,
			Reason: formatEvalFailure(in.ToolName, err.Error()),
		}
	}

	deniedErr := agento11y.HookDeniedFromResponse(resp)

	// Resolve any redaction transform before logging so the decision line can
	// report whether the tool input was rewritten. A deny never carries a
	// usable transform, so only the allow path looks for one.
	var updatedInput json.RawMessage
	if deniedErr == nil {
		updatedInput = extractToolCallTransform(resp, strings.TrimSpace(in.ToolCallID), logger)
	}

	if resp != nil && logger != nil {
		logger.Printf(
			"guard: tool=%q endpoint=%q action=%q rule_id=%q reason=%q transform_applied=%t",
			in.ToolName,
			endpoint,
			string(resp.Action),
			resp.RuleID,
			resp.Reason,
			len(updatedInput) > 0,
		)
	}

	if deniedErr != nil {
		var ruleReason string
		var denied *agento11y.HookDeniedError
		if errors.As(deniedErr, &denied) {
			ruleReason = denied.Reason
		}
		ruleID := ""
		if resp != nil {
			ruleID = resp.RuleID
		}
		return Result{
			Action: agento11y.HookActionDeny,
			Reason: formatPolicyDeny(in.ToolName, ruleReason),
			RuleID: ruleID,
		}
	}

	return Result{
		Action:           agento11y.HookActionAllow,
		UpdatedInputJSON: updatedInput,
	}
}

// extractToolCallTransform walks the server-returned transformed_input for
// the tool_call part matching toolCallID and returns its arguments as raw
// JSON. Returns nil on any mismatch or parse failure so the caller falls
// through to the original tool input unchanged. Mirrors pi guard.ts
// extractToolCallTransform; keep the two in sync.
func extractToolCallTransform(resp *agento11y.HookEvaluateResponse, toolCallID string, logger *log.Logger) json.RawMessage {
	// Treat an absent or empty output the same way: there is no transform to
	// apply, so stay silent. This mirrors pi guard.ts, whose early return also
	// covers the empty-output case; the no-match line below is reserved for a
	// transform that carried parts but none for this tool call.
	if resp == nil || resp.TransformedInput == nil || len(resp.TransformedInput.Output) == 0 {
		return nil
	}
	for _, msg := range resp.TransformedInput.Output {
		for _, part := range msg.Parts {
			if part.Kind != agento11y.PartKindToolCall || part.ToolCall == nil || part.ToolCall.ID != toolCallID {
				continue
			}
			// A matching tool_call whose args we cannot parse means the
			// server sent a transform we cannot apply; log it. The no-match
			// case stays silent, since that just means there is no redaction
			// for this call.
			raw := part.ToolCall.InputJSON
			if len(raw) > 0 {
				raw = unwrapProtoJSONBytes(raw)
			}
			if len(raw) == 0 {
				if logger != nil {
					logger.Printf("guard: tool-call transform for %s dropped: empty arguments", toolCallID)
				}
				return nil
			}
			if !json.Valid(raw) {
				if logger != nil {
					logger.Printf("guard: tool-call transform for %s dropped: invalid JSON arguments", toolCallID)
				}
				return nil
			}
			var obj map[string]any
			if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
				if logger != nil {
					logger.Printf("guard: tool-call transform for %s dropped: arguments were not a JSON object", toolCallID)
				}
				return nil
			}
			if logger != nil {
				logger.Printf("guard: tool-call transform for %s applied", toolCallID)
			}
			return raw
		}
	}
	// A transform was present in the response but none of its tool_call parts
	// matched this call's ID, so the original input is left unchanged. Worth a
	// line because it is otherwise indistinguishable from a plain allow.
	if logger != nil {
		logger.Printf("guard: tool-call transform present but no part matched %s", toolCallID)
	}
	return nil
}

// unwrapProtoJSONBytes unwraps transform arguments that arrived as a JSON
// string. The Sigil server marshals the proto `bytes input_json` field with
// encoding/json, which base64-encodes it; other emitters may put plain JSON
// text in the string. Values that are not JSON strings pass through
// unchanged. Same purpose as the JS SDK's maybeDecodeGoProtoJSONBytes, but
// stricter: decoded bytes are used only when they are valid JSON; otherwise
// the raw string contents are returned.
func unwrapProtoJSONBytes(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && json.Valid(decoded) {
		return decoded
	}
	return json.RawMessage(s)
}

// hookSpecificOutputDeny is the PreToolUse deny envelope shared by Claude
// Code and Codex. Both hosts read this exact JSON shape on stdout.
type hookSpecificOutputDeny struct {
	HookSpecificOutput hookSpecificOutputDenyBody `json:"hookSpecificOutput"`
}

type hookSpecificOutputDenyBody struct {
	HookEventName            string `json:"hookEventName"`
	PermissionDecision       string `json:"permissionDecision"`
	PermissionDecisionReason string `json:"permissionDecisionReason,omitempty"`
}

// WriteHookSpecificOutputDeny writes the PreToolUse deny JSON used by Claude
// Code and Codex. The two hosts share the exact wire format, so they share
// this writer; reason falls back to a generic message when blank.
func WriteHookSpecificOutputDeny(stdout io.Writer, reason string) {
	if stdout == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "tool call denied by Sigil guard"
	}
	_ = json.NewEncoder(stdout).Encode(hookSpecificOutputDeny{
		HookSpecificOutput: hookSpecificOutputDenyBody{
			HookEventName:            "PreToolUse",
			PermissionDecision:       "deny",
			PermissionDecisionReason: reason,
		},
	})
}

// hookSpecificOutputUpdatedInput is the PreToolUse allow+updatedInput
// envelope shared by Claude Code and Codex. Both hosts replace the tool
// arguments with updatedInput when permissionDecision is allow.
type hookSpecificOutputUpdatedInput struct {
	HookSpecificOutput hookSpecificOutputUpdatedInputBody `json:"hookSpecificOutput"`
}

type hookSpecificOutputUpdatedInputBody struct {
	HookEventName      string          `json:"hookEventName"`
	PermissionDecision string          `json:"permissionDecision"`
	UpdatedInput       json.RawMessage `json:"updatedInput"`
}

// WriteHookSpecificOutputUpdatedInput writes the PreToolUse allow JSON that
// replaces the tool arguments with updatedInput. Claude Code and Codex share
// the exact wire format, so they share this writer; it writes nothing when
// updatedInput is empty.
func WriteHookSpecificOutputUpdatedInput(stdout io.Writer, updatedInput json.RawMessage) {
	if stdout == nil || len(updatedInput) == 0 {
		return
	}
	_ = json.NewEncoder(stdout).Encode(hookSpecificOutputUpdatedInput{
		HookSpecificOutput: hookSpecificOutputUpdatedInputBody{
			HookEventName:      "PreToolUse",
			PermissionDecision: "allow",
			UpdatedInput:       updatedInput,
		},
	})
}
