package local

import (
	"encoding/json"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func packRequest(tool, input string) agento11y.HookEvaluateRequest {
	return agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePostflight,
		Input: agento11y.HookInput{Output: []agento11y.Message{{
			Role: "assistant",
			Parts: []agento11y.Part{{Kind: agento11y.PartKindToolCall, ToolCall: &agento11y.ToolCall{
				ID: "c1", Name: tool, InputJSON: json.RawMessage(input),
			}}},
		}}},
	}
}

func TestPermissionsAndDiskPacks(t *testing.T) {
	for _, tc := range []struct {
		pack, command string
		deny          bool
	}{
		{packPermissions, "chmod -R 755 /", true},
		{packPermissions, "chmod 2>/dev/null -R 755 /", true},
		{packPermissions, "chmod -R 2>/dev/null 755 /", true},
		{packPermissions, "chmod -R 755 2>/dev/null /", true},
		{packPermissions, "chmod -R 755 /tmp > /", false},
		{packPermissions, "chmod 2> -R 755 /", false},
		{packPermissions, "chmod2>/dev/null -R 755 /", false},
		{packPermissions, `chmod --reference="/tmp/file" -R /`, true},
		{packPermissions, `chmod --reference="/tmp/file -R /" /tmp`, false},
		{packPermissions, "chmod -R 755 /*", true},
		{packPermissions, "chmod -R 755 ~/", true},
		{packPermissions, `chmod -R 755 "$HOME"`, true},
		{packPermissions, `chmod -R 755 "$HOME/"`, true},
		{packPermissions, `chmod -R 755 "${HOME}"`, true},
		{packPermissions, "chmod --recursive 755 /", true},
		{packPermissions, "chown --recursive root /", true},
		{packPermissions, "chmod -R 755 /; echo ok", true},
		{packPermissions, "chmod 777 /; echo ok", true},
		{packPermissions, "chmod 777 ~/*", true},
		{packPermissions, `chown '-R' root '/'`, true},
		{packPermissions, "chmod -R 755 /tmp; echo /", false},
		{packPermissions, "chmod -R 755 /tmp", false},
		{packPermissions, "chmod --recursive 755 ~/project", false},
		{packPermissions, `chmod -R 755 "$HOME/project"`, false},
		{packPermissions, "chown --recursive root /tmp", false},
		{packPermissions, "chmod 755 /", false},
		{packPermissions, `chmod -R 755 '$HOME'`, false},
		{packPermissions, `chmod -R 755 "~/"`, false},
		{packPermissions, "chmod -R 755 /tmp | echo /", false},
		{packPermissions, "chmod -R 755 /tmp\n echo /", false},
		{packDisk, "dd if=/dev/zero of=/dev/sda", true},
		{packDisk, "dd 2>/dev/null of=/dev/sda", true},
		{packDisk, "dd if=/dev/zero 2>/dev/null of=/dev/sda", true},
		{packDisk, "dd>/dev/null of=/dev/sda", true},
		{packDisk, "dd2>/dev/null of=/dev/sda", false},
		{packDisk, "dd >/tmp/log2>of=/dev/sda", false},
		{packDisk, "dd 2>&1 of=/dev/sda", true},
		{packDisk, `dd 2>/tmp/"log file" of=/dev/sda`, true},
		{packDisk, `dd if="/tmp/input file" of=/dev/sda`, true},
		{packDisk, `dd if=/tmp/"input file" of=/dev/sda`, true},
		{packDisk, "dd > of=/dev/sda", false},
		{packDisk, "dd 2>of=/dev/sda", false},
		{packDisk, "dd 2>/tmp/logof=/dev/sda", false},
		{packDisk, `dd 2>"log of=/dev/sda"`, false},
		{packDisk, `dd 2>/tmp/"log of=/dev/sda"`, false},
		{packDisk, `dd 2>$(printf '%s' of=/dev/sda)`, false},
		{packDisk, `dd if="/tmp/input of=/dev/sda" of=/tmp/image`, false},
		{packDisk, "dd 2>/dev/null; echo of=/dev/sda", false},
		{packDisk, "dd 2>/dev/null | echo of=/dev/sda", false},
		{packDisk, "dd 2>/dev/null && echo of=/dev/sda", false},
		{packDisk, "dd 2>/dev/null\n echo of=/dev/sda", false},
		{packDisk, `dd if=/dev/zero of="/dev/sda"`, true},
		{packDisk, `dd if=/dev/zero of='/dev/sda'`, true},
		{packDisk, `dd if=/dev/zero "of=/dev/sda"`, true},
		{packDisk, `'dd' if=/dev/zero of=/dev/sda`, true},
		{packDisk, `dd if=/dev/zero of="/dev/sda"; echo ok`, true},
		{packDisk, "dd if=/dev/zero of=/tmp/image bs=1 count=1; echo of=/dev/sda", false},
		{packDisk, `dd if=/dev/zero of="/tmp/image" bs=1 count=1; echo of=/dev/sda`, false},
		{packDisk, "dd if=/dev/zero of=/tmp/image bs=1 count=1\n echo of=/dev/sda", false},
		{packDisk, "dd if=/dev/zero of=/tmp/image bs=1 count=1", false},
		{packDisk, "dd if=/dev/sda of=/tmp/image bs=1 count=1", false},
	} {
		t.Run(tc.command, func(t *testing.T) {
			rule, err := packRule(tc.pack)
			require.NoError(t, err)
			data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
			require.NoError(t, err)
			engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
			require.Empty(t, engine.Status().Errors)
			input, err := json.Marshal(map[string]string{"command": tc.command})
			require.NoError(t, err)
			want := agento11y.HookActionAllow
			if tc.deny {
				want = agento11y.HookActionDeny
			}
			assert.Equal(t, want, engine.Evaluate(packRequest("Bash", string(input))).Action)
		})
	}
}

