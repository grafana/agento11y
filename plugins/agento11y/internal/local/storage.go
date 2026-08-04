// Package local implements the in-process HTTP receiver used by
// `agento11y pi --local` and `agento11y claude --local`. It stores generation
// exports to JSONL files under the agento11y state root so agent sessions can
// be inspected with standard shell tools, without requiring a Grafana Cloud
// or local stack deployment.
package local

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
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

	// logger reports lines a read skipped. nil discards them; the daemon
	// attaches its logger through SetLogger before it serves.
	logger *log.Logger

	// openAppend opens one JSONL file for appending. Tests replace it to
	// count open/close cycles; a nil value uses openAppendFile.
	openAppend func(path string) (io.WriteCloser, error)

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

// SetLogger attaches a logger for read-side diagnostics. Call it before
// the first read; a Storage without one stays silent.
func (s *Storage) SetLogger(logger *log.Logger) { s.logger = logger }

// logSkipped reports lines a read could not decode. skipped counts one
// request, and what names that read in the log line. An interrupted append
// leaves a truncated tail, which costs at most one line per file, so a
// count above the number of files the request read means the store holds
// records this build cannot read.
func (s *Storage) logSkipped(what string, skipped int) {
	if skipped == 0 || s.logger == nil {
		return
	}
	s.logger.Printf("local: %s: skipped %d unparseable lines", what, skipped)
}

// Path returns the absolute path for the named JSONL file in this Storage.
func (s *Storage) Path(name string) string {
	return filepath.Join(s.dir, name)
}

// Append writes one JSON-encoded record followed by a newline to the named
// file. Files are created with 0o600 permissions; the per-path mutex
// prevents concurrent goroutines in this process from interleaving lines.
//
// This is the single-record form. Generation ingest batches through
// AppendGenerations instead.
func (s *Storage) Append(name string, record any) error {
	_, err := appendJSONL(s, name, []any{record})
	return err
}

// appendJSONL writes every record as one JSON line to the named file
// through a single open and close cycle. It returns how many records
// reached the file: on error the records before the returned count are
// written and the rest are not, so a caller with per-record results can
// report exactly which ones it accepted.
//
// A missing parent directory is recreated once and the open retried, so
// ingest and export recover if the conversations directory disappears
// while the daemon runs.
func appendJSONL[T any](s *Storage, name string, records []T) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}
	path := s.Path(name)
	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	f, err := s.openForAppend(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	for i, record := range records {
		data, err := json.Marshal(record)
		if err != nil {
			return i, fmt.Errorf("local storage: marshal %s: %w", name, err)
		}
		data = append(data, '\n')
		if _, err := f.Write(data); err != nil {
			return i, fmt.Errorf("local storage: write %s: %w", path, err)
		}
	}
	return len(records), nil
}

// openForAppend opens path for appending, recreating the parent directory
// when it is gone (a user or another tool removed it under the running
// daemon) and retrying the open once.
func (s *Storage) openForAppend(path string) (io.WriteCloser, error) {
	open := s.openAppend
	if open == nil {
		open = openAppendFile
	}
	f, err := open(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("local storage: open %s: %w", path, err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return nil, fmt.Errorf("local storage: mkdir %s: %w", filepath.Dir(path), mkErr)
	}
	f, err = open(path)
	if err != nil {
		return nil, fmt.Errorf("local storage: open %s: %w", path, err)
	}
	return f, nil
}

func openAppendFile(path string) (io.WriteCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
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

// AppendGeneration writes one record into conversations/<id>.jsonl through
// AppendGenerations. Only the tests call it; the ingest handler passes a
// whole batch.
func (s *Storage) AppendGeneration(rec generationRecord) error {
	_, err := s.AppendGenerations(rec.ConversationID, []generationRecord{rec})
	return err
}

// AppendGenerations writes every record for one conversation into
// conversations/<convID>.jsonl through a single open and close cycle, so
// one export request costs one file open per conversation it touches.
//
// It returns how many records were written. On error the first n records
// are stored and the remaining records are not. The ingest handler turns
// that boundary into a per-generation accepted or rejected result.
func (s *Storage) AppendGenerations(convID string, recs []generationRecord) (int, error) {
	if convID == "" {
		return 0, fmt.Errorf("local storage: empty conversation_id")
	}
	if !validConversationID(convID) {
		return 0, fmt.Errorf("local storage: unsafe conversation_id %q", convID)
	}
	for _, rec := range recs {
		if rec.ConversationID != convID {
			return 0, fmt.Errorf("local storage: record conversation_id %q does not match batch %q", rec.ConversationID, convID)
		}
	}
	return appendJSONL(s, filepath.Join(ConversationsDir, convID+".jsonl"), recs)
}

func validConversationID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\") && !strings.ContainsRune(id, 0)
}
