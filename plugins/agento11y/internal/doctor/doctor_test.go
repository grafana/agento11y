package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// mergeEnv returns the union of the given env maps, later entries winning.
func mergeEnv(envs ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, env := range envs {
		maps.Copy(out, env)
	}
	return out
}

// isolateEnv points the dotenv/state roots at a fresh tempdir and clears the
// branded env vars so a test never reads the developer's real config.
//
// It clears every AGENTO11Y_* and SIGIL_* key the shell exports rather than a
// hand-listed subset, so a family a test does not name cannot let a host value
// decide the result.
func isolateEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "AGENTO11Y_") || strings.HasPrefix(key, "SIGIL_") {
			t.Setenv(key, "")
		}
	}
	// trackedKeys adds the unbranded OTel vars and any branded key the shell
	// happens not to export.
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
	probeConversationsFn = func(context.Context, string, envValue, string, bool) *ProbeResult {
		return &ProbeResult{StatusCode: 200, OK: true}
	}
	probeOTLPFn = func(context.Context, envValue) *AnalyticsProbe { return nil }
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
		wantColor bool // NoColor
		wantErr   bool
	}{
		{name: "no flags"},
		{name: "json", args: []string{"--json"}, wantJSON: true},
		{name: "no-color", args: []string{"--no-color"}, wantColor: true},
		// Probing is unconditional now. The old flags must still parse, so a
		// script or runbook that passes them does not start failing.
		{name: "probe is accepted and ignored", args: []string{"--probe"}},
		{name: "online alias is accepted and ignored", args: []string{"--online"}},
		{name: "combined", args: []string{"--json", "--probe", "--no-color"}, wantJSON: true, wantColor: true},
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
			if opts.JSON != tc.wantJSON || opts.NoColor != tc.wantColor {
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
		name string
		// endpoint is shorthand for a fully configured section with that
		// endpoint; set osEnv instead to control the other credentials.
		endpoint   string
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
		{
			// Set credentials alone are not a working config: an endpoint the
			// exporter cannot build a request from fails without the network.
			name:       "malformed endpoint fails offline",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "not a url ://", "AGENTO11Y_AUTH_TENANT_ID": "1", "AGENTO11Y_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthError,
			wantMsg:    "AGENTO11Y_ENDPOINT is not a usable endpoint",
		},
		{
			name:       "endpoint with an empty host fails offline",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "https:///generations", "AGENTO11Y_AUTH_TENANT_ID": "1", "AGENTO11Y_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthError,
			wantMsg:    "not a usable endpoint",
		},
		{
			// The exporter prepends https:// (http:// under INSECURE) to a
			// scheme-less endpoint, so it is usable and doctor must accept it.
			name:       "scheme-less endpoint stays valid",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "collector.local:4317", "AGENTO11Y_AUTH_TENANT_ID": "1", "AGENTO11Y_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthOK,
		},
		{
			// The reported failure: the Agent Observability app page pasted in as
			// the endpoint. Every export is redirected to /login, so doctor must
			// say so without the network.
			name:       "app page path fails offline",
			endpoint:   "https://mystack.grafana.net/plugins/grafana-agento11y-app",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			// The path check ignores the host, so a stack on a custom domain is
			// caught too.
			name:       "app page path on a custom domain fails offline",
			endpoint:   "https://grafana.example.com/plugins/grafana-agento11y-app",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			name:       "another plugins path fails offline",
			endpoint:   "https://host/plugins/x",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			// The other app URL: login.go prints this one after a successful
			// login, so it is at least as likely a paste as the plugin page.
			name:       "app path fails offline",
			endpoint:   "https://mystack.grafana.net/a/grafana-agento11y-app",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			name:       "app path on a custom domain fails offline",
			endpoint:   "https://grafana.example.com/a/grafana-agento11y-app",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			name:       "a page under the app path fails offline",
			endpoint:   "https://grafana.example.com/a/grafana-agento11y-app/conversations",
			wantHealth: HealthError,
			wantMsg:    "points at a Grafana app page",
		},
		{
			// A bare /a/ is not judged: a self-hosted deployment could sit there.
			name:       "another app path is not judged",
			endpoint:   "https://grafana.example.com/a/other-app",
			wantHealth: HealthOK,
		},
		{
			name:       "grafana cloud stack host warns",
			endpoint:   "https://mystack.grafana.net",
			wantHealth: HealthWarn,
			wantMsg:    "does not look like an Agent Observability API URL",
		},
		{
			name:       "otlp gateway in the conversations variable warns",
			endpoint:   "https://otlp-gateway-prod-us-east-2.grafana.net/otlp",
			wantHealth: HealthWarn,
			wantMsg:    "belongs in AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT",
		},
		// Every real API host shape: the first label starts with sigil or
		// agento11y, so none of them warns.
		{name: "prod sigil cell", endpoint: "https://sigil-prod-us-east-0.grafana.net", wantHealth: HealthOK},
		{name: "prod agento11y cell", endpoint: "https://agento11y-prod-us-east-0.grafana.net", wantHealth: HealthOK},
		{name: "dev sigil cell", endpoint: "https://sigil-dev-001.grafana-dev.net", wantHealth: HealthOK},
		{name: "regional dev sigil cell", endpoint: "https://sigil-dev-eu-west-2.grafana-dev.net", wantHealth: HealthOK},
		{name: "dev agento11y cell", endpoint: "https://agento11y-dev-001.grafana-dev.net", wantHealth: HealthOK},
		{name: "ops sigil cell", endpoint: "https://sigil-ops-001.grafana-ops.net", wantHealth: HealthOK},
		{name: "sigilai", endpoint: "https://sigilai.grafana.net", wantHealth: HealthOK},
		// Outside the Grafana-owned domains the host is not judged at all: a
		// self-hosted Sigil or a test collector can use any hostname.
		{name: "self-hosted host is not judged", endpoint: "https://sigil.example", wantHealth: HealthOK},
		{name: "internal host is not judged", endpoint: "https://sigil.internal.example", wantHealth: HealthOK},
		{name: "single-label host is not judged", endpoint: "https://x", wantHealth: HealthOK},
		{name: "scheme-less collector is not judged", endpoint: "collector.local:4317", wantHealth: HealthOK},
		{name: "loopback receiver is not judged", endpoint: "http://127.0.0.1:9000", wantHealth: HealthOK},
		{
			// The shape heuristic must not turn an error into a warning.
			name:       "suspicious host does not downgrade a credentials error",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "https://mystack.grafana.net", "AGENTO11Y_AUTH_TENANT_ID": "1"},
			wantHealth: HealthError,
			wantMsg:    "incomplete credentials; missing AGENTO11Y_AUTH_TOKEN",
		},
		{
			name:       "malformed endpoint error survives the shape check",
			osEnv:      map[string]string{"AGENTO11Y_ENDPOINT": "https:///plugins/grafana-agento11y-app", "AGENTO11Y_AUTH_TENANT_ID": "1", "AGENTO11Y_AUTH_TOKEN": "glc_t"},
			wantHealth: HealthError,
			wantMsg:    "not a usable endpoint",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			osEnv := tc.osEnv
			if tc.endpoint != "" {
				osEnv = map[string]string{
					"AGENTO11Y_ENDPOINT":       tc.endpoint,
					"AGENTO11Y_AUTH_TENANT_ID": "1",
					"AGENTO11Y_AUTH_TOKEN":     "glc_t",
				}
			}
			sec := collectConversations(osEnv, nil)
			if sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q (messages %v)", sec.Health, tc.wantHealth, sec.Messages)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			// The shorthand configures the preferred spellings only, so a healthy
			// section must be silent: any message there is a stray shape warning.
			if tc.endpoint != "" && tc.wantHealth == HealthOK && len(sec.Messages) != 0 {
				t.Fatalf("healthy section carries messages %v", sec.Messages)
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

// TestRenderHuman_RowNamesOnlyTheWinningSpelling walks the whole path from the
// snapshot to the rendered row. A row that named the losing spelling would send
// a user to edit a value the binary never reads.
func TestRenderHuman_RowNamesOnlyTheWinningSpelling(t *testing.T) {
	osEnv := map[string]string{
		"AGENTO11Y_ENDPOINT":       "https://shell.example",
		"AGENTO11Y_AUTH_TENANT_ID": "12345",
		"AGENTO11Y_AUTH_TOKEN":     "glc_shell",
	}
	fileEnv := map[string]string{"SIGIL_ENDPOINT": "https://file.example"}

	var buf bytes.Buffer
	renderHuman(&buf, &Report{Conversations: collectConversations(osEnv, fileEnv)}, false)
	out := buf.String()

	if want := "https://shell.example (AGENTO11Y_ENDPOINT, env)"; !strings.Contains(out, want) {
		t.Fatalf("endpoint row missing %q:\n%s", want, out)
	}
	endpointRow, _, _ := strings.Cut(out[strings.Index(out, "endpoint:"):], "\n")
	for _, loser := range []string{"SIGIL_ENDPOINT", sourceConfig, "file.example"} {
		if strings.Contains(endpointRow, loser) {
			t.Fatalf("endpoint row %q names the losing spelling %q", endpointRow, loser)
		}
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

// TestConflictMessage checks the sentence a user reads when both spellings of
// one setting are set. It has to name the losing variable and where it is set:
// the two spellings are the same setting, so a reader told only the winner
// cannot tell what to change.
func TestConflictMessage(t *testing.T) {
	tests := []struct {
		name    string
		osEnv   map[string]string
		fileEnv map[string]string
		want    string
	}{
		{
			name:  "both in the environment",
			osEnv: map[string]string{"AGENTO11Y_ENDPOINT": "https://a", "SIGIL_ENDPOINT": "https://b"},
			want: "AGENTO11Y_ENDPOINT and SIGIL_ENDPOINT are both set in the environment, to different values; " +
				"using AGENTO11Y_ENDPOINT. SIGIL_ENDPOINT is the old name for the same setting: unset it.",
		},
		{
			name:    "both in config.env",
			fileEnv: map[string]string{"AGENTO11Y_ENDPOINT": "https://a", "SIGIL_ENDPOINT": "https://b"},
			want: "AGENTO11Y_ENDPOINT and SIGIL_ENDPOINT are both set in config.env, to different values; " +
				"using AGENTO11Y_ENDPOINT. SIGIL_ENDPOINT is the old name for the same setting: remove it from config.env.",
		},
		{
			name:    "preferred in the environment beats legacy in config.env",
			osEnv:   map[string]string{"AGENTO11Y_ENDPOINT": "https://a"},
			fileEnv: map[string]string{"SIGIL_ENDPOINT": "https://b"},
			want: "AGENTO11Y_ENDPOINT is set in the environment and SIGIL_ENDPOINT in config.env, to different values; " +
				"using AGENTO11Y_ENDPOINT. SIGIL_ENDPOINT is the old name for the same setting: remove it from config.env.",
		},
		{
			// The surprising case: the old name wins. Naming the rule is the only
			// way a reader can tell why the config.env value is not in force.
			name:    "legacy in the environment beats preferred in config.env",
			osEnv:   map[string]string{"SIGIL_ENDPOINT": "https://b"},
			fileEnv: map[string]string{"AGENTO11Y_ENDPOINT": "https://a"},
			want: "SIGIL_ENDPOINT is set in the environment and AGENTO11Y_ENDPOINT in config.env, to different values; " +
				"using SIGIL_ENDPOINT, because the environment outranks config.env. " +
				"SIGIL_ENDPOINT is the old name for the same setting: unset it.",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := resolveFamily("ENDPOINT", tc.osEnv, tc.fileEnv)
			if !r.conflict {
				t.Fatalf("resolveFamily = %+v, want a conflict", r)
			}
			if got := conflictMessage(r); got != tc.want {
				t.Fatalf("conflictMessage =\n%s\nwant\n%s", got, tc.want)
			}
		})
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
			wantMsg: "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT and SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT are both set in the environment, " +
				"to different values; using AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT",
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
			if tc.wantVar != "" && sec.Endpoint.Key != tc.wantVar {
				t.Fatalf("endpoint key = %q, want %q", sec.Endpoint.Key, tc.wantVar)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
		})
	}
}

func TestRunProbes(t *testing.T) {
	tests := []struct {
		name string
		conv *ProbeResult
		otlp *AnalyticsProbe
		// convHealth and analyticsHealth are the verdicts the offline collectors
		// reached; "" means ok.
		convHealth      Health
		analyticsHealth Health
		// convKey is the endpoint spelling that won; "" leaves it unset.
		convKey         string
		wantConv        Health
		wantAnalytics   Health
		wantConvSkipped bool
		// wantConvMsgs and wantOTLPMsgs are the section message counts. A probe
		// that repeats what its own row prints shows up as an extra message here.
		wantConvMsgs int
		wantOTLPMsgs int
		// wantConvMsg and wantAnalyticsMsg are substrings the section messages must
		// carry. The probe rows show only the status and the URL, so a cause the
		// message drops is absent from the whole human report.
		wantConvMsg      string
		wantAnalyticsMsg string
	}{
		{
			name:          "all reachable",
			conv:          &ProbeResult{StatusCode: 200, OK: true},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthOK,
			wantAnalytics: HealthOK,
		},
		{
			// The row prints the status alone, so the section message has to
			// carry the credential diagnosis.
			name:             "401 escalates and reports why auth failed",
			conv:             &ProbeResult{StatusCode: 401, Message: "credentials rejected"},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 401, Message: "credentials rejected"}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:         HealthError,
			wantAnalytics:    HealthError,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "the conversations endpoint rejected the export request: HTTP 401: credentials rejected",
			wantAnalyticsMsg: "the OTLP endpoint rejected the metrics export: HTTP 401: credentials rejected",
		},
		{
			name:             "403 escalates and reports the missing scope",
			conv:             &ProbeResult{StatusCode: 403, Message: "missing sigil:write"},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 403, Message: "missing traces:write"}},
			wantConv:         HealthError,
			wantAnalytics:    HealthError,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "the conversations endpoint rejected the export request: HTTP 403: missing sigil:write",
			wantAnalyticsMsg: "the OTLP endpoint rejected the traces export: HTTP 403: missing traces:write",
		},
		{
			// Both signals authenticate with the same credentials, so one
			// diagnosis covers them and is printed once.
			name:             "both OTLP signals failing the same way share one message",
			conv:             &ProbeResult{StatusCode: 202, OK: true},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 401, Message: "credentials rejected"}, Traces: &ProbeResult{StatusCode: 401, Message: "credentials rejected"}},
			wantConv:         HealthOK,
			wantAnalytics:    HealthError,
			wantOTLPMsgs:     1,
			wantAnalyticsMsg: "the OTLP endpoint rejected the metrics and traces exports: HTTP 401: credentials rejected",
		},
		{
			// A token that carries one write scope and not the other fails the
			// signals differently, and each failure is its own diagnosis.
			name:             "OTLP signals failing differently get a line each",
			conv:             &ProbeResult{StatusCode: 202, OK: true},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 401, Message: "credentials rejected"}, Traces: &ProbeResult{StatusCode: 403, Message: "missing traces:write"}},
			wantConv:         HealthOK,
			wantAnalytics:    HealthError,
			wantOTLPMsgs:     2,
			wantAnalyticsMsg: "the OTLP endpoint rejected the traces export: HTTP 403: missing traces:write",
		},
		{
			name:             "transport error escalates",
			conv:             &ProbeResult{StatusCode: 0, Message: "connection refused"},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 0, Message: "dial tcp: lookup otlp.example: no such host"}, Traces: &ProbeResult{StatusCode: 0, Message: "dial tcp: lookup otlp.example: no such host"}},
			wantConv:         HealthError,
			wantAnalytics:    HealthError,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "no response: connection refused",
			wantAnalyticsMsg: "no response: dial tcp: lookup otlp.example: no such host",
		},
		{
			name:             "5xx escalates",
			conv:             &ProbeResult{StatusCode: 503},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 500}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:         HealthError,
			wantAnalytics:    HealthError,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "HTTP 503",
			wantAnalyticsMsg: "HTTP 500",
		},
		{
			// The endpoint answered, but it bounced the export POST somewhere
			// else, so nothing is ingested.
			name:          "conversations redirect escalates",
			conv:          &ProbeResult{StatusCode: 302, Message: "redirected to /login"},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthError,
			wantAnalytics: HealthOK,
			wantConvMsgs:  1,
			wantConvMsg:   "is not an Agent Observability API URL",
		},
		{
			// The legacy spelling won, so the message must name it rather than
			// the preferred one the user never set.
			name:          "redirect message names the resolved endpoint key",
			conv:          &ProbeResult{StatusCode: 302, Message: "redirected to /login"},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			convKey:       "SIGIL_ENDPOINT",
			wantConv:      HealthError,
			wantAnalytics: HealthOK,
			wantConvMsgs:  1,
			wantConvMsg:   "SIGIL_ENDPOINT is not an Agent Observability API URL",
		},
		{
			name:             "otlp metrics redirect escalates",
			conv:             &ProbeResult{StatusCode: 200, OK: true},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 307, Message: "redirected to /login"}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:         HealthOK,
			wantAnalytics:    HealthError,
			wantOTLPMsgs:     1,
			wantAnalyticsMsg: "the OTLP endpoint redirected the export request, so it is not an OTLP ingest URL",
		},
		{
			// Either signal redirecting is enough to condemn the endpoint.
			name:             "otlp traces redirect escalates",
			conv:             &ProbeResult{StatusCode: 200, OK: true},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 302, Message: "redirected to /login"}},
			wantConv:         HealthOK,
			wantAnalytics:    HealthError,
			wantOTLPMsgs:     1,
			wantAnalyticsMsg: "the OTLP endpoint redirected the export request, so it is not an OTLP ingest URL",
		},
		{
			// The host answered but does not serve the export route, so the
			// endpoint belongs to some other service. Every host 404s an unknown
			// path, so this must never read as healthy.
			name:             "404 escalates on both pipelines",
			conv:             &ProbeResult{StatusCode: 404},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 404}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:         HealthError,
			wantAnalytics:    HealthError,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "no generation-export route (HTTP 404)",
			wantAnalyticsMsg: "no ingest route (HTTP 404)",
		},
		{
			// A body-validating endpoint can answer 400/415 to the minimal probe
			// body while real exports work, so warn instead of failing. The real
			// export route answers 202, so it cannot pass either.
			name:             "body rejection warns",
			conv:             &ProbeResult{StatusCode: 400},
			otlp:             &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 415}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:         HealthWarn,
			wantAnalytics:    HealthWarn,
			wantConvMsgs:     1,
			wantOTLPMsgs:     1,
			wantConvMsg:      "answered HTTP 400 instead of HTTP 202",
			wantAnalyticsMsg: "did not accept the probe",
		},
		{
			// 405 is the one non-2xx that passes: a method-restricted route still
			// proves the request reached the service and passed auth.
			name:          "405 stays healthy",
			conv:          &ProbeResult{StatusCode: 405, OK: true},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 405, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			wantConv:      HealthOK,
			wantAnalytics: HealthOK,
		},
		{
			// A probe warning must not clear the offline endpoint error either.
			name:            "body rejection does not downgrade an analytics error",
			conv:            &ProbeResult{StatusCode: 202, OK: true},
			otlp:            &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 400}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			analyticsHealth: HealthError,
			wantConv:        HealthOK,
			wantAnalytics:   HealthError,
			wantOTLPMsgs:    1,
		},
		{
			// Only HealthError suppresses probing, which is what makes the
			// offline shape heuristic safe: a warned endpoint is still probed,
			// and the redirect proves it is broken.
			name:          "warned endpoint is probed and escalated",
			conv:          &ProbeResult{StatusCode: 302, Message: "redirected to /login"},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			convHealth:    HealthWarn,
			wantConv:      HealthError,
			wantAnalytics: HealthOK,
			wantConvMsgs:  2, // the offline warning, plus the probe verdict
			wantConvMsg:   "is not an Agent Observability API URL",
		},
		{
			// A reachable endpoint does not clear the offline warning either.
			name:          "warned endpoint that answers stays warned",
			conv:          &ProbeResult{StatusCode: 202, OK: true},
			otlp:          &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			convHealth:    HealthWarn,
			wantConv:      HealthWarn,
			wantAnalytics: HealthOK,
			wantConvMsgs:  1, // the offline warning, alone
		},
		{
			// The offline endpoint check already failed, so probing would
			// report the same fault a second and third time. This covers both
			// offline rejections: a malformed endpoint and an app-page path.
			name:            "endpoint already rejected offline is not probed",
			conv:            &ProbeResult{StatusCode: 0, Message: "invalid endpoint: parse …"},
			otlp:            &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200, OK: true}, Traces: &ProbeResult{StatusCode: 200, OK: true}},
			convHealth:      HealthError,
			wantConv:        HealthError,
			wantAnalytics:   HealthOK,
			wantConvSkipped: true,
			wantConvMsgs:    1, // the offline message, alone
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prevConv, prevOTLP := probeConversationsFn, probeOTLPFn
			t.Cleanup(func() { probeConversationsFn, probeOTLPFn = prevConv, prevOTLP })
			probeConversationsFn = func(context.Context, string, envValue, string, bool) *ProbeResult { return tc.conv }
			var gotOTLPTenant envValue
			probeOTLPFn = func(_ context.Context, tenant envValue) *AnalyticsProbe {
				gotOTLPTenant = tenant
				return tc.otlp
			}

			// A case that starts in error reached that verdict offline, so it
			// carries the offline check's message into the probe stage.
			convHealth, convMsgs := tc.convHealth, []string(nil)
			if convHealth == "" {
				convHealth = HealthOK
			} else {
				convMsgs = []string{"AGENTO11Y_ENDPOINT is not a usable endpoint: …"}
			}
			analyticsHealth := tc.analyticsHealth
			if analyticsHealth == "" {
				analyticsHealth = HealthOK
			}
			r := &Report{
				Conversations: ConversationsSection{
					Endpoint: envValue{Set: true, Value: "https://sigil.example.net", Key: tc.convKey},
					TenantID: envValue{Set: true, Value: "t", Key: "AGENTO11Y_AUTH_TENANT_ID"},
					Token:    tokenValue{Set: true},
					Health:   convHealth,
					Messages: convMsgs,
				},
				Analytics: AnalyticsSection{
					Endpoint: envValue{Set: true, Value: "https://otlp.example.net"},
					Health:   analyticsHealth,
				},
			}
			runProbes(context.Background(), r, map[string]string{}, nil)
			if r.Conversations.Health != tc.wantConv {
				t.Fatalf("conversations health = %q, want %q", r.Conversations.Health, tc.wantConv)
			}
			if tc.wantConvMsg != "" && !strings.Contains(strings.Join(r.Conversations.Messages, " "), tc.wantConvMsg) {
				t.Fatalf("conversations messages %v missing %q", r.Conversations.Messages, tc.wantConvMsg)
			}
			if tc.wantConvSkipped {
				if r.Conversations.Probe != nil {
					t.Fatalf("conversations probe ran = %+v, want skipped", r.Conversations.Probe)
				}
			} else if r.Conversations.Probe == nil {
				t.Fatal("conversations probe did not run")
			} else if r.Conversations.Probe.Message != tc.conv.Message {
				// The JSON report prints the probe result as it stands, so the
				// section must not edit the explanation out of it.
				t.Fatalf("probe message = %q, want %q", r.Conversations.Probe.Message, tc.conv.Message)
			}
			if r.Analytics.Health != tc.wantAnalytics {
				t.Fatalf("analytics health = %q, want %q", r.Analytics.Health, tc.wantAnalytics)
			}
			if tc.wantAnalyticsMsg != "" && !strings.Contains(strings.Join(r.Analytics.Messages, " "), tc.wantAnalyticsMsg) {
				t.Fatalf("analytics messages %v missing %q", r.Analytics.Messages, tc.wantAnalyticsMsg)
			}
			if gotOTLPTenant != r.Conversations.TenantID {
				t.Fatalf("OTLP probe tenant = %+v, want %+v", gotOTLPTenant, r.Conversations.TenantID)
			}
			if len(r.Conversations.Messages) != tc.wantConvMsgs {
				t.Fatalf("conversations messages = %v, want %d", r.Conversations.Messages, tc.wantConvMsgs)
			}
			if len(r.Analytics.Messages) != tc.wantOTLPMsgs {
				t.Fatalf("analytics messages = %v, want %d", r.Analytics.Messages, tc.wantOTLPMsgs)
			}
		})
	}
}

