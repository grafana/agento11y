package guard

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markerLiteral matches the TypeScript copies of EvaluationFailureRuleID.
var markerLiteral = regexp.MustCompile(`EVALUATION_FAILURE_RULE_ID\s*=\s*"([^"]+)"`)

// TestEvaluationFailureRuleID_MatchesPlugins compares the marker rule ID
// against the two TypeScript copies of it.
//
// The Go tests bind to the symbol and each TS test hardcodes its own copy, so
// changing the Go value alone leaves every suite green while the two plugins
// stop recognising a fail-closed deny.
func TestEvaluationFailureRuleID_MatchesPlugins(t *testing.T) {
	// This test file lives at plugins/agento11y/internal/agents/guard.
	plugins, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	require.NoError(t, err)

	for _, plugin := range []string{"pi", "opencode"} {
		t.Run(plugin, func(t *testing.T) {
			path := filepath.Join(plugins, plugin, "src", "guard.ts")
			src, err := os.ReadFile(path)
			require.NoError(t, err, "the marker must stay comparable across languages; if %s moved, update this test", path)

			m := markerLiteral.FindSubmatch(src)
			require.NotNil(t, m, "%s no longer declares EVALUATION_FAILURE_RULE_ID as a string literal", path)
			assert.Equal(t, EvaluationFailureRuleID, string(m[1]),
				"%s must use the same marker as guard.EvaluationFailureRuleID, or a fail-closed deny is rendered as a policy denial", path)
		})
	}
}

// TestPromptMessages_MatchOpencode keeps Go and OpenCode prompt errors equal.
// Quoted literals prevent partial matches and tolerate source reformatting.
func TestPromptMessages_MatchOpencode(t *testing.T) {
	// This test file lives at plugins/agento11y/internal/agents/guard.
	plugins, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	require.NoError(t, err)
	path := filepath.Join(plugins, "opencode", "src", "guard.ts")
	src, err := os.ReadFile(path)
	require.NoError(t, err, "the wording must stay comparable across languages; if %s moved, update this test", path)

	for _, tc := range []struct {
		fn   string
		want string
	}{
		{fn: "formatPromptDeny", want: promptDenyPrefix},
		{fn: "formatPromptEvalFailure", want: promptEvalFailurePrefix},
	} {
		t.Run(tc.fn, func(t *testing.T) {
			assert.True(t, bytes.Contains(src, []byte(strconv.Quote(tc.want))),
				"%s must hold the same sentence as the Go guard package for %s:\n%s", path, tc.fn, tc.want)
		})
	}
}
