package history

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestResolveTargetFreezesEnvironmentAndCallerMaps(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENDPOINT", "https://api.example.invalid")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant-before")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "token-before")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://collector.example.invalid")
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
	headers := map[string]string{"X-Snapshot": "before"}
	otlpHeaders := map[string]string{"X-Collector": "before"}
	target, err := ResolveTarget(Target{Headers: headers, OTLPHeaders: otlpHeaders}, nil)
	require.NoError(t, err)
	key := target.destination()
	headers["X-Snapshot"] = "after"
	otlpHeaders["X-Collector"] = "after"
	target.Headers["X-Snapshot"] = "edited"
	target.OTLPHeaders["X-Collector"] = "edited"
	target.Protocol = agento11y.GenerationExportProtocolHTTP
	target.OTLPEndpoint = "https://edited-collector.example.invalid"
	target.Endpoint = "https://edited.example.invalid"
	target.TenantID = "edited"
	t.Setenv("AGENTO11Y_ENDPOINT", "https://after.example.invalid")
	t.Setenv("AGENTO11Y_PROTOCOL", "unsupported")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant-after")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "token-after")
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")
	resolved, err := ResolveTarget(target, nil)
	require.NoError(t, err)
	require.Equal(t, "https://api.example.invalid", resolved.Endpoint)
	require.Equal(t, "tenant-before", resolved.TenantID)
	require.Equal(t, "token-before", resolved.AuthToken)
	require.Equal(t, "metadata_only", resolved.ContentMode)
	require.Equal(t, "before", resolved.Headers["X-Snapshot"])
	require.Equal(t, "before", resolved.OTLPHeaders["X-Collector"])
	require.Equal(t, agento11y.GenerationExportProtocolOTel, resolved.Protocol)
	require.Equal(t, "https://collector.example.invalid", resolved.OTLPEndpoint)
	require.Equal(t, key, target.destination())
}

func TestResolveTargetFreezesEffectiveOTLPRoute(t *testing.T) {
	for _, endpointKey := range []string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		for _, tc := range []struct {
			name         string
			protocol     agento11y.GenerationExportProtocol
			envProtocol  string
			otlp         string
			local        bool
			wantOTLP     string
			wantProtocol agento11y.GenerationExportProtocol
		}{
			{name: "implicit OTel", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "whitespace OTel", envProtocol: " \t ", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "ambient HTTP", envProtocol: " HTTP "},
			{name: "HTTP overrides ambient OTel", protocol: agento11y.GenerationExportProtocolHTTP, envProtocol: "otel"},
			{name: "grpc falls back to HTTP", envProtocol: " GrPc "},
			{name: "none falls back to HTTP", envProtocol: " NONE "},
			{name: "OTel overrides ambient HTTP", protocol: " OtEl ", envProtocol: "http", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "OTel overrides invalid ambient protocol", protocol: agento11y.GenerationExportProtocolOTel, envProtocol: "unsupported", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "ambient OTel", envProtocol: " OtEl ", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "local HTTP", local: true, protocol: agento11y.GenerationExportProtocolHTTP},
			{name: "local implicit OTel", local: true, wantOTLP: "http://localhost:12345/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "local OTel", local: true, envProtocol: "otel", wantOTLP: "http://localhost:12345/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "explicit HTTP collector", protocol: agento11y.GenerationExportProtocolHTTP, otlp: " https://explicit.example.invalid/otlp ", wantOTLP: "https://explicit.example.invalid/otlp"},
			{name: "explicit OTel collector", envProtocol: "otel", otlp: "https://explicit.example.invalid/otlp", wantOTLP: "https://explicit.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "explicit local collector", local: true, otlp: "http://localhost:23456/collector", wantOTLP: "http://localhost:23456/collector", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "HTTP overrides invalid ambient protocol", protocol: agento11y.GenerationExportProtocolHTTP, envProtocol: "unsupported"},
		} {
			t.Run(endpointKey+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv(endpointKey, "https://collector.example.invalid/otlp/v1/traces")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
				t.Setenv("AGENTO11Y_PROTOCOL", tc.envProtocol)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
				input := Target{
					Endpoint: "https://api.example.invalid", TenantID: "tenant", AuthToken: "token",
					Protocol: tc.protocol, OTLPEndpoint: tc.otlp, ContentMode: "metadata_only",
					Headers: map[string]string{"authorization": "Bearer must-not-leak", ForwardMarkerHeader: "must-be-replaced"},
				}
				if tc.local {
					input.Endpoint = "http://localhost:12345"
				}
				target, err := ResolveTarget(input, nil)
				require.NoError(t, err)
				wantProtocol := tc.wantProtocol
				if wantProtocol == "" {
					wantProtocol = agento11y.GenerationExportProtocolHTTP
				}
				require.Equal(t, wantProtocol, target.Protocol)
				require.Equal(t, tc.wantOTLP, target.OTLPEndpoint)
				if tc.local {
					require.Equal(t, map[string]string{ForwardMarkerHeader: "1"}, target.Headers)
					require.Equal(t, target.Headers, target.OTLPHeaders)
					require.Equal(t, "full", target.ContentMode)
				}
				target.OTLPEndpoint = "https://edited.example.invalid"
				t.Setenv(endpointKey, "https://after.example.invalid")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "false")
				t.Setenv("AGENTO11Y_PROTOCOL", "unsupported")
				frozen, err := ResolveTarget(target, nil)
				require.NoError(t, err)
				require.Equal(t, tc.wantOTLP, frozen.OTLPEndpoint)
				require.Equal(t, wantProtocol, frozen.Protocol)
			})
		}
	}
}

