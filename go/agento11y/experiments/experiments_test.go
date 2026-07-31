package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agento11y "github.com/grafana/agento11y/go/agento11y"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
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
  - test_case_id: disabled
    input: skip
    weight: 0
  - test_case_id: default
    input: run
`))
	if err != nil {
		t.Fatal(err)
	}
	if suite.SuiteID != "smoke" || len(suite.TestCases) != 3 ||
		suite.TestCases[0].EffectiveWeight() != 2.5 ||
		suite.TestCases[1].EffectiveWeight() != 0 ||
		suite.TestCases[2].EffectiveWeight() != 1 {
		t.Fatalf("unexpected suite: %#v", suite)
	}
	data, err := MarshalSuite(*suite)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "suite_id: smoke") || !strings.Contains(string(data), "cases:") ||
		!strings.Contains(string(data), "id: scalar") || !strings.Contains(string(data), "weight: 0") {
		t.Fatalf("unexpected YAML:\n%s", data)
	}
	roundTrip, err := ParseSuite(data)
	if err != nil {
		t.Fatal(err)
	}
	if roundTrip.TestCases[0].Input != "hello" || roundTrip.TestCases[0].Expected != "world" ||
		roundTrip.TestCases[1].EffectiveWeight() != 0 ||
		roundTrip.TestCases[2].EffectiveWeight() != 1 {
		t.Fatalf("portable values were not preserved: %#v", roundTrip.TestCases)
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

func TestWithTrialEnterFailureReleasesClaimForRetry(t *testing.T) {
	var mu sync.Mutex
	trialUpserts := 0
	var trialIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/trials"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			trialUpserts++
			trialIDs = append(trialIDs, body["trial_id"].(string))
			attempt := trialUpserts
			mu.Unlock()
			if attempt == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"trial upsert rejected"}`))
				return
			}
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"running"}`))
		case r.URL.Path == "/api/v1/scores:export":
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"score","accepted":true}]}`))
		case r.Method == http.MethodPatch:
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"completed"}`))
		case strings.HasSuffix(r.URL.Path, ":finalize"):
			_, _ = w.Write([]byte(`{"experiment_id":"run","name":"run","status":"completed"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientOptions{
		Endpoint: server.URL, IngestToken: "token",
		UseExperimentalOTel: boolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()
	experiment, err := NewExperiment(client, ExperimentOptions{ExperimentID: "run", Name: "run"})
	if err != nil {
		t.Fatal(err)
	}

	callbackCalls := 0
	err = experiment.WithTrialByCaseID(context.Background(), "case", func(context.Context, *Trial) error {
		callbackCalls++
		return nil
	})
	if err == nil {
		t.Fatal("expected the first trial upsert to fail")
	}
	if callbackCalls != 0 {
		t.Fatal("callback must not run when trial entry fails")
	}
	experiment.mu.Lock()
	open, claimed := len(experiment.open), len(experiment.claimed)
	experiment.mu.Unlock()
	if open != 0 || claimed != 0 {
		t.Fatalf("failed entry left trial registered: open=%d claimed=%d", open, claimed)
	}

	err = experiment.WithTrialByCaseID(context.Background(), "case", func(_ context.Context, trial *Trial) error {
		callbackCalls++
		_, scoreErr := trial.FinalScore(true, ScoreOptions{})
		return scoreErr
	})
	if err != nil {
		t.Fatalf("retrying the same case and attempt failed: %v", err)
	}
	if err := experiment.Finalize(context.Background(), ExperimentStatusCompleted); err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	defer mu.Unlock()
	if trialUpserts != 2 {
		t.Fatalf("expected only the failed entry and its retry, got %d trial upserts", trialUpserts)
	}
	if len(trialIDs) != 2 || trialIDs[0] != trialIDs[1] {
		t.Fatalf("retry must preserve the stable trial ID: %#v", trialIDs)
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

func TestTrialCloseRetriesBufferedScoresBeforeFinalizing(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	defer func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
	}()

	scoreExports, patches := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/scores:export":
			scoreExports++
			if scoreExports == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"temporary score export failure"}`))
				return
			}
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"score","accepted":true}]}`))
		case r.Method == http.MethodPatch:
			patches++
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"completed"}`))
		default:
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"running"}`))
		}
	}))
	defer server.Close()

	enabled := true
	client, err := NewClient(ClientOptions{
		Endpoint: server.URL, IngestToken: "token", UseExperimentalOTel: &enabled,
	})
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
	if _, err := trial.FinalScore(0.75, ScoreOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := trial.Close(context.Background(), nil); err == nil {
		t.Fatal("expected the first close to fail while exporting scores")
	}
	trial.mu.Lock()
	closed, buffered := trial.closed, len(trial.buffer)
	trial.mu.Unlock()
	if closed || buffered != 1 {
		t.Fatalf("failed score export must keep trial retryable: closed=%t buffered=%d", closed, buffered)
	}
	if trial.Status != TrialStatusCompleted {
		t.Fatalf("expected exported completed status, got %q", trial.Status)
	}
	if patches != 0 {
		t.Fatalf("terminal update ran before scores were exported: patches=%d", patches)
	}
	if len(recorder.Ended()) != 0 {
		t.Fatal("retryable close failure must keep the trial span open")
	}

	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatalf("second close should retry buffered scores: %v", err)
	}
	trial.mu.Lock()
	closed, buffered = trial.closed, len(trial.buffer)
	trial.mu.Unlock()
	if !closed || buffered != 0 || scoreExports != 2 || patches != 1 {
		t.Fatalf(
			"unexpected retry result: closed=%t buffered=%d exports=%d patches=%d",
			closed, buffered, scoreExports, patches,
		)
	}
	spans := recorder.Ended()
	if len(spans) != 1 || spans[0].Status().Code != codes.Ok {
		t.Fatalf("successful retry must end one successful trial span: %#v", spans)
	}
}

