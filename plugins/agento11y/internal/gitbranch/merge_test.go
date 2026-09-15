package gitbranch

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for merge-status tests")
	}
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@example.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@example.com",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func initRepo(t *testing.T) string {
	t.Helper()
	requireGit(t)
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "file")
	gitRun(t, dir, "commit", "-m", "init")
	return dir
}

func TestInspectMerges(t *testing.T) {
	dir := initRepo(t)

	gitRun(t, dir, "checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "feat")
	gitRun(t, dir, "checkout", "main")
	gitRun(t, dir, "merge", "--no-ff", "-m", "merge feat", "feat")

	gitRun(t, dir, "checkout", "-b", "open")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("open\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "open")

	got := InspectMerges(dir)
	if got.Default != "main" {
		t.Fatalf("default = %q, want main", got.Default)
	}
	if s := got.Status("main"); s != MergeDefault {
		t.Errorf("main status = %q, want %q", s, MergeDefault)
	}
	if s := got.Status("feat"); s != MergeMerged {
		t.Errorf("feat status = %q, want %q", s, MergeMerged)
	}
	if s := got.Status("open"); s != MergeOpen {
		t.Errorf("open status = %q, want %q", s, MergeOpen)
	}
	if s := got.Status("deleted-after-merge"); s != MergeClosed {
		t.Errorf("missing branch status = %q, want %q", s, MergeClosed)
	}
	if s := got.Status(""); s != "" {
		t.Errorf("empty branch status = %q, want empty", s)
	}
}

func TestStatusDetachedHeadStaysUnknown(t *testing.T) {
	r := &RepoMerges{
		Default: "main",
		refs:    map[string]struct{}{},
		merged:  map[string]struct{}{},
	}
	if s := r.Status("a1b2c3d4e5f6"); s != "" {
		t.Fatalf("detached status = %q, want unknown", s)
	}
	if s := r.Status("deleted-after-merge"); s != MergeClosed {
		t.Fatalf("missing branch status = %q, want %q", s, MergeClosed)
	}
}

func TestStatusLocalAheadOfStaleRemoteMerged(t *testing.T) {
	dir := initRepo(t)

	gitRun(t, dir, "checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "feat")
	gitRun(t, dir, "checkout", "main")
	gitRun(t, dir, "merge", "--no-ff", "-m", "merge feat", "feat")
	gitRun(t, dir, "update-ref", "refs/remotes/origin/feat", "feat")

	gitRun(t, dir, "checkout", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat-again\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "more work")

	got := InspectMerges(dir)
	if s := got.Status("feat"); s != MergeOpen {
		t.Fatalf("local-ahead status = %q, want %q", s, MergeOpen)
	}

	gitRun(t, dir, "checkout", "main")
	gitRun(t, dir, "branch", "-D", "feat")
	got = InspectMerges(dir)
	if s := got.Status("feat"); s != MergeMerged {
		t.Fatalf("stale-remote-only status = %q, want %q", s, MergeMerged)
	}
}

func TestStatusOverBudgetStaysUnknown(t *testing.T) {
	r := &RepoMerges{
		Default:    "main",
		workspace:  "x",
		mergedInto: "refs/heads/main",
		refs:       map[string]struct{}{"feat": {}},
		merged:     map[string]struct{}{},
		workUntil:  time.Now().Add(-time.Second),
	}
	if s := r.Status("feat"); s != "" {
		t.Fatalf("over-budget status = %q, want unknown", s)
	}
}

func TestInspectMergesMultiCommitSquashIsMerged(t *testing.T) {
	dir := initRepo(t)

	gitRun(t, dir, "checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "feat")
	if err := os.WriteFile(filepath.Join(dir, "other"), []byte("more\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "other")
	gitRun(t, dir, "commit", "-m", "more")
	gitRun(t, dir, "checkout", "main")
	gitRun(t, dir, "merge", "--squash", "feat")
	gitRun(t, dir, "commit", "-m", "Remove sql abstraction tooling (#9476)")

	got := InspectMerges(dir)
	if s := got.Status("feat"); s != MergeMerged {
		t.Fatalf("multi-commit squash feat status = %q, want %q", s, MergeMerged)
	}
}

func TestInspectMergesSquashMergedIsMerged(t *testing.T) {
	dir := initRepo(t)

	gitRun(t, dir, "checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "feat")
	gitRun(t, dir, "checkout", "main")
	gitRun(t, dir, "merge", "--squash", "feat")
	gitRun(t, dir, "commit", "-m", "feat(agento11y): add rules list-scores (#1226)")

	got := InspectMerges(dir)
	if s := got.Status("feat"); s != MergeMerged {
		t.Fatalf("squash feat status = %q, want %q", s, MergeMerged)
	}
	if s := got.Status("open-missing"); s != MergeClosed {
		t.Fatalf("missing branch status = %q, want %q", s, MergeClosed)
	}
}

func TestInspectMergesUnlandedBranchStaysOpen(t *testing.T) {
	dir := initRepo(t)

	gitRun(t, dir, "checkout", "-b", "feat")
	if err := os.WriteFile(filepath.Join(dir, "file"), []byte("feat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "commit", "-am", "feat")
	gitRun(t, dir, "checkout", "main")

	got := InspectMerges(dir)
	if s := got.Status("feat"); s != MergeOpen {
		t.Fatalf("unlanded feat status = %q, want %q", s, MergeOpen)
	}
}

func TestInspectMergesOutsideCheckout(t *testing.T) {
	got := InspectMerges(t.TempDir())
	if got.Status("main") != "" {
		t.Fatalf("status = %q, want empty", got.Status("main"))
	}
	if InspectMerges("").Status("main") != "" {
		t.Fatal("empty workspace should stay unknown")
	}
}

func TestInspectMergesForEachRefFailureStaysUnknown(t *testing.T) {
	dir := initRepo(t)
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := filepath.Join(bin, "git")
	script := "#!/bin/sh\nfor arg in \"$@\"; do\n  if [ \"$arg\" = \"for-each-ref\" ]; then\n    exit 128\n  fi\ndone\nexec '" +
		strings.ReplaceAll(realGit, "'", `'\''`) +
		"' \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got := InspectMerges(dir)
	if got.Default != "" {
		t.Fatalf("default = %q, want empty after for-each-ref failure", got.Default)
	}
	if s := got.Status("main"); s != "" {
		t.Fatalf("status = %q, want unknown", s)
	}
}
