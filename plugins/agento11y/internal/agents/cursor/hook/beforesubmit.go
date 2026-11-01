package hook

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

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
// The conversation title and the rest of the session are conversation-scoped,
// so they are recorded even when generation_id is missing: first prompt wins
// the title, and a later sessionStart must not wipe it.
func capturePrompt(p Payload, cfg config.Config, logger *log.Logger) {
	if p.ConversationID == "" {
		logger.Print("beforeSubmitPrompt: missing conversation_id — skipping")
		return
	}
	recordSession(p, logger)
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

// recordSession sets the session's ConversationTitle to fragment.SessionTitle
// of the prompt, but only when the title is not already set (first prompt
// wins). The title is built only then, so a later prompt with a large paste
// does not pay for its redaction. It also fills in the workspace roots, user
// email and Cursor version the session lacks, which every hook payload
// carries. sessionStart normally saves them first, but a reopened chat whose
// session file is gone gets no second sessionStart. A missing session file is
// created so a beforeSubmit that races ahead of sessionStart still leaves a
// title for stop to load. UpdateSession holds the session lock so this write
// cannot replace a sessionStart that landed between load and save.
func recordSession(p Payload, logger *log.Logger) {
	err := fragment.UpdateSession(p.ConversationID, logger, func(s *fragment.Session) bool {
		changed := false
		if s.ConversationTitle == "" && strings.TrimSpace(p.Prompt) != "" {
			s.ConversationTitle = fragment.SessionTitle(p.Prompt)
			changed = s.ConversationTitle != ""
		}
		if len(s.WorkspaceRoots) == 0 && len(p.WorkspaceRoots) > 0 {
			s.WorkspaceRoots = p.WorkspaceRoots
			changed = true
		}
		if s.UserEmail == "" && p.UserEmail != "" {
			s.UserEmail = p.UserEmail
			changed = true
		}
		if s.CursorVersion == "" && p.CursorVersion != "" {
			s.CursorVersion = p.CursorVersion
			changed = true
		}
		return changed
	})
	if err != nil {
		logger.Printf("beforeSubmitPrompt: save session: %v", err)
	}
}
