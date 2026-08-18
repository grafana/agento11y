package entry

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain stubs the interactive login flow for the whole test package so
// launcher dispatch tests don't accidentally drive the real huh form when
// run from a TTY. Individual tests that exercise the login path can
// override the stub via withStubLoginRun.
//
// It also points HOME/XDG_* at a throwaway dir for the whole package: run()
// applies the dotenv config via os.Setenv, which t.Setenv cannot undo, so a
// single hook or launcher dispatch test reading the developer's real
// ~/.config/agento11y/config.env would leak SIGIL_* values (e.g. guard flags)
// into every later test in the package.
//
// It clears both spellings of every alias family for the same reason, and
// isolateDotenvHome pins them per test on top of that. A developer shell that
// exports AGENTO11Y_LOCAL=true would otherwise route every launcher test into
// local mode, and the daemon those tests would start is this test binary.
func TestMain(m *testing.M) {
	loginRun = func(context.Context, login.RunOpts) error { return login.ErrNotInteractive }
	for _, suffix := range envconfig.AliasSuffixes {
		_ = os.Unsetenv(envconfig.PreferredKey(suffix))
		_ = os.Unsetenv(envconfig.LegacyKey(suffix))
	}
	tmp, err := os.MkdirTemp("", "sigil-entry-test-home-*")
	if err != nil {
		panic(err)
	}
	_ = os.Setenv("HOME", tmp)
	_ = os.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	_ = os.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// TestAliasEnvIsScrubbedForThePackage pins the TestMain guard. Launcher tests
// that do not call isolateDotenvHome run against the process environment, and a
// shell AGENTO11Y_LOCAL=true would send them into local mode, where the daemon
// they start is this test binary running the whole suite again.
func TestAliasEnvIsScrubbedForThePackage(t *testing.T) {
	for _, suffix := range envconfig.AliasSuffixes {
		for _, key := range []string{envconfig.PreferredKey(suffix), envconfig.LegacyKey(suffix)} {
			if v := strings.TrimSpace(os.Getenv(key)); v != "" {
				t.Errorf("%s = %q at test start; TestMain must clear every alias family", key, v)
			}
		}
	}
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
	if !strings.Contains(stderr.String(), "[--local|--no-local]") {
		t.Fatalf("usage does not offer --no-local: %q", stderr.String())
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
// set only in the dotenv config still routes the logger to the per-app log
// file. Cursor and Codex headless launch hooks under a stripped environment
// where the dotenv is the only source of SIGIL_DEBUG. Runs against both the
// preferred agento11y config dir and the legacy sigil fallback.
func TestRun_DotenvSIGILDebugEnablesLogging(t *testing.T) {
	for _, app := range []string{"agento11y", "sigil"} {
		t.Run(app, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("HOME", dir)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
			// ApplyEnv materializes the winner under both spellings via
			// os.Setenv, so pin both blank to keep the subtests hermetic.
			t.Setenv("SIGIL_DEBUG", "")
			t.Setenv("AGENTO11Y_DEBUG", "")

			cfgDir := filepath.Join(dir, "config", app)
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

			logPath := filepath.Join(dir, "state", "agento11y", "logs", "agento11y.log")
			data, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("debug log not created at %s: %v", logPath, err)
			}
			if !strings.Contains(string(data), "hook ran") {
				t.Fatalf("debug log missing expected line; got %q", data)
			}
		})
	}
}

func TestRun_HookLocalRewritesEndpoint(t *testing.T) {
	tests := []struct {
		name        string
		setup       func(t *testing.T)
		startErr    error
		wantStart   bool
		wantRewrite bool
	}{
		{
			name: "shell LOCAL rewrites cloud endpoint",
			setup: func(t *testing.T) {
				t.Setenv("AGENTO11Y_LOCAL", "true")
			},
			wantStart:   true,
			wantRewrite: true,
		},
		{
			name: "config.env LOCAL rewrites cloud endpoint",
			setup: func(t *testing.T) {
				cfgDir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agento11y")
				if err := os.MkdirAll(cfgDir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte("AGENTO11Y_LOCAL=true\n"), 0o600); err != nil {
					t.Fatalf("write dotenv: %v", err)
				}
			},
			wantStart:   true,
			wantRewrite: true,
		},
		{
			name: "LOCAL off leaves cloud endpoint",
			setup: func(t *testing.T) {
				t.Setenv("AGENTO11Y_LOCAL", "false")
			},
		},
		{
			name: "daemon start failure still points at loopback",
			setup: func(t *testing.T) {
				t.Setenv("AGENTO11Y_LOCAL", "true")
			},
			startErr:    errors.New("bind failed"),
			wantStart:   true,
			wantRewrite: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resetLocalHookStartFailureForTest()
			isolateDotenvHome(t)
			t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example.test")
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.test")
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")
			tc.setup(t)

			startCalls := 0
			restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
				startCalls++
				if tc.startErr != nil {
					return nil, tc.startErr
				}
				return &local.Status{PID: os.Getpid(), Port: 8765, Endpoint: "http://127.0.0.1:8765", StartedAt: time.Now().UTC().Format(time.RFC3339Nano)}, nil
			})
			t.Cleanup(restore)

			var gotEndpoint, gotCapture string
			prev := agents
			t.Cleanup(func() { agents = prev })
			agents = map[string]agentHook{
				"cursor": func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error {
					gotEndpoint = os.Getenv("AGENTO11Y_ENDPOINT")
					gotCapture = os.Getenv("AGENTO11Y_CONTENT_CAPTURE_MODE")
					return nil
				},
			}

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{"cursor", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
			})
			if gotExit != nil {
				t.Fatalf("exit code = %d, want no exit", *gotExit)
			}
			if stderr.Len() != 0 {
				t.Fatalf("stderr = %q, want empty (no local-mode banner on hooks)", stderr.String())
			}
			if (startCalls > 0) != tc.wantStart {
				t.Fatalf("daemon starts = %d, wantStart %v", startCalls, tc.wantStart)
			}
			if tc.wantRewrite {
				if gotEndpoint != "http://127.0.0.1:8765" {
					t.Fatalf("ENDPOINT = %q, want local receiver", gotEndpoint)
				}
				if gotCapture != "full" {
					t.Fatalf("CONTENT_CAPTURE_MODE = %q, want full", gotCapture)
				}
				return
			}
			if gotEndpoint != "https://cloud.example.test" {
				t.Fatalf("ENDPOINT = %q, want cloud", gotEndpoint)
			}
			if gotCapture != "metadata_only" {
				t.Fatalf("CONTENT_CAPTURE_MODE = %q, want metadata_only", gotCapture)
			}
		})
	}
}

