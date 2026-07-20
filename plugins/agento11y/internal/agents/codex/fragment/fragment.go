package fragment

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/fragmentstore"
)

const DefaultStaleAge = 24 * time.Hour

type ToolRecord struct {
	ToolName     string          `json:"toolName"`
	ToolUseID    string          `json:"toolUseId,omitempty"`
	ToolInput    json.RawMessage `json:"toolInput,omitempty"`
	ToolResponse json.RawMessage `json:"toolResponse,omitempty"`
	Status       string          `json:"status,omitempty"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	Cwd          string          `json:"cwd,omitempty"`
	StartedAt    string          `json:"startedAt,omitempty"`
	CompletedAt  string          `json:"completedAt,omitempty"`
	DurationMs   *float64        `json:"durationMs,omitempty"`
}

type Session struct {
	SessionID      string `json:"sessionId"`
	Cwd            string `json:"cwd,omitempty"`
	Source         string `json:"source,omitempty"`
	Model          string `json:"model,omitempty"`
	TranscriptPath string `json:"transcriptPath,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastEventAt    string `json:"lastEventAt,omitempty"`
}

type SubagentLink struct {
	ChildSessionID     string `json:"childSessionId"`
	ParentSessionID    string `json:"parentSessionId,omitempty"`
	ParentTurnID       string `json:"parentTurnId,omitempty"`
	ParentGenerationID string `json:"parentGenerationId,omitempty"`
	SpawnCallID        string `json:"spawnCallId,omitempty"`
	AgentRole          string `json:"agentRole,omitempty"`
	AgentNickname      string `json:"agentNickname,omitempty"`
	AgentDepth         int    `json:"agentDepth,omitempty"`
	Source             string `json:"source,omitempty"`
	LastResolvedAt     string `json:"lastResolvedAt,omitempty"`
}

type Fragment struct {
	SessionID            string       `json:"sessionId"`
	TurnID               string       `json:"turnId"`
	Cwd                  string       `json:"cwd,omitempty"`
	Source               string       `json:"source,omitempty"`
	Model                string       `json:"model,omitempty"`
	Prompt               string       `json:"prompt,omitempty"`
	TranscriptPath       string       `json:"transcriptPath,omitempty"`
	LastAssistantMessage string       `json:"lastAssistantMessage,omitempty"`
	Tools                []ToolRecord `json:"tools,omitempty"`
	StopHookActive       bool         `json:"stopHookActive,omitempty"`
	StartedAt            string       `json:"startedAt,omitempty"`
	CompletedAt          string       `json:"completedAt,omitempty"`
	LastEventAt          string       `json:"lastEventAt,omitempty"`
	// PendingRetry is set when a Stop attempt completed the read-side work
	// (mapping, etc.) but failed to export via Sigil. The next Stop event for
	// the same session sweeps these and tries again so a transient ingest
	// hiccup doesn't silently lose a turn after the 24h cleanup.
	PendingRetry bool `json:"pendingRetry,omitempty"`
}

func Touch(f *Fragment, ts string) {
	if ts == "" {
		return
	}
	if f.StartedAt == "" {
		f.StartedAt = ts
	}
	f.LastEventAt = ts
}

func TouchSession(s *Session, ts string) {
	if ts == "" {
		return
	}
	if s.StartedAt == "" {
		s.StartedAt = ts
	}
	s.LastEventAt = ts
}

func LoadSessionTolerant(sessionID string, logger *log.Logger) *Session {
	path := SessionFilePath(sessionID)
	s, corrupt, err := fragmentstore.ReadJSON[Session](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "session ", path, corrupt, err)
		return nil
	}
	if s == nil {
		return nil
	}
	s.SessionID = sessionID
	return s
}

func SaveSession(s *Session) error {
	return fragmentstore.WriteJSON(SessionFilePath(s.SessionID), s)
}

func UpdateSession(sessionID string, logger *log.Logger, mutate func(s *Session) bool) error {
	return fragmentstore.WithFileLock(SessionFilePath(sessionID), func() error {
		s := LoadSessionTolerant(sessionID, logger)
		if s == nil {
			s = &Session{SessionID: sessionID}
		}
		if !mutate(s) {
			return nil
		}
		s.SessionID = sessionID
		return SaveSession(s)
	})
}

func LoadSubagentLinkTolerant(childSessionID string, logger *log.Logger) *SubagentLink {
	path := SubagentLinkFilePath(childSessionID)
	link, corrupt, err := fragmentstore.ReadJSON[SubagentLink](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "subagent link ", path, corrupt, err)
		return nil
	}
	if link == nil {
		return nil
	}
	link.ChildSessionID = childSessionID
	return link
}

