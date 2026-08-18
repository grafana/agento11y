// Package entry implements the shared CLI entrypoint behind the
// cmd/agento11y and cmd/agento11y binaries. Both commands are the same single
// binary used by the Claude Code, Codex, Copilot, Cursor, OpenCode, pi, and
// Vibe agent plugins. It accepts:
//
//	agento11y <agent> hook                                            — dispatch a JSON hook payload on stdin to <agent>
//	agento11y claude   [--local|--no-local] [--tag k=v] [-- args...]  — exec claude after bootstrapping the agento11y-claude-code plugin
//	agento11y codex    [--local|--no-local] [--tag k=v] [-- args...]  — exec codex after bootstrapping the agento11y-codex plugin
//	agento11y copilot  [--local|--no-local] [--tag k=v] [-- args...]  — exec copilot after bootstrapping the sigil-copilot plugin
//	agento11y opencode [--local|--no-local] [--tag k=v] [-- args...]  — exec opencode after bootstrapping the @grafana/agento11y-opencode plugin
//	agento11y pi       [--local|--no-local] [--tag k=v] [-- args...]  — exec pi after bootstrapping the @grafana/agento11y-pi extension
//	agento11y vibe     [--local|--no-local] [--tag k=v] [-- args...]  — exec vibe after installing the sigil hook in vibe's hooks.toml
//	agento11y cursor   install|uninstall                              — wire (or remove) the Cursor hook in ~/.cursor/hooks.json
//	agento11y local start|status|stop                                 — manage the local capture daemon
//	agento11y history import <agent> [flags]                          — backfill an agent's existing local sessions
//	agento11y skills list|show <name>                                 — print an agent skill bundled into this binary
//	agento11y help                                                    — print the expanded command list
//	agento11y --version                                               — print the build version
//
// --tag is repeatable and adds key=value pairs to SIGIL_TAGS so they land
// on every generation the launched session produces.
//
// --local can also be turned on for every launch — and for hook dispatch —
// with AGENTO11Y_LOCAL=true in the shell or in config.env. --no-local runs
// one session against Cloud while that setting stays on.
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
	"path/filepath"
	"strings"
	"sync"
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
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
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

// renderLocalBanner draws the local-mode banner. envKey names the variable that
// turned local mode on (AGENTO11Y_LOCAL or the legacy SIGIL_LOCAL), and is
// empty when a flag on this command line did.
func renderLocalBanner(uiURL string, posture local.ForwardPosture, postureErr error, envKey string) string {
	privacy := localPrivacyLines(posture, postureErr == nil)
	lines := make([]string, 0, len(privacy)+3)
	title := localBannerTitle.Render("agento11y local mode")
	if envKey != "" {
		title += "  " + localBannerLabel.Render("(enabled by "+envKey+")")
	}
	lines = append(lines, title)
	for _, line := range privacy {
		lines = append(lines, localBannerLabel.Render(line))
	}
	lines = append(lines, "", localBannerLabel.Render("View ")+localBannerURL.Render(uiURL))
	return localBannerBox.Render(strings.Join(lines, "\n"))
}

// localPrivacyLines describes what leaves the machine in this session. The
// daemon is shared and re-reads config.env, so the claim has to come from the
// posture it reports rather than from the fact that --local was passed.
func localPrivacyLines(posture local.ForwardPosture, known bool) []string {
	switch {
	case !known:
		// The daemon did not answer. Say what is certain (the local store)
		// rather than guess at the forwarding posture.
		return []string{"Captured agent data is recorded on this machine."}
	case !posture.Enabled:
		return []string{"Captured agent data stays on this machine."}
	case posture.Hooks:
		return []string{
			"Captured agent data is also forwarded to Grafana Cloud (" + posture.Mode + ").",
			"Guard checks send tool calls, and any conversation checked before it is sent",
			"to the model, to Cloud regardless of that mode.",
		}
	default:
		return []string{"Captured agent data is also forwarded to Grafana Cloud (" + posture.Mode + ")."}
	}
}

