package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/local"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain stubs the interactive login flow for the whole test package so
// launcher dispatch tests don't accidentally drive the real huh form when
// run from a TTY. Individual tests that exercise the login path can
// override the stub via withStubLoginRun.
func TestMain(m *testing.M) {
	loginRun = func(context.Context, login.RunOpts) error { return login.ErrNotInteractive }
	os.Exit(m.Run())
}

func TestRun_VersionFlag(t *testing.T) {
	prev := version
	version = "v0.0.1-test"
	t.Cleanup(func() { version = prev })

	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			withExit(t, func() {
				run([]string{flag}, strings.NewReader(""), &stdout, &stderr)
			})
			if got := strings.TrimSpace(stdout.String()); got != "v0.0.1-test" {
				t.Fatalf("stdout = %q, want %q", got, "v0.0.1-test")
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr non-empty: %q", stderr.String())
			}
		})
	}
}

func TestRun_UsageOnZeroArgs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run(nil, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "usage:") {
		t.Fatalf("stderr missing usage message: %q", stderr.String())
	}
}

func TestRun_UnknownAgentExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"bogus-agent", "hook"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), `unknown agent "bogus-agent"`) {
		t.Fatalf("stderr missing unknown-agent message: %q", stderr.String())
	}
}

func TestRun_UnknownVerbExits2(t *testing.T) {
	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude-code", "launch"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), `unknown verb "launch"`) {
		t.Fatalf("stderr missing unknown-verb message: %q", stderr.String())
	}
}

func TestRun_DispatchesToMatchingAgentHook(t *testing.T) {
	// Swap in a mock hook so we don't depend on real adapter behaviour.
	called := map[string]int{}
	wantAgents := []string{"claude-code", "codex", "copilot", "cursor"}

	prev := agents
	t.Cleanup(func() { agents = prev })
	agents = map[string]agentHook{}
	for _, a := range wantAgents {
		name := a
		agents[name] = func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error {
			called[name]++
			return nil
		}
	}

	for _, a := range wantAgents {
		var stdout, stderr bytes.Buffer
		withExit(t, func() {
			run([]string{a, "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
		})
		if called[a] != 1 {
			t.Errorf("agent %q called %d times, want 1", a, called[a])
		}
		if stderr.Len() != 0 {
			t.Errorf("stderr non-empty for %q: %q", a, stderr.String())
		}
	}
}

// TestRun_DotenvSIGILDebugEnablesLogging guards the ordering invariant in
// run(): dotenv.ApplyEnv must run before cli.InitLogger so SIGIL_DEBUG=true
// set only in $XDG_CONFIG_HOME/sigil/config.env still routes the logger to
// the per-app log file. Cursor and Codex headless launch hooks under a
// stripped environment where the dotenv is the only source of SIGIL_DEBUG.
func TestRun_DotenvSIGILDebugEnablesLogging(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("SIGIL_DEBUG", "")

	cfgDir := filepath.Join(dir, "config", "sigil")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte("SIGIL_DEBUG=true\n"), 0o600); err != nil {
		t.Fatalf("write dotenv: %v", err)
	}

	prev := agents
	t.Cleanup(func() { agents = prev })
	agents = map[string]agentHook{
		"claude-code": func(_ context.Context, _ io.Reader, _ io.Writer, logger *log.Logger) error {
			logger.Print("hook ran")
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"claude-code", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
	})

	logPath := filepath.Join(dir, "state", "sigil", "logs", "sigil.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("debug log not created at %s: %v", logPath, err)
	}
	if !strings.Contains(string(data), "hook ran") {
		t.Fatalf("debug log missing expected line; got %q", data)
	}
}

func TestRun_HookErrorIsSwallowedAfterDispatch(t *testing.T) {
	prev := agents
	t.Cleanup(func() { agents = prev })
	agents = map[string]agentHook{
		"claude-code": func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error {
			return errors.New("synthetic failure")
		},
	}

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude-code", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit code = %d, want no exit", *gotExit)
	}
}

