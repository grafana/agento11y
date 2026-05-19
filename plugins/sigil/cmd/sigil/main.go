// Command sigil is the single binary used by the Claude Code, Codex, and
// Cursor agent plugins. It accepts:
//
//	sigil <agent> hook   — dispatch a JSON hook payload on stdin to <agent>
//	sigil --version      — print the build version
//
// Unknown agents and unknown verbs exit with code 2 and a usage message on
// stderr. The binary must never crash the calling agent process; once argv
// parsing succeeds, all errors are swallowed (and logged when
// SIGIL_DEBUG=true) and the process exits 0.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/cli"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/dotenv"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/claudecode"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/codex"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor"
)

// version is overridden via -ldflags at build time.
var version = "dev"

// agentHook is the entrypoint each agent adapter exposes.
type agentHook func(ctx context.Context, stdin io.Reader, stdout io.Writer, log *log.Logger) error

// agents maps the argv agent name to its adapter Hook. The map is a package
// var so tests can substitute mock hooks.
var agents = map[string]agentHook{
	"claude-code": claudecode.Hook,
	"codex":       codex.Hook,
	"cursor":      cursor.Hook,
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

	if len(args) < 2 {
		_, _ = fmt.Fprintln(stderr, "usage: sigil <agent> hook")
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
