package hook

import (
	"regexp"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
)

// thoughtGenerationSuffix matches Cursor's per-model-step invocation id:
// the turn generation_id plus "-<step>-<4 alnum chars>". afterAgentThought
// reports that step id; every other hook reports the bare turn id. See
// https://forum.cursor.com/t/generation-id-has-extra-suffix-for-afteragentthought/166275
var thoughtGenerationSuffix = regexp.MustCompile(`-\d+-[a-z0-9]{4}$`)

// turnGenerationID collapses a Cursor afterAgentThought generation_id onto
// the turn-level id used by beforeSubmitPrompt / postToolUse / stop. Bare
// turn ids and anything that does not match the suffix are returned unchanged.
func turnGenerationID(generationID string) string {
	trimmed := strings.TrimSpace(generationID)
	if trimmed == "" {
		return trimmed
	}
	if loc := thoughtGenerationSuffix.FindStringIndex(trimmed); loc != nil {
		return trimmed[:loc[0]]
	}
	return trimmed
}

// resolvedModel prefers the legacy composer slug (`model`) and falls back to
// the structured `model_id` Cursor sends on the common hook schema.
func resolvedModel(p Payload) string {
	if m := strings.TrimSpace(p.Model); m != "" {
		return m
	}
	return strings.TrimSpace(p.ModelID)
}

// applyModelMeta copies model and provider onto the fragment when the
// fragment still lacks them. Returns true when either field was filled so
// callers can decide whether to rewrite.
func applyModelMeta(f *fragment.Fragment, p Payload) bool {
	changed := false
	if model := resolvedModel(p); model != "" && f.Model == "" {
		f.Model = model
		changed = true
	}
	if provider := strings.TrimSpace(p.Provider); provider != "" && f.Provider == "" {
		f.Provider = provider
		changed = true
	}
	return changed
}

// applyStopModel prefers stop's legacy `model` slug when present — preToolUse
// may already have stored the shorter model_id. Falls back to model_id only
// when the fragment still has no model.
func applyStopModel(frag *fragment.Fragment, p Payload) {
	if model := strings.TrimSpace(p.Model); model != "" {
		frag.Model = model
	} else if frag.Model == "" {
		if id := strings.TrimSpace(p.ModelID); id != "" {
			frag.Model = id
		}
	}
	if provider := strings.TrimSpace(p.Provider); provider != "" {
		frag.Provider = provider
	}
}
