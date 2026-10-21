package hook

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/codex/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

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
