package experiments

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	agento11y "github.com/grafana/agento11y/go/agento11y"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func boolPointer(value bool) *bool { return &value }

func TestPortableSuiteYAMLAliasesAndRoundTrip(t *testing.T) {
	suite, err := ParseSuite([]byte(`
id: smoke
name: Smoke
version: v2
test_cases:
  - test_case_id: scalar
    input: hello
    expected: world
    weight: 2.5
    metadata:
      owner: eval
`))
	if err != nil {
		t.Fatal(err)
	}
	if suite.SuiteID != "smoke" || len(suite.TestCases) != 1 || suite.TestCases[0].Weight != 2.5 {
		t.Fatalf("unexpected suite: %#v", suite)
	}
	data, err := MarshalSuite(*suite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "suite_id: smoke") || !strings.Contains(string(data), "cases:") ||
		!strings.Contains(string(data), "id: scalar") {
		t.Fatalf("unexpected YAML:\n%s", data)
	}
	roundTrip, err := ParseSuite(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.TestCases[0].Input != "hello" || roundTrip.TestCases[0].Expected != "world" {
		t.Fatalf("scalar values were not preserved: %#v", roundTrip.TestCases[0])
	}
}

func TestLLMJudgeSelectsCompleteTopLevelObject(t *testing.T) {
	judge, err := NewLLMJudge(LLMJudgeOptions{
		EvaluatorID: "judge", ModelName: "grader", PassThreshold: 0.8,
		Invoke: func(context.Context, string) (JudgeResponse, error) {
			return JudgeResponse{Text: `rubric {"rubric":{"score":0.1}} final {"score": 1.4, "passed": false, "explanation":"explicit"}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := judge.EvaluateOutput(context.Background(), EvaluationInput{Input: "q", Output: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Value != float64(1) || result.Passed || result.Explanation != "explicit" {
		t.Fatalf("unexpected judge result: %#v", result)
	}
}

func TestRegexJudgeOptions(t *testing.T) {
	judge, err := NewRegexJudge(RegexJudgeOptions{
		EvaluatorID: "regex", Pattern: `\d+`, FullMatch: true, Negate: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := judge.EvaluateOutput(context.Background(), EvaluationInput{Output: "abc1"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Passed {
		t.Fatalf("expected negated full-match to pass: %#v", result)
	}
}

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   map[string]any
}

func TestExperimentLifecycleContractAndStableOccurrences(t *testing.T) {
	var mu sync.Mutex
	var requests []capturedRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		requests = append(requests, capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/experiment-runs:upsert":
			_, _ = w.Write([]byte(`{"experiment_id":"run-1","name":"run","status":"running"}`))
		case strings.HasSuffix(r.URL.Path, ":finalize"):
			_, _ = w.Write([]byte(`{"experiment_id":"run-1","name":"run","status":"completed"}`))
		case r.URL.Path == "/api/v1/scores:export":
			_, _ = w.Write([]byte(`{"accepted":2,"results":[{"score_id":"one","accepted":true},{"score_id":"two","accepted":true}]}`))
		default:
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run-1","test_case_id":"case","attempt":1,"status":"running"}`))
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		Endpoint: server.URL, TenantID: "123", IngestToken: "token",
		UseExperimentalOTel: boolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()

	planned := 7
	experiment, err := NewExperiment(client, ExperimentOptions{
		ExperimentID: "run-1", Name: "run", PlannedTrialCount: &planned,
		Suite:     &TestSuite{SuiteID: "suite", Version: "v3", TestCases: []TestCase{{TestCaseID: "case"}}},
		Candidate: &Candidate{ModelName: "model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := experiment.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	trial, err := experiment.NewTrialByCaseID("case")
	if err != nil {
		t.Fatal(err)
	}
	if err := trial.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := trial.CheckScore("verifier", true, ScoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := trial.CheckScore("verifier", true, ScoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first.ScoreID == second.ScoreID {
		t.Fatalf("repeated verifier score IDs must be occurrence-aware: %q", first.ScoreID)
	}
	if _, err := trial.FinalScore(true, ScoreOptions{}); err != nil {
		t.Fatal(err)
	}
	// The test response only accounts for two records; flush the two verifier
	// scores separately, then leave the final score to close.
	trial.mu.Lock()
	final := trial.buffer[len(trial.buffer)-1]
	trial.buffer = trial.buffer[:2]
	trial.mu.Unlock()
	if _, err := trial.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	trial.mu.Lock()
	trial.buffer = append(trial.buffer, final)
	trial.mu.Unlock()
	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := experiment.NewTrialByCaseID("case"); err == nil {
		t.Fatal("expected duplicate case/attempt claim to fail")
	}
	if err := experiment.Finalize(context.Background(), ExperimentStatusCompleted); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) == 0 {
		t.Fatal("no requests captured")
	}
	upsert := requests[0]
	if upsert.Body["planned_trial_count"] != float64(7) || upsert.Body["suite_id"] != "suite" {
		t.Fatalf("missing exact run fields: %#v", upsert.Body)
	}
	if upsert.Header.Get("X-Sigil-Ingest-Actor") != defaultIngestActor ||
		!strings.HasPrefix(upsert.Header.Get("Authorization"), "Basic ") {
		t.Fatalf("unexpected ingest headers: %#v", upsert.Header)
	}
	for _, request := range requests {
		if strings.HasSuffix(request.Path, ":finalize") {
			if _, exists := request.Body["score_count"]; exists {
				t.Fatalf("normal finalization must omit score_count: %#v", request.Body)
			}
		}
	}
}

func TestTrialTerminalUpdateFailureIsRetryable(t *testing.T) {
	patches := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch {
			patches++
			if patches == 1 {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"temporary terminal conflict"}`))
				return
			}
		}
		_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"running"}`))
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{Endpoint: server.URL, IngestToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()
	trial, err := NewTrial(client, TrialRef{ExperimentID: "run", TestCaseID: "case"})
	if err != nil {
		t.Fatal(err)
	}
	if err := trial.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := trial.FinalScore(true, ScoreOptions{}); err != nil {
		t.Fatal(err)
	}
	// No score route is configured, so clear it: this test isolates retryable
	// terminal PATCH behavior.
	trial.mu.Lock()
	trial.buffer = nil
	trial.mu.Unlock()
	if err := trial.Close(context.Background(), nil); err == nil {
		t.Fatal("expected first close to fail")
	}
	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatalf("second close should retry terminal update: %v", err)
	}
}

func TestClientRedactsScoresAndTextArtifactsByDefault(t *testing.T) {
	const secret = "glc_abcdefghijklmnopqrstuvwxyz123456"
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/scores:export" {
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"score","accepted":true}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"artifact_id":"artifact","name":"log","kind":"text"}`))
	}))
	defer server.Close()
	client, err := NewClient(ClientOptions{Endpoint: server.URL, IngestToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()
	passed := true
	_, err = client.ExportScores(context.Background(), []ScoreItem{{
		ScoreID: "score", TrialID: "trial", EvaluatorID: "eval",
		EvaluatorVersion: "1", ScoreKey: "final", Value: StringScoreValue(secret),
		Passed: &passed, Explanation: secret, Metadata: map[string]any{"token": secret},
	}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.UploadArtifact(context.Background(), "run", "trial", agento11y.TrialArtifactUpload{
		Name: "log", Kind: "text", MIME: "text/plain", Content: []byte(secret),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range bodies {
		if strings.Contains(body, secret) {
			t.Fatalf("secret leaked in request body: %s", body)
		}
	}
}

func TestExperimentOTelIsOptInAndRedactsEventExplanation(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	defer func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
	}()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/scores:export":
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"score","accepted":true}]}`))
		default:
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"running"}`))
		}
	}))
	defer server.Close()

	disabled := false
	client, err := NewClient(ClientOptions{
		Endpoint: server.URL, IngestToken: "token", UseExperimentalOTel: &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	trial, _ := NewTrial(client, TrialRef{ExperimentID: "run", TestCaseID: "disabled"})
	if err := trial.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := trial.FinalScore(true, ScoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(recorder.Ended()) != 0 {
		t.Fatal("experimental OTel must be disabled by default/option")
	}
	_ = client.Shutdown(context.Background())

	enabled := true
	client, err = NewClient(ClientOptions{
		Endpoint: server.URL, IngestToken: "token", UseExperimentalOTel: &enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()
	trial, _ = NewTrial(client, TrialRef{ExperimentID: "run", TestCaseID: "enabled"})
	if err := trial.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	const secret = "glc_abcdefghijklmnopqrstuvwxyz123456"
	if _, err := trial.FinalScore(true, ScoreOptions{Explanation: secret}); err != nil {
		t.Fatal(err)
	}
	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || len(spans[0].Events()) != 1 ||
		spans[0].Events()[0].Name != "gen_ai.evaluation.result" {
		t.Fatalf("unexpected spans/events: %#v", spans)
	}
	for _, attr := range spans[0].Events()[0].Attributes {
		if strings.Contains(attr.Value.AsString(), secret) {
			t.Fatalf("secret leaked in OTel event: %#v", attr)
		}
	}
}
