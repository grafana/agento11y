package entry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/grafana/agento11y/plugins/agento11y/internal/cli"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"golang.org/x/term"
)

// historyImportOptions is the parsed `agento11y history import` command line.
type historyImportOptions struct {
	Agent       history.AgentID
	SourcePaths []string
	Since       time.Time
	Until       time.Time
	Workspace   string
	MaxSessions int
	MaxTurns    int
	All         bool
	Yes         bool
	DryRun      bool
	Force       bool
	Local       bool
}

// repeatedFlag collects a flag given more than once.
type repeatedFlag []string

func (f *repeatedFlag) String() string { return strings.Join(*f, ",") }

func (f *repeatedFlag) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return errors.New("empty value")
	}
	*f = append(*f, v)
	return nil
}

// historyAgentNames renders the registry as a usage fragment such as
// "claude-code|codex". Nothing in this file hardcodes an agent, so registering
// one importer makes it appear here, in the picker, and in the error messages.
func historyAgentNames() string {
	ids := history.AgentIDs()
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = string(id)
	}
	return strings.Join(names, "|")
}

func historyUsageLine() string {
	return "usage: agento11y history import <" + historyAgentNames() + "> [flags]"
}

// historyAgentTable lists each agent with its display name and aliases, for
// the flag help and for an unknown-agent error.
func historyAgentTable() []string {
	specs := history.Specs()
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		line := "  " + string(spec.ID) + "  " + spec.DisplayName
		if len(spec.Aliases) > 0 {
			aliases := append([]string(nil), spec.Aliases...)
			sort.Strings(aliases)
			line += " (also: " + strings.Join(aliases, ", ") + ")"
		}
		out = append(out, line)
	}
	return out
}

// runHistoryCommand dispatches `agento11y history <verb>`.
func runHistoryCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, historyUsageLine())
		exit(2)
		return
	}
	if args[0] != "import" {
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown history verb %q (only \"import\" supported)\n", args[0])
		exit(2)
		return
	}
	runHistoryImport(args[1:], stdin, stdout, stderr)
}

// historyNow is a package var so tests can pin the 90-day default boundary.
var historyNow = func() time.Time { return time.Now() }

// historyEnsureLocal starts the local daemon and returns its endpoint. It is a
// package var so tests can run the command without a daemon.
var historyEnsureLocal = func(ctx context.Context) (string, error) {
	status, err := local.EnsureRunning(ctx, local.StateDir(), nil)
	if err != nil {
		return "", err
	}
	return status.Endpoint, nil
}

// historySelect shows the session picker. It is a package var so tests can
// drive selection without a TTY.
var historySelect = historySelectSessions

