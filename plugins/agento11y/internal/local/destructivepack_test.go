package local

import (
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDestructivePackRecursiveForce(t *testing.T) {
	rule, err := packRule(packDestructive)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)

	check := func(t *testing.T, command string, deny bool) {
		t.Helper()
		input, err := json.Marshal(map[string]string{"command": command})
		require.NoError(t, err)
		tc := agento11y.ToolCall{ID: "c1", Name: "Bash", InputJSON: input}
		result := engine.Evaluate(agento11y.HookEvaluateRequest{
			Phase: agento11y.HookPhasePostflight,
			Input: agento11y.HookInput{Output: []agento11y.Message{{
				Role:  "assistant",
				Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &tc}},
			}}},
		})
		want := agento11y.HookActionAllow
		if deny {
			want = agento11y.HookActionDeny
		}
		assert.Equal(t, want, result.Action)
	}

	for _, tt := range []struct {
		command string
		deny    bool
	}{
		{"rm -rf /tmp/x", true},
		{`rm -rf"" /tmp/x`, true},
		{`rm -rf'' /tmp/x`, true},
		{"rm 2>/dev/null -rf /tmp/x", true},
		{"rm -r 2>/dev/null -f /tmp/x", true},
		{"rm -f 2>/dev/null -r /tmp/x", true},
		{"rm -r>/dev/null -f /tmp/x", true},
		{"rm>/dev/null -rf /tmp/x", true},
		{"rm2>/dev/null -rf /tmp/x", false},
		{"rm -r2>/dev/null -f /tmp/x", false},
		{"rm --recursive2>/dev/null -f /tmp/x", false},
		{"rm -r >/tmp/log>/tmp/log2 -f /tmp/x", true},
		{"rm -r >/tmp/log2>-f /tmp/x", false},
		{"rm -r 2>&1 -f /tmp/x", true},
		{"rm -r 2>&- -f /tmp/x", true},
		{"rm -r 2>|/tmp/log -f /tmp/x", true},
		{`rm -r <<< '-f' /tmp/x`, false},
		{"rm -r >>/tmp/log 2>&1 -f /tmp/x", true},
		{"rm -r </dev/null -f /tmp/x", true},
		{"rm -r &>/dev/null -f /tmp/x", true},
		{`rm -r 2>"/tmp/log file" -f /tmp/x`, true},
		{`rm -r 2>/tmp/"log file" -f /tmp/x`, true},
		{`rm -r /tmp/"path with spaces" -f`, true},
		{`rm --preserve-root="all" -rf /tmp/x`, true},
		{`rm -v"" -rf /tmp/x`, true},
		{`rm "--preserve-root"=all -rf /tmp/x`, true},
		{"rm > -rf /tmp/x", false},
		{"rm 2>-rf /tmp/x", false},
		{"rm >/tmp/log-rf /tmp/x", false},
		{"rm -r > -f /tmp/x", false},
		{"rm -f 2> -r /tmp/x", false},
		{`rm -r 2>"log -f" /tmp/x`, false},
		{`rm -r 2>/tmp/"log -f" /tmp/x`, false},
		{`rm -r 2>$(printf '%s' -f) /tmp/x`, false},
		{`rm -r 2>$(basename $(printf '%s' -f)) /tmp/x`, false},
		{`rm -r /tmp/"path -f"`, false},
		{`rm --preserve-root="all -rf" /tmp/x`, false},
		{`rm --preserve-root="all" -- -rf`, false},
		{"rm -r 2>`printf '%s' -f` /tmp/x", false},
		{`rm -r --force"-file" /tmp/x`, false},
		{`rm -rf" suffix" /tmp/x`, false},
		{"rm -r 2>/dev/null; echo -f", false},
		{"rm -r 2>/dev/null | echo -f", false},
		{"rm -r 2>/dev/null && echo -f", false},
		{"rm -r 2>/dev/null\n echo -f", false},
		{"rm 2>/dev/null -- -rf", false},
		{"rm -- 2>/dev/null -rf", false},
		{"rm -r > -- -f /tmp/x", true},
		{`rm --"" -rf`, false},
		{`rm -"-" -rf`, false},
		{`rm "-"'-' -rf`, false},
		{`rm \-\- -rf`, false},
		{`rm -r --"" -f`, false},
		{`rm '' -rf /tmp/x`, true},
		{`rm -"" -rf /tmp/x`, true},
		{`rm '-rf' /tmp/x`, true},
		{`rm "-rf" /tmp/x`, true},
		{`rm '-r' /tmp/x`, false},
		{`rm "-f" /tmp/x`, false},
		{`rm '-r' /tmp/x "-f"`, true},
		{`rm '--recursive' /tmp/x '--force'`, true},
		{`rm -r /tmp/$(basename $(pwd)) -f`, true},
		{`rm -r /tmp/$(basename $(pwd))`, false},
		{`rm -r /tmp/$(basename $(pwd)) ; echo -f`, false},
		{`rm -r /tmp/$(printf '%s' $(echo -f))`, false},
		{`rm -r "/tmp/path with spaces" -f`, true},
		{`rm -r "/tmp/path with spaces"; echo -f`, false},
		{"rm -- -rf", false},
		{`rm '--' '-rf'`, false},
		{`rm "--" "-rf"`, false},
		{"rm -r -- -f", false},
		{"rm -f -- -r", false},
		{"rm -rf -- /tmp/x", true},
		{"rm -fr /tmp/x", true},
		{`sh -c 'rm -rf'`, true},
		{`sh -c "rm -r -f"`, true},
		{"rm -Rf /tmp/x", true},
		{"rm -fR /tmp/x", true},
		{"rm -r -f /tmp/x", true},
		{"rm -r /tmp/$(date) -f", true},
		{"rm -f /tmp/$(date | head -1) -r", true},
		{"rm -r /tmp/`date | head -1` -f", true},
		{`rm -r "/tmp/$(date)" -f`, true},
		{"rm /tmp/$(date) -rf", true},
		{"rm /tmp/`date` -r -f", true},
		{"rm -r /tmp/$(date) | echo -f", false},
		{"rm -r /tmp/`date` && echo -f", false},
		{"rm -r /tmp/$(printf '%s' -f)", false},
		{"rm -f /tmp/`printf '%s' -r`", false},
		{"rm -f -r /tmp/x", true},
		{"rm -r --force /tmp/x", true},
		{"rm --force -r /tmp/x", true},
		{"rm -R -f /tmp/x", true},
		{"rm -f -R /tmp/x", true},
		{"rm -R --force /tmp/x", true},
		{"rm --force -R /tmp/x", true},
		{"rm --recursive -f /tmp/x", true},
		{"rm -f --recursive /tmp/x", true},
		{"rm --recursive --force /tmp/x", true},
		{"rm --force --recursive /tmp/x", true},
		{"rm -v -r /tmp/x -f", true},
		{"rm -r /tmp/$(echo x | cat) -f", true},
		{"rm -f /tmp/x -v --recursive", true},
		{"rm\t-r\t-f /tmp/x", true},
		{"rm -r -f; echo ok", true},
		{"echo ok; rm -r -f /tmp/x", true},
		{"rm /tmp/x", false},
		{"rm -r /tmp/x", false},
		{"rm -R /tmp/x", false},
		{"rm --recursive /tmp/x", false},
		{"rm -r /tmp/$(date)", false},
		{"rm -f /tmp/x", false},
		{"rm --force /tmp/x", false},
		{"rm --recursive-file /tmp/x -f", false},
		{"rm -r --force-file /tmp/x", false},
	} {
		t.Run(tt.command, func(t *testing.T) { check(t, tt.command, tt.deny) })
	}

	for _, separator := range []string{";", "&&", "||", "|", "&", "\n", "\r\n"} {
		for _, flags := range [][2]string{{"-r", "-f"}, {"-f", "-r"}, {"--recursive", "--force"}, {"--force", "--recursive"}} {
			for _, command := range []string{
				"rm " + flags[0] + " /tmp/x" + separator + " echo " + flags[1],
				"rm " + flags[0] + " 2>/dev/null" + separator + " echo " + flags[1],
				"rm 2>/dev/null" + separator + " echo " + flags[0] + " " + flags[1],
				"rm " + flags[0] + separator + flags[1],
				"rm" + separator + "echo " + flags[0] + " " + flags[1],
			} {
				t.Run(command, func(t *testing.T) { check(t, command, false) })
			}
		}
	}
}
