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
	target, err := ResolveTarget(Target{Headers: headers}, nil)
	require.NoError(t, err)
	key := target.destination()
	headers["X-Snapshot"] = "after"
	target.Headers["X-Snapshot"] = "edited"
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
	require.Equal(t, agento11y.GenerationExportProtocolHTTP, resolved.Protocol)
	require.Empty(t, resolved.OTLPEndpoint)
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
			{name: "default HTTP"},
			{name: "ambient HTTP", envProtocol: " HTTP "},
			{name: "HTTP overrides ambient OTel", protocol: agento11y.GenerationExportProtocolHTTP, envProtocol: "otel"},
			{name: "grpc falls back to HTTP", envProtocol: " GrPc "},
			{name: "none falls back to HTTP", envProtocol: " NONE "},
			{name: "requested OTel", protocol: " OtEl ", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "ambient OTel", envProtocol: " OtEl ", wantOTLP: "http://collector.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "local HTTP", local: true, wantOTLP: "http://localhost:12345/otlp"},
			{name: "local OTel", local: true, envProtocol: "otel", wantOTLP: "http://localhost:12345/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "explicit HTTP collector", otlp: " https://explicit.example.invalid/otlp ", wantOTLP: "https://explicit.example.invalid/otlp"},
			{name: "explicit OTel collector", envProtocol: "otel", otlp: "https://explicit.example.invalid/otlp", wantOTLP: "https://explicit.example.invalid/otlp", wantProtocol: agento11y.GenerationExportProtocolOTel},
			{name: "explicit local collector", local: true, otlp: "http://localhost:23456/collector", wantOTLP: "http://localhost:23456/collector"},
		} {
			t.Run(endpointKey+"/"+tc.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv(endpointKey, "https://collector.example.invalid/otlp/v1/traces")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
				t.Setenv("AGENTO11Y_PROTOCOL", tc.envProtocol)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
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
			explicit bool
		}{
			{name: "default HTTP"},
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

				target := Target{Endpoint: "http://api.example.invalid", TenantID: "tenant", AuthToken: "test-token"}
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
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
	base := Target{Endpoint: "https://api.example.invalid", TenantID: "tenant", Protocol: agento11y.GenerationExportProtocolOTel, OTLPEndpoint: "https://collector.example.invalid"}
	otel, err := ResolveTarget(base, nil)
	require.NoError(t, err)
	require.Empty(t, otel.AuthToken)
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
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
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
		Protocol:     agento11y.GenerationExportProtocolOTel,
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
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
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
	_, _, err := NewTargetExporter(t.Context(), Target{Endpoint: server.URL, Protocol: agento11y.GenerationExportProtocolOTel}, nil)
	require.ErrorContains(t, err, "restart")
	require.Zero(t, generationRequests.Load())
}

func TestResolvedOTelTargetFreezesOptIn(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
		}
	}))
	defer collector.Close()
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
	target, err := ResolveTarget(Target{Endpoint: "https://api.example.invalid", TenantID: "tenant", Protocol: agento11y.GenerationExportProtocolOTel, OTLPEndpoint: collector.URL}, nil)
	require.NoError(t, err)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	_, cleanup, err := NewTargetExporter(t.Context(), target, nil)
	require.NoError(t, err)
	require.NoError(t, cleanup(context.Background()))
}