// TestRun_LauncherDispatch covers argv parsing for both agent launchers.
// pi and claude share the same flag-parsing path, so each row pins both
// agents to the same behaviour (separator handling, exit codes, stderr
// hints). When launcherErr is non-nil the stub returns it after being
// called, which exercises the "sigil: ..." error-formatting path.
func TestRun_LauncherDispatch(t *testing.T) {
	boom := errors.New("boom")
	exitPtr := func(c int) *int { return &c }

	cases := []struct {
		name               string
		agent              string
		argv               []string // appended after the agent name
		launcherErr        error
		wantCalled         int
		wantArgs           []string // ignored when wantCalled == 0
		wantExit           *int     // nil → run must not call exit
		wantStderrContains string
		wantStderrPrefix   string
	}{
		{name: "pi bare", agent: "pi", wantCalled: 1},
		{name: "pi separator only", agent: "pi", argv: []string{"--"}, wantCalled: 1},
		{name: "pi forwards args after separator", agent: "pi", argv: []string{"--", "--print", "hi"}, wantCalled: 1, wantArgs: []string{"--print", "hi"}},
		{name: "pi missing separator exits 2", agent: "pi", argv: []string{"--print", "hi"}, wantExit: exitPtr(2), wantStderrContains: "use `sigil pi -- <args>`"},
		{name: "pi unknown options before separator exits 2", agent: "pi", argv: []string{"--debug", "--", "x"}, wantExit: exitPtr(2), wantStderrContains: "unknown options before `--`: [--debug]"},
		{name: "pi launcher error exits 1", agent: "pi", argv: []string{"--"}, launcherErr: boom, wantCalled: 1, wantExit: exitPtr(1), wantStderrPrefix: "sigil:"},

		{name: "claude bare", agent: "claude", wantCalled: 1},
		{name: "claude separator only", agent: "claude", argv: []string{"--"}, wantCalled: 1},
		{name: "claude forwards args after separator", agent: "claude", argv: []string{"--", "--resume", "abc"}, wantCalled: 1, wantArgs: []string{"--resume", "abc"}},
		{name: "claude missing separator exits 2", agent: "claude", argv: []string{"foo"}, wantExit: exitPtr(2), wantStderrContains: "use `sigil claude -- <args>`"},
		{name: "claude unknown options before separator exits 2", agent: "claude", argv: []string{"--foo", "--", "args"}, wantExit: exitPtr(2), wantStderrContains: "unknown options before `--`: [--foo]"},
		{name: "claude launcher error exits 1", agent: "claude", argv: []string{"--"}, launcherErr: boom, wantCalled: 1, wantExit: exitPtr(1), wantStderrPrefix: "sigil:"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotArgs []string
			called := 0
			withStubLauncher(t, tc.agent, func(_ context.Context, args []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				called++
				gotArgs = append([]string{}, args...)
				return tc.launcherErr
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{tc.agent}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})

			switch {
			case tc.wantExit == nil && gotExit != nil:
				t.Fatalf("exit = %d, want no exit (stderr=%q)", *gotExit, stderr.String())
			case tc.wantExit != nil && (gotExit == nil || *gotExit != *tc.wantExit):
				t.Fatalf("exit = %v, want %d (stderr=%q)", gotExit, *tc.wantExit, stderr.String())
			}
			if called != tc.wantCalled {
				t.Fatalf("launcher called %d times, want %d", called, tc.wantCalled)
			}
			if tc.wantCalled > 0 {
				if tc.wantArgs == nil {
					if len(gotArgs) != 0 {
						t.Fatalf("launcher args = %v, want empty", gotArgs)
					}
				} else if !reflect.DeepEqual(gotArgs, tc.wantArgs) {
					t.Fatalf("launcher args = %v, want %v", gotArgs, tc.wantArgs)
				}
			}
			if tc.wantStderrContains != "" && !strings.Contains(stderr.String(), tc.wantStderrContains) {
				t.Fatalf("stderr missing %q: %q", tc.wantStderrContains, stderr.String())
			}
			if tc.wantStderrPrefix != "" && !strings.HasPrefix(stderr.String(), tc.wantStderrPrefix) {
				t.Fatalf("stderr does not start with %q: %q", tc.wantStderrPrefix, stderr.String())
			}
		})
	}
}

// `codex` is registered in both `agents` and `launchers`. The dispatcher
// must prefer the hook branch when the second arg is the literal verb
// `hook` so plugins/codex/hooks/hooks.json (which invokes `sigil codex hook`)
// keeps working after the launcher was added.
func TestRun_CodexHookDispatchesEvenWithLauncher(t *testing.T) {
	hookCalls := 0
	prevAgents := agents
	t.Cleanup(func() { agents = prevAgents })
	agents = map[string]agentHook{
		"codex": func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error {
			hookCalls++
			return nil
		},
	}
	withStubLauncher(t, "codex", func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		t.Fatal("launcher must not be called for `sigil codex hook`")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"codex", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", gotExit)
	}
	if hookCalls != 1 {
		t.Fatalf("hook called %d times, want 1", hookCalls)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr non-empty: %q", stderr.String())
	}
}

