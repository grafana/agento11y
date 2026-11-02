package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
	"github.com/stretchr/testify/require"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type destinationDelivery struct {
	path   string
	body   []byte
	header http.Header
}

type destinationSink struct {
	server *httptest.Server
	calls  chan destinationDelivery
}

func newDestinationSink(t *testing.T, outcome string) *destinationSink {
	t.Helper()
	sink := &destinationSink{calls: make(chan destinationDelivery, 32)}
	sink.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "read request", http.StatusBadRequest)
			return
		}
		sink.calls <- destinationDelivery{r.URL.Path, body, r.Header.Clone()}
		if outcome == "redirect" {
			w.Header().Set("Location", sink.server.URL+"/redirected")
			w.WriteHeader(http.StatusTemporaryRedirect)
			return
		}
		if outcome == "reject" {
			w.Header().Set("Content-Type", wire.ContentTypeJSON)
			switch r.URL.Path {
			case wire.GenerationExportHTTPPath:
				var request generationsRequest
				_ = json.Unmarshal(body, &request)
				response := generationsResponse{}
				for _, raw := range request.Generations {
					var generation struct {
						ID string `json:"id"`
					}
					_ = json.Unmarshal(raw, &generation)
					response.Results = append(response.Results, generationResult{GenerationID: generation.ID, Error: "remote rejection"})
				}
				_ = json.NewEncoder(w).Encode(response)
			case "/v1/traces":
				_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"1"}}`)
			case "/v1/metrics":
				_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedDataPoints":"1"}}`)
			}
			return
		}
		if r.URL.Path == wire.GenerationExportHTTPPath {
			r.Body = io.NopCloser(bytes.NewReader(body))
			writeAcceptedGenerationResponse(w, r)
			return
		}
		w.Header().Set("Content-Type", wire.ContentTypeProto)
	}))
	t.Cleanup(sink.server.Close)
	return sink
}

func writeDestinationSidecar(t *testing.T, configPath string, sinks ...*destinationSink) {
	t.Helper()
	var data []byte
	for i, sink := range sinks {
		data = fmt.Appendf(data, "[[destinations]]\nname = 'extra-%d'\nendpoint = '%s'\ntenant_id = 'extra-tenant-%d'\ntoken = 'extra-token-%d'\notlp_endpoint = '%s'\n", i, sink.server.URL, i, i, sink.server.URL)
	}
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(configPath), forwardingFile), data, 0o600))
}

func newDestinationReceiver(t *testing.T, primary bool, mode string, enabled bool, sinks ...*destinationSink) *Server {
	t.Helper()
	clearForwardEnv(t)
	config := map[string]string{
		"AGENTO11Y_LOCAL_FORWARD":                fmt.Sprint(enabled),
		"AGENTO11Y_CONTENT_CAPTURE_MODE":         mode,
		"AGENTO11Y_PROTOCOL":                     "otel",
		"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
	}
	extras := sinks
	if primary {
		config["AGENTO11Y_ENDPOINT"] = sinks[0].server.URL
		config["AGENTO11Y_AUTH_TENANT_ID"] = "primary-tenant"
		config["AGENTO11Y_AUTH_TOKEN"] = "primary-token"
		config["AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT"] = sinks[0].server.URL
		extras = sinks[1:]
	}
	path := writeConfigEnvFile(t, config)
	writeDestinationSidecar(t, path, extras...)
	storage, err := NewStorage(t.TempDir())
	require.NoError(t, err)
	receiver := NewServer(storage, nil, path)
	// httptest TLS certificates share a test CA, so this client trusts both sinks.
	if len(sinks) > 0 {
		receiver.forward.client = sinks[0].server.Client()
	}
	t.Cleanup(receiver.Close)
	return receiver
}

func destinationHTTPBatch(t *testing.T, id string) []byte {
	t.Helper()
	generation := contentRichGeneration(t)
	generation.Id = id
	generation.StartedAt = timestamppb.New(time.Unix(1700000000, 123456789))
	generation.CompletedAt = timestamppb.New(time.Unix(1700000002, 987654321))
	body, err := wire.MarshalExportGenerationsJSON(&agento11yv1.ExportGenerationsRequest{Generations: []*agento11yv1.Generation{generation}})
	require.NoError(t, err)
	return body
}

