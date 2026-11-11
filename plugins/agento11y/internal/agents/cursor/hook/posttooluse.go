package hook

import (
	"encoding/json"
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
)

// PostToolUse appends a ToolRecord to the fragment. The same handler covers
// both the success event (postToolUse) and the failure event
// (postToolUseFailure) — pass isFailure=true for the latter so the resulting
// record gets `status="error"` and the extracted error message.
//
// Tool input/output and the failure message are dropped at fragment-write
// time when the active content-capture mode isn't `full`. We never persist
// bytes we don't intend to export.
func PostToolUse(p Payload, cfg config.Config, logger *log.Logger, isFailure bool) {
	if p.ConversationID == "" || p.GenerationID == "" {
		label := "postToolUse"
		if isFailure {
			label = "postToolUseFailure"
		}
		logger.Printf("%s: missing conversation_id or generation_id", label)
		return
	}
	ts := p.ResolvedTimestamp()
	status := p.Status
	var errorMsg, failureType string
	if isFailure {
		status = "error"
		errorMsg = errorMessageForMode(p, cfg.ContentCapture)
		failureType = failureTypeForStore(p.FailureType)
	}

	rec := fragment.ToolRecord{
		ToolName:     stringOr(p.ToolName, "unknown"),
		ToolUseID:    p.ToolUseID,
		DurationMs:   p.Duration,
		Cwd:          p.Cwd,
		Status:       status,
		CompletedAt:  ts,
		ErrorMessage: errorMsg,
		FailureType:  failureType,
	}
	// Persist input/output bytes only when full mode is active. Anything else
	// would be silently dropped at emit time, but writing to disk first would
	// leak content into the buffered fragment file (mode 0600 still — but
	// avoidable disk-residency is avoidable disk-residency).
	if cfg.ContentCapture == agento11y.ContentCaptureModeFull {
		rec.ToolInput = p.ToolInput
		rec.ToolOutput = p.ToolOutput
	}

	err := fragment.Update(p.ConversationID, p.GenerationID, logger, func(f *fragment.Fragment) bool {
		fragment.Touch(f, ts)
		mergeModelParams(f, p.ModelParams)
		f.Tools = append(f.Tools, rec)
		return true
	})
	if err != nil {
		label := "postToolUse"
		if isFailure {
			label = "postToolUseFailure"
		}
		logger.Printf("%s: save: %v", label, err)
		return
	}
}

// errorMessageForMode returns the redacted tool failure message in `full` mode
// and "" in every other mode, so the span falls back to the generic "tool
// returned error". The message is tool content, and it is the one content field
// the SDK forwards to the span status and error event under no_tool_content.
// codex and copilot gate and redact the same field the same way.
//
// Current Cursor payloads send error_message. Older ones send error as a
// string or {message, code}. error_message wins when both are set.
func errorMessageForMode(p Payload, mode agento11y.ContentCaptureMode) string {
	if mode != agento11y.ContentCaptureModeFull {
		return ""
	}
	msg := strings.TrimSpace(p.ErrorMessage)
	if msg == "" {
		msg = extractToolError(p.Error)
	}
	if msg == "" {
		return ""
	}
	return redact.New().Redact(msg)
}

// failureTypeForStore keeps Cursor's failure classification in every capture
// mode. It is an enum (error, timeout, permission_denied), not tool content.
// A value that is not one of those is still kept, redacted, and cut, so a
// host that puts a secret in the field cannot export it whole.
func failureTypeForStore(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return redact.CutTitle(redact.New().RedactLightweight(raw), 64)
}

// extractToolError parses the polymorphic `error` field into a single string.
// Accepts either a JSON string ("oops") or an object ({"message": "...",
// "code": "..."}). Returns "" when nothing parseable is available.
func extractToolError(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil && asString != "" {
		return asString
	}
	var asObj struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &asObj); err == nil {
		return asObj.Message
	}
	return ""
}

func stringOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
