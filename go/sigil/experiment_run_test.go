package sigil

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExperimentRunTrialIDWithoutSuiteDoesNotPanic(t *testing.T) {
	exp := NewExperimentRun(ExperimentOptions{RunID: "run-no-suite", Name: "no suite"})
	trial := exp.TrialID("case-1")
	if trial == nil {
		t.Fatal("expected trial")
	}
	if trial.ref.TestCaseID != "case-1" || trial.ref.TestCaseName != "case-1" {
		t.Fatalf("unexpected trial ref: %#v", trial.ref)
	}
}

func TestExperimentRunCopiesSuiteAtBoundary(t *testing.T) {
	suite := &TestSuite{
		SuiteID: "suite-1",
		TestCases: []TestCase{{
			TestCaseID: "case-1",
			Tags:       []string{"original"},
			Metadata:   map[string]any{"key": "value"},
		}},
	}
	exp := NewExperimentRun(ExperimentOptions{RunID: "run-1", Suite: suite})

	suite.TestCases[0].TestCaseID = "mutated"
	suite.TestCases[0].Tags[0] = "mutated"
	suite.TestCases[0].Metadata["key"] = "mutated"

	cases := exp.Suite().Cases()
	if len(cases) != 1 || cases[0].TestCaseID != "case-1" || cases[0].Tags[0] != "original" || cases[0].Metadata["key"] != "value" {
		t.Fatalf("experiment retained caller-owned suite data: %#v", cases)
	}

	cases[0].Tags[0] = "caller-mutated"
	cases[0].Metadata["key"] = "caller-mutated"
	fresh := exp.Suite().Cases()
	if fresh[0].Tags[0] != "original" || fresh[0].Metadata["key"] != "value" {
		t.Fatalf("suite accessor returned mutable internals: %#v", fresh)
	}
}

func TestExperimentRunWithTrialEndsLifecycle(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
	recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
	recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	testCase := TestCase{TestCaseID: "case-1", Name: "Case 1", Metadata: map[string]any{"task": "demo"}}
	exp := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-1", Name: "run"})
	evaluator := Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}
	err := exp.WithTrial(context.Background(), testCase, func(ctx context.Context, trial *Trial) error {
		run, ok := experimentRunFromContext(ctx)
		if !ok || run.RunID != "run-1" {
			t.Fatalf("expected experiment-aware callback context, got run=%#v ok=%v", run, ok)
		}
		trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &evaluator})
		return nil
	}, WithTrialMetadata(testCase.Metadata))
	if err != nil {
		t.Fatalf("with trial: %v", err)
	}
	if recorder.requestCount() != 3 {
		t.Fatalf("expected create, score export, update; got %d request(s)", recorder.requestCount())
	}
	if got := exp.AcceptedScores(); got != 1 {
		t.Fatalf("expected one accepted score, got %d", got)
	}
	updateReq := recorder.request(2)
	if updateReq.Payload["status"] != "completed" {
		t.Fatalf("expected completed trial update, got %#v", updateReq.Payload)
	}
}

func TestExperimentRunWithTrialFinalizesFailedOnPanic(t *testing.T) {
	type trialRunner func(*ExperimentRun, context.Context, TestCase, func(context.Context, *Trial) error) error
	tests := []struct {
		name string
		run  trialRunner
	}{
		{
			name: "WithTrial",
			run: func(exp *ExperimentRun, ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error) error {
				return exp.WithTrial(ctx, testCase, fn)
			},
		},
		{
			name: "WithTrialID",
			run: func(exp *ExperimentRun, ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error) error {
				return exp.WithTrialID(ctx, testCase.TestCaseID, fn)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &experimentRecorder{}
			recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
			recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
			recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
			server := httptest.NewServer(recorder.handler(t))
			defer server.Close()

			client := newExperimentTestClient(t, server.URL)
			exp := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-1", Name: "run"})
			testCase := TestCase{TestCaseID: "case-1", Name: "Case 1"}
			evaluator := Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}

			var recovered any
			func() {
				defer func() {
					recovered = recover()
				}()
				_ = tt.run(exp, context.Background(), testCase, func(_ context.Context, trial *Trial) error {
					trial.Succeed()
					trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &evaluator})
					panic("boom")
				})
			}()

			if recovered != "boom" {
				t.Fatalf("expected panic to be rethrown, got %#v", recovered)
			}
			if recorder.requestCount() != 3 {
				t.Fatalf("expected create, score export, update; got %d request(s)", recorder.requestCount())
			}
			updateReq := recorder.request(2)
			if updateReq.Payload["status"] != "failed" {
				t.Fatalf("expected failed trial update, got %#v", updateReq.Payload)
			}
			if updateReq.Payload["error"] != "trial callback panic: boom" {
				t.Fatalf("expected panic error in trial update, got %#v", updateReq.Payload)
			}
		})
	}
}

