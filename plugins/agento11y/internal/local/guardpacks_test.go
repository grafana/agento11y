package local

import (
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestApplyPackUpdates_PreservesCustomRules(t *testing.T) {
	custom := guardeval.Rule{RuleID: "block.rm", Phase: "postflight"}
	out, err := applyPackUpdates([]guardeval.Rule{custom}, map[string]bool{packSecrets: true, packGit: true})
	require.NoError(t, err)
	require.Len(t, out, 3)
	assert.Equal(t, "block.rm", out[0].RuleID)
	assert.Equal(t, packRuleID(packSecrets), out[1].RuleID)
	assert.Equal(t, packRuleID(packGit), out[2].RuleID)

	out, err = applyPackUpdates(out, map[string]bool{packSecrets: false})
	require.NoError(t, err)
	require.Len(t, out, 2)
	assert.Equal(t, "block.rm", out[0].RuleID)
	assert.Equal(t, packRuleID(packGit), out[1].RuleID)
}

func TestApplyPackUpdates_PreservesUnknownPackPrefix(t *testing.T) {
	future := guardeval.Rule{RuleID: "pack.newfeature", Phase: "postflight"}
	hand := guardeval.Rule{RuleID: "pack.myown", Phase: "postflight"}
	out, err := applyPackUpdates([]guardeval.Rule{future, hand}, map[string]bool{packGit: true})
	require.NoError(t, err)
	ids := make([]string, 0, len(out))
	for _, r := range out {
		ids = append(ids, r.RuleID)
	}
	assert.Equal(t, []string{"pack.newfeature", "pack.myown", packRuleID(packGit)}, ids)
}

func TestApplyPackUpdates_UnknownPack(t *testing.T) {
	_, err := applyPackUpdates(nil, map[string]bool{"nope": true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown pack")
}

func TestPackRulesCompile(t *testing.T) {
	for _, id := range []string{packSecrets, packFiles, packGit, packDestructive, packPermissions, packDisk} {
		t.Run(id, func(t *testing.T) {
			rule, err := packRule(id)
			require.NoError(t, err)
			data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
			require.NoError(t, err)
			engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
			require.Empty(t, engine.Status().Errors, engine.Status().Errors)
			assert.Equal(t, 1, engine.Status().Enforcing)
		})
	}
}

func TestPacksFromRules(t *testing.T) {
	packs := packsFromRules([]guardeval.Rule{{RuleID: packRuleID(packSecrets)}})
	require.Len(t, packs, len(catalogPacks()))
	assert.True(t, packs[0].Enabled)
	for _, p := range packs[1:] {
		assert.False(t, p.Enabled, p.ID)
	}
}

func TestEnvFileBlockedNamesCoverHostTools(t *testing.T) {
	names := envFileBlockedNames()
	require.Len(t, names, len(envFileToolNames)*3)
	for i, tool := range envFileToolNames {
		assert.Equal(t, tool+`(*/.env*)`, names[i*3])
		assert.Equal(t, tool+`(*".env*)`, names[i*3+1])
		assert.Equal(t, tool+`(* .env*)`, names[i*3+2])
	}
}
