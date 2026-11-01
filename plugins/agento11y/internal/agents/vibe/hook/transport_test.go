package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestPostAgentGenerationTransport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, gate, capture                                                          string
		httpConfig, missingEndpoint, canceled, oldDaemon, reject, telemetryFailure, priorFound bool
		wantFailure, recordFailure, cancelExport, synthetic                                    bool
	}{
		{name: "HTTP default", httpConfig: true},
		{name: "explicit HTTP", protocol: "http", httpConfig: true},
		{name: "recorder failure keeps state and tools", protocol: "otel", gate: "true", httpConfig: true, recordFailure: true, wantFailure: true},
		{name: "OTel without HTTP credentials", protocol: "otel", gate: "true"},
		{name: "OTel with HTTP configured", protocol: "otel", gate: "true", httpConfig: true},
		{name: "skill full capture", protocol: "otel", gate: "true", capture: "full"},
		{name: "skill no tool content", protocol: "otel", gate: "true", capture: "no_tool_content"},
		{name: "skill metadata only", protocol: "otel", gate: "true", capture: "metadata_only"},
		{name: "synthetic slash transcript has no matching post", protocol: "otel", gate: "true", synthetic: true},
		{name: "OTel telemetry failure is independent", protocol: "otel", gate: "true", telemetryFailure: true},
		{name: "missing opt-in", protocol: "otel", httpConfig: true, wantFailure: true},
		{name: "missing exporter", protocol: "otel", gate: "true", httpConfig: true, missingEndpoint: true, wantFailure: true},
		{name: "invalid protocol", protocol: "unsupported", gate: "true", httpConfig: true, wantFailure: true},
		{name: "setup canceled removes pre-save", protocol: "otel", gate: "true", httpConfig: true, canceled: true, wantFailure: true},
		{name: "setup canceled restores prior", protocol: "otel", gate: "true", httpConfig: true, canceled: true, priorFound: true, wantFailure: true},
		{name: "old daemon removes pre-save", protocol: "otel", gate: "true", httpConfig: true, oldDaemon: true, wantFailure: true},
		{name: "old daemon restores prior", protocol: "otel", gate: "true", httpConfig: true, oldDaemon: true, priorFound: true, wantFailure: true},
		{name: "failed confirmation retries new session", protocol: "otel", gate: "true", httpConfig: true, reject: true, wantFailure: true},
		{name: "failed confirmation retries prior session", protocol: "otel", gate: "true", httpConfig: true, reject: true, priorFound: true, wantFailure: true},
		{name: "canceled confirmation retains tools and retries", protocol: "otel", gate: "true", httpConfig: true, cancelExport: true, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", tc.gate)
			contentMode := "full_with_metadata_spans"
			if tc.protocol != "otel" {
				contentMode = "full"
			}
			if tc.capture != "" {
				contentMode = tc.capture
			}
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", contentMode)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var mu sync.Mutex
			var httpRequests, tools int
			var ids []string
			var wireSpans []*tracepb.Span
			var output, mode, operation string
			reject := tc.reject
			cancelExport := tc.cancelExport
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/healthz":
					if tc.oldDaemon {
						_, _ = w.Write([]byte(`{"status":"ok"}`))
					} else {
						_, _ = w.Write([]byte(`{"capabilities":["otel_generation_ingest_v1"]}`))
					}
				case "/api/v1/generations:export":
					httpRequests++
					body, _ := io.ReadAll(r.Body)
					writeAcceptedGenerationResponse(t, w, body)
				case "/v1/traces", "/otlp/v1/traces":
					if r.Header.Get("Content-Type") != "application/x-protobuf" {
						t.Error("expected binary OTLP")
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					var req coltracepb.ExportTraceServiceRequest
					if err := proto.Unmarshal(body, &req); err != nil {
						t.Error(err)
					}
					marked := false
					for _, rs := range req.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								wireSpans = append(wireSpans, span)
								attrs := map[string]string{}
								for _, a := range span.Attributes {
									attrs[a.Key] = a.Value.GetStringValue()
								}
								if attrs["gen_ai.operation.name"] == "execute_tool" {
									tools++
								}
								if attrs["agento11y.record"] != "true" {
									continue
								}
								marked = true
								ids = append(ids, attrs["agento11y.generation.id"])
								operation = attrs["gen_ai.operation.name"]
								output = attrs["gen_ai.output.messages"]
								var metadata map[string]any
								if err := json.Unmarshal([]byte(attrs["agento11y.generation.metadata"]), &metadata); err != nil {
									t.Error(err)
								}
								mode, _ = metadata["agento11y.sdk.content_capture_mode"].(string)
							}
						}
					}
					if marked && cancelExport {
						cancel()
						http.Error(w, "canceled", http.StatusBadRequest)
						return
					}
					if (marked && reject) || (!marked && tc.telemetryFailure) {
						http.Error(w, "rejected", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
				case "/v1/metrics", "/otlp/v1/metrics":
					if tc.telemetryFailure {
						http.Error(w, "rejected", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			if tc.httpConfig {
				t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
				t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant")
				t.Setenv("AGENTO11Y_AUTH_TOKEN", "token")
			}
			if !tc.missingEndpoint && tc.protocol != "" && tc.protocol != "http" {
				endpoint := server.URL
				if tc.oldDaemon {
					endpoint += "/otlp"
				}
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			}
			const sessionID = "transport-session"
			prior := state.Session{}
			if tc.priorFound {
				prior = state.Session{Title: "prior title", SessionCost: 0.01, SessionPromptTokens: 10, LastGenerationID: "prior-generation"}
				must(t, state.Save(sessionID, prior))
			}
			// Setup-failure rows still need evidence to test retention.
			duration := 10.0
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			PostTool(Payload{SessionID: sessionID, ToolCallIDValue: "call_meow_3", ToolNameValue: "skill",
				ToolInputValue: json.RawMessage(`{"name":"review"}`), ToolStatusValue: "failure", DurationMsValue: &duration}, discardLogger())
			event, ok := toolevents.Load(sessionID)["call_meow_3"]
			require.True(t, ok)
			require.Equal(t, "review", event.SkillName)
			if tc.missingEndpoint || tc.protocol == "" || tc.protocol == "http" {
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			} else if tc.oldDaemon {
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
			}
			dir := t.TempDir()
			must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
			must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
			must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `"name": "bash"`, `"name": "skill"`))
			must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `{\"command\": \"grep -c escape /keyboard/cats/sir-pounce/rap-sheet.txt\"}`, `{\"name\": \"review\"}`))
			if tc.synthetic {
				// Vibe can record a slash expansion as a synthetic tool call:
				// its transcript ID has no corresponding post_tool execution.
				must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `call_meow_3`, `synthetic-slash-review`))
				must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `which one keeps pressing escape?`, `/review`))
			}
			if tc.recordFailure {
				must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `"name": "skill"`, `"name": ""`))
			}
			payload := Payload{SessionID: sessionID, TranscriptPath: filepath.Join(dir, "messages.jsonl")}
			if tc.canceled {
				cancel()
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			PostAgent(ctx, payload, logger)
			if tc.wantFailure {
				if got, found := state.Load(sessionID); found != tc.priorFound || got != prior {
					t.Fatalf("failure changed state: found=%v state=%+v; logs: %s", found, got, logs.String())
				}
				if got := toolevents.Load(sessionID); len(got) != 1 || got[event.ToolCallID] != event {
					t.Fatalf("failure consumed tool events: %v", got)
				}
				if tc.oldDaemon && !strings.Contains(logs.String(), "restart") {
					t.Fatalf("missing restart instruction: %s", logs.String())
				}
				if tc.recordFailure && !strings.Contains(logs.String(), "emit: recorder:") {
					t.Fatalf("did not reach recorder failure: %s", logs.String())
				}
				if tc.canceled && !strings.Contains(logs.String(), "otel setup") {
					t.Fatalf("did not reach setup after pre-save: %s", logs.String())
				}
				if tc.reject || tc.cancelExport {
					mu.Lock()
					if tc.cancelExport && tools == 0 {
						t.Error("canceled hook context prevented cleanup from exporting tool spans")
					}
					reject, cancelExport = false, false
					mu.Unlock()
					PostAgent(context.Background(), payload, logger)
				} else {
					mu.Lock()
					defer mu.Unlock()
					if httpRequests != 0 || len(ids) != 0 {
						t.Fatalf("exported after setup failure: HTTP=%d spans=%v", httpRequests, ids)
					}
					return
				}
			}
			if got, found := state.Load(sessionID); !found || got.Offset == 0 || got.LastGenerationID == "" {
				t.Fatalf("state not advanced: %+v; logs: %s", got, logs.String())
			}
			if got := toolevents.Load(sessionID); len(got) != 0 {
				t.Fatalf("confirmed turn kept tool events: %v", got)
			}
			PostAgent(context.Background(), payload, logger)
			mu.Lock()
			defer mu.Unlock()
			if tc.protocol != "otel" {
				if httpRequests != 1 || len(ids) != 0 {
					t.Fatalf("HTTP=%d spans=%v", httpRequests, ids)
				}
				return
			}
			wantSpans := 1
			if tc.reject || tc.cancelExport {
				wantSpans = 2
			}
			if httpRequests != 0 || len(ids) != wantSpans || ids[0] == "" || tools == 0 {
				t.Fatalf("HTTP=%d spans=%v tools=%d", httpRequests, ids, tools)
			}
			if (tc.reject || tc.cancelExport) && ids[0] != ids[1] {
				t.Fatalf("retry changed generation ID: %v", ids)
			}
			wantMode := contentMode
			if wantMode == "full_with_metadata_spans" {
				wantMode = "full" // OTel upgrades this mode before mapping and emitting tool spans.
			}
			if mode != wantMode || operation != "chat" {
				t.Fatalf("output=%q capture=%q operation=%q", output, mode, operation)
			}
			if wantMode != "metadata_only" {
				require.Contains(t, output, "Sir Pounce")
			} else {
				require.NotContains(t, output, "Sir Pounce")
			}
			parents := map[string]*tracepb.Span{}
			for _, span := range wireSpans {
				if vibeWireAttrs(span)["agento11y.record"] == "true" {
					parents[string(span.TraceId)] = span
				}
			}
			require.Len(t, parents, wantSpans)
			callsByTrace := map[string]map[string]bool{}
			for _, span := range wireSpans {
				attrs := vibeWireAttrs(span)
				if attrs["gen_ai.operation.name"] != "execute_tool" {
					continue
				}
				parent := parents[string(span.TraceId)]
				require.NotNil(t, parent, "tool must share generation trace")
				require.Equal(t, parent.SpanId, span.ParentSpanId)
				require.NotEqual(t, parent.SpanId, span.SpanId)
				require.Equal(t, tracepb.Span_SPAN_KIND_INTERNAL, span.Kind)
				callID := attrs["gen_ai.tool.call.id"]
				if callsByTrace[string(span.TraceId)] == nil {
					callsByTrace[string(span.TraceId)] = map[string]bool{}
				}
				calls := callsByTrace[string(span.TraceId)]
				require.False(t, calls[callID], "classification added an execution for %s", callID)
				calls[callID] = true
				require.Equal(t, "skill", attrs["gen_ai.tool.name"])
				require.Equal(t, "function", attrs["gen_ai.tool.type"])
				require.NotContains(t, attrs, "agento11y.skill.name")
				if callID == event.ToolCallID {
					require.Equal(t, "execute_tool skill review", span.Name)
					require.Equal(t, "review", attrs["gen_ai.skill.name"])
					require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
					require.Equal(t, uint64(event.CompletedAt.UnixNano()), span.EndTimeUnixNano)
					require.Equal(t, uint64(event.CompletedAt.Add(-10*time.Millisecond).UnixNano()), span.StartTimeUnixNano)
				} else {
					require.Equal(t, "execute_tool skill", span.Name)
					require.NotContains(t, attrs, "gen_ai.skill.name")
					require.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
					require.Equal(t, span.StartTimeUnixNano, span.EndTimeUnixNano)
				}
				if mode == "full" {
					if callID == event.ToolCallID {
						require.JSONEq(t, `{"name":"review"}`, attrs["gen_ai.tool.call.arguments"])
					}
					require.NotEmpty(t, attrs["gen_ai.tool.call.arguments"])
					require.NotEmpty(t, attrs["gen_ai.tool.call.result"])
				} else {
					require.NotContains(t, attrs, "gen_ai.tool.call.arguments")
					require.NotContains(t, attrs, "gen_ai.tool.call.result")
					require.NotContains(t, span.String(), "/keyboard/cats")
					require.NotContains(t, span.String(), "crimes:")
				}
			}
			wantCalls := 2
			if tc.priorFound {
				wantCalls = 3 // The prior fixture offset is zero, so it includes the first turn.
			}
			require.Equal(t, wantCalls*wantSpans, tools)
			for _, calls := range callsByTrace {
				require.Len(t, calls, wantCalls)
				require.True(t, calls["call_meow_2"])
				if tc.synthetic {
					require.True(t, calls["synthetic-slash-review"])
					require.NotContains(t, calls, event.ToolCallID, "orphan post must not create an execution")
				} else {
					require.True(t, calls[event.ToolCallID])
				}
			}
		})
	}
}

func vibeWireAttrs(span *tracepb.Span) map[string]string {
	attrs := make(map[string]string, len(span.Attributes))
	for _, attr := range span.Attributes {
		attrs[attr.Key] = attr.Value.GetStringValue()
	}
	return attrs
}

func TestBuildClientRequiresGenerationProvider(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	client, err := buildClient(agento11y.ContentCaptureModeFull, nil, agento11y.GenerationExportProtocolOTel, "", "", "", "", discardLogger())
	if err == nil || client != nil {
		t.Fatalf("missing generation provider: client=%v err=%v", client, err)
	}
}