func runHistoryImport(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	fs := flag.NewFlagSet("history import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		sources     repeatedFlag
		since       = fs.String("since", "", "only sessions active at or after this time (RFC3339 or a duration such as 30d); default 90d")
		until       = fs.String("until", "", "only sessions started at or before this time (RFC3339 or a duration such as 7d)")
		workspace   = fs.String("workspace", "", "only sessions whose workspace path contains this text")
		maxSessions = fs.Int("max-sessions", 0, "import at most this many sessions, most recent first")
		maxTurns    = fs.Int("max-turns", 0, "import at most this many turns from each session")
		all         = fs.Bool("all", false, "import every matching session without showing the picker")
		yes         = fs.Bool("yes", false, "skip the confirmation prompt")
		dryRun      = fs.Bool("dry-run", false, "show what would be imported and exit")
		force       = fs.Bool("force", false, "re-export turns already recorded in the import ledger")
		useLocal    = fs.Bool("local", false, "import into the local daemon instead of Grafana Cloud")
	)
	fs.Var(&sources, "source", "restrict to this discovered path (repeatable); a path outside the agent's roots matches nothing")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, historyUsageLine())
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Agents:")
		for _, line := range historyAgentTable() {
			_, _ = fmt.Fprintln(stderr, line)
		}
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Flags:")
		fs.PrintDefaults()
	}
	// The agent comes first, before the flags: Go's flag package stops parsing
	// at the first non-flag argument, so `import --dry-run claude-code` would
	// otherwise leave the flags unparsed.
	agentArg := ""
	flagArgs := args
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		agentArg, flagArgs = flagArgs[0], flagArgs[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		exit(2)
		return
	}
	if extra := fs.Args(); len(extra) > 0 {
		_, _ = fmt.Fprintf(stderr, "agento11y: unexpected argument %q\n", extra[0])
		exit(2)
		return
	}
	if agentArg == "" {
		_, _ = fmt.Fprintln(stderr, historyUsageLine())
		exit(2)
		return
	}
	agent, ok := history.Resolve(agentArg)
	if !ok {
		_, _ = fmt.Fprintf(stderr, "agento11y: unknown history agent %q\n", agentArg)
		_, _ = fmt.Fprintln(stderr, "known agents:")
		for _, line := range historyAgentTable() {
			_, _ = fmt.Fprintln(stderr, line)
		}
		exit(2)
		return
	}

	now := historyNow()
	opts := historyImportOptions{
		Agent:       agent,
		SourcePaths: sources,
		Workspace:   strings.TrimSpace(*workspace),
		MaxSessions: *maxSessions,
		MaxTurns:    *maxTurns,
		All:         *all,
		Yes:         *yes,
		DryRun:      *dryRun,
		Force:       *force,
		Local:       *useLocal,
	}

	var err error
	// An unset --since uses history.DefaultSinceWindow.
	if opts.Since, err = parseHistoryBound(*since, now, now.Add(-history.DefaultSinceWindow)); err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: invalid --since %q: %v\n", *since, err)
		exit(2)
		return
	}
	if opts.Until, err = parseHistoryBound(*until, now, time.Time{}); err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: invalid --until %q: %v\n", *until, err)
		exit(2)
		return
	}
	if !opts.Until.IsZero() && opts.Until.Before(opts.Since) {
		_, _ = fmt.Fprintf(stderr, "agento11y: --until %s is before --since %s\n",
			opts.Until.Format(time.RFC3339), opts.Since.Format(time.RFC3339))
		exit(2)
		return
	}
	if opts.MaxSessions < 0 || opts.MaxTurns < 0 {
		_, _ = fmt.Fprintln(stderr, "agento11y: --max-sessions and --max-turns cannot be negative")
		exit(2)
		return
	}

	interactive := historyIsInteractive(stdin)
	// Without a terminal there is no picker and no confirmation, so an import
	// would run unattended over whatever discovery found. Only an explicit
	// --all --yes says that is what the caller wants; anything else prints the
	// plan and exports nothing.
	if !interactive && !opts.DryRun && !(opts.All && opts.Yes) {
		_, _ = fmt.Fprintln(stderr, "agento11y: stdin is not a terminal, so this is a dry run.")
		_, _ = fmt.Fprintln(stderr, "agento11y: pass --all --yes to import without a terminal.")
		opts.DryRun = true
	}

	if err := historyImport(opts, interactive, stdout, stderr); err != nil {
		_, _ = fmt.Fprintf(stderr, "agento11y: %v\n", err)
		exit(1)
	}
}

// parseHistoryBound accepts an RFC3339 timestamp, a duration back from now
// ("90d", "12h"), or an empty string, which yields fallback.
func parseHistoryBound(raw string, now, fallback time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if d, err := parseHistoryDuration(raw); err == nil {
		if d < 0 {
			return time.Time{}, errors.New("duration cannot be negative")
		}
		return now.Add(-d), nil
	}
	return time.Time{}, errors.New("want an RFC3339 timestamp or a duration such as 90d")
}

// parseHistoryDuration extends time.ParseDuration with a day unit, which is
// the unit a history window is naturally expressed in.
func parseHistoryDuration(raw string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(raw, "d"); ok {
		d, err := time.ParseDuration(days + "h")
		if err != nil {
			return 0, err
		}
		return d * 24, nil
	}
	return time.ParseDuration(raw)
}