// usageLine is a function rather than a constant because the history agents
// come from the importer registry: adding an importer must not need an edit
// here.
func usageLine() string {
	return "usage: agento11y login [--endpoint url] [--tenant id] [--token value|--token-stdin] " +
		"[--otlp-endpoint url] [--no-verify] [--yes] | agento11y doctor [--json] | " +
		"agento11y skills list|show <name> | agento11y local start|status|stop | " +
		"agento11y history import <" + historyAgentNames() + "> | agento11y cursor install|uninstall | agento11y <agent> hook | " +
		"agento11y <claude|codex|copilot|opencode|pi|vibe> [--local|--no-local] [--tag key=value]... [-- args...]"
}

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
		_, _ = fmt.Fprintln(stderr, usageLine())
		exit(2)
		return
	}

	// `agento11y help` answers on stdout and exits 0, unlike the arity guard
	// below, which is misuse and writes the one-line usage form to stderr.
	if args[0] == "help" || args[0] == "--help" || args[0] == "-h" {
		runHelpCommand(stdout)
		return
	}

	// `agento11y login` is a top-level subcommand handled before launcher and
	// hook dispatch so it can run without a verb argument and without an
	// agent name. It owns its own flag parsing.
	if args[0] == "login" {
		runLoginCommand(args[1:], stdin, stderr)
		return
	}

	if args[0] == "local" {
		runLocalCommand(args[1:], stdout, stderr)
		return
	}

	// `agento11y history import <agent>` backfills sessions an agent wrote
	// before agento11y was installed. It is dispatched here, alongside `local`,
	// because it is a top-level verb rather than an agent name.
	if args[0] == "history" {
		runHistoryCommand(args[1:], stdin, stdout, stderr)
		return
	}

	// `agento11y doctor` is a read-only diagnostic, dispatched before launcher
	// dispatch like `local`. It owns its own flag parsing.
	if args[0] == "doctor" {
		runDoctorCommand(args[1:], stdout, stderr)
		return
	}

	// `agento11y skills` sits next to `doctor`, above launcher and hook
	// dispatch, so an agent named `skills` cannot shadow it.
	if args[0] == skills.Command {
		runSkillsCommand(args[1:], stdout, stderr)
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
		//
		// The LOCAL family is read around that merge, not after it: ApplyEnv
		// writes the winning value under both spellings, which erases which one
		// the user set, and the banner names that spelling.
		localValue, localKey, inShell := envconfig.LookupEnv("LOCAL")
		fileEnv := dotenv.ApplyEnv(nil)
		if !inShell {
			localValue, localKey, _ = envconfig.LookupMap(fileEnv, "LOCAL")
		}
		envLocal := localEnvRequest{on: envconfig.ParseBool(localValue), key: localKey}

		launcherArgs, localEnv, ok := parseLauncherArgs(args[0], args[1:], stderr, envLocal)
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
			case errors.Is(err, login.ErrNotVerified):
				logger.Printf("auto-login: %v", err)
				_, _ = fmt.Fprintln(stderr, "agento11y: credentials not saved; continuing without capture")
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
		_, _ = fmt.Fprintln(stderr, usageLine())
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
	applyLocalHookEnv(logger)

	if err := hook(context.Background(), stdin, stdout, logger); err != nil {
		logger.Printf("hook: %v", err)
	}
}

// hookLocalStartTimeout is the same deadline launchers use. It must be
// longer than startDaemon's 5s readiness wait: a shorter context cancels
// that poll, kills a healthy child, and then the failure stamp skips
// retries while the hook still points at a dead loopback URL. Repeat
// start cost after a real failure is bounded by skipLocalHookStart, not
// by shrinking this timeout.
const hookLocalStartTimeout = 10 * time.Second

// hookStartFailFile / hookStartFailTTL remember a failed start across
// hook processes (Cursor execs a new binary per event). A healthy
// IsRunning check still wins, so a later successful `agento11y local start`
// is picked up without waiting out the TTL.
const (
	hookStartFailFile = "hook-start-failed"
	hookStartFailTTL  = 30 * time.Second
)

var (
	localHookStartMu     sync.Mutex
	localHookStartFailed bool
	// hookLocalReceiverSupported is a test seam for the Windows path,
	// where the local receiver cannot start and hooks must keep Cloud
	// credentials instead of rewriting to loopback.
	hookLocalReceiverSupported = local.ReceiverSupported
)

