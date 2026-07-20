// Package copilot implements the GitHub Copilot CLI agent adapter for the
// consolidated agento11y binary. The dispatcher in cmd/agento11y routes
// `agento11y copilot hook` here.
package copilot

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/hook"
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
	// The Copilot payload carries no host identifier and one shared
	// ~/.copilot/hooks file is read by both the CLI and VS Code, so the
	// surface (vscode vs copilot-cli) is resolved at runtime: an explicit
	// AGENTO11Y_COPILOT_HOOK_SURFACE env wins, otherwise it is inferred from the
	// process tree. Stamp it onto the payload so handlers persist it.
	payload.SurfaceMarker = surfaceDetect()
	surfaceLog := payload.SurfaceMarker
	if surfaceLog == "" {
		surfaceLog = "unknown"
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
	logger.Printf("dispatch: event=%s surface=%s", eventName, surfaceLog)
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