func historyIsInteractive(stdin io.Reader) bool {
	f, ok := stdin.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func historyImport(opts historyImportOptions, interactive bool, stdout, stderr io.Writer) error {
	ctx := context.Background()
	// The dotenv file is applied before the logger is built so AGENTO11Y_DEBUG
	// set only in config.env still turns on file logging, as it does for the
	// other commands.
	dotenv.ApplyEnv(nil)
	logger := cli.InitLogger("history")

	filter := history.NewFilter()
	filter.Since = opts.Since
	filter.Until = opts.Until
	filter.Workspace = opts.Workspace
	filter.SourcePaths = opts.SourcePaths
	filter.MaxSessions = opts.MaxSessions
	filter.MaxTurns = opts.MaxTurns

	plan, err := history.BuildPlan(ctx, history.PlanOptions{Agent: opts.Agent, Filter: filter})
	if err != nil {
		return err
	}
	printHistoryPlan(stdout, opts, plan)

	if len(opts.SourcePaths) > 0 && len(plan.Sessions) == 0 {
		// --source filters discovery; it never adds a root. A path outside the
		// agent's roots therefore matches nothing, which is worth saying.
		_, _ = fmt.Fprintln(stderr, "agento11y: no discovered session matched --source. The flag filters the paths under the agent's roots; it cannot add a new root.")
	}
	if opts.DryRun {
		_, _ = fmt.Fprintln(stdout, "Dry run: nothing was decoded, exported, or stored.")
		return nil
	}
	if len(plan.Sessions) == 0 {
		return nil
	}

	sessions := plan.Sessions
	if interactive && !opts.All {
		selected, err := historySelect(sessions)
		if err != nil {
			return err
		}
		sessions = selected
	}
	if len(sessions) == 0 {
		_, _ = fmt.Fprintln(stdout, "No sessions selected.")
		return nil
	}
	if interactive && !opts.Yes {
		confirmed, err := historyConfirm(opts, sessions)
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(stdout, "Import cancelled.")
			return nil
		}
	}

	target, err := historyTarget(ctx, opts)
	if err != nil {
		return err
	}
	exporter, cleanup, err := history.NewTargetExporter(ctx, target, logger)
	if err != nil {
		return err
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := cleanup(shutdownCtx); err != nil {
			logger.Printf("shutdown import exporter: %v", err)
		}
	}()

	// Progress is a redrawn line, so it is rate-limited rather than written per
	// turn: a large import exports hundreds of thousands of turns, and a
	// redirected stderr would otherwise collect one line for each.
	lastReport := time.Time{}
	result, err := history.RunImport(ctx, history.ImportOptions{
		Agent:      opts.Agent,
		Filter:     filter,
		Sessions:   sessions,
		Collisions: plan.Collisions,
		Force:      opts.Force,
		Target:     target,
		Exporter:   exporter,
		OnProgress: func(p history.Progress) {
			now := time.Now()
			if now.Sub(lastReport) < historyProgressInterval {
				return
			}
			lastReport = now
			_, _ = fmt.Fprintf(stderr, "\rimporting %s: session %d/%d  imported %d  skipped %d  failed %d",
				p.Agent, p.Sessions, p.Total, p.Imported, p.Skipped, p.Failed)
		},
	})
	if !lastReport.IsZero() {
		_, _ = fmt.Fprintln(stderr)
	}
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "Imported %d turns from %d sessions (%d already imported, %d failed).\n",
		result.Imported, result.Sessions, result.Skipped, result.Failed)
	for _, warning := range result.Warnings {
		_, _ = fmt.Fprintf(stderr, "agento11y: warning: %s\n", warning)
	}
	if result.Failed > 0 {
		// A script cannot see the counters, so a failed turn has to reach the
		// exit status. The ledger holds those failures for a rerun to retry.
		return fmt.Errorf("%d turns failed to export; rerun to retry them", result.Failed)
	}
	// A completed import answers the viewer's one-time offer, whether it
	// exported turns or found them all already imported.
	if err := history.MarkPrompt(opts.Agent, history.PromptImported); err != nil {
		logger.Printf("record import prompt state: %v", err)
	}
	return nil
}

// historyProgressInterval is how often the progress line is redrawn.
const historyProgressInterval = 200 * time.Millisecond

