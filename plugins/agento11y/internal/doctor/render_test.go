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
			Endpoint: envValue{Set: true, Value: "https://sigil.example", Source: sourceEnv, Key: "AGENTO11Y_ENDPOINT"},
			TenantID: envValue{Set: true, Value: "12345", Source: sourceEnv, Key: "AGENTO11Y_AUTH_TENANT_ID"},
			Token:    tokenValue{Set: true, Prefix: "glc_", Source: sourceEnv, Key: "AGENTO11Y_AUTH_TOKEN"},
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
	renderHuman(&buf, r, false)
	out := buf.String()

	if strings.Contains(out, "\x1b[") {
		t.Fatalf("--no-color output contains ANSI escapes:\n%q", out)
	}
	for _, want := range []string{
		"Conversations (generation export)",
		"Analytics (OTLP metrics & traces)",
		"https://sigil.example (AGENTO11Y_ENDPOINT, env)",
		"set (glc_…, AGENTO11Y_AUTH_TOKEN, env)",
		// No variable is configured, so the row says the value is the built-in one.
		"content capture:  metadata_only (default)",
		"guards:           disabled (default)",
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
			renderHuman(&buf, r, false)
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

func TestDescribeProbeRow(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name   string
		result *ProbeResult
		want   string
	}{
		{name: "nil is skipped", result: nil, want: "skipped"},
		{name: "ok", result: &ProbeResult{URL: "https://otlp.example/otlp/v1/metrics", StatusCode: 200, OK: true}, want: "HTTP 200 (https://otlp.example/otlp/v1/metrics)"},
		// The diagnosis belongs on the section message line, so the row carries
		// the URL alone.
		{name: "auth failure drops the diagnosis", result: &ProbeResult{URL: "https://otlp.example/otlp/v1/traces", StatusCode: 403, Message: "missing metrics:write/traces:write scope"}, want: "HTTP 403 (https://otlp.example/otlp/v1/traces)"},
		{name: "transport error has no status", result: &ProbeResult{URL: "https://sigil.example/api", Message: "connection refused"}, want: "no response (https://sigil.example/api)"},
		// A probe that failed before it built a request has no URL to report.
		{name: "no url", result: &ProbeResult{Message: "invalid endpoint: parse …"}, want: "no response"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeProbeRow(p, tc.result); got != tc.want {
				t.Fatalf("describeProbeRow = %q, want %q", got, tc.want)
			}
		})
	}
}

