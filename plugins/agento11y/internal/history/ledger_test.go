package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// pinStateHome points the application state root at a fresh directory so a
// test never reads or writes the developer's real ledger and prompt state.
func pinStateHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

func identity(i int) SourceIdentity {
	return SourceIdentity(fmt.Sprintf("%064x", i))
}

func openTestLedger(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := openLedgerAt(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

func TestLedgerMark(t *testing.T) {
	tests := []struct {
		name   string
		marks  []func(*Ledger) error
		key    SourceIdentity
		want   Entry
		absent bool
	}{
		{
			name: "first mark records first seen and attempt",
			marks: []func(*Ledger) error{
				func(l *Ledger) error { return l.Mark(identity(1), StatusPending, "gen-1", "", 100) },
			},
			key: identity(1),
			want: Entry{
				Key: identity(1), Status: StatusPending, Attempts: 1,
				GenerationID: "gen-1", FirstSeenUnix: 100, UpdatedUnix: 100,
			},
		},
		{
			name: "later mark keeps first seen and counts attempts",
			marks: []func(*Ledger) error{
				func(l *Ledger) error { return l.Mark(identity(2), StatusPending, "gen-2", "", 100) },
				func(l *Ledger) error { return l.Mark(identity(2), StatusExported, "gen-2", "", 200) },
			},
			key: identity(2),
			want: Entry{
				Key: identity(2), Status: StatusExported, Attempts: 2,
				GenerationID: "gen-2", FirstSeenUnix: 100, UpdatedUnix: 200,
			},
		},
		{
			name: "failure records the error class",
			marks: []func(*Ledger) error{
				func(l *Ledger) error {
					return l.Mark(identity(3), StatusFailed, "gen-3", "export_failed", 300)
				},
			},
			key: identity(3),
			want: Entry{
				Key: identity(3), Status: StatusFailed, Attempts: 1, GenerationID: "gen-3",
				ErrorClass: "export_failed", FirstSeenUnix: 300, UpdatedUnix: 300,
			},
		},
		{
			name: "a later success clears the error class",
			marks: []func(*Ledger) error{
				func(l *Ledger) error {
					return l.Mark(identity(4), StatusFailed, "gen-4", "export_failed", 300)
				},
				func(l *Ledger) error { return l.Mark(identity(4), StatusExported, "gen-4", "", 400) },
			},
			key: identity(4),
			want: Entry{
				Key: identity(4), Status: StatusExported, Attempts: 2, GenerationID: "gen-4",
				FirstSeenUnix: 300, UpdatedUnix: 400,
			},
		},
		{
			name:   "unmarked key has no entry",
			key:    identity(99),
			absent: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pinStateHome(t)
			l := openTestLedger(t, filepath.Join(t.TempDir(), "ledger.jsonl"))
			for _, mark := range tc.marks {
				if err := mark(l); err != nil {
					t.Fatalf("mark: %v", err)
				}
			}
			got, ok := l.Status(tc.key)
			if ok == tc.absent {
				t.Fatalf("Status(%s) present=%v, want present=%v", tc.key[:8], ok, !tc.absent)
			}
			if tc.absent {
				return
			}
			if got != tc.want {
				t.Fatalf("Status()\n got %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestLedgerShouldImport(t *testing.T) {
	tests := []struct {
		name   string
		status EntryStatus
		marked bool
		force  bool
		want   bool
	}{
		{name: "unseen turn is imported", want: true},
		{name: "exported turn is skipped", status: StatusExported, marked: true, want: false},
		{name: "exported turn with force is imported", status: StatusExported, marked: true, force: true, want: true},
		{name: "failed turn is retried", status: StatusFailed, marked: true, want: true},
		{name: "pending turn is retried", status: StatusPending, marked: true, want: true},
		{name: "skipped turn is reconsidered", status: StatusSkipped, marked: true, want: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pinStateHome(t)
			l := openTestLedger(t, filepath.Join(t.TempDir(), "ledger.jsonl"))
			key := identity(1)
			if tc.marked {
				if err := l.Mark(key, tc.status, "gen", "", 1); err != nil {
					t.Fatalf("mark: %v", err)
				}
			}
			if got := l.ShouldImport(key, tc.force); got != tc.want {
				t.Fatalf("ShouldImport() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestLedgerMarkAppendsOnly is the structural half of the linear-scaling
// requirement: a mark must add bytes at the end and leave every earlier byte
// alone. The cost half is TestLedgerMarkScalesLinearly.
func TestLedgerMarkAppendsOnly(t *testing.T) {
	pinStateHome(t)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := openTestLedger(t, path)

	var prefix []byte
	var lastSize int64
	for i := range 50 {
		if err := l.Mark(identity(i), StatusExported, "gen", "", int64(i)); err != nil {
			t.Fatalf("mark %d: %v", i, err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read ledger: %v", err)
		}
		if int64(len(data)) <= lastSize {
			t.Fatalf("mark %d did not grow the file: %d -> %d bytes", i, lastSize, len(data))
		}
		if !slices.Equal(data[:len(prefix)], prefix) {
			t.Fatalf("mark %d rewrote existing ledger bytes", i)
		}
		prefix = data
		lastSize = int64(len(data))
	}
}

// TestLedgerMarkScalesLinearly pins the requirement that the ledger performs
// constant work per mark. The previous implementation cloned the map and
// rewrote the whole file on every call, which took 109 seconds to reach 20,000
// entries and would have taken hours for the 277,625 turns on the development
// machine.
//
// The measure is bytes allocated, not wall clock. Wall clock made this test
// flaky: `go test ./...` runs packages in parallel, and load that arrives
// during the 20,000-mark run but not the 2,000-mark run inflates the ratio on
// an implementation that is linear. Allocation is unaffected by load, and it
// separates the two implementations just as widely: cloning the map and
// re-marshalling every record per mark measures at 100x here, an append at 9x.
func TestLedgerMarkScalesLinearly(t *testing.T) {
	pinStateHome(t)

	markN := func(n int) uint64 {
		path := filepath.Join(t.TempDir(), "ledger.jsonl")
		l, err := openLedgerAt(path)
		if err != nil {
			t.Fatalf("open ledger: %v", err)
		}
		defer func() { _ = l.Close() }()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		for i := range n {
			if err := l.Mark(identity(i), StatusExported, "gen", "", int64(i)); err != nil {
				t.Fatalf("mark %d: %v", i, err)
			}
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	// TotalAlloc counts the whole process, so another goroutine can only add to
	// a sample. The smallest one is the closest to what Mark itself allocated.
	minAllocs := func(n, runs int) uint64 {
		samples := make([]uint64, runs)
		for i := range runs {
			samples[i] = markN(n)
		}
		return slices.Min(samples)
	}

	small := minAllocs(2_000, 3)
	large := minAllocs(20_000, 3)

	// Ten times the work, so allow 15x for the fixed cost of opening a ledger.
	// Quadratic behaviour would put the ratio near 100x.
	const maxRatio = 15
	if large > maxRatio*small {
		t.Fatalf("20,000 marks allocated %d bytes, more than %dx the %d for 2,000 marks", large, maxRatio, small)
	}
}

func TestLedgerCompactsOnOpen(t *testing.T) {
	pinStateHome(t)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")

	l := openTestLedger(t, path)
	for round := range 4 {
		for i := range 10 {
			status := StatusPending
			if round == 3 {
				status = StatusExported
			}
			if err := l.Mark(identity(i), status, "gen", "", int64(round)); err != nil {
				t.Fatalf("mark: %v", err)
			}
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	before := countLines(t, path)
	if before != 40 {
		t.Fatalf("wrote %d records, want 40", before)
	}

	reopened := openTestLedger(t, path)
	if got := countLines(t, path); got != 10 {
		t.Fatalf("compacted ledger has %d records, want 10", got)
	}
	if got := reopened.Len(); got != 10 {
		t.Fatalf("in-memory ledger has %d keys, want 10", got)
	}
	for i := range 10 {
		e, ok := reopened.Status(identity(i))
		if !ok {
			t.Fatalf("key %d missing after compaction", i)
		}
		if e.Status != StatusExported {
			t.Fatalf("key %d status = %q, want %q", i, e.Status, StatusExported)
		}
		if e.Attempts != 4 {
			t.Fatalf("key %d attempts = %d, want 4", i, e.Attempts)
		}
	}

	// A second open finds one record per key and must not rewrite again.
	if err := reopened.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	openTestLedger(t, path)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if after.Size() != info.Size() {
		t.Fatalf("second open rewrote the ledger: %d -> %d bytes", info.Size(), after.Size())
	}
}

func TestLedgerFailedWriteKeepsInMemoryState(t *testing.T) {
	pinStateHome(t)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l := openTestLedger(t, path)

	key := identity(1)
	if err := l.Mark(key, StatusPending, "gen-1", "", 100); err != nil {
		t.Fatalf("mark: %v", err)
	}
	before, _ := l.Status(key)

	// Close the append handle behind the ledger's back so the next write
	// fails the way a full disk would.
	if err := l.file.Close(); err != nil {
		t.Fatalf("close file: %v", err)
	}

	if err := l.Mark(key, StatusExported, "gen-1", "", 200); err == nil {
		t.Fatal("Mark() returned nil after a failed write")
	}
	after, ok := l.Status(key)
	if !ok {
		t.Fatal("entry disappeared after a failed write")
	}
	if after != before {
		t.Fatalf("in-memory entry changed after a failed write:\n got %+v\nwant %+v", after, before)
	}
	if l.ShouldImport(key, false) != true {
		t.Fatal("turn should still need import after a failed exported mark")
	}
	l.file = nil // the cleanup Close must not close an already-closed handle
}

func TestLedgerSkipsUnreadableRecords(t *testing.T) {
	pinStateHome(t)
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	good, err := json.Marshal(Entry{Key: identity(1), Status: StatusExported, Attempts: 1})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	content := string(good) + "\n" + `{"key":` + "\n" // a torn final line
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	l := openTestLedger(t, path)
	if got := l.Len(); got != 1 {
		t.Fatalf("ledger has %d keys, want 1", got)
	}
	if l.ShouldImport(identity(1), false) {
		t.Fatal("exported turn should be skipped after reopen")
	}
}

func TestOpenLedgerRejectsEmptyAgent(t *testing.T) {
	pinStateHome(t)
	if _, err := OpenLedger(""); err == nil {
		t.Fatal("OpenLedger(\"\") returned nil error")
	}
}

func TestLedgerPathUsesApplicationStateRoot(t *testing.T) {
	state := pinStateHome(t)
	// Only the legacy root exists, so the ledger must be created there and the
	// preferred root must stay absent.
	legacy := filepath.Join(state, "sigil")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatalf("mkdir legacy: %v", err)
	}

	l, err := OpenLedger(AgentClaudeCode)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = l.Close() }()

	if !strings.HasPrefix(ledgerPath(AgentClaudeCode), legacy+string(filepath.Separator)) {
		t.Fatalf("ledger path %q is not under the legacy state root %q", ledgerPath(AgentClaudeCode), legacy)
	}
	if _, err := os.Stat(filepath.Join(state, "agento11y")); !os.IsNotExist(err) {
		t.Fatalf("opening the ledger created the preferred state root (stat err %v)", err)
	}
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	trimmed := strings.TrimRight(string(data), "\n")
	if trimmed == "" {
		return 0
	}
	return strings.Count(trimmed, "\n") + 1
}
