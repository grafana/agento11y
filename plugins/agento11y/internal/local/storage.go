// Package local implements the in-process HTTP receiver used by
// `agento11y pi --local` and `agento11y claude --local`. It stores generation
// exports to JSONL files under the agento11y state root so agent sessions can
// be inspected with standard shell tools, without requiring a Grafana Cloud
// or local stack deployment.
package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// File names under the local state root. Kept exported so tests and docs
// can reference the canonical paths.
const (
	// ConversationsDir holds one JSONL file per conversation. Each line
	// is a generationRecord; the filename is <conversation_id>.jsonl.
	ConversationsDir = "conversations"

	StatusFile = "server.json"
	LockFile   = "server.lock"
)

// StateDir returns the root directory for local capture data.
// All JSONL files and the server status file live under this directory.
func StateDir() string {
	return filepath.Join(xdg.AppStateRoot(), "local")
}

// Storage owns the JSONL files under StateDir and serialises writes so
// concurrent handlers can append safely.
type Storage struct {
	dir string

	// One mutex per file path. We don't expect contention high enough to
	// need finer locking; this just stops interleaved JSON lines.
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewStorage returns a Storage rooted at dir. The directory, the
// conversations subdir, and their parents are created with 0o700
// permissions on first use.
func NewStorage(dir string) (*Storage, error) {
	if dir == "" {
		return nil, fmt.Errorf("local storage: empty dir")
	}
	if err := os.MkdirAll(filepath.Join(dir, ConversationsDir), 0o700); err != nil {
		return nil, fmt.Errorf("local storage: mkdir %s: %w", dir, err)
	}
	return &Storage{dir: dir, locks: map[string]*sync.Mutex{}}, nil
}

// Dir returns the storage root directory.
func (s *Storage) Dir() string { return s.dir }

// Path returns the absolute path for the named JSONL file in this Storage.
func (s *Storage) Path(name string) string {
	return filepath.Join(s.dir, name)
}

// Append writes one JSON-encoded record followed by a newline to the named
// file. Files are created with 0o600 permissions; the per-path mutex
// prevents concurrent goroutines in this process from interleaving lines.
func (s *Storage) Append(name string, record any) error {
	path := s.Path(name)
	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("local storage: marshal %s: %w", name, err)
	}
	data = append(data, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("local storage: open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("local storage: write %s: %w", path, err)
	}
	return nil
}

func (s *Storage) lockFor(path string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lock, ok := s.locks[path]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[path] = lock
	}
	return lock
}

// AppendGeneration writes one record into conversations/<id>.jsonl.
func (s *Storage) AppendGeneration(rec generationRecord) error {
	if rec.ConversationID == "" {
		return fmt.Errorf("local storage: empty conversation_id")
	}
	if !validConversationID(rec.ConversationID) {
		return fmt.Errorf("local storage: unsafe conversation_id %q", rec.ConversationID)
	}
	return s.Append(filepath.Join(ConversationsDir, rec.ConversationID+".jsonl"), rec)
}

func validConversationID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\") && !strings.ContainsRune(id, 0)
}
