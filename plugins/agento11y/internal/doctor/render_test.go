package doctor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() *Report {
	return &Report{
		Binary: BinarySection{Version: "v1.2.3"},
		Config: ConfigSection{
			Path: "/tmp/agento11y/config.env", Exists: true,
			ContentCaptureMode: "metadata_only", Health: HealthOK,
		},
		Conversations: ConversationsSection{
			Endpoint: envValue{Set: true, Value: "https://sigil.example", Source: sourceEnv},
			TenantID: envValue{Set: true, Value: "12345", Source: sourceEnv},
			Token:    tokenValue{Set: true, Prefix: "glc_", Source: sourceEnv},
			Health:   HealthOK,
		},
		Analytics: AnalyticsSection{
			Health:   HealthError,
			Messages: []string{"no OTLP endpoint set"},
		},
		Agents: []AgentStatus{
			{Name: "claude", OnPath: true, Install: InstallStateInstalled, Version: "0.3.0", Health: HealthOK},
			{Name: "cursor", OnPath: true, HookBased: true, Version: "v1.2.3", Note: "hook-based", Health: HealthOK},
		},
	}
}

func TestRenderJSON_ValidAndNoToken(t *testing.T) {
	r := sampleReport()
	var buf bytes.Buffer
	if err := renderJSON(&buf, r); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	// The token value never appears in the JSON contract — only presence and
	// the non-secret prefix.
	if !strings.Contains(buf.String(), `"prefix": "glc_"`) {
		t.Fatalf("expected redacted token prefix in output:\n%s", buf.String())
	}
}

func TestRenderHuman_NoColorIsPlain(t *testing.T) {
	r := sampleReport()
	var buf bytes.Buffer
	renderHuman(&buf, r, false, false)
	out := buf.String()

	if strings.Contains(out, "\x1b[") {
		t.Fatalf("--no-color output contains ANSI escapes:\n%q", out)
	}
	for _, want := range []string{
		"Conversations (generation export)",
		"Analytics (OTLP metrics & traces)",
		"https://sigil.example (env)",
		"set (glc_…, env)",
		"1 problem(s)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("human output missing %q:\n%s", want, out)
		}
	}
}

// The binary version is printed exactly as the build stamped it, in the
// heading and in cursor's line, which reports the same string. The `v` prefix
// lives in the release ldflags (-X main.version=v{{ .Version }}) and nowhere
// else, so a build that stamps it and one that doesn't both render one `v`.
func TestRenderHuman_BinaryVersionPrintedAsStamped(t *testing.T) {
	for _, version := range []string{"v0.22.0", "0.22.0", "dev"} {
		t.Run(version, func(t *testing.T) {
			r := sampleReport()
			r.Binary.Version = version
			r.Agents = []AgentStatus{{Name: "cursor", OnPath: true, HookBased: true, Version: version, Health: HealthOK}}
			var buf bytes.Buffer
			renderHuman(&buf, r, false, false)
			out := buf.String()
			heading, _, _ := strings.Cut(out, "\n")
			if want := "agento11y doctor " + version; heading != want {
				t.Fatalf("heading = %q, want %q", heading, want)
			}
			if want := "detected " + version + "\n"; !strings.Contains(out, want) {
				t.Fatalf("cursor line missing %q:\n%s", want, out)
			}
		})
	}
}

// Every value starts in the same column, whatever the longest key is.
func TestReportBody_PadsToWidestKey(t *testing.T) {
	var b reportBody
	b.kv("tags", "my_label=my_value")
	b.kv("local forwarding", "true")
	b.kv("local guard checks", "not forwarded")

	want := strings.Join([]string{
		"  tags:               my_label=my_value",
		"  local forwarding:   true",
		"  local guard checks: not forwarded",
		"",
	}, "\n")
	if got := b.flush(palette{}); got != want {
		t.Fatalf("flush =\n%s\nwant:\n%s", got, want)
	}
}

