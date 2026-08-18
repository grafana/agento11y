package hook

import (
	"log"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// SessionStart records the workspace metadata so handleStop can resolve
// git.branch and the user id when it builds the Generation.
func SessionStart(p Payload, logger *log.Logger) {
	if p.ConversationID == "" {
		logger.Print("sessionStart: missing conversation_id")
		return
	}
	s := fragment.Session{
		ConversationID:    p.ConversationID,
		WorkspaceRoots:    p.WorkspaceRoots,
		UserEmail:         p.UserEmail,
		CursorVersion:     p.CursorVersion,
		IsBackgroundAgent: p.IsBackgroundAgent,
		StartedAt:         p.ResolvedTimestamp(),
	}
	// beforeSubmit can race ahead of sessionStart and stamp the first-prompt
	// title onto a session file. Keep that title: this payload never carries
	// one, and overwriting would send the conversation out untitled.
	if existing := fragment.LoadSession(p.ConversationID, logger); existing != nil {
		s.ConversationTitle = existing.ConversationTitle
		if s.StartedAt == "" {
			s.StartedAt = existing.StartedAt
		}
	}
	if err := fragment.SaveSession(s); err != nil {
		logger.Printf("sessionStart: save: %v", err)
		return
	}
	logger.Printf("sessionStart: saved session conv=%s", p.ConversationID)
}
