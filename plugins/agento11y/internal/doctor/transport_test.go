package doctor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/localhealth"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestGenerationTransportSnapshot(t *testing.T) {
	isolateEnv(t)
	t.Setenv("AGENTO11Y_PROTOCOL", "http")
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	osEnv := map[string]string{"SIGIL_PROTOCOL": "otel"}
	fileEnv := map[string]string{
		"AGENTO11Y_PROTOCOL":                     "http",
		"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "yes",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  "http://127.0.0.1:4318/otlp",
		"AGENTO11Y_CONTENT_CAPTURE_MODE":         "full_with_metadata_spans",
	}
	cfg := collectConfig(osEnv, fileEnv)
	if cfg.GenerationProtocol.Value != "otel" || cfg.GenerationProtocol.Key != "SIGIL_PROTOCOL" || cfg.GenerationProtocol.Source != sourceEnv || !cfg.GenerationProtocol.Conflict {
		t.Fatalf("protocol = %+v", cfg.GenerationProtocol)
	}
	if !cfg.ExperimentalEnabled || cfg.ExperimentalFeatures.Source != sourceConfig || cfg.Health != HealthOK {
		t.Fatalf("experimental = %+v, enabled=%v, health=%s", cfg.ExperimentalFeatures, cfg.ExperimentalEnabled, cfg.Health)
	}
	sec := collectConversations(osEnv, fileEnv)
	if sec.Protocol != "otel" || !sec.configured() || !strings.Contains(strings.Join(sec.Messages, " "), "upgrades full_with_metadata_spans to full") {
		t.Fatalf("conversations = %+v", sec)
	}
	if cfg.ContentCaptureMode != "full_with_metadata_spans" {
		t.Fatal("doctor changed the requested capture mode")
	}
}

func TestGenerationTransportSnapshotKeys(t *testing.T) {
	isolateEnv(t)
	for _, key := range []string{"SIGIL_PROTOCOL", "AGENTO11Y_PROTOCOL", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "OTEL_EXPORTER_OTLP_INSECURE", "AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE"} {
		t.Setenv(key, "snapshot")
	}
	t.Setenv("SIGIL_ENABLE_EXPERIMENTAL_FEATURES", "true")
	snapshot := SnapshotEnv()
	for _, key := range []string{"SIGIL_PROTOCOL", "AGENTO11Y_PROTOCOL", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "OTEL_EXPORTER_OTLP_INSECURE", "AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE"} {
		if snapshot[key] != "snapshot" {
			t.Errorf("snapshot omitted %s", key)
		}
	}
	if _, ok := snapshot["SIGIL_ENABLE_EXPERIMENTAL_FEATURES"]; ok {
		t.Fatal("experimental gate must be preferred-only")
	}
}

func TestGenerationTransportInvalidNeverFallsBack(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"unknown protocol", map[string]string{"AGENTO11Y_PROTOCOL": "unsupported"}},
		{"missing opt-in", map[string]string{"AGENTO11Y_PROTOCOL": "otel"}},
		{"invalid opt-in", map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "enabled"}},
		{"missing endpoint", map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true"}},
		{"invalid endpoint", map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "not-a-url"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			probeConversationsFn = func(context.Context, string, envValue, string, bool, string) *ProbeResult {
				t.Fatal("invalid generation transport fell back to HTTP")
				return nil
			}
			tc.env["AGENTO11Y_ENDPOINT"] = "http://127.0.0.1:9000"
			tc.env["AGENTO11Y_AUTH_TENANT_ID"] = "tenant"
			tc.env["AGENTO11Y_AUTH_TOKEN"] = "secret"
			r := Collect(context.Background(), Options{}, Params{OSEnv: tc.env})
			if r.Conversations.Health != HealthError || r.Config.Health != HealthError || r.exitCode() != 1 || r.Conversations.Probe != nil {
				t.Fatalf("health conversations=%s config=%s probe=%+v", r.Conversations.Health, r.Config.Health, r.Conversations.Probe)
			}
		})
	}
}