func TestRun_HookLocalSkipsStartAfterFailure(t *testing.T) {
	resetLocalHookStartFailureForTest()
	isolateDotenvHome(t)
	t.Setenv("AGENTO11Y_LOCAL", "true")
	t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example.test")
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.test")

	startCalls := 0
	restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
		startCalls++
		return nil, errors.New("bind failed")
	})
	t.Cleanup(restore)

	prev := agents
	t.Cleanup(func() { agents = prev })
	agents = map[string]agentHook{
		"cursor": func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error { return nil },
	}

	runHook := func() {
		var stdout, stderr bytes.Buffer
		gotExit := withExit(t, func() {
			run([]string{"cursor", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
		})
		if gotExit != nil {
			t.Fatalf("exit code = %d, want no exit", *gotExit)
		}
	}

	runHook()
	if startCalls != 1 {
		t.Fatalf("first hook start calls = %d, want 1", startCalls)
	}
	runHook()
	if startCalls != 1 {
		t.Fatalf("second hook start calls = %d, want 1 (in-process short-circuit)", startCalls)
	}

	// A later hook process only has the stamp file, not the in-memory flag.
	resetLocalHookStartFailureForTest()
	runHook()
	if startCalls != 1 {
		t.Fatalf("third hook start calls = %d, want 1 (stamp-file short-circuit)", startCalls)
	}
	if os.Getenv("AGENTO11Y_ENDPOINT") != "http://127.0.0.1:8765" {
		t.Fatalf("ENDPOINT = %q, want loopback fallback", os.Getenv("AGENTO11Y_ENDPOINT"))
	}
}

func TestRun_HookLocalUnsupportedKeepsCloudEndpoint(t *testing.T) {
	resetLocalHookStartFailureForTest()
	isolateDotenvHome(t)
	t.Setenv("AGENTO11Y_LOCAL", "true")
	t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example.test")
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.test")
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")

	prevSupported := hookLocalReceiverSupported
	hookLocalReceiverSupported = func() bool { return false }
	t.Cleanup(func() { hookLocalReceiverSupported = prevSupported })

	startCalls := 0
	restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
		startCalls++
		return nil, errors.New("should not start")
	})
	t.Cleanup(restore)

	prev := agents
	t.Cleanup(func() { agents = prev })
	agents = map[string]agentHook{
		"cursor": func(_ context.Context, _ io.Reader, _ io.Writer, _ *log.Logger) error { return nil },
	}

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"cursor", "hook"}, strings.NewReader(`{}`), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit code = %d, want no exit", *gotExit)
	}
	if startCalls != 0 {
		t.Fatalf("start calls = %d, want 0", startCalls)
	}
	if os.Getenv("AGENTO11Y_ENDPOINT") != "https://cloud.example.test" {
		t.Fatalf("ENDPOINT = %q, want cloud", os.Getenv("AGENTO11Y_ENDPOINT"))
	}
	if os.Getenv("AGENTO11Y_CONTENT_CAPTURE_MODE") != "metadata_only" {
		t.Fatalf("CONTENT_CAPTURE_MODE = %q, want metadata_only", os.Getenv("AGENTO11Y_CONTENT_CAPTURE_MODE"))
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
		{name: "pi missing separator exits 2", agent: "pi", argv: []string{"--print", "hi"}, wantExit: exitPtr(2), wantStderrContains: "use `agento11y pi -- <args>`"},
		{name: "pi unknown options before separator exits 2", agent: "pi", argv: []string{"--debug", "--", "x"}, wantExit: exitPtr(2), wantStderrContains: "unknown options before `--`: [--debug]"},
		{name: "pi launcher error exits 1", agent: "pi", argv: []string{"--"}, launcherErr: boom, wantCalled: 1, wantExit: exitPtr(1), wantStderrPrefix: "agento11y:"},

		{name: "claude bare", agent: "claude", wantCalled: 1},
		{name: "claude separator only", agent: "claude", argv: []string{"--"}, wantCalled: 1},
		{name: "claude forwards args after separator", agent: "claude", argv: []string{"--", "--resume", "abc"}, wantCalled: 1, wantArgs: []string{"--resume", "abc"}},
		{name: "claude missing separator exits 2", agent: "claude", argv: []string{"foo"}, wantExit: exitPtr(2), wantStderrContains: "use `agento11y claude -- <args>`"},
		{name: "claude unknown options before separator exits 2", agent: "claude", argv: []string{"--foo", "--", "args"}, wantExit: exitPtr(2), wantStderrContains: "unknown options before `--`: [--foo]"},
		{name: "claude launcher error exits 1", agent: "claude", argv: []string{"--"}, launcherErr: boom, wantCalled: 1, wantExit: exitPtr(1), wantStderrPrefix: "agento11y:"},

		{name: "opencode bare", agent: "opencode", wantCalled: 1},
		{name: "opencode separator only", agent: "opencode", argv: []string{"--"}, wantCalled: 1},
		{name: "opencode forwards args after separator", agent: "opencode", argv: []string{"--", "run", "say hi"}, wantCalled: 1, wantArgs: []string{"run", "say hi"}},
		{name: "opencode missing separator exits 2", agent: "opencode", argv: []string{"run", "hi"}, wantExit: exitPtr(2), wantStderrContains: "use `agento11y opencode -- <args>`"},
		{name: "opencode unknown options before separator exits 2", agent: "opencode", argv: []string{"--debug", "--", "x"}, wantExit: exitPtr(2), wantStderrContains: "unknown options before `--`: [--debug]"},
		{name: "opencode launcher error exits 1", agent: "opencode", argv: []string{"--"}, launcherErr: boom, wantCalled: 1, wantExit: exitPtr(1), wantStderrPrefix: "agento11y:"},
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
// `hook` so plugins/codex/hooks/hooks.json (which invokes `<binary> codex hook`)
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
	if !strings.Contains(stderr.String(), "use `agento11y codex -- <args>`") {
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
	if !strings.HasPrefix(stderr.String(), "agento11y:") {
		t.Fatalf("stderr does not start with agento11y: %q", stderr.String())
	}
}

// `sigil cursor install`/`uninstall` must dispatch to the installer before
// the generic non-`hook` verb rejection, while `sigil cursor hook` still
// reaches the hook handler and an unknown cursor verb still exits 2. Each
// row stubs all three seams with counters so the want* fields pin both the
// branch that fired and the ones that must stay untouched.
func TestRun_CursorInstallDispatch(t *testing.T) {
	exitPtr := func(c int) *int { return &c }

	cases := []struct {
		name               string
		verb               string
		installErr         error
		wantInstall        int
		wantUninstall      int
		wantHook           int
		wantExit           *int // nil → run must not call exit
		wantStderrContains string
	}{
		{name: "install dispatches to seam", verb: "install", wantInstall: 1},
		{name: "uninstall dispatches to seam", verb: "uninstall", wantUninstall: 1},
		{name: "hook verb still dispatches to handler", verb: "hook", wantHook: 1},
		{name: "unknown cursor verb exits 2", verb: "bogus", wantExit: exitPtr(2), wantStderrContains: `unknown verb "bogus"`},
		{name: "install error exits 1", verb: "install", installErr: errors.New("boom"), wantInstall: 1, wantExit: exitPtr(1), wantStderrContains: "agento11y: boom"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			// Credentials present so install does not chain the login prompt.
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")

			install, uninstall, hook := 0, 0, 0
			withStubCursorInstall(t, func(_, _ io.Writer, _ *log.Logger) error {
				install++
				return tc.installErr
			})
			withStubCursorUninstall(t, func(io.Writer, io.Writer, *log.Logger) error {
				uninstall++
				return nil
			})
			prev := agents
			t.Cleanup(func() { agents = prev })
			agents = map[string]agentHook{
				"cursor": func(context.Context, io.Reader, io.Writer, *log.Logger) error {
					hook++
					return nil
				},
			}

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{"cursor", tc.verb}, strings.NewReader(`{}`), &stdout, &stderr)
			})

			switch {
			case tc.wantExit == nil && gotExit != nil:
				t.Fatalf("exit = %d, want no exit (stderr=%q)", *gotExit, stderr.String())
			case tc.wantExit != nil && (gotExit == nil || *gotExit != *tc.wantExit):
				t.Fatalf("exit = %v, want %d (stderr=%q)", gotExit, *tc.wantExit, stderr.String())
			}
			assert.Equal(t, tc.wantInstall, install, "install calls")
			assert.Equal(t, tc.wantUninstall, uninstall, "uninstall calls")
			assert.Equal(t, tc.wantHook, hook, "hook calls")
			if tc.wantStderrContains != "" {
				assert.Contains(t, stderr.String(), tc.wantStderrContains)
			}
		})
	}
}

