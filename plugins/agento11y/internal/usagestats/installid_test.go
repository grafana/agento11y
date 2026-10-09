package usagestats

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
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

// TestInstallIDRejectsCorruptContent: a truncated file must not be adopted,
// or every install with the same corruption shares one id.
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

// TestInstallIDUnwritableDirReportsEphemeral: an unwritable state directory
// yields a different id every run, so it must not report as persisted.
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

// TestInstallIDWithoutDurableStateIsEphemeral: StateRoot falls back to TMPDIR
// rather than failing, so without the durability check an id would reset on
// reboot and still report as persisted.
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

// TestInstallIDPathIsEmptyWithoutDurableState makes the case above reachable
// in production, not just in tests.
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

// TestInstallIDConcurrentCreationAgreesOnOneID is the regression test for the
// read-then-write race.
//
// installID used to read, find nothing, generate, and write. Concurrent first
// runs all missed the read, so each wrote its own id and returned it as
// persisted — one machine reporting many installations. The O_EXCL claim means
// exactly one caller creates the file and the rest adopt its id.
func TestInstallIDConcurrentCreationAgreesOnOneID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-id")

	const callers = 24
	type result struct {
		id        string
		persisted bool
	}
	results := make([]result, callers)

	var start sync.WaitGroup
	var done sync.WaitGroup
	start.Add(1)
	for i := range callers {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			start.Wait() // release everyone at once to widen the window
			id, persisted := installID(path)
			results[i] = result{id: id, persisted: persisted}
		}(i)
	}
	start.Done()
	done.Wait()

	persistedIDs := map[string]int{}
	for _, r := range results {
		if r.persisted {
			persistedIDs[r.id]++
		}
		if _, err := uuid.Parse(r.id); err != nil {
			t.Errorf("id %q is not a UUID", r.id)
		}
	}

	if len(persistedIDs) != 1 {
		t.Errorf("%d distinct ids reported as persisted, want 1: %v", len(persistedIDs), persistedIDs)
	}

	stored, ok := readInstallID(path)
	if !ok {
		t.Fatal("no valid id was stored")
	}
	if _, agrees := persistedIDs[stored]; !agrees {
		t.Errorf("stored id %q is not the one callers reported: %v", stored, persistedIDs)
	}

	// The id must still be stable afterwards.
	again, persisted := installID(path)
	if !persisted || again != stored {
		t.Errorf("installID() = %q, %v after the race; want %q, true", again, persisted, stored)
	}
}

// TestInstallIDRepairsACorruptFile: without the repair a damaged file would
// make every future run report a throwaway id.
func TestInstallIDRepairsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "install-id")
	if err := os.WriteFile(path, []byte("not-a-uuid"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	id, persisted := installID(path)
	if !persisted {
		t.Error("persisted = false for a corrupt but writable file; it should be repaired")
	}

	stored, ok := readInstallID(path)
	if !ok {
		t.Fatal("file was not repaired")
	}
	if stored != id {
		t.Errorf("stored %q but returned %q", stored, id)
	}

	again, persisted := installID(path)
	if !persisted || again != id {
		t.Errorf("installID() = %q, %v on the next run; want %q, true", again, persisted, id)
	}
}
