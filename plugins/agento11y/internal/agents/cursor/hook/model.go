package hook

import (
	"encoding/json"
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

// maxModelParamLen bounds both the id and the value of a model param. They
// become generation tags, and tags become metric attributes, so an unexpected
// long value must not turn into unbounded metric cardinality.
const maxModelParamLen = 64

// modelParamID matches the ids Cursor documents (optimize_for, thinking,
// context, effort). Anything outside this shape is skipped rather than
// promoted to a tag key.
var modelParamID = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)

// modelParams flattens the payload's model_params list into id -> value.
// Values arrive as JSON strings today; a non-string is kept as its raw JSON
// literal so a schema change degrades to a slightly uglier tag rather than to
// no tag at all. Returns nil when nothing usable is present.
func modelParams(p Payload) map[string]string {
	if len(p.ModelParams) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.ModelParams))
	for _, mp := range p.ModelParams {
		id := strings.TrimSpace(mp.ID)
		if id == "" || len(id) > maxModelParamLen || !modelParamID.MatchString(id) {
			continue
		}
		value := strings.TrimSpace(string(mp.Value))
		if value == "" || value == "null" {
			continue
		}
		var asString string
		if err := json.Unmarshal(mp.Value, &asString); err == nil {
			value = strings.TrimSpace(asString)
		}
		if value == "" || len(value) > maxModelParamLen {
			continue
		}
		out[id] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// applyModelParams merges the payload's model_params onto the fragment. Later
// events win per id, but an event that carries no params never clears what an
// earlier one stored — preToolUse-only turns would otherwise lose them.
// Returns true when the fragment changed.
func applyModelParams(f *fragment.Fragment, p Payload) bool {
	params := modelParams(p)
	if len(params) == 0 {
		return false
	}
	changed := false
	for id, value := range params {
		if f.ModelParams[id] == value {
			continue
		}
		if f.ModelParams == nil {
			f.ModelParams = make(map[string]string, len(params))
		}
		f.ModelParams[id] = value
		changed = true
	}
	return changed
}

// applyModelMeta copies model, provider, and model params onto the fragment
// when the fragment still lacks them. Returns true when any field was filled
// so callers can decide whether to rewrite.
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
	if applyModelParams(f, p) {
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
	applyModelParams(frag, p)
}
