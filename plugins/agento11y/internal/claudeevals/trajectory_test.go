package claudeevals

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func traceFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/trace-with.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fixtureTrajectory(t *testing.T) *Trajectory {
	t.Helper()
	a := attempt{StartedAt: "2026-09-12T03:36:22Z"}
	trajectory, err := parseTrajectory(bytes.NewReader(traceFixture(t)), a, "Rename getUser to fetchUser", "gen-eval-test", "rename [with]")
	if err != nil {
		t.Fatal(err)
	}
	g := &trajectory.Generation
	g.TraceID, g.SpanID = traceID(g.ID).String(), spanID(g.ID).String()
	g.Tags = map[string]string{"experiment_id": "experiment-test", "trial_id": "trial-test", "test_case_id": "case-test"}
	return trajectory
}

func TestTrajectoryUsesFinalUsageAndPreservesConversation(t *testing.T) {
	trajectory := fixtureTrajectory(t)
	g := trajectory.Generation
	if g.Usage.InputTokens != 28903 || g.Usage.OutputTokens != 103 || g.Usage.TotalTokens != 29006 {
		t.Fatalf("incorrect aggregate usage: %+v", g.Usage)
	}
	if g.Usage.CacheReadInputTokens != 22010 || g.Usage.CacheWriteInputTokens != 6887 || g.Usage.InputSemantics != agento11y.TokenInputSemanticsInclusive {
		t.Fatal("cache semantics lost")
	}
	if g.OperationName != "invoke_agent" || len(g.Output) != 4 || len(trajectory.Activities) != 3 {
		t.Fatalf("missing activity/messages: %d/%d", len(trajectory.Activities), len(g.Output))
	}
	if trajectory.Activities[1].Kind != "execute_tool" || trajectory.Activities[1].Name != "Skill" || len(trajectory.Activities[1].Result) == 0 {
		t.Fatal("missing Skill execution/result")
	}
	if g.Output[1].Role != agento11y.RoleTool || !strings.Contains(g.Output[3].Parts[0].Text, "refactor:") {
		t.Fatal("conversation ordering/content lost")
	}
	// A replayed result is cumulative, not a second invocation's token bill.
	data := traceFixture(t)
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	data = append(data, append(lines[len(lines)-1], '\n')...)
	duplicate, err := parseTrajectory(bytes.NewReader(data), attempt{StartedAt: g.StartedAt.Format("2006-01-02T15:04:05Z07:00")}, "prompt", "id", "title")
	if err != nil || duplicate.Generation.Usage.TotalTokens != g.Usage.TotalTokens {
		t.Fatal("counted cumulative usage twice", err)
	}
	emptyThinking := `{"type":"assistant","timestamp":"2026-09-12T03:36:22Z","message":{"id":"empty","model":"claude-sonnet-4-6","content":[{"type":"thinking","thinking":""}]}}` + "\n"
	withEmpty, err := parseTrajectory(bytes.NewReader(append([]byte(emptyThinking), traceFixture(t)...)), attempt{StartedAt: "2026-09-12T03:36:22Z"}, "prompt", "id", "title")
	if err != nil {
		t.Fatal(err)
	}
	if err := agento11y.ValidateGeneration(withEmpty.Generation); err != nil {
		t.Fatalf("empty thinking block broke export: %v", err)
	}
	data = bytes.Join(lines[:len(lines)-1], []byte("\n"))
	if _, err := parseTrajectory(bytes.NewReader(data), attempt{StartedAt: "2026-09-12T03:36:22Z"}, "prompt", "id", "title"); err == nil {
		t.Fatal("guessed final usage from partial assistant counters")
	}
}

