package doctor

// TestRenderHumanGolden snapshots the whole rendered report for representative
// states, so a change to any line the reader sees shows up as a diff. The
// other render tests assert on fragments, which cannot catch a wrong prefix or
// a misaligned column.
//
// Sections whose collector is pure (conversations, analytics) are built by
// calling it, so the fixtures carry the messages doctor really writes. The
// config section is hand-built because collectConfig reads the process
// environment and the filesystem; its message strings are copied from
// collectConfig.
//
// Golden files live in testdata/render/<scenario>.golden.txt. Set
// UPDATE_GOLDENS=1 to reseed them, matching the harness in
// internal/entry/golden_integration_test.go.

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

func TestRenderHumanGolden(t *testing.T) {
	tests := []struct {
		name   string
		report *Report
	}{
		{name: "healthy", report: goldenHealthyReport()},
		{name: "minimal", report: goldenMinimalReport()},
		{name: "broken", report: goldenBrokenReport()},
		{name: "probed", report: goldenProbedReport()},
		{name: "redirected", report: goldenRedirectedReport()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderHuman(&buf, tc.report, false)
			assertGoldenText(t, filepath.Join("testdata", "render", tc.name+".golden.txt"), buf.String())
			assertOneTrailerPerRow(t, buf.String())
		})
	}
}

// kvRow matches a rendered key/value row and captures the value. The key class
// admits the space in "local guard checks" and the hyphen in "auto-update";
// without the hyphen that row is silently skipped. Section titles, messages and
// the summary are not rows, and their parentheses are part of the prose rather
// than a renderer-added trailer.
var kvRow = regexp.MustCompile(`^ {2}([a-z][a-z -]*):\s+(.*)$`)

// rowLine matches every indented line that is not a section message. Section
// titles, the summary and the probe hint start at column 0. A row this matches
// but kvRow does not is a row the grammar check would skip, which is how a
// hyphen in "auto-update" once excluded that row from the check.
var rowLine = regexp.MustCompile(`^ {2}[^!\s]`)

// assertOneTrailerPerRow enforces the row grammar on a whole report: a row is a
// value plus at most one parenthesized trailer. Two groups mean a fault and a
// provenance note are competing for the same slot, which is the shape this
// renderer is built to avoid.
func assertOneTrailerPerRow(t *testing.T, report string) {
	t.Helper()
	for line := range strings.SplitSeq(report, "\n") {
		m := kvRow.FindStringSubmatch(line)
		if m == nil {
			if rowLine.MatchString(line) {
				t.Errorf("kvRow does not match row %q, so the grammar check skips it", line)
			}
			continue
		}
		if strings.Count(m[2], "(") > 1 {
			t.Errorf("row %q has more than one parenthesized group: %q", m[1], m[2])
		}
		if strings.Contains(m[2], ") (") {
			t.Errorf("row %q has adjacent parenthesized groups: %q", m[1], m[2])
		}
	}
}

// goldenHealthyReport is a fully configured install. It opts into local
// forwarding so the two longest keys ("local forwarding", "local guard
// checks") are rendered and the column is as wide as it gets.
func goldenHealthyReport() *Report {
	osEnv := map[string]string{
		"AGENTO11Y_ENDPOINT":                    "https://sigil.example",
		"AGENTO11Y_AUTH_TENANT_ID":              "12345",
		"AGENTO11Y_AUTH_TOKEN":                  "glc_secret",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example/otlp",
	}
	conversations := collectConversations(osEnv, nil)
	return &Report{
		Binary:        BinarySection{Version: "v0.22.0"},
		Conversations: conversations,
		Analytics:     collectAnalytics(osEnv, nil, conversations.configured()),
		Config: ConfigSection{
			Path: "/home/u/.config/agento11y/config.env", Exists: true,
			ContentCaptureMode: "full",
			ContentModeKey:     "AGENTO11Y_CONTENT_CAPTURE_MODE", ContentModeSource: sourceConfig,
			GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true,
			GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceConfig,
			Tags:             map[string]string{"team": "assistant", "env": "prod"},
			TagsKey:          "AGENTO11Y_TAGS",
			TagsSource:       sourceConfig,
			LocalForward:     envValue{Set: true, Value: "true", Source: sourceEnv, Key: "AGENTO11Y_LOCAL_FORWARD"},
			LocalHookForward: HookForwardSection{Enabled: true},
			Health:           HealthOK,
		},
		Agents: []AgentStatus{
			{Name: "claude", OnPath: true, Install: InstallStateInstalled, Version: "0.3.0", Health: HealthOK},
			{Name: "codex", OnPath: false, Install: InstallStateUnknown, Health: HealthSkipped},
			{Name: "cursor", OnPath: true, HookBased: true, Version: "v0.22.0", Note: "hook-based; configured in Cursor settings", Health: HealthOK},
		},
	}
}

