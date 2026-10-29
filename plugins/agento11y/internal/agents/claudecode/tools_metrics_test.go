package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/xdg"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestHookLateCompletionMetricDelivery(t *testing.T) {
	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeMetadataOnly,
		agento11y.ContentCaptureModeNoToolContent,
		agento11y.ContentCaptureModeFull,
		agento11y.ContentCaptureModeFullWithMetadataSpans,
	} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failed=%t", mode.String(), failed), func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", mode.String())
				var mu sync.Mutex
				var metrics []*metricpb.Metric
				var spans []*tracepb.Span
				generationCount := 0
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					mu.Lock()
					defer mu.Unlock()
					switch r.URL.Path {
					case "/api/v1/generations:export":
						var request struct{ Generations []json.RawMessage }
						if err := json.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						generationCount += len(request.Generations)
						r.Body = io.NopCloser(bytes.NewReader(body))
						acceptGenerationExport(w, r)
					case "/v1/metrics":
						var request colmetricpb.ExportMetricsServiceRequest
						if err := proto.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						for _, resource := range request.ResourceMetrics {
							for _, scope := range resource.ScopeMetrics {
								metrics = append(metrics, scope.Metrics...)
							}
						}
						w.Header().Set("Content-Type", "application/x-protobuf")
					case "/v1/traces":
						var request coltracepb.ExportTraceServiceRequest
						if err := proto.Unmarshal(body, &request); err != nil {
							t.Error(err)
						}
						for _, resource := range request.ResourceSpans {
							for _, scope := range resource.ScopeSpans {
								spans = append(spans, scope.Spans...)
							}
						}
						w.Header().Set("Content-Type", "application/x-protobuf")
					default:
						t.Errorf("unexpected telemetry request: %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
				}))
				defer server.Close()
				setHookExportEnv(t, server.URL)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)

				const sessionID = "late-metric-session"
				path := filepath.Join(t.TempDir(), "session.jsonl")
				text := strings.Join([]string{
					buildHookUserJSONLAt(sessionID, "load review", "2025-06-01T12:00:00Z"),
					buildHookAssistantLineAt(sessionID, "request", "tool_use", "2025-06-01T12:00:01Z", []any{map[string]any{
						"type": "tool_use", "id": "call", "name": "Skill", "input": map[string]any{"skill": "review", "args": "private-argument-sentinel"},
					}}, 7),
				}, "\n") + "\n"
				if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
					t.Fatal(err)
				}
				input := hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path}
				checkState := func() {
					t.Helper()
					saved, err := os.ReadFile(filepath.Join(xdg.AppStateRoot(), "claude-code", state.SanitizeSessionID(sessionID)+".state"))
					if err != nil {
						t.Fatal(err)
					}
					for _, sentinel := range []string{"private-argument-sentinel", "private-instructions-sentinel"} {
						if bytes.Contains(saved, []byte(sentinel)) {
							t.Fatalf("persisted tool content: %s", sentinel)
						}
					}
				}
				runHookForTest(t, input)
				checkState()

				metricNames := map[string]bool{}
				checkMetrics := func(wantTool bool) {
					t.Helper()
					found := false
					for _, metric := range metrics {
						if metric.Name == "gen_ai.execute_tool.duration" {
							t.Error("unexpected second tool duration instrument")
						}
						if metric.Name != "gen_ai.client.operation.duration" {
							continue
						}
						for _, point := range metric.GetHistogram().GetDataPoints() {
							attributes := map[string]string{}
							for _, attribute := range point.Attributes {
								attributes[attribute.Key] = attribute.Value.GetStringValue()
							}
							if attributes["gen_ai.operation.name"] != "execute_tool" {
								continue
							}
							found = true
							want := map[string]string{
								"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "Skill",
								"gen_ai.provider.name": "anthropic", "gen_ai.request.model": "claude-opus-4",
								"gen_ai.agent.name": "claude-code", "gen_ai.agent.version": "1.0.0",
								"gen_ai.skill.name": "review", "agento11y.tool.execution.kind": "load_skill",
								"error.type": "", "error.category": "",
							}
							if failed {
								want["error.type"], want["error.category"] = "tool_execution_error", "sdk_error"
							}
							if !maps.Equal(attributes, want) {
								t.Errorf("tool metric attributes = %v, want %v", attributes, want)
							}
							bounds := []float64{0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}
							buckets := make([]uint64, len(bounds)+1)
							buckets[7] = 1
							if metric.Unit != "s" || point.Count != 1 || point.GetSum() != 1.2 ||
								!slices.Equal(point.ExplicitBounds, bounds) || !slices.Equal(point.BucketCounts, buckets) {
								t.Errorf("tool histogram unit=%q point=%v, want one 1.2s observation with original bounds", metric.Unit, point)
							}
						}
					}
					if found != wantTool {
						t.Errorf("completed tool metric present=%t, want %t", found, wantTool)
					}
				}
				mu.Lock()
				checkMetrics(false)
				for _, metric := range metrics {
					metricNames[metric.Name] = true
				}
				if generationCount != 1 {
					t.Errorf("call-only generation count=%d, want 1", generationCount)
				}
				var parent *tracepb.Span
				for _, span := range spans {
					if strings.HasPrefix(span.Name, "execute_tool") {
						t.Error("call-only hook emitted a completed tool span")
					} else if strings.HasPrefix(span.Name, "generateText") {
						parent = span
					}
				}
				metrics, spans = nil, nil
				mu.Unlock()

				result := buildHookToolResultJSONLAt(sessionID, "call", "private-instructions-sentinel", "2025-06-01T12:00:02.2Z")
				if failed {
					result = strings.Replace(result, `"tool_use_id":"call"`, `"tool_use_id":"call","is_error":true`, 1)
				}
				if err := os.WriteFile(path, []byte(text+result+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				runHookForTest(t, input)
				checkState()
				mu.Lock()
				checkMetrics(true)
				for _, metric := range metrics {
					if !metricNames[metric.Name] {
						t.Errorf("late completion introduced instrument %q", metric.Name)
					}
				}
				tools := 0
				for _, span := range spans {
					if !strings.HasPrefix(span.Name, "execute_tool") {
						continue
					}
					tools++
					if parent == nil || !bytes.Equal(span.ParentSpanId, parent.SpanId) || !bytes.Equal(span.TraceId, parent.TraceId) {
						t.Errorf("late tool did not retain its generation span parent: %v", span)
					}
				}
				if tools != 1 || generationCount != 1 {
					t.Errorf("after result: tool spans=%d generations=%d, want 1/1", tools, generationCount)
				}
				metrics, spans = nil, nil
				mu.Unlock()

				for _, event := range []string{"Stop", "SessionEnd", "SessionEnd"} {
					input.HookEventName = event
					runHookForTest(t, input)
				}
				mu.Lock()
				defer mu.Unlock()
				checkMetrics(false)
				if len(spans) != 0 || generationCount != 1 {
					t.Errorf("redundant hooks replayed telemetry: spans=%d generations=%d", len(spans), generationCount)
				}
			})
		}
	}
}