func TestRun_CodexLauncherBare(t *testing.T) {
	var got []string
	called := 0
	withStubLauncher(t, "codex", func(_ context.Context, args []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		called++
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"codex"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit code = %d, want no exit", *gotExit)
	}
	if called != 1 {
		t.Fatalf("launcher called %d times, want 1", called)
	}
	if len(got) != 0 {
		t.Fatalf("launcher args = %v, want empty", got)
	}
}

func TestRun_CodexLauncherForwardsArgs(t *testing.T) {
	var got []string
	withStubLauncher(t, "codex", func(_ context.Context, args []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"codex", "--", "exec", "prompt"}, strings.NewReader(""), &stdout, &stderr)
	})
	if !reflect.DeepEqual(got, []string{"exec", "prompt"}) {
		t.Fatalf("launcher args = %v, want [exec prompt]", got)
	}
}

func TestRun_CodexLauncherMissingSeparatorExits2(t *testing.T) {
	withStubLauncher(t, "codex", func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		t.Fatal("launcher must not be called when separator is missing")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"codex", "foo"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "use `sigil codex -- <args>`") {
		t.Fatalf("stderr missing forward-args hint: %q", stderr.String())
	}
}

func TestRun_CodexLauncherErrorExits1(t *testing.T) {
	withStubLauncher(t, "codex", func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		return errors.New("boom")
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"codex", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 1 {
		t.Fatalf("exit = %v, want 1", gotExit)
	}
	if !strings.HasPrefix(stderr.String(), "sigil:") {
		t.Fatalf("stderr does not start with sigil: %q", stderr.String())
	}
}

// withStubLauncher replaces the launchers map with a single entry for the
// duration of the test.
func withStubLauncher(t *testing.T, name string, fn agentLauncher) {
	t.Helper()
	withStubLaunchers(t, map[string]agentLauncher{name: fn})
}

func withStubLaunchers(t *testing.T, stubs map[string]agentLauncher) {
	t.Helper()
	prev := launchers
	t.Cleanup(func() { launchers = prev })
	launchers = stubs
}

// withStubLoginRun replaces the package's loginRun seam for the duration of
// a single test so per-test login behaviour can be asserted without driving
// huh's TUI.
func withStubLoginRun(t *testing.T, fn func(context.Context, login.RunOpts) error) {
	t.Helper()
	prev := loginRun
	t.Cleanup(func() { loginRun = prev })
	loginRun = fn
}

// isolateDotenvHome points $HOME/$XDG_CONFIG_HOME at a fresh tempdir so
// dotenv reads/writes do not touch the user's real config during a test.
func isolateDotenvHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return dir
}

// withExit replaces the package's exit function with a recorder, runs f, and
// returns the recorded code (nil if exit was never called).
func withExit(t *testing.T, f func()) (code *int) {
	t.Helper()
	prev := exit
	t.Cleanup(func() { exit = prev })
	exit = func(c int) {
		v := c
		code = &v
		panic(exitSentinel{})
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(exitSentinel); !ok {
				panic(r)
			}
		}
	}()
	f()
	return
}

type exitSentinel struct{}

func TestRun_LoginSubcommand_NotInteractiveExits1(t *testing.T) {
	isolateDotenvHome(t)
	withStubLoginRun(t, func(context.Context, login.RunOpts) error {
		return login.ErrNotInteractive
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"login"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 1 {
		t.Fatalf("exit = %v, want 1", gotExit)
	}
	if !strings.Contains(stderr.String(), "stdin is not a terminal") {
		t.Errorf("stderr missing non-interactive hint: %q", stderr.String())
	}
}

func TestRun_LoginSubcommand_AbortedExits0(t *testing.T) {
	isolateDotenvHome(t)
	withStubLoginRun(t, func(context.Context, login.RunOpts) error {
		return login.ErrAborted
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"login"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit (aborted login is not an error)", *gotExit)
	}
	if !strings.Contains(stderr.String(), "Aborted.") {
		t.Errorf("stderr missing Aborted: %q", stderr.String())
	}
}

func TestRun_LoginSubcommand_BadFlagExits2(t *testing.T) {
	isolateDotenvHome(t)

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"login", "--no-such-flag"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
}

func TestRun_LauncherAutoPromptsWhenCredsMissing(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	loginCalled := 0
	withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) error {
		loginCalled++
		// Simulate the prompt populating the credential env vars.
		_ = os.Setenv("SIGIL_ENDPOINT", "https://sigil.example.com")
		_ = os.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
		_ = os.Setenv("SIGIL_AUTH_TOKEN", "secret")
		return nil
	})

	launcherCalled := 0
	withStubLauncher(t, "pi", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
		launcherCalled++
		if os.Getenv("SIGIL_ENDPOINT") != "https://sigil.example.com" {
			t.Errorf("launcher saw SIGIL_ENDPOINT = %q, want set by login", os.Getenv("SIGIL_ENDPOINT"))
		}
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", *gotExit)
	}
	if loginCalled != 1 {
		t.Errorf("loginRun called %d times, want 1", loginCalled)
	}
	if launcherCalled != 1 {
		t.Errorf("launcher called %d times, want 1", launcherCalled)
	}
}

func TestRun_LauncherSkipsAutoPromptWhenCredsPresent(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "https://sigil.example.com")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "secret")

	withStubLoginRun(t, func(context.Context, login.RunOpts) error {
		t.Fatal("loginRun must not be called when credentials are present")
		return nil
	})

	launcherCalled := 0
	withStubLauncher(t, "pi", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
		launcherCalled++
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", *gotExit)
	}
	if launcherCalled != 1 {
		t.Errorf("launcher called %d times, want 1", launcherCalled)
	}
}

