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

	"golang.org/x/term"

	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats/capture"
)

// usageStatsConfigValue reads the reporting mode from config.env.
//
// It reads the file directly and never calls dotenv.ApplyEnv: those call sites
// are per-branch because renderLocalBanner and runDoctorCommand both need to
// see the environment before the merge.
var usageStatsConfigValue = sync.OnceValue(func() string {
	fileEnv, err := dotenv.ReadDotenv(dotenv.FilePath(), nil)
	if err != nil {
		return ""
	}
	value, _, _ := envconfig.LookupMap(fileEnv, usagestats.EnvSuffix)
	return value
})

// usageStatsShell is captured at package init, which runs before run() merges
// config.env into the environment. Reading it later would let a config.env
// value pass for a shell one and override DO_NOT_TRACK.
var usageStatsShell = usagestats.CaptureShellEnv()

// emitUsage is the test seam. Production points at emitUsageEvent.
var emitUsage = emitUsageEvent

var usageOnce sync.Once

// emitUsageOnce emits at most one event per invocation.
func emitUsageOnce() {
	usageOnce.Do(func() {
		emitUsage(capture.Snapshot(), time.Now())
	})
}

// resetUsageOnceForTest rearms the process-global guard between tests.
func resetUsageOnceForTest() {
	usageOnce = sync.Once{}
}

// emitUsageEvent resolves the mode and either prints the event or sends it.
// Every failure is silent: reporting must not change what the user sees or
// what the process returns.
func emitUsageEvent(inv capture.Invocation, now time.Time) {
	if inv.Suppressed {
		return
	}
	mode := usagestats.ResolveMode(usageStatsShell, usageStatsConfigValue)
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

// logUsageEvent prints the exact bytes Export would send. Same struct, same
// encoder, so the preview cannot drift from the payload.
func logUsageEvent(w io.Writer, event usagestats.Event) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "%s\n", body)
}

// buildUsageEvent assembles the event from the recorded invocation.
//
// Argv-derived fields arrive already resolved against a closed vocabulary by
// the dispatch branch that recorded them. This must not read os.Args: that is
// what keeps "no argument value reaches the wire" a property of the code.
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

// usageOutcome maps the invocation to an outcome and a coarse error kind.
//
// Launched is terminal: after the execve handoff no later fact about this
// process is meaningful. A missing Completed means the dispatcher never
// returned, which detects a panic without calling recover().
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
//
// term.IsTerminal, not a ModeCharDevice check: /dev/null and Windows NUL are
// character devices too, so the mode bit reports `>/dev/null` as interactive.
// The rest of the binary already asks isatty (clihelp, history).
func stdoutIsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// resolveUsageSkill returns name when it is a bundled skill, else "".
// The bundled set is embedded at build time, so a member of it is not user
// input. Anything else is dropped.
func resolveUsageSkill(name string) string {
	if name == "" {
		return ""
	}
	if slices.Contains(skills.Names(), name) {
		return name
	}
	return ""
}

// usageFlagNames returns the allowlisted flag names in args, sorted and
// comma-joined.
//
// Parsing stops at the first bare "--" so arguments forwarded to the wrapped
// CLI cannot reach the event, and a name survives only if this binary defines
// it somewhere. Values are discarded in every form.
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

// flagName returns a flag argument's bare name, without dashes or value.
func flagName(arg string) (string, bool) {
	if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
		return "", false
	}
	trimmed := strings.TrimLeft(arg, "-")
	if i := strings.IndexByte(trimmed, '='); i >= 0 {
		trimmed = trimmed[:i]
	}
	if trimmed == "" {
		return "", false
	}
	return trimmed, true
}

// usageFlagAllowlist is the union of every flag name the binary defines,
// derived from the flag sets themselves so a new flag is covered as soon as it
// is defined rather than when someone remembers to list it.
//
// The help-page registry is not sufficient on its own: helpFlags returns nil
// for doctor and `guards test`, which own their flag sets elsewhere, so those
// are added from their own constructors.
var usageFlagAllowlist = sync.OnceValue(func() map[string]bool {
	allowed := map[string]bool{}
	add := func(fs *flag.FlagSet) {
		fs.VisitAll(func(f *flag.Flag) { allowed[f.Name] = true })
	}

	for path := range publicHelpPages() {
		fs := helpFlags(path)
		if fs == nil {
			fs = newCommandFlags(path)
		}
		add(fs)
	}

	var guardsOpts guardsTestOptions
	add(newGuardsTestFlags(&guardsOpts))
	for _, name := range doctor.FlagNames() {
		allowed[name] = true
	}

	// Answered before any command flag set is built, so not in the registry.
	for _, name := range []string{"help", "h", "version"} {
		allowed[name] = true
	}
	return allowed
})
