package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// withoutRecordedMetrics drops the keys a failed export remembers, which
// TestHookRetryAfterFailedExportCountsTokensOnce covers, so a test can check
// that a failure left the rest of the state alone.
func withoutRecordedMetrics(s state.Session) state.Session {
	s.MetricsRecorded = nil
	return s
}

// A hook whose generation export fails keeps its transcript offset, so the
// next hook sends the same turn again. Sigil stores a generation ID once, so
// the token metric must count the turn once too: not again when the failed
// hook's metrics arrived, even if only an earlier periodic export delivered
// them, and still once when they did not.
func TestHookRetryAfterFailedExportCountsTokensOnce(t *testing.T) {
	for _, tc := range []struct {
		name string
		// metrics is what happens to the failed hook's metric exports: all delivered,
		// the first delivered and the rest rejected, or all rejected.
		metrics string
	}{
		{name: "metrics delivered", metrics: "delivered"},
		{name: "metrics delivered before the final flush failed", metrics: "first"},
		{name: "metrics also failed", metrics: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())

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
			tokenSum := func() float64 {
				var total float64
				for _, sum := range tokenSeries {
					total += sum
				}
				return total
			}
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
						Generations []struct {
							ID string `json:"id"`
						} `json:"generations"`
					}
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					for _, generation := range request.Generations {
						stored[generation.ID]++
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					acceptGenerationExport(w, r)
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
			setHookExportEnv(t, server.URL)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)

			const sessionID = "retry-metric-session"
			path := filepath.Join(t.TempDir(), "session.jsonl")
			text := strings.Join([]string{
				buildHookUserJSONLAt(sessionID, "list the files", "2025-06-01T12:00:00Z"),
				buildHookAssistantLineAt(sessionID, "request-1", "end_turn", "2025-06-01T12:00:01Z", []any{
					map[string]any{"type": "text", "text": "done"},
				}, 50),
			}, "\n") + "\n"
			if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
				t.Fatal(err)
			}
			input := hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path}

			firstLogs := runHookForTest(t, input)
			if !strings.Contains(firstLogs, "flush:") {
				t.Fatalf("the first hook's export did not fail:\n%s", firstLogs)
			}
			mu.Lock()
			failing = false
			mu.Unlock()
			runHookForTest(t, input)

			mu.Lock()
			defer mu.Unlock()
			if len(stored) != 1 {
				t.Fatalf("stored generations = %v, want exactly one", stored)
			}
			// The turn has 3 input and 50 output tokens.
			const turnTokens = 53
			if got := tokenSum(); got != turnTokens {
				t.Fatalf("gen_ai.client.token.usage counted %v tokens for one stored turn of %d", got, turnTokens)
			}
			if remembered := state.Load(sessionID).MetricsRecorded; len(remembered) != 0 {
				t.Fatalf("metrics_recorded = %v after a successful export, want it cleared", remembered)
			}
		})
	}
}
