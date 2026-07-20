package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isolateEnv points the dotenv/state roots at a fresh tempdir and clears the
// tracked env vars so a test never reads the developer's real config.
func isolateEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	for _, k := range trackedKeys {
		t.Setenv(k, "")
	}
	return dir
}

// stubSeams replaces the network/agent seams with cheap fakes so Collect
// tests stay hermetic.
func stubSeams(t *testing.T) {
	t.Helper()
	prevAgents, prevConv, prevOTLP := collectAgents, probeConversationsFn, probeOTLPFn
	t.Cleanup(func() {
		collectAgents, probeConversationsFn, probeOTLPFn = prevAgents, prevConv, prevOTLP
	})
	collectAgents = func(context.Context, string) []AgentStatus { return nil }
	probeConversationsFn = func(context.Context, string, string, string, bool) *ProbeResult {
		return &ProbeResult{StatusCode: 200, OK: true}
	}
	probeOTLPFn = func(context.Context) *AnalyticsProbe { return nil }
}

func writeConfig(t *testing.T, content string) {
	t.Helper()
	writeConfigApp(t, "agento11y", content)
}

func writeConfigApp(t *testing.T, app, content string) {
	t.Helper()
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), app, "config.env")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		wantJSON  bool
		wantProbe bool
		wantColor bool // NoColor
		wantErr   bool
	}{
		{name: "no flags"},
		{name: "json", args: []string{"--json"}, wantJSON: true},
		{name: "probe", args: []string{"--probe"}, wantProbe: true},
		{name: "online alias", args: []string{"--online"}, wantProbe: true},
		{name: "no-color", args: []string{"--no-color"}, wantColor: true},
		{name: "combined", args: []string{"--json", "--probe", "--no-color"}, wantJSON: true, wantProbe: true, wantColor: true},
		{name: "unknown flag", args: []string{"--nope"}, wantErr: true},
		{name: "positional arg", args: []string{"extra"}, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseFlags(tc.args, &bytes.Buffer{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %v", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if opts.JSON != tc.wantJSON || opts.Probe != tc.wantProbe || opts.NoColor != tc.wantColor {
				t.Fatalf("opts = %+v", opts)
			}
		})
	}
}

