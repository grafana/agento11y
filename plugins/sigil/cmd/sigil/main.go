// Command sigil is the single binary used by the Claude Code, Codex, Cursor,
// and pi agent plugins. It accepts:
//
//	sigil <agent> hook           — dispatch a JSON hook payload on stdin to <agent>
//	sigil claude [-- args...]    — exec claude after bootstrapping the sigil-cc plugin
//	sigil pi [-- args...]        — exec pi after bootstrapping the @grafana/sigil-pi extension
//	sigil --version              — print the build version
//
// Unknown agents and unknown verbs exit with code 2 and a usage message on
// stderr. For hook agents the binary must never crash the calling agent
// process; once argv parsing succeeds, all errors are swallowed (and logged
// when SIGIL_DEBUG=true) and the process exits 0. Launcher agents (`claude`
// and `pi`) are invoked by a human, so errors surface on stderr with a
// non-zero exit code.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/pi"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/cli"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/dotenv"
)

const usageLine = "usage: sigil <agent> hook | sigil claude [-- args...] | sigil pi [-- args...]"

// version is overridden via -ldflags at build time.
var version = "dev"

// agentHook is the entrypoint each hook agent adapter exposes.
type agentHook func(ctx context.Context, stdin io.Reader, stdout io.Writer, log *log.Logger) error

// agentLauncher is the entrypoint each launcher agent adapter exposes. It
// owns the user's terminal — args after the `--` separator are forwarded
// unchanged to the underlying CLI via process replacement.
type agentLauncher func(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer, log *log.Logger) error

// agents maps the argv agent name to its adapter Hook. The map is a package
// var so tests can substitute mock hooks.
var agents = map[string]agentHook{
	"claude-code": claudecode.Hook,
	"codex":       codex.Hook,
	"cursor":      cursor.Hook,
}

// launchers maps the argv name to its launcher adapter. Launchers are
// invoked directly by a human (no JSON on stdin) and replace the current
// process with the target CLI. The launcher name is the target CLI's own
// name (`claude`, `pi`), not the hook agent name (`claude-code`).
var launchers = map[string]agentLauncher{
	"claude": claudecode.Launch,
	"pi":     pi.Launch,
}

// exit is a package var so tests can intercept termination.
var exit = os.Exit

func main() {
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

	// Launcher dispatch handles `sigil <launcher> [-- args...]` before the
	// hook branch because launchers have no verb (single mode of operation).
	if launcher, ok := launchers[args[0]]; ok {
		launcherArgs, ok := parseLauncherArgs(args[0], args[1:], stderr)
		if !ok {
			return
		}

		dotenv.ApplyEnv("sigil", nil)
		logger := cli.InitLogger("sigil", args[0], "SIGIL_DEBUG")
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

		if err := launcher(context.Background(), launcherArgs, stdin, stdout, stderr, logger); err != nil {
			logger.Printf("launch %s: %v", args[0], err)
			_, _ = fmt.Fprintf(stderr, "sigil: %v\n", err)
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
		_, _ = fmt.Fprintf(stderr, "sigil: unknown agent %q\n", agent)
		exit(2)
		return
	}
	if verb != "hook" {
		_, _ = fmt.Fprintf(stderr, "sigil: unknown verb %q (only \"hook\" supported)\n", verb)
		exit(2)
		return
	}

	// Propagate the build version to the claude-code adapter so its hook
	// evaluation request carries the right agent_version. Other adapters
	// don't need it yet.
	claudecode.Version = version

	// Apply the dotenv file before initialising the logger so SIGIL_DEBUG=true
	// set only in $XDG_CONFIG_HOME/sigil/config.env still enables file logging.
	// Cursor (and Codex headless) launch hooks under a stripped environment
	// where the dotenv is the only place SIGIL_DEBUG could come from.
	dotenv.ApplyEnv("sigil", nil)
	logger := cli.InitLogger("sigil", agent, "SIGIL_DEBUG")
	defer cli.RecoverAndLog(logger)

	if err := hook(context.Background(), stdin, stdout, logger); err != nil {
		logger.Printf("hook: %v", err)
	}
}

// parseLauncherArgs splits sigil-side tokens from forwarded args at the first
// `--`. There are no sigil-side flags yet, so any token before `--` is an
// error.
func parseLauncherArgs(name string, rest []string, stderr io.Writer) ([]string, bool) {
	sep := -1
	for i, a := range rest {
		if a == "--" {
			sep = i
			break
		}
	}
	switch {
	case sep < 0 && len(rest) == 0:
		return nil, true
	case sep < 0:
		_, _ = fmt.Fprintf(stderr, "sigil: use `sigil %s -- <args>` to forward args to %[1]s\n", name)
		exit(2)
		return nil, false
	default:
		if sep > 0 {
			_, _ = fmt.Fprintf(stderr, "sigil: unknown options before `--`: %v\n", rest[:sep])
			exit(2)
			return nil, false
		}
		return rest[sep+1:], true
	}
}
