package entry

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
)

// TestAgentIntegration drives the real Claude Code and Codex CLIs, headless,
// against a scripted fake model, then checks what the plugin exported with
// TestGoldenIntegration's capture server, secret guard, and invariants, plus
// the companion OTLP spans and token metrics. That test replays hook payloads
// written by hand; here the agent writes them, so an agent release that
// changes a payload, a transcript, or when a hook fires fails a run.
//
// Opt-in, since it needs the CLIs. CI installs the versions pinned in
// testdata/agent-clis:
//
//	npm ci --prefix testdata/agent-clis
//	PATH=$PWD/testdata/agent-clis/node_modules/.bin:$PATH AGENT_CLI_TESTS=claude,codex \
//	    go test . -run TestAgentIntegration -count=1 -v
//
// Each case in agentCaseNames runs in full and metadata-only capture; the
// baseline also runs with the capture mode unset. The CLIs get a dummy key,
// the fake model's URL, and a throwaway HOME, so no real model or account is
// involved.
func TestAgentIntegration(t *testing.T) {
	selected, err := parseAgentSelection(os.Getenv("AGENT_CLI_TESTS"))
	if err != nil {
		t.Fatal(err)
	}
	if len(selected) == 0 {
		t.Skip("set AGENT_CLI_TESTS=claude,codex to drive the real agent CLIs")
	}
	// A CLI started from inside another agent session inherits that session's
	// variables, and they change the run: CLAUDE_CODE_EAGER_FLUSH alone hides
	// the Stop-before-transcript race. Start every CLI without them.
	clearAgentEnvironment(t)

	binDir := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(binDir, "agento11y"), "github.com/grafana/agento11y/plugins/agento11y/cmd/agento11y")
	build.Env = goToolchainEnv
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build agento11y: %v\n%s", err, out)
	}
	// The hooks run `agento11y <agent> hook` from PATH.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// The repository root, from plugins/agento11y/internal/entry.
	repo, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range agentCLINames {
		t.Run(name, func(t *testing.T) {
			if !selected[name] {
				t.Skipf("%s not in AGENT_CLI_TESTS", name)
			}
			for _, mode := range []string{"full", "metadata_only", "default"} {
				t.Run(mode, func(t *testing.T) {
					for _, scenarioName := range agentCaseNames {
						if mode == "default" && scenarioName != "baseline" {
							continue
						}
						t.Run(scenarioName, func(t *testing.T) {
							runAgentCase(t, name, mode, scenarioName, repo)
						})
					}
				})
			}
		})
	}

	// A fork (--fork-session) copies the original session's turns into a new
	// transcript under a new session ID. The original exported them; the fork
	// must export only its own turn.
	t.Run("claude/fork", func(t *testing.T) {
		if !strings.Contains(","+selected+",", ",claude,") {
			t.Skip("claude not in AGENT_CLI_TESTS")
		}
		sc := scenario{
			Agent: "claude-code",
			Env: map[string]string{
				"ANTHROPIC_BASE_URL":                       modelURL,
				"ANTHROPIC_API_KEY":                        "test",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			},
			RawSecrets: []string{secret},
			// The original's tool call and reply, then the fork's reply, which
			// carries the fork's own prompt.
			Invariants: scenarioInvariants{ExportCount: 2, Generations: 3, AnyGeneration: []string{`"text":"Say done"`}},
		}
		stateDir, capture := startHookExport(t, sc)
		model.reset()
		plugin := filepath.Join(repo, "plugins", "claude-code")
		workDir := t.TempDir()
		var output []string
		for _, argv := range [][]string{
			{"claude", "-p", prompt, "--plugin-dir", plugin, "--session-id", sessionID, "--allowedTools", "Bash"},
			{"claude", "-p", "Say done", "--plugin-dir", plugin, "--resume", sessionID, "--fork-session", "--allowedTools", "Bash"},
		} {
			output = append(output, runAgentCommand(t, workDir, argv))
		}
		registerExportDiagnostics(t, stateDir, append(output, model.requestLog()...))
		checkExports(t, "claude-fork", sc, capture.snapshot())
	})
}

const agentSessionID = "7f0c9a4e-3b1d-4c2e-9f6a-2d8b5e1c7a90"

