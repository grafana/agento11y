package claudeevals

import (
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/localhealth"
	"github.com/stretchr/testify/require"
	colmetric "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestTrajectoryOTelDelivery(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, failure string
		content                 bool
		failMetric              bool
	}{
		{name: "implicit metadata only"},
		{name: "implicit full", content: true},
		{name: "implicit metrics cannot fail generation confirmation", failMetric: true},
		{name: "implicit failed export", failure: "export"},
		{name: "implicit partial rejection", failure: "partial"},
		{name: "metadata only", protocol: "otel"},
		{name: "full", protocol: "otel", content: true},
		{name: "metrics cannot fail generation confirmation", protocol: "otel", failMetric: true},
		{name: "failed export", protocol: "otel", failure: "export"},
		{name: "partial rejection", protocol: "otel", failure: "partial"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
				t.Setenv(key, "")
			}
			var mu sync.Mutex
			var spans []*tracepb.Span
			metricNames := map[string]bool{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					_, _ = fmt.Fprintf(w, `{"status":"ok","capabilities":[%q]}`, localhealth.OTelGenerationCapability)
				case "/v1/traces":
					reader := io.Reader(r.Body)
					if r.Header.Get("Content-Encoding") == "gzip" {
						gz, err := gzip.NewReader(reader)
						if err != nil {
							t.Error(err)
							return
						}
						defer gz.Close()
						reader = gz
					}
					body, err := io.ReadAll(reader)
					if err != nil {
						t.Error(err)
						return
					}
					var request coltrace.ExportTraceServiceRequest
					if err := proto.Unmarshal(body, &request); err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					for _, resource := range request.ResourceSpans {
						for _, scope := range resource.ScopeSpans {
							spans = append(spans, scope.Spans...)
						}
					}
					mu.Unlock()
					if tc.failure == "export" {
						http.Error(w, "denied", http.StatusForbidden)
					} else if tc.failure == "partial" {
						w.Header().Set("Content-Type", "application/x-protobuf")
						body, _ := proto.Marshal(&coltrace.ExportTraceServiceResponse{PartialSuccess: &coltrace.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "rejected"}})
						_, _ = w.Write(body)
					}
				case "/v1/metrics":
					reader := io.Reader(r.Body)
					if r.Header.Get("Content-Encoding") == "gzip" {
						gz, err := gzip.NewReader(reader)
						if err != nil {
							t.Error(err)
							return
						}
						defer gz.Close()
						reader = gz
					}
					body, err := io.ReadAll(reader)
					if err != nil {
						t.Error(err)
						return
					}
					var request colmetric.ExportMetricsServiceRequest
					if err := proto.Unmarshal(body, &request); err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					for _, resource := range request.ResourceMetrics {
						for _, scope := range resource.ScopeMetrics {
							for _, metric := range scope.Metrics {
								metricNames[metric.Name] = true
							}
						}
					}
					mu.Unlock()
					if tc.failMetric {
						http.Error(w, "metrics denied", http.StatusForbidden)
					}
				default:
					t.Errorf("unexpected HTTP generation request: %s", r.URL.Path)
					http.Error(w, "unexpected", http.StatusBadRequest)
				}
			}))
			defer server.Close()
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "")
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			telemetry, err := NewTelemetry(context.Background())
			require.NoError(t, err)
			require.Equal(t, agento11y.GenerationExportProtocolOTel, telemetry.protocol)
			require.Empty(t, os.Getenv(agento11y.EnvEnableExperimentalFeatures))
			require.ErrorIs(t, agento11y.RequireExperimental(agento11y.FeatureOTelGenerationExport), agento11y.ErrExperimentalFeatureDisabled)
			defer func() { _ = telemetry.Shutdown(context.Background()) }()
			trajectory := fixtureTrajectory(t)
			err = telemetry.export(context.Background(), trajectory, tc.content)
			if tc.failure != "" {
				require.ErrorContains(t, err, "flush conversation")
				require.Error(t, telemetry.client.Flush(context.Background()), "an empty flush must not clear the failed generation")
				return
			}
			require.NoError(t, err)
			mu.Lock()
			got := append([]*tracepb.Span(nil), spans...)
			hasTokens := metricNames["gen_ai.client.token.usage"]
			hasDuration := metricNames["gen_ai.client.operation.duration"]
			mu.Unlock()
			require.True(t, hasTokens, "eval token usage must still be recorded")
			require.True(t, hasDuration, "eval duration must still be recorded")
			require.Len(t, got, 4)
			marked := 0
			for _, span := range got {
				require.Equal(t, trajectory.Generation.TraceID, hex.EncodeToString(span.TraceId))
				attrs := map[string]string{}
				for _, attr := range span.Attributes {
					attrs[attr.Key] = attr.Value.GetStringValue()
				}
				if attrs["agento11y.record"] != "true" {
					continue
				}
				marked++
				require.Equal(t, trajectory.Generation.SpanID, hex.EncodeToString(span.SpanId))
				require.Equal(t, trajectory.Generation.ID, attrs["agento11y.generation.id"])
				require.Equal(t, tc.content, attrs["gen_ai.output.messages"] != "")
				wantCapture := "metadata_only"
				if tc.content {
					wantCapture = "full"
				}
				var metadata map[string]any
				require.NoError(t, json.Unmarshal([]byte(attrs["agento11y.generation.metadata"]), &metadata))
				require.Equal(t, wantCapture, metadata["agento11y.sdk.content_capture_mode"])
				require.Equal(t, uint64(trajectory.Generation.StartedAt.UnixNano()), span.StartTimeUnixNano)
				require.Equal(t, uint64(trajectory.Generation.CompletedAt.UnixNano()), span.EndTimeUnixNano)
			}
			require.Equal(t, 1, marked)
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			shutdownErr := telemetry.Shutdown(canceled)
			if !tc.failMetric {
				require.NoError(t, shutdownErr)
			}
		})
	}
}