func TestRun_LauncherContinuesWhenLoginAborted(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	withStubLoginRun(t, func(context.Context, login.RunOpts) error {
		return login.ErrAborted
	})

	launcherCalled := 0
	withStubLauncher(t, "pi", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
		launcherCalled++
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", *gotExit)
	}
	if launcherCalled != 1 {
		t.Errorf("launcher called %d times, want 1 (aborted login must not block launch)", launcherCalled)
	}
	if !strings.Contains(stderr.String(), "setup aborted") {
		t.Errorf("stderr missing aborted notice: %q", stderr.String())
	}
}

// TestRun_LocalSubcommand covers the friendly-failure paths of the
// `sigil local` verbs when no daemon is running: status / stop just
// print the friendly message, unknown / missing verb exit 2 with a
// usage hint. The happy-path verbs (start, restart) that spawn or
// adopt a real daemon are exercised elsewhere via inProcessDaemon.
func TestRun_LocalSubcommand(t *testing.T) {
	cases := []struct {
		name          string
		argv          []string
		wantExit      *int   // nil = no exit
		wantStdoutHas string // empty = skip
		wantStderrHas string // empty = skip
	}{
		{name: "status with no daemon prints friendly message", argv: []string{"local", "status"}, wantStdoutHas: "not running"},
		{name: "stop with no daemon prints friendly message", argv: []string{"local", "stop"}, wantStdoutHas: "not running"},
		{name: "unknown verb exits 2", argv: []string{"local", "bogus"}, wantExit: intPtr(2), wantStderrHas: `unknown local verb "bogus"`},
		{name: "no verb exits 2 with usage hint", argv: []string{"local"}, wantExit: intPtr(2), wantStderrHas: "usage: sigil local"},
		{name: "usage hint lists restart", argv: []string{"local"}, wantExit: intPtr(2), wantStderrHas: "restart"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(tc.argv, strings.NewReader(""), &stdout, &stderr)
			})
			switch {
			case tc.wantExit == nil && gotExit != nil:
				t.Fatalf("exit = %d, want no exit (stderr=%q)", *gotExit, stderr.String())
			case tc.wantExit != nil && (gotExit == nil || *gotExit != *tc.wantExit):
				t.Fatalf("exit = %v, want %d", gotExit, *tc.wantExit)
			}
			if tc.wantStdoutHas != "" && !strings.Contains(stdout.String(), tc.wantStdoutHas) {
				t.Fatalf("stdout missing %q: %q", tc.wantStdoutHas, stdout.String())
			}
			if tc.wantStderrHas != "" && !strings.Contains(stderr.String(), tc.wantStderrHas) {
				t.Fatalf("stderr missing %q: %q", tc.wantStderrHas, stderr.String())
			}
		})
	}
}

func intPtr(c int) *int { return &c }