func destinationTraceBatch(t *testing.T, id string) []byte {
	t.Helper()
	client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
	_, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ID: id, ConversationID: "otel-destinations", ConversationTitle: "secret title",
		SystemPrompt: "secret system", StartedAt: time.Unix(1700000000, 123456789),
		Model: agento11y.ModelRef{Provider: "openai", Name: "model"},
	})
	recorder.SetResult(agento11y.Generation{
		Input:       []agento11y.Message{agento11y.UserTextMessage("secret input")},
		Output:      []agento11y.Message{agento11y.AssistantTextMessage("secret output")},
		CompletedAt: time.Unix(1700000002, 987654321),
	}, nil)
	recorder.End()
	require.NoError(t, client.Flush(context.Background()))
	transport.request.ResourceSpans[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{stringKV("service.name", "destination-test")}}
	transport.request.ResourceSpans[0].ScopeSpans[0].Spans[0].ParentSpanId = bytes.Repeat([]byte{3}, 8)
	body, err := proto.Marshal(&transport.request)
	require.NoError(t, err)
	return body
}

func destinationMetricsBatch(t *testing.T) []byte {
	t.Helper()
	sum := 4.25
	request := &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{stringKV("service.name", "destination-test")}},
		ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{
			Name: "gen_ai.client.operation.duration", Unit: "s",
			Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{
				AggregationTemporality: metricpb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
				DataPoints: []*metricpb.HistogramDataPoint{{
					StartTimeUnixNano: 1700000000123456789, TimeUnixNano: 1700000002987654321,
					Count: 3, Sum: &sum, ExplicitBounds: []float64{1, 2}, BucketCounts: []uint64{1, 1, 1},
					Attributes: []*commonpb.KeyValue{stringKV("gen_ai.tool.name", "read")},
				}},
			}},
		}}}},
	}}}
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	return body
}

// A remote error must never become a rejection of already persisted local data.
func sendDestinationIngress(t *testing.T, receiver *Server, signal string, body []byte, marker bool) {
	t.Helper()
	path, contentType := wire.GenerationExportHTTPPath, wire.ContentTypeJSON
	if signal != "generations" {
		path, contentType = "/otlp/v1/"+signal, wire.ContentTypeProto
	}
	request := newLocalRequest(http.MethodPost, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	if marker {
		request.Header.Set(ForwardMarkerHeader, "1")
	}
	response := httptest.NewRecorder()
	receiver.ServeHTTP(response, request)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	switch signal {
	case "generations":
		var ack generationsResponse
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &ack))
		original, err := wire.UnmarshalExportGenerationsJSON(body)
		require.NoError(t, err)
		require.Len(t, ack.Results, len(original.Generations))
		for i, result := range ack.Results {
			require.Equal(t, original.Generations[i].Id, result.GenerationID)
			require.True(t, result.Accepted)
			require.Empty(t, result.Error)
		}
	case "traces":
		var ack coltracepb.ExportTraceServiceResponse
		require.NoError(t, proto.Unmarshal(response.Body.Bytes(), &ack))
		require.Zero(t, ack.GetPartialSuccess().GetRejectedSpans())
	case "metrics":
		var ack colmetricpb.ExportMetricsServiceResponse
		require.NoError(t, proto.Unmarshal(response.Body.Bytes(), &ack))
		require.Zero(t, ack.GetPartialSuccess().GetRejectedDataPoints())
	}
	receiver.forward.wait()
}

func takeDestinationDelivery(t *testing.T, sink *destinationSink, path string) destinationDelivery {
	t.Helper()
	require.Len(t, sink.calls, 1, "exactly one signal request; no cross-protocol export or redirect follow")
	delivery := <-sink.calls
	require.Equal(t, path, delivery.path)
	require.Equal(t, "1", delivery.header.Get(ForwardMarkerHeader))
	return delivery
}

