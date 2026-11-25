package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/state"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/claudecode/transcript"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// A fork (claude --resume --fork-session, SessionStart source "fork") starts a
// new session whose transcript copies the original's turns with their
// timestamps and a new session ID. The original already exported those turns;
// the fork must export only its own, and count only their tokens.
func TestHookForkExportsOnlyItsOwnTurns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		// want is how many generations the fork's first Stop exports.
		want int
	}{
		{name: "fork", source: "fork", want: 1},
		// A session agento11y first sees on resume has no copies to skip: the
		// earlier turns are its own and were never exported.
		{name: "resume", source: "resume", want: 3},
		{name: "no SessionStart", want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("XDG_STATE_HOME", t.TempDir())

			var (
				mu          sync.Mutex
				exported    []string
				tokenSeries = map[string]float64{}
				// copiedToolSpans counts trace exports that mention the copied tool call.
				copiedToolSpans int
			)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/generations:export":
					var request struct {
						Generations []struct {
							ID string `json:"id"`
						} `json:"generations"`
					}
					_ = json.Unmarshal(body, &request)
					for _, generation := range request.Generations {
						exported = append(exported, generation.ID)
					}
					r.Body = io.NopCloser(bytes.NewReader(body))
					acceptGenerationExport(w, r)
					return
				case "/v1/traces":
					if bytes.Contains(body, []byte("tu_1")) {
						copiedToolSpans++
					}
				case "/v1/metrics":
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

			const fork = "fork-session"
			path := filepath.Join(t.TempDir(), fork+".jsonl")
			if tc.source != "" {
				runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: tc.source})
			}

			// Claude Code writes the fork's transcript with its first prompt: the
			// original's turns, stamped when they happened, then the new turn.
			now := time.Now().UTC()
			at := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339Nano) }
			copied := []string{
				buildHookUserJSONLAt(fork, "list the files", "2025-06-01T12:00:00Z"),
				buildHookAssistantToolUseJSONLAt(fork, "req_1", "tu_1", "Bash", "2025-06-01T12:00:01Z"),
				buildHookToolResultJSONLAt(fork, "tu_1", "a.go", "2025-06-01T12:00:02Z"),
				buildHookAssistantJSONLAt(fork, "req_2", "end_turn", "two files", 5, "2025-06-01T12:00:03Z"),
			}
			own := []string{
				buildHookUserJSONLAt(fork, "say done", at(50*time.Millisecond)),
				buildHookAssistantJSONLAt(fork, "req_3", "end_turn", "done", 9, at(400*time.Millisecond)),
			}
			if err := os.WriteFile(path, []byte(strings.Join(append(copied, own...), "\n")+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})

			mu.Lock()
			first := slices.Clone(exported)
			var tokens float64
			for _, sum := range tokenSeries {
				tokens += sum
			}
			mu.Unlock()
			if len(first) != tc.want {
				t.Fatalf("first Stop exported %d generations, want %d:\n%s", len(first), tc.want, logs)
			}
			if tc.source == "fork" {
				// Only req_3 is the fork's: 3 input and 9 output tokens.
				if tokens != 12 {
					t.Errorf("gen_ai.client.token.usage counted %v tokens, want the fork's own 12", tokens)
				}
				if copiedToolSpans != 0 {
					t.Errorf("%d trace exports carry the copied tool call", copiedToolSpans)
				}
				// The copied tool call is not tracked as the fork's.
				if pending := state.Load(fork).Tools.Pending; len(pending) != 0 {
					t.Errorf("pending tools after the fork's Stop = %v, want none", slices.Collect(maps.Keys(pending)))
				}
			}

			// A later turn in the fork is exported as usual.
			later := buildHookUserJSONLAt(fork, "again", at(time.Second)) + "\n" +
				buildHookAssistantJSONLAt(fork, "req_4", "end_turn", "again", 4, at(1500*time.Millisecond)) + "\n"
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.WriteString(later); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
			runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
			mu.Lock()
			defer mu.Unlock()
			if got := len(exported) - len(first); got != 1 {
				t.Errorf("second Stop exported %d generations, want the new turn", got)
			}
		})
	}
}

