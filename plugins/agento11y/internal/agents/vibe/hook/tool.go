package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/meta"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

// guardEvalBuffer pads the guard timeout to bound the whole before_tool
// handler; the SDK's own timeout (cfg.TimeoutMs) does the real work.
const guardEvalBuffer = 500 * time.Millisecond

// BeforeTool evaluates a tool call against Sigil guard policy before vibe
// runs it. With guards disabled (the default) it is a pass-through that
// writes nothing, which vibe reads as "allow". A deny writes vibe's
// structured deny response so the call never runs; a redaction transform
// writes a tool_input rewrite so vibe runs the redacted arguments. Stdout is
// the only channel that influences vibe here, so this is the one handler
// that writes to it.
func BeforeTool(ctx context.Context, stdout io.Writer, p Payload, logger *log.Logger) {
	cfg := envconfig.ResolveGuards(logger)
	if !cfg.Enabled {
		return
	}
	toolName := p.ToolName()
	if toolName == "" {
		logger.Print("before_tool: missing tool_name; allowing")
		return
	}

	envconfig.ApplyLocalAuthPlaceholders()
	provider, modelName := guardModel(p)

	evalCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutMs)*time.Millisecond+guardEvalBuffer)
	defer cancel()

	res := guard.EvaluateToolCall(evalCtx, cfg, guard.ToolCallInput{
		AgentName:     mapper.AgentName,
		ToolName:      toolName,
		ToolCallID:    p.ToolCallID(),
		ToolInputJSON: p.ToolInput(),
		ModelProvider: provider,
		ModelName:     modelName,
	}, logger)
	if res.Blocked() {
		writeBeforeToolDeny(stdout, res.Reason)
		return
	}
	if len(res.UpdatedInputJSON) > 0 {
		writeBeforeToolRewrite(stdout, res.UpdatedInputJSON)
	}
}

// AfterTool records a completed tool call's timing and status so the
// post_agent_turn export can attach them to that tool's execute_tool span.
// It is skipped when no OTel exporter is configured, because without one the
// spans are no-ops and the recorded events would never be read.
func AfterTool(p Payload, logger *log.Logger) {
	if otel.EndpointFromEnv() == "" {
		return
	}
	if p.SessionID == "" || p.ToolCallID() == "" {
		logger.Print("after_tool: missing session_id or tool_call_id")
		return
	}
	ev := toolevents.Event{
		ToolCallID:  p.ToolCallID(),
		ToolName:    p.ToolName(),
		Status:      p.ToolStatus(),
		DurationMs:  p.DurationMs(),
		Error:       p.ToolErrorText(),
		CompletedAt: time.Now().UTC(),
	}
	if err := toolevents.Save(p.SessionID, ev); err != nil {
		logger.Printf("after_tool: save tool event: %v", err)
	}
}

// guardModel returns the model provider/name for the guard request,
// best-effort from the session meta.json. Guards work without it (the helper
// falls back to "unknown"), so any read error is ignored.
func guardModel(p Payload) (provider, modelName string) {
	if p.TranscriptPath == "" {
		return "", ""
	}
	m, err := meta.Load(p.TranscriptPath)
	if err != nil {
		return "", ""
	}
	provider, apiName := m.ActiveModelRef()
	modelName = strings.TrimSpace(m.Config.ActiveModel)
	if modelName == "" {
		modelName = apiName
	}
	return provider, modelName
}

// beforeToolDeny is vibe's structured deny response: a non-empty stdout
// object with decision="deny". vibe surfaces Reason to the model as the
// blocked-tool error (vibe/core/hooks/models.py HookStructuredResponse).
type beforeToolDeny struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// beforeToolRewrite replaces the tool arguments via hook_specific_output.
// decision is omitted (defaults to allow) so the call proceeds with the
// rewritten input rather than skipping the user's permission prompt.
type beforeToolRewrite struct {
	HookSpecificOutput beforeToolRewriteBody `json:"hook_specific_output"`
}

type beforeToolRewriteBody struct {
	ToolInput json.RawMessage `json:"tool_input"`
}

func writeBeforeToolDeny(stdout io.Writer, reason string) {
	if stdout == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "tool call denied by Sigil guard"
	}
	_ = json.NewEncoder(stdout).Encode(beforeToolDeny{Decision: "deny", Reason: reason})
}

func writeBeforeToolRewrite(stdout io.Writer, updatedInput json.RawMessage) {
	if stdout == nil || len(updatedInput) == 0 {
		return
	}
	_ = json.NewEncoder(stdout).Encode(beforeToolRewrite{
		HookSpecificOutput: beforeToolRewriteBody{ToolInput: updatedInput},
	})
}
