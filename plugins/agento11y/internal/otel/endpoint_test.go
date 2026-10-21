package otel

import (
	"context"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestGenerationEffectiveEndpoint(t *testing.T) {
	for _, name := range []string{"ambient insecure", "explicit HTTPS", "explicit HTTP"} {
		t.Run(name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
			var traces atomic.Int32
			handler := http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
				if (req.TLS != nil) != (name == "explicit HTTPS") {
					t.Error("wrong wire transport")
				}
				switch req.URL.Path {
				case "/otlp/v1/traces":
					traces.Add(1)
				case "/otlp/v1/metrics":
				default:
					t.Errorf("unexpected path %s", req.URL.Path)
				}
			})
			var server *httptest.Server
			if name == "explicit HTTPS" {
				server = httptest.NewTLSServer(handler)
				cert := filepath.Join(t.TempDir(), "collector.pem")
				if err := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("OTEL_EXPORTER_OTLP_CERTIFICATE", cert)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			endpoint := server.URL + "/otlp"
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", strings.Replace(endpoint, "http://", "https://", 1))
			opts := Options{}
			if name != "ambient insecure" {
				opts.Endpoint = endpoint
			}
			p, err := SetupWithOptions(t.Context(), "test", opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Shutdown(context.Background()) }()
			if got := p.GenerationEndpoint(); got != endpoint {
				t.Fatalf("generation endpoint = %q, want %q", got, endpoint)
			}
			_, span := p.GenerationTracerProvider().Tracer("test").Start(t.Context(), "generation")
			span.End()
			if err := p.GenerationFlusher().ForceFlush(t.Context()); err != nil {
				t.Fatal(err)
			}
			if traces.Load() != 1 {
				t.Fatalf("trace requests = %d", traces.Load())
			}
		})
	}
}