func TestOTLPProbeTenant(t *testing.T) {
	tenant := envValue{Set: true, Value: "t", Key: "AGENTO11Y_AUTH_TENANT_ID"}
	tests := []struct {
		name    string
		headers string
		want    envValue
	}{
		{name: "no headers", want: tenant},
		{name: "headers without authorization", headers: "X-Extra=1", want: tenant},
		{
			// An explicit Authorization header replaces the Basic auth
			// built from the tenant id, so the request never reads it.
			name:    "explicit authorization drops the tenant",
			headers: "Authorization=Bearer explicit",
		},
		{name: "authorization with no value is ignored", headers: "Authorization=", want: tenant},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			osEnv := map[string]string{}
			if tc.headers != "" {
				osEnv["OTEL_EXPORTER_OTLP_HEADERS"] = tc.headers
			}
			if got := otlpProbeTenant(tenant, osEnv, nil); got != tc.want {
				t.Fatalf("tenant = %+v, want %+v", got, tc.want)
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

// TestCollectConfig_ContentMode covers the content capture row: the effective
// mode, and which variable supplied it. Doctor resolves the mode from the env
// snapshot so it can attribute the value; the parsing and the fall-back stay in
// envconfig, shared with the hooks.
func TestCollectConfig_ContentMode(t *testing.T) {
	tests := []struct {
		name         string
		osEnv        map[string]string
		fileEnv      map[string]string
		wantMode     string
		wantKey      string
		wantSource   string
		wantFellBack bool
		wantHealth   Health // "" = don't assert
		wantMsg      string // substring of a ConfigSection message
		wantRendered string
	}{
		{
			// Nothing is configured, so the row has to say the value is the
			// built-in one rather than a choice the user made.
			name: "unset uses the built-in default", wantMode: "metadata_only",
			wantRendered: "content capture:  metadata_only (default)",
		},
		{
			// The rejected value did not supply the mode in force, so the row credits
			// the built-in default and the message names the variable to fix. Naming
			// the variable on the row would make it identical to a valid value.
			name: "invalid mode falls back", osEnv: map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "bogus"},
			wantMode: "metadata_only", wantFellBack: true, wantHealth: HealthWarn,
			wantMsg:      "the SIGIL_CONTENT_CAPTURE_MODE value is invalid; using metadata_only",
			wantRendered: "content capture:  metadata_only (default)",
		},
		{
			name: "from the shell", osEnv: map[string]string{"AGENTO11Y_CONTENT_CAPTURE_MODE": "full"},
			wantMode: "full", wantKey: "AGENTO11Y_CONTENT_CAPTURE_MODE", wantSource: sourceEnv,
			wantRendered: "content capture:  full (AGENTO11Y_CONTENT_CAPTURE_MODE, env)",
		},
		{
			name: "from config.env", fileEnv: map[string]string{"AGENTO11Y_CONTENT_CAPTURE_MODE": "no_tool_content"},
			wantMode: "no_tool_content", wantKey: "AGENTO11Y_CONTENT_CAPTURE_MODE", wantSource: sourceConfig,
			wantRendered: "content capture:  no_tool_content (AGENTO11Y_CONTENT_CAPTURE_MODE, config.env)",
		},
		{
			// The shell wins, so the row must not name the config.env value the
			// hooks never see.
			name:  "shell wins over config.env",
			osEnv: map[string]string{"AGENTO11Y_CONTENT_CAPTURE_MODE": "full"},
			fileEnv: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "metadata_only",
			},
			wantMode: "full", wantKey: "AGENTO11Y_CONTENT_CAPTURE_MODE", wantSource: sourceEnv,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)

			sec := collectConfig(tc.osEnv, tc.fileEnv)
			if sec.ContentModeFellBack != tc.wantFellBack {
				t.Fatalf("ContentModeFellBack = %v, want %v", sec.ContentModeFellBack, tc.wantFellBack)
			}
			if sec.ContentCaptureMode != tc.wantMode {
				t.Fatalf("mode = %q, want %q", sec.ContentCaptureMode, tc.wantMode)
			}
			if sec.ContentModeKey != tc.wantKey || sec.ContentModeSource != tc.wantSource {
				t.Fatalf("content mode provenance = %q/%q, want %q/%q",
					sec.ContentModeKey, sec.ContentModeSource, tc.wantKey, tc.wantSource)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			if tc.wantRendered == "" {
				return
			}
			var buf bytes.Buffer
			renderHuman(&buf, &Report{Config: sec}, false)
			if !strings.Contains(buf.String(), tc.wantRendered) {
				t.Fatalf("rendered report missing %q:\n%s", tc.wantRendered, buf.String())
			}
		})
	}
}