// goldenMinimalReport is a fresh install with nothing configured. It renders
// no tags and no local forwarding, so its widest key is shorter than the
// healthy report's: the two files together pin the column to the widest key
// rendered rather than to a constant.
func goldenMinimalReport() *Report {
	return &Report{
		Binary:        BinarySection{Version: "v0.22.0"},
		Conversations: collectConversations(nil, nil),
		Analytics:     collectAnalytics(nil, nil, false),
		Config: ConfigSection{
			Path: "/home/u/.config/agento11y/config.env",
			// The daemon's answer with LOCAL_FORWARD unset. The line itself is
			// suppressed, which is what this fixture pins.
			LocalHookForward: HookForwardSection{Reason: local.HookForwardReason(false, false, "", "", "")},
			// Nothing is configured, so both shared settings report the built-in
			// value rather than a choice the user made.
			ContentCaptureMode: "metadata_only",
			GuardsTimeoutMs:    1500, GuardsFailOpen: true,
			Health: HealthOK,
		},
		Agents: []AgentStatus{
			{Name: "claude", OnPath: false, Install: InstallStateNotInstalled, Health: HealthWarn},
		},
	}
}

// goldenBrokenReport is the support case: a malformed endpoint, no OTLP
// endpoint, a config that fell back, a local mode value the launcher ignores,
// and agents whose state is neither plain "installed" nor plain "missing".
func goldenBrokenReport() *Report {
	conversations := collectConversations(
		map[string]string{"AGENTO11Y_ENDPOINT": "not a url ://"},
		map[string]string{"AGENTO11Y_AUTH_TENANT_ID": "12345", "SIGIL_AUTH_TOKEN": "glc_secret"},
	)
	return &Report{
		Binary:        BinarySection{Version: "dev"},
		Conversations: conversations,
		Analytics:     collectAnalytics(nil, nil, conversations.configured()),
		Config: ConfigSection{
			Path: "/home/u/.config/agento11y/config.env", Exists: true,
			// The mode came from a value envconfig rejected, so the row credits the
			// built-in default and the message names the variable to fix.
			ContentCaptureMode: "metadata_only", ContentModeFellBack: true,
			// GUARDS_ENABLED is valid and off, under the legacy spelling the row has to
			// name. The timeout is the value that fell back.
			GuardsEnabled: false, GuardsTimeoutMs: 1500, GuardsFailOpen: true, GuardsFellBack: true,
			GuardsKey: "SIGIL_GUARDS_ENABLED", GuardsSource: sourceEnv,
			// The launcher ignores this value, so the row reports the state in force.
			Local:        envValue{Set: true, Value: "enabled", Source: sourceEnv, Key: "AGENTO11Y_LOCAL"},
			LocalInvalid: true,
			LocalForward: envValue{Set: true, Value: "true", Source: sourceConfig, Key: "AGENTO11Y_LOCAL_FORWARD"},
			// Forwarding is on but guards are off, so nothing is chained.
			LocalHookForward: HookForwardSection{Reason: local.HookForwardReason(true, false, "", "", "")},
			DisallowedKeys:   []string{"AWS_SECRET"},
			Health:           HealthWarn,
			Messages: []string{
				"config.env has keys agento11y ignores: AWS_SECRET",
				"the AGENTO11Y_CONTENT_CAPTURE_MODE value is invalid; using metadata_only",
				"the AGENTO11Y_GUARDS_TIMEOUT_MS value is invalid; guards use the default",
				"the AGENTO11Y_LOCAL value is not a boolean; local mode stays off",
			},
		},
		Agents: []AgentStatus{
			// The probe errored: the line says the state is unknown instead of
			// asserting "not configured" and "unknown" at once.
			{Name: "copilot", OnPath: false, Install: InstallStateUnknown, notInstalledLabel: "not configured", Note: "hook-based; stat /home/u/.copilot/hooks/agento11y.json: operation not permitted", Health: HealthWarn},
			{Name: "pi", OnPath: true, Install: InstallStateNotInstalled, Health: HealthWarn},
			// Installed through config with no CLI on PATH: the qualifier and the note
			// share one trailer after the version.
			{Name: "opencode", OnPath: false, Install: InstallStateInstalled, Version: "next", Note: "npm spec pinned", Health: HealthOK},
		},
		AutoUpdate:         envValue{Set: true, Value: "false", Source: sourceConfig, Key: "AGENTO11Y_AUTO_UPDATE"},
		AutoUpdateDisabled: true,
	}
}

