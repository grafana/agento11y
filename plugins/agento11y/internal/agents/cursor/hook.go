// Package cursor implements the Cursor agent adapter for the consolidated
// sigil binary. The dispatcher in cmd/sigil routes `sigil cursor hook` here.
//
// Cursor expects a permissive JSON response on pre-execution events when the
// plugin wants to allow the action — a missing or non-JSON stdout on those
// events is treated as a block. We always emit the permissive response on
// beforeSubmitPrompt regardless of how dispatch terminates, and on preToolUse
// whenever the guard handler did not get a chance to answer itself.
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

// Hook reads a Cursor hook JSON payload from stdin and dispatches it to the
// matching handler. On beforeSubmitPrompt the permissive response is
// always emitted to stdout, even on parse failure, so Cursor never blocks
// the user's input on telemetry trouble. preToolUse gets the same treatment
// when dispatch bails before the guard handler runs; the handler itself
// always writes a response on every evaluation outcome.
func Hook(ctx context.Context, stdin io.Reader, stdout io.Writer, logger *log.Logger) error {
	var raw []byte
	var event string
	parsed := false
	preToolUseHandled := false
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
		if event == "beforeSubmitPrompt" {
			_, _ = fmt.Fprint(stdout, permissiveResponse)
		}
		if event == "preToolUse" && !preToolUseHandled {
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
		hook.BeforeSubmit(payload, cfg, logger)
	case "preToolUse":
		hook.PreToolUse(ctx, payload, stdout, logger)
		preToolUseHandled = true
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
