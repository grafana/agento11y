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

	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push --force"))
	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push -f"))
	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push origin main --force"))
	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push --force && echo ok"))
	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push --force&&true"))
	assert.Equal(t, agento11y.HookActionDeny, actionFor("git push -f; echo ok"))
	assert.Equal(t, agento11y.HookActionAllow, actionFor("git push --force-with-lease"))
	assert.Equal(t, agento11y.HookActionAllow, actionFor("git push --force-if-includes"))
	assert.Equal(t, agento11y.HookActionAllow, actionFor("git push origin main"))
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
