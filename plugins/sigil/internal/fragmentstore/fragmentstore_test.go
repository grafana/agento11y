package fragmentstore

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

type rec struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func TestWriteJSONRoundTripsAndCleansTempFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	target := filepath.Join(dir, "frag.json")

	if err := WriteJSON(target, rec{Name: "a", Count: 1}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}

	got, corrupt, err := ReadJSON[rec](target)
	if err != nil || corrupt || got == nil {
		t.Fatalf("ReadJSON after write: got=%v corrupt=%v err=%v", got, corrupt, err)
	}
	if got.Name != "a" || got.Count != 1 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
}

func TestWriteJSONPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	dir := filepath.Join(t.TempDir(), "nested", "dir")
	target := filepath.Join(dir, "frag.json")
	if err := WriteJSON(target, rec{Name: "a", Count: 1}); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o; want 700", perm)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o; want 600", perm)
	}
}

func TestReadJSONOutcomes(t *testing.T) {
	validJSON, err := json.Marshal(rec{Name: "ok", Count: 2})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	cases := []struct {
		name        string
		fileName    string
		writeBytes  []byte // nil = skip write (missing-file case)
		wantCorrupt bool
		wantErr     bool
		wantGot     *rec
	}{
		{
			name:     "missing file",
			fileName: "nope.json",
		},
		{
			name:        "corrupt json",
			fileName:    "bad.json",
			writeBytes:  []byte("{not json"),
			wantCorrupt: true,
			wantErr:     true,
		},
		{
			name:       "valid json",
			fileName:   "ok.json",
			writeBytes: validJSON,
			wantGot:    &rec{Name: "ok", Count: 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), tc.fileName)
			if tc.writeBytes != nil {
				if err := os.WriteFile(p, tc.writeBytes, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, corrupt, err := ReadJSON[rec](p)
			if corrupt != tc.wantCorrupt {
				t.Errorf("corrupt = %v; want %v", corrupt, tc.wantCorrupt)
			}
			if (err != nil) != tc.wantErr {
				t.Errorf("err = %v; want err != nil = %v", err, tc.wantErr)
			}
			switch {
			case tc.wantGot == nil && got != nil:
				t.Errorf("got = %+v; want nil", got)
			case tc.wantGot != nil && got == nil:
				t.Errorf("got = nil; want %+v", tc.wantGot)
			case tc.wantGot != nil && *got != *tc.wantGot:
				t.Errorf("got = %+v; want %+v", *got, *tc.wantGot)
			}
		})
	}
}

func TestWithFileLockSerializesAccess(t *testing.T) {
	target := filepath.Join(t.TempDir(), "locked.json")

	var (
		mu      sync.Mutex
		inside  int
		maxSeen int
	)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = WithFileLock(target, func() error {
				mu.Lock()
				inside++
				if inside > maxSeen {
					maxSeen = inside
				}
				mu.Unlock()

				time.Sleep(2 * time.Millisecond)

				mu.Lock()
				inside--
				mu.Unlock()
				return nil
			})
		})
	}
	wg.Wait()

	if maxSeen != 1 {
		t.Fatalf("lock allowed %d concurrent holders; want 1", maxSeen)
	}
	// Lock file is released.
	if _, err := os.Stat(target + ".lock"); !os.IsNotExist(err) {
		t.Errorf("lock file not released: err=%v", err)
	}
}

func TestWithFileLockReclaimsStaleLock(t *testing.T) {
	target := filepath.Join(t.TempDir(), "stale.json")
	lockPath := target + ".lock"
	if err := os.WriteFile(lockPath, []byte("pid=999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate the lock past the stale threshold.
	old := time.Now().Add(-2 * DefaultStaleLockAge)
	if err := os.Chtimes(lockPath, old, old); err != nil {
		t.Fatal(err)
	}

	ran := false
	if err := WithFileLock(target, func() error { ran = true; return nil }); err != nil {
		t.Fatalf("WithFileLock: %v", err)
	}
	if !ran {
		t.Fatal("fn did not run; stale lock was not reclaimed")
	}
}

func TestCleanupStaleDirRemovesOldSkipsRecentAndSkipPath(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	cutoff := now.Add(-1 * time.Hour)

	write := func(name string, modAgo time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := now.Add(-modAgo)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
		return p
	}

	oldFile := write("old.json", 3*time.Hour)
	recentFile := write("recent.json", 1*time.Minute)
	skipFile := write("skip.json", 3*time.Hour)
	nonJSON := write("old.txt", 3*time.Hour)

	if err := CleanupStaleDir(dir, cutoff, nil, skipFile); err != nil {
		t.Fatalf("CleanupStaleDir: %v", err)
	}

	if _, err := os.Stat(oldFile); !os.IsNotExist(err) {
		t.Errorf("old .json should be removed")
	}
	if _, err := os.Stat(recentFile); err != nil {
		t.Errorf("recent .json should be kept: %v", err)
	}
	if _, err := os.Stat(skipFile); err != nil {
		t.Errorf("skip path should be kept: %v", err)
	}
	if _, err := os.Stat(nonJSON); err != nil {
		t.Errorf("non-json file should be kept: %v", err)
	}
}

func TestCleanupStaleDirMissingDirIsNoOp(t *testing.T) {
	if err := CleanupStaleDir(filepath.Join(t.TempDir(), "nope"), time.Now(), nil, ""); err != nil {
		t.Fatalf("CleanupStaleDir on missing dir = %v; want nil", err)
	}
}

func TestLogLoadErr(t *testing.T) {
	cases := []struct {
		name    string
		label   string
		corrupt bool
		want    string
	}{
		{name: "read with empty label", want: "fragment: read /p/frag.json: boom"},
		{name: "corrupt with empty label", corrupt: true, want: "fragment: corrupt /p/frag.json: boom"},
		{name: "read with label", label: "session ", want: "fragment: read session /p/frag.json: boom"},
		{name: "corrupt with label", label: "subagent link ", corrupt: true, want: "fragment: corrupt subagent link /p/frag.json: boom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := log.New(&buf, "", 0)
			LogLoadErr(logger, tc.label, "/p/frag.json", tc.corrupt, errBoom)
			if got := strings.TrimRight(buf.String(), "\n"); got != tc.want {
				t.Errorf("LogLoadErr = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestLogLoadErrNilLoggerIsNoOp(t *testing.T) {
	// Must not panic.
	LogLoadErr(nil, "", "/p/frag.json", true, errBoom)
}

var errBoom = errorString("boom")

type errorString string

func (e errorString) Error() string { return string(e) }
