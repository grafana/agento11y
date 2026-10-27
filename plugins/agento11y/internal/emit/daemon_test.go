package emit_test

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
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/config"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/hook"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func TestOldDaemonPreservesCopilotRetry(t *testing.T) {
	for _, insecure := range []bool{false, true} {
		name := "HTTP"
		if insecure {
			name = "HTTPS with insecure"
		}
		t.Run(name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("AGENTO11Y_PROTOCOL", "otel")
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			var capable atomic.Bool
			var health, traces, generationHTTP atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/healthz":
					health.Add(1)
					if r.Header.Get("Authorization") != "" {
						t.Error("health check sent authorization")
					}
					if capable.Load() {
						_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					} else {
						_, _ = io.WriteString(w, `{"status":"ok"}`)
					}
				case "/otlp/v1/traces":
					traces.Add(1)
				case "/otlp/v1/metrics":
				default:
					generationHTTP.Add(1)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			endpoint := srv.URL + "/otlp"
			if insecure {
				endpoint = strings.Replace(endpoint, "http://", "https://", 1)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
			}
			t.Setenv("AGENTO11Y_ENDPOINT", srv.URL)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			var logs bytes.Buffer
			logger := log.New(&logs, "", 0)
			cfg := config.Config{ContentCapture: agento11y.ContentCaptureModeFull}
			hook.UserPromptSubmit(hook.Payload{SessionIDJSON: "retry", Prompt: "test content", Model: "gpt-5"}, cfg, logger)
			stop := hook.Payload{SessionIDJSON: "retry", StopReasonJSON: "end_turn"}
			hook.Stop(stop, cfg, logger)
			session := fragment.LoadSessionTolerant("retry", logger)
			if session == nil || session.ActiveTurnID != "turn-000001" || fragment.LoadTolerant("retry", "turn-000001", logger) == nil {
				t.Fatal("old receiver cleared retry state")
			}
			if health.Load() != 1 || traces.Load() != 0 || generationHTTP.Load() != 0 || !strings.Contains(logs.String(), "restart") {
				t.Fatalf("health=%d traces=%d HTTP=%d logs=%s", health.Load(), traces.Load(), generationHTTP.Load(), logs.String())
			}
			capable.Store(true)
			hook.Stop(stop, cfg, logger)
			session = fragment.LoadSessionTolerant("retry", logger)
			if session == nil || session.ActiveTurnID != "" || fragment.LoadTolerant("retry", "turn-000001", logger) != nil {
				t.Fatal("confirmed retry did not clear state")
			}
			if traces.Load() != 1 || generationHTTP.Load() != 0 {
				t.Fatalf("traces=%d HTTP=%d", traces.Load(), generationHTTP.Load())
			}
		})
	}
}
