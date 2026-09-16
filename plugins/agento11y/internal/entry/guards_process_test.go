package entry

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuardsBrokenPipe(t *testing.T) {
	if os.Getenv("GUARDS_TEST_BROKEN_PIPE") == "1" {
		code := 0
		exit = func(value int) { code = value }
		run(os.Args[slices.Index(os.Args, "--")+1:], strings.NewReader(""), os.Stdout, os.Stderr)
		_ = os.RemoveAll(os.Getenv("HOME"))
		os.Exit(code)
	}
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"result", []string{"test", "--json", "echo ok"}},
		{"help", []string{"test", "--help"}},
		{"parse error", []string{"test", "--unknown"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer, err := os.Pipe()
			require.NoError(t, err)
			require.NoError(t, reader.Close())
			t.Cleanup(func() { _ = writer.Close() })
			args := append([]string{"-test.run=^TestGuardsBrokenPipe$", "--", "guards"}, tc.args...)
			cmd := exec.Command(binary, args...)
			cmd.Env = []string{"GUARDS_TEST_BROKEN_PIPE=1", "HOME=" + t.TempDir(), "TMPDIR=" + t.TempDir()}
			var stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = writer, &stderr
			if tc.name == "parse error" {
				cmd.Stdout, cmd.Stderr = &stderr, writer
			}
			err = cmd.Run()
			var exitErr *exec.ExitError
			require.True(t, errors.As(err, &exitErr), "error: %v", err)
			require.Equal(t, 2, exitErr.ExitCode(), "stderr: %s", stderr.String())
			if tc.name != "parse error" {
				require.Contains(t, stderr.String(), "write guards result:")
			}
		})
	}
}