func TestReceiverForwardDestinations(t *testing.T) {
	for _, primary := range []bool{true, false} {
		for _, mode := range []string{"full", "metadata_only"} {
			t.Run(fmt.Sprintf("primary=%t/%s", primary, mode), func(t *testing.T) {
				first, second := newDestinationSink(t, ""), newDestinationSink(t, "")
				receiver := newDestinationReceiver(t, primary, mode, true, first, second)

				body := destinationHTTPBatch(t, "http-generation")
				sendDestinationIngress(t, receiver, "generations", body, false)
				original, err := wire.UnmarshalExportGenerationsJSON(body)
				require.NoError(t, err)
				var firstBody []byte
				for _, sink := range []*destinationSink{first, second} {
					delivery := takeDestinationDelivery(t, sink, wire.GenerationExportHTTPPath)
					if firstBody == nil {
						firstBody = delivery.body
					} else {
						require.Equal(t, firstBody, delivery.body)
					}
					forwarded, err := wire.UnmarshalExportGenerationsJSON(delivery.body)
					require.NoError(t, err)
					require.Len(t, forwarded.Generations, 1)
					got, want := forwarded.Generations[0], original.Generations[0]
					require.Equal(t, want.Id, got.Id)
					require.Equal(t, want.ConversationId, got.ConversationId)
					require.True(t, proto.Equal(want.StartedAt, got.StartedAt))
					require.True(t, proto.Equal(want.CompletedAt, got.CompletedAt))
					if mode == "full" {
						require.True(t, proto.Equal(want, got))
					} else {
						require.NotContains(t, string(delivery.body), "secret")
					}
				}

				body = destinationTraceBatch(t, "otel-generation")
				sendDestinationIngress(t, receiver, "traces", body, false)
				var originalTrace coltracepb.ExportTraceServiceRequest
				require.NoError(t, proto.Unmarshal(body, &originalTrace))
				firstBody = nil
				for _, sink := range []*destinationSink{first, second} {
					delivery := takeDestinationDelivery(t, sink, "/v1/traces")
					if firstBody == nil {
						firstBody = delivery.body
					} else {
						require.Equal(t, firstBody, delivery.body)
					}
					var forwarded coltracepb.ExportTraceServiceRequest
					require.NoError(t, proto.Unmarshal(delivery.body, &forwarded))
					require.Len(t, forwarded.ResourceSpans, 1)
					gotResource, wantResource := forwarded.ResourceSpans[0], originalTrace.ResourceSpans[0]
					require.True(t, proto.Equal(wantResource.Resource, gotResource.Resource))
					require.Len(t, gotResource.ScopeSpans, 1)
					require.True(t, proto.Equal(wantResource.ScopeSpans[0].Scope, gotResource.ScopeSpans[0].Scope))
					require.Len(t, gotResource.ScopeSpans[0].Spans, 1)
					got, want := gotResource.ScopeSpans[0].Spans[0], wantResource.ScopeSpans[0].Spans[0]
					require.NotEmpty(t, want.TraceId)
					require.NotEmpty(t, want.SpanId)
					require.NotEmpty(t, want.ParentSpanId)
					require.Equal(t, want.TraceId, got.TraceId)
					require.Equal(t, want.SpanId, got.SpanId)
					require.Equal(t, want.ParentSpanId, got.ParentSpanId)
					require.Equal(t, want.StartTimeUnixNano, got.StartTimeUnixNano)
					require.Equal(t, want.EndTimeUnixNano, got.EndTimeUnixNano)
					require.Equal(t, "otel-generation", stringAttribute(got.Attributes, "agento11y.generation.id"))
					if mode == "full" {
						require.Equal(t, body, delivery.body)
					} else {
						require.NotContains(t, string(delivery.body), "secret")
					}
				}

				body = destinationMetricsBatch(t)
				sendDestinationIngress(t, receiver, "metrics", body, false)
				for _, sink := range []*destinationSink{first, second} {
					delivery := takeDestinationDelivery(t, sink, "/v1/metrics")
					require.Equal(t, body, delivery.body, "all resource/point attributes, timestamps, histogram values and buckets are unchanged")
				}
				for _, conversation := range []string{"conv-1", "otel-destinations"} {
					detail, err := receiver.storage.ConversationDetail(conversation)
					require.NoError(t, err)
					require.Len(t, detail.Generations, 1)
					require.Contains(t, detail.Generations[0].Input[0].Parts[0].Text, "secret")
				}
				statuses := receiver.forward.destinationStatuses(receiver.forward.load())
				require.Len(t, statuses, 2)
				for _, status := range statuses {
					require.Empty(t, status.Failures)
					for _, label := range []string{forwardLabelGenerations, forwardLabelOTLPGenerations, otlpForwardLabel("metrics")} {
						require.EqualValues(t, 1, status.Legs[label].Succeeded)
					}
				}
			})
		}
	}
}