// Without CLAUDE_CODE_EAGER_FLUSH, a fork's Stop can run before its own turn
// reaches the transcript, with only the copies on disk. That hook must not
// export the copies; the next one, once the fork's turn has landed, skips them.
func TestHookForkWaitsForItsOwnTurn(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var (
		mu       sync.Mutex
		exported []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/generations:export" {
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Generations []struct {
					ID string `json:"id"`
				} `json:"generations"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			for _, generation := range request.Generations {
				exported = append(exported, generation.ID)
			}
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	const fork = "fork-session"
	path := filepath.Join(t.TempDir(), fork+".jsonl")
	runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: "fork"})
	copied := strings.Join([]string{
		buildHookUserJSONLAt(fork, "list the files", "2025-06-01T12:00:00Z"),
		buildHookAssistantJSONLAt(fork, "req_1", "end_turn", "two files", 5, "2025-06-01T12:00:01Z"),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(copied), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
	mu.Lock()
	early := len(exported)
	mu.Unlock()
	if early != 0 {
		t.Fatalf("a Stop with only the copies on disk exported %d generations:\n%s", early, logs)
	}

	now := time.Now().UTC()
	own := buildHookUserJSONLAt(fork, "say done", now.Add(50*time.Millisecond).Format(time.RFC3339Nano)) + "\n" +
		buildHookAssistantJSONLAt(fork, "req_2", "end_turn", "done", 9, now.Add(400*time.Millisecond).Format(time.RFC3339Nano)) + "\n"
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(own); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
	mu.Lock()
	defer mu.Unlock()
	if len(exported) != 1 {
		t.Errorf("exported %d generations once the fork's turn landed, want its own 1", len(exported))
	}
}

// Without CLAUDE_CODE_EAGER_FLUSH a Stop can run with the fork's prompt on disk
// and its reply still on the way. The copies end in a finished turn, so the
// fork's Stop has to settle on its own turn, past them, as a fresh session's
// Stop would.
func TestHookForkStopWaitsForItsOwnReply(t *testing.T) {
	prev := transcriptSettleWindow
	transcriptSettleWindow = 2 * time.Second
	t.Cleanup(func() { transcriptSettleWindow = prev })
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var (
		mu       sync.Mutex
		exported []string
		bodies   []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/generations:export" {
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Generations []struct {
					ID string `json:"id"`
				} `json:"generations"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			for _, generation := range request.Generations {
				exported = append(exported, generation.ID)
			}
			bodies = append(bodies, body...)
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	const fork = "fork-session"
	path := filepath.Join(t.TempDir(), fork+".jsonl")
	runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: "fork"})
	text := strings.Join([]string{
		buildHookUserJSONLAt(fork, "list the files", "2025-06-01T12:00:00Z"),
		buildHookAssistantJSONLAt(fork, "req_1", "end_turn", "two files", 5, "2025-06-01T12:00:01Z"),
		buildHookUserJSONLAt(fork, "say done", time.Now().UTC().Format(time.RFC3339Nano)),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	landed := make(chan struct{})
	go func() {
		defer close(landed)
		time.Sleep(300 * time.Millisecond)
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = f.WriteString(buildHookAssistantJSONLAt(fork, "req_2", "end_turn", "done", 9, time.Now().UTC().Format(time.RFC3339Nano)) + "\n")
	}()
	logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
	<-landed
	mu.Lock()
	defer mu.Unlock()
	if len(exported) != 1 || !bytes.Contains(bodies, []byte(`"response_id":"req_2"`)) {
		t.Errorf("the fork's Stop exported %d generations (%s), want only its own reply:\n%s", len(exported), bodies, logs)
	}
}

func TestDropForkCopies(t *testing.T) {
	forkedAt := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	line := func(ts string, end int64) transcript.Line {
		return transcript.Line{Type: "assistant", Timestamp: ts, EndOffset: end}
	}
	for _, tc := range []struct {
		name  string
		lines []transcript.Line
		want  []int64
	}{
		{name: "empty"},
		{name: "copies only", lines: []transcript.Line{line("2026-10-31T11:00:00Z", 10), line("2026-10-31T11:00:01Z", 20)}},
		{name: "copies then the fork's turn", lines: []transcript.Line{
			line("2026-10-31T11:00:01Z", 20), line("2026-10-31T12:00:01Z", 30),
		}, want: []int64{30}},
		{name: "the fork's turn above the copies", lines: []transcript.Line{
			line("2026-10-31T12:00:01Z", 10), line("2026-10-31T11:00:01Z", 20),
		}, want: []int64{10}},
		// Before is strict: a line stamped at the fork's start is the fork's.
		{name: "stamped at the fork's start", lines: []transcript.Line{line("2026-10-31T12:00:00Z", 20)}, want: []int64{20}},
		// An unreadable timestamp is kept, as a fork without the check would keep it.
		{name: "unreadable timestamp", lines: []transcript.Line{line("2026-10-31T11:00:01Z", 20), line("yesterday", 30)}, want: []int64{30}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []int64
			for _, l := range dropForkCopies(tc.lines, forkedAt) {
				got = append(got, l.EndOffset)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("kept %v, want %v", got, tc.want)
			}
		})
	}
}

