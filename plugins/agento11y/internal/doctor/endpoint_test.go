package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
)

func TestGenerationOrdinaryCollector(t *testing.T) {
	for _, status := range []int{404, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			isolateEnv(t)
			stubSeams(t)
			var traces, generations int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"status":"ok"}`)
				case "/v1/traces":
					traces++
				default:
					generations++
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			env := map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, "AGENTO11Y_LOCAL": "false"}
			r := Collect(t.Context(), Options{}, Params{OSEnv: env})
			if r.Conversations.Protocol != "otel" || r.Conversations.Health != HealthOK || traces != 1 || generations != 0 || r.Conversations.LocalCapability != nil {
				t.Fatalf("conversations=%+v traces=%d other=%d", r.Conversations, traces, generations)
			}
			if r.Config.Health != HealthOK || r.Config.GenerationProtocol != (envValue{}) || r.Config.ExperimentalEnabled {
				t.Fatalf("default config = %+v", r.Config)
			}
			messages := strings.Join(r.Conversations.Messages, " ")
			for _, want := range []string{"generation storage is not verified", "Pi and OpenCode live plugins keep HTTP generation export"} {
				if !strings.Contains(messages, want) {
					t.Fatalf("default transport diagnostic missing %q: %s", want, messages)
				}
			}
		})
	}
}

func TestGenerationEffectiveLocalNotCloud(t *testing.T) {
	isolateEnv(t)
	stubSeams(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
		}
	}))
	defer srv.Close()
	env := map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": strings.Replace(srv.URL, "http://", "https://", 1) + "/otlp", "AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE": "true", "AGENTO11Y_AUTH_TENANT_ID": "tenant", "AGENTO11Y_OTEL_AUTH_TOKEN": "test-token"}
	r := Collect(t.Context(), Options{}, Params{OSEnv: env})
	if r.Conversations.Health != HealthOK || r.Conversations.LocalCapability == nil || !r.Conversations.LocalCapability.OK {
		t.Fatalf("conversations=%+v", r.Conversations)
	}
	if r.cloudReady() || r.exitCodeFor(Options{RequireCloud: true}) != 1 {
		t.Fatal("effective local daemon satisfies require-cloud")
	}
	if r.Conversations.Endpoint.Value != srv.URL+"/otlp" {
		t.Fatalf("reported endpoint=%s", r.Conversations.Endpoint.Value)
	}
}

func TestGenerationManagedLocalDestination(t *testing.T) {
	for _, source := range []string{"shell", "config.env"} {
		for _, tc := range []struct {
			name                    string
			optIn, capable, stopped bool
			want                    Health
		}{
			{"capable", true, true, false, HealthOK}, {"old daemon", false, false, false, HealthError}, {"experimental disabled", false, true, false, HealthOK},
			{"stopped daemon", false, true, true, HealthOK},
		} {
			t.Run(source+"/"+tc.name, func(t *testing.T) {
				isolateEnv(t)
				stubSeams(t)
				var health, traces int
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "" {
						t.Error("local probe sent Cloud authorization")
					}
					switch r.URL.Path {
					case "/healthz":
						health++
						if tc.capable {
							_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
						} else {
							_, _ = io.WriteString(w, `{"status":"ok"}`)
						}
					case "/otlp/v1/traces":
						traces++
					default:
						t.Errorf("unexpected request %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				defer srv.Close()
				if err := local.SaveStatus(local.StateDir(), local.Status{PID: os.Getpid(), Endpoint: srv.URL}); err != nil {
					t.Fatal(err)
				}
				wantCalls := 1
				if tc.stopped {
					srv.Close()
					wantCalls = 0
				}
				env := map[string]string{"AGENTO11Y_LOCAL": "true", "AGENTO11Y_LOCAL_FORWARD": "false", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": fmt.Sprint(tc.optIn)}
				if source == "config.env" {
					var b strings.Builder
					for k, v := range env {
						fmt.Fprintf(&b, "%s=%s\n", k, v)
					}
					writeConfig(t, b.String())
					env = nil
				}
				r := Collect(context.Background(), Options{}, Params{OSEnv: env})
				if r.Conversations.Health != tc.want || r.Config.Health != HealthOK {
					t.Fatalf("conversations=%+v config=%+v", r.Conversations, r.Config)
				}
				if tc.want == HealthOK {
					if r.exitCode() != 0 || health != wantCalls || traces != wantCalls || r.Conversations.Endpoint.Value != srv.URL+"/otlp" || needsSetup(r) {
						t.Fatalf("exit=%d health=%d traces=%d needsSetup=%v conversations=%+v", r.exitCode(), health, traces, needsSetup(r), r.Conversations)
					}
				} else if traces != 0 || r.exitCode() != 1 {
					t.Fatalf("exit=%d traces=%d", r.exitCode(), traces)
				}
				if !tc.capable && (health != 1 || !strings.Contains(strings.Join(r.Conversations.Messages, " "), "restart")) {
					t.Fatalf("old daemon not rejected: health=%d section=%+v", health, r.Conversations)
				}
				if r.cloudReady() || r.exitCodeFor(Options{RequireCloud: true}) != 1 {
					t.Fatal("managed local-only capture satisfies require-cloud")
				}
			})
		}
	}
}
