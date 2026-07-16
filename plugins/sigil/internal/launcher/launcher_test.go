package launcher

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExecForwardsArgvAndEnv(t *testing.T) {
	var gotArgv0 string
	var gotArgv, gotEnv []string
	execFn := func(argv0 string, argv []string, envv []string) error {
		gotArgv0 = argv0
		gotArgv = argv
		gotEnv = envv
		return nil
	}

	args := []string{"exec", "--model", "gpt-5", "hi"}
	env := []string{"SIGIL_ENDPOINT=http://127.0.0.1:9999", "PATH=/usr/bin"}
	if err := Exec(execFn, "/usr/local/bin/codex", "codex", args, env); err != nil {
		t.Fatalf("Exec returned err: %v", err)
	}
	if gotArgv0 != "/usr/local/bin/codex" {
		t.Errorf("argv0 = %q", gotArgv0)
	}
	wantArgv := append([]string{"/usr/local/bin/codex"}, args...)
	if !reflect.DeepEqual(gotArgv, wantArgv) {
		t.Errorf("argv = %v; want %v", gotArgv, wantArgv)
	}
	if !reflect.DeepEqual(gotEnv, env) {
		t.Errorf("env = %v; want %v", gotEnv, env)
	}
}

func TestExecWrapsFailureWithName(t *testing.T) {
	cases := []struct{ name string }{{"codex"}, {"copilot"}, {"claude"}, {"pi"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			execFn := func(string, []string, []string) error { return errors.New("boom") }
			err := Exec(execFn, "/bin/"+tc.name, tc.name, nil, nil)
			if err == nil || !strings.Contains(err.Error(), "exec "+tc.name) {
				t.Fatalf("err = %v; want contains %q", err, "exec "+tc.name)
			}
			if !strings.Contains(err.Error(), "boom") {
				t.Fatalf("err = %v; want underlying error wrapped", err)
			}
		})
	}
}

func TestRunSteps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses unix shell utilities")
	}
	cases := []struct {
		name         string
		steps        [][]string
		wantErr      bool
		wantErrSub   string
		wantOutput   []string
		forbidOutput []string
	}{
		{
			name: "runs each step in order and writes to w",
			steps: [][]string{
				{"-c", "echo first"},
				{"-c", "echo second"},
			},
			wantOutput: []string{"first", "second"},
		},
		{
			name: "stops on first failure and reports bin name + argv",
			steps: [][]string{
				{"-c", "echo ok"},
				{"-c", "echo bad; exit 7"},
				{"-c", "echo unreached"},
			},
			wantErr:      true,
			wantErrSub:   "sh -c echo bad; exit 7",
			forbidOutput: []string{"unreached"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := RunSteps(context.Background(), "/bin/sh", &buf, tc.steps)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v; want substring %q", err, tc.wantErrSub)
				}
			} else if err != nil {
				t.Fatalf("RunSteps: %v", err)
			}
			got := buf.String()
			for _, want := range tc.wantOutput {
				if !strings.Contains(got, want) {
					t.Errorf("output = %q; want substring %q", got, want)
				}
			}
			for _, forbidden := range tc.forbidOutput {
				if strings.Contains(got, forbidden) {
					t.Errorf("output = %q; should not contain %q", got, forbidden)
				}
			}
		})
	}
}

func TestOutput(t *testing.T) {
	cases := []struct {
		name       string
		needsUnix  bool
		bin        string
		args       []string
		wantErr    bool
		wantStdout string
		wantErrSub string
	}{
		{
			name:       "returns stdout",
			needsUnix:  true,
			bin:        "/bin/echo",
			args:       []string{"hello"},
			wantStdout: "hello",
		},
		{
			name:       "attaches stderr on failure",
			needsUnix:  true,
			bin:        "/bin/sh",
			args:       []string{"-c", "echo diag 1>&2; exit 3"},
			wantErr:    true,
			wantErrSub: "diag",
		},
		{
			name:    "missing binary errors",
			bin:     "/no/such/binary-xyz",
			args:    []string{"arg"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.needsUnix && runtime.GOOS == "windows" {
				t.Skip("uses unix shell utilities")
			}
			out, err := Output(context.Background(), tc.bin, tc.args...)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				if tc.wantErrSub != "" && !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v; want substring %q", err, tc.wantErrSub)
				}
				return
			}
			if err != nil {
				t.Fatalf("Output: %v", err)
			}
			if tc.wantStdout != "" && strings.TrimSpace(string(out)) != tc.wantStdout {
				t.Fatalf("stdout = %q; want %q", out, tc.wantStdout)
			}
		})
	}
}

