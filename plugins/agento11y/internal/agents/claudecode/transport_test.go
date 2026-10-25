package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/mapper"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestHookRecordFailureTransport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol string
		savedPrefix    bool
	}{
		{name: "HTTP from zero", protocol: "http"},
		{name: "HTTP saved offset", protocol: "http", savedPrefix: true},
		{name: "OTel from zero", protocol: "otel"},
		{name: "OTel saved offset", protocol: "otel", savedPrefix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			const sessionID = "record-failure-session"
			prefix := ""
			if tc.savedPrefix {
				prefix = buildHookAssistantJSONL(sessionID, "saved", "end_turn", "already exported", 5) + "\n"
			}
			prior := state.Session{Offset: int64(len(prefix)), Title: "prior title", Model: "prior model"}
			if err := state.Save(sessionID, prior); err != nil {
				t.Fatal(err)
			}
			text := prefix + strings.Join([]string{
				buildHookAssistantJSONL(sessionID, "A", "end_turn", "valid before", 5),
				strings.ReplaceAll(buildHookAssistantJSONL(sessionID, "B", "end_turn", "invalid middle", 5), `"model":"claude-opus-4"`, `"model":""`),
				buildHookAssistantJSONL(sessionID, "C", "end_turn", "valid after", 5),
			}, "\n") + "\n"
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			lines, safeOffset, _, titles := readTranscriptSettled(t.Context(), path, prior.Offset, true, log.New(io.Discard, "", 0))
			mappedState := prior
			gens, _ := mapper.Process(lines, &mappedState, mapper.Options{SessionID: sessionID, Titles: titles}, nil)
			if len(gens) != 3 || safeOffset != int64(len(text)) {
				t.Fatalf("generations=%d safe offset=%d, want 3/%d", len(gens), safeOffset, len(text))
			}
			for i, gen := range gens {
				err := agento11y.ValidateGeneration(gen)
				if i == 1 {
					if err == nil || err.Error() != "generation.model.name is required" {
						t.Fatalf("middle generation validation: %v", err)
					}
				} else if err != nil {
					t.Fatalf("valid generation %d: %v", i, err)
				}
			}

			var mu sync.Mutex
			var httpIDs, otelIDs []string
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
						Generations []struct{ ID string } `json:"generations"`
					}
					if err := json.Unmarshal(body, &req); err != nil {
						t.Error(err)
					}
					for _, gen := range req.Generations {
						httpIDs = append(httpIDs, gen.ID)
					}
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
					for _, rs := range req.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								var marked bool
								var id string
								for _, attr := range span.Attributes {
									switch attr.Key {
									case "agento11y.record":
										marked = attr.Value.GetStringValue() == "true"
									case "agento11y.generation.id":
										id = attr.Value.GetStringValue()
									}
								}
								if marked {
									otelIDs = append(otelIDs, id)
								}
							}
						}
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
				case "/v1/metrics":
					w.Header().Set("Content-Type", "application/x-protobuf")
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			if tc.protocol == "otel" {
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			}
			for stop := 1; stop <= 2; stop++ {
				logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path})
				wantState := prior
				var wantHTTP, wantOTel []string
				if tc.protocol == "otel" {
					wantOTel = []string{gens[0].ID}
					if stop == 2 {
						wantOTel = append(wantOTel, gens[0].ID)
					}
				} else {
					wantState.Offset = safeOffset
					wantHTTP = []string{gens[0].ID, gens[2].ID}
				}
				if got := state.Load(sessionID); !reflect.DeepEqual(got, wantState) {
					t.Fatalf("Stop%d state=%+v, want %+v; logs: %s", stop, got, wantState, logs)
				}
				if stop == 1 || tc.protocol == "otel" {
					if !strings.Contains(logs, "enqueue: recorder:") || !strings.Contains(logs, "generation.model.name is required") {
						t.Fatalf("Stop%d missing recorder failure: %s", stop, logs)
					}
				} else if strings.Contains(logs, "enqueue:") {
					t.Fatalf("repeated Stop replayed invalid generation: %s", logs)
				}
				mu.Lock()
				gotHTTP, gotOTel := slices.Clone(httpIDs), slices.Clone(otelIDs)
				mu.Unlock()
				if !slices.Equal(gotHTTP, wantHTTP) || !slices.Equal(gotOTel, wantOTel) {
					t.Fatalf("Stop%d HTTP=%v OTel=%v, want HTTP=%v OTel=%v; logs: %s", stop, gotHTTP, gotOTel, wantHTTP, wantOTel, logs)
				}
			}
		})
	}
}