// applyLocalHookEnv rewrites the hook process so AGENTO11Y_LOCAL=true (or
// SIGIL_LOCAL) sends generations to the local daemon instead of Grafana
// Cloud. Launchers already do this for `agento11y <agent>`; Cursor, Copilot,
// Vibe, and a plain `claude`/`codex` whose agento11y hooks are installed
// have no launcher wrap, so the hook itself applies the same LaunchEnv
// contract.
//
// The rewrite is silent: hooks fire many times per turn and must not print
// the local-mode banner to the host agent's stderr. A failure to start the
// daemon is logged and the env still points at loopback so a down receiver
// cannot leak the turn to Cloud. A later hook in this process, or a later
// process within hookStartFailTTL, skips the start attempt unless a
// receiver is already healthy. On platforms where the receiver cannot
// run (Windows), the Cloud endpoint is left in place.
func applyLocalHookEnv(logger *log.Logger) {
	if !envconfig.ParseBool(envconfig.Getenv("LOCAL")) {
		return
	}
	if !hookLocalReceiverSupported() {
		logger.Printf("local hook: receiver not supported on this OS; exporting to the configured endpoint")
		return
	}
	endpoint := fmt.Sprintf("http://127.0.0.1:%d", local.DefaultPort)
	dir := local.StateDir()
	if status, err := local.IsRunning(dir); err == nil && status != nil && status.Endpoint != "" {
		endpoint = status.Endpoint
		clearLocalHookStartFailure(dir)
	} else if !skipLocalHookStart(dir) {
		ctx, cancel := context.WithTimeout(context.Background(), hookLocalStartTimeout)
		status, err := local.EnsureRunning(ctx, dir, logger)
		cancel()
		if err != nil {
			logger.Printf("local hook: failed to start receiver: %v", err)
			markLocalHookStartFailure(dir)
		} else if status != nil && status.Endpoint != "" {
			endpoint = status.Endpoint
			clearLocalHookStartFailure(dir)
		}
	}
	local.LaunchEnv{Endpoint: endpoint, OTLPEndpoint: endpoint + "/otlp"}.ApplyOS()
	logger.Printf("local hook: endpoint=%s", endpoint)
}

func skipLocalHookStart(dir string) bool {
	localHookStartMu.Lock()
	failed := localHookStartFailed
	localHookStartMu.Unlock()
	if failed {
		return true
	}
	raw, err := os.ReadFile(filepath.Join(dir, hookStartFailFile))
	if err != nil {
		return false
	}
	stamped, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return false
	}
	return time.Since(stamped) < hookStartFailTTL
}

func markLocalHookStartFailure(dir string) {
	localHookStartMu.Lock()
	localHookStartFailed = true
	localHookStartMu.Unlock()
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, hookStartFailFile), []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o600)
}

func clearLocalHookStartFailure(dir string) {
	localHookStartMu.Lock()
	localHookStartFailed = false
	localHookStartMu.Unlock()
	_ = os.Remove(filepath.Join(dir, hookStartFailFile))
}

func resetLocalHookStartFailureForTest() {
	localHookStartMu.Lock()
	localHookStartFailed = false
	localHookStartMu.Unlock()
}

