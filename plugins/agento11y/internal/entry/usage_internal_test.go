package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/launcher"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats"
	"github.com/grafana/agento11y/plugins/agento11y/internal/usagestats/capture"
)

// sampleInvocation is a representative recorded invocation.
func sampleInvocation(now time.Time) capture.Invocation {
	return capture.Invocation{
		Start:     now.Add(-42 * time.Millisecond),
		Command:   "skills show",
		Surface:   usagestats.SurfaceCommand,
		Skill:     "setup-coding-agent",
		Flags:     "json",
		Completed: true,
	}
}

func TestStartUsageRecordsOnlyResolvedFields(t *testing.T) {
	for _, tc := range []struct {
		name, surface, command, agent, skill, flags string
		suppressed                                  bool
		args                                        []string
	}{
		{name: "skills list", args: []string{"skills", "list"}, surface: usagestats.SurfaceCommand, command: "skills list"},
		{name: "known skill", args: []string{"skills", "show", "setup-coding-agent"}, surface: usagestats.SurfaceCommand, command: "skills show", skill: "setup-coding-agent"},
		{name: "unknown skill", args: []string{"skills", "show", "private-value"}, surface: usagestats.SurfaceCommand, command: "skills show"},
		{name: "launcher", args: []string{"codex", "--local", "--", "private-value"}, surface: usagestats.SurfaceLauncher, command: "codex", agent: "codex", flags: "local"},
		{name: "bare help", surface: usagestats.SurfaceHelp, command: "help"},
		{name: "long root help", args: []string{"--help"}, surface: usagestats.SurfaceHelp, command: "help", flags: "help"},
		{name: "short root help", args: []string{"-h"}, surface: usagestats.SurfaceHelp, command: "help", flags: "h"},
		{name: "help command", args: []string{"help"}, surface: usagestats.SurfaceHelp, command: "help"},
		{name: "help flag", args: []string{"skills", "list", "--help"}, surface: usagestats.SurfaceHelp, command: "skills list", flags: "help"},
		{name: "hook", args: []string{"codex", "hook"}, suppressed: true},
		{name: "daemon", args: []string{"local", "serve"}, suppressed: true},
		{name: "version", args: []string{"--version"}, suppressed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture.Reset()
			startUsage(tc.args)
			got := capture.Snapshot()
			if got.Start.IsZero() || got.Surface != tc.surface || got.Command != tc.command || got.Agent != tc.agent || got.Skill != tc.skill || got.Flags != tc.flags || got.Suppressed != tc.suppressed {
				t.Errorf("capture = %+v", got)
			}
		})
	}
	capture.Reset()
}