// A key/value line renders whatever its key is. The line kind, not the key's
// content, decides how it is written, so an agent whose name resolves to an
// empty string still gets a row.
func TestReportBody_EmptyKeyStillRenders(t *testing.T) {
	var b reportBody
	b.kv("", "orphan")
	b.kv("k", "v")

	want := "  :  orphan\n  k: v\n"
	if got := b.flush(palette{}); got != want {
		t.Fatalf("flush = %q, want %q", got, want)
	}
}

func TestRenderHuman_ProbeHint(t *testing.T) {
	const hint = "agento11y doctor --probe"
	nothingProbeable := func() *Report {
		r := sampleReport()
		r.Conversations = ConversationsSection{Health: HealthWarn}
		r.Analytics = AnalyticsSection{Health: HealthWarn}
		return r
	}
	tests := []struct {
		name     string
		report   func() *Report
		probed   bool
		wantHint bool
	}{
		{name: "shown when configured and not probed", report: sampleReport, probed: false, wantHint: true},
		{name: "hidden when probed", report: sampleReport, probed: true, wantHint: false},
		{name: "hidden when nothing is probeable", report: nothingProbeable, probed: false, wantHint: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			renderHuman(&buf, tc.report(), false, tc.probed)
			if got := strings.Contains(buf.String(), hint); got != tc.wantHint {
				t.Fatalf("probe hint present = %v, want %v:\n%s", got, tc.wantHint, buf.String())
			}
		})
	}
}

