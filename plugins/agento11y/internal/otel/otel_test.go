package otel

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
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

func TestProbeConfig(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example/otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	metrics, traces, ok := ProbeConfig()
	if !ok {
		t.Fatal("expected ok=true when an OTLP endpoint is configured")
	}
	if metrics.URL != "https://otlp.example/otlp/v1/metrics" {
		t.Fatalf("metrics URL = %q", metrics.URL)
	}
	if traces.URL != "https://otlp.example/otlp/v1/traces" {
		t.Fatalf("traces URL = %q", traces.URL)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant:token"))
	if metrics.Headers["Authorization"] != want {
		t.Fatalf("metrics auth = %q, want %q", metrics.Headers["Authorization"], want)
	}
	// Headers must be independent copies so a probe mutating one signal's
	// headers cannot corrupt the other's.
	metrics.Headers["Authorization"] = "tampered"
	if traces.Headers["Authorization"] != want {
		t.Fatalf("traces headers aliased metrics headers")
	}
}

func TestProbeConfigInsecureDropsToHTTP(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example/otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	metrics, traces, ok := ProbeConfig()
	if !ok {
		t.Fatal("expected ok=true when an OTLP endpoint is configured")
	}
	// Real export ships cleartext when insecure is set, so the probe must hit
	// http to test the same transport.
	if metrics.URL != "http://otlp.example/otlp/v1/metrics" {
		t.Fatalf("metrics URL = %q", metrics.URL)
	}
	if traces.URL != "http://otlp.example/otlp/v1/traces" {
		t.Fatalf("traces URL = %q", traces.URL)
	}
}

func TestProbeConfigNoEndpoint(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, _, ok := ProbeConfig(); ok {
		t.Fatal("expected ok=false when no OTLP endpoint is configured")
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

	providers, err := Setup(context.Background(), "test-instance")
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

func TestSetupAttachesServiceInstanceID(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "agento11y")

	captured := newOTLPCapture(t)
	defer captured.server.Close()

	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", captured.server.URL+"/otlp")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	ctx := context.Background()
	providers, err := Setup(ctx, "sess-abc")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}

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

	traceAttrs := captured.traceResourceAttrs(t)
	if got := findAttr(traceAttrs, "service.instance.id"); got != "sess-abc" {
		t.Fatalf("trace service.instance.id = %q, want %q", got, "sess-abc")
	}
	if got := findAttr(traceAttrs, "service.name"); got != "agento11y" {
		t.Fatalf("trace service.name = %q, want %q", got, "agento11y")
	}

	metricAttrs := captured.metricResourceAttrs(t)
	if got := findAttr(metricAttrs, "service.instance.id"); got != "sess-abc" {
		t.Fatalf("metric service.instance.id = %q, want %q", got, "sess-abc")
	}
	if got := findAttr(metricAttrs, "service.name"); got != "agento11y" {
		t.Fatalf("metric service.name = %q, want %q", got, "agento11y")
	}
}

func TestSetupGeneratesInstanceIDWhenEmpty(t *testing.T) {
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	ids := make([]string, 0, 2)
	for range 2 {
		captured := newOTLPCapture(t)
		t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", captured.server.URL+"/otlp")

		ctx := context.Background()
		providers, err := Setup(ctx, "")
		if err != nil {
			captured.server.Close()
			t.Fatalf("Setup: %v", err)
		}
		_, span := providers.Tracer("test").Start(ctx, "span")
		span.End()
		if err := providers.ForceFlush(); err != nil {
			captured.server.Close()
			t.Fatalf("ForceFlush: %v", err)
		}
		if err := providers.Shutdown(ctx); err != nil {
			captured.server.Close()
			t.Fatalf("Shutdown: %v", err)
		}
		ids = append(ids, findAttr(captured.traceResourceAttrs(t), "service.instance.id"))
		captured.server.Close()
	}

	if ids[0] == "" || ids[1] == "" {
		t.Fatalf("expected non-empty service.instance.id values, got %q and %q", ids[0], ids[1])
	}
	if ids[0] == ids[1] {
		t.Fatalf("expected distinct generated ids, got duplicate %q", ids[0])
	}
}

type otlpCapture struct {
	mu         sync.Mutex
	traceReqs  []*coltracepb.ExportTraceServiceRequest
	metricReqs []*colmetricpb.ExportMetricsServiceRequest
	server     *httptest.Server
}

func newOTLPCapture(t *testing.T) *otlpCapture {
	t.Helper()
	c := &otlpCapture{}
	c.server = newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := readOTLPBody(r)
		if err != nil {
			t.Errorf("read body for %s: %v", r.URL.Path, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/otlp/v1/traces":
			req := &coltracepb.ExportTraceServiceRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				t.Errorf("unmarshal trace export: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			c.traceReqs = append(c.traceReqs, req)
		case "/otlp/v1/metrics":
			req := &colmetricpb.ExportMetricsServiceRequest{}
			if err := proto.Unmarshal(body, req); err != nil {
				t.Errorf("unmarshal metrics export: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			c.metricReqs = append(c.metricReqs, req)
		}
		w.WriteHeader(http.StatusOK)
	}))
	return c
}

func (c *otlpCapture) traceResourceAttrs(t *testing.T) []*commonpb.KeyValue {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.traceReqs) == 0 {
		t.Fatal("no trace exports captured")
	}
	spans := c.traceReqs[0].GetResourceSpans()
	if len(spans) == 0 {
		t.Fatal("trace export has no ResourceSpans")
	}
	return spans[0].GetResource().GetAttributes()
}

func (c *otlpCapture) metricResourceAttrs(t *testing.T) []*commonpb.KeyValue {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.metricReqs) == 0 {
		t.Fatal("no metric exports captured")
	}
	rms := c.metricReqs[0].GetResourceMetrics()
	if len(rms) == 0 {
		t.Fatal("metric export has no ResourceMetrics")
	}
	return rms[0].GetResource().GetAttributes()
}

func findAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

func readOTLPBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
	}
	return body, nil
}
