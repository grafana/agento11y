package xdg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSafeComponentAddsHashToAvoidCollisions(t *testing.T) {
	a := SafeComponent("a/b")
	b := SafeComponent("a_b")
	if a == b {
		t.Fatalf("expected distinct safe components, both were %q", a)
	}
	if !strings.HasPrefix(a, "a_b-") || !strings.HasPrefix(b, "a_b-") {
		t.Fatalf("expected readable prefix plus hash, got %q and %q", a, b)
	}
}

func TestSafeComponentReturnsUnknownForEmpty(t *testing.T) {
	if got := SafeComponent(""); got != "unknown" {
		t.Fatalf("SafeComponent(\"\") = %q, want %q", got, "unknown")
	}
	if got := SafeComponent("   "); got != "unknown" {
		t.Fatalf("SafeComponent(blank) = %q, want %q", got, "unknown")
	}
}

func TestStateRootIgnoresRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative")

	got := StateRoot("sigil")
	if !filepath.IsAbs(got) {
		t.Fatalf("StateRoot() = %q, want absolute path", got)
	}
	if strings.HasPrefix(got, "relative") {
		t.Fatalf("StateRoot() used relative XDG_STATE_HOME: %q", got)
	}
}

func TestStateRootHonorsAbsoluteXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/test-state")
	got := StateRoot("sigil")
	want := filepath.Join("/tmp/test-state", "sigil")
	if got != want {
		t.Fatalf("StateRoot() = %q, want %q", got, want)
	}
}

func TestLogFilePathUsesStateRoot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	got := LogFilePath()
	want := filepath.Join(dir, "agento11y", "logs", "agento11y.log")
	if got != want {
		t.Fatalf("LogFilePath() = %q, want %q", got, want)
	}
}

func TestAppStateRoot(t *testing.T) {
	cases := []struct {
		name     string
		existing []string
		want     string
	}{
		{name: "fresh install uses new dir", existing: nil, want: "agento11y"},
		{name: "legacy only keeps legacy dir", existing: []string{"sigil"}, want: "sigil"},
		{name: "new only uses new dir", existing: []string{"agento11y"}, want: "agento11y"},
		{name: "both prefer new dir", existing: []string{"sigil", "agento11y"}, want: "agento11y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("XDG_STATE_HOME", dir)
			for _, name := range tc.existing {
				if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			if got, want := AppStateRoot(), filepath.Join(dir, tc.want); got != want {
				t.Fatalf("AppStateRoot() = %q, want %q", got, want)
			}
		})
	}
}

func TestConfigFilePathUsesConfigRoot(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/test-config")
	got := ConfigFilePath("sigil", "config.env")
	want := filepath.Join("/tmp/test-config", "sigil", "config.env")
	if got != want {
		t.Fatalf("ConfigFilePath() = %q, want %q", got, want)
	}
}