func TestGenerationOTelProbe(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		headers     string
		wantHealth  Health
		wantMessage string
	}{
		{"accepted", 200, "", HealthOK, "transport/authentication verified; generation storage is not verified"},
		{"accepted with explicit auth", 200, "Authorization=Bearer explicit-secret", HealthOK, "generation storage is not verified"},
		{"credentials rejected", 401, "", HealthError, "AUTH_TENANT_ID (tenant)"},
		{"explicit credentials rejected", 401, "Authorization=Bearer explicit-secret", HealthError, "invalid or expired"},
		{"scope denied", 403, "", HealthError, "traces:write"},
		{"redirect", 302, "", HealthError, "HTTP 302"},
		{"missing route", 404, "", HealthError, "HTTP 404"},
		{"method rejected", 405, "", HealthWarn, "not accepted"},
		{"body rejected", 400, "", HealthWarn, "not accepted"},
		{"server failure", 503, "", HealthError, "HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			var healthHits, traceHits int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				switch req.URL.Path {
				case "/healthz":
					healthHits++
					if req.Header.Get("Authorization") != "" || req.Header.Get("X-Scope-OrgID") != "" || req.Header.Get("X-Custom") != "" {
						t.Error("health check inherited exporter headers")
					}
					_, _ = io.WriteString(w, `{"capabilities":["`+localhealth.OTelGenerationCapability+`"]}`)
				case "/otlp/v1/traces":
					traceHits++
					if req.Method != http.MethodPost || req.Header.Get("Content-Type") != "application/x-protobuf" {
						t.Error("generation probe must use OTLP protobuf POST")
					}
					body, err := io.ReadAll(req.Body)
					if err != nil {
						t.Error(err)
					}
					var batch collectortrace.ExportTraceServiceRequest
					if err := proto.Unmarshal(body, &batch); err != nil || len(body) != 0 || len(batch.ResourceSpans) != 0 {
						t.Error("probe sent generation or content")
					}
					wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:otel-secret"))
					if tc.headers != "" {
						wantAuth = "Bearer explicit-secret"
					}
					if req.Header.Get("Authorization") != wantAuth || req.Header.Get("X-Custom") != "test" {
						t.Error("trace probe did not use exporter headers")
					}
					if tc.status == 302 {
						w.Header().Set("Location", "/must-not-follow")
					}
					w.WriteHeader(tc.status)
				default:
					t.Errorf("unexpected request: %s", req.URL.Path)
					w.WriteHeader(500)
				}
			}))
			defer srv.Close()
			env := map[string]string{
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  srv.URL + "/otlp",
				"AGENTO11Y_ENDPOINT":                     ":invalid unrelated API endpoint",
				"AGENTO11Y_AUTH_MODE":                    "basic",
				"AGENTO11Y_AUTH_TENANT_ID":               "tenant",
				"AGENTO11Y_OTEL_AUTH_TOKEN":              "otel-secret",
				"OTEL_EXPORTER_OTLP_HEADERS":             "X-Custom=test," + tc.headers,
			}
			if tc.name == "accepted with explicit auth" {
				delete(env, "AGENTO11Y_AUTH_TENANT_ID")
				delete(env, "AGENTO11Y_OTEL_AUTH_TOKEN")
				delete(env, "AGENTO11Y_ENDPOINT")
			}
			probeConversationsFn = func(context.Context, string, envValue, string, bool, string) *ProbeResult {
				t.Error("OTel mode probed the generation HTTP API")
				return nil
			}
			collectAgents = func(context.Context, string) []AgentStatus {
				return []AgentStatus{{Name: "claude", Install: InstallStateInstalled, Health: HealthOK}, {Name: "pi", Install: InstallStateInstalled, Health: HealthOK}, {Name: "opencode", Install: InstallStateInstalled, Health: HealthOK}}
			}
			r := Collect(context.Background(), Options{}, Params{OSEnv: env})
			if healthHits != 1 || traceHits != 1 {
				t.Fatalf("health=%d traces=%d", healthHits, traceHits)
			}
			if r.Conversations.Health != tc.wantHealth || !strings.Contains(strings.Join(r.Conversations.Messages, " "), tc.wantMessage) {
				t.Fatalf("conversations = %+v", r.Conversations)
			}
			if tc.headers != "" && tc.status == 401 && strings.Contains(r.Conversations.Probe.Message, "AUTH_TENANT_ID") {
				t.Fatal("explicit auth failure blamed unrelated tenant")
			}
			if r.Config.Health != HealthOK || r.HTTPAPIs == nil || r.HTTPAPIs.Health != HealthWarn {
				t.Fatal("unrelated API credentials blocked OTel capture or lacked a separate diagnostic")
			}
			for _, a := range r.Agents[1:] {
				if a.Health != HealthWarn || !strings.Contains(a.Note, "Go history imports support OTel") {
					t.Fatalf("agent = %+v", a)
				}
			}
			if tc.status == 200 && r.exitCode() != 0 {
				t.Fatal("OTLP-only capture failed unrelated API checks")
			}
			if r.cloudReady() {
				t.Fatal("local OTLP capture satisfies --require-cloud")
			}
			var human, machine bytes.Buffer
			renderHuman(&human, r, false)
			if err := renderJSON(&machine, r); err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"otel-secret", "explicit-secret"} {
				if strings.Contains(human.String()+machine.String(), secret) {
					t.Fatal("report exposed a token")
				}
			}
			for _, text := range []string{"transport:", "otel", "AGENTO11Y_PROTOCOL", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "HTTP APIs", "local ingest"} {
				if !strings.Contains(human.String(), text) {
					t.Errorf("human report missing %q", text)
				}
			}
			var decoded Report
			if err := json.Unmarshal(machine.Bytes(), &decoded); err != nil || !decoded.Config.ExperimentalEnabled || decoded.Config.GenerationProtocol.Value != "otel" {
				t.Fatal("JSON lost transport configuration")
			}
		})
	}
}