func TestTrialFlushCreatesTrialBeforePublishingScores(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/trials"):
			requests = append(requests, "trial")
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run","test_case_id":"case","attempt":1,"status":"running"}`))
		case r.URL.Path == "/api/v1/scores:export":
			requests = append(requests, "scores")
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"score","accepted":true}]}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
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
	if _, err := trial.FinalScore(true, ScoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := trial.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || requests[0] != "trial" || requests[1] != "scores" {
		t.Fatalf("strict ingest requires trial upsert before score export: %#v", requests)
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

	errored, err := NewTrial(client, TrialRef{ExperimentID: "run", TestCaseID: "errored"})
	if err != nil {
		t.Fatal(err)
	}
	if err := errored.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := errored.Close(context.Background(), errors.New("callback failed")); err != nil {
		t.Fatal(err)
	}
	spans = recorder.Ended()
	if len(spans) != 2 || spans[1].Status().Code != codes.Error {
		t.Fatalf("errored trial must end its span with error status: %#v", spans)
	}
}

// trialEvaluationServer serves the ingest routes a cloud-evaluated trial uses.
// evaluationStatuses is consumed in order by the trigger and each status read;
// the last entry repeats, so a single "queued" entry keeps polling forever.
type trialEvaluationServer struct {
	mu                sync.Mutex
	requests          []capturedRequest
	statuses          []string
	evaluationError   string
	evaluationID      string
	onStatusRequest   func(count int)
	server            *httptest.Server
	triggerStatusCode int
	triggerBody       string
}

func newTrialEvaluationServer(t *testing.T, statuses ...string) *trialEvaluationServer {
	t.Helper()
	s := &trialEvaluationServer{statuses: statuses, evaluationID: "teval-1"}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.requests = append(s.requests, capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		statusReads := 0
		for _, request := range s.requests {
			if strings.Contains(request.Path, "/evaluations/") {
				statusReads++
			}
		}
		hook := s.onStatusRequest
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/experiment-runs:upsert":
			_, _ = w.Write([]byte(`{"experiment_id":"run-1","name":"run","status":"running"}`))
		case strings.HasSuffix(r.URL.Path, ":finalize"):
			_, _ = w.Write([]byte(`{"experiment_id":"run-1","name":"run","status":"completed"}`))
		case r.URL.Path == "/api/v1/scores:export":
			_, _ = w.Write([]byte(`{"accepted":1,"results":[{"score_id":"one","accepted":true}]}`))
		case r.URL.Path == "/api/v1/generations:export":
			// The exporter checks for one result per exported generation.
			generations, _ := body["generations"].([]any)
			results := make([]map[string]any, 0, len(generations))
			for _, generation := range generations {
				fields, _ := generation.(map[string]any)
				id, _ := fields["id"].(string)
				results = append(results, map[string]any{"generation_id": id, "accepted": true})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"results": results})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":evaluate"):
			if s.triggerStatusCode != 0 {
				w.WriteHeader(s.triggerStatusCode)
				_, _ = w.Write([]byte(s.triggerBody))
				return
			}
			s.writeEvaluation(w, s.nextStatus())
		case strings.Contains(r.URL.Path, "/evaluations/"):
			if hook != nil {
				hook(statusReads)
			}
			s.writeEvaluation(w, s.nextStatus())
		default:
			_, _ = w.Write([]byte(`{"trial_id":"trial","experiment_id":"run-1","test_case_id":"case","attempt":1,"status":"running"}`))
		}
	}))
	t.Cleanup(s.server.Close)
	return s
}