// On first install (no credentials yet) the login prompt is chained, mirroring
// the launcher auto-prompt; when credentials are present it is skipped.
func TestRun_CursorInstallLoginChain(t *testing.T) {
	cases := []struct {
		name           string
		creds          bool
		wantLoginCalls int
	}{
		{name: "chains login when credentials missing", creds: false, wantLoginCalls: 1},
		{name: "skips login when credentials present", creds: true, wantLoginCalls: 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			if tc.creds {
				t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
				t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
				t.Setenv("SIGIL_AUTH_TOKEN", "token")
			} else {
				t.Setenv("SIGIL_ENDPOINT", "")
				t.Setenv("SIGIL_AUTH_TENANT_ID", "")
				t.Setenv("SIGIL_AUTH_TOKEN", "")
			}

			withStubCursorInstall(t, func(io.Writer, io.Writer, *log.Logger) error { return nil })
			loginCalls := 0
			withStubLoginRun(t, func(context.Context, login.RunOpts) error {
				loginCalls++
				return login.ErrNotInteractive
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run([]string{"cursor", "install"}, strings.NewReader(""), &stdout, &stderr)
			})
			require.Nil(t, gotExit, "stderr=%q", stderr.String())
			assert.Equal(t, tc.wantLoginCalls, loginCalls)
		})
	}
}

