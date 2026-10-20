package hook

import (
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// AfterAgentThought marks `thinkingPresent=true` on the fragment and, when
// Cursor sent a model on the common payload, fills frag.Model if still empty.
//
// Cursor fires this event for every completed thinking block, and often keys
// it with a per-step generation_id (`<turn-uuid>-<n>-<xxxx>`). We collapse
// that onto the turn id so thoughts attach to the same fragment as tools and
// stop, rather than creating orphan thought-only files that sessionEnd would
// later emit as empty generations with model "unknown".
//
// `text` is that block's aggregated thinking. It is stored when the capture
// mode exports assistant text, and redacted when the turn is mapped. The
// event is also the earliest model-output signal, so its timestamp is the
// time-to-first-token mark when no earlier thought or response was seen.
func AfterAgentThought(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("afterAgentThought: missing conversation_id or generation_id")
		return
	}
	genID := turnGenerationID(p.GenerationID)
	ts := p.ResolvedTimestamp()
	keepText := keepsAssistantText(cfg.ContentCapture)

	err := fragment.Update(p.ConversationID, genID, logger, func(f *fragment.Fragment) bool {
		changed := false
		if !f.ThinkingPresent {
			fragment.Touch(f, ts)
			f.ThinkingPresent = true
			changed = true
		}
		if noteFirstToken(f, p) {
			changed = true
		}
		if recordThinking(f, p.Text, p.DurationMs, ts, keepText) {
			fragment.Touch(f, ts)
			changed = true
		}
		if applyModelMeta(f, p) {
			changed = true
		}
		return changed
	})
	if err != nil {
		logger.Printf("afterAgentThought: save: %v", err)
		return
	}
}

// keepsAssistantText reports whether this capture mode exports assistant
// text. Thinking text follows the same gate: it is model output, not tool
// content.
func keepsAssistantText(mode agento11y.ContentCaptureMode) bool {
	return mode == agento11y.ContentCaptureModeFull ||
		mode == agento11y.ContentCaptureModeNoToolContent
}

// recordThinking stores one completed thinking block. A repeat of the same
// aggregated text does not append. A longer text that starts with the
// previous block replaces it, so a host that resends a growing aggregation
// does not duplicate the prefix. Without text, only the duration is kept.
func recordThinking(f *fragment.Fragment, text string, duration *float64, ts string, keepText bool) bool {
	text = strings.TrimSpace(text)
	if keepText && text != "" {
		if n := len(f.Thinking); n > 0 {
			last := &f.Thinking[n-1]
			if last.Text == text || strings.HasPrefix(text, last.Text) {
				changed := false
				if last.Text != text {
					last.Text = text
					last.Timestamp = ts
					changed = true
				}
				if duration != nil && (last.DurationMs == nil || *last.DurationMs != *duration) {
					last.DurationMs = duration
					last.Timestamp = ts
					changed = true
				}
				return changed
			}
		}
		f.Thinking = append(f.Thinking, fragment.ThinkingSegment{
			Text:       text,
			DurationMs: duration,
			Timestamp:  ts,
		})
		return true
	}
	if duration == nil {
		return false
	}
	if f.ThinkingDurationMs != nil && *f.ThinkingDurationMs == *duration {
		return false
	}
	f.ThinkingDurationMs = duration
	return true
}
