package hook

import (
	"log"
	"strings"
	"unicode/utf8"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/config"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
)

// maxTitleLen caps the conversation title derived from the first user prompt.
const maxTitleLen = 100

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
//
// On the first prompt of a conversation the handler also stamps the session's
// ConversationTitle so the mapper can surface a human-readable name instead of
// the raw conversation UUID.
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

	if p.Prompt != "" {
		setConversationTitle(p.ConversationID, p.Prompt, logger)
	}

	logger.Printf("beforeSubmitPrompt: captured gen=%s promptLen=%d", p.GenerationID, len(p.Prompt))
}

// setConversationTitle sets the session's ConversationTitle to a truncated
// version of prompt, but only when the title is not already set (first
// prompt wins).
func setConversationTitle(conversationID, prompt string, logger *log.Logger) {
	sess := fragment.LoadSession(conversationID, logger)
	if sess == nil || sess.ConversationTitle != "" {
		return
	}
	title := strings.TrimSpace(prompt)
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen]
		for !utf8.ValidString(title) {
			title = title[:len(title)-1]
		}
	}
	sess.ConversationTitle = title
	if err := fragment.SaveSession(*sess); err != nil {
		logger.Printf("beforeSubmitPrompt: save session title: %v", err)
	}
}