func TestReportExitCode(t *testing.T) {
	tests := []struct {
		name string
		conv Health
		anal Health
		conf Health
		want int
	}{
		{name: "all ok", conv: HealthOK, anal: HealthOK, conf: HealthOK, want: 0},
		{name: "warnings only", conv: HealthWarn, anal: HealthWarn, conf: HealthWarn, want: 0},
		{name: "analytics broken", conv: HealthOK, anal: HealthError, conf: HealthOK, want: 1},
		{name: "conversations broken", conv: HealthError, anal: HealthOK, conf: HealthOK, want: 1},
		{name: "config broken", conv: HealthOK, anal: HealthOK, conf: HealthError, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := &Report{
				Conversations: ConversationsSection{Health: tc.conv},
				Analytics:     AnalyticsSection{Health: tc.anal},
				Config:        ConfigSection{Health: tc.conf},
			}
			if got := r.exitCode(); got != tc.want {
				t.Fatalf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestCollectConversations(t *testing.T) {
	tests := []struct {
		name       string
		osEnv      map[string]string
		wantHealth Health
		wantMsg    string
	}{
		{
			name:       "all set",
			osEnv:      map[string]string{"SIGIL_ENDPOINT": "https://x", "SIGIL_AUTH_TENANT_ID": "1", "SIGIL_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthOK,
		},
		{
			name:       "none set",
			osEnv:      map[string]string{},
			wantHealth: HealthWarn,
			wantMsg:    "not configured",
		},
		{
			name:       "missing token",
			osEnv:      map[string]string{"SIGIL_ENDPOINT": "https://x", "SIGIL_AUTH_TENANT_ID": "1"},
			wantHealth: HealthError,
			wantMsg:    "AGENTO11Y_AUTH_TOKEN",
		},
		{
			name:       "preferred-only credentials",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "https://x", "AGENTO11Y_AUTH_TENANT_ID": "1", "AGENTO11Y_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthOK,
		},
		{
			name:       "legacy-only credentials suggest migration",
			osEnv:      map[string]string{"SIGIL_ENDPOINT": "https://x", "SIGIL_AUTH_TENANT_ID": "1", "SIGIL_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthOK,
			wantMsg:    "preferred names are AGENTO11Y_*",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sec := collectConversations(tc.osEnv, nil)
			if sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
		})
	}
}

func TestCollectConversationsConflictingTokens(t *testing.T) {
	osEnv := map[string]string{
		"AGENTO11Y_ENDPOINT":       "https://x",
		"AGENTO11Y_AUTH_TENANT_ID": "1",
		"AGENTO11Y_AUTH_TOKEN":     "preferred-secret",
		"SIGIL_AUTH_TOKEN":         "legacy-secret",
	}
	sec := collectConversations(osEnv, nil)
	if sec.Token.Key != "AGENTO11Y_AUTH_TOKEN" {
		t.Fatalf("token key = %q, want AGENTO11Y_AUTH_TOKEN", sec.Token.Key)
	}
	if !sec.Token.Conflict {
		t.Fatalf("token conflict not flagged: %+v", sec.Token)
	}
	encoded, err := json.Marshal(sec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	for _, secret := range []string{"preferred-secret", "legacy-secret"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("section JSON leaks token value %q: %s", secret, encoded)
		}
	}
	if !strings.Contains(strings.Join(sec.Messages, " "), "AGENTO11Y_AUTH_TOKEN") {
		t.Fatalf("messages %v do not name the selected key", sec.Messages)
	}
}

func TestResolveFamilySourceBeatsSpelling(t *testing.T) {
	osEnv := map[string]string{"SIGIL_ENDPOINT": "shell-legacy"}
	fileEnv := map[string]string{"AGENTO11Y_ENDPOINT": "file-preferred"}
	r := resolveFamily("ENDPOINT", osEnv, fileEnv)
	if r.value != "shell-legacy" || r.key != "SIGIL_ENDPOINT" || r.source != sourceEnv {
		t.Fatalf("resolveFamily = %+v, want shell legacy winner", r)
	}
	if !r.conflict {
		t.Fatalf("expected conflict flag when spellings disagree: %+v", r)
	}
}

func TestCollectAnalytics(t *testing.T) {
	tests := []struct {
		name         string
		osEnv        map[string]string
		convConfig   bool
		wantHealth   Health
		wantEndpoint string
		wantVar      string
		wantMsg      string
	}{
		{
			name: "sigil otlp set with auth",
			osEnv: map[string]string{
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp",
				"SIGIL_AUTH_TENANT_ID":              "12345",
				"SIGIL_OTEL_AUTH_TOKEN":             "glc_tok",
			},
			wantHealth:   HealthOK,
			wantEndpoint: "https://otlp",
			wantVar:      "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
			wantMsg:      "preferred names are AGENTO11Y_*",
		},
		{
			name: "conflicting otlp spellings flag the disagreement",
			osEnv: map[string]string{
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://preferred",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":     "https://legacy",
				"AGENTO11Y_AUTH_TENANT_ID":              "12345",
				"AGENTO11Y_OTEL_AUTH_TOKEN":             "glc_tok",
			},
			wantHealth:   HealthOK,
			wantEndpoint: "https://preferred",
			wantVar:      "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT",
			wantMsg:      "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT and its other spelling are both set with different values",
		},
		{
			name: "standard otel fallback with auth via SIGIL_AUTH_TOKEN",
			osEnv: map[string]string{
				"OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp2",
				"SIGIL_AUTH_TENANT_ID":        "12345",
				"SIGIL_AUTH_TOKEN":            "glc_tok",
			},
			wantHealth:   HealthOK,
			wantEndpoint: "https://otlp2",
			wantVar:      "OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name: "auth via OTEL_EXPORTER_OTLP_HEADERS authorization entry",
			osEnv: map[string]string{
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp",
				"OTEL_EXPORTER_OTLP_HEADERS":        "authorization=Basic abc,x-extra=1",
			},
			wantHealth:   HealthOK,
			wantEndpoint: "https://otlp",
			wantVar:      "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name: "empty authorization header value is not auth",
			osEnv: map[string]string{
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp",
				"OTEL_EXPORTER_OTLP_HEADERS":        "authorization=  ,x-extra=1",
			},
			wantHealth:   HealthWarn,
			wantEndpoint: "https://otlp",
			wantVar:      "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name:         "endpoint set but no auth resolvable is a warning",
			osEnv:        map[string]string{"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp"},
			wantHealth:   HealthWarn,
			wantEndpoint: "https://otlp",
			wantVar:      "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		{
			name:       "missing but conversations configured is the headline error",
			osEnv:      map[string]string{},
			convConfig: true,
			wantHealth: HealthError,
		},
		{
			name:       "missing and nothing configured is just a warning",
			osEnv:      map[string]string{},
			convConfig: false,
			wantHealth: HealthWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sec := collectAnalytics(tc.osEnv, nil, tc.convConfig)
			if sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
			if tc.wantEndpoint != "" && sec.Endpoint.Value != tc.wantEndpoint {
				t.Fatalf("endpoint = %q, want %q", sec.Endpoint.Value, tc.wantEndpoint)
			}
			if tc.wantVar != "" && sec.EndpointVar != tc.wantVar {
				t.Fatalf("endpoint var = %q, want %q", sec.EndpointVar, tc.wantVar)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
		})
	}
}

func TestRunProbes(t *testing.T) {
	tests := []struct {
		name          string
		conv          *ProbeResult
		otlp          *AnalyticsProbe
		wantConv      Health
		wantAnalytics Health
	}{
		{
			name:          "all reachable",
			conv:          &ProbeResult{StatusCode: 200, OK: true},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthOK,
			wantAnalytics: HealthOK,
		},
		{
			name:          "auth failure escalates",
			conv:          &ProbeResult{StatusCode: 403, Message: "missing sigil:write"},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 401}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthError,
			wantAnalytics: HealthError,
		},
		{
			name:          "transport error escalates",
			conv:          &ProbeResult{StatusCode: 0, Message: "connection refused"},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 0, Message: "timeout"}, Traces: &ProbeResult{StatusCode: 0, Message: "timeout"}},
			wantConv:      HealthError,
			wantAnalytics: HealthError,
		},
		{
			name:          "5xx escalates",
			conv:          &ProbeResult{StatusCode: 503},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 500}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthError,
			wantAnalytics: HealthError,
		},
		{
			name:          "benign 4xx stays healthy",
			conv:          &ProbeResult{StatusCode: 400},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 415}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthOK,
			wantAnalytics: HealthOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prevConv, prevOTLP := probeConversationsFn, probeOTLPFn
			t.Cleanup(func() { probeConversationsFn, probeOTLPFn = prevConv, prevOTLP })
			probeConversationsFn = func(context.Context, string, string, string, bool) *ProbeResult { return tc.conv }
			probeOTLPFn = func(context.Context) *AnalyticsProbe { return tc.otlp }

			r := &Report{
				Conversations: ConversationsSection{
					Endpoint: envValue{Set: true, Value: "https://sigil.example.net"},
					TenantID: envValue{Set: true, Value: "t"},
					Token:    tokenValue{Set: true},
					Health:   HealthOK,
				},
				Analytics: AnalyticsSection{
					Endpoint: envValue{Set: true, Value: "https://otlp.example.net"},
					Health:   HealthOK,
				},
			}
			runProbes(context.Background(), r, map[string]string{}, nil)
			if r.Conversations.Health != tc.wantConv {
				t.Fatalf("conversations health = %q, want %q", r.Conversations.Health, tc.wantConv)
			}
			if r.Analytics.Health != tc.wantAnalytics {
				t.Fatalf("analytics health = %q, want %q", r.Analytics.Health, tc.wantAnalytics)
			}
		})
	}
}

