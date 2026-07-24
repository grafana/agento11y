package agento11y

import (
	"fmt"
	"strings"
	"time"
)

func validateScore(score ScoreItem) error {
	var missing []string
	for name, value := range map[string]string{
		"score_id":          score.ScoreID,
		"evaluator_id":      score.EvaluatorID,
		"evaluator_version": score.EvaluatorVersion,
		"score_key":         score.ScoreKey,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: missing required field(s): %s", ErrScoreValidationFailed, strings.Join(missing, ", "))
	}
	if strings.TrimSpace(score.GenerationID) == "" && strings.TrimSpace(score.TrialID) == "" {
		return fmt.Errorf("%w: generation_id or trial_id is required", ErrScoreValidationFailed)
	}
	set := 0
	if score.Value.Number != nil {
		set++
	}
	if score.Value.Bool != nil {
		set++
	}
	if score.Value.String != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("%w: value must set exactly one of number/bool/string", ErrScoreValidationFailed)
	}
	return nil
}

func serializeUpsertRequest(req CreateExperimentRequest) map[string]any {
	out := map[string]any{
		"name":   strings.TrimSpace(req.Name),
		"source": experimentRunSource(),
	}
	if strings.TrimSpace(req.RunID) != "" {
		out["experiment_id"] = strings.TrimSpace(req.RunID)
	}
	if req.Description != "" {
		out["description"] = req.Description
	}
	if len(req.Tags) > 0 {
		out["tags"] = append([]string(nil), req.Tags...)
	}
	if strings.TrimSpace(req.SuiteID) != "" {
		out["suite_id"] = strings.TrimSpace(req.SuiteID)
	}
	if strings.TrimSpace(req.SuiteVersion) != "" {
		out["suite_version"] = strings.TrimSpace(req.SuiteVersion)
	}
	if len(req.Candidate) > 0 {
		out["candidate"] = cloneMetadata(req.Candidate)
	}
	if req.PlannedTrialCount != nil {
		out["planned_trial_count"] = *req.PlannedTrialCount
	}
	if len(req.Metadata) > 0 {
		out["metadata"] = cloneMetadata(req.Metadata)
	}
	return out
}

func serializeScore(score ScoreItem) map[string]any {
	out := map[string]any{
		"score_id":          score.ScoreID,
		"evaluator_id":      score.EvaluatorID,
		"evaluator_version": score.EvaluatorVersion,
		"score_key":         score.ScoreKey,
		"value":             serializeScoreValue(score.Value),
	}
	if score.GenerationID != "" {
		out["generation_id"] = score.GenerationID
	}
	if score.ConversationID != "" {
		out["conversation_id"] = score.ConversationID
	}
	if runID := score.ResolvedRunID(); runID != "" {
		out["experiment_id"] = runID
	}
	if score.TrialID != "" {
		out["trial_id"] = score.TrialID
	}
	if score.TestCaseID != "" {
		out["test_case_id"] = score.TestCaseID
	}
	if score.TraceID != "" {
		out["trace_id"] = score.TraceID
	}
	if score.SpanID != "" {
		out["span_id"] = score.SpanID
	}
	if score.GraderConversationID != "" {
		out["grader_conversation_id"] = score.GraderConversationID
	}
	if score.GraderGenerationID != "" {
		out["grader_generation_id"] = score.GraderGenerationID
	}
	if score.GraderTraceID != "" {
		out["grader_trace_id"] = score.GraderTraceID
	}
	if score.RuleID != "" {
		out["rule_id"] = score.RuleID
	}
	if score.EvaluatorKind != "" {
		out["evaluator_kind"] = score.EvaluatorKind
	}
	if score.Passed != nil {
		out["passed"] = *score.Passed
	}
	if score.Explanation != "" {
		out["explanation"] = score.Explanation
	}
	if len(score.Metadata) > 0 {
		out["metadata"] = cloneMetadata(score.Metadata)
	}
	if score.CreatedAt != nil {
		out["created_at"] = score.CreatedAt.UTC().Format(time.RFC3339Nano)
	}
	if score.Source != nil && (score.Source.Kind != "" || score.Source.ID != "") {
		out["source"] = map[string]any{"kind": score.Source.Kind, "id": score.Source.ID}
	}
	return out
}

func serializeScoreValue(value ScoreValue) map[string]any {
	if value.Number != nil {
		return map[string]any{"number": *value.Number}
	}
	if value.Bool != nil {
		return map[string]any{"bool": *value.Bool}
	}
	if value.String != nil {
		return map[string]any{"string": *value.String}
	}
	return map[string]any{}
}

func experimentRunSource() map[string]string {
	return map[string]string{"kind": "sdk", "id": "go"}
}

func normalizeExperimentFinalStatus(status ExperimentStatus) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(string(status)))
	switch normalized {
	case "succeeded", "completed":
		return "completed", nil
	case "failed":
		return "failed", nil
	default:
		return "", fmt.Errorf("%w: status must be completed or failed", ErrExperimentValidationFailed)
	}
}
