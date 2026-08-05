// Package gitbranch resolves git facts about a workspace root — the checked
// out branch and the repository it belongs to — without shelling out to
// `git`. Shared across the codex, copilot, and cursor mappers so non-cursor
// agents don't import a cursor-scoped package.
package gitbranch

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// goos is a test seam for runtime.GOOS.
var goos = runtime.GOOS

var (
	gitDirIndirection = regexp.MustCompile(`(?m)^gitdir:\s*(.+)$`)
	headRefRegex      = regexp.MustCompile(`^ref:\s*refs/heads/(.+)$`)
	shaRegex          = regexp.MustCompile(`^[0-9a-fA-F]{7,}$`)
	originSectionRe   = regexp.MustCompile(`^\[\s*remote\s+"origin"\s*\]`)
	schemeRe          = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)
)

// Resolve walks up to 6 parent directories from workspaceRoot looking for a
// `.git` entry, follows `gitdir:` indirection used by worktrees and
// submodules, and reads HEAD from the resolved git directory.
//
// Returns the branch name on a symbolic ref, the first 12 hex chars on a
// detached HEAD, or "" on any failure (no `.git` found, unreadable file,
// unrecognized HEAD content).
func Resolve(workspaceRoot string) string {
	gitDir := findGitDir(workspaceRoot)
	if gitDir == "" {
		return ""
	}
	return readHeadBranch(gitDir)
}

// Repo returns the repository the workspace belongs to, as `owner/name` taken
// from the `origin` remote URL. Nested namespaces are kept whole, so a GitLab
// subgroup remote resolves to `group/subgroup/name`. Without an origin remote
// it falls back to the name of the checkout directory. Returns "" when
// workspaceRoot is outside a git checkout.
//
// In a linked worktree the remote lives in the main repository, which the
// worktree names in its `commondir` file, so both the URL and the directory
// fallback describe the main checkout rather than the worktree.
func Repo(workspaceRoot string) string {
	gitDir := findGitDir(workspaceRoot)
	if gitDir == "" {
		return ""
	}
	commonDir := resolveCommonDir(gitDir)
	if repo := repoFromRemoteURL(originURL(commonDir)); repo != "" {
		return repo
	}
	return repoFromGitDir(commonDir)
}

// findGitDir walks up to 6 parent directories from workspaceRoot looking for a
// `.git` entry and returns the git directory it names, or "" when there is
// none.
func findGitDir(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	current := workspaceRoot
	for range 6 {
		if gitDir := resolveGitDir(filepath.Join(current, ".git")); gitDir != "" {
			return gitDir
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

// resolveCommonDir returns the git directory holding the shared repository
// state. A linked worktree's own git directory holds only HEAD and index; its
// `commondir` file points at the main repository, which is where `config`
// (and therefore the remote URL) lives. Without that file gitDir is already
// the common directory.
func resolveCommonDir(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		return gitDir
	}
	target := strings.TrimSpace(string(raw))
	if target == "" {
		return gitDir
	}
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	return filepath.Clean(filepath.Join(gitDir, target))
}

// originURL returns the `url` value of the `[remote "origin"]` section of the
// repository config, or "" when the file or the section is missing. The value
// is decoded the way git reads it, so `url = "git@github.com:org/repo.git"`
// gives the same string as the unquoted spelling.
func originURL(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "config"))
	if err != nil {
		return ""
	}
	inOrigin := false
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inOrigin = originSectionRe.MatchString(line)
			continue
		}
		if !inOrigin {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(key) != "url" {
			continue
		}
		return configValue(strings.TrimSpace(value))
	}
	return ""
}

// configValue decodes one git-config value: it drops the quotes around a
// quoted value, resolves backslash escapes, and cuts an unquoted `#` or `;`
// comment together with the whitespace in front of it. Quoting is legal for
// any value, so without this a remote written as
// `url = "git@github.com:org/repo.git"` would keep its closing quote and turn
// the repo tag into `org/repo.git"`. Git rejects a config file whose value
// leaves a quote open; this reads that value to the end of the line instead,
// so one stray quote still yields a usable repo tag.
func configValue(raw string) string {
	var b strings.Builder
	inQuotes := false
	// end is the length of b up to the last byte that was not unquoted
	// whitespace, which is where the value stops if a comment follows.
	end := 0
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '"':
			inQuotes = !inQuotes
			continue
		case !inQuotes && (c == '#' || c == ';'):
			return b.String()[:end]
		case c == '\\' && i+1 < len(raw):
			i++
			b.WriteByte(configEscape(raw[i]))
		default:
			b.WriteByte(c)
		}
		if inQuotes || (c != ' ' && c != '\t') {
			end = b.Len()
		}
	}
	return b.String()[:end]
}