func TestRecordedBaselineTrajectory(t *testing.T) {
	data, err := os.ReadFile("testdata/trace-without.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	trajectory, err := parseTrajectory(bytes.NewReader(data), attempt{StartedAt: "2026-09-12T03:36:22Z"}, "prompt", "baseline", "without")
	if err != nil {
		t.Fatal(err)
	}
	if err := agento11y.ValidateGeneration(trajectory.Generation); err != nil {
		t.Fatal(err)
	}
	for _, activity := range trajectory.Activities {
		if activity.Kind != "chat" {
			t.Fatal("invented a tool execution in the baseline")
		}
	}
}

func TestTrajectoryRootAndMetadataOnly(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "sandbox", "out", "trace.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, traceFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	var doc document
	if err := json.Unmarshal(fixture(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc.Suite.Ablation = "none"
	delete(doc.Cases[0].Arms, "without")
	doc.Cases[0].Arms["with"][0].TracePath = path
	data, _ := json.Marshal(doc)
	plan, err := Parse(bytes.NewReader(data), Options{TraceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	trial := plan.Runs[0].Trials[0]
	if trial.Create.ConversationID == "" || trial.Create.TraceID == "" || trial.Scores[0].GenerationID == "" || *trial.Update.OutputTokens != 103 {
		t.Fatal("trial not bound to real trajectory")
	}
	encoded, _ := json.Marshal(plan)
	if bytes.Contains(encoded, []byte("getUser")) || bytes.Contains(encoded, []byte("Base directory")) {
		t.Fatal("trajectory leaked content without consent")
	}
	if _, err := readTrajectory(t.TempDir(), path, attempt{}, "", "", ""); err == nil {
		t.Fatal("read outside authorized root")
	}
	outside := filepath.Join(t.TempDir(), "trace.jsonl")
	if err := os.WriteFile(outside, traceFixture(t), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "escape", "out", "trace.jsonl")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, err := readTrajectory(root, link, attempt{}, "", "", ""); err == nil {
		t.Fatal("symlink escaped authorized root")
	}
}

func TestTrajectoryExportsLinkedGenerationsAndOTLP(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	for _, key := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_RESOURCE_ATTRIBUTES", "OTEL_SERVICE_NAME"} {
		t.Setenv(key, "")
	}
	var mu sync.Mutex
	var spans []*tracepb.Span
	var generations []struct {
		ID      string `json:"id"`
		TraceID string `json:"trace_id"`
		SpanID  string `json:"span_id"`
		Usage   struct {
			TotalTokens string `json:"total_tokens"`
		} `json:"usage"`
		Input  []agento11y.Message `json:"input"`
		Output []agento11y.Message `json:"output"`
	}
	failTrace := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader := io.Reader(r.Body)
		if r.Header.Get("Content-Encoding") == "gzip" {
			gz, err := gzip.NewReader(r.Body)
			if err != nil {
				t.Error(err)
				return
			}
			defer gz.Close()
			reader = gz
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/generations:export":
			var req struct {
				Generations json.RawMessage `json:"generations"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Error(err)
				return
			}
			generations = nil
			if err := json.Unmarshal(req.Generations, &generations); err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{map[string]any{"generation_id": generations[0].ID, "accepted": true}}})
		case "/v1/traces":
			if failTrace {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			var req coltrace.ExportTraceServiceRequest
			if err := proto.Unmarshal(body, &req); err != nil {
				t.Error(err)
				return
			}
			for _, resource := range req.ResourceSpans {
				for _, scope := range resource.ScopeSpans {
					spans = append(spans, scope.Spans...)
				}
			}
		case "/v1/metrics":
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "test")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "test-only")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
	telemetry, err := NewTelemetry(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = telemetry.Shutdown(context.Background()) }()
	trajectory := fixtureTrajectory(t)
	if err := telemetry.export(context.Background(), trajectory, true); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gotGenerations := append(generations[:0:0], generations...)
	gotSpans := append([]*tracepb.Span(nil), spans...)
	spans = nil
	mu.Unlock()
	if len(gotGenerations) != 1 || len(gotSpans) != 4 {
		t.Fatalf("got %d generations/%d spans", len(gotGenerations), len(gotSpans))
	}
	g := gotGenerations[0]
	if g.TraceID != trajectory.Generation.TraceID || g.SpanID != trajectory.Generation.SpanID || g.Usage.TotalTokens != "29006" || len(g.Output) != 4 {
		t.Fatal("export lost generation identity, usage or conversation")
	}
	for _, span := range gotSpans {
		if hex.EncodeToString(span.TraceId) != g.TraceID || span.EndTimeUnixNano < span.StartTimeUnixNano {
			t.Fatal("unrelated trace or inverted span window")
		}
		if hex.EncodeToString(span.SpanId) != g.SpanID && hex.EncodeToString(span.ParentSpanId) != g.SpanID {
			t.Fatal("child span not linked to invocation")
		}
	}
	if err := telemetry.export(context.Background(), trajectory, false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	retried := generations[0]
	failTrace = true
	mu.Unlock()
	if retried.TraceID != g.TraceID || retried.SpanID != g.SpanID {
		t.Fatal("retry changed generation's trace identity")
	}
	redacted, _ := json.Marshal(retried)
	for _, content := range []string{"getUser", "fetchUser", "refactor:", "Base directory"} {
		if bytes.Contains(redacted, []byte(content)) {
			t.Fatal("metadata-only capture exported message content")
		}
	}
	if err := telemetry.export(context.Background(), trajectory, true); err == nil {
		t.Fatal("trace delivery failure was reported as success")
	}
}