// goldenProbedReport is a run whose probes all answered: every probe line is
// present and the config-only hint is suppressed.
func goldenProbedReport() *Report {
	r := goldenHealthyReport()
	// A transport error: nothing answered, so the row has no status and the cause
	// survives only on the message line.
	r.Conversations.Probe = &ProbeResult{URL: "https://sigil.example/api/v1/generations:export", Message: `Post "https://sigil.example/api/v1/generations:export": dial tcp 10.0.0.1:443: connect: connection refused`}
	r.Conversations.Health = HealthError
	r.Conversations.Messages = []string{"could not reach the conversations endpoint: " + describeProbe(r.Conversations.Probe)}
	r.Analytics.Probe = &AnalyticsProbe{
		Metrics: &ProbeResult{URL: "https://otlp.example/otlp/v1/metrics", StatusCode: 200, OK: true},
		Traces:  &ProbeResult{URL: "https://otlp.example/otlp/v1/traces", StatusCode: 403, Message: "the token is likely missing the metrics:write/traces:write scope"},
	}
	r.Analytics.Health = HealthError
	r.Analytics.Messages = otlpAuthMessages(r.Analytics.Probe)
	return r
}

// goldenRedirectedReport is the reported support case: the endpoint answers the
// export POST with a redirect to a login page, so conversations are broken even
// though the endpoint resolves and responds.
func goldenRedirectedReport() *Report {
	r := goldenHealthyReport()
	r.Conversations.Probe = &ProbeResult{
		URL:        "https://sigil.example/api/v1/generations:export",
		StatusCode: 302,
		Message:    "redirected to /login",
	}
	r.Conversations.Health = HealthError
	r.Conversations.Messages = []string{
		"the conversations endpoint redirected the export request (" + describeProbe(r.Conversations.Probe) +
			"), so AGENTO11Y_ENDPOINT is not an Agent Observability API URL. " + apiURLHint,
	}
	r.Analytics.Probe = &AnalyticsProbe{
		Metrics: &ProbeResult{URL: "https://otlp.example/otlp/v1/metrics", StatusCode: 200, OK: true},
		Traces:  &ProbeResult{URL: "https://otlp.example/otlp/v1/traces", StatusCode: 200, OK: true},
	}
	return r
}

// assertGoldenText compares got against the file at path, or rewrites the file
// when UPDATE_GOLDENS=1.
func assertGoldenText(t *testing.T, path, got string) {
	t.Helper()
	if os.Getenv("UPDATE_GOLDENS") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatalf("write golden %s: %v", path, err)
		}
		t.Logf("UPDATE_GOLDENS=1: wrote %s", path)
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with UPDATE_GOLDENS=1 to seed): %v", path, err)
	}
	if string(want) != got {
		t.Fatalf("golden mismatch for %s\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}
