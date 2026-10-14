package opencode

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tailscale/hujson"
)

// defaultHostVersion is what `opencode --version` prints in a test that does
// not name a release: a supported OpenCode 1, whose behaviour every test
// written before OpenCode 2 support pins.
const defaultHostVersion = "1.18.29\n"

// TestMain keeps the suite away from any real opencode. The developer's PATH
// may hold one, and Status and Prepare would run its `--version`. Tests that
// need a host stub lookPath and, for another release, versionOutput.
func TestMain(m *testing.M) {
	lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
	versionOutput = func(context.Context, string) ([]byte, error) { return []byte(defaultHostVersion), nil }
	os.Exit(m.Run())
}

func TestLaunch(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	configWithPlugin := `{"plugin":["@grafana/agento11y-opencode"]}`
	configWithLegacyPlugin := `{"plugin":["@grafana/sigil-opencode"]}`
	configWithOther := `{"plugin":["other-plugin"]}`
	configMissing := "" // empty string = don't write config file
	configV2WithPlugin := `{"plugins":["@grafana/agento11y-opencode"]}`
	onPath := func(string) (string, error) { return binPath, nil }

	cases := []struct {
		name string

		lookPath     func(string) (string, error)
		version      string                                         // `opencode --version` output; "" = defaultHostVersion
		versionErr   error                                          // makes the version probe fail
		configBody   string                                         // "" means don't write
		configFiles  map[string]string                              // extra global config files by basename
		autoUpdate   string                                         // SIGIL_AUTO_UPDATE; "" = "false"
		runInstall   func(context.Context, string, io.Writer) error // nil = success; serves both releases
		execFn       func(string, []string, []string) error         // nil = success
		args         []string
		localEnv     *local.LaunchEnv
		sameConfig   bool // the config files must be byte-identical afterwards
		wantUpdate   int  // expected runUpdate call count
		wantInstall2 int  // expected OpenCode 2 install call count

		wantErr      string   // substring; "" means no error
		wantErrType  any      // pointer to a typed error errors.As must find, e.g. new(*VersionError)
		wantInstall  int      // expected runInstall call count
		wantExec     bool     // whether execFn must be called
		wantExecArgv []string // nil = don't assert
		wantEnv      map[string]string
		wantStderr   []string // substrings that must appear in stderr
		wantNoStderr []string // substrings that must not appear in stderr
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
			// A config that still references the pre-rename package name counts
			// as installed — reinstalling would register the plugin twice.
			name:         "skips install when legacy plugin name installed",
			lookPath:     func(string) (string, error) { return binPath, nil },
			configBody:   configWithLegacyPlugin,
			wantInstall:  0,
			wantExec:     true,
			wantExecArgv: []string{binPath},
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
		{
			name:         "v1 daily refresh still runs when due",
			lookPath:     onPath,
			configBody:   configWithPlugin,
			autoUpdate:   "true",
			wantUpdate:   1,
			wantExec:     true,
			wantExecArgv: []string{binPath},
			wantStderr:   []string{"refreshing " + PluginSource + " in opencode"},
		},
		{
			name:         "oldest supported v1 release keeps the v1 commands",
			lookPath:     onPath,
			version:      "1.3.13\n",
			configBody:   configMissing,
			runInstall:   func(context.Context, string, io.Writer) error { return errors.New("network down") },
			args:         []string{"run", "hi"},
			wantInstall:  1,
			wantExec:     true,
			wantExecArgv: []string{binPath, "run", "hi"},
			wantStderr:   []string{"opencode plugin " + PluginSource + " --global"},
			wantNoStderr: []string{"plugin add"},
		},

		// OpenCode 2 sessions: install with `plugin add`, never refresh, and get
		// --standalone so the private server inherits the launch environment.
		{
			name:         "v2 TUI installs with plugin add and runs standalone",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configWithOther,
			localEnv:     &local.LaunchEnv{Endpoint: "http://127.0.0.1:9000", OTLPEndpoint: "http://127.0.0.1:9000/otlp"},
			wantInstall2: 1,
			wantExec:     true,
			wantExecArgv: []string{binPath, "--standalone"},
			wantEnv:      map[string]string{"SIGIL_ENDPOINT": "http://127.0.0.1:9000"},
			wantStderr:   []string{"installing " + PluginSource + " into opencode"},
		},
		{
			name:         "oldest supported v2 release",
			lookPath:     onPath,
			version:      "opencode v2.0.22\n",
			configBody:   configV2WithPlugin,
			args:         []string{"run", "hi"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "run", "hi", "--standalone"},
		},
		{
			// effect/cli colors the version line on a terminal.
			name:         "colored v2 version line",
			lookPath:     onPath,
			version:      "\x1b[1mopencode\x1b[0m \x1b[2mv\x1b[0m\x1b[1m2.0.29\x1b[0m\n",
			configBody:   configV2WithPlugin,
			args:         []string{"mini"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "mini", "--standalone"},
		},
		{
			// The legacy key still registers the plugin on v2, so a
			// second entry is never added under `plugins`.
			name:         "v2 counts a legacy plugin key entry as installed",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configWithPlugin,
			wantExec:     true,
			wantExecArgv: []string{binPath, "--standalone"},
		},
		{
			name:         "v2 reads opencode.jsonc next to opencode.json",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   `{"theme":"dark"}`,
			configFiles:  map[string]string{"opencode.jsonc": "{\n  // capture\n  \"plugins\": [{\"package\": \"@grafana/agento11y-opencode@0.24.0\", \"options\": {}}],\n}\n"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "--standalone"},
		},
		{
			name:         "v2 never refreshes, even when a refresh is due",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configV2WithPlugin,
			autoUpdate:   "true",
			wantExec:     true,
			wantNoStderr: []string{"refreshing"},
		},
		{
			name:         "v2 install failure prints the v2 command",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configMissing,
			runInstall:   func(context.Context, string, io.Writer) error { return errors.New("network down") },
			wantInstall2: 1,
			wantExec:     true,
			wantStderr:   []string{"install of " + PluginSource + " failed", "opencode plugin add " + PluginSource + "\n"},
			wantNoStderr: []string{"--global", "--force"},
		},
		{
			// Adding the package again would append it after the user's
			// selector and turn it back on.
			name:         "v2 leaves a plugin the user turned off alone",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   `{"plugins":["@grafana/agento11y-opencode","-grafana.agento11y"]}`,
			sameConfig:   true,
			wantExec:     true,
			wantExecArgv: []string{binPath, "--standalone"},
			wantStderr:   []string{`is turned off by "-grafana.agento11y"`, "runs without agento11y capture"},
		},
		{
			name:         "v2 keeps an explicit standalone",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configV2WithPlugin,
			args:         []string{"run", "--standalone", "hi"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "run", "--standalone", "hi"},
		},

		// OpenCode 2 non-session commands run as typed: no migration, install,
		// refresh, or --standalone.
		{
			name:         "v2 plugin maintenance runs directly",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configWithLegacyPlugin,
			autoUpdate:   "true",
			args:         []string{"plugin", "list"},
			sameConfig:   true,
			wantExec:     true,
			wantExecArgv: []string{binPath, "plugin", "list"},
			wantNoStderr: []string{"installing", "migrating", "refreshing"},
		},
		{
			name:         "v2 help runs directly without an install",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configMissing,
			args:         []string{"run", "--help"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "run", "--help"},
		},
		{
			name:         "v2 serve runs directly",
			lookPath:     onPath,
			version:      "opencode v2.0.25\n",
			configBody:   configMissing,
			args:         []string{"serve", "--port", "4096"},
			wantExec:     true,
			wantExecArgv: []string{binPath, "serve", "--port", "4096"},
		},

		// Refusals happen before anything changes: no migration, install,
		// refresh, or exec.
		{
			name:        "v2 --server is refused",
			lookPath:    onPath,
			version:     "opencode v2.0.25\n",
			configBody:  configWithLegacyPlugin,
			autoUpdate:  "true",
			args:        []string{"run", "--server", "http://127.0.0.1:4096", "hi"},
			sameConfig:  true,
			wantErr:     "Drop --server",
			wantErrType: new(*ArgumentError),
		},
		{
			name:        "v2 negated standalone is refused",
			lookPath:    onPath,
			version:     "opencode v2.0.25\n",
			configBody:  configMissing,
			args:        []string{"--no-standalone"},
			wantErr:     "--no-standalone attaches the session to OpenCode's background service",
			wantErrType: new(*ArgumentError),
		},
		{
			name:        "v2 release before 2.0.22 is refused",
			lookPath:    onPath,
			version:     "opencode v2.0.21\n",
			configBody:  configWithLegacyPlugin,
			autoUpdate:  "true",
			sameConfig:  true,
			wantErr:     "OpenCode 2.0.21 is older than 2.0.22",
			wantErrType: new(*VersionError),
		},
		{
			name:        "v1 release before 1.3.13 is refused",
			lookPath:    onPath,
			version:     "1.3.12\n",
			configBody:  configWithLegacyPlugin,
			sameConfig:  true,
			wantErr:     "OpenCode 1.3.12 is older than 1.3.13",
			wantErrType: new(*VersionError),
		},
		{
			name:        "prerelease is refused",
			lookPath:    onPath,
			version:     "opencode v2.1.0-beta.1\n",
			configBody:  configWithLegacyPlugin,
			sameConfig:  true,
			wantErr:     "OpenCode 2.1.0-beta.1 is not a stable release",
			wantErrType: new(*VersionError),
		},
		{
			name:        "future major is refused",
			lookPath:    onPath,
			version:     "opencode v3.0.0\n",
			configBody:  configMissing,
			wantErr:     "OpenCode 3.0.0 is newer than this agento11y release supports",
			wantErrType: new(*VersionError),
		},
		{
			name:        "source build without a version is refused",
			lookPath:    onPath,
			version:     "opencode vlocal\n",
			configBody:  configMissing,
			wantErr:     `printed "opencode vlocal"`,
			wantErrType: new(*VersionError),
		},
		{
			name:        "failed version probe is refused",
			lookPath:    onPath,
			versionErr:  errors.New("signal: killed"),
			configBody:  configWithLegacyPlugin,
			sameConfig:  true,
			wantErr:     "`opencode --version` failed: signal: killed",
			wantErrType: new(*VersionError),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			autoUpdate := tc.autoUpdate
			if autoUpdate == "" {
				autoUpdate = "false"
			}
			t.Setenv("SIGIL_AUTO_UPDATE", autoUpdate)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			files := map[string]string{}
			if tc.configBody != "" {
				files["opencode.json"] = tc.configBody
			}
			maps.Copy(files, tc.configFiles)
			dir := withConfigFiles(t, files)
			withLookPath(t, tc.lookPath)
			version := tc.version
			if version == "" {
				version = defaultHostVersion
			}
			withVersionOutput(t, version, tc.versionErr)

			installFn := tc.runInstall
			if installFn == nil {
				installFn = func(context.Context, string, io.Writer) error { return nil }
			}
			installCalls, install2Calls, updateCalls := 0, 0, 0
			withRunInstall(t, func(ctx context.Context, bin string, w io.Writer) error {
				installCalls++
				if bin != binPath {
					t.Errorf("install bin = %q, want %q", bin, binPath)
				}
				return installFn(ctx, bin, w)
			})
			withRunInstallV2(t, func(ctx context.Context, bin string, w io.Writer) error {
				install2Calls++
				return installFn(ctx, bin, w)
			})
			withRunUpdate(t, func(context.Context, string, io.Writer) error {
				updateCalls++
				return nil
			})

			execMock := tc.execFn
			if execMock == nil {
				execMock = func(string, []string, []string) error { return nil }
			}
			var execArgv, execEnv []string
			execCalled := false
			withExecFn(t, func(p string, argv []string, env []string) error {
				execCalled = true
				execArgv = append([]string{}, argv...)
				execEnv = append([]string{}, env...)
				return execMock(p, argv, env)
			})

			var stderr bytes.Buffer
			var logbuf bytes.Buffer
			logger := log.New(&logbuf, "", 0)

			err := Launch(context.Background(), tc.args, tc.localEnv, strings.NewReader(""), io.Discard, &stderr, logger, "dev")

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			if tc.wantErrType != nil {
				assert.ErrorAs(t, err, tc.wantErrType)
			}
			assert.Equal(t, tc.wantInstall, installCalls, "v1 install calls")
			assert.Equal(t, tc.wantInstall2, install2Calls, "v2 install calls")
			assert.Equal(t, tc.wantUpdate, updateCalls, "update calls")
			assert.Equal(t, tc.wantExec, execCalled)
			if tc.wantExecArgv != nil {
				assert.Equal(t, tc.wantExecArgv, execArgv)
			}
			gotEnv := envMap(execEnv)
			for key, want := range tc.wantEnv {
				assert.Equal(t, want, gotEnv[key], key)
			}
			for _, want := range tc.wantStderr {
				assert.Contains(t, stderr.String(), want)
			}
			for _, notWant := range tc.wantNoStderr {
				assert.NotContains(t, stderr.String(), notWant)
			}
			for _, want := range tc.wantLog {
				assert.Contains(t, logbuf.String(), want)
			}
			if tc.sameConfig {
				for name, body := range files {
					got, err := os.ReadFile(filepath.Join(dir, name))
					require.NoError(t, err)
					assert.Equal(t, body, string(got), "%s must be unchanged", name)
				}
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

func TestInstall(t *testing.T) {
	t.Run("already installed does not run the installer", func(t *testing.T) {
		withConfig(t, `{"plugin":["@grafana/agento11y-opencode"]}`)
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withRunInstall(t, func(context.Context, string, io.Writer) error {
			t.Fatal("install must not run when the plugin is registered")
			return nil
		})

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		require.NoError(t, err)
		assert.False(t, changed)
	})

	// Which config keys count depends on the release, so a config that
	// already registers the plugin still needs the host to be read; the
	// reconciler defers instead of reporting the wrong release's answer.
	t.Run("already installed still needs the host", func(t *testing.T) {
		dir := withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
		withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		assert.False(t, changed)
		assert.ErrorIs(t, err, ErrCLINotFound)
		got, readErr := os.ReadFile(filepath.Join(dir, "opencode.json"))
		require.NoError(t, readErr)
		assert.Equal(t, `{"plugin":["@grafana/sigil-opencode"]}`, string(got), "no migration without a verified host")
	})

	t.Run("unsupported release is refused before migration or install", func(t *testing.T) {
		dir := withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withVersionOutput(t, "opencode v2.0.10\n", nil)
		for _, seam := range []func(*testing.T, func(context.Context, string, io.Writer) error){withRunInstall, withRunInstallV2} {
			seam(t, func(context.Context, string, io.Writer) error {
				t.Fatal("install must not run for an unsupported release")
				return nil
			})
		}

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		assert.False(t, changed)
		var versionErr *VersionError
		require.ErrorAs(t, err, &versionErr)
		assert.Equal(t, "2.0.10", versionErr.Reported)
		got, readErr := os.ReadFile(filepath.Join(dir, "opencode.json"))
		require.NoError(t, readErr)
		assert.Equal(t, `{"plugin":["@grafana/sigil-opencode"]}`, string(got))
	})

	t.Run("v2 installs with plugin add", func(t *testing.T) {
		withConfig(t, `{"plugin":["other-plugin"]}`)
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withVersionOutput(t, "opencode v2.0.22\n", nil)
		withRunInstall(t, func(context.Context, string, io.Writer) error {
			t.Fatal("the v1 installer must not run on v2")
			return nil
		})
		calls := 0
		withRunInstallV2(t, func(context.Context, string, io.Writer) error {
			calls++
			return nil
		})

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, 1, calls)
	})

	t.Run("v2 migrates a native plugins entry and counts it as installed", func(t *testing.T) {
		dir := withConfigFiles(t, map[string]string{
			"opencode.jsonc": "{\n  // fleet\n  \"plugins\": [{\"package\": \"@grafana/sigil-opencode@0.13.0\", \"options\": {\"agentName\": \"oc\"}}],\n}\n",
		})
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withVersionOutput(t, "opencode v2.0.25\n", nil)
		withRunInstallV2(t, func(context.Context, string, io.Writer) error {
			t.Fatal("the migrated entry counts as installed")
			return nil
		})

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		require.NoError(t, err)
		assert.False(t, changed)
		got, readErr := os.ReadFile(filepath.Join(dir, "opencode.jsonc"))
		require.NoError(t, readErr)
		assert.Contains(t, string(got), "// fleet")
		assert.Contains(t, string(got), `"package": "@grafana/agento11y-opencode"`)
		assert.Contains(t, string(got), `"agentName": "oc"`)
		assert.NotContains(t, string(got), "sigil-opencode")
	})

	t.Run("v2 does not turn a disabled plugin back on", func(t *testing.T) {
		withConfig(t, `{"plugins":["@grafana/agento11y-opencode","-grafana.*"]}`)
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withVersionOutput(t, "opencode v2.0.25\n", nil)
		withRunInstallV2(t, func(context.Context, string, io.Writer) error {
			t.Fatal("install must not re-add a plugin the user turned off")
			return nil
		})

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		require.NoError(t, err)
		assert.False(t, changed)
	})

	t.Run("missing host is reported", func(t *testing.T) {
		withConfig(t, "")
		withLookPath(t, func(string) (string, error) { return "", exec.ErrNotFound })

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		assert.False(t, changed)
		assert.ErrorIs(t, err, ErrCLINotFound)
	})

	t.Run("installs without launching host", func(t *testing.T) {
		withConfig(t, "")
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		calls := 0
		withRunInstall(t, func(_ context.Context, bin string, _ io.Writer) error {
			calls++
			assert.Equal(t, "/usr/local/bin/opencode", bin)
			return nil
		})

		changed, err := Install(context.Background(), io.Discard, nopLogger())
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, 1, calls)
	})

	t.Run("legacy migration failure is reported so reconciliation retries", func(t *testing.T) {
		withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
		withWriteConfig(t, func(string, []byte) error {
			return errors.New("permission denied")
		})
		withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
		withRunInstall(t, func(context.Context, string, io.Writer) error {
			t.Fatal("install must not run after a failed legacy migration")
			return nil
		})

		var output bytes.Buffer
		changed, err := Install(context.Background(), &output, nopLogger())
		assert.False(t, changed)
		assert.ErrorContains(t, err, "migrate legacy OpenCode plugin configuration")
		assert.ErrorContains(t, err, "write")
		assert.Contains(t, output.String(), "To migrate manually")
	})
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
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "")
			t.Setenv("SIGIL_AUTH_TOKEN", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.presetMode)

			withConfig(t, `{"plugin":["@grafana/agento11y-opencode"]}`)
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

		// v2 probes OpenCode 2's registration instead of OpenCode 1's.
		v2             bool
		files          map[string]string // further config files by basename
		wantSource     string            // v2: the spec OpenCode 2 loads
		wantDisabledBy string            // v2: the selector that turned it off
	}{
		{
			name:     "bare package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/agento11y-opencode"]}`,
			want:     true,
		},
		{
			name:     "versioned package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/agento11y-opencode@0.6.0"]}`,
			want:     true,
		},
		{
			name:     "dist-tag package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/agento11y-opencode@next"]}`,
			want:     true,
		},
		{
			name:     "tuple form matches",
			write:    true,
			contents: `{"plugin":[["@grafana/agento11y-opencode",{"agentName":"opencode"}]]}`,
			want:     true,
		},
		{
			name:     "tuple form with versioned name matches",
			write:    true,
			contents: `{"plugin":[["@grafana/agento11y-opencode@0.6.0",{}]]}`,
			want:     true,
		},
		{
			name:     "legacy bare package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode"]}`,
			want:     true,
		},
		{
			name:     "legacy versioned package string matches",
			write:    true,
			contents: `{"plugin":["@grafana/sigil-opencode@0.13.0"]}`,
			want:     true,
		},
		{
			name:     "legacy tuple form matches",
			write:    true,
			contents: `{"plugin":[["@grafana/sigil-opencode",{"agentName":"opencode"}]]}`,
			want:     true,
		},
		{
			name:     "prefix collision does not match",
			write:    true,
			contents: `{"plugin":["@grafana/agento11y-opencode-extra"]}`,
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
			contents: "{\n  \"plugin\": [\n    \"@grafana/agento11y-opencode\",\n  ],\n}\n",
			want:     true,
		},
		{
			name:     "jsonc line comment accepted",
			write:    true,
			contents: "// global opencode config\n{\n  \"plugin\": [\"@grafana/agento11y-opencode\"] // sigil capture\n}\n",
			want:     true,
		},
		{
			name:     "jsonc block comment accepted",
			write:    true,
			contents: "{\n  /* installed by sigil */\n  \"plugin\": [\"@grafana/agento11y-opencode\"]\n}\n",
			want:     true,
		},
		{
			// OpenCode 1 does not read the native key.
			name:     "v1 ignores the native plugins key",
			write:    true,
			contents: `{"plugins":["@grafana/agento11y-opencode"]}`,
			want:     false,
		},

		// OpenCode 2: the legacy `plugin` list, then the native `plugins` list,
		// in opencode.json and then opencode.jsonc.
		{name: "v2 native string", v2: true, write: true, contents: `{"plugins":["@grafana/agento11y-opencode"]}`, want: true, wantSource: "@grafana/agento11y-opencode"},
		{name: "v2 native object with options", v2: true, write: true, contents: `{"plugins":[{"package":"@grafana/agento11y-opencode","options":{"agentName":"oc"}}]}`, want: true, wantSource: "@grafana/agento11y-opencode"},
		{name: "v2 native object with a pinned spec", v2: true, write: true, contents: `{"plugins":[{"package":"@grafana/agento11y-opencode@0.24.0"}]}`, want: true, wantSource: "@grafana/agento11y-opencode@0.24.0"},
		{name: "v2 legacy tuple", v2: true, write: true, contents: `{"plugin":[["@grafana/agento11y-opencode@next",{}]]}`, want: true, wantSource: "@grafana/agento11y-opencode@next"},
		{name: "v2 legacy name", v2: true, write: true, contents: `{"plugins":["@grafana/sigil-opencode"]}`, want: true, wantSource: "@grafana/sigil-opencode"},
		{
			// Normalization puts every legacy entry before the native ones, and
			// the host keeps the first spec that loads the plugin id.
			name:       "v2 legacy key wins over an earlier-written native key",
			v2:         true,
			write:      true,
			contents:   `{"plugins":["@grafana/agento11y-opencode@0.24.0"],"plugin":["@grafana/agento11y-opencode@0.23.0"]}`,
			want:       true,
			wantSource: "@grafana/agento11y-opencode@0.23.0",
		},
		{
			name:       "v2 reads opencode.jsonc after opencode.json",
			v2:         true,
			write:      true,
			contents:   `{"plugins":["other-plugin"]}`,
			files:      map[string]string{"opencode.jsonc": "{\n  // capture\n  \"plugins\": [\"@grafana/agento11y-opencode\",],\n}\n"},
			want:       true,
			wantSource: "@grafana/agento11y-opencode",
		},
		{
			name:       "v2 prefers opencode.json when both register it",
			v2:         true,
			write:      true,
			contents:   `{"plugins":["@grafana/agento11y-opencode@0.23.0"]}`,
			files:      map[string]string{"opencode.jsonc": `{"plugins":["@grafana/agento11y-opencode@0.24.0"]}`},
			want:       true,
			wantSource: "@grafana/agento11y-opencode@0.23.0",
		},
		{name: "v2 only jsonc", v2: true, files: map[string]string{"opencode.jsonc": `{"plugin":["@grafana/agento11y-opencode"]}`}, want: true, wantSource: "@grafana/agento11y-opencode"},
		{
			name:           "v2 disabled by id stays registered",
			v2:             true,
			write:          true,
			contents:       `{"plugins":["@grafana/agento11y-opencode","-grafana.agento11y"]}`,
			want:           true,
			wantSource:     "@grafana/agento11y-opencode",
			wantDisabledBy: "-grafana.agento11y",
		},
		{
			name:           "v2 disabled by a wildcard in the other file",
			v2:             true,
			write:          true,
			contents:       `{"plugin":["@grafana/agento11y-opencode"]}`,
			files:          map[string]string{"opencode.jsonc": `{"plugins":["-*"]}`},
			want:           true,
			wantSource:     "@grafana/agento11y-opencode",
			wantDisabledBy: "-*",
		},
		{
			// A selector only acts on plugins already loaded.
			name:       "v2 selector before the entry does nothing",
			v2:         true,
			write:      true,
			contents:   `{"plugins":["-grafana.agento11y","@grafana/agento11y-opencode"]}`,
			want:       true,
			wantSource: "@grafana/agento11y-opencode",
		},
		{name: "v2 re-enabled by a later selector", v2: true, write: true, contents: `{"plugins":["@grafana/agento11y-opencode","-*","grafana.*"]}`, want: true, wantSource: "@grafana/agento11y-opencode"},
		{name: "v2 re-enabled by a later entry", v2: true, write: true, contents: `{"plugins":["@grafana/agento11y-opencode","-grafana.agento11y",{"package":"@grafana/agento11y-opencode"}]}`, want: true, wantSource: "@grafana/agento11y-opencode"},
		{name: "v2 unrelated selector", v2: true, write: true, contents: `{"plugins":["@grafana/agento11y-opencode","-opencode.*","-grafana.other"]}`, want: true, wantSource: "@grafana/agento11y-opencode"},
		{name: "v2 prefix collision does not match", v2: true, write: true, contents: `{"plugins":["@grafana/agento11y-opencode-extra"]}`, want: false},
		// The host drops entries it cannot decode.
		{name: "v2 object without package", v2: true, write: true, contents: `{"plugins":[{"name":"@grafana/agento11y-opencode"}]}`, want: false},
		{name: "v2 object with non-object options", v2: true, write: true, contents: `{"plugins":[{"package":"@grafana/agento11y-opencode","options":[]}]}`, want: false},
		{name: "v2 legacy tuple without options", v2: true, write: true, contents: `{"plugin":[["@grafana/agento11y-opencode"]]}`, want: false},
		{name: "v2 native tuple", v2: true, write: true, contents: `{"plugins":[["@grafana/agento11y-opencode",{}]]}`, want: false},
		{name: "v2 plugins not a list", v2: true, write: true, contents: `{"plugins":"@grafana/agento11y-opencode"}`, want: false},
		{name: "v2 missing files", v2: true, want: false},
		{name: "v2 malformed jsonc surfaces error", v2: true, files: map[string]string{"opencode.jsonc": `{"plugins":[`}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.write {
				files["opencode.json"] = tc.contents
			}
			maps.Copy(files, tc.files)
			withConfigFiles(t, files)
			if !tc.v2 {
				got, err := pluginInstalled()
				if (err != nil) != tc.wantErr {
					t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
				}
				if got != tc.want {
					t.Fatalf("got = %v, want %v", got, tc.want)
				}
				return
			}
			got, err := pluginRegistered(generationV2)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			assert.Equal(t, tc.want, got)
			if tc.wantErr {
				return
			}
			reg, err := readRegistrationV2()
			require.NoError(t, err)
			assert.Equal(t, tc.wantSource, reg.source)
			assert.Equal(t, tc.wantDisabledBy, reg.disabledBy)
		})
	}
}