// describeProbe is the message-text formatter: a section message has no URL
// column, so the diagnosis is the point there.
func TestDescribeProbe(t *testing.T) {
	tests := []struct {
		name   string
		result *ProbeResult
		want   string
	}{
		{name: "nil is skipped", result: nil, want: "skipped"},
		{name: "status only", result: &ProbeResult{StatusCode: 503}, want: "HTTP 503"},
		{name: "status and message", result: &ProbeResult{StatusCode: 503, Message: "upstream down"}, want: "HTTP 503: upstream down"},
		{name: "transport error", result: &ProbeResult{Message: "connection refused"}, want: "no response: connection refused"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeProbe(tc.result); got != tc.want {
				t.Fatalf("describeProbe = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRenderHuman_ProbeDiagnosisAppearsOnce pins the split between the row and
// the section message: the row names the URL that answered, and the diagnosis is
// printed once, on the `!` line runProbes wrote it to.
func TestRenderHuman_ProbeDiagnosisAppearsOnce(t *testing.T) {
	const diagnosis = "missing metrics:write/traces:write scope"
	r := sampleReport()
	r.Analytics = AnalyticsSection{
		Endpoint: envValue{Set: true, Value: "https://otlp.example/otlp", Source: sourceEnv, Key: "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT"},
		Health:   HealthError,
		Probe: &AnalyticsProbe{
			Traces: &ProbeResult{URL: "https://otlp.example/otlp/v1/traces", StatusCode: 403, Message: diagnosis},
		},
		Messages: []string{"OTLP endpoint rejected auth (401/403) — the token is likely " + diagnosis},
	}
	var buf bytes.Buffer
	renderHuman(&buf, r, false)
	out := buf.String()

	if want := "traces probe:     HTTP 403 (https://otlp.example/otlp/v1/traces)"; !strings.Contains(out, want) {
		t.Fatalf("probe row missing %q:\n%s", want, out)
	}
	if got := strings.Count(out, diagnosis); got != 1 {
		t.Fatalf("diagnosis appears %d times, want 1:\n%s", got, out)
	}
}

// TestRenderHuman_FaultsStayOnTheMessageLine covers the three inline fault
// suffixes the renderer used to append. Each fault already has a section
// message, so repeating it on the row would print it twice and open a second
// parenthesized group.
func TestRenderHuman_FaultsStayOnTheMessageLine(t *testing.T) {
	tests := []struct {
		name      string
		config    func(*ConfigSection)
		message   string
		wantRow   string
		wantNoRow string
	}{
		{
			// The rejected value supplied nothing, so the row reports the built-in mode
			// and names no variable for it.
			name: "content capture fell back",
			config: func(c *ConfigSection) {
				c.ContentCaptureMode = "metadata_only"
				c.ContentModeFellBack = true
			},
			message:   "the AGENTO11Y_CONTENT_CAPTURE_MODE value is invalid; using metadata_only",
			wantRow:   "content capture:  metadata_only (default)",
			wantNoRow: "invalid value, fell back",
		},
		{
			// GUARDS_ENABLED decided the state and is valid, so the row keeps naming
			// it; the broken timeout is named by the message alone.
			name: "guards timeout invalid",
			config: func(c *ConfigSection) {
				c.GuardsEnabled, c.GuardsTimeoutMs, c.GuardsFailOpen = true, 1500, true
				c.GuardsKey, c.GuardsSource = "AGENTO11Y_GUARDS_ENABLED", sourceEnv
				c.GuardsFellBack = true
			},
			message:   "the AGENTO11Y_GUARDS_TIMEOUT_MS value is invalid; guards use the default",
			wantRow:   "guards:           enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)",
			wantNoRow: "invalid value, fell back",
		},
		{
			// The launcher ignores the value, so the row reports the state in force and
			// carries the rejected value in its one trailer.
			name: "local mode value is not a boolean",
			config: func(c *ConfigSection) {
				c.Local = envValue{Set: true, Value: "enabled", Source: sourceEnv, Key: "AGENTO11Y_LOCAL"}
				c.LocalInvalid = true
			},
			message:   "the AGENTO11Y_LOCAL value is not a boolean; local mode stays off",
			wantRow:   `local mode:       off (AGENTO11Y_LOCAL="enabled" is not a boolean, env)`,
			wantNoRow: "invalid value, local mode is off",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReport()
			r.Config.Health = HealthWarn
			r.Config.Messages = []string{tc.message}
			tc.config(&r.Config)
			var buf bytes.Buffer
			renderHuman(&buf, r, false)
			out := buf.String()

			if !strings.Contains(out, tc.wantRow) {
				t.Fatalf("row missing %q:\n%s", tc.wantRow, out)
			}
			if strings.Contains(out, tc.wantNoRow) {
				t.Fatalf("row still carries the inline fault %q:\n%s", tc.wantNoRow, out)
			}
			if got := strings.Count(out, tc.message); got != 1 {
				t.Fatalf("fault appears %d times, want 1:\n%s", got, out)
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
		// A host agent plugin's own version is a dotted number or a dist-tag
		// (opencode and pi can report the tail of an npm spec); only the number
		// takes the prefix.
		{name: "agent dist-tag version", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "next", Health: HealthOK}, want: "installed next"},
		// A commit sha is labelled so it doesn't read as a version, and one that
		// starts with a digit must not pick up the `v` prefix. A sha too short to
		// recognise is printed bare rather than as `v1a2b3c`.
		{name: "agent commit sha version", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "d9db82cfb562", Health: HealthOK}, want: "installed (commit d9db82c)"},
		{name: "agent digit-leading commit sha version", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "1a2b3c4d5e6f", Health: HealthOK}, want: "installed (commit 1a2b3c4)"},
		{name: "agent short digit-leading sha", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "1a2b3c", Health: HealthOK}, want: "installed 1a2b3c"},
		{name: "agent sha already at the cut length", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "d9db82c", Health: HealthOK}, want: "installed (commit d9db82c)"},
		{name: "agent full commit sha version", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "d9db82cfb562487b174006aabbd435d155e59278", Health: HealthOK}, want: "installed (commit d9db82c)"},
		// The commit label is a trailer part, so a sha, the missing-CLI qualifier
		// and a note share one group instead of opening three.
		{name: "agent commit sha without cli", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, Version: "d9db82cfb562", Health: HealthOK}, want: "installed (commit d9db82c, CLI not on PATH)"},
		{name: "agent commit sha without cli and a note", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, Version: "d9db82cfb562", Note: "plugin store", Health: HealthOK}, want: "installed (commit d9db82c, CLI not on PATH, plugin store)"},
		{name: "hook-file based commit sha with a note", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "d9db82cfb562", notInstalledLabel: "not configured", Note: "hook-based", Health: HealthOK}, want: "installed (commit d9db82c, hook-based)"},
		// A version must never contradict the install state: a probe that reports
		// a version alongside `not installed` prints the state alone.
		{name: "version suppressed when not installed", agent: AgentStatus{OnPath: true, Install: InstallStateNotInstalled, Version: "1.2.3", Health: HealthWarn}, want: "on PATH, plugin not installed"},
		{name: "version suppressed when not installed off path", agent: AgentStatus{OnPath: false, Install: InstallStateNotInstalled, Version: "1.2.3", Health: HealthWarn}, want: "plugin not installed"},
		{name: "hook-file based version suppressed when not configured", agent: AgentStatus{OnPath: false, Install: InstallStateNotInstalled, Version: "1.2.3", notInstalledLabel: "not configured", Health: HealthWarn}, want: "not configured"},
		{name: "skipped not on path", agent: AgentStatus{OnPath: false, Install: InstallStateUnknown, Health: HealthSkipped}, want: "not found on PATH"},
		{name: "installed on path", agent: AgentStatus{OnPath: true, Install: InstallStateInstalled, Version: "0.3.0", Health: HealthOK}, want: "installed v0.3.0"},
		// The qualifier follows the version, so no renderer-added parentheses sit
		// inside the value.
		{name: "config-based installed without cli", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, Version: "2.0.0", Health: HealthOK}, want: "installed v2.0.0 (CLI not on PATH)"},
		// A qualifier and a note share one trailer rather than opening two groups.
		{name: "config-based installed without cli and a note", agent: AgentStatus{OnPath: false, Install: InstallStateInstalled, Version: "2.0.0", Note: "npm spec pinned", Health: HealthOK}, want: "installed v2.0.0 (CLI not on PATH, npm spec pinned)"},
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

