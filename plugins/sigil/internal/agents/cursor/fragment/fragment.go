// Package fragment is the on-disk per-generation accumulator. Each Cursor
// hook invocation is a fresh process, so per-turn state is kept on disk
// between events.
//
// We don't lock the fragment file. Cursor's hook runtime appears to dispatch
// events serially per conversation/generation in practice (the official
// shell-script examples and ecosystem plugins use plain `>> file` appends
// without coordination). If that assumption ever breaks, the failure mode is
// a lost append on a streaming chunk or a tool record — recoverable for
// telemetry, not correctness-critical.
package fragment

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/fragmentstore"
)

// ToolRecord captures one tool invocation observed via postToolUse(Failure).
//
// ToolInput and ToolOutput are stored only when the active content-capture
// mode is `full` — otherwise the hook handler drops them before the fragment
// is saved, so we never persist bytes we don't intend to export.
type ToolRecord struct {
	ToolName     string          `json:"toolName"`
	ToolUseID    string          `json:"toolUseId,omitempty"`
	ToolInput    json.RawMessage `json:"toolInput,omitempty"`
	ToolOutput   json.RawMessage `json:"toolOutput,omitempty"`
	DurationMs   *float64        `json:"durationMs,omitempty"`
	Cwd          string          `json:"cwd,omitempty"`
	Status       string          `json:"status,omitempty"`
	CompletedAt  string          `json:"completedAt,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
}

// AssistantSegment is one chunk of assistant response text, in arrival order.
type AssistantSegment struct {
	Text      string `json:"text"`
	Timestamp string `json:"timestamp,omitempty"`
}

// TokenCounts are observed in afterAgentResponse and stop. Stop's counts
// reflect the full turn (including cache + tool rounds) and overwrite any
// earlier afterAgentResponse counts.
type TokenCounts struct {
	InputTokens      *int64 `json:"inputTokens,omitempty"`
	OutputTokens     *int64 `json:"outputTokens,omitempty"`
	CacheReadTokens  *int64 `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens *int64 `json:"cacheWriteTokens,omitempty"`
}

// Fragment is the per-generation accumulator written to disk.
type Fragment struct {
	ConversationID  string             `json:"conversationId"`
	GenerationID    string             `json:"generationId"`
	Assistant       []AssistantSegment `json:"assistant,omitempty"`
	Tools           []ToolRecord       `json:"tools,omitempty"`
	UserPrompt      string             `json:"userPrompt,omitempty"`
	TokenUsage      *TokenCounts       `json:"tokenUsage,omitempty"`
	ThinkingPresent bool               `json:"thinkingPresent,omitempty"`
	StartedAt       string             `json:"startedAt,omitempty"`
	LastEventAt     string             `json:"lastEventAt,omitempty"`
	Model           string             `json:"model,omitempty"`
	Provider        string             `json:"provider,omitempty"`

	// PendingStop is set by handleStop before it tries to emit. If emission
	// fails the fragment stays on disk with this set so sessionEnd can replay
	// with the original status instead of "aborted". Absent → stop never ran.
	PendingStop *PendingStop `json:"pendingStop,omitempty"`
}

// PendingStop pairs the stop event's status and error so they always move
// together. The empty Status string encodes "stop was seen but status was
// unspecified" (distinct from a nil PendingStop, which means stop never ran).
type PendingStop struct {
	Status string          `json:"status"`
	Error  json.RawMessage `json:"error,omitempty"`
}

// Session captures the metadata recorded at sessionStart time.
type Session struct {
	ConversationID    string   `json:"conversationId"`
	WorkspaceRoots    []string `json:"workspaceRoots,omitempty"`
	UserEmail         string   `json:"userEmail,omitempty"`
	CursorVersion     string   `json:"cursorVersion,omitempty"`
	IsBackgroundAgent bool     `json:"isBackgroundAgent,omitempty"`
	ConversationTitle string   `json:"conversationTitle,omitempty"`
	StartedAt         string   `json:"startedAt,omitempty"`
}

// Touch keeps the per-hook timestamps in sync. First arrival wins for
// StartedAt; last arrival wins for LastEventAt.
func Touch(f *Fragment, ts string) {
	if ts == "" {
		return
	}
	if f.StartedAt == "" {
		f.StartedAt = ts
	}
	f.LastEventAt = ts
}

