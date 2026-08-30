package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"
	"unicode/utf8"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// maxTitleLen caps the conversation title derived from the first user prompt.
const maxTitleLen = 100

// beforeSubmitDeny is Cursor's response for blocking a submitted prompt.
// UserMessage is shown to the user; the model is not called.
type beforeSubmitDeny struct {
	Continue    bool   `json:"continue"`
	UserMessage string `json:"user_message,omitempty"`
}

// BeforeSubmit evaluates preflight guards before capturing the prompt. A deny
// writes Cursor's stop response; an allow lets the dispatcher respond.
// Denied messages never reach the session file or conversation title.
func BeforeSubmit(ctx context.Context, stdout io.Writer, p Payload, cfg config.Config, logger *log.Logger) {
	res := guard.EvaluatePrompt(ctx, envconfig.ResolveGuards(logger), guard.PromptInput{
		AgentName:      cfg.Agent(),
		AgentVersion:   strings.TrimSpace(p.CursorVersion),
		ConversationID: p.ConversationID,
		ModelProvider:  strings.TrimSpace(p.Provider),
		ModelName:      resolvedModel(p),
		Prompt:         p.Prompt,
	}, logger)
	if res.Blocked() {
		_ = json.NewEncoder(stdout).Encode(beforeSubmitDeny{
			Continue:    false,
			UserMessage: res.Reason,
		})
		return
	}

	capturePrompt(p, cfg, logger)
}

// capturePrompt records the user prompt for the upcoming generation. Cursor
// doesn't always assign a generation_id at prompt-submit time; without one we
// cannot key the fragment, so skip the fragment write — the turn will still
// be exported, just without the user prompt in `input`.
//
// Prompt bytes are persisted only when the active content-capture mode would
// export them (full / no_tool_content). In metadata_only the mapper drops the
// prompt at emit time anyway, and writing it to disk first leaks opted-out
// content into the fragment file (mode 0600 — but avoidable disk-residency is
// avoidable disk-residency).
//
// The conversation title is session-scoped, so it is stamped even when
// generation_id is missing: first prompt wins, and a later sessionStart must
// not wipe it.
func capturePrompt(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" {
		logger.Print("beforeSubmitPrompt: missing conversation_id — skipping")
		return
	}
	if p.Prompt != "" {
		setConversationTitle(p.ConversationID, p.Prompt, logger)
	}
	if p.GenerationID == "" {
		logger.Print("beforeSubmitPrompt: no generation_id yet — skipping fragment")
		return
	}
	ts := p.ResolvedTimestamp()
	keepPrompt := cfg.ContentCapture == agento11y.ContentCaptureModeFull ||
		cfg.ContentCapture == agento11y.ContentCaptureModeNoToolContent

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

// setConversationTitle sets the session's ConversationTitle to a truncated
// version of prompt, but only when the title is not already set (first
// prompt wins). A missing session file is created so a beforeSubmit that
// races ahead of sessionStart still leaves a title for stop to load.
// UpdateSession holds the session lock so this write cannot replace a
// sessionStart that landed between load and save.
func setConversationTitle(conversationID, prompt string, logger *log.Logger) {
	title := strings.TrimSpace(prompt)
	if title == "" {
		return
	}
	if len(title) > maxTitleLen {
		title = title[:maxTitleLen]
		for !utf8.ValidString(title) {
			title = title[:len(title)-1]
		}
	}
	err := fragment.UpdateSession(conversationID, logger, func(s *fragment.Session) bool {
		if s.ConversationTitle != "" {
			return false
		}
		s.ConversationTitle = title
		return true
	})
	if err != nil {
		logger.Printf("beforeSubmitPrompt: save session title: %v", err)
	}
}
