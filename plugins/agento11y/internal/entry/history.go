package entry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/grafana/agento11y/plugins/agento11y/internal/cli"
	"github.com/grafana/agento11y/plugins/agento11y/internal/clihelp"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
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
	NoLocal     bool
	// LocalEnvKey names the LOCAL spelling that selected the local store.
	// It stays empty when a flag or the first-run answer selected it.
	LocalEnvKey string
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

// runHistoryCommand dispatches `agento11y history <verb>`.
func runHistoryCommand(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	if len(args) == 0 {
		printHelp("history", stdout)
		return
	}
	if args[0] == "import" {
		runHistoryImport(args[1:], stdin, stdout, stderr)
		return
	}
	fs := flag.NewFlagSet("history", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	if err := fs.Parse(args); errors.Is(err, flag.ErrHelp) {
		printHelp("history", stdout)
	} else if err != nil {
		usageError(stderr, "history", err.Error())
		exit(2)
	} else {
		usageError(stderr, "history", fmt.Sprintf("unknown history verb %q", args[0]))
		exit(2)
	}
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

// historySelect and historyConfirm are package vars so tests can drive the
// interactive path without a TTY.
var (
	historySelect  = historySelectSessions
	historyConfirm = historyConfirmImport
)

type historyImportFlags struct {
	sources                                    repeatedFlag
	since, until, workspace                    string
	maxSessions, maxTurns                      int
	all, yes, dryRun, force, useLocal, noLocal bool
}

func newHistoryImportFlags() (*flag.FlagSet, *historyImportFlags) {
	fs := flag.NewFlagSet("history import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	f := new(historyImportFlags)
	fs.StringVar(&f.since, "since", "", "only sessions active at or after this time (RFC3339 or a duration such as 30d); default 90d")
	fs.StringVar(&f.until, "until", "", "only sessions started at or before this time (RFC3339 or a duration such as 7d)")
	fs.StringVar(&f.workspace, "workspace", "", "only sessions whose workspace path contains this text")
	fs.IntVar(&f.maxSessions, "max-sessions", 0, "import at most this many sessions, most recent first")
	fs.IntVar(&f.maxTurns, "max-turns", 0, "import at most this many turns from each session")
	fs.BoolVar(&f.all, "all", false, "import every matching session without showing the picker")
	fs.BoolVar(&f.yes, "yes", false, "skip the confirmation prompt")
	fs.BoolVar(&f.dryRun, "dry-run", false, "show what would be imported and exit")
	fs.BoolVar(&f.force, "force", false, "re-export turns already recorded in the import ledger")
	fs.BoolVar(&f.useLocal, "local", false, "import into the local daemon instead of Grafana Cloud")
	fs.BoolVar(&f.noLocal, "no-local", false, "import into Grafana Cloud even when local mode is on")
	fs.Var(&f.sources, "source", "restrict to this discovered path (repeatable); a path outside the agent's roots matches nothing")
	return fs, f
}

func historyHelpFlags() *flag.FlagSet {
	fs, _ := newHistoryImportFlags()
	return fs
}

func runHistoryImport(args []string, stdin io.Reader, stdout, stderr io.Writer) {
	fs, f := newHistoryImportFlags()
	// The agent comes first, before the flags: Go's flag package stops parsing
	// at the first non-flag argument, so `import --dry-run claude-code` would
	// otherwise leave the flags unparsed.
	agentArg := ""
	flagArgs := args
	if len(flagArgs) > 0 && !strings.HasPrefix(flagArgs[0], "-") {
		agentArg, flagArgs = flagArgs[0], flagArgs[1:]
	}
	if err := fs.Parse(flagArgs); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printHelp("history import", stdout)
			return
		}
		usageError(stderr, "history import", err.Error())
		exit(2)
		return
	}
	if extra := fs.Args(); len(extra) > 0 {
		usageError(stderr, "history import", fmt.Sprintf("unexpected argument %q", extra[0]))
		exit(2)
		return
	}
	if agentArg == "" {
		usageError(stderr, "history import", "an agent is required")
		exit(2)
		return
	}
	agent, ok := history.Resolve(agentArg)
	if !ok {
		usageError(stderr, "history import", fmt.Sprintf("unknown history agent %q (known agents: %s)", agentArg, historyAgentNames()))
		exit(2)
		return
	}

	now := historyNow()
	opts := historyImportOptions{
		Agent:       agent,
		SourcePaths: f.sources,
		Workspace:   strings.TrimSpace(f.workspace),
		MaxSessions: f.maxSessions,
		MaxTurns:    f.maxTurns,
		All:         f.all,
		Yes:         f.yes,
		DryRun:      f.dryRun,
		Force:       f.force,
		Local:       f.useLocal && !f.noLocal,
		NoLocal:     f.noLocal,
	}

	var err error
	// An unset --since defaults to 90 days. The local store is a linear-scan
	// JSONL store, and an unbounded first import would make the viewer slow
	// before the user ever sees it.
	if opts.Since, err = parseHistoryBound(f.since, now, now.Add(-history.DefaultSinceWindow)); err != nil {
		usageError(stderr, "history import", fmt.Sprintf("invalid --since %q: %v", f.since, err))
		exit(2)
		return
	}
	if opts.Until, err = parseHistoryBound(f.until, now, time.Time{}); err != nil {
		usageError(stderr, "history import", fmt.Sprintf("invalid --until %q: %v", f.until, err))
		exit(2)
		return
	}
	if !opts.Until.IsZero() && opts.Until.Before(opts.Since) {
		usageError(stderr, "history import", fmt.Sprintf("--until %s is before --since %s",
			opts.Until.Format(time.RFC3339), opts.Since.Format(time.RFC3339)))
		exit(2)
		return
	}
	if opts.MaxSessions < 0 || opts.MaxTurns < 0 {
		usageError(stderr, "history import", "--max-sessions and --max-turns cannot be negative")
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

func historyResolveDestination(ctx context.Context, opts *historyImportOptions, envLocal localEnvRequest, interactive bool, stderr io.Writer, logger *log.Logger) error {
	switch {
	case opts.NoLocal:
		opts.Local = false
	case opts.Local:
		return nil
	case envLocal.on:
		opts.Local = true
		opts.LocalEnvKey = envLocal.key
		return nil
	}
	if historyDestinationConfigured() || !interactive {
		return nil
	}

	// History imports marked as locally forwarded never relay to Cloud, so this
	// setup cannot offer the local daemon's Local + Cloud behavior.
	result, err := loginRun(ctx, login.RunOpts{
		Stderr:           stderr,
		Logger:           logger,
		OfferLocal:       !opts.NoLocal && !localDestinationSet(),
		KeepLocalSetting: opts.NoLocal,
	})
	switch {
	case err == nil:
		opts.Local = result.LocalMode
		return nil
	case errors.Is(err, login.ErrAborted), errors.Is(err, login.ErrNotInteractive):
		return errors.New("nothing was imported because setup did not finish; run `agento11y login` or pass --local to import into the local store")
	case errors.Is(err, login.ErrNotVerified):
		logger.Printf("import setup: %v", err)
		return errors.New("nothing was imported because the endpoint did not accept those credentials; run `agento11y login` to try again, `agento11y login --yes` to save them anyway, or pass --local to import into the local store")
	default:
		return fmt.Errorf("setup: %w", err)
	}
}

// historyDestinationConfigured reports whether first-run setup has anything to
// ask. Cloud needs complete credentials, while a loopback endpoint needs none.
func historyDestinationConfigured() bool {
	return dotenv.HasCredentials() || envconfig.IsLocalEndpoint(envconfig.Getenv("ENDPOINT"))
}

func historyImport(opts historyImportOptions, interactive bool, stdout, stderr io.Writer) error {
	ctx := context.Background()
	// The dotenv file is applied before the logger is built so AGENTO11Y_DEBUG
	// set only in config.env still turns on file logging, as it does for the
	// other commands. Read LOCAL around that merge so an error can name the
	// spelling that selected the local store.
	localValue, localKey, inShell := envconfig.LookupEnv("LOCAL")
	fileEnv := dotenv.ApplyEnv(nil)
	if !inShell {
		localValue, localKey, _ = envconfig.LookupMap(fileEnv, "LOCAL")
	}
	envLocal := localEnvRequest{on: envconfig.ParseBool(localValue), key: localKey}
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
		_, _ = fmt.Fprintln(stdout, clihelp.New(stdout).Detail("Dry run: nothing was decoded, exported, or stored."))
		return nil
	}
	if len(plan.Sessions) == 0 {
		return nil
	}

	if err := historyResolveDestination(ctx, &opts, envLocal, interactive, stderr, logger); err != nil {
		return err
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
		_, _ = fmt.Fprintln(stdout, clihelp.New(stdout).Detail("No sessions selected."))
		return nil
	}
	if interactive && !opts.Yes {
		confirmed, err := historyConfirm(opts, sessions)
		if err != nil {
			return err
		}
		if !confirmed {
			_, _ = fmt.Fprintln(stdout, clihelp.New(stdout).Detail("Import cancelled."))
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
	renderer := clihelp.New(stdout)
	summary := fmt.Sprintf("Imported %d turns from %d sessions (%d already imported, %d failed).", result.Imported, result.Sessions, result.Skipped, result.Failed)
	if result.Failed > 0 {
		summary = renderer.Warning(summary)
	} else {
		summary = renderer.Success(summary)
	}
	_, _ = fmt.Fprintln(stdout, summary)
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

// historyTarget builds the export destination from the resolved Local value.
// A Cloud import leaves the target empty so [history.NewTargetExporter] reads
// the configured endpoint. A local import points at the daemon, where the
// exporter enables full content and adds the marker that prevents forwarding.
func historyTarget(ctx context.Context, opts historyImportOptions) (history.Target, error) {
	if !opts.Local {
		return history.Target{}, nil
	}
	startCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint, err := historyEnsureLocal(startCtx)
	if err != nil {
		if opts.LocalEnvKey != "" {
			return history.Target{}, fmt.Errorf("start the local receiver: %w (local mode is on because %s is set; pass --no-local to import into Grafana Cloud)", err, opts.LocalEnvKey)
		}
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
	renderer := clihelp.New(stdout)
	heading := fmt.Sprintf("%s history since %s", name, opts.Since.Format(time.RFC3339))
	if !opts.Until.IsZero() {
		heading += " until " + opts.Until.Format(time.RFC3339)
	}
	_, _ = fmt.Fprintln(stdout, renderer.Heading(heading))

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

func historyDestinationName(opts historyImportOptions) string {
	if opts.Local {
		return "the local store on this machine"
	}
	return "Grafana Cloud"
}

func historyConfirmImport(opts historyImportOptions, sessions []history.SessionPreview) (bool, error) {
	turns, approx := historyTurnTotals(sessions)
	confirmed := false
	form := huh.NewForm(huh.NewGroup(
		huh.NewConfirm().
			Title(fmt.Sprintf("Import %d sessions (%s turns) into %s?", len(sessions), historyTurnCount(turns, approx), historyDestinationName(opts))).
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