// bootstrapHarness builds a BootstrapSpec wired against a recording execFn
// and in-memory stderr/log writers. The default spec is the "happy path"
// (probe says installed, no Update set): individual tests override only the
// fields they care about.
type bootstrapHarness struct {
	spec   BootstrapSpec
	stderr *bytes.Buffer
	logBuf *bytes.Buffer

	execCalls int
	execBin   string
	execArgs  []string
	execEnv   []string

	installCalls            int
	installWriter           io.Writer
	postInstallHintCalls    int
	installRecoveryHintRuns int
	updateCalls             int
}

func newBootstrapHarness() *bootstrapHarness {
	stderr := &bytes.Buffer{}
	logBuf := &bytes.Buffer{}
	h := &bootstrapHarness{stderr: stderr, logBuf: logBuf}
	h.spec = BootstrapSpec{
		BinName:     "toy",
		PluginLabel: "sigil-toy",
		LookPath:    func(name string) (string, error) { return "/bin/" + name, nil },
		ExecFn: func(bin string, argv, env []string) error {
			h.execCalls++
			h.execBin = bin
			h.execArgs = argv
			h.execEnv = env
			return nil
		},
		Args:        []string{"--flag"},
		Env:         []string{"K=V"},
		Logger:      log.New(logBuf, "", 0),
		Stderr:      stderr,
		Probe:       func(context.Context, string) (bool, error) { return true, nil },
		ProbeErrLog: "toy probe",
		Install:     func(context.Context, string, io.Writer) error { return nil },
	}
	return h
}

