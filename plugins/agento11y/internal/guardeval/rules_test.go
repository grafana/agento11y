package guardeval

import (
	"context"
	"log"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// compileRulesForTest compiles rules that are expected to be valid, failing the
// test on any per-rule compile error.
func compileRulesForTest(t *testing.T, raw ...Rule) []CompiledRule {
	t.Helper()
	rules, errs := compileGuardRules(raw)
	require.Empty(t, errs)
	return rules
}

func evaluateForTest(rules []CompiledRule, logger *log.Logger, req agento11y.HookEvaluateRequest) Response {
	resp, _, _ := evaluateWithTransform(context.Background(), rules, logger, req)
	return resp
}

func TestRuleIdentitySelectionPreservesSource(t *testing.T) {
	on, off := true, false
	for _, tc := range []struct {
		name   string
		states []*bool
		want   int
	}{
		{"enabled_twice", []*bool{nil, &on}, 0},
		{"disabled_then_enabled", []*bool{&off, nil, &on}, 1},
		{"disabled_twice", []*bool{&off, &off}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var rules []Rule
			for i, enabled := range tc.states {
				id := " shared "
				if i > 0 {
					id = "shared"
				}
				rules = append(rules, Rule{RuleID: id, Enabled: enabled, Priority: i + 1, ToolFilter: &ToolFilterConfig{BlockedNames: []string{"Bash"}}})
			}
			selected, errs := filterRuleIDs(rules)
			require.Len(t, errs, len(rules)-1)
			require.Len(t, selected, 1)
			assert.Equal(t, rules[tc.want], selected[0])
			data, err := EncodeRules(rules)
			require.NoError(t, err)
			engine := NewEngineFromContents("guards.toml", data, nil)
			if tc.states[tc.want] != nil && !*tc.states[tc.want] {
				assert.Empty(t, engine.rules)
			} else {
				require.Len(t, engine.rules, 1)
				assert.Equal(t, tc.want+1, engine.rules[0].priority)
				assert.Equal(t, "shared", engine.rules[0].id)
			}
		})
	}
}

func TestCompileGuardRules_BadInlineRegexSkipsOnlyThatRule(t *testing.T) {
	rules, errs := compileGuardRules([]Rule{
		{
			RuleID:     "bad",
			Phase:      "postflight",
			Evaluators: []EvaluatorSpec{{Kind: "regex", Config: map[string]any{"patterns": []any{"("}}}},
		},
		{
			RuleID:     "good",
			Phase:      "postflight",
			ToolFilter: &ToolFilterConfig{BlockedNames: []string{"Bash"}},
		},
	})
	require.Len(t, errs, 1)
	assert.Contains(t, errs[0].Error(), `rule "bad"`)
	// The healthy rule still enforces: one uncompilable regex must not disable
	// the rest of the file.
	require.Len(t, rules, 1)
	assert.Equal(t, "good", rules[0].id)
}