func TestResolveEnv(t *testing.T) {
	tests := []struct {
		name       string
		osEnv      map[string]string
		fileEnv    map[string]string
		wantSet    bool
		wantValue  string
		wantSource string
	}{
		{name: "os env wins", osEnv: map[string]string{"K": "fromenv"}, fileEnv: map[string]string{"K": "fromfile"}, wantSet: true, wantValue: "fromenv", wantSource: sourceEnv},
		{name: "config fallback", osEnv: map[string]string{}, fileEnv: map[string]string{"K": "fromfile"}, wantSet: true, wantValue: "fromfile", wantSource: sourceConfig},
		{name: "unset", osEnv: map[string]string{}, fileEnv: map[string]string{}, wantSet: false},
		{name: "blank os falls through to file", osEnv: map[string]string{"K": "  "}, fileEnv: map[string]string{"K": "fromfile"}, wantSet: true, wantValue: "fromfile", wantSource: sourceConfig},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveEnv("K", tc.osEnv, tc.fileEnv)
			if got.set != tc.wantSet || got.value != tc.wantValue {
				t.Fatalf("got %+v", got)
			}
			if tc.wantSet && got.source != tc.wantSource {
				t.Fatalf("source = %q, want %q", got.source, tc.wantSource)
			}
		})
	}
}

