package sigil

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for errors.Is matching.
var (
	// ErrNilClient is returned when a nil *Client is used.
	ErrNilClient = errors.New("sigil: nil client")
	// ErrNilRecorder is returned when a nil recorder is used.
	ErrNilRecorder = errors.New("sigil: nil recorder")
	// ErrRecorderAlreadyEnded is returned on duplicate End calls.
	ErrRecorderAlreadyEnded = errors.New("sigil: recorder already ended")
	// ErrRecorderNotReady is returned when a recorder has nil internals.
	ErrRecorderNotReady = errors.New("sigil: recorder not initialized")
	// ErrToolNameRequired is returned when StartToolExecution receives an empty tool name.
	ErrToolNameRequired = errors.New("sigil: tool name is required")
	// ErrValidationFailed wraps generation validation failures.
	ErrValidationFailed = errors.New("sigil: generation validation failed")
	// ErrEmbeddingValidationFailed wraps embedding validation failures.
	ErrEmbeddingValidationFailed = errors.New("sigil: embedding validation failed")
	// ErrEnqueueFailed wraps generation enqueue failures.
	ErrEnqueueFailed = errors.New("sigil: generation enqueue failed")
	// ErrWorkflowStepValidationFailed wraps workflow-step validation failures.
	ErrWorkflowStepValidationFailed = errors.New("sigil: workflow step validation failed")
	// ErrWorkflowStepEnqueueFailed wraps workflow-step enqueue failures.
	ErrWorkflowStepEnqueueFailed = errors.New("sigil: workflow step enqueue failed")
	// ErrQueueFull is returned when the generation queue is at capacity.
	ErrQueueFull = errors.New("sigil: generation queue is full")
	// ErrWorkflowStepQueueFull is returned when the workflow-step queue is at
	// capacity. It is a distinct sentinel from ErrQueueFull (which names the
	// generation queue), mirroring the JS and Python SDKs' separate messages.
	ErrWorkflowStepQueueFull = errors.New("sigil: workflow step queue is full")
	// ErrClientShutdown is returned when enqueue happens after shutdown starts.
	ErrClientShutdown = errors.New("sigil: client is shutting down")
	// ErrMappingFailed wraps provider-to-generation mapping failures.
	ErrMappingFailed = errors.New("sigil: generation mapping failed")
	// ErrRatingValidationFailed wraps conversation rating input validation failures.
	ErrRatingValidationFailed = errors.New("sigil: conversation rating validation failed")
	// ErrRatingConflict wraps idempotency conflicts when submitting conversation ratings.
	ErrRatingConflict = errors.New("sigil: conversation rating conflict")
	// ErrRatingTransportFailed wraps conversation rating transport failures.
	ErrRatingTransportFailed = errors.New("sigil: conversation rating transport failed")
	// ErrExperimentValidationFailed wraps experiment lifecycle validation failures.
	ErrExperimentValidationFailed = errors.New("sigil: experiment validation failed")
	// ErrExperimentNotFound wraps experiment lifecycle not-found responses.
	ErrExperimentNotFound = errors.New("sigil: experiment not found")
	// ErrExperimentConflict wraps experiment lifecycle conflict responses.
	ErrExperimentConflict = errors.New("sigil: experiment conflict")
	// ErrExperimentTransportFailed wraps experiment lifecycle transport failures.
	ErrExperimentTransportFailed = errors.New("sigil: experiment transport failed")
	// ErrScoreValidationFailed wraps score export validation failures.
	ErrScoreValidationFailed = errors.New("sigil: score validation failed")
	// ErrScoreExportFailed wraps score export transport failures or rejected scores.
	ErrScoreExportFailed = errors.New("sigil: score export failed")
	// ErrHookDenied is the sentinel returned when hook evaluation responds
	// with action: "deny". Use errors.Is to detect this independently from
	// HookDeniedError's typed assertion.
	ErrHookDenied = errors.New("sigil: hook evaluation denied")
	// ErrHookTransportFailed wraps hook evaluation transport failures.
	// Only surfaced when HooksConfig.FailOpen is false.
	ErrHookTransportFailed = errors.New("sigil: hook evaluation transport failed")
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
		reason = "request blocked by Sigil hook rule"
	}
	if id := strings.TrimSpace(e.RuleID); id != "" {
		return fmt.Sprintf("sigil hook denied by rule %s: %s", id, reason)
	}
	return fmt.Sprintf("sigil hook denied: %s", reason)
}

// Unwrap exposes ErrHookDenied for errors.Is matching.
func (e *HookDeniedError) Unwrap() error {
	return ErrHookDenied
}