func TestHookGenerationTransport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, gate                                                       string
		httpConfig, missingEndpoint, canceled, oldDaemon, reject, telemetryFailure bool
		wantFailure, recordFailure                                                 bool
	}{
		{name: "HTTP default", httpConfig: true},
		{name: "explicit HTTP", protocol: "http", httpConfig: true},
		{name: "SDK grpc falls back to HTTP", protocol: "grpc", httpConfig: true},
		{name: "SDK none falls back to HTTP", protocol: "none", httpConfig: true},
		{name: "OTel without HTTP credentials", protocol: "otel", gate: "true"},
		{name: "OTel with HTTP configured", protocol: "otel", gate: "true", httpConfig: true},
		{name: "OTel telemetry failure is independent", protocol: "otel", gate: "true", telemetryFailure: true},
		{name: "missing opt-in", protocol: "otel", httpConfig: true, wantFailure: true},
		{name: "missing exporter", protocol: "otel", gate: "true", httpConfig: true, missingEndpoint: true, wantFailure: true},
		{name: "invalid protocol", protocol: "unsupported", gate: "true", httpConfig: true, wantFailure: true},
		{name: "setup canceled", protocol: "otel", gate: "true", httpConfig: true, canceled: true, wantFailure: true},
		{name: "old daemon", protocol: "otel", gate: "true", httpConfig: true, oldDaemon: true, wantFailure: true},
		{name: "recorder failure keeps offset", protocol: "otel", gate: "true", httpConfig: true, recordFailure: true, wantFailure: true},
		{name: "failed confirmation retries transcript", protocol: "otel", gate: "true", httpConfig: true, reject: true, wantFailure: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", tc.gate)
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var mu sync.Mutex
			var httpRequests int
			var ids []string
			var output, mode, operation string
			reject := tc.reject
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					acceptGenerationExport(w, r)
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
			prior := state.Session{Title: "prior title", Model: "prior model"}
			if err := state.Save(sessionID, prior); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "transcript.jsonl")
			text := buildHookUserJSONL(sessionID, "prompt") + "\n" + buildHookAssistantJSONL(sessionID, "request-1", "end_turn", "retained reply", 5) + "\n"
			if tc.recordFailure {
				text = strings.ReplaceAll(text, `"model":"claude-opus-4"`, `"model":""`)
			}
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if tc.canceled {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			if err := Hook(ctx, bytes.NewReader(payload), io.Discard, logger); err != nil {
				t.Fatal(err)
			}
			if tc.wantFailure {
				if got := state.Load(sessionID); !reflect.DeepEqual(got, prior) {
					t.Fatalf("failure changed state: %+v; logs: %s", got, logs.String())
				}
				if tc.recordFailure && !strings.Contains(logs.String(), "enqueue: recorder:") {
					t.Fatalf("did not reach recorder failure: %s", logs.String())
				}
				if tc.oldDaemon && !strings.Contains(logs.String(), "restart") {
					t.Fatalf("missing restart instruction: %s", logs.String())
				}
				if tc.reject {
					mu.Lock()
					reject = false
					mu.Unlock()
					if err := Hook(context.Background(), bytes.NewReader(payload), io.Discard, logger); err != nil {
						t.Fatal(err)
					}
				} else {
					mu.Lock()
					defer mu.Unlock()
					if httpRequests != 0 || len(ids) != 0 {
						t.Fatalf("exported after setup failure: HTTP=%d spans=%v", httpRequests, ids)
					}
					return
				}
			}
			if got := state.Load(sessionID); got.Offset != int64(len(text)) {
				t.Fatalf("offset=%d, want %d; logs: %s", got.Offset, len(text), logs.String())
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
			if tc.reject {
				wantSpans = 2
			}
			if httpRequests != 0 || len(ids) != wantSpans || ids[0] == "" {
				t.Fatalf("HTTP=%d spans=%v", httpRequests, ids)
			}
			if tc.reject && ids[0] != ids[1] {
				t.Fatalf("retry changed generation ID: %v", ids)
			}
			if !strings.Contains(output, "retained reply") || mode != "full" || operation != "chat" {
				t.Fatalf("output=%q capture=%q operation=%q", output, mode, operation)
			}
		})
	}
}
