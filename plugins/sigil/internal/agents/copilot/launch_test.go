package copilot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLaunch(t *testing.T) {
	const binPath = "/usr/local/bin/copilot"
	pluginInstalledOut := []byte("Installed plugins:\n  • sigil-copilot (v0.1.0)\n")
	pluginOtherOut := []byte("Installed plugins:\n  • other-plugin (v1.0.0)\n")
	pluginEmptyOut := []byte("Installed plugins:\n")

	cases := []struct {
		name string

		lookPath   func(string) (string, error)
		pluginList func(context.Context, string) ([]byte, error)
		runInstall func(context.Context, string, io.Writer) error // nil = success
		execFn     func(string, []string, []string) error         // nil = success
		args       []string

		wantErr      string   // substring; "" means no error
		wantInstall  int      // expected runInstall call count
		wantExec     bool     // whether execFn must be called
		wantExecArgv []string // nil = don't assert
		wantStderr   []string // substrings that must appear in stderr
		wantLog      []string // substrings that must appear in the logger output
	}{
		{
			name:     "missing copilot binary",
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			wantErr:  "copilot CLI not found",
		},
		{
			name:         "skips install when plugin installed and forwards args",
			lookPath:     func(string) (string, error) { return binPath, nil },
			pluginList:   func(context.Context, string) ([]byte, error) { return pluginInstalledOut, nil },
			args:         []string{"exec", "hi"},
			wantInstall:  0,
			wantExec:     true,
			wantExecArgv: []string{binPath, "exec", "hi"},
		},
		{
			name:        "runs install when plugin missing",
			lookPath:    func(string) (string, error) { return binPath, nil },
			pluginList:  func(context.Context, string) ([]byte, error) { return pluginOtherOut, nil },
			wantInstall: 1,
			wantExec:    true,
			wantStderr:  []string{"registering " + PluginName + " with copilot"},
		},
		{
			name:        "runs install when plugin list probe fails",
			lookPath:    func(string) (string, error) { return binPath, nil },
			pluginList:  func(context.Context, string) ([]byte, error) { return nil, errors.New("probe boom") },
			wantInstall: 1,
			wantExec:    true,
			wantLog:     []string{"probe boom"},
		},
		{
			name:        "continues when install fails",
			lookPath:    func(string) (string, error) { return binPath, nil },
			pluginList:  func(context.Context, string) ([]byte, error) { return pluginEmptyOut, nil },
			runInstall:  func(context.Context, string, io.Writer) error { return errors.New("network down") },
			wantInstall: 1,
			wantExec:    true,
			wantStderr: []string{
				"install of " + PluginName + " failed",
				"network down",
				"copilot plugin install grafana/sigil-sdk:plugins/copilot",
			},
		},
		{
			name:       "exec failure surfaces error",
			lookPath:   func(string) (string, error) { return binPath, nil },
			pluginList: func(context.Context, string) ([]byte, error) { return pluginInstalledOut, nil },
			execFn:     func(string, []string, []string) error { return errors.New("exec boom") },
			wantExec:   true,
			wantErr:    "exec copilot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			withLookPath(t, tc.lookPath)

			listFn := tc.pluginList
			if listFn == nil {
				listFn = func(context.Context, string) ([]byte, error) {
					t.Fatal("pluginList must not be called")
					return nil, nil
				}
			}
			withPluginList(t, listFn)

			installFn := tc.runInstall
			if installFn == nil {
				installFn = func(context.Context, string, io.Writer) error { return nil }
			}
			installCalls := 0
			withRunInstall(t, func(ctx context.Context, bin string, w io.Writer) error {
				installCalls++
				if bin != binPath {
					t.Errorf("install bin = %q, want %q", bin, binPath)
				}
				return installFn(ctx, bin, w)
			})

			execMock := tc.execFn
			if execMock == nil {
				execMock = func(string, []string, []string) error { return nil }
			}
			var execArgv []string
			execCalled := false
			withExecFn(t, func(p string, argv []string, env []string) error {
				execCalled = true
				execArgv = append([]string{}, argv...)
				return execMock(p, argv, env)
			})

			var stderr bytes.Buffer
			var logbuf bytes.Buffer
			logger := log.New(&logbuf, "", 0)

			err := Launch(context.Background(), tc.args, nil, strings.NewReader(""), io.Discard, &stderr, logger, "dev")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.wantInstall, installCalls)
			assert.Equal(t, tc.wantExec, execCalled)
			if tc.wantExecArgv != nil {
				assert.Equal(t, tc.wantExecArgv, execArgv)
			}
			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}
			for _, want := range tc.wantLog {
				assert.Contains(t, logbuf.String(), want)
			}
		})
	}
}

