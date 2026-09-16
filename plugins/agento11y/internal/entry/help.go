package entry

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/clihelp"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
)

func publicHelpPages() map[string]clihelp.Page {
	pages := map[string]clihelp.Page{}
	add := func(path, summary, usage string) {
		command := strings.TrimSpace("agento11y " + path)
		pages[path] = clihelp.Page{Command: command, Summary: summary, Usage: []string{strings.TrimSpace(command + " " + usage)}}
	}
	add("", "Send coding-agent sessions to Grafana Agent Observability.", "<command> [flags]")
	add("help", "Show help for a command.", "[command path]")
	add("login", "Save capture settings to config.env. Prompt for missing values. Verify credentials before saving unless --no-verify is set.", "[flags]")
	add("doctor", "Check export pipelines, configuration, and installed integrations without changing them.", "[flags]")
	add("guards", "Test a command against saved local guard rules without executing it.", "<command>")
	add("guards test", "Test a command against saved local guard rules without executing it.", "[--json] [--rules path] [--tool name] [--agent name] [--stdin] [--] <command>")
	add("agents", "Configure registered agent integrations without launching them.", "<command>")
	add("agents reconcile", "Reconcile selected integrations and print a JSON receipt.", "--agents all|name[,name...] --json")
	add("cursor", "Manage Cursor hooks. Cursor is a GUI app and has no launcher.", "<command>")
	add("cursor install", "Install Cursor hooks and offer capture setup if needed.", "")
	add("cursor uninstall", "Remove agento11y hooks from Cursor.", "")
	add("local", "Manage the local capture daemon and viewer.", "<command>")
	for _, row := range []clihelp.Row{{Name: "start", Description: "Start the receiver if needed and print its address."}, {Name: "open", Description: "Start the receiver if needed, print its address, and try to open the viewer."}, {Name: "status", Description: "Report whether the receiver is running."}, {Name: "stop", Description: "Stop the receiver."}, {Name: "restart", Description: "Stop and start the receiver."}} {
		usage := ""
		if row.Name == "status" {
			usage = "[--json]"
		}
		add("local "+row.Name, row.Description, usage)
	}
	add("history", "Backfill sessions recorded before agento11y was installed.", "<command>")
	add("history import", "Import native agent sessions. Without a terminal, preview only unless --all --yes is set.", "<agent> [flags]")
	add("skills", "List or print the agent skills bundled into this binary.", "<command>")
	add("skills list", "List bundled skills and their descriptions.", "")
	add("skills show", "Print a bundled skill as raw Markdown.", "<name>")
	add("skills get", "Print a bundled skill as raw Markdown. Alias for skills show.", "<name>")
	add("claude eval", "Export Claude plugin eval results to Experiments.", "<command>")
	add("claude eval import", "Export Claude plugin eval results. Use --dry-run to preview the redacted JSON without uploading.", "<results.json> [flags]")
	launcherNames := make([]string, 0, len(launchers))
	for name := range launchers {
		launcherNames = append(launcherNames, name)
	}
	sort.Strings(launcherNames)
	for _, name := range launcherNames {
		add(name, "Configure capture and launch "+name+". Arguments after -- belong to the host agent.", "[flags] [-- args...]")
	}
	for _, name := range []string{"claude", "copilot", "opencode", "pi"} {
		add(name+" install", "Install the "+name+" integration without launching it or prompting.", "[--json]")
	}
	for _, spec := range history.Specs() {
		for _, name := range append([]string{string(spec.ID)}, spec.Aliases...) {
			add("history import "+name, "Import sessions from "+spec.DisplayName+".", "[flags]")
		}
	}
	children := func(path, title string, names ...string) {
		page := pages[path]
		section := clihelp.Section{Title: title}
		for _, name := range names {
			child := strings.TrimSpace(path + " " + name)
			summary, _, _ := strings.Cut(pages[child].Summary, ". ")
			section.Rows = append(section.Rows, clihelp.Row{Name: name, Description: strings.TrimSuffix(summary, ".") + "."})
		}
		page.Sections = append(page.Sections, section)
		pages[path] = page
	}
	children("", "Setup", "login", "doctor", "agents")
	children("", "Launchers", launcherNames...)
	children("", "Commands", "cursor", "guards", "local", "history", "skills", "help")
	children("guards", "Commands", "test")
	children("agents", "Commands", "reconcile")
	children("cursor", "Commands", "install", "uninstall")
	children("local", "Commands", "start", "open", "status", "stop", "restart")
	children("history", "Commands", "import")
	children("skills", "Commands", "list", "show", "get")
	children("claude eval", "Commands", "import")
	for _, name := range []string{"claude", "copilot", "opencode", "pi"} {
		children(name, "Commands", "install")
	}
	children("claude", "Experiments", "eval")
	page := pages["agents reconcile"]
	targets := clihelp.Section{Title: "Installers"}
	for _, spec := range registeredInstallers() {
		targets.Rows = append(targets.Rows, clihelp.Row{Name: spec.Name, Description: "Available to --agents."})
	}
	page.Sections = append(page.Sections, targets)
	pages["agents reconcile"] = page
	page = pages["history import"]
	importers := clihelp.Section{Title: "Agents"}
	for _, spec := range history.Specs() {
		description := spec.DisplayName
		if len(spec.Aliases) > 0 {
			description += " (also: " + strings.Join(spec.Aliases, ", ") + ")"
		}
		importers.Rows = append(importers.Rows, clihelp.Row{Name: string(spec.ID), Description: description})
	}
	page.Sections = append(page.Sections, importers)
	pages["history import"] = page
	page = pages[""]
	page.Examples = []clihelp.Row{{Name: "agento11y login", Description: "Choose local capture or Grafana Cloud."}, {Name: "agento11y claude", Description: "Start a coding agent."}, {Name: "agento11y local open", Description: "Open the local viewer."}, {Name: "agento11y help history import", Description: "Read command-specific help."}}
	page.Sections = append(page.Sections, clihelp.Section{Title: "Flags", Rows: []clihelp.Row{{Name: "--version", Description: "Print the build version."}, {Name: "--help, -h", Description: "Show help."}}})
	pages[""] = page
	return pages
}

