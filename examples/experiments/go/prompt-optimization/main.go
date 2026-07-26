package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y/experiments"
)

type config struct {
	Endpoint       string
	TenantID       string
	IngestToken    string
	LLMBaseURL     string
	Model          string
	ReasoningModel string
	ModelProvider  string
	Start          string
	SuiteVersion   string
	Rounds         int
	Candidates     int
	RunID          string
	GrafanaURL     string
	RecordContent  bool
}

type historyEntry struct {
	ExperimentID string
	Label        string
	Score        float64
}

func main() {
	config, err := parseConfig()
	if err != nil {
		log.Fatal(err)
	}
	if err := run(context.Background(), config); err != nil {
		log.Fatal(err)
	}
}

func parseConfig() (config, error) {
	var config config
	flag.StringVar(&config.Endpoint, "endpoint", os.Getenv("AGENTO11Y_ENDPOINT"), "Agent Observability ingest endpoint (default $AGENTO11Y_ENDPOINT)")
	flag.StringVar(&config.TenantID, "tenant", os.Getenv("AGENTO11Y_AUTH_TENANT_ID"), "Agent Observability tenant ID (default $AGENTO11Y_AUTH_TENANT_ID)")
	flag.StringVar(&config.IngestToken, "ingest-token", os.Getenv("AGENTO11Y_AUTH_TOKEN"), "Agent Observability ingest token (default $AGENTO11Y_AUTH_TOKEN)")
	flag.StringVar(&config.LLMBaseURL, "llm-base-url", getenv("AGENTO11Y_OPTIMIZER_LLM_BASE_URL", "https://api.anthropic.com/v1"), "OpenAI-compatible LLM base URL")
	flag.StringVar(&config.Model, "model", getenv("AGENTO11Y_OPTIMIZER_MODEL", "claude-haiku-4-5"), "task model")
	flag.StringVar(&config.ReasoningModel, "reasoning-model", getenv("AGENTO11Y_OPTIMIZER_REASONING_MODEL", "claude-sonnet-5"), "model that proposes prompt rewrites")
	flag.StringVar(&config.ModelProvider, "model-provider", getenv("AGENTO11Y_OPTIMIZER_MODEL_PROVIDER", "anthropic"), "provider recorded on generations")
	flag.StringVar(&config.Start, "start", getenv("AGENTO11Y_OPTIMIZER_START", "naive"), "starting prompt: naive or production")
	flag.StringVar(&config.SuiteVersion, "suite-version", os.Getenv("AGENTO11Y_OPTIMIZER_SUITE_VERSION"), "stored suite version; defaults to a local fixture digest")
	flag.IntVar(&config.Rounds, "rounds", envInt("AGENTO11Y_OPTIMIZER_ROUNDS", 2), "hill-climbing rounds")
	flag.IntVar(&config.Candidates, "candidates", envInt("AGENTO11Y_OPTIMIZER_CANDIDATES", 4), "candidate prompts proposed per round")
	flag.StringVar(&config.RunID, "run-id", getenv("AGENTO11Y_OPTIMIZER_RUN_ID", "promptopt-"+time.Now().Format("20060102-150405")), "prefix for experiment IDs")
	flag.StringVar(&config.GrafanaURL, "grafana-url", getenv("AGENTO11Y_OPTIMIZER_GRAFANA_URL", getenv("AGENTO11Y_GRAFANA_URL", "http://localhost:3000")), "Grafana base URL used in links")
	flag.BoolVar(&config.RecordContent, "record-content", envBool("AGENTO11Y_OPTIMIZER_RECORD_CONTENT"), "disable SDK secret redaction so full prompts and fixtures are recorded")
	flag.Parse()

	config.Start = strings.ToLower(strings.TrimSpace(config.Start))
	if config.Start != "naive" && config.Start != "production" {
		return config, fmt.Errorf("--start must be naive or production")
	}
	if config.Rounds < 0 {
		return config, fmt.Errorf("--rounds must be non-negative")
	}
	if config.Candidates <= 0 {
		return config, fmt.Errorf("--candidates must be positive")
	}
	if strings.TrimSpace(config.RunID) == "" {
		return config, fmt.Errorf("--run-id must not be empty")
	}
	return config, nil
}

