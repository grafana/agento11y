package kiro

import (
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/fragmentstore"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTelTurnConfirmation(t *testing.T) {
	for _, failure := range []string{"", "rejected", "partial", "invalid", "opt-in", "endpoint", "old daemon"} {
		t.Run(failure, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			setupHook(t)
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			var marked, generationHTTP, auxiliary atomic.Int64
			var lastID atomic.Value
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
									if attr.Key == "agento11y.generation.id" {
										lastID.Store(attr.Value.GetStringValue())
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
			case "opt-in":
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			case "endpoint":
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			case "old daemon":
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
			}
			exportTurn = sendTurn
			fire(t, `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt":"hello"}`)
			fire(t, `{"hook_event_name":"postToolUse","session_id":"s","tool_name":"bash","tool_input":{"cmd":"true"},"tool_response":{"ok":true}}`)
			fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
			require.Zero(t, generationHTTP.Load())
			if failure != "" {
				s, _, err := fragmentstore.ReadJSON[session](statePath("s"))
				require.NoError(t, err)
				require.Len(t, s.Turns, 1)
				require.Equal(t, "hello", s.Turns[0].Prompt)
				require.False(t, s.Turns[0].CompletedAt.IsZero())
				if failure != "rejected" && failure != "partial" {
					require.Zero(t, marked.Load())
					return
				}
				require.Equal(t, int64(1), marked.Load())
				require.Equal(t, s.Turns[0].ID, lastID.Load())
				rejecting.Store(false)
				fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
				require.Equal(t, s.Turns[0].ID, lastID.Load())
			}
			wantMarked := int64(1)
			if failure != "" {
				wantMarked = 2
			}
			require.Equal(t, wantMarked, marked.Load())
			require.Zero(t, generationHTTP.Load())
			require.Positive(t, auxiliary.Load())
			require.NoFileExists(t, statePath("s"))
			fire(t, `{"hook_event_name":"Stop","session_id":"s"}`)
			require.Equal(t, wantMarked, marked.Load())
		})
	}
}

func TestOTelCanceledSetup(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_PROTOCOL", "otel")
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	now := time.Now()
	err := sendTurn(ctx, "s", turn{ID: "gen", StartedAt: now, CompletedAt: now}, agento11y.ContentCaptureModeFull, log.New(io.Discard, "", 0))
	require.ErrorContains(t, err, "generation tracer provider")
}
