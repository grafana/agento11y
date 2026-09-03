package agento11y

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		Passed:      new(true),
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

func TestTrialScoreIDIncludesGenerationAndEvaluatorVersion(t *testing.T) {
	trial := NewTrial(nil, TrialRef{RunID: "run-score-id", TestCaseID: "case-score-id"})
	evV1 := Evaluator{EvaluatorID: "judge", Version: "v1", Kind: EvaluatorKindCustom}
	evV2 := Evaluator{EvaluatorID: "judge", Version: "v2", Kind: EvaluatorKindCustom}

	first := trial.Score("quality", NumberScoreValue(1), ScoreOptions{GenerationID: "gen-a", Evaluator: &evV1})
	same := trial.Score("quality", NumberScoreValue(1), ScoreOptions{GenerationID: "gen-a", Evaluator: &evV1})
	differentGeneration := trial.Score("quality", NumberScoreValue(1), ScoreOptions{GenerationID: "gen-b", Evaluator: &evV1})
	differentVersion := trial.Score("quality", NumberScoreValue(1), ScoreOptions{GenerationID: "gen-a", Evaluator: &evV2})

	if first.ScoreID != same.ScoreID {
		t.Fatalf("expected same score dimensions to produce stable ID, got %q and %q", first.ScoreID, same.ScoreID)
	}
	if first.ScoreID == differentGeneration.ScoreID {
		t.Fatalf("expected generation ID to affect score ID, got %q", first.ScoreID)
	}
	if first.ScoreID == differentVersion.ScoreID {
		t.Fatalf("expected evaluator version to affect score ID, got %q", first.ScoreID)
	}
}

