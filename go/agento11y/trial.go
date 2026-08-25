package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"time"
)

type TrialOption func(*Trial)

func WithTrialAttempt(attempt int) TrialOption {
	return func(t *Trial) {
		if attempt > 0 {
			t.ref.Attempt = attempt
			t.trialID = StableID("trial", t.ref.RunID, t.ref.TestCaseID, t.ref.Attempt)
			t.generationID = StableID("gen", t.ref.RunID, t.ref.TestCaseID, t.ref.Attempt)
		}
	}
}

func WithTrialMetadata(metadata map[string]any) TrialOption {
	return func(t *Trial) {
		maps.Copy(t.metadata, metadata)
	}
}

func WithTrialDefaultEvaluator(evaluator Evaluator) TrialOption {
	return func(t *Trial) {
		t.defaultEvaluator = evaluator.normalized()
	}
}

type Trial struct {
	client *Client
	ref    TrialRef

	experiment       *ExperimentRun
	candidate        *Candidate
	defaultEvaluator Evaluator
	metadata         map[string]any

	trialID        string
	status         TrialStatus
	conversationID string
	traceID        string
	spanID         string
	errorText      string
	generationID   string

	generationBound    bool
	generationExported bool
	hasGeneration      bool
	flushFailed        bool
	io                 map[string]any
	trialCreated       bool
	usage              map[string]any
	started            time.Time

	buffer         []ScoreItem
	accepted       int
	hasFinal       bool
	cloudEvaluated bool
	finalPassed    *bool
	artifacts      []map[string]any
}

