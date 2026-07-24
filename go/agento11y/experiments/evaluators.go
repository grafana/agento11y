package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const DefaultLLMJudgePrompt = `Grade the candidate output against the input and expected result.

Input:
{input}

Expected:
{expected}

Candidate output:
{output}

Return only JSON with this shape:
{"score": <number from 0 to 1>, "passed": <boolean>, "explanation": "<brief reason>"}
`

type GraderGeneration struct {
	Input         string
	Output        string
	ModelProvider string
	ModelName     string
	AgentName     string
	AgentVersion  string
	OperationName string
	Usage         *TokenUsage
}

type EvaluationResult struct {
	Evaluator   Evaluator
	Value       any
	Passed      bool
	Explanation string
	ScoreKey    string
	Metadata    map[string]any
	Grader      *GraderGeneration
}

type EvaluationInput struct {
	Input    any
	Output   any
	Expected any
}

type OutputEvaluator interface {
	EvaluateOutput(context.Context, EvaluationInput) (EvaluationResult, error)
}

// JudgeResponse is the typed provider-neutral return value for an LLM judge.
type JudgeResponse struct {
	Text  string
	Usage *TokenUsage
}

type JudgeInvoke func(context.Context, string) (JudgeResponse, error)
type JudgeParser func(string) (score float64, passed bool, explanation string, err error)

type LLMJudgeOptions struct {
	EvaluatorID    string
	Invoke         JudgeInvoke
	ModelName      string
	PromptTemplate string
	ModelProvider  string
	Version        string
	ScoreKey       string
	PassThreshold  float64
	// PassThresholdValue permits explicitly configuring a zero threshold.
	// When nil and PassThreshold is zero, the default is 0.5.
	PassThresholdValue *float64
	Parser             JudgeParser
	AgentName          string
	AgentVersion       string
	OperationName      string
}

type LLMJudge struct {
	opts      LLMJudgeOptions
	Evaluator Evaluator
}

func NewLLMJudge(opts LLMJudgeOptions) (*LLMJudge, error) {
	opts.EvaluatorID = strings.TrimSpace(opts.EvaluatorID)
	opts.ModelName = strings.TrimSpace(opts.ModelName)
	if opts.EvaluatorID == "" {
		return nil, errors.New("evaluator ID is required")
	}
	if opts.Invoke == nil {
		return nil, errors.New("invoke callback is required")
	}
	if opts.ModelName == "" {
		return nil, errors.New("model name is required")
	}
	if opts.PromptTemplate == "" {
		opts.PromptTemplate = DefaultLLMJudgePrompt
	}
	if opts.Version == "" {
		opts.Version = "1"
	}
	if opts.ScoreKey == "" {
		opts.ScoreKey = "final"
	}
	if opts.PassThresholdValue != nil {
		opts.PassThreshold = *opts.PassThresholdValue
	} else if opts.PassThreshold == 0 {
		opts.PassThreshold = 0.5
	}
	if opts.PassThreshold < 0 || opts.PassThreshold > 1 {
		return nil, errors.New("pass threshold must be between 0 and 1")
	}
	if opts.AgentName == "" {
		opts.AgentName = "agento11y-llm-judge"
	}
	if opts.OperationName == "" {
		opts.OperationName = "llm-judge"
	}
	return &LLMJudge{
		opts: opts,
		Evaluator: Evaluator{
			EvaluatorID: opts.EvaluatorID, Version: opts.Version, Kind: EvaluatorKindLLMJudge,
		},
	}, nil
}

func (j *LLMJudge) EvaluateOutput(ctx context.Context, input EvaluationInput) (EvaluationResult, error) {
	if j == nil || j.opts.Invoke == nil {
		return EvaluationResult{}, errors.New("LLM judge is not initialized")
	}
	prompt := renderJudgePrompt(j.opts.PromptTemplate, input)
	response, err := j.opts.Invoke(ctx, prompt)
	if err != nil {
		return EvaluationResult{}, err
	}
	var score float64
	var passed bool
	var explanation string
	if j.opts.Parser != nil {
		score, passed, explanation, err = j.opts.Parser(response.Text)
	} else {
		score, passed, explanation, err = parseJudgeResponse(response.Text, j.opts.PassThreshold)
	}
	if err != nil {
		return EvaluationResult{}, err
	}
	metadata := map[string]any{}
	if j.opts.ModelName != "" {
		metadata["judge_model"] = j.opts.ModelName
	}
	if j.opts.ModelProvider != "" {
		metadata["judge_provider"] = j.opts.ModelProvider
	}
	agentVersion := j.opts.AgentVersion
	if agentVersion == "" {
		agentVersion = j.opts.Version
	}
	return EvaluationResult{
		Evaluator: j.Evaluator, Value: score, Passed: passed,
		Explanation: explanation, ScoreKey: j.opts.ScoreKey, Metadata: metadata,
		Grader: &GraderGeneration{
			Input: prompt, Output: response.Text, ModelProvider: j.opts.ModelProvider,
			ModelName: j.opts.ModelName, AgentName: j.opts.AgentName,
			AgentVersion: agentVersion, OperationName: j.opts.OperationName,
			Usage: response.Usage,
		},
	}, nil
}