// LoadTolerant reads the fragment from disk. Missing files return nil; corrupt
// JSON is logged and treated as missing. The corrupt file stays on disk so
// sessionEnd can quarantine it rather than the data being silently destroyed.
func LoadTolerant(conversationID, generationID string, logger *log.Logger) *Fragment {
	path := FragmentFilePath(conversationID, generationID)
	f, corrupt, err := fragmentstore.ReadJSON[Fragment](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "", path, corrupt, err)
		return nil
	}
	if f == nil {
		return nil
	}
	// Reassert IDs from the path in case the on-disk file was tampered with.
	f.ConversationID = conversationID
	f.GenerationID = generationID
	return f
}

// Save writes the fragment atomically. Mode 0600 — fragments may carry
// prompts and tool I/O between hook invocations.
func Save(f *Fragment) error {
	return fragmentstore.WriteJSON(FragmentFilePath(f.ConversationID, f.GenerationID), f)
}

// Delete removes the fragment file. ENOENT is not an error.
func Delete(conversationID, generationID string) error {
	err := os.Remove(FragmentFilePath(conversationID, generationID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Quarantine renames a corrupt fragment to `<path>.corrupt-<pid>` so the data
// isn't silently destroyed during sessionEnd's sweep.
func Quarantine(conversationID, generationID string) error {
	src := FragmentFilePath(conversationID, generationID)
	dst := fmt.Sprintf("%s.corrupt-%d", src, os.Getpid())
	if err := os.Rename(src, dst); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("fragment: quarantine: %w", err)
	}
	return nil
}

// LoadSession reads session metadata. Returns nil when missing; corrupt files
// are logged and treated as missing.
func LoadSession(conversationID string, logger *log.Logger) *Session {
	path := SessionFilePath(conversationID)
	s, corrupt, err := fragmentstore.ReadJSON[Session](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "session ", path, corrupt, err)
		return nil
	}
	return s
}

// SaveSession writes session metadata atomically.
func SaveSession(s Session) error {
	return fragmentstore.WriteJSON(SessionFilePath(s.ConversationID), s)
}

// Update is the canonical read-modify-write. The mutator returns true when
// the fragment should be saved; false skips the write (used by
// afterAgentThought to avoid rewriting the fragment when ThinkingPresent is
// already set).
//
// A corrupt or unreadable fragment on disk is treated as "start fresh".
func Update(conversationID, generationID string, logger *log.Logger, mutate func(f *Fragment) bool) error {
	f := LoadTolerant(conversationID, generationID, logger)
	if f == nil {
		f = &Fragment{ConversationID: conversationID, GenerationID: generationID}
	}
	if !mutate(f) {
		return nil
	}
	f.ConversationID = conversationID
	f.GenerationID = generationID
	return Save(f)
}

// ListFragmentIDs returns the original generation IDs in a conversation
// directory. Missing directories return nil; other errors are logged and
// return nil.
//
// IDs come from the JSON body, not the filename: FragmentFilePath routes the
// generation ID through xdg.SafeComponent for path-traversal safety, so the
// filename component is not the inverse of the ID. Reading the JSON gives
// callers an ID they can hand back to LoadTolerant/Delete/Quarantine without
// double-encoding.
func ListFragmentIDs(conversationID string, logger *log.Logger) []string {
	dir := ConversationDir(conversationID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if logger != nil {
			logger.Printf("fragment: readdir %s: %v", dir, err)
		}
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if ParseFragmentFilename(e.Name()) == "" {
			continue
		}
		path := filepath.Join(dir, e.Name())
		f, corrupt, err := fragmentstore.ReadJSON[Fragment](path)
		if err != nil {
			fragmentstore.LogLoadErr(logger, "", path, corrupt, err)
			continue
		}
		if f == nil || f.GenerationID == "" {
			continue
		}
		ids = append(ids, f.GenerationID)
	}
	return ids
}

// RemoveConversationDir wipes the entire per-conversation directory. Best
// effort — sessionEnd uses this once all fragments have been emitted.
func RemoveConversationDir(conversationID string) error {
	return os.RemoveAll(ConversationDir(conversationID))
}
