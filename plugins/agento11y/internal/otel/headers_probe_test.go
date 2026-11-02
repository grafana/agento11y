package otel_test

import (
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/doctor"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

// External package avoids the doctor -> otel import cycle while exercising its real probe.
func TestGenerationExportHeadersMatchDoctorProbe(t *testing.T) {
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
	for _, tc := range []struct {
		name       string
		tenant     string
		token      string
		rawHeaders string
		headers    map[string]string
		wantAuth   string
		wantExtra  string
	}{
		{name: "tokenless resolved empty headers"},
		{name: "tenant without token", tenant: "tenant"},
		{name: "synthesized auth", tenant: "tenant", token: "token", wantAuth: basic},
		{name: "explicit generic auth", tenant: "tenant", token: "token", rawHeaders: "Authorization=Bearer generic,X-Extra=generic", wantAuth: "Bearer generic", wantExtra: "generic"},
		{name: "non-auth generic header", rawHeaders: "X-Extra=generic", wantExtra: "generic"},
		{name: "explicit empty override", tenant: "tenant", token: "token", rawHeaders: "Authorization=Bearer generic", headers: map[string]string{}},
		{name: "explicit auth override", rawHeaders: "Authorization=Bearer generic", headers: map[string]string{"Authorization": "Bearer override"}, wantAuth: "Bearer override"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", tc.tenant)
			t.Setenv("AGENTO11Y_AUTH_TOKEN", tc.token)
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", tc.rawHeaders)
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Bearer trace-only,X-Traces-Only=leak")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "Authorization=Bearer metric-only,X-Metrics-Only=leak")
			t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/wrong")
			t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/wrong")

			var mu sync.Mutex
			counts := map[string]int{}
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for key, want := range map[string]string{
					"Authorization": tc.wantAuth, "X-Extra": tc.wantExtra,
					"X-Traces-Only": "", "X-Metrics-Only": "",
				} {
					if got := r.Header.Get(key); got != want {
						t.Errorf("%s %s = %q, want %q", r.URL.Path, key, got, want)
					}
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/otlp/v1/metrics":
					counts["metrics"]++
				case "/otlp/v1/traces":
					req := &coltracepb.ExportTraceServiceRequest{}
					if err := proto.Unmarshal(body, req); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if len(req.ResourceSpans) == 0 {
						counts["probe"]++
					}
					for _, rs := range req.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								if span.Name == "tool" {
									counts["tool"]++
								} else {
									counts["generation"]++
								}
							}
						}
					}
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				w.WriteHeader(http.StatusOK)
			}))
			listener, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			server.Listener = listener
			server.Start()
			defer server.Close()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
			_, target, ok := otel.ProbeConfig()
			if !ok {
				t.Fatal("missing probe target")
			}
			if tc.headers != nil {
				target.Headers = tc.headers
			}
			if result := doctor.ProbeGenerationOTLP(t.Context(), target.URL, target.Headers); !result.OK {
				t.Fatalf("doctor probe failed: %+v", result)
			}

			p, err := otel.SetupWithOptions(t.Context(), "header-parity", otel.Options{Headers: tc.headers})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Shutdown(context.Background()) }()
			client := agento11y.NewClient(agento11y.Config{
				EnableExperimentalFeatures: new(true),
				GenerationExport:           agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolOTel},
				TracerProvider:             p.GenerationTracerProvider(),
				MeterProvider:              p.MeterProvider(),
				Flusher:                    p.GenerationFlusher(),
			})
			defer func() { _ = client.Shutdown(context.Background()) }()
			_, rec := client.StartGeneration(t.Context(), agento11y.GenerationStart{Model: agento11y.ModelRef{Provider: "test", Name: "test"}})
			rec.End()
			if err := rec.Err(); err != nil {
				t.Fatal(err)
			}
			if err := client.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, span := p.Tracer("test").Start(t.Context(), "tool")
			span.End()
			counter, err := p.Meter("test").Int64Counter("header_parity")
			if err != nil {
				t.Fatal(err)
			}
			counter.Add(t.Context(), 1)
			if err := p.ForceFlush(); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, kind := range []string{"probe", "generation", "tool", "metrics"} {
				if counts[kind] == 0 {
					t.Errorf("no %s request reached loopback collector", kind)
				}
			}
		})
	}
}