func TestDescribeAgent(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name  string
		agent AgentStatus
		want  string
	}{
		// cursor reports the agento11y binary version, which the release ldflags
		// stamp with the `v` and a dev build reports as a bare word. It is
		// printed as stamped, the way the heading prints it.
		{name: "hook-based", agent: AgentStatus{HookBased: true, OnPath: true, Version: "v1.2.3", Note: "hook-based", Health: HealthOK}, want: "detected v1.2.3 (hook-based)"},
		{name: "hook-based unstamped version", agent: AgentStatus{HookBased: true, OnPath: true, Version: "0.22.0", Health: HealthOK}, want: "detected 0.22.0"},
		{name: "hook-based dev version", agent: AgentStatus{HookBased: true, OnPath: true, Version: "dev", Health: HealthOK}, want: "detected dev"},
		// A host agent's own version is numeric (claude) or a dist-tag
		// (opencode, pi, from an npm spec); only the number takes the prefix.
		{name: "agent dist-tag version", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "next", Health: HealthOK}, want: "installed next"},
		{name: "skipped not on path", agent: AgentStatus{OnPath: false, Install: InstallStateUnknown, Health: HealthSkipped}, want: "not found on PATH"},
		{name: "installed on path", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "0.3.0", Health: HealthOK}, want: "installed v0.3.0"},
		{name: "config-based installed without cli", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, Version: "2.0.0", Health: HealthOK}, want: "installed (CLI not on PATH) v2.0.0"},
		{name: "on path not installed", agent: AgentStatus{OnPath: true, Install: InstallStateNotInstalled, Health: HealthWarn}, want: "on PATH, plugin not installed"},
		{name: "config-based not installed without cli", agent: AgentStatus{OnPath: false, Install: InstallStateNotInstalled, Health: HealthWarn}, want: "plugin not installed"},
		{name: "hook-file based installed (copilot)", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, notInstalledLabel: "not configured", Note: "hook-based", Health: HealthOK}, want: "installed (hook-based)"},
		{name: "hook-file based not configured ignores PATH (copilot)", agent: AgentStatus{OnPath: true, Install: InstallStateNotInstalled, notInstalledLabel: "not configured", Note: "hook-based", Health: HealthWarn}, want: "not configured (hook-based)"},
		// A probe that errored: one line must not assert a known state and an
		// unknown one at the same time.
		{name: "probe errored", agent: AgentStatus{OnPath: true, Install: InstallStateUnknown, Note: "stat hooks.json: operation not permitted", Health: HealthWarn}, want: "install state unknown (stat hooks.json: operation not permitted)"},
		{name: "hook-file based probe errored (copilot)", agent: AgentStatus{OnPath: false, Install: InstallStateUnknown, notInstalledLabel: "not configured", Note: "hook-based; stat hooks.json: operation not permitted", Health: HealthWarn}, want: "install state unknown (hook-based; stat hooks.json: operation not permitted)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeAgent(p, tc.agent); got != tc.want {
				t.Fatalf("describeAgent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatTags(t *testing.T) {
	tests := []struct {
		name string
		tags map[string]string
		want string
	}{
		// Keys are sorted so the rendered line is stable regardless of map order.
		{name: "sorted by key", tags: map[string]string{"team": "assistant", "env": "prod", "az": "1"}, want: "az=1, env=prod, team=assistant"},
		{name: "single", tags: map[string]string{"team": "assistant"}, want: "team=assistant"},
		{name: "empty", tags: map[string]string{}, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTags(tc.tags); got != tc.want {
				t.Fatalf("formatTags = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderHuman_TagsLine(t *testing.T) {
	tests := []struct {
		name     string
		tags     map[string]string
		source   string
		wantLine string // "" = expect no tags line at all
	}{
		{name: "tags shown with source", tags: map[string]string{"team": "assistant", "env": "prod"}, source: sourceConfig, wantLine: "env=prod, team=assistant (config.env)"},
		{name: "no tags omits line", tags: nil, wantLine: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReport()
			r.Config.Tags = tc.tags
			r.Config.TagsSource = tc.source
			var buf bytes.Buffer
			renderHuman(&buf, r, false, false)
			out := buf.String()
			if tc.wantLine == "" {
				if strings.Contains(out, "tags:") {
					t.Fatalf("expected no tags line:\n%s", out)
				}
				return
			}
			if !strings.Contains(out, "tags:") || !strings.Contains(out, tc.wantLine) {
				t.Fatalf("expected tags line %q:\n%s", tc.wantLine, out)
			}
		})
	}
}

func TestDescribeGuards(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name   string
		config ConfigSection
		want   string
	}{
		{name: "disabled", config: ConfigSection{GuardsEnabled: false}, want: "disabled"},
		{name: "enabled fail-open", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true}, want: "enabled (timeout 1500ms, fail-open)"},
		{name: "enabled fail-closed", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 500, GuardsFailOpen: false}, want: "enabled (timeout 500ms, fail-closed)"},
		{name: "disabled after fallback", config: ConfigSection{GuardsEnabled: false, GuardsFellBack: true}, want: "disabled (invalid value, fell back)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeGuards(p, tc.config); got != tc.want {
				t.Fatalf("describeGuards = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderHuman_GuardsLine(t *testing.T) {
	tests := []struct {
		name     string
		config   ConfigSection
		wantLine string
	}{
		{name: "disabled", config: ConfigSection{GuardsEnabled: false}, wantLine: "disabled"},
		{name: "enabled fail-open", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true}, wantLine: "enabled (timeout 1500ms, fail-open)"},
		{name: "enabled fail-closed", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 500, GuardsFailOpen: false}, wantLine: "enabled (timeout 500ms, fail-closed)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReport()
			r.Config.GuardsEnabled = tc.config.GuardsEnabled
			r.Config.GuardsTimeoutMs = tc.config.GuardsTimeoutMs
			r.Config.GuardsFailOpen = tc.config.GuardsFailOpen
			var buf bytes.Buffer
			renderHuman(&buf, r, false, false)
			out := buf.String()
			if !strings.Contains(out, "guards:") || !strings.Contains(out, tc.wantLine) {
				t.Fatalf("expected guards line %q:\n%s", tc.wantLine, out)
			}
		})
	}
}

func TestDescribeToken(t *testing.T) {
	tests := []struct {
		name  string
		token tokenValue
		want  string
	}{
		{name: "unset", token: tokenValue{}, want: "not set"},
		{name: "with prefix", token: tokenValue{Set: true, Prefix: "glc_", Source: sourceEnv}, want: "set (glc_…, env)"},
		{name: "no prefix", token: tokenValue{Set: true, Source: sourceConfig}, want: "set (config.env)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeToken(tc.token); got != tc.want {
				t.Fatalf("describeToken = %q, want %q", got, tc.want)
			}
		})
	}
}
