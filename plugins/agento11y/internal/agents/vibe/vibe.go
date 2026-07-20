// Package vibe implements the Mistral Vibe agent adapter for the
// consolidated agento11y binary. The dispatcher in cmd/agento11y routes
// `agento11y vibe hook` here.
//
// Vibe fires hooks for three event types, all wired here:
//   - post_agent_turn exports one generation per turn from the on-disk
//     <session_dir>/messages.jsonl plus the sibling meta.json.
//   - before_tool evaluates the tool call against agento11y guard policy and may
//     deny or rewrite it (when guards are enabled).
//   - after_tool records the tool's timing/status for its execute_tool span.
package vibe

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/hook"
)

// Hook reads one vibe hook JSON payload from stdin and dispatches it. It
// always returns nil so a failure never surfaces as an exit code that
// interrupts the user's session. Only before_tool writes to stdout (its
// guard decision); post_agent_turn and after_tool write nothing, because
// vibe interprets a non-empty stdout on those as a deny/retry signal.
func Hook(ctx context.Context, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		logger.Printf("dispatch: read stdin: %v", err)
		return nil
	}
	if strings.TrimSpace(string(raw)) == "" {
		logger.Print("dispatch: empty stdin")
		return nil
	}
	var payload hook.Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Printf("dispatch: invalid JSON: %v", err)
		return nil
	}
	if payload.HookEventName == "" {
		logger.Print("dispatch: missing hook_event_name")
		return nil
	}
	logger.Printf("dispatch: event=%s session=%s", payload.HookEventName, payload.SessionID)
	switch payload.HookEventName {
	case "post_agent_turn":
		hook.PostAgentTurn(ctx, payload, logger)
	case "before_tool":
		hook.BeforeTool(ctx, stdout, payload, logger)
	case "after_tool":
		hook.AfterTool(payload, logger)
	default:
		logger.Printf("dispatch: unhandled event %q", payload.HookEventName)
	}
	return nil
}
