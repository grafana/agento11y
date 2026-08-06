package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// PromptDecision records how the user answered the one-time import offer.
type PromptDecision string

const (
	PromptSkipped  PromptDecision = "skipped"
	PromptImported PromptDecision = "imported"
)

type promptState struct {
	Agent       AgentID        `json:"agent"`
	Decision    PromptDecision `json:"decision"`
	UpdatedUnix int64          `json:"updated_unix"`
}

// promptStateDir sits under xdg.AppStateRoot, which prefers
// $XDG_STATE_HOME/agento11y but keeps the legacy .../sigil root when that is
// the only one present. Writing to the preferred root here would create it as
// a side effect and move the whole binary's state root, orphaning the existing
// fragment stores, hook offsets, and update stamps.
func promptStateDir() string {
	return filepath.Join(xdg.AppStateRoot(), "history", "prompts")
}

func promptStatePath(agent AgentID) string {
	return filepath.Join(promptStateDir(), xdg.SafeComponent(string(agent))+".json")
}

// ShouldOfferPrompt reports whether the viewer should offer to import an
// agent's history. Once the user dismisses the offer or completes an import,
// it stops asking.
func ShouldOfferPrompt(agent AgentID) (bool, error) {
	if _, ok := Spec(agent); !ok {
		return false, fmt.Errorf("history: unknown agent %q", agent)
	}
	data, err := os.ReadFile(promptStatePath(agent))
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	var state promptState
	if err := json.Unmarshal(data, &state); err != nil {
		return true, nil
	}
	switch state.Decision {
	case PromptSkipped, PromptImported:
		return false, nil
	default:
		return true, nil
	}
}

// MarkPrompt records the offer decision. The file holds no source paths,
// titles, prompts, responses, or tool data.
func MarkPrompt(agent AgentID, decision PromptDecision) error {
	if _, ok := Spec(agent); !ok {
		return fmt.Errorf("history: unknown agent %q", agent)
	}
	switch decision {
	case PromptSkipped, PromptImported:
	default:
		return fmt.Errorf("history: unknown prompt decision %q", decision)
	}
	if err := os.MkdirAll(promptStateDir(), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(promptState{
		Agent:       agent,
		Decision:    decision,
		UpdatedUnix: time.Now().UTC().Unix(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(promptStatePath(agent), data, 0o600)
}
