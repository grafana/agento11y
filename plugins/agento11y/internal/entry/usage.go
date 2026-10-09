package entry

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats/capture"
)

// usageStatsConfigValue reads the reporting mode from config.env.
//
// It reads the file directly and never calls dotenv.ApplyEnv. The merge points
// in run() are per-branch on purpose — renderLocalBanner reports which
// spelling the user set, and runDoctorCommand snapshots the environment before
// the merge so doctor can attribute each value to the shell or config.env — so
// the emitter must not move them.
//
// Because the shell is read through envconfig.LookupEnv and the file through
// this separate read, the resolved mode is the same whether or not ApplyEnv has
// already run in the branch that reached the emitter: ApplyEnv only ever
// promotes a file value into a blank shell slot, and both paths funnel through
// the same parser. TestResolveModeIsStableAcrossApplyEnv pins that.
//
// Memoised because the emitter can be reached from more than one exit path
// before the once-guard settles.
var usageStatsConfigValue = sync.OnceValue(func() string {
	fileEnv, err := dotenv.ReadDotenv(dotenv.FilePath(), nil)
	if err != nil {
		return ""
	}
	value, _, _ := envconfig.LookupMap(fileEnv, usagestats.EnvSuffix)
	return value
})

// emitUsage is the seam tests replace with a recorder. Production points at
// emitUsageEvent.
var emitUsage = emitUsageEvent

// usageOnce guarantees at most one event per invocation. The snapshot and the
// event are both built inside the Do, so a fact recorded after the first emit
// cannot retroactively change an event that was already sent.
var usageOnce sync.Once

// emitUsageOnce emits the usage event for this invocation, at most once.
//
// Nothing calls this yet. The lifecycle wiring — wrapping the exit seam,
// deferring from Main, and hooking the execve handoff — lands separately, so
// that the question of what is collected can be reviewed apart from the
// question of exactly when it fires.
func emitUsageOnce() {
	usageOnce.Do(func() {
		emitUsage(capture.Snapshot(), time.Now())
	})
}

// resetUsageOnceForTest rearms the once-guard. The guard is process-global, so
// without this a second test in the same binary would silently emit nothing.
func resetUsageOnceForTest() {
	usageOnce = sync.Once{}
}

// emitUsageEvent resolves the mode and either prints the event or sends it.
//
// Every failure here is silent and non-fatal. Usage statistics must never
// change what the user sees or what the process returns, so a missing config
// file, an unwritable state directory, or an unreachable endpoint all end with
// the command's own behaviour untouched.
func emitUsageEvent(inv capture.Invocation, now time.Time) {
	if inv.Suppressed {
		return
	}
	mode := usagestats.ResolveMode(usageStatsConfigValue)
	if mode == usagestats.ModeDisabled {
		return
	}
	event := buildUsageEvent(inv, now)
	if mode == usagestats.ModeLog {
		logUsageEvent(os.Stderr, event)
		return
	}
	usagestats.Export(event, version)
}

// logUsageEvent prints the exact bytes Export would send.
//
// It marshals the same struct with the same encoder so the preview cannot
// drift from the payload: a log mode that showed a different shape than the
// wire would be worse than no log mode, because it would be trusted.
// TestLogModeMatchesExportedBody asserts the two are byte-identical.
func logUsageEvent(w io.Writer, event usagestats.Event) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", body)
}

// buildUsageEvent assembles the event from the recorded invocation.
//
// Every field drawn from argv arrives here already resolved against a closed
// vocabulary by the dispatch branch that recorded it. This function does not
// see os.Args and must not read it: that is what keeps "no argument value can
// reach the wire" a property of the code rather than a promise in a comment.
func buildUsageEvent(inv capture.Invocation, now time.Time) usagestats.Event {
	installID, persisted := usagestats.InstallID()
	ciProvider, isCI := usagestats.DetectCI()
	outcome, errorKind := usageOutcome(inv)

	command := inv.Command
	if command == "" {
		command = usagestats.CommandUnknown
	}

	return usagestats.Event{
		Service:            usagestats.ServiceName,
		Version:            version,
		OS:                 runtime.GOOS,
		Arch:               runtime.GOARCH,
		InstallID:          installID,
		InstallIDPersisted: persisted,
		Command:            command,
		Surface:            inv.Surface,
		Agent:              inv.Agent,
		Skill:              inv.Skill,
		Flags:              inv.Flags,
		Outcome:            outcome,
		ExitCode:           inv.ExitCode,
		ErrorKind:          errorKind,
		DurationMS:         inv.Duration(now).Milliseconds(),
		IsTTY:              stdoutIsTTY(),
		IsCI:               isCI,
		CIProvider:         ciProvider,
	}
}