// runLoginCommand handles `agento11y login`. Values can arrive as flags, on
// stdin (--token-stdin), or from the prompt; whatever is still missing after
// the flags is asked for, and a run that supplies --endpoint, --tenant, and a
// token needs no terminal at all. Misuse of the flags exits 2, a refused or
// failed save exits 1.
func runLoginCommand(args []string, stdin io.Reader, stderr io.Writer) {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: agento11y login [--endpoint url] [--tenant id] [--token value|--token-stdin] [--otlp-endpoint url] [--no-verify] [--yes]")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Save agento11y credentials to $XDG_CONFIG_HOME/agento11y/config.env")
		_, _ = fmt.Fprintln(stderr, "(or the old $XDG_CONFIG_HOME/sigil/config.env if only that file exists).")
		_, _ = fmt.Fprintln(stderr, "Values not given as flags are prompted for. Login asks for your Grafana stack,")
		_, _ = fmt.Fprintln(stderr, "prints that stack's coding-agent setup page, and tries to open it. Paste the")
		_, _ = fmt.Fprintln(stderr, "environment block from that page to fill every credential. Before writing the")
		_, _ = fmt.Fprintln(stderr, "file, login sends one request to the endpoint to check the credentials,")
		_, _ = fmt.Fprintln(stderr, "unless --no-verify is passed.")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "  --endpoint url        conversations API URL")
		_, _ = fmt.Fprintln(stderr, "  --tenant id           instance ID")
		_, _ = fmt.Fprintln(stderr, "  --token value         access-policy token with the sigil:write scope")
		_, _ = fmt.Fprintln(stderr, "  --token-stdin         read the token from stdin; needs --endpoint and --tenant")
		_, _ = fmt.Fprintln(stderr, "  --otlp-endpoint url   OTLP endpoint for SDK traces and metrics")
		_, _ = fmt.Fprintln(stderr, "  --no-verify           write the file without checking the credentials")
		_, _ = fmt.Fprintln(stderr, "  --yes                 save even when the check fails")
	}
	var (
		endpoint     string
		tenant       string
		token        string
		otlpEndpoint string
		tokenStdin   bool
		noVerify     bool
		assumeYes    bool
	)
	fs.StringVar(&endpoint, "endpoint", "", "conversations API URL")
	fs.StringVar(&tenant, "tenant", "", "instance ID")
	fs.StringVar(&token, "token", "", "access-policy token")
	fs.BoolVar(&tokenStdin, "token-stdin", false, "read the token from stdin")
	fs.StringVar(&otlpEndpoint, "otlp-endpoint", "", "OTLP endpoint")
	fs.BoolVar(&noVerify, "no-verify", false, "skip the credential check")
	fs.BoolVar(&assumeYes, "yes", false, "save even when the check fails")
	if err := fs.Parse(args); err != nil {
		exit(2)
		return
	}
	if fs.NArg() > 0 {
		_, _ = fmt.Fprintf(stderr, "agento11y login: unexpected arguments: %v\n", fs.Args())
		fs.Usage()
		exit(2)
		return
	}

	if tokenStdin {
		var ok bool
		token, ok = readTokenStdin(fs, endpoint, tenant, stdin, stderr)
		if !ok {
			exit(2)
			return
		}
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
		Endpoint:     endpoint,
		TenantID:     tenant,
		Token:        token,
		OTLPEndpoint: otlpEndpoint,
		SkipVerify:   noVerify,
		AssumeYes:    assumeYes,
	})
	switch {
	case err == nil:
		return
	case errors.Is(err, login.ErrAborted):
		_, _ = fmt.Fprintln(stderr, "Aborted.")
		return
	case errors.Is(err, login.ErrNotVerified):
		_, _ = fmt.Fprintln(stderr, "agento11y login: nothing was saved. Fix the values and run `agento11y login` again, or pass --yes to save them as they are.")
		exit(1)
		return
	case errors.Is(err, login.ErrNotInteractive):
		_, _ = fmt.Fprintln(stderr, "agento11y login: cannot prompt because stdin is not a terminal. Run from an interactive shell, pass --endpoint, --tenant and --token (or --token-stdin), or set AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TENANT_ID and AGENTO11Y_AUTH_TOKEN in your environment (the legacy SIGIL_* names still work).")
		exit(1)
		return
	default:
		_, _ = fmt.Fprintf(stderr, "agento11y: login failed: %v\n", err)
		exit(1)
		return
	}
}

// readTokenStdin reads the token for --token-stdin. Consuming stdin leaves
// the prompt library without an input source, so the flag is only accepted
// when --endpoint and --tenant supply the other two required values and no
// value has to be asked for. (A failed credential check can still ask "Save
// anyway?" when stderr is a terminal; pass --yes or --no-verify to keep a
// script from stopping there.) --token names a second source for one value,
// so the two flags cannot be combined. ok is false when the caller must exit
// with a usage error.
func readTokenStdin(fs *flag.FlagSet, endpoint, tenant string, stdin io.Reader, stderr io.Writer) (string, bool) {
	passed := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { passed[f.Name] = true })
	if passed["token"] {
		_, _ = fmt.Fprintln(stderr, "agento11y login: --token and --token-stdin are mutually exclusive; pass the token one way only")
		return "", false
	}

	var missing []string
	if strings.TrimSpace(endpoint) == "" {
		missing = append(missing, "--endpoint")
	}
	if strings.TrimSpace(tenant) == "" {
		missing = append(missing, "--tenant")
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(stderr, "agento11y login: --token-stdin also requires %s\n", strings.Join(missing, " and "))
		return "", false
	}

	data, err := io.ReadAll(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y login: could not read the token from stdin: %v\n", err)
		return "", false
	}
	// A token is one line. Surrounding whitespace is dropped, which is what
	// login.Run does with every value anyway. An embedded newline is rejected
	// rather than trimmed: the config file stores one value per line, so
	// writing such a token would split the line and corrupt the file.
	token := strings.TrimSpace(string(data))
	if token == "" {
		_, _ = fmt.Fprintln(stderr, "agento11y login: --token-stdin was passed but stdin carried no token")
		return "", false
	}
	if strings.ContainsAny(token, "\r\n") {
		_, _ = fmt.Fprintln(stderr, "agento11y login: the token read from stdin spans more than one line")
		return "", false
	}
	return token, true
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
		case errors.Is(err, login.ErrNotVerified):
			logger.Printf("auto-login: %v", err)
			_, _ = fmt.Fprintln(stderr, "agento11y: credentials not saved; run `agento11y login` when ready")
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

