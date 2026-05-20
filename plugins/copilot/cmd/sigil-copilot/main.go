package main

import (
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/plugins/copilot/internal/config"
	"github.com/grafana/sigil-sdk/plugins/copilot/internal/fragment"
	"github.com/grafana/sigil-sdk/plugins/copilot/internal/hook"
	"github.com/grafana/sigil-sdk/plugins/copilot/internal/util"
)

func main() {
	config.ApplyEnv(nil)
	logger := initLogger()
	defer recoverAndLog(logger)
	run(logger, os.Stdin)
}

func run(logger *log.Logger, stdin io.Reader) {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		logger.Printf("dispatch: read stdin: %v", err)
		return
	}
	if strings.TrimSpace(string(raw)) == "" {
		logger.Print("dispatch: empty stdin")
		return
	}
	var payload hook.Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Printf("dispatch: invalid JSON: %v", err)
		return
	}
	eventName := payload.EventName()
	if eventName == "" {
		eventName = strings.TrimSpace(os.Getenv("SIGIL_COPILOT_HOOK_EVENT"))
	}
	if eventName == "" {
		logger.Print("dispatch: missing hook_event_name")
		return
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
		hook.PreToolUse(payload, cfg, logger)
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
}

func initLogger() *log.Logger {
	logger := log.New(io.Discard, "sigil-copilot: ", log.Ltime)
	if !util.ParseBool(os.Getenv("SIGIL_DEBUG")) {
		return logger
	}
	path := fragment.LogFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return logger
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return logger
	}
	return log.New(f, "sigil-copilot: ", log.Ldate|log.Ltime|log.Lmicroseconds)
}

func recoverAndLog(logger *log.Logger) {
	if r := recover(); r != nil {
		logger.Printf("dispatch: panic: %v", r)
	}
}