// agentCLI is how one CLI is installed and invoked for one case.
type agentCLI struct {
	agent string
	env   map[string]string
	setup [][]string
	start func(prompt string) []string
	// conversation is Claude's session ID or the Codex thread.started ID.
	resume func(conversation, prompt string) []string
}

func newAgentCLI(name, scenarioName, repo, modelURL string) agentCLI {
	if name == "claude" {
		pluginArgs := []string{"--plugin-dir", filepath.Join(repo, "plugins", "claude-code"), "--allowedTools", "Bash"}
		env := map[string]string{
			"ANTHROPIC_BASE_URL":                       modelURL,
			"ANTHROPIC_API_KEY":                        "test",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		}
		switch scenarioName {
		case "model_error_503":
			env["CLAUDE_CODE_MAX_RETRIES"] = "1"
		case "model_error_400":
			env["CLAUDE_CODE_MAX_RETRIES"] = "0"
		}
		return agentCLI{
			agent: cliAgent(name), env: env,
			start: func(prompt string) []string {
				return slices.Concat([]string{"claude", "-p", prompt, "--session-id", agentSessionID}, pluginArgs)
			},
			// Resume restores the conversation but not the launch options.
			resume: func(conversation, prompt string) []string {
				return slices.Concat([]string{"claude", "-p", prompt, "--resume", conversation}, pluginArgs)
			},
		}
	}
	provider := `name="fake",base_url="` + modelURL + `/v1",wire_api="responses",env_key="FAKE_MODEL_KEY"`
	switch scenarioName {
	case "model_error_503":
		provider += ",request_max_retries=1,stream_max_retries=0,supports_websockets=false"
	case "model_error_400":
		provider += ",request_max_retries=0,stream_max_retries=0,supports_websockets=false"
	}
	// Codex's own sandbox is not under test, and it cannot nest inside a
	// sandboxed CI container; the fake model only issues echo commands. The
	// sandbox flag is not global, so it precedes the resume subcommand.
	common := []string{"codex", "exec", "--json", "--dangerously-bypass-hook-trust", "--skip-git-repo-check", "-s", "danger-full-access",
		"-c", "model_provider=fake", "-c", "model_providers.fake={" + provider + "}", "-m", fakeModelName}
	return agentCLI{
		agent: cliAgent(name), env: map[string]string{"FAKE_MODEL_KEY": "test"},
		setup: [][]string{
			{"codex", "plugin", "marketplace", "add", repo},
			{"codex", "plugin", "add", "agento11y-codex@agento11y"},
		},
		start: func(prompt string) []string { return slices.Concat(common, []string{prompt}) },
		resume: func(conversation, prompt string) []string {
			return slices.Concat(common, []string{"resume", conversation, prompt})
		},
	}
}