func run(ctx context.Context, config config) error {
	items, err := loadFixtures()
	if err != nil {
		return err
	}
	if len(items) != 27 {
		return fmt.Errorf("embedded suite has %d fixtures, want 27", len(items))
	}
	suiteVersion := config.SuiteVersion
	if suiteVersion == "" {
		suiteVersion = localSuiteVersion()
	}

	var redactSecrets *bool
	if config.RecordContent {
		value := false
		redactSecrets = &value
		fmt.Println("warning: content recording enabled; prompts and fixture inputs may contain sensitive data")
	}
	client, err := experiments.NewClient(experiments.ClientOptions{
		Endpoint:      config.Endpoint,
		TenantID:      config.TenantID,
		IngestToken:   config.IngestToken,
		GrafanaURL:    config.GrafanaURL,
		RedactSecrets: redactSecrets,
	})
	if err != nil {
		return err
	}
	defer func() {
		if err := client.Shutdown(context.Background()); err != nil {
			log.Printf("shut down Agent Observability client: %v", err)
		}
	}()
	llm, err := newLLMClient(config.LLMBaseURL, "", nil)
	if err != nil {
		return err
	}

	startPrompt := naiveSystemPrompt
	if config.Start == "production" {
		startPrompt = productionSystemPrompt()
	}
	best := promptCandidate{Label: "baseline (" + config.Start + ")", SystemPrompt: startPrompt}
	evaluation := evaluationConfig{
		Model: config.Model, ModelProvider: config.ModelProvider, SuiteVersion: suiteVersion,
	}

	fmt.Printf("run id: %s\n", config.RunID)
	fmt.Printf("suite: %s @ %s\n", suiteID, suiteVersion)
	fmt.Printf("\n--- baseline (%s prompt) ---\n", config.Start)
	bestScore, err := evaluateCandidate(
		ctx, client, llm, items, best,
		config.RunID+"-baseline",
		"Unit inference - baseline ("+config.Start+" prompt)",
		evaluation,
	)
	if err != nil {
		return err
	}
	baselineScore := bestScore
	fmt.Printf("  score %.3f  <- objective read back from Agent Observability report\n", bestScore)

	history := []historyEntry{{
		ExperimentID: config.RunID + "-baseline", Label: best.Label, Score: bestScore,
	}}
	evaluated := map[string]bool{best.promptVersion(): true}

	for round := 1; round <= config.Rounds; round++ {
		fmt.Printf("\n--- round %d ---\n", round)
		proposed, err := proposeCandidates(ctx, llm, best.SystemPrompt, bestScore, config.Candidates, config.ReasoningModel)
		if err != nil {
			fmt.Printf("  %v; stopping\n", err)
			break
		}
		candidates := make([]promptCandidate, 0, len(proposed))
		for _, candidate := range proposed {
			if !evaluated[candidate.promptVersion()] {
				candidates = append(candidates, candidate)
			}
		}
		if len(candidates) == 0 {
			fmt.Println("  no new candidates proposed; stopping")
			break
		}
		for candidateIndex, candidate := range candidates {
			evaluated[candidate.promptVersion()] = true
			experimentID := fmt.Sprintf("%s-r%d-c%d", config.RunID, round, candidateIndex+1)
			score, err := evaluateCandidate(
				ctx, client, llm, items, candidate,
				experimentID,
				fmt.Sprintf("Unit inference - round %d candidate %d", round, candidateIndex+1),
				evaluation,
			)
			if err != nil {
				return err
			}
			history = append(history, historyEntry{
				ExperimentID: experimentID, Label: candidate.Label, Score: score,
			})
			marker := ""
			if score > bestScore {
				best, bestScore, marker = candidate, score, "  <- new best"
			}
			fmt.Printf("  %.3f  %s%s\n", score, candidate.Label, marker)
		}
	}

	fmt.Println("\n=== result ===")
	fmt.Printf("baseline score : %.3f\n", baselineScore)
	fmt.Printf("best score     : %.3f\n", bestScore)
	fmt.Printf("best prompt    :\n%s\n", best.SystemPrompt)
	sort.SliceStable(history, func(i, j int) bool {
		return history[i].Score > history[j].Score
	})
	fmt.Println("\nexperiments recorded in Agent Observability:")
	for _, entry := range history {
		fmt.Printf("  %.3f  %s  (%s)\n", entry.Score, entry.ExperimentID, entry.Label)
	}
	comparisonURL, err := experimentComparisonURL(config.GrafanaURL)
	if err != nil {
		return err
	}
	fmt.Printf("\ncompare them in Grafana:\n  %s\n", comparisonURL)
	return nil
}

func experimentComparisonURL(baseURL string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return "", fmt.Errorf("parse Grafana URL: %w", err)
	}
	if base.Scheme != "http" && base.Scheme != "https" {
		return "", fmt.Errorf("Grafana URL must use http or https")
	}
	return base.ResolveReference(&url.URL{
		Path: "/a/grafana-agento11y-app/evaluation/experiments",
	}).String(), nil
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envInt(key string, fallback int) int {
	value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key)))
	if err != nil {
		return fallback
	}
	return value
}

func envBool(key string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(key)))
	return err == nil && value
}