// TestCollectConfig_ContentModeReadsTheSnapshot pins the wiring: the mode is
// resolved from the snapshot doctor was handed, not from the process env after
// the dotenv merge. Reading the process env would attribute every config.env
// value to the shell.
func TestCollectConfig_ContentModeReadsTheSnapshot(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")

	sec := collectConfig(nil, map[string]string{"AGENTO11Y_CONTENT_CAPTURE_MODE": "no_tool_content"})
	if sec.ContentCaptureMode != "no_tool_content" || sec.ContentModeSource != sourceConfig {
		t.Fatalf("mode = %q from %q, want no_tool_content from config.env", sec.ContentCaptureMode, sec.ContentModeSource)
	}
}

func TestCollectConfig_RedactInput(t *testing.T) {
	// The family has to be in the snapshot the binary passes in, or a
	// shell-exported opt-out reads as unset here while the hooks act on it.
	if !slices.Contains(trackedSuffixes, "REDACT_INPUT_MESSAGES") {
		t.Fatalf("trackedSuffixes is missing REDACT_INPUT_MESSAGES, so SnapshotEnv would not record it")
	}

	tests := []struct {
		name         string
		raw          string
		wantRedact   bool
		wantFellBack bool
		wantHealth   Health // "" = don't assert
	}{
		{name: "unset redacts", wantRedact: true},
		{name: "explicit true", raw: "true", wantRedact: true},
		{name: "opt-out", raw: "false", wantRedact: false},
		{name: "typo keeps redaction on and warns", raw: "flase", wantRedact: true, wantFellBack: true, wantHealth: HealthWarn},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			osEnv := map[string]string{}
			if tc.raw != "" {
				osEnv["SIGIL_REDACT_INPUT_MESSAGES"] = tc.raw
			}

			sec := collectConfig(osEnv, nil)
			if sec.RedactInput != tc.wantRedact {
				t.Fatalf("RedactInput = %v, want %v", sec.RedactInput, tc.wantRedact)
			}
			if sec.RedactInputFellBack != tc.wantFellBack {
				t.Fatalf("RedactInputFellBack = %v, want %v", sec.RedactInputFellBack, tc.wantFellBack)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
		})
	}
}