func (s *trialEvaluationServer) nextStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.statuses) == 0 {
		return "queued"
	}
	status := s.statuses[len(s.statuses)-1]
	if len(s.statuses) > 1 {
		status = s.statuses[0]
		s.statuses = s.statuses[1:]
	}
	return status
}

func (s *trialEvaluationServer) writeEvaluation(w http.ResponseWriter, status string) {
	payload := map[string]any{
		"evaluation_id": s.evaluationID, "experiment_id": "run-1", "trial_id": "trial",
		"test_case_id": "case", "conversation_id": "conv-1",
		"evaluator_id": "helpfulness", "evaluator_version": "v3",
		"status": status, "attempts": 0,
	}
	code := http.StatusAccepted
	if status == "success" || status == "failed" {
		code = http.StatusOK
	}
	if status == "failed" {
		payload["error"] = s.evaluationError
	}
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(payload)
}

func (s *trialEvaluationServer) captured() []capturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]capturedRequest(nil), s.requests...)
}

func (s *trialEvaluationServer) statusRequestCount() int {
	count := 0
	for _, request := range s.captured() {
		if strings.Contains(request.Path, "/evaluations/") {
			count++
		}
	}
	return count
}

func (s *trialEvaluationServer) newClient(t *testing.T) *Client {
	t.Helper()
	client, err := NewClient(ClientOptions{
		Endpoint: s.server.URL, TenantID: "123", IngestToken: "token",
		UseExperimentalOTel: boolPointer(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client
}

func TestClientForwardsTrialEvaluationCalls(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	client := server.newClient(t)

	evaluation, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", TriggerTrialEvaluationRequest{
		EvaluatorID: "helpfulness", EvaluatorVersion: "v3",
	})
	if err != nil {
		t.Fatalf("trigger trial evaluation: %v", err)
	}
	if evaluation.Status != TrialEvaluationStatusQueued || evaluation.EvaluationID != "teval-1" {
		t.Fatalf("unexpected evaluation: %#v", evaluation)
	}
	if _, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial-1", "teval-1"); err != nil {
		t.Fatalf("get trial evaluation: %v", err)
	}

	requests := server.captured()
	if len(requests) != 2 {
		t.Fatalf("expected a trigger and a status request, got %#v", requests)
	}
	trigger := requests[0]
	if trigger.Method != http.MethodPost || trigger.Path != "/api/v1/experiment-runs/exp-1/trials/trial-1:evaluate" {
		t.Fatalf("unexpected trigger request %s %s", trigger.Method, trigger.Path)
	}
	if trigger.Body["evaluator_id"] != "helpfulness" || trigger.Body["evaluator_version"] != "v3" {
		t.Fatalf("wrapper must forward evaluator identity unchanged: %#v", trigger.Body)
	}
	status := requests[1]
	if status.Method != http.MethodGet || status.Path != "/api/v1/experiment-runs/exp-1/trials/trial-1/evaluations/teval-1" {
		t.Fatalf("unexpected status request %s %s", status.Method, status.Path)
	}
}

