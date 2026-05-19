package hook

import (
	"log"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
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
	if err := fragment.SaveSession(s); err != nil {
		logger.Printf("sessionStart: save: %v", err)
		return
	}
	logger.Printf("sessionStart: saved session conv=%s", p.ConversationID)
}
