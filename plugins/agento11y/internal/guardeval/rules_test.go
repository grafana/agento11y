package guardeval

import (
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
	resp, _ := evaluateWithTransform(rules, logger, req)
	return resp
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
