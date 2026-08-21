// Package cursor implements the Cursor adapter for the agento11y binary.
//
// Cursor expects JSON responses for beforeSubmitPrompt and preToolUse. The
// dispatcher sends a permissive response when a handler writes nothing, so
// behavior does not depend on Cursor's permissive default.
package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/hook"
)

const permissiveResponse = `{"continue":true,"permission":"allow"}` + "\n"

var (
	beforeSubmitMarker = []byte(`"hook_event_name":"beforeSubmitPrompt"`)
	preToolUseMarker   = []byte(`"hook_event_name":"preToolUse"`)
)

// answeredWriter prevents a fallback response after a handler writes one,
// including when the handler panics.
type answeredWriter struct {
	w        io.Writer
	answered bool
}

func (a *answeredWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		a.answered = true
	}
	return a.w.Write(p)
}

// Hook dispatches a Cursor hook payload. If a pre-execution handler writes
// nothing, Hook sends one permissive response.
func Hook(ctx context.Context, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	var raw []byte
	var event string
	parsed := false
	out := &answeredWriter{w: stdout}
	defer func() {
		// Before the payload parses we can only guess the event from the raw
		// bytes, and beforeSubmitPrompt wins so a nested preToolUse marker can't
		// trigger a second permissive line. Once parsed, the real event name
		// drives the fallback and substring collisions stop mattering.
		if !parsed {
			switch {
			case bytes.Contains(raw, beforeSubmitMarker):
				event = "beforeSubmitPrompt"
			case bytes.Contains(raw, preToolUseMarker):
				event = "preToolUse"
			}
		}
		if !out.answered && (event == "beforeSubmitPrompt" || event == "preToolUse") {
			_, _ = fmt.Fprint(stdout, permissiveResponse)
		}
	}()

	var err error
	raw, err = io.ReadAll(stdin)
	if err != nil {
		logger.Printf("dispatch: read stdin: %v", err)
		return nil
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		logger.Print("dispatch: empty stdin")
		return nil
	}

	var payload hook.Payload
	if err := json.Unmarshal(raw, &payload); err != nil {
		logger.Printf("dispatch: invalid JSON: %v", err)
		return nil
	}
	parsed = true
	event = payload.HookEventName
	if event == "" {
		logger.Print("dispatch: missing hook_event_name")
		return nil
	}
	logger.Printf("dispatch: event=%s", event)

	cfg := config.Load(logger)

	switch event {
	case "sessionStart":
		hook.SessionStart(payload, logger)
	case "beforeSubmitPrompt":
		hook.BeforeSubmit(ctx, out, payload, cfg, logger)
	case "preToolUse":
		hook.PreToolUse(ctx, payload, cfg, out, logger)
	case "afterAgentResponse":
		hook.AfterAgentResponse(payload, cfg, logger)
	case "afterAgentThought":
		hook.AfterAgentThought(payload, logger)
	case "postToolUse":
		hook.PostToolUse(payload, cfg, logger, false)
	case "postToolUseFailure":
		hook.PostToolUse(payload, cfg, logger, true)
	case "stop":
		hook.Stop(payload, cfg, logger)
	case "sessionEnd":
		hook.SessionEnd(payload, cfg, logger)
	default:
		logger.Printf("dispatch: unknown event %q", event)
	}
	return nil
}