// withStubCursorInstall / withStubCursorUninstall replace the cursor install
// seams so dispatch can be asserted without touching ~/.cursor/hooks.json.
func withStubCursorInstall(t *testing.T, fn func(io.Writer, io.Writer, *log.Logger) error) {
	t.Helper()
	prev := cursorInstall
	t.Cleanup(func() { cursorInstall = prev })
	cursorInstall = fn
}

func withStubCursorUninstall(t *testing.T, fn func(io.Writer, io.Writer, *log.Logger) error) {
	t.Helper()
	prev := cursorUninstall
	t.Cleanup(func() { cursorUninstall = prev })
	cursorUninstall = fn
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
	// Pin both spellings of every alias family blank: the developer's shell
	// and materialization from earlier tests (which writes os.Setenv without
	// cleanup) must not leak into launcher dispatch.
	envconfig.PinAliasEnvBlank(t)
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
	got := stderr.String()
	if !strings.Contains(got, "stdin is not a terminal") {
		t.Errorf("stderr missing non-interactive hint: %q", got)
	}
	// The hint must name the canonical AGENTO11Y_* variables so non-interactive
	// callers set the right spellings, and note the legacy SIGIL_* names still work.
	for _, want := range []string{"AGENTO11Y_ENDPOINT", "AGENTO11Y_AUTH_TENANT_ID", "AGENTO11Y_AUTH_TOKEN"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr missing canonical var %q: %q", want, got)
		}
	}
	if !strings.Contains(got, "SIGIL_*") {
		t.Errorf("stderr missing legacy SIGIL_* fallback note: %q", got)
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
		{name: "no verb exits 2 with usage hint", argv: []string{"local"}, wantExit: intPtr(2), wantStderrHas: "usage: agento11y local"},
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

func TestRun_DoctorSubcommand(t *testing.T) {
	cases := []struct {
		name          string
		argv          []string
		env           map[string]string
		wantExit      *int // nil = no exit
		wantStdoutHas string
	}{
		{name: "unconfigured is healthy and exits 0", argv: []string{"doctor"}, wantStdoutHas: "agento11y doctor"},
		{name: "json mode emits sections", argv: []string{"doctor", "--json"}, wantStdoutHas: `"conversations"`},
		{
			name:     "conversations set but no OTLP exits 1",
			argv:     []string{"doctor"},
			env:      map[string]string{"SIGIL_ENDPOINT": "https://x", "SIGIL_AUTH_TENANT_ID": "1", "SIGIL_AUTH_TOKEN": "glc_t"},
			wantExit: intPtr(1),
		},
		{name: "bad flag exits 2", argv: []string{"doctor", "--nope"}, wantExit: intPtr(2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			// Empty PATH so no host-agent binaries are found: the agent sweep
			// then never shells out, keeping the test hermetic and fast.
			t.Setenv("PATH", t.TempDir())
			for _, k := range []string{
				"SIGIL_ENDPOINT", "SIGIL_AUTH_TENANT_ID", "SIGIL_AUTH_TOKEN",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT",
			} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
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
	dir, baseURL, _ = inProcessDaemonWithStartCount(t)
	return dir, baseURL
}

// inProcessDaemonWithStartCount is inProcessDaemon plus a counter of daemon
// starts, so a test can prove that a suppressed local mode starts nothing.
func inProcessDaemonWithStartCount(t *testing.T) (dir string, baseURL string, starts *int) {
	t.Helper()
	starts = new(int)
	dir = filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	storage, err := local.NewStorage(dir)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	ts := httptest.NewServer(local.NewServer(storage, nil, filepath.Join(dir, "config.env")))
	t.Cleanup(ts.Close)

	// Pin the daemon launcher to return the running test server's
	// address. The same stub stays installed for each invocation in the
	// test, so each launch sees the same daemon.
	var once sync.Once
	host := strings.TrimPrefix(ts.URL, "http://")
	colon := strings.LastIndex(host, ":")
	port, _ := strconv.Atoi(host[colon+1:])
	restore := local.SetStartDaemonForTesting(func(_ context.Context, _ string, _ *log.Logger) (*local.Status, error) {
		*starts++
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
	return dir, ts.URL, starts
}

// TestRenderLocalBanner_PrivacyClaimTracksPosture covers the one sentence in
// the launcher banner that can be false: with forwarding on, and especially
// with guard evaluation chained to Cloud, "stays on this machine" is wrong.
func TestRenderLocalBanner_PrivacyClaimTracksPosture(t *testing.T) {
	cases := []struct {
		name       string
		posture    local.ForwardPosture
		postureErr error
		want       []string
		wantNone   []string
	}{
		{
			name:     "forwarding_off",
			want:     []string{"stays on this machine"},
			wantNone: []string{"Grafana Cloud"},
		},
		{
			name:     "forwarding_on",
			posture:  local.ForwardPosture{Enabled: true, Mode: "metadata_only"},
			want:     []string{"forwarded to Grafana Cloud", "metadata_only"},
			wantNone: []string{"stays on this machine", "Guard checks"},
		},
		{
			name:    "guards_chained",
			posture: local.ForwardPosture{Enabled: true, Mode: "metadata_only", Hooks: true},
			// Naming only tool calls would be too narrow: pi checks the
			// outgoing conversation before it is sent to the model, and that
			// request is relayed too.
			want:     []string{"forwarded to Grafana Cloud", "Guard checks send tool calls", "before it is sent"},
			wantNone: []string{"stays on this machine"},
		},
		{
			// The daemon did not answer, so no posture may be claimed either
			// way.
			name:       "posture_unknown",
			posture:    local.ForwardPosture{Enabled: true, Hooks: true},
			postureErr: errors.New("connection refused"),
			want:       []string{"recorded on this machine"},
			wantNone:   []string{"stays on this machine", "Grafana Cloud"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderLocalBanner("http://127.0.0.1:8765", tc.posture, tc.postureErr, "")
			for _, want := range tc.want {
				assert.Contains(t, got, want)
			}
			for _, none := range tc.wantNone {
				assert.NotContains(t, got, none)
			}
		})
	}
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
		{name: "opencode"},
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
			assert.Contains(t, stderr.String(), "agento11y local mode")
		})
	}
}

// TestRun_LauncherLocalFromEnv covers the LOCAL alias family as a persistent
// stand-in for --local, plus the two ways to force one Cloud session while it
// is on: a shell override and --no-local.
func TestRun_LauncherLocalFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name       string
		shellEnv   map[string]string
		configEnv  string   // config.env contents; written when non-empty
		argv       []string // launcher-side args, before any `--`
		wantLocal  bool
		wantSource string // variable the banner names, or "" for no source line
	}{
		{
			name:       "shell preferred spelling",
			shellEnv:   map[string]string{"AGENTO11Y_LOCAL": "true"},
			wantLocal:  true,
			wantSource: "AGENTO11Y_LOCAL",
		},
		{
			name:       "config.env",
			configEnv:  "AGENTO11Y_LOCAL=true\n",
			wantLocal:  true,
			wantSource: "AGENTO11Y_LOCAL",
		},
		{
			// The banner names the spelling the user set, so a legacy setter
			// greps for a variable that is in their config.
			name:       "shell legacy spelling",
			shellEnv:   map[string]string{"SIGIL_LOCAL": "on"},
			wantLocal:  true,
			wantSource: "SIGIL_LOCAL",
		},
		{
			name:       "config.env legacy spelling",
			configEnv:  "SIGIL_LOCAL=true\n",
			wantLocal:  true,
			wantSource: "SIGIL_LOCAL",
		},
		{
			// A shell value beats a config.env one, so this is the one-off
			// Cloud session for a user who set the file value.
			name:      "shell false beats config.env true",
			shellEnv:  map[string]string{"AGENTO11Y_LOCAL": "false"},
			configEnv: "AGENTO11Y_LOCAL=true\n",
		},
		{
			name:     "unsupported boolean",
			shellEnv: map[string]string{"AGENTO11Y_LOCAL": "enabled"},
		},
		{
			name:     "no-local beats env",
			shellEnv: map[string]string{"AGENTO11Y_LOCAL": "true"},
			argv:     []string{"--no-local"},
		},
		{
			name:     "no-local after local",
			shellEnv: map[string]string{"AGENTO11Y_LOCAL": "true"},
			argv:     []string{"--local", "--no-local"},
		},
		{
			name:     "local after no-local",
			shellEnv: map[string]string{"AGENTO11Y_LOCAL": "true"},
			argv:     []string{"--no-local", "--local"},
		},
		{
			// Nothing in the environment asked for local mode, so the banner
			// must not blame one.
			name:      "flag only",
			argv:      []string{"--local"},
			wantLocal: true,
		},
		{
			// The user asked for local mode in this command, so the setting is
			// not what they need to know about.
			name:      "flag with env on",
			shellEnv:  map[string]string{"AGENTO11Y_LOCAL": "true"},
			argv:      []string{"--local"},
			wantLocal: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := isolateDotenvHome(t)
			if tc.configEnv != "" {
				cfgDir := filepath.Join(dir, "config", "agento11y")
				require.NoError(t, os.MkdirAll(cfgDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(cfgDir, "config.env"), []byte(tc.configEnv), 0o600))
			}
			for k, v := range tc.shellEnv {
				t.Setenv(k, v)
			}
			_, daemonURL, starts := inProcessDaemonWithStartCount(t)

			// No credentials are configured, so a Cloud session prompts for
			// login and a local one does not.
			loginCalls := 0
			withStubLoginRun(t, func(context.Context, login.RunOpts) error {
				loginCalls++
				return login.ErrNotInteractive
			})

			var gotEnv *local.LaunchEnv
			called := false
			withStubLauncher(t, "claude", func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				called = true
				gotEnv = env
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{"claude"}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})
			require.Nil(t, gotExit, "stderr=%q", stderr.String())
			require.True(t, called, "launcher was not called")

			if !tc.wantLocal {
				assert.Nil(t, gotEnv)
				assert.Equal(t, 0, *starts, "daemon must not start")
				assert.NotContains(t, stderr.String(), "agento11y local mode")
				assert.Equal(t, 1, loginCalls, "a Cloud session with no credentials prompts for login")
				if slices.Contains(tc.argv, "--no-local") {
					// The agent and anything it starts inherit this environment,
					// where dotenv materialized the family under both spellings.
					assert.Equal(t, "false", envconfig.Getenv("LOCAL"))
				}
				return
			}

			require.NotNil(t, gotEnv)
			assert.Equal(t, daemonURL, gotEnv.Endpoint)
			assert.Equal(t, daemonURL+"/otlp", gotEnv.OTLPEndpoint)
			assert.Contains(t, stderr.String(), "agento11y local mode")
			assert.Equal(t, 0, loginCalls, "local mode injects placeholder credentials instead of prompting")
			if tc.wantSource != "" {
				assert.Contains(t, stderr.String(), "(enabled by "+tc.wantSource+")")
			} else {
				assert.NotContains(t, stderr.String(), "(enabled by")
			}
		})
	}
}

