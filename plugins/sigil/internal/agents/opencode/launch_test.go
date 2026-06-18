package opencode

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
	const binPath = "/usr/local/bin/opencode"
	configWithPlugin := `{"plugin":["@grafana/sigil-opencode"]}`
	configWithOther := `{"plugin":["other-plugin"]}`
	configMissing := "" // empty string = don't write config file

	cases := []struct {
		name string

		lookPath   func(string) (string, error)
		configBody string                                         // "" means don't write
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
			name:     "missing opencode binary",
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			wantErr:  "opencode CLI not found",
		},
		{
			name:         "skips install when plugin installed and forwards args",
			lookPath:     func(string) (string, error) { return binPath, nil },
			configBody:   configWithPlugin,
			args:         []string{"run", "hi"},
			wantInstall:  0,
			wantExec:     true,
			wantExecArgv: []string{binPath, "run", "hi"},
		},
		{
			name:        "runs install when plugin missing",
			lookPath:    func(string) (string, error) { return binPath, nil },
			configBody:  configWithOther,
			wantInstall: 1,
			wantExec:    true,
			wantStderr:  []string{"installing " + PluginSource + " into opencode"},
		},
		{
			name:        "runs install when config file missing",
			lookPath:    func(string) (string, error) { return binPath, nil },
			configBody:  configMissing,
			wantInstall: 1,
			wantExec:    true,
		},
		{
			name:        "continues when install fails",
			lookPath:    func(string) (string, error) { return binPath, nil },
			configBody:  configMissing,
			runInstall:  func(context.Context, string, io.Writer) error { return errors.New("network down") },
			wantInstall: 1,
			wantExec:    true,
			wantStderr: []string{
				"install of " + PluginSource + " failed",
				"network down",
				"opencode plugin " + PluginSource + " --global",
			},
		},
		{
			name:       "exec failure surfaces error",
			lookPath:   func(string) (string, error) { return binPath, nil },
			configBody: configWithPlugin,
			execFn:     func(string, []string, []string) error { return errors.New("exec boom") },
			wantExec:   true,
			wantErr:    "exec opencode",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			withConfig(t, tc.configBody)
			withLookPath(t, tc.lookPath)

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

func TestLaunch_InvalidConfigProbeFallsThroughToInstall(t *testing.T) {
	t.Setenv("SIGIL_AUTO_UPDATE", "false")
	withConfig(t, `{"plugin":[`) // truncated JSON
	withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })

	installCalls := 0
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		installCalls++
		return nil
	})
	execCalled := false
	withExecFn(t, func(string, []string, []string) error {
		execCalled = true
		return nil
	})

	var stderr bytes.Buffer
	var logbuf bytes.Buffer
	logger := log.New(&logbuf, "", 0)
	require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, logger, "dev"))

	assert.Equal(t, 1, installCalls, "install must run after probe failure")
	assert.True(t, execCalled, "exec must be called even after probe failure")
	assert.Contains(t, stderr.String(), "opencode config probe failed")
	assert.Contains(t, logbuf.String(), "opencode config probe")
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
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.presetMode)

			withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
			withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
			withRunInstall(t, func(context.Context, string, io.Writer) error {
				t.Fatal("runInstall must not be called when plugin is installed")
				return nil
			})

			var execEnv []string
			var execArgv []string
			withExecFn(t, func(_ string, argv []string, env []string) error {
				execArgv = append([]string{}, argv...)
				execEnv = append([]string{}, env...)
				return nil
			})

			localEnv := &local.LaunchEnv{Endpoint: "http://127.0.0.1:9000", OTLPEndpoint: "http://127.0.0.1:9000/otlp"}
			err := Launch(context.Background(), []string{"run", "hi"}, localEnv, strings.NewReader(""), io.Discard, io.Discard, nopLogger(), "dev")
			require.NoError(t, err)
			assert.Equal(t, []string{"/usr/local/bin/opencode", "run", "hi"}, execArgv)
			got := envMap(execEnv)
			assert.Equal(t, "http://127.0.0.1:9000", got["SIGIL_ENDPOINT"])
			assert.Equal(t, "http://127.0.0.1:9000/otlp", got["SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"])
			assert.Equal(t, "local", got["SIGIL_AUTH_TENANT_ID"])
			assert.Equal(t, "local", got["SIGIL_AUTH_TOKEN"])
			assert.Equal(t, tc.wantMode, got["SIGIL_CONTENT_CAPTURE_MODE"])
		})
	}
}

