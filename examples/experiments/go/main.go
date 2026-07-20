package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
)

var suite = agento11y.TestSuite{
	SuiteID: "go-experiment-example",
	Name:    "Go example suite",
	Version: "2026-06-02",
	TestCases: []agento11y.TestCase{
		{
			TestCaseID: "capital-france",
			Name:       "Capital of France",
			Input:      "What is the capital of France?",
			Expected:   "Paris",
			Metadata:   map[string]any{"task_id": "capital_lookup", "task_category": "trivia"},
		},
		{
			TestCaseID: "two-plus-two",
			Name:       "Arithmetic",
			Input:      "What is 2 + 2? Answer with just the number.",
			Expected:   "4",
			Metadata:   map[string]any{"task_id": "arithmetic", "task_category": "math"},
		},
		{
			TestCaseID: "largest-planet",
			Name:       "Largest planet",
			Input:      "What is the largest planet in our solar system?",
			Expected:   "Jupiter",
			Metadata:   map[string]any{"task_id": "astronomy", "task_category": "trivia"},
		},
	},
}

func main() {
	ctx := context.Background()
	client := buildClient()
	defer func() { _ = client.Shutdown(ctx) }()

	runID := getenv("RUN_ID", "go-experiment-"+getenv("GIT_SHA", "local"))
	candidate := &agento11y.Candidate{AgentName: "go-example-agent", GitSHA: getenv("GIT_SHA", "local")}
	evaluator := agento11y.Evaluator{EvaluatorID: "example.exact_match", Version: "2026-06-02", Kind: agento11y.EvaluatorKindDeterministic}
	run, err := agento11y.WithExperiment(ctx, agento11y.ExperimentOptions{
		Client:           client,
		RunID:            runID,
		Name:             "Go example experiment",
		Suite:            &suite,
		Candidate:        candidate,
		DefaultEvaluator: &evaluator,
		Tags:             []string{"example", "go"},
	}, func(ctx context.Context, exp *agento11y.ExperimentRun) error {
		for _, testCase := range suite.Cases() {
			if err := runTestCase(ctx, client, exp, testCase, evaluator); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatalf("run experiment: %v", err)
	}

	log.Printf("Experiment %q finished: %d score(s) accepted.", run.RunID, run.AcceptedScores())
	if report, err := run.Report(ctx); err == nil {
		log.Printf("pass_rate=%.2f final_score_avg=%.2f", report.Summary.PassRate, report.Summary.FinalScoreAvg)
	}
	log.Printf("View in Sigil: %s", run.URL())
}

func runTestCase(ctx context.Context, client *agento11y.Client, exp *agento11y.ExperimentRun, testCase agento11y.TestCase, evaluator agento11y.Evaluator) (err error) {
	return exp.WithTrial(ctx, testCase, func(ctx context.Context, trial *agento11y.Trial) error {
		response, err := callRemoteInstrumentedAgent(ctx, client, exp.RunID, testCase)
		if err != nil {
			return err
		}
		trial.BindGeneration(response.GenerationID, response.ConversationID)
		exactMatchScore(trial, testCase, response.Answer, evaluator)
		return nil
	}, agento11y.WithTrialMetadata(testCase.Metadata))
}

func buildClient() *agento11y.Client {
	endpoint := strings.TrimRight(requireEnv("AGENTO11Y_ENDPOINT"), "/")
	authMode := agento11y.ExportAuthMode(strings.ToLower(getenv("AGENTO11Y_AUTH_MODE", string(agento11y.ExportAuthModeBasic))))
	authToken := strings.TrimSpace(os.Getenv("AGENTO11Y_AUTH_TOKEN"))
	tenantID := requireEnv("AGENTO11Y_AUTH_TENANT_ID")

	cfg := agento11y.DefaultConfig()
	cfg.API.Endpoint = endpoint
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = endpoint
	cfg.GenerationExport.Auth = authConfig(authMode, tenantID, authToken)
	cfg.GenerationExport.Insecure = agento11y.BoolPtr(false)
	return agento11y.NewClient(cfg)
}

func authConfig(mode agento11y.ExportAuthMode, tenantID string, token string) agento11y.AuthConfig {
	switch mode {
	case agento11y.ExportAuthModeBasic:
		if token == "" {
			log.Fatal("AGENTO11Y_AUTH_TOKEN is required when AGENTO11Y_AUTH_MODE=basic")
		}
		return agento11y.AuthConfig{
			Mode:          agento11y.ExportAuthModeBasic,
			TenantID:      tenantID,
			BasicPassword: token,
		}
	case agento11y.ExportAuthModeBearer:
		if token == "" {
			log.Fatal("AGENTO11Y_AUTH_TOKEN is required when AGENTO11Y_AUTH_MODE=bearer")
		}
		return agento11y.AuthConfig{
			Mode:        agento11y.ExportAuthModeBearer,
			BearerToken: token,
		}
	case agento11y.ExportAuthModeTenant:
		return agento11y.AuthConfig{
			Mode:     agento11y.ExportAuthModeTenant,
			TenantID: tenantID,
		}
	case agento11y.ExportAuthModeNone:
		return agento11y.AuthConfig{Mode: agento11y.ExportAuthModeNone}
	default:
		log.Fatalf("unsupported AGENTO11Y_AUTH_MODE %q", mode)
		return agento11y.AuthConfig{}
	}
}

type remoteAgentResponse struct {
	Answer         string
	GenerationID   string
	ConversationID string
}

func callRemoteInstrumentedAgent(_ context.Context, client *agento11y.Client, runID string, item agento11y.TestCase) (remoteAgentResponse, error) {
	// In a real A2A/HTTP runner, runID would be serialized into request
	// metadata or a header, then restored by the receiving service.
	return remoteInstrumentedAgent(context.Background(), client, runID, item)
}

func remoteInstrumentedAgent(ctx context.Context, client *agento11y.Client, runID string, item agento11y.TestCase) (remoteAgentResponse, error) {
	question := fmt.Sprint(item.Input)
	generationID := agento11y.StableID("gen", runID, item.TestCaseID)
	conversationID := agento11y.StableID("conv", runID, item.TestCaseID)

	ctx = agento11y.WithExperimentRunID(ctx, runID)
	ctx = agento11y.WithConversationID(ctx, conversationID)
	ctx = agento11y.WithAgentName(ctx, "go-example-agent")

	_, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
		ID:    generationID,
		Model: agento11y.ModelRef{Provider: "example", Name: "canned-answer"},
	})
	defer rec.End()

	answer := answerQuestion(question)
	rec.SetResult(agento11y.Generation{
		Model:  agento11y.ModelRef{Provider: "example", Name: "canned-answer"},
		Input:  []agento11y.Message{agento11y.UserTextMessage(question)},
		Output: []agento11y.Message{agento11y.AssistantTextMessage(answer)},
	}, nil)
	return remoteAgentResponse{
		Answer:         answer,
		GenerationID:   generationID,
		ConversationID: conversationID,
	}, nil
}

func exactMatchScore(trial *agento11y.Trial, item agento11y.TestCase, output string, evaluator agento11y.Evaluator) {
	expected := strings.ToLower(fmt.Sprint(item.Expected))
	actual := strings.ToLower(output)
	passed := strings.Contains(actual, expected)
	value := 0.0
	if passed {
		value = 1.0
	}

	trial.FinalScore(agento11y.NumberScoreValue(value), agento11y.ScoreOptions{
		Passed:      &passed,
		Explanation: fmt.Sprintf("expected %q, got %q", item.Expected, output),
		Evaluator:   &evaluator,
	})
}

func answerQuestion(question string) string {
	for _, item := range suite.TestCases {
		if fmt.Sprint(item.Input) == question {
			return fmt.Sprint(item.Expected)
		}
	}
	return ""
}

func getenv(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func requireEnv(key string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		log.Fatalf("%s is required", key)
	}
	return value
}
