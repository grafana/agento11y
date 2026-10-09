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

// TestBuildUsageEventRecordsUnknownCommand pins that a dispatch which
// resolved nothing reports the placeholder rather than an empty string, so a
// query can tell "we did not recognise this" from "the field is missing".
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
			// Completed never set: the dispatcher did not return. This is how
			// a panic is detected without calling recover() and changing what
			// the runtime prints.
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
			// Launched is terminal: the process image is replaced at the
			// handoff, so a later exit code is not about this invocation.
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

// TestNoArgvValueReachesTheEvent is the test that would catch a real leak.
//
// It drives the builder with secrets in every argv position that has ever been
// a plausible leak — a token, an endpoint, a tenant id, a tag value, a path
// traversal, arguments forwarded past "--" — and asserts none of them appears
// anywhere in the marshalled event. One test, every historical leak class.
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
			// Build the event the way dispatch would: a resolved command path
			// from the help registry, an allowlisted flag-name set, and a
			// validated skill. Nothing else from argv is offered to it.
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

// TestUsageFlagNamesStopsAtDoubleDash pins the rule that keeps arguments
// forwarded to the wrapped CLI out of the event. Those tokens are the user's
// own prompt-adjacent arguments.
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

// TestUsageFlagAllowlistCoversDefinedFlags pins the derivation. A hand-written
// allowlist goes stale silently and records nothing for a newly added flag;
// deriving it from the registry means a new flag is picked up as soon as it is
// defined.
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

	// Spot-check that real flags survive and invented ones do not, so a
	// trivially-true allowlist (everything, or nothing) fails here.
	if !allowed["json"] {
		t.Error("allowlist is missing --json")
	}
	if allowed["definitely-not-a-flag"] {
		t.Error("allowlist admits an undefined flag; it must not be permissive")
	}
}

// TestResolveUsageSkillOnlyAcceptsBundledSkills pins the one dimension
// agento11y can carry that gcx cannot. The bundled set is embedded at build
// time, so a member of it is safe to report; anything else is dropped.
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

// TestLogModeMatchesExportedBody is the test that keeps log mode honest.
//
// Log mode is the user's only way to inspect what would be sent, so it is
// trusted; a preview that drifted from the payload would be worse than no
// preview. The printer lives here and the POST lives in usagestats, so this
// has to straddle both packages — which is exactly why it is easy to let them
// drift without noticing.
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

// captureEmitted runs the emitter with the given mode and returns whatever it
// wrote to stderr. The endpoint is pointed at an unroutable address so an
// enabled mode cannot reach the real receiver even if the test is wrong.
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

// TestResolveModeIsStableAcrossApplyEnv pins the property that lets the
// emitter resolve its mode without moving dotenv.ApplyEnv.
//
// ApplyEnv's call sites are per-branch on purpose: renderLocalBanner reports
// which spelling the user set, and runDoctorCommand snapshots the environment
// before the merge to attribute each value to the shell or config.env. So the
// answer must not depend on whether the merge has already happened in the
// branch that reached the emitter.
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

// TestEmitUsageOnceIsNotWiredYet documents the deliberate state of this PR:
// the emitter exists and is fully tested, but no dispatch path reaches it, so
// merging this collects nothing. The lifecycle wiring lands separately.
func TestEmitUsageOnceIsNotWiredYet(t *testing.T) {
	capture.Reset()
	resetUsageOnceForTest()

	calls := 0
	original := emitUsage
	emitUsage = func(capture.Invocation, time.Time) { calls++ }
	t.Cleanup(func() { emitUsage = original })

	// Called twice on purpose: the once-guard is what will make the wiring
	// safe across the several exit paths that reach it.
	emitUsageOnce()
	emitUsageOnce()

	if calls != 1 {
		t.Errorf("emitUsageOnce() fired %d times, want exactly 1", calls)
	}
}
