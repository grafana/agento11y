package history

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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
	if _, err := OpenLedger("", localDestination); err == nil {
		t.Fatal("OpenLedger with no agent returned nil error")
	}
	if _, err := OpenLedger(AgentClaudeCode, " "); err == nil {
		t.Fatal("OpenLedger with no destination returned nil error")
	}
}

func TestTargetDestination(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	cloud := func(endpoint, tenant string) string {
		t.Setenv("AGENTO11Y_AUTH_TENANT_ID", tenant)
		return Target{Endpoint: endpoint}.destination()
	}

	// The daemon's port moves between runs, and its store does not.
	if a, b := cloud("http://127.0.0.1:8765", ""), cloud("http://localhost:8768/", ""); a != localDestination || b != localDestination {
		t.Errorf("loopback destinations = %q, %q, want both %q", a, b, localDestination)
	}
	stack := cloud("https://agento11y-prod.grafana.net", "123")
	if stack == localDestination || !strings.HasPrefix(stack, "cloud-") {
		t.Fatalf("Cloud destination = %q", stack)
	}
	// One endpoint spelled another way is one destination: a new ledger would
	// send every turn again.
	for _, spelling := range []string{
		"https://Agento11y-Prod.grafana.net/",
		"HTTPS://agento11y-prod.grafana.net//",
		"https://agento11y-prod.grafana.net:443",
		"agento11y-prod.grafana.net",
		"Agento11y-Prod.grafana.net/",
		"https://agento11y-prod.grafana.net.",
		"https://agento11y-prod.grafana.net.:443/",
		"https://user:secret@agento11y-prod.grafana.net",
	} {
		if same := cloud(spelling, "123"); same != stack {
			t.Errorf("%q gave %q, want %q", spelling, same, stack)
		}
	}
	if other := cloud("https://agento11y-prod.grafana.net", "456"); other == stack {
		t.Error("two tenants on one endpoint share a destination")
	}
	if a, b := cloud("https://[2001:db8::1]:443/", "1"), cloud("https://[2001:db8::1]", "1"); a != b {
		t.Errorf("one IPv6 endpoint gave %q and %q", a, b)
	}

	// The SDK sends an endpoint without a scheme over http when
	// AGENTO11Y_INSECURE is set, and over https otherwise.
	selfHosted := cloud("http://sigil.internal:8080", "1")
	t.Setenv("AGENTO11Y_INSECURE", "true")
	if got := cloud("sigil.internal:8080", "1"); got != selfHosted {
		t.Errorf("insecure endpoint without a scheme gave %q, want %q", got, selfHosted)
	}
	t.Setenv("AGENTO11Y_INSECURE", "")
	if got := cloud("sigil.internal:8080", "1"); got == selfHosted {
		t.Error("an endpoint without a scheme was read as http without AGENTO11Y_INSECURE")
	}
	if strings.Contains(stack, "grafana") || strings.Contains(stack, "123") {
		t.Errorf("destination %q names its endpoint or tenant", stack)
	}

	// An empty target endpoint resolves as the exporter resolves it.
	t.Setenv("AGENTO11Y_ENDPOINT", "https://agento11y-prod.grafana.net")
	if got := cloud("", "123"); got != stack {
		t.Errorf("configured endpoint gave %q, want %q", got, stack)
	}
	t.Setenv("AGENTO11Y_ENDPOINT", "http://127.0.0.1:8765")
	if got := cloud("", "123"); got != localDestination {
		t.Errorf("configured loopback endpoint gave %q, want %q", got, localDestination)
	}
}