func TestLaunch_LocalEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		presetMode string
		wantMode   string
	}{
		{name: "defaults full capture", wantMode: "full"},
		{name: "preserves user capture mode", presetMode: "metadata_only", wantMode: "metadata_only"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.presetMode)

			withLookPath(t, func(string) (string, error) { return "/usr/local/bin/copilot", nil })
			withPluginList(t, func(context.Context, string) ([]byte, error) {
				return []byte("Installed plugins:\n  • sigil-copilot (v0.1.0)\n"), nil
			})
			withRunInstall(t, func(context.Context, string, io.Writer) error {
				t.Fatal("runInstall must not be called when plugin is installed")
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

func TestPluginInstalled_ParsesPluginListOutput(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want bool
	}{
		{
			name: "real direct-install output",
			out:  "Installed plugins:\n  • sigil-copilot (v0.1.0)\n",
			want: true,
		},
		{
			name: "header line only",
			out:  "Installed plugins:\n",
			want: false,
		},
		{
			name: "empty",
			out:  "",
			want: false,
		},
		{
			name: "other plugin",
			out:  "Installed plugins:\n  • other-plugin (v1.0.0)\n",
			want: false,
		},
		{
			name: "prefix collision",
			out:  "Installed plugins:\n  • my-sigil-copilot (v0.1.0)\n",
			want: false,
		},
		{
			name: "suffix collision",
			out:  "Installed plugins:\n  • sigil-copilot-staging (v0.1.0)\n",
			want: false,
		},
		{
			name: "bare bullet line",
			out:  "Installed plugins:\n  •\n",
			want: false,
		},
		{
			name: "sigil-copilot among other plugins",
			out:  "Installed plugins:\n  • other (v1.0.0)\n  • sigil-copilot (v0.1.0)\n",
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withPluginList(t, func(context.Context, string) ([]byte, error) {
				return []byte(tc.out), nil
			})
			got, err := pluginInstalled(context.Background(), "/usr/local/bin/copilot")
			require.NoError(t, err)
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func launchWithLogger(t *testing.T, args []string, stderr io.Writer, logger *log.Logger) error {
	t.Helper()
	return Launch(context.Background(), args, nil, strings.NewReader(""), io.Discard, stderr, logger, "dev")
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

func nopLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

func withRunUpdate(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	runUpdate = fn
}

func TestLaunch_RefreshesInstalledPlugin(t *testing.T) {
	const binPath = "/usr/local/bin/copilot"
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	withLookPath(t, func(string) (string, error) { return binPath, nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("Installed plugins:\n  • sigil-copilot (v0.1.0)\n"), nil
	})
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already installed")
		return nil
	})

	updateCalls := 0
	withRunUpdate(t, func(_ context.Context, bin string, _ io.Writer) error {
		updateCalls++
		if bin != binPath {
			t.Errorf("update bin = %q", bin)
		}
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	require.NoError(t, launchWithLogger(t, nil, &stderr, log.New(io.Discard, "", 0)))
	if updateCalls != 1 {
		t.Fatalf("runUpdate calls = %d, want 1", updateCalls)
	}
	if !strings.Contains(stderr.String(), "refreshing "+PluginName+" in copilot") {
		t.Fatalf("stderr missing refresh message: %q", stderr.String())
	}
	stamp := filepath.Join(state, "sigil", "update-checks", PluginName+".stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("expected update stamp: %v", err)
	}
}
