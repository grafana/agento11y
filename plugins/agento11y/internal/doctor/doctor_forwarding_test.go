package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

const extraForwarding = `[[destinations]]
name = "archive"
endpoint = "https://archive.example.test"
tenant_id = "extra-tenant-private"
token = "extra-token-private"
otlp_endpoint = "https://collector.example.test/otlp"
otlp_token = "extra-otlp-private"
otlp_headers = "Authorization=Bearer extra-header-private,X-Custom=custom-private"
`

func writeForwarding(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(filepath.Dir(dotenv.FilePath()), "forwarding.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertForwardingSecretsAbsent(t *testing.T, text string) {
	t.Helper()
	for _, secret := range []string{"extra-tenant-private", "extra-token-private", "extra-otlp-private", "extra-header-private", "custom-private", "url-user-private", "url-password-private", "url-query-private", "url-fragment-private", "body-secret-private"} {
		if strings.Contains(text, secret) {
			t.Fatal("forwarding diagnostic exposed a credential or response body")
		}
	}
}

func forwardingOutput(t *testing.T, sec *LocalForwardingSection) string {
	t.Helper()
	r := goldenMinimalReport()
	r.LocalForwarding = sec
	var human, machine bytes.Buffer
	renderHuman(&human, r, false)
	if err := renderJSON(&machine, r); err != nil {
		t.Fatal(err)
	}
	assertForwardingSecretsAbsent(t, human.String()+machine.String())
	return human.String()
}

type forwardingTransport func(*http.Request) (*http.Response, error)

func (f forwardingTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func guardForwardingRequests(t *testing.T, allowed string) {
	t.Helper()
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	http.DefaultTransport = forwardingTransport(func(req *http.Request) (*http.Response, error) {
		if allowed == "" || req.URL.Scheme+"://"+req.URL.Host != allowed || req.Method != http.MethodGet || req.URL.Path != "/api/v1/config" {
			t.Error("doctor attempted an extra endpoint probe or non-read-only request")
			return nil, fmt.Errorf("unexpected forwarding request")
		}
		return previous.RoundTrip(req)
	})
}

func TestLocalForwardingInspection(t *testing.T) {
	for _, tc := range []struct {
		name, content    string
		missing, invalid bool
		want             int
	}{
		{name: "missing", missing: true},
		{name: "empty"},
		{name: "extra only", content: extraForwarding, want: 1},
		{name: "otlp only", content: "[[destinations]]\nname='metrics'\notlp_endpoint='https://collector.example.test'\n", want: 1},
		{name: "primary reserved", content: strings.Replace(extraForwarding, "archive", "primary", 1), invalid: true},
		{name: "duplicate name", content: extraForwarding + extraForwarding, invalid: true},
		{name: "too many", content: "[[destinations]]\nname='a'\notlp_endpoint='https://a.test'\n[[destinations]]\nname='b'\notlp_endpoint='https://b.test'\n[[destinations]]\nname='c'\notlp_endpoint='https://c.test'\n[[destinations]]\nname='d'\notlp_endpoint='https://d.test'\n", invalid: true},
		{name: "malformed TOML", content: "token = 'extra-token-private\n", invalid: true},
		{name: "unsupported field", content: extraForwarding + "password='extra-token-private'\n", invalid: true},
		{name: "missing credentials", content: "[[destinations]]\nname='archive'\nendpoint='https://archive.example.test'\n", invalid: true},
		{name: "userinfo", content: strings.Replace(extraForwarding, "https://archive.example.test", "https://url-user-private:url-password-private@archive.example.test", 1), invalid: true},
		{name: "query", content: strings.Replace(extraForwarding, "https://archive.example.test", "https://archive.example.test?token=url-query-private", 1), invalid: true},
		{name: "fragment", content: strings.Replace(extraForwarding, "https://archive.example.test", "https://archive.example.test#url-fragment-private", 1), invalid: true},
		{name: "loopback OTLP", content: strings.Replace(extraForwarding, "https://collector.example.test/otlp", "http://127.0.0.1:8765/otlp", 1), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			guardForwardingRequests(t, "")
			if !tc.missing {
				writeForwarding(t, tc.content)
			}
			probeConversationsFn = func(context.Context, string, envValue, string, bool, string) *ProbeResult {
				t.Fatal("extra destination was probed as the primary endpoint")
				return nil
			}
			probeOTLPFn = func(context.Context, envValue) *AnalyticsProbe {
				t.Fatal("extra OTLP destination was probed")
				return nil
			}
			r := Collect(t.Context(), Options{}, Params{})
			sec := r.LocalForwarding
			if sec == nil || (sec.Error != "") != tc.invalid || len(sec.Destinations) != tc.want {
				t.Fatal("unexpected forwarding inspection")
			}
			if sec.Path != filepath.Join(filepath.Dir(r.Config.Path), "forwarding.toml") || sec.Delivery != "not_checked" || sec.ForwardStatus != nil {
				t.Fatal("wrong forwarding path or unavailable daemon reported a posture")
			}
			if sec.Health == HealthError || r.exitCode() != 0 || r.cloudReady() || r.exitCodeFor(Options{RequireCloud: true}) != 1 {
				t.Fatal("extra destinations changed primary pipeline semantics")
			}
			text := forwardingOutput(t, sec)
			if !strings.Contains(text, "Local forwarding") || !strings.Contains(text, "not checked") {
				t.Fatal("missing forwarding or delivery diagnostic")
			}
			if tc.want > 0 && !strings.Contains(text, sec.Destinations[0].Name) {
				t.Fatal("extra-only destination is not visible")
			}
			if !tc.missing {
				data, err := os.ReadFile(sec.Path)
				if err != nil || string(data) != tc.content {
					t.Fatal("doctor modified forwarding.toml")
				}
			}
		})
	}
}

func TestLocalForwardingUnreadableFile(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	path := filepath.Join(filepath.Dir(dotenv.FilePath()), "forwarding.toml")
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	r := Collect(t.Context(), Options{}, Params{})
	if r.LocalForwarding.Error == "" || r.LocalForwarding.Health != HealthWarn || r.exitCode() != 0 {
		t.Fatal("unreadable extra file did not remain an advisory diagnostic")
	}
	forwardingOutput(t, r.LocalForwarding)
}

func TestLocalForwardingUsesResolvedConfigPath(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	writeConfigApp(t, "sigil", "SIGIL_LOCAL_FORWARD=false\n")
	path := writeForwarding(t, extraForwarding)
	r := Collect(t.Context(), Options{}, Params{})
	if r.LocalForwarding.Path != path || filepath.Dir(path) != filepath.Dir(r.Config.Path) || len(r.LocalForwarding.Destinations) != 1 {
		t.Fatal("forwarding inspection did not follow the resolved legacy config path")
	}
	forwardingOutput(t, r.LocalForwarding)
}

func TestLocalForwardingInvalidFileDoesNotOverrideCloud(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	guardForwardingRequests(t, "")
	writeConfig(t, "AGENTO11Y_LOCAL_FORWARD=true\nAGENTO11Y_ENDPOINT=https://primary.example.test\nAGENTO11Y_AUTH_TENANT_ID=tenant\nAGENTO11Y_AUTH_TOKEN=token\nAGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://primary-otlp.example.test\n")
	writeForwarding(t, "invalid = 'extra-token-private\n")
	var conversations, analytics int
	probeConversationsFn = func(_ context.Context, endpoint string, _ envValue, _ string, _ bool, _ string) *ProbeResult {
		conversations++
		if endpoint != "https://shell.example.test" {
			t.Fatal("primary probe changed")
		}
		return &ProbeResult{StatusCode: 200, OK: true}
	}
	probeOTLPFn = func(context.Context, envValue) *AnalyticsProbe {
		analytics++
		return &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200}, Traces: &ProbeResult{StatusCode: 200}}
	}
	r := Collect(t.Context(), Options{}, Params{OSEnv: map[string]string{"AGENTO11Y_ENDPOINT": "https://shell.example.test"}})
	// Primary probes retain shell-first precedence, while local forwarding uses the file.
	if conversations != 1 || analytics != 1 || !r.cloudReady() || r.exitCodeFor(Options{RequireCloud: true}) != 0 {
		t.Fatal("invalid extra file changed primary probes or require-cloud")
	}
	sec := r.LocalForwarding
	if sec.Health != HealthWarn || sec.Error == "" || len(sec.Destinations) != 1 {
		t.Fatal("invalid extra file is not a separate section diagnostic")
	}
	primary := sec.Destinations[0]
	if primary.Name != "primary" || primary.Endpoint != "https://primary.example.test/api/v1/generations:export" || !primary.HTTPGenerations || !primary.OTLP {
		t.Fatal("reserved primary does not reflect daemon precedence and capabilities")
	}
	forwardingOutput(t, sec)
}

