package fragment

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
)

// withTempState points the package's state root at a fresh tempdir for one test.
func withTempState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	return dir
}

func newTestLogger() *log.Logger {
	return log.New(&bytes.Buffer{}, "", 0)
}

func TestUpdate_AppliesMutation(t *testing.T) {
	withTempState(t)
	logger := newTestLogger()

	err := Update("conv", "gen1", logger, func(f *Fragment) bool {
		f.UserPrompt = "hello"
		Touch(f, "2026-04-28T00:00:00Z")
		return true
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := LoadTolerant("conv", "gen1", logger)
	if got == nil {
		t.Fatal("expected fragment, got nil")
	}
	if got.UserPrompt != "hello" {
		t.Errorf("UserPrompt = %q; want %q", got.UserPrompt, "hello")
	}
	if got.StartedAt != "2026-04-28T00:00:00Z" {
		t.Errorf("StartedAt = %q; want %q", got.StartedAt, "2026-04-28T00:00:00Z")
	}
}

func TestSave_CreatesPrivateConversationDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permissions only")
	}
	withTempState(t)
	logger := newTestLogger()

	if err := Update("conv", "gen1", logger, func(f *Fragment) bool { return true }); err != nil {
		t.Fatalf("Update: %v", err)
	}

	dirInfo, err := os.Stat(ConversationDir("conv"))
	if err != nil {
		t.Fatalf("stat conversation dir: %v", err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("conversation dir mode = %o; want 700", got)
	}

	fileInfo, err := os.Stat(FragmentFilePath("conv", "gen1"))
	if err != nil {
		t.Fatalf("stat fragment: %v", err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("fragment file mode = %o; want 600", got)
	}
}

func TestUpdate_MutatorReturnsFalseSkipsSave(t *testing.T) {
	withTempState(t)
	logger := newTestLogger()

	// First write so the file exists.
	err := Update("conv", "gen1", logger, func(f *Fragment) bool {
		f.ThinkingPresent = true
		return true
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	path := FragmentFilePath("conv", "gen1")
	stat1, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	// Sleep a tick so a rewrite would change mtime detectably.
	if err := os.Chtimes(path, stat1.ModTime().Add(-1), stat1.ModTime().Add(-1)); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	stat1, _ = os.Stat(path)

	// Mutator returns false — should not rewrite.
	err = Update("conv", "gen1", logger, func(f *Fragment) bool {
		// thinkingPresent already true — caller would skip save.
		if f.ThinkingPresent {
			return false
		}
		f.ThinkingPresent = true
		return true
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	stat2, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !stat2.ModTime().Equal(stat1.ModTime()) {
		t.Errorf("file was rewritten: mtime %v -> %v", stat1.ModTime(), stat2.ModTime())
	}
}

func TestLoadTolerant(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T)
		want  bool // true means non-nil result expected
	}{
		{
			name: "corrupt fragment treated as missing",
			setup: func(t *testing.T) {
				path := FragmentFilePath("conv", "gen1")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, []byte("{not valid json"), 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
			},
		},
		{
			name:  "missing fragment returns nil",
			setup: func(t *testing.T) {},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempState(t)
			tc.setup(t)
			got := LoadTolerant("conv", "gen1", newTestLogger())
			if (got != nil) != tc.want {
				t.Errorf("got %+v; want non-nil=%v", got, tc.want)
			}
		})
	}
}

func TestUpdate_ReassertsIDs(t *testing.T) {
	withTempState(t)
	logger := newTestLogger()

	// Mutator (somehow) overwrites the IDs to point at a different generation.
	// The Update wrapper should defensively reassert the locked IDs so the save
	// still goes to the original key.
	err := Update("conv", "gen1", logger, func(f *Fragment) bool {
		f.ConversationID = "other-conv"
		f.GenerationID = "other-gen"
		f.UserPrompt = "x"
		return true
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	got := LoadTolerant("conv", "gen1", logger)
	if got == nil {
		t.Fatal("expected fragment at original key, got nil")
	}
	if got.UserPrompt != "x" {
		t.Errorf("UserPrompt = %q; want %q", got.UserPrompt, "x")
	}

	// And nothing should land at the would-be-target keys.
	if other := LoadTolerant("other-conv", "other-gen", logger); other != nil {
		t.Errorf("save should not have escaped the lock; got %+v", other)
	}
}

func TestListFragmentIDs(t *testing.T) {
	cases := []struct {
		name  string
		conv  string
		setup func(t *testing.T, logger *log.Logger)
		want  []string
	}{
		{
			name: "lists fragments and ignores non-fragment files",
			conv: "conv",
			setup: func(t *testing.T, logger *log.Logger) {
				for _, gid := range []string{"gen1", "gen2", "gen3"} {
					if err := Update("conv", gid, logger, func(f *Fragment) bool { return true }); err != nil {
						t.Fatalf("seed %s: %v", gid, err)
					}
				}
				// Drop a non-fragment file; must not show up.
				noisePath := filepath.Join(ConversationDir("conv"), "session.json")
				if err := os.WriteFile(noisePath, []byte("{}"), 0o600); err != nil {
					t.Fatalf("write noise: %v", err)
				}
			},
			want: []string{"gen1", "gen2", "gen3"},
		},
		{
			name:  "missing dir returns empty",
			conv:  "nonexistent",
			setup: func(t *testing.T, _ *log.Logger) {},
			want:  nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withTempState(t)
			logger := newTestLogger()
			tc.setup(t, logger)
			got := ListFragmentIDs(tc.conv, logger)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d ids; want %d (got=%v want=%v)", len(got), len(tc.want), got, tc.want)
			}
			sort.Strings(got)
			want := append([]string(nil), tc.want...)
			sort.Strings(want)
			for i := range got {
				if got[i] != want[i] {
					t.Errorf("id[%d] = %q; want %q", i, got[i], want[i])
				}
			}
			// Round-trip: every returned ID must load its fragment back.
			for _, id := range got {
				if f := LoadTolerant(tc.conv, id, logger); f == nil {
					t.Errorf("LoadTolerant(%q, %q) returned nil; ListFragmentIDs must return loadable ids", tc.conv, id)
				}
			}
		})
	}
}

func TestSaveSessionAndLoadSession(t *testing.T) {
	withTempState(t)
	logger := newTestLogger()

	err := SaveSession(Session{
		ConversationID: "conv",
		WorkspaceRoots: []string{"/ws"},
		UserEmail:      "alice@example.com",
		CursorVersion:  "1.0",
	})
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}

	got := LoadSession("conv", logger)
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.UserEmail != "alice@example.com" {
		t.Errorf("UserEmail = %q; want %q", got.UserEmail, "alice@example.com")
	}
	if len(got.WorkspaceRoots) != 1 || got.WorkspaceRoots[0] != "/ws" {
		t.Errorf("WorkspaceRoots = %v; want [/ws]", got.WorkspaceRoots)
	}
}

func TestDelete_Idempotent(t *testing.T) {
	withTempState(t)
	logger := newTestLogger()

	if err := Update("conv", "gen1", logger, func(f *Fragment) bool { return true }); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Delete("conv", "gen1"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	// Deleting again must succeed — we want idempotent cleanup so concurrent
	// stop/sessionEnd retries don't error on each other.
	if err := Delete("conv", "gen1"); err != nil {
		t.Errorf("second delete should be idempotent; got %v", err)
	}
}
