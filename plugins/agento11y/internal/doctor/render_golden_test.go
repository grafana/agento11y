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
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

func TestRenderHumanGolden(t *testing.T) {
	tests := []struct {
		name   string
		report *Report
		probed bool
	}{
		{name: "healthy", report: goldenHealthyReport()},
		{name: "minimal", report: goldenMinimalReport()},
		{name: "broken", report: goldenBrokenReport()},
		{name: "probed", report: goldenProbedReport(), probed: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderHuman(&buf, tc.report, false, tc.probed)
			assertGoldenText(t, filepath.Join("testdata", "render", tc.name+".golden.txt"), buf.String())
		})
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
			GuardsEnabled:      true, GuardsTimeoutMs: 1500, GuardsFailOpen: true,
			Tags:             map[string]string{"team": "assistant", "env": "prod"},
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
			LocalHookForward:   HookForwardSection{Reason: local.HookForwardReason(false, false, "", "", "")},
			ContentCaptureMode: "full",
			GuardsTimeoutMs:    1500, GuardsFailOpen: true,
			Health: HealthOK,
		},
		Agents: []AgentStatus{
			{Name: "claude", OnPath: false, Install: InstallStateNotInstalled, Health: HealthWarn},
		},
	}
}

// goldenBrokenReport is the support case: a malformed endpoint, no OTLP
// endpoint, a config that fell back, and an agent whose install probe errored.
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
			ContentCaptureMode: "metadata_only", ContentModeFellBack: true,
			GuardsEnabled: false, GuardsFellBack: true,
			LocalForward: envValue{Set: true, Value: "true", Source: sourceConfig, Key: "AGENTO11Y_LOCAL_FORWARD"},
			// Forwarding is on but guards are off, so nothing is chained.
			LocalHookForward: HookForwardSection{Reason: local.HookForwardReason(true, false, "", "", "")},
			DisallowedKeys:   []string{"AWS_SECRET"},
			Health:           HealthWarn,
			Messages: []string{
				"config.env has keys agento11y ignores: AWS_SECRET",
				"the CONTENT_CAPTURE_MODE value is invalid; using metadata_only",
				"a GUARDS_* value is invalid; falling back to defaults",
			},
		},
		Agents: []AgentStatus{
			// The probe errored: the line says the state is unknown instead of
			// asserting "not configured" and "unknown" at once.
			{Name: "copilot", OnPath: false, Install: InstallStateUnknown, notInstalledLabel: "not configured", Note: "hook-based; stat /home/u/.copilot/hooks/agento11y.json: operation not permitted", Health: HealthWarn},
			{Name: "pi", OnPath: true, Install: InstallStateNotInstalled, Health: HealthWarn},
		},
		AutoUpdateDisabled: true,
	}
}

// goldenProbedReport is a --probe run: every probe line is present and the
// config-only hint is suppressed.
func goldenProbedReport() *Report {
	r := goldenHealthyReport()
	r.Conversations.Probe = &ProbeResult{URL: "https://sigil.example/api/v1/generations:export", StatusCode: 200, OK: true}
	r.Analytics.Probe = &AnalyticsProbe{
		Metrics: &ProbeResult{URL: "https://otlp.example/otlp/v1/metrics", StatusCode: 200, OK: true},
		Traces:  &ProbeResult{URL: "https://otlp.example/otlp/v1/traces", StatusCode: 403, Message: "missing metrics:write/traces:write scope"},
	}
	r.Analytics.Health = HealthError
	r.Analytics.Messages = []string{"OTLP endpoint rejected auth (401/403) — the token is likely missing metrics:write/traces:write scope"}
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
