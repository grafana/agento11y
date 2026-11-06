package gitbranch

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
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

type discoveryCase struct {
	name       string
	setup      func(t *testing.T) string
	wantRepo   string
	wantBranch string
}

func discoveryCases() []discoveryCase {
	var cases []discoveryCase
	for _, depth := range []int{5, 6, 12} {
		for _, pathKind := range []string{"absolute", "relative", "dot"} {
			cases = append(cases, discoveryCase{
				name: fmt.Sprintf("depth %d %s", depth, pathKind),
				setup: func(t *testing.T) string {
					root := t.TempDir()
					writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/deep-branch\n")
					writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@github.com:fixture/deep.git"))
					relative := filepath.FromSlash(strings.Repeat("nested/", depth))
					deep := filepath.Join(root, relative)
					if err := os.MkdirAll(deep, 0o755); err != nil {
						t.Fatal(err)
					}
					switch pathKind {
					case "relative":
						t.Chdir(root)
						return relative
					case "dot":
						t.Chdir(deep)
						return "."
					default:
						return deep
					}
				},
				wantRepo: "fixture/deep", wantBranch: "deep-branch",
			})
		}
	}
	return append(cases,
		discoveryCase{
			name: "nearest nested checkout",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/HEAD"), "ref: refs/heads/outer\n")
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("git@github.com:fixture/outer.git"))
				inner := filepath.Join(root, "inner")
				writeFile(t, filepath.Join(inner, ".git/HEAD"), "ref: refs/heads/inner\n")
				writeFile(t, filepath.Join(inner, ".git/config"), gitConfig("git@github.com:fixture/inner.git"))
				deep := filepath.Join(inner, filepath.FromSlash(strings.Repeat("nested/", 12)))
				if err := os.MkdirAll(deep, 0o755); err != nil {
					t.Fatal(err)
				}
				return deep
			},
			wantRepo: "fixture/inner", wantBranch: "inner",
		},
		discoveryCase{
			name: "deep linked worktree",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				main := filepath.Join(root, "main")
				writeFile(t, filepath.Join(main, ".git/HEAD"), "ref: refs/heads/main\n")
				writeFile(t, filepath.Join(main, ".git/config"), gitConfig("git@github.com:fixture/main.git"))
				writeFile(t, filepath.Join(main, ".git/worktrees/feature/HEAD"), "ref: refs/heads/worktree-branch\n")
				writeFile(t, filepath.Join(main, ".git/worktrees/feature/commondir"), "../..\n")
				wt := filepath.Join(root, "worktree")
				writeFile(t, filepath.Join(wt, ".git"), "gitdir: ../main/.git/worktrees/feature\n")
				deep := filepath.Join(wt, filepath.FromSlash(strings.Repeat("nested/", 12)))
				if err := os.MkdirAll(deep, 0o755); err != nil {
					t.Fatal(err)
				}
				return deep
			},
			wantRepo: "fixture/main", wantBranch: "worktree-branch",
		},
		discoveryCase{
			name: "filesystem root",
			setup: func(t *testing.T) string {
				root := filepath.VolumeName(t.TempDir()) + string(filepath.Separator)
				if _, err := os.Stat(filepath.Join(root, ".git")); !os.IsNotExist(err) {
					t.Skip("filesystem root is not a fixture without .git")
				}
				return root
			},
		},
	)
}

