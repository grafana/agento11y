package hook

import (
	"log"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// AfterAgentThought marks `thinkingPresent=true` on the fragment and, when
// Cursor sent a model on the common payload, fills frag.Model if still empty.
//
// Cursor fires this event for every model "thought" — potentially many per
// generation — and often keys it with a per-step generation_id
// (`<turn-uuid>-<n>-<xxxx>`). We collapse that onto the turn id so thoughts
// attach to the same fragment as tools and stop, rather than creating
// orphan thought-only files that sessionEnd would later emit as empty
// generations with model "unknown".
//
// Thinking text itself is intentionally never persisted or exported.
func AfterAgentThought(p Payload, logger *log.Logger) {
	if p.ConversationID == "" || p.GenerationID == "" {
		logger.Print("afterAgentThought: missing conversation_id or generation_id")
		return
	}
	genID := turnGenerationID(p.GenerationID)
	ts := p.ResolvedTimestamp()

	err := fragment.Update(p.ConversationID, genID, logger, func(f *fragment.Fragment) bool {
		changed := false
		if !f.ThinkingPresent {
			fragment.Touch(f, ts)
			f.ThinkingPresent = true
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