func TestUsageNoticeAppearsOncePerRevision(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldVersion := version
	version = "v0.1.0"
	t.Cleanup(func() { version = oldVersion })
	capture.Reset()
	startUsage([]string{"skills", "list"})
	oldShell := usageStatsShell
	usageStatsShell = usagestats.ShellEnv{Mode: "enabled"}
	t.Cleanup(func() { usageStatsShell = oldShell; capture.Reset() })

	var first bytes.Buffer
	showUsageNotice(&first)
	want := "The agento11y CLI sends anonymous usage statistics about its own commands, never operations performed by a coding agent. " +
		"To turn reporting off, set AGENTO11Y_ANONYMOUS_USAGE_STATS=disabled " +
		"or DO_NOT_TRACK=1 in your shell.\n"
	if first.String() != want {
		t.Fatalf("first-run notice = %q, want %q", first.String(), want)
	}
	var second bytes.Buffer
	showUsageNotice(&second)
	if second.Len() != 0 {
		t.Errorf("notice repeated for same revision: %q", second.String())
	}

	marker := filepath.Join(filepath.Dir(usagestats.InstallIDPath()), "usage-stats-notice")
	if err := os.WriteFile(marker, []byte("0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var revised bytes.Buffer
	showUsageNotice(&revised)
	if revised.Len() == 0 {
		t.Error("new notice revision was not shown")
	}
}

func TestUsageNoticeSkipsSuppressedAndDisabledInvocations(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldShell := usageStatsShell
	t.Cleanup(func() { usageStatsShell = oldShell; capture.Reset() })
	for _, tc := range []struct {
		args []string
		mode string
	}{
		{args: []string{"--version"}, mode: "enabled"},
		{args: []string{"codex", "hook"}, mode: "enabled"},
		{args: []string{"skills", "list"}, mode: "disabled"},
	} {
		capture.Reset()
		startUsage(tc.args)
		usageStatsShell = usagestats.ShellEnv{Mode: tc.mode}
		var out bytes.Buffer
		showUsageNotice(&out)
		if out.Len() != 0 {
			t.Errorf("args=%v mode=%s: unexpected notice %q", tc.args, tc.mode, out.String())
		}
	}
}

func TestBuildUsageEventFillsTheEnvelope(t *testing.T) {
	now := time.Now()
	event := buildUsageEvent(sampleInvocation(now), now)

	if event.Service != usagestats.ServiceName {
		t.Errorf("Service = %q, want %q", event.Service, usagestats.ServiceName)
	}
	if event.Command != "skills show" || event.Surface != usagestats.SurfaceCommand {
		t.Errorf("command/surface = %q/%q", event.Command, event.Surface)
	}
	if event.Skill != "setup-coding-agent" || event.Flags != "json" {
		t.Errorf("skill/flags = %q/%q", event.Skill, event.Flags)
	}
	if event.Outcome != usagestats.OutcomeOK || event.ErrorKind != "" {
		t.Errorf("outcome/errorKind = %q/%q", event.Outcome, event.ErrorKind)
	}
	if event.DurationMS != 42 {
		t.Errorf("DurationMS = %d, want 42", event.DurationMS)
	}
	if event.InstallID == "" {
		t.Error("InstallID is empty")
	}
	if event.OS == "" || event.Arch == "" {
		t.Errorf("os/arch = %q/%q", event.OS, event.Arch)
	}
}

// TestBuildUsageEventRecordsUnknownCommand: the placeholder lets a query tell
// "unrecognised" from "field missing".
func TestBuildUsageEventRecordsUnknownCommand(t *testing.T) {
	now := time.Now()
	event := buildUsageEvent(capture.Invocation{Start: now, Completed: true, ExitCode: 2}, now)

	if event.Command != usagestats.CommandUnknown {
		t.Errorf("Command = %q, want %q", event.Command, usagestats.CommandUnknown)
	}
}

func TestUsageOutcome(t *testing.T) {
	tests := []struct {
		name          string
		inv           capture.Invocation
		wantOutcome   string
		wantErrorKind string
	}{
		{
			name:        "clean command",
			inv:         capture.Invocation{Completed: true},
			wantOutcome: usagestats.OutcomeOK,
		},
		{
			name:        "help",
			inv:         capture.Invocation{Completed: true, Surface: usagestats.SurfaceHelp},
			wantOutcome: usagestats.OutcomeHelp,
		},
		{
			name:          "usage error",
			inv:           capture.Invocation{Completed: true, ExitCode: 2},
			wantOutcome:   usagestats.OutcomeUsageError,
			wantErrorKind: usagestats.ErrorKindUsage,
		},
		{
			name:          "runtime error",
			inv:           capture.Invocation{Completed: true, ExitCode: 1},
			wantOutcome:   usagestats.OutcomeError,
			wantErrorKind: usagestats.ErrorKindRuntime,
		},
		{
			// Completed never set: detects a panic without recover().
			name:          "panic",
			inv:           capture.Invocation{},
			wantOutcome:   usagestats.OutcomePanic,
			wantErrorKind: usagestats.ErrorKindRuntime,
		},
		{
			name:        "launched",
			inv:         capture.Invocation{Launched: true, Surface: usagestats.SurfaceLauncher},
			wantOutcome: usagestats.OutcomeLaunched,
		},
		{
			// Launched is terminal: a later exit code is not about this run.
			name:        "launched wins over a later exit code",
			inv:         capture.Invocation{Launched: true, ExitCode: 1, Completed: true},
			wantOutcome: usagestats.OutcomeLaunched,
		},
		{
			name:        "launched wins over a missing completion",
			inv:         capture.Invocation{Launched: true},
			wantOutcome: usagestats.OutcomeLaunched,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			outcome, errorKind := usageOutcome(tc.inv)
			if outcome != tc.wantOutcome || errorKind != tc.wantErrorKind {
				t.Errorf("usageOutcome() = %q, %q; want %q, %q", outcome, errorKind, tc.wantOutcome, tc.wantErrorKind)
			}
		})
	}
}

// TestNoArgvValueReachesTheEvent is the test that would catch a real leak: it
// drives the builder with secrets in every argv position that has ever been a
// plausible one and asserts none reaches the marshalled event.
func TestNoArgvValueReachesTheEvent(t *testing.T) {
	secrets := []string{
		"sk-live-abcdef123456",
		"glc_eyJrIjoic2VjcmV0In0=",
		"acme.grafana.net",
		"https://acme.grafana.net",
		"123456",
		"customer=acme",
		"acme-migration",
		"/Users/someone/code/secret-project",
		"../../etc/passwd",
		"--dangerously-skip-permissions",
		"ivana@example.com",
		"feature/private-branch",
	}

	corpus := [][]string{
		{"login", "--token", "sk-live-abcdef123456"},
		{"login", "--endpoint", "https://acme.grafana.net", "--tenant", "123456"},
		{"login", "--token-stdin"},
		{"claude", "--tag", "customer=acme", "--", "--dangerously-skip-permissions"},
		{"codex", "--local", "--", "/Users/someone/code/secret-project"},
		{"skills", "show", "../../etc/passwd"},
		{"history", "import", "claude-code", "--since", "2026-01-01"},
		{"doctor", "--json"},
		{"guards", "test", "--tool", "Bash", "--", "rm -rf /Users/someone"},
		{"local", "status", "--json"},
	}

	now := time.Now()
	for _, args := range corpus {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			// Build it the way dispatch does: resolved path, allowlisted flag
			// names, validated skill. Nothing else from argv is offered.
			path, consumed, _ := resolveCommandPath(args)
			rest := args
			if consumed > 0 {
				rest = args[consumed:]
			}
			skill := ""
			if path == "skills show" && len(rest) > 0 {
				skill = resolveUsageSkill(rest[0])
			}

			inv := capture.Invocation{
				Start:     now,
				Surface:   usagestats.SurfaceCommand,
				Command:   path,
				Flags:     usageFlagNames(rest),
				Skill:     skill,
				Completed: true,
			}

			body, err := json.Marshal(buildUsageEvent(inv, now))
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			for _, secret := range secrets {
				if bytes.Contains(body, []byte(secret)) {
					t.Errorf("event carries %q\nargv: %v\nbody: %s", secret, args, body)
				}
			}
		})
	}
}

