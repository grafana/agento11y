package hook

import (
	"log"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// AfterAgentThought marks `thinkingPresent=true` on the fragment. Cursor
// fires this event for every model "thought" — potentially many per
// generation — but the only state we keep is a presence flag. This handler
// short-circuits the rewrite when the flag is already set so we don't pay
// lock + read + write + rename per thought.
//
// Thinking text itself is intentionally never persisted or exported.
func AfterAgentThought(p Payload, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("afterAgentThought: missing conversation_id or generation_id")
		return
	}
	ts := p.ResolvedTimestamp()

	err := fragment.Update(p.ConversationID, p.GenerationID, logger, func(f *fragment.Fragment) bool {
		if f.ThinkingPresent {
			// Already noted — skip the rewrite.
			return false
		}
		fragment.Touch(f, ts)
		f.ThinkingPresent = true
		return true
	})
	if err != nil {
		logger.Printf("afterAgentThought: save: %v", err)
		return
	}
}
