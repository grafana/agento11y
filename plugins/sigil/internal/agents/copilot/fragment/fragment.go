package fragment

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/fragmentstore"
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

type ErrorRecord struct {
	Context     string `json:"context,omitempty"`
	Name        string `json:"name,omitempty"`
	Message     string `json:"message,omitempty"`
	Recoverable bool   `json:"recoverable,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

type SubagentRecord struct {
	AgentName        string `json:"agentName,omitempty"`
	AgentDisplayName string `json:"agentDisplayName,omitempty"`
	AgentDescription string `json:"agentDescription,omitempty"`
	TranscriptPath   string `json:"transcriptPath,omitempty"`
	StartedAt        string `json:"startedAt,omitempty"`
	CompletedAt      string `json:"completedAt,omitempty"`
	StopReason       string `json:"stopReason,omitempty"`
}

type TokenUsage struct {
	InputTokens           *int64 `json:"inputTokens,omitempty"`
	OutputTokens          *int64 `json:"outputTokens,omitempty"`
	CacheReadInputTokens  *int64 `json:"cacheReadInputTokens,omitempty"`
	CacheWriteInputTokens *int64 `json:"cacheWriteInputTokens,omitempty"`
	ReasoningTokens       *int64 `json:"reasoningTokens,omitempty"`
}

type Session struct {
	SessionID      string `json:"sessionId"`
	Cwd            string `json:"cwd,omitempty"`
	Source         string `json:"source,omitempty"`
	Surface        string `json:"surface,omitempty"`
	InitialPrompt  string `json:"initialPrompt,omitempty"`
	TranscriptPath string `json:"transcriptPath,omitempty"`
	StartedAt      string `json:"startedAt,omitempty"`
	LastEventAt    string `json:"lastEventAt,omitempty"`
	NextTurnIndex  int    `json:"nextTurnIndex,omitempty"`
	ActiveTurnID   string `json:"activeTurnId,omitempty"`
}

type Fragment struct {
	SessionID       string           `json:"sessionId"`
	TurnID          string           `json:"turnId"`
	Cwd             string           `json:"cwd,omitempty"`
	Source          string           `json:"source,omitempty"`
	Surface         string           `json:"surface,omitempty"`
	Prompt          string           `json:"prompt,omitempty"`
	PromptHash      string           `json:"promptHash,omitempty"`
	InitialPrompt   string           `json:"initialPrompt,omitempty"`
	TranscriptPath  string           `json:"transcriptPath,omitempty"`
	AgentVersion    string           `json:"agentVersion,omitempty"`
	Model           string           `json:"model,omitempty"`
	Provider        string           `json:"provider,omitempty"`
	ReasoningEffort string           `json:"reasoningEffort,omitempty"`
	NativeTurnID    string           `json:"nativeTurnId,omitempty"`
	InteractionID   string           `json:"interactionId,omitempty"`
	RequestID       string           `json:"requestId,omitempty"`
	MessageID       string           `json:"messageId,omitempty"`
	AssistantText   string           `json:"assistantText,omitempty"`
	StopReason      string           `json:"stopReason,omitempty"`
	NextToolIndex   int              `json:"nextToolIndex,omitempty"`
	Tools           []ToolRecord     `json:"tools,omitempty"`
	Errors          []ErrorRecord    `json:"errors,omitempty"`
	Subagents       []SubagentRecord `json:"subagents,omitempty"`
	TokenUsage      TokenUsage       `json:"tokenUsage"`
	StartedAt       string           `json:"startedAt,omitempty"`
	CompletedAt     string           `json:"completedAt,omitempty"`
	LastEventAt     string           `json:"lastEventAt,omitempty"`
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

func StartNextTurn(sessionID string, logger *log.Logger, ts string) (string, *Session, error) {
	var out *Session
	var turnID string
	err := fragmentstore.WithFileLock(SessionFilePath(sessionID), func() error {
		s := LoadSessionTolerant(sessionID, logger)
		if s == nil {
			s = &Session{SessionID: sessionID}
		}
		s.NextTurnIndex++
		turnID = fmt.Sprintf("turn-%06d", s.NextTurnIndex)
		s.ActiveTurnID = turnID
		TouchSession(s, ts)
		if err := SaveSession(s); err != nil {
			return err
		}
		copied := *s
		out = &copied
		return nil
	})
	return turnID, out, err
}

func EnsureActiveTurn(sessionID string, logger *log.Logger, ts string) (string, *Session, error) {
	var out *Session
	var turnID string
	err := fragmentstore.WithFileLock(SessionFilePath(sessionID), func() error {
		s := LoadSessionTolerant(sessionID, logger)
		if s == nil {
			s = &Session{SessionID: sessionID}
		}
		if s.ActiveTurnID == "" {
			s.NextTurnIndex++
			s.ActiveTurnID = fmt.Sprintf("turn-%06d", s.NextTurnIndex)
		}
		turnID = s.ActiveTurnID
		TouchSession(s, ts)
		if err := SaveSession(s); err != nil {
			return err
		}
		copied := *s
		out = &copied
		return nil
	})
	return turnID, out, err
}

func ClearActiveTurn(sessionID, turnID string, logger *log.Logger) error {
	return UpdateSession(sessionID, logger, func(s *Session) bool {
		if s.ActiveTurnID == "" {
			return false
		}
		if turnID != "" && s.ActiveTurnID != turnID {
			return false
		}
		s.ActiveTurnID = ""
		return true
	})
}

func DeleteSession(sessionID string) error {
	return fragmentstore.WithFileLock(SessionFilePath(sessionID), func() error {
		err := os.Remove(SessionFilePath(sessionID))
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

func ListTurnIDs(sessionID string, logger *log.Logger) []string {
	dir := TurnsDir(sessionID)
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
	type item struct {
		id   string
		name string
	}
	items := make([]item, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, _, err := fragmentstore.ReadJSON[Fragment](path)
		if err != nil || f == nil || f.TurnID == "" {
			continue
		}
		items = append(items, item{id: f.TurnID, name: entry.Name()})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.id)
	}
	return out
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
}