func TestNilClientTrialEvaluationCallsDoNotRequest(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	var client *Client
	if _, err := client.TriggerTrialEvaluation(context.Background(), "exp-1", "trial-1", TriggerTrialEvaluationRequest{
		EvaluatorID: "helpfulness",
	}); !errors.Is(err, agento11y.ErrNilClient) {
		t.Fatalf("expected ErrNilClient, got %v", err)
	}
	if _, err := client.GetTrialEvaluation(context.Background(), "exp-1", "trial-1", "teval-1"); !errors.Is(err, agento11y.ErrNilClient) {
		t.Fatalf("expected ErrNilClient, got %v", err)
	}
	if len(server.captured()) != 0 {
		t.Fatalf("a nil client must not issue requests, got %#v", server.captured())
	}
}

func newCloudEvaluatedTrial(t *testing.T, server *trialEvaluationServer) (*Experiment, *Trial) {
	t.Helper()
	client := server.newClient(t)
	experiment, err := NewExperiment(client, ExperimentOptions{
		ExperimentID: "run-1", Name: "run",
		Suite: &TestSuite{SuiteID: "suite", TestCases: []TestCase{{TestCaseID: "case", Input: "2+2"}}},
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
	return experiment, trial
}

func indexOfRequest(requests []capturedRequest, match func(capturedRequest) bool) int {
	for i, request := range requests {
		if match(request) {
			return i
		}
	}
	return -1
}

func TestTrialEvaluatePersistsConversationAndClosesCompleted(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued", "claimed", "success")
	_, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1").RecordIO(RecordIOOptions{Input: "2+2", Output: "4"})

	evaluation, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		EvaluatorVersion: "v3", PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Status != TrialEvaluationStatusSuccess {
		t.Fatalf("unexpected evaluation: %#v", evaluation)
	}
	if err := trial.Close(context.Background(), nil); err != nil {
		t.Fatalf("close: %v", err)
	}
	if trial.Status != TrialStatusCompleted || trial.Error != "" {
		t.Fatalf("cloud-evaluated trial must close completed: status=%q error=%q", trial.Status, trial.Error)
	}

	requests := server.captured()
	trigger := indexOfRequest(requests, func(r capturedRequest) bool {
		return r.Method == http.MethodPost && strings.HasSuffix(r.Path, ":evaluate")
	})
	if trigger < 0 {
		t.Fatalf("no evaluation trigger request: %#v", requests)
	}
	binding := indexOfRequest(requests, func(r capturedRequest) bool {
		return r.Method == http.MethodPatch && r.Body["conversation_id"] == "conv-1"
	})
	if binding < 0 || binding > trigger {
		t.Fatalf("conversation must be persisted before the trigger: binding=%d trigger=%d %#v", binding, trigger, requests)
	}
	generation := indexOfRequest(requests, func(r capturedRequest) bool {
		return r.Path == "/api/v1/generations:export"
	})
	if generation < 0 || generation > trigger {
		t.Fatalf("anchor generation must be exported before the trigger: generation=%d trigger=%d", generation, trigger)
	}
	if server.statusRequestCount() != 2 {
		t.Fatalf("expected two status reads for queued then claimed, got %d", server.statusRequestCount())
	}
	for _, request := range requests {
		if request.Path == "/api/v1/scores:export" {
			t.Fatalf("cloud evaluation must not export a local score: %#v", request.Body)
		}
	}
	terminal := requests[len(requests)-1]
	if terminal.Method != http.MethodPatch || terminal.Body["status"] != "completed" {
		t.Fatalf("expected a completed terminal update, got %#v", terminal)
	}
	if _, exists := terminal.Body["error"]; exists {
		t.Fatalf("completed trial must carry no error text: %#v", terminal.Body)
	}
}

func TestTrialCallbackErrorAfterCloudEvaluationStillFails(t *testing.T) {
	server := newTrialEvaluationServer(t, "success")
	experiment, err := func() (*Experiment, error) {
		client := server.newClient(t)
		return NewExperiment(client, ExperimentOptions{ExperimentID: "run-1", Name: "run"})
	}()
	if err != nil {
		t.Fatal(err)
	}
	if err := experiment.Enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	callbackErr := errors.New("assertion failed after grading")
	err = experiment.WithTrialByCaseID(context.Background(), "case", func(ctx context.Context, trial *Trial) error {
		trial.BindConversation("conv-1")
		if _, evalErr := trial.Evaluate(ctx, "helpfulness", EvaluateOptions{PollInterval: time.Millisecond}); evalErr != nil {
			return evalErr
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("expected the callback error, got %v", err)
	}
	requests := server.captured()
	terminal := requests[len(requests)-1]
	if terminal.Method != http.MethodPatch || terminal.Body["status"] != "failed" ||
		terminal.Body["error"] != callbackErr.Error() {
		t.Fatalf("callback error must win over cloud evaluation: %#v", terminal)
	}
}

func TestTrialEvaluateSurfacesWorkerFailure(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued", "failed")
	server.evaluationError = "evaluator budget exhausted"
	_, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	_, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{PollInterval: time.Millisecond})
	var failed *agento11y.TrialEvaluationFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("expected *TrialEvaluationFailedError, got %v", err)
	}
	if failed.EvaluationID != "teval-1" || failed.Detail != "evaluator budget exhausted" {
		t.Fatalf("unexpected failure detail: %#v", failed)
	}
	if !errors.Is(err, agento11y.ErrTrialEvaluationFailed) {
		t.Fatalf("expected ErrTrialEvaluationFailed, got %v", err)
	}
	trial.mu.Lock()
	cloudEvaluated := trial.cloudEvaluated
	trial.mu.Unlock()
	if cloudEvaluated {
		t.Fatal("a failed evaluation must not mark the trial cloud-evaluated")
	}
}

func TestTrialEvaluateTimesOutAndBacksOff(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	_, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	start := time.Now()
	_, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		Timeout: 50 * time.Millisecond, PollInterval: time.Millisecond,
	})
	elapsed := time.Since(start)
	var timedOut *agento11y.TrialEvaluationTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("expected *TrialEvaluationTimeoutError, got %v", err)
	}
	if timedOut.EvaluationID != "teval-1" {
		t.Fatalf("timeout must carry the evaluation ID: %#v", timedOut)
	}
	if !errors.Is(err, agento11y.ErrTrialEvaluationTimeout) {
		t.Fatalf("expected ErrTrialEvaluationTimeout, got %v", err)
	}
	// A fixed 1ms interval would issue tens of reads in 50ms.
	if reads := server.statusRequestCount(); reads > 7 {
		t.Fatalf("poll interval must back off, got %d status reads in %s", reads, elapsed)
	}
}