// localEnvRequest is the LOCAL family as the launcher acts on it: whether it
// turns local mode on, and the spelling that set it so diagnostics can name a
// variable the user actually set.
type localEnvRequest struct {
	on  bool
	key string
}

// parseLauncherArgs splits sigil-side tokens from forwarded args at the
// first `--`. Recognised sigil-side flags are:
//   - `--local`, which redirects the launched agent at the local receiver.
//     envLocal carries the same request from the LOCAL env family, which the
//     caller resolves across the shell and config.env.
//   - `--no-local`, which forces a Cloud session for this run. It wins over
//     both `--local` and the env family, whatever the argument order, so a
//     user with local mode on by default can opt out once.
//   - `--tag key=value` (repeatable; also `--tag=key=value`), which adds
//     a tag to SIGIL_TAGS so it lands on every generation the session
//     produces. Flag tags merge onto (and override) any SIGIL_TAGS already
//     in the environment.
//
// Any other token before `--` is an error.
//
// Returns the forwarded args plus a non-nil *local.LaunchEnv when the session
// is local, that is when `--local` or the env family asked for it and
// `--no-local` did not; the env values point at the local daemon. When --tag is used,
// SIGIL_TAGS is updated in the current process environment so the exec'd
// child (which inherits os.Environ via local.Environ) sees it.
//
// Diagnostics distinguish two cases:
//   - No `--` and there are unrecognised tokens: the user probably
//     forgot the separator, so we point them at `agento11y <name> -- <args>`.
//   - `--` is present but unrecognised tokens precede it: those are
//     genuinely unknown sigil-side options, so we name them explicitly.
func parseLauncherArgs(name string, rest []string, stderr io.Writer, envLocal localEnvRequest) ([]string, *local.LaunchEnv, bool) {
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

	localFlag := false
	noLocalFlag := false
	var flagTags []string
	var unknown []string
	for i := 0; i < len(launcherSide); i++ {
		tok := launcherSide[i]
		switch {
		case tok == "--local":
			localFlag = true
		case tok == "--no-local":
			noLocalFlag = true
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

	if noLocalFlag {
		// The agent, its hooks, and any nested agento11y call inherit this
		// environment, where dotenv already materialized the family under both
		// spellings. Leaving it true there would describe a session that is not
		// local. --tag writes back for the same reason.
		envconfig.SetBothEnv("LOCAL", "false")
	}

	var localEnv *local.LaunchEnv
	if !noLocalFlag && (localFlag || envLocal.on) {
		// An explicit --local speaks for itself, so name the variable only when
		// it is what turned local mode on.
		sourceKey := envLocal.key
		if localFlag {
			sourceKey = ""
		}
		endpoint, otlp, err := setupLocalLaunch(stderr, sourceKey)
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
// endpoint URLs the launcher should pass to the agent. envKey names the
// variable that turned local mode on, or is empty when a flag did.
func setupLocalLaunch(stderr io.Writer, envKey string) (endpoint, otlp string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	dir := local.StateDir()
	status, err := local.EnsureRunning(ctx, dir, nil)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: failed to start local receiver: %v\n", err)
		if envKey != "" {
			// The user did not ask for local mode in this command, so the
			// failure looks like the launcher is broken. Name the setting and
			// the way past it.
			_, _ = fmt.Fprintf(stderr, "agento11y: local mode is on because %s is set; pass --no-local to run this session against Grafana Cloud\n", envKey)
		}
		return "", "", err
	}

	endpoint = status.Endpoint
	otlp = status.Endpoint + "/otlp"

	posture, postureErr := local.FetchForwardPosture(ctx, status.Endpoint)
	if postureErr != nil {
		// The banner falls back to wording that is true in every posture, which
		// on its own reads like "nothing is forwarded". Say why it is hedged.
		_, _ = fmt.Fprintf(stderr, "agento11y: could not read the daemon's forwarding posture: %v\n", postureErr)
	}
	_, _ = fmt.Fprintln(stderr, renderLocalBanner(status.Endpoint, posture, postureErr, envKey))
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
