package gitbranch

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFile creates a file with the given content under root, ensuring parent
// dirs exist. Test helper so HEAD/.git fixtures are easy to set up inline.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestResolve(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (workspaceRoot string)
		want  string
	}{
		{
			name: "regular repo",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/feature/fancy\n")
				return root
			},
			want: "feature/fancy",
		},
		{
			name: "detached HEAD returns 12-char prefix",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/HEAD"), "abcdef0123456789abcdef0123456789abcdef01\n")
				return root
			},
			want: "abcdef012345",
		},
		{
			name: "gitdir indirection (worktree)",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				actualGitDir := filepath.Join(root, "actual-git")
				writeFile(t, filepath.Join(root, "wt/.git"), "gitdir: ../actual-git\n")
				writeFile(t, filepath.Join(actualGitDir, "HEAD"), "ref: refs/heads/wt-branch\n")
				return filepath.Join(root, "wt")
			},
			want: "wt-branch",
		},
		{
			name: "walks up parent directories",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/main\n")
				deep := filepath.Join(root, "a", "b", "c")
				if err := os.MkdirAll(deep, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return deep
			},
			want: "main",
		},
		{
			name: "no .git found",
			setup: func(t *testing.T) string {
				return t.TempDir()
			},
			want: "",
		},
		{
			name: "empty workspace root",
			setup: func(t *testing.T) string {
				return ""
			},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := tc.setup(t)
			if got := Resolve(root); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}