// runAgentCase runs one case in a fresh HOME, plugin state, work directory,
// model script, and receivers. A resumed case keeps all of them across its
// invocations and checks the cumulative exports after each one.
func runAgentCase(t *testing.T, cliName, mode, name, repo string) {
	provider := agentProvider(cliAgent(cliName))
	turns := agentCaseTurns(name, provider)
	model := newScriptedModel(provider, turns)
	cli := newAgentCLI(cliName, name, repo, newLoopbackServer(t, model).URL)
	sc := scenario{Agent: cli.agent, Env: cli.env}
	if name == "baseline" {
		sc.Invariants.ExportCount = 1
	}
	stateDir, capture := startHookExport(t, sc)
	telemetry := newAgentOTLPCapture(t)
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", telemetry.server.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	workDir := setupAgentMetadataFixture(t)
	checkAgentAccounts(t)
	unsetTestEnv(t, "AGENTO11Y_CONTENT_CAPTURE_MODE", "SIGIL_CONTENT_CAPTURE_MODE")
	if mode != "default" {
		t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode)
	}
	var trail []string
	registerAgentDiagnostics(t, stateDir, func() []string { return append(slices.Clone(trail), model.requestLog()...) })
	run := func(argv []string, failure string) agentOutcome {
		t.Helper()
		outcome := runAgentProcess(t.Context(), workDir, argv)
		trail = append(trail, outcome.trail())
		if err := outcome.check(failure); err != nil {
			t.Fatal(err)
		}
		return outcome
	}
	for _, argv := range cli.setup {
		run(argv, "")
	}

	wantMode := "metadata_only"
	if mode == "full" {
		wantMode = "full"
	}
	conversation := agentSessionID
	var previous []*agento11yv1.Generation
	requests := 0
	for i, turn := range turns {
		argv := cli.start(turn.prompt)
		if i > 0 {
			argv = cli.resume(conversation, turn.prompt)
		}
		var failure string
		if last := turn.responses[len(turn.responses)-1]; last.status != 0 {
			failure = last.message
		}
		outcome := run(argv, failure)
		if cli.agent == "codex" {
			events, err := parseCodexEvents(outcome.stdout)
			if err != nil {
				t.Fatal(err)
			}
			var calls []shellCall
			for _, response := range turn.responses {
				calls = append(calls, response.calls...)
			}
			if err := events.checkCommands(calls); err != nil {
				t.Fatal(err)
			}
			conversation = events.thread
		}

		requests += len(turn.responses)
		if err := model.verify(i + 1); err != nil {
			t.Fatal(err)
		}
		if got := model.requestCount(); got != requests {
			t.Fatalf("after invocation %d: %d model requests, want %d", i+1, got, requests)
		}
		want := expectAgent(cli.agent, wantMode, turns[:i+1])
		want.secrets = agentCaseSecrets
		if cli.agent == "claude-code" {
			want.conversation = agentSessionID
		}
		generations, err := validateGenerationExports(capture.snapshot(), want)
		if err != nil {
			t.Fatalf("after invocation %d: %v", i+1, err)
		}
		if err := telemetry.validate(want, generations); err != nil {
			t.Fatalf("after invocation %d: %v", i+1, err)
		}
		for j, g := range previous {
			if generations[j].Id != g.Id {
				t.Fatalf("invocation %d changed generation %d from %s to %s", i+1, j, g.Id, generations[j].Id)
			}
		}
		previous = generations
	}
	checkExports(t, name, sc, capture.snapshot())
}

// goToolchainEnv is the environment the test process started with. Package
// variables initialize before TestMain moves HOME, so building the plugin
// with it reuses the real Go module and build caches instead of downloading
// every module into the throwaway HOME.
var goToolchainEnv = os.Environ()

// agentOutcome is how one CLI command ended. exitCode is -1 when no exit
// status was collected: the process never started, timed out, was killed, or
// left its output pipes open past WaitDelay.
type agentOutcome struct {
	argv           []string
	stdout, stderr string
	exitCode       int
	err            error
}

func runAgentProcess(ctx context.Context, dir string, argv []string) agentOutcome {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := newAgentCommand(ctx, dir, argv)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	outcome := agentOutcome{argv: argv, stdout: stdout.String(), stderr: stderr.String(), exitCode: -1, err: err}
	var exitErr *exec.ExitError
	switch {
	case ctx.Err() != nil:
		outcome.err = fmt.Errorf("%w: %v", ctx.Err(), err)
	case err == nil:
		outcome.exitCode = 0
	case errors.As(err, &exitErr):
		outcome.exitCode = exitErr.ExitCode()
	}
	return outcome
}

func (o agentOutcome) trail() string {
	return fmt.Sprintf("$ %s\n[exit %d, err %v]\nstdout:\n%s\nstderr:\n%s", strings.Join(o.argv, " "), o.exitCode, o.err, o.stdout, o.stderr)
}

// check requires a clean exit when failure is empty. Otherwise the process
// must exit nonzero on its own and print failure, so a timeout, a kill, or a
// missing executable never passes as an expected failure.
func (o agentOutcome) check(failure string) error {
	if failure == "" {
		if o.exitCode != 0 || o.err != nil {
			return fmt.Errorf("command failed: %v\n%s", o.err, o.trail())
		}
		return nil
	}
	if o.exitCode <= 0 {
		return fmt.Errorf("command exit %d (%v), want a nonzero exit\n%s", o.exitCode, o.err, o.trail())
	}
	if !strings.Contains(o.stdout+o.stderr, failure) {
		return fmt.Errorf("command output lacks %q\n%s", failure, o.trail())
	}
	return nil
}

func newAgentCommand(ctx context.Context, dir string, argv []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	configureAgentProcess(cmd)
	// Limit pipe draining even if process-tree cancellation fails.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

const (
	fakeToolOutput = "hello-from-fake"
	fakeToolCallID = "call_fake"
)