func TestTrialArtifactWithoutClientReturnsErrNilClient(t *testing.T) {
	tests := []struct {
		name  string
		trial *Trial
	}{
		{name: "nil trial"},
		{name: "nil client", trial: NewTrial(nil, TrialRef{RunID: "run-artifact", TestCaseID: "case-artifact"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.trial.Artifact(context.Background(), ArtifactOptions{Name: "output", Text: "hello"})
			if !errors.Is(err, ErrNilClient) {
				t.Fatalf("expected ErrNilClient, got %v", err)
			}
		})
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

func TestTrialEndRetryRecomputesStatusAfterScoreFlushFailure(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-flush-retry"})
	recorder.push(http.StatusInternalServerError, map[string]any{"error": "score export failed"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-flush-retry"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-flush-retry"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	client.config.GenerationExport.MaxRetries = 0
	trial := NewTrial(client, TrialRef{RunID: "run-flush-retry", TestCaseID: "case-flush-retry"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})

	if err := trial.End(context.Background(), nil); err == nil {
		t.Fatal("expected first end to fail score export")
	}
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("retry end: %v", err)
	}
	if recorder.requestCount() != 5 {
		t.Fatalf("expected create, failed score export, failed update, retried score export, completed update; got %d requests", recorder.requestCount())
	}
	failedUpdate := recorder.request(2)
	if failedUpdate.Payload["status"] != "failed" || failedUpdate.Payload["error"] == "" {
		t.Fatalf("expected failed trial update after first end, got %#v", failedUpdate.Payload)
	}
	completedUpdate := recorder.request(4)
	if completedUpdate.Payload["status"] != "completed" {
		t.Fatalf("expected completed trial update after retry, got %#v", completedUpdate.Payload)
	}
	if _, ok := completedUpdate.Payload["error"]; ok {
		t.Fatalf("expected retry to clear stale flush error, got %#v", completedUpdate.Payload)
	}
}

func TestTrialSucceedWithoutFinalScoreFinalizesFailed(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-succeed-no-score"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-succeed-no-score"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-succeed-no-score", TestCaseID: "case-succeed-no-score"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.Succeed()
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}
	if recorder.requestCount() != 2 {
		t.Fatalf("expected trial create and update requests, got %d", recorder.requestCount())
	}
	updateReq := recorder.request(1)
	if updateReq.Method != http.MethodPatch || updateReq.Path != "/api/v1/experiment-runs/run-succeed-no-score/trials/"+trial.trialID {
		t.Fatalf("unexpected trial update request: %#v", updateReq)
	}
	if updateReq.Payload["error"] != "trial exited without a final score" {
		t.Fatalf("expected missing final score error, got %#v", updateReq.Payload)
	}
}

func trialEvaluationResponse(status string, overrides map[string]any) map[string]any {
	body := map[string]any{
		"evaluation_id":     "teval-1",
		"experiment_id":     "run-cloud",
		"trial_id":          "trial-cloud",
		"test_case_id":      "case-cloud",
		"conversation_id":   "conv-1",
		"evaluator_id":      "helpfulness",
		"evaluator_version": "v3",
		"status":            status,
		"attempts":          float64(0),
	}
	maps.Copy(body, overrides)
	return body
}

func TestTrialEvaluateClosesCompletedAndRunOmitsScoreCount(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-cloud"})})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	recorder.push(http.StatusOK, trialEvaluationResponse("success", nil))
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-cloud", "status": "completed"})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-cloud", Name: "cloud run"})
	if err := run.Enter(context.Background()); err != nil {
		t.Fatalf("enter run: %v", err)
	}
	trial := run.TrialID("case-cloud")
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
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
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}
	if trial.status != TrialStatusCompleted || trial.errorText != "" {
		t.Fatalf("cloud-evaluated trial must end completed: status=%q error=%q", trial.status, trial.errorText)
	}
	if err := run.Finalize(context.Background(), ExperimentStatusCompleted, ""); err != nil {
		t.Fatalf("finalize run: %v", err)
	}

	binding := recorder.request(2)
	if binding.Method != http.MethodPatch || binding.Payload["conversation_id"] != "conv-1" {
		t.Fatalf("expected the conversation to be persisted before the trigger, got %#v", binding)
	}
	trigger := recorder.request(3)
	if trigger.Method != http.MethodPost || trigger.Path != "/api/v1/experiment-runs/run-cloud/trials/"+trial.trialID+":evaluate" {
		t.Fatalf("unexpected trigger request %s %s", trigger.Method, trigger.Path)
	}
	if trigger.Payload["evaluator_id"] != "helpfulness" || trigger.Payload["evaluator_version"] != "v3" {
		t.Fatalf("unexpected trigger body: %#v", trigger.Payload)
	}
	status := recorder.request(4)
	if status.Method != http.MethodGet || status.Path != "/api/v1/experiment-runs/run-cloud/trials/"+trial.trialID+"/evaluations/teval-1" {
		t.Fatalf("unexpected status request %s %s", status.Method, status.Path)
	}
	terminal := recorder.request(5)
	if terminal.Method != http.MethodPatch || terminal.Payload["status"] != "completed" {
		t.Fatalf("expected a completed terminal update, got %#v", terminal)
	}
	if _, exists := terminal.Payload["error"]; exists {
		t.Fatalf("cloud-evaluated trial must not carry error text: %#v", terminal.Payload)
	}
	finalize := recorder.request(6)
	if finalize.Method != http.MethodPost || finalize.Path != "/api/v1/experiment-runs/run-cloud:finalize" {
		t.Fatalf("unexpected finalize request %s %s", finalize.Method, finalize.Path)
	}
	if _, exists := finalize.Payload["score_count"]; exists {
		t.Fatalf("a cloud-evaluated run must leave score counting to the backend: %#v", finalize.Payload)
	}
}

func TestExperimentRunFinalizeStillAssertsLocalScoreCount(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-local"})})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-local"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-local"})
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-local", "status": "completed"})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-local", Name: "local run"})
	if err := run.Enter(context.Background()); err != nil {
		t.Fatalf("enter run: %v", err)
	}
	trial := run.TrialID("case-local")
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	if err := trial.End(context.Background(), nil); err != nil {
		t.Fatalf("end trial: %v", err)
	}
	if err := run.Finalize(context.Background(), ExperimentStatusCompleted, ""); err != nil {
		t.Fatalf("finalize run: %v", err)
	}
	finalize := recorder.request(4)
	if finalize.Payload["score_count"] != float64(1) {
		t.Fatalf("a locally scored run must still assert its score count: %#v", finalize.Payload)
	}
}

