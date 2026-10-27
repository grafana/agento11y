package claudecode

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestHook_LateCompletionMetricDelivery(t *testing.T) {
	for _, protocol := range []string{"http", "otel"} {
		for _, mode := range []string{"full", "no_tool_content", "metadata_only", "full_with_metadata_spans"} {
			t.Run(protocol+"/"+mode, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				var mu sync.Mutex
				var spans []*tracepb.Span
				var samples []*metricpb.HistogramDataPoint
				var generationCount int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/healthz":
						_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					case "/api/v1/generations:export":
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						var req struct {
							Generations []json.RawMessage `json:"generations"`
						}
						if err := json.Unmarshal(body, &req); err != nil {
							t.Error(err)
						}
						generationCount += len(req.Generations)
						r.Body = io.NopCloser(bytes.NewReader(body))
						acceptGenerationExport(w, r)
					case "/v1/traces":
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						var req coltracepb.ExportTraceServiceRequest
						if err := proto.Unmarshal(body, &req); err != nil {
							t.Error(err)
						}
						for _, resource := range req.ResourceSpans {
							for _, scope := range resource.ScopeSpans {
								for _, span := range scope.Spans {
									spans = append(spans, span)
									if otlpStringAttr(span.Attributes, "agento11y.record") == "true" {
										generationCount++
									}
								}
							}
						}
						w.Header().Set("Content-Type", "application/x-protobuf")
					case "/v1/metrics":
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
						}
						var req colmetricpb.ExportMetricsServiceRequest
						if err := proto.Unmarshal(body, &req); err != nil {
							t.Error(err)
						}
						for _, resource := range req.ResourceMetrics {
							for _, scope := range resource.ScopeMetrics {
								for _, metric := range scope.Metrics {
									if metric.Name == "gen_ai.execute_tool.duration" {
										if metric.Unit != "s" || metric.GetHistogram() == nil {
											t.Errorf("invalid draft histogram: %v", metric)
										}
										samples = append(samples, metric.GetHistogram().GetDataPoints()...)
									}
								}
							}
						}
						w.Header().Set("Content-Type", "application/x-protobuf")
					default:
						t.Errorf("unexpected request: %s", r.URL.Path)
					}
				}))
				defer server.Close()
				setHookExportEnv(t, server.URL)
				t.Setenv("AGENTO11Y_PROTOCOL", protocol)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
				t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode)
				const session = "late-metric-delivery"
				const callAt = "2025-06-01T12:00:01Z"
				const resultAt = "2025-06-01T12:00:02.2Z"
				path := filepath.Join(t.TempDir(), "transcript.jsonl")
				call := buildHookAssistantLineAt(session, "request-1", "tool_use", callAt, []any{
					map[string]any{"type": "tool_use", "id": "skill-1", "name": "Skill", "input": map[string]any{"skill": "review"}},
				}, 7) + "\n"
				if err := os.WriteFile(path, []byte(call), 0o600); err != nil {
					t.Fatal(err)
				}
				input := hookInput{HookEventName: "Stop", SessionID: session, TranscriptPath: path}
				logs := runHookForTest(t, input)
				mu.Lock()
				initialSamples, initialGenerations := len(samples), generationCount
				mu.Unlock()
				if initialSamples != 0 || initialGenerations != 1 {
					t.Fatalf("call-only hook emitted %d samples/%d generations, want 0/1; logs: %s", initialSamples, initialGenerations, logs)
				}
				var result map[string]any
				if err := json.Unmarshal([]byte(buildHookToolResultJSONLAt(session, "skill-1", "private instructions", resultAt)), &result); err != nil {
					t.Fatal(err)
				}
				result["toolUseResult"] = map[string]any{"success": true, "commandName": "review"}
				encoded, err := json.Marshal(result)
				if err != nil {
					t.Fatal(err)
				}
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := f.Write(append(encoded, '\n')); err != nil {
					t.Fatal(err)
				}
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
				logs = runHookForTest(t, input)
				mu.Lock()
				if len(samples) == 0 || generationCount != 1 {
					t.Errorf("result-only hook emitted %d samples, generations=%d, want samples and no replay; logs: %s", len(samples), generationCount, logs)
				}
				var parent, tool *tracepb.Span
				for _, span := range spans {
					if otlpStringAttr(span.Attributes, "gen_ai.tool.call.id") == "skill-1" {
						if tool != nil {
							t.Error("duplicate tool span")
						}
						tool = span
					} else if otlpStringAttr(span.Attributes, "gen_ai.operation.name") == "generateText" || otlpStringAttr(span.Attributes, "agento11y.record") == "true" {
						parent = span
					}
				}
				if parent == nil || tool == nil {
					t.Errorf("missing parent/tool spans: %v", spans)
				} else {
					if !bytes.Equal(parent.TraceId, tool.TraceId) || !bytes.Equal(parent.SpanId, tool.ParentSpanId) {
						t.Error("late tool did not retain original generation trace/span context")
					}
					start, _ := time.Parse(time.RFC3339Nano, callAt)
					end, _ := time.Parse(time.RFC3339Nano, resultAt)
					if tool.StartTimeUnixNano != uint64(start.UnixNano()) || tool.EndTimeUnixNano != uint64(end.UnixNano()) {
						t.Errorf("tool times = %d/%d, want %d/%d", tool.StartTimeUnixNano, tool.EndTimeUnixNano, start.UnixNano(), end.UnixNano())
					}
				}
				for _, point := range samples {
					if point.Count != 1 || point.Sum == nil || math.Abs(point.GetSum()-1.2) > 1e-9 {
						t.Errorf("histogram count/sum=%d/%v, want 1/1.2", point.Count, point.Sum)
					}
					if otlpStringAttr(point.Attributes, "gen_ai.skill.name") != "review" || otlpStringAttr(point.Attributes, "agento11y.tool.execution.kind") != "load_skill" || otlpStringAttr(point.Attributes, "gen_ai.tool.name") != "Skill" {
						t.Errorf("missing verified metric identity: %v", point.Attributes)
					}
					for _, attr := range point.Attributes {
						switch attr.Key {
						case "gen_ai.tool.call.id", "gen_ai.conversation.id", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.tool.description", "gen_ai.skill.source.uri":
							t.Errorf("content or high-cardinality metric dimension: %s", attr.Key)
						}
					}
					var bucketCount uint64
					for i, count := range point.BucketCounts {
						bucketCount += count
						if count != 0 && (i != 7 || count != 1) {
							t.Errorf("unexpected bucket[%d]=%d for 1.2 seconds", i, count)
						}
					}
					wantBounds := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
					if !slices.Equal(point.ExplicitBounds, wantBounds) || len(point.BucketCounts) != len(wantBounds)+1 || bucketCount != point.Count {
						t.Errorf("invalid histogram buckets: %v", point)
					}
				}
				beforeSamples, beforeSpans := len(samples), len(spans)
				mu.Unlock()
				logs = runHookForTest(t, input)
				mu.Lock()
				if len(samples) != beforeSamples || len(spans) != beforeSpans || generationCount != 1 {
					t.Errorf("redundant hook replayed telemetry: samples=%d/%d spans=%d/%d generations=%d; logs: %s", len(samples), beforeSamples, len(spans), beforeSpans, generationCount, logs)
				}
				mu.Unlock()
			})
		}
	}
}

func otlpStringAttr(attrs []*commonpb.KeyValue, key string) string {
	for _, attr := range attrs {
		if attr.Key == key {
			return attr.Value.GetStringValue()
		}
	}
	return ""
}
