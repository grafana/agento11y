package claudecode

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// Claude Code kills a SessionEnd hook that is still running 1.5s into its exit,
// and a slow OTLP endpoint can hold the hook in a flush past that, after the
// turn's metrics arrived but before the hook saved what it exported. The
// resumed session's next hook then sends the turn again and must not count its
// tokens a second time.
func TestHookKilledAfterMetricsCountsTokensOnce(t *testing.T) {
	const sessionID = "killed-session"
	for _, tc := range []struct {
		name  string
		env   map[string]string
		lines []string
		// stalls is the OTLP path that never answers while the first hook runs.
		stalls string
		// sigil is whether generations go to the Sigil export API.
		sigil bool
		// tokens is what the killed hook counts, input and output together.
		tokens float64
	}{
		{
			// The hook dies in its final trace flush.
			name:   "after the tool call",
			stalls: "/v1/traces",
			sigil:  true,
			tokens: 10,
			lines: []string{
				buildHookUserJSONL(sessionID, "list the files"),
				buildHookAssistantToolUseJSONLAt(sessionID, "req_a", "tu_1", "Bash", hookFixtureTimestamp),
				buildHookToolResultJSONL(sessionID, "tu_1", "a.go"),
			},
		},
		{
			// Over the OTel transport, a generation that fails validation ends the
			// hook early, and it dies while flushing what it had.
			name:   "after an invalid generation over OTel",
			stalls: "/v1/traces",
			// Both turns: the SDK counts a generation's tokens even when it fails
			// validation.
			tokens: 53 + 12,
			env: map[string]string{
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
			},
			lines: []string{
				buildHookUserJSONLAt(sessionID, "one", "2025-06-01T12:00:00Z"),
				buildHookAssistantLineAt(sessionID, "request-1", "end_turn", "2025-06-01T12:00:01Z", []any{
					map[string]any{"type": "text", "text": "one"},
				}, 50),
				buildHookUserJSONLAt(sessionID, "two", "2025-06-01T12:00:02Z"),
				strings.Replace(buildHookAssistantLineAt(sessionID, "request-2", "end_turn", "2025-06-01T12:00:03Z", []any{
					map[string]any{"type": "text", "text": "two"},
				}, 9), `"model":"claude-opus-4"`, `"model":""`, 1),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var (
				mu       sync.Mutex
				resumed  bool
				received = make(chan struct{}, 1)
				stored   = map[string]int{}
				// The latest cumulative sum per series, by whether the resumed hook sent it.
				tokenSeries = map[bool]map[string]float64{false: {}, true: {}}
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				phase := resumed
				mu.Unlock()
				if r.URL.Path == tc.stalls && !phase {
					<-r.Context().Done()
					return
				}
				switch r.URL.Path {
				case "/api/v1/generations:export":
					var request struct {
						Generations []struct {
							ID string `json:"id"`
						} `json:"generations"`
					}
					_ = json.Unmarshal(body, &request)
					mu.Lock()
					for _, generation := range request.Generations {
						stored[generation.ID]++
					}
					mu.Unlock()
					r.Body = io.NopCloser(bytes.NewReader(body))
					acceptGenerationExport(w, r)
					return
				case "/v1/metrics":
					var request colmetricpb.ExportMetricsServiceRequest
					_ = proto.Unmarshal(body, &request)
					tokens := false
					mu.Lock()
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
									tokenSeries[phase][key] = point.GetSum()
									tokens = true
								}
							}
						}
					}
					mu.Unlock()
					if tokens && !phase {
						select {
						case received <- struct{}{}:
						default:
						}
					}
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
			}))
			defer server.Close()
			setHookExportEnv(t, server.URL)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			path := filepath.Join(t.TempDir(), "session.jsonl")
			if err := os.WriteFile(path, []byte(strings.Join(tc.lines, "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(hookInput{HookEventName: "SessionEnd", SessionID: sessionID, TranscriptPath: path})
			if err != nil {
				t.Fatal(err)
			}

			var childLog bytes.Buffer
			child := exec.Command(os.Args[0], "-test.run=^$")
			child.Env = append(os.Environ(), hookChildEnv+"=1")
			child.Stdin = bytes.NewReader(payload)
			child.Stderr = &childLog
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-received:
			case <-time.After(10 * time.Second):
				_ = child.Process.Kill()
				_ = child.Wait()
				t.Fatalf("the hook never delivered its token metrics:\n%s", childLog.String())
			}
			_ = child.Process.Kill()
			_ = child.Wait()
			// The kill has to land before the hook saved its offset, or the
			// resumed hook below has nothing to send again.
			if killed := state.Load(sessionID); killed.Offset != 0 {
				t.Fatalf("the hook finished (offset %d) before it was killed:\n%s", killed.Offset, childLog.String())
			}

			mu.Lock()
			resumed = true
			mu.Unlock()
			runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path})

			mu.Lock()
			defer mu.Unlock()
			if tc.sigil {
				for id, sends := range stored {
					if sends != 2 {
						t.Errorf("generation %s sent %d times, want 2 (killed hook and resumed hook)", id, sends)
					}
				}
				if len(stored) != 1 {
					t.Errorf("stored generations = %v, want the tool-call turn", stored)
				}
			}
			var killedTokens float64
			for _, sum := range tokenSeries[false] {
				killedTokens += sum
			}
			if killedTokens != tc.tokens {
				t.Errorf("the killed hook counted %v tokens, want %v", killedTokens, tc.tokens)
			}
			for key, sum := range tokenSeries[true] {
				if sum > 0 {
					t.Errorf("the resumed hook counted %v tokens again (%s)", sum, key)
				}
			}
		})
	}
}