func TestTrialEvaluateSurfacesWorkerFailure(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	recorder.push(http.StatusOK, trialEvaluationResponse("failed", map[string]any{"error": "evaluator budget exhausted"}))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.BindConversation("conv-1")

	_, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{PollInterval: time.Millisecond})
	var failed *TrialEvaluationFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("expected *TrialEvaluationFailedError, got %v", err)
	}
	if failed.EvaluationID != "teval-1" || failed.Detail != "evaluator budget exhausted" {
		t.Fatalf("unexpected failure detail: %#v", failed)
	}
	if !errors.Is(err, ErrTrialEvaluationFailed) {
		t.Fatalf("expected ErrTrialEvaluationFailed, got %v", err)
	}
	if trial.cloudEvaluated {
		t.Fatal("a failed evaluation must not mark the trial cloud-evaluated")
	}
}

func TestTrialEvaluateTimesOutAfterBackingOff(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.BindConversation("conv-1")

	_, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		Timeout: 50 * time.Millisecond, PollInterval: time.Millisecond,
	})
	var timedOut *TrialEvaluationTimeoutError
	if !errors.As(err, &timedOut) {
		t.Fatalf("expected *TrialEvaluationTimeoutError, got %v", err)
	}
	if timedOut.EvaluationID != "teval-1" || !errors.Is(err, ErrTrialEvaluationTimeout) {
		t.Fatalf("unexpected timeout error: %#v (%v)", timedOut, err)
	}
	// Doubling from 1ms cannot fit more than a handful of reads into 50ms.
	if got := recorder.requestCount(); got > 9 {
		t.Fatalf("poll interval must back off, got %d requests", got)
	}
}

func TestTrialEvaluateReadsStatusAfterTheFinalSleep(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	recorder.push(http.StatusOK, trialEvaluationResponse("success", nil))
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
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
	if got := recorder.requestCount(); got != 4 {
		t.Fatalf("expected exactly one status read after the clamped sleep, got %d requests", got)
	}
}

func TestTimedOutEvaluationStillOmitsScoreCount(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-cloud"})})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-cloud", "status": "completed"})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-cloud", Name: "cloud run"})
	if err := run.Enter(context.Background()); err != nil {
		t.Fatalf("enter run: %v", err)
	}
	trial := run.TrialID("case-cloud")
	if err := trial.Start(context.Background()); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.BindConversation("conv-1")

	// One clamped sleep, one status read, then the deadline: the evaluation is
	// still queued server-side and its score will land after this run finalizes.
	if _, err := trial.Evaluate(context.Background(), "helpfulness", EvaluateOptions{
		Timeout: 5 * time.Millisecond, PollInterval: 10 * time.Millisecond,
	}); !errors.Is(err, ErrTrialEvaluationTimeout) {
		t.Fatalf("expected ErrTrialEvaluationTimeout, got %v", err)
	}
	if err := run.Finalize(context.Background(), ExperimentStatusFailed, "evaluation timed out"); err != nil {
		t.Fatalf("finalize run: %v", err)
	}
	finalize := recorder.request(recorder.requestCount() - 1)
	if finalize.Method != http.MethodPost || finalize.Path != "/api/v1/experiment-runs/run-cloud:finalize" {
		t.Fatalf("unexpected finalize request %s %s", finalize.Method, finalize.Path)
	}
	if _, exists := finalize.Payload["score_count"]; exists {
		t.Fatalf("a queued evaluation still writes a score, so the count must be omitted: %#v", finalize.Payload)
	}
}