// A background /fork can write the fork's own reply above the copied history.
// The copies are skipped wherever they sit; the fork's turn is kept.
func TestHookForkKeepsItsTurnAboveTheCopies(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var (
		mu       sync.Mutex
		exported []string
		bodies   []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/generations:export" {
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Generations []struct {
					ID string `json:"id"`
				} `json:"generations"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			for _, generation := range request.Generations {
				exported = append(exported, generation.ID)
			}
			bodies = append(bodies, body...)
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full")

	const fork = "fork-session"
	path := filepath.Join(t.TempDir(), fork+".jsonl")
	runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: "fork"})
	now := time.Now().UTC()
	text := strings.Join([]string{
		buildHookUserJSONLAt(fork, "say done", now.Add(50*time.Millisecond).Format(time.RFC3339Nano)),
		buildHookAssistantJSONLAt(fork, "req_3", "end_turn", "done", 9, now.Add(400*time.Millisecond).Format(time.RFC3339Nano)),
		buildHookUserJSONLAt(fork, "list the files", "2025-06-01T12:00:00Z"),
		buildHookAssistantJSONLAt(fork, "req_1", "end_turn", "two files", 5, "2025-06-01T12:00:01Z"),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
	runHookForTest(t, hookInput{HookEventName: "SessionEnd", SessionID: fork, TranscriptPath: path})
	mu.Lock()
	defer mu.Unlock()
	if len(exported) != 1 {
		t.Errorf("exported %d generations, want the fork's own reply:\n%s", len(exported), logs)
	}
	// The fork's prompt is its own line too, and goes out with its reply.
	if !bytes.Contains(bodies, []byte("say done")) || bytes.Contains(bodies, []byte("list the files")) {
		t.Errorf("export carries the wrong prompt: %s", bodies)
	}
}

// A fork's start time is set once. Should a second SessionStart with source
// "fork" reach the same session before its first turn is exported, moving the
// start time later would drop that turn as a copy.
func TestHookForkStartIsSetOnce(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	var (
		mu       sync.Mutex
		exported []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/generations:export" {
			body, _ := io.ReadAll(r.Body)
			var request struct {
				Generations []struct {
					ID string `json:"id"`
				} `json:"generations"`
			}
			_ = json.Unmarshal(body, &request)
			mu.Lock()
			for _, generation := range request.Generations {
				exported = append(exported, generation.ID)
			}
			mu.Unlock()
			r.Body = io.NopCloser(bytes.NewReader(body))
			acceptGenerationExport(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer server.Close()
	setHookExportEnv(t, server.URL)

	const fork = "fork-session"
	path := filepath.Join(t.TempDir(), fork+".jsonl")
	runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: "fork"})
	now := time.Now().UTC()
	text := strings.Join([]string{
		buildHookUserJSONLAt(fork, "list the files", "2025-06-01T12:00:00Z"),
		buildHookAssistantJSONLAt(fork, "req_1", "end_turn", "two files", 5, "2025-06-01T12:00:01Z"),
		buildHookUserJSONLAt(fork, "say done", now.Add(50*time.Millisecond).Format(time.RFC3339Nano)),
		buildHookAssistantJSONLAt(fork, "req_2", "end_turn", "done", 9, now.Add(100*time.Millisecond).Format(time.RFC3339Nano)),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	runHookForTest(t, hookInput{HookEventName: "SessionStart", SessionID: fork, TranscriptPath: path, Source: "fork"})
	logs := runHookForTest(t, hookInput{HookEventName: "Stop", SessionID: fork, TranscriptPath: path})
	mu.Lock()
	defer mu.Unlock()
	if len(exported) != 1 {
		t.Errorf("exported %d generations, want the fork's own turn:\n%s", len(exported), logs)
	}
}
