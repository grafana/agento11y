package entry

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/opencode/sessiondb/sessiondbtest"
	"github.com/grafana/agento11y/plugins/agento11y/internal/history"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHistoryCLIFreezesTargetBeforeExporterAndLedger(t *testing.T) {
	for _, agent := range []string{"claude-code", "auto"} {
		for _, protocol := range []string{"http", "otel"} {
			t.Run(agent+"/"+protocol, func(t *testing.T) {
				isolateDotenvHome(t)
				withHistoryNow(t)
				writeClaudeHistory(t, "frozen-session", 24*time.Hour)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer collector-before")
				apiToken := "token-before"
				if protocol == "otel" {
					apiToken = ""
				}
				pin := func() {
					t.Setenv("AGENTO11Y_ENDPOINT", "https://127.0.0.1:9999")
					t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant-before")
					t.Setenv("AGENTO11Y_AUTH_TOKEN", apiToken)
					t.Setenv("AGENTO11Y_PROTOCOL", protocol)
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
					t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")
					t.Setenv("AGENTO11Y_HEADERS", "X-Snapshot=before")
				}
				pin()
				withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
					t.Fatal("resolved history must not request HTTP login")
					return login.Result{}, nil
				})
				prev := historyNewExporter
				t.Cleanup(func() { historyNewExporter = prev })
				exported, constructed := 0, 0
				mutate := true
				historyNewExporter = func(_ context.Context, target history.Target, _ *log.Logger) (history.TurnExporter, func(context.Context) error, error) {
					constructed++
					assert.Equal(t, "https://127.0.0.1:9999", target.Endpoint)
					assert.Equal(t, "tenant-before", target.TenantID)
					assert.Equal(t, agento11y.GenerationExportProtocol(protocol), target.Protocol)
					if mutate {
						t.Setenv("AGENTO11Y_ENDPOINT", "https://127.0.0.1:9998")
						t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant-after")
						t.Setenv("AGENTO11Y_AUTH_TOKEN", "token-after")
						t.Setenv("AGENTO11Y_PROTOCOL", "unsupported")
						t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
						t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "invalid")
						t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer after")
						t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
						t.Setenv("AGENTO11Y_HEADERS", "X-Snapshot=after")
					}
					frozen, err := history.ResolveTarget(target, nil)
					require.NoError(t, err)
					assert.Equal(t, "https://127.0.0.1:9999", frozen.Endpoint)
					if protocol == "otel" {
						assert.Equal(t, "http://127.0.0.1:4318", frozen.OTLPEndpoint)
					} else {
						assert.Empty(t, frozen.OTLPEndpoint)
					}
					assert.Equal(t, "tenant-before", frozen.TenantID)
					assert.Equal(t, apiToken, frozen.AuthToken)
					assert.Equal(t, agento11y.GenerationExportProtocol(protocol), frozen.Protocol)
					assert.Equal(t, "full_with_metadata_spans", frozen.ContentMode)
					assert.Equal(t, map[string]string{"X-Snapshot": "before"}, frozen.Headers)
					assert.Equal(t, "Bearer collector-before", frozen.OTLPHeaders["Authorization"])
					require.NotNil(t, frozen.EnableExperimentalFeatures)
					assert.True(t, *frozen.EnableExperimentalFeatures)
					// A non-production exporter makes RunImport use the CLI's target.
					return acceptingExporter{exported: &exported}, func(context.Context) error { return nil }, nil
				}
				args := []string{"history", "import", agent, "--all", "--yes"}
				stdout, stderr, code := runHistory(t, args...)
				require.Nil(t, code, "stdout=%s stderr=%s", stdout, stderr)
				assert.Equal(t, 1, exported)
				mutate = false
				pin()
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer collector-before")
				stdout, stderr, code = runHistory(t, args...)
				require.Nil(t, code, "stderr=%s", stderr)
				assert.Contains(t, stdout, "1 already imported, 0 failed")
				assert.Equal(t, 1, exported)
				assert.Equal(t, 2, constructed)
			})
		}
	}
}

