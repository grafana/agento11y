package fragment

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestStateRoot_XDGOverride(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	if got := StateRoot(); got != "/custom/state/sigil/cursor" {
		t.Errorf("got %q want /custom/state/sigil/cursor", got)
	}
}

func TestFragmentFilePath_Layout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x")
	got := FragmentFilePath("conv-uuid", "gen-id")
	prefix := filepath.Join("/x", "sigil", "cursor") + "/"
	if !strings.HasPrefix(got, prefix) || !strings.HasSuffix(got, ".json") {
		t.Errorf("got %q, want path under %s ending in .json", got, prefix)
	}
}

func TestFragmentFilePath_PathTraversalNeutralised(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/x")
	got := FragmentFilePath("../../etc/passwd", "gen")
	stateRoot := filepath.Join("/x", "sigil", "cursor")
	rel, err := filepath.Rel(stateRoot, got)
	if err != nil {
		t.Fatalf("Rel error: %v", err)
	}
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
	t.Setenv("XDG_STATE_HOME", "/x")
	got := SessionFilePath("conv1")
	if !strings.HasSuffix(got, "/session.json") {
		t.Errorf("got %q does not end with /session.json", got)
	}
	if !strings.Contains(got, "/sigil/cursor/conv1") {
		t.Errorf("got %q missing /sigil/cursor/conv1 segment", got)
	}
}
