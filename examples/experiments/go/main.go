package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/agento11y/experiments"
)

var suite = experiments.TestSuite{
	SuiteID: "go-experiment-example",
	Name:    "Go example suite",
	Version: "2026-07-24",
	TestCases: []experiments.TestCase{
		{TestCaseID: "capital-france", Input: "What is the capital of France?", Expected: "Paris"},
		{TestCaseID: "two-plus-two", Input: "What is 2 + 2? Answer with just the number.", Expected: "4"},
		{TestCaseID: "largest-planet", Input: "What is the largest planet in our solar system?", Expected: "Jupiter"},
	},
}

func main() {
	ctx := context.Background()
	client, err := experiments.NewClientFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = client.Shutdown(context.Background()) }()

	attempts := envInt("N_ATTEMPTS", 1)
	planned := len(suite.TestCases) * attempts // send exactly what this job plans
	runID := getenv("RUN_ID", experiments.StableID("exp", suite.SuiteID, getenv("GIT_SHA", "local")))
	verifier := experiments.Evaluator{
		EvaluatorID: "example.exact_match", Version: "2026-07-24",
		Kind: experiments.EvaluatorKindDeterministic,
	}

	run, err := experiments.WithExperiment(ctx, client, experiments.ExperimentOptions{
		ExperimentID: runID,
		Name:         "Go streaming benchmark",
		Suite:        &suite,
		Candidate: &experiments.Candidate{
			AgentName: "go-example-agent", ModelProvider: "example",
			ModelName: "canned-answer", GitSHA: getenv("GIT_SHA", "local"),
		},
		DefaultEvaluator:  &verifier,
		PlannedTrialCount: &planned,
		Tags:              []string{"example", "go", "streaming"},
	}, func(ctx context.Context, run *experiments.Experiment) error {
		for _, testCase := range suite.Cases() {
			for attempt := 1; attempt <= attempts; attempt++ {
				if err := publishAttempt(ctx, run, testCase, attempt, verifier); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		log.Fatalf("run experiment: %v", err)
	}
	log.Printf("published %d score(s): %s", run.AcceptedScores(), run.URL())
}

func publishAttempt(ctx context.Context, run *experiments.Experiment, testCase experiments.TestCase, attempt int, verifier experiments.Evaluator) error {
	return run.WithTrial(ctx, testCase, func(ctx context.Context, trial *experiments.Trial) error {
		answer := answerQuestion(fmt.Sprint(testCase.Input))
		inputTokens, outputTokens, cost := 12, 3, 0.0002
		trial.RecordIO(experiments.RecordIOOptions{
			Input: fmt.Sprint(testCase.Input), Output: answer,
			ModelProvider: "example", ModelName: "canned-answer",
			InputTokens: &inputTokens, OutputTokens: &outputTokens,
		}).SetUsage(&inputTokens, &outputTokens, &cost)

		passed := strings.EqualFold(answer, fmt.Sprint(testCase.Expected))
		if _, err := trial.CheckScore("exact_match", passed, experiments.ScoreOptions{
			Evaluator:   &verifier,
			Explanation: fmt.Sprintf("expected %q, got %q", testCase.Expected, answer),
		}); err != nil {
			return err
		}
		// Harbor-style runners often publish several verifier results for one attempt.
		if _, err := trial.CheckScore("non_empty", answer != "", experiments.ScoreOptions{}); err != nil {
			return err
		}
		if _, err := trial.FinalScore(passed, experiments.ScoreOptions{Evaluator: &verifier}); err != nil {
			return err
		}

		path, cleanup, err := resultArtifact(testCase.TestCaseID, answer)
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err := trial.Artifact(ctx, experiments.ArtifactOptions{
			Name: "attempt-output.txt", Path: path,
		}); err != nil {
			return err
		}

		// Publish this scored attempt now. RUN_ID + case ID + attempt preserve
		// run/trial/generation/score identities when a job resumes.
		_, err = trial.Flush(ctx)
		return err
	}, experiments.TrialOptions{Attempt: attempt, Metadata: testCase.Metadata})
}

func resultArtifact(testCaseID, answer string) (string, func(), error) {
	file, err := os.CreateTemp("", "agento11y-"+testCaseID+"-*.txt")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.Remove(file.Name()) }
	if _, err := file.WriteString(answer); err != nil {
		_ = file.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return "", func() {}, err
	}
	return file.Name(), cleanup, nil
}

func answerQuestion(question string) string {
	for _, testCase := range suite.TestCases {
		if fmt.Sprint(testCase.Input) == question {
			return fmt.Sprint(testCase.Expected)
		}
	}
	return ""
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