func TestTargetExporterHTTPOnlyUsesExplicitOTLPRoute(t *testing.T) {
	for _, endpointKey := range []string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		for _, tc := range []struct {
			name     string
			protocol string
			override agento11y.GenerationExportProtocol
			explicit bool
		}{
			{name: "HTTP overrides ambient OTel", protocol: "otel", override: agento11y.GenerationExportProtocolHTTP},
			{name: "ambient HTTP", protocol: "http"},
			{name: "grpc falls back to HTTP", protocol: "grpc"},
			{name: "none falls back to HTTP", protocol: "none"},
			{name: "explicit collector control", protocol: "http", explicit: true},
		} {
			t.Run(endpointKey+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
				var generations, traces, metrics, ambient atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasPrefix(r.URL.Path, "/ambient/") {
						ambient.Add(1)
					}
					switch {
					case r.URL.Path == "/api/v1/generations:export":
						generations.Add(1)
						_, _ = io.WriteString(w, acceptAllGenerations(r))
					case strings.HasSuffix(r.URL.Path, "/v1/traces"):
						traces.Add(1)
					case strings.HasSuffix(r.URL.Path, "/v1/metrics"):
						metrics.Add(1)
					default:
						t.Errorf("unexpected request path %q", r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				t.Setenv(endpointKey, server.URL+"/ambient")

				// Exercise the nonlocal policy without DNS or external traffic.
				previous := http.DefaultTransport
				transport := previous.(*http.Transport).Clone()
				transport.Proxy = nil
				transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return new(net.Dialer).DialContext(ctx, network, server.Listener.Addr().String())
				}
				http.DefaultTransport = transport
				defer func() {
					transport.CloseIdleConnections()
					http.DefaultTransport = previous
				}()

				target := Target{Endpoint: "http://api.example.invalid", TenantID: "tenant", AuthToken: "test-token", Protocol: tc.override}
				if tc.explicit {
					target.OTLPEndpoint = server.URL + "/explicit"
				}
				exporter, cleanup, err := NewTargetExporter(t.Context(), target, nil)
				require.NoError(t, err)
				defer func() {
					// Shutdown drains both telemetry providers before checking absence.
					require.NoError(t, cleanup(context.Background()))
					require.EqualValues(t, 1, generations.Load())
					require.Zero(t, ambient.Load())
					if tc.explicit {
						require.Positive(t, traces.Load())
						require.Positive(t, metrics.Load())
					} else {
						require.Zero(t, traces.Load())
						require.Zero(t, metrics.Load())
					}
				}()
				require.NoError(t, exporter.ExportHistoricalGeneration(t.Context(), turn("http-history")))
				require.NoError(t, exporter.Flush(t.Context()))
			})
		}
	}
}

func TestResolveCloudOTelRequiresIdentityNotAPIToken(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	base := Target{Endpoint: "https://api.example.invalid", TenantID: "tenant", OTLPEndpoint: "https://collector.example.invalid"}
	otel, err := ResolveTarget(base, nil)
	require.NoError(t, err)
	require.Empty(t, otel.AuthToken)
	require.Equal(t, agento11y.GenerationExportProtocolOTel, otel.Protocol)
	base.Protocol, base.AuthToken = agento11y.GenerationExportProtocolHTTP, "rotated-token"
	base.OTLPEndpoint = "https://other-collector.example.invalid"
	httpTarget, err := ResolveTarget(base, nil)
	require.NoError(t, err)
	require.Equal(t, otel.destination(), httpTarget.destination())
	base.Protocol, base.AuthToken = agento11y.GenerationExportProtocolOTel, ""
	for _, field := range []string{"endpoint", "tenant", "collector", "remote collector for local import"} {
		t.Run(field, func(t *testing.T) {
			bad := base
			switch field {
			case "endpoint":
				bad.Endpoint = ""
			case "tenant":
				bad.TenantID = ""
			case "collector":
				bad.OTLPEndpoint = ""
			case "remote collector for local import":
				bad.Endpoint = "http://127.0.0.1:4318"
			}
			_, err := ResolveTarget(bad, nil)
			require.Error(t, err)
		})
	}
}

