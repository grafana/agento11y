package gitbranch

import (
	"bytes"
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Merge status of a recorded session branch against the workspace's current
// default branch (origin/HEAD, else main/master/trunk). Ancestry needs git;
// Resolve and Repo stay file-only.
const (
	MergeDefault = "default"
	MergeMerged  = "merged"
	MergeOpen    = "open"
	MergeClosed  = "closed"
)

const gitTimeout = 2 * time.Second
const mergeWorkBudget = 4 * time.Second
const squashLogLimit = "120"

const (
	squashLookback  = 2 * 24 * time.Hour
	squashLookahead = 7 * 24 * time.Hour
)

// RepoMerges is one workspace's default branch and which refs are merged
// into it, from a pair of `git for-each-ref` calls plus patch equivalence
// for squash and rebase landings that do not create ancestry.
type RepoMerges struct {
	Default    string
	workspace  string
	mergedInto string
	refs       map[string]struct{}
	merged     map[string]struct{}
	landed     map[string]bool
	patchIDs   map[string]struct{}
	patchSince time.Time
	patchUntil time.Time
	workUntil  time.Time
}

// InspectMerges reads merge ancestry for workspace. Missing git, a missing
// checkout, or any command failure yields an empty result, so Status returns
// "" (unknown) for every branch.
func InspectMerges(workspace string) *RepoMerges {
	empty := &RepoMerges{refs: map[string]struct{}{}, merged: map[string]struct{}{}}
	if workspace == "" || findGitDir(workspace) == "" {
		return empty
	}
	defaultBranch := resolveDefaultBranch(workspace)
	if defaultBranch == "" {
		return empty
	}
	mergedInto := defaultRef(workspace, defaultBranch)
	refs, err := refShortNames(workspace, false, mergedInto)
	if err != nil {
		return empty
	}
	merged, err := refShortNames(workspace, true, mergedInto)
	if err != nil {
		return empty
	}
	return &RepoMerges{
		Default:    defaultBranch,
		workspace:  workspace,
		mergedInto: mergedInto,
		refs:       refs,
		merged:     merged,
		workUntil:  time.Now().Add(mergeWorkBudget),
	}
}

// Status reports default, merged, open, or closed against the workspace's
// current default branch. Empty means git could not tell (no checkout).
// Merged includes squash and rebase landings that `for-each-ref --merged`
// does not see because those commits are not ancestors of the default branch.
func (r *RepoMerges) Status(branch string) string {
	if r == nil || r.Default == "" || branch == "" {
		return ""
	}
	if branch == r.Default {
		return MergeDefault
	}
	if r.mergedIntoDefault(branch) {
		return MergeMerged
	}
	if hasRef(r.refs, branch) {
		eq, known := r.equivalentOnDefault(branch)
		if !known {
			return ""
		}
		if eq {
			return MergeMerged
		}
		return MergeOpen
	}
	if shaRegex.MatchString(branch) {
		// Detached HEAD is recorded as a short SHA, not a ref, so it is
		// unknown rather than a deleted branch.
		return ""
	}
	return MergeClosed
}

// equivalentOnDefault reports whether branch has already landed on the default
// branch without being an ancestor. Single-commit squash and rebase landings
// show up in `git cherry` as '-' lines. GitHub-style squash of several commits
// does not, so those compare the combined `A...B` patch-id to first-parent
// commits on the default branch around the branch tip.
func (r *RepoMerges) equivalentOnDefault(branch string) (landed bool, known bool) {
	if v, ok := r.landed[branch]; ok {
		return v, true
	}
	if r.workspace == "" || r.mergedInto == "" {
		return false, true
	}
	if r.overBudget() {
		return false, false
	}
	if r.landed == nil {
		r.landed = map[string]bool{}
	}
	ref := branch
	if _, ok := r.refs[branch]; !ok {
		ref = "origin/" + branch
	}
	if r.cherryLanded(ref) {
		r.landed[branch] = true
		return true, true
	}
	ok, done := r.squashPatchLanded(ref)
	if !done {
		return false, false
	}
	r.landed[branch] = ok
	return ok, true
}

func (r *RepoMerges) cherryLanded(ref string) bool {
	out, err := gitOutput(r.workspace, "cherry", r.mergedInto, ref)
	if err != nil {
		return false
	}
	saw := false
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		saw = true
		if strings.HasPrefix(line, "+") {
			return false
		}
	}
	return saw
}