func helpPage(path string) (clihelp.Page, bool) {
	page, ok := publicHelpPages()[path]
	if path == "doctor" {
		return doctor.HelpPage(), true
	}
	if fs := helpFlags(path); ok && fs != nil {
		page.Sections = append(page.Sections, clihelp.Section{Title: "Flags", Rows: clihelp.Flags(fs, nil, nil)})
	}
	return page, ok
}

func printHelp(path string, stdout io.Writer) {
	page, _ := helpPage(path)
	clihelp.New(stdout).Render(page)
	if path == "" || path == "login" || path == "skills" {
		_, _ = fmt.Fprintf(stdout, "\nSetup skill:\n  agento11y skills list\n  %s\n%s\n%s\n", skills.SetupCodingAgentCommand, skills.SetupCodingAgentHintIntro, skills.SetupCodingAgentPasteLine)
	}
}

func newCommandFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}

func helpFlags(path string) *flag.FlagSet {
	switch {
	case path == "login":
		fs, _ := newLoginFlags()
		return fs
	case path == "local status":
		fs, _ := newJSONFlags(path)
		return fs
	case path == "agents reconcile":
		fs, _, _ := newReconcileFlags()
		return fs
	case path == "history import" || strings.HasPrefix(path, "history import "):
		return historyHelpFlags()
	case path == "claude eval import":
		return claudeEvalHelpFlags()
	case path == "claude install" || path == "copilot install" || path == "opencode install" || path == "pi install":
		fs, _ := newJSONFlags(path)
		return fs
	default:
		if _, ok := launchers[path]; ok {
			fs, _ := newLauncherFlags(path)
			return fs
		}
	}
	return nil
}

func usageError(stderr io.Writer, path, message string) {
	page, ok := helpPage(path)
	if !ok {
		page, _ = helpPage("")
	}
	clihelp.UsageError(stderr, page, message)
}

