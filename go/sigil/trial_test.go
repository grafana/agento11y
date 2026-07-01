package sigil

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTrialLifecycleCreatesTypedTrialAndFinalScore(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	suite := &TestSuite{SuiteID: "smoke", Name: "Smoke", Version: "1.2.0", TestCases: []TestCase{{TestCaseID: "add", Name: "Addition", Input: "2+2"}}}
	exp := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-1", Name: "smoke run", Suite: suite})
	trial := exp.Trial(suite.TestCases[0], WithTrialMetadata(map[string]any{
		"task_category": "math",
		"task_id":       "caller-task",
		"trial_id":      "caller-trial",
		"attempt":       99,
	}))
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	verifier := Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}
	trial.FinalScore(NumberScoreValue(1), ScoreOptions{
		Passed:      BoolPtr(true),
		Explanation: "matched",
		Evaluator:   &verifier,
		Metadata: map[string]any{
			"trial_id": "score-option-trial",
			"attempt":  42,
		},
	})
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}

	createReq := recorder.request(0)
	if createReq.Method != http.MethodPost || createReq.Path != "/api/v1/experiment-runs/run-1/trials" {
		t.Fatalf("unexpected trial create: %#v", createReq)
	}
	createMetadata := createReq.Payload["metadata"].(map[string]any)
	if createMetadata["task_category"] != "math" || createMetadata["task_id"] != "caller-task" || createMetadata["test_case_name"] != "Addition" {
		t.Fatalf("trial metadata omitted from upsert: %#v", createMetadata)
	}
	scoreReq := recorder.request(1)
	score := scoreReq.Payload["scores"].([]any)[0].(map[string]any)
	if score["experiment_id"] != "run-1" || score["test_case_id"] != "add" || score["trial_id"] == "" {
		t.Fatalf("unexpected score: %#v", score)
	}
	scoreMetadata := score["metadata"].(map[string]any)
	if scoreMetadata["task_id"] != "add" || scoreMetadata["trial_id"] != trial.trialID || scoreMetadata["attempt"] != float64(1) {
		t.Fatalf("score metadata must keep SDK identifiers authoritative: %#v", scoreMetadata)
	}
	if scoreMetadata["task_category"] != "math" {
		t.Fatalf("score metadata omitted caller metadata: %#v", scoreMetadata)
	}
	if _, ok := score["generation_id"]; ok {
		t.Fatalf("score without RecordIO/BindGeneration must not send generation_id: %#v", score)
	}
	updateReq := recorder.request(2)
	if updateReq.Method != http.MethodPatch || updateReq.Payload["status"] != "completed" {
		t.Fatalf("unexpected trial update: %#v", updateReq)
	}
}

func TestTrialEndCreatesTrialWhenStartWasSkipped(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-no-start"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-no-start"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-no-start", TestCaseID: "case-no-start"})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial without start: %v", err)
	}
	if recorder.requestCount() != 3 {
		t.Fatalf("expected trial create, score export, and update requests, got %d", recorder.requestCount())
	}
	if req := recorder.request(0); req.Method != http.MethodPost || req.Path != "/api/v1/experiment-runs/run-no-start/trials" {
		t.Fatalf("unexpected trial create request: %#v", req)
	}
	if req := recorder.request(2); req.Method != http.MethodPatch || req.Path != "/api/v1/experiment-runs/run-no-start/trials/"+trial.trialID {
		t.Fatalf("unexpected trial update request: %#v", req)
	}
}

func TestRecordIOTokensAreIncludedInTrialUpdate(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-usage"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-usage"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	inputTokens := 11
	outputTokens := 7
	trial := NewTrial(client, TrialRef{RunID: "run-usage", TestCaseID: "case-usage"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.RecordIO(RecordIOOptions{Input: "question", Output: "answer", InputTokens: &inputTokens, OutputTokens: &outputTokens})
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}
	updateReq := recorder.request(2)
	if updateReq.Payload["input_tokens"] != float64(inputTokens) || updateReq.Payload["output_tokens"] != float64(outputTokens) {
		t.Fatalf("expected token usage on trial update, got %#v", updateReq.Payload)
	}
}

func TestTrialEndUsesCleanupContextAfterCallerContextCanceled(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-canceled"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-canceled"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-canceled", TestCaseID: "case-canceled"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := trial.End(ctx, nil); err != nil {
		t.Fatalf("end trial with canceled context: %v", err)
	}
	if recorder.requestCount() != 3 {
		t.Fatalf("expected create, score export, and update requests, got %d", recorder.requestCount())
	}
	if req := recorder.request(1); req.Method != http.MethodPost || req.Path != "/api/v1/scores:export" {
		t.Fatalf("unexpected score export request: %#v", req)
	}
	if req := recorder.request(2); req.Method != http.MethodPatch || req.Path != "/api/v1/experiment-runs/run-canceled/trials/"+trial.trialID {
		t.Fatalf("unexpected trial update request: %#v", req)
	}
}

func TestTrialEndFinalizesFailedWhenScoreFlushFails(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-flush-fails"})
	recorder.push(http.StatusInternalServerError, map[string]any{"error": "score export failed"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-flush-fails"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	client.config.GenerationExport.MaxRetries = 0
	trial := NewTrial(client, TrialRef{RunID: "run-flush-fails", TestCaseID: "case-flush-fails"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})

	if err := trial.End(context.Background(), nil); err == nil {
		t.Fatal("expected score flush error")
	}
	if recorder.requestCount() != 3 {
		t.Fatalf("expected trial create, score export, and failed update, got %d requests", recorder.requestCount())
	}
	if req := recorder.request(1); req.Method != http.MethodPost || req.Path != "/api/v1/scores:export" {
		t.Fatalf("unexpected score export request: %#v", req)
	}
	updateReq := recorder.request(2)
	if updateReq.Method != http.MethodPatch || updateReq.Path != "/api/v1/experiment-runs/run-flush-fails/trials/"+trial.trialID {
		t.Fatalf("unexpected trial update request: %#v", updateReq)
	}
	if updateReq.Payload["status"] != "failed" || updateReq.Payload["error"] == "" {
		t.Fatalf("expected failed trial update with error, got %#v", updateReq.Payload)
	}
}
