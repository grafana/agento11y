package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/trace/noop"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestReceiverOTelGenerationFailureSurvivesToolSuccess(t *testing.T) {
	for _, mode := range []string{"metadata_only", "full"} {
		t.Run(mode, func(t *testing.T) {
			clearForwardEnv(t)
			storage, err := NewStorage(t.TempDir())
			require.NoError(t, err)
			var calls atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", wire.ContentTypeProto)
				if calls.Add(1) == 1 {
					body, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1}})
					_, _ = w.Write(body)
				}
			}))
			defer cloud.Close()
			path := writeConfigEnvFile(t, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD":                "true",
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  cloud.URL,
				"AGENTO11Y_CONTENT_CAPTURE_MODE":         mode,
			})
			receiver := NewServer(storage, nil, path)
			defer receiver.Close()
			receiver.forward.client = cloud.Client()

			generationBatch := func(ids ...string) []byte {
				client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
				for _, id := range ids {
					_, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
						ID: id, ConversationID: "forward-conversation",
						Model: agento11y.ModelRef{Provider: "openai", Name: "model"},
					})
					recorder.SetResult(agento11y.Generation{Input: []agento11y.Message{agento11y.UserTextMessage("local content")}}, nil)
					recorder.End()
				}
				require.NoError(t, client.Flush(context.Background()))
				body, err := proto.Marshal(&transport.request)
				require.NoError(t, err)
				return body
			}
			send := func(body []byte) {
				request := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(gzipBytes(t, body)))
				request.Header.Set("Content-Type", wire.ContentTypeProto)
				request.Header.Set("Content-Encoding", "gzip")
				response := httptest.NewRecorder()
				receiver.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				receiver.forward.wait()
			}
			status := func() forwardStatus {
				response := httptest.NewRecorder()
				receiver.ServeHTTP(response, newLocalRequest(http.MethodGet, "/api/v1/config", nil))
				require.Equal(t, http.StatusOK, response.Code)
				var config configResponse
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &config))
				return config.ForwardStatus
			}

			send(generationBatch("partially-rejected", "possibly-accepted"))
			failed := status()
			require.True(t, failed.Generations)
			require.Len(t, failed.Failures, 1)
			require.Contains(t, failed.Failures[0].Detail, "rejected_spans=1")
			require.Equal(t, forwardLabelOTLPGenerations, failed.Failures[0].Label)
			require.Empty(t, failed.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			localBefore, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)

			toolBody, err := proto.Marshal(traceRequestForSpans(&tracepb.Span{
				TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8),
				StartTimeUnixNano: 1_000_000_000, EndTimeUnixNano: 2_000_000_000,
				Attributes: []*commonpb.KeyValue{
					stringKV("gen_ai.operation.name", "execute_tool"),
					stringKV("gen_ai.tool.name", "read_file"),
					stringKV("gen_ai.conversation.id", "forward-conversation"),
				},
			}))
			require.NoError(t, err)
			send(toolBody)
			afterTool := status()
			require.Equal(t, failed.Failures, afterTool.Failures, "tool-only success must not clear a generation rejection")
			require.Equal(t, failed.Legs[forwardLabelOTLPGenerations], afterTool.Legs[forwardLabelOTLPGenerations])
			require.NotEmpty(t, afterTool.Legs[otlpForwardLabel("traces")].LastSuccessAt)
			localAfter, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)
			require.Equal(t, localBefore, localAfter)
			detail, err := storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 2)
			require.Equal(t, "local content", detail.Generations[0].Input[0].Parts[0].Text)
			require.EqualValues(t, 2, calls.Load(), "status reads must not replay a partially accepted batch")

			send(generationBatch("genuinely-accepted"))
			recovered := status()
			require.Empty(t, recovered.Failures, "generation success must clear its own warning")
			require.NotEmpty(t, recovered.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			require.EqualValues(t, 3, calls.Load())
			detail, err = storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 3)
		})
	}
}