func NewTrial(client *Client, ref TrialRef, opts ...TrialOption) *Trial {
	ref.RunID = strings.TrimSpace(ref.RunID)
	ref.TestCaseID = strings.TrimSpace(ref.TestCaseID)
	if ref.Attempt <= 0 {
		ref.Attempt = 1
	}
	t := &Trial{
		client:           client,
		ref:              ref,
		defaultEvaluator: Evaluator{EvaluatorID: "sdk", Version: "0", Kind: EvaluatorKindCustom},
		metadata:         map[string]any{},
		trialID:          StableID("trial", ref.RunID, ref.TestCaseID, ref.Attempt),
		status:           TrialStatusRunning,
		generationID:     StableID("gen", ref.RunID, ref.TestCaseID, ref.Attempt),
		io:               map[string]any{},
		usage:            map[string]any{},
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	return t
}

func NewTrialFromRef(client *Client, ref *TrialRef, opts ...TrialOption) (*Trial, error) {
	if ref == nil {
		return nil, fmt.Errorf("%w: trial ref is required; set AGENTO11Y_EXPERIMENT_ID and AGENTO11Y_TEST_CASE_ID", ErrExperimentValidationFailed)
	}
	return NewTrial(client, *ref, opts...), nil
}

func (t *Trial) Start(ctx context.Context) error {
	if t == nil || t.client == nil {
		return ErrNilClient
	}
	if t.ref.RunID == "" {
		return fmt.Errorf("%w: run_id is required", ErrExperimentValidationFailed)
	}
	if t.ref.TestCaseID == "" {
		return fmt.Errorf("%w: test_case_id is required", ErrExperimentValidationFailed)
	}
	t.started = time.Now()
	return t.createTrial(ctx)
}

func (t *Trial) End(ctx context.Context, err error) error {
	if t == nil {
		return ErrNilClient
	}
	t.resolveEndStatus(err)
	cleanupCtx, cancel := experimentCleanupContext(ctx)
	defer cancel()
	if err := t.createTrial(cleanupCtx); err != nil {
		return err
	}
	_, flushErr := t.Flush(cleanupCtx)
	if flushErr != nil {
		t.status = TrialStatusErrored
		t.errorText = flushErr.Error()
		t.flushFailed = true
		if finalizeErr := t.finalizeTrial(cleanupCtx); finalizeErr != nil {
			return errors.Join(flushErr, fmt.Errorf("finalize trial %q: %w", t.trialID, finalizeErr))
		}
		return flushErr
	}
	return t.finalizeTrial(cleanupCtx)
}

func (t *Trial) resolveEndStatus(err error) {
	if err != nil {
		t.status = TrialStatusErrored
		t.errorText = err.Error()
		t.flushFailed = false
		return
	}
	if !t.hasFinal && t.cloudEvaluated {
		// A stored evaluator graded this trial; the verdict and the score count come
		// from the backend, not from a local final score.
		if t.status == TrialStatusRunning || t.flushFailed {
			t.status = TrialStatusCompleted
			t.errorText = ""
		}
		t.flushFailed = false
		return
	}
	if !t.hasFinal {
		t.status = TrialStatusFailed
		if t.errorText == "" || t.flushFailed {
			t.errorText = "trial exited without a final score"
		}
		t.flushFailed = false
		return
	}
	if t.status == TrialStatusRunning || t.flushFailed {
		if t.finalPassed != nil && *t.finalPassed {
			t.status = TrialStatusPassed
		} else {
			t.status = TrialStatusFailed
		}
		t.errorText = ""
	}
	t.flushFailed = false
}

func (t *Trial) createTrial(ctx context.Context) error {
	if t == nil || t.client == nil {
		return ErrNilClient
	}
	if t.trialCreated {
		return nil
	}
	metadata := cloneMetadata(t.metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if t.ref.TestCaseName != "" {
		metadata["test_case_name"] = t.ref.TestCaseName
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	_, err := t.client.UpsertTrial(ctx, t.ref.RunID, UpsertTrialRequest{
		TrialID:        t.trialID,
		TestCaseID:     t.ref.TestCaseID,
		Attempt:        t.ref.Attempt,
		Status:         string(TrialStatusRunning),
		ConversationID: t.conversationID,
		TraceID:        t.traceID,
		SpanID:         t.spanID,
		Metadata:       metadata,
	})
	if err != nil {
		return err
	}
	t.trialCreated = true
	return nil
}

func (t *Trial) finalizeTrial(ctx context.Context) error {
	if !t.trialCreated {
		return nil
	}
	backendStatus := "completed"
	if t.status == TrialStatusErrored {
		backendStatus = "failed"
	}
	req := UpdateTrialRequest{
		Status:         backendStatus,
		Error:          t.errorText,
		ConversationID: t.conversationID,
		TraceID:        t.traceID,
		SpanID:         t.spanID,
	}
	if v, ok := t.usage["cost"].(float64); ok {
		req.Cost = &v
	}
	if v, ok := t.usage["input_tokens"].(int); ok {
		req.InputTokens = &v
	}
	if v, ok := t.usage["output_tokens"].(int); ok {
		req.OutputTokens = &v
	}
	if !t.started.IsZero() {
		ms := int(time.Since(t.started).Milliseconds())
		req.DurationMillis = &ms
	}
	_, err := t.client.UpdateTrial(ctx, t.ref.RunID, t.trialID, req)
	return err
}

// EvaluateOptions configures Trial.Evaluate. A zero Timeout or PollInterval
// uses the default; a negative one is rejected.
type EvaluateOptions struct {
	// EvaluatorVersion pins a stored evaluator version. Empty lets Agent
	// Observability pin the latest active version.
	EvaluatorVersion string
	// Timeout bounds the wait for a terminal status. Defaults to 300s.
	Timeout time.Duration
	// PollInterval is the first delay between status reads, doubling up to 5s.
	// Defaults to 500ms.
	PollInterval time.Duration
}

const (
	defaultEvaluationTimeout      = 300 * time.Second
	defaultEvaluationPollInterval = 500 * time.Millisecond
	// Ceiling for the Evaluate poll backoff, so a long wait does not keep reading
	// status at the floor rate.
	maxEvaluationPollInterval = 5 * time.Second
)

func (o EvaluateOptions) resolve() (timeout, pollInterval time.Duration, err error) {
	if o.Timeout < 0 {
		return 0, 0, fmt.Errorf("%w: evaluation timeout must not be negative", ErrExperimentValidationFailed)
	}
	if o.PollInterval < 0 {
		return 0, 0, fmt.Errorf("%w: evaluation poll interval must not be negative", ErrExperimentValidationFailed)
	}
	timeout, pollInterval = o.Timeout, o.PollInterval
	if timeout == 0 {
		timeout = defaultEvaluationTimeout
	}
	if pollInterval == 0 {
		pollInterval = defaultEvaluationPollInterval
	}
	return timeout, pollInterval, nil
}

// Evaluate runs a stored evaluator against this trial's bound conversation.
//
// It grades the conversation Agent Observability already stored, using an
// evaluator defined in your tenant, instead of a score computed locally. The
// current conversation binding is persisted and the recorded generation is
// exported before the evaluation is queued, so the evaluator can read the
// conversation it is asked to grade. A successful evaluation lets the trial end
// as completed without a local FinalScore. Queuing one also makes the owning
// ExperimentRun leave score counting to the backend, since the evaluator writes
// a score this process never sees.
//
// Options.Timeout and ctx both bound the wait. Worker failure returns
// *TrialEvaluationFailedError, an exceeded deadline returns
// *TrialEvaluationTimeoutError, and a cancelled context returns ctx.Err(). A
// transport error while polling propagates and abandons the wait; the evaluation
// keeps running server-side, and triggering it again returns the same row.
//
// Only a trial created by ExperimentRun.Trial can mark its run, so a caller who
// built this trial with NewTrial or NewTrialFromRef must leave
// CompleteExperimentOptions.ScoreCount unset when finalizing the run itself.
//
// Experimental: Config.EnableExperimentalFeatures takes precedence over
// AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES. A closed gate returns
// ErrExperimentalFeatureDisabled before any trial state or generation is sent.
// This call can change or be removed in any release.
func (t *Trial) Evaluate(ctx context.Context, evaluatorID string, options ...EvaluateOptions) (*TrialEvaluation, error) {
	if t == nil || t.client == nil {
		return nil, ErrNilClient
	}
	// Checked before the trial is created and the generation is flushed, so a
	// blocked call leaves nothing behind.
	if err := t.client.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts := EvaluateOptions{}
	if len(options) > 0 {
		opts = options[0]
	}
	evaluatorID = strings.TrimSpace(evaluatorID)
	if evaluatorID == "" {
		return nil, fmt.Errorf("%w: evaluator_id is required", ErrExperimentValidationFailed)
	}
	timeout, pollInterval, err := opts.resolve()
	if err != nil {
		return nil, err
	}
	if t.conversationID == "" {
		return nil, fmt.Errorf("%w: bind a conversation before evaluating a trial", ErrExperimentValidationFailed)
	}

	if err := t.createTrial(ctx); err != nil {
		return nil, evaluationRequestError(ctx, err)
	}
	// BindConversation and RecordIO are local until now, and the backend rejects
	// an evaluation for a trial with no stored conversation.
	if _, err := t.client.UpdateTrial(ctx, t.ref.RunID, t.trialID, UpdateTrialRequest{
		ConversationID: t.conversationID,
	}); err != nil {
		return nil, evaluationRequestError(ctx, err)
	}
	// The evaluator reads the stored conversation, so the generation has to exist
	// before the wait starts, not when the trial ends.
	if err := t.ensureGeneration(ctx); err != nil {
		return nil, evaluationRequestError(ctx, err)
	}
	if err := t.client.Flush(ctx); err != nil {
		return nil, evaluationRequestError(ctx, err)
	}

	deadline := time.Now().Add(timeout)
	evaluation, err := t.client.TriggerTrialEvaluation(ctx, t.ref.RunID, t.trialID, TriggerTrialEvaluationRequest{
		EvaluatorID: evaluatorID, EvaluatorVersion: opts.EvaluatorVersion,
	})
	if err != nil {
		return nil, evaluationRequestError(ctx, err)
	}
	// The evaluation row exists from here on, and the score it writes counts toward
	// the run's stored total whether or not this wait sees it finish. Marking the
	// run only on success would leave a timed-out wait asserting a stale count.
	t.experiment.markCloudEvaluated()
	// Back off so a long wait costs tens of status reads, not hundreds. A caller
	// who asked for a slower cadence than the cap keeps it.
	interval := pollInterval
	maxInterval := max(pollInterval, maxEvaluationPollInterval)
	for {
		switch evaluation.Status {
		case TrialEvaluationStatusSuccess:
			// End owns the trial's terminal state; a stored evaluator's verdict is not
			// a local one. This only records that the score exists server-side.
			t.cloudEvaluated = true
			return evaluation, nil
		case TrialEvaluationStatusFailed:
			return nil, &TrialEvaluationFailedError{
				EvaluationID: evaluation.EvaluationID, Detail: evaluation.Error,
			}
		case TrialEvaluationStatusQueued, TrialEvaluationStatusClaimed:
		default:
			// Unreachable: Client.TriggerTrialEvaluation and Client.GetTrialEvaluation
			// reject a status the SDK does not know.
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return nil, evaluationTimeoutError(evaluation.EvaluationID, timeout)
		}
		if err := sleepBackoff(ctx, min(interval, remaining)); err != nil {
			return nil, err
		}
		interval = min(interval*2, maxInterval)
		// Every sleep is followed by a status read, including the one clamped to the
		// remaining budget, so an evaluation that finishes in the last window is not
		// reported as a timeout.
		evaluation, err = t.client.GetTrialEvaluation(ctx, t.ref.RunID, t.trialID, evaluation.EvaluationID)
		if err != nil {
			return nil, evaluationRequestError(ctx, err)
		}
	}
}

// evaluationRequestError keeps cancellation a single error shape. The eval
// transport reports a cancelled request as ErrExperimentTransportFailed with the
// context error only in its message, so errors.Is(err, context.Canceled) would
// not hold for a caller who cancelled the wait.
func evaluationRequestError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return err
}

func evaluationTimeoutError(evaluationID string, timeout time.Duration) error {
	return &TrialEvaluationTimeoutError{
		EvaluationID: evaluationID,
		Detail:       fmt.Sprintf("waited %s", timeout),
	}
}

func (t *Trial) BindTrace(traceID, spanID string) *Trial {
	t.traceID = strings.TrimSpace(traceID)
	t.spanID = strings.TrimSpace(spanID)
	return t
}

func (t *Trial) BindConversation(conversationID string) *Trial {
	t.conversationID = strings.TrimSpace(conversationID)
	return t
}

func (t *Trial) BindGeneration(generationID, conversationID string) *Trial {
	generationID = strings.TrimSpace(generationID)
	if generationID != "" {
		t.generationID = generationID
		t.generationBound = true
		t.hasGeneration = true
	}
	if strings.TrimSpace(conversationID) != "" {
		t.conversationID = strings.TrimSpace(conversationID)
	}
	return t
}

type RecordIOOptions struct {
	Input         any
	Output        any
	ModelProvider string
	ModelName     string
	AgentName     string
	InputTokens   *int
	OutputTokens  *int
}

func (t *Trial) RecordIO(opts RecordIOOptions) *Trial {
	if opts.Input != nil {
		t.io["input_text"] = fmt.Sprint(opts.Input)
	}
	if opts.Output != nil {
		t.io["output_text"] = fmt.Sprint(opts.Output)
	}
	if opts.ModelProvider != "" {
		t.io["model_provider"] = opts.ModelProvider
	}
	if opts.ModelName != "" {
		t.io["model_name"] = opts.ModelName
	}
	if opts.AgentName != "" {
		t.io["agent_name"] = opts.AgentName
	}
	if opts.InputTokens != nil {
		t.io["input_tokens"] = *opts.InputTokens
		t.usage["input_tokens"] = *opts.InputTokens
	}
	if opts.OutputTokens != nil {
		t.io["output_tokens"] = *opts.OutputTokens
		t.usage["output_tokens"] = *opts.OutputTokens
	}
	if t.hasRecordedGenerationData() {
		t.hasGeneration = true
		if t.conversationID == "" {
			t.conversationID = StableID("conv", t.ref.RunID, t.ref.TestCaseID, t.ref.Attempt)
		}
	}
	return t
}

func (t *Trial) hasRecordedGenerationData() bool {
	if t == nil {
		return false
	}
	_, hasInput := t.io["input_text"]
	_, hasOutput := t.io["output_text"]
	_, hasInputTokens := t.io["input_tokens"]
	_, hasOutputTokens := t.io["output_tokens"]
	return hasInput || hasOutput || hasInputTokens || hasOutputTokens
}

func (t *Trial) SetUsage(inputTokens, outputTokens *int, cost *float64) *Trial {
	if inputTokens != nil {
		t.usage["input_tokens"] = *inputTokens
	}
	if outputTokens != nil {
		t.usage["output_tokens"] = *outputTokens
	}
	if cost != nil {
		t.usage["cost"] = *cost
	}
	return t
}

type ScoreOptions struct {
	Evaluator            *Evaluator
	Passed               *bool
	Explanation          string
	GenerationID         string
	GraderConversationID string
	GraderGenerationID   string
	GraderTraceID        string
	Metadata             map[string]any
}

func (t *Trial) Score(scoreKey string, value ScoreValue, opts ScoreOptions) ScoreItem {
	if scoreKey == "final" {
		opts.Passed = inferFinalPassed(value, opts.Passed)
	}
	ev := t.defaultEvaluator
	if opts.Evaluator != nil {
		ev = opts.Evaluator.normalized()
	}
	generationID := strings.TrimSpace(opts.GenerationID)
	if generationID == "" && t.hasGeneration {
		generationID = t.generationID
	}
	scoreID := StableID("score", t.ref.RunID, t.trialID, scoreKey, generationID, ev.EvaluatorID, ev.Version)
	metadata := map[string]any{}
	maps.Copy(metadata, t.metadata)
	maps.Copy(metadata, opts.Metadata)
	metadata["task_id"] = t.ref.TestCaseID
	metadata["trial_id"] = t.trialID
	metadata["attempt"] = t.ref.Attempt
	item := ScoreItem{
		ScoreID:              scoreID,
		EvaluatorID:          ev.EvaluatorID,
		EvaluatorVersion:     ev.Version,
		EvaluatorKind:        string(ev.Kind),
		ScoreKey:             scoreKey,
		Value:                value,
		GenerationID:         generationID,
		TrialID:              t.trialID,
		ConversationID:       t.conversationID,
		TraceID:              t.traceID,
		SpanID:               t.spanID,
		RunID:                t.ref.RunID,
		TestCaseID:           t.ref.TestCaseID,
		GraderConversationID: opts.GraderConversationID,
		GraderGenerationID:   opts.GraderGenerationID,
		GraderTraceID:        opts.GraderTraceID,
		Passed:               opts.Passed,
		Explanation:          opts.Explanation,
		Metadata:             metadata,
		Source:               &ScoreSource{Kind: "experiment", ID: t.ref.RunID},
	}
	t.buffer = append(t.buffer, item)
	if scoreKey == "final" {
		t.hasFinal = true
		t.finalPassed = opts.Passed
	}
	return item
}

// FinalScore records the headline score. Boolean scores infer the trial verdict
// from the value; numeric and string scores require ScoreOptions.Passed.
func (t *Trial) FinalScore(value ScoreValue, opts ScoreOptions) ScoreItem {
	opts.Passed = inferFinalPassed(value, opts.Passed)
	return t.Score("final", value, opts)
}

func inferFinalPassed(value ScoreValue, passed *bool) *bool {
	if passed == nil && value.Bool != nil {
		passed := *value.Bool
		return &passed
	}
	return passed
}

func (t *Trial) CheckScore(name string, passed bool, opts ScoreOptions) ScoreItem {
	if opts.Evaluator == nil {
		ev := Evaluator{
			EvaluatorID: t.defaultEvaluator.EvaluatorID + "." + name,
			Version:     t.defaultEvaluator.Version,
			Kind:        EvaluatorKindDeterministic,
		}
		opts.Evaluator = &ev
	}
	opts.Passed = &passed
	return t.Score(name, BoolScoreValue(passed), opts)
}

func (t *Trial) RubricScore(name string, value ScoreValue, opts ScoreOptions) ScoreItem {
	if opts.Evaluator == nil {
		ev := Evaluator{
			EvaluatorID: t.defaultEvaluator.EvaluatorID + "." + name,
			Version:     t.defaultEvaluator.Version,
			Kind:        EvaluatorKindLLMJudge,
		}
		opts.Evaluator = &ev
	}
	return t.Score(name, value, opts)
}

type ArtifactOptions struct {
	Name    string
	Kind    string
	MIME    string
	Content []byte
	Data    any
	Text    string
}

func (t *Trial) Artifact(ctx context.Context, opts ArtifactOptions) (*TrialArtifact, error) {
	if t == nil || t.client == nil {
		return nil, ErrNilClient
	}
	content := opts.Content
	kind := opts.Kind
	mime := opts.MIME
	if len(content) == 0 && opts.Data != nil {
		raw, err := json.Marshal(opts.Data)
		if err != nil {
			return nil, err
		}
		content = raw
		if kind == "" {
			kind = "json"
		}
		if mime == "" {
			mime = "application/json"
		}
	}
	if len(content) == 0 && opts.Text != "" {
		content = []byte(opts.Text)
		if kind == "" {
			kind = "text"
		}
		if mime == "" {
			mime = "text/plain"
		}
	}
	record, err := t.client.UploadTrialArtifact(ctx, t.ref.RunID, t.trialID, TrialArtifactUpload{
		Name:    opts.Name,
		Kind:    kind,
		MIME:    mime,
		Content: content,
	})
	if err != nil {
		return nil, err
	}
	artifactID := ""
	if record != nil {
		artifactID = record.ArtifactID
	}
	t.artifacts = append(t.artifacts, map[string]any{"name": opts.Name, "kind": kind, "artifact_id": artifactID})
	return record, nil
}

func (t *Trial) Succeed() *Trial {
	t.status = TrialStatusPassed
	t.flushFailed = false
	return t
}

func (t *Trial) Fail(errorText string) *Trial {
	t.status = TrialStatusFailed
	t.flushFailed = false
	if errorText != "" {
		t.errorText = errorText
	}
	return t
}

func (t *Trial) ensureGeneration(ctx context.Context) error {
	if t.generationExported || !t.hasRecordedGenerationData() {
		return nil
	}
	generation := t.recordedGeneration()
	if t.client.hasRecordedGenerationID(t.generationID) {
		if t.client.hasRecordedGenerationIO(t.generationID, generationIOFingerprint(generation)) {
			if err := t.client.Flush(ctx); err != nil {
				return err
			}
			t.generationExported = true
			return nil
		}
		if err := t.client.Flush(ctx); err != nil {
			return err
		}
	}
	ctx, recorder := t.client.StartGeneration(ctx, t.recordedGenerationStart(generation))
	recorder.SetResult(generation, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		return err
	}
	if err := t.client.Flush(ctx); err != nil {
		return err
	}
	t.generationExported = true
	return nil
}

func (t *Trial) recordedGenerationStart(generation Generation) GenerationStart {
	return GenerationStart{
		ID:             generation.ID,
		ConversationID: generation.ConversationID,
		Model:          generation.Model,
		AgentName:      generation.AgentName,
		OperationName:  "invoke_agent",
		Tags:           map[string]string{"experiment.run_id": t.ref.RunID, "task_id": t.ref.TestCaseID},
		Metadata: map[string]any{
			"experiment_run_id": t.ref.RunID,
			"task_id":           t.ref.TestCaseID,
			"trial_id":          t.trialID,
			"attempt":           t.ref.Attempt,
		},
	}
}

func (t *Trial) recordedGeneration() Generation {
	caseInput := ""
	if t.experiment != nil && t.experiment.suite != nil {
		if tc, ok := t.experiment.suite.Case(t.ref.TestCaseID); ok && tc.Input != nil {
			caseInput = fmt.Sprint(tc.Input)
		}
	}
	provider := firstNonBlank(firstString(t.io["model_provider"]), candidateModelProvider(t.candidate), "eval")
	model := firstNonBlank(firstString(t.io["model_name"]), candidateModelName(t.candidate), "experiment")
	agentName := firstNonBlank(firstString(t.io["agent_name"]), candidateAgentName(t.candidate))
	usage := TokenUsage{}
	if v, ok := t.io["input_tokens"].(int); ok {
		usage.InputTokens = int64(v)
	}
	if v, ok := t.io["output_tokens"].(int); ok {
		usage.OutputTokens = int64(v)
	}
	return Generation{
		ID:             t.generationID,
		ConversationID: t.conversationID,
		Model:          ModelRef{Provider: provider, Name: model},
		AgentName:      agentName,
		Input:          textMessages(RoleUser, firstNonBlank(firstString(t.io["input_text"]), caseInput)),
		Output:         textMessages(RoleAssistant, firstString(t.io["output_text"])),
		Usage:          usage,
	}
}

func (t *Trial) Flush(ctx context.Context) (int, error) {
	if t == nil || t.client == nil {
		return 0, ErrNilClient
	}
	if len(t.buffer) == 0 {
		if err := t.ensureGeneration(ctx); err != nil {
			return 0, err
		}
		if err := t.client.Flush(ctx); err != nil {
			return 0, err
		}
		return 0, nil
	}
	if err := t.ensureGeneration(ctx); err != nil {
		return 0, err
	}
	if err := t.client.Flush(ctx); err != nil {
		return 0, err
	}
	pending := append([]ScoreItem(nil), t.buffer...)
	response, err := t.client.ExportScores(ctx, pending)
	if err != nil {
		return 0, err
	}
	accepted, err := acceptedOrError(response)
	if err != nil {
		return 0, err
	}
	t.buffer = t.buffer[len(pending):]
	t.accepted += accepted
	if t.experiment != nil {
		t.experiment.recordAccepted(accepted)
	}
	return accepted, nil
}

func (t *Trial) AcceptedScores() int {
	if t == nil {
		return 0
	}
	return t.accepted
}
