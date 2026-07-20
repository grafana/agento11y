// Package entry implements the shared CLI entrypoint behind the
// cmd/agento11y and cmd/agento11y binaries. Both commands are the same single
// binary used by the Claude Code, Codex, Copilot, Cursor, OpenCode, pi, and
// Vibe agent plugins. It accepts:
//
//	agento11y <agent> hook                                — dispatch a JSON hook payload on stdin to <agent>
//	agento11y claude   [--local] [--tag k=v] [-- args...] — exec claude after bootstrapping the sigil-cc plugin
//	agento11y codex    [--local] [--tag k=v] [-- args...] — exec codex after bootstrapping the sigil-codex plugin
//	agento11y copilot  [--local] [--tag k=v] [-- args...] — exec copilot after bootstrapping the sigil-copilot plugin
//	agento11y opencode [--local] [--tag k=v] [-- args...] — exec opencode after bootstrapping the @grafana/agento11y-opencode plugin
//	agento11y pi       [--local] [--tag k=v] [-- args...] — exec pi after bootstrapping the @grafana/agento11y-pi extension
//	agento11y vibe     [--local] [--tag k=v] [-- args...] — exec vibe after installing the sigil hook in vibe's hooks.toml
//	agento11y cursor   install|uninstall                  — wire (or remove) the Cursor hook in ~/.cursor/hooks.json
//	agento11y local start|status|stop                     — manage the local capture daemon
//	agento11y --version                                   — print the build version
//
// --tag is repeatable and adds key=value pairs to SIGIL_TAGS so they land
// on every generation the launched session produces.
//
// Unknown agents and unknown verbs exit with code 2 and a usage message on
// stderr. For hook agents the binary must never crash the calling agent
// process; once argv parsing succeeds, all errors are swallowed (and logged
// when SIGIL_DEBUG=true) and the process exits 0. Launcher agents (`claude`,
// `codex`, `copilot`, `opencode`, `pi`, and `vibe`) are invoked by a human,
// so errors surface on stderr with a non-zero exit code.
package entry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor"
	cursorinstall "github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/install"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/pi"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe"
	"github.com/grafana/agento11y/plugins/agento11y/internal/cli"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/grafana/agento11y/plugins/agento11y/internal/useragent"
)

// Banner used by `agento11y <agent> --local` to call out that local capture
// is on and tell the user where to view the data. Styled to match the
// login banner (Grafana orange, rounded border) so the two surfaces feel
// like one product.
var (
	localBannerOrange = lipgloss.Color("#FF671D")
	localBannerBox    = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(localBannerOrange).
				Padding(0, 1).
				MarginBottom(2)
	localBannerTitle = lipgloss.NewStyle().Bold(true).Foreground(localBannerOrange)
	localBannerLabel = lipgloss.NewStyle().Faint(true)
	localBannerURL   = lipgloss.NewStyle().Underline(true)
)

func renderLocalBanner(uiURL string) string {
	lines := []string{
		localBannerTitle.Render("agento11y local mode"),
		localBannerLabel.Render("Captured agent data stays on this machine."),
		"",
		localBannerLabel.Render("View ") + localBannerURL.Render(uiURL),
	}
	return localBannerBox.Render(strings.Join(lines, "\n"))
}

const usageLine = "usage: agento11y login | agento11y doctor [--json] [--probe] | agento11y local start|status|stop | agento11y cursor install|uninstall | agento11y <agent> hook | agento11y <claude|codex|copilot|opencode|pi|vibe> [--local] [--tag key=value]... [-- args...]"

// version is the build version received from the calling main package via
// Main. It stays a package var (defaulting to "dev") so tests can override
// it.
var version = "dev"

// agentHook is the entrypoint each hook agent adapter exposes.
type agentHook func(ctx context.Context, stdin io.Reader, stdout io.Writer, log *log.Logger) error