func TestRepo(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
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
			name: "https remote drops query token",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("https://fixture-user:fixture-password@git.example.com/group/subgroup/repo.git?token=synthetic-query-token&next=/private/path"))
				return root
			},
			want: "group/subgroup/repo",
		},
		{
			name: "ssh url with port drops query token",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("ssh://git@git.example.com:2222/group/subgroup/repo.git?token=synthetic-query-token"))
				return root
			},
			want: "group/subgroup/repo",
		},
		{
			name: "quoted url drops fragment token",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig(`"https://git.example.com/group/repo.git#synthetic-fragment-token?next=/private/path"`))
				return root
			},
			want: "group/repo",
		},
		{
			name: "file url drops query and fragment tokens",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig(`"file:///srv/git/repo.git?token=synthetic-query-token#synthetic-fragment-token"`))
				return root
			},
			want: "repo",
		},
		{
			name: "url keeps encoded path delimiters but drops query token",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig("https://git.example.com/group%2Fsubgroup/repo%3Fname%23pin.git?token=synthetic-query-token"))
				return root
			},
			want: "group%2Fsubgroup/repo%3Fname%23pin",
		},
		{
			name: "scp remote keeps question mark and hash in filename",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig(`"git@git.example.com:group/repo?name#pin.git"`))
				return root
			},
			want: "group/repo?name#pin",
		},
		{
			name: "filesystem remote keeps question mark and hash in filename",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				writeFile(t, filepath.Join(root, ".git/config"), gitConfig(`"/srv/git/repo?name#pin.git"`))
				return root
			},
			want: "repo?name#pin",
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
	for _, dc := range discoveryCases() {
		cases = append(cases, struct {
			name  string
			setup func(t *testing.T) string
			goos  string
			want  string
		}{name: dc.name, setup: dc.setup, want: dc.wantRepo})
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
	t.Setenv("PATH", t.TempDir())
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
	for _, dc := range discoveryCases() {
		cases = append(cases, struct {
			name  string
			setup func(t *testing.T) string
			want  string
		}{name: dc.name, setup: dc.setup, want: dc.wantBranch})
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

// remotesCorpus is shared with the pi, opencode and Hermes plugins, which
// derive the repo tag with their own copies of repoFromRemoteURL.
const remotesCorpus = "../../../../conformance/git-remotes/remotes.tsv"

// The corpus lists remote URLs with the label the last release (v0.57.0)
// gave each one and the label wanted now. Rows where the two differ are the
// label changes this package ships, and each one is logged so the change can
// be read off a test run.
func TestRepoFromRemoteURLCorpus(t *testing.T) {
	setGOOS(t, "linux")
	raw, err := os.ReadFile(remotesCorpus)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if lines[0] != "url\trelease\twant" {
		t.Fatalf("header = %q", lines[0])
	}
	for _, line := range lines[1:] {
		cols := strings.Split(line, "\t")
		if len(cols) != 3 {
			t.Fatalf("row %q has %d columns", line, len(cols))
		}
		url, release, want := cols[0], cols[1], cols[2]
		if got := repoFromRemoteURL(url); got != want {
			t.Errorf("repoFromRemoteURL(%q) = %q want %q", url, got, want)
		}
		if want != release {
			t.Logf("changed from release: %s: %q -> %q", url, release, want)
		}
	}
}

// Remotes that name the same repository must give one repo label, or the
// Repository picker lists the repository once per spelling.
func TestRepoFromRemoteURLSameRepositorySameLabel(t *testing.T) {
	pairs := [][2]string{
		{"git@github.com:grafana/agento11y.git", "https://github.com/grafana/agento11y"},
		{"git@github.com:Grafana/Agento11y.git", "https://github.com/grafana/agento11y"},
		{"git@ssh.dev.azure.com:v3/org/proj/repo", "https://dev.azure.com/org/proj/_git/repo"},
		{"https://org.visualstudio.com/DefaultCollection/proj/_git/repo", "https://dev.azure.com/org/proj/_git/repo"},
		{"ssh://git@bitbucket.example.com:7999/proj/repo.git", "https://bitbucket.example.com/scm/proj/repo.git"},
		{"ssh://git@altssh.bitbucket.org:443/Team/Repo.git", "git@bitbucket.org:team/repo.git"},
		{"https://github.com/grafana/agento11y.git/", "https://github.com/grafana/agento11y"},
		{"org-123@github.com:grafana/agento11y.git", "https://github.com/grafana/agento11y"},
	}
	for _, p := range pairs {
		a, b := repoFromRemoteURL(p[0]), repoFromRemoteURL(p[1])
		if a != b {
			t.Errorf("%q -> %q, %q -> %q", p[0], a, p[1], b)
		}
	}
}

// A host outside the known case-insensitive list can be case-sensitive or
// can hold paths that only look like Azure DevOps ones, so it must keep the
// label the last release gave it. The one exception is a three-segment
// http(s) path under `scm/` on a host other than gitlab.com, which loses that
// prefix.
func TestRepoFromRemoteURLUnknownHostsMatchRelease(t *testing.T) {
	setGOOS(t, "linux")
	hosts := []string{
		"gitlab.com", "GitLab.com", "github.example.com", "github.com.example.net",
		"notgithub.com", "bitbucket.org.example", "dev.azure.com.example", "notdev.azure.com",
		"visualstudio.com", "ssh.dev.azure.com.example", "git.example.com", "gh", "[::1]",
	}
	segments := []string{"v3", "_git", "_ssh", "DefaultCollection", "scm", "Org", "proj", "Repo", "a.git", "My%20Project", ""}
	users := []string{"", "git@", "org@", "user:pass@"}
	ports := []string{"", ":22", ":7999"}
	rng := rand.New(rand.NewPCG(1, 2))
	pick := func(list []string) string { return list[rng.IntN(len(list))] }
	for range 5000 {
		host := pick(hosts)
		parts := make([]string, 1+rng.IntN(5))
		for i := range parts {
			parts[i] = pick(segments)
		}
		path := strings.Join(parts, "/")
		if rng.IntN(4) == 0 {
			path += ".git/"
		}
		var url string
		switch rng.IntN(5) {
		case 0:
			url = pick(users) + host + ":" + path
		case 1:
			url = "ssh://" + pick(users) + host + pick(ports) + "/" + path
		case 2:
			url = "git://" + host + pick(ports) + "/" + path
		case 3:
			url = "http://" + pick(users) + host + pick(ports) + "/" + path
		default:
			url = "https://" + pick(users) + host + pick(ports) + "/" + path
		}
		want := releaseRepoFromRemoteURL(url)
		if strings.HasPrefix(url, "http") && !strings.EqualFold(host, "gitlab.com") {
			if rest, ok := strings.CutPrefix(want, "scm/"); ok && strings.Count(rest, "/") == 1 {
				want = rest
			}
		}
		if got := repoFromRemoteURL(url); got != want {
			t.Fatalf("repoFromRemoteURL(%q) = %q want %q", url, got, want)
		}
	}
}

// releaseRepoFromRemoteURL is repoFromRemoteURL as released in v0.57.0, kept
// as the oracle for the unknown-host property. The helpers it calls are
// unchanged since that release.
func releaseRepoFromRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if schemeRe.MatchString(s) {
		s, _, _ = strings.Cut(s, "?")
		s, _, _ = strings.Cut(s, "#")
	}
	var path string
	switch {
	case strings.HasPrefix(s, "file://"):
		return trimRepoName(filepath.Base(strings.TrimRight(strings.TrimPrefix(s, "file://"), "/")))
	case schemeRe.MatchString(s):
		rest := s[strings.Index(s, "://")+3:]
		_, after, ok := strings.Cut(rest, "/")
		if !ok {
			return ""
		}
		path = after
	case isSCPLike(s):
		_, path, _ = strings.Cut(s, ":")
	default:
		return trimRepoName(filepath.Base(strings.TrimRight(s, "/")))
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return trimRepoName(path)
}