// TestUsageFlagNamesStopsAtDoubleDash keeps arguments forwarded to the wrapped
// CLI out of the event.
func TestUsageFlagNamesStopsAtDoubleDash(t *testing.T) {
	got := usageFlagNames([]string{"--local", "--", "--json", "--token"})
	if strings.Contains(got, "json") || strings.Contains(got, "token") {
		t.Errorf("flags = %q; everything after a bare -- belongs to the wrapped CLI", got)
	}
	if got != "local" {
		t.Errorf("flags = %q, want %q", got, "local")
	}
}

func TestUsageFlagNamesDropsValuesAndUnknowns(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "no flags", args: []string{"show", "setup-coding-agent"}, want: ""},
		{name: "equals form keeps only the name", args: []string{"--tenant=123456"}, want: "tenant"},
		{name: "separated value is never a flag", args: []string{"--tenant", "123456"}, want: "tenant"},
		{name: "unknown flag dropped", args: []string{"--totally-made-up"}, want: ""},
		{name: "sorted and deduplicated", args: []string{"--json", "--local", "--json"}, want: "json,local"},
		{name: "bare dash ignored", args: []string{"-"}, want: ""},
		{name: "help is allowed", args: []string{"--help"}, want: "help"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageFlagNames(tc.args); got != tc.want {
				t.Errorf("usageFlagNames(%v) = %q, want %q", tc.args, got, tc.want)
			}
		})
	}
}

