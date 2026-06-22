package codex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunch_MissingCodexBinary(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
	if err == nil || !strings.Contains(err.Error(), "codex CLI not found") {
		t.Fatalf("err = %v, want contains \"codex CLI not found\"", err)
	}
}

func TestLaunch_SkipsInstallWhenPluginInstalledAndEnabled(t *testing.T) {
	t.Setenv("SIGIL_AUTO_UPDATE", "false")
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is installed and enabled")
		return nil
	})

	var execArgv []string
	withExecFn(t, func(_ string, argv []string, _ []string) error {
		execArgv = append([]string{}, argv...)
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), []string{"exec", "hi"}, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !reflect.DeepEqual(execArgv, []string{"/usr/local/bin/codex", "exec", "hi"}) {
		t.Fatalf("exec argv = %v", execArgv)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr non-empty: %q", stderr.String())
	}
}

func TestLaunch_RunsInstallWhenPluginMissing(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  other-plugin@elsewhere (installed, enabled)\n"), nil
	})

	installCalls := 0
	withRunInstall(t, func(_ context.Context, bin string, _ io.Writer) error {
		installCalls++
		if bin != "/usr/local/bin/codex" {
			t.Errorf("install bin = %q, want /usr/local/bin/codex", bin)
		}
		return nil
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1", installCalls)
	}
	if !execCalled {
		t.Fatal("execFn was not called after install")
	}
	got := stderr.String()
	if !strings.Contains(got, "registering "+PluginName+" with codex") {
		t.Fatalf("stderr missing install message: %q", got)
	}
	if !strings.Contains(got, "/hooks") {
		t.Fatalf("stderr missing /hooks trust hint: %q", got)
	}
}

func TestLaunch_RunsInstallWhenPluginInstalledButDisabled(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  sigil-codex@grafana-sigil (installed, disabled)\n"), nil
	})

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1 (disabled plugin should trigger install)", installCalls)
	}
	if !execCalled {
		t.Fatal("execFn was not called")
	}
}

// A failed install must not block the user. The launcher must print the
// failure and a manual-retry hint listing the correct codex verbs on stderr,
// then still exec codex so the workflow keeps moving.
func TestLaunch_InstallFailureContinuesToExec(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte(""), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		return errors.New("network down")
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	var stderr bytes.Buffer
	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if !execCalled {
		t.Fatal("execFn was not called after install failure")
	}
	got := stderr.String()
	if !strings.Contains(got, "install of "+PluginName+" failed") {
		t.Fatalf("stderr missing failure message: %q", got)
	}
	if !strings.Contains(got, "network down") {
		t.Fatalf("stderr missing underlying error: %q", got)
	}
	if !strings.Contains(got, "codex plugin marketplace add grafana/sigil-sdk") {
		t.Fatalf("stderr missing marketplace add hint: %q", got)
	}
	if !strings.Contains(got, "codex plugin add sigil-codex@grafana-sigil") {
		t.Fatalf("stderr missing plugin add hint: %q", got)
	}
	// The /hooks trust hint should NOT appear when install failed.
	if strings.Contains(got, "/hooks") {
		t.Fatalf("stderr unexpectedly contains /hooks hint on failure: %q", got)
	}
}

func TestLaunch_PluginListProbeFailureFallsThroughToInstall(t *testing.T) {
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return nil, errors.New("probe boom")
	})

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})

	execCalled := false
	withExecFn(t, func(_ string, _ []string, _ []string) error {
		execCalled = true
		return nil
	})

	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)
	if err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, logger, "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	if installCalls != 1 {
		t.Fatalf("runInstall calls = %d, want 1 (probe failure should trigger install)", installCalls)
	}
	if !execCalled {
		t.Fatal("execFn was not called")
	}
	if !strings.Contains(logbuf.String(), "probe boom") {
		t.Fatalf("logger missing probe failure: %q", logbuf.String())
	}
}

func TestLaunch_ExecFailureSurfacesError(t *testing.T) {
	t.Setenv("SIGIL_AUTO_UPDATE", "false")
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error { return nil })
	withExecFn(t, func(string, []string, []string) error {
		return errors.New("exec boom")
	})

	err := Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
	if err == nil || !strings.Contains(err.Error(), "exec codex") {
		t.Fatalf("err = %v, want contains \"exec codex\"", err)
	}
}

func TestLaunch_LocalEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		presetMode string
		wantMode   string
	}{
		{name: "defaults full capture", wantMode: "full"},
		{name: "forces full even when a capture mode is set", presetMode: "metadata_only", wantMode: "full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.presetMode)

			withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
			withPluginList(t, func(context.Context, string) ([]byte, error) {
				return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
			})
			withRunInstall(t, func(context.Context, string, io.Writer) error {
				t.Fatal("runInstall must not be called when plugin is installed and enabled")
				return nil
			})

			var execEnv []string
			withExecFn(t, func(_ string, _ []string, env []string) error {
				execEnv = append([]string{}, env...)
				return nil
			})

			localEnv := &local.LaunchEnv{Endpoint: "http://127.0.0.1:9000", OTLPEndpoint: "http://127.0.0.1:9000/otlp"}
			err := Launch(context.Background(), []string{"exec", "hi"}, localEnv, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
			require.NoError(t, err)
			got := envMap(execEnv)
			assert.Equal(t, "http://127.0.0.1:9000", got["SIGIL_ENDPOINT"])
			assert.Equal(t, "http://127.0.0.1:9000/otlp", got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"])
			assert.Equal(t, "local", got["SIGIL_AUTH_TENANT_ID"])
			assert.Equal(t, "local", got["SIGIL_AUTH_TOKEN"])
			assert.Equal(t, tc.wantMode, got["SIGIL_CONTENT_CAPTURE_MODE"])
		})
	}
}