func TestIsCommitSHA(t *testing.T) {
	tests := []struct {
		name    string
		version string
		want    bool
	}{
		{name: "short claude sha", version: "d9db82cfb562", want: true},
		{name: "full sha", version: "d9db82cfb562487b174006aabbd435d155e59278", want: true},
		{name: "digit-leading sha", version: "1a2b3c4d5e6f", want: true},
		{name: "all digits", version: "1234567", want: true},
		{name: "seven chars is the shortest accepted", version: "abcdef1", want: true},
		{name: "uppercase hex", version: "D9DB82C", want: true},
		{name: "six chars is too short", version: "abcdef", want: false},
		{name: "empty", version: "", want: false},
		// A dotted string is a version even when it is long enough and every other
		// byte is hex.
		{name: "dotted hex-only version", version: "0.19.10", want: false},
		{name: "dist tag", version: "next", want: false},
		{name: "non-hex letters", version: "deadbeefzz", want: false},
		{name: "prerelease", version: "1.2.3-rc1", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCommitSHA(tc.version); got != tc.want {
				t.Fatalf("isCommitSHA(%q) = %v, want %v", tc.version, got, tc.want)
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
		key      string
		source   string
		wantLine string // "" = expect no tags line at all
	}{
		{name: "tags shown with provenance", tags: map[string]string{"team": "assistant", "env": "prod"}, key: "AGENTO11Y_TAGS", source: sourceConfig, wantLine: "env=prod, team=assistant (AGENTO11Y_TAGS, config.env)"},
		{name: "legacy spelling", tags: map[string]string{"env": "prod"}, key: "SIGIL_TAGS", source: sourceEnv, wantLine: "env=prod (SIGIL_TAGS, env)"},
		{name: "no tags omits line", tags: nil, wantLine: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReport()
			r.Config.Tags = tc.tags
			r.Config.TagsKey = tc.key
			r.Config.TagsSource = tc.source
			var buf bytes.Buffer
			renderHuman(&buf, r, false)
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

func TestDescribeRedactInput(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name   string
		config ConfigSection
		want   string
	}{
		{name: "enabled", config: ConfigSection{RedactInput: true}, want: "enabled"},
		{name: "disabled", config: ConfigSection{RedactInput: false}, want: "disabled (prompts export unredacted)"},
		{name: "enabled after fallback", config: ConfigSection{RedactInput: true, RedactInputFellBack: true}, want: "enabled (invalid value, fell back)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeRedactInput(p, tc.config); got != tc.want {
				t.Fatalf("describeRedactInput = %q, want %q", got, tc.want)
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
		// No GUARDS_* variable is set, so the row says the value is the built-in one.
		{name: "disabled by default", config: ConfigSection{GuardsEnabled: false}, want: "disabled (default)"},
		{name: "disabled explicitly", config: ConfigSection{GuardsEnabled: false, GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceConfig}, want: "disabled (AGENTO11Y_GUARDS_ENABLED, config.env)"},
		{name: "enabled fail-open", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true, GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceEnv}, want: "enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)"},
		{name: "enabled fail-closed", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 500, GuardsFailOpen: false, GuardsKey: "SIGIL_GUARDS_ENABLED", GuardsSource: sourceEnv}, want: "enabled, timeout 500ms, fail-closed (SIGIL_GUARDS_ENABLED, env)"},
		// A rejected GUARDS_TIMEOUT_MS leaves GUARDS_ENABLED as the key the row
		// names. The fallback itself is reported once, on the section message line,
		// so it adds no second group to the row.
		{name: "timeout fell back", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true, GuardsFellBack: true, GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceEnv}, want: "enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)"},
		// A rejected GUARDS_ENABLED value decided nothing, so the row reports the
		// built-in default and names no variable.
		{name: "enabled value rejected", config: ConfigSection{GuardsEnabled: false, GuardsFellBack: true}, want: "disabled (default)"},
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
		{name: "disabled", config: ConfigSection{GuardsEnabled: false}, wantLine: "disabled (default)"},
		{name: "enabled fail-open", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 1500, GuardsFailOpen: true, GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceEnv}, wantLine: "enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)"},
		{name: "enabled fail-closed", config: ConfigSection{GuardsEnabled: true, GuardsTimeoutMs: 500, GuardsFailOpen: false, GuardsKey: "AGENTO11Y_GUARDS_ENABLED", GuardsSource: sourceEnv}, wantLine: "enabled, timeout 500ms, fail-closed (AGENTO11Y_GUARDS_ENABLED, env)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sampleReport()
			r.Config.GuardsEnabled = tc.config.GuardsEnabled
			r.Config.GuardsTimeoutMs = tc.config.GuardsTimeoutMs
			r.Config.GuardsFailOpen = tc.config.GuardsFailOpen
			r.Config.GuardsKey = tc.config.GuardsKey
			r.Config.GuardsSource = tc.config.GuardsSource
			var buf bytes.Buffer
			renderHuman(&buf, r, false)
			out := buf.String()
			if !strings.Contains(out, "guards:") || !strings.Contains(out, tc.wantLine) {
				t.Fatalf("expected guards line %q:\n%s", tc.wantLine, out)
			}
		})
	}
}

