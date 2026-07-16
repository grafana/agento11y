// Package state persists per-session vibe agent state across hook
// invocations: the byte offset into messages.jsonl and the prior
// session-token snapshot used to compute per-turn deltas.
package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/xdg"
)

// Session holds the persisted state for a single vibe session.
//
// Offset tracks how far into messages.jsonl we have already exported, so
// subsequent post_agent_turn hooks only read new lines.
//
// SessionPromptTokens / SessionCompletionTokens / SessionCost and the
// ToolCalls* counters record the meta.json session-wide totals at the time
// of the last successful export. The next export uses these as the baseline
// for the per-turn delta of tokens, cost, and tool-call outcomes.
//
// LastGenerationID is the generation ID of the most recent export for this
// session. A child (subagent) session reads its parent session's
// LastGenerationID to set a real ParentGenerationIDs edge.
type Session struct {
	Offset                  int64   `json:"offset"`
	SessionPromptTokens     int64   `json:"session_prompt_tokens,omitempty"`
	SessionCompletionTokens int64   `json:"session_completion_tokens,omitempty"`
	SessionCost             float64 `json:"session_cost,omitempty"`
	ToolCallsRejected       int64   `json:"tool_calls_rejected,omitempty"`
	ToolCallsHookDenied     int64   `json:"tool_calls_hook_denied,omitempty"`
	ToolCallsFailed         int64   `json:"tool_calls_failed,omitempty"`
	LastGenerationID        string  `json:"last_generation_id,omitempty"`
	Title                   string  `json:"title,omitempty"`
}

func dir() string {
	return filepath.Join(xdg.StateRoot("sigil"), "vibe")
}

// SanitizeSessionID delegates to xdg.SafeComponent so session-ID-derived
// filenames are scrubbed and hash-suffixed consistently across agents.
func SanitizeSessionID(id string) string {
	return xdg.SafeComponent(id)
}

func path(sessionID string) string {
	return filepath.Join(dir(), SanitizeSessionID(sessionID)+".state")
}

// Load reads the persisted state for a session. The bool reports whether a
// usable state file was found: it is false when the file does not exist or
// is corrupt, in which case a zero-value Session is returned. Callers use it
// to tell a legitimate first turn (no prior state, found=false, low step
// count) apart from a mid-session state loss (found=false but the session is
// already several turns in), which must not be billed the full cumulative
// total as a single turn.
func Load(sessionID string) (Session, bool) {
	data, err := os.ReadFile(path(sessionID))
	if err != nil {
		return Session{}, false
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		fmt.Fprintln(os.Stderr, "agento11y: corrupt vibe state, resetting:", err)
		return Session{}, false
	}
	return s, true
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
	target := path(sessionID)
	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("write temp state: %w", err)
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename state: %w", err)
	}
	return nil
}

// Delete removes the persisted state for a session. A missing file is not an
// error, so callers can use it to roll back to the no-state condition.
func Delete(sessionID string) error {
	if err := os.Remove(path(sessionID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("delete state: %w", err)
	}
	return nil
}
