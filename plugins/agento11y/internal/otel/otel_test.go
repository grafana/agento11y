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
	"os"
	"sync"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"

	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	"google.golang.org/protobuf/proto"
)

func TestEndpointFromEnvPrefersSigilPrefix(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "https://sigil.example/otlp")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example/otlp")

	if got := EndpointFromEnv(); got != "https://sigil.example/otlp" {
		t.Fatalf("EndpointFromEnv() = %q", got)
	}
}

func TestEndpointFromEnvFallsBackToStandardOtel(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://otel.example/otlp")

	if got := EndpointFromEnv(); got != "https://otel.example/otlp" {
		t.Fatalf("EndpointFromEnv() = %q", got)
	}
}

func TestExporterConfigUsesSigilPrefixedValues(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
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
	envconfig.PinAliasEnvBlank(t)
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
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
	t.Setenv("SIGIL_AUTH_TOKEN", "token")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer explicit")

	cfg := exporterConfigFromEnv("https://sigil.example/otlp")

	if got := cfg.headers["Authorization"]; got != "Bearer explicit" {
		t.Fatalf("Authorization = %q", got)
	}
}

func TestProbeConfig(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
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
	envconfig.PinAliasEnvBlank(t)
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
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if _, _, ok := ProbeConfig(); ok {
		t.Fatal("expected ok=false when no OTLP endpoint is configured")
	}
}

func TestSignalEndpointURLAppendsOTLPHTTPPaths(t *testing.T) {
	if got := SignalEndpointURL("https://otlp.example/otlp", "traces"); got != "https://otlp.example/otlp/v1/traces" {
		t.Fatalf("trace endpoint = %q", got)
	}
	if got := SignalEndpointURL("https://otlp.example/otlp/v1/traces", "metrics"); got != "https://otlp.example/otlp/v1/metrics" {
		t.Fatalf("metric endpoint = %q", got)
	}
}