func TestTrialEvaluateReadsStatusAfterTheFinalSleep(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued", "success")
	_, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	// A poll interval at least as long as the timeout clamps the only sleep to the
	// whole budget. The status read still has to happen, or an evaluation that
	// finishes inside that window is reported as a timeout.
	evaluation, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		Timeout: 20 * time.Millisecond, PollInterval: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Status != TrialEvaluationStatusSuccess {
		t.Fatalf("unexpected evaluation: %#v", evaluation)
	}
	if reads := server.statusRequestCount(); reads != 1 {
		t.Fatalf("expected exactly one status read after the clamped sleep, got %d", reads)
	}
}

func TestQueuedEvaluationDropsFinalizeScoreCount(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	experiment, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	// The evaluation stays queued server-side and its score lands after this run
	// finalizes, so a caller-supplied count cannot be asserted.
	if _, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		Timeout: 5 * time.Millisecond, PollInterval: 10 * time.Millisecond,
	}); !errors.Is(err, agento11y.ErrTrialEvaluationTimeout) {
		t.Fatalf("expected ErrTrialEvaluationTimeout, got %v", err)
	}
	count := 0
	if err := experiment.Finalize(context.Background(), ExperimentStatusFailed, FinalizeOptions{
		ScoreCount: &count, Error: "evaluation timed out",
	}); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	requests := server.captured()
	finalize := indexOfRequest(requests, func(r capturedRequest) bool {
		return strings.HasSuffix(r.Path, ":finalize")
	})
	if finalize < 0 {
		t.Fatalf("no finalize request: %#v", requests)
	}
	if _, exists := requests[finalize].Body["score_count"]; exists {
		t.Fatalf("a cloud-evaluated experiment must not assert a score count: %#v", requests[finalize].Body)
	}
}