// TestUsageFlagAllowlistCoversDefinedFlags pins the derivation: a hand-written
// allowlist would silently record nothing for a newly added flag.
func TestUsageFlagAllowlistCoversDefinedFlags(t *testing.T) {
	allowed := usageFlagAllowlist()

	for path := range publicHelpPages() {
		fs := helpFlags(path)
		if fs == nil {
			fs = newCommandFlags(path)
		}
		fs.VisitAll(func(f *flag.Flag) {
			if !allowed[f.Name] {
				t.Errorf("flag %q of command %q is defined but not allowlisted", f.Name, path)
			}
		})
	}

	// doctor and `guards test` own their flag sets outside the help registry,
	// so the loop above cannot see them. They must be checked from their own
	// constructors, or this test shares the allowlist's blind spot.
	var guardsOpts guardsTestOptions
	newGuardsTestFlags(&guardsOpts).VisitAll(func(f *flag.Flag) {
		if !allowed[f.Name] {
			t.Errorf("guards test flag %q is defined but not allowlisted", f.Name)
		}
	})
	for _, name := range doctor.FlagNames() {
		if !allowed[name] {
			t.Errorf("doctor flag %q is defined but not allowlisted", name)
		}
	}

	// Spot-check so a trivially-true allowlist fails here.
	if !allowed["json"] {
		t.Error("allowlist is missing --json")
	}
	if allowed["definitely-not-a-flag"] {
		t.Error("allowlist admits an undefined flag; it must not be permissive")
	}
}

// TestUsageFlagNamesRecordsDoctorAndGuardsFlags: these commands' flags are the
// ones the registry walk cannot reach.
func TestUsageFlagNamesRecordsDoctorAndGuardsFlags(t *testing.T) {
	if got := usageFlagNames([]string{"--require-cloud", "--no-color"}); got != "no-color,require-cloud" {
		t.Errorf("doctor flags = %q, want %q", got, "no-color,require-cloud")
	}
	if got := usageFlagNames([]string{"--stdin", "--tool", "Bash"}); got != "stdin,tool" {
		t.Errorf("guards flags = %q, want %q", got, "stdin,tool")
	}
}

// TestStdoutIsTTYIsNotJustACharDevice: a ModeCharDevice check reports
// /dev/null as interactive.
func TestStdoutIsTTYIsNotJustACharDevice(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("cannot open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = devNull.Close() })

	info, err := devNull.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode()&os.ModeCharDevice == 0 {
		t.Skip("platform does not report the null device as a character device")
	}

	original := os.Stdout
	os.Stdout = devNull
	t.Cleanup(func() { os.Stdout = original })

	if stdoutIsTTY() {
		t.Error("stdoutIsTTY() = true for the null device; it must ask isatty, not the mode bit")
	}
}

// TestResolveUsageSkillOnlyAcceptsBundledSkills: the bundled set is embedded
// at build time, so a member is not user input. Anything else is dropped.
func TestResolveUsageSkillOnlyAcceptsBundledSkills(t *testing.T) {
	names := skills.Names()
	if len(names) == 0 {
		t.Fatal("no bundled skills; the allowlist would be vacuous")
	}
	if got := resolveUsageSkill(names[0]); got != names[0] {
		t.Errorf("resolveUsageSkill(%q) = %q, want it accepted", names[0], got)
	}

	for _, bad := range []string{"", "   ", "../../etc/passwd", "my-private-skill", "SETUP-CODING-AGENT", "setup-coding-agent/../x"} {
		if got := resolveUsageSkill(bad); got != "" {
			t.Errorf("resolveUsageSkill(%q) = %q, want it dropped", bad, got)
		}
	}
}

// TestLogModeMatchesExportedBody keeps log mode honest: it is the user's only
// way to inspect what would be sent, and the printer and the POST live in
// different packages.
func TestLogModeMatchesExportedBody(t *testing.T) {
	now := time.Now()
	event := buildUsageEvent(sampleInvocation(now), now)

	var logged bytes.Buffer
	logUsageEvent(&logged, event)

	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(usagestats.EnvEndpoint, server.URL)
	usagestats.Export(event, version)
	posted := <-bodies

	if got, want := strings.TrimSpace(logged.String()), string(posted); got != want {
		t.Errorf("log mode and the wire disagree\nlogged: %s\nposted: %s", got, want)
	}
}

func TestEmitUsageEventRespectsMode(t *testing.T) {
	now := time.Now()
	inv := sampleInvocation(now)

	t.Run("suppressed emits nothing", func(t *testing.T) {
		suppressed := inv
		suppressed.Suppressed = true
		if got := captureEmitted(t, suppressed, ""); got != "" {
			t.Errorf("a suppressed invocation reported %q", got)
		}
	})

	t.Run("disabled emits nothing", func(t *testing.T) {
		if got := captureEmitted(t, inv, "disabled"); got != "" {
			t.Errorf("a disabled invocation reported %q", got)
		}
	})

	t.Run("log prints and sends nothing", func(t *testing.T) {
		got := captureEmitted(t, inv, "log")
		if got == "" {
			t.Fatal("log mode printed nothing")
		}
		var decoded usagestats.Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &decoded); err != nil {
			t.Fatalf("log output is not an Event: %v", err)
		}
		if decoded.Command != "skills show" {
			t.Errorf("logged command = %q", decoded.Command)
		}
	})
}