func TestTokenPrefix(t *testing.T) {
	tests := []struct {
		token string
		want  string
	}{
		{token: "glc_abcdef", want: "glc_"},
		{token: "glsa_xyz", want: "glsa_"},
		{token: "nounderscore", want: ""},
		{token: "", want: ""},
		{token: "_leading", want: ""},
		{token: "averyverylongprefix_x", want: ""}, // prefix too long to be a scheme marker
	}
	for _, tc := range tests {
		t.Run(tc.token, func(t *testing.T) {
			if got := tokenPrefix(tc.token); got != tc.want {
				t.Fatalf("tokenPrefix(%q) = %q, want %q", tc.token, got, tc.want)
			}
		})
	}
}

func TestDisallowedKeys(t *testing.T) {
	isolateEnv(t)
	writeConfig(t, strings.Join([]string{
		"SIGIL_ENDPOINT=https://x",
		"export OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp",
		"# comment",
		"RANDOM_KEY=nope",
		"AWS_SECRET=leak",
		"RANDOM_KEY=dup", // duplicate reported once
	}, "\n"))

	got := disallowedKeys(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "agento11y", "config.env"))
	want := []string{"RANDOM_KEY", "AWS_SECRET"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("disallowedKeys = %v, want %v", got, want)
	}
}

// TestCollectConfig_PathResolution pins the reported config path to the
// dotenv resolver: the new agento11y path wins when present, the legacy
// sigil path is still reported while only it exists, and a missing config
// reports the new default with Exists=false.
func TestCollectConfig_PathResolution(t *testing.T) {
	tests := []struct {
		name       string
		apps       []string
		wantApp    string
		wantExists bool
	}{
		{name: "no config reports new default", wantApp: "agento11y", wantExists: false},
		{name: "new only", apps: []string{"agento11y"}, wantApp: "agento11y", wantExists: true},
		{name: "legacy only", apps: []string{"sigil"}, wantApp: "sigil", wantExists: true},
		{name: "both prefer new", apps: []string{"agento11y", "sigil"}, wantApp: "agento11y", wantExists: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			for _, app := range tc.apps {
				writeConfigApp(t, app, "SIGIL_ENDPOINT=https://x\n")
			}

			sec := collectConfig(nil, nil)
			want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), tc.wantApp, "config.env")
			if sec.Path != want {
				t.Fatalf("Path = %q, want %q", sec.Path, want)
			}
			if sec.Exists != tc.wantExists {
				t.Fatalf("Exists = %v, want %v", sec.Exists, tc.wantExists)
			}
		})
	}
}

