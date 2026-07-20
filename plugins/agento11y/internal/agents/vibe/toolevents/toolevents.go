// Package toolevents persists per-tool-call outcomes between vibe's
// after_tool hook fires and the post_agent_turn export.
//
// Vibe runs the tool calls within a turn concurrently, so multiple
// after_tool hooks can fire at once. Each writes its own file named by the
// (sanitized) tool_call_id, so two concurrent fires never touch the same
// file and no lock is needed. The post_agent_turn handler loads the events
// for the session to give each execute_tool span real timing and an error
// status, then clears them so they do not leak into the next turn.
package toolevents

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// Event is one tool call's recorded outcome, taken from the after_tool hook
// payload. CompletedAt is stamped when the event is saved (after_tool fires
// right after the tool body returns), so it is a close stand-in for the real
// completion time. DurationMs is vibe's measured tool-body duration.
type Event struct {
	ToolCallID  string    `json:"tool_call_id"`
	ToolName    string    `json:"tool_name,omitempty"`
	Status      string    `json:"status,omitempty"`
	DurationMs  float64   `json:"duration_ms,omitempty"`
	Error       string    `json:"error,omitempty"`
	CompletedAt time.Time `json:"completed_at,omitzero"`
}

// Failed reports whether the tool body did not succeed. Vibe reports
// "success", "failure", or "cancelled"; the latter two are surfaced as a
// span error.
func (e Event) Failed() bool {
	return e.Status == "failure" || e.Status == "cancelled"
}

// ErrorOr returns the recorded error, falling back to a status-derived
// message when the payload carried no error text.
func (e Event) ErrorOr() error {
	if msg := strings.TrimSpace(e.Error); msg != "" {
		return errors.New(msg)
	}
	status := e.Status
	if status == "" {
		status = "failure"
	}
	return fmt.Errorf("tool %s", status)
}

func root() string {
	return filepath.Join(xdg.StateRoot("sigil"), "vibe", "tools")
}

func sessionDir(sessionID string) string {
	return filepath.Join(root(), xdg.SafeComponent(sessionID))
}

func eventPath(sessionID, toolCallID string) string {
	return filepath.Join(sessionDir(sessionID), xdg.SafeComponent(toolCallID)+".json")
}

// Save writes one tool event for the session. The write is atomic
// (temp file + rename) so a concurrent Load never sees a half-written file.
// A blank ToolCallID is rejected because the file name derives from it and a
// shared "unknown" name would let concurrent fires clobber each other.
func Save(sessionID string, e Event) error {
	if strings.TrimSpace(e.ToolCallID) == "" {
		return errors.New("toolevents: empty tool_call_id")
	}
	dir := sessionDir(sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create tool-events dir: %w", err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal tool event: %w", err)
	}
	target := eventPath(sessionID, e.ToolCallID)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp tool event: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename tool event: %w", err)
	}
	return nil
}

// Load reads every persisted tool event for the session, keyed by
// ToolCallID. A missing directory yields an empty map; unreadable or corrupt
// entries are skipped so one bad file never blocks the export.
func Load(sessionID string) map[string]Event {
	out := map[string]Event{}
	entries, err := os.ReadDir(sessionDir(sessionID))
	if err != nil {
		return out
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(sessionDir(sessionID), entry.Name()))
		if err != nil {
			continue
		}
		var e Event
		if err := json.Unmarshal(data, &e); err != nil || e.ToolCallID == "" {
			continue
		}
		out[e.ToolCallID] = e
	}
	return out
}

// Clear removes all persisted tool events for the session. Called after a
// successful export so the next turn starts clean.
func Clear(sessionID string) {
	_ = os.RemoveAll(sessionDir(sessionID))
}
