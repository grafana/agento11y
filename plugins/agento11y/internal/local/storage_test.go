package local

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func TestStorage_AppendsJSONL(t *testing.T) {
	s := newStorage(t)
	type rec struct {
		Name string `json:"name"`
		N    int    `json:"n"`
	}
	for i, name := range []string{"alpha", "beta", "gamma"} {
		if err := s.Append("test.jsonl", rec{Name: name, N: i}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	lines := readLines(t, filepath.Join(s.Dir(), "test.jsonl"))
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3", len(lines))
	}
	for i, line := range lines {
		var got rec
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("decode line %d: %v", i, err)
		}
		if got.N != i {
			t.Errorf("line %d N = %d, want %d", i, got.N, i)
		}
	}
}

func TestStorage_FilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix-only permission check")
	}
	s := newStorage(t)
	if err := s.Append("file.jsonl", map[string]string{"x": "y"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	info, err := os.Stat(filepath.Join(s.Dir(), "file.jsonl"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("file mode = %v, want 0600", mode)
	}
	dirInfo, err := os.Stat(s.Dir())
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if mode := dirInfo.Mode().Perm(); mode != 0o700 {
		t.Errorf("dir mode = %v, want 0700", mode)
	}
}

// TestAppendGeneration covers generation storage: populated
// conversation IDs land in conversations/<id>.jsonl, and missing or
// path-shaped IDs are rejected.
func TestAppendGeneration(t *testing.T) {
	cases := []struct {
		name     string
		convID   string
		wantPath string
		wantErr  bool
	}{
		{name: "populated id writes per-conversation file", convID: "conv-A", wantPath: "conv-A.jsonl"},
		{name: "empty id rejected", convID: "", wantErr: true},
		{name: "path id rejected", convID: "../runs", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStorage(t)
			rec := generationRecord{
				ConversationID: tc.convID,
				GenerationID:   "gen-1",
				Generation:     json.RawMessage(`{"id":"gen-1"}`),
			}
			err := s.AppendGeneration(rec)
			if tc.wantErr {
				if err == nil {
					t.Fatal("AppendGeneration returned nil, want error")
				}
				assertConversationDirEmpty(t, s)
				return
			}
			if err != nil {
				t.Fatalf("AppendGeneration: %v", err)
			}
			if _, err := os.Stat(filepath.Join(s.Dir(), ConversationsDir, tc.wantPath)); err != nil {
				t.Fatalf("expected file %s: %v", tc.wantPath, err)
			}
		})
	}
}

func assertConversationDirEmpty(t *testing.T, s *Storage) {
	t.Helper()
	convDir := filepath.Join(s.Dir(), ConversationsDir)
	entries, err := os.ReadDir(convDir)
	if err != nil {
		t.Fatalf("read %s: %v", convDir, err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s not empty: %v", convDir, entries)
	}
}

func TestStorage_ConcurrentAppends(t *testing.T) {
	s := newStorage(t)
	const writers = 8
	const each = 50

	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for i := range each {
				if err := s.Append("concurrent.jsonl", map[string]int{"w": id, "i": i}); err != nil {
					t.Errorf("worker %d append: %v", id, err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	lines := readLines(t, filepath.Join(s.Dir(), "concurrent.jsonl"))
	if got, want := len(lines), writers*each; got != want {
		t.Fatalf("lines = %d, want %d", got, want)
	}
	// Every line must be valid JSON — interleaved writes would corrupt
	// at least one of them.
	for i, line := range lines {
		var m map[string]int
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line %d not valid JSON: %v (%q)", i, err, line)
		}
	}
}

func newStorage(t *testing.T) *Storage {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStorage(filepath.Join(dir, "local"))
	if err != nil {
		t.Fatalf("NewStorage: %v", err)
	}
	return s
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		out = append(out, sc.Text())
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	return out
}
