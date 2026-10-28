package hook

import (
	"bytes"
	"encoding/json"
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
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestUnverifiedSkillObservationsRemainGeneric(t *testing.T) {
	raw, err := os.ReadFile("../skilldetect/testdata/0.165.0/negative-observations.json")
	require.NoError(t, err)
	var fixture struct {
		SourceCommit string `json:"source_commit"`
		Cases        []struct {
			Name   string    `json:"name"`
			Events []Payload `json:"events"`
		} `json:"cases"`
	}
	require.NoError(t, json.Unmarshal(raw, &fixture))
	require.Equal(t, "360319d8dee62d87e2d59fc0c21805d79519cfed", fixture.SourceCommit)
	require.Len(t, fixture.Cases, 12)
	for _, tc := range fixture.Cases {
		for _, mode := range []agento11y.ContentCaptureMode{
			agento11y.ContentCaptureModeFull,
			agento11y.ContentCaptureModeNoToolContent,
			agento11y.ContentCaptureModeMetadataOnly,
			agento11y.ContentCaptureModeFullWithMetadataSpans,
		} {
			t.Run(tc.Name+"/"+mode.String(), func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("AGENTO11Y_PROTOCOL", "otel")
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				var mu sync.Mutex
				var spans []*tracepb.Span
				var reject atomic.Bool
				reject.Store(true)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasSuffix(r.URL.Path, "/traces") {
						w.WriteHeader(http.StatusOK)
						return
					}
					body, readErr := io.ReadAll(r.Body)
					if readErr != nil {
						t.Error(readErr)
						return
					}
					var req coltracepb.ExportTraceServiceRequest
					if decodeErr := proto.Unmarshal(body, &req); decodeErr != nil {
						t.Error(decodeErr)
						return
					}
					marked := false
					mu.Lock()
					for _, rs := range req.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								for _, attr := range span.Attributes {
									if attr.Key == "agento11y.record" {
										marked = true
									}
								}
								spans = append(spans, span)
							}
						}
					}
					mu.Unlock()
					if marked && reject.Load() {
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				defer server.Close()
				t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
				cfg := config.Config{ContentCapture: mode}
				var logs bytes.Buffer
				logger := log.New(&logs, "", 0)
				completed := time.Date(2026, 10, 28, 12, 0, 1, 0, time.UTC)
				duration := 250.0
				for _, event := range tc.Events {
					event.SessionID = "session"
					event.TurnID = "turn"
					event.Model = "gpt-5"
					event.Timestamp = completed.Format(time.RFC3339Nano)
					event.ToolDurationMs = &duration
					PostToolUse(event, cfg, logger)
				}
				frag := fragment.LoadTolerant("session", "turn", logger)
				require.NotNil(t, frag)
				require.Len(t, frag.Tools, len(tc.Events))
				for _, tool := range frag.Tools {
					require.Equal(t, mode == agento11y.ContentCaptureModeFull, len(tool.ToolInput) > 0)
					require.Equal(t, mode == agento11y.ContentCaptureModeFull, len(tool.ToolResponse) > 0)
					require.Empty(t, tool.Status)
				}
				for attempt := range 2 {
					payload := Payload{SessionID: "session", TurnID: "turn", Timestamp: completed.Add(time.Second).Format(time.RFC3339Nano)}
					if attempt == 1 {
						reject.Store(false)
						payload.TurnID = "next-turn"
						payload.Model = "gpt-5"
					}
					Stop(payload, cfg, logger)
					if attempt == 0 {
						pending := fragment.LoadTolerant("session", "turn", logger)
						require.NotNil(t, pending)
						require.True(t, pending.PendingRetry)
					} else {
						require.Nil(t, fragment.LoadTolerant("session", "turn", logger))
						require.Nil(t, fragment.LoadTolerant("session", "next-turn", logger))
					}
					mu.Lock()
					exported := spans
					spans = nil
					mu.Unlock()
					parents := make(map[string]*tracepb.Span)
					tools := make(map[string]*tracepb.Span)
					markedCount := 0
					for _, span := range exported {
						attrs := make(map[string]string)
						for _, attr := range span.Attributes {
							attrs[attr.Key] = attr.Value.GetStringValue()
							require.False(t, strings.HasPrefix(attr.Key, "gen_ai.skill."))
						}
						for _, event := range span.Events {
							for _, attr := range event.Attributes {
								require.False(t, strings.HasPrefix(attr.Key, "gen_ai.skill."))
							}
						}
						if attrs["agento11y.record"] == "true" {
							markedCount++
						}
						if strings.HasPrefix(span.Name, "execute_tool ") {
							id := attrs["gen_ai.tool.call.id"]
							require.NotContains(t, tools, id)
							tools[id] = span
							require.Equal(t, "execute_tool", attrs["gen_ai.operation.name"])
							require.Equal(t, mode == agento11y.ContentCaptureModeFull, attrs["gen_ai.tool.call.arguments"] != "")
							require.Equal(t, mode == agento11y.ContentCaptureModeFull, attrs["gen_ai.tool.call.result"] != "")
							require.Equal(t, uint64(completed.Add(-250*time.Millisecond).UnixNano()), span.StartTimeUnixNano)
							require.Equal(t, uint64(completed.UnixNano()), span.EndTimeUnixNano)
							require.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
						} else {
							parents[string(span.SpanId)] = span
						}
					}
					require.Len(t, tools, len(tc.Events), "exports=%v logs=%s", exported, logs.String())
					require.Len(t, parents, attempt+1)
					require.Equal(t, attempt+1, markedCount)
					for _, event := range tc.Events {
						span := tools[event.ToolUseID]
						require.NotNil(t, span)
						require.Equal(t, "execute_tool "+event.ToolName, span.Name)
						parent := parents[string(span.ParentSpanId)]
						require.NotNil(t, parent)
						require.Equal(t, parent.TraceId, span.TraceId)
						var generationID string
						for _, attr := range parent.Attributes {
							if attr.Key == "agento11y.generation.id" {
								generationID = attr.Value.GetStringValue()
							}
						}
						require.Equal(t, mapper.GenerationID("session", "turn"), generationID)
					}
				}
			})
		}
	}
}