// TestRun_LauncherLocalDaemonFailure covers the failure a user did not ask for:
// with the setting on, a daemon that will not start blocks every launch, and
// the error has to name the setting and the way past it.
func TestRun_LauncherLocalDaemonFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		shellEnv map[string]string
		argv     []string
		wantHint bool
	}{
		{
			name:     "env driven",
			shellEnv: map[string]string{"AGENTO11Y_LOCAL": "true"},
			wantHint: true,
		},
		{
			// The user asked for local mode in this command, so a bare failure
			// is the whole answer.
			name: "flag only",
			argv: []string{"--local"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			for k, v := range tc.shellEnv {
				t.Setenv(k, v)
			}
			restore := local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
				return nil, errors.New("port 8765 is taken")
			})
			t.Cleanup(restore)
			withStubLauncher(t, "claude", func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				t.Fatal("launcher must not be called when the receiver did not start")
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{"claude"}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})
			require.NotNil(t, gotExit)
			assert.Equal(t, 1, *gotExit)
			assert.Contains(t, stderr.String(), "failed to start local receiver")
			if tc.wantHint {
				assert.Contains(t, stderr.String(), "local mode is on because AGENTO11Y_LOCAL is set")
				assert.Contains(t, stderr.String(), "--no-local")
			} else {
				assert.NotContains(t, stderr.String(), "--no-local")
			}
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
		{name: "missing separator", agent: "pi", argv: []string{"--local", "--bogus"}, wantStderrContain: "use `agento11y pi -- <args>`"},
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
			assert.NotContains(t, stderr.String(), "agento11y local mode")
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
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "pi"} {
		stubs[name] = func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
			envs[name] = env
			return nil
		}
	}
	withStubLaunchers(t, stubs)

	var stdout, stderr bytes.Buffer
	for _, name := range []string{"pi", "claude", "codex", "copilot", "opencode"} {
		stdout.Reset()
		stderr.Reset()
		gotExit := withExit(t, func() {
			run([]string{name, "--local"}, strings.NewReader(""), &stdout, &stderr)
		})
		require.Nil(t, gotExit, "launcher=%s stderr=%q", name, stderr.String())
	}

	require.Len(t, envs, 5)
	endpoint := envs["pi"].Endpoint
	for _, name := range []string{"claude", "codex", "copilot", "opencode", "pi"} {
		require.NotNil(t, envs[name], name)
		assert.Equal(t, endpoint, envs[name].Endpoint, name)
	}
}

