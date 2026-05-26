package updatecheck

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestShouldRun(t *testing.T) {
	root := t.TempDir()
	withStateRoot(t, root)

	if !ShouldRun("sigil-cc", time.Hour, "1.0.0") {
		t.Fatal("missing stamp should run")
	}

	Record("sigil-cc", "1.0.0")
	if ShouldRun("sigil-cc", time.Hour, "1.0.0") {
		t.Fatal("fresh stamp should skip")
	}

	if !ShouldRun("sigil-cc", time.Hour, "1.0.1") {
		t.Fatal("version mismatch should run")
	}

	Record("sigil-cc", "1.0.1")
	stale := time.Now().Add(-2 * time.Hour)
	path := filepath.Join(root, "update-checks", "sigil-cc.stamp")
	if err := os.Chtimes(path, stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if !ShouldRun("sigil-cc", time.Hour, "1.0.1") {
		t.Fatal("stale stamp should run")
	}
}

func TestDisabled(t *testing.T) {
	for _, v := range []string{"0", "false", "False", "no", "off"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("SIGIL_AUTO_UPDATE", v)
			if !Disabled() {
				t.Fatalf("Disabled() = false for %q", v)
			}
		})
	}

	t.Setenv("SIGIL_AUTO_UPDATE", "true")
	if Disabled() {
		t.Fatal("true should keep auto-update enabled")
	}
}

func withStateRoot(t *testing.T, root string) {
	t.Helper()
	prev := stateRoot
	t.Cleanup(func() { stateRoot = prev })
	stateRoot = func() string { return root }
}
