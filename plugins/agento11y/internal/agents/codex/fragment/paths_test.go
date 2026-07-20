package fragment

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFragmentFilePathSanitizesIDs(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	got := FragmentFilePath("../session", "turn/one")
	if strings.Contains(got, "..") {
		t.Fatalf("path was not sanitized: %q", got)
	}
	wantSuffix := filepath.Join(filepath.Join("agento11y", "codex"), "turns", safeComponent("../session"), safeComponent("turn/one")+".json")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("path = %q, want suffix %q", got, wantSuffix)
	}
}

func TestSessionFilePathSanitizesID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	got := SessionFilePath("../session")
	if strings.Contains(got, "..") {
		t.Fatalf("path was not sanitized: %q", got)
	}
	wantSuffix := filepath.Join(filepath.Join("agento11y", "codex"), "sessions", safeComponent("../session")+".json")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("path = %q, want suffix %q", got, wantSuffix)
	}
}

func TestSubagentLinkFilePathSanitizesID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/state")
	got := SubagentLinkFilePath("../session")
	if strings.Contains(got, "..") {
		t.Fatalf("path was not sanitized: %q", got)
	}
	wantSuffix := filepath.Join(filepath.Join("agento11y", "codex"), "subagents", safeComponent("../session")+".json")
	if !strings.HasSuffix(got, wantSuffix) {
		t.Fatalf("path = %q, want suffix %q", got, wantSuffix)
	}
}

func TestSafeComponentAddsHashToAvoidCollisions(t *testing.T) {
	a := safeComponent("a/b")
	b := safeComponent("a_b")
	if a == b {
		t.Fatalf("expected distinct safe components, both were %q", a)
	}
	if !strings.HasPrefix(a, "a_b-") || !strings.HasPrefix(b, "a_b-") {
		t.Fatalf("expected readable prefix plus hash, got %q and %q", a, b)
	}
}

func TestStateRootIgnoresRelativeXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative")

	got := StateRoot()
	if !filepath.IsAbs(got) {
		t.Fatalf("StateRoot() = %q, want absolute path", got)
	}
	if strings.HasPrefix(got, "relative") {
		t.Fatalf("StateRoot() used relative XDG_STATE_HOME: %q", got)
	}
}
