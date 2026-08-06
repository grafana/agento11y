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

// gitConfig renders a minimal repository config with an origin remote, the
// shape `git remote add origin <url>` writes.
func gitConfig(url string) string {
	return "[core]\n\trepositoryformatversion = 0\n[remote \"origin\"]\n\turl = " + url +
		"\n\tfetch = +refs/heads/*:refs/remotes/origin/*\n"
}

// setGOOS points the platform seam at value for the duration of one test, so
// the Windows branches are exercised on any host.
func setGOOS(t *testing.T, value string) {
	t.Helper()
	restore := goos
	goos = value
	t.Cleanup(func() { goos = restore })
}

func TestRepo(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) (workspaceRoot string)
		goos  string // empty leaves the host platform in place
		want  string
	}{
		{
			name: "scp-style ssh remote",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@github.com:grafana/agento11y.git"))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "https remote",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("https://github.com/grafana/agento11y.git"))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "ssh url remote without .git suffix",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("ssh://git@github.com/grafana/agento11y"))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "ssh url remote with port",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("ssh://git@github.com:2222/grafana/agento11y.git"))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "nested namespace is kept whole",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@gitlab.example.com:group/subgroup/repo.git"))
				return root
			},
			want: "group/subgroup/repo",
		},
		{
			name: "filesystem remote uses the directory name",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("/srv/git/agento11y.git"))
				return root
			},
			want: "agento11y",
		},
		{
			name: "windows drive remote uses the directory name",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("C:/repos/agento11y.git"))
				return root
			},
			goos: "windows",
			want: "agento11y",
		},
		{
			name: "quoted remote drops the quotes",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig(`"git@github.com:grafana/agento11y.git"`))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "trailing comment is not part of the url",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@github.com:grafana/agento11y.git ; the main remote"))
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "other remotes are ignored",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				cfg := "[remote \"upstream\"]\n\turl = git@github.com:upstream/other.git\n" +
					gitConfig("git@github.com:grafana/agento11y.git")
				writeFile(t, filepath.Join(root, ".git/config"), cfg)
				return root
			},
			want: "grafana/agento11y",
		},
		{
			name: "worktree reads the main repository config",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				main := filepath.Join(root, "agento11y")
				writeFile(t, filepath.Join(main, ".git/config"), gitConfig("git@github.com:grafana/agento11y.git"))
				wt := filepath.Join(root, "worktrees", "age-1234")
				writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git/worktrees/age-1234")+"\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/age-1234/commondir"), "../..\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/age-1234/HEAD"), "ref: refs/heads/feature/auto-tags\n")
				return wt
			},
			want: "grafana/agento11y",
		},
		{
			name: "worktree without a remote falls back to the main checkout name",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				main := filepath.Join(root, "agento11y")
				writeFile(t, filepath.Join(main, ".git/config"), "[core]\n\tbare = false\n")
				wt := filepath.Join(root, "worktrees", "age-1234")
				writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git/worktrees/age-1234")+"\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/age-1234/commondir"), "../..\n")
				return wt
			},
			want: "agento11y",
		},
		{
			name: "no origin remote falls back to the checkout name",
			setup: func(t *testing.T) string {
				root := filepath.Join(t.TempDir(), "my-project")
				writeFile(t, filepath.Join(root, ".git/config"), "[core]\n\tbare = false\n")
				return root
			},
			want: "my-project",
		},
		{
			name: "no config file falls back to the checkout name",
			setup: func(t *testing.T) string {
				root := filepath.Join(t.TempDir(), "my-project")
				writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/main\n")
				return root
			},
			want: "my-project",
		},
		{
			name: "walks up parent directories",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@github.com:grafana/agento11y.git"))
				deep := filepath.Join(root, "a", "b", "c")
				if err := os.MkdirAll(deep, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return deep
			},
			want: "grafana/agento11y",
		},
		{
			name:  "outside a checkout",
			setup: func(t *testing.T) string { return t.TempDir() },
			want:  "",
		},
		{
			name:  "empty workspace root",
			setup: func(t *testing.T) string { return "" },
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.goos != "" {
				setGOOS(t, tc.goos)
			}
			root := tc.setup(t)
			if got := Repo(root); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

// isSCPLike splits remotes that name a host from remotes that name a local
// path. A single-letter prefix is ambiguous: a drive on Windows, an ssh
// `Host` alias everywhere else, which is why the decision reads the platform.
func TestIsSCPLike(t *testing.T) {
	cases := []struct {
		name string
		url  string
		goos string
		want bool
	}{
		{name: "scp-style ssh remote", url: "git@github.com:grafana/agento11y.git", goos: "linux", want: true},
		{name: "absolute path", url: "/srv/git/agento11y.git", goos: "linux", want: false},
		{name: "path containing a colon", url: "/srv/git:odd/agento11y.git", goos: "linux", want: false},
		{name: "drive remote on windows", url: "C:/repos/agento11y.git", goos: "windows", want: false},
		{name: "drive remote with backslashes on windows", url: `D:\repos\agento11y.git`, goos: "windows", want: false},
		{name: "single-letter host stays a host off windows", url: "C:/repos/agento11y.git", goos: "linux", want: true},
		{name: "two-letter host is not a drive", url: "gh:grafana/agento11y.git", goos: "windows", want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setGOOS(t, tc.goos)
			if got := isSCPLike(tc.url); got != tc.want {
				t.Errorf("isSCPLike(%q) on %s = %v want %v", tc.url, tc.goos, got, tc.want)
			}
		})
	}
}