func TestTargetExporterOTelUsesSnapshotAndNoGenerationHTTP(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	var mu sync.Mutex
	var spans []*tracepb.Span
	var generationHTTP int
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(r.URL.Path, "generations") {
			generationHTTP++
		}
		if !strings.HasSuffix(r.URL.Path, "/traces") {
			return
		}
		assert.Equal(t, "Bearer snapshot", r.Header.Get("Authorization"))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request coltracepb.ExportTraceServiceRequest
		if err := proto.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		for _, rs := range request.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				spans = append(spans, ss.Spans...)
			}
		}
	}))
	t.Cleanup(collector.Close)
	target, err := ResolveTarget(Target{
		Endpoint: "https://api.example.invalid", TenantID: "tenant",
		OTLPEndpoint: collector.URL, OTLPHeaders: map[string]string{"Authorization": "Bearer snapshot"},
		ContentMode: "full_with_metadata_spans",
	}, nil)
	require.NoError(t, err)
	t.Setenv("AGENTO11Y_PROTOCOL", "http")
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "after")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "after")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer after")
	exporter, cleanup, err := NewTargetExporter(t.Context(), target, nil)
	require.NoError(t, err)
	cleanupCtx, cancelCleanup := context.WithCancel(context.Background())
	cancelCleanup()
	t.Cleanup(func() { require.NoError(t, cleanup(cleanupCtx)) })
	started := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	gen := turn("turn-1")
	gen.Gen.ID, gen.Gen.StartedAt, gen.Gen.CompletedAt = "snapshot-gen", started, started.Add(time.Second)
	gen.Gen.ParentGenerationIDs = []string{"parent-gen"}
	gen.Gen.Output = []agento11y.Message{agento11y.AssistantTextMessage("historical content")}
	require.NoError(t, exporter.ExportHistoricalGeneration(t.Context(), gen))
	require.NoError(t, exporter.Flush(t.Context()))
	mu.Lock()
	defer mu.Unlock()
	require.Zero(t, generationHTTP)
	require.Len(t, spans, 1)
	require.Equal(t, uint64(started.UnixNano()), spans[0].StartTimeUnixNano)
	require.Equal(t, uint64(started.Add(time.Second).UnixNano()), spans[0].EndTimeUnixNano)
	require.Len(t, spans[0].TraceId, 16)
	require.Len(t, spans[0].SpanId, 8)
	attrs := map[string]string{}
	var parents []string
	for _, a := range spans[0].Attributes {
		attrs[a.Key] = a.Value.GetStringValue()
		if a.Key == "agento11y.generation.parent_generation_ids" {
			for _, value := range a.Value.GetArrayValue().GetValues() {
				parents = append(parents, value.GetStringValue())
			}
		}
	}
	require.Equal(t, []string{"parent-gen"}, parents)
	require.Equal(t, "chat", attrs["gen_ai.operation.name"])
	require.Equal(t, "true", attrs["agento11y.record"])
	require.Equal(t, "snapshot-gen", attrs["agento11y.generation.id"])
	require.Contains(t, attrs["gen_ai.output.messages"], "historical content")
	require.Equal(t, "after", os.Getenv("AGENTO11Y_AUTH_TOKEN"), "exporter must not mutate environment")
}