type RegexJudgeOptions struct {
	EvaluatorID     string
	Pattern         string
	Version         string
	ScoreKey        string
	FullMatch       bool
	Negate          bool
	Explanation     string
	CaseInsensitive bool
	Multiline       bool
	DotAll          bool
}

type RegexJudge struct {
	opts      RegexJudgeOptions
	pattern   *regexp.Regexp
	Evaluator Evaluator
}

func NewRegexJudge(opts RegexJudgeOptions) (*RegexJudge, error) {
	if strings.TrimSpace(opts.EvaluatorID) == "" {
		return nil, errors.New("evaluator ID is required")
	}
	if opts.Pattern == "" {
		return nil, errors.New("pattern is required")
	}
	prefix := ""
	if opts.CaseInsensitive {
		prefix += "i"
	}
	if opts.Multiline {
		prefix += "m"
	}
	if opts.DotAll {
		prefix += "s"
	}
	pattern := opts.Pattern
	if prefix != "" {
		pattern = "(?" + prefix + ")" + pattern
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("compile regex judge pattern: %w", err)
	}
	if opts.Version == "" {
		opts.Version = "1"
	}
	if opts.ScoreKey == "" {
		opts.ScoreKey = "regex_match"
	}
	return &RegexJudge{
		opts: opts, pattern: compiled,
		Evaluator: Evaluator{EvaluatorID: opts.EvaluatorID, Version: opts.Version, Kind: EvaluatorKindDeterministic},
	}, nil
}

func (j *RegexJudge) EvaluateOutput(_ context.Context, input EvaluationInput) (EvaluationResult, error) {
	if j == nil || j.pattern == nil {
		return EvaluationResult{}, errors.New("regex judge is not initialized")
	}
	text := fmt.Sprint(input.Output)
	matched := j.pattern.MatchString(text)
	if j.opts.FullMatch {
		location := j.pattern.FindStringIndex(text)
		matched = location != nil && location[0] == 0 && location[1] == len(text)
	}
	passed := matched
	if j.opts.Negate {
		passed = !matched
	}
	explanation := j.opts.Explanation
	if explanation == "" {
		switch {
		case passed && j.opts.Negate:
			explanation = fmt.Sprintf("output did not match /%s/", j.opts.Pattern)
		case passed:
			explanation = fmt.Sprintf("output matched /%s/", j.opts.Pattern)
		case j.opts.Negate:
			explanation = fmt.Sprintf("output matched excluded /%s/", j.opts.Pattern)
		default:
			explanation = fmt.Sprintf("output did not match /%s/", j.opts.Pattern)
		}
	}
	return EvaluationResult{
		Evaluator: j.Evaluator, Value: passed, Passed: passed,
		Explanation: explanation, ScoreKey: j.opts.ScoreKey,
		Metadata: map[string]any{"pattern": j.opts.Pattern},
	}, nil
}

func renderJudgePrompt(template string, input EvaluationInput) string {
	replacer := strings.NewReplacer(
		"{input}", fmt.Sprint(input.Input),
		"{output}", fmt.Sprint(input.Output),
		"{expected}", fmt.Sprint(input.Expected),
	)
	return replacer.Replace(template)
}

func parseJudgeResponse(raw string, threshold float64) (float64, bool, string, error) {
	objects := topLevelJSONObjects(raw)
	for _, object := range slices.Backward(objects) {
		rawScore, ok := object["score"]
		if !ok {
			continue
		}
		score, ok := numberValue(rawScore)
		if !ok {
			continue
		}
		score = min(1, max(0, score))
		passed := score >= threshold
		if rawPassed, ok := object["passed"]; ok {
			passed = passedValue(rawPassed, passed)
		} else if rawPassed, ok := object["pass"]; ok {
			passed = passedValue(rawPassed, passed)
		}
		explanation := stringValue(object["explanation"])
		if explanation == "" {
			explanation = stringValue(object["reason"])
		}
		return score, passed, strings.TrimSpace(explanation), nil
	}
	if len(objects) == 0 {
		return 0, false, "", errors.New("LLM judge response did not contain a JSON object")
	}
	return 0, false, "", errors.New("LLM judge response requires a numeric score")
}

func topLevelJSONObjects(raw string) []map[string]any {
	var objects []map[string]any
	for cursor := 0; cursor < len(raw); {
		relative := strings.IndexByte(raw[cursor:], '{')
		if relative < 0 {
			break
		}
		start := cursor + relative
		decoder := json.NewDecoder(strings.NewReader(raw[start:]))
		var value any
		if err := decoder.Decode(&value); err != nil {
			cursor = start + 1
			continue
		}
		if object, ok := value.(map[string]any); ok {
			objects = append(objects, object)
		}
		cursor = start + int(decoder.InputOffset())
	}
	return objects
}

func numberValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(typed), 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func passedValue(value any, fallback bool) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "true", "yes", "1", "pass", "passed":
			return true
		case "false", "no", "0", "fail", "failed":
			return false
		}
	}
	return fallback
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	return fmt.Sprint(value)
}
