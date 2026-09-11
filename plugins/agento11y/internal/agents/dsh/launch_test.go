package dsh

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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

func TestLaunch(t *testing.T) {
	const binPath = "/usr/local/bin/dsh"

	cases := []struct {
		name string

		lookPath    func(string) (string, error)
		preinstall  bool
		prune       bool
		overlayBody string
		runInstall  func(context.Context, string, io.Writer) error
		execFn      func(string, []string, []string) error
		args        []string

		wantErr       string
		wantInstall   int
		wantExec      bool
		wantNoOverlay bool
		wantExecArgv  func(dir string) []string
		wantStderr    []string
		wantLog       []string
	}{
		{
			name:     "missing dsh binary",
			lookPath: func(string) (string, error) { return "", exec.ErrNotFound },
			wantErr:  "dsh CLI not found on PATH",
		},
		{
			name:        "skips install when the overlay is healthy and forwards args",
			lookPath:    func(string) (string, error) { return binPath, nil },
			preinstall:  true,
			args:        []string{"--profile", "tui"},
			wantInstall: 0,
			wantExec:    true,
			wantExecArgv: func(dir string) []string {
				return []string{binPath, "--patch", patchPath(dir), "--profile", "tui"}
			},
		},
		{
			name:        "the managed overlay precedes a user patch",
			lookPath:    func(string) (string, error) { return binPath, nil },
			preinstall:  true,
			args:        []string{"web", "--patch", "/tmp/other.yml"},
			wantInstall: 0,
			wantExec:    true,
			wantExecArgv: func(dir string) []string {
				return []string{binPath, "web", "--patch", patchPath(dir), "--patch", "/tmp/other.yml"}
			},
		},
		{
			name:        "installs when no overlay exists",
			lookPath:    func(string) (string, error) { return binPath, nil },
			wantInstall: 1,
			wantExec:    true,
			wantStderr:  []string{"installing " + PluginName + " for dsh"},
		},
		{
			name:        "reinstalls when the overlay names another plugin",
			lookPath:    func(string) (string, error) { return binPath, nil },
			overlayBody: "- insert:\n    - id: someone-else\n      name: /elsewhere/index.js\n",
			wantInstall: 1,
			wantExec:    true,
		},
		{
			name:        "reinstalls and logs when the overlay does not parse",
			lookPath:    func(string) (string, error) { return binPath, nil },
			overlayBody: "- insert: [",
			wantInstall: 1,
			wantExec:    true,
			wantLog:     []string{"dsh overlay probe"},
		},
		{
			// dsh aborts on an unreadable patch, so a failed install must omit it.
			name:          "continues without capture when install fails",
			lookPath:      func(string) (string, error) { return binPath, nil },
			runInstall:    func(context.Context, string, io.Writer) error { return errors.New("network down") },
			args:          []string{"web"},
			wantInstall:   1,
			wantExec:      true,
			wantNoOverlay: true,
			wantExecArgv: func(string) []string {
				return []string{binPath, "web"}
			},
			wantStderr: []string{
				"install of " + PluginName + " failed",
				"network down",
				"npm install --prefix ",
			},
		},
		{
			name:       "drops the overlay when a failed refresh leaves no bundle",
			lookPath:   func(string) (string, error) { return binPath, nil },
			preinstall: true,
			prune:      true,
			runInstall: func(context.Context, string, io.Writer) error { return errors.New("network down") },
			args:       []string{"--profile", "tui"},
			// The pruned bundle makes the probe report not installed, so
			// this is the install path rather than the TTL-gated update.
			wantInstall:   1,
			wantExec:      true,
			wantNoOverlay: true,
			wantExecArgv: func(string) []string {
				return []string{binPath, "--profile", "tui"}
			},
		},
		{
			name:       "exec failure surfaces the error",
			lookPath:   func(string) (string, error) { return binPath, nil },
			preinstall: true,
			execFn:     func(string, []string, []string) error { return errors.New("exec boom") },
			wantExec:   true,
			wantErr:    "exec dsh",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGIL_AUTO_UPDATE", "false")
			dir := withStateDir(t)
			if tc.preinstall {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				require.NoError(t, writePatch(dir))
			}
			if tc.prune {
				require.NoError(t, os.Remove(bundlePath(dir)))
			}
			if tc.overlayBody != "" {
				require.NoError(t, os.WriteFile(patchPath(dir), []byte(tc.overlayBody), 0o600))
			}
			withLookPath(t, tc.lookPath)

			installFn := tc.runInstall
			if installFn == nil {
				installFn = func(_ context.Context, d string, _ io.Writer) error {
					writeBundle(t, d, `{"version":"1.2.3"}`)
					return nil
				}
			}
			installCalls := 0
			withRunInstall(t, func(ctx context.Context, d string, w io.Writer) error {
				installCalls++
				assert.Equal(t, dir, d, "npm must install into the owned directory")
				return installFn(ctx, d, w)
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

			var stderr, logbuf bytes.Buffer
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
			if execCalled && !tc.wantNoOverlay {
				assert.Contains(t, execArgv, patchPath(dir), "the managed overlay must reach dsh")
			}
			if execCalled && tc.wantNoOverlay {
				assert.NotContains(t, execArgv, patchPath(dir), "an overlay dsh cannot resolve must not reach it")
			}
			if tc.wantExecArgv != nil {
				assert.Equal(t, tc.wantExecArgv(dir), execArgv)
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

// dsh 0.1.3-alpha.2's parser constrains patch placement by invocation shape
// and applies patches in argument order (apps/cli/src/args.ts).
func TestLaunchArgs(t *testing.T) {
	const patch = "/state/dsh/agento11y.cordis.yml"
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{
			// dsh errors on the missing --profile either way; the overlay
			// changes nothing.
			name: "no user args",
			want: []string{"--patch", patch},
		},
		{
			// `web` is a subcommand, so a --patch before it is a parent
			// option and dsh exits 1.
			name: "web alias takes the overlay after it",
			args: []string{"web"},
			want: []string{"web", "--patch", patch},
		},
		{
			name: "web app flags follow the overlay",
			args: []string{"web", "--host", "127.0.0.1", "--no-open"},
			want: []string{"web", "--patch", patch, "--host", "127.0.0.1", "--no-open"},
		},
		{
			name: "separated web alias keeps an app separator",
			args: []string{"--", "web"},
			want: []string{"web", "--patch", patch, "--"},
		},
		{
			name: "separated web app flags follow the overlay",
			args: []string{"--", "web", "--host", "127.0.0.1", "--no-open"},
			want: []string{"web", "--patch", patch, "--", "--host", "127.0.0.1", "--no-open"},
		},
		{
			name: "separated web app patch stays after the app separator",
			args: []string{"--", "web", "--patch", "/tmp/app.yml"},
			want: []string{"web", "--patch", patch, "--", "--patch", "/tmp/app.yml"},
		},
		{
			name: "separated web app default-config flag keeps the overlay",
			args: []string{"--", "web", "--dump-default-config"},
			want: []string{"web", "--patch", patch, "--", "--dump-default-config"},
		},
		{
			// The user's own overlay is applied after ours, so it can
			// override or disable our row.
			name: "user patch stays after ours under the web alias",
			args: []string{"web", "--patch", "/tmp/other.yml"},
			want: []string{"web", "--patch", patch, "--patch", "/tmp/other.yml"},
		},
		{
			name: "launcher flags take the overlay first",
			args: []string{"--profile", "tui"},
			want: []string{"--patch", patch, "--profile", "tui"},
		},
		{
			name: "app args after the launcher flags are untouched",
			args: []string{"--profile", "tui", "--resume", "abc"},
			want: []string{"--patch", patch, "--profile", "tui", "--resume", "abc"},
		},
		{
			name: "user patch stays after ours under --profile",
			args: []string{"--profile", "tui", "--patch", "/tmp/other.yml"},
			want: []string{"--patch", patch, "--profile", "tui", "--patch", "/tmp/other.yml"},
		},
		{
			// `plugin` forwards to pnpm and boots nothing.
			name: "plugin subcommand gets no overlay",
			args: []string{"plugin", "--profile", "tui", "add", "pkg"},
			want: []string{"plugin", "--profile", "tui", "add", "pkg"},
		},
		{
			name: "plugin after launcher flags gets no overlay",
			args: []string{"--profile", "tui", "plugin", "add", "pkg"},
			want: []string{"--profile", "tui", "plugin", "add", "pkg"},
		},
		{
			name: "plugin after inline launcher flag gets no overlay",
			args: []string{"--profile=tui", "plugin", "add", "pkg"},
			want: []string{"--profile=tui", "plugin", "add", "pkg"},
		},
		{
			name: "plugin after app flags keeps the overlay",
			args: []string{"--profile", "tui", "--resume", "plugin"},
			want: []string{"--patch", patch, "--profile", "tui", "--resume", "plugin"},
		},
		{
			// dsh refuses --dump-default-config alongside any --patch.
			name: "default-config dump gets no overlay",
			args: []string{"--profile", "web", "--dump-default-config"},
			want: []string{"--profile", "web", "--dump-default-config"},
		},
		{
			name: "default-config dump under the web alias gets no overlay",
			args: []string{"web", "--dump-default-config"},
			want: []string{"web", "--dump-default-config"},
		},
		{
			name: "default-config dump after an inline profile gets no overlay",
			args: []string{"--profile=tui", "--dump-default-config"},
			want: []string{"--profile=tui", "--dump-default-config"},
		},
		{
			// Unlike --dump-default-config, --dump-config accepts a patch.
			name: "composed dump keeps the overlay",
			args: []string{"web", "--dump-config"},
			want: []string{"web", "--patch", patch, "--dump-config"},
		},
		{
			// Past the first token dsh does not own, the flag belongs to the
			// web app and must not cost the user their capture.
			name: "an app flag of the same name keeps the overlay",
			args: []string{"web", "--host", "x", "--dump-default-config"},
			want: []string{"web", "--patch", patch, "--host", "x", "--dump-default-config"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, launchArgs(patch, tc.args))
		})
	}
}

func TestInstalled(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		want    bool
		wantErr string
	}{
		{
			name:  "no overlay",
			setup: func(*testing.T, string) {},
			want:  false,
		},
		{
			name: "overlay and bundle both present",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				require.NoError(t, writePatch(dir))
			},
			want: true,
		},
		{
			// dsh aborts when an overlay row names a missing bundle.
			name: "bundle removed under a valid overlay",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				require.NoError(t, writePatch(dir))
				require.NoError(t, os.Remove(bundlePath(dir)))
			},
			want: false,
		},
		{
			name: "overlay names another path",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				body := "- insert:\n    - id: agento11y\n      name: " + filepath.Join(dir, "elsewhere.js") + "\n"
				require.NoError(t, os.WriteFile(patchPath(dir), []byte(body), 0o600))
			},
			want: false,
		},
		{
			name: "overlay names another plugin id",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				body := "- insert:\n    - id: someone-else\n      name: " + bundlePath(dir) + "\n"
				require.NoError(t, os.WriteFile(patchPath(dir), []byte(body), 0o600))
			},
			want: false,
		},
		{
			name: "overlay carries a second row",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				body := "- insert:\n    - id: agento11y\n      name: " + bundlePath(dir) +
					"\n    - id: extra\n      name: /elsewhere.js\n"
				require.NoError(t, os.WriteFile(patchPath(dir), []byte(body), 0o600))
			},
			want: false,
		},
		{
			name: "overlay carries a second entry",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"1.2.3"}`)
				body := "- insert:\n    - id: agento11y\n      name: " + bundlePath(dir) +
					"\n- insert:\n    - id: extra\n      name: /elsewhere.js\n"
				require.NoError(t, os.WriteFile(patchPath(dir), []byte(body), 0o600))
			},
			want: false,
		},
		{
			name: "overlay does not parse",
			setup: func(t *testing.T, dir string) {
				require.NoError(t, os.WriteFile(patchPath(dir), []byte("- insert: ["), 0o600))
			},
			want:    false,
			wantErr: "parse",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := withStateDir(t)
			tc.setup(t, dir)

			got, err := installed(dir)

			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestInstall(t *testing.T) {
	t.Run("creates the directory at 0700 and the overlay at 0600", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state", "dsh")
		withRunInstall(t, func(_ context.Context, d string, _ io.Writer) error {
			writeBundle(t, d, `{"version":"1.2.3"}`)
			return nil
		})

		require.NoError(t, install(context.Background(), dir, io.Discard))

		dirInfo, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm())

		patchInfo, err := os.Stat(patchPath(dir))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), patchInfo.Mode().Perm())

		data, err := os.ReadFile(patchPath(dir))
		require.NoError(t, err)
		var overlay []patchOverlay
		require.NoError(t, yaml.Unmarshal(data, &overlay))
		assert.Equal(t, []patchOverlay{{Insert: []patchRow{{ID: patchRowID, Name: bundlePath(dir)}}}}, overlay)

		ok, err := installed(dir)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("a failed npm leaves no overlay behind", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state", "dsh")
		withRunInstall(t, func(context.Context, string, io.Writer) error {
			return errors.New("registry unreachable")
		})

		err := install(context.Background(), dir, io.Discard)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "registry unreachable")
		_, statErr := os.Stat(patchPath(dir))
		assert.True(t, os.IsNotExist(statErr), "no overlay may name a bundle that was never installed")
	})

	t.Run("npm reporting success without a bundle leaves no overlay behind", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "state", "dsh")
		withRunInstall(t, func(context.Context, string, io.Writer) error { return nil })

		err := install(context.Background(), dir, io.Discard)

		require.Error(t, err)
		assert.Contains(t, err.Error(), "could not verify the expected bundle")
		_, statErr := os.Stat(patchPath(dir))
		assert.True(t, os.IsNotExist(statErr))
	})

	t.Run("a failed npm keeps an existing overlay intact", func(t *testing.T) {
		dir := withStateDir(t)
		writeBundle(t, dir, `{"version":"1.2.3"}`)
		require.NoError(t, writePatch(dir))
		before, err := os.ReadFile(patchPath(dir))
		require.NoError(t, err)

		withRunInstall(t, func(context.Context, string, io.Writer) error {
			return errors.New("registry unreachable")
		})
		require.Error(t, install(context.Background(), dir, io.Discard))

		after, err := os.ReadFile(patchPath(dir))
		require.NoError(t, err)
		assert.Equal(t, before, after, "a refresh failure must not disturb a working install")
	})
}

func TestStatus(t *testing.T) {
	cases := []struct {
		name        string
		setup       func(t *testing.T, dir string)
		wantOK      bool
		wantVersion string
	}{
		{
			name:   "not installed",
			setup:  func(*testing.T, string) {},
			wantOK: false,
		},
		{
			name: "installed reports the package version",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, `{"version":"9.8.7"}`)
				require.NoError(t, writePatch(dir))
			},
			wantOK:      true,
			wantVersion: "9.8.7",
		},
		{
			name: "installed with an unreadable package.json reports no version",
			setup: func(t *testing.T, dir string) {
				writeBundle(t, dir, "")
				require.NoError(t, writePatch(dir))
			},
			wantOK:      true,
			wantVersion: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := withStateDir(t)
			tc.setup(t, dir)
			// Doctor reports managed state even when dsh is absent from PATH.
			withLookPath(t, func(string) (string, error) {
				t.Fatal("Status must not look dsh up on PATH")
				return "", nil
			})

			ok, version, err := Status(context.Background())

			require.NoError(t, err)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.wantVersion, version)
		})
	}
}

func withStateDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "dsh")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	prev := stateDirFn
	t.Cleanup(func() { stateDirFn = prev })
	stateDirFn = func() string { return dir }
	return dir
}

func writeBundle(t *testing.T, dir, pkgJSON string) {
	t.Helper()
	bundle := bundlePath(dir)
	require.NoError(t, os.MkdirAll(filepath.Dir(bundle), 0o700))
	require.NoError(t, os.WriteFile(bundle, []byte("export const name = 'agento11y';\n"), 0o600))
	if pkgJSON != "" {
		require.NoError(t, os.WriteFile(filepath.Join(packageDir(dir), "package.json"), []byte(pkgJSON), 0o600))
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