func TestTrialEvaluateHonorsContextCancellation(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusAccepted, trialEvaluationResponse("queued", nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	handler := recorder.handler(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/evaluations/") {
			// Cancel during the wait. Whether cancellation lands on the in-flight
			// status read or on the next sleep, Evaluate reports the context error.
			cancel()
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
	if err := trial.Start(ctx); err != nil {
		t.Fatalf("start trial: %v", err)
	}
	trial.BindConversation("conv-1")

	_, err := trial.Evaluate(ctx, "helpfulness", EvaluateOptions{
		Timeout: 5 * time.Second, PollInterval: time.Millisecond,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	var timedOut *TrialEvaluationTimeoutError
	var failed *TrialEvaluationFailedError
	if errors.As(err, &timedOut) || errors.As(err, &failed) {
		t.Fatalf("cancellation must not be reported as timeout or failure: %v", err)
	}
}

func TestTrialEvaluateValidatesOptionsWithoutRequests(t *testing.T) {
	cases := []struct {
		name             string
		bindConversation bool
		evaluatorID      string
		options          EvaluateOptions
	}{
		{name: "no bound conversation", evaluatorID: "helpfulness"},
		{name: "empty evaluator id", bindConversation: true, evaluatorID: " "},
		{name: "negative timeout", bindConversation: true, evaluatorID: "helpfulness", options: EvaluateOptions{Timeout: -time.Second}},
		{name: "negative poll interval", bindConversation: true, evaluatorID: "helpfulness", options: EvaluateOptions{PollInterval: -time.Millisecond}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &experimentRecorder{}
			recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
			server := httptest.NewServer(recorder.handler(t))
			defer server.Close()

			client := newExperimentTestClient(t, server.URL)
			trial := NewTrial(client, TrialRef{RunID: "run-cloud", TestCaseID: "case-cloud"})
			if tc.bindConversation {
				trial.BindConversation("conv-1")
			}
			if _, err := trial.Evaluate(context.Background(), tc.evaluatorID, tc.options); !errors.Is(err, ErrExperimentValidationFailed) {
				t.Fatalf("expected ErrExperimentValidationFailed, got %v", err)
			}
			if recorder.requestCount() != 0 {
				t.Fatalf("invalid options must not issue a request, got %d", recorder.requestCount())
			}
		})
	}
}

func TestTrialCallbackErrorAfterCloudEvaluationEndsFailed(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	recorder.push(http.StatusOK, trialEvaluationResponse("success", nil))
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-cloud"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	run := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-cloud", Name: "cloud run"})
	callbackErr := errors.New("assertion failed after grading")
	err := run.WithTrialID(context.Background(), "case-cloud", func(ctx context.Context, trial *Trial) error {
		trial.BindConversation("conv-1")
		if _, evalErr := trial.Evaluate(ctx, "helpfulness", EvaluateOptions{PollInterval: time.Millisecond}); evalErr != nil {
			return evalErr
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("expected the callback error, got %v", err)
	}
	terminal := recorder.request(recorder.requestCount() - 1)
	if terminal.Method != http.MethodPatch || terminal.Payload["status"] != "failed" ||
		terminal.Payload["error"] != callbackErr.Error() {
		t.Fatalf("callback error must win over cloud evaluation: %#v", terminal)
	}
}

func TestTrialEvaluationErrorMessageFormat(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "worker failure with detail",
			err:  &TrialEvaluationFailedError{EvaluationID: "teval-1", Detail: "evaluator budget exhausted"},
			want: "agento11y trial evaluation teval-1 failed: evaluator budget exhausted",
		},
		{
			name: "worker failure without detail",
			err:  &TrialEvaluationFailedError{},
			want: "agento11y trial evaluation unknown failed",
		},
		{
			name: "timeout carries the wait",
			err:  evaluationTimeoutError("teval-1", 200*time.Millisecond),
			want: "agento11y trial evaluation teval-1 timed out: waited 200ms",
		},
		{
			name: "timeout without detail",
			err:  &TrialEvaluationTimeoutError{EvaluationID: " "},
			want: "agento11y trial evaluation unknown timed out",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
		})
	}
}