func TestCollectConfig_ContentMode(t *testing.T) {
	tests := []struct {
		name         string
		mode         string
		wantMode     string
		wantFellBack bool
		wantHealth   Health // "" = don't assert
	}{
		{name: "invalid mode falls back", mode: "bogus", wantMode: "metadata_only", wantFellBack: true, wantHealth: HealthWarn},
		{name: "valid mode no fallback", mode: "full", wantMode: "full", wantFellBack: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", tc.mode)

			sec := collectConfig(nil, nil)
			if sec.ContentModeFellBack != tc.wantFellBack {
				t.Fatalf("ContentModeFellBack = %v, want %v", sec.ContentModeFellBack, tc.wantFellBack)
			}
			if sec.ContentCaptureMode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", sec.ContentCaptureMode, tc.wantMode)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
		})
	}
}

func TestCollectConfig_Guards(t *testing.T) {
	tests := []struct {
		name          string
		enabled       string
		timeout       string
		failOpen      string
		wantEnabled   bool
		wantTimeoutMs int
		wantFailOpen  bool
		wantFellBack  bool
		wantHealth    Health // "" = don't assert
	}{
		{name: "unset uses defaults", wantEnabled: false, wantTimeoutMs: 1500, wantFailOpen: true},
		{name: "enabled fail-open", enabled: "true", wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true},
		{name: "enabled fail-closed with timeout", enabled: "1", timeout: "500", failOpen: "false", wantEnabled: true, wantTimeoutMs: 500, wantFailOpen: false},
		{name: "invalid enabled falls back", enabled: "maybe", wantEnabled: false, wantTimeoutMs: 1500, wantFailOpen: true, wantFellBack: true, wantHealth: HealthWarn},
		{name: "invalid timeout falls back", enabled: "true", timeout: "-1", wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true, wantFellBack: true, wantHealth: HealthWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			// ResolveGuards reads the process env directly, so clear the guard
			// vars first to keep the baseline independent of the host shell.
			t.Setenv("SIGIL_GUARDS_ENABLED", tc.enabled)
			t.Setenv("SIGIL_GUARDS_TIMEOUT_MS", tc.timeout)
			t.Setenv("SIGIL_GUARDS_FAIL_OPEN", tc.failOpen)

			sec := collectConfig(nil, nil)
			if sec.GuardsEnabled != tc.wantEnabled {
				t.Fatalf("GuardsEnabled = %v, want %v", sec.GuardsEnabled, tc.wantEnabled)
			}
			if sec.GuardsTimeoutMs != tc.wantTimeoutMs {
				t.Fatalf("GuardsTimeoutMs = %d, want %d", sec.GuardsTimeoutMs, tc.wantTimeoutMs)
			}
			if sec.GuardsFailOpen != tc.wantFailOpen {
				t.Fatalf("GuardsFailOpen = %v, want %v", sec.GuardsFailOpen, tc.wantFailOpen)
			}
			if sec.GuardsFellBack != tc.wantFellBack {
				t.Fatalf("GuardsFellBack = %v, want %v", sec.GuardsFellBack, tc.wantFellBack)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
		})
	}
}