func TestTargetExporterRejectsOldLocalDaemon(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	var generationRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"status":"ok"}`)
			return
		}
		if strings.Contains(r.URL.Path, "traces") || strings.Contains(r.URL.Path, "generations") {
			generationRequests.Add(1)
		}
	}))
	defer server.Close()
	_, _, err := NewTargetExporter(t.Context(), Target{Endpoint: server.URL}, nil)
	require.ErrorContains(t, err, "restart")
	require.Zero(t, generationRequests.Load())
}

func TestLocalTargetExporterIsolatesImplicitOTel(t *testing.T) {
	for _, protocol := range []agento11y.GenerationExportProtocol{"", agento11y.GenerationExportProtocolOTel, agento11y.GenerationExportProtocolHTTP} {
		t.Run(string(protocol), func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://cloud.example.invalid")
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "cloud-tenant")
			t.Setenv("AGENTO11Y_AUTH_TOKEN", "cloud-token")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer cloud")
			var mu sync.Mutex
			var bodies []byte
			var paths []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					return
				}
				assert.Empty(t, r.Header.Get("Authorization"))
				assert.Equal(t, "1", r.Header.Get(ForwardMarkerHeader))
				assert.Equal(t, "snapshot", r.Header.Get("X-Local"))
				if r.URL.Path == "/otlp/v1/metrics" {
					return // Empty provider resource metrics are not live usage.
				}
				if r.URL.Path == "/api/v1/generations:export" {
					body, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					mu.Lock()
					bodies = append(bodies, body...)
					paths = append(paths, r.URL.Path)
					mu.Unlock()
					r.Body = io.NopCloser(strings.NewReader(string(body)))
					_, _ = io.WriteString(w, acceptAllGenerations(r))
					return
				}
				assert.Equal(t, "/otlp/v1/traces", r.URL.Path)
				body, err := io.ReadAll(r.Body)
				assert.NoError(t, err)
				mu.Lock()
				bodies = append(bodies, body...)
				paths = append(paths, r.URL.Path)
				mu.Unlock()
			}))
			defer server.Close()
			exporter, cleanup, err := NewTargetExporter(t.Context(), Target{
				Endpoint: server.URL, Protocol: protocol, ContentMode: "metadata_only",
				AuthToken: "explicit-cloud-token",
				Headers:   map[string]string{"aUtHoRiZaTiOn": "Bearer explicit", ForwardMarkerHeader: "0", "X-Local": "snapshot"},
			}, nil)
			require.NoError(t, err)
			defer func() { require.NoError(t, cleanup(context.Background())) }()
			gen := turn("local")
			gen.Gen.Output = []agento11y.Message{agento11y.AssistantTextMessage("full historical content")}
			require.NoError(t, exporter.ExportHistoricalGeneration(t.Context(), gen))
			require.NoError(t, exporter.Flush(t.Context()))
			mu.Lock()
			defer mu.Unlock()
			require.Contains(t, string(bodies), "full historical content", "local imports must force full capture")
			require.NotContains(t, string(bodies), "cloud-token")
			if protocol == agento11y.GenerationExportProtocolHTTP {
				require.Equal(t, []string{"/api/v1/generations:export"}, paths)
			} else {
				require.Equal(t, []string{"/otlp/v1/traces"}, paths)
			}
		})
	}
}

func TestResolveTargetSelectsBeforeDestinationValidation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		protocol agento11y.GenerationExportProtocol
		ambient  string
		endpoint string
		otlp     string
		want     string
	}{
		{name: "unknown before missing endpoint", protocol: "invalid", want: "unsupported Target.Protocol"},
		{name: "unknown ambient before missing endpoint", ambient: "invalid", want: "unsupported AGENTO11Y_PROTOCOL"},
		{name: "implicit OTel missing collector", endpoint: "https://api.example.invalid", want: "OTLP base endpoint"},
		{name: "whitespace override selects OTel", protocol: " \t ", ambient: "http", endpoint: "https://api.example.invalid", want: "OTLP base endpoint"},
		{name: "OTel overrides HTTP and validates collector", protocol: " OtEl ", ambient: "http", endpoint: "https://api.example.invalid", otlp: "grpc://collector.example.invalid", want: "OTLP base endpoint"},
		{name: "HTTP ignores invalid ambient protocol", protocol: " HTTP ", ambient: "invalid", endpoint: "https://api.example.invalid"},
		{name: "local OTel rejects remote route", endpoint: "http://localhost:12345", otlp: "https://collector.example.invalid", want: "loopback OTLP"},
		{name: "local HTTP rejects explicit remote route", protocol: "http", endpoint: "http://localhost:12345", otlp: "https://collector.example.invalid", want: "loopback OTLP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_PROTOCOL", tc.ambient)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			_, err := ResolveTarget(Target{Protocol: tc.protocol, Endpoint: tc.endpoint, OTLPEndpoint: tc.otlp, TenantID: "tenant", AuthToken: "token"}, nil)
			if tc.want != "" {
				require.ErrorContains(t, err, tc.want)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestResolvedOTelTargetNeedsNoGlobalOptIn(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
		}
	}))
	defer collector.Close()
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	target, err := ResolveTarget(Target{Endpoint: "https://api.example.invalid", TenantID: "tenant", OTLPEndpoint: collector.URL}, nil)
	require.NoError(t, err)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	_, cleanup, err := NewTargetExporter(t.Context(), target, nil)
	require.NoError(t, err)
	require.NoError(t, cleanup(context.Background()))
}