// The cost of remembering records before export: a hook killed while every
// endpoint stalls loses that turn's token metrics. The resumed hook sends the
// generation again, so Sigil stores it, but counts none of its tokens. Undo
// this only together with the double count it prevents.
func TestHookKilledBeforeMetricsLosesThem(t *testing.T) {
	const sessionID = "stalled-session"
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var (
		mu      sync.Mutex
		resumed bool
		stored  int
		largest float64
		// exporting is signalled when the first hook's export reaches the server.
		exporting = make(chan struct{}, 1)
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		phase := resumed
		mu.Unlock()
		if !phase {
			select {
			case exporting <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		switch r.URL.Path {
		case "/api/v1/generations:export":
			mu.Lock()
			stored++
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		case "/v1/metrics":
			var request colmetricpb.ExportMetricsServiceRequest
			_ = proto.Unmarshal(body, &request)
			mu.Lock()
			for _, resource := range request.ResourceMetrics {
				for _, scope := range resource.ScopeMetrics {
					for _, metric := range scope.Metrics {
						if metric.Name != "gen_ai.client.token.usage" {
							continue
						}
						for _, point := range metric.GetHistogram().GetDataPoints() {
							largest = max(largest, point.GetSum())
						}
					}
				}
			}
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)

	path := filepath.Join(t.TempDir(), "session.jsonl")
	lines := []string{
		buildHookUserJSONL(sessionID, "list the files"),
		buildHookAssistantToolUseJSONLAt(sessionID, "req_a", "tu_1", "Bash", hookFixtureTimestamp),
		buildHookToolResultJSONL(sessionID, "tu_1", "a.go"),
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(hookInput{HookEventName: "SessionEnd", SessionID: sessionID, TranscriptPath: path})
	if err != nil {
		t.Fatal(err)
	}

	var childLog bytes.Buffer
	child := exec.Command(os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), hookChildEnv+"=1")
	child.Stdin = bytes.NewReader(payload)
	child.Stderr = &childLog
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	// Kill the hook while it is stuck exporting the turn.
	select {
	case <-exporting:
	case <-time.After(10 * time.Second):
		_ = child.Process.Kill()
		_ = child.Wait()
		t.Fatalf("the hook never exported:\n%s", childLog.String())
	}
	_ = child.Process.Kill()
	_ = child.Wait()
	if killed := state.Load(sessionID); killed.Offset != 0 {
		t.Fatalf("the hook finished (offset %d) before it was killed:\n%s", killed.Offset, childLog.String())
	}

	mu.Lock()
	resumed = true
	mu.Unlock()
	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: sessionID, TranscriptPath: path})

	mu.Lock()
	defer mu.Unlock()
	if stored != 1 {
		t.Errorf("the resumed hook sent the generation %d times, want 1", stored)
	}
	if largest != 0 {
		t.Errorf("the resumed hook counted tokens (a datapoint of %v), want none: the turn was remembered before the kill", largest)
	}
}

// A collector that comes back after a failed hook's last flush must not
// receive metrics the hook did not save as delivered: the retry would count
// them again. Every metric export is rejected until the generation export
// fails, then one more, then they are accepted.
func TestHookRetryAfterLateCollectorCountsTokensOnce(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	var (
		mu          sync.Mutex
		failing     = true
		gaveUp      bool
		rejectAfter = 1
		tokenSeries = map[string]float64{}
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/generations:export":
			if failing {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		case "/v1/metrics":
			if failing && (!gaveUp || rejectAfter > 0) {
				if gaveUp {
					rejectAfter--
				}
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			var request colmetricpb.ExportMetricsServiceRequest
			_ = proto.Unmarshal(body, &request)
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
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)

	const sessionID = "late-collector-session"
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
	payload, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	// The hook logs "flush:" when the generation export has failed.
	logs := &watchWriter{match: "flush:", onMatch: func() {
		mu.Lock()
		gaveUp = true
		mu.Unlock()
	}}
	if err := Hook(context.Background(), bytes.NewReader(payload), io.Discard, log.New(logs, "", 0)); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if !gaveUp {
		mu.Unlock()
		t.Fatalf("the first hook's export did not fail:\n%s", logs.String())
	}
	failing = false
	mu.Unlock()
	runHookForTest(t, input)

	mu.Lock()
	defer mu.Unlock()
	var tokens float64
	for _, sum := range tokenSeries {
		tokens += sum
	}
	// The turn has 3 input and 50 output tokens.
	if tokens != 53 {
		t.Fatalf("gen_ai.client.token.usage counted %v tokens for one turn of 53", tokens)
	}
}

// watchWriter calls onMatch once when a write contains match.
type watchWriter struct {
	bytes.Buffer
	match   string
	onMatch func()
	matched bool
}

func (w *watchWriter) Write(p []byte) (int, error) {
	if !w.matched && bytes.Contains(p, []byte(w.match)) {
		w.matched = true
		w.onMatch()
	}
	return w.Buffer.Write(p)
}
