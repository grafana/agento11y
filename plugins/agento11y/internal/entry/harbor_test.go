package entry

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
)

// TestRun_HarborDispatch covers the bridge's own exit path, which bypasses the
// launchers map and so needs the same child-exit handling separately.
func TestRun_HarborDispatch(t *testing.T) {
	exitPtr := func(c int) *int { return &c }

	cases := []struct {
		name       string
		runErr     error
		wantExit   *int
		wantStderr string
	}{
		{name: "bridge success exits normally"},
		{name: "bridge exit code is propagated", runErr: fmt.Errorf("exec agento11y-harbor: %w", &launcher.ExitError{Code: 42}), wantExit: exitPtr(42)},
		{name: "bridge exit 2 is not a usage error", runErr: &launcher.ExitError{Code: 2}, wantExit: exitPtr(2)},
		{name: "start failure is reported", runErr: errors.New("Harbor integration is not installed"), wantExit: exitPtr(1), wantStderr: "agento11y: Harbor integration is not installed\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			var gotArgs []string
			prev := harborRun
			t.Cleanup(func() { harborRun = prev })
			harborRun = func(args []string, _ io.Writer) error {
				gotArgs = args
				return tc.runErr
			}

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{"harbor", "export", "jobs/run-1"}, strings.NewReader(""), &stdout, &stderr)
			})

			switch {
			case tc.wantExit == nil && gotExit != nil:
				t.Fatalf("exit = %d, want no exit (stderr=%q)", *gotExit, stderr.String())
			case tc.wantExit != nil && (gotExit == nil || *gotExit != *tc.wantExit):
				t.Fatalf("exit = %v, want %d (stderr=%q)", gotExit, *tc.wantExit, stderr.String())
			}
			if want := []string{"export", "jobs/run-1"}; !slices.Equal(gotArgs, want) {
				t.Errorf("bridge args = %q, want %q", gotArgs, want)
			}
			if stderr.String() != tc.wantStderr {
				t.Errorf("stderr = %q, want %q", stderr.String(), tc.wantStderr)
			}
		})
	}
}
