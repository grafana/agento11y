package copilot

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os/exec"
	"reflect"
	"strings"
	"testing"
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

			err := Launch(context.Background(), tc.args, strings.NewReader(""), io.Discard, &stderr, logger)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want contains %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Launch returned err: %v", err)
			}
			if installCalls != tc.wantInstall {
				t.Errorf("runInstall calls = %d, want %d", installCalls, tc.wantInstall)
			}
			if execCalled != tc.wantExec {
				t.Errorf("execFn called = %v, want %v", execCalled, tc.wantExec)
			}
			if tc.wantExecArgv != nil && !reflect.DeepEqual(execArgv, tc.wantExecArgv) {
				t.Errorf("exec argv = %v, want %v", execArgv, tc.wantExecArgv)
			}
			for _, want := range tc.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q: %q", want, stderr.String())
				}
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(logbuf.String(), want) {
					t.Errorf("log missing %q: %q", want, logbuf.String())
				}
			}
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