// agentLauncher is the entrypoint each launcher agent adapter exposes. It
// owns the user's terminal — args after the `--` separator are forwarded
// unchanged to the underlying CLI via process replacement. localEnv is
// non-nil when the caller requested `--local`, in which case the agent's
// child inherits local-mode SIGIL_* env vars from local.LaunchEnv.Apply.
// binaryVersion is the build version forwarded so launchers can stamp
// update-check state with the version that performed the refresh.
type agentLauncher func(ctx context.Context, args []string, localEnv *local.LaunchEnv, stdin io.Reader, stdout, stderr io.Writer, log *log.Logger, binaryVersion string) error

// agents maps the argv agent name to its adapter Hook. The map is a package
// var so tests can substitute mock hooks.
var agents = map[string]agentHook{
	"claude-code": claudecode.Hook,
	"codex":       codex.Hook,
	"copilot":     copilot.Hook,
	"cursor":      cursor.Hook,
	"vibe":        vibe.Hook,
}

// launchers maps the argv name to its launcher adapter. Launchers are
// invoked directly by a human (no JSON on stdin) and replace the current
// process with the target CLI. The launcher name is the target CLI's own
// name (`claude`, `pi`), not the hook agent name (`claude-code`).
var launchers = map[string]agentLauncher{
	"claude":   claudecode.Launch,
	"codex":    codex.Launch,
	"copilot":  copilot.Launch,
	"opencode": opencode.Launch,
	"pi":       pi.Launch,
	"vibe":     vibe.Launch,
}

// exit is a package var so tests can intercept termination.
var exit = os.Exit

// loginRun is a package var so tests can stub the interactive login flow
// without driving the huh TTY. Production code points at login.Run.
var loginRun = login.Run

// cursorInstall and cursorUninstall are package vars so tests can stub the
// filesystem-touching `agento11y cursor install`/`uninstall` flow.
var (
	cursorInstall   = cursorinstall.Run
	cursorUninstall = cursorinstall.Uninstall
)

