// Package local implements the in-process HTTP receiver used by
// `agento11y pi --local` and `agento11y claude --local`. It stores generation
// exports in SQLite under the agento11y state root, without requiring a
// Grafana Cloud or local stack deployment.
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
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// errLockBusy reports that another process holds the requested file lock.
var errLockBusy = errors.New("lock held by another process")

// File names under the local state root. Kept exported so tests and docs
// can reference the canonical paths.
const (
	// ConversationsDir holds one JSONL file per conversation. Each line
	// is a generationRecord; the filename is <conversation_id>.jsonl.
	ConversationsDir = "conversations"

	StatusFile = "server.json"
	LockFile   = "server.lock"

	// MigrationLockFile lets one daemon at a time copy JSONL into SQLite.
	MigrationLockFile = "migration.lock"
	// RetirementLockFile coordinates dual-writes with cross-process JSONL retirement.
	RetirementLockFile = "retirement.lock"
)

// StateDir returns the root directory for local capture data.
func StateDir() string {
	return filepath.Join(xdg.AppStateRoot(), "local")
}

// Storage owns the local SQLite store and any legacy JSONL files being
// migrated. It serialises legacy appends so concurrent handlers cannot
// interleave lines.
type Storage struct {
	dir string

	// logger reports lines a read skipped. nil discards them; the daemon
	// attaches its logger through SetLogger before it serves.
	logger *log.Logger

	// sql mirrors successful JSONL appends while an existing store migrates,
	// then becomes the only write target after the rollback copy is retired.
	// It is nil in tests that exercise the legacy writer alone.
	sql *sqlStore

	// modeMu keeps the one-time switch to SQLite-only writes from racing an
	// in-flight dual-write in this process.
	modeMu  sync.RWMutex
	sqlOnly bool

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
	if skipped == 0 {
		return
	}
	s.logf("local: %s: skipped %d unparseable lines", what, skipped)
}

// logf writes one diagnostic line, and does nothing on a Storage with no
// logger.
func (s *Storage) logf(format string, args ...any) {
	if s.logger == nil {
		return
	}
	s.logger.Printf(format, args...)
}

// Path returns the absolute path for the named JSONL file in this Storage.
func (s *Storage) Path(name string) string {
	return filepath.Join(s.dir, name)
}

// appendJSONLLocked appends legacy JSONL with the per-path lock already
// held. Generation ingest keeps that lock through its SQLite mirror write so
// migration cannot snapshot the JSONL between the two writes.
func appendJSONLLocked[T any](s *Storage, name string, records []T) (written int, previousSize int64, previousMTime time.Time, err error) {
	path := s.Path(name)
	if info, statErr := os.Stat(path); statErr == nil {
		previousSize = info.Size()
		previousMTime = info.ModTime()
	}
	written, err = writeJSONL(s, path, name, records)
	return written, previousSize, previousMTime, err
}

func writeJSONL[T any](s *Storage, path, name string, records []T) (int, error) {
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
	f, err := openAppendFile(path)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("local storage: open %s: %w", path, err)
	}
	if mkErr := os.MkdirAll(filepath.Dir(path), 0o700); mkErr != nil {
		return nil, fmt.Errorf("local storage: mkdir %s: %w", filepath.Dir(path), mkErr)
	}
	f, err = openAppendFile(path)
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

func (s *Storage) AppendGeneration(rec generationRecord) error {
	_, err := s.AppendGenerations(rec.ConversationID, []generationRecord{rec})
	return err
}

// AppendGenerations stores one conversation batch and returns the prefix
// accepted by ingest: recs[:n] was written. JSONL can return a short prefix;
// SQLite-only writes return either zero or the full batch.
func (s *Storage) AppendGenerations(convID string, recs []generationRecord) (int, error) {
	if convID == "" {
		return 0, fmt.Errorf("local storage: empty conversation_id")
	}
	if !validConversationID(convID) {
		return 0, fmt.Errorf("local storage: unsafe conversation_id %q", convID)
	}
	if len(recs) == 0 {
		return 0, nil
	}
	for _, rec := range recs {
		if rec.ConversationID != convID {
			return 0, fmt.Errorf("local storage: record conversation_id %q does not match batch %q", rec.ConversationID, convID)
		}
	}
	var written int
	err := withRetirementReadLock(s.dir, func() error {
		var appendErr error
		written, appendErr = s.appendGenerationsLocked(convID, recs)
		return appendErr
	})
	return written, err
}

func (s *Storage) appendGenerationsLocked(convID string, recs []generationRecord) (int, error) {
	s.refreshSQLiteOnly()
	s.modeMu.RLock()
	defer s.modeMu.RUnlock()
	if s.sqlOnly {
		if s.sql == nil {
			return 0, errors.New("local storage: SQLite-only store is not open")
		}
		if err := s.sql.writeGenerations(recs); err != nil {
			return 0, fmt.Errorf("local storage: write SQLite: %w", err)
		}
		return len(recs), nil
	}

	name := filepath.Join(ConversationsDir, convID+".jsonl")
	path := s.Path(name)
	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	written, previousSize, previousMTime, err := appendJSONLLocked(s, name, recs)
	if written > 0 && s.sql != nil {
		var currentSize int64
		var mtime time.Time
		if info, statErr := os.Stat(path); statErr == nil {
			currentSize = info.Size()
			mtime = info.ModTime()
		}
		if sqlErr := s.sql.appendGenerations(name, previousSize, currentSize, previousMTime, mtime, recs[:written]); sqlErr != nil {
			s.logf("local: mirror conversation %s to SQLite: %v", convID, sqlErr)
			if markerErr := s.markForRemigration(convID); markerErr != nil {
				s.logf("local: record conversation %s for SQLite migration: %v", convID, markerErr)
			}
		}
	}
	return written, err
}

func (s *Storage) refreshSQLiteOnly() {
	if s.sql == nil {
		return
	}
	s.modeMu.RLock()
	only := s.sqlOnly
	s.modeMu.RUnlock()
	if only {
		return
	}
	retired, err := s.sql.jsonlRetired()
	if err != nil {
		s.logf("local: read SQLite retirement state: %v", err)
		return
	}
	if retired {
		s.setSQLiteOnly()
	}
}

// writeFileAtomic writes data to dir/name through a temp file and a
// rename, so a concurrent reader sees either the old file or the new one
// and never a half-written one. The file ends up at 0o600 whatever the
// caller's umask is.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// A no-op once the rename below succeeds; on any earlier return it keeps
	// a failed write from leaving the temp file behind.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, name))
}

func validConversationID(id string) bool {
	return id != "" && !strings.ContainsAny(id, "/\\") && !strings.ContainsRune(id, 0)
}
