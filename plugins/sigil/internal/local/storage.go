// Package local implements the in-process HTTP receiver used by
// `sigil pi --local` and `sigil claude --local`. It stores generation
// exports to JSONL files under the Sigil state root so agent sessions can
// be inspected with standard shell tools, without requiring a Sigil Cloud
// or local stack deployment.
package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/xdg"
)

// aggregateCacheTTL bounds how long a cached per-file aggregate is trusted
// without re-checking the file. mtime+size is the primary validator —
// conversation files are append-only, so any new generation grows the
// file and invalidates the entry. The TTL is only a defensive backstop
// for a same-size+same-mtime overwrite (which append-only capture won't
// normally produce), and is set well above the viewer's 30s poll so idle
// conversations stay cached across polls.
const aggregateCacheTTL = 5 * time.Minute

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
	return filepath.Join(xdg.StateRoot("sigil"), "local")
}

// Storage owns the JSONL files under StateDir and serialises writes so
// concurrent handlers can append safely.
type Storage struct {
	dir string

	// One mutex per file path. We don't expect contention high enough to
	// need finer locking; this just stops interleaved JSON lines.
	mu    sync.Mutex
	locks map[string]*sync.Mutex

	// now is the clock used for aggregate-cache TTL checks; a seam so
	// tests can advance time without sleeping.
	now func() time.Time

	// agg caches per-file conversation summaries and token points so the
	// list/metrics endpoints don't re-parse unchanged files on every poll.
	// Entries are keyed by path and never evicted: local capture has no
	// deletion or rotation path, so a stale key can't accumulate in
	// practice, and a dead entry never surfaces (the endpoints only read
	// keys for files present in the current ReadDir).
	aggMu sync.Mutex
	agg   map[string]fileAggregate
}

// fileAggregate is one conversation file's cached summary and token
// points, plus the mtime+size+cachedAt used to decide whether the cache
// entry is still valid (see fileAggregateFor). hasSummary is false when
// the file had no decodable records, so empty files are not re-scanned
// every poll.
type fileAggregate struct {
	summary    ConversationSummary
	hasSummary bool
	points     []TokenUsagePoint
	mtime      time.Time
	size       int64
	cachedAt   time.Time
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
	return &Storage{
		dir:   dir,
		locks: map[string]*sync.Mutex{},
		now:   time.Now,
		agg:   map[string]fileAggregate{},
	}, nil
}

// fileAggregateFor returns the cached aggregate for path when the file's
// mtime and size still match the cached entry and the entry is younger
// than aggregateCacheTTL; otherwise it re-scans the file once (computing
// the summary and token points together), stores the result, and returns
// it. The scan runs outside the lock, so two concurrent cold callers may
// scan the same file once each — idempotent, last write wins.
//
// info is the caller's current view of the file (from ReadDir) and is used
// only for the cache-hit check. The stored mtime/size come from
// scanFileAggregate, which stats the fd it reads, so the cache key always
// describes the exact bytes scanned even if the file grew between the
// caller's stat and the scan.
func (s *Storage) fileAggregateFor(path string, info os.FileInfo) (fileAggregate, error) {
	s.aggMu.Lock()
	if e, ok := s.agg[path]; ok &&
		e.mtime.Equal(info.ModTime()) &&
		e.size == info.Size() &&
		s.now().Sub(e.cachedAt) < aggregateCacheTTL {
		s.aggMu.Unlock()
		return e, nil
	}
	s.aggMu.Unlock()

	agg, err := scanFileAggregate(path)
	if err != nil {
		return fileAggregate{}, err
	}
	agg.cachedAt = s.now()

	s.aggMu.Lock()
	s.agg[path] = agg
	s.aggMu.Unlock()
	return agg, nil
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
