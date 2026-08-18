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
	// UpdateSession holds the session lock so a racing beforeSubmit title
	// write cannot replace this file with a title-only session (and so we
	// keep a title that beforeSubmit already stamped).
	err := fragment.UpdateSession(p.ConversationID, logger, func(s *fragment.Session) bool {
		s.WorkspaceRoots = p.WorkspaceRoots
		s.UserEmail = p.UserEmail
		s.CursorVersion = p.CursorVersion
		s.IsBackgroundAgent = p.IsBackgroundAgent
		if ts := p.ResolvedTimestamp(); ts != "" {
			s.StartedAt = ts
		}
		return true
	})
	if err != nil {
		logger.Printf("sessionStart: save: %v", err)
		return
	}
	logger.Printf("sessionStart: saved session conv=%s", p.ConversationID)
}