func TestExperimentRunWithTrialErrorOverridesTerminalStatus(t *testing.T) {
	type trialRunner func(*ExperimentRun, context.Context, TestCase, func(context.Context, *Trial) error) error
	tests := []struct {
		name string
		run  trialRunner
	}{
		{
			name: "WithTrial",
			run: func(exp *ExperimentRun, ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error) error {
				return exp.WithTrial(ctx, testCase, fn)
			},
		},
		{
			name: "WithTrialID",
			run: func(exp *ExperimentRun, ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error) error {
				return exp.WithTrialID(ctx, testCase.TestCaseID, fn)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := &experimentRecorder{}
			recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
			recorder.push(http.StatusAccepted, map[string]any{"results": []map[string]any{{"score_id": "score-1", "accepted": true}}})
			recorder.push(http.StatusOK, map[string]any{"trial_id": "trial-1"})
			server := httptest.NewServer(recorder.handler(t))
			defer server.Close()

			client := newExperimentTestClient(t, server.URL)
			exp := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-1", Name: "run"})
			testCase := TestCase{TestCaseID: "case-1", Name: "Case 1"}
			evaluator := Evaluator{EvaluatorID: "exact", Version: "1", Kind: EvaluatorKindDeterministic}

			err := tt.run(exp, context.Background(), testCase, func(_ context.Context, trial *Trial) error {
				trial.Succeed()
				trial.FinalScore(BoolScoreValue(true), ScoreOptions{Evaluator: &evaluator})
				return errors.New("callback failed")
			})
			if err == nil || err.Error() != "callback failed" {
				t.Fatalf("expected callback error, got %v", err)
			}
			if recorder.requestCount() != 3 {
				t.Fatalf("expected create, score export, update; got %d request(s)", recorder.requestCount())
			}
			updateReq := recorder.request(2)
			if updateReq.Payload["status"] != "failed" || updateReq.Payload["error"] != "callback failed" {
				t.Fatalf("expected failed trial update from callback error, got %#v", updateReq.Payload)
			}
		})
	}
}

func TestExperimentRunWithTrialRequiresTestCaseID(t *testing.T) {
	client := newExperimentTestClient(t, "http://example.invalid")
	exp := NewExperimentRun(ExperimentOptions{Client: client, RunID: "run-1", Name: "run"})
	err := exp.WithTrial(context.Background(), TestCase{}, func(context.Context, *Trial) error {
		return nil
	})
	if !errors.Is(err, ErrExperimentValidationFailed) {
		t.Fatalf("expected validation error, got %v", err)
	}
}

func TestFinalScoreBoolInfersPassed(t *testing.T) {
	trial := NewTrial(nil, TrialRef{RunID: "run-1", TestCaseID: "case-1"})
	score := trial.FinalScore(BoolScoreValue(true), ScoreOptions{})
	if score.Passed == nil || !*score.Passed {
		t.Fatalf("expected bool final score to infer passed, got %#v", score.Passed)
	}
	if trial.finalPassed == nil || !*trial.finalPassed {
		t.Fatalf("expected trial final verdict to be true, got %#v", trial.finalPassed)
	}

	trial = NewTrial(nil, TrialRef{RunID: "run-1", TestCaseID: "case-2"})
	score = trial.Score("final", BoolScoreValue(false), ScoreOptions{})
	if score.Passed == nil || *score.Passed {
		t.Fatalf("expected Score(\"final\", bool) to infer failed, got %#v", score.Passed)
	}
	if trial.finalPassed == nil || *trial.finalPassed {
		t.Fatalf("expected trial final verdict to be false, got %#v", trial.finalPassed)
	}
}

