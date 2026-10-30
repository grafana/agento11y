package local

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGitPackForcePush(t *testing.T) {
	legacyEngine := NewGuardsEngineFromContents("legacy.toml", legacyGuardPacks, nil)
	require.Empty(t, legacyEngine.Status().Errors)
	rule, err := packRule(packGit)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)

	actionFor := func(cmd string) agento11y.HookAction {
		t.Helper()
		tc := agento11y.ToolCall{
			ID:        "c1",
			Name:      "Bash",
			InputJSON: json.RawMessage(fmt.Sprintf(`{"command":%q}`, cmd)),
		}
		return engine.Evaluate(agento11y.HookEvaluateRequest{
			Phase: agento11y.HookPhasePostflight,
			Input: agento11y.HookInput{Output: []agento11y.Message{{
				Role:  "assistant",
				Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &tc}},
			}}},
		}).Action
	}

	for _, tt := range []struct {
		command string
		deny    bool
	}{
		{`sh -c 'git push --force' >/dev/null`, true},
		{`sh -c 'git push --force' 2>/dev/null`, true},
		{`git push --force";not-a-flag"`, false},
		{"git push --force`printf %s -with-lease`", false},
		{`sh -c "git push --force" >/dev/null`, true},
		{`sh -c 'git reset --hard'; echo done`, true},
		{`sh -c "git reset --hard"; echo done`, true},
		{"sh -c 'git push --force'\necho done", true},
		{`git push --force">file"`, false},
		{`git push --force" |suffix"`, false},
		{"echo `git push --force`", true},
		{`git push --force"-with-lease"`, false},
		{"git reset --hard HEAD", true},
		{"git reset HEAD --hard", true},
		{"git reset HEAD --soft", false},
		{"git reset HEAD; echo --hard", false},
		{"git reset -- HEAD --hard", false},
		{"git -C /tmp/repo push --force", true},
		{`git -C "/tmp/repo with spaces" -c core.pager=cat push origin --force`, true},
		{"git --git-dir=/tmp/repo/.git push --force", true},
		{"git --work-tree /tmp/repo push --force-with-lease", false},
		{"git -C /tmp/repo push --force-with-lease", false},
		{"git -C /tmp/repo status; echo push --force", false},
		{"git clean -n; echo -f", false},
		{"git clean -n && echo -f", false},
		{"git clean -n\n echo -f", false},
		{"git clean -f; echo ok", true},
		{`git clean '-f'`, true},
		{"git clean -- -f", false},
		{"git clean -e -- -fd", true},
		{"git clean --exclude -- -fd", true},
		{"git clean -qe -- -fd", true},
		{"git clean -de -- -fd", true},
		{`git clean -e"" -- -fd`, true},
		{"git clean -e -fd", false},
		{"git clean -ef", false},
		{"git clean -qef", false},
		{"git clean -dfevalue", true},
		{"git clean -e-- -fd", true},
		{`git clean '-e--' -fd`, true},
		{`git clean --exclude="--" -fd`, true},
		{`git clean '--exclude=--' -fd`, true},
		{"git clean --exclude= -fd", true},
		{"git clean --exclude= --force", true},
		{`git clean '--exclude=' -fd`, true},
		{`git clean "--exclude=" -fd`, true},
		{`git clean --exclude="" -fd`, true},
		{"git clean --exclude= -- -fd", false},
		{"git clean --exclude=; echo -fd", false},
		{"git clean --exclude=-fd", false},
		{`git clean 'path with spaces' -fd`, true},
		{`git clean path/"with spaces" -fd`, true},
		{`git clean -e '-f'`, false},
		{"git clean -e -e -- -fd", false},
		{"git clean -e --exclude -- -fd", false},
		{`git clean '-e' '--' -fd`, true},
		{`git clean --exclude "--" -fd`, true},
		{"git clean -e -- -e -- -fd", true},
		{"git clean -e -- 2>/dev/null -fd", true},
		{"git clean -- -fd", false},
		{`git clean "--" -fd`, false},
		{"git clean -e -- -- -fd", false},
		{"git clean -- -e -- -fd", false},
		{"git clean -e --; echo -fd", false},
		{`git push "--force"`, true},
		{`git push --force""`, true},
		{`git reset --hard'' HEAD`, true},
		{`git push --force""-with-lease`, false},
		{`git push "--force-with-lease"`, false},
		{`git push "--" --force`, false},
		{"git push --force", true},
		{`git push --repo="origin" --force`, true},
		{`git push --repo='origin' --force`, true},
		{`git push "--repo"=origin --force`, true},
		{`git push -u"" origin --force`, true},
		{`git push ori"gin" --force`, true},
		{`git push --repo="$(printf '%s' origin)" --force`, true},
		{`git push --repo="origin" --force-with-lease`, false},
		{`git push --repo="origin" --force-with-lease=main:abc123`, false},
		{`git push --repo="origin --force"`, false},
		{`git push --repo="$(printf '%s' --force)"`, false},
		{`git push --repo="origin"; echo --force`, false},
		{`git push --repo="origin" | echo --force`, false},
		{`git push --repo="origin" && echo --force`, false},
		{`git push --repo="origin" --force"-with-lease"`, false},
		{`git push "--force"-with-lease`, false},
		{`git push --"force-with-lease"`, false},
		{`git push --repo="origin" -- --force`, false},
		{`git push --force' suffix'`, false},
		{`git push --"" --force`, false},
		{`git push -"-" --force`, false},
		{"git push 2>/dev/null --force", true},
		{"git push origin>/dev/null --force", true},
		{"git push 2>&1 --force", true},
		{"git push2>/dev/null --force", false},
		{"git push >/tmp/log2>--force", false},
		{"git push origin >/tmp/log>/dev/null --force", true},
		{"git push 2>/dev/null --force-with-lease", false},
		{"git push > --force", false},
		{"git push > /tmp/log--force", false},
		{"git push 2> -f", false},
		{`git push 2>"log --force"`, false},
		{`git push 2>$(printf '%s' --force)`, false},
		{"git push 2>/dev/null; echo --force", false},
		{"git push 2>/dev/null\n echo --force", false},
		{"git push -- 2>/dev/null --force", false},
		{"git push > -- --force", true},
		{"git clean -n 2>/dev/null; echo -f", false},
		{"git clean 2>/dev/null -f", true},
		{"git clean > -f", false},
		{"git reset 2>/dev/null HEAD --hard", true},
		{"git reset HEAD > --hard", false},
		{"git push -f", true},
		{"git push -fu origin main", true},
		{"git push -uf origin main", true},
		{"git push origin main --force", true},
		{"git push origin HEAD:$(git branch --show-current) --force", true},
		{"git push origin `git branch --show-current` --force", true},
		{"git push origin HEAD:$(git branch --show-current | head -1) -uf", true},
		{"git push origin HEAD:`git branch --show-current | head -1` --force", true},
		{`git push origin "HEAD:$(git branch --show-current)" --force`, true},
		{"git push origin HEAD:$(git branch --show-current)", false},
		{"git push origin HEAD:$(git branch --show-current) --force-with-lease", false},
		{"git push origin HEAD:$(git branch --show-current) | echo --force", false},
		{"git push origin HEAD:`git branch --show-current` && echo -f", false},
		{"git push --force && echo ok", true},
		{"git push --force&&true", true},
		{"git push -f; echo ok", true},
		{"git push -uf; echo ok", true},
		{"git push --force-with-lease", false},
		{"git push --force-with-lease=main:abc123", false},
		{"git push --force-if-includes", false},
		{"git push -u origin main", false},
		{"git push; echo -f", false},
		{"git push && echo -uf", false},
	} {
		t.Run(tt.command, func(t *testing.T) {
			want := agento11y.HookActionAllow
			if tt.deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, actionFor(tt.command))
			input, err := json.Marshal(map[string]string{"command": tt.command})
			require.NoError(t, err)
			assert.Equal(t, want, legacyEngine.Evaluate(packRequest("Bash", string(input))).Action, "legacy upgrade")
			parsed, _, err := legacyEngine.EvaluateWithShellMetadata(t.Context(), packRequest("Bash", string(input)), []guardeval.ShellMetadata{{Dialect: "bash"}})
			require.NoError(t, err)
			assert.Equal(t, want, parsed.Action, "production parser")
		})
	}
}