// usageOutcome maps the recorded invocation to an outcome and a coarse error
// kind.
//
// Launched is checked first and is terminal: once the execve handoff is
// reached the process image is replaced, so no later fact about this process
// is meaningful. A launcher whose exec syscall then fails still reports
// "launched", because the attempt is what we are counting and the failure is
// already visible as a non-zero exit from the user's shell.
//
// A missing Completed flag means the dispatcher did not return, which is how a
// panic is detected without calling recover() and altering the crash output.
func usageOutcome(inv capture.Invocation) (outcome, errorKind string) {
	switch {
	case inv.Launched:
		return usagestats.OutcomeLaunched, ""
	case !inv.Completed:
		return usagestats.OutcomePanic, usagestats.ErrorKindRuntime
	case inv.ExitCode == 2:
		return usagestats.OutcomeUsageError, usagestats.ErrorKindUsage
	case inv.ExitCode != 0:
		return usagestats.OutcomeError, usagestats.ErrorKindRuntime
	case inv.Surface == usagestats.SurfaceHelp:
		return usagestats.OutcomeHelp, ""
	default:
		return usagestats.OutcomeOK, ""
	}
}

// stdoutIsTTY reports whether stdout is an interactive terminal.
func stdoutIsTTY() bool {
	file, ok := any(os.Stdout).(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

// resolveUsageSkill returns name when it is a bundled skill, else "".
//
// The bundled set is embedded at build time (internal/skills), so it is a
// closed vocabulary and a member of it is safe to report. This is the one
// dimension agento11y can carry that gcx cannot: there, skill names are
// arbitrary user input and so are categorically never sent.
//
// Anything else — a typo, a path traversal attempt, a private skill name — is
// dropped rather than recorded as "unknown", because the distinction between
// "no skill argument" and "an unrecognised one" is not worth a field.
func resolveUsageSkill(name string) string {
	if name == "" {
		return ""
	}
	if slices.Contains(skills.Names(), name) {
		return name
	}
	return ""
}

// usageFlagNames extracts the allowlisted flag names from a command's
// arguments, sorted and comma-joined.
//
// Two rules carry the privacy guarantee:
//
//   - Parsing stops at the first bare "--". Everything after it belongs to the
//     wrapped CLI, so `agento11y claude -- --dangerously-skip-permissions`
//     must contribute nothing. Those tokens are the user's prompt-adjacent
//     arguments and are none of our business.
//   - A name survives only if the binary defines it for some command. The
//     allowlist — not a blocklist, not a heuristic — is the guarantee, because
//     an unrecognised token is by definition one this binary does not control.
//
// Values are discarded in every form: "--flag=value" keeps only "flag", and a
// separated "--flag value" never considers the value because it does not start
// with a dash.
func usageFlagNames(args []string) string {
	allowed := usageFlagAllowlist()
	var names []string
	for _, arg := range args {
		if arg == "--" {
			break
		}
		name, ok := flagName(arg)
		if !ok || !allowed[name] {
			continue
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return strings.Join(names, ",")
}

// flagName returns the bare name of a flag argument, without dashes or value.
func flagName(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return "", false
	}
	trimmed := strings.TrimLeft(arg, "-")
	if trimmed == "" {
		return "", false
	}
	if i := strings.IndexByte(trimmed, '='); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// usageFlagAllowlist is the union of every flag name the binary defines,
// collected by walking the help-page registry and asking each command for its
// flag set.
//
// Deriving it rather than hand-listing it is deliberate: a hand-written list
// goes stale silently, recording nothing for a newly added flag, whereas this
// picks up a new flag as soon as it is defined.
// TestUsageFlagAllowlistCoversDefinedFlags pins the derivation.
var usageFlagAllowlist = sync.OnceValue(func() map[string]bool {
	allowed := map[string]bool{}
	for path := range publicHelpPages() {
		fs := helpFlags(path)
		if fs == nil {
			fs = newCommandFlags(path)
		}
		fs.VisitAll(func(f *flag.Flag) { allowed[f.Name] = true })
	}
	// Help and version are answered before any command flag set is built, so
	// they are not in any registry, yet they are the two most common flags a
	// user types.
	for _, name := range []string{"help", "h", "version"} {
		allowed[name] = true
	}
	return allowed
})