func TestPluginInstalled_JsoncFilename(t *testing.T) {
	withConfigFiles(t, map[string]string{
		"opencode.jsonc": "{\n  // sigil\n  \"plugin\": [\"@grafana/agento11y-opencode\"],\n}\n",
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
		"opencode.jsonc": `{"plugin":["@grafana/agento11y-opencode"]}`,
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

	withConfig(t, `{"plugin":["@grafana/agento11y-opencode"]}`)
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
	stamp := filepath.Join(state, "agento11y", "update-checks", PluginName+".stamp")
	if _, err := os.Stat(stamp); err != nil {
		t.Fatalf("expected update stamp: %v", err)
	}
}

// A config that still references the legacy @grafana/sigil-opencode name is
// rewritten to the renamed package before the probe runs: the old package is
// frozen on npm, so a legacy entry never sees updates again. OpenCode
// installs npm plugins listed in its config at startup, so the rewrite alone
// is enough.
func TestLaunch_MigratesLegacyConfig(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"

	cases := []struct {
		name            string
		file            string // config basename; "" = opencode.json
		config          string
		wantContains    []string
		wantNotContains []string
		wantPluginCount int // 0 = skip the standardized entry-count check

		// version is the host's `--version` output; "" = defaultHostVersion.
		version string
		// other is a second config file by basename, with the substrings it
		// must and must not hold afterwards.
		other                     map[string]string
		wantOther, wantNotOther   []string
		wantPluginsCount          int  // native `plugins` entries after the rewrite; 0 = skip
		wantUnchanged, noAnnounce bool // the file must not change, and no migration is announced
	}{
		{
			name:            "string entry replaced",
			config:          `{"plugin":["@grafana/sigil-opencode"]}`,
			wantContains:    []string{`"@grafana/agento11y-opencode"`},
			wantNotContains: []string{"sigil-opencode"},
			wantPluginCount: 1,
		},
		{
			name:            "pinned entry loses its pin",
			config:          `{"plugin":["@grafana/sigil-opencode@0.13.0"]}`,
			wantContains:    []string{`"@grafana/agento11y-opencode"`},
			wantNotContains: []string{"sigil-opencode", "0.13.0"},
			wantPluginCount: 1,
		},
		{
			name:            "tuple entry keeps options",
			config:          `{"plugin":[["@grafana/sigil-opencode@0.13.0",{"enabled":true}]]}`,
			wantContains:    []string{`"@grafana/agento11y-opencode"`, `{"enabled":true}`},
			wantNotContains: []string{"sigil-opencode"},
			wantPluginCount: 1,
		},
		{
			name:            "jsonc comments survive",
			file:            "opencode.jsonc",
			config:          "{\n  // my config\n  \"plugin\": [\"@grafana/sigil-opencode\"]\n}\n",
			wantContains:    []string{"// my config", `"@grafana/agento11y-opencode"`},
			wantNotContains: []string{"sigil-opencode"},
			wantPluginCount: 1,
		},
		{
			name:            "both names present removes the legacy entry",
			config:          `{"plugin":["@grafana/sigil-opencode","@grafana/agento11y-opencode"]}`,
			wantContains:    []string{`"@grafana/agento11y-opencode"`},
			wantNotContains: []string{"sigil-opencode"},
			wantPluginCount: 1,
		},
		{
			name:            "multiple legacy entries collapse to one new entry",
			config:          `{"plugin":["@grafana/sigil-opencode","@grafana/sigil-opencode@0.13.0"]}`,
			wantContains:    []string{`"@grafana/agento11y-opencode"`},
			wantNotContains: []string{"sigil-opencode"},
			wantPluginCount: 1,
		},
		{
			// OpenCode 1 reads only the first file and the singular key, so its
			// migration leaves the native key and opencode.jsonc alone.
			name:          "v1 leaves the native key and the second file alone",
			config:        `{"plugins":["@grafana/sigil-opencode"]}`,
			other:         map[string]string{"opencode.jsonc": `{"plugin":["@grafana/sigil-opencode"]}`},
			wantOther:     []string{"sigil-opencode"},
			wantUnchanged: true,
			noAnnounce:    true,
		},

		// OpenCode 2 reads both files and both keys.
		{
			name:             "v2 native string entry replaced",
			version:          "opencode v2.0.25\n",
			config:           `{"plugins":["@grafana/sigil-opencode@0.13.0"]}`,
			wantContains:     []string{`"@grafana/agento11y-opencode"`},
			wantNotContains:  []string{"sigil-opencode", "0.13.0"},
			wantPluginsCount: 1,
		},
		{
			name:             "v2 native object keeps its options",
			version:          "opencode v2.0.25\n",
			config:           `{"plugins":["other",{"package":"@grafana/sigil-opencode","options":{"agentName":"oc"}}]}`,
			wantContains:     []string{`{"package":"@grafana/agento11y-opencode","options":{"agentName":"oc"}}`, `"other"`},
			wantNotContains:  []string{"sigil-opencode"},
			wantPluginsCount: 2,
		},
		{
			name:             "v2 legacy key tuple keeps its options",
			version:          "opencode v2.0.25\n",
			config:           `{"plugin":[["@grafana/sigil-opencode",{"enabled":true}]],"plugins":["other"]}`,
			wantContains:     []string{`["@grafana/agento11y-opencode",{"enabled":true}]`},
			wantNotContains:  []string{"sigil-opencode"},
			wantPluginCount:  1,
			wantPluginsCount: 1,
		},
		{
			// The current package's pin is the user's choice and survives.
			name:             "v2 current package pin survives next to a legacy entry",
			version:          "opencode v2.0.25\n",
			config:           `{"plugin":["@grafana/sigil-opencode"],"plugins":[{"package":"@grafana/agento11y-opencode@0.24.0","options":{"a":1}}]}`,
			wantContains:     []string{`{"package":"@grafana/agento11y-opencode@0.24.0","options":{"a":1}}`},
			wantNotContains:  []string{"sigil-opencode"},
			wantPluginsCount: 1,
		},
		{
			name:             "v2 legacy entries in both keys collapse to one",
			version:          "opencode v2.0.25\n",
			config:           `{"plugins":["@grafana/sigil-opencode@0.13.0","keep"],"plugin":["@grafana/sigil-opencode"]}`,
			wantContains:     []string{`"plugin":["@grafana/agento11y-opencode"]`, `"plugins":["keep"]`},
			wantNotContains:  []string{"sigil-opencode"},
			wantPluginCount:  1,
			wantPluginsCount: 1,
		},
		{
			name:             "v2 rewrites opencode.jsonc and keeps its comments",
			version:          "opencode v2.0.25\n",
			file:             "opencode.jsonc",
			config:           "{\n  // fleet config\n  \"plugins\": [\"@grafana/sigil-opencode\", /* keep */ \"other\",],\n}\n",
			other:            map[string]string{"opencode.json": `{"theme":"dark"}`},
			wantOther:        []string{`{"theme":"dark"}`},
			wantContains:     []string{"// fleet config", "/* keep */", `"@grafana/agento11y-opencode"`},
			wantNotContains:  []string{"sigil-opencode"},
			wantPluginsCount: 2,
		},
		{
			// opencode.json loads first, so its legacy entry is the one kept.
			name:             "v2 legacy entries in both files collapse to the first",
			version:          "opencode v2.0.25\n",
			config:           `{"plugins":[{"package":"@grafana/sigil-opencode","options":{"first":true}}]}`,
			other:            map[string]string{"opencode.jsonc": `{"plugins":["@grafana/sigil-opencode","other"]}`},
			wantContains:     []string{`{"package":"@grafana/agento11y-opencode","options":{"first":true}}`},
			wantOther:        []string{`"plugins":["other"]`},
			wantNotOther:     []string{"sigil-opencode", "agento11y-opencode"},
			wantPluginsCount: 1,
		},
		{
			// The renamed package in one file means every legacy entry in the
			// other goes, so the two never load side by side.
			name:            "v2 new name in the other file removes the legacy entry",
			version:         "opencode v2.0.25\n",
			config:          `{"plugin":["@grafana/sigil-opencode","other"]}`,
			other:           map[string]string{"opencode.jsonc": `{"plugins":["@grafana/agento11y-opencode@0.24.0"]}`},
			wantContains:    []string{`"plugin":["other"]`},
			wantNotContains: []string{"sigil-opencode", "agento11y-opencode"},
			wantOther:       []string{`"@grafana/agento11y-opencode@0.24.0"`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			file := tc.file
			if file == "" {
				file = "opencode.json"
			}
			files := map[string]string{file: tc.config}
			maps.Copy(files, tc.other)
			dir := withConfigFiles(t, files)
			withLookPath(t, func(string) (string, error) { return binPath, nil })
			if tc.version != "" {
				withVersionOutput(t, tc.version, nil)
			}
			installs := 0
			withRunInstall(t, func(context.Context, string, io.Writer) error {
				installs++
				return nil
			})
			withRunInstallV2(t, func(context.Context, string, io.Writer) error {
				installs++
				return nil
			})
			execCalled := false
			withExecFn(t, func(string, []string, []string) error {
				execCalled = true
				return nil
			})

			var stderr bytes.Buffer
			require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"))
			assert.True(t, execCalled, "exec must run after migration")
			if tc.noAnnounce {
				assert.NotContains(t, stderr.String(), "migrating")
			} else {
				assert.Contains(t, stderr.String(), "migrating "+legacyPluginName+" to "+PluginName)
				assert.Zero(t, installs, "the migrated entry counts as installed")
			}

			rewritten, err := os.ReadFile(filepath.Join(dir, file))
			require.NoError(t, err)
			if tc.wantUnchanged {
				assert.Equal(t, tc.config, string(rewritten))
			}
			for _, want := range tc.wantContains {
				assert.Contains(t, string(rewritten), want)
			}
			for _, notWant := range tc.wantNotContains {
				assert.NotContains(t, string(rewritten), notWant)
			}
			if tc.wantPluginCount > 0 || tc.wantPluginsCount > 0 {
				std, err := hujson.Standardize(rewritten)
				require.NoError(t, err)
				var c struct {
					Plugin  []json.RawMessage `json:"plugin"`
					Plugins []json.RawMessage `json:"plugins"`
				}
				require.NoError(t, json.Unmarshal(std, &c))
				if tc.wantPluginCount > 0 {
					assert.Len(t, c.Plugin, tc.wantPluginCount)
				}
				if tc.wantPluginsCount > 0 {
					assert.Len(t, c.Plugins, tc.wantPluginsCount)
				}
			}
			for name := range tc.other {
				other, err := os.ReadFile(filepath.Join(dir, name))
				require.NoError(t, err)
				for _, want := range tc.wantOther {
					assert.Contains(t, string(other), want)
				}
				for _, notWant := range tc.wantNotOther {
					assert.NotContains(t, string(other), notWant)
				}
			}
		})
	}
}

// After a successful migration the probe sees the new package name, so the
// legacy refresh suppression no longer applies and the normal 24-hour
// refresh runs.
func TestLaunch_MigrationRestoresRefresh(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("SIGIL_AUTO_UPDATE", "")

	withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
	withLookPath(t, func(string) (string, error) { return binPath, nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called: the migrated entry counts as installed")
		return nil
	})
	updateCalls := 0
	withRunUpdate(t, func(context.Context, string, io.Writer) error {
		updateCalls++
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"))
	assert.Equal(t, 1, updateCalls, "refresh must run once migration replaced the legacy entry")
}

// When the config cannot be rewritten, the migration fails without blocking
// the launch: the error is logged, a recovery hint reaches stderr, and the
// legacy refresh-skip stays active so the plugin is not registered twice.
func TestLaunch_MigrationFailureFallsBackToLegacySkip(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("SIGIL_AUTO_UPDATE", "")

	dir := withConfig(t, `{"plugin":["@grafana/sigil-opencode"]}`)
	withWriteConfig(t, func(string, []byte) error { return errors.New("permission denied") })

	withLookPath(t, func(string) (string, error) { return binPath, nil })
	withRunInstall(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runInstall must not be called when the legacy plugin is installed")
		return nil
	})
	withRunUpdate(t, func(context.Context, string, io.Writer) error {
		t.Fatal("runUpdate must not be called while the legacy entry remains")
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
	assert.True(t, execCalled, "exec must run even when migration fails")
	assert.Contains(t, stderr.String(), "migration of "+legacyPluginName+" failed")
	assert.Contains(t, stderr.String(), PluginName)
	assert.Contains(t, stderr.String(), filepath.Join(dir, "opencode.json"))
	assert.Contains(t, logbuf.String(), "opencode legacy migration")

	unchanged, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"plugin":["@grafana/sigil-opencode"]}`, string(unchanged), "config must stay untouched on failure")
}

func TestLaunch_SkipsRefreshWhenUpdateDisabled(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	t.Setenv("SIGIL_AUTO_UPDATE", "false")

	withConfig(t, `{"plugin":["@grafana/agento11y-opencode"]}`)
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
	t.Setenv("XDG_CONFIG_HOME", testXDGConfigHome)
	got, err := defaultConfigDir()
	require.NoError(t, err)
	assert.Equal(t, wantXDGConfigDir, got)

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv(homeEnvVar, testHome)
	got, err = defaultConfigDir()
	require.NoError(t, err)
	assert.Equal(t, wantHomeConfigDir, got)

	// OpenCode 2 makes OPENCODE_CONFIG_DIR its global config directory;
	// OpenCode 1 reads it as an extra directory, so its probe ignores it.
	managed := t.TempDir()
	t.Setenv("OPENCODE_CONFIG_DIR", managed)
	got, err = defaultConfigDir()
	require.NoError(t, err)
	assert.Equal(t, wantHomeConfigDir, got)
	got, err = defaultConfigDirV2()
	require.NoError(t, err)
	assert.Equal(t, managed, got)

	t.Setenv("OPENCODE_CONFIG_DIR", "")
	t.Setenv("XDG_CONFIG_HOME", testXDGConfigHome)
	got, err = defaultConfigDirV2()
	require.NoError(t, err)
	assert.Equal(t, wantXDGConfigDir, got)
}

// OPENCODE_CONFIG_DIR moves OpenCode 2's whole global config, so the probe,
// the migration, and the install decision all follow it.
func TestLaunch_V2HonorsOpencodeConfigDir(t *testing.T) {
	const binPath = "/usr/local/bin/opencode"
	t.Setenv("SIGIL_AUTO_UPDATE", "false")
	managed := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(managed, "opencode.json"), []byte(`{"plugins":["@grafana/sigil-opencode"]}`), 0o600))
	t.Setenv("OPENCODE_CONFIG_DIR", managed)
	// The XDG directory holds the v1 answer; v2 must not read it.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	prev, prevV2 := configDirFn, configDirV2Fn
	t.Cleanup(func() { configDirFn, configDirV2Fn = prev, prevV2 })
	configDirFn, configDirV2Fn = defaultConfigDir, defaultConfigDirV2

	withLookPath(t, func(string) (string, error) { return binPath, nil })
	withVersionOutput(t, "opencode v2.0.25\n", nil)
	withRunInstallV2(t, func(context.Context, string, io.Writer) error {
		t.Error("the migrated entry in OPENCODE_CONFIG_DIR counts as installed")
		return nil
	})
	withExecFn(t, func(string, []string, []string) error { return nil })

	var stderr bytes.Buffer
	require.NoError(t, Launch(context.Background(), nil, nil, strings.NewReader(""), io.Discard, &stderr, nopLogger(), "dev"))
	assert.Contains(t, stderr.String(), filepath.Join(managed, "opencode.json"))
	got, err := os.ReadFile(filepath.Join(managed, "opencode.json"))
	require.NoError(t, err)
	assert.Equal(t, `{"plugins":["@grafana/agento11y-opencode"]}`, string(got))
}

// withConfig points configDirFn at a temp directory, optionally
// seeding it with an opencode.json containing the given contents.
// Pass "" to leave the directory empty. Returns the config directory.
func withConfig(t *testing.T, contents string) string {
	t.Helper()
	files := map[string]string{}
	if contents != "" {
		files["opencode.json"] = contents
	}
	return withConfigFiles(t, files)
}

// withConfigFiles points both releases' config directory seams at a temp
// directory and writes the supplied {basename: contents} files into it.
// Returns the config directory.
func withConfigFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, contents := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	prev, prevV2 := configDirFn, configDirV2Fn
	t.Cleanup(func() { configDirFn, configDirV2Fn = prev, prevV2 })
	configDirFn = func() (string, error) { return dir, nil }
	configDirV2Fn = func() (string, error) { return dir, nil }
	return dir
}

// withVersionOutput makes `opencode --version` print out, or fail with err.
func withVersionOutput(t *testing.T, out string, err error) {
	t.Helper()
	prev := versionOutput
	t.Cleanup(func() { versionOutput = prev })
	versionOutput = func(context.Context, string) ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(out), nil
	}
}

func withRunInstallV2(t *testing.T, fn func(context.Context, string, io.Writer) error) {
	t.Helper()
	prev := runInstallV2
	t.Cleanup(func() { runInstallV2 = prev })
	runInstallV2 = fn
}

// withCache points cacheDirFn at a temp directory and writes the supplied
// files into it, creating parent directories. Keys are slash-separated paths
// relative to the cache directory. Returns the cache directory.
func withCache(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, contents := range files {
		path := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	prev := cacheDirFn
	t.Cleanup(func() { cacheDirFn = prev })
	cacheDirFn = func() (string, error) { return dir, nil }
	return dir
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

func withWriteConfig(t *testing.T, fn func(string, []byte) error) {
	t.Helper()
	prev := writeConfig
	t.Cleanup(func() { writeConfig = prev })
	writeConfig = fn
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
		{spec: "@grafana/agento11y-opencode", want: ""},
		{spec: "@grafana/agento11y-opencode@0.6.0", want: "0.6.0"},
		{spec: "@grafana/agento11y-opencode@next", want: "next"},
	}
	for _, tc := range cases {
		t.Run(tc.spec, func(t *testing.T) {
			require.Equal(t, tc.want, versionFromNpmSpec(tc.spec))
		})
	}
}

func TestStatus(t *testing.T) {
	tests := []struct {
		name   string
		config string
		// cache holds files written under the fake opencode cache directory,
		// keyed by slash-separated relative path.
		cache         map[string]string
		wantInstalled bool
		wantVersion   string

		// version puts opencode on PATH printing this `--version`; "" keeps
		// it off PATH, where Status tries the v1 probe and then the v2 one.
		version     string
		configFiles map[string]string // further config files by basename
		wantErr     string
	}{
		{name: "installed", config: `{"plugin":["@grafana/agento11y-opencode@0.6.0"]}`, wantInstalled: true, wantVersion: "0.6.0"},
		{name: "legacy name installed", config: `{"plugin":["@grafana/sigil-opencode@0.13.0"]}`, wantInstalled: true, wantVersion: "0.13.0"},
		{name: "not installed", config: `{"plugin":["other-plugin"]}`, wantInstalled: false},
		// The common real install: a bare spec, which opencode resolves as
		// `<name>@latest` and installs into its cache under that raw spec.
		{
			name:   "bare spec resolves from the cache tree",
			config: `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.19.0"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.19.0",
		},
		{
			name:   "bare legacy spec resolves from the cache tree",
			config: `{"plugin":["@grafana/sigil-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/sigil-opencode@latest/node_modules/@grafana/sigil-opencode/package.json": `{"name":"@grafana/sigil-opencode","version":"0.13.0"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.13.0",
		},
		// A dist tag is passed through as written, so the cache directory is
		// named with the tag and only the tree knows the number.
		{
			name:   "dist tag resolves from the cache tree",
			config: `{"plugin":["@grafana/agento11y-opencode@next"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@next/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.20.0-rc.1"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.20.0-rc.1",
		},
		// The cache is what opencode loads, so it wins over a disagreeing pin.
		{
			name:   "cache tree wins over a disagreeing pinned spec",
			config: `{"plugin":["@grafana/agento11y-opencode@0.6.0"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@0.6.0/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.6.1"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.6.1",
		},
		// A cache directory for a different spec is not what opencode loads for
		// this config entry, so it must not answer.
		{
			name:   "cache tree for another spec is ignored",
			config: `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@0.6.0/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.6.0"}`,
			},
			wantInstalled: true,
		},
		// Pre-migration opencode installed plugins straight under the cache root.
		{
			name:   "legacy cache layout",
			config: `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.18.0"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.18.0",
		},
		{
			name:   "current cache layout wins over the legacy one",
			config: `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.19.0"}`,
				"node_modules/@grafana/agento11y-opencode/package.json":                                             `{"name":"@grafana/agento11y-opencode","version":"0.18.0"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.19.0",
		},
		// Cache reads are best-effort: an absent or unreadable tree means the
		// version is unknown, and a pinned spec still answers.
		{
			name:          "bare spec with no cache tree",
			config:        `{"plugin":["@grafana/agento11y-opencode"]}`,
			wantInstalled: true,
		},
		{
			name:   "malformed cache package.json falls back to the pinned spec",
			config: `{"plugin":["@grafana/agento11y-opencode@0.6.0"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@0.6.0/node_modules/@grafana/agento11y-opencode/package.json": `{"name":`,
			},
			wantInstalled: true,
			wantVersion:   "0.6.0",
		},
		{
			name:   "cache package.json without a version",
			config: `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode"}`,
			},
			wantInstalled: true,
		},
		// A tuple entry carries options next to the spec; the spec still resolves.
		{
			name:   "tuple entry resolves from the cache tree",
			config: `{"plugin":[["@grafana/agento11y-opencode",{"foo":true}]]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"name":"@grafana/agento11y-opencode","version":"0.19.0"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.19.0",
		},
		{
			name:          "supported v1 on PATH reads the v1 layout",
			version:       "1.18.29\n",
			config:        `{"plugin":["@grafana/agento11y-opencode"]}`,
			cache:         map[string]string{"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.19.0"}`},
			wantInstalled: true,
			wantVersion:   "0.19.0",
		},
		{
			name:    "unsupported release on PATH is an error",
			version: "opencode v2.0.10\n",
			config:  `{"plugins":["@grafana/agento11y-opencode"]}`,
			wantErr: "OpenCode 2.0.10 is older than 2.0.22",
		},

		// OpenCode 2: <cache>/npm/<spec>/<generation>/node_modules/<name>.
		{
			name:    "v2 bare spec resolves the newest generation",
			version: "opencode v2.0.25\n",
			config:  `{"plugins":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"npm/@grafana/agento11y-opencode@latest/999/node_modules/@grafana/agento11y-opencode/package.json":             `{"version":"0.23.0"}`,
				"npm/@grafana/agento11y-opencode@latest/1000/node_modules/@grafana/agento11y-opencode/package.json":            `{"version":"0.24.0"}`,
				"npm/@grafana/agento11y-opencode@latest/.staging-2000-x/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"9.9.9"}`,
			},
			wantInstalled: true,
			wantVersion:   "0.24.0",
		},
		{
			name:          "v2 latest tag shares the bare spec's directory",
			version:       "opencode v2.0.25\n",
			config:        `{"plugins":[{"package":"@grafana/agento11y-opencode@latest"}]}`,
			cache:         map[string]string{"npm/@grafana/agento11y-opencode@latest/1730000000000/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.24.0"}`},
			wantInstalled: true,
			wantVersion:   "0.24.0",
		},
		{
			name:          "v2 pinned spec has its own directory",
			version:       "opencode v2.0.25\n",
			config:        `{"plugin":["@grafana/agento11y-opencode@0.23.0"]}`,
			cache:         map[string]string{"npm/@grafana/agento11y-opencode@0.23.0/1730000000000/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.23.0"}`},
			wantInstalled: true,
			wantVersion:   "0.23.0",
		},
		{
			name:          "v2 key directory without generations",
			version:       "opencode v2.0.25\n",
			config:        `{"plugins":["@grafana/agento11y-opencode"]}`,
			cache:         map[string]string{"npm/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.22.0"}`},
			wantInstalled: true,
			wantVersion:   "0.22.0",
		},
		{
			// OpenCode 2 does not load OpenCode 1's cache.
			name:    "v2 ignores a stale v1 cache",
			version: "opencode v2.0.25\n",
			config:  `{"plugins":["@grafana/agento11y-opencode"]}`,
			cache: map[string]string{
				"packages/@grafana/agento11y-opencode@latest/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.19.0"}`,
				"node_modules/@grafana/agento11y-opencode/package.json":                                             `{"version":"0.18.0"}`,
			},
			wantInstalled: true,
		},
		{
			name:          "v2 falls back to a pinned spec",
			version:       "opencode v2.0.25\n",
			config:        `{"plugins":["@grafana/agento11y-opencode@0.24.1"]}`,
			wantInstalled: true,
			wantVersion:   "0.24.1",
		},
		{
			name:          "v2 registration in opencode.jsonc",
			version:       "opencode v2.0.25\n",
			configFiles:   map[string]string{"opencode.jsonc": "// c\n{\"plugins\":[\"@grafana/agento11y-opencode@next\"]}"},
			cache:         map[string]string{"npm/@grafana/agento11y-opencode@next/5/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.25.0-rc.1"}`},
			wantInstalled: true,
			wantVersion:   "0.25.0-rc.1",
		},
		{
			name:    "v2 plugin turned off is an error naming the selector",
			version: "opencode v2.0.25\n",
			config:  `{"plugins":["@grafana/agento11y-opencode","-grafana.agento11y"]}`,
			wantErr: `turned off by "-grafana.agento11y"`,
		},
		{
			name:          "v2 native key reads without opencode on PATH",
			config:        `{"plugins":["@grafana/agento11y-opencode"]}`,
			cache:         map[string]string{"npm/@grafana/agento11y-opencode@latest/7/node_modules/@grafana/agento11y-opencode/package.json": `{"version":"0.24.0"}`},
			wantInstalled: true,
			wantVersion:   "0.24.0",
		},
		{
			name:    "v1 does not read the native key",
			version: "1.18.29\n",
			config:  `{"plugins":["@grafana/agento11y-opencode"]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.config != "" {
				files["opencode.json"] = tc.config
			}
			maps.Copy(files, tc.configFiles)
			withConfigFiles(t, files)
			withCache(t, tc.cache)
			if tc.version != "" {
				withLookPath(t, func(string) (string, error) { return "/usr/local/bin/opencode", nil })
				withVersionOutput(t, tc.version, nil)
			} else {
				withVersionOutput(t, "", errors.New("the version probe must not run without opencode on PATH"))
			}

			installed, version, err := Status(context.Background())
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.False(t, installed)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantInstalled, installed)
			require.Equal(t, tc.wantVersion, version)
		})
	}
}