func TestGenerationOTelLocalCapability(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		want       string
	}{
		{"old daemon", `{}`, 200, "restart"},
		{"invalid health", `not JSON`, 200, "decode local capabilities"},
		{"unhealthy daemon", `{}`, 503, "HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			var traces int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.URL.Path != "/healthz" {
					traces++
					t.Errorf("unsupported daemon received %s", req.URL.Path)
				}
				if req.Header.Get("Authorization") != "" {
					t.Error("health check leaked Cloud authorization")
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			env := map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL + "/otlp", "OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer cloud-secret"}
			r := Collect(context.Background(), Options{}, Params{OSEnv: env})
			if traces != 0 || r.Conversations.LocalCapability == nil || r.Conversations.Health != HealthError || r.Conversations.Probe != nil || r.Conversations.LocalCapability.OK || !strings.Contains(strings.Join(r.Conversations.Messages, " "), tc.want) {
				t.Fatalf("conversations = %+v", r.Conversations)
			}
		})
	}
}

func TestGenerationProbeTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"standard endpoint", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://127.0.0.1:4318/otlp"}, "https://127.0.0.1:4318/otlp/v1/traces"},
		{"legacy over standard", map[string]string{"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4318/otlp/v1/metrics", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:9999"}, "http://127.0.0.1:4318/otlp/v1/traces"},
		{"preferred insecure", map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://127.0.0.1:4318/otlp", "AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE": "true"}, "http://127.0.0.1:4318/otlp/v1/traces"},
		{"standard insecure", map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://127.0.0.1:4318/otlp", "OTEL_EXPORTER_OTLP_INSECURE": "true"}, "http://127.0.0.1:4318/otlp/v1/traces"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, actual, ok := otel.ProbeConfig()
			got := generationProbeTarget(tc.env, nil)
			if !ok || got.URL != tc.want || got.URL != actual.URL {
				t.Fatalf("got=%s exporter=%s want=%s", got.URL, actual.URL, tc.want)
			}
		})
	}
}