// Main is the entrypoint shared by cmd/agento11y and cmd/agento11y.
// buildVersion is the caller's -ldflags-stamped main.version; each main
// package declares its own variable so the -X flag does not depend on this
// module's import path.
func Main(buildVersion string) {
	version = buildVersion
	run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-version") {
		_, _ = fmt.Fprintln(stdout, version)
		return
	}

	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, usageLine)
		exit(2)
		return
	}

	// `agento11y login` is a top-level subcommand handled before launcher and
	// hook dispatch so it can run without a verb argument and without an
	// agent name. It owns its own flag parsing.
	if args[0] == "login" {
		runLoginCommand(args[1:], stderr)
		return
	}

	if args[0] == "local" {
		runLocalCommand(args[1:], stdout, stderr)
		return
	}

	// `agento11y doctor` is a read-only diagnostic, dispatched before launcher
	// dispatch like `local`. It owns its own flag parsing.
	if args[0] == "doctor" {
		runDoctorCommand(args[1:], stdout, stderr)
		return
	}

	// Launcher dispatch handles `sigil <launcher> [--local] [-- args...]`
	// before the hook branch because launchers have no verb (single mode
	// of operation).
	//
	// One exception: when a name appears in both maps (today: `codex`,
	// which is both a launcher and a hook agent), the literal verb `hook`
	// always means hook dispatch. Without this guard `agento11y codex hook`
	// would hit the launcher branch, fail parseLauncherArgs because there
	// is no `--`, and exit 2 — breaking every hook fired by
	// plugins/codex/hooks/hooks.json.
	_, isHookAgent := agents[args[0]]
	isHookCall := len(args) >= 2 && args[1] == "hook" && isHookAgent
	if launcher, ok := launchers[args[0]]; ok && !isHookCall {
		// dotenv must run before parseLauncherArgs so XDG_STATE_HOME set
		// only in $XDG_CONFIG_HOME/agento11y/config.env reaches local.StateDir()
		// when --local is used. Otherwise the daemon dir is resolved against
		// the wrong root.
		dotenv.ApplyEnv(nil)
		launcherArgs, localEnv, ok := parseLauncherArgs(args[0], args[1:], stderr)
		if !ok {
			return
		}

		logger := cli.InitLogger(args[0])

		// Auto-prompt for credentials on first run. login.Run returns
		// ErrNotInteractive when stdin is not a TTY (e.g. CI, piped input);
		// in that case we silently fall through to exec, matching the
		// previous behaviour where hooks just emit a "missing credentials"
		// line on stderr. A failed or aborted login does not block the
		// launch — the user explicitly asked to start claude/pi, and we
		// don't want sigil to gate that on its own setup.
		//
		// In --local mode we never prompt: the launcher will inject
		// placeholder credentials so the SDK proceeds without contacting
		// Grafana Cloud.
		if localEnv == nil && !dotenv.HasCredentials() {
			err := loginRun(context.Background(), login.RunOpts{
				Stderr: stderr,
				Logger: logger,
			})
			switch {
			case err == nil, errors.Is(err, login.ErrNotInteractive):
				// either succeeded or no TTY; continue.
			case errors.Is(err, login.ErrAborted):
				_, _ = fmt.Fprintln(stderr, "agento11y: setup aborted; continuing without capture")
			default:
				logger.Printf("auto-login: %v", err)
				_, _ = fmt.Fprintf(stderr, "agento11y: setup failed (%v); continuing without capture\n", err)
			}
		}
		// Launcher panics must surface to the user (non-zero exit, message on
		// stderr) — log to the debug file, then re-panic so the Go runtime
		// reports it. cli.RecoverAndLog would silently swallow the panic and
		// exit 0, which is the hook-agent contract, not the launcher one.
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("launch %s: panic: %v", args[0], r)
				panic(r)
			}
		}()

		if err := launcher(context.Background(), launcherArgs, localEnv, stdin, stdout, stderr, logger, version); err != nil {
			logger.Printf("launch %s: %v", args[0], err)
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		return
	}

	if len(args) < 2 {
		_, _ = fmt.Fprintln(stderr, usageLine)
		exit(2)
		return
	}

	agent, verb := args[0], args[1]
	hook, ok := agents[agent]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown agent %q\n", agent)
		exit(2)
		return
	}

	// Cursor has no launcher (it is a GUI app), so `agento11y cursor install`
	// wires its hooks directly. This branch sits before the generic
	// non-`hook` verb rejection below so `install`/`uninstall` reach the
	// installer while `agento11y cursor hook` still falls through to dispatch.
	if agent == "cursor" && (verb == "install" || verb == "uninstall") {
		runCursorInstall(verb, stdout, stderr)
		return
	}

	if verb != "hook" {
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown verb %q (only \"hook\" supported)\n", verb)
		exit(2)
		return
	}

	// Propagate the build version to the claude-code adapter so its hook
	// evaluation request carries the right agent_version. Other adapters
	// don't need it yet.
	claudecode.Version = version

	// Propagate the build version to the generation-export User-Agent so each
	// agent plugin identifies itself, e.g. "agento11y-plugin-cursor/<ver> ...".
	useragent.Version = version

	// Apply the dotenv file before initialising the logger so SIGIL_DEBUG=true
	// set only in $XDG_CONFIG_HOME/agento11y/config.env still enables file logging.
	// Cursor (and Codex headless) launch hooks under a stripped environment
	// where the dotenv is the only place SIGIL_DEBUG could come from.
	dotenv.ApplyEnv(nil)
	logger := cli.InitLogger(agent)
	defer cli.RecoverAndLog(logger)

	if err := hook(context.Background(), stdin, stdout, logger); err != nil {
		logger.Printf("hook: %v", err)
	}
}