func SaveSubagentLink(link *SubagentLink) error {
	return fragmentstore.WriteJSON(SubagentLinkFilePath(link.ChildSessionID), link)
}

func UpdateSubagentLink(childSessionID string, logger *log.Logger, mutate func(link *SubagentLink) bool) error {
	return fragmentstore.WithFileLock(SubagentLinkFilePath(childSessionID), func() error {
		link := LoadSubagentLinkTolerant(childSessionID, logger)
		if link == nil {
			link = &SubagentLink{ChildSessionID: childSessionID}
		}
		if !mutate(link) {
			return nil
		}
		link.ChildSessionID = childSessionID
		return SaveSubagentLink(link)
	})
}

func DeleteSubagentLink(childSessionID string) error {
	return fragmentstore.WithFileLock(SubagentLinkFilePath(childSessionID), func() error {
		err := os.Remove(SubagentLinkFilePath(childSessionID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func LoadTolerant(sessionID, turnID string, logger *log.Logger) *Fragment {
	path := FragmentFilePath(sessionID, turnID)
	f, corrupt, err := fragmentstore.ReadJSON[Fragment](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "", path, corrupt, err)
		return nil
	}
	if f == nil {
		return nil
	}
	f.SessionID = sessionID
	f.TurnID = turnID
	return f
}

func Save(f *Fragment) error {
	return fragmentstore.WriteJSON(FragmentFilePath(f.SessionID, f.TurnID), f)
}

func Delete(sessionID, turnID string) error {
	return fragmentstore.WithFileLock(FragmentFilePath(sessionID, turnID), func() error {
		err := os.Remove(FragmentFilePath(sessionID, turnID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func Update(sessionID, turnID string, logger *log.Logger, mutate func(f *Fragment) bool) error {
	return fragmentstore.WithFileLock(FragmentFilePath(sessionID, turnID), func() error {
		f := LoadTolerant(sessionID, turnID, logger)
		if f == nil {
			f = &Fragment{SessionID: sessionID, TurnID: turnID}
		}
		if !mutate(f) {
			return nil
		}
		f.SessionID = sessionID
		f.TurnID = turnID
		return Save(f)
	})
}

// ListTurnFiles returns the absolute paths of every turn fragment JSON on
// disk for a session. Lock files and non-JSON entries are skipped. Returns
// an empty slice if the session has no turns dir or it can't be read.
func ListTurnFiles(sessionID string, logger *log.Logger) []string {
	dir := TurnsDir(sessionID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) && logger != nil {
			logger.Printf("fragment: list turns %s: %v", dir, err)
		}
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// LoadFile reads a fragment from an explicit file path. Used by the retry
// sweep which enumerates the turns directory directly.
func LoadFile(path string, logger *log.Logger) *Fragment {
	f, corrupt, err := fragmentstore.ReadJSON[Fragment](path)
	if err != nil {
		fragmentstore.LogLoadErr(logger, "", path, corrupt, err)
		return nil
	}
	return f
}

// DeleteFile removes a fragment file by absolute path.
func DeleteFile(path string) error {
	return fragmentstore.WithFileLock(path, func() error {
		err := os.Remove(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
}

func CleanupStale(maxAge time.Duration, now time.Time, logger *log.Logger) {
	CleanupStaleExcept(maxAge, now, logger, "", "")
}

func CleanupStaleExcept(maxAge time.Duration, now time.Time, logger *log.Logger, sessionID, turnID string) {
	if maxAge <= 0 {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	root := StateRoot()
	cutoff := now.Add(-maxAge)
	turnSkipPath := ""
	if sessionID != "" && turnID != "" {
		turnSkipPath = FragmentFilePath(sessionID, turnID)
	}
	sessionSkipPath := ""
	if sessionID != "" {
		sessionSkipPath = SessionFilePath(sessionID)
	}
	if err := fragmentstore.CleanupStaleDir(filepath.Join(root, "turns"), cutoff, logger, turnSkipPath); err != nil && logger != nil {
		logger.Printf("fragment: cleanup turns: %v", err)
	}
	if err := fragmentstore.CleanupStaleDir(filepath.Join(root, "sessions"), cutoff, logger, sessionSkipPath); err != nil && logger != nil {
		logger.Printf("fragment: cleanup sessions: %v", err)
	}
	subagentSkipPath := ""
	if sessionID != "" {
		subagentSkipPath = SubagentLinkFilePath(sessionID)
	}
	if err := fragmentstore.CleanupStaleDir(filepath.Join(root, "subagents"), cutoff, logger, subagentSkipPath); err != nil && logger != nil {
		logger.Printf("fragment: cleanup subagents: %v", err)
	}
}
