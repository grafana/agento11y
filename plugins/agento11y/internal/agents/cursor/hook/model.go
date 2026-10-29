package hook

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/redact"
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
	if mergeModelParams(f, p.ModelParams) {
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
	mergeModelParams(frag, p.ModelParams)
}

// thoughtStepID returns Cursor's per-model-step generation_id when
// afterAgentThought attached the `-<n>-<xxxx>` suffix. A bare turn id
// returns empty: that event has no step identity to match against.
func thoughtStepID(generationID string) string {
	trimmed := strings.TrimSpace(generationID)
	if trimmed == "" || turnGenerationID(trimmed) == trimmed {
		return ""
	}
	return trimmed
}

// maxModelParamBytes bounds one model_params value stored on the fragment.
const maxModelParamBytes = 256

// mergeModelParams upserts Cursor's model_params onto the fragment. A missing
// or unreadable payload changes nothing, so a later event that omits the
// field cannot wipe parameters an earlier event stored. Returns true when a
// value changed.
func mergeModelParams(f *fragment.Fragment, raw json.RawMessage) bool {
	parsed := parseModelParams(raw)
	if len(parsed) == 0 {
		return false
	}
	if f.ModelParams == nil {
		f.ModelParams = make(map[string]string, len(parsed))
	}
	changed := false
	for id, value := range parsed {
		if f.ModelParams[id] == value {
			continue
		}
		f.ModelParams[id] = value
		changed = true
	}
	return changed
}

// parseModelParams reads model_params without failing the hook event. Values
// may be strings, bools, or numbers; anything else is JSON-encoded. An id or
// value that is empty is skipped. A shape that is not an array yields nothing.
func parseModelParams(raw json.RawMessage) map[string]string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var items []map[string]any
	if err := dec.Decode(&items); err != nil {
		return nil
	}
	out := make(map[string]string, len(items))
	for _, item := range items {
		id, ok := modelParamScalar(item["id"])
		if !ok {
			continue
		}
		value, ok := modelParamScalar(item["value"])
		if !ok {
			continue
		}
		value = trimModelParam(value)
		if value == "" {
			continue
		}
		out[id] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func modelParamScalar(v any) (string, bool) {
	switch x := v.(type) {
	case nil:
		return "", false
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return "", false
		}
		return s, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		return x.String(), true
	default:
		b, err := json.Marshal(x)
		if err != nil || len(b) == 0 || bytes.Equal(b, []byte("null")) {
			return "", false
		}
		return string(b), true
	}
}

func trimModelParam(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxModelParamBytes {
		return value
	}
	return redact.CutTitle(value, maxModelParamBytes)
}
