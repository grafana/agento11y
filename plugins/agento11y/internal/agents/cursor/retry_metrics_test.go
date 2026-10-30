package cursor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// A stop whose generation export fails keeps the fragment, and sessionEnd
// sends it again. Sigil stores a generation ID once, so the token metric must
// count the turn once too: not again when stop's metrics arrived, even if
// only an earlier periodic export delivered them, and still once when they did
// not.
func TestHookRetryAfterFailedStopCountsTokensOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// metrics is what happens to stop's metric exports: all delivered,
		// the first delivered and the rest rejected, or all rejected.
		metrics string
	}{
		{name: "metrics delivered", metrics: "delivered"},
		{name: "metrics delivered before the final flush failed", metrics: "first"},
		{name: "metrics also failed", metrics: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			t.Setenv("XDG_STATE_HOME", state)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(state, "config"))
			t.Setenv("HOME", state)

			var (
				mu      sync.Mutex
				failing = true
				// failedHookMetricExports counts metric exports while the export fails.
				failedHookMetricExports int
				stored                  = map[string]int{}
				// Cumulative histograms repeat their running total on every
				// export, so keep the latest sum per series. Each hook process
				// starts new series.
				tokenSeries = map[string]float64{}
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/generations:export":
					if failing {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					var request struct {
						Generations []map[string]any `json:"generations"`
					}
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					results := make([]map[string]any, 0, len(request.Generations))
					for _, generation := range request.Generations {
						id, _ := generation["id"].(string)
						stored[id]++
						results = append(results, map[string]any{"generation_id": id, "accepted": true})
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
				case "/v1/metrics":
					if failing {
						failedHookMetricExports++
						if tc.metrics == "rejected" || (tc.metrics == "first" && failedHookMetricExports > 1) {
							// A permanent rejection, so the exporter doesn't back off.
							w.WriteHeader(http.StatusBadRequest)
							return
						}
					}
					var request colmetricpb.ExportMetricsServiceRequest
					if err := proto.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					for _, resource := range request.ResourceMetrics {
						for _, scope := range resource.ScopeMetrics {
							for _, metric := range scope.Metrics {
								if metric.Name != "gen_ai.client.token.usage" {
									continue
								}
								for _, point := range metric.GetHistogram().GetDataPoints() {
									key := fmt.Sprint(point.GetStartTimeUnixNano())
									for _, attribute := range point.Attributes {
										key += "," + attribute.Key + "=" + attribute.Value.GetStringValue()
									}
									tokenSeries[key] = point.GetSum()
								}
							}
						}
					}
					w.Header().Set("Content-Type", "application/x-protobuf")
				case "/v1/traces":
					w.Header().Set("Content-Type", "application/x-protobuf")
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()

			envconfig.PinAliasEnvBlank(t)
			t.Setenv("SIGIL_ENDPOINT", server.URL)
			t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant")
			t.Setenv("SIGIL_AUTH_TOKEN", "token")
			t.Setenv("SIGIL_PROTOCOL", "http")
			t.Setenv("SIGIL_USER_ID", "")
			t.Setenv("SIGIL_TAGS", "")
			t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "full")
			t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			t.Setenv("AGENTO11Y_AUTO_CODING_AGENT_TAGS", "false")

			base := map[string]any{
				"conversation_id": "conv-retry",
				"workspace_roots": []string{t.TempDir()},
				"user_email":      "dev@example.com",
				"cursor_version":  "2.0.1",
			}
			var logs bytes.Buffer
			send := func(event map[string]any) {
				t.Helper()
				payload := maps.Clone(base)
				maps.Copy(payload, event)
				raw, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				if err := Hook(context.Background(), bytes.NewReader(raw), io.Discard, log.New(&logs, "", 0)); err != nil {
					t.Fatalf("Hook(%s): %v", event["hook_event_name"], err)
				}
			}

			send(map[string]any{
				"hook_event_name": "sessionStart",
				"session_id":      "conv-retry",
				"timestamp":       "2026-10-30T09:00:00Z",
			})
			send(map[string]any{
				"hook_event_name": "beforeSubmitPrompt",
				"generation_id":   "gen-retry",
				"prompt":          "list go files",
				"timestamp":       "2026-10-30T09:00:01Z",
			})
			send(map[string]any{
				"hook_event_name": "afterAgentResponse",
				"generation_id":   "gen-retry",
				"text":            "done",
				"model":           "gpt-5",
				"timestamp":       "2026-10-30T09:00:02Z",
			})
			send(map[string]any{
				"hook_event_name": "stop",
				"generation_id":   "gen-retry",
				"model":           "gpt-5",
				"status":          "completed",
				"loop_count":      0,
				"input_tokens":    3,
				"output_tokens":   50,
				"timestamp":       "2026-10-30T09:00:03Z",
			})
			if !strings.Contains(logs.String(), "preserving fragment for sessionEnd retry") {
				t.Fatalf("stop's export did not fail:\n%s", logs.String())
			}
			mu.Lock()
			failing = false
			mu.Unlock()
			send(map[string]any{
				"hook_event_name": "sessionEnd",
				"session_id":      "conv-retry",
				"reason":          "window_close",
				"final_status":    "completed",
				"timestamp":       "2026-10-30T09:01:00Z",
			})

			mu.Lock()
			defer mu.Unlock()
			if len(stored) != 1 {
				t.Fatalf("stored generations = %v, want exactly one\nhook log:\n%s", stored, logs.String())
			}
			var total float64
			for _, sum := range tokenSeries {
				total += sum
			}
			// The turn has 3 input and 50 output tokens.
			const turnTokens = 53
			if total != turnTokens {
				t.Fatalf("gen_ai.client.token.usage counted %v tokens for one stored turn of %d", total, turnTokens)
			}
		})
	}
}