func TestUsageStatsOnlySendFromVersionedBuilds(t *testing.T) {
	isolateDotenvHome(t)
	t.Setenv(usagestats.EnvDoNotTrack, "")
	freshUsageStatsConfigValue(t)
	for _, key := range []string{envconfig.PreferredKey(usagestats.EnvSuffix), envconfig.LegacyKey(usagestats.EnvSuffix)} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}

	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	t.Setenv(usagestats.EnvEndpoint, server.URL)

	oldVersion, oldShell := version, usageStatsShell
	t.Cleanup(func() { version, usageStatsShell = oldVersion, oldShell })
	usageStatsShell = usagestats.CaptureShellEnv()
	version = "dev"
	if got := usageStatsMode(); got != usagestats.ModeDisabled {
		t.Fatalf("development parent mode = %q, want disabled", got)
	}
	for range 2 {
		propagateUsageStatsPreference()
		dotenv.ApplyEnv(nil)
		for _, key := range []string{envconfig.PreferredKey(usagestats.EnvSuffix), envconfig.LegacyKey(usagestats.EnvSuffix)} {
			if value, exists := os.LookupEnv(key); exists {
				t.Fatalf("development parent synthesized %s=%q without a preference", key, value)
			}
		}
	}
	assertUsageStatsSubprocess(t, usagestats.ModeEnabled)

	usageStatsShell = usagestats.ShellEnv{Mode: "enabled"}
	inv := sampleInvocation(time.Now())
	for _, candidate := range []string{"dev", "dev-60b2ab8", "dev-60b2ab8-dirty", ""} {
		version = candidate
		if mode := usageStatsMode(); mode != usagestats.ModeDisabled {
			t.Errorf("version %q: mode = %q, want disabled", candidate, mode)
		}
		emitUsageEvent(inv, time.Now())
	}
	if requests != 0 {
		t.Fatalf("development builds sent %d events", requests)
	}
	version = "v0.1.0"
	if mode := usageStatsMode(); mode != usagestats.ModeEnabled {
		t.Fatalf("release mode = %q, want enabled", mode)
	}
	emitUsageEvent(inv, time.Now())
	if requests != 1 {
		t.Errorf("versioned build sent %d events, want 1", requests)
	}
}

// captureEmitted runs the emitter and returns what it wrote to stderr. The
// endpoint is unroutable so a wrong test cannot send anything.
func captureEmitted(t *testing.T, inv capture.Invocation, mode string) string {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(usagestats.EnvDoNotTrack, "")
	t.Setenv(usagestats.EnvEndpoint, "http://127.0.0.1:0/must-not-be-reached")

	// The emitter reads the snapshot taken at init, not the live environment.
	originalShell := usageStatsShell
	usageStatsShell = usagestats.ShellEnv{Mode: mode}
	t.Cleanup(func() { usageStatsShell = originalShell })

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stderr
	os.Stderr = write
	emitUsageEvent(inv, time.Now())
	os.Stderr = original
	_ = write.Close()

	out, _ := io.ReadAll(read)
	_ = read.Close()
	return string(out)
}

// TestResolveModeIsStableAcrossApplyEnv lets the emitter resolve its mode
// without moving dotenv.ApplyEnv, whose call sites are per-branch because
// renderLocalBanner and runDoctorCommand need the pre-merge environment.
func TestResolveModeIsStableAcrossApplyEnv(t *testing.T) {
	writeUsageStatsConfig(t, "AGENTO11Y_ANONYMOUS_USAGE_STATS=disabled\n")

	shell := usagestats.CaptureShellEnv()
	before := usagestats.ResolveMode(shell, freshUsageStatsConfigValue(t))
	dotenv.ApplyEnv(nil)
	after := usagestats.ResolveMode(shell, freshUsageStatsConfigValue(t))

	if before != usagestats.ModeDisabled {
		t.Errorf("mode before ApplyEnv = %q, want the config.env value %q", before, usagestats.ModeDisabled)
	}
	if after != before {
		t.Errorf("mode changed across ApplyEnv: %q then %q", before, after)
	}
}