func TestFilesPackBasenamesAndDelimiters(t *testing.T) {
	rule := filesPackRule()
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)
	for _, tool := range envFileToolNames {
		for _, path := range []string{".env", "/repo/.env", ".env.local", "/repo/.env.production", ".env/foo", "/repo/.env/credentials", ".environment", "/repo/.envoy.yaml", ".envrc", "config.env"} {
			t.Run(tool+"/"+path, func(t *testing.T) {
				input, err := json.Marshal(map[string]string{"file_path": path})
				require.NoError(t, err)
				want := agento11y.HookActionAllow
				if path == ".env" || path == "/repo/.env" || path == ".env.local" || path == "/repo/.env.production" || path == ".env/foo" || path == "/repo/.env/credentials" {
					want = agento11y.HookActionDeny
				}
				assert.Equal(t, want, engine.Evaluate(packRequest(tool, string(input))).Action)
			})
		}
	}
	for _, tool := range []string{"Bash", "terminal"} {
		for _, tc := range []struct {
			command string
			deny    bool
		}{
			{"cat <.env", true}, {"echo test >.env", true}, {"cat .env;true", true},
			{"cat .env.local", true}, {`cat <".env"`, true}, {"cat .env|wc -c", true},
			{"cat .env/foo", true}, {"cat /repo/.env/credentials", true},
			{"cat .environment", false}, {"cat <.environment", false}, {"echo test >.envoy.yaml", false},
			{"cat .envrc;true", false}, {"cat config.env", false},
		} {
			t.Run(tool+"/"+tc.command, func(t *testing.T) {
				input, err := json.Marshal(map[string]string{"command": tc.command})
				require.NoError(t, err)
				want := agento11y.HookActionAllow
				if tc.deny {
					want = agento11y.HookActionDeny
				}
				assert.Equal(t, want, engine.Evaluate(packRequest(tool, string(input))).Action)
			})
		}
	}
}

func TestSecretsPackDecodedStrings(t *testing.T) {
	data, err := guardeval.EncodeRules([]guardeval.Rule{secretsPackRule()})
	require.NoError(t, err)
	engine := guardeval.NewEngineFromContents("guards.toml", data, nil)
	require.Empty(t, engine.Status().Errors)
	for _, input := range []string{
		`{"command":"echo ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"command":"echo \u0067hp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"command":"echo\nghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"command":"echo\tghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`,
		`{"command":"echo ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\u0061"}`,
	} {
		t.Run(input, func(t *testing.T) {
			request := packRequest("Bash", input)
			result := engine.Evaluate(request)
			require.Equal(t, agento11y.HookActionAllow, result.Action)
			require.NotNil(t, result.TransformedInput)
			got := string(result.TransformedInput.Output[0].Parts[0].ToolCall.InputJSON)
			assert.Contains(t, got, "[REDACTED:")
			assert.NotContains(t, got, "aaaa")
			assert.Equal(t, input, string(request.Input.Output[0].Parts[0].ToolCall.InputJSON))
		})
	}
	assert.Nil(t, engine.Evaluate(packRequest("Bash", `{"command":"echo ghp_short"}`)).TransformedInput)
}