// runLoginCommand handles `agento11y login`. The flow is interactive-only: any
// args (including unknown flags) are rejected with exit 2. Non-interactive
// callers should set SIGIL_* env vars or edit $XDG_CONFIG_HOME/agento11y/config.env
// directly.
func runLoginCommand(args []string, stderr io.Writer) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: agento11y login")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Interactively save agento11y credentials to $XDG_CONFIG_HOME/agento11y/config.env")
		_, _ = fmt.Fprintln(stderr, "(or the old $XDG_CONFIG_HOME/sigil/config.env if only that file exists).")
	}
	if err := fs.Parse(args); err != nil {
		exit(2)
		return
	}
	if fs.NArg() > 0 {
		fs.Usage()
		exit(2)
		return
	}

	dotenv.ApplyEnv(nil)
	logger := cli.InitLogger("login")

	err := loginRun(context.Background(), login.RunOpts{
		// Only the explicit `agento11y login` shows the “Try sigil claude/pi”
		// hint. The launcher auto-prompt path leaves this false because the
		// launcher is about to exec the agent anyway.
		ShowNextStep: true,
		Stderr:       stderr,
		Logger:       logger,
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, login.ErrAborted):
		_, _ = fmt.Fprintln(stderr, "Aborted.")
		return
	case errors.Is(err, login.ErrNotInteractive):
		_, _ = fmt.Fprintln(stderr, "agento11y login: cannot prompt because stdin is not a terminal. Run from an interactive shell, or set SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID and SIGIL_AUTH_TOKEN in your environment.")
		exit(1)
		return
	default:
		_, _ = fmt.Fprintf(stderr, "agento11y: login failed: %v\n", err)
		exit(1)
		return
	}
}

// runCursorInstall handles `agento11y cursor install` and `sigil cursor
// uninstall`. install wires agento11y's hook into ~/.cursor/hooks.json and, when
// no credentials are configured yet, chains the interactive login prompt the
// same way the launchers do; uninstall removes the hook entries.
func runCursorInstall(verb string, stdout, stderr io.Writer) {
	// dotenv must run before InitLogger so SIGIL_DEBUG=true set only in
	// $XDG_CONFIG_HOME/agento11y/config.env still enables file logging, and
	// before HasCredentials so dotenv-supplied credentials are visible.
	dotenv.ApplyEnv(nil)
	logger := cli.InitLogger("cursor")

	if verb == "uninstall" {
		if err := cursorUninstall(stdout, stderr, logger); err != nil {
			logger.Printf("cursor uninstall: %v", err)
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
		}
		return
	}

	if err := cursorInstall(stdout, stderr, logger); err != nil {
		logger.Printf("cursor install: %v", err)
		_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
		exit(1)
		return
	}

	// Wiring the hook does nothing without credentials, so chain the login
	// prompt on first install, mirroring the launcher auto-prompt. login.Run
	// returns ErrNotInteractive when stdin is not a TTY (CI, piped input), in
	// which case we skip silently and leave `agento11y login` for later. A failed
	// or aborted login never fails the install: the hook is already wired.
	if !dotenv.HasCredentials() {
		err := loginRun(context.Background(), login.RunOpts{
			Stderr: stderr,
			Logger: logger,
		})
		switch {
		case err == nil, errors.Is(err, login.ErrNotInteractive):
			// either succeeded or no TTY; nothing to report.
		case errors.Is(err, login.ErrAborted):
			_, _ = fmt.Fprintln(stderr, "agento11y: setup aborted; run `agento11y login` when ready")
		default:
			logger.Printf("auto-login: %v", err)
			_, _ = fmt.Fprintf(stderr, "agento11y: setup failed (%v); run `agento11y login` when ready\n", err)
		}
	}
}

// runDoctorCommand handles `agento11y doctor`. doctor is strictly read-only and
// owns its own flag parsing. The OS environment is snapshotted before dotenv
// is applied so doctor can attribute each value to the OS env vs config.env.
func runDoctorCommand(args []string, stdout, stderr io.Writer) {
	osEnv := doctor.SnapshotEnv()
	dotenv.ApplyEnv(nil)
	code := doctor.Run(context.Background(), args, doctor.Params{
		Version: version,
		OSEnv:   osEnv,
		Stdout:  stdout,
		Stderr:  stderr,
	})
	if code != 0 {
		exit(code)
	}
}