// TestDoNotTrackIsNotDefeatedByConfigEnv is the end-to-end guard for the bug
// the snapshot fixes.
//
// dotenv.ApplyEnv writes config.env values into the environment under the
// branded names. Resolving from the live environment afterwards read the file's
// value as though the user had exported it, which outranks DO_NOT_TRACK and
// silently re-enabled reporting. Capturing the shell before the merge is what
// keeps the opt-out intact.
func TestDoNotTrackIsNotDefeatedByConfigEnv(t *testing.T) {
	for _, fileKey := range []string{envconfig.PreferredKey(usagestats.EnvSuffix), envconfig.LegacyKey(usagestats.EnvSuffix)} {
		t.Run(fileKey, func(t *testing.T) {
			for _, tc := range []struct {
				name, dnt, preferred, legacy, file string
				want                               usagestats.Mode
				subprocess                         bool
			}{
				{name: "DNT 1", dnt: "1", file: "enabled", want: usagestats.ModeDisabled, subprocess: true},
				{name: "DNT trimmed true", dnt: " \tTrUe \t", file: "enabled", want: usagestats.ModeDisabled},
				{name: "preferred enabled wins", dnt: "1", preferred: "enabled", file: "disabled", want: usagestats.ModeEnabled},
				{name: "legacy enabled wins", dnt: "1", legacy: "enabled", file: "disabled", want: usagestats.ModeEnabled, subprocess: true},
				{name: "blank preferred", preferred: " \t", legacy: "disabled", file: "enabled", want: usagestats.ModeDisabled},
				{name: "shell log", preferred: "log", file: "enabled", want: usagestats.ModeLog},
				{name: "shell disabled", preferred: "disabled", file: "enabled", want: usagestats.ModeDisabled},
				{name: "shell invalid", preferred: "invalid", file: "enabled", want: usagestats.ModeDisabled},
				{name: "legacy log", legacy: "log", file: "enabled", want: usagestats.ModeLog},
				{name: "legacy invalid", legacy: "invalid", file: "enabled", want: usagestats.ModeDisabled},
				{name: "DNT 0 inactive", dnt: "0", file: "enabled", want: usagestats.ModeEnabled},
				{name: "DNT false inactive", dnt: "false", file: "enabled", want: usagestats.ModeEnabled},
				{name: "DNT yes inactive", dnt: "yes", file: "enabled", want: usagestats.ModeEnabled},
				{name: "DNT on inactive", dnt: "on", file: "enabled", want: usagestats.ModeEnabled},
				{name: "DNT empty inactive", file: "enabled", want: usagestats.ModeEnabled},
				{name: "file disabled", file: "disabled", want: usagestats.ModeDisabled},
				{name: "file log", file: "log", want: usagestats.ModeLog, subprocess: true},
				{name: "file invalid", file: "invalid", want: usagestats.ModeDisabled},
				{name: "missing file", want: usagestats.ModeEnabled},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.file == "" {
						isolateDotenvHome(t)
					} else {
						writeUsageStatsConfig(t, fileKey+"="+tc.file+"\n")
					}
					t.Setenv(usagestats.EnvDoNotTrack, tc.dnt)
					t.Setenv(envconfig.PreferredKey(usagestats.EnvSuffix), tc.preferred)
					t.Setenv(envconfig.LegacyKey(usagestats.EnvSuffix), tc.legacy)
					oldShell := usageStatsShell
					usageStatsShell = usagestats.CaptureShellEnv()
					t.Cleanup(func() { usageStatsShell = oldShell })
					configValue := freshUsageStatsConfigValue(t)
					if got := usagestats.ResolveMode(usageStatsShell, configValue); got != tc.want {
						t.Fatalf("parent mode before merge = %q, want %q", got, tc.want)
					}
					for range 2 {
						propagateUsageStatsPreference()
						dotenv.ApplyEnv(nil)
						if got := usagestats.ResolveMode(usageStatsShell, configValue); got != tc.want {
							t.Fatalf("parent mode after merge = %q, want %q", got, tc.want)
						}
						inherited := string(tc.want)
						if tc.file == "" {
							inherited = ""
						}
						assertInheritedUsageStats(t, inherited)
						if got := usagestats.ResolveMode(usagestats.CaptureShellEnv(), configValue); got != tc.want {
							t.Fatalf("fresh child mode = %q, want %q", got, tc.want)
						}
					}
					if tc.subprocess {
						assertUsageStatsSubprocess(t, tc.want)
					}
				})
			}
		})
	}
}