func TestNormalizeTag(t *testing.T) {
	for _, tc := range []struct {
		in     string
		want   string
		wantOK bool
	}{
		{in: "project=hackathon", want: "project=hackathon", wantOK: true},
		{in: "  project = hackathon  ", want: "project=hackathon", wantOK: true},
		{in: "empty=", want: "empty=", wantOK: true},
		{in: "value=has=equals", want: "value=has=equals", wantOK: true},
		{in: "noequals", wantOK: false},
		{in: "=novalue", wantOK: false},
		{in: "  =trimmed", wantOK: false},
	} {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := normalizeTag(tc.in)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				assert.Equal(t, tc.want, got)
			}
		})
	}
}

func TestMergeTags(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing string
		flags    []string
		want     string
	}{
		{name: "no existing", existing: "", flags: []string{"project=hackathon"}, want: "project=hackathon"},
		{name: "append to existing", existing: "env=prod", flags: []string{"project=hackathon"}, want: "env=prod,project=hackathon"},
		{name: "flag overrides existing in place", existing: "project=old,env=prod", flags: []string{"project=new"}, want: "project=new,env=prod"},
		{name: "multiple flags", existing: "", flags: []string{"project=hackathon", "team=ai"}, want: "project=hackathon,team=ai"},
		{name: "drops malformed existing entries", existing: "bogus, =bad ,env=prod", flags: []string{"project=x"}, want: "env=prod,project=x"},
		{name: "last flag wins for duplicate key", existing: "", flags: []string{"k=1", "k=2"}, want: "k=2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, mergeTags(tc.existing, tc.flags))
		})
	}
}