func TestHistoryIndependentAgentsSupportOTel(t *testing.T) {
	for _, agent := range []string{"pi", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			isolateDotenvHome(t)
			withHistoryNow(t)
			writeIndependentHistory(t, agent)
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "Authorization=Bearer must-not-leak")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			storage, err := local.NewStorage(t.TempDir())
			require.NoError(t, err)
			handler := local.NewServer(storage, nil, filepath.Join(t.TempDir(), "config.env"))
			var traces, generations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Empty(t, r.Header.Get("Authorization"))
				switch r.URL.Path {
				case "/otlp/v1/traces":
					traces.Add(1)
					assert.Equal(t, "1", r.Header.Get(history.ForwardMarkerHeader))
				case "/otlp/v1/metrics":
					assert.Equal(t, "1", r.Header.Get(history.ForwardMarkerHeader))
				case "/api/v1/generations:export":
					generations.Add(1)
					t.Error("OTel history sent an HTTP generation export")
				}
				handler.ServeHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			prev := historyEnsureLocal
			t.Cleanup(func() { historyEnsureLocal = prev })
			historyEnsureLocal = func(context.Context) (string, error) { return srv.URL, nil }
			withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
				t.Fatal("local OTel history requested HTTP login")
				return login.Result{}, nil
			})
			stdout, stderr, code := runHistory(t, "history", "import", agent, "--local", "--all", "--yes")
			require.Nil(t, code, "stdout=%s stderr=%s", stdout, stderr)
			assert.Contains(t, stdout, "Imported 1 turns from 1 sessions")
			assert.Positive(t, traces.Load())
			assert.Zero(t, generations.Load())
			conversations, total, err := storage.ListConversations(local.ConversationListOptions{})
			require.NoError(t, err)
			require.Equal(t, 1, total)
			stored, err := storage.ConversationDetail(conversations[0].ID)
			require.NoError(t, err)
			require.Len(t, stored.Generations, 1)
			assert.NotEmpty(t, stored.Generations[0].Output)
			assert.Equal(t, "test answer", stored.Generations[0].Output[0].Parts[0].Text)
		})
	}
}

func TestHistoryOldDaemonStopsBeforeExporter(t *testing.T) {
	for _, agent := range []string{"claude-code", "auto"} {
		t.Run(agent, func(t *testing.T) {
			isolateDotenvHome(t)
			withHistoryNow(t)
			writeClaudeHistory(t, "session", 24*time.Hour)
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/healthz", r.URL.Path)
				_, _ = io.WriteString(w, `{}`)
			}))
			t.Cleanup(srv.Close)
			prevLocal, prevExporter := historyEnsureLocal, historyNewExporter
			t.Cleanup(func() { historyEnsureLocal, historyNewExporter = prevLocal, prevExporter })
			historyEnsureLocal = func(context.Context) (string, error) { return srv.URL, nil }
			historyNewExporter = func(context.Context, history.Target, *log.Logger) (history.TurnExporter, func(context.Context) error, error) {
				t.Fatal("old daemon allowed capture to start")
				return nil, nil, nil
			}
			_, stderr, code := runHistory(t, "history", "import", agent, "--local", "--all", "--yes")
			require.NotNil(t, code)
			assert.Equal(t, 1, *code)
			assert.Contains(t, stderr, "restart")
		})
	}
}

func writeIndependentHistory(t *testing.T, agent string) {
	t.Helper()
	created, updated := historyFixedNow.Add(-25*time.Hour), historyFixedNow.Add(-24*time.Hour)
	if agent == "opencode" {
		db := sessiondbtest.New(t)
		t.Setenv("OPENCODE_DB", db.Path)
		db.AddSession("session", "", "test history", "/work/repo", created.UnixMilli(), updated.UnixMilli())
		db.AddMessage("assistant", "session", created.UnixMilli(), updated.UnixMilli(),
			fmt.Sprintf(`{"role":"assistant","modelID":"test-model","providerID":"test-provider","time":{"created":%d,"completed":%d}}`, created.UnixMilli(), updated.UnixMilli()))
		db.AddPart("text", "assistant", "session", created.UnixMilli(), updated.UnixMilli(), `{"type":"text","text":"test answer"}`)
		return
	}
	root := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", root)
	path := filepath.Join(root, "sessions", "--work-repo--", "test-session.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	body := fmt.Sprintf(`{"type":"session","version":3,"id":"test-session","timestamp":%q,"cwd":"/work/repo"}
{"type":"message","id":"user","parentId":null,"timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":"test prompt"}],"timestamp":%d}}
{"type":"message","id":"assistant","parentId":"user","timestamp":%q,"message":{"role":"assistant","api":"anthropic-messages","provider":"test-provider","model":"test-model","content":[{"type":"text","text":"test answer"}],"usage":{"input":10,"output":5,"cacheRead":0,"cacheWrite":0,"totalTokens":15,"cost":{"total":0}},"stopReason":"stop","timestamp":%d}}
`, created.Format(time.RFC3339), created.Format(time.RFC3339), created.UnixMilli(), updated.Format(time.RFC3339), updated.UnixMilli())
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chtimes(path, updated, updated))
}