func TestDefaultCacheDir(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", testXDGCacheHome)
	got, err := defaultCacheDir()
	require.NoError(t, err)
	assert.Equal(t, wantXDGCacheDir, got)

	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv(homeEnvVar, testHome)
	got, err = defaultCacheDir()
	require.NoError(t, err)
	assert.Equal(t, wantHomeCacheDir, got)
}

func TestCachedPackageSpec(t *testing.T) {
	cases := []struct {
		source string
		want   string
		// wantWindowsKey is OpenCode 2's cache key on Windows, which replaces
		// characters a Windows path cannot hold; "" = same as want.
		wantWindowsKey string
	}{
		// opencode rewrites a bare name to `<name>@latest` before installing, and
		// names the cache directory after the spec it installed.
		{source: "@grafana/agento11y-opencode", want: "@grafana/agento11y-opencode@latest"},
		{source: "@grafana/agento11y-opencode@0.6.0", want: "@grafana/agento11y-opencode@0.6.0"},
		{source: "@grafana/agento11y-opencode@next", want: "@grafana/agento11y-opencode@next"},
		{source: "@grafana/agento11y-opencode@>=0.20 <1", want: "@grafana/agento11y-opencode@>=0.20 <1", wantWindowsKey: "@grafana/agento11y-opencode@_=0.20 _1"},
	}
	for _, tc := range cases {
		t.Run(tc.source, func(t *testing.T) {
			require.Equal(t, tc.want, cachedPackageSpec(tc.source))
			require.Equal(t, tc.want, npmCacheKey(tc.source, false))
			wantWindows := tc.wantWindowsKey
			if wantWindows == "" {
				wantWindows = tc.want
			}
			require.Equal(t, wantWindows, npmCacheKey(tc.source, true))
		})
	}
}

