package hook

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/cursor/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTelConfirmationPreservesCursorFragments(t *testing.T) {
	for _, handler := range []struct {
		name string
		run  func(Payload, config.Config, *log.Logger)
	}{
		{"stop", Stop},
		{"sessionEnd", SessionEnd},
	} {
		for _, failure := range []string{"", "rejected", "partial", "record", "invalid", "opt-in", "endpoint", "old daemon"} {
			t.Run(handler.name+"/"+failure, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("XDG_STATE_HOME", t.TempDir())
				t.Setenv("AGENTO11Y_PROTOCOL", "otel")
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				var marked, generationHTTP, auxiliary atomic.Int64
				var rejecting atomic.Bool
				rejecting.Store(failure == "rejected" || failure == "partial")
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case r.URL.Path == "/healthz":
						if failure == "old daemon" {
							_, _ = w.Write([]byte(`{"status":"ok"}`))
						} else {
							_, _ = w.Write([]byte(`{"capabilities":["otel_generation_ingest_v1"]}`))
						}
						return
					case strings.Contains(r.URL.Path, "generations"):
						generationHTTP.Add(1)
					case strings.HasSuffix(r.URL.Path, "/traces"):
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
				require.NoError(t, fragment.SaveSession(fragment.Session{ConversationID: "conv", UserEmail: "user@example.test"}))
				ids := []string{"gen"}
				if handler.name == "sessionEnd" {
					ids = append(ids, "stranded")
				}
				for _, id := range ids {
					require.NoError(t, fragment.Save(&fragment.Fragment{ConversationID: "conv", GenerationID: id, Model: "gpt-5", UserPrompt: "hello"}))
				}
				if failure == "record" {
					frag := fragment.LoadTolerant("conv", ids[len(ids)-1], logger)
					frag.Tools = []fragment.ToolRecord{{ToolName: " "}}
					require.NoError(t, fragment.Save(frag))
				}
				if handler.name == "sessionEnd" && failure == "rejected" {
					require.NoError(t, fragment.Save(&fragment.Fragment{ConversationID: "conv", GenerationID: "75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi", ThinkingPresent: true}))
				}
				payload := Payload{ConversationID: "conv", GenerationID: "gen", Status: "completed"}
				cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
				if failure == "invalid" {
					client, err := buildClient(Payload{}, nil, cfg, nil, logger)
					require.Error(t, err)
					require.Nil(t, client)
				}
				handler.run(payload, cfg, logger)
				require.Zero(t, generationHTTP.Load())
				require.Equal(t, failure == "invalid", config.HasCredentials())
				if failure == "invalid" {
					require.Contains(t, logs.String(), handler.name+": generation transport")
					require.Zero(t, auxiliary.Load())
				}
				if failure == "record" {
					bad := fragment.LoadTolerant("conv", ids[len(ids)-1], logger)
					require.NotNil(t, bad)
					require.Equal(t, int64(len(ids)-1), marked.Load())
					if handler.name == "sessionEnd" {
						require.Nil(t, fragment.LoadTolerant("conv", "gen", logger))
						require.NotNil(t, fragment.LoadSession("conv", logger))
					}
					bad.Tools = nil
					require.NoError(t, fragment.Save(bad))
					handler.run(payload, cfg, logger)
				} else if failure != "" {
					for _, id := range ids {
						require.NotNil(t, fragment.LoadTolerant("conv", id, logger))
					}
					session := fragment.LoadSession("conv", logger)
					require.NotNil(t, session)
					require.Equal(t, "user@example.test", session.UserEmail)
					if handler.name == "stop" {
						require.Equal(t, "completed", fragment.LoadTolerant("conv", "gen", logger).PendingStop.Status)
					}
					if handler.name == "sessionEnd" && failure == "rejected" {
						require.Nil(t, fragment.LoadTolerant("conv", "75945372-0ac1-4187-a87c-465c20ee3eba-0-75wi", logger))
					}
					if failure != "rejected" && failure != "partial" {
						require.Zero(t, marked.Load())
						return
					}
					require.Equal(t, int64(len(ids)), marked.Load())
					rejecting.Store(false)
					handler.run(payload, cfg, logger)
				}
				wantMarked := int64(len(ids))
				if failure == "rejected" || failure == "partial" {
					wantMarked *= 2
				}
				require.Equal(t, wantMarked, marked.Load())
				require.Zero(t, generationHTTP.Load())
				require.Positive(t, auxiliary.Load())
				for _, id := range ids {
					require.Nil(t, fragment.LoadTolerant("conv", id, logger))
				}
			})
		}
	}
}

func TestStrandedUnreadableFragmentIsQuarantined(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	require.NoError(t, fragment.Save(&fragment.Fragment{ConversationID: "conv", GenerationID: "bad"}))
	path := fragment.FragmentFilePath("conv", "bad")
	require.NoError(t, os.WriteFile(path, []byte("{"), 0600))
	emitted, ok := emitOneStranded(t.Context(), nil, nil, "conv", "bad", config.Config{}, log.New(io.Discard, "", 0))
	require.False(t, emitted)
	require.True(t, ok)
	require.NoFileExists(t, path)
	quarantined, err := filepath.Glob(path + ".corrupt-*")
	require.NoError(t, err)
	require.Len(t, quarantined, 1)
	body, err := os.ReadFile(quarantined[0])
	require.NoError(t, err)
	require.Equal(t, "{", string(body))
}
