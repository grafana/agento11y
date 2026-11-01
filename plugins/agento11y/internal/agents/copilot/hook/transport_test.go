package hook

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTelStopConfirmation(t *testing.T) {
	type testCase struct {
		name, failure, surface, toolName, input, wantSkill, storedSkill string
		pending                                                         int
		failed                                                          bool
	}
	var tests []testCase
	for _, failure := range []string{"", "rejected", "partial", "invalid", "opt-in", "endpoint", "old daemon", "old daemon insecure"} {
		name := failure
		if name == "" {
			name = "accepted skill"
		}
		tests = append(tests, testCase{name: name, failure: failure, surface: "copilot-cli", toolName: "skill", input: `{"skill":"review","private":"private-input"}`, pending: 1, wantSkill: "review"})
	}
	tests = append(tests,
		testCase{name: "wrapped post only", surface: "copilot-cli", toolName: "skill", input: `"{\"skill\":\"review\"}"`, wantSkill: "review"},
		testCase{name: "failure stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"review"}`, pending: 1, failed: true},
		testCase{name: "post only failure stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"review"}`, failed: true},
		testCase{name: "VS Code stays generic", surface: "vscode", toolName: "skill", input: `{"skill":"review"}`, pending: 1},
		testCase{name: "unknown surface stays generic", toolName: "skill", input: `{"skill":"review"}`, pending: 1},
		testCase{name: "VS Code persisted identity stays generic", surface: "vscode", toolName: "skill", input: `{"skill":"review"}`, pending: 1, storedSkill: "review"},
		testCase{name: "unknown persisted identity stays generic", toolName: "skill", input: `{"skill":"review"}`, pending: 1, storedSkill: "review"},
		testCase{name: "unsafe persisted identity stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"/private/review"}`, pending: 1, storedSkill: "/private/review"},
		testCase{name: "failed persisted identity stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"review"}`, pending: 1, failed: true, storedSkill: "review"},
		testCase{name: "exact tool name required", surface: "copilot-cli", toolName: "Skill", input: `{"skill":"review"}`, pending: 1},
		testCase{name: "unsafe identity stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"/private/review"}`, pending: 1},
		testCase{name: "ambiguous completions stay generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"review"}`, pending: 2},
		testCase{name: "ambiguous failure then success stays generic", surface: "copilot-cli", toolName: "skill", input: `{"skill":"review"}`, pending: 2, failed: true},
	)
	for _, mode := range []agento11y.ContentCaptureMode{
		agento11y.ContentCaptureModeFull, agento11y.ContentCaptureModeNoToolContent,
		agento11y.ContentCaptureModeFullWithMetadataSpans, agento11y.ContentCaptureModeMetadataOnly,
	} {
		for _, tt := range tests {
			t.Run(mode.String()+"/"+tt.name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("AGENTO11Y_PROTOCOL", "otel")
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				var marked, generationHTTP, auxiliary atomic.Int64
				var rejecting atomic.Bool
				rejecting.Store(tt.failure != "")
				var mu sync.Mutex
				var spans []*tracepb.Span
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/healthz" {
						if tt.failure == "old daemon" || tt.failure == "old daemon insecure" {
							_, _ = w.Write([]byte(`{"status":"ok"}`))
						} else {
							_, _ = w.Write([]byte(`{"capabilities":["otel_generation_ingest_v1"]}`))
						}
						return
					}
					if strings.Contains(r.URL.Path, "generations") {
						generationHTTP.Add(1)
					}
					if strings.HasSuffix(r.URL.Path, "/traces") {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						var req coltracepb.ExportTraceServiceRequest
						if err := proto.Unmarshal(body, &req); err != nil {
							t.Error(err)
							return
						}
						var records int64
						mu.Lock()
						for _, rs := range req.ResourceSpans {
							for _, ss := range rs.ScopeSpans {
								for _, span := range ss.Spans {
									spans = append(spans, span)
									if copilotWireAttrs(span)["agento11y.record"] == "true" {
										records++
									}
								}
							}
						}
						mu.Unlock()
						if records > 0 {
							marked.Add(records)
							if rejecting.Load() {
								if tt.failure == "partial" {
									body, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: records, ErrorMessage: "rejected"}})
									w.Header().Set("Content-Type", "application/x-protobuf")
									_, _ = w.Write(body)
								} else {
									w.WriteHeader(http.StatusBadRequest)
								}
							}
							return
						}
					}
					// Rejected tool telemetry must not affect generation confirmation.
					auxiliary.Add(1)
					w.WriteHeader(http.StatusBadRequest)
				}))
				defer server.Close()
				t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
				switch tt.failure {
				case "invalid":
					t.Setenv("AGENTO11Y_PROTOCOL", "invalid")
				case "opt-in":
					t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
				case "endpoint":
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
				case "old daemon":
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
				case "old daemon insecure":
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", strings.Replace(server.URL, "http://", "https://", 1)+"/otlp")
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
				}
				logger := log.New(io.Discard, "", 0)
				cfg := config.Config{ContentCapture: mode}
				UserPromptSubmit(Payload{SessionIDJSON: "sess", Prompt: "hello", Model: "gpt-5", SurfaceMarker: tt.surface, Timestamp: json.RawMessage(`"2026-05-18T12:00:00Z"`)}, cfg, logger)
				p := Payload{SessionIDJSON: "sess", ToolNameJSON: tt.toolName, SurfaceMarker: tt.surface,
					ToolInputJSON: json.RawMessage(`{"skill":"original","private":"private-input"}`), Timestamp: json.RawMessage(`"2026-05-18T12:00:01Z"`)}
				for range tt.pending {
					PreToolUse(context.Background(), io.Discard, p, cfg, logger)
				}
				if tt.pending > 0 {
					before := fragment.LoadTolerant("sess", "turn-000001", logger)
					require.NotNil(t, before)
					require.Len(t, before.Tools, tt.pending)
					for _, tool := range before.Tools {
						require.Empty(t, tool.SkillName)
					}
				}
				duration := 250.0
				p.DurationMsJSON = &duration
				p.Timestamp = json.RawMessage(`"2026-05-18T12:00:03Z"`)
				p.ToolInputJSON = json.RawMessage(tt.input)
				p.ToolResultJSON = json.RawMessage(`{"text_result_for_llm":"private-result"}`)
				PostToolUse(p, cfg, logger, tt.failed)
				if tt.pending > 1 {
					poisoned := fragment.LoadTolerant("sess", "turn-000001", logger)
					require.Len(t, poisoned.Tools, tt.pending)
					for _, tool := range poisoned.Tools {
						require.True(t, tool.SkillAttributionAmbiguous)
						require.Empty(t, tool.SkillName)
					}
					p.ToolInputJSON = json.RawMessage(`{"skill":"other"}`)
					PostToolUse(p, cfg, logger, false)
				}
				persisted := fragment.LoadTolerant("sess", "turn-000001", logger)
				require.NotNil(t, persisted)
				require.Equal(t, tt.surface, persisted.Surface)
				require.Len(t, persisted.Tools, max(1, tt.pending))
				for i, tool := range persisted.Tools {
					require.Equal(t, tt.wantSkill, tool.SkillName)
					require.Equal(t, tt.pending > 1, tool.SkillAttributionAmbiguous)
					require.Equal(t, fmt.Sprintf("turn-000001-tool-%03d", i+1), tool.ToolUseID)
					wantStatus := "completed"
					if tt.failed && i == len(persisted.Tools)-1 {
						wantStatus = "error" // pending records are matched newest first
					}
					require.Equal(t, wantStatus, tool.Status)
					wantStarted := "2026-05-18T12:00:01Z"
					if tt.pending == 0 {
						wantStarted = "2026-05-18T12:00:03Z"
					}
					require.Equal(t, wantStarted, tool.StartedAt)
					require.Equal(t, "2026-05-18T12:00:03Z", tool.CompletedAt)
					require.NotNil(t, tool.DurationMs)
					require.Equal(t, duration, *tool.DurationMs)
				}
				if mode != agento11y.ContentCaptureModeFull {
					data, err := os.ReadFile(fragment.FragmentFilePath("sess", "turn-000001"))
					require.NoError(t, err)
					require.NotContains(t, string(data), "private-input")
					require.NotContains(t, string(data), "private-result")
				}
				if tt.storedSkill != "" {
					require.NoError(t, fragment.Update("sess", "turn-000001", logger, func(f *fragment.Fragment) bool {
						f.Tools[0].SkillName = tt.storedSkill
						return true
					}))
				}
				payload := Payload{SessionIDJSON: "sess", StopReasonJSON: "end_turn", Timestamp: json.RawMessage(`"2026-05-18T12:00:04Z"`)}
				Stop(payload, cfg, logger)
				require.Zero(t, generationHTTP.Load())
				require.False(t, config.HasCredentials())
				if tt.failure != "" {
					session := fragment.LoadSessionTolerant("sess", logger)
					require.NotNil(t, session)
					require.Equal(t, "turn-000001", session.ActiveTurnID)
					retained := fragment.LoadTolerant("sess", session.ActiveTurnID, logger)
					require.NotNil(t, retained)
					require.Equal(t, persisted.Tools, retained.Tools, "retry must retain safe identity and original tool records")
					if tt.failure != "rejected" && tt.failure != "partial" {
						require.Zero(t, marked.Load())
						return
					}
					require.Equal(t, int64(1), marked.Load())
					rejecting.Store(false)
					Stop(payload, cfg, logger)
				}
				wantMarked := int64(1)
				if tt.failure != "" {
					wantMarked = 2
				}
				require.Equal(t, wantMarked, marked.Load())
				require.Zero(t, generationHTTP.Load())
				require.Positive(t, auxiliary.Load())
				require.Empty(t, fragment.LoadSessionTolerant("sess", logger).ActiveTurnID)
				require.Nil(t, fragment.LoadTolerant("sess", "turn-000001", logger))
				Stop(payload, cfg, logger)
				require.Equal(t, wantMarked, marked.Load())

				mu.Lock()
				wireSpans := append([]*tracepb.Span(nil), spans...)
				mu.Unlock()
				require.Len(t, wireSpans, int(wantMarked)*(1+len(persisted.Tools)), "classification must not add spans")
				generations := map[string]*tracepb.Span{}
				for _, span := range wireSpans {
					if copilotWireAttrs(span)["agento11y.record"] == "true" {
						generations[hex.EncodeToString(span.TraceId)] = span
					}
				}
				require.Len(t, generations, int(wantMarked))
				callsByTrace := map[string]map[string]bool{}
				for _, span := range wireSpans {
					attrs := copilotWireAttrs(span)
					if attrs["agento11y.record"] == "true" {
						continue
					}
					traceID := hex.EncodeToString(span.TraceId)
					parent := generations[traceID]
					require.NotNil(t, parent, "tool must share the generation trace")
					require.Equal(t, parent.SpanId, span.ParentSpanId)
					require.NotEqual(t, parent.SpanId, span.SpanId)
					require.Equal(t, tracepb.Span_SPAN_KIND_INTERNAL, span.Kind)
					if callsByTrace[traceID] == nil {
						callsByTrace[traceID] = map[string]bool{}
					}
					callID := attrs["gen_ai.tool.call.id"]
					require.False(t, callsByTrace[traceID][callID], "duplicate tool call span")
					callsByTrace[traceID][callID] = true
					var original *fragment.ToolRecord
					for i := range persisted.Tools {
						if persisted.Tools[i].ToolUseID == callID {
							original = &persisted.Tools[i]
						}
					}
					require.NotNil(t, original, "original call id must survive classification")
					require.Equal(t, tt.toolName, attrs["gen_ai.tool.name"])
					require.Equal(t, "function", attrs["gen_ai.tool.type"])
					wantName := "execute_tool " + tt.toolName
					if tt.wantSkill != "" {
						wantName += " " + tt.wantSkill
						require.Equal(t, tt.wantSkill, attrs["gen_ai.skill.name"])
					} else {
						require.NotContains(t, attrs, "gen_ai.skill.name")
					}
					require.Equal(t, wantName, span.Name)
					require.Equal(t, "execute_tool", attrs["gen_ai.operation.name"])
					require.NotContains(t, attrs, "agento11y.skill.name")
					wantStatus := tracepb.Status_STATUS_CODE_OK
					if original.Status == "error" {
						wantStatus = tracepb.Status_STATUS_CODE_ERROR
					}
					require.Equal(t, wantStatus, span.GetStatus().GetCode())
					completed, err := time.Parse(time.RFC3339Nano, original.CompletedAt)
					require.NoError(t, err)
					require.Equal(t, uint64(completed.UnixNano()), span.EndTimeUnixNano)
					require.Equal(t, uint64(completed.Add(-250*time.Millisecond).UnixNano()), span.StartTimeUnixNano)
					if mode == agento11y.ContentCaptureModeFull && tt.pending > 0 {
						require.Contains(t, attrs["gen_ai.tool.call.arguments"], "private-input")
						require.Contains(t, attrs["gen_ai.tool.call.result"], "private-result")
					} else {
						require.NotContains(t, attrs, "gen_ai.tool.call.arguments")
						if mode != agento11y.ContentCaptureModeFull {
							require.NotContains(t, attrs, "gen_ai.tool.call.result")
						}
					}
				}
				for _, calls := range callsByTrace {
					require.Len(t, calls, len(persisted.Tools))
				}
				if mode != agento11y.ContentCaptureModeFull {
					for _, span := range wireSpans {
						require.NotContains(t, span.String(), "private-input", "payload leaked to OTLP")
						require.NotContains(t, span.String(), "private-result", "payload leaked to OTLP")
					}
				}
			})
		}
	}
}

func copilotWireAttrs(span *tracepb.Span) map[string]string {
	attrs := make(map[string]string, len(span.Attributes))
	for _, attr := range span.Attributes {
		attrs[attr.Key] = attr.Value.GetStringValue()
	}
	return attrs
}