func TestGitPackBranchDelete(t *testing.T) {
	rule, err := packRule(packGit)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)

	actionFor := func(cmd string) agento11y.HookAction {
		t.Helper()
		tc := agento11y.ToolCall{
			ID:        "c1",
			Name:      "Bash",
			InputJSON: json.RawMessage(fmt.Sprintf(`{"command":%q}`, cmd)),
		}
		return engine.Evaluate(agento11y.HookEvaluateRequest{
			Phase: agento11y.HookPhasePostflight,
			Input: agento11y.HookInput{Output: []agento11y.Message{{
				Role:  "assistant",
				Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &tc}},
			}}},
		}).Action
	}

	assert.Equal(t, agento11y.HookActionDeny, actionFor("git branch -D leftover"))
	assert.Equal(t, agento11y.HookActionAllow, actionFor("git branch -d leftover"))
	assert.Equal(t, agento11y.HookActionAllow, actionFor("git branch --delete leftover"))
}

// TestGitPackDiscardWorkingTree covers the other ways git throws away
// uncommitted work: checking out the whole tree or with --force, restoring a
// path into the working tree, and a +refspec push, which forces like --force.
// A word in quotes or with a backslash is the word the shell runs.
func TestGitPackDiscardWorkingTree(t *testing.T) {
	rule, err := packRule(packGit)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)

	for _, tt := range []struct {
		cmd  string
		deny bool
	}{
		{"git checkout HEAD .", true},
		{"git checkout main -- src", true},
		{"git checkout ./", true},
		{"git checkout :/", true},
		{"git checkout --force main", true},
		{"git switch --discard-changes main", true},
		{"git switch -f main", true},
		{"git restore .", true},
		{"git restore -- src/main.go", true},
		{"git restore --worktree .", true},
		{"git restore -W src", true},
		{"git restore --source HEAD~1 main.go", true},
		{"git restore -s HEAD~1 main.go", true},
		{"git restore -W --staged .", true},
		{"git restore -SW .", true},
		{"git diff --name-only | xargs -r git restore", true},
		{"git ls-files -m | git restore --pathspec-from-file -", true},
		{"git restore --pathspec-from-file=-", true},
		{"git reset --har HEAD~1", true},
		{"git clean --f -d", true},
		{"git clean --fo -d", true},
		{"git checkout --forc main", true},
		{"git checkout --f main", true},
		{"git switch --disc main", true},
		{"git -p reset --hard", true},
		{"git --no-advice reset --hard", true},
		{"git --glob-pathspecs push --force", true},
		{"git --no-lazy-fetch clean -fd", true},
		{"git reset --help", false},
		{"git switch --fo main", false},
		{"git switch --detach main", false},
		{`git push origin "+main:main"`, true},
		{`"git" reset --hard`, true},
		{`git re"se"t --hard`, true},
		{`g\it reset --hard`, true},
		{"git checkout ./*", true},
		{"git checkout './*'", true},
		{"git restore --worktr .", true},
		{"git restore --sou HEAD a.go", true},
		{"git restore --staged --worktr .", true},
		{"git restore . 2>/dev/null", true},
		{"git restore . # put it back", true},
		{"git restore . && git status", true},
		{`bash -c "git restore ."`, true},
		{`git $"reset" --hard`, true},
		{"git \"reset\" --hard\necho '", true},
		{"git restore -s HEAD~1 --staged .", false},
		{"git restore --st .", false},
		{"git restore --staged . && git status", false},
		{"git checkout main", false},
		{"git checkout feature/fix-tests", false},
		{"git switch -c feature", false},
		{"git switch main", false},
		{"git restore -S src/main.go", false},
		{"git push origin main", false},
		{"git push origin main:main", false},
		{`git commit -m "reset hard"`, false},
		{`git log --grep "checkout ."`, false},
	} {
		t.Run(tt.cmd, func(t *testing.T) {
			input, err := json.Marshal(map[string]string{"command": tt.cmd})
			require.NoError(t, err)
			want := agento11y.HookActionAllow
			if tt.deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, engine.Evaluate(packRequest("Bash", string(input))).Action)
		})
	}
}
