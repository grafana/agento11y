package hook

import (
	"encoding/json"
	"log"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// PostToolUse appends a ToolRecord to the fragment. The same handler covers
// both the success event (postToolUse) and the failure event
// (postToolUseFailure) — pass isFailure=true for the latter so the resulting
// record gets `status="error"` and the extracted error message.
//
// Tool input/output are dropped at fragment-write time when the active
// content-capture mode isn't `full`. We never persist bytes we don't intend
// to export.
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
	var errorMsg string
	if isFailure {
		status = "error"
		errorMsg = extractToolError(p.Error)
	}

	rec := fragment.ToolRecord{
		ToolName:     stringOr(p.ToolName, "unknown"),
		ToolUseID:    p.ToolUseID,
		DurationMs:   p.Duration,
		Cwd:          p.Cwd,
		Status:       status,
		CompletedAt:  ts,
		ErrorMessage: errorMsg,
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
