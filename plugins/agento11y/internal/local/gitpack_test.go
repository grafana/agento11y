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
		{"git push --force", true},
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
		{"git push origin main", false},
		{"git push; echo -f", false},
		{"git push && echo -uf", false},
	} {
		t.Run(tt.command, func(t *testing.T) {
			want := agento11y.HookActionAllow
			if tt.deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, actionFor(tt.command))
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