func TestCollectConfig_Guards(t *testing.T) {
	// The guard families have to be in the snapshot the binary passes in, or a
	// shell-exported timeout reads as unset here while the hooks act on it.
	for _, suffix := range []string{"GUARDS_ENABLED", "GUARDS_FAIL_OPEN", "GUARDS_TIMEOUT_MS"} {
		if !slices.Contains(trackedSuffixes, suffix) {
			t.Fatalf("trackedSuffixes is missing %s, so SnapshotEnv would not record it", suffix)
		}
	}
	tests := []struct {
		name          string
		osEnv         map[string]string
		fileEnv       map[string]string
		wantEnabled   bool
		wantTimeoutMs int
		wantFailOpen  bool
		wantFellBack  bool
		wantKey       string
		wantSource    string
		wantHealth    Health // "" = don't assert
		wantMsg       string // substring of a ConfigSection message
		wantRendered  string
	}{
		{
			name: "unset uses defaults", wantEnabled: false, wantTimeoutMs: 1500, wantFailOpen: true,
			wantRendered: "guards:           disabled (default)",
		},
		{
			name: "enabled fail-open", osEnv: map[string]string{"AGENTO11Y_GUARDS_ENABLED": "true"},
			wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true,
			wantKey: "AGENTO11Y_GUARDS_ENABLED", wantSource: sourceEnv,
			wantRendered: "guards:           enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)",
		},
		{
			name: "enabled fail-closed with timeout from config.env",
			fileEnv: map[string]string{
				"AGENTO11Y_GUARDS_ENABLED":    "1",
				"AGENTO11Y_GUARDS_TIMEOUT_MS": "500",
				"AGENTO11Y_GUARDS_FAIL_OPEN":  "false",
			},
			wantEnabled: true, wantTimeoutMs: 500, wantFailOpen: false,
			wantKey: "AGENTO11Y_GUARDS_ENABLED", wantSource: sourceConfig,
			wantRendered: "guards:           enabled, timeout 500ms, fail-closed (AGENTO11Y_GUARDS_ENABLED, config.env)",
		},
		{
			name: "legacy spelling", osEnv: map[string]string{"SIGIL_GUARDS_ENABLED": "true"},
			wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true,
			wantKey: "SIGIL_GUARDS_ENABLED", wantSource: sourceEnv,
			wantRendered: "guards:           enabled, timeout 1500ms, fail-open (SIGIL_GUARDS_ENABLED, env)",
		},
		{
			// The rejected value did not decide whether guards run, so the row credits
			// the built-in default and the message names the variable to fix.
			name: "invalid enabled falls back", osEnv: map[string]string{"AGENTO11Y_GUARDS_ENABLED": "maybe"},
			wantEnabled: false, wantTimeoutMs: 1500, wantFailOpen: true, wantFellBack: true,
			wantHealth:   HealthWarn,
			wantMsg:      "the AGENTO11Y_GUARDS_ENABLED value is invalid; guards use the default",
			wantRendered: "guards:           disabled (default)",
		},
		{
			// GUARDS_ENABLED is fine, so the row keeps naming it. Only the message can
			// say the timeout is the broken value.
			name: "invalid timeout falls back",
			osEnv: map[string]string{
				"AGENTO11Y_GUARDS_ENABLED":    "true",
				"AGENTO11Y_GUARDS_TIMEOUT_MS": "-1",
			},
			wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true, wantFellBack: true,
			wantKey: "AGENTO11Y_GUARDS_ENABLED", wantSource: sourceEnv, wantHealth: HealthWarn,
			wantMsg:      "the AGENTO11Y_GUARDS_TIMEOUT_MS value is invalid; guards use the default",
			wantRendered: "guards:           enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, env)",
		},
		{
			// The fail mode is the third family, and it is the one the row never names.
			name: "invalid fail-open falls back",
			fileEnv: map[string]string{
				"AGENTO11Y_GUARDS_ENABLED":   "true",
				"AGENTO11Y_GUARDS_FAIL_OPEN": "fals",
			},
			wantEnabled: true, wantTimeoutMs: 1500, wantFailOpen: true, wantFellBack: true,
			wantKey: "AGENTO11Y_GUARDS_ENABLED", wantSource: sourceConfig, wantHealth: HealthWarn,
			wantMsg:      "the AGENTO11Y_GUARDS_FAIL_OPEN value is invalid; guards use the default",
			wantRendered: "guards:           enabled, timeout 1500ms, fail-open (AGENTO11Y_GUARDS_ENABLED, config.env)",
		},
		{
			// Only GUARDS_ENABLED names the row, and it is unset, so the row
			// reports the default even though a timeout is configured.
			name:    "timeout alone leaves the row on its default",
			fileEnv: map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "800"},
			// TimeoutMs is still resolved; the row just cannot claim a key for it.
			wantEnabled: false, wantTimeoutMs: 800, wantFailOpen: true,
			wantRendered: "guards:           disabled (default)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// collectConfig reads guards from the snapshot alone, but it still calls
			// dotenv.FilePath(), which follows HOME. Without isolateEnv the developer's
			// own config.env decides Path, Exists and DisallowedKeys, which the
			// rendered rows below are matched against.
			isolateEnv(t)

			sec := collectConfig(tc.osEnv, tc.fileEnv)
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
			if sec.GuardsKey != tc.wantKey || sec.GuardsSource != tc.wantSource {
				t.Fatalf("guards provenance = %q/%q, want %q/%q",
					sec.GuardsKey, sec.GuardsSource, tc.wantKey, tc.wantSource)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			if tc.wantRendered == "" {
				return
			}
			var buf bytes.Buffer
			renderHuman(&buf, &Report{Config: sec}, false)
			if !strings.Contains(buf.String(), tc.wantRendered) {
				t.Fatalf("rendered report missing %q:\n%s", tc.wantRendered, buf.String())
			}
		})
	}
}

