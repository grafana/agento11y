package entry

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
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
	// so the loop above cannot see them. Checking them from their own
	// constructors is the point: the first version of this test shared the
	// allowlist's blind spot and passed while those flags went unrecorded.
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

// TestUsageFlagNamesRecordsDoctorAndGuardsFlags is the regression test for the
// gap above: these commands' flags were silently dropped.
func TestUsageFlagNamesRecordsDoctorAndGuardsFlags(t *testing.T) {
	if got := usageFlagNames([]string{"--require-cloud", "--no-color"}); got != "no-color,require-cloud" {
		t.Errorf("doctor flags = %q, want %q", got, "no-color,require-cloud")
	}
	if got := usageFlagNames([]string{"--stdin", "--tool", "Bash"}); got != "stdin,tool" {
		t.Errorf("guards flags = %q, want %q", got, "stdin,tool")
	}
}

// TestStdoutIsTTYIsNotJustACharDevice pins the fix for a ModeCharDevice check,
// which reports /dev/null as interactive.
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

// TestLogModeMatchesExportedBody keeps log mode honest. It is the user's only
// way to inspect what would be sent, and the printer and the POST live in
// different packages, so they are easy to let drift.
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

// captureEmitted runs the emitter and returns what it wrote to stderr. The
// endpoint is unroutable so a wrong test cannot reach the real receiver.
func captureEmitted(t *testing.T, inv capture.Invocation, mode string) string {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(envconfig.PreferredKey(usagestats.EnvSuffix), mode)
	t.Setenv(usagestats.EnvDoNotTrack, "")
	t.Setenv(usagestats.EnvEndpoint, "http://127.0.0.1:0/must-not-be-reached")

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
// renderLocalBanner and runDoctorCommand both need the pre-merge environment.
func TestResolveModeIsStableAcrossApplyEnv(t *testing.T) {
	dir := isolateDotenvHome(t)
	path := dotenv.FilePath()
	if err := os.MkdirAll(dirOf(path), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("AGENTO11Y_ANONYMOUS_USAGE_STATS=disabled\n"), 0o600); err != nil {
		t.Fatalf("write config.env: %v", err)
	}
	t.Logf("config.env at %s (home %s)", path, dir)

	configValue := func() string {
		fileEnv, err := dotenv.ReadDotenv(dotenv.FilePath(), nil)
		if err != nil {
			return ""
		}
		value, _, _ := envconfig.LookupMap(fileEnv, usagestats.EnvSuffix)
		return value
	}

	before := usagestats.ResolveMode(configValue)
	dotenv.ApplyEnv(nil)
	after := usagestats.ResolveMode(configValue)

	if before != usagestats.ModeDisabled {
		t.Errorf("mode before ApplyEnv = %q, want the config.env value %q", before, usagestats.ModeDisabled)
	}
	if after != before {
		t.Errorf("mode changed across ApplyEnv: %q then %q", before, after)
	}
}

func dirOf(path string) string {
	if i := strings.LastIndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

// TestEmitUsageOnceIsNotWiredYet documents the deliberate state: the emitter
// exists and is tested, but no dispatch path reaches it.
func TestEmitUsageOnceIsNotWiredYet(t *testing.T) {
	capture.Reset()
	resetUsageOnceForTest()

	calls := 0
	original := emitUsage
	emitUsage = func(capture.Invocation, time.Time) { calls++ }
	t.Cleanup(func() { emitUsage = original })

	// Twice on purpose: the guard is what makes the wiring safe across the
	// several exit paths that will reach it.
	emitUsageOnce()
	emitUsageOnce()

	if calls != 1 {
		t.Errorf("emitUsageOnce() fired %d times, want exactly 1", calls)
	}
}