// writeLegacyLedger writes the ledger an agent had before destinations, with n
// exported turns.
func writeLegacyLedger(t *testing.T, n int) {
	t.Helper()
	l, err := openLedgerAt(legacyLedgerPath(AgentClaudeCode))
	if err != nil {
		t.Fatalf("open old ledger: %v", err)
	}
	for i := range n {
		if err := l.Mark(identity(i), StatusExported, "", "", 1); err != nil {
			t.Fatalf("mark: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close old ledger: %v", err)
	}
}

func TestOpenLedgerSeedsEveryDestination(t *testing.T) {
	pinStateHome(t)
	writeLegacyLedger(t, 2)
	for _, destination := range []string{"cloud-0123456789abcdef", localDestination} {
		l, err := OpenLedger(AgentClaudeCode, destination)
		if err != nil {
			t.Fatalf("open %s: %v", destination, err)
		}
		if l.Len() != 2 {
			t.Errorf("%s ledger holds %d turns, want 2", destination, l.Len())
		}
		_ = l.Close()
	}
}

// A seed must never replace a ledger another import has already put in place:
// that ledger may hold turns it exported since, and losing them would send
// them again. This holds with a hard link and with the copy a file system
// without hard links falls back to.
func TestPublishSeedKeepsALedgerThatIsAlreadyThere(t *testing.T) {
	for _, tt := range []struct {
		name string
		link func(oldname, newname string) error
	}{
		{name: "hard link", link: os.Link},
		{name: "no hard links", link: func(string, string) error { return errors.New("operation not supported") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			prev := linkFile
			linkFile = tt.link
			t.Cleanup(func() { linkFile = prev })
			path := filepath.Join(t.TempDir(), "cloud.jsonl")
			seed, err := writeLedgerTemp(path, map[SourceIdentity]Entry{identity(0): {Status: StatusExported}})
			if err != nil {
				t.Fatal(err)
			}

			// No ledger yet: the seed goes in place.
			if err := publishSeed(seed, path); err != nil {
				t.Fatalf("publish into an empty place: %v", err)
			}
			if got := countLines(t, path); got != 1 {
				t.Fatalf("published ledger holds %d records, want 1", got)
			}

			// A ledger with marks of its own is already there: it stays.
			l, err := openLedgerAt(path)
			if err != nil {
				t.Fatal(err)
			}
			for i := 1; i <= 3; i++ {
				if err := l.Mark(identity(i), StatusExported, "", "", 1); err != nil {
					t.Fatal(err)
				}
			}
			_ = l.Close()
			late, err := writeLedgerTemp(path, map[SourceIdentity]Entry{identity(9): {Status: StatusExported}})
			if err != nil {
				t.Fatal(err)
			}
			if err := publishSeed(late, path); err != nil {
				t.Fatalf("publish over an existing ledger: %v", err)
			}
			entries, _, err := readLedger(path)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 4 {
				t.Errorf("ledger holds %d turns after a late seed, want the 4 already there", len(entries))
			}
			if _, ok := entries[identity(9)]; ok {
				t.Error("the late seed replaced the ledger that was already there")
			}
		})
	}
}

// Regression: two imports opening a Cloud destination for the first time at
// once both seeded it through one temporary file, and the ledger they left lost
// some of the old turns, which the next import sent again.
func TestOpenLedgerSeedsOnceUnderConcurrentFirstOpens(t *testing.T) {
	pinStateHome(t)
	const turns, openers = 5000, 8
	writeLegacyLedger(t, turns)
	const destination = "cloud-0123456789abcdef"

	start := make(chan struct{})
	errs := make(chan error, openers)
	lens := make(chan int, openers)
	for range openers {
		go func() {
			<-start
			l, err := OpenLedger(AgentClaudeCode, destination)
			if err != nil {
				errs <- err
				return
			}
			lens <- l.Len()
			errs <- l.Close()
		}()
	}
	close(start)
	for range openers {
		if err := <-errs; err != nil {
			t.Errorf("open: %v", err)
		}
	}
	close(lens)
	for n := range lens {
		if n != turns {
			t.Errorf("an opener saw %d turns, want %d", n, turns)
		}
	}
	if got := countLines(t, ledgerPath(AgentClaudeCode, destination)); got != turns {
		t.Errorf("ledger file holds %d records, want %d", got, turns)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(ledgerPath(AgentClaudeCode, destination)), "*.tmp"))
	if len(leftovers) != 0 {
		t.Errorf("temporary files left behind: %v", leftovers)
	}
}

// An old ledger that cannot be read must stop the import: treating it as empty
// would send every turn to Grafana Cloud again.
func TestOpenLedgerFailsOnAnUnreadableOldLedger(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file modes do not deny reads here")
	}
	pinStateHome(t)
	writeLegacyLedger(t, 1)
	if err := os.Chmod(legacyLedgerPath(AgentClaudeCode), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(AgentClaudeCode, "cloud-0123456789abcdef"); err == nil {
		t.Fatal("OpenLedger returned nil error for an unreadable old ledger")
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

	l, err := OpenLedger(AgentClaudeCode, localDestination)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer func() { _ = l.Close() }()

	if path := ledgerPath(AgentClaudeCode, localDestination); !strings.HasPrefix(path, legacy+string(filepath.Separator)) {
		t.Fatalf("ledger path %q is not under the legacy state root %q", path, legacy)
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
