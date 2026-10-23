package local

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStockShellPolicies(t *testing.T) {
	for _, tc := range []struct {
		pack, command  string
		deny, fallback bool
	}{
		{packDestructive, `echo "rm -rf ./src"`, false, false},
		{packDestructive, `echo "$(rm -rf ./src)"`, true, false},
		{packDestructive, `bash -c 'rm -rf ./src'`, true, false},
		{packDestructive, "cat <<'EOF'\nrm -rf ./src\nEOF", false, false},
		{packDestructive, "cat <<EOF\n$(rm -rf ./src)\nEOF", true, false},
		{packDestructive, "bash <<'EOF'\nrm -rf ./src\nEOF", true, false},
		{packDestructive, `rm -r -f ./src`, true, false},
		{packDestructive, `rm -- -rf`, false, false},
		{packDestructive, "echo 'rm -rf ./src'\r", true, true},
		{packDestructive, "echo 'rm -rf ./src'\x00", true, true},
		{packDestructive, `echo "rm -rf ./src`, true, true},
		{packDestructive, `python -c 'print("rm -rf ./src")'`, true, true},
		{packDestructive, `x='$(rm -rf ./src)'; echo "${x@P}"`, true, true},
		{packDestructive, `[[ 'a[$(rm -rf ./src)]' -eq 0 ]]`, true, true},
		{packDestructive, `[[ 0 -lt 'a[$(rm -rf ./src)]' ]]`, true, true},
		{packDestructive, `a=(); [[ -v 'a[$(rm -rf ./src)]' ]]`, true, true},
		{packDestructive, `declare -a 'a[$(rm -rf ./src)]=0'`, true, true},
		{packDestructive, `declare -a 'a=($(rm -rf ./src))'`, true, true},
		{packDestructive, `declare -a a='($(rm -rf ./src))'`, true, true},
		{packDestructive, `readonly -a 'a=($(rm -rf ./src))'`, true, true},
		{packDestructive, `ref='a[$(rm -rf ./src)]'; echo "${!ref}"`, true, true},
		{packDestructive, `git -c core.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, true, true},
		{packDestructive, `git -ccore.sshCommand='rm -rf ./src #' ls-remote ssh://example/repo`, true, true},
		{packDestructive, `GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.sshCommand GIT_CONFIG_VALUE_0='rm -rf ./src #' git ls-remote ssh://example/repo`, true, true},
		{packDestructive, `git -c web.browser=test -c 'browser.test.cmd=rm -rf ./src #' web--browse /tmp/example`, true, true},
		{packDestructive, `git --exec-path -c 'core.sshCommand=rm -rf ./src #' status`, false, false},
		{packDestructive, `x='a[$(rm -rf ./src)]'; a[x]=0`, true, true},
		{packDestructive, `a=(); x='a[$(rm -rf ./src)]'; for (( i=x; i<0; i++ )); do :; done`, true, true},
		{packDestructive, `declare -i x='a[$(rm -rf ./src)]'`, true, true},
		{packDestructive, `typeset -i x='a[$(rm -rf ./src)]'`, true, true},
		{packDestructive, `printf -v 'a[$(rm -rf ./src)]' %s x`, true, true},
		{packDestructive, `printf '%s' 'a[$(rm -rf ./src)]'`, false, false},
		{packDestructive, `[[ 'a[$(rm -rf ./src)]' == literal ]]`, false, false},
		{packGit, `echo "git reset --hard"`, false, false},
		{packGit, `git reset --hard`, true, false},
		{packGit, `git clean -n -f`, false, false},
		{packGit, `git clean -e -n -f`, true, false},
		{packGit, `git clean -n -f --no-dry-run`, true, false},
		{packGit, `git clean --dry-run --force --no-dry-run`, true, false},
		{packGit, `git clean --no-dry-run -f -n`, false, false},
		{packGit, `git clean -nf --no-dry-r`, true, true},
		{packGit, `HOME=--no-dry-run; git clean -nf "$HOME"`, true, true},
		{packGit, `HOME=--no-dry-run; git clean -nf -- "$HOME"`, false, false},
		{packGit, `pattern=; git clean -e $pattern -n -f`, true, true},
		{packGit, `pattern=; git clean --exclude $pattern --dry-run --force`, true, true},
		{packGit, `git clean -e "$pattern" -n -f`, true, true},
		{packGit, `dir='. reset --hard'; git -C $dir HEAD`, false, true},
		{packGit, `git --git-dir "$dir" reset --hard`, true, true},
		{packGit, `git -C ./repo push --force`, true, false},
		{packGit, `git push --force-with-lease`, false, false},
		{packGit, `git push -o --force`, false, false},
		{packPermissions, `chmod -R 755 /`, true, false},
		{packPermissions, `chmod -RH 755 "$HOME"`, true, true},
		{packPermissions, `chmod -R -x "$HOME"`, true, true},
		{packPermissions, `chmod -R -- -x "$HOME"`, true, false},
		{packPermissions, `chmod -R -w /`, true, false},
		{packPermissions, `chmod -R -wx /tmp/project`, false, false},
		{packPermissions, `chmod --reference=/tmp/file -R /`, true, false},
		{packPermissions, `chmod -R 755 /tmp; echo /`, false, false},
		{packPermissions, `chmod 777 "$HOME"`, true, true},
		{packPermissions, `chmod -- 777 "$HOME"`, true, false},
		{packPermissions, `chmod -R 755 '$HOME'`, false, false},
		{packDisk, `dd if=/dev/zero of=/dev/sda`, true, false},
		{packDisk, `dd if=/dev/zero of=/tmp/image; echo of=/dev/sda`, false, false},
		{packDisk, `fdisk -l /dev/sda`, false, false},
		{packDisk, `fdisk --output -l /dev/sda`, true, false},
		{packDisk, `mkfs.ext4 /dev/sda`, true, false},
		{packDisk, `echo wipefs`, false, false},
	} {
		t.Run(tc.pack+"/"+tc.command, func(t *testing.T) {
			rule, err := packRule(tc.pack)
			require.NoError(t, err)
			data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
			require.NoError(t, err)
			engine := NewGuardsEngineFromContents("test.toml", data, nil)
			require.Empty(t, engine.Status().Errors)
			input, err := json.Marshal(map[string]string{"command": tc.command})
			require.NoError(t, err)
			resp, _, err := engine.EvaluateWithShellMetadata(context.Background(), packRequest("Bash", string(input)), []guardeval.ShellMetadata{{Dialect: "bash"}})
			require.NoError(t, err)
			assert.Equal(t, tc.deny, resp.Action == agento11y.HookActionDeny)
			require.Len(t, resp.Evaluations, 1)
			assert.Equal(t, tc.fallback, strings.Contains(resp.Evaluations[0].Explanation, "incomplete"), resp.Evaluations[0].Explanation)
		})
	}
}

func TestStockShellRecognition(t *testing.T) {
	rule, err := packRule(packDestructive)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	raw, err := guardeval.ParseRules(data)
	require.NoError(t, err)
	for _, tc := range []struct {
		name, key string
		value     any
		stock     bool
	}{
		{"unchanged", "", nil, true},
		{"enabled", "enabled", true, true},
		{"disabled", "enabled", false, true},
		{"invalid enabled", "enabled", "true", false},
		{"metadata", "note", "custom", false},
		{"empty match", "match", map[string]any{}, false},
		{"whitespace", "phase", " postflight ", false},
		{"private profile", "shellPolicy", "destructive", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fields map[string]any
			decoder := json.NewDecoder(bytes.NewReader(raw[0]))
			decoder.UseNumber()
			require.NoError(t, decoder.Decode(&fields))
			if tc.key != "" {
				fields[tc.key] = tc.value
			}
			item, err := json.Marshal(fields)
			require.NoError(t, err)
			assert.Equal(t, tc.stock, classifyStockShellPolicy(fields) != "")
			after, err := json.Marshal(fields)
			require.NoError(t, err)
			assert.Equal(t, item, after)
			decoded, errs := guardeval.DecodeRules([]json.RawMessage{item})
			if tc.name == "invalid enabled" {
				require.NotEmpty(t, errs)
				return
			}
			require.Empty(t, errs)
			updated, err := applyPackUpdates(decoded, map[string]bool{packSecrets: false})
			require.NoError(t, err)
			saved, err := guardeval.EncodeRules(updated)
			require.NoError(t, err)
			engine := NewGuardsEngineFromContents("saved", saved, nil)
			resp, _, err := engine.EvaluateWithShellMetadata(t.Context(), packRequest("Bash", `{"command":"echo \"rm -rf ./src\""}`), []guardeval.ShellMetadata{{Dialect: "bash"}})
			require.NoError(t, err)
			assert.Equal(t, !tc.stock, resp.Action == agento11y.HookActionDeny, "stock eligibility after unrelated save")
			if tc.name == "disabled" {
				assert.Empty(t, resp.Evaluations)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		engine   *guardeval.Engine
		metadata []guardeval.ShellMetadata
		deny     bool
	}{
		{"production", NewGuardsEngineFromContents("test", data, nil), []guardeval.ShellMetadata{{Dialect: "bash"}}, false},
		{"generic", guardeval.NewEngineFromContents("test", data, nil), []guardeval.ShellMetadata{{Dialect: "bash"}}, true},
		{"no metadata", NewGuardsEngineFromContents("test", data, nil), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _, err := tc.engine.EvaluateWithShellMetadata(context.Background(), packRequest("Bash", `{"command":"echo \"rm -rf ./src\""}`), tc.metadata)
			require.NoError(t, err)
			assert.Equal(t, tc.deny, resp.Action == agento11y.HookActionDeny)
		})
	}
	custom := append(append([]byte(nil), data...), []byte("note = 'custom'\n")...)
	// The appended TOML key is inside the evaluator table.
	resp, _, err := NewGuardsEngineFromContents("custom", custom, nil).EvaluateWithShellMetadata(context.Background(), packRequest("Bash", `{"command":"echo \"rm -rf ./src\""}`), []guardeval.ShellMetadata{{Dialect: "bash"}})
	require.NoError(t, err)
	assert.Equal(t, agento11y.HookActionDeny, resp.Action)
	for _, kind := range []string{"shell_policy", "shell_command"} {
		t.Run("unsupported kind/"+kind, func(t *testing.T) {
			invalid := strings.Replace(string(data), `kind = "regex"`, `kind = "`+kind+`"`, 1)
			engine := NewGuardsEngineFromContents("invalid", []byte(invalid), nil)
			require.Len(t, engine.Status().Errors, 1)
			assert.Contains(t, engine.Status().Errors[0], "not supported locally")
			assert.Zero(t, engine.Status().Enforcing)
		})
	}
}

func TestStockShellConcurrentRequests(t *testing.T) {
	rule, err := packRule(packDestructive)
	require.NoError(t, err)
	data, err := guardeval.EncodeRules([]guardeval.Rule{rule})
	require.NoError(t, err)
	engine := NewGuardsEngineFromContents("test", data, nil)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			command := `echo "rm -rf ./src"`
			want := agento11y.HookActionAllow
			if i%2 == 0 {
				command = `rm -rf ./src`
				want = agento11y.HookActionDeny
			}
			input, err := json.Marshal(map[string]string{"command": command})
			assert.NoError(t, err)
			resp, _, err := engine.EvaluateWithShellMetadata(context.Background(), packRequest("Bash", string(input)), []guardeval.ShellMetadata{{Dialect: "bash"}})
			assert.NoError(t, err)
			assert.Equal(t, want, resp.Action)
		})
	}
	wg.Wait()
}