func assertInheritedUsageStats(t *testing.T, want string) {
	t.Helper()
	for _, key := range []string{envconfig.PreferredKey(usagestats.EnvSuffix), envconfig.LegacyKey(usagestats.EnvSuffix)} {
		if got := os.Getenv(key); got != want {
			t.Errorf("inherited %s = %q, want %q", key, got, want)
		}
	}
}

func assertUsageStatsSubprocess(t *testing.T, want usagestats.Mode) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		env  *local.LaunchEnv
	}{
		{name: "normal"},
		{name: "local", env: &local.LaunchEnv{Endpoint: "http://127.0.0.1:0", OTLPEndpoint: "http://127.0.0.1:0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(executable, "-test.run=^TestUsageStatsInheritedPreference$")
			cmd.Env = append(local.Environ(tc.env), "AGENTO11Y_USAGE_STATS_CHILD_MODE="+string(want))
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("usage preference subprocess: %v\n%s", err, out)
			}
		})
	}
}

func TestUsageStatsInheritedPreference(t *testing.T) {
	want := os.Getenv("AGENTO11Y_USAGE_STATS_CHILD_MODE")
	if want == "" {
		return
	}
	// TestMain clears the live aliases and changes HOME. Only the startup snapshot describes the inherited preference.
	if got := usagestats.ResolveMode(usageStatsShell, nil); got != usagestats.Mode(want) {
		t.Fatalf("startup mode = %q, want %q (snapshot: %+v)", got, want, usageStatsShell)
	}
	oldVersion := version
	version = "v0.1.0"
	t.Cleanup(func() { version = oldVersion })
	if got := usageStatsMode(); got != usagestats.Mode(want) {
		t.Fatalf("versioned child mode = %q, want %q", got, want)
	}
}

