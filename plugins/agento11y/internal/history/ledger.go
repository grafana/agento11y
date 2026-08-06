package history

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
)

// EntryStatus is the lifecycle of one turn's import.
type EntryStatus string

const (
	// StatusPending means the turn was selected but export has not confirmed
	// success. Pending entries are retried on the next run.
	StatusPending EntryStatus = "pending"
	// StatusExported means the generation reached its destination. Exported
	// entries are skipped on a rerun unless force is set.
	StatusExported EntryStatus = "exported"
	// StatusFailed means export was attempted and failed. Failed entries are
	// retried on a rerun.
	StatusFailed EntryStatus = "failed"
	// StatusSkipped means the turn was intentionally not imported. Skipped
	// entries are reconsidered on a rerun.
	StatusSkipped EntryStatus = "skipped"
)

// Entry is the per-turn ledger record. It is content-free by construction:
// every field is identity, status, or a coarse error class. The key is already
// a hash, and no field carries prompt, response, tool output, path, session ID,
// or title.
type Entry struct {
	Key      SourceIdentity `json:"key"`
	Status   EntryStatus    `json:"status"`
	Attempts int            `json:"attempts"`
	// GenerationID is the deterministic export ID, itself a hash-derived
	// token, kept so a rerun can correlate without re-reading the source.
	GenerationID string `json:"generation_id,omitempty"`
	// ErrorClass is a short fixed category ("export_failed"), never the raw
	// error string, which could echo source content.
	ErrorClass    string `json:"error_class,omitempty"`
	FirstSeenUnix int64  `json:"first_seen_unix"`
	UpdatedUnix   int64  `json:"updated_unix"`
}

// Ledger is the private, idempotent import record for one agent. It lives
// under the application state root with 0700 directories and 0600 files, and
// is safe for concurrent use.
//
// The file is append-only JSONL: one status record per [Ledger.Mark], with the
// latest status per key held in memory. Rewriting the whole file on every mark
// is quadratic: marking 20,000 keys that way took 109 seconds, which
// extrapolates to hours and about 12 TB written for the 277,625 turns on the
// development machine.
//
// Duplicate records for one key accumulate as an import reruns, so
// [OpenLedger] compacts the file when it holds more records than keys.
type Ledger struct {
	path    string
	mu      sync.Mutex
	file    *os.File
	entries map[SourceIdentity]Entry
}

func ledgerDir() string {
	return filepath.Join(xdg.AppStateRoot(), "history", "ledger")
}

func ledgerPath(agent AgentID) string {
	// The registered agent IDs are already filename-safe; SafeComponent keeps
	// that true if the set ever grows.
	return filepath.Join(ledgerDir(), xdg.SafeComponent(string(agent))+".jsonl")
}

// OpenLedger loads (or creates) the ledger for an agent. The caller must call
// [Ledger.Close].
//
// A record that cannot be decoded is skipped: a torn final line after a crash
// costs one turn's status, and re-importing that turn is harmless because the
// generation ID is deterministic. A read or permission error is returned
// rather than swallowed, because degrading to an empty ledger would silently
// re-export the whole history.
func OpenLedger(agent AgentID) (*Ledger, error) {
	if strings.TrimSpace(string(agent)) == "" {
		return nil, errors.New("history: open ledger for empty agent")
	}
	return openLedgerAt(ledgerPath(agent))
}

func openLedgerAt(path string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	entries, records, err := readLedger(path)
	if err != nil {
		return nil, err
	}
	l := &Ledger{path: path, entries: entries}
	// More records than keys means earlier runs re-marked the same turns.
	// Compacting on open keeps the file proportional to the turns imported
	// rather than to the number of import attempts.
	if records > len(entries) {
		if err := l.compact(); err != nil {
			return nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open import ledger: %w", err)
	}
	l.file = f
	return l, nil
}

// readLedger returns the latest status per key plus the number of records the
// file held. OpenLedger compares the two to decide whether compaction is due.
func readLedger(path string) (map[SourceIdentity]Entry, int, error) {
	entries := map[SourceIdentity]Entry{}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return entries, 0, nil
		}
		return nil, 0, fmt.Errorf("read import ledger: %w", err)
	}
	defer func() { _ = f.Close() }()

	records := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), maxLedgerLineBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Entry
		if err := json.Unmarshal(line, &e); err != nil || e.Key == "" {
			continue // torn or unreadable record; the turn is re-imported
		}
		records++
		entries[e.Key] = e
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, fmt.Errorf("read import ledger: %w", err)
	}
	return entries, records, nil
}

// maxLedgerLineBytes bounds one record. Records are fixed-shape and content
// free, so anything near this is corruption rather than a large turn.
const maxLedgerLineBytes = 64 * 1024

// compact rewrites the file with one record per key through a temporary file
// and a rename, so a crash mid-compaction leaves the previous ledger intact.
// Callers must not hold l.mu; OpenLedger runs it before the append handle
// exists.
func (l *Ledger) compact() error {
	tmp := l.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("compact ledger: %w", err)
	}
	w := bufio.NewWriter(f)
	for key, e := range l.entries {
		e.Key = key
		data, err := json.Marshal(e)
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("compact ledger: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
			return fmt.Errorf("compact ledger: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("compact ledger: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compact ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compact ledger: %w", err)
	}
	return nil
}

// Close releases the append handle. It is safe to call more than once.
func (l *Ledger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Status returns the recorded entry for a source turn.
func (l *Ledger) Status(id SourceIdentity) (Entry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[id]
	return e, ok
}

// Len returns the number of distinct turns the ledger knows about.
func (l *Ledger) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// ShouldImport reports whether a turn needs export. An exported turn is
// skipped unless force is set; anything else (unseen, pending, failed,
// skipped) is imported. This is the resume rule that makes a cancelled or
// failed run pick up where it stopped.
func (l *Ledger) ShouldImport(id SourceIdentity, force bool) bool {
	if force {
		return true
	}
	e, ok := l.Status(id)
	if !ok {
		return true
	}
	return e.Status != StatusExported
}

// Mark records a turn's status by appending one record. genID and errorClass
// are optional; a caller must never pass source content, because the ledger is
// a privacy boundary. nowUnix is the wall clock, passed in so the clock stays
// at the call site and tests stay deterministic.
//
// The in-memory status changes only after the append succeeds, so a failed
// write leaves the turn looking un-exported and a retry re-exports it rather
// than skipping a success that never reached disk.
func (l *Ledger) Mark(id SourceIdentity, status EntryStatus, genID, errorClass string, nowUnix int64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	e := l.entries[id]
	e.Key = id
	if e.FirstSeenUnix == 0 {
		e.FirstSeenUnix = nowUnix
	}
	e.Status = status
	e.Attempts++
	e.UpdatedUnix = nowUnix
	if genID != "" {
		e.GenerationID = genID
	}
	// The error class is sticky on failure only; a later success clears it.
	if status == StatusFailed {
		e.ErrorClass = errorClass
	} else {
		e.ErrorClass = ""
	}

	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("marshal ledger record: %w", err)
	}
	if l.file == nil {
		return errors.New("history: ledger is closed")
	}
	if _, err := l.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("append ledger record: %w", err)
	}
	l.entries[id] = e
	return nil
}