// configValue decodes a value the way git reads it, so a quoted or commented
// remote URL reaches repoFromRemoteURL as the bare URL. Every want below is
// what `git config --file` prints for the same line, except the unterminated
// quote: git rejects the whole file there, and this reads the value to the
// end of the line instead so one stray quote still yields a repo tag.
func TestConfigValue(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "bare value", raw: "git@github.com:grafana/agento11y.git", want: "git@github.com:grafana/agento11y.git"},
		{name: "quoted value", raw: `"git@github.com:grafana/agento11y.git"`, want: "git@github.com:grafana/agento11y.git"},
		{name: "hash comment", raw: "https://github.com/grafana/agento11y.git # main", want: "https://github.com/grafana/agento11y.git"},
		{name: "semicolon comment", raw: "https://github.com/grafana/agento11y.git ; main", want: "https://github.com/grafana/agento11y.git"},
		{name: "hash inside quotes is kept", raw: `"https://host/grafana/agento11y#pin"`, want: "https://host/grafana/agento11y#pin"},
		{name: "escaped backslashes collapse", raw: `"D:\\repos\\agento11y.git"`, want: `D:\repos\agento11y.git`},
		{name: "escaped quote", raw: `"a\"b"`, want: `a"b`},
		{name: "quoted and bare parts join", raw: `"git@github.com:grafana/"agento11y.git`, want: "git@github.com:grafana/agento11y.git"},
		{name: "unterminated quote reads to the end of the line", raw: `"git@github.com:grafana/agento11y.git`, want: "git@github.com:grafana/agento11y.git"},
		{name: "empty value", raw: "", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := configValue(tc.raw); got != tc.want {
				t.Errorf("configValue(%q) = %q want %q", tc.raw, got, tc.want)
			}
		})
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
			name: "linked worktree reads its own HEAD",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				main := filepath.Join(root, "agento11y")
				writeFile(t, filepath.Join(main, ".git/HEAD"), "ref: refs/heads/main\n")
				wt := filepath.Join(root, "worktrees", "age-1234")
				writeFile(t, filepath.Join(wt, ".git"), "gitdir: "+filepath.Join(main, ".git/worktrees/age-1234")+"\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/age-1234/commondir"), "../..\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/age-1234/HEAD"), "ref: refs/heads/feature/auto-tags\n")
				return wt
			},
			want: "feature/auto-tags",
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
