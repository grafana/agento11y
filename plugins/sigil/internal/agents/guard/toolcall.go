// Package guard evaluates tool calls against Sigil guard policy and
// returns a host-neutral result; callers translate it into their own
// stdout response shape.
package guard

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
)

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
	Action sigil.HookAction
	// Reason is the deny reason from Sigil or the host-friendly description
	// of a fail-closed transport/config error.
	Reason string
	// RuleID identifies the rule that produced a deny verdict, when known.
	RuleID string
}

// Blocked reports whether the host should refuse to execute the tool call.
func (r Result) Blocked() bool {
	return r.Action == sigil.HookActionDeny
}

// EvaluateToolCall asks Sigil whether the tool call should proceed. It
// reads SIGIL_ENDPOINT / SIGIL_AUTH_TENANT_ID / SIGIL_AUTH_TOKEN from the
// process environment and honours envconfig.GuardsConfig for the
// enabled/timeout/fail-open knobs. Callers are expected to gate this
// helper behind cfg.Enabled — when disabled it short-circuits to allow.
//
// Behaviour:
//   - guards disabled or tool name empty: returns allow.
//   - credentials missing and fail-open: returns allow.
//   - credentials missing and fail-closed: returns deny with a credentials reason.
//   - Sigil returns allow: returns allow.
//   - Sigil returns deny: returns deny with the rule reason.
//   - transport error and fail-open: returns allow (matches SDK behaviour).
//   - transport error and fail-closed: returns deny with a transport reason.
func EvaluateToolCall(ctx context.Context, cfg envconfig.GuardsConfig, in ToolCallInput, logger *log.Logger) Result {
	if !cfg.Enabled {
		return Result{Action: sigil.HookActionAllow}
	}
	if strings.TrimSpace(in.ToolName) == "" {
		return Result{Action: sigil.HookActionAllow}
	}

	endpoint := strings.TrimSpace(os.Getenv("SIGIL_ENDPOINT"))
	tenantID := strings.TrimSpace(os.Getenv("SIGIL_AUTH_TENANT_ID"))
	authToken := strings.TrimSpace(os.Getenv("SIGIL_AUTH_TOKEN"))
	if endpoint == "" || tenantID == "" || authToken == "" {
		if cfg.FailOpen {
			if logger != nil {
				logger.Printf("guard: missing SIGIL_* credentials; failing open")
			}
			return Result{Action: sigil.HookActionAllow}
		}
		if logger != nil {
			logger.Printf("guard: missing SIGIL_* credentials; failing closed")
		}
		return Result{
			Action: sigil.HookActionDeny,
			Reason: "sigil guard evaluation unavailable: missing SIGIL_ENDPOINT/SIGIL_AUTH_TENANT_ID/SIGIL_AUTH_TOKEN",
		}
	}

	failOpen := cfg.FailOpen
	clientCfg := sigil.Config{
		API: sigil.APIConfig{
			Endpoint: endpoint,
		},
		Hooks: sigil.HooksConfig{
			Enabled:  true,
			Phases:   []sigil.HookPhase{sigil.HookPhasePostflight},
			Timeout:  time.Duration(cfg.TimeoutMs) * time.Millisecond,
			FailOpen: &failOpen,
		},
		GenerationExport: sigil.GenerationExportConfig{
			Auth: sigil.AuthConfig{
				Mode:          sigil.ExportAuthModeBasic,
				BasicUser:     tenantID,
				BasicPassword: authToken,
				TenantID:      tenantID,
			},
		},
	}

	client := sigil.NewClient(clientCfg)
	defer func() { _ = client.Shutdown(ctx) }()

	provider := strings.TrimSpace(in.ModelProvider)
	if provider == "" {
		provider = "unknown"
	}
	modelName := strings.TrimSpace(in.ModelName)
	if modelName == "" {
		modelName = "unknown"
	}
	hookCtx := sigil.HookContext{
		AgentName:    in.AgentName,
		AgentVersion: in.AgentVersion,
		Model: &sigil.HookModel{
			Provider: provider,
			Name:     modelName,
		},
	}

	req := sigil.HookEvaluateRequest{
		Phase:   sigil.HookPhasePostflight,
		Context: hookCtx,
		Input: sigil.HookInput{
			Output: []sigil.Message{{
				Role: sigil.RoleAssistant,
				Parts: []sigil.Part{{
					Kind: sigil.PartKindToolCall,
					ToolCall: &sigil.ToolCall{
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
			Action: sigil.HookActionDeny,
			Reason: "sigil guard evaluation failed: " + err.Error(),
		}
	}

	if resp != nil && logger != nil {
		logger.Printf(
			"guard: tool=%q endpoint=%q action=%q rule_id=%q reason=%q",
			in.ToolName,
			endpoint,
			string(resp.Action),
			resp.RuleID,
			resp.Reason,
		)
	}

	if deniedErr := sigil.HookDeniedFromResponse(resp); deniedErr != nil {
		reason := "tool call denied by Sigil guard"
		var denied *sigil.HookDeniedError
		if errors.As(deniedErr, &denied) && strings.TrimSpace(denied.Reason) != "" {
			reason = denied.Reason
		}
		ruleID := ""
		if resp != nil {
			ruleID = resp.RuleID
		}
		return Result{
			Action: sigil.HookActionDeny,
			Reason: reason,
			RuleID: ruleID,
		}
	}

	return Result{Action: sigil.HookActionAllow}
}
