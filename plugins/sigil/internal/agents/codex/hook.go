// Package codex implements the Codex agent adapter for the consolidated
// sigil binary. The dispatcher in cmd/sigil routes `sigil codex hook` here.
package codex

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex/fragment"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex/hook"
)

// Hook reads a Codex hook JSON payload from stdin and dispatches it to the
// matching handler. Telemetry errors are logged; the function returns nil
// in almost every case because hooks must never crash the agent.
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
	defer func() {
		fragment.CleanupStaleExcept(fragment.DefaultStaleAge, time.Now(), logger, payload.SessionID, payload.TurnID)
	}()
	cfg := config.Load(logger)
	logger.Printf("dispatch: event=%s", payload.HookEventName)
	switch payload.HookEventName {
	case "SessionStart":
		hook.SessionStart(payload, cfg, logger)
	case "UserPromptSubmit":
		hook.UserPromptSubmit(payload, cfg, logger)
	case "PreToolUse":
		hook.PreToolUse(ctx, stdout, payload, cfg, logger)
	case "PostToolUse":
		hook.PostToolUse(payload, cfg, logger)
	case "Stop":
		hook.Stop(payload, cfg, logger)
	default:
		logger.Printf("dispatch: unknown event %q", payload.HookEventName)
	}
	return nil
}