// TestCollectConfig_GuardsFaultsMatchEnvconfig pins doctor's fault detection to
// envconfig's. collectConfig names the rejected variable itself rather than
// reading the resolver's diagnostics, so without this a rule that changed in
// envconfig alone would leave doctor calling a discarded value good, or naming a
// variable the hooks accepted.
func TestCollectConfig_GuardsFaultsMatchEnvconfig(t *testing.T) {
	envs := []map[string]string{
		nil,
		{"AGENTO11Y_GUARDS_ENABLED": "true"},
		{"AGENTO11Y_GUARDS_ENABLED": "maybe"},
		{"AGENTO11Y_GUARDS_TIMEOUT_MS": "500"},
		{"AGENTO11Y_GUARDS_TIMEOUT_MS": "0"},
		{"SIGIL_GUARDS_TIMEOUT_MS": "abc"},
		{"AGENTO11Y_GUARDS_FAIL_OPEN": "off"},
		{"AGENTO11Y_GUARDS_FAIL_OPEN": "fals"},
		{"AGENTO11Y_GUARDS_ENABLED": "ture", "SIGIL_GUARDS_TIMEOUT_MS": "-3"},
	}
	for _, osEnv := range envs {
		// fmt prints a map in sorted key order, so the subtest name is stable.
		t.Run(fmt.Sprint(osEnv), func(t *testing.T) {
			isolateEnv(t)

			// The resolver's own diagnostics are the reference: it logs one line per
			// value it rejects.
			var buf bytes.Buffer
			envconfig.ResolveGuardsWith(log.New(&buf, "", 0), func(suffix string) (value, key string, ok bool) {
				r := resolveFamily(suffix, osEnv, nil)
				return r.value, r.key, r.set
			})
			logged := buf.String()

			sec := collectConfig(osEnv, nil)
			if want := logged != ""; sec.GuardsFellBack != want {
				t.Fatalf("GuardsFellBack = %v, want %v (envconfig logged %q)", sec.GuardsFellBack, want, logged)
			}
			messages := strings.Join(sec.Messages, " ")
			for key := range osEnv {
				if !strings.Contains(logged, key+"=") {
					continue
				}
				if !strings.Contains(messages, key) {
					t.Fatalf("envconfig rejected %s but messages %v do not name it", key, sec.Messages)
				}
			}
		})
	}
}

// TestCollectConfig_AgentName pins the agent-name row: the effective value, the
// spelling that supplied it, and whether it came from the shell or config.env.
// Without the row a user who renamed an agent has no way to see why their guard
// rule stopped matching.
func TestCollectConfig_AgentName(t *testing.T) {
	tests := []struct {
		name         string
		osEnv        map[string]string
		fileEnv      map[string]string
		wantName     string
		wantKey      string
		wantSource   string
		wantMsg      string
		wantHealth   Health
		wantRendered string // "" = assert the row is absent
	}{
		{name: "unset reports no override"},
		{
			name:     "from the shell",
			osEnv:    map[string]string{"AGENTO11Y_AGENT_NAME": "claude-code-e2e"},
			wantName: "claude-code-e2e", wantKey: "AGENTO11Y_AGENT_NAME", wantSource: sourceEnv,
			wantRendered: "agent name: claude-code-e2e (AGENTO11Y_AGENT_NAME, env)",
		},
		{
			name:     "from config.env",
			fileEnv:  map[string]string{"AGENTO11Y_AGENT_NAME": "config-name"},
			wantName: "config-name", wantKey: "AGENTO11Y_AGENT_NAME", wantSource: sourceConfig,
			wantRendered: "agent name: config-name (AGENTO11Y_AGENT_NAME, config.env)",
		},
		{
			name:     "shell wins over config.env",
			osEnv:    map[string]string{"AGENTO11Y_AGENT_NAME": "shell-name"},
			fileEnv:  map[string]string{"AGENTO11Y_AGENT_NAME": "config-name"},
			wantName: "shell-name", wantKey: "AGENTO11Y_AGENT_NAME", wantSource: sourceEnv,
		},
		{
			name:     "legacy-only name suggests migration",
			osEnv:    map[string]string{"SIGIL_AGENT_NAME": "legacy-name"},
			wantName: "legacy-name", wantKey: "SIGIL_AGENT_NAME", wantSource: sourceEnv,
			wantMsg: "preferred name is AGENTO11Y_AGENT_NAME",
		},
		{
			name: "conflicting spellings flag the disagreement",
			osEnv: map[string]string{
				"AGENTO11Y_AGENT_NAME": "preferred-name",
				"SIGIL_AGENT_NAME":     "legacy-name",
			},
			wantName: "preferred-name", wantKey: "AGENTO11Y_AGENT_NAME", wantSource: sourceEnv,
			wantMsg: "AGENTO11Y_AGENT_NAME and SIGIL_AGENT_NAME are both set in the environment, to different values; using AGENTO11Y_AGENT_NAME",
		},
		{
			name:     "a slash in the name warns that the run reads as a subagent",
			osEnv:    map[string]string{"AGENTO11Y_AGENT_NAME": "team/e2e"},
			wantName: "team/e2e", wantKey: "AGENTO11Y_AGENT_NAME", wantSource: sourceEnv,
			wantMsg:    "contains a slash",
			wantHealth: HealthWarn,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			sec := collectConfig(tc.osEnv, tc.fileEnv)
			if sec.AgentName != tc.wantName {
				t.Fatalf("agent name = %q, want %q", sec.AgentName, tc.wantName)
			}
			if sec.AgentNameKey != tc.wantKey {
				t.Fatalf("agent name key = %q, want %q", sec.AgentNameKey, tc.wantKey)
			}
			if sec.AgentNameSource != tc.wantSource {
				t.Fatalf("agent name source = %q, want %q", sec.AgentNameSource, tc.wantSource)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			if tc.wantHealth != "" && sec.Health != tc.wantHealth {
				t.Fatalf("health = %q, want %q", sec.Health, tc.wantHealth)
			}
			var buf bytes.Buffer
			renderHuman(&buf, &Report{Config: sec}, false)
			// The renderer pads keys to the width of the widest one in the
			// section, so compare with runs of spaces collapsed.
			rendered := strings.Join(strings.Fields(buf.String()), " ")
			if tc.wantRendered != "" && !strings.Contains(rendered, tc.wantRendered) {
				t.Fatalf("rendered report missing %q:\n%s", tc.wantRendered, buf.String())
			}
			if tc.wantName == "" && strings.Contains(rendered, "agent name") {
				t.Fatalf("rendered report names an agent with no override set:\n%s", buf.String())
			}
		})
	}
}