func TestReceiverOTelGenerationForwarding(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		rejected bool
	}{
		{mode: "metadata_only"},
		{mode: "full"},
		{mode: "full_with_metadata_spans"},
		{mode: "no_tool_content"},
		{mode: "metadata_only", rejected: true},
	} {
		t.Run(fmt.Sprintf("%s/rejected=%t", tc.mode, tc.rejected), func(t *testing.T) {
			clearForwardEnv(t)
			storage, err := NewStorage(t.TempDir())
			require.NoError(t, err)
			type traceDelivery struct {
				body      []byte
				persisted bool
				marker    string
			}
			delivered := make(chan traceDelivery, 2)
			var httpGenerations atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/v1/traces" {
					detail, err := storage.ConversationDetail("forward-conversation")
					delivered <- traceDelivery{body: body, persisted: err == nil && len(detail.Generations) == 1 && len(detail.Generations[0].Input) == 1 && len(detail.Generations[0].Input[0].Parts) == 1 && detail.Generations[0].Input[0].Parts[0].Text == "secret input", marker: r.Header.Get(ForwardMarkerHeader)}
				} else if r.URL.Path == wire.GenerationExportHTTPPath {
					httpGenerations.Add(1)
				}
				w.Header().Set("Content-Type", wire.ContentTypeProto)
				w.WriteHeader(http.StatusOK)
				if tc.rejected && r.URL.Path == "/v1/traces" {
					response, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1}})
					_, _ = w.Write(response)
				}
			}))
			defer cloud.Close()
			path := writeConfigEnvFile(t, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD":                "true",
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  cloud.URL,
				"AGENTO11Y_ENDPOINT":                     cloud.URL,
				"AGENTO11Y_AUTH_TENANT_ID":               "test-tenant",
				"AGENTO11Y_AUTH_TOKEN":                   "test-token",
				"AGENTO11Y_CONTENT_CAPTURE_MODE":         tc.mode,
			})
			receiver := NewServer(storage, log.New(io.Discard, "", 0), path)
			defer receiver.Close()
			receiver.forward.client = cloud.Client()
			client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
			_, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
				ID: "forward-generation", ConversationID: "forward-conversation", ConversationTitle: "secret title",
				SystemPrompt: "secret system", Model: agento11y.ModelRef{Provider: "openai", Name: "model"},
				Metadata: map[string]any{"keep": "non-content", "sigil.conversation.title": "secret legacy title"},
			})
			recorder.SetResult(agento11y.Generation{
				Input:  []agento11y.Message{agento11y.UserTextMessage("secret input")},
				Output: []agento11y.Message{agento11y.AssistantTextMessage("secret output")},
			}, nil)
			recorder.End()
			require.NoError(t, client.Flush(context.Background()))
			body, err := proto.Marshal(&transport.request)
			require.NoError(t, err)
			request := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(gzipBytes(t, body)))
			request.Header.Set("Content-Type", wire.ContentTypeProto)
			request.Header.Set("Content-Encoding", "gzip")
			response := httptest.NewRecorder()
			receiver.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			receiver.forward.wait()
			require.Len(t, delivered, 1)
			cloudCopy := <-delivered
			require.True(t, cloudCopy.persisted, "local persistence must precede Cloud delivery")
			require.Equal(t, "1", cloudCopy.marker)
			require.Zero(t, httpGenerations.Load(), "OTel ingress must not also forward generation HTTP")
			if tc.mode == "full" {
				decoded, err := gunzipLimited(cloudCopy.body, maxOTLPTraceBodyBytes)
				require.NoError(t, err)
				require.Equal(t, body, decoded)
			} else {
				require.NotContains(t, string(cloudCopy.body), "secret")
				var cloudTrace coltracepb.ExportTraceServiceRequest
				require.NoError(t, proto.Unmarshal(cloudCopy.body, &cloudTrace))
				forwarded := cloudTrace.ResourceSpans[0].ScopeSpans[0].Spans[0]
				original := transport.request.ResourceSpans[0].ScopeSpans[0].Spans[0]
				require.Equal(t, original.TraceId, forwarded.TraceId)
				require.Equal(t, original.SpanId, forwarded.SpanId)
				require.Equal(t, original.ParentSpanId, forwarded.ParentSpanId)
				attrs := forwarded.Attributes
				require.Equal(t, "forward-generation", stringAttribute(attrs, "agento11y.generation.id"))
				require.Equal(t, "true", stringAttribute(attrs, "agento11y.record"))
				require.JSONEq(t, `{"keep":"non-content","agento11y.sdk.name":"sdk-go","agento11y.sdk.content_capture_mode":"metadata_only"}`, stringAttribute(attrs, "agento11y.generation.metadata"))
			}
			detail, err := storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Equal(t, "secret title", detail.Title)
			require.Len(t, detail.Generations, 1)
			require.Equal(t, "secret output", detail.Generations[0].Output[0].Parts[0].Text)
			data, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)
			require.Contains(t, string(data), "secret title")
			require.Contains(t, string(data), "secret system")
			require.Contains(t, string(data), "full")
			if tc.rejected {
				status := receiver.forward.status()
				require.Len(t, status.Failures, 1)
				require.Equal(t, forwardLabelOTLPGenerations, status.Failures[0].Label)
				require.Contains(t, status.Failures[0].Detail, "rejected_spans=1")
				require.Empty(t, status.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			} else {
				require.Empty(t, receiver.forward.status().Failures)
			}

			legacy := contentRichGeneration(t)
			envelope, err := json.Marshal(generationsRequest{Generations: []json.RawMessage{generationRawJSON(t, legacy)}})
			require.NoError(t, err)
			response = httptest.NewRecorder()
			legacyRequest := newLocalRequest(http.MethodPost, wire.GenerationExportHTTPPath, bytes.NewReader(envelope))
			legacyRequest.Header.Set("Content-Type", wire.ContentTypeJSON)
			receiver.ServeHTTP(response, legacyRequest)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			receiver.forward.wait()
			require.EqualValues(t, 1, httpGenerations.Load(), "legacy Pi/OpenCode HTTP ingress remains forwarded under OTel")
			require.Empty(t, delivered)
			if tc.rejected {
				require.Len(t, receiver.forward.status().Failures, 1, "HTTP success must not clear the OTLP rejection")
			}
		})
	}
}

