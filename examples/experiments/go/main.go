package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/grafana/sigil-sdk/go/sigil"
)

var suite = sigil.TestSuite{
	SuiteID: "go-experiment-example",
	Name:    "Go example suite",
	Version: "2026-06-02",
	TestCases: []sigil.TestCase{
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
	candidate := &sigil.Candidate{AgentName: "go-example-agent", GitSHA: getenv("GIT_SHA", "local")}
	evaluator := sigil.Evaluator{EvaluatorID: "example.exact_match", Version: "2026-06-02", Kind: sigil.EvaluatorKindDeterministic}
	run, err := sigil.WithExperiment(ctx, sigil.ExperimentOptions{
		Client:           client,
		RunID:            runID,
		Name:             "Go example experiment",
		Suite:            &suite,
		Candidate:        candidate,
		DefaultEvaluator: &evaluator,
		Tags:             []string{"example", "go"},
	}, func(ctx context.Context, exp *sigil.ExperimentRun) error {
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

	log.Printf("Experiment %q finished: %d score(s) accepted.", run.ExperimentID, run.AcceptedScores())
	if report, err := run.Report(ctx); err == nil {
		log.Printf("pass_rate=%.2f final_score_avg=%.2f", report.Summary.PassRate, report.Summary.FinalScoreAvg)
	}
	log.Printf("View in Sigil: %s", run.URL())
}

func runTestCase(ctx context.Context, client *sigil.Client, exp *sigil.ExperimentRun, testCase sigil.TestCase, evaluator sigil.Evaluator) (err error) {
	trial := exp.Trial(testCase, sigil.WithTrialMetadata(testCase.Metadata))
	if err := trial.Start(ctx); err != nil {
		return err
	}
	defer func() {
		if endErr := trial.End(ctx, err); err == nil {
			err = endErr
		}
	}()

	response, err := callRemoteInstrumentedAgent(ctx, client, exp.ExperimentID, testCase)
	if err != nil {
		return err
	}
	trial.BindGeneration(response.GenerationID, response.ConversationID)
	exactMatchScore(trial, testCase, response.Answer, evaluator)
	return nil
}

func buildClient() *sigil.Client {
	endpoint := strings.TrimRight(requireEnv("SIGIL_ENDPOINT"), "/")
	authMode := sigil.ExportAuthMode(strings.ToLower(getenv("SIGIL_AUTH_MODE", string(sigil.ExportAuthModeBasic))))
	authToken := strings.TrimSpace(os.Getenv("SIGIL_AUTH_TOKEN"))
	tenantID := requireEnv("SIGIL_AUTH_TENANT_ID")

	cfg := sigil.DefaultConfig()
	cfg.API.Endpoint = endpoint
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = endpoint
	cfg.GenerationExport.Auth = authConfig(authMode, tenantID, authToken)
	cfg.GenerationExport.Insecure = sigil.BoolPtr(false)
	return sigil.NewClient(cfg)
}

func authConfig(mode sigil.ExportAuthMode, tenantID string, token string) sigil.AuthConfig {
	switch mode {
	case sigil.ExportAuthModeBasic:
		if token == "" {
			log.Fatal("SIGIL_AUTH_TOKEN is required when SIGIL_AUTH_MODE=basic")
		}
		return sigil.AuthConfig{
			Mode:          sigil.ExportAuthModeBasic,
			TenantID:      tenantID,
			BasicPassword: token,
		}
	case sigil.ExportAuthModeBearer:
		if token == "" {
			log.Fatal("SIGIL_AUTH_TOKEN is required when SIGIL_AUTH_MODE=bearer")
		}
		return sigil.AuthConfig{
			Mode:        sigil.ExportAuthModeBearer,
			BearerToken: token,
		}
	case sigil.ExportAuthModeTenant:
		return sigil.AuthConfig{
			Mode:     sigil.ExportAuthModeTenant,
			TenantID: tenantID,
		}
	case sigil.ExportAuthModeNone:
		return sigil.AuthConfig{Mode: sigil.ExportAuthModeNone}
	default:
		log.Fatalf("unsupported SIGIL_AUTH_MODE %q", mode)
		return sigil.AuthConfig{}
	}
}

type remoteAgentResponse struct {
	Answer         string
	GenerationID   string
	ConversationID string
}

func callRemoteInstrumentedAgent(_ context.Context, client *sigil.Client, runID string, item sigil.TestCase) (remoteAgentResponse, error) {
	// In a real A2A/HTTP runner, runID would be serialized into request
	// metadata or a header, then restored by the receiving service.
	return remoteInstrumentedAgent(context.Background(), client, runID, item)
}

func remoteInstrumentedAgent(ctx context.Context, client *sigil.Client, runID string, item sigil.TestCase) (remoteAgentResponse, error) {
	question := fmt.Sprint(item.Input)
	generationID := sigil.StableID("gen", runID, item.TestCaseID)
	conversationID := sigil.StableID("conv", runID, item.TestCaseID)

	ctx = sigil.WithExperimentRunID(ctx, runID)
	ctx = sigil.WithConversationID(ctx, conversationID)
	ctx = sigil.WithAgentName(ctx, "go-example-agent")

	_, rec := client.StartGeneration(ctx, sigil.GenerationStart{
		ID:    generationID,
		Model: sigil.ModelRef{Provider: "example", Name: "canned-answer"},
	})
	defer rec.End()

	answer := answerQuestion(question)
	rec.SetResult(sigil.Generation{
		Model:  sigil.ModelRef{Provider: "example", Name: "canned-answer"},
		Input:  []sigil.Message{sigil.UserTextMessage(question)},
		Output: []sigil.Message{sigil.AssistantTextMessage(answer)},
	}, nil)
	return remoteAgentResponse{
		Answer:         answer,
		GenerationID:   generationID,
		ConversationID: conversationID,
	}, nil
}

func exactMatchScore(trial *sigil.Trial, item sigil.TestCase, output string, evaluator sigil.Evaluator) {
	expected := strings.ToLower(fmt.Sprint(item.Expected))
	actual := strings.ToLower(output)
	passed := strings.Contains(actual, expected)
	value := 0.0
	if passed {
		value = 1.0
	}

	trial.FinalScore(sigil.NumberScoreValue(value), sigil.ScoreOptions{
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
