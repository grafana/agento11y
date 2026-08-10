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
	"time"

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

	// StoreFile records what the daemon has already done to the store on
	// disk, so the modification-time pass over every conversation file runs
	// once per store and not on every start. See storeMeta.
	StoreFile = "store.json"
	// RepairLockFile guards that pass across processes, because more than
	// one daemon can share a store.
	RepairLockFile = "repair.lock"
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

	// summaries holds the decoded projection of every conversation file the
	// list and the token chart have read, so an unchanged file is not
	// decoded again. It carries its own mutex; the read paths take no other
	// lock.
	summaries summaryCache

	// chtimes stamps a file's modification time. Tests replace it to
	// exercise the failure path; a nil value uses os.Chtimes.
	chtimes func(path string, atime, mtime time.Time) error

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

// Append writes one JSON-encoded record followed by a newline to the named
// file. Files are created with 0o600 permissions; the per-path mutex
// prevents concurrent goroutines in this process from interleaving lines.
//
// This is the single-record form. Generation ingest batches through
// AppendGenerations instead.
func (s *Storage) Append(name string, record any) error {
	_, err := appendJSONL(s, name, []any{record}, nil)
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
//
// activities carries the activity of each record, in the same order. It
// stamps the file's modification time, which is what the conversation list
// orders and bounds on. A nil slice leaves the write time in place, which
// is what Append's non-conversation files want.
func appendJSONL[T any](s *Storage, name string, records []T, activities []time.Time) (int, error) {
	if len(records) == 0 {
		return 0, nil
	}
	path := s.Path(name)
	lock := s.lockFor(path)
	lock.Lock()
	defer lock.Unlock()

	var previous time.Time
	if len(activities) > 0 {
		if info, err := os.Stat(path); err == nil {
			previous = info.ModTime()
		}
	}
	written, err := writeJSONL(s, path, name, records)
	// Only the records that reached the file may set its modification time.
	// A batch cut short by a write error would otherwise stamp activity the
	// file does not hold, which shows the conversation in ranges its records
	// do not cover.
	if activity := newestActivity(activities, written); !activity.IsZero() {
		s.stampActivity(path, activity, previous)
	}
	return written, err
}

// newestActivity is the latest of the first n activities, zero when there
// is none.
func newestActivity(activities []time.Time, n int) time.Time {
	var newest time.Time
	for _, when := range activities[:min(n, len(activities))] {
		if when.After(newest) {
			newest = when
		}
	}
	return newest
}

// writeJSONL is the write half of appendJSONL, split out so the file is
// closed before the stamp is applied to it.
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

// stampActivity sets a conversation file's modification time to the newest
// activity it holds, so the newest-first order in conversationFiles stays
// monotone and the Since bounds computed from it stay sound. previous is
// the file's modification time before this append, zero for a new file.
//
// The stamp never moves backwards: a history import appending months-old
// turns to a conversation that also holds today's must not sink the file
// below them. It never moves past now either, because a generation dated in
// the future would pin its file to the top of every page.
//
// A failed stamp costs ordering, not data: the records are already on disk,
// so the caller still reports them as written and logs the failure.
func (s *Storage) stampActivity(path string, activity, previous time.Time) {
	now := time.Now()
	// A modification time in the future does not come from the records; a
	// restore, or a copy from a machine with a fast clock, leaves one. Clamp
	// it to now, because carrying it into the guard below would skip the
	// stamp and leave the file ordered by its write time.
	if previous.After(now) {
		previous = now
	}
	stamp := activity
	if previous.After(stamp) {
		stamp = previous
	}
	if stamp.IsZero() || stamp.After(now) {
		return
	}
	if err := s.setModTime(path, stamp); err != nil {
		s.logf("local: stamp %s: %v", path, err)
	}
}

func (s *Storage) setModTime(path string, when time.Time) error {
	chtimes := s.chtimes
	if chtimes == nil {
		chtimes = os.Chtimes
	}
	return chtimes(path, when, when)
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
// AppendGenerations, deriving the activity stamp from the record itself.
// Only the tests call it; the ingest handler passes a whole batch with the
// activity it already decoded.
func (s *Storage) AppendGeneration(rec generationRecord) error {
	var gen summaryGeneration
	// A generation this build cannot decode leaves gen zero, so the stamp
	// falls back to received_at. The list drops such a record too, so the
	// ignored error costs nothing.
	_ = json.Unmarshal(rec.Generation, &gen)
	_, err := s.AppendGenerations(rec.ConversationID, []generationRecord{rec}, []time.Time{recordActivity(gen, rec.ReceivedAt)})
	return err
}

// AppendGenerations writes every record for one conversation into
// conversations/<convID>.jsonl through a single open and close cycle, so
// one export request costs one file open per conversation it touches.
//
// It returns how many records were written. On error the first n records
// are stored and the remaining records are not. The ingest handler turns
// that boundary into a per-generation accepted or rejected result.
//
// activities carries one recordActivity per record, in the same order as
// recs. The file's modification time takes the newest activity among the
// records that reached it. A nil slice appends without stamping.
func (s *Storage) AppendGenerations(convID string, recs []generationRecord, activities []time.Time) (int, error) {
	if convID == "" {
		return 0, fmt.Errorf("local storage: empty conversation_id")
	}
	if !validConversationID(convID) {
		return 0, fmt.Errorf("local storage: unsafe conversation_id %q", convID)
	}
	if len(activities) != 0 && len(activities) != len(recs) {
		return 0, fmt.Errorf("local storage: %d activity stamps for %d records", len(activities), len(recs))
	}
	for _, rec := range recs {
		if rec.ConversationID != convID {
			return 0, fmt.Errorf("local storage: record conversation_id %q does not match batch %q", rec.ConversationID, convID)
		}
	}
	name := filepath.Join(ConversationsDir, convID+".jsonl")
	written, err := appendJSONL(s, name, recs, activities)
	if written > 0 || err != nil {
		// A partial write moved the file too, so the cached projection goes
		// on the error path as well.
		s.summaries.invalidate(s.Path(name))
	}
	return written, err
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