func TestFinalScoreNumberRequiresExplicitPassed(t *testing.T) {
	trial := NewTrial(nil, TrialRef{RunID: "run-1", TestCaseID: "case-1"})
	score := trial.FinalScore(NumberScoreValue(0.2), ScoreOptions{})
	if score.Passed != nil {
		t.Fatalf("expected numeric final score to require explicit passed, got %#v", score.Passed)
	}
	if trial.finalPassed != nil {
		t.Fatalf("expected trial final verdict to remain nil, got %#v", trial.finalPassed)
	}
}

func TestWithExperimentFinalizesFailedOnPanic(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-panic"})})
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-panic", "status": "failed"})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	defer func() {
		recovered := recover()
		if recovered != "boom" {
			t.Fatalf("expected panic to be rethrown, got %#v", recovered)
		}
		if recorder.requestCount() != 2 {
			t.Fatalf("expected create and failed finalize requests, got %d", recorder.requestCount())
		}
		finalizeReq := recorder.request(1)
		if finalizeReq.Method != http.MethodPost || finalizeReq.Path != "/api/v1/experiment-runs/run-panic:finalize" {
			t.Fatalf("unexpected finalize request: %#v", finalizeReq)
		}
		if finalizeReq.Payload["status"] != "failed" || finalizeReq.Payload["error"] != "boom" {
			t.Fatalf("expected failed finalize payload, got %#v", finalizeReq.Payload)
		}
	}()

	_, _ = WithExperiment(context.Background(), ExperimentOptions{Client: client, RunID: "run-panic", Name: "panic"}, func(context.Context, *ExperimentRun) error {
		panic("boom")
	})
}

func TestWithExperimentFinalizesWithCleanupContextAfterCancel(t *testing.T) {
	recorder := &experimentRecorder{}
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-canceled"})})
	recorder.push(http.StatusOK, map[string]any{"run": experimentBody(map[string]any{"experiment_id": "run-canceled", "status": "failed"})})
	server := httptest.NewServer(recorder.handler(t))
	defer server.Close()

	client := newExperimentTestClient(t, server.URL)
	ctx, cancel := context.WithCancel(context.Background())
	_, err := WithExperiment(ctx, ExperimentOptions{Client: client, RunID: "run-canceled", Name: "canceled"}, func(context.Context, *ExperimentRun) error {
		cancel()
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if recorder.requestCount() != 2 {
		t.Fatalf("expected create and failed finalize requests, got %d", recorder.requestCount())
	}
	finalizeReq := recorder.request(1)
	if finalizeReq.Method != http.MethodPost || finalizeReq.Path != "/api/v1/experiment-runs/run-canceled:finalize" {
		t.Fatalf("unexpected finalize request: %#v", finalizeReq)
	}
	if finalizeReq.Payload["status"] != "failed" || finalizeReq.Payload["error"] != context.Canceled.Error() {
		t.Fatalf("expected failed finalize payload, got %#v", finalizeReq.Payload)
	}
}

func TestTrialRefEnvRoundTrip(t *testing.T) {
	ref := TrialRef{RunID: "run-4", TestCaseID: "c1", Attempt: 3, SuiteID: "s", SuiteVersion: "2.0.0"}
	env := ref.ToEnv()
	if env[EnvExperimentID] != "run-4" || env[EnvAttempt] != "3" {
		t.Fatalf("unexpected env: %#v", env)
	}
	t.Setenv(EnvExperimentID, env[EnvExperimentID])
	t.Setenv(EnvTestCaseID, env[EnvTestCaseID])
	t.Setenv(EnvAttempt, env[EnvAttempt])
	t.Setenv(EnvSuiteID, env[EnvSuiteID])
	t.Setenv(EnvSuiteVersion, env[EnvSuiteVersion])
	restored, ok := TrialRefFromEnv()
	if !ok || restored.RunID != "run-4" || restored.TestCaseID != "c1" || restored.Attempt != 3 {
		t.Fatalf("unexpected ref: %#v ok=%v", restored, ok)
	}
}