// TestExporterHeaders covers the exported header builder the local daemon's
// Cloud forwarder shares with the exporter: explicit headers pass through, an
// explicit Authorization wins, and OTEL_AUTH_TOKEN outranks AUTH_TOKEN.
func TestExporterHeaders(t *testing.T) {
	basic := func(pair string) string {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(pair))
	}
	cases := []struct {
		name       string
		raw        string
		tenant     string
		otelToken  string
		authToken  string
		wantAuth   string
		wantExtras map[string]string
	}{
		{
			name: "synthesizes_from_auth_token", tenant: "tenant", authToken: "token",
			wantAuth: basic("tenant:token"),
		},
		{
			name: "otel_token_wins", tenant: "tenant", otelToken: "otel", authToken: "token",
			wantAuth: basic("tenant:otel"),
		},
		{
			name: "explicit_authorization_wins", raw: "Authorization=Bearer explicit",
			tenant: "tenant", authToken: "token", wantAuth: "Bearer explicit",
		},
		{
			name: "keeps_other_headers", raw: "X-Extra=ok", tenant: "tenant", authToken: "token",
			wantAuth: basic("tenant:token"), wantExtras: map[string]string{"X-Extra": "ok"},
		},
		{
			name: "no_credentials_means_no_authorization", raw: "X-Extra=ok",
			wantExtras: map[string]string{"X-Extra": "ok"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ExporterHeaders(tc.raw, tc.tenant, tc.otelToken, tc.authToken)
			if got["Authorization"] != tc.wantAuth {
				t.Fatalf("Authorization = %q, want %q", got["Authorization"], tc.wantAuth)
			}
			for k, v := range tc.wantExtras {
				if got[k] != v {
					t.Fatalf("header %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestBasicAuthHeaderValue(t *testing.T) {
	cases := []struct {
		name   string
		tenant string
		token  string
		want   string
		wantOK bool
	}{
		{name: "tenant and token", tenant: "123456", token: "glc_secret", want: "Basic " + base64.StdEncoding.EncodeToString([]byte("123456:glc_secret")), wantOK: true},
		{name: "trims whitespace", tenant: "  123456  ", token: "  glc_secret  ", want: "Basic " + base64.StdEncoding.EncodeToString([]byte("123456:glc_secret")), wantOK: true},
		{name: "blank tenant", tenant: "  ", token: "glc_secret"},
		{name: "blank token", tenant: "123456", token: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BasicAuthHeaderValue(tc.tenant, tc.token)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if got != tc.want {
				t.Fatalf("value = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolveExporterConfigOptions covers the explicit endpoint and header
// overrides. A daemon resolves its Cloud forwarding from the same process
// environment, so the overrides must not read through to it or change it.
func TestResolveExporterConfigOptions(t *testing.T) {
	const envEndpoint = "https://env.example:4318"
	cases := []struct {
		name         string
		envEndpoint  string
		envInsecure  string
		opts         Options
		wantOK       bool
		wantURL      string
		wantHeaders  map[string]string
		wantInsecure bool
	}{
		{
			name:        "zero options keep the environment",
			envEndpoint: envEndpoint,
			wantOK:      true,
			wantURL:     envEndpoint,
			wantHeaders: map[string]string{"Authorization": "Basic env-credential"},
		},
		{
			name:        "endpoint override keeps the environment headers",
			envEndpoint: envEndpoint,
			opts:        Options{Endpoint: "https://import.example:4318"},
			wantOK:      true,
			wantURL:     "https://import.example:4318",
			wantHeaders: map[string]string{"Authorization": "Basic env-credential"},
		},
		{
			name:        "headers replace rather than merge",
			envEndpoint: envEndpoint,
			opts: Options{
				Endpoint: "https://import.example:4318",
				Headers:  map[string]string{"Authorization": "Bearer test"},
			},
			wantOK:      true,
			wantURL:     "https://import.example:4318",
			wantHeaders: map[string]string{"Authorization": "Bearer test"},
		},
		{
			name:        "an empty header map sends no headers",
			envEndpoint: envEndpoint,
			opts:        Options{Headers: map[string]string{}},
			wantOK:      true,
			wantURL:     envEndpoint,
			wantHeaders: map[string]string{},
		},
		{
			name:         "the environment decides the transport for its own endpoint",
			envEndpoint:  envEndpoint,
			envInsecure:  "true",
			wantOK:       true,
			wantURL:      envEndpoint,
			wantHeaders:  map[string]string{"Authorization": "Basic env-credential"},
			wantInsecure: true,
		},
		{
			name:        "an https override is not downgraded by the environment",
			envEndpoint: envEndpoint,
			envInsecure: "true",
			opts:        Options{Endpoint: "https://import.example:4318"},
			wantOK:      true,
			wantURL:     "https://import.example:4318",
			wantHeaders: map[string]string{"Authorization": "Basic env-credential"},
		},
		{
			name:         "an http override sends cleartext without the environment saying so",
			envEndpoint:  envEndpoint,
			opts:         Options{Endpoint: "http://127.0.0.1:4318"},
			wantOK:       true,
			wantURL:      "http://127.0.0.1:4318",
			wantHeaders:  map[string]string{"Authorization": "Basic env-credential"},
			wantInsecure: true,
		},
		{
			name: "no endpoint anywhere reports nothing to export to",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Basic env-credential")
			t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", tc.envInsecure)
			if tc.envEndpoint != "" {
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", tc.envEndpoint)
			}

			cfg, ok := resolveExporterConfig(tc.opts)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if cfg.endpoint != tc.wantURL {
				t.Errorf("endpoint = %q, want %q", cfg.endpoint, tc.wantURL)
			}
			if cfg.insecure != tc.wantInsecure {
				t.Errorf("insecure = %v, want %v", cfg.insecure, tc.wantInsecure)
			}
			if len(cfg.headers) != len(tc.wantHeaders) {
				t.Fatalf("headers = %+v, want %+v", cfg.headers, tc.wantHeaders)
			}
			for k, v := range tc.wantHeaders {
				if cfg.headers[k] != v {
					t.Errorf("header %s = %q, want %q", k, cfg.headers[k], v)
				}
			}
			// The overrides must leave the environment other resolvers read
			// (the daemon's forwarding config) exactly as it was.
			if got := os.Getenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT"); got != tc.envEndpoint {
				t.Errorf("env endpoint = %q, want %q", got, tc.envEndpoint)
			}
			if got := os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"); got != "Authorization=Basic env-credential" {
				t.Errorf("env headers = %q, want them unchanged", got)
			}
		})
	}
}

// TestSetupWithOptionsExportsToExplicitTarget stands up two collectors and
// asserts the exporter uses the caller's endpoint and header, not the
// environment's. It also pins that the call leaves the process environment
// alone: an in-process caller shares it with the daemon's forwarding config.
func TestSetupWithOptionsExportsToExplicitTarget(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")
	t.Setenv("OTEL_SERVICE_NAME", "")
	envBefore := map[string]string{
		"OTEL_EXPORTER_OTLP_INSECURE":         "true",
		"OTEL_EXPORTER_OTLP_TRACES_INSECURE":  "true",
		"OTEL_EXPORTER_OTLP_METRICS_INSECURE": "true",
		"OTEL_SERVICE_NAME":                   "",
	}
	for k, v := range envBefore {
		t.Setenv(k, v)
	}

	var mu sync.Mutex
	var envHits int
	envTarget := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		envHits++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer envTarget.Close()

	auth := make(chan string, 4)
	importTarget := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/otlp/v1/traces" {
			auth <- r.Header.Get("Authorization")
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer importTarget.Close()

	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", envTarget.URL+"/otlp")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "env-tenant")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "env-token")

	ctx := context.Background()
	providers, err := SetupWithOptions(ctx, "import-run", Options{
		Endpoint: importTarget.URL + "/otlp",
		Headers:  map[string]string{"Authorization": "Bearer test"},
	})
	if err != nil {
		t.Fatalf("SetupWithOptions: %v", err)
	}
	_, span := providers.Tracer("test").Start(ctx, "span")
	span.End()
	if err := providers.ForceFlush(); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
	if err := providers.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case got := <-auth:
		if got != "Bearer test" {
			t.Fatalf("Authorization = %q, want %q", got, "Bearer test")
		}
	default:
		t.Fatal("no trace export reached the explicit endpoint")
	}
	for k, want := range envBefore {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q after SetupWithOptions, want %q", k, got, want)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if envHits != 0 {
		t.Fatalf("environment endpoint received %d requests, want 0", envHits)
	}
}

func TestSetupExportsToSignalSpecificPaths(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
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

// TestSetupAttachesServiceInstanceID also covers the default service name,
// which goes on the resource because OTEL_SERVICE_NAME is blank here.
func TestSetupAttachesServiceInstanceID(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("OTEL_SERVICE_NAME", "")

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
	envconfig.PinAliasEnvBlank(t)
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
