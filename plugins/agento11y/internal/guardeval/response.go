package guardeval

import "github.com/grafana/agento11y/go/agento11y"

// Evaluation extends the SDK evaluation shape with Effect because the
// standalone plugin depends on the published SDK type, which has no local
// effect field. Cloud evaluator fields pass through unchanged. Local
// EvaluatorID and LatencyMs stay zero because local rules have no evaluator
// resource and are not timed.
type Evaluation struct {
	RuleID        string `json:"rule_id"`
	Effect        string `json:"effect,omitempty"`
	EvaluatorID   string `json:"evaluator_id"`
	EvaluatorKind string `json:"evaluator_kind"`
	Passed        bool   `json:"passed"`
	LatencyMs     int64  `json:"latency_ms"`
	Explanation   string `json:"explanation,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// Response is the local engine's hook verdict. It matches the SDK response on
// the wire, with Evaluation carrying the local-only effect field.
type Response struct {
	Action           agento11y.HookAction `json:"action"`
	RuleID           string               `json:"rule_id,omitempty"`
	Reason           string               `json:"reason,omitempty"`
	TransformedInput *agento11y.HookInput `json:"transformed_input,omitempty"`
	Evaluations      []Evaluation         `json:"evaluations"`
}

// FromHookResponse copies an SDK response into the local wire shape. Effect
// remains empty for Cloud evaluations.
func FromHookResponse(resp agento11y.HookEvaluateResponse) Response {
	out := Response{
		Action:           resp.Action,
		RuleID:           resp.RuleID,
		Reason:           resp.Reason,
		TransformedInput: resp.TransformedInput,
		Evaluations:      make([]Evaluation, len(resp.Evaluations)),
	}
	for i, evaluation := range resp.Evaluations {
		out.Evaluations[i] = Evaluation{
			RuleID:        evaluation.RuleID,
			EvaluatorID:   evaluation.EvaluatorID,
			EvaluatorKind: evaluation.EvaluatorKind,
			Passed:        evaluation.Passed,
			LatencyMs:     evaluation.LatencyMs,
			Explanation:   evaluation.Explanation,
			Reason:        evaluation.Reason,
		}
	}
	return out
}
