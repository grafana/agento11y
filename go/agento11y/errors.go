package agento11y

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for errors.Is matching.
var (
	// ErrNilClient is returned when a nil *Client is used.
	ErrNilClient = errors.New("agento11y: nil client")
	// ErrNilRecorder is returned when a nil recorder is used.
	ErrNilRecorder = errors.New("agento11y: nil recorder")
	// ErrRecorderAlreadyEnded is returned on duplicate End calls.
	ErrRecorderAlreadyEnded = errors.New("agento11y: recorder already ended")
	// ErrRecorderNotReady is returned when a recorder has nil internals.
	ErrRecorderNotReady = errors.New("agento11y: recorder not initialized")
	// ErrToolNameRequired is returned when StartToolExecution receives an empty tool name.
	ErrToolNameRequired = errors.New("agento11y: tool name is required")
	// ErrValidationFailed wraps generation validation failures.
	ErrValidationFailed = errors.New("agento11y: generation validation failed")
	// ErrEmbeddingValidationFailed wraps embedding validation failures.
	ErrEmbeddingValidationFailed = errors.New("agento11y: embedding validation failed")
	// ErrEnqueueFailed wraps generation enqueue failures.
	ErrEnqueueFailed = errors.New("agento11y: generation enqueue failed")
	// ErrWorkflowStepValidationFailed wraps workflow-step validation failures.
	ErrWorkflowStepValidationFailed = errors.New("agento11y: workflow step validation failed")
	// ErrWorkflowStepEnqueueFailed wraps workflow-step enqueue failures.
	ErrWorkflowStepEnqueueFailed = errors.New("agento11y: workflow step enqueue failed")
	// ErrQueueFull is returned when the generation queue is at capacity.
	ErrQueueFull = errors.New("agento11y: generation queue is full")
	// ErrWorkflowStepQueueFull is returned when the workflow-step queue is at
	// capacity. It is a distinct sentinel from ErrQueueFull (which names the
	// generation queue), mirroring the JS and Python SDKs' separate messages.
	ErrWorkflowStepQueueFull = errors.New("agento11y: workflow step queue is full")
	// ErrClientShutdown is returned when enqueue happens after shutdown starts.
	ErrClientShutdown = errors.New("agento11y: client is shutting down")
	// ErrMappingFailed wraps provider-to-generation mapping failures.
	ErrMappingFailed = errors.New("agento11y: generation mapping failed")
	// ErrRatingValidationFailed wraps conversation rating input validation failures.
	ErrRatingValidationFailed = errors.New("agento11y: conversation rating validation failed")
	// ErrRatingConflict wraps idempotency conflicts when submitting conversation ratings.
	ErrRatingConflict = errors.New("agento11y: conversation rating conflict")
	// ErrRatingTransportFailed wraps conversation rating transport failures.
	ErrRatingTransportFailed = errors.New("agento11y: conversation rating transport failed")
	// ErrExperimentValidationFailed wraps experiment lifecycle validation failures.
	ErrExperimentValidationFailed = errors.New("agento11y: experiment validation failed")
	// ErrExperimentNotFound wraps experiment lifecycle not-found responses.
	ErrExperimentNotFound = errors.New("agento11y: experiment not found")
	// ErrExperimentConflict wraps experiment lifecycle conflict responses.
	ErrExperimentConflict = errors.New("agento11y: experiment conflict")
	// ErrExperimentTransportFailed wraps experiment lifecycle transport failures.
	ErrExperimentTransportFailed = errors.New("agento11y: experiment transport failed")
	// ErrTrialEvaluationFailed is returned when a stored evaluator finishes a
	// trial evaluation unsuccessfully. Use errors.As with
	// *TrialEvaluationFailedError to read the evaluation ID.
	ErrTrialEvaluationFailed = errors.New("agento11y: trial evaluation failed")
	// ErrTrialEvaluationTimeout is returned when waiting for a trial evaluation
	// exceeds the caller's deadline. Use errors.As with
	// *TrialEvaluationTimeoutError to read the evaluation ID.
	ErrTrialEvaluationTimeout = errors.New("agento11y: trial evaluation timed out")
	// ErrScoreValidationFailed wraps score export validation failures.
	ErrScoreValidationFailed = errors.New("agento11y: score validation failed")
	// ErrScoreExportFailed wraps score export transport failures or rejected scores.
	ErrScoreExportFailed = errors.New("agento11y: score export failed")
	// ErrHookDenied is the sentinel returned when hook evaluation responds
	// with action: "deny". Use errors.Is to detect this independently from
	// HookDeniedError's typed assertion.
	ErrHookDenied = errors.New("agento11y: hook evaluation denied")
	// ErrHookTransportFailed wraps hook evaluation transport failures.
	// Only surfaced when HooksConfig.FailOpen is false.
	ErrHookTransportFailed = errors.New("agento11y: hook evaluation transport failed")
)

// HookDeniedError is returned by EvaluateHook (and surfaced by framework
// adapters) when the server denies the request via a hook rule.
type HookDeniedError struct {
	RuleID      string
	Reason      string
	Evaluations []HookEvaluation
}

// Error formats the deny reason and the rule that triggered it.
func (e *HookDeniedError) Error() string {
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "request blocked by Agent Observability hook rule"
	}
	if id := strings.TrimSpace(e.RuleID); id != "" {
		return fmt.Sprintf("agento11y hook denied by rule %s: %s", id, reason)
	}
	return fmt.Sprintf("agento11y hook denied: %s", reason)
}

// Unwrap exposes ErrHookDenied for errors.Is matching.
func (e *HookDeniedError) Unwrap() error {
	return ErrHookDenied
}

// TrialEvaluationFailedError is returned when a stored evaluator ends a trial
// evaluation unsuccessfully. The evaluation keeps its row server-side, so
// triggering the same evaluator again requeues it.
type TrialEvaluationFailedError struct {
	EvaluationID string
	Detail       string
}

// Error formats the backend failure reason and the evaluation it came from.
func (e *TrialEvaluationFailedError) Error() string {
	return trialEvaluationMessage("failed", e.EvaluationID, e.Detail)
}

// Unwrap exposes ErrTrialEvaluationFailed for errors.Is matching.
func (e *TrialEvaluationFailedError) Unwrap() error {
	return ErrTrialEvaluationFailed
}

// TrialEvaluationTimeoutError is returned when a trial evaluation has not
// reached a terminal status before the caller's deadline. The evaluation keeps
// running server-side.
type TrialEvaluationTimeoutError struct {
	EvaluationID string
	Detail       string
}

// Error formats the wait that was abandoned and the evaluation it was for.
func (e *TrialEvaluationTimeoutError) Error() string {
	return trialEvaluationMessage("timed out", e.EvaluationID, e.Detail)
}

// Unwrap exposes ErrTrialEvaluationTimeout for errors.Is matching.
func (e *TrialEvaluationTimeoutError) Unwrap() error {
	return ErrTrialEvaluationTimeout
}

// trialEvaluationMessage formats one trial evaluation error. A blank detail is
// dropped instead of restating the outcome.
func trialEvaluationMessage(outcome, evaluationID, detail string) string {
	id := strings.TrimSpace(evaluationID)
	if id == "" {
		id = "unknown"
	}
	if detail = strings.TrimSpace(detail); detail == "" {
		return fmt.Sprintf("agento11y trial evaluation %s %s", id, outcome)
	}
	return fmt.Sprintf("agento11y trial evaluation %s %s: %s", id, outcome, detail)
}