func (r *RepoMerges) squashPatchLanded(ref string) (landed bool, done bool) {
	if r.overBudget() {
		return false, false
	}
	ids, err := gitPatchIDs(r.workspace, "diff", r.mergedInto+"..."+ref)
	if err != nil || len(ids) != 1 {
		return false, err == nil
	}
	if r.overBudget() {
		return false, false
	}
	tipRaw, err := gitOutput(r.workspace, "log", "-1", "--format=%ct", ref)
	if err != nil {
		return false, true
	}
	tipSec, err := strconv.ParseInt(strings.TrimSpace(tipRaw), 10, 64)
	if err != nil || tipSec <= 0 {
		return false, true
	}
	if r.overBudget() {
		return false, false
	}
	tip := time.Unix(tipSec, 0)
	if !r.loadMainPatchIDs(tip.Add(-squashLookback), tip.Add(squashLookahead)) {
		return false, false
	}
	_, ok := r.patchIDs[ids[0]]
	return ok, true
}

func (r *RepoMerges) loadMainPatchIDs(since, until time.Time) bool {
	if r.patchIDs == nil {
		r.patchIDs = map[string]struct{}{}
	} else if !r.patchSince.IsZero() && !since.Before(r.patchSince) && !until.After(r.patchUntil) {
		return true
	}
	if r.overBudget() {
		return false
	}
	if !r.patchSince.IsZero() && since.After(r.patchSince) {
		since = r.patchSince
	}
	if !r.patchUntil.IsZero() && until.Before(r.patchUntil) {
		until = r.patchUntil
	}
	ids, err := gitPatchIDs(r.workspace,
		"log", "--first-parent", "-p", "--reverse", "-n", squashLogLimit,
		"--since="+since.Format(time.RFC3339),
		"--until="+until.Format(time.RFC3339),
		r.mergedInto,
	)
	if err != nil {
		return false
	}
	for _, id := range ids {
		r.patchIDs[id] = struct{}{}
	}
	r.patchSince = since
	r.patchUntil = until
	return true
}

func (r *RepoMerges) overBudget() bool {
	return !r.workUntil.IsZero() && time.Now().After(r.workUntil)
}

// mergedIntoDefault prefers the local branch. After a PR lands, origin/feat
// often stays an ancestor of default while local feat keeps new commits;
// those sessions are open, not merged.
func (r *RepoMerges) mergedIntoDefault(branch string) bool {
	if _, ok := r.merged[branch]; ok {
		return true
	}
	if _, local := r.refs[branch]; local {
		return false
	}
	_, ok := r.merged["origin/"+branch]
	return ok
}

func hasRef(set map[string]struct{}, branch string) bool {
	if _, ok := set[branch]; ok {
		return true
	}
	_, ok := set["origin/"+branch]
	return ok
}

func resolveDefaultBranch(workspace string) string {
	out, err := gitOutput(workspace, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err == nil {
		name := strings.TrimSpace(out)
		name = strings.TrimPrefix(name, "origin/")
		if name != "" && name != "HEAD" {
			return name
		}
	}
	for _, name := range []string{"main", "master", "trunk"} {
		if _, err := gitOutput(workspace, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err == nil {
			return name
		}
	}
	return ""
}

func defaultRef(workspace, defaultBranch string) string {
	remote := "refs/remotes/origin/" + defaultBranch
	if _, err := gitOutput(workspace, "rev-parse", "--verify", "--quiet", remote); err == nil {
		return remote
	}
	return "refs/heads/" + defaultBranch
}

func refShortNames(workspace string, mergedOnly bool, mergedInto string) (map[string]struct{}, error) {
	args := []string{"for-each-ref", "--format=%(refname:short)"}
	if mergedOnly {
		args = append(args, "--merged", mergedInto)
	}
	args = append(args, "refs/heads", "refs/remotes/origin")
	out, err := gitOutput(workspace, args...)
	if err != nil {
		return nil, err
	}
	names := map[string]struct{}{}
	for line := range strings.SplitSeq(out, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == "HEAD" || name == "origin" || name == "origin/HEAD" {
			continue
		}
		names[name] = struct{}{}
	}
	return names, nil
}

func gitPatchIDs(workspace string, gitArgs ...string) ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	gitCmd := exec.CommandContext(ctx, "git", append([]string{"-C", workspace}, gitArgs...)...)
	gitCmd.Env = append(gitCmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	gitCmd.Stderr = nil
	stdout, err := gitCmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	idCmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	idCmd.Env = gitCmd.Env
	idCmd.Stdin = stdout
	idCmd.Stderr = nil
	var out bytes.Buffer
	idCmd.Stdout = &out
	if err := gitCmd.Start(); err != nil {
		return nil, err
	}
	if err := idCmd.Start(); err != nil {
		_ = gitCmd.Wait()
		return nil, err
	}
	if err := gitCmd.Wait(); err != nil {
		_ = idCmd.Wait()
		return nil, err
	}
	if err := idCmd.Wait(); err != nil {
		return nil, err
	}
	var ids []string
	for line := range strings.SplitSeq(out.String(), "\n") {
		id, _, _ := strings.Cut(strings.TrimSpace(line), " ")
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func gitOutput(workspace string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", workspace}, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	err := cmd.Run()
	return stdout.String(), err
}