func TestTrialEvaluateSurfacesTriggerRejection(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	server.triggerStatusCode = http.StatusConflict
	server.triggerBody = `{"error":"experiment is no longer running"}`
	experiment, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	if _, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		PollInterval: time.Millisecond,
	}); !errors.Is(err, agento11y.ErrExperimentConflict) {
		t.Fatalf("expected ErrExperimentConflict, got %v", err)
	}
	if server.statusRequestCount() != 0 {
		t.Fatalf("a rejected trigger must not start polling, got %d status reads", server.statusRequestCount())
	}
	experiment.mu.Lock()
	cloudEvaluated := experiment.cloudEvaluated
	experiment.mu.Unlock()
	if cloudEvaluated {
		t.Fatal("a rejected trigger queues nothing, so the experiment must keep its own score count")
	}
}

func TestTrialEvaluateHonorsContextCancellation(t *testing.T) {
	server := newTrialEvaluationServer(t, "queued")
	_, trial := newCloudEvaluatedTrial(t, server)
	trial.BindConversation("conv-1")

	ctx, cancel := context.WithCancel(context.Background())
	server.mu.Lock()
	// Cancel during the wait. Whether cancellation lands on the in-flight status
	// read or on the next sleep, Evaluate reports the context error.
	server.onStatusRequest = func(count int) {
		if count == 1 {
			cancel()
		}
	}
	server.mu.Unlock()
	defer cancel()

	_, err := trial.Evaluate(ctx, "helpfulness", EvaluateOptions{
		Timeout: 5 * time.Second, PollInterval: time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	var timedOut *agento11y.TrialEvaluationTimeoutError
	var failed *agento11y.TrialEvaluationFailedError
	if errors.As(err, &timedOut) || errors.As(err, &failed) {
		t.Fatalf("cancellation must not be reported as timeout or failure: %v", err)
	}
}

func TestTrialEvaluateRejectsInvalidOptions(t *testing.T) {
	cases := []struct {
		name           string
		bindConversion bool
		evaluatorID    string
		options        EvaluateOptions
	}{
		{name: "no bound conversation", evaluatorID: "helpfulness"},
		{name: "empty evaluator id", bindConversion: true, evaluatorID: "  "},
		{name: "negative timeout", bindConversion: true, evaluatorID: "helpfulness", options: EvaluateOptions{Timeout: -time.Second}},
		{name: "negative poll interval", bindConversion: true, evaluatorID: "helpfulness", options: EvaluateOptions{PollInterval: -time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newTrialEvaluationServer(t, "success")
			client := server.newClient(t)
			trial, err := NewTrial(client, TrialRef{ExperimentID: "run-1", TestCaseID: "case"})
			if err != nil {
				t.Fatal(err)
			}
			if tc.bindConversion {
				trial.BindConversation("conv-1")
			}
			if _, err := trial.Evaluate(context.Background(), tc.evaluatorID, tc.options); !errors.Is(err, agento11y.ErrExperimentValidationFailed) {
				t.Fatalf("expected ErrExperimentValidationFailed, got %v", err)
			}
			if requests := server.captured(); len(requests) != 0 {
				t.Fatalf("invalid options must not issue a request, got %#v", requests)
			}
		})
	}
}

func TestTrialEvaluateDefaultsWaitOptions(t *testing.T) {
	timeout, pollInterval, err := EvaluateOptions{EvaluatorVersion: "v3"}.resolve()
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if timeout != defaultEvaluationTimeout || pollInterval != defaultEvaluationPollInterval {
		t.Fatalf("unset durations must use the defaults, got %s and %s", timeout, pollInterval)
	}
}
