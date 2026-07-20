package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// Session holds the persisted state for a single session.
type Session struct {
	Offset int64  `json:"offset"`
	Title  string `json:"title,omitempty"`
	// Model is captured from SessionStart so tool hooks can include model context
	// when calling Sigil guards (PreToolUse events do not include model fields).
	Model string `json:"model,omitempty"`
}

func dir() string {
	return filepath.Join(xdg.StateRoot("sigil"), "claude-code")
}

// SanitizeSessionID delegates to xdg.SafeComponent so session-ID-derived
// filenames are scrubbed and hash-suffixed consistently across agents.
func SanitizeSessionID(id string) string {
	return xdg.SafeComponent(id)
}

func path(sessionID string) string {
	return filepath.Join(dir(), SanitizeSessionID(sessionID)+".state")
}

// Load reads the persisted state for a session.
// Returns zero-value Session if the file doesn't exist or is corrupt.
func Load(sessionID string) Session {
	data, err := os.ReadFile(path(sessionID))
	if err != nil {
		return Session{}
	}

	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "agento11y: corrupt state file, resetting:", err)
		return Session{}
	}

	return s
}

// Save atomically writes session state to disk.
func Save(sessionID string, s Session) error {
	d := dir()
	if err := os.MkdirAll(d, 0o755); err != nil {
		return fmt.Errorf("create state dir: %w", err)
	}

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal state: %w", err)
	}

	tmp := path(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}

	if err := os.Rename(tmp, path(sessionID)); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}

	return nil
}