func TestPluginInstalled_PluginEntryShapes(t *testing.T) {
	cases := []struct {
		name     string
		write    bool
		contents string
		want     bool
		wantErr  bool
	}{
		{
			name:     "bare package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode"]}`,
			want:     true,
		},
		{
			name:     "versioned package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode@0.6.0"]}`,
			want:     true,
		},
		{
			name:     "dist-tag package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode@next"]}`,
			want:     true,
		},
		{
			name:     "tuple form matches",
			write:    true,
			contents: `{"plugin":[["@grafana/sigil-opencode",{"agentName":"opencode"}]]}`,
			want:     true,
		},
		{
			name:     "tuple form with versioned name matches",
			write:    true,
			contents: `{"plugin":[["@grafana/sigil-opencode@0.6.0",{}]]}`,
			want:     true,
		},
		{
			name:     "prefix collision does not match",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode-extra"]}`,
			want:     false,
		},
		{
			name:     "unrelated package does not match",
			write:    true,
			contents: `{"plugin":["other-plugin",["another",{}]]}`,
			want:     false,
		},
		{
			name:     "empty plugin list",
			write:    true,
			contents: `{"plugin":[]}`,
			want:     false,
		},
		{
			name:     "config without plugin field",
			write:    true,
			contents: `{"theme":"dark"}`,
			want:     false,
		},
		{
			name:  "missing file treated as not installed",
			write: false,
			want:  false,
		},
		{
			name:     "malformed json surfaces error",
			write:    true,
			contents: `{"plugin":[`,
			wantErr:  true,
		},
		{
			name:     "tuple with empty array ignored",
			write:    true,
			contents: `{"plugin":[[]]}`,
			want:     false,
		},
		{
			name:     "jsonc trailing comma accepted",
			write:    true,
			contents: "{\n  \"plugin\": [\n    \"@grafana/sigil-opencode\",\n  ],\n}\n",
			want:     true,
		},
		{
			name:     "jsonc line comment accepted",
			write:    true,
			contents: "// global opencode config\n{\n  \"plugin\": [\"@grafana/sigil-opencode\"] // sigil capture\n}\n",
			want:     true,
		},
		{
			name:     "jsonc block comment accepted",
			write:    true,
			contents: "{\n  /* installed by sigil */\n  \"plugin\": [\"@grafana/sigil-opencode\"]\n}\n",
			want:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.write {
				withConfig(t, tc.contents)
			} else {
				withConfig(t, "")
			}
			got, err := pluginInstalled()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPluginInstalled_JsoncFilename(t *testing.T) {
	withConfigFiles(t, map[string]string{
		"opencode.jsonc": "{\n  // sigil\n  \"plugin\": [\"@grafana/sigil-opencode\"],\n}\n",
	})
	got, err := pluginInstalled()
	require.NoError(t, err)
	assert.True(t, got, "plugin in opencode.jsonc must be detected")
}

func TestPluginInstalled_JsonTakesPrecedenceOverJsonc(t *testing.T) {
	// When both files exist, opencode.json wins. Verify we don't fall
	// through to opencode.jsonc and report a stale answer.
	withConfigFiles(t, map[string]string{
		"opencode.json":  `{"plugin":["other-plugin"]}`,
		"opencode.jsonc": `{"plugin":["@grafana/sigil-opencode"]}`,
	})
	got, err := pluginInstalled()
	require.NoError(t, err)
	assert.False(t, got, "opencode.json should take precedence")
}

func TestLaunch_RefreshesInstalledPluginWhenUpdateDue(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("SIGIL_AUTO_UPDATE", "")

	withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
	withLookPath(t, func(string) (string, error) { return binPath, nil })
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
	require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"))
	if updateCalls != 1 {
		t.Fatalf("runUpdate calls = %d, want 1", updateCalls)
	}
	if !strings.Contains(stderr.String(), "refreshing "+PluginSource+" in opencode") {
		t.Fatalf("stderr missing refresh message: %q", stderr.String())
	}
	stamp := filepath.Join(state, "sigil", "update-checks", PluginName+".stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("expected update stamp: %v", err)
	}
}

func TestLaunch_SkipsRefreshWhenUpdateDisabled(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	t.Setenv("SIGIL_AUTO_UPDATE", "false")

	withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
	withLookPath(t, func(string) (string, error) { return binPath, nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when plugin is already installed")
		return nil
	})
	withRunUpdate(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runUpdate must not be called when auto-update is disabled")
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"))
}

func TestDefaultConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/custom/config")
	got, err := defaultConfigDir()
	require.NoError(t, err)
	assert.Equal(t, "/custom/config/opencode", got)

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/home/user")
	got, err = defaultConfigDir()
	require.NoError(t, err)
	assert.Equal(t, "/home/user/.config/opencode", got)
}

// withConfig points configDirFn at a temp directory, optionally
// seeding it with an opencode.json containing the given contents.
// Pass "" to leave the directory empty.
func withConfig(t *testing.T, contents string) {
	t.Helper()
	files := map[string]string{}
	if contents != "" {
		files["opencode.json"] = contents
	}
	withConfigFiles(t, files)
}

// withConfigFiles points configDirFn at a temp directory and writes the
// supplied {basename: contents} files into it.
func withConfigFiles(t *testing.T, files map[string]string) {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	prev := configDirFn
	t.Cleanup(func() { configDirFn = prev })
	configDirFn = func() (string, error) { return dir, nil }
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

func withRunUpdate(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runUpdate
	t.Cleanup(func() { runUpdate = prev })
	runUpdate = fn
}

func withExecFn(t *testing.T, fn func(string, []string, []string) error) {
	t.Helper()
	prev := execFn
	t.Cleanup(func() { execFn = prev })
	execFn = fn
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

func TestVersionFromNpmSpec(t *testing.T) {
	cases := []struct {
		spec string
		want string
	}{
		{spec: "@grafana/sigil-opencode", want: ""},
		{spec: "@grafana/sigil-opencode@0.6.0", want: "0.6.0"},
		{spec: "@grafana/sigil-opencode@next", want: "next"},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			require.Equal(t, tc.want, versionFromNpmSpec(tc.spec))
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name          string
		config        string
		wantInstalled bool
		wantVersion   string
	}{
		{name: "installed", config: `{"plugin":["@grafana/sigil-opencode@0.6.0"]}`, wantInstalled: true, wantVersion: "0.6.0"},
		{name: "not installed", config: `{"plugin":["other-plugin"]}`, wantInstalled: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			withConfig(t, tc.config)

			installed, version, err := Status(context.Background())
			require.NoError(t, err)
			require.Equal(t, tc.wantInstalled, installed)
			require.Equal(t, tc.wantVersion, version)
		})
	}
}