func routeHelp(args []string, stdout, stderr io.Writer) bool {
	if len(args) == 0 {
		printHelp("", stdout)
		return true
	}
	if args[0] == "--version" || args[0] == "-version" {
		return false
	}
	// guards has its own help and flag parser, which documents the dry-run
	// contract more precisely than the generic command pages can.
	if args[0] == "guards" {
		return false
	}
	if len(args) > 1 && args[0] == "local" && args[1] == "serve" {
		return false
	}
	if args[0] == "help" || (len(args) > 1 && args[0] == "local" && args[1] == "help") {
		topic := args[1:]
		if args[0] == "local" {
			topic = append([]string{"local"}, args[2:]...)
		}
		if len(topic) == 1 && (topic[0] == "--help" || topic[0] == "-h") {
			topic = []string{"help"}
		}
		return helpTopic(topic, stdout, stderr)
	}
	if len(args) > 1 && args[1] == "hook" {
		return false
	}
	if args[0] == "--help" || args[0] == "-h" {
		if len(args) == 1 {
			printHelp("", stdout)
		} else {
			usageError(stderr, "", "unexpected arguments after help")
			exit(2)
		}
		return true
	}
	pages := publicHelpPages()
	path := ""
	consumed := 0
	for i := 1; i <= len(args); i++ {
		candidate := strings.Join(args[:i], " ")
		if _, ok := pages[candidate]; !ok {
			break
		}
		path, consumed = candidate, i
	}
	if consumed == 0 {
		if _, internal := agents[args[0]]; internal {
			return false
		}
		usageError(stderr, "", fmt.Sprintf("unknown command %q", args[0]))
		exit(2)
		return true
	}
	rest := args[consumed:]
	group := path == "local" || path == "history" || path == "skills" || path == "agents" || path == "cursor" || path == "claude eval"
	if group && len(rest) == 0 {
		printHelp(path, stdout)
		return true
	}
	if _, launcher := launchers[path]; launcher {
		_, err := parseLauncherOptions(path, rest)
		return parsedHelp(path, err, stdout, stderr)
	}
	if path == "doctor" {
		return routeDoctorHelp(rest, stdout, stderr)
	}
	fs := helpFlags(path)
	if fs == nil {
		fs = newCommandFlags(path)
	}
	// Eval accepts a leading file; history importer names are resolved above.
	if path == "claude eval import" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	// Preserve history parser ownership even for an unknown importer.
	if path == "history import" && len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		rest = rest[1:]
	}
	err := fs.Parse(rest)
	if err != nil {
		return parsedHelp(path, err, stdout, stderr)
	}
	if group && fs.NArg() > 0 {
		usageError(stderr, path, fmt.Sprintf("unknown %s verb %q", path, fs.Arg(0)))
		exit(2)
		return true
	}
	if path == "skills show" || path == "skills get" || path == "history import" || strings.HasPrefix(path, "history import ") || path == "claude eval import" {
		return false
	}
	if fs.NArg() > 0 {
		usageError(stderr, path, fmt.Sprintf("unexpected arguments: %v", fs.Args()))
		exit(2)
		return true
	}
	return false
}

func routeDoctorHelp(args []string, stdout, stderr io.Writer) bool {
	opts, err := doctor.ParseOptions(args)
	if errors.Is(err, flag.ErrHelp) && opts.NoColor {
		renderer := clihelp.New(stdout)
		renderer.Plain()
		renderer.Render(doctor.HelpPage())
		return true
	}
	return parsedHelp("doctor", err, stdout, stderr)
}

func parsedHelp(path string, err error, stdout, stderr io.Writer) bool {
	if errors.Is(err, flag.ErrHelp) {
		printHelp(path, stdout)
		return true
	}
	if err != nil {
		usageError(stderr, path, err.Error())
		exit(2)
		return true
	}
	return false
}

func helpTopic(topic []string, stdout, stderr io.Writer) bool {
	path := strings.Join(topic, " ")
	if _, ok := helpPage(path); ok {
		printHelp(path, stdout)
		return true
	}
	nearest := ""
	for i := 1; i <= len(topic); i++ {
		candidate := strings.Join(topic[:i], " ")
		if _, ok := helpPage(candidate); !ok {
			break
		}
		nearest = candidate
	}
	usageError(stderr, nearest, fmt.Sprintf("unknown help topic %q", path))
	exit(2)
	return true
}
