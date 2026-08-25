// Command cloud-evaluator grades experiment trials with an evaluator stored in
// Agent Observability instead of a score computed in the runner.
//
// Use this shape when the grading prompt lives in your tenant. The runner only
// has to make the agent's conversation findable:
//
//  1. Open an experiment with the ingest credential.
//  2. For each case, run the already-instrumented agent and bind its conversation.
//  3. Call trial.Evaluate(evaluatorID) and let the stored evaluator score it.
//  4. Close the trial without a local FinalScore; the backend owns the verdict.
//
// The canned agent here publishes its own generation so the example runs without
// a provider key. A real agent instrumented with the SDK or a provider wrapper
// already emits that generation, and you only need its conversation ID.
//
// SDK config via env: AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TOKEN, optional
// AGENTO11Y_AUTH_TENANT_ID and AGENTO11Y_EXPERIMENT_ID. This example also reads
// EVALUATOR_ID, EVALUATOR_VERSION, and GIT_SHA, which are its own knobs and not
// SDK settings.
package main

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"

	agento11y "github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/experiments"
)

type evalCase struct {
	ID       string
	Question string
}

var cases = []evalCase{
	{ID: "capital-france", Question: "What is the capital of France?"},
	{ID: "two-plus-two", Question: "What is 2 + 2? Answer with just the number."},
	{ID: "largest-planet", Question: "What is the largest planet in our solar system?"},
}

var answers = map[string]string{
	"capital-france": "Paris",
	"two-plus-two":   "4",
	"largest-planet": "Jupiter",
}

func main() {
	ctx := context.Background()
	client, err := experiments.NewClient(experiments.ClientOptions{
		EnableExperimentalFeatures: agento11y.BoolPtr(true),
	})
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()

	gitSHA := getenv("GIT_SHA", "manual")
	experimentID := getenv("AGENTO11Y_EXPERIMENT_ID", "cloud-evaluator-"+gitSHA)
	// The evaluator must already exist in your tenant; this example does not create one.
	evaluatorID := getenv("EVALUATOR_ID", "helpfulness")
	evaluatorVersion := getenv("EVALUATOR_VERSION", "")
	planned := len(cases)

	run, err := experiments.WithExperiment(ctx, client, experiments.ExperimentOptions{
		ExperimentID:      experimentID,
		Name:              "Go cloud evaluator example",
		PlannedTrialCount: &planned,
		Candidate:         &experiments.Candidate{AgentName: "example-agent", GitSHA: gitSHA},
		Tags:              []string{"example", "go", "cloud-evaluator"},
	}, func(ctx context.Context, run *experiments.Experiment) error {
		for _, testCase := range cases {
			if err := gradeCase(ctx, client, run, testCase, evaluatorID, evaluatorVersion); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatalf("run experiment: %v", err)
	}

	report, err := run.Report(ctx)
	if err != nil {
		log.Fatalf("report: %v", err)
	}
	log.Printf("trials=%d completed=%d", report.Summary.TrialCount, report.Summary.CompletedCount)
	// Only a score stored under the "final" key feeds report.Summary.PassRate, and a
	// stored evaluator scores under its own key, so that stays nil. The rows below
	// show what the backend actually attached.
	for _, row := range report.Rows {
		for _, result := range row.Trials {
			keys := make([]string, 0, len(result.Scores))
			for _, score := range result.Scores {
				keys = append(keys, score.ScoreKey)
			}
			if len(keys) == 0 {
				keys = append(keys, "none")
			}
			log.Printf("  %s: scores=%s", row.TestCaseID, strings.Join(keys, ", "))
		}
	}
	log.Printf("View in Agent Observability: %s", run.URL())
}

func gradeCase(
	ctx context.Context,
	client *experiments.Client,
	run *experiments.Experiment,
	testCase evalCase,
	evaluatorID, evaluatorVersion string,
) error {
	return run.WithTrialByCaseID(ctx, testCase.ID, func(ctx context.Context, trial *experiments.Trial) error {
		answer := answers[testCase.ID]
		// Stand-in for your instrumentation's conversation. With a real
		// instrumented agent, bind the ID it already exported instead.
		conversationID := trial.TrialID + "-agent"
		if err := client.RecordGeneration(ctx, trial.GenerationID, experiments.GenerationOptions{
			ConversationID: conversationID,
			InputText:      testCase.Question,
			OutputText:     answer,
			ModelProvider:  "example",
			ModelName:      "canned-answer",
			AgentName:      "example-agent",
			OperationName:  "answer_question",
			Usage:          experiments.TokenUsage{InputTokens: 12, OutputTokens: 3},
			Tags:           map[string]string{"experiment.run_id": run.ExperimentID, "task_id": testCase.ID},
		}); err != nil {
			return err
		}
		// The evaluator grades the conversation this binding points at.
		trial.BindConversation(conversationID)

		// Blocks until the worker finishes. No local FinalScore follows: the
		// stored evaluator writes the score and the report counts it.
		evaluation, err := trial.Evaluate(ctx, evaluatorID, experiments.EvaluateOptions{
			EvaluatorVersion: evaluatorVersion,
		})
		if err != nil {
			var failed *agento11y.TrialEvaluationFailedError
			var timedOut *agento11y.TrialEvaluationTimeoutError
			switch {
			case errors.As(err, &failed):
				log.Printf("%s: evaluation %s failed: %s", testCase.ID, failed.EvaluationID, failed.Detail)
			case errors.As(err, &timedOut):
				log.Printf("%s: evaluation %s still pending: %s", testCase.ID, timedOut.EvaluationID, timedOut.Detail)
			}
			return err
		}
		version := evaluation.EvaluatorVersion
		if version == "" {
			version = "latest"
		}
		log.Printf("%s: %s evaluator=%s@%s attempts=%d",
			testCase.ID, evaluation.Status, evaluation.EvaluatorID, version, evaluation.Attempts)
		return nil
	})
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