func TestLocalForwardingPrimaryRoutes(t *testing.T) {
	const primary = "AGENTO11Y_LOCAL_FORWARD=true\nAGENTO11Y_ENDPOINT=https://primary.example.test\nAGENTO11Y_AUTH_TENANT_ID=extra-tenant-private\nAGENTO11Y_AUTH_TOKEN=extra-token-private\nAGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test/otlp\n"
	for _, tc := range []struct {
		name, route, credentials, primaryExtra string
		duplicate                              bool
		shellOTLP                              string
	}{
		{"HTTP collision", "endpoint='https://PRIMARY.example.test:443/api/v1/generations:export'", "tenant_id='extra-tenant-private'\ntoken='extra-token-private'", "", true, ""},
		{"HTTP different tenant", "endpoint='https://PRIMARY.example.test:443/api/v1/generations:export'", "tenant_id='other-tenant'\ntoken='extra-token-private'", "", false, ""},
		{"HTTP query remains distinct", "endpoint='https://PRIMARY.example.test:443/api/v1/generations:export'", "tenant_id='extra-tenant-private'\ntoken='extra-token-private'", "AGENTO11Y_ENDPOINT=https://primary.example.test?token=url-query-private\n", false, ""},
		{"OTLP collision", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='extra-tenant-private'\notlp_token='extra-token-private'", "", true, ""},
		{"OTLP different tenant", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='other-tenant'\notlp_token='extra-token-private'", "", false, ""},
		{"OTLP dedicated token", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='extra-tenant-private'\notlp_token='extra-otlp-private'", "AGENTO11Y_OTEL_AUTH_TOKEN=extra-otlp-private\n", true, ""},
		{"OTLP different token", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='extra-tenant-private'\notlp_token='extra-token-private'", "AGENTO11Y_OTEL_AUTH_TOKEN=extra-otlp-private\n", false, ""},
		{"OTLP explicit headers", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='other-tenant'\notlp_headers='authorization=Bearer extra-header-private,x-custom=custom-private'", "OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer extra-header-private,X-Custom=custom-private\n", true, ""},
		{"OTLP different headers", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "otlp_headers='Authorization=Bearer extra-header-private,X-Custom=other-value'", "OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer extra-header-private,X-Custom=custom-private\n", false, ""},
		{"OTLP bare shell endpoint is trimmed", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/traces'", "tenant_id='extra-tenant-private'\notlp_token='extra-token-private'", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=\n", true, "  https://collector.example.test/otlp  "},
		{"OTLP bare shell schemeless endpoint is trimmed", "otlp_endpoint='https://COLLECTOR.example.test:443/otlp/v1/metrics'", "tenant_id='extra-tenant-private'\notlp_token='extra-token-private'", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=\n", true, "  collector.example.test/otlp  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			guardForwardingRequests(t, "")
			restore := local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
				t.Fatal("doctor tried to start a daemon")
				return nil, nil
			})
			t.Cleanup(restore)
			config := primary + tc.primaryExtra
			if tc.shellOTLP != "" {
				config = strings.Replace(config, "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.test/otlp\n", "", 1)
			}
			writeConfig(t, config)
			content := "[[destinations]]\nname='same-gateway'\n" + tc.route + "\n" + tc.credentials + "\n" + strings.Replace(extraForwarding, "https://collector.example.test/otlp", "https://other-collector.example.test/otlp", 1)
			path := writeForwarding(t, content)
			var conversations, analytics int
			probeConversationsFn = func(_ context.Context, endpoint string, _ envValue, _ string, _ bool, _ string) *ProbeResult {
				conversations++
				if endpoint != "https://shell.example.test" {
					t.Fatal("primary probe precedence changed")
				}
				return &ProbeResult{StatusCode: 200, OK: true}
			}
			probeOTLPFn = func(context.Context, envValue) *AnalyticsProbe {
				analytics++
				return &AnalyticsProbe{Metrics: &ProbeResult{StatusCode: 200}, Traces: &ProbeResult{StatusCode: 200}}
			}
			r := Collect(t.Context(), Options{}, Params{OSEnv: map[string]string{
				"AGENTO11Y_ENDPOINT": "https://shell.example.test", "AGENTO11Y_AUTH_TENANT_ID": "shell-tenant", "AGENTO11Y_AUTH_TOKEN": "shell-token",
				"OTEL_EXPORTER_OTLP_ENDPOINT": tc.shellOTLP,
			}})
			sec := r.LocalForwarding
			if conversations != 1 || analytics != 1 || !r.cloudReady() || r.exitCodeFor(Options{RequireCloud: true}) != 0 {
				t.Fatal("combined validation changed primary probes or require-cloud")
			}
			if tc.duplicate {
				if sec.Health != HealthWarn || sec.Error == "" || len(sec.Destinations) != 1 || sec.Destinations[0].Name != "primary" {
					t.Error("duplicate primary route must warn and disable every extra")
				}
			} else if sec.Health != HealthOK || sec.Error != "" || len(sec.Destinations) != 3 {
				t.Error("distinct effective routes must remain valid")
			}
			if sec.Delivery != "not_checked" || sec.ForwardStatus != nil {
				t.Fatal("offline doctor claimed delivery history")
			}
			if _, err := os.Stat(local.StateDir()); !os.IsNotExist(err) {
				t.Fatal("doctor created daemon state")
			}
			text := forwardingOutput(t, sec)
			if tc.duplicate && !strings.Contains(text, "all extra destinations are disabled") {
				t.Error("warning did not explain that all extras are disabled")
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != content {
				t.Fatal("doctor modified forwarding.toml")
			}
			if data, err := os.ReadFile(r.Config.Path); err != nil || string(data) != config {
				t.Fatal("doctor modified config.env")
			}
		})
	}
}

func liveForwardStatus() ForwardStatus {
	leg := ForwardLeg{Attempted: 4, Succeeded: 1, Failed: 2, Dropped: 1, LastSuccessAt: "2026-06-03T10:00:00Z", LastFailureAt: "2026-06-03T10:01:00Z", LastFailureDetail: "generation endpoint rejected export (HTTP 403)"}
	failure := ForwardFailure{Destination: "archive", At: leg.LastFailureAt, Label: "generations", Detail: leg.LastFailureDetail}
	return ForwardStatus{
		Enabled: true, Mode: "metadata_only", Generations: true, OTLP: true, Hooks: true,
		ForwardingPath: "/home/u/.config/agento11y/forwarding.toml",
		Destinations: []ForwardDestinationStatus{
			{ForwardDestinationInfo: local.ForwardDestinationInfo{Name: "primary", Endpoint: "https://primary.example.test/api/v1/generations:export", HTTPGenerations: true}, Revision: 1},
			{ForwardDestinationInfo: local.ForwardDestinationInfo{Name: "archive", Endpoint: "https://archive.example.test/api/v1/generations:export", OTLPEndpoint: "https://collector.example.test/otlp", HTTPGenerations: true, OTLP: true}, Revision: 3, Legs: map[string]ForwardLeg{"generations": leg}, Failures: []ForwardFailure{failure}},
		},
		Legs:     map[string]ForwardLeg{"hooks": {LastSuccessAt: "2026-06-03T09:00:00Z", LastFailureAt: "2026-06-03T09:01:00Z", LastFailureDetail: "hook check failed (HTTP 403)"}},
		Failures: []ForwardFailure{failure, {At: "2026-06-03T09:01:00Z", Label: "hooks", Detail: "hook check failed (HTTP 403)"}}, HookFailOpens: 1,
	}
}

func serveForwardStatus(t *testing.T, status int, body string) *atomic.Int32 {
	t.Helper()
	requests := new(atomic.Int32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/config" {
			t.Error("doctor sent a request other than read-only config inspection")
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Scope-OrgID") != "" {
			t.Error("doctor sent credentials to the local daemon")
		}
		if status >= 300 && status < 400 {
			w.Header().Set("Location", "https://remote.example.test/?token=body-secret-private")
		}
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	if err := local.SaveStatus(local.StateDir(), local.Status{Endpoint: srv.URL, PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	return requests
}

func TestLocalForwardingRuntime(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	loadLocalStatus = local.LoadStatus
	writeForwarding(t, extraForwarding)
	want := liveForwardStatus()
	body, err := json.Marshal(map[string]any{"forwardStatus": want, "token": "extra-token-private", "otlpHeaders": "extra-header-private"})
	if err != nil {
		t.Fatal(err)
	}
	requests := serveForwardStatus(t, 200, string(body))
	status, err := local.LoadStatus(local.StateDir())
	if err != nil || status == nil {
		t.Fatal("missing test daemon status")
	}
	guardForwardingRequests(t, status.Endpoint)
	r := Collect(t.Context(), Options{}, Params{})
	sec := r.LocalForwarding
	if requests.Load() != 1 || sec.Delivery != "available" || !reflect.DeepEqual(sec.ForwardStatus, &want) || sec.Health != HealthWarn {
		t.Fatal("daemon forwarding snapshot was not preserved")
	}
	text := forwardingOutput(t, sec)
	for _, fragment := range []string{"revision 3", "attempted 4, succeeded 1, failed 2, dropped 1", "2026-06-03T10:00:00Z", "HTTP 403", "primary forwarding enabled true", "regardless of capture mode", "extra endpoints are not probed"} {
		if !strings.Contains(text, fragment) {
			t.Fatalf("missing delivery diagnostic %q", fragment)
		}
	}
	if strings.Contains(text, "hooks: attempted") {
		t.Error("untracked hook counters were rendered as measurements")
	}
	for _, fragment := range []string{"hooks", "2026-06-03T09:00:00Z", "2026-06-03T09:01:00Z hook check failed (HTTP 403)", "hooks: hook check failed (HTTP 403)", "guard fail-opens:"} {
		if !strings.Contains(text, fragment) {
			t.Errorf("missing hook history %q", fragment)
		}
	}
	var encoded bytes.Buffer
	if err := renderJSON(&encoded, r); err != nil {
		t.Fatal(err)
	}
	var decoded Report
	if err := json.Unmarshal(encoded.Bytes(), &decoded); err != nil || !reflect.DeepEqual(decoded.LocalForwarding.ForwardStatus, &want) {
		t.Fatal("JSON lost forwarding delivery fields")
	}
}

func TestLocalForwardingUnavailableStatus(t *testing.T) {
	hooks, err := json.Marshal(liveForwardStatus().Legs["hooks"])
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(map[string]any{"forwardStatus": liveForwardStatus()})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"missing", `{}`, 200},
		{"null", `{"forwardStatus":null}`, 200},
		{"empty", `{"forwardStatus":{}}`, 200},
		{"malformed", `{"forwardStatus":"extra-token-private"}`, 200},
		{"invalid JSON", `{"token":"body-secret-private`, 200},
		{"missing boolean", strings.Replace(string(valid), `"enabled":true,`, "", 1), 200},
		{"missing destination capability", strings.ReplaceAll(string(valid), `"httpGenerations":true,`, ""), 200},
		{"null destination capability", strings.ReplaceAll(string(valid), `"httpGenerations":true`, `"httpGenerations":null`), 200},
		{"inconsistent mode", strings.Replace(string(valid), `"enabled":true`, `"enabled":false`, 1), 200},
		{"wrong boolean", strings.Replace(string(valid), `"enabled":true`, `"enabled":"false"`, 1), 200},
		{"unknown mode", strings.Replace(string(valid), "metadata_only", "unknown", 1), 200},
		{"missing leg counter", strings.Replace(string(valid), `"succeeded":1,`, "", 1), 200},
		{"null leg counter", strings.Replace(string(valid), `"succeeded":1`, `"succeeded":null`, 1), 200},
		{"null leg", strings.Replace(string(valid), `"hooks":`+string(hooks), `"hooks":null`, 1), 200},
		{"missing failure detail", strings.Replace(string(valid), `"detail":"generation endpoint rejected export (HTTP 403)"`, `"other":"ignored"`, 1), 200},
		{"missing destinations", `{"forwardStatus":{"enabled":false,"mode":"off","generations":false,"otlp":false,"hooks":false}}`, 200},
		{"null destinations", `{"forwardStatus":{"enabled":false,"mode":"off","generations":false,"otlp":false,"hooks":false,"destinations":null}}`, 200},
		{"bad destination", `{"forwardStatus":{"enabled":false,"mode":"off","generations":false,"otlp":false,"hooks":false,"destinations":[{}]}}`, 200},
		{"trailing garbage", string(valid) + "body-secret-private", 200},
		{"redirect", "body-secret-private", 302},
		{"not found", "body-secret-private", 404},
		{"server error", "body-secret-private", 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			loadLocalStatus = local.LoadStatus
			writeForwarding(t, extraForwarding)
			requests := serveForwardStatus(t, tc.status, tc.body)
			status, err := local.LoadStatus(local.StateDir())
			if err != nil || status == nil {
				t.Fatal("missing test daemon status")
			}
			guardForwardingRequests(t, status.Endpoint)
			r := Collect(t.Context(), Options{}, Params{})
			sec := r.LocalForwarding
			if requests.Load() != 1 || sec.ForwardStatus != nil || sec.Delivery != "unavailable" || len(sec.Destinations) != 1 {
				t.Fatal("unavailable status became a known forwarding posture or erased configuration")
			}
			text := forwardingOutput(t, sec)
			if !strings.Contains(text, "forwarding state unknown") || strings.Contains(text, "runtime forwarding:") {
				t.Fatal("unavailable status was rendered as forwarding off")
			}
		})
	}
}

func TestLocalForwardingEndpointSecrecy(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	env := map[string]string{
		"AGENTO11Y_ENDPOINT":          "https://url-user-private:url-password-private@primary.example.test?token=url-query-private#url-fragment-private",
		"AGENTO11Y_AUTH_TENANT_ID":    "extra-tenant-private",
		"AGENTO11Y_AUTH_TOKEN":        "extra-token-private",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "https://url-user-private:url-password-private@collector.example.test/otlp?token=url-query-private#url-fragment-private",
	}
	sec := collectLocalForwarding(t.Context(), dotenv.FilePath(), env, nil)
	if len(sec.Destinations) != 1 || sec.Destinations[0].Name != "primary" || !sec.Destinations[0].HTTPGenerations || !sec.Destinations[0].OTLP {
		t.Fatal("primary destination capabilities were not reported")
	}
	forwardingOutput(t, sec)

	loadLocalStatus = local.LoadStatus
	st := liveForwardStatus()
	st.Destinations[0].Endpoint = env["AGENTO11Y_ENDPOINT"]
	st.Destinations[1].OTLPEndpoint = env["OTEL_EXPORTER_OTLP_ENDPOINT"]
	body, err := json.Marshal(map[string]any{"forwardStatus": st})
	if err != nil {
		t.Fatal(err)
	}
	serveForwardStatus(t, 200, string(body))
	sec = collectLocalForwarding(t.Context(), dotenv.FilePath(), env, nil)
	if sec.ForwardStatus == nil {
		t.Fatal("missing daemon status")
	}
	forwardingOutput(t, sec)
}

func TestLocalForwardingRuntimeOffIsExplicit(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	loadLocalStatus = local.LoadStatus
	writeForwarding(t, extraForwarding)
	serveForwardStatus(t, 200, `{"forwardStatus":{"enabled":false,"mode":"off","generations":false,"otlp":false,"hooks":false,"destinations":[]}}`)
	r := Collect(t.Context(), Options{}, Params{})
	if r.LocalForwarding.Delivery != "available" || r.LocalForwarding.ForwardStatus == nil || r.LocalForwarding.ForwardStatus.Enabled {
		t.Fatal("explicit off status is not available")
	}
	text := forwardingOutput(t, r.LocalForwarding)
	if !strings.Contains(text, "enabled false, mode off") || !strings.Contains(text, "archive") {
		t.Fatal("forwarding off erased configured extra destinations")
	}
}

func TestLocalForwardingDaemonUnavailableNoStart(t *testing.T) {
	for _, endpoint := range []string{"", "http://127.0.0.1:1", "https://remote.example.test", "http://url-user-private:url-password-private@127.0.0.1:1"} {
		t.Run(endpoint, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			loadLocalStatus = local.LoadStatus
			restore := local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
				t.Fatal("doctor tried to start a daemon")
				return nil, nil
			})
			t.Cleanup(restore)
			if endpoint != "" {
				if err := local.SaveStatus(local.StateDir(), local.Status{Endpoint: endpoint, PID: os.Getpid()}); err != nil {
					t.Fatal(err)
				}
			}
			r := Collect(t.Context(), Options{}, Params{})
			if r.LocalForwarding.Delivery != "not_checked" || r.LocalForwarding.ForwardStatus != nil {
				t.Fatal("unavailable daemon reported delivery")
			}
			forwardingOutput(t, r.LocalForwarding)
			if endpoint == "" {
				if _, err := os.Stat(local.StateDir()); !os.IsNotExist(err) {
					t.Fatal("doctor created local daemon state")
				}
			}
		})
	}
}
