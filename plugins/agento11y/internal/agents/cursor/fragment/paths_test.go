package fragment

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoot_XDGOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	want := filepath.Join(dir, "agento11y", "cursor")
	if got := StateRoot(); got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestFragmentFilePath_Layout(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	got := FragmentFilePath("conv-uuid", "gen-id")
	prefix := filepath.Join(dir, "agento11y", "cursor") + string(filepath.Separator)
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, ".json") {
		t.Errorf("got %q, want path under %s ending in .json", got, prefix)
	}
}

func TestFragmentFilePath_PathTraversalNeutralised(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	got := FragmentFilePath("../../etc/passwd", "gen")
	stateRoot := filepath.Join(dir, "agento11y", "cursor")
	rel, err := filepath.Rel(stateRoot, got)
	if err != nil {
		t.Fatalf("Rel error: %v", err)
	}
	rel = filepath.ToSlash(rel)
	if strings.HasPrefix(rel, "..") || strings.Contains(rel, "/../") {
		t.Errorf("path escaped state root: rel=%q got=%q", rel, got)
	}
}

func TestParseFragmentFilename(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"gen-abc.json", "abc"},
		{"gen-.json", ""},
		{"session.json", ""},
		{"gen-abc.txt", ""},
		{"abc.json", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			if got := ParseFragmentFilename(tc.in); got != tc.want {
				t.Errorf("ParseFragmentFilename(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSessionFilePath_LooksRight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	got := filepath.ToSlash(SessionFilePath("conv1"))
	if !strings.HasSuffix(got, "/session.json") {
		t.Errorf("got %q does not end with /session.json", got)
	}
	if !strings.Contains(got, "/agento11y/cursor/conv1") {
		t.Errorf("got %q missing /agento11y/cursor/conv1 segment", got)
	}
}
