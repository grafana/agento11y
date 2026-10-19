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
	for _, key := range []string{"shell_metadata", "SHELL_METADATA", "Shell_Metadata", "ſhell_metadata"} {
		for _, tc := range []struct {
			name, command, metadata string
			deny, fallback, redact  bool
		}{
			{"literal", `echo "rm -rf ./src"`, verified, false, false, false},
			{"substitution", `echo "$(rm -rf ./src)"`, verified, true, false, false},
			{"missing metadata", `echo "rm -rf ./src"`, "", true, true, false},
			{"metadata inside arguments is not trusted", `echo "rm -rf ./src"`, "", true, true, false},
			{"missing locator", `echo "rm -rf ./src"`, `[{"dialect":"bash"}]`, true, true, false},
			{"duplicate locator", `echo "rm -rf ./src"`, `[{"message_index":0,"part_index":0,"dialect":"bash"},{"message_index":0,"part_index":0,"dialect":"bash"}]`, true, true, false},
			{"unsupported dialect", `echo "rm -rf ./src"`, `[{"message_index":0,"part_index":0,"dialect":"fish"}]`, true, true, false},
			{"wrong locator", `echo "rm -rf ./src"`, `[{"message_index":1,"part_index":0,"dialect":"bash"}]`, true, true, false},
			{"partial", `echo "$(rm -rf ./src)"; eval "$script"`, verified, true, true, false},
			{"redacted relay strips metadata", `echo "secret rm -rf ./src"`, verified, false, false, true},
			{"redaction does not hide denial", `rm -rf ./src`, verified, true, false, true},
		} {
			t.Run(key+"/"+tc.name, func(t *testing.T) {
				cloud := newHookCloud(t)
				s, _ := newForwardingTestServer(t, cloud.srv, hookEnv(cloud.srv.URL, nil))
				rule, err := packRule(packDestructive)
				require.NoError(t, err)
				rules := []guardeval.Rule{rule}
				if tc.redact {
					rules = append(rules, guardeval.Rule{RuleID: "redact", Priority: -1, Transform: &guardeval.TransformConfig{Patterns: []guardeval.TransformPattern{{Regex: "secret|rm -rf", Replacement: "masked"}}}})
				}
				data, err := guardeval.EncodeRules(rules)
				require.NoError(t, err)
				writeGuardsFileFor(t, s, string(data))
				req := packRequest("Bash", "{}")
				input, err := json.Marshal(map[string]any{"command": tc.command, "shell_metadata": json.RawMessage(verified)})
				require.NoError(t, err)
				req.Input.Output[0].Parts[0].ToolCall.InputJSON = input
				body, err := json.Marshal(req)
				require.NoError(t, err)
				if tc.metadata != "" {
					var fields map[string]json.RawMessage
					require.NoError(t, json.Unmarshal(body, &fields))
					fields[key] = json.RawMessage(tc.metadata)
					fields["extra"] = json.RawMessage(`{"keep":true}`)
					body, err = json.Marshal(fields)
					require.NoError(t, err)
				}
				status, resp := postHook(t, s, string(body), nil)
				require.Equal(t, http.StatusOK, status)
				assert.Equal(t, tc.deny, resp.Action == agento11y.HookActionDeny)
				var explanation string
				for _, ev := range resp.Evaluations {
					explanation += ev.Explanation
				}
				assert.Equal(t, tc.fallback, strings.Contains(explanation, "incomplete"), explanation)
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