func TestReceiverForwardDestinationsRemoteFailureKeepsLocalAck(t *testing.T) {
	for _, outcome := range []string{"reject", "redirect"} {
		for _, signal := range []string{"generations", "traces", "metrics"} {
			t.Run(outcome+"/"+signal, func(t *testing.T) {
				primary, extra := newDestinationSink(t, ""), newDestinationSink(t, outcome)
				receiver := newDestinationReceiver(t, true, "full", true, primary, extra)
				body, path, label := destinationHTTPBatch(t, "local-ack"), wire.GenerationExportHTTPPath, forwardLabelGenerations
				conversation := "conv-1"
				if signal == "traces" {
					body, path, label, conversation = destinationTraceBatch(t, "local-ack"), "/v1/traces", forwardLabelOTLPGenerations, "otel-destinations"
				}
				if signal == "metrics" {
					body, path, label = destinationMetricsBatch(t), "/v1/metrics", otlpForwardLabel("metrics")
				}
				sendDestinationIngress(t, receiver, signal, body, false)
				takeDestinationDelivery(t, primary, path)
				takeDestinationDelivery(t, extra, path)
				statuses := receiver.forward.destinationStatuses(receiver.forward.load())
				require.Len(t, statuses, 2)
				require.Empty(t, statuses[0].Failures)
				require.EqualValues(t, 1, statuses[0].Legs[label].Succeeded)
				require.Len(t, statuses[1].Failures, 1)
				require.EqualValues(t, 1, statuses[1].Legs[label].Failed)
				if signal != "metrics" {
					detail, err := receiver.storage.ConversationDetail(conversation)
					require.NoError(t, err)
					require.Len(t, detail.Generations, 1)
					require.Equal(t, "local-ack", detail.Generations[0].GenerationID)
				}
			})
		}
	}
}

func TestReceiverForwardDestinationsSuppressed(t *testing.T) {
	for _, primary := range []bool{true, false} {
		for _, gate := range []string{"global-off", "forward-marker"} {
			t.Run(fmt.Sprintf("primary=%t/%s", primary, gate), func(t *testing.T) {
				first, second := newDestinationSink(t, ""), newDestinationSink(t, "")
				receiver := newDestinationReceiver(t, primary, "full", gate != "global-off", first, second)
				cfg := receiver.forward.load()
				require.Len(t, cfg.destinations, 2, "suppression must not pass because recipients were missing")
				for _, signal := range []string{"generations", "traces", "metrics"} {
					body := destinationHTTPBatch(t, "suppressed")
					if signal == "traces" {
						body = destinationTraceBatch(t, "suppressed-otel")
					}
					if signal == "metrics" {
						body = destinationMetricsBatch(t)
					}
					sendDestinationIngress(t, receiver, signal, body, gate == "forward-marker")
				}
				require.Empty(t, first.calls)
				require.Empty(t, second.calls)
				for _, conversation := range []string{"conv-1", "otel-destinations"} {
					detail, err := receiver.storage.ConversationDetail(conversation)
					require.NoError(t, err)
					require.Len(t, detail.Generations, 1)
				}
			})
		}
	}
}