// historyTarget builds the export destination. Without --local the import goes
// to the configured Grafana Cloud endpoint; with it, to the local daemon, which
// [history.NewTargetExporter] recognises as loopback and gives full content and
// the forward marker, so the backfill stays on this machine.
func historyTarget(ctx context.Context, opts historyImportOptions) (history.Target, error) {
	if !opts.Local {
		return history.Target{}, nil
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint, err := historyEnsureLocal(startCtx)
	if err != nil {
		return history.Target{}, fmt.Errorf("start the local receiver: %w", err)
	}
	return history.Target{
		Endpoint:     endpoint,
		OTLPEndpoint: endpoint + "/otlp",
	}, nil
}

func printHistoryPlan(stdout io.Writer, opts historyImportOptions, plan history.ImportPlan) {
	spec, _ := history.Spec(plan.Agent)
	name := spec.DisplayName
	if name == "" {
		name = string(plan.Agent)
	}
	_, _ = fmt.Fprintf(stdout, "%s history since %s", name, opts.Since.Format(time.RFC3339))
	if !opts.Until.IsZero() {
		_, _ = fmt.Fprintf(stdout, " until %s", opts.Until.Format(time.RFC3339))
	}
	_, _ = fmt.Fprintln(stdout)

	turns, approx := historyTurnTotals(plan.Sessions)
	_, _ = fmt.Fprintf(stdout, "  planned: %d sessions, %s turns\n", len(plan.Sessions), historyTurnCount(turns, approx))
	if len(plan.Skipped) > 0 {
		_, _ = fmt.Fprintf(stdout, "  skipped: %d sessions (%s)\n", len(plan.Skipped), historySkipSummary(plan.Skipped))
	}
	for _, c := range plan.Collisions {
		_, _ = fmt.Fprintf(stdout, "  note: session ID %s is claimed by %d files; each keeps its own conversation\n",
			c.SessionID, len(c.Sources))
	}
	for _, w := range plan.Warnings {
		_, _ = fmt.Fprintf(stdout, "  warning: %s\n", w)
	}
}

func historyTurnTotals(sessions []history.SessionPreview) (turns int, approx bool) {
	for _, s := range sessions {
		turns += s.TurnCount
		approx = approx || s.ApproxTurns
	}
	return turns, approx
}

// historyTurnCount renders a turn total, marking it approximate when any
// session's count was estimated rather than counted.
func historyTurnCount(turns int, approx bool) string {
	if approx {
		return fmt.Sprintf("about %d", turns)
	}
	return fmt.Sprintf("%d", turns)
}

func historySkipSummary(skipped []history.SkippedSession) string {
	counts := map[history.SkipReason]int{}
	for _, s := range skipped {
		counts[s.Reason]++
	}
	reasons := make([]string, 0, len(counts))
	for reason, n := range counts {
		reasons = append(reasons, fmt.Sprintf("%s: %d", reason, n))
	}
	sort.Strings(reasons)
	return strings.Join(reasons, ", ")
}

// historySelectSessions shows the multi-select picker, pre-selecting every
// session so Enter imports the whole plan.
func historySelectSessions(sessions []history.SessionPreview) ([]history.SessionPreview, error) {
	options := make([]huh.Option[int], len(sessions))
	for i, s := range sessions {
		options[i] = huh.NewOption(historySessionLabel(s), i).Selected(true)
	}
	var chosen []int
	form := huh.NewForm(huh.NewGroup(
		huh.NewMultiSelect[int]().
			Title("Sessions to import").
			Description("Space toggles, Enter confirms.").
			Options(options...).
			Value(&chosen),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return nil, nil
		}
		return nil, err
	}
	out := make([]history.SessionPreview, 0, len(chosen))
	for _, i := range chosen {
		out = append(out, sessions[i])
	}
	return out, nil
}

// historySessionLabel renders one picker row. It shows the last activity, the
// workspace, and the turn count, never prompt text.
func historySessionLabel(s history.SessionPreview) string {
	when := "unknown date"
	if !s.LastActivityAt.IsZero() {
		when = s.LastActivityAt.Local().Format("2006-01-02 15:04")
	}
	workspace := s.Workspace
	if workspace == "" {
		workspace = "unknown workspace"
	}
	turns := fmt.Sprintf("%d turns", s.TurnCount)
	if s.ApproxTurns {
		turns = fmt.Sprintf("about %d turns", s.TurnCount)
	}
	return fmt.Sprintf("%s  %s  %s", when, workspace, turns)
}

func historyConfirm(opts historyImportOptions, sessions []history.SessionPreview) (bool, error) {
	turns, approx := historyTurnTotals(sessions)
	destination := "Grafana Cloud"
	if opts.Local {
		destination = "the local store on this machine"
	}
	confirmed := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(fmt.Sprintf("Import %d sessions (%s turns) into %s?", len(sessions), historyTurnCount(turns, approx), destination)).
			Value(&confirmed),
	))
	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			return false, nil
		}
		return false, err
	}
	return confirmed, nil
}
