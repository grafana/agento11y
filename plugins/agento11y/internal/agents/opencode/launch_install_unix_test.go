//go:build !windows

package opencode

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOpencode writes a script that appends its argv to a log file and exits
// non-zero unless the argv starts with acceptPrefix, the way each OpenCode
// release rejects the other release's plugin command. Returns the script path
// and a reader for the recorded invocations, one line per call.
func fakeOpencode(t *testing.T, acceptPrefix string) (string, func() []string) {
	t.Helper()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	bin := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\n" +
		"echo \"$*\" >> '" + logPath + "'\n" +
		"case \"$*\" in\n" +
		"  '" + acceptPrefix + "'*) echo ok; exit 0 ;;\n" +
		"esac\n" +
		"echo \"Unknown subcommand\" >&2\n" +
		"exit 1\n"
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o755))
	return bin, func() []string {
		b, err := os.ReadFile(logPath)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		require.NoError(t, err)
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
}

// The default installers run the host's real CLI, so the exact argv is the
// contract with each OpenCode release. OpenCode 1.19.3 and 2.0.30 each reject
// the other's spelling with a non-zero exit and leave the config untouched.
func TestDefaultPluginCommands_Argv(t *testing.T) {
	v1Install := "plugin " + PluginSource + " --global"
	v1Update := "plugin " + PluginSource + " --global --force"
	v2Install := "plugin add " + PluginSource

	cases := []struct {
		name     string
		run      func(context.Context, string, io.Writer) error
		accept   string
		wantCall string
		wantErr  bool
	}{
		{name: "v1 install", run: defaultRunInstall, accept: v1Install, wantCall: v1Install},
		{name: "v1 update", run: defaultRunUpdate, accept: v1Update, wantCall: v1Update},
		{name: "v2 install", run: defaultRunInstallV2, accept: v2Install, wantCall: v2Install},
		{name: "v2 host rejects v1 install", run: defaultRunInstall, accept: v2Install, wantCall: v1Install, wantErr: true},
		{name: "v1 host rejects v2 install", run: defaultRunInstallV2, accept: v1Install, wantCall: v2Install, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bin, calls := fakeOpencode(t, tc.accept)

			var out bytes.Buffer
			err := tc.run(context.Background(), bin, &out)
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "opencode "+tc.wantCall)
			} else {
				require.NoError(t, err)
				assert.Contains(t, out.String(), "ok")
			}
			assert.Equal(t, []string{tc.wantCall}, calls())
		})
	}
}