// TestCollectConfig_AgentNameReadsTheSnapshot pins the same wiring the content
// mode has: the name is resolved from the snapshot doctor was handed, not from
// the process env after the dotenv merge, so a config.env value is not
// attributed to the shell.
func TestCollectConfig_AgentNameReadsTheSnapshot(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTO11Y_AGENT_NAME", "shell-name")

	sec := collectConfig(nil, map[string]string{"AGENTO11Y_AGENT_NAME": "config-name"})
	if sec.AgentName != "config-name" || sec.AgentNameSource != sourceConfig {
		t.Fatalf("agent name = %q from %q, want config-name from config.env", sec.AgentName, sec.AgentNameSource)
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
			wantMsg: "AGENTO11Y_TAGS and SIGIL_TAGS are both set in the environment, to different values; using AGENTO11Y_TAGS. " +
				"SIGIL_TAGS is the old name for the same setting: unset it.",
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

// TestCollectConfig_AutoTags covers the automatic-tag row: the enabled names,
// the values they resolve to, how the allowlist narrows the switch, and the
// cases the row cannot show on its own (unsupported name, no value here,
// explicit tag wins, allowlist without the switch). The repository and branch
// resolve from the directory the test runs in, so the cases here use `user`,
// whose value comes from the same snapshot.
func TestCollectConfig_AutoTags(t *testing.T) {
	tests := []struct {
		name            string
		osEnv           map[string]string
		fileEnv         map[string]string
		wantNames       []string
		wantTags        map[string]string
		wantUnresolved  []string
		wantShadowed    []string
		wantUnknown     []string
		wantSource      string
		wantNamesSource string
		wantMsg         string
		wantNoMsg       string
	}{
		{name: "switch unset resolves nothing"},
		{
			name:  "switch blank resolves nothing",
			osEnv: map[string]string{"AGENTO11Y_AUTO_CODING_AGENT_TAGS": "   "},
		},
		{
			name:  "switch off resolves nothing",
			osEnv: map[string]string{"AGENTO11Y_AUTO_CODING_AGENT_TAGS": "false"},
		},
		{
			// Outside a checkout `repo` and `branch` have no value, so the switch on
			// its own reports three enabled names and one tag.
			name: "switch on takes every name",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS": "true",
				"AGENTO11Y_USER_ID":                "alice@example.com",
			},
			wantNames:      []string{"user", "repo", "branch"},
			wantTags:       map[string]string{"user": "alice@example.com"},
			wantUnresolved: []string{"repo", "branch"},
			wantSource:     sourceEnv,
			wantMsg:        "these auto tags resolved no value in this directory and are left off: repo, branch",
		},
		{
			name: "the allowlist narrows the switch",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user",
				"AGENTO11Y_USER_ID":                      "alice@example.com",
			},
			wantNames:       []string{"user"},
			wantTags:        map[string]string{"user": "alice@example.com"},
			wantSource:      sourceEnv,
			wantNamesSource: sourceEnv,
		},
		{
			name: "from config.env when the environment is unset",
			fileEnv: map[string]string{
				"SIGIL_AUTO_CODING_AGENT_TAGS":       "true",
				"SIGIL_AUTO_CODING_AGENT_TAGS_NAMES": "user",
				"SIGIL_USER_ID":                      "alice@example.com",
			},
			wantNames:       []string{"user"},
			wantTags:        map[string]string{"user": "alice@example.com"},
			wantSource:      sourceConfig,
			wantNamesSource: sourceConfig,
		},
		{
			// Without USER_ID the value depends on which agent runs the session,
			// so the row shows a placeholder rather than the account name doctor
			// itself runs under.
			name: "the user value is left to the agent when USER_ID is unset",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user",
			},
			wantNames:       []string{"user"},
			wantTags:        map[string]string{"user": agentUserPlaceholder},
			wantSource:      sourceEnv,
			wantNamesSource: sourceEnv,
			wantMsg:         "the auto tag user depends on the coding agent",
		},
		{
			// An explicit tag supplies the value, so the agent never gets a say and
			// the placeholder message stays off.
			name: "an explicit user tag leaves the agent nothing to decide",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user",
				"AGENTO11Y_TAGS":                         "user=team-account",
			},
			wantNames:       []string{"user"},
			wantShadowed:    []string{"user"},
			wantSource:      sourceEnv,
			wantNamesSource: sourceEnv,
			wantNoMsg:       "the auto tag user depends on the coding agent",
		},
		{
			name: "an explicit tag wins over the resolved value",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user",
				"AGENTO11Y_USER_ID":                      "alice@example.com",
				"AGENTO11Y_TAGS":                         "user=team-account",
			},
			wantNames:       []string{"user"},
			wantShadowed:    []string{"user"},
			wantSource:      sourceEnv,
			wantNamesSource: sourceEnv,
			wantMsg:         "these auto tags are also set in AGENTO11Y_TAGS, which wins: user",
		},
		{
			name: "an unsupported name is reported and the rest still resolve",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user,team",
				"AGENTO11Y_USER_ID":                      "alice@example.com",
			},
			wantNames:       []string{"user"},
			wantTags:        map[string]string{"user": "alice@example.com"},
			wantUnknown:     []string{"team"},
			wantSource:      sourceEnv,
			wantNamesSource: sourceEnv,
			wantMsg:         "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES has unsupported names team; supported: user, repo, branch, all",
		},
		{
			name: "only unsupported names leave the row off",
			osEnv: map[string]string{
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
				"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "team",
			},
			wantUnknown: []string{"team"},
			wantMsg:     "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES names no supported value, so no automatic tags are attached",
		},
		{
			name:    "the allowlist alone attaches nothing",
			osEnv:   map[string]string{"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "user"},
			wantMsg: "AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES is set but AGENTO11Y_AUTO_CODING_AGENT_TAGS is off, so no automatic tags are attached",
		},
		{
			name:    "a list in the switch is not a boolean",
			osEnv:   map[string]string{"AGENTO11Y_AUTO_CODING_AGENT_TAGS": "user,repo"},
			wantMsg: "the AGENTO11Y_AUTO_CODING_AGENT_TAGS value is not a boolean; automatic tags stay off, and the names go in AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			// The repository and branch would resolve from the working directory,
			// which is this package's source tree. Running in an empty directory
			// keeps the cases above independent of the checkout.
			t.Chdir(t.TempDir())
			sec := collectConfig(tc.osEnv, tc.fileEnv)

			if !slices.Equal(sec.AutoTagNames, tc.wantNames) {
				t.Errorf("auto tag names = %v, want %v", sec.AutoTagNames, tc.wantNames)
			}
			if !maps.Equal(sec.AutoTags, tc.wantTags) {
				t.Errorf("auto tags = %v, want %v", sec.AutoTags, tc.wantTags)
			}
			if !slices.Equal(sec.AutoTagUnresolved, tc.wantUnresolved) {
				t.Errorf("unresolved = %v, want %v", sec.AutoTagUnresolved, tc.wantUnresolved)
			}
			if !slices.Equal(sec.AutoTagShadowed, tc.wantShadowed) {
				t.Errorf("shadowed = %v, want %v", sec.AutoTagShadowed, tc.wantShadowed)
			}
			if !slices.Equal(sec.AutoTagUnknown, tc.wantUnknown) {
				t.Errorf("unknown = %v, want %v", sec.AutoTagUnknown, tc.wantUnknown)
			}
			if sec.AutoTagsSource != tc.wantSource {
				t.Errorf("auto tags source = %q, want %q", sec.AutoTagsSource, tc.wantSource)
			}
			if sec.AutoTagNamesSource != tc.wantNamesSource {
				t.Errorf("auto tag names source = %q, want %q", sec.AutoTagNamesSource, tc.wantNamesSource)
			}
			if tc.wantMsg != "" && !strings.Contains(strings.Join(sec.Messages, " "), tc.wantMsg) {
				t.Errorf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			if tc.wantNoMsg != "" && strings.Contains(strings.Join(sec.Messages, " "), tc.wantNoMsg) {
				t.Errorf("messages %v should not contain %q", sec.Messages, tc.wantNoMsg)
			}
		})
	}
}

// TestCollectConfig_AutoTagsResolveFromTheCheckout pins the git side of the
// row: with `repo` and `branch` enabled, doctor reports what the current
// checkout resolves to, which is what the hooks would attach.
func TestCollectConfig_AutoTagsResolveFromTheCheckout(t *testing.T) {
	isolateEnv(t)
	root := t.TempDir()
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The two files gitbranch reads. No `git` binary needed, matching
	// internal/gitbranch's fixtures.
	if err := os.WriteFile(filepath.Join(gitDir, "config"),
		[]byte("[remote \"origin\"]\n\turl = git@github.com:grafana/agento11y.git\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"),
		[]byte("ref: refs/heads/feature/auto-tags\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	sec := collectConfig(map[string]string{
		"AGENTO11Y_AUTO_CODING_AGENT_TAGS":       "true",
		"AGENTO11Y_AUTO_CODING_AGENT_TAGS_NAMES": "repo,branch",
	}, nil)
	want := map[string]string{"repo": "grafana/agento11y", "git.branch": "feature/auto-tags"}
	if !maps.Equal(sec.AutoTags, want) {
		t.Fatalf("auto tags = %v, want %v", sec.AutoTags, want)
	}
}