func TestReceiverToolHistogramForwarding(t *testing.T) {
	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeMetadataOnly,
		agento11y.ContentCaptureModeFull,
		agento11y.ContentCaptureModeFullWithMetadataSpans,
		agento11y.ContentCaptureModeNoToolContent,
	} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gzip=%t", mode.String(), compressed), func(t *testing.T) {
				clearForwardEnv(t)
				ctx := t.Context()
				storage, err := NewStorage(t.TempDir())
				require.NoError(t, err)
				type delivery struct {
					body                          []byte
					contentType, encoding, marker string
				}
				capture := func(r *http.Request) (delivery, error) {
					body, err := io.ReadAll(r.Body)
					return delivery{body, r.Header.Get("Content-Type"), r.Header.Get("Content-Encoding"), r.Header.Get(ForwardMarkerHeader)}, err
				}
				delivered := make(chan delivery, 4)
				cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/metrics" {
						t.Errorf("unexpected upstream request: %s", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					item, err := capture(r)
					if err != nil {
						t.Error(err)
						http.Error(w, "read metrics", http.StatusBadRequest)
						return
					}
					delivered <- item
					w.Header().Set("Content-Type", wire.ContentTypeProto)
				}))
				defer cloud.Close()
				path := writeConfigEnvFile(t, map[string]string{
					"AGENTO11Y_LOCAL_FORWARD":               "true",
					"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL,
					"AGENTO11Y_CONTENT_CAPTURE_MODE":        mode.String(),
				})
				receiver := NewServer(storage, log.New(io.Discard, "", 0), path)
				defer receiver.Close()
				receiver.forward.client = cloud.Client()
				incoming := make(chan delivery, 4)
				loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != otlpMetricsPath {
						t.Errorf("unexpected local request: %s", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					item, err := capture(r)
					if err != nil {
						t.Error(err)
						http.Error(w, "read metrics", http.StatusBadRequest)
						return
					}
					incoming <- item
					r.Body = io.NopCloser(bytes.NewReader(item.body))
					receiver.ServeHTTP(w, r)
				}))
				defer loopback.Close()
				compression := otlpmetrichttp.NoCompression
				if compressed {
					compression = otlpmetrichttp.GzipCompression
				}
				exporter, err := otlpmetrichttp.New(ctx,
					otlpmetrichttp.WithEndpointURL(loopback.URL+otlpMetricsPath),
					otlpmetrichttp.WithHeaders(map[string]string{}),
					otlpmetrichttp.WithCompression(compression),
				)
				require.NoError(t, err)
				mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter, sdkmetric.WithInterval(time.Hour))))
				defer func() {
					// Shutdown exports a final batch; drain its relay before closing servers.
					err := mp.Shutdown(context.Background())
					receiver.forward.wait()
					require.NoError(t, err)
				}()
				client := agento11y.NewClient(agento11y.Config{
					GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone},
					TracerProvider:   noop.NewTracerProvider(), MeterProvider: mp,
					ContentCapture: mode, Tags: map[string]string{"team": "receiver-test"},
				})
				defer func() { require.NoError(t, client.Shutdown(context.Background())) }()

				bounds := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
				type expectedPoint struct {
					attrs         map[string]string
					count         uint64
					sum, min, max float64
					buckets       []uint64
				}
				identity := func(attrs map[string]string) string {
					t.Helper()
					data, err := json.Marshal(attrs)
					require.NoError(t, err)
					return string(data)
				}
				want := map[string]*expectedPoint{}
				started := time.Unix(1, 0)
				for i, tc := range []struct {
					kind              agento11y.ToolExecutionKind
					skill, metricKind string
					metricSkill       string
					duration          time.Duration
					failed            bool
				}{
					{kind: agento11y.ToolExecutionKindLoadSkill, skill: " review ", metricKind: "load_skill", metricSkill: "review", duration: 20 * time.Millisecond},
					{kind: agento11y.ToolExecutionKindLoadSkill, skill: "review", metricKind: "load_skill", metricSkill: "review", duration: 20 * time.Millisecond},
					{kind: agento11y.ToolExecutionKindLoadSkill, skill: "review", metricKind: "load_skill", metricSkill: "review", duration: 40 * time.Millisecond, failed: true},
					{kind: agento11y.ToolExecutionKindLoadSkill, skill: "draft", metricKind: "load_skill", metricSkill: "draft", duration: 1200 * time.Millisecond},
					{kind: agento11y.ToolExecutionKindLoadSkill, skill: " \t", metricKind: "load_skill", duration: 80 * time.Millisecond},
					{skill: "private-unrecognized", duration: 160 * time.Millisecond},
					{kind: "unknown", skill: "private-unrecognized", duration: 320 * time.Millisecond},
					{kind: agento11y.ToolExecutionKindReadSkillResource, skill: "review", metricKind: "read_skill_resource", metricSkill: "review", duration: 640 * time.Millisecond},
					{kind: agento11y.ToolExecutionKindCommand, skill: "review", metricKind: "command", metricSkill: "review", duration: 2560 * time.Millisecond},
				} {
					_, recorder := client.StartToolExecution(ctx, agento11y.ToolExecutionStart{
						ToolName: "read", ExecutionKind: tc.kind, SkillName: tc.skill, StartedAt: started,
						RequestProvider: "provider", RequestModel: "model", AgentName: "agent", AgentVersion: "test",
						ConversationID: "private-conversation", ToolCallID: fmt.Sprintf("private-call-%d", i),
						SkillDescription: "private-description", SkillSourceURI: "file:///private/source",
						SkillResourceName: "private-resource", ProcessExecutableName: "private-executable",
						ProcessExecutablePath: "/private/executable",
					})
					code := int64(17)
					recorder.SetResult(agento11y.ToolExecutionEnd{
						Arguments: "private-arguments", Result: "private-result", ProcessExitCode: &code,
						CompletedAt: started.Add(tc.duration),
					})
					attrs := map[string]string{
						"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "read",
						"gen_ai.provider.name": "provider", "gen_ai.request.model": "model",
						"gen_ai.agent.name": "agent", "gen_ai.agent.version": "test",
						"agento11y.tag.team": "receiver-test", "error.type": "", "error.category": "",
					}
					if tc.failed {
						recorder.SetExecError(errors.New("private-failure"))
						attrs["error.type"], attrs["error.category"] = "tool_execution_error", "sdk_error"
					}
					recorder.End()
					recorder.End()
					if tc.metricKind != "" {
						attrs["agento11y.tool.execution.kind"] = tc.metricKind
					}
					if tc.metricSkill != "" {
						attrs["gen_ai.skill.name"] = tc.metricSkill
					}
					key := identity(attrs)
					seconds := tc.duration.Seconds()
					point := want[key]
					if point == nil {
						point = &expectedPoint{attrs: attrs, min: seconds, max: seconds, buckets: make([]uint64, len(bounds)+1)}
						want[key] = point
					}
					point.count++
					point.sum += seconds
					point.min, point.max = min(point.min, seconds), max(point.max, seconds)
					bucket := 0
					for bucket < len(bounds) && seconds > bounds[bucket] {
						bucket++
					}
					point.buckets[bucket]++
				}
				require.NoError(t, mp.ForceFlush(ctx))
				receiver.forward.wait()
				require.Len(t, incoming, 1)
				require.Len(t, delivered, 1)
				original, forwarded := <-incoming, <-delivered
				require.Equal(t, original.body, forwarded.body)
				require.Equal(t, original.contentType, forwarded.contentType)
				require.Equal(t, wire.ContentTypeProto, forwarded.contentType)
				require.Equal(t, original.encoding, forwarded.encoding)
				require.Equal(t, compressed, forwarded.encoding == "gzip")
				require.Equal(t, "1", forwarded.marker)
				body := forwarded.body
				if compressed {
					body, err = gunzipLimited(body, maxOTLPBodyBytes)
					require.NoError(t, err)
				}
				var request colmetricpb.ExportMetricsServiceRequest
				require.NoError(t, proto.Unmarshal(body, &request))
				require.Len(t, request.ResourceMetrics, 1)
				require.Len(t, request.ResourceMetrics[0].ScopeMetrics, 1)
				metrics := request.ResourceMetrics[0].ScopeMetrics[0].Metrics
				require.Len(t, metrics, 1)
				require.Equal(t, "gen_ai.client.operation.duration", metrics[0].Name)
				require.Equal(t, "s", metrics[0].Unit)
				points := metrics[0].GetHistogram().GetDataPoints()
				require.Len(t, points, len(want))
				var count uint64
				var sum float64
				for _, point := range points {
					attrs := map[string]string{}
					for _, attr := range point.Attributes {
						attrs[attr.Key] = attr.Value.GetStringValue()
					}
					expected := want[identity(attrs)]
					require.NotNil(t, expected, "unexpected metric attributes: %v", attrs)
					require.Equal(t, expected.attrs, attrs)
					require.Equal(t, expected.count, point.Count)
					require.InDelta(t, expected.sum, point.GetSum(), 1e-12)
					require.Equal(t, expected.min, point.GetMin())
					require.Equal(t, expected.max, point.GetMax())
					require.Equal(t, bounds, point.ExplicitBounds)
					require.Equal(t, expected.buckets, point.BucketCounts)
					require.Empty(t, point.Exemplars)
					require.NotContains(t, identity(attrs), "private-")
					count += point.Count
					sum += point.GetSum()
					delete(want, identity(attrs))
				}
				require.Empty(t, want)
				require.EqualValues(t, 9, count)
				require.InDelta(t, 5.04, sum, 1e-12)
				require.Empty(t, receiver.forward.status().Failures)
				require.NotEmpty(t, receiver.forward.status().Legs[otlpForwardLabel("metrics")].LastSuccessAt)
			})
		}
	}
}
