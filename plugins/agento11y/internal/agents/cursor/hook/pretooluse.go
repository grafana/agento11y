package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// preToolUseResponse is the flat JSON Cursor reads from preToolUse stdout.
// `permission` is required on every response; `updated_input` replaces the
// tool arguments in full when present; `agent_message` carries the deny
// reason to the model.
type preToolUseResponse struct {
	Permission   string          `json:"permission"`
	AgentMessage string          `json:"agent_message,omitempty"`
	UpdatedInput json.RawMessage `json:"updated_input,omitempty"`
}

// PreToolUse evaluates the tool call against agento11y guards and writes exactly
// one preToolUse response to stdout: deny with the policy reason when
// blocked, allow with updated_input when a Transform rule redacted the
// arguments, plain allow otherwise (including guards disabled). All agento11y
// transport, credential, fail-open/closed, and transform-extraction
// behaviour lives in the shared guard helper so this stays in lockstep with
// the other agents.
//
// Cursor puts `model` on every agent hook. Tool-heavy turns often never
// reach afterAgentResponse or stop, so we also persist model/provider onto
// the fragment here — otherwise the mapper falls back to "unknown".
func PreToolUse(ctx context.Context, p Payload, cfg config.Config, stdout io.Writer, logger *log.Logger) {
	persistModelFromPreToolUse(p, logger)

	modelName := resolvedModel(p)
	res := guard.EvaluateToolCall(ctx, envconfig.ResolveGuards(logger), guard.ToolCallInput{
		AgentName:     cfg.Agent(),
		AgentVersion:  strings.TrimSpace(p.CursorVersion),
		ModelProvider: strings.TrimSpace(p.Provider),
		ModelName:     modelName,
		ToolName:      strings.TrimSpace(p.ToolName),
		ToolCallID:    strings.TrimSpace(p.ToolUseID),
		ToolInputJSON: p.ToolInput,
	}, logger)

	resp := preToolUseResponse{Permission: "allow"}
	switch {
	case res.Blocked():
		resp = preToolUseResponse{Permission: "deny", AgentMessage: res.Reason}
	case len(res.UpdatedInputJSON) > 0:
		// Cursor rejects Shell-style updated_input without a string command
		// field, which would error the tool call instead of running it. A
		// transform that strips command therefore fails open: keep the
		// original arguments.
		if hasStringCommand(p.ToolInput) && !hasStringCommand(res.UpdatedInputJSON) {
			logger.Printf("guard: tool-call transform for %s dropped: transformed arguments missing string command field", p.ToolUseID)
			break
		}
		resp.UpdatedInput = res.UpdatedInputJSON
	}
	_ = json.NewEncoder(stdout).Encode(resp)
}

func hasStringCommand(raw json.RawMessage) bool {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return false
	}
	_, ok := obj["command"].(string)
	return ok
}

// persistModelFromPreToolUse copies model/provider onto the turn fragment
// when Cursor sent them. Missing conversation/generation ids skip silently
// — the guard response still goes out.
func persistModelFromPreToolUse(p Payload, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		return
	}
	if resolvedModel(p) == "" && strings.TrimSpace(p.Provider) == "" {
		return
	}
	ts := p.ResolvedTimestamp()
	err := fragment.Update(p.ConversationID, p.GenerationID, logger, func(f *fragment.Fragment) bool {
		if !applyModelMeta(f, p) {
			return false
		}
		fragment.Touch(f, ts)
		return true
	})
	if err != nil {
		logger.Printf("preToolUse: save model: %v", err)
	}
}