func TestBootstrap(t *testing.T) {
	cases := []struct {
		name       string
		setup      func(t *testing.T, h *bootstrapHarness)
		run        func(t *testing.T, h *bootstrapHarness) error
		wantErrSub string
		assert     func(t *testing.T, h *bootstrapHarness)
	}{
		{
			name: "look path failure",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.LookPath = func(string) (string, error) { return "", errors.New("not found") }
			},
			wantErrSub: "toy CLI not found on PATH",
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.execCalls != 0 {
					t.Fatalf("exec invoked %d times; want 0", h.execCalls)
				}
			},
		},
		{
			name: "already installed skips install and execs",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.Install = func(context.Context, string, io.Writer) error { h.installCalls++; return nil }
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.installCalls != 0 {
					t.Fatalf("install ran %d times; want 0", h.installCalls)
				}
				if h.execCalls != 1 || h.execBin != "/bin/toy" {
					t.Fatalf("exec = (%d, %q); want (1, /bin/toy)", h.execCalls, h.execBin)
				}
				if h.stderr.Len() != 0 {
					t.Fatalf("stderr = %q; want empty", h.stderr.String())
				}
			},
		},
		{
			name: "runs install when missing",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.Probe = func(context.Context, string) (bool, error) { return false, nil }
				h.spec.Install = func(_ context.Context, _ string, w io.Writer) error {
					h.installCalls++
					h.installWriter = w
					fmt.Fprintln(w, "installed!")
					return nil
				}
				h.spec.PostInstallHint = func(io.Writer) { h.postInstallHintCalls++ }
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.installCalls != 1 {
					t.Fatalf("install ran %d times; want 1", h.installCalls)
				}
				if h.installWriter != h.stderr {
					t.Fatal("install received writer other than spec.Stderr")
				}
				if h.postInstallHintCalls != 1 {
					t.Fatalf("PostInstallHint ran %d times; want 1", h.postInstallHintCalls)
				}
				if !strings.Contains(h.stderr.String(), "registering sigil-toy with toy") {
					t.Fatalf("stderr = %q; want default register message", h.stderr.String())
				}
				if h.execCalls != 1 {
					t.Fatalf("exec ran %d times; want 1", h.execCalls)
				}
			},
		},
		{
			name: "install failure falls through to exec",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.Probe = func(context.Context, string) (bool, error) { return false, nil }
				h.spec.Install = func(context.Context, string, io.Writer) error { return errors.New("network down") }
				h.spec.InstallRecoveryHint = func(w io.Writer) {
					h.installRecoveryHintRuns++
					fmt.Fprintln(w, "          toy install sigil-toy")
				}
				h.spec.PostInstallHint = func(io.Writer) { h.postInstallHintCalls++ }
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.installRecoveryHintRuns != 1 {
					t.Fatalf("InstallRecoveryHint ran %d times; want 1", h.installRecoveryHintRuns)
				}
				if h.postInstallHintCalls != 0 {
					t.Fatalf("PostInstallHint ran %d times after Install error; want 0", h.postInstallHintCalls)
				}
				got := h.stderr.String()
				for _, want := range []string{
					"install of sigil-toy failed: network down",
					"continuing without Sigil capture",
					"toy install sigil-toy",
				} {
					if !strings.Contains(got, want) {
						t.Errorf("stderr missing %q; got %q", want, got)
					}
				}
				if !strings.Contains(h.logBuf.String(), "install sigil-toy: network down") {
					t.Errorf("log = %q; want install failure logged", h.logBuf.String())
				}
				if h.execCalls != 1 {
					t.Fatalf("exec ran %d times; want 1 (install failure must not block exec)", h.execCalls)
				}
			},
		},
		{
			name: "probe error is treated as missing",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.Probe = func(context.Context, string) (bool, error) { return false, errors.New("parse fail") }
				h.spec.ProbeErrLog = "toy probe"
				h.spec.ProbeErrEcho = true
				h.spec.Install = func(context.Context, string, io.Writer) error { h.installCalls++; return nil }
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.installCalls != 1 {
					t.Fatal("probe error should fall through to install")
				}
				if !strings.Contains(h.logBuf.String(), "toy probe: parse fail") {
					t.Errorf("log = %q; want probe error logged", h.logBuf.String())
				}
				if !strings.Contains(h.stderr.String(), "agento11y: toy probe failed: parse fail") {
					t.Errorf("stderr = %q; want probe error echoed when ProbeErrEcho is set", h.stderr.String())
				}
			},
		},
		{
			name: "update path respects update TTL and records on success",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.PluginLabel = fmt.Sprintf("sigil-bootstrap-update-%d", time.Now().UnixNano())
				h.spec.Update = func(context.Context, string, io.Writer) error { h.updateCalls++; return nil }
				h.spec.UpdateTTL = time.Hour
				h.spec.SigilVersion = "v-test"
			},
			run: func(t *testing.T, h *bootstrapHarness) error {
				if err := Bootstrap(context.Background(), h.spec); err != nil {
					return err
				}
				if h.updateCalls != 1 {
					t.Fatalf("update ran %d times on first run; want 1", h.updateCalls)
				}
				if !strings.Contains(h.stderr.String(), "refreshing "+h.spec.PluginLabel+" in toy") {
					t.Errorf("stderr = %q; want default refresh message", h.stderr.String())
				}

				// Second invocation within the TTL must not call Update again.
				h.stderr.Reset()
				return Bootstrap(context.Background(), h.spec)
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if h.updateCalls != 1 {
					t.Fatalf("update ran %d times after TTL gating; want 1", h.updateCalls)
				}
			},
		},
		{
			name: "message overrides",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.Probe = func(context.Context, string) (bool, error) { return false, nil }
				h.spec.RegisterMessage = "agento11y: installing custom into toy\n"
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if !strings.Contains(h.stderr.String(), "installing custom into toy") {
					t.Errorf("stderr = %q; want RegisterMessage override", h.stderr.String())
				}
				if strings.Contains(h.stderr.String(), "registering sigil-toy with toy") {
					t.Errorf("stderr = %q; default register message should not appear", h.stderr.String())
				}
			},
		},
		{
			name: "no update when update is nil",
			setup: func(t *testing.T, h *bootstrapHarness) {
				h.spec.UpdateTTL = time.Hour
				h.spec.SigilVersion = "v-test"
			},
			assert: func(t *testing.T, h *bootstrapHarness) {
				if strings.Contains(h.stderr.String(), "refreshing") {
					t.Errorf("stderr = %q; want no refresh message when Update is nil", h.stderr.String())
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newBootstrapHarness()
			if tc.setup != nil {
				tc.setup(t, h)
			}
			run := tc.run
			if run == nil {
				run = func(t *testing.T, h *bootstrapHarness) error {
					return Bootstrap(context.Background(), h.spec)
				}
			}
			err := run(t, h)
			if tc.wantErrSub != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v; want substring %q", err, tc.wantErrSub)
				}
			} else if err != nil {
				t.Fatalf("Bootstrap: %v", err)
			}
			if tc.assert != nil {
				tc.assert(t, h)
			}
		})
	}
}