// configEscape resolves the escapes git recognizes inside a config value.
// A remote URL only ever carries `\\` and `\"`; git also defines \n, \t and
// \b. Git treats every other escape as an error, and this returns the bare
// character for it.
func configEscape(c byte) byte {
	switch c {
	case 'n':
		return '\n'
	case 't':
		return '\t'
	case 'b':
		return '\b'
	default:
		return c
	}
}

// repoFromRemoteURL reduces a remote URL to the path that identifies the
// repository on its host: `git@host:owner/name.git`, `https://host/owner/name`
// and `ssh://git@host:2222/owner/name` all yield `owner/name`. A filesystem
// remote has no host-side path, so it yields the directory name alone.
func repoFromRemoteURL(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
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
		// A local path remote (/srv/git/name.git, ../name, ~/name).
		return trimRepoName(filepath.Base(strings.TrimRight(s, "/")))
	}
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	return trimRepoName(path)
}

// isSCPLike reports whether s uses git's scp-style syntax, `[user@]host:path`.
// The marker is a colon before the first slash: `git@github.com:owner/name`
// is scp-style, `/srv/git:odd/name` is a filesystem path.
//
// On Windows a single-letter prefix is a drive designator, so
// `C:/repos/name.git` is a local path there. The check is platform-gated
// because git gates it the same way: its drive-prefix test is a no-op on
// POSIX builds, where `g:owner/name.git` is a working remote that reaches
// host `g` through an ssh `Host g` alias.
func isSCPLike(s string) bool {
	colon := strings.Index(s, ":")
	if colon <= 0 {
		return false
	}
	if goos == "windows" && hasDrivePrefix(s) {
		return false
	}
	slash := strings.Index(s, "/")
	return slash == -1 || colon < slash
}

// hasDrivePrefix reports whether s starts with a Windows drive designator,
// `C:` or `c:`.
func hasDrivePrefix(s string) bool {
	if len(s) < 2 || s[1] != ':' {
		return false
	}
	c := s[0]
	return ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z')
}

func trimRepoName(path string) string {
	return strings.TrimSuffix(path, ".git")
}

// repoFromGitDir names the repository after its directory, used when the
// repository has no origin remote. `<checkout>/.git` yields the checkout
// directory name; a bare or submodule git directory yields its own name.
func repoFromGitDir(gitDir string) string {
	base := filepath.Base(gitDir)
	if base == ".git" {
		base = filepath.Base(filepath.Dir(gitDir))
	}
	base = trimRepoName(base)
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
}

// resolveGitDir maps `<workspace>/.git` to the actual git directory. Returns
// the path when `.git` is a directory, follows `gitdir: <path>` when it's a
// file (worktrees, submodules), or "" when missing.
func resolveGitDir(gitPath string) string {
	info, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return gitPath
	}
	if !info.Mode().IsRegular() {
		return ""
	}
	raw, err := os.ReadFile(gitPath)
	if err != nil {
		return ""
	}
	m := gitDirIndirection.FindStringSubmatch(strings.TrimSpace(string(raw)))
	if len(m) < 2 {
		return ""
	}
	target := strings.TrimSpace(m[1])
	if filepath.IsAbs(target) {
		return target
	}
	return filepath.Clean(filepath.Join(filepath.Dir(gitPath), target))
}

func readHeadBranch(gitDir string) string {
	raw, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	content := strings.TrimSpace(string(raw))
	if m := headRefRegex.FindStringSubmatch(content); len(m) >= 2 {
		return m[1]
	}
	if shaRegex.MatchString(content) {
		// Detached HEAD: keep the first 12 hex chars to match
		// `git rev-parse --short=12 HEAD`.
		if len(content) > 12 {
			return content[:12]
		}
		return content
	}
	return ""
}