// TestUnreadableConfigDoesNotEnableReporting: ReadDotenv returns a partial map
// plus an error, so discarding the error used to discard a saved opt-out and
// fall through to the enabled default.
func TestUnreadableConfigDoesNotEnableReporting(t *testing.T) {
	for _, tc := range []struct {
		name, file, shell string
		want              usagestats.Mode
	}{
		{name: "disabled before scanner error", file: "disabled", want: usagestats.ModeDisabled},
		{name: "enabled before scanner error", file: "enabled", want: usagestats.ModeDisabled},
		{name: "shell enabled over partial enabled", file: "enabled", shell: "enabled", want: usagestats.ModeEnabled},
		{name: "shell enabled over partial disabled", file: "disabled", shell: "enabled", want: usagestats.ModeEnabled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A line past bufio.Scanner's token limit makes the parse fail after the
			// setting has already been read.
			writeUsageStatsConfig(t, "AGENTO11Y_ANONYMOUS_USAGE_STATS="+tc.file+"\n# "+strings.Repeat("x", 70<<10)+"\n")
			t.Setenv(usagestats.EnvDoNotTrack, "")
			t.Setenv(envconfig.PreferredKey(usagestats.EnvSuffix), tc.shell)
			fileEnv, err := dotenv.ReadDotenv(dotenv.FilePath(), nil)
			if err == nil || fileEnv[envconfig.PreferredKey(usagestats.EnvSuffix)] != tc.file {
				t.Fatalf("expected a partial config and scanner error, got value %q, error %v", fileEnv[envconfig.PreferredKey(usagestats.EnvSuffix)], err)
			}
			oldShell := usageStatsShell
			usageStatsShell = usagestats.CaptureShellEnv()
			t.Cleanup(func() { usageStatsShell = oldShell })
			configValue := freshUsageStatsConfigValue(t)
			if got := usagestats.ResolveMode(usageStatsShell, configValue); got != tc.want {
				t.Fatalf("unreadable config mode = %q, want %q", got, tc.want)
			}
			for range 2 {
				propagateUsageStatsPreference()
				dotenv.ApplyEnv(nil)
				assertInheritedUsageStats(t, string(tc.want))
				if got := usagestats.ResolveMode(usagestats.CaptureShellEnv(), configValue); got != tc.want {
					t.Fatalf("child mode = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// writeUsageStatsConfig points $HOME at a tempdir and writes config.env.
func writeUsageStatsConfig(t *testing.T, contents string) {
	t.Helper()
	isolateDotenvHome(t)
	path := dotenv.FilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config.env: %v", err)
	}
}

// freshUsageStatsConfigValue rebuilds the production reader so the test sees
// the config.env it just wrote. Returning a local copy of the logic instead
// would let the production reader regress without failing anything.
func freshUsageStatsConfigValue(t *testing.T) func() string {
	t.Helper()
	original := usageStatsConfigValue
	usageStatsConfigValue = newUsageStatsConfigValue()
	t.Cleanup(func() { usageStatsConfigValue = original })
	return usageStatsConfigValue
}

func TestEmitUsageOnce(t *testing.T) {
	capture.Reset()
	resetUsageOnceForTest()

	calls := 0
	original := emitUsage
	emitUsage = func(capture.Invocation, time.Time) { calls++ }
	t.Cleanup(func() { emitUsage = original })

	// Twice on purpose: the guard is what keeps the several exit paths that
	// will reach it from double-reporting.
	emitUsageOnce()
	emitUsageOnce()

	if calls != 1 {
		t.Errorf("emitUsageOnce() fired %d times, want exactly 1", calls)
	}
}

func TestMainReportsHelpAndLauncherOnceAndSuppressesVersion(t *testing.T) {
	oldArgs, oldShell, oldEmit, oldVersion := os.Args, usageStatsShell, emitUsage, version
	oldLauncher, oldBeforeExec := launchers["codex"], launcher.BeforeExec
	t.Cleanup(func() {
		os.Args, usageStatsShell, emitUsage, version = oldArgs, oldShell, oldEmit, oldVersion
		launchers["codex"], launcher.BeforeExec = oldLauncher, oldBeforeExec
		capture.Reset()
		resetUsageOnceForTest()
	})
	for _, tc := range []struct {
		args    []string
		want    string
		outcome string
		blocked bool
	}{
		{args: []string{"help"}, want: "help", outcome: usagestats.OutcomeHelp},
		{args: []string{"codex"}, want: "codex", outcome: usagestats.OutcomeLaunched},
		{args: []string{"--version"}, blocked: true},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			writeUsageStatsConfig(t, "AGENTO11Y_ANONYMOUS_USAGE_STATS=enabled\n")
			t.Setenv(usagestats.EnvDoNotTrack, "1")
			t.Setenv(usagestats.EnvEndpoint, "http://127.0.0.1:0/must-not-be-reached")
			freshUsageStatsConfigValue(t)
			usageStatsShell = usagestats.CaptureShellEnv()
			capture.Reset()
			resetUsageOnceForTest()
			os.Args = append([]string{"agento11y"}, tc.args...)
			launchCalls := 0
			launchers["codex"] = func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
				launchCalls++
				assertInheritedUsageStats(t, "disabled")
				launcher.BeforeExec()
				return nil
			}
			var events []capture.Invocation
			emitUsage = func(inv capture.Invocation, _ time.Time) {
				assertInheritedUsageStats(t, "disabled")
				events = append(events, inv)
			}
			Main("dev")
			wantLaunchCalls := 0
			if tc.want == "codex" {
				wantLaunchCalls = 1
			}
			if launchCalls != wantLaunchCalls {
				t.Errorf("launcher calls = %d, want %d", launchCalls, wantLaunchCalls)
			}
			if len(events) != 1 {
				t.Fatalf("emission calls = %d, want one lifecycle call", len(events))
			}
			inv := events[0]
			if inv.Suppressed != tc.blocked {
				t.Fatalf("suppressed = %v, want %v", inv.Suppressed, tc.blocked)
			}
			if !tc.blocked {
				outcome, _ := usageOutcome(inv)
				if inv.Command != tc.want || outcome != tc.outcome {
					t.Errorf("command/outcome = %q/%q, want %q/%q", inv.Command, outcome, tc.want, tc.outcome)
				}
			}
		})
	}
}