// parseLauncherArgs splits sigil-side tokens from forwarded args at the
// first `--`. Recognised sigil-side flags are:
//   - `--local`, which redirects the launched agent at the local receiver.
//   - `--tag key=value` (repeatable; also `--tag=key=value`), which adds
//     a tag to SIGIL_TAGS so it lands on every generation the session
//     produces. Flag tags merge onto (and override) any SIGIL_TAGS already
//     in the environment.
//
// Any other token before `--` is an error.
//
// Returns the forwarded args plus a non-nil *local.LaunchEnv when --local
// was set; the env values point at the local daemon. When --tag is used,
// SIGIL_TAGS is updated in the current process environment so the exec'd
// child (which inherits os.Environ via local.Environ) sees it.
//
// Diagnostics distinguish two cases:
//   - No `--` and there are unrecognised tokens: the user probably
//     forgot the separator, so we point them at `agento11y <name> -- <args>`.
//   - `--` is present but unrecognised tokens precede it: those are
//     genuinely unknown sigil-side options, so we name them explicitly.
func parseLauncherArgs(name string, rest []string, stderr io.Writer) ([]string, *local.LaunchEnv, bool) {
	sep := -1
	for i, a := range rest {
		if a == "--" {
			sep = i
			break
		}
	}

	var launcherSide []string
	var forwarded []string
	if sep < 0 {
		launcherSide = rest
	} else {
		launcherSide = rest[:sep]
		forwarded = rest[sep+1:]
	}

	localRequested := false
	var flagTags []string
	var unknown []string
	for i := 0; i < len(launcherSide); i++ {
		tok := launcherSide[i]
		switch {
		case tok == "--local":
			localRequested = true
		case tok == "--tag":
			if i+1 >= len(launcherSide) {
				_, _ = fmt.Fprintln(stderr, "agento11y: --tag requires a key=value argument")
				exit(2)
				return nil, nil, false
			}
			i++
			kv, ok := normalizeTag(launcherSide[i])
			if !ok {
				_, _ = fmt.Fprintf(stderr, "agento11y: invalid --tag %q (want key=value)\n", launcherSide[i])
				exit(2)
				return nil, nil, false
			}
			flagTags = append(flagTags, kv)
		case strings.HasPrefix(tok, "--tag="):
			raw := strings.TrimPrefix(tok, "--tag=")
			kv, ok := normalizeTag(raw)
			if !ok {
				_, _ = fmt.Fprintf(stderr, "agento11y: invalid --tag %q (want key=value)\n", raw)
				exit(2)
				return nil, nil, false
			}
			flagTags = append(flagTags, kv)
		default:
			unknown = append(unknown, tok)
		}
	}

	if len(unknown) > 0 {
		if sep < 0 {
			_, _ = fmt.Fprintf(stderr, "agento11y: use `agento11y %s -- <args>` to forward args to %[1]s\n", name)
		} else {
			_, _ = fmt.Fprintf(stderr, "agento11y: unknown options before `--`: %v\n", unknown)
		}
		exit(2)
		return nil, nil, false
	}

	if len(flagTags) > 0 {
		// Merge onto the effective selected tags and write the result under
		// both branded spellings so old child processes see it too.
		envconfig.SetBothEnv("TAGS", mergeTags(envconfig.Getenv("TAGS"), flagTags))
	}

	var localEnv *local.LaunchEnv
	if localRequested {
		endpoint, otlp, err := setupLocalLaunch(stderr)
		if err != nil {
			exit(1)
			return nil, nil, false
		}
		localEnv = &local.LaunchEnv{Endpoint: endpoint, OTLPEndpoint: otlp}
	}
	return forwarded, localEnv, true
}

// normalizeTag validates a `--tag` value and returns it as a trimmed
// `key=value` pair. The key must be non-empty; the value may be empty
// (matching the SDK's SIGIL_TAGS parser, which keeps empty values). ok is
// false when the token has no `=` or an empty key.
func normalizeTag(raw string) (string, bool) {
	rawKey, rawValue, ok := strings.Cut(raw, "=")
	if !ok {
		return "", false
	}
	key := strings.TrimSpace(rawKey)
	if key == "" {
		return "", false
	}
	return key + "=" + strings.TrimSpace(rawValue), true
}

