package hook

import (
	"log"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
)

// BeforeSubmit captures the user prompt for the upcoming generation. Cursor
// doesn't always assign a generation_id at prompt-submit time; without one we
// cannot key the fragment, so skip silently — the turn will still be exported,
// just without the user prompt in `input`.
//
// Prompt bytes are persisted only when the active content-capture mode would
// export them (full / no_tool_content). In metadata_only the mapper drops the
// prompt at emit time anyway, and writing it to disk first leaks opted-out
// content into the fragment file (mode 0600 — but avoidable disk-residency is
// avoidable disk-residency).
func BeforeSubmit(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("beforeSubmitPrompt: no generation_id yet — skipping")
		return
	}
	ts := p.ResolvedTimestamp()
	keepPrompt := cfg.ContentCapture == sigil.ContentCaptureModeFull ||
		cfg.ContentCapture == sigil.ContentCaptureModeNoToolContent

	err := fragment.Update(p.ConversationID, p.GenerationID, logger, func(f *fragment.Fragment) bool {
		fragment.Touch(f, ts)
		if keepPrompt && p.Prompt != "" {
			f.UserPrompt = p.Prompt
		}
		return true
	})
	if err != nil {
		logger.Printf("beforeSubmitPrompt: save: %v", err)
		return
	}
	logger.Printf("beforeSubmitPrompt: captured gen=%s promptLen=%d", p.GenerationID, len(p.Prompt))
}
