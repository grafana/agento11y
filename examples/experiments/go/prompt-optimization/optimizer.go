package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/grafana/agento11y/go/agento11y/experiments"
)

const (
	agentName            = "unit-inference"
	evaluatorID          = "unit_match"
	evaluatorVersion     = "1.0.0"
	reasoningMaxTokens   = 4096
	optimizerDescription = "metaprompt-by-hand"
)

type promptCandidate struct {
	Label        string
	SystemPrompt string
}

func (c promptCandidate) promptVersion() string {
	sum := sha256.Sum256([]byte(c.SystemPrompt))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type proposalResponse struct {
	Prompts []struct {
		Prompt []chatMessage `json:"prompt"`
		Focus  string        `json:"improvement_focus"`
	} `json:"prompts"`
}

func proposeCandidates(ctx context.Context, llm *llmClient, current string, currentScore float64, count int, model string) ([]promptCandidate, error) {
	answer, _, err := llm.chat(ctx, chatRequest{
		Model: model,
		Messages: []chatMessage{
			{
				Role:    "system",
				Content: "You are an expert prompt engineer. Your task is to improve prompts for any type of task.",
			},
			{
				Role: "user",
				Content: fmt.Sprintf(
					"The prompt instructs a model to do this:\n%s\n\nHard constraints:\n%s\n\nCurrent prompt:\n%s\n\nCurrent score: %.3f\n\nGenerate [%d] improved versions of this prompt.\nEvery version must respect the hard constraints above.\nReturn valid JSON with the top-level key \"prompts\", where each item has a \"prompt\" key holding a list of chat messages and an \"improvement_focus\" key naming the change in a few words.",
					taskDescription, outputConstraint, current, currentScore, count,
				),
			},
		},
		MaxTokens: reasoningMaxTokens,
	})
	if err != nil {
		return nil, err
	}
	return extractCandidates(answer)
}

func extractCandidates(text string) ([]promptCandidate, error) {
	trimmed := strings.TrimSpace(text)
	if json.Valid([]byte(trimmed)) {
		if candidates := candidatesFromJSON([]byte(trimmed)); len(candidates) > 0 {
			return candidates, nil
		}
	}
	for index, character := range text {
		if character != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(text[index:]))
		var object json.RawMessage
		if err := decoder.Decode(&object); err != nil {
			continue
		}
		if candidates := candidatesFromJSON(object); len(candidates) > 0 {
			return candidates, nil
		}
	}
	return nil, errors.New("reasoning model did not return a usable JSON object")
}

func candidatesFromJSON(data []byte) []promptCandidate {
	var parsed proposalResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	candidates := make([]promptCandidate, 0, len(parsed.Prompts))
	for i, item := range parsed.Prompts {
		systemPrompt := ""
		for _, message := range item.Prompt {
			if message.Role == "system" {
				systemPrompt = message.Content
				break
			}
		}
		if systemPrompt == "" && len(item.Prompt) > 0 {
			systemPrompt = item.Prompt[0].Content
		}
		if systemPrompt == "" {
			continue
		}
		label := item.Focus
		if label == "" {
			label = fmt.Sprintf("candidate %d", i+1)
		}
		candidates = append(candidates, promptCandidate{Label: label, SystemPrompt: systemPrompt})
	}
	return candidates
}

type evaluationConfig struct {
	Model         string
	ModelProvider string
	SuiteVersion  string
}

func evaluateCandidate(
	ctx context.Context,
	client *experiments.Client,
	llm *llmClient,
	items []fixture,
	candidate promptCandidate,
	experimentID string,
	experimentName string,
	config evaluationConfig,
) (float64, error) {
	suite := buildSuite(items, config.SuiteVersion)
	planned := len(items)
	evaluator := experiments.Evaluator{
		EvaluatorID: evaluatorID,
		Version:     evaluatorVersion,
		Kind:        experiments.EvaluatorKindDeterministic,
	}
	run, err := experiments.WithExperiment(ctx, client, experiments.ExperimentOptions{
		ExperimentID: experimentID,
		Name:         experimentName,
		Suite:        &suite,
		Candidate: &experiments.Candidate{
			AgentName:     agentName,
			AgentVersion:  candidate.promptVersion(),
			PromptVersion: candidate.promptVersion(),
			ModelProvider: config.ModelProvider,
			ModelName:     config.Model,
		},
		DefaultEvaluator:  &evaluator,
		PlannedTrialCount: &planned,
		Tags:              []string{"prompt-optimization"},
		Metadata: map[string]any{
			"improvement_focus": candidate.Label,
			"optimizer":         optimizerDescription,
		},
	}, func(ctx context.Context, run *experiments.Experiment) error {
		for index, item := range items {
			testCase := suite.TestCases[index]
			if err := run.WithTrial(ctx, testCase, func(ctx context.Context, trial *experiments.Trial) error {
				input := []chatMessage{
					{Role: "system", Content: candidate.SystemPrompt},
					{Role: "user", Content: userMessage(item)},
				}
				answer, usage, err := callTask(ctx, llm, config.Model, input)
				if err != nil {
					return fmt.Errorf("run fixture %q: %w", item.ID, err)
				}
				recordedInput, err := json.Marshal(input)
				if err != nil {
					return fmt.Errorf("encode recorded input for fixture %q: %w", item.ID, err)
				}
				inputTokens, outputTokens := usage.PromptTokens, usage.CompletionTokens
				trial.RecordIO(experiments.RecordIOOptions{
					Input:         string(recordedInput),
					Output:        answer,
					ModelProvider: config.ModelProvider,
					ModelName:     config.Model,
					AgentName:     agentName,
					AgentVersion:  candidate.promptVersion(),
					InputTokens:   &inputTokens,
					OutputTokens:  &outputTokens,
				}).SetUsage(&inputTokens, &outputTokens, nil)

				value, explanation := scoreOutput(item, answer)
				passed := value == 1
				if _, err := trial.FinalScore(value, experiments.ScoreOptions{
					Evaluator:   &evaluator,
					Passed:      &passed,
					Explanation: explanation,
					Metadata: map[string]any{
						"expected_unit": item.ExpectedUnit,
						"resolved_unit": resolveUnit(answer, item),
						"scorer":        "production-parity-unit-resolution",
					},
				}); err != nil {
					return fmt.Errorf("score fixture %q: %w", item.ID, err)
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("run experiment %q: %w", experimentID, err)
	}
	report, err := run.Report(ctx)
	if err != nil {
		return 0, fmt.Errorf("read report for experiment %q: %w", experimentID, err)
	}
	if report.Summary.FinalScoreAvg == nil {
		return 0, fmt.Errorf("report for experiment %q has no final score average", experimentID)
	}
	return *report.Summary.FinalScoreAvg, nil
}

func callTask(ctx context.Context, llm *llmClient, model string, messages []chatMessage) (string, tokenUsage, error) {
	return llm.chat(ctx, chatRequest{
		Model:       model,
		Messages:    messages,
		Temperature: float64Pointer(0),
		MaxTokens:   taskMaxTokens,
	})
}

func float64Pointer(value float64) *float64 {
	return &value
}