// mergeTags layers flag-supplied `key=value` tags onto an existing
// SIGIL_TAGS CSV value and returns the merged CSV. Existing keys keep their
// position but take the flag's value (flags win); new keys are appended in
// flag order. Malformed existing entries (no `=`, empty key) are dropped,
// matching the SDK's parseCSVKV. flagTags entries are assumed already
// normalised by normalizeTag.
func mergeTags(existing string, flagTags []string) string {
	var order []string
	vals := map[string]string{}
	add := func(k, v string) {
		if _, seen := vals[k]; !seen {
			order = append(order, k)
		}
		vals[k] = v
	}
	for part := range strings.SplitSeq(existing, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		rawKey, rawValue, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		key := strings.TrimSpace(rawKey)
		if key == "" {
			continue
		}
		add(key, strings.TrimSpace(rawValue))
	}
	for _, t := range flagTags {
		key, value, _ := strings.Cut(t, "=")
		add(key, value)
	}
	parts := make([]string, 0, len(order))
	for _, k := range order {
		parts = append(parts, k+"="+vals[k])
	}
	return strings.Join(parts, ",")
}

// setupLocalLaunch starts the local receiver if needed and returns the
// endpoint URLs the launcher should pass to the agent.
func setupLocalLaunch(stderr io.Writer) (endpoint, otlp string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := local.StateDir()
	status, err := local.EnsureRunning(ctx, dir, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: failed to start local receiver: %v\n", err)
		return "", "", err
	}

	endpoint = status.Endpoint
	otlp = status.Endpoint + "/otlp"

	_, _ = fmt.Fprintln(stderr, renderLocalBanner(status.Endpoint))
	return endpoint, otlp, nil
}

// runLocalCommand dispatches `agento11y local <verb>` subcommands.
func runLocalCommand(args []string, stdout, stderr io.Writer) {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, "usage: agento11y local start | status | stop | restart | serve")
		exit(2)
		return
	}
	// Apply dotenv before resolving the state dir so XDG_STATE_HOME set
	// only in $XDG_CONFIG_HOME/agento11y/config.env reaches local.StateDir().
	// Each verb relies on that resolution.
	dotenv.ApplyEnv(nil)
	dir := local.StateDir()
	switch args[0] {
	case "start":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := local.EnsureRunning(ctx, dir, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		_, _ = fmt.Fprintf(stdout, "agento11y local receiver running at %s (pid %d)\n", status.Endpoint, status.PID)
	case "status":
		status, err := local.IsRunning(dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		if status == nil {
			_, _ = fmt.Fprintln(stdout, "agento11y local receiver: not running")
			return
		}
		_, _ = fmt.Fprintf(stdout, "agento11y local receiver: running at %s (pid %d, started %s)\n", status.Endpoint, status.PID, status.StartedAt)
	case "stop":
		stopped, err := local.Stop(dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		if !stopped {
			_, _ = fmt.Fprintln(stdout, "agento11y local receiver: not running")
			return
		}
		_, _ = fmt.Fprintln(stdout, "agento11y local receiver stopped")
	case "restart":
		// `stop` errors only when the daemon is running but unkillable;
		// treat "not running" as already-stopped and proceed to start.
		if _, err := local.Stop(dir); err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		status, err := local.EnsureRunning(ctx, dir, nil)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
		_, _ = fmt.Fprintf(stdout, "agento11y local receiver running at %s (pid %d)\n", status.Endpoint, status.PID)
	case "serve":
		// Internal: invoked by the daemon child. Blocks until SIGTERM.
		logger := cli.InitLogger("local")
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer cancel()
		if err := local.Serve(ctx, dir, local.DefaultPort, logger); err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			exit(1)
			return
		}
	default:
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown local verb %q\n", args[0])
		exit(2)
	}
}