func TestOTelStopConfirmation(t *testing.T) {
	for _, failure := range []string{"", "rejected", "partial", "invalid", "opt-in", "endpoint", "old daemon"} {
		t.Run(failure, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			var marked, generationHTTP, auxiliary atomic.Int64
			var rejecting atomic.Bool
			rejecting.Store(failure != "")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					if failure == "old daemon" {
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
					for _, rs := range req.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								for _, attr := range span.Attributes {
									if attr.Key == "agento11y.record" && attr.Value.GetStringValue() == "true" {
										records++
									}
								}
							}
						}
					}
					if records > 0 {
						marked.Add(records)
						if rejecting.Load() {
							if failure == "partial" {
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
				auxiliary.Add(1)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			switch failure {
			case "invalid":
				t.Setenv("AGENTO11Y_PROTOCOL", "invalid")
				t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant")
				t.Setenv("AGENTO11Y_AUTH_TOKEN", "test-token")
			case "opt-in":
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			case "endpoint":
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			case "old daemon":
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			for _, id := range []string{"old", "current"} {
				require.NoError(t, fragment.Update("sess", id, logger, func(f *fragment.Fragment) bool {
					f.Model = "gpt-5"
					f.Prompt = "hello"
					f.PendingRetry = id == "old"
					return true
				}))
			}
			cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
			if failure == "invalid" {
				client, err := buildClient(cfg, "", "", nil, logger)
				require.Error(t, err)
				require.Nil(t, client)
			}
			payload := Payload{SessionID: "sess", TurnID: "current"}
			Stop(payload, cfg, logger)
			require.Zero(t, generationHTTP.Load())
			require.Equal(t, failure == "invalid", config.HasCredentials())
			if failure == "invalid" {
				require.Contains(t, logs.String(), "stop: generation transport:")
				require.Zero(t, auxiliary.Load())
			}
			if failure != "" {
				for _, id := range []string{"old", "current"} {
					got := fragment.LoadTolerant("sess", id, logger)
					require.NotNil(t, got)
					require.True(t, got.PendingRetry)
				}
				if failure != "rejected" && failure != "partial" {
					require.Zero(t, marked.Load())
					return
				}
				require.Equal(t, int64(2), marked.Load())
				rejecting.Store(false)
				Stop(payload, cfg, logger)
			}
			wantMarked := int64(2)
			if failure != "" {
				wantMarked = 4
			}
			require.Equal(t, wantMarked, marked.Load())
			require.Zero(t, generationHTTP.Load())
			require.Positive(t, auxiliary.Load())
			require.Nil(t, fragment.LoadTolerant("sess", "old", logger))
			require.Nil(t, fragment.LoadTolerant("sess", "current", logger))
		})
	}
}
