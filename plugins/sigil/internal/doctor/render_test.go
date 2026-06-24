package doctor

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func sampleReport() *Report {
	return &Report{
		Sigil: SigilSection{Version: "1.2.3"},
		Config: ConfigSection{
			Path: "/tmp/sigil/config.env", Exists: true,
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
			{Name: "claude", OnPath: true, Installed: true, Version: "0.3.0", Health: HealthOK},
			{Name: "cursor", OnPath: true, HookBased: true, Version: "1.2.3", Note: "hook-based", Health: HealthOK},
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

func TestRenderHuman_ProbeHint(t *testing.T) {
	const hint = "sigil doctor --probe"
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
		{name: "hook-based", agent: AgentStatus{HookBased: true, OnPath: true, Version: "1.2.3", Note: "hook-based", Health: HealthOK}, want: "detected v1.2.3 (hook-based)"},
		{name: "skipped not on path", agent: AgentStatus{OnPath: false, Health: HealthSkipped}, want: "not found on PATH"},
		{name: "installed on path", agent: AgentStatus{OnPath: true, Installed: true, Version: "0.3.0", Health: HealthOK}, want: "installed v0.3.0"},
		{name: "config-based installed without cli", agent: AgentStatus{OnPath: false, Installed: true, Version: "2.0.0", Health: HealthOK}, want: "installed (CLI not on PATH) v2.0.0"},
		{name: "on path not installed", agent: AgentStatus{OnPath: true, Installed: false, Health: HealthWarn}, want: "on PATH, plugin not installed"},
		{name: "config-based not installed without cli", agent: AgentStatus{OnPath: false, Installed: false, Health: HealthWarn}, want: "plugin not installed"},
		{name: "hook-file based installed (copilot)", agent: AgentStatus{OnPath: false, Installed: true, notInstalledLabel: "not configured", Note: "hook-based", Health: HealthOK}, want: "installed (hook-based)"},
		{name: "hook-file based not configured ignores PATH (copilot)", agent: AgentStatus{OnPath: true, Installed: false, notInstalledLabel: "not configured", Note: "hook-based", Health: HealthWarn}, want: "not configured (hook-based)"},
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
