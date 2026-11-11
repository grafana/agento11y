package entry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_HarborHTTPScopedSetup(t *testing.T) {
	for _, tt := range []struct {
		name, protocol string
		credentials    bool
	}{
		{name: "default first run"},
		{name: "explicit OTel still uses HTTP setup", protocol: "otel"},
		{name: "explicit HTTP", protocol: "http"},
		{name: "API-only default config is ready", credentials: true},
		{name: "API-only explicit OTel config is ready", protocol: "otel", credentials: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("AGENTO11Y_PROTOCOL", tt.protocol)
			if tt.credentials {
				t.Setenv("AGENTO11Y_ENDPOINT", "https://api.example.com")
				t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant")
				t.Setenv("AGENTO11Y_AUTH_TOKEN", "token")
			}
			logins := 0
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				logins++
				assert.Equal(t, agento11y.GenerationExportProtocolHTTP, opts.GenerationProtocol)
				assert.False(t, opts.OfferLocal)
				assert.False(t, opts.OfferLocalDaemon)
				return login.Result{}, login.ErrNotInteractive
			})
			previous := harborRun
			t.Cleanup(func() { harborRun = previous })
			launches := 0
			harborRun = func([]string, io.Writer) error {
				launches++
				assert.Equal(t, tt.protocol, envconfig.Getenv("PROTOCOL"))
				return nil
			}
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run([]string{"harbor", "export", "job"}, strings.NewReader(""), &stdout, &stderr) })
			require.Nil(t, code, "stderr=%s", stderr.String())
			if tt.credentials {
				assert.Zero(t, logins)
			} else {
				assert.Equal(t, 1, logins)
			}
			assert.Equal(t, 1, launches)
		})
	}
}

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