func TestLaunch_ForwardsArgvUnchanged(t *testing.T) {
	t.Setenv("SIGIL_AUTO_UPDATE", "false")
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error { return nil })

	var execArgv []string
	withExecFn(t, func(_ string, argv []string, _ []string) error {
		execArgv = append([]string{}, argv...)
		return nil
	})

	args := []string{"exec", "--model", "gpt-5", "hi"}
	if err := Launch(context.Background(), args, nil, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev"); err != nil {
		t.Fatalf("Launch returned err: %v", err)
	}
	want := append([]string{"/usr/local/bin/codex"}, args...)
	if !reflect.DeepEqual(execArgv, want) {
		t.Fatalf("exec argv = %v, want %v", execArgv, want)
	}
}

func TestPluginInstalled_ParsesPluginListOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "installed enabled",
			out:  "  sigil-codex@grafana-sigil (installed, enabled)\n",
			want: true,
		},
		{
			name: "installed disabled",
			out:  "  sigil-codex@grafana-sigil (installed, disabled)\n",
			want: false,
		},
		{
			name: "not installed",
			out:  "  sigil-codex@grafana-sigil (not installed)\n",
			want: false,
		},
		{
			name: "empty",
			out:  "",
			want: false,
		},
		{
			name: "other plugins only",
			out:  "  other@somewhere (installed, enabled)\n",
			want: false,
		},
		{
			name: "mixed lines",
			out:  "  other@somewhere (installed, disabled)\n  sigil-codex@grafana-sigil (installed, enabled)\n",
			want: true,
		},
		{
			name: "name prefix collision",
			out:  "  my-sigil-codex@grafana-sigil (installed, enabled)\n",
			want: false,
		},
		{
			name: "alias suffix collision",
			out:  "  sigil-codex@grafana-sigil-staging (installed, enabled)\n",
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withPluginList(t, func(context.Context, string) ([]byte, error) {
				return []byte(tc.out), nil
			})
			got, err := pluginInstalled(context.Background(), "/usr/local/bin/codex")
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func withLookPath(t *testing.T, fn func(string) (string, error)) {
	t.Helper()
	prev := lookPath
	t.Cleanup(func() { lookPath = prev })
	lookPath = fn
}

func withRunInstall(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runInstall
	t.Cleanup(func() { runInstall = prev })
	runInstall = fn
}

func withExecFn(t *testing.T, fn func(string, []string, []string) error) {
	t.Helper()
	prev := execFn
	t.Cleanup(func() { execFn = prev })
	execFn = fn
}

func withPluginList(t *testing.T, fn func(context.Context, string) ([]byte, error)) {
	t.Helper()
	prev := pluginList
	t.Cleanup(func() { pluginList = prev })
	pluginList = fn
}

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

func withRunUpdate(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	runUpdate = fn
}

func launch(t *testing.T, args []string, stdout, stderr io.Writer) error {
	t.Helper()
	return launchWithLogger(t, args, stdout, stderr, nopLogger())
}

func launchWithLogger(t *testing.T, args []string, stdout, stderr io.Writer, logger *log.Logger) error {
	t.Helper()
	return Launch(context.Background(), args, nil, strings.NewReader(""), stdout, stderr, logger, "dev")
}

func launchOK(t *testing.T, args []string, stdout, stderr io.Writer) {
	t.Helper()
	require.NoError(t, launch(t, args, stdout, stderr))
}

func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func TestLaunch_RefreshesInstalledPlugin(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/codex", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already installed")
		return nil
	})

	updateCalls := 0
	withRunUpdate(t, func(_ context.Context, bin string, _ io.Writer) error {
		updateCalls++
		if bin != "/usr/local/bin/codex" {
			t.Errorf("update bin = %q", bin)
		}
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	launchOK(t, nil, io.Discard, &stderr)
	if updateCalls != 1 {
		t.Fatalf("runUpdate calls = %d, want 1", updateCalls)
	}
	if !strings.Contains(stderr.String(), "refreshing "+PluginName+" in codex") {
		t.Fatalf("stderr missing refresh message: %q", stderr.String())
	}
	stamp := filepath.Join(state, "sigil", "update-checks", PluginName+".stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("expected update stamp: %v", err)
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name          string
		lookPath      func(string) (string, error)
		pluginList    func(context.Context, string) ([]byte, error)
		wantInstalled bool
		wantErr       bool
	}{
		{
			name:     "installed",
			lookPath: func(string) (string, error) { return "/usr/local/bin/codex", nil },
			pluginList: func(context.Context, string) ([]byte, error) {
				return []byte("  sigil-codex@grafana-sigil (installed, enabled)\n"), nil
			},
			wantInstalled: true,
		},
		{
			name:     "not on path",
			lookPath: func(string) (string, error) { return "", errors.New("not found") },
			wantErr:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withLookPath(t, tc.lookPath)
			if tc.pluginList != nil {
				withPluginList(t, tc.pluginList)
			}

			installed, version, err := Status(context.Background())
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, tc.wantInstalled, installed)
			require.Empty(t, version) // codex plugin list does not expose a version
		})
	}
}