// TestCollectConfig_Local covers the LOCAL row: doctor reports the value the
// launcher and hooks act on (shell before config.env), renders it, warns when
// the value is not a boolean, and states that local mode covers launches and
// hooks.
func TestCollectConfig_Local(t *testing.T) {
	// The table below builds osEnv by hand, so it cannot catch a family missing
	// from the snapshot the binary actually passes in. Without the entry, a
	// shell-exported AGENTO11Y_LOCAL reads as unset here while the launcher acts
	// on it.
	if !slices.Contains(trackedSuffixes, "LOCAL") {
		t.Fatal("LOCAL must be in trackedSuffixes or SnapshotEnv drops it and this report reads it as unset")
	}
	const scopeMsg = "local mode sends `agento11y <agent>` launches and agento11y hooks to the local viewer"
	const launcherOnlyMsg = "local capture covers `agento11y <agent>` launches."
	const invalidMsg = "the AGENTO11Y_LOCAL value is not a boolean, so it is ignored; the capture row states the destination in force"
	tests := []struct {
		name            string
		osEnv           map[string]string
		fileEnv         map[string]string
		wantValue       string // "" means the family is unset
		wantSource      string
		wantInvalid     bool
		wantHealth      Health
		wantScopeMsg    bool
		wantMsg         string // substring of a ConfigSection message
		wantRendered    []string
		wantNotRendered []string
	}{
		{
			// Capture falls back to local here, and that covers launches only, so
			// the narrower caveat replaces the scope message.
			name:            "unset",
			wantHealth:      HealthOK,
			wantMsg:         launcherOnlyMsg,
			wantNotRendered: []string{"local mode"},
		},
		{
			name:            "cloud credentials send launches to cloud",
			osEnv:           map[string]string{"AGENTO11Y_ENDPOINT": "https://example.net", "AGENTO11Y_AUTH_TENANT_ID": "12345", "AGENTO11Y_AUTH_TOKEN": "glc_secret"},
			wantHealth:      HealthOK,
			wantRendered:    []string{"capture:          Grafana Cloud (credentials configured)"},
			wantNotRendered: []string{launcherOnlyMsg},
		},
		{
			// The pin covers hooks too, so the launcher-only caveat must not fire.
			name:            "from config.env",
			fileEnv:         map[string]string{"AGENTO11Y_LOCAL": "true"},
			wantValue:       "true",
			wantSource:      sourceConfig,
			wantHealth:      HealthOK,
			wantScopeMsg:    true,
			wantRendered:    []string{"local mode", "local (AGENTO11Y_LOCAL=true, config.env)"},
			wantNotRendered: []string{launcherOnlyMsg},
		},
		{
			name:         "from env",
			osEnv:        map[string]string{"AGENTO11Y_LOCAL": "true"},
			wantValue:    "true",
			wantSource:   sourceEnv,
			wantHealth:   HealthOK,
			wantScopeMsg: true,
			wantRendered: []string{"local mode", "local (AGENTO11Y_LOCAL=true, env)"},
		},
		{
			name:         "legacy spelling",
			osEnv:        map[string]string{"SIGIL_LOCAL": "on"},
			wantValue:    "on",
			wantSource:   sourceEnv,
			wantHealth:   HealthOK,
			wantScopeMsg: true,
			wantMsg:      "the preferred name is AGENTO11Y_LOCAL",
			wantRendered: []string{"local (SIGIL_LOCAL=on, env)"},
		},
		{
			// The one-off Cloud session: the shell value is what the launcher
			// acts on, so it is what doctor must report.
			name:            "shell false overrides config.env true",
			osEnv:           map[string]string{"AGENTO11Y_LOCAL": "false"},
			fileEnv:         map[string]string{"AGENTO11Y_LOCAL": "true"},
			wantValue:       "false",
			wantSource:      sourceEnv,
			wantHealth:      HealthOK,
			wantRendered:    []string{"Grafana Cloud (AGENTO11Y_LOCAL=false, env)"},
			wantNotRendered: []string{"invalid value"},
		},
		{
			// The launcher ignores a value outside the boolean whitelist, so the row
			// states the mode in force and keeps the rejected value in its one
			// trailer. Printing "enabled" as the state would confirm the wrong belief.
			name:            "value outside the boolean whitelist",
			osEnv:           map[string]string{"AGENTO11Y_LOCAL": "enabled"},
			wantValue:       "enabled",
			wantSource:      sourceEnv,
			wantInvalid:     true,
			wantHealth:      HealthWarn,
			wantMsg:         invalidMsg,
			wantRendered:    []string{`capture:          local (default; no Cloud credentials configured)`},
			wantNotRendered: []string{"enabled (AGENTO11Y_LOCAL, env)", "invalid value, local mode is off"},
		},
		{
			// Skipping the rejected value leaves the credentials rule to pick the
			// destination, so the message must not claim the default applied.
			name: "value outside the whitelist with cloud credentials",
			osEnv: map[string]string{
				"AGENTO11Y_LOCAL": "enabled", "AGENTO11Y_ENDPOINT": "https://example.net",
				"AGENTO11Y_AUTH_TENANT_ID": "12345", "AGENTO11Y_AUTH_TOKEN": "glc_secret",
			},
			wantValue:       "enabled",
			wantSource:      sourceEnv,
			wantInvalid:     true,
			wantHealth:      HealthWarn,
			wantMsg:         invalidMsg,
			wantRendered:    []string{"capture:          Grafana Cloud (credentials configured)"},
			wantNotRendered: []string{"default applies", launcherOnlyMsg},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			sec := collectConfig(tc.osEnv, tc.fileEnv)
			if sec.Local.Set != (tc.wantValue != "") {
				t.Fatalf("Local.Set = %v, want %v", sec.Local.Set, tc.wantValue != "")
			}
			if sec.Local.Value != tc.wantValue {
				t.Fatalf("Local.Value = %q, want %q", sec.Local.Value, tc.wantValue)
			}
			if sec.Local.Source != tc.wantSource {
				t.Fatalf("Local.Source = %q, want %q", sec.Local.Source, tc.wantSource)
			}
			if sec.LocalInvalid != tc.wantInvalid {
				t.Fatalf("LocalInvalid = %v, want %v", sec.LocalInvalid, tc.wantInvalid)
			}
			if sec.Health != tc.wantHealth {
				t.Fatalf("Health = %q, want %q (messages %v)", sec.Health, tc.wantHealth, sec.Messages)
			}

			joined := strings.Join(sec.Messages, " ")
			if tc.wantMsg != "" && !strings.Contains(joined, tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
			if got := strings.Contains(joined, scopeMsg); got != tc.wantScopeMsg {
				t.Fatalf("scope message present = %v, want %v (messages %v)", got, tc.wantScopeMsg, sec.Messages)
			}

			var buf bytes.Buffer
			renderHuman(&buf, &Report{Config: sec}, false)
			rendered := buf.String()
			for _, want := range tc.wantRendered {
				if !strings.Contains(rendered, want) {
					t.Fatalf("rendered report missing %q:\n%s", want, rendered)
				}
			}
			for _, none := range tc.wantNotRendered {
				if strings.Contains(rendered, none) {
					t.Fatalf("rendered report contains %q:\n%s", none, rendered)
				}
			}
		})
	}
}

// TestCollect_LocalModeScopeReachesTheReport pins the wiring: the scope message
// has to survive Collect, not just collectConfig, or the whole warning is
// invisible in the command a user actually runs.
func TestCollect_LocalModeScopeReachesTheReport(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	writeConfig(t, "AGENTO11Y_LOCAL=true\n")

	r := Collect(context.Background(), Options{}, Params{Version: "1.2.3"})
	if !r.Config.Local.Set || r.Config.Local.Value != "true" {
		t.Fatalf("Config.Local = %+v, want the config.env value", r.Config.Local)
	}
	joined := strings.Join(r.Config.Messages, " ")
	if !strings.Contains(joined, "local mode sends `agento11y <agent>` launches and agento11y hooks to the local viewer") {
		t.Fatalf("report messages %v missing the local-mode scope message", r.Config.Messages)
	}
}

