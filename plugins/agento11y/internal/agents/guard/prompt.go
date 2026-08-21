package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// Prompt errors go to the user, so they omit the model-facing guardBehaviorHint.
// TestPromptMessages_MatchOpencode keeps the Go and OpenCode copies in sync.
const (
	promptDenyPrefix        = "A Grafana Agent Observability policy blocked this message, so it was not sent to the model."
	promptEvalFailurePrefix = "agento11y could not evaluate the Grafana Agent Observability guard and blocked this message. Set AGENTO11Y_GUARDS_FAIL_OPEN=true in your shell or in ~/.config/agento11y/config.env. Restart the agent and resend."
)

func formatPromptDeny(reason string) string {
	if r := strings.TrimSpace(reason); r != "" {
		return promptDenyPrefix + " Reason: " + r
	}
	return promptDenyPrefix
}

// FormatPromptEvalFailure formats a fail-closed prompt error. The local daemon
// uses it when a Cloud request contains no tool call.
func FormatPromptEvalFailure(detail string) string {
	if d := strings.TrimSpace(detail); d != "" {
		return promptEvalFailurePrefix + " Details: " + d
	}
	return promptEvalFailurePrefix
}

// PromptInput contains the fields for a preflight prompt evaluation.
type PromptInput struct {
	// AgentName identifies the host agent (e.g. "codex", "cursor").
	AgentName string
	// AgentVersion is the host agent build version, when known.
	AgentVersion string
	// ModelProvider and ModelName describe the upstream model, when known.
	ModelProvider string
	ModelName     string
	// Prompt is the message the user just submitted.
	Prompt string
}

// PromptResult is a host-independent prompt guard result.
type PromptResult struct {
	// Action is allow or deny.
	Action agento11y.HookAction
	// Reason explains a denial and is always set when Action is deny.
	Reason string
	// RuleID identifies the denying rule. EvaluationFailureRuleID marks an
	// evaluation failure.
	RuleID string
}

// Blocked reports whether the host should refuse to send the prompt.
func (r PromptResult) Blocked() bool {
	return r.Action == agento11y.HookActionDeny
}

// EvaluatePrompt applies preflight guards to one submitted prompt. It uses the
// same guard settings and fail-open behavior as EvaluateToolCall.
//
// The request contains only the submitted user message. The server flattens
// all messages and roles, so history could match assistant text or overflow
// the judge prompt.
//
// Transform results are ignored because these hosts cannot rewrite prompts.
func EvaluatePrompt(ctx context.Context, cfg envconfig.GuardsConfig, in PromptInput, logger *log.Logger) PromptResult {
	if !cfg.Enabled {
		return PromptResult{Action: agento11y.HookActionAllow}
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return PromptResult{Action: agento11y.HookActionAllow}
	}

	client, endpoint, err := newGuardClient(cfg, agento11y.HookPhasePreflight)
	if err != nil {
		if cfg.FailOpen {
			if logger != nil {
				logger.Printf("guard: prompt: missing AGENTO11Y_*/SIGIL_* credentials; failing open")
			}
			return PromptResult{Action: agento11y.HookActionAllow}
		}
		if logger != nil {
			logger.Printf("guard: prompt: missing AGENTO11Y_*/SIGIL_* credentials; failing closed")
		}
		return PromptResult{
			Action: agento11y.HookActionDeny,
			Reason: FormatPromptEvalFailure(err.Error()),
			RuleID: EvaluationFailureRuleID,
		}
	}
	defer func() { _ = client.Shutdown(ctx) }()

	provider := strings.TrimSpace(in.ModelProvider)
	if provider == "" {
		provider = "unknown"
	}
	modelName := strings.TrimSpace(in.ModelName)
	if modelName == "" {
		modelName = "unknown"
	}

	req := agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePreflight,
		Context: agento11y.HookContext{
			AgentName:    in.AgentName,
			AgentVersion: in.AgentVersion,
			Model: &agento11y.HookModel{
				Provider: provider,
				Name:     modelName,
			},
		},
		Input: agento11y.HookInput{
			Messages: []agento11y.Message{{
				Role:  agento11y.RoleUser,
				Parts: []agento11y.Part{agento11y.TextPart(in.Prompt)},
			}},
		},
	}

	resp, err := client.EvaluateHook(ctx, req)
	if err != nil {
		// FailOpen=false returns an error; fail-open returns a synthetic allow.
		if logger != nil {
			logger.Printf("guard: prompt: endpoint=%q evaluate err=%v", endpoint, err)
		}
		return PromptResult{
			Action: agento11y.HookActionDeny,
			Reason: FormatPromptEvalFailure(err.Error()),
			RuleID: EvaluationFailureRuleID,
		}
	}

	if resp != nil && logger != nil {
		logger.Printf(
			"guard: prompt: endpoint=%q action=%q rule_id=%q reason=%q",
			endpoint,
			string(resp.Action),
			resp.RuleID,
			resp.Reason,
		)
		// These hosts cannot apply prompt transforms, so log dropped transforms.
		if resp.TransformedInput != nil && len(resp.TransformedInput.Messages) > 0 {
			logger.Printf("guard: prompt: transform dropped: this host cannot replace a submitted message")
		}
	}

	deniedErr := agento11y.HookDeniedFromResponse(resp)
	if deniedErr == nil {
		return PromptResult{Action: agento11y.HookActionAllow}
	}

	var ruleReason string
	var denied *agento11y.HookDeniedError
	if errors.As(deniedErr, &denied) {
		ruleReason = denied.Reason
	}
	ruleID := ""
	if resp != nil {
		ruleID = resp.RuleID
	}
	// Evaluation failures already have user-facing text; do not wrap them as
	// policy denials.
	if ruleID == EvaluationFailureRuleID {
		if strings.TrimSpace(ruleReason) == "" {
			ruleReason = FormatPromptEvalFailure("")
		}
		return PromptResult{
			Action: agento11y.HookActionDeny,
			Reason: ruleReason,
			RuleID: ruleID,
		}
	}
	return PromptResult{
		Action: agento11y.HookActionDeny,
		Reason: formatPromptDeny(ruleReason),
		RuleID: ruleID,
	}
}

// promptBlock is the prompt block response shared by Claude Code and Codex.
type promptBlock struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// WritePromptBlock writes a Claude Code/Codex prompt block. An empty reason
// uses a generic message.
//
// The process must exit zero: both hooks use `agento11y ... || sigil ...`, so
// a non-zero exit runs the fallback instead of blocking.
func WritePromptBlock(stdout io.Writer, reason string) {
	if stdout == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "message blocked by agento11y guard"
	}
	_ = json.NewEncoder(stdout).Encode(promptBlock{
		Decision: "block",
		Reason:   reason,
	})
}
