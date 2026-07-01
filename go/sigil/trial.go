package sigil

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
	io                 map[string]any
	trialCreated       bool
	usage              map[string]any
	started            time.Time

	buffer      []ScoreItem
	accepted    int
	hasFinal    bool
	finalPassed *bool
	artifacts   []map[string]any
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
		return nil, fmt.Errorf("%w: trial ref is required; set SIGIL_EXPERIMENT_ID and SIGIL_TEST_CASE_ID", ErrExperimentValidationFailed)
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
	if err != nil {
		t.status = TrialStatusErrored
		t.errorText = err.Error()
	} else if t.status == TrialStatusRunning {
		if t.hasFinal {
			if t.finalPassed != nil && *t.finalPassed {
				t.status = TrialStatusPassed
			} else {
				t.status = TrialStatusFailed
			}
		} else {
			t.status = TrialStatusFailed
			t.errorText = "trial exited without a final score"
		}
	}
	cleanupCtx, cancel := experimentCleanupContext(ctx)
	defer cancel()
	if err := t.createTrial(cleanupCtx); err != nil {
		return err
	}
	_, flushErr := t.Flush(cleanupCtx)
	if flushErr != nil {
		t.status = TrialStatusErrored
		t.errorText = flushErr.Error()
		if finalizeErr := t.finalizeTrial(cleanupCtx); finalizeErr != nil {
			return errors.Join(flushErr, fmt.Errorf("finalize trial %q: %w", t.trialID, finalizeErr))
		}
		return flushErr
	}
	return t.finalizeTrial(cleanupCtx)
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
	scoreID := StableID("score", t.ref.RunID, t.trialID, scoreKey, ev.EvaluatorID)
	metadata := map[string]any{}
	maps.Copy(metadata, t.metadata)
	maps.Copy(metadata, opts.Metadata)
	metadata["task_id"] = t.ref.TestCaseID
	metadata["trial_id"] = t.trialID
	metadata["attempt"] = t.ref.Attempt
	generationID := strings.TrimSpace(opts.GenerationID)
	if generationID == "" && t.hasGeneration {
		generationID = t.generationID
	}
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
	return t
}

func (t *Trial) Fail(errorText string) *Trial {
	t.status = TrialStatusFailed
	if errorText != "" {
		t.errorText = errorText
	}
	return t
}

func (t *Trial) ensureGeneration(ctx context.Context) error {
	if t.generationExported || !t.hasRecordedGenerationData() {
		return nil
	}
	if t.client.hasRecordedGenerationID(t.generationID) {
		if err := t.client.Flush(ctx); err != nil {
			return err
		}
		t.generationExported = true
		return nil
	}
	caseInput := ""
	if t.experiment != nil && t.experiment.suite != nil {
		if tc, ok := t.experiment.suite.Case(t.ref.TestCaseID); ok && tc.Input != nil {
			caseInput = fmt.Sprint(tc.Input)
		}
	}
	provider := firstNonBlank(firstString(t.io["model_provider"]), candidateModelProvider(t.candidate), "eval")
	model := firstNonBlank(firstString(t.io["model_name"]), candidateModelName(t.candidate), "experiment")
	agentName := firstNonBlank(firstString(t.io["agent_name"]), candidateAgentName(t.candidate))
	ctx, recorder := t.client.StartGeneration(ctx, GenerationStart{
		ID:             t.generationID,
		ConversationID: t.conversationID,
		Model:          ModelRef{Provider: provider, Name: model},
		AgentName:      agentName,
		OperationName:  "invoke_agent",
		Tags:           map[string]string{"experiment.run_id": t.ref.RunID, "task_id": t.ref.TestCaseID},
		Metadata: map[string]any{
			"experiment_run_id": t.ref.RunID,
			"task_id":           t.ref.TestCaseID,
			"trial_id":          t.trialID,
			"attempt":           t.ref.Attempt,
		},
	})
	usage := TokenUsage{}
	if v, ok := t.io["input_tokens"].(int); ok {
		usage.InputTokens = int64(v)
	}
	if v, ok := t.io["output_tokens"].(int); ok {
		usage.OutputTokens = int64(v)
	}
	recorder.SetResult(Generation{
		ID:             t.generationID,
		ConversationID: t.conversationID,
		Model:          ModelRef{Provider: provider, Name: model},
		AgentName:      agentName,
		Input:          textMessages(RoleUser, firstNonBlank(firstString(t.io["input_text"]), caseInput)),
		Output:         textMessages(RoleAssistant, firstString(t.io["output_text"])),
		Usage:          usage,
	}, nil)
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

func (t *Trial) Flush(ctx context.Context) (int, error) {
	if t == nil || t.client == nil {
		return 0, ErrNilClient
	}
	if len(t.buffer) == 0 {
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
