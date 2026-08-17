package entry

import (
	"fmt"
	"io"

	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
)

// skillsUsage is the misuse form for `agento11y skills`. It names only `show`,
// though `get` also works, because a usage line is there to get the command
// run.
const skillsUsage = "usage: agento11y " + skills.Command + " " + skills.ListVerb +
	" | agento11y " + skills.Command + " " + skills.ShowVerb + " <name>"

// listHint is the second stderr line on a failed lookup. It names the command
// that lists the bundled skills.
const listHint = "agento11y: run `agento11y " + skills.Command + " " + skills.ListVerb + "` to see the bundled skills"

// runSkillsCommand dispatches `agento11y skills <verb>`. The skills ship
// inside the binary, so no verb here reads the filesystem or the network. The
// verbs come from internal/skills, which also owns the hints that print them.
func runSkillsCommand(args []string, stdout, stderr io.Writer) {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, skillsUsage)
		exit(2)
		return
	}
	switch args[0] {
	case skills.ListVerb:
		all := skills.All()
		width := 0
		for _, skill := range all {
			if len(skill.Name) > width {
				width = len(skill.Name)
			}
		}
		for _, skill := range all {
			_, _ = fmt.Fprintf(stdout, "%-*s  %s\n", width, skill.Name, skill.Description)
		}
	case skills.ShowVerb, skills.GetVerb:
		if len(args) != 2 {
			_, _ = fmt.Fprintln(stderr, skillsUsage)
			exit(2)
			return
		}
		skill, err := skills.Get(args[1])
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
			_, _ = fmt.Fprintln(stderr, listHint)
			exit(2)
			return
		}
		_, _ = io.WriteString(stdout, skill.Body)
	default:
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown skills verb %q\n", args[0])
		_, _ = fmt.Fprintln(stderr, skillsUsage)
		exit(2)
	}
}

// runHelpCommand prints the expanded command list, which the one-line
// usageLine cannot carry.
func runHelpCommand(stdout io.Writer) {
	_, _ = io.WriteString(stdout, helpBody)
	_, _ = fmt.Fprintf(stdout, "  %s\n\n", skills.SetupCodingAgentCommand)
	_, _ = fmt.Fprintln(stdout, skills.SetupCodingAgentHintIntro)
	_, _ = fmt.Fprintln(stdout, skills.SetupCodingAgentPasteLine)
}

// helpBody is every part of the help block that does not interpolate.
//
// The rows are hand-written, because run dispatches with a flat if-chain over
// args[0] rather than a table a test can read. A new subcommand needs four
// edits: run, this block, usageLine, and the list in
// TestRun_HelpIsARealCommand. Only the launcher names are checked
// automatically.
const helpBody = `agento11y sends coding-agent sessions to Grafana Agent observability.

Usage:
  agento11y <command> [flags]

Launch a coding agent (wires the plugin, then runs it):
  claude, codex, copilot, opencode, pi, vibe
      agento11y <name> [--local|--no-local] [--tag key=value]... [-- args...]
  cursor install|uninstall
      Wire (or remove) the Cursor hook. Cursor is a GUI app and has no launcher.
  claude install [--json]
      Register the Claude Code plugin without launching it or prompting.
  agents reconcile --agents claude,cursor --json
      Emit a stable receipt for managed-device inventory without launching agents.

Commands:
  login       Save endpoint, tenant, token, and OTLP endpoint to config.env.
  doctor      Check both export pipelines, the config, and installed plugins.
  skills      List or print the agent skills bundled into this binary.
  local       Manage the local capture daemon: start, status, stop.
  history     Backfill sessions an agent wrote before agento11y was installed.
  help        Print this text.

  <agent> hook
      Internal. Host agents call this with a JSON payload on stdin.

Flags:
  --version   Print the build version.
  --help, -h  Print this text. Both are top-level forms: a subcommand given
              the wrong arguments prints its own usage line instead.

Skills:
  agento11y skills list
`