func TestCollectConfig_Tags(t *testing.T) {
	tests := []struct {
		name       string
		osEnv      map[string]string
		fileEnv    map[string]string
		wantTags   map[string]string
		wantSource string
		wantMsg    string
	}{
		{name: "no tags", wantTags: nil},
		{
			name:       "from env",
			osEnv:      map[string]string{"SIGIL_TAGS": "team=assistant,env=prod"},
			wantTags:   map[string]string{"team": "assistant", "env": "prod"},
			wantSource: sourceEnv,
		},
		{
			name:       "from config.env when env unset",
			fileEnv:    map[string]string{"SIGIL_TAGS": "team=alerting"},
			wantTags:   map[string]string{"team": "alerting"},
			wantSource: sourceConfig,
		},
		{
			name:     "all entries malformed yields no tags",
			osEnv:    map[string]string{"SIGIL_TAGS": "novalue,=noKey"},
			wantTags: nil,
		},
		{
			name: "conflicting spellings flag the disagreement",
			osEnv: map[string]string{
				"AGENTO11Y_TAGS": "env=prod",
				"SIGIL_TAGS":     "env=staging",
			},
			wantTags:   map[string]string{"env": "prod"},
			wantSource: sourceEnv,
			wantMsg:    "AGENTO11Y_TAGS and its other spelling are both set with different values; using AGENTO11Y_TAGS",
		},
		{
			name:       "legacy-only tags suggest migration",
			osEnv:      map[string]string{"SIGIL_TAGS": "env=prod"},
			wantTags:   map[string]string{"env": "prod"},
			wantSource: sourceEnv,
			wantMsg:    "preferred name is AGENTO11Y_TAGS",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			sec := collectConfig(tc.osEnv, tc.fileEnv)
			if len(sec.Tags) != len(tc.wantTags) {
				t.Fatalf("tags = %v, want %v", sec.Tags, tc.wantTags)
			}
			for k, v := range tc.wantTags {
				if sec.Tags[k] != v {
					t.Fatalf("tags[%q] = %q, want %q", k, sec.Tags[k], v)
				}
			}
			if sec.TagsSource != tc.wantSource {
				t.Fatalf("tags source = %q, want %q", sec.TagsSource, tc.wantSource)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
		})
	}
}

func TestRun(t *testing.T) {
	convOnly := map[string]string{
		"SIGIL_ENDPOINT":       "https://sigil.example.net",
		"SIGIL_AUTH_TENANT_ID": "12345",
		"SIGIL_AUTH_TOKEN":     "glc_supersecretvalue",
	}
	healthy := map[string]string{
		"SIGIL_ENDPOINT":                    "https://sigil.example.net",
		"SIGIL_AUTH_TENANT_ID":              "12345",
		"SIGIL_AUTH_TOKEN":                  "glc_t",
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.net/otlp",
	}
	tests := []struct {
		name     string
		args     []string
		osEnv    map[string]string
		stub     bool // swap the network/agent seams for fakes
		wantCode int
		check    func(t *testing.T, stdout string)
	}{
		{
			// conversations set but analytics unset → exit 1.
			name:     "json redacts token and flags the analytics gap",
			args:     []string{"--json"},
			osEnv:    convOnly,
			stub:     true,
			wantCode: 1,
			check: func(t *testing.T, stdout string) {
				if strings.Contains(stdout, "supersecret") {
					t.Fatalf("token value leaked into JSON output:\n%s", stdout)
				}
				var report map[string]any
				if err := json.Unmarshal([]byte(stdout), &report); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
				for _, key := range []string{"sigil", "config", "conversations", "analytics", "agents"} {
					if _, ok := report[key]; !ok {
						t.Fatalf("JSON missing section %q", key)
					}
				}
			},
		},
		{name: "fully configured exits 0", osEnv: healthy, stub: true, wantCode: 0},
		{name: "bad flag exits 2", args: []string{"--bogus"}, wantCode: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			if tc.stub {
				stubSeams(t)
			}
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), tc.args, Params{Version: "1.2.3", OSEnv: tc.osEnv, Stdout: &stdout, Stderr: &stderr})
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d (stdout=%s stderr=%s)", code, tc.wantCode, stdout.String(), stderr.String())
			}
			if tc.check != nil {
				tc.check(t, stdout.String())
			}
		})
	}
}