func TestReceiverForwardDestinationsAddingSidecarDoesNotReplay(t *testing.T) {
	sink := newDestinationSink(t, "")
	receiver := newDestinationReceiver(t, false, "full", true)
	receiver.forward.client = sink.server.Client()
	sendDestinationIngress(t, receiver, "generations", destinationHTTPBatch(t, "stored-http"), false)
	sendDestinationIngress(t, receiver, "traces", destinationTraceBatch(t, "stored-otel"), false)
	writeDestinationSidecar(t, receiver.configPath, sink)
	for range 2 {
		var config configResponse
		require.Equal(t, http.StatusOK, getJSON(t, receiver, "/api/v1/config", &config))
		var sessions any
		require.Equal(t, http.StatusOK, getJSON(t, receiver, "/api/v1/conversations", &sessions))
		receiver.forward.wait()
		require.Empty(t, sink.calls, "config reload and history reads must not replay stored sessions")
	}
	require.Len(t, receiver.forward.load().destinations, 1)
	sendDestinationIngress(t, receiver, "generations", destinationHTTPBatch(t, "fresh-http"), false)
	delivery := takeDestinationDelivery(t, sink, wire.GenerationExportHTTPPath)
	require.Contains(t, string(delivery.body), "fresh-http")
	require.NotContains(t, string(delivery.body), "stored-http")
	sendDestinationIngress(t, receiver, "traces", destinationTraceBatch(t, "fresh-otel"), false)
	delivery = takeDestinationDelivery(t, sink, "/v1/traces")
	require.Contains(t, string(delivery.body), "fresh-otel")
	require.NotContains(t, string(delivery.body), "stored-otel")
	for _, conversation := range []string{"conv-1", "otel-destinations"} {
		detail, err := receiver.storage.ConversationDetail(conversation)
		require.NoError(t, err)
		require.Len(t, detail.Generations, 2)
	}
}

func TestReceiverForwardDestinationsHistoryStaysLocal(t *testing.T) {
	for _, protocol := range []string{"http", "otel"} {
		t.Run(protocol, func(t *testing.T) {
			isolateHistoryState(t)
			primary, extra := newDestinationSink(t, ""), newDestinationSink(t, "")
			receiver := newDestinationReceiver(t, true, "full", true, primary, extra)
			t.Setenv("AGENTO11Y_PROTOCOL", protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			// History's SDK reads process configuration; receiver forwarding uses
			// config.env.
			var traces, metrics, generations atomic.Int32
			loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					if r.Header.Get(ForwardMarkerHeader) != "1" {
						t.Error("history ingress must carry the no-forward marker")
					}
					switch r.URL.Path {
					case otlpTracesPath:
						traces.Add(1)
					case otlpMetricsPath:
						metrics.Add(1)
					case wire.GenerationExportHTTPPath:
						generations.Add(1)
					}
				}
				receiver.ServeHTTP(w, r)
			}))
			t.Cleanup(loopback.Close)
			receiver.SetLocalEndpoint(loopback.URL)
			require.Len(t, receiver.forward.load().destinations, 2)
			writeHistoryOpenCodeV2(t, sessiondbtest.WithoutV1())
			v2run := waitForTerminalRun(t, receiver, startImport(t, receiver, `{"agent":"opencode"}`))
			require.Equal(t, importStatusCompleted, v2run.Status)
			require.Equal(t, 1, v2run.Imported)
			require.Empty(t, v2run.Warnings)
			writeHistoryTranscript(t, "sidecar-private", 3, time.Now().Add(-2*time.Hour))
			run := waitForTerminalRun(t, receiver, startImport(t, receiver, `{"agent":"claude-code"}`))
			require.Equal(t, importStatusCompleted, run.Status)
			require.Equal(t, 3, run.Imported)
			receiver.forward.wait()
			require.Empty(t, primary.calls)
			require.Empty(t, extra.calls)
			detail, err := receiver.storage.ConversationDetail("sidecar-private")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 3)
			raw, err := json.Marshal(detail)
			require.NoError(t, err)
			require.Contains(t, string(raw), "answer 0")
			v2detail, err := receiver.storage.ConversationDetail("v2-private")
			require.NoError(t, err)
			require.Len(t, v2detail.Generations, 1)
			raw, err = json.Marshal(v2detail)
			require.NoError(t, err)
			require.Contains(t, string(raw), "private-v2-answer")
			require.Positive(t, traces.Load())
			require.Positive(t, metrics.Load())
			if protocol == "otel" {
				require.Zero(t, generations.Load(), "OTel history must not also export HTTP generations")
			} else {
				require.Positive(t, generations.Load())
			}
		})
	}
}
