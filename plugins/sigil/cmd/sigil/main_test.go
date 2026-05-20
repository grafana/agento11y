package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

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
		run([]string{"codex", "launch"}, strings.NewReader(""), &stdout, &stderr)
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
	wantAgents := []string{"claude-code", "codex", "cursor"}

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

func TestRun_LauncherBareLaunch(t *testing.T) {
	var got []string
	called := 0
	withStubLauncher(t, "pi", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		called++
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi"}, strings.NewReader(""), &stdout, &stderr)
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

func TestRun_LauncherSeparatorOnly(t *testing.T) {
	var got []string
	withStubLauncher(t, "pi", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if len(got) != 0 {
		t.Fatalf("launcher args = %v, want empty", got)
	}
}

func TestRun_LauncherForwardsArgsAfterSeparator(t *testing.T) {
	var got []string
	withStubLauncher(t, "pi", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"pi", "--", "--print", "hi"}, strings.NewReader(""), &stdout, &stderr)
	})
	if !reflect.DeepEqual(got, []string{"--print", "hi"}) {
		t.Fatalf("launcher args = %v, want [--print hi]", got)
	}
}

func TestRun_LauncherMissingSeparatorExits2(t *testing.T) {
	withStubLauncher(t, "pi", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		t.Fatal("launcher must not be called when separator is missing")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--print", "hi"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "use `sigil pi -- <args>`") {
		t.Fatalf("stderr missing forward-args hint: %q", stderr.String())
	}
}

func TestRun_LauncherUnknownFlagsBeforeSeparatorExit2(t *testing.T) {
	withStubLauncher(t, "pi", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		t.Fatal("launcher must not be called when unknown options precede separator")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--debug", "--", "x"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "unknown options before `--`") {
		t.Fatalf("stderr missing unknown-options message: %q", stderr.String())
	}
}

func TestRun_LauncherErrorExits1(t *testing.T) {
	withStubLauncher(t, "pi", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		return errors.New("boom")
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 1 {
		t.Fatalf("exit = %v, want 1", gotExit)
	}
	if !strings.HasPrefix(stderr.String(), "sigil:") {
		t.Fatalf("stderr does not start with sigil: %q", stderr.String())
	}
}

func TestRun_ClaudeLauncherBare(t *testing.T) {
	var got []string
	called := 0
	withStubLauncher(t, "claude", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		called++
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude"}, strings.NewReader(""), &stdout, &stderr)
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

func TestRun_ClaudeLauncherSeparatorOnly(t *testing.T) {
	var got []string
	withStubLauncher(t, "claude", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"claude", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if len(got) != 0 {
		t.Fatalf("launcher args = %v, want empty", got)
	}
}

func TestRun_ClaudeLauncherForwardsArgs(t *testing.T) {
	var got []string
	withStubLauncher(t, "claude", func(_ context.Context, args []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		got = append([]string{}, args...)
		return nil
	})

	var stdout, stderr bytes.Buffer
	withExit(t, func() {
		run([]string{"claude", "--", "--resume", "abc"}, strings.NewReader(""), &stdout, &stderr)
	})
	if !reflect.DeepEqual(got, []string{"--resume", "abc"}) {
		t.Fatalf("launcher args = %v, want [--resume abc]", got)
	}
}

func TestRun_ClaudeLauncherMissingSeparatorExits2(t *testing.T) {
	withStubLauncher(t, "claude", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		t.Fatal("launcher must not be called when separator is missing")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude", "foo"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "use `sigil claude -- <args>`") {
		t.Fatalf("stderr missing forward-args hint: %q", stderr.String())
	}
}

func TestRun_ClaudeLauncherUnknownOptionsExits2(t *testing.T) {
	withStubLauncher(t, "claude", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		t.Fatal("launcher must not be called when unknown options precede separator")
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude", "--foo", "--", "args"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit == nil || *gotExit != 2 {
		t.Fatalf("exit = %v, want 2", gotExit)
	}
	if !strings.Contains(stderr.String(), "unknown options before `--`: [--foo]") {
		t.Fatalf("stderr missing unknown-options message: %q", stderr.String())
	}
}

func TestRun_ClaudeLauncherErrorExits1(t *testing.T) {
	withStubLauncher(t, "claude", func(_ context.Context, _ []string, _ io.Reader, _, _ io.Writer, _ *log.Logger) error {
		return errors.New("boom")
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"claude", "--"}, strings.NewReader(""), &stdout, &stderr)
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
	prev := launchers
	t.Cleanup(func() { launchers = prev })
	launchers = map[string]agentLauncher{name: fn}
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
