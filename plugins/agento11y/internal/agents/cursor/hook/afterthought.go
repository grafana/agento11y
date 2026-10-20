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
// later emit as empty generations with model "unknown". The original step id
// is kept on the thinking segment so a later block from a different step is
// not treated as a resend of the first.
//
// `text` is that block's aggregated thinking. It is stored when the capture
// mode exports assistant text, and redacted when the turn is mapped.
// Duration is stored in every capture mode. The hook timestamp is the block's
// completion time, not a first-token mark, so it is not recorded as TTFT.
func AfterAgentThought(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("afterAgentThought: missing conversation_id or generation_id")
		return
	}
	genID := turnGenerationID(p.GenerationID)
	stepID := thoughtStepID(p.GenerationID)
	ts := p.ResolvedTimestamp()
	keepText := keepsAssistantText(cfg.ContentCapture)

	err := fragment.Update(p.ConversationID, genID, logger, func(f *fragment.Fragment) bool {
		changed := false
		if !f.ThinkingPresent {
			fragment.Touch(f, ts)
			f.ThinkingPresent = true
			changed = true
		}
		if recordThinking(f, p.Text, p.DurationMs, ts, stepID, keepText) {
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

// recordThinking stores one completed thinking block. Duration is kept even
// when the capture mode drops text. A later event replaces an existing
// segment when it carries the same step id. Cursor also fires the same
// block twice — once with the `-<n>-<xxxx>` suffix and once with the bare
// turn id — so those two spellings are paired once. After that pair is
// closed, a later bare event starts a new block. Two different step ids
// stay separate even when their text matches.
func recordThinking(f *fragment.Fragment, text string, duration *float64, ts, stepID string, keepText bool) bool {
	if keepText {
		text = strings.TrimSpace(text)
	} else {
		text = ""
	}
	duration = cloneDuration(duration)
	if i := thinkingStepIndex(f, stepID); i >= 0 {
		return updateThinkingSegment(&f.Thinking[i], text, duration, ts)
	}
	if i := thinkingResendIndex(f, text, stepID); i >= 0 {
		seg := &f.Thinking[i]
		changed := false
		if stepID != "" && seg.StepID == "" {
			seg.StepID = stepID
			changed = true
		}
		if !seg.Paired {
			seg.Paired = true
			changed = true
		}
		return updateThinkingSegment(seg, text, duration, ts) || changed
	}
	if text == "" && duration == nil {
		return false
	}
	f.Thinking = append(f.Thinking, fragment.ThinkingSegment{
		Text:       text,
		DurationMs: duration,
		Timestamp:  ts,
		StepID:     stepID,
	})
	return true
}

func updateThinkingSegment(seg *fragment.ThinkingSegment, text string, duration *float64, ts string) bool {
	changed := false
	if text != "" && seg.Text != text {
		seg.Text = text
		seg.Timestamp = ts
		changed = true
	}
	if duration != nil && (seg.DurationMs == nil || *seg.DurationMs != *duration) {
		seg.DurationMs = duration
		seg.Timestamp = ts
		changed = true
	}
	return changed
}

// thinkingResendIndex finds the last segment when Cursor resent the same
// block with the other generation_id spelling. A block is paired at most
// once, so the next thought's leading bare id cannot overwrite the last
// finished step.
func thinkingResendIndex(f *fragment.Fragment, text, stepID string) int {
	n := len(f.Thinking)
	if n == 0 {
		return -1
	}
	last := &f.Thinking[n-1]
	if last.Paired {
		return -1
	}
	if stepID != "" {
		if last.StepID != "" || !thinkingLooksLikeResend(last, text) {
			return -1
		}
		return n - 1
	}
	if last.StepID == "" || !thinkingLooksLikeResend(last, text) {
		return -1
	}
	return n - 1
}

func thinkingLooksLikeResend(seg *fragment.ThinkingSegment, text string) bool {
	if text != "" || seg.Text != "" {
		return text == seg.Text ||
			(text != "" && seg.Text != "" && (strings.HasPrefix(text, seg.Text) || strings.HasPrefix(seg.Text, text)))
	}
	// metadata_only: the suffix and bare events are the same block, even
	// when the later fire reports a longer duration.
	return true
}

func thinkingStepIndex(f *fragment.Fragment, stepID string) int {
	if stepID == "" {
		return -1
	}
	for i := range f.Thinking {
		if f.Thinking[i].StepID == stepID {
			return i
		}
	}
	return -1
}

func cloneDuration(d *float64) *float64 {
	if d == nil {
		return nil
	}
	v := *d
	return &v
}