// inProcessDaemon swaps the local daemon for an httptest.Server so the
// launcher tests can exercise the --local code path without forking a
// real child process. The server runs the same handlers production
// uses, so URL routing and JSONL writes are real.
func inProcessDaemon(t *testing.T) (dir string, baseURL string) {
	t.Helper()
	dir = filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	storage, err := local.NewStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	ts := httptest.NewServer(local.NewServer(storage, nil))
	t.Cleanup(ts.Close)

	// Pin the daemon launcher to return the running test server's
	// address. The same stub stays installed for each invocation in the
	// test, so each launch sees the same daemon.
	var once sync.Once
	host := strings.TrimPrefix(ts.URL, "http://")
	colon := strings.LastIndex(host, ":")
	port, _ := strconv.Atoi(host[colon+1:])
	restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
		s := local.Status{
			PID:       os.Getpid(),
			Port:      port,
			Endpoint:  ts.URL,
			StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}
		once.Do(func() { _ = local.SaveStatus(dir, s) })
		return &s, nil
	})
	t.Cleanup(restore)
	return dir, ts.URL
}

// TestRun_LauncherLocalFlagInjectsOpts checks that --local before --
// boots the receiver and surfaces a populated local env to every launcher.
// The full launcher path (env injection, exec) is covered by each agent's
// launch_test.go.
func TestRun_LauncherLocalFlagInjectsOpts(t *testing.T) {
	for _, tc := range []struct {
		name string
	}{
		{name: "claude"},
		{name: "codex"},
		{name: "copilot"},
		{name: "pi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			_, daemonURL := inProcessDaemon(t)

			var gotEnv *local.LaunchEnv
			withStubLauncher(t, tc.name, func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				gotEnv = env
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{tc.name, "--local"}, strings.NewReader(""), &stdout, &stderr)
			})
			require.Nil(t, gotExit, "stderr=%q", stderr.String())
			require.NotNil(t, gotEnv)
			assert.Equal(t, daemonURL, gotEnv.Endpoint)
			assert.Equal(t, daemonURL+"/otlp", gotEnv.OTLPEndpoint)
			assert.Contains(t, stderr.String(), "sigil local mode")
		})
	}
}

func TestRun_LauncherLocalFlagInvalidArgsDoNotStartDaemon(t *testing.T) {
	for _, tc := range []struct {
		name              string
		agent             string
		argv              []string
		wantStderrContain string
	}{
		{name: "missing separator", agent: "pi", argv: []string{"--local", "--bogus"}, wantStderrContain: "use `sigil pi -- <args>`"},
		{name: "unknown before separator", agent: "claude", argv: []string{"--local", "--bogus", "--", "x"}, wantStderrContain: "unknown options before `--`: [--bogus]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			startCalls := 0
			restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
				startCalls++
				return &local.Status{PID: os.Getpid(), Port: 8765, Endpoint: "http://127.0.0.1:8765", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
			})
			t.Cleanup(restore)
			withStubLauncher(t, tc.agent, func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				t.Fatal("launcher must not be called for invalid sigil-side args")
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{tc.agent}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})
			require.NotNil(t, gotExit)
			assert.Equal(t, 2, *gotExit)
			assert.Equal(t, 0, startCalls)
			assert.Contains(t, stderr.String(), tc.wantStderrContain)
			assert.NotContains(t, stderr.String(), "sigil local mode")
		})
	}
}

func TestRun_LauncherLocalFlagWithForwardedArgs(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	inProcessDaemon(t)

	var gotArgs []string
	withStubLauncher(t, "claude", func(_ context.Context, args []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
		gotArgs = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude", "--local", "--", "--version"}, strings.NewReader(""), &stdout, &stderr)
	})
	require.Nil(t, gotExit, "stderr=%q", stderr.String())
	assert.Equal(t, []string{"--version"}, gotArgs)
}

// TestRun_LocalLaunchersShareReceiver verifies that running local launchers
// back-to-back reuses the same daemon endpoint.
func TestRun_LocalLaunchersShareReceiver(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	inProcessDaemon(t)

	envs := map[string]*local.LaunchEnv{}
	stubs := map[string]agentLauncher{}
	for _, name := range []string{"claude", "codex", "copilot", "pi"} {
		stubs[name] = func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
			envs[name] = env
			return nil
		}
	}
	withStubLaunchers(t, stubs)

	var stdout, stderr bytes.Buffer
	for _, name := range []string{"pi", "claude", "codex", "copilot"} {
		stdout.Reset()
		stderr.Reset()
		gotExit := withExit(t, func() {
			run([]string{name, "--local"}, strings.NewReader(""), &stdout, &stderr)
		})
		require.Nil(t, gotExit, "launcher=%s stderr=%q", name, stderr.String())
	}

	require.Len(t, envs, 4)
	endpoint := envs["pi"].Endpoint
	for _, name := range []string{"claude", "codex", "copilot", "pi"} {
		require.NotNil(t, envs[name], name)
		assert.Equal(t, endpoint, envs[name].Endpoint, name)
	}
}
