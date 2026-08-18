// Package meta parses vibe's <session_dir>/meta.json, the sibling to
// messages.jsonl. Vibe writes this file once per turn alongside the
// transcript (vibe/core/agent_loop.py _save_messages), so when our hook
// fires both files already reflect the finished turn.
package meta

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Meta is the subset of meta.json fields the vibe agent adapter consumes.
// Unknown fields are ignored so vibe can add new top-level keys without
// breaking us.
type Meta struct {
	SessionID       string       `json:"session_id"`
	ParentSessionID string       `json:"parent_session_id,omitempty"`
	Title           string       `json:"title,omitempty"`
	Stats           Stats        `json:"stats"`
	Config          Config       `json:"config"`
	ToolsAvailable  []ToolDef    `json:"tools_available,omitempty"`
	SystemPrompt    SystemPrompt `json:"system_prompt"`
}

// Stats mirrors meta.json's `stats` block. Token fields and the
// tool_calls_* counters are session-wide running totals; per-turn values
// are the delta against the previous run's snapshot stored in state.
//
// SessionCost is a pointer so a meta.json that carries no `session_cost`
// stays distinguishable from one reporting a cost of 0: the first is a cost
// vibe never reported, the second a turn that was free.
type Stats struct {
	Steps                    int      `json:"steps"`
	SessionPromptTokens      int64    `json:"session_prompt_tokens"`
	SessionCompletionTokens  int64    `json:"session_completion_tokens"`
	LastTurnPromptTokens     int64    `json:"last_turn_prompt_tokens"`
	LastTurnCompletionTokens int64    `json:"last_turn_completion_tokens"`
	LastTurnDuration         float64  `json:"last_turn_duration"`
	ContextTokens            int64    `json:"context_tokens"`
	InputPricePerMillion     float64  `json:"input_price_per_million"`
	OutputPricePerMillion    float64  `json:"output_price_per_million"`
	SessionTotalLLMTokens    int64    `json:"session_total_llm_tokens"`
	SessionCost              *float64 `json:"session_cost"`

	// tool_calls_* are session-wide running totals of how the session's
	// tool calls resolved. The per-turn delta (against the prior state
	// snapshot) tells us whether this turn had any rejected, hook-denied,
	// or failed tool calls so the export can surface a failure signal.
	ToolCallsAgreed     int64 `json:"tool_calls_agreed"`
	ToolCallsRejected   int64 `json:"tool_calls_rejected"`
	ToolCallsHookDenied int64 `json:"tool_calls_hook_denied"`
	ToolCallsFailed     int64 `json:"tool_calls_failed"`
	ToolCallsSucceeded  int64 `json:"tool_calls_succeeded"`
}

// Config mirrors the subset of meta.json's `config` block we read.
// ActiveModel is the human-friendly alias ("mistral-medium-3.5"); the
// matching entry in Models carries the API id ("mistral-vibe-cli-latest")
// and provider ("mistral").
type Config struct {
	ActiveModel string       `json:"active_model"`
	Models      ModelConfigs `json:"models,omitempty"`
}

// ModelConfigs is meta.json's config.models. Vibe wrote a JSON array of model
// tables until 2.21.0 and an alias-keyed object from 2.21.0 on (models:
// dict[str, ModelConfig] in vibe/core/config/vibe_schema.py). Both decode into
// the same slice, so ActiveModelRef has one shape to search. The array shape
// still turns up because vibe rewrites meta.json only on save, so a session
// started under an older vibe keeps its array until the first save.
type ModelConfigs []ModelConfig

// UnmarshalJSON decodes either shape. A value in neither shape (a string, a
// number, an object of something other than model tables) decodes to no models
// instead of an error: models only feeds ActiveModelRef, which falls back to
// the alias, and an error here would stop the whole turn export. A malformed
// element inside an array still errors.
func (m *ModelConfigs) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*m = nil
		return nil
	}
	if strings.HasPrefix(string(data), "[") {
		var list []ModelConfig
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*m = list
		return nil
	}
	byAlias := map[string]ModelConfig{}
	if err := json.Unmarshal(data, &byAlias); err != nil {
		*m = nil
		return nil
	}
	out := make(ModelConfigs, 0, len(byAlias))
	for alias, mc := range byAlias {
		// Vibe resolves active_model against the key, and raises when the key
		// and the inner alias disagree (normalize_model_configs in
		// vibe/core/config/vibe_schema.py), so the key wins here too.
		mc.Alias = alias
		out = append(out, mc)
	}
	// Sorted so iteration order does not depend on map order. Aliases come
	// from the map keys, so no two entries tie.
	slices.SortFunc(out, func(a, b ModelConfig) int { return strings.Compare(a.Alias, b.Alias) })
	*m = out
	return nil
}

// ModelConfig is one entry of config.models. Alias matches ActiveModel.
type ModelConfig struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Alias    string `json:"alias"`
}

// ToolDef is one entry of meta.json's tools_available[].
type ToolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

// SystemPrompt mirrors meta.json's `system_prompt` block. It is a dict
// (the full system message) so the meaningful field is Content.
type SystemPrompt struct {
	Content string `json:"content,omitempty"`
}

// ActiveModelRef resolves the active model alias to its provider and API
// id from the models[] table. Provider falls back to "mistral" and the API
// id to the alias itself if no entry matches, so this never returns an
// empty provider for a configured model.
func (m Meta) ActiveModelRef() (provider, apiName string) {
	alias := m.Config.ActiveModel
	for _, mc := range m.Config.Models {
		if mc.Alias == alias || mc.Name == alias {
			provider = mc.Provider
			apiName = mc.Name
			break
		}
	}
	if provider == "" {
		provider = "mistral"
	}
	if apiName == "" {
		apiName = alias
	}
	return provider, apiName
}

// Path derives meta.json's path from the transcript path. Vibe places
// both files in the same session directory.
func Path(transcriptPath string) string {
	return filepath.Join(filepath.Dir(transcriptPath), "meta.json")
}

// Load parses the meta.json sibling of the given transcript path.
func Load(transcriptPath string) (Meta, error) {
	p := Path(transcriptPath)
	data, err := os.ReadFile(p)
	if err != nil {
		return Meta{}, fmt.Errorf("read %s: %w", p, err)
	}
	var m Meta
	if err := json.Unmarshal(data, &m); err != nil {
		return Meta{}, fmt.Errorf("parse %s: %w", p, err)
	}
	return m, nil
}
