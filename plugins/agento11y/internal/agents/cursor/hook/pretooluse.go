package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/mapper"
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
func PreToolUse(ctx context.Context, p Payload, stdout io.Writer, logger *log.Logger) {
	res := guard.EvaluateToolCall(ctx, envconfig.ResolveGuards(logger), guard.ToolCallInput{
		AgentName:     mapper.AgentName,
		AgentVersion:  strings.TrimSpace(p.CursorVersion),
		ModelProvider: strings.TrimSpace(p.Provider),
		ModelName:     strings.TrimSpace(p.Model),
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
