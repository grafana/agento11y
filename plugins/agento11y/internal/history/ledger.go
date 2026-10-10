package history

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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

// Ledger is the private, idempotent import record for one agent's imports into
// one destination. It lives under the application state root with 0700
// directories and 0600 files, and is safe for concurrent use.
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

// ledgerPath is the ledger for one agent's imports into one destination, under
// a directory per agent.
func ledgerPath(agent AgentID, destination string) string {
	// The registered agent IDs are already filename-safe; SafeComponent keeps
	// that true if the set ever grows.
	return filepath.Join(ledgerDir(), xdg.SafeComponent(string(agent)), xdg.SafeComponent(destination)+".jsonl")
}

// legacyLedgerPath is the one ledger an agent had before destinations, beside
// the per-agent directories. It did not record where its turns went.
func legacyLedgerPath(agent AgentID) string {
	return filepath.Join(ledgerDir(), xdg.SafeComponent(string(agent))+".jsonl")
}

// localDestination is the local daemon's store, whatever port it listens on:
// the port can change between runs, and the store behind it does not.
const localDestination = "local"

// destination names the store a target writes to, for its ledger. A loopback
// endpoint is local, because the exporter treats it as the local daemon. Any
// other is Grafana Cloud, one destination per endpoint and tenant, hashed so a
// ledger's file name holds neither.
func (t Target) destination() string {
	endpoint := t.endpoint()
	if envconfig.IsLocalEndpoint(endpoint) {
		return localDestination
	}
	tenant := strings.TrimSpace(envconfig.Getenv("AUTH_TENANT_ID"))
	sum := sha256.Sum256([]byte(normalizedEndpoint(endpoint) + "\n" + tenant))
	return "cloud-" + hex.EncodeToString(sum[:8])
}

// normalizedEndpoint spells one endpoint one way, so a respelling does not
// start a new ledger and send every turn again. Like the SDK, it reads an
// endpoint without a scheme as https, or as http when AGENTO11Y_INSECURE is
// set. It lowercases the scheme and host, and drops credentials, a trailing
// dot from the host, the scheme's default port, and any trailing slash.
func normalizedEndpoint(endpoint string) string {
	lower := strings.ToLower(endpoint)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		scheme := "https://"
		if envconfig.ParseBool(envconfig.Getenv("INSECURE")) {
			scheme = "http://"
		}
		endpoint = scheme + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return endpoint
	}
	u.Scheme = strings.ToLower(u.Scheme)
	// Credentials in the URL name who connects, not where to.
	u.User = nil
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String()
}

// OpenLedger loads (or creates) the ledger for an agent's imports into one
// destination. The caller must call [Ledger.Close].
//
// A record that cannot be decoded is skipped: a torn final line after a crash
// costs one turn's status, and that turn is imported again under the same
// generation ID. A read or permission error is returned rather than swallowed,
// because degrading to an empty ledger would silently re-export the whole
// history.
func OpenLedger(agent AgentID, destination string) (*Ledger, error) {
	if strings.TrimSpace(string(agent)) == "" {
		return nil, errors.New("history: open ledger for empty agent")
	}
	if strings.TrimSpace(destination) == "" {
		return nil, errors.New("history: open ledger for empty destination")
	}
	path := ledgerPath(agent, destination)
	if err := seedLedger(path, legacyLedgerPath(agent)); err != nil {
		return nil, err
	}
	return openLedgerAt(path)
}

// seedLedger starts a destination's ledger from the agent's ledger from before
// destinations, the first time it is opened. That ledger does not say where
// its turns went, so the destination takes all of them, because sending them
// again is not harmless anywhere:
//   - Grafana Cloud only recognises a generation ID it already holds for about
//     a day. Past that, a repeat is stored, counted, and billed a second time.
//   - The local store keeps one turn per generation ID within a conversation,
//     but a session that a second file has claimed since its import now goes
//     out under another conversation ID, so its turns would show twice.
//
// A turn the old ledger holds that this destination never received needs
// --force there once.
func seedLedger(path, legacy string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat import ledger: %w", err)
	}
	entries, _, err := readLedger(legacy)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create ledger dir: %w", err)
	}
	tmp, err := writeLedgerTemp(path, entries)
	if err != nil {
		return fmt.Errorf("seed import ledger: %w", err)
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := publishSeed(tmp, path); err != nil {
		return fmt.Errorf("seed import ledger: %w", err)
	}
	return nil
}

// linkFile is os.Link, replaceable so a test can stand in for a file system
// without hard links.
var linkFile = os.Link

// publishSeed puts a fully written seed at path only if no ledger is there
// yet. Two imports opening a destination for the first time at once both
// seed it, and the one that loses must keep the winner's ledger, which may
// already hold turns it exported.
//
// A hard link does that in one step. On a file system without hard links, the
// ledger is created exclusively and the seed copied in, which also never
// replaces another ledger.
func publishSeed(tmp, path string) error {
	err := linkFile(tmp, path)
	if err == nil || errors.Is(err, os.ErrExist) {
		return nil
	}
	return copyExclusive(tmp, path)
}

// copyExclusive copies src to dst, creating dst only if it does not exist. An
// existing dst is left as it is.
func copyExclusive(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
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
	tmp, err := writeLedgerTemp(l.path, l.entries)
	if err != nil {
		return fmt.Errorf("compact ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("compact ledger: %w", err)
	}
	return nil
}

// writeLedgerTemp writes one record per key to a new temporary file beside
// path and returns its name. Each caller gets a file of its own, so two
// processes writing at once never truncate each other's, and the data is on
// disk before the caller puts the file in place.
func writeLedgerTemp(path string, entries map[SourceIdentity]Entry) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	w := bufio.NewWriter(f)
	for key, e := range entries {
		e.Key = key
		data, err := json.Marshal(e)
		if err != nil {
			return fail(err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return fail(err)
		}
	}
	if err := w.Flush(); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return tmp, nil
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
