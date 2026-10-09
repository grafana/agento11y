package usagestats

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/uuid"
)

func TestInstallIDCreatesThenReuses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-id")

	first, persisted := installID(path)
	if !persisted {
		t.Fatalf("persisted = false for a writable path")
	}
	if _, err := uuid.Parse(first); err != nil {
		t.Fatalf("id %q is not a UUID: %v", first, err)
	}

	second, persisted := installID(path)
	if !persisted {
		t.Fatalf("persisted = false on reuse")
	}
	if second != first {
		t.Errorf("id changed across invocations: %q then %q; it must be stable for the installation", first, second)
	}
}

func TestInstallIDFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "install-id")
	if _, persisted := installID(path); !persisted {
		t.Fatalf("persisted = false")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode = %o, want 600; the id is the one correlator in the event", got)
	}
}

// TestInstallIDRejectsCorruptContent is the case worth having. A truncated or
// hand-edited file must not become a permanent correlator shared by every
// installation that suffered the same corruption — that would silently merge
// unrelated machines into one apparent installation, which is worse than a
// fresh id.
func TestInstallIDRejectsCorruptContent(t *testing.T) {
	for _, content := range []string{"", "   ", "not-a-uuid", "0", "6c1f0b6e-0d2a"} {
		t.Run("content="+content, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "install-id")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}

			id, _ := installID(path)
			if _, err := uuid.Parse(id); err != nil {
				t.Errorf("id %q is not a UUID; corrupt state must yield a fresh one", id)
			}
			if id == content {
				t.Errorf("corrupt content %q was adopted as the id", content)
			}
		})
	}
}

func TestInstallIDTrimsStoredWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-id")
	want := uuid.NewString()
	if err := os.WriteFile(path, []byte("  "+want+"\n\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, persisted := installID(path)
	if !persisted || got != want {
		t.Errorf("installID() = %q, %v; want %q, true", got, persisted, want)
	}
}

// TestInstallIDUnwritableDirReportsEphemeral covers the case the receiver has
// to be able to exclude: without persisted=false, a machine whose state
// directory cannot be written counts as a brand-new installation on every
// single invocation and swamps the installation count.
func TestInstallIDUnwritableDirReportsEphemeral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits this test relies on")
	}

	dir := t.TempDir()
	locked := filepath.Join(dir, "locked")
	if err := os.Mkdir(locked, 0o500); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })

	id, persisted := installID(filepath.Join(locked, "sub", "install-id"))
	if persisted {
		t.Errorf("persisted = true for an unwritable directory")
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("id %q is not a UUID; an ephemeral id is still reported so the event is not lost", id)
	}
}

// TestInstallIDWithoutDurableStateIsEphemeral is agento11y-specific and has no
// gcx counterpart: xdg.StateRoot falls back to the OS temp directory rather
// than failing, so without the durability check an id would be written to
// TMPDIR, reset on reboot, and still be reported as persisted.
func TestInstallIDWithoutDurableStateIsEphemeral(t *testing.T) {
	id, persisted := installID("")
	if persisted {
		t.Errorf("persisted = true with no durable state directory")
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Errorf("id %q is not a UUID", id)
	}

	other, _ := installID("")
	if other == id {
		t.Errorf("two ephemeral ids were identical (%q); they must not correlate across invocations", id)
	}
}

// TestInstallIDPathIsEmptyWithoutDurableState ties InstallIDPath to the
// durability verdict, which is what makes the case above reachable in
// production rather than only in tests.
func TestInstallIDPathIsEmptyWithoutDurableState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", "")
	}

	if got := InstallIDPath(); got != "" {
		t.Errorf("InstallIDPath() = %q with no resolvable home; want \"\" so the id is not written under TMPDIR", got)
	}
}
