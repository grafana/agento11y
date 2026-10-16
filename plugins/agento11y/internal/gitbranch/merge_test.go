package gitbranch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

// fakeGitRealEnv names the real git for a copy of this test binary installed
// as git on PATH. The copy fails every for-each-ref and runs the real git for
// everything else. A Go binary works on every platform, where a shell script
// named git is not found on Windows.
const fakeGitRealEnv = "AGENTO11Y_TEST_FAKE_GIT_REAL"

func TestMain(m *testing.M) {
	if realGit := os.Getenv(fakeGitRealEnv); realGit != "" {
		os.Exit(runFakeGit(realGit, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakeGit(realGit string, args []string) int {
	if slices.Contains(args, "for-each-ref") {
		return 128
	}
	cmd := exec.Command(realGit, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return exitErr.ExitCode()
		}
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// installFakeGit puts a copy of this test binary first on PATH as git.
func installFakeGit(t *testing.T, realGit string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	name := "git"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = src.Close() }()
	dst, err := os.OpenFile(filepath.Join(bin, name), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(dst, src)
	if closeErr := dst.Close(); copyErr != nil || closeErr != nil {
		t.Fatalf("copy test binary: copy=%v close=%v", copyErr, closeErr)
	}
	t.Setenv(fakeGitRealEnv, realGit)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

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
	installFakeGit(t, realGit)

	got := InspectMerges(dir)
	if got.Default != "" {
		t.Fatalf("default = %q, want empty after for-each-ref failure", got.Default)
	}
	if s := got.Status("main"); s != "" {
		t.Fatalf("status = %q, want unknown", s)
	}
}
