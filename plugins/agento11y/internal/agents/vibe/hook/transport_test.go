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

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/vibe/toolevents"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestPostAgentGenerationTransport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, gate                                                                   string
		httpConfig, missingEndpoint, canceled, oldDaemon, reject, telemetryFailure, priorFound bool
		wantFailure, recordFailure, cancelExport                                               bool
	}{
		{name: "HTTP default", httpConfig: true},
		{name: "explicit HTTP", protocol: "http", httpConfig: true},
		{name: "recorder failure keeps state and tools", protocol: "otel", gate: "true", httpConfig: true, recordFailure: true, wantFailure: true},
		{name: "OTel without HTTP credentials", protocol: "otel", gate: "true"},
		{name: "OTel with HTTP configured", protocol: "otel", gate: "true", httpConfig: true},
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
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", tc.gate)
			contentMode := "full_with_metadata_spans"
			if tc.protocol != "otel" {
				contentMode = "full"
			}
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", contentMode)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			var mu sync.Mutex
			var httpRequests, tools int
			var ids []string
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
			event := toolevents.Event{ToolCallID: "call_meow_3", ToolName: "bash", Status: "failure", DurationMs: 10}
			must(t, toolevents.Save(sessionID, event))
			dir := t.TempDir()
			must(t, copyFile(filepath.Join("..", "testdata", "messages.jsonl"), filepath.Join(dir, "messages.jsonl")))
			must(t, copyFile(filepath.Join("..", "testdata", "meta.json"), filepath.Join(dir, "meta.json")))
			if tc.recordFailure {
				must(t, replaceInFile(filepath.Join(dir, "messages.jsonl"), `"name": "bash"`, `"name": ""`))
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
			if !strings.Contains(output, "Sir Pounce") || mode != "full" || operation != "chat" {
				t.Fatalf("output=%q capture=%q operation=%q", output, mode, operation)
			}
		})
	}
}

func TestBuildClientRequiresGenerationProvider(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	client, err := buildClient(agento11y.ContentCaptureModeFull, nil, agento11y.GenerationExportProtocolOTel, "", "", "", "", discardLogger())
	if err == nil || client != nil {
		t.Fatalf("missing generation provider: client=%v err=%v", client, err)
	}
}