// TestCollectConfig_LocalForward covers the LOCAL_FORWARD attribution: doctor
// reports shell-first like every other family, and calls out the case where the
// local daemon (which prefers config.env) would use the other value.
func TestCollectConfig_LocalForward(t *testing.T) {
	tests := []struct {
		name       string
		osEnv      map[string]string
		fileEnv    map[string]string
		wantSet    bool
		wantValue  string
		wantSource string
		wantMsg    string
	}{
		{name: "unset"},
		{
			name:      "from config.env",
			fileEnv:   map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true"},
			wantSet:   true,
			wantValue: "true", wantSource: sourceConfig,
		},
		{
			name:      "from env",
			osEnv:     map[string]string{"SIGIL_LOCAL_FORWARD": "true"},
			wantSet:   true,
			wantValue: "true", wantSource: sourceEnv,
		},
		{
			// The daemon prefers config.env so it forwards nothing, while
			// doctor's shell-first precedence reports true.
			name:      "env and config.env disagree",
			osEnv:     map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true"},
			fileEnv:   map[string]string{"AGENTO11Y_LOCAL_FORWARD": "false"},
			wantSet:   true,
			wantValue: "true", wantSource: sourceEnv,
			wantMsg: "the local daemon uses the config.env value",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// collectConfig resolves LOCAL_FORWARD from its arguments, so the
			// table needs no exported values.
			isolateEnv(t)
			sec := collectConfig(tc.osEnv, tc.fileEnv)
			if sec.LocalForward.Set != tc.wantSet {
				t.Fatalf("LocalForward.Set = %v, want %v", sec.LocalForward.Set, tc.wantSet)
			}
			if sec.LocalForward.Value != tc.wantValue {
				t.Fatalf("LocalForward.Value = %q, want %q", sec.LocalForward.Value, tc.wantValue)
			}
			if sec.LocalForward.Source != tc.wantSource {
				t.Fatalf("LocalForward.Source = %q, want %q", sec.LocalForward.Source, tc.wantSource)
			}
			joined := strings.Join(sec.Messages, " ")
			if tc.wantMsg == "" {
				if strings.Contains(joined, "LOCAL_FORWARD") {
					t.Fatalf("unexpected LOCAL_FORWARD message: %v", sec.Messages)
				}
				return
			}
			if !strings.Contains(joined, tc.wantMsg) {
				t.Fatalf("messages %v missing %q", sec.Messages, tc.wantMsg)
			}
		})
	}
}

// TestCollectConfig_LocalHookForward covers the derived line: whether a
// --local session's guard checks reach Cloud. It is the combination of four
// separately reported settings, and it is the one that decides whether tool
// calls leave the machine under a reduced capture mode.
func TestCollectConfig_LocalHookForward(t *testing.T) {
	const cloud = "https://cloud.example.test"
	// The table below builds osEnv by hand, so it cannot catch a family
	// missing from the snapshot the binary actually passes in. Without this,
	// a shell-exported guard toggle is invisible to doctor while the daemon
	// acts on it.
	for _, suffix := range []string{"LOCAL_FORWARD", "GUARDS_ENABLED", "ENDPOINT", "AUTH_TENANT_ID", "AUTH_TOKEN"} {
		if !slices.Contains(trackedSuffixes, suffix) {
			t.Fatalf("%s must be in trackedSuffixes or SnapshotEnv drops it and this report reads it as unset", suffix)
		}
	}
	cloudCreds := map[string]string{
		"AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	}
	tests := []struct {
		name        string
		env         map[string]string // shell
		fileEnv     map[string]string // config.env
		wantEnabled bool
		wantReason  string // substring
		wantMessage string // substring of a ConfigSection message; "" means none
	}{
		{
			name: "forward and guards on with credentials",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true",
				"AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
			},
			wantEnabled: true,
		},
		{
			name: "forwarding off",
			env: map[string]string{
				"AGENTO11Y_GUARDS_ENABLED": "true",
				"AGENTO11Y_ENDPOINT":       cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
			},
			wantReason: "LOCAL_FORWARD",
		},
		{
			name: "guards off",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true",
				"AGENTO11Y_ENDPOINT":      cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
			},
			wantReason: "GUARDS_ENABLED",
		},
		{
			name: "local endpoint",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true",
				"AGENTO11Y_ENDPOINT": "http://127.0.0.1:8765", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
			},
			wantReason: "is local",
		},
		{
			name: "placeholder credentials",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true",
				"AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "local", "AGENTO11Y_AUTH_TOKEN": "local",
			},
			wantReason: "placeholder",
		},
		{
			name: "missing credentials",
			env: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true",
				"AGENTO11Y_ENDPOINT": cloud,
			},
			wantReason: "Cloud credentials",
		},
		{
			// The daemon prefers config.env over its own environment, so a
			// config.env that turns guards on is what it acts on. Reporting
			// the shell's answer here would print "not forwarded" while every
			// tool call is being sent to Cloud.
			name:        "config.env enables what the shell disables",
			env:         mergeEnv(cloudCreds, map[string]string{"AGENTO11Y_GUARDS_ENABLED": "false"}),
			fileEnv:     map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true"},
			wantEnabled: true,
			wantMessage: "disagree about local guard chaining",
		},
		{
			name:        "config.env disables what the shell enables",
			env:         mergeEnv(cloudCreds, map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true"}),
			fileEnv:     map[string]string{"AGENTO11Y_GUARDS_ENABLED": "false"},
			wantReason:  "GUARDS_ENABLED",
			wantMessage: "disagree about local guard chaining",
		},
		{
			// Agreeing sources must not produce the warning.
			name:        "both sources agree",
			env:         mergeEnv(cloudCreds, map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_GUARDS_ENABLED": "true"}),
			fileEnv:     map[string]string{"AGENTO11Y_GUARDS_ENABLED": "true"},
			wantEnabled: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)

			sec := collectConfig(tc.env, tc.fileEnv)
			if tc.wantMessage == "" {
				for _, m := range sec.Messages {
					if strings.Contains(m, "disagree about local guard chaining") {
						t.Fatalf("unexpected precedence warning: %q", m)
					}
				}
			} else if !slices.ContainsFunc(sec.Messages, func(m string) bool { return strings.Contains(m, tc.wantMessage) }) {
				t.Fatalf("messages = %q, want one containing %q", sec.Messages, tc.wantMessage)
			}
			if sec.LocalHookForward.Enabled != tc.wantEnabled {
				t.Fatalf("LocalHookForward.Enabled = %v, want %v (reason %q)", sec.LocalHookForward.Enabled, tc.wantEnabled, sec.LocalHookForward.Reason)
			}
			if tc.wantReason == "" {
				if sec.LocalHookForward.Reason != "" {
					t.Fatalf("Reason = %q, want empty", sec.LocalHookForward.Reason)
				}
			} else if !strings.Contains(sec.LocalHookForward.Reason, tc.wantReason) {
				t.Fatalf("Reason = %q, want substring %q", sec.LocalHookForward.Reason, tc.wantReason)
			}

			// The rendered report must state the Cloud reach only when it is
			// real, and must give the reason otherwise.
			var buf bytes.Buffer
			renderHuman(&buf, &Report{Config: sec}, false)
			rendered := buf.String()
			if tc.wantEnabled {
				if !strings.Contains(rendered, "forwarded to Cloud") {
					t.Fatalf("rendered report missing the Cloud reach line:\n%s", rendered)
				}
				return
			}
			if strings.Contains(rendered, "forwarded to Cloud") {
				t.Fatalf("rendered report claims Cloud reach when the leg is off:\n%s", rendered)
			}
			if sec.LocalForward.Set && !strings.Contains(rendered, tc.wantReason) {
				t.Fatalf("rendered report missing reason %q:\n%s", tc.wantReason, rendered)
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
				for _, key := range []string{"agento11y", "config", "conversations", "analytics", "agents"} {
					if _, ok := report[key]; !ok {
						t.Fatalf("JSON missing section %q", key)
					}
				}
			},
		},
		{
			// Support reads this shape. Each reported value carries the winning key
			// and source, one field per fact, and never the token itself.
			name: "json carries provenance for every reported value",
			args: []string{"--json"},
			osEnv: mergeEnv(healthy, map[string]string{
				"AGENTO11Y_AUTH_TOKEN":           "glc_secret",
				"AGENTO11Y_TAGS":                 "env=prod",
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
				"AGENTO11Y_GUARDS_ENABLED":       "true",
				"AGENTO11Y_AUTO_UPDATE":          "false",
			}),
			stub:     true,
			wantCode: 0,
			check: func(t *testing.T, stdout string) {
				if strings.Contains(stdout, "glc_secret") {
					t.Fatalf("token value leaked into JSON output:\n%s", stdout)
				}
				// endpoint.key already carries the endpoint variable name, so the
				// second field for the same fact is gone.
				if strings.Contains(stdout, "endpoint_var") {
					t.Fatalf("endpoint_var is still in the JSON contract:\n%s", stdout)
				}
				var report Report
				if err := json.Unmarshal([]byte(stdout), &report); err != nil {
					t.Fatalf("invalid JSON: %v", err)
				}
				for _, field := range []struct{ name, got, want string }{
					{"analytics.endpoint.key", report.Analytics.Endpoint.Key, "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"},
					{"config.tags_key", report.Config.TagsKey, "AGENTO11Y_TAGS"},
					{"config.content_capture_key", report.Config.ContentModeKey, "AGENTO11Y_CONTENT_CAPTURE_MODE"},
					{"config.content_capture_source", report.Config.ContentModeSource, sourceEnv},
					{"config.guards_key", report.Config.GuardsKey, "AGENTO11Y_GUARDS_ENABLED"},
					{"config.guards_source", report.Config.GuardsSource, sourceEnv},
					{"auto_update.key", report.AutoUpdate.Key, "AGENTO11Y_AUTO_UPDATE"},
				} {
					if field.got != field.want {
						t.Errorf("%s = %q, want %q", field.name, field.got, field.want)
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
