// Package copilot implements the GitHub Copilot CLI agent adapter for the
// consolidated sigil binary. The dispatcher in cmd/sigil routes
// `sigil copilot hook` here.
package copilot

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/hook"
)

// Hook reads a Copilot hook JSON payload from stdin and dispatches it to the
// matching handler. Telemetry errors are logged; the function returns nil
// in almost every case because hooks must never crash the agent.
//
// Copilot accepts both snake_case and camelCase event names. When the
// payload omits the event name (the early Copilot CLI did this for several
// events), SIGIL_COPILOT_HOOK_EVENT is consulted as a fallback — the
// hooks.json manifest sets it per hook entry.
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
	eventName := payload.EventName()
	if eventName == "" {
		eventName = strings.TrimSpace(os.Getenv("SIGIL_COPILOT_HOOK_EVENT"))
	}
	if eventName == "" {
		logger.Print("dispatch: missing hook_event_name")
		return nil
	}
	sessionID := payload.SessionID()
	defer func() {
		activeTurnID := ""
		if sessionID != "" {
			if session := fragment.LoadSessionTolerant(sessionID, logger); session != nil {
				activeTurnID = session.ActiveTurnID
			}
		}
		fragment.CleanupStaleExcept(fragment.DefaultStaleAge, time.Now(), logger, sessionID, activeTurnID)
	}()
	cfg := config.Load(logger)
	logger.Printf("dispatch: event=%s", eventName)
	switch eventName {
	case "sessionStart", "SessionStart":
		hook.SessionStart(payload, cfg, logger)
	case "sessionEnd", "SessionEnd":
		hook.SessionEnd(payload, logger)
	case "userPromptSubmitted", "UserPromptSubmit", "UserPromptSubmitted":
		hook.UserPromptSubmit(payload, cfg, logger)
	case "preToolUse", "PreToolUse":
		hook.PreToolUse(ctx, stdout, payload, cfg, logger)
	case "postToolUse", "PostToolUse":
		hook.PostToolUse(payload, cfg, logger, false)
	case "postToolUseFailure", "PostToolUseFailure":
		hook.PostToolUse(payload, cfg, logger, true)
	case "errorOccurred", "ErrorOccurred":
		hook.ErrorOccurred(payload, cfg, logger)
	case "subagentStart", "SubagentStart":
		hook.SubagentStart(payload, cfg, logger)
	case "subagentStop", "SubagentStop":
		hook.SubagentStop(payload, logger)
	case "agentStop", "AgentStop", "stop", "Stop":
		hook.Stop(payload, cfg, logger)
	default:
		logger.Printf("dispatch: unknown event %q", eventName)
	}
	return nil
}
