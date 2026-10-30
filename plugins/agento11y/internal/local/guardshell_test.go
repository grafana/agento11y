package local

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLocalShellMetadata(t *testing.T) {
	for _, key := range []string{"shell_metadata", "SHELL_METADATA", "Shell_Metadata", "ſhell_metadata"} {
		for _, tc := range []struct {
			name, metadata string
			want           int
		}{
			{"valid", `[{"message_index":0,"part_index":0,"dialect":"bash"}]`, 1},
			{"unknown dialect", `[{"message_index":0,"part_index":0,"dialect":"fish"}]`, 1},
			{"duplicates retained for rejection", `[{"message_index":0,"part_index":0,"dialect":"bash"},{"message_index":0,"part_index":0,"dialect":"bash"}]`, 2},
			{"missing indices", `[{"dialect":"bash"}]`, 0},
			{"missing dialect", `[{"message_index":0,"part_index":0}]`, 0},
			{"null index", `[{"message_index":null,"part_index":0,"dialect":"bash"}]`, 0},
			{"negative index", `[{"message_index":-1,"part_index":0,"dialect":"bash"}]`, 0},
			{"wrong type", `[{"message_index":"0","part_index":0,"dialect":"bash"}]`, 0},
			{"null item", `[null]`, 0},
			{"null", `null`, 0},
			{"not array", `{}`, 0},
			{"invalid duplicate invalidates all", `[{"message_index":0,"part_index":0,"dialect":"bash"},{"message_index":0,"part_index":0}]`, 0},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				body := []byte(`{"phase":"postflight","input":{},"` + key + `":` + tc.metadata + `}`)
				req, metadata, err := decodeLocalHookEvaluateRequest(body)
				require.NoError(t, err)
				assert.Equal(t, agento11y.HookPhasePostflight, req.Phase)
				assert.Len(t, metadata, tc.want)
				stripped, err := stripShellMetadata(body)
				require.NoError(t, err)
				assert.JSONEq(t, `{"phase":"postflight","input":{}}`, string(stripped))
			})
		}
	}
}

func TestServerHookShellMetadata(t *testing.T) {
	const verified = `[{"message_index":0,"part_index":0,"dialect":"bash"}]`
	commandInput := func(command string) string {
		input, err := json.Marshal(map[string]string{"command": command})
		require.NoError(t, err)
		return string(input)
	}
	oversized := commandInput("echo " + strings.Repeat("a", 64*1024) + `; r\m -rf ./src`)
	for _, key := range []string{"shell_metadata", "SHELL_METADATA", "Shell_Metadata", "ſhell_metadata"} {
		for _, tc := range []struct {
			name, tool, input, metadata string
			deny, fallback, redact      bool
		}{
			{"literal", "Bash", commandInput(`echo "rm -rf ./src"`), verified, false, false, false},
			{"substitution", "Bash", commandInput(`echo "$(rm -rf ./src)"`), verified, true, false, false},
			{"missing metadata", "Bash", commandInput(`echo "rm -rf ./src"`), "", false, true, false},
			{"metadata inside arguments is not trusted", "Bash", `{"command":"echo \"rm -rf ./src\"","shell_metadata":` + verified + `}`, "", false, true, false},
			{"missing locator", "Bash", commandInput(`echo "rm -rf ./src"`), `[{"dialect":"bash"}]`, false, true, false},
			{"duplicate locator", "Bash", commandInput(`echo "rm -rf ./src"`), `[{"message_index":0,"part_index":0,"dialect":"bash"},{"message_index":0,"part_index":0,"dialect":"bash"}]`, false, true, false},
			{"unsupported dialect", "Bash", commandInput(`echo "rm -rf ./src"`), `[{"message_index":0,"part_index":0,"dialect":"fish"}]`, false, true, false},
			{"wrong locator", "Bash", commandInput(`echo "rm -rf ./src"`), `[{"message_index":1,"part_index":0,"dialect":"bash"}]`, false, true, false},
			{"generic tool with metadata", "shell", commandInput(`git clean -n -f`), verified, true, true, false},
			{"partial", "Bash", commandInput(`echo "$(rm -rf ./src)"; eval "$script"`), verified, true, true, false},
			{"redacted relay strips metadata", "Bash", commandInput(`echo "secret rm -rf ./src"`), verified, false, false, true},
			{"redaction does not hide denial", "Bash", commandInput(`rm -rf ./src`), verified, true, false, true},
			{"nested local_shell command", "local_shell", `{"action":{"type":"exec","command":["git","reset","--hard"]}}`, "", true, true, false},
			{"nested local_shell safe command", "local_shell", `{"action":{"type":"exec","command":["git","status"]}}`, "", false, true, false},
			{"multiple command keys", "Bash", `{"command":"echo safe","cmd":"git reset --hard"}`, "", true, true, false},
			{"multiple safe command keys", "Bash", `{"command":"echo safe","cmd":"git status"}`, "", false, true, false},
			{"unknown key fallback", "Bash", `{"payload":"git reset --hard"}`, "", true, true, false},
			{"unknown safe key fallback", "Bash", `{"payload":"git status"}`, "", false, true, false},
			{"oversized without metadata", "Bash", oversized, "", true, true, false},
			{"oversized with metadata", "Bash", oversized, verified, true, true, false},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				cloud := newHookCloud(t)
				s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))
				rule, err := packRule(packDestructive)
				require.NoError(t, err)
				gitRule, err := packRule(packGit)
				require.NoError(t, err)
				rules := []guardeval.Rule{rule, gitRule}
				if tc.redact {
					rules = append(rules, guardeval.Rule{RuleID: "redact", Priority: -1, Transform: &guardeval.TransformConfig{Patterns: []guardeval.TransformPattern{{Regex: "secret|rm -rf", Replacement: "masked"}}}})
				}
				data, err := guardeval.EncodeRules(rules)
				require.NoError(t, err)
				writeGuardsFileFor(t, s, string(data))
				req := packRequest(tc.tool, tc.input)
				body, err := json.Marshal(req)
				require.NoError(t, err)
				var requestFields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(body, &requestFields))
				requestFields["extra"] = json.RawMessage(`{"keep":true}`)
				if tc.metadata != "" {
					requestFields[key] = json.RawMessage(tc.metadata)
				}
				body, err = json.Marshal(requestFields)
				require.NoError(t, err)
				status, resp := postHook(t, s, string(body), nil)
				require.Equal(t, http.StatusOK, status)
				assert.Equal(t, tc.deny, resp.Action == agento11y.HookActionDeny)
				var explanation string
				for _, ev := range resp.Evaluations {
					explanation += ev.Explanation
				}
				assert.Equal(t, tc.fallback, strings.Contains(explanation, "incomplete"), explanation)
				assert.Equal(t, tc.fallback, strings.Contains(explanation, "used legacy regex matching"), explanation)
				if tc.deny {
					assert.Zero(t, cloud.count())
					return
				}
				require.Equal(t, 1, cloud.count())
				_, relayed, _ := cloud.lastCall()
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal([]byte(relayed), &fields))
				for field := range fields {
					assert.False(t, strings.EqualFold(field, "shell_metadata"), field)
				}
				if !tc.redact {
					assert.JSONEq(t, `{"keep":true}`, string(fields["extra"]))
				} else {
					assert.NotContains(t, relayed, "secret")
				}
			})
		}
	}
}
