package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/grafana/sigil-sdk/go/sigil"
)

var dataset = []sigil.DatasetItem{
	{
		ID:       "capital-france",
		Input:    "What is the capital of France?",
		Expected: "Paris",
		Metadata: map[string]any{"task_id": "capital_lookup", "task_category": "trivia"},
	},
	{
		ID:       "two-plus-two",
		Input:    "What is 2 + 2? Answer with just the number.",
		Expected: "4",
		Metadata: map[string]any{"task_id": "arithmetic", "task_category": "math"},
	},
	{
		ID:       "largest-planet",
		Input:    "What is the largest planet in our solar system?",
		Expected: "Jupiter",
		Metadata: map[string]any{"task_id": "astronomy", "task_category": "trivia"},
	},
}

func main() {
	ctx := context.Background()
	client := buildClient()
	defer func() { _ = client.Shutdown(ctx) }()

	runID := getenv("RUN_ID", "go-experiment-"+getenv("GIT_SHA", "local"))
	runner := sigil.ExperimentRunner{
		Client:      client,
		RunID:       runID,
		Name:        "Go example experiment",
		Dataset:     map[string]any{"id": "go-experiment-example", "version": "2026-06-02"},
		Candidate:   map[string]any{"git_sha": getenv("GIT_SHA", "local")},
		Tags:        []string{"example", "go"},
		AgentName:   "go-example-agent",
		FetchReport: true,
	}

	result, err := runner.Run(ctx, dataset, target(client), []sigil.DatasetScorer{exactMatchScorer})
	if err != nil {
		log.Fatalf("run experiment: %v", err)
	}

	fmt.Printf("\nExperiment %q finished: %d score(s) accepted.\n", result.RunID, result.AcceptedScores)
	if result.Report != nil {
		fmt.Printf("pass_rate=%.2f mean_score=%.2f\n", result.Report.Summary.PassRate, result.Report.Summary.MeanScore)
	}
	fmt.Printf("View in Sigil: %s\n", result.URL)
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

func target(client *sigil.Client) sigil.DatasetTarget {
	return func(ctx context.Context, item sigil.DatasetItem) (sigil.TargetResult, error) {
		answer, err := existingInstrumentedAgent(ctx, client, fmt.Sprint(item.Input))
		if err != nil {
			return sigil.TargetResult{}, err
		}
		return sigil.TargetResult{Output: answer}, nil
	}
}

func existingInstrumentedAgent(ctx context.Context, client *sigil.Client, question string) (string, error) {
	_, rec := client.StartGeneration(ctx, sigil.GenerationStart{
		Model: sigil.ModelRef{Provider: "example", Name: "canned-answer"},
	})
	defer rec.End()

	answer := answerQuestion(question)
	rec.SetResult(sigil.Generation{
		Model:  sigil.ModelRef{Provider: "example", Name: "canned-answer"},
		Input:  []sigil.Message{sigil.UserTextMessage(question)},
		Output: []sigil.Message{sigil.AssistantTextMessage(answer)},
	}, nil)
	return answer, nil
}

func exactMatchScorer(_ context.Context, item sigil.DatasetItem, result sigil.TargetResult) ([]sigil.ScoreOutput, error) {
	expected := strings.ToLower(fmt.Sprint(item.Expected))
	actual := strings.ToLower(fmt.Sprint(result.Output))
	passed := strings.Contains(actual, expected)
	value := 0.0
	if passed {
		value = 1.0
	}

	return []sigil.ScoreOutput{
		{
			EvaluatorID:      "example.exact_match",
			EvaluatorVersion: "2026-06-02",
			ScoreKey:         "exact_match",
			Value:            sigil.NumberScoreValue(value),
			Passed:           &passed,
			Explanation:      fmt.Sprintf("expected %q, got %q", item.Expected, result.Output),
		},
	}, nil
}

func answerQuestion(question string) string {
	for _, item := range dataset {
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