// TestRun_LauncherTagFlagSetsSigilTags checks that --tag tokens before `--`
// merge into SIGIL_TAGS for the exec'd child while args after `--` are
// forwarded untouched.
func TestRun_LauncherTagFlagSetsSigilTags(t *testing.T) {
	for _, tc := range []struct {
		name        string
		existingTag string
		argv        []string
		wantTags    string
		wantArgs    []string
	}{
		{name: "single tag no separator", argv: []string{"--tag", "project=hackathon"}, wantTags: "project=hackathon", wantArgs: nil},
		{name: "equals form", argv: []string{"--tag=project=hackathon"}, wantTags: "project=hackathon", wantArgs: nil},
		{name: "repeated with forwarded args", argv: []string{"--tag", "project=hackathon", "--tag", "team=ai", "--", "--resume"}, wantTags: "project=hackathon,team=ai", wantArgs: []string{"--resume"}},
		{name: "merges onto existing env", existingTag: "env=prod", argv: []string{"--tag", "project=hackathon"}, wantTags: "env=prod,project=hackathon", wantArgs: nil},
		{name: "overrides existing key", existingTag: "project=old", argv: []string{"--tag", "project=new"}, wantTags: "project=new", wantArgs: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			// Register SIGIL_TAGS for cleanup so os.Setenv inside run does
			// not leak into other tests.
			t.Setenv("SIGIL_TAGS", tc.existingTag)

			var gotTags, gotPreferredTags string
			var gotArgs []string
			withStubLauncher(t, "claude", func(_ context.Context, args []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				gotTags = os.Getenv("SIGIL_TAGS")
				gotPreferredTags = os.Getenv("AGENTO11Y_TAGS")
				gotArgs = args
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{"claude"}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})
			require.Nil(t, gotExit, "stderr=%q", stderr.String())
			assert.Equal(t, tc.wantTags, gotTags)
			assert.Equal(t, tc.wantTags, gotPreferredTags, "--tag must write both spellings")
			assert.Equal(t, tc.wantArgs, gotArgs)
		})
	}
}

func TestRun_LauncherTagFlagInvalid(t *testing.T) {
	for _, tc := range []struct {
		name              string
		argv              []string
		wantStderrContain string
	}{
		{name: "missing argument", argv: []string{"--tag"}, wantStderrContain: "--tag requires a key=value argument"},
		{name: "missing argument before separator", argv: []string{"--tag", "--", "x"}, wantStderrContain: "--tag requires a key=value argument"},
		{name: "greedily consumes next token", argv: []string{"--tag", "--local"}, wantStderrContain: "invalid --tag \"--local\""},
		{name: "no equals", argv: []string{"--tag", "bogus"}, wantStderrContain: "invalid --tag \"bogus\""},
		{name: "empty key", argv: []string{"--tag", "=novalue"}, wantStderrContain: "invalid --tag \"=novalue\""},
		{name: "equals form no value separator", argv: []string{"--tag=bogus"}, wantStderrContain: "invalid --tag \"bogus\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("SIGIL_ENDPOINT", "https://cloud.example.com")
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			withStubLauncher(t, "claude", func(_ context.Context, _ []string, _ *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				t.Fatal("launcher must not be called for invalid --tag")
				return nil
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(append([]string{"claude"}, tc.argv...), strings.NewReader(""), &stdout, &stderr)
			})
			require.NotNil(t, gotExit)
			assert.Equal(t, 2, *gotExit)
			assert.Contains(t, stderr.String(), tc.wantStderrContain)
		})
	}
}

