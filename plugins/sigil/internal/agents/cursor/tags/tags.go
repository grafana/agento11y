package tags

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BuiltinInputs carries the values used to populate built-in tag keys.
type BuiltinInputs struct {
	WorkspaceRoot     string
	Cwd               string
	Entrypoint        string
	GitBranch         string
	IsBackgroundAgent bool
}

// Build returns the per-generation built-in tags. SIGIL_TAGS-supplied values
// are layered in by the SDK at the client level; built-ins take precedence
// because the SDK merges per-generation Tags atop client-level Tags.
// Returns nil when no built-ins are present.
func Build(in BuiltinInputs) map[string]string {
	out := make(map[string]string, 4)
	if in.GitBranch != "" {
		out["git.branch"] = in.GitBranch
	}
	cwd := in.Cwd
	if cwd == "" {
		cwd = in.WorkspaceRoot
	}
	if cwd != "" {
		out["cwd"] = cwd
	}
	if in.Entrypoint != "" {
		out["entrypoint"] = in.Entrypoint
	}
	if in.IsBackgroundAgent {
		out["subagent"] = "true"
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

var (
	gitDirIndirection = regexp.MustCompile(`(?m)^gitdir:\s*(.+)$`)
	headRefRegex      = regexp.MustCompile(`^ref:\s*refs/heads/(.+)$`)
	shaRegex          = regexp.MustCompile(`^[0-9a-fA-F]{7,}$`)
)

// ResolveGitBranch walks up to 6 parent directories from workspaceRoot looking
// for a `.git` entry, follows `gitdir:` indirection used by worktrees and
// submodules, and reads HEAD from the resolved git directory.
//
// Returns the branch name on a symbolic ref, the first 12 hex chars on a
// detached HEAD, or "" on any failure (no `.git` found, unreadable file,
// unrecognized HEAD content).
func ResolveGitBranch(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	current := workspaceRoot
	for i := 0; i < 6; i++ {
		gitPath := filepath.Join(current, ".git")
		if gitDir := resolveGitDir(gitPath); gitDir != "" {
			return readHeadBranch(gitDir)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
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
