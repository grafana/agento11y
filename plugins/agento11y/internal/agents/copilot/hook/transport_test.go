package hook

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTelStopConfirmation(t *testing.T) {
	for _, failure := range []string{"", "rejected", "partial", "invalid", "opt-in", "endpoint", "old daemon", "old daemon insecure"} {
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
					if failure == "old daemon" || failure == "old daemon insecure" {
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
			cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
			UserPromptSubmit(Payload{SessionIDJSON: "sess", Prompt: "hello", Model: "gpt-5"}, cfg, logger)
			PostToolUse(Payload{SessionIDJSON: "sess", ToolNameJSON: "bash", ToolInputJSON: []byte(`{"cmd":"true"}`), ToolResultJSON: []byte(`{"text_result_for_llm":"ok"}`)}, cfg, logger, false)
			payload := Payload{SessionIDJSON: "sess", StopReasonJSON: "end_turn"}
			Stop(payload, cfg, logger)
			require.Zero(t, generationHTTP.Load())
			require.False(t, config.HasCredentials())
			if failure != "" {
				session := fragment.LoadSessionTolerant("sess", logger)
				require.NotNil(t, session)
				require.Equal(t, "turn-000001", session.ActiveTurnID)
				require.NotNil(t, fragment.LoadTolerant("sess", session.ActiveTurnID, logger))
				if failure != "rejected" && failure != "partial" {
					require.Zero(t, marked.Load())
					return
				}
				require.Equal(t, int64(1), marked.Load())
				rejecting.Store(false)
				Stop(payload, cfg, logger)
			}
			wantMarked := int64(1)
			if failure != "" {
				wantMarked = 2
			}
			require.Equal(t, wantMarked, marked.Load())
			require.Zero(t, generationHTTP.Load())
			require.Positive(t, auxiliary.Load())
			require.Empty(t, fragment.LoadSessionTolerant("sess", logger).ActiveTurnID)
			require.Nil(t, fragment.LoadTolerant("sess", "turn-000001", logger))
			Stop(payload, cfg, logger)
			require.Equal(t, wantMarked, marked.Load())
		})
	}
}
