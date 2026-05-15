package otel

import (
	"context"
	"encoding/base64"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestEndpointFromEnvPrefersSigilPrefix(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "https://sigil.example/otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example/otlp")

	if got := EndpointFromEnv(); got != "https://sigil.example/otlp" {
		t.Fatalf("EndpointFromEnv() = %q", got)
	}
}

func TestEndpointFromEnvFallsBackToStandardOtel(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example/otlp")

	if got := EndpointFromEnv(); got != "https://otel.example/otlp" {
		t.Fatalf("EndpointFromEnv() = %q", got)
	}
}

func TestExporterConfigUsesSigilPrefixedValues(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "https://wrong.example/traces")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Bearer wrong")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Sigil-Test=ok")

	cfg := exporterConfigFromEnv("https://sigil.example/otlp")

	if cfg.endpoint != "https://sigil.example/otlp" {
		t.Fatalf("endpoint = %q", cfg.endpoint)
	}
	if !cfg.insecure {
		t.Fatal("expected insecure=true")
	}
	if cfg.headers["Authorization"] == "Bearer wrong" {
		t.Fatalf("signal-specific headers should not be imported: %+v", cfg.headers)
	}
	if cfg.headers["X-Sigil-Test"] != "ok" {
		t.Fatalf("generic OTel headers missing: %+v", cfg.headers)
	}
}

func TestExporterConfigUsesSigilOtelTokenWhenSet(t *testing.T) {
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "generation-token")
	t.Setenv("SIGIL_OTEL_AUTH_TOKEN", "otel-token")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	cfg := exporterConfigFromEnv("https://sigil.example/otlp")

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:otel-token"))
	if got := cfg.headers["Authorization"]; got != want {
		t.Fatalf("Authorization = %q, want %q", got, want)
	}
}

func TestExporterConfigKeepsExplicitAuthorization(t *testing.T) {
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer explicit")

	cfg := exporterConfigFromEnv("https://sigil.example/otlp")

	if got := cfg.headers["Authorization"]; got != "Bearer explicit" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestSignalEndpointURLAppendsOTLPHTTPPaths(t *testing.T) {
	if got := signalEndpointURL("https://otlp.example/otlp", "traces"); got != "https://otlp.example/otlp/v1/traces" {
		t.Fatalf("trace endpoint = %q", got)
	}
	if got := signalEndpointURL("https://otlp.example/otlp/v1/traces", "metrics"); got != "https://otlp.example/otlp/v1/metrics" {
		t.Fatalf("metric endpoint = %q", got)
	}
}

func TestSetupExportsToSignalSpecificPaths(t *testing.T) {
	var mu sync.Mutex
	paths := map[string]int{}
	authHeaders := map[string]string{}
	server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths[r.URL.Path]++
		authHeaders[r.URL.Path] = r.Header.Get("Authorization")
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "http://127.0.0.1:1/wrong")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "http://127.0.0.1:1/wrong")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS", "Authorization=Bearer wrong")

	providers, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	ctx := context.Background()
	_, span := providers.Tracer("test").Start(ctx, "span")
	span.End()
	counter, err := providers.Meter("test").Int64Counter("requests")
	if err != nil {
		t.Fatalf("Int64Counter: %v", err)
	}
	counter.Add(ctx, 1)
	if err := providers.ForceFlush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if err := providers.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, path := range []string{"/otlp/v1/traces", "/otlp/v1/metrics"} {
		if paths[path] == 0 {
			t.Fatalf("no request to %s; got paths %+v", path, paths)
		}
		wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
		if authHeaders[path] != wantAuth {
			t.Fatalf("Authorization for %s = %q, want %q", path, authHeaders[path], wantAuth)
		}
	}
	if paths["/otlp"] != 0 {
		t.Fatalf("unexpected request to base /otlp path: %+v", paths)
	}
}

func newTestServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback listen unavailable in this sandbox: %v", err)
	}
	server := httptest.NewUnstartedServer(handler)
	server.Listener = listener
	server.Start()
	return server
}