func TestDescribeToken(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name  string
		token tokenValue
		want  string
	}{
		{name: "unset", token: tokenValue{}, want: "not set"},
		{name: "with prefix", token: tokenValue{Set: true, Prefix: "glc_", Source: sourceEnv, Key: "AGENTO11Y_AUTH_TOKEN"}, want: "set (glc_…, AGENTO11Y_AUTH_TOKEN, env)"},
		{name: "legacy spelling", token: tokenValue{Set: true, Prefix: "glc_", Source: sourceConfig, Key: "SIGIL_AUTH_TOKEN"}, want: "set (glc_…, SIGIL_AUTH_TOKEN, config.env)"},
		// A hand-built value carries no key, so the source stands alone rather
		// than leaving an empty slot in the trailer.
		{name: "no key", token: tokenValue{Set: true, Source: sourceConfig}, want: "set (config.env)"},
		{name: "no prefix", token: tokenValue{Set: true, Source: sourceConfig, Key: "AGENTO11Y_AUTH_TOKEN"}, want: "set (AGENTO11Y_AUTH_TOKEN, config.env)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeToken(p, tc.token); got != tc.want {
				t.Fatalf("describeToken = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribeSource(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{name: "no parts", parts: nil, want: ""},
		{name: "all parts empty", parts: []string{"", ""}, want: ""},
		{name: "detail only", parts: []string{"present"}, want: "(present)"},
		{name: "key and source", parts: []string{"AGENTO11Y_ENDPOINT", "env"}, want: "(AGENTO11Y_ENDPOINT, env)"},
		{name: "detail then provenance", parts: []string{"glc_…", "SIGIL_AUTH_TOKEN", "config.env"}, want: "(glc_…, SIGIL_AUTH_TOKEN, config.env)"},
		// A missing middle part is dropped rather than rendered as an empty slot.
		{name: "empty part dropped", parts: []string{"", "config.env"}, want: "(config.env)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeSource(p, tc.parts...); got != tc.want {
				t.Fatalf("describeSource = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDescribeLocal covers the one row whose value is not the resolved string:
// the launcher acts on the boolean whitelist, so a value outside it leaves local
// mode off and the row has to say so.
func TestDescribeLocal(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name    string
		value   envValue
		invalid bool
		want    string
	}{
		{name: "valid value", value: envValue{Set: true, Value: "true", Source: sourceEnv, Key: "AGENTO11Y_LOCAL"}, want: "true (AGENTO11Y_LOCAL, env)"},
		{name: "rejected value", value: envValue{Set: true, Value: "enabled", Source: sourceEnv, Key: "AGENTO11Y_LOCAL"}, invalid: true, want: `off (AGENTO11Y_LOCAL="enabled" is not a boolean, env)`},
		// A hand-built value carries no key, so the trailer quotes the value alone.
		{name: "rejected value without a key", value: envValue{Set: true, Value: "enabled", Source: sourceConfig}, invalid: true, want: `off ("enabled" is not a boolean, config.env)`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeLocal(p, tc.value, tc.invalid); got != tc.want {
				t.Fatalf("describeLocal = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDescribeEnv(t *testing.T) {
	p := palette{color: false}
	tests := []struct {
		name  string
		value envValue
		want  string
	}{
		// No variable supplied the value, so there is no key or source to name.
		{name: "unset has no trailer", value: envValue{}, want: "not set"},
		{name: "key and source", value: envValue{Set: true, Value: "https://sigil.example", Source: sourceEnv, Key: "AGENTO11Y_ENDPOINT"}, want: "https://sigil.example (AGENTO11Y_ENDPOINT, env)"},
		{name: "legacy spelling", value: envValue{Set: true, Value: "https://sigil.example", Source: sourceConfig, Key: "SIGIL_ENDPOINT"}, want: "https://sigil.example (SIGIL_ENDPOINT, config.env)"},
		{name: "no key", value: envValue{Set: true, Value: "true", Source: sourceEnv}, want: "true (env)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := describeEnv(p, tc.value); got != tc.want {
				t.Fatalf("describeEnv = %q, want %q", got, tc.want)
			}
		})
	}
}