// TestPlanV2Args pins how OpenCode 2 arguments are classified, following the
// host's command tree (packages/cli/src/commands/commands.ts) and effect/cli's
// lexer and parser rules.
func TestPlanV2Args(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		want       []string // nil = the args unchanged
		wantDirect bool
		wantErr    string
	}{
		// Sessions gain a trailing --standalone.
		{name: "bare TUI", args: nil, want: []string{"--standalone"}},
		{name: "TUI with a directory", args: []string{"./project"}, want: []string{"./project", "--standalone"}},
		{name: "TUI flags", args: []string{"-c", "--prompt", "hi"}, want: []string{"-c", "--prompt", "hi", "--standalone"}},
		{name: "run", args: []string{"run", "say hi"}, want: []string{"run", "say hi", "--standalone"}},
		{name: "mini", args: []string{"mini", "--model", "a/b"}, want: []string{"mini", "--model", "a/b", "--standalone"}},
		// A trailing --standalone cannot swallow a message word that is also a
		// boolean literal, which --standalone placed first would.
		{name: "run message that is a boolean literal", args: []string{"run", "yes"}, want: []string{"run", "yes", "--standalone"}},
		{name: "stays before end of options", args: []string{"run", "--", "--literal"}, want: []string{"run", "--standalone", "--", "--literal"}},
		// A root flag's value is not a subcommand.
		{name: "session named like a subcommand", args: []string{"-s", "plugin"}, want: []string{"-s", "plugin", "--standalone"}},
		{name: "prompt named like a subcommand", args: []string{"--prompt", "serve"}, want: []string{"--prompt", "serve", "--standalone"}},
		{name: "clustered value flag", args: []string{"-cs", "run"}, want: []string{"-cs", "run", "--standalone"}},
		{name: "boolean consumes a literal", args: []string{"--print-logs", "yes", "run"}, want: []string{"--print-logs", "yes", "run", "--standalone"}},
		{name: "global value flag", args: []string{"--log-level", "debug", "run", "hi"}, want: []string{"--log-level", "debug", "run", "hi", "--standalone"}},
		{name: "unknown directory word", args: []string{"plugn"}, want: []string{"plugn", "--standalone"}},
		// Already standalone.
		{name: "explicit standalone", args: []string{"run", "--standalone"}},
		{name: "inline true", args: []string{"mini", "--standalone=yes"}},
		{name: "separate true literal", args: []string{"--standalone", "1"}},

		// Everything else runs as typed.
		{name: "plugin add", args: []string{"plugin", "add", "x"}, wantDirect: true},
		{name: "plugin update", args: []string{"plugin", "update"}, wantDirect: true},
		{name: "serve", args: []string{"serve"}, wantDirect: true},
		{name: "acp", args: []string{"acp"}, wantDirect: true},
		{name: "auth login", args: []string{"auth", "login"}, wantDirect: true},
		{name: "session list", args: []string{"session", "list"}, wantDirect: true},
		{name: "upgrade alias", args: []string{"update"}, wantDirect: true},
		{name: "global flag before a subcommand", args: []string{"--print-logs", "service", "status"}, wantDirect: true},
		{name: "root help", args: []string{"--help"}, wantDirect: true},
		{name: "short version", args: []string{"-v"}, wantDirect: true},
		{name: "help on a session command", args: []string{"run", "-h"}, wantDirect: true},
		{name: "help in a short cluster", args: []string{"run", "-ch"}, wantDirect: true},
		{name: "long alias resolves", args: []string{"--h"}, wantDirect: true},
		{name: "negated action still runs", args: []string{"--no-version"}, wantDirect: true},
		{name: "completions", args: []string{"--completions=zsh"}, wantDirect: true},
		{name: "wizard", args: []string{"run", "--wizard"}, wantDirect: true},
		// Help after the end of options is a message, not a flag.
		{name: "help after end of options", args: []string{"run", "--", "--help"}, want: []string{"run", "--standalone", "--", "--help"}},
		// A help/version flag wins over a conflict the host would never reach.
		{name: "help beats server", args: []string{"run", "--server", "http://x", "--help"}, wantDirect: true},

		// Refused: another server.
		{name: "server", args: []string{"run", "--server", "http://127.0.0.1:4096"}, wantErr: "Drop --server"},
		{name: "inline server", args: []string{"--server=http://x"}, wantErr: "Drop --server"},
		{name: "server on mini", args: []string{"mini", "--server"}, wantErr: "Drop --server"},
		{name: "negated standalone", args: []string{"run", "--no-standalone"}, wantErr: "--no-standalone attaches the session"},
		{name: "inline false", args: []string{"--standalone=false"}, wantErr: "--standalone=false attaches the session"},
		{name: "separate false literal", args: []string{"mini", "--standalone", "off"}, wantErr: "Drop --standalone off"},
		// Maintenance commands are the user's to run; --server there is theirs.
		{name: "server on a maintenance command", args: []string{"models", "--server", "http://x"}, wantDirect: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, direct, err := planV2Args(tc.args)
			if tc.wantErr != "" {
				var argErr *ArgumentError
				require.ErrorAs(t, err, &argErr)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDirect, direct)
			want := tc.want
			if want == nil {
				want = tc.args
			}
			if len(want) == 0 {
				assert.Empty(t, got)
				return
			}
			assert.Equal(t, want, got)
		})
	}
}