// TestRun_LoginSubcommandFlags covers the flag surface of `agento11y login`:
// which values reach RunOpts, and which combinations the command refuses
// before it ever prompts.
func TestRun_LoginSubcommandFlags(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		// stubErr is what the stubbed login.Run returns, for the cases that
		// check how the command reports an outcome rather than how it parses
		// its flags.
		stubErr    error
		wantExit   *int
		wantOpts   *login.RunOpts
		wantStderr []string
	}{
		{
			name: "every value as a flag",
			args: []string{
				"login",
				"--endpoint", "https://example.invalid",
				"--tenant", "123",
				"--token", "secret-token",
				"--otlp-endpoint", "https://otlp.example",
				"--no-verify",
				"--yes",
			},
			wantOpts: &login.RunOpts{
				Endpoint:     "https://example.invalid",
				TenantID:     "123",
				Token:        "secret-token",
				OTLPEndpoint: "https://otlp.example",
				SkipVerify:   true,
				AssumeYes:    true,
				ShowNextStep: true,
			},
		},
		{
			name:  "token from stdin loses the surrounding whitespace",
			args:  []string{"login", "--endpoint", "https://example.invalid", "--tenant", "123", "--token-stdin", "--no-verify"},
			stdin: "  secret-token  \n",
			wantOpts: &login.RunOpts{
				Endpoint:     "https://example.invalid",
				TenantID:     "123",
				Token:        "secret-token",
				SkipVerify:   true,
				ShowNextStep: true,
			},
		},
		{
			// The config file stores one value per line, so a token that
			// spans two lines would split the line it is written on.
			name:       "token from stdin spanning two lines",
			args:       []string{"login", "--endpoint", "https://example.invalid", "--tenant", "123", "--token-stdin"},
			stdin:      "first-line\nsecond-line\n",
			wantExit:   intPtr(2),
			wantStderr: []string{"more than one line"},
		},
		{
			name:       "token from stdin without the other required flags",
			args:       []string{"login", "--token-stdin"},
			stdin:      "secret-token\n",
			wantExit:   intPtr(2),
			wantStderr: []string{"--token-stdin", "--endpoint", "--tenant"},
		},
		{
			name:       "token from stdin without a tenant",
			args:       []string{"login", "--endpoint", "https://example.invalid", "--token-stdin"},
			stdin:      "secret-token\n",
			wantExit:   intPtr(2),
			wantStderr: []string{"--tenant"},
		},
		{
			name:       "two token sources",
			args:       []string{"login", "--endpoint", "https://example.invalid", "--tenant", "123", "--token", "secret", "--token-stdin"},
			stdin:      "other\n",
			wantExit:   intPtr(2),
			wantStderr: []string{"mutually exclusive"},
		},
		{
			name:       "empty stdin",
			args:       []string{"login", "--endpoint", "https://example.invalid", "--tenant", "123", "--token-stdin"},
			stdin:      "\n",
			wantExit:   intPtr(2),
			wantStderr: []string{"no token"},
		},
		{
			name:       "positional argument",
			args:       []string{"login", "unexpected"},
			wantExit:   intPtr(2),
			wantStderr: []string{"unexpected arguments", "usage: agento11y login"},
		},
		{
			name:     "unknown flag",
			args:     []string{"login", "--no-such-flag"},
			wantExit: intPtr(2),
		},
		{
			// The endpoint refused the credentials and the user did not
			// override the save, so nothing was written and the command says
			// how to save them anyway.
			name:       "credentials the endpoint refused",
			args:       []string{"login"},
			stubErr:    login.ErrNotVerified,
			wantExit:   intPtr(1),
			wantOpts:   &login.RunOpts{ShowNextStep: true},
			wantStderr: []string{"nothing was saved", "--yes"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateDotenvHome(t)
			var got *login.RunOpts
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) error {
				got = &opts
				return c.stubErr
			})

			var stdout, stderr bytes.Buffer
			gotExit := withExit(t, func() {
				run(c.args, strings.NewReader(c.stdin), &stdout, &stderr)
			})
			switch {
			case c.wantExit == nil && gotExit != nil:
				t.Fatalf("exit = %d, want no exit (stderr: %s)", *gotExit, stderr.String())
			case c.wantExit != nil && (gotExit == nil || *gotExit != *c.wantExit):
				t.Fatalf("exit = %v, want %d", gotExit, *c.wantExit)
			}
			for _, want := range c.wantStderr {
				if !strings.Contains(stderr.String(), want) {
					t.Errorf("stderr missing %q:\n%s", want, stderr.String())
				}
			}
			if c.wantOpts == nil {
				if got != nil {
					t.Errorf("login ran with %+v, want no run at all", *got)
				}
				return
			}
			if got == nil {
				t.Fatalf("login did not run (stderr: %s)", stderr.String())
			}
			want := *c.wantOpts
			// Stderr, Logger, and Stdin are wiring, not user input.
			if got.Endpoint != want.Endpoint || got.TenantID != want.TenantID || got.Token != want.Token ||
				got.OTLPEndpoint != want.OTLPEndpoint || got.SkipVerify != want.SkipVerify ||
				got.AssumeYes != want.AssumeYes || got.ShowNextStep != want.ShowNextStep {
				t.Errorf("RunOpts =\n%+v\nwant\n%+v", *got, want)
			}
		})
	}
}

// TestRun_LauncherAutoLoginKeepsNextStepQuiet pins the automatic login the
// launchers fire: same flow, no next-step hint, and no flag-supplied values.
func TestRun_LauncherAutoLoginKeepsNextStepQuiet(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv("SIGIL_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	var got *login.RunOpts
	withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) error {
		got = &opts
		return login.ErrNotInteractive
	})
	withStubLauncher(t, "pi", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
		return nil
	})

	var stdout, stderr bytes.Buffer
	gotExit := withExit(t, func() {
		run([]string{"pi", "--"}, strings.NewReader(""), &stdout, &stderr)
	})
	if gotExit != nil {
		t.Fatalf("exit = %v, want no exit", *gotExit)
	}
	if got == nil {
		t.Fatal("auto-login did not run")
	}
	if got.ShowNextStep {
		t.Error("auto-login must leave ShowNextStep false")
	}
	if got.SkipVerify || got.AssumeYes {
		t.Errorf("auto-login must verify and must not assume yes: %+v", *got)
	}
	if got.Endpoint != "" || got.TenantID != "" || got.Token != "" || got.OTLPEndpoint != "" {
		t.Errorf("auto-login must supply no explicit values: %+v", *got)
	}
}
