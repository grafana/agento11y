package copilot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/execpath"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain redirects user-level Copilot hooks writes to a throwaway COPILOT_HOME
// so tests exercising the install path never touch the developer's real
// ~/.copilot. Individual tests override COPILOT_HOME with their own temp dir.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "sigil-copilot-hooks-test-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("COPILOT_HOME", tmp)
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestLaunch(t *testing.T) {
	const binPath = "/usr/local/bin/copilot"
	pluginInstalledOut := []byte("Installed plugins:\n  • sigil-copilot (v0.2.0)\n")
	pluginOtherOut := []byte("Installed plugins:\n  • other-plugin (v1.0.0)\n")

	cases := []struct {
		name string

		lookPath     func(string) (string, error)
		pluginList   func(context.Context, string) ([]byte, error)
		runUninstall func(context.Context, string, io.Writer) error // nil = success
		execFn       func(string, []string, []string) error         // nil = success
		args         []string

		wantErr       string   // substring; "" means no error
		wantUninstall int      // expected runUninstall call count
		wantExec      bool     // whether execFn must be called
		wantExecArgv  []string // nil = don't assert
		wantStderr    []string // substrings that must appear in stderr
		wantLog       []string // substrings that must appear in the logger output
	}{
		{
			name:     "missing copilot binary still surfaces error",
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			wantErr:  "copilot CLI not found",
		},
		{
			name:          "no plugin installed forwards args without uninstall",
			lookPath:      func(string) (string, error) { return binPath, nil },
			pluginList:    func(context.Context, string) ([]byte, error) { return pluginOtherOut, nil },
			args:          []string{"exec", "hi"},
			wantUninstall: 0,
			wantExec:      true,
			wantExecArgv:  []string{binPath, "exec", "hi"},
		},
		{
			name:          "stale plugin is uninstalled then execs",
			lookPath:      func(string) (string, error) { return binPath, nil },
			pluginList:    func(context.Context, string) ([]byte, error) { return pluginInstalledOut, nil },
			wantUninstall: 1,
			wantExec:      true,
			wantStderr:    []string{"removing the legacy " + PluginName + " plugin"},
		},
		{
			name:       "plugin list probe failure still attempts best-effort uninstall",
			lookPath:   func(string) (string, error) { return binPath, nil },
			pluginList: func(context.Context, string) ([]byte, error) { return nil, errors.New("probe boom") },
			// The probe can't confirm the plugin state, but the shared hooks
			// file is already written, so we must still try to remove a
			// possible leftover plugin to avoid double-firing.
			wantUninstall: 1,
			wantExec:      true,
			wantLog:       []string{"probe boom"},
		},
		{
			name:          "probe failure with failing best-effort uninstall stays quiet and non-fatal",
			lookPath:      func(string) (string, error) { return binPath, nil },
			pluginList:    func(context.Context, string) ([]byte, error) { return nil, errors.New("probe boom") },
			runUninstall:  func(context.Context, string, io.Writer) error { return errors.New("not installed") },
			wantUninstall: 1,
			wantExec:      true,
			// Best-effort cleanup must not surface the alarming manual-removal
			// guidance: when the probe failed the plugin may simply be absent.
			wantLog: []string{"best-effort uninstall " + PluginName},
		},
		{
			name:          "continues when uninstall fails",
			lookPath:      func(string) (string, error) { return binPath, nil },
			pluginList:    func(context.Context, string) ([]byte, error) { return pluginInstalledOut, nil },
			runUninstall:  func(context.Context, string, io.Writer) error { return errors.New("network down") },
			wantUninstall: 1,
			wantExec:      true,
			wantStderr: []string{
				"could not remove the " + PluginName + " plugin",
				"network down",
				"copilot plugin uninstall " + PluginName,
			},
		},
		{
			name:       "exec failure surfaces error",
			lookPath:   func(string) (string, error) { return binPath, nil },
			pluginList: func(context.Context, string) ([]byte, error) { return pluginOtherOut, nil },
			execFn:     func(string, []string, []string) error { return errors.New("exec boom") },
			wantExec:   true,
			wantErr:    "exec copilot",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withLookPath(t, tc.lookPath)

			listFn := tc.pluginList
			if listFn == nil {
				listFn = func(context.Context, string) ([]byte, error) {
					t.Fatal("pluginList must not be called")
					return nil, nil
				}
			}
			withPluginList(t, listFn)

			uninstallFn := tc.runUninstall
			if uninstallFn == nil {
				uninstallFn = func(context.Context, string, io.Writer) error { return nil }
			}
			uninstallCalls := 0
			withRunUninstall(t, func(ctx context.Context, bin string, w io.Writer) error {
				uninstallCalls++
				if bin != binPath {
					t.Errorf("uninstall bin = %q, want %q", bin, binPath)
				}
				return uninstallFn(ctx, bin, w)
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
			assert.Equal(t, tc.wantUninstall, uninstallCalls)
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
		{name: "forces full even when a capture mode is set", presetMode: "metadata_only", wantMode: "full"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.presetMode)

			withLookPath(t, func(string) (string, error) { return "/usr/local/bin/copilot", nil })
			withPluginList(t, func(context.Context, string) ([]byte, error) {
				return []byte("Installed plugins:\n  • other-plugin (v1.0.0)\n"), nil
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

func withRunUninstall(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runUninstall
	t.Cleanup(func() { runUninstall = prev })
	runUninstall = fn
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

// userHooksPath returns the sigil.json path under the test's COPILOT_HOME.
func userHooksPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(os.Getenv("COPILOT_HOME"), "hooks", "sigil.json")
}

// withExecutable pins the executable path hook commands are built from, so
// tests can assert the exact generated command line.
func withExecutable(t *testing.T, path string) {
	t.Helper()
	prev := execpath.Executable
	t.Cleanup(func() { execpath.Executable = prev })
	execpath.Executable = func() (string, error) { return path, nil }
}

// assertValidUserHooks checks the written file is the expected Copilot hooks
// document: version 1, every wired event, and the shared executable-path
// hook command carrying its event env var.
func assertValidUserHooks(t *testing.T, path, wantCommand string) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)

	var f struct {
		Version int `json:"version"`
		Hooks   map[string][]struct {
			Hooks []struct {
				Type    string            `json:"type"`
				Command string            `json:"command"`
				Env     map[string]string `json:"env"`
				Timeout int               `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	require.NoError(t, json.Unmarshal(data, &f), "written hooks file must be valid JSON")
	assert.Equal(t, 1, f.Version)

	wantEvents := []string{
		"sessionStart", "sessionEnd", "userPromptSubmitted", "preToolUse",
		"postToolUse", "postToolUseFailure", "errorOccurred", "subagentStart",
		"subagentStop", "agentStop",
	}
	for _, ev := range wantEvents {
		groups, ok := f.Hooks[ev]
		require.Truef(t, ok, "missing event %q", ev)
		require.Len(t, groups, 1)
		require.Len(t, groups[0].Hooks, 1)
		cmd := groups[0].Hooks[0]
		assert.Equal(t, "command", cmd.Type)
		assert.Equal(t, wantCommand, cmd.Command)
		assert.Equal(t, ev, cmd.Env["SIGIL_COPILOT_HOOK_EVENT"])
		// The shared user file must NOT pin a surface — runtime detection
		// distinguishes VS Code from the copilot CLI for this same file.
		assert.Empty(t, cmd.Env["SIGIL_COPILOT_HOOK_SURFACE"])
	}
	assert.Equal(t, 30, f.Hooks["agentStop"][0].Hooks[0].Timeout)
}

// When the copilot CLI is missing, the launcher cannot start a session, but it
// must still install the shared user-level hooks (read by Copilot in VS Code)
// before surfacing the not-found error.
func TestLaunch_MissingBinaryInstallsUserHooks(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, "/usr/local/bin/agento11y")
	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

	var stderr bytes.Buffer
	err := launchWithLogger(t, nil, &stderr, nopLogger())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "copilot CLI not found")

	assertValidUserHooks(t, userHooksPath(t), "/usr/local/bin/agento11y copilot hook")
	assert.Contains(t, stderr.String(), "installed Copilot hooks at")
}

// The shared hooks file must be installed and KEPT even when copilot is present
// and a stale plugin is uninstalled — VS Code relies on it and the CLI reads
// the same file, so it is the single source of truth.
func TestLaunch_InstallsAndKeepsUserHooks(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, "/usr/local/bin/agento11y")
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/copilot", nil })
	withPluginList(t, func(context.Context, string) ([]byte, error) {
		return []byte("Installed plugins:\n  • sigil-copilot (v0.2.0)\n"), nil
	})
	uninstalled := false
	withRunUninstall(t, func(context.Context, string, io.Writer) error {
		uninstalled = true
		return nil
	})
	execCalled := false
	withExecFn(t, func(string, []string, []string) error {
		execCalled = true
		return nil
	})

	require.NoError(t, launchWithLogger(t, nil, io.Discard, nopLogger()))
	assert.True(t, execCalled, "should exec copilot")
	assert.True(t, uninstalled, "stale plugin should be uninstalled")
	// The shared file must remain after the run.
	assertValidUserHooks(t, userHooksPath(t), "/usr/local/bin/agento11y copilot hook")
}

// A hooks file written by an older version with the literal `sigil copilot
// hook` command must be replaced with the executable-path form, in place and
// without duplicate entries (the whole file is sigil-owned).
func TestLaunch_ReplacesLegacyLiteralHookCommand(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, "/usr/local/bin/agento11y")
	legacy, err := renderUserHooks("sigil copilot hook")
	require.NoError(t, err)
	dir := filepath.Join(os.Getenv("COPILOT_HOME"), "hooks")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "sigil.json"), legacy, 0o644))

	withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })
	err = launchWithLogger(t, nil, io.Discard, nopLogger())
	require.Error(t, err, "copilot binary is absent; hooks must still be refreshed")

	assertValidUserHooks(t, userHooksPath(t), "/usr/local/bin/agento11y copilot hook")
	data, err := os.ReadFile(userHooksPath(t))
	require.NoError(t, err)
	assert.NotContains(t, string(data), `"sigil copilot hook"`)
}

// The generated hook command must shell-quote executable paths a shell would
// otherwise split or interpret.
func TestWriteUserHooks_QuotesExecutablePath(t *testing.T) {
	t.Setenv("COPILOT_HOME", t.TempDir())
	withExecutable(t, "/Users/Jane Doe/bin/agento11y")

	path, wrote, err := writeUserHooks()
	require.NoError(t, err)
	assert.True(t, wrote)
	assertValidUserHooks(t, path, "'/Users/Jane Doe/bin/agento11y' copilot hook")
}

func TestRenderUserHooks_IsStableAndValid(t *testing.T) {
	const command = "/usr/local/bin/agento11y copilot hook"
	a, err := renderUserHooks(command)
	require.NoError(t, err)
	b, err := renderUserHooks(command)
	require.NoError(t, err)
	assert.Equal(t, a, b, "render must be deterministic for idempotent writes")
	assert.True(t, json.Valid(a))
	assert.Contains(t, string(a), `"/usr/local/bin/agento11y copilot hook"`)
	// The shared file must not pin a surface marker.
	assert.NotContains(t, string(a), "SIGIL_COPILOT_HOOK_SURFACE")
}

func TestParsePluginListStatus_Version(t *testing.T) {
	cases := []struct {
		name          string
		out           string
		wantInstalled bool
		wantVersion   string
	}{
		{name: "with version", out: "Installed plugins:\n  • sigil-copilot (v0.1.0)\n", wantInstalled: true, wantVersion: "0.1.0"},
		{name: "no parenthesised version", out: "Installed plugins:\n  • sigil-copilot\n", wantInstalled: true, wantVersion: ""},
		{name: "absent", out: "Installed plugins:\n", wantInstalled: false, wantVersion: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			installed, version := parsePluginListStatus([]byte(tc.out))
			require.Equal(t, tc.wantInstalled, installed)
			require.Equal(t, tc.wantVersion, version)
		})
	}
}

// Status detects the shared hooks file (capture is hook-based, not plugin
// based), so it never consults `plugin list` or PATH.
func TestStatus(t *testing.T) {
	tests := []struct {
		name          string
		writeHooks    bool
		wantInstalled bool
	}{
		{name: "hooks file present", writeHooks: true, wantInstalled: true},
		{name: "hooks file absent", writeHooks: false, wantInstalled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("COPILOT_HOME", home)
			if tc.writeHooks {
				dir := filepath.Join(home, "hooks")
				require.NoError(t, os.MkdirAll(dir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(dir, "sigil.json"), []byte("{}"), 0o644))
			}

			installed, version, err := Status(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.wantInstalled, installed)
			require.Empty(t, version, "hooks file carries no version")
		})
	}
}
