package experiments

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agento11y "github.com/grafana/agento11y/go/agento11y"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

type ExperimentOptions struct {
	ExperimentID        string
	RunID               string
	Name                string
	Suite               *TestSuite
	Candidate           *Candidate
	DefaultEvaluator    *Evaluator
	Description         string
	Tags                []string
	Metadata            map[string]any
	PlannedTrialCount   *int
	AutoFinalize        *bool
	UseExperimentalOTel *bool
}

type Experiment struct {
	client           *Client
	ExperimentID     string
	RunID            string
	Name             string
	Suite            *TestSuite
	Candidate        *Candidate
	defaultEvaluator Evaluator
	description      string
	tags             []string
	metadata         map[string]any
	plannedCount     *int
	autoFinalize     bool
	useOTel          bool

	mu        sync.Mutex
	status    ExperimentStatus
	accepted  int
	finalized bool
	open      map[string]*Trial
	claimed   map[string]struct{}
}

func NewExperiment(client *Client, opts ExperimentOptions) (*Experiment, error) {
	if client == nil {
		return nil, agento11y.ErrNilClient
	}
	if opts.PlannedTrialCount != nil && *opts.PlannedTrialCount < 0 {
		return nil, errors.New("planned trial count must be non-negative")
	}
	experimentID := firstNonBlank(opts.ExperimentID, opts.RunID)
	if experimentID == "" {
		experimentID = StableID("exp", opts.Name, randomIdentity())
	}
	autoFinalize := true
	if opts.AutoFinalize != nil {
		autoFinalize = *opts.AutoFinalize
	}
	useOTel := client.useExperimentalOTel
	if opts.UseExperimentalOTel != nil {
		useOTel = *opts.UseExperimentalOTel
	}
	evaluator := Evaluator{EvaluatorID: "sdk", Version: "0", Kind: EvaluatorKindCustom}
	if opts.DefaultEvaluator != nil {
		evaluator = opts.DefaultEvaluator.normalized()
	}
	return &Experiment{
		client: client, ExperimentID: experimentID, RunID: experimentID,
		Name: firstNonBlank(opts.Name, experimentID), Suite: cloneSuite(opts.Suite),
		Candidate: cloneCandidate(opts.Candidate), defaultEvaluator: evaluator,
		description: opts.Description, tags: append([]string(nil), opts.Tags...),
		metadata: cloneMap(opts.Metadata), plannedCount: cloneInt(opts.PlannedTrialCount),
		autoFinalize: autoFinalize, useOTel: useOTel, status: ExperimentStatusRunning,
		open: map[string]*Trial{}, claimed: map[string]struct{}{},
	}, nil
}

func (e *Experiment) Enter(ctx context.Context) error {
	if e == nil || e.client == nil {
		return agento11y.ErrNilClient
	}
	metadata := cloneMap(e.metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	var suiteID, suiteVersion string
	if e.Suite != nil {
		suiteID, suiteVersion = e.Suite.SuiteID, e.Suite.Version
		metadata["suite_id"], metadata["suite_version"] = suiteID, suiteVersion
	}
	candidate := map[string]any{}
	if e.Candidate != nil {
		candidate = e.Candidate.AsMetadata()
		maps.Copy(metadata, candidate)
	}
	_, err := e.client.UpsertExperiment(ctx, agento11y.CreateExperimentRequest{
		RunID: e.ExperimentID, Name: e.Name, Source: agento11y.ExperimentSourceExternal,
		Description: e.description, Tags: append([]string(nil), e.tags...),
		SuiteID: suiteID, SuiteVersion: suiteVersion, Candidate: candidate,
		PlannedTrialCount: cloneInt(e.plannedCount), Metadata: metadata,
	})
	return err
}

func WithExperiment(ctx context.Context, client *Client, opts ExperimentOptions, fn func(context.Context, *Experiment) error) (experiment *Experiment, err error) {
	experiment, err = NewExperiment(client, opts)
	if err != nil {
		return experiment, err
	}
	if err = experiment.Enter(ctx); err != nil {
		return experiment, err
	}
	defer func() {
		recovered := recover()
		if recovered != nil {
			if experiment.autoFinalize {
				cleanup, cancel := cleanupContext(ctx)
				_ = experiment.Finalize(cleanup, ExperimentStatusFailed, FinalizeOptions{Error: fmt.Sprint(recovered)})
				cancel()
			}
			panic(recovered)
		}
	}()
	err = fn(ctx, experiment)
	if !experiment.autoFinalize {
		return experiment, err
	}
	cleanup, cancel := cleanupContext(ctx)
	defer cancel()
	if err != nil {
		finalizeErr := experiment.Finalize(cleanup, ExperimentStatusFailed, FinalizeOptions{Error: err.Error()})
		return experiment, errors.Join(err, finalizeErr)
	}
	return experiment, experiment.Finalize(cleanup, ExperimentStatusCompleted)
}

type ExperimentFromSuiteOptions struct {
	Version             string
	TestSuitesClient    *TestSuitesClient
	ServiceAccountToken string
	ControlEndpoint     string
	GrafanaURL          string
	Experiment          ExperimentOptions
}

func NewExperimentFromSuite(ctx context.Context, client *Client, suiteID string, opts ExperimentFromSuiteOptions) (*Experiment, error) {
	suites := opts.TestSuitesClient
	if suites == nil {
		var err error
		suites, err = NewTestSuitesClient(TestSuitesClientOptions{
			GrafanaURL: opts.GrafanaURL, ServiceAccountToken: opts.ServiceAccountToken,
			ControlEndpoint: opts.ControlEndpoint,
		})
		if err != nil {
			return nil, err
		}
	}
	suite, err := suites.PullSuite(ctx, suiteID, firstNonBlank(opts.Version, "latest_published"))
	if err != nil {
		return nil, err
	}
	experimentOpts := opts.Experiment
	experimentOpts.Suite = suite
	if experimentOpts.Name == "" {
		experimentOpts.Name = firstNonBlank(suite.Name, suite.SuiteID) + " experiment"
	}
	return NewExperiment(client, experimentOpts)
}

func WithExperimentFromSuite(ctx context.Context, client *Client, suiteID string, opts ExperimentFromSuiteOptions, fn func(context.Context, *Experiment) error) (*Experiment, error) {
	experiment, err := NewExperimentFromSuite(ctx, client, suiteID, opts)
	if err != nil {
		return nil, err
	}
	return WithExperiment(ctx, client, ExperimentOptions{
		ExperimentID: experiment.ExperimentID, Name: experiment.Name, Suite: experiment.Suite,
		Candidate: experiment.Candidate, DefaultEvaluator: &experiment.defaultEvaluator,
		Description: experiment.description, Tags: experiment.tags, Metadata: experiment.metadata,
		PlannedTrialCount: experiment.plannedCount, AutoFinalize: &experiment.autoFinalize,
		UseExperimentalOTel: &experiment.useOTel,
	}, fn)
}

type TrialOptions struct {
	Attempt      int
	TrajectoryID string
	Metadata     map[string]any
}

func (e *Experiment) NewTrial(testCase TestCase, options ...TrialOptions) (*Trial, error) {
	return e.newTrial(testCase.TestCaseID, &testCase, options...)
}

func (e *Experiment) NewTrialByCaseID(testCaseID string, options ...TrialOptions) (*Trial, error) {
	var testCase *TestCase
	if e != nil && e.Suite != nil {
		if found, ok := e.Suite.Case(testCaseID); ok {
			testCase = &found
		}
	}
	return e.newTrial(testCaseID, testCase, options...)
}

// NewTrialID is an alias for NewTrialByCaseID.
func (e *Experiment) NewTrialID(testCaseID string, options ...TrialOptions) (*Trial, error) {
	return e.NewTrialByCaseID(testCaseID, options...)
}

func (e *Experiment) newTrial(testCaseID string, testCase *TestCase, options ...TrialOptions) (*Trial, error) {
	if e == nil || e.client == nil {
		return nil, agento11y.ErrNilClient
	}
	opts := TrialOptions{Attempt: 1}
	if len(options) > 0 {
		opts = options[0]
		if opts.Attempt <= 0 {
			opts.Attempt = 1
		}
	}
	testCaseID = strings.TrimSpace(testCaseID)
	if testCaseID == "" {
		return nil, errors.New("test case ID is required")
	}
	testCaseName := testCaseID
	if testCase != nil {
		testCaseName = firstNonBlank(testCase.Name, testCaseID)
	}
	ref := TrialRef{
		ExperimentID: e.ExperimentID, RunID: e.ExperimentID,
		TestCaseID: testCaseID, Attempt: opts.Attempt,
		SuiteName: e.Name, TestCaseName: testCaseName, TrajectoryID: opts.TrajectoryID,
	}
	if e.Suite != nil {
		ref.SuiteID, ref.SuiteVersion = e.Suite.SuiteID, e.Suite.Version
		ref.SuiteName = firstNonBlank(e.Suite.Name, e.Name)
	}
	trialID := StableID("trial", ref.ExperimentID, ref.TestCaseID, ref.Attempt)
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, exists := e.claimed[trialID]; exists {
		return nil, fmt.Errorf("trial for test case %q attempt %d already exists; increment attempt for a retry", testCaseID, opts.Attempt)
	}
	trial := newTrial(e.client, ref, e, testCase, e.Candidate, &e.defaultEvaluator, opts.Metadata, e.useOTel)
	e.claimed[trialID] = struct{}{}
	e.open[trialID] = trial
	return trial, nil
}

func (e *Experiment) WithTrial(ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error, options ...TrialOptions) (err error) {
	trial, err := e.NewTrial(testCase, options...)
	if err != nil {
		return err
	}
	return withTrial(ctx, trial, fn)
}

func (e *Experiment) WithTrialByCaseID(ctx context.Context, testCaseID string, fn func(context.Context, *Trial) error, options ...TrialOptions) (err error) {
	trial, err := e.NewTrialByCaseID(testCaseID, options...)
	if err != nil {
		return err
	}
	return withTrial(ctx, trial, fn)
}

func withTrial(ctx context.Context, trial *Trial, fn func(context.Context, *Trial) error) (err error) {
	if fn == nil {
		return errors.New("trial callback is required")
	}
	if err := trial.Enter(ctx); err != nil {
		trial.abandon(err)
		return err
	}
	ctx = trial.Context(ctx)
	defer func() {
		recovered := recover()
		callbackErr := err
		if recovered != nil {
			callbackErr = fmt.Errorf("trial callback panic: %v", recovered)
		}
		cleanup, cancel := cleanupContext(ctx)
		closeErr := trial.Close(cleanup, callbackErr)
		cancel()
		err = errors.Join(err, closeErr)
		if recovered != nil {
			panic(recovered)
		}
	}()
	return fn(ctx, trial)
}

type FinalizeOptions struct {
	Error      string
	ScoreCount *int
}

func (e *Experiment) Finalize(ctx context.Context, status ExperimentStatus, optional ...FinalizeOptions) error {
	if e == nil || e.client == nil {
		return agento11y.ErrNilClient
	}
	opts := FinalizeOptions{}
	if len(optional) > 0 {
		opts = optional[0]
	}
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	e.mu.Lock()
	if e.finalized {
		e.mu.Unlock()
		return nil
	}
	open := make([]*Trial, 0, len(e.open))
	for _, trial := range e.open {
		open = append(open, trial)
	}
	e.mu.Unlock()
	var closeErrors []error
	for _, trial := range open {
		if err := trial.Close(ctx, nil); err != nil {
			closeErrors = append(closeErrors, err)
		}
	}
	if len(closeErrors) > 0 {
		status = ExperimentStatusFailed
		opts.ScoreCount = nil
		opts.Error = strings.Trim(strings.Join([]string{opts.Error, "trial close failed: " + errors.Join(closeErrors...).Error()}, "; "), "; ")
	}
	if err := e.client.Flush(ctx); err != nil {
		return errors.Join(errors.Join(closeErrors...), err)
	}
	if _, err := e.client.Finalize(ctx, e.ExperimentID, status, opts.ScoreCount, opts.Error); err != nil {
		return errors.Join(errors.Join(closeErrors...), err)
	}
	e.mu.Lock()
	e.status, e.finalized = status, true
	e.mu.Unlock()
	return errors.Join(closeErrors...)
}

func (e *Experiment) Report(ctx context.Context) (*ExperimentReport, error) {
	if e == nil || e.client == nil {
		return nil, agento11y.ErrNilClient
	}
	return e.client.GetReport(ctx, e.ExperimentID)
}

func (e *Experiment) URL() string {
	if e == nil || e.client == nil {
		return ""
	}
	return e.client.ExperimentURL(e.ExperimentID)
}

func (e *Experiment) AcceptedScores() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.accepted
}

func (e *Experiment) Status() ExperimentStatus {
	if e == nil {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status
}

func (e *Experiment) trialClosed(t *Trial) {
	e.mu.Lock()
	delete(e.open, t.TrialID)
	e.mu.Unlock()
}

func (e *Experiment) trialAbandoned(t *Trial) {
	e.mu.Lock()
	delete(e.open, t.TrialID)
	delete(e.claimed, t.TrialID)
	e.mu.Unlock()
}

func (e *Experiment) recordAccepted(count int) {
	e.mu.Lock()
	e.accepted += count
	e.mu.Unlock()
}

type Trial struct {
	client     *Client
	experiment *Experiment
	testCase   *TestCase
	candidate  *Candidate
	evaluator  Evaluator
	metadata   map[string]any
	useOTel    bool

	Ref            TrialRef
	TrialID        string
	GenerationID   string
	Status         TrialStatus
	ConversationID string
	TraceID        string
	SpanID         string
	Error          string

	mu                 sync.Mutex
	started            time.Time
	created            bool
	closed             bool
	generationBound    bool
	generationExported bool
	hasGeneration      bool
	io                 map[string]any
	usage              map[string]any
	buffer             []ScoreItem
	accepted           int
	hasFinal           bool
	finalPassed        *bool
	occurrences        map[string]int
	artifacts          []ExperimentArtifactRef
	span               trace.Span
	spanContext        context.Context
}

func NewTrial(client *Client, ref TrialRef, options ...TrialOptions) (*Trial, error) {
	if client == nil {
		return nil, agento11y.ErrNilClient
	}
	if len(options) > 0 && options[0].Attempt > 0 {
		ref.Attempt = options[0].Attempt
	}
	ref = ref.normalized()
	if ref.ExperimentID == "" || ref.TestCaseID == "" {
		return nil, errors.New("trial ref requires experiment ID and test case ID")
	}
	metadata := map[string]any(nil)
	if len(options) > 0 {
		metadata = options[0].Metadata
	}
	return newTrial(client, ref, nil, nil, nil, nil, metadata, client.useExperimentalOTel), nil
}

func NewTrialFromRef(client *Client, ref *TrialRef, options ...TrialOptions) (*Trial, error) {
	if ref == nil {
		return nil, errors.New("trial ref is required; set AGENTO11Y_EXPERIMENT_ID and AGENTO11Y_TEST_CASE_ID")
	}
	return NewTrial(client, *ref, options...)
}

func newTrial(client *Client, ref TrialRef, experiment *Experiment, testCase *TestCase, candidate *Candidate, evaluator *Evaluator, metadata map[string]any, useOTel bool) *Trial {
	ref = ref.normalized()
	defaultEvaluator := Evaluator{EvaluatorID: "sdk", Version: "0", Kind: EvaluatorKindCustom}
	if evaluator != nil {
		defaultEvaluator = evaluator.normalized()
	}
	return &Trial{
		client: client, experiment: experiment, testCase: cloneTestCasePtr(testCase),
		candidate: cloneCandidate(candidate), evaluator: defaultEvaluator,
		metadata: cloneMap(metadata), useOTel: useOTel, Ref: ref,
		TrialID:      StableID("trial", ref.ExperimentID, ref.TestCaseID, ref.Attempt),
		GenerationID: StableID("gen", ref.ExperimentID, ref.TestCaseID, ref.Attempt),
		Status:       TrialStatusRunning, io: map[string]any{}, usage: map[string]any{},
		occurrences: map[string]int{},
	}
}

func (t *Trial) Enter(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errors.New("cannot enter a closed trial")
	}
	t.started = time.Now()
	t.mu.Unlock()
	if t.useOTel {
		tracer := otel.Tracer("sigil_sdk.experiments")
		attrs := t.identityAttributes()
		ctx, t.span = tracer.Start(ctx, "eval.trial "+t.Ref.TestCaseID, trace.WithAttributes(attrs...))
		t.spanContext = ctx
		spanContext := trace.SpanContextFromContext(ctx)
		if spanContext.IsValid() {
			t.TraceID, t.SpanID = spanContext.TraceID().String(), spanContext.SpanID().String()
		}
	}
	return t.create(ctx)
}

// Context returns a context carrying the opt-in trial span. Without
// experimental OTel enabled it returns the input unchanged.
func (t *Trial) Context(ctx context.Context) context.Context {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.spanContext != nil {
		return t.spanContext
	}
	return contextOrBackground(ctx)
}

func (t *Trial) create(ctx context.Context) error {
	t.mu.Lock()
	if t.created {
		t.mu.Unlock()
		return nil
	}
	t.mu.Unlock()
	_, err := t.client.UpsertTrial(ctx, t.Ref.ExperimentID, agento11y.UpsertTrialRequest{
		TrialID: t.TrialID, TestCaseID: t.Ref.TestCaseID, Attempt: t.Ref.Attempt,
		Status: string(TrialStatusRunning), ConversationID: t.ConversationID,
		TraceID: t.TraceID, SpanID: t.SpanID, TestCase: t.testCaseSnapshot(),
		Metadata: mapIf(t.Ref.TestCaseName != "", "test_case_name", t.Ref.TestCaseName),
	})
	if err == nil {
		t.mu.Lock()
		t.created = true
		t.mu.Unlock()
	}
	return err
}

func (t *Trial) Close(ctx context.Context, callbackErr error) error {
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	if callbackErr != nil && t.Status == TrialStatusRunning {
		t.Status, t.Error = TrialStatusErrored, callbackErr.Error()
	} else if t.Status == TrialStatusRunning {
		switch {
		case !t.hasFinal:
			t.Status, t.Error = TrialStatusFailed, "trial closed without a final score"
		case t.finalPassed == nil:
			t.Status = TrialStatusCompleted
		case *t.finalPassed:
			t.Status = TrialStatusPassed
		default:
			t.Status = TrialStatusFailed
		}
	}
	t.mu.Unlock()
	if err := t.create(ctx); err != nil {
		t.endSpan(err)
		return err
	}
	if _, err := t.Flush(ctx); err != nil {
		t.endSpan(err)
		return err
	}
	if err := t.finalize(ctx); err != nil {
		t.endSpan(err)
		return err
	}
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.endSpan(nil)
	if t.experiment != nil {
		t.experiment.trialClosed(t)
	}
	return nil
}

func (t *Trial) abandon(err error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.mu.Unlock()
	t.endSpan(err)
	if t.experiment != nil {
		t.experiment.trialAbandoned(t)
	}
}

func (t *Trial) finalize(ctx context.Context) error {
	t.mu.Lock()
	if !t.created {
		t.mu.Unlock()
		return nil
	}
	backendStatus := "completed"
	if t.Status == TrialStatusErrored {
		backendStatus = "failed"
	}
	req := agento11y.UpdateTrialRequest{
		Status: backendStatus, Error: t.Error, ConversationID: t.ConversationID,
		TraceID: t.TraceID, SpanID: t.SpanID,
	}
	if value, ok := t.usage["cost"].(float64); ok {
		req.Cost = &value
	}
	if value, ok := t.usage["input_tokens"].(int); ok {
		req.InputTokens = &value
	}
	if value, ok := t.usage["output_tokens"].(int); ok {
		req.OutputTokens = &value
	}
	if !t.started.IsZero() {
		value := int(time.Since(t.started).Milliseconds())
		req.DurationMillis = &value
	}
	t.mu.Unlock()
	_, err := t.client.UpdateTrial(ctx, t.Ref.ExperimentID, t.TrialID, req)
	return err
}

func (t *Trial) BindTrace(traceID, spanID string) *Trial {
	t.mu.Lock()
	t.TraceID, t.SpanID = strings.TrimSpace(traceID), strings.TrimSpace(spanID)
	t.mu.Unlock()
	return t
}

func (t *Trial) BindConversation(conversationID string) *Trial {
	t.mu.Lock()
	t.ConversationID = strings.TrimSpace(conversationID)
	t.mu.Unlock()
	return t
}

func (t *Trial) BindGeneration(generationID, conversationID string) *Trial {
	t.mu.Lock()
	if strings.TrimSpace(generationID) != "" {
		t.GenerationID = strings.TrimSpace(generationID)
		t.generationBound, t.generationExported, t.hasGeneration = true, true, true
	}
	if strings.TrimSpace(conversationID) != "" {
		t.ConversationID = strings.TrimSpace(conversationID)
	}
	t.mu.Unlock()
	return t
}

type RecordIOOptions struct {
	Input, Output                                     any
	ModelProvider, ModelName, AgentName, AgentVersion string
	InputTokens, OutputTokens                         *int
}

func (t *Trial) RecordIO(opts RecordIOOptions) *Trial {
	t.mu.Lock()
	t.hasGeneration = true
	if t.ConversationID == "" {
		t.ConversationID = StableID("conv", t.Ref.ExperimentID, t.Ref.TestCaseID, t.Ref.Attempt)
	}
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
	if opts.AgentVersion != "" {
		t.io["agent_version"] = opts.AgentVersion
	}
	if opts.InputTokens != nil {
		t.io["input_tokens"], t.usage["input_tokens"] = *opts.InputTokens, *opts.InputTokens
	}
	if opts.OutputTokens != nil {
		t.io["output_tokens"], t.usage["output_tokens"] = *opts.OutputTokens, *opts.OutputTokens
	}
	t.mu.Unlock()
	return t
}

func (t *Trial) SetUsage(inputTokens, outputTokens *int, cost *float64) *Trial {
	t.mu.Lock()
	if inputTokens != nil {
		t.usage["input_tokens"] = *inputTokens
	}
	if outputTokens != nil {
		t.usage["output_tokens"] = *outputTokens
	}
	if cost != nil {
		t.usage["cost"] = *cost
	}
	t.mu.Unlock()
	return t
}

type ScoreOptions struct {
	Evaluator                                                                          *Evaluator
	Passed                                                                             *bool
	Explanation, GenerationID, GraderConversationID, GraderGenerationID, GraderTraceID string
	Metadata                                                                           map[string]any
}

func (t *Trial) Score(scoreKey string, value any, opts ScoreOptions) (ScoreItem, error) {
	return t.score(scoreKey, value, opts, "")
}

func (t *Trial) score(scoreKey string, value any, opts ScoreOptions, scoreID string) (ScoreItem, error) {
	scoreValue, err := coerceScoreValue(value)
	if err != nil {
		return ScoreItem{}, err
	}
	evaluator := t.evaluator
	if opts.Evaluator != nil {
		evaluator = opts.Evaluator.normalized()
	}
	if scoreKey == "final" && opts.Passed == nil && scoreValue.Bool != nil {
		passed := *scoreValue.Bool
		opts.Passed = &passed
	}
	t.mu.Lock()
	if scoreID == "" {
		scoreID = t.nextScoreID(scoreKey, evaluator.EvaluatorID)
	}
	metadata := cloneMap(t.metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	maps.Copy(metadata, opts.Metadata)
	metadata["task_id"], metadata["trial_id"], metadata["attempt"] = t.Ref.TestCaseID, t.TrialID, t.Ref.Attempt
	generationID := strings.TrimSpace(opts.GenerationID)
	if generationID == "" && t.hasGeneration {
		generationID = t.GenerationID
	}
	item := ScoreItem{
		ScoreID: scoreID, EvaluatorID: evaluator.EvaluatorID, EvaluatorVersion: evaluator.Version,
		EvaluatorKind: string(evaluator.Kind), ScoreKey: scoreKey, Value: scoreValue,
		GenerationID: generationID, TrialID: t.TrialID, ConversationID: t.ConversationID,
		TraceID: t.TraceID, SpanID: t.SpanID, RunID: t.Ref.ExperimentID,
		TestCaseID: t.Ref.TestCaseID, GraderConversationID: opts.GraderConversationID,
		GraderGenerationID: opts.GraderGenerationID, GraderTraceID: opts.GraderTraceID,
		Passed: opts.Passed, Explanation: opts.Explanation, Metadata: metadata,
		Source: &ScoreSource{Kind: "experiment", ID: t.Ref.ExperimentID},
	}
	t.buffer = append(t.buffer, item)
	if scoreKey == "final" {
		t.hasFinal, t.finalPassed = true, opts.Passed
	}
	t.emitEvaluationEvent(scoreKey, scoreValue, evaluator, opts)
	t.mu.Unlock()
	return item, nil
}

func (t *Trial) nextScoreID(scoreKey, evaluatorID string) string {
	key := scoreKey + "\x1f" + evaluatorID
	occurrence := t.occurrences[key]
	t.occurrences[key] = occurrence + 1
	if occurrence == 0 {
		return StableID("score", t.Ref.ExperimentID, t.TrialID, scoreKey, evaluatorID)
	}
	return StableID("score", t.Ref.ExperimentID, t.TrialID, scoreKey, evaluatorID, occurrence+1)
}

func (t *Trial) FinalScore(value any, opts ScoreOptions) (ScoreItem, error) {
	return t.Score("final", value, opts)
}

func (t *Trial) CheckScore(name string, passed bool, opts ScoreOptions) (ScoreItem, error) {
	if opts.Evaluator == nil {
		evaluator := Evaluator{EvaluatorID: t.evaluator.EvaluatorID + "." + name, Version: t.evaluator.Version, Kind: EvaluatorKindDeterministic}
		opts.Evaluator = &evaluator
	}
	opts.Passed = &passed
	return t.Score(name, passed, opts)
}

func (t *Trial) RubricScore(name string, value any, opts ScoreOptions) (ScoreItem, error) {
	if opts.Evaluator == nil {
		evaluator := Evaluator{EvaluatorID: t.evaluator.EvaluatorID + "." + name, Version: t.evaluator.Version, Kind: EvaluatorKindLLMJudge}
		opts.Evaluator = &evaluator
	}
	return t.Score(name, value, opts)
}

type RecordEvaluationOptions struct {
	ScoreKey      string
	PublishGrader *bool
	Metadata      map[string]any
}

func (t *Trial) RecordEvaluation(ctx context.Context, result EvaluationResult, optional ...RecordEvaluationOptions) (ScoreItem, error) {
	opts := RecordEvaluationOptions{}
	if len(optional) > 0 {
		opts = optional[0]
	}
	publishGrader := true
	if opts.PublishGrader != nil {
		publishGrader = *opts.PublishGrader
	}
	scoreKey := firstNonBlank(opts.ScoreKey, result.ScoreKey, "final")
	t.mu.Lock()
	scoreID := t.nextScoreID(scoreKey, result.Evaluator.EvaluatorID)
	t.mu.Unlock()
	var graderConversationID, graderGenerationID string
	if result.Grader != nil && publishGrader {
		graderGenerationID = StableID("gen", scoreID, "grader")
		graderConversationID = StableID("conv", scoreID, "grader")
		grader := result.Grader
		usage := TokenUsage{}
		if grader.Usage != nil {
			usage = *grader.Usage
		}
		if err := t.client.RecordGeneration(ctx, graderGenerationID, GenerationOptions{
			ConversationID: graderConversationID, InputText: grader.Input, OutputText: grader.Output,
			ModelProvider: grader.ModelProvider, ModelName: grader.ModelName,
			AgentName: grader.AgentName, AgentVersion: grader.AgentVersion,
			OperationName: firstNonBlank(grader.OperationName, "evaluate"), Usage: usage,
			Tags: map[string]string{
				"experiment.run_id": t.Ref.ExperimentID, "test.case.id": t.Ref.TestCaseID,
				"test.case.attempt": fmt.Sprint(t.Ref.Attempt), "evaluator.id": result.Evaluator.EvaluatorID,
			},
			Metadata: mergeMaps(result.Metadata, map[string]any{
				"experiment_run_id": t.Ref.ExperimentID, "test_case_id": t.Ref.TestCaseID, "attempt": t.Ref.Attempt,
			}),
		}); err != nil {
			return ScoreItem{}, err
		}
	}
	passed := result.Passed
	evaluator := result.Evaluator
	return t.score(scoreKey, result.Value, ScoreOptions{
		Evaluator: &evaluator, Passed: &passed, Explanation: result.Explanation,
		GraderConversationID: graderConversationID, GraderGenerationID: graderGenerationID,
		Metadata: mergeMaps(result.Metadata, opts.Metadata),
	}, scoreID)
}

func (t *Trial) EvaluateOutput(ctx context.Context, evaluator OutputEvaluator, input EvaluationInput, options ...RecordEvaluationOptions) (ScoreItem, error) {
	if evaluator == nil {
		return ScoreItem{}, errors.New("output evaluator is required")
	}
	result, err := evaluator.EvaluateOutput(ctx, input)
	if err != nil {
		return ScoreItem{}, err
	}
	return t.RecordEvaluation(ctx, result, options...)
}

type ArtifactOptions struct {
	Name, Path, Kind, MIME, Text string
	Content                      []byte
	Data                         any
}

func (t *Trial) Artifact(ctx context.Context, opts ArtifactOptions) (*TrialArtifact, error) {
	content, kind, mediaType, err := artifactContent(opts)
	if err != nil {
		return nil, err
	}
	record, err := t.client.UploadArtifact(ctx, t.Ref.ExperimentID, t.TrialID, agento11y.TrialArtifactUpload{
		Name: opts.Name, Kind: kind, MIME: mediaType, Content: content,
	})
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.artifacts = append(t.artifacts, ExperimentArtifactRef{ArtifactID: record.ArtifactID, Name: opts.Name, Kind: kind, MIME: mediaType})
	t.mu.Unlock()
	return record, nil
}

func (t *Trial) Flush(ctx context.Context) (int, error) {
	if err := t.create(ctx); err != nil {
		return 0, err
	}
	if err := t.ensureGeneration(ctx); err != nil {
		return 0, err
	}
	t.mu.Lock()
	if len(t.buffer) == 0 {
		t.mu.Unlock()
		return 0, t.client.Flush(ctx)
	}
	pending := append([]ScoreItem(nil), t.buffer...)
	t.mu.Unlock()
	response, err := t.client.ExportScores(ctx, pending)
	if err != nil {
		return 0, err
	}
	count, err := acceptedCount(response)
	if err != nil {
		return 0, err
	}
	t.mu.Lock()
	t.buffer = t.buffer[len(pending):]
	t.accepted += count
	t.mu.Unlock()
	if t.experiment != nil {
		t.experiment.recordAccepted(count)
	}
	return count, nil
}

func (t *Trial) ensureGeneration(ctx context.Context) error {
	t.mu.Lock()
	if t.generationExported || t.generationBound || len(t.io) == 0 {
		t.mu.Unlock()
		return nil
	}
	ioValues := cloneMap(t.io)
	conversationID, generationID := t.ConversationID, t.GenerationID
	ref, candidate, testCase := t.Ref, cloneCandidate(t.candidate), cloneTestCasePtr(t.testCase)
	t.mu.Unlock()
	inputText := stringValue(ioValues["input_text"])
	if inputText == "" && testCase != nil && testCase.Input != nil {
		inputText = fmt.Sprint(testCase.Input)
	}
	usage := TokenUsage{}
	if value, ok := ioValues["input_tokens"].(int); ok {
		usage.InputTokens = int64(value)
	}
	if value, ok := ioValues["output_tokens"].(int); ok {
		usage.OutputTokens = int64(value)
	}
	opts := GenerationOptions{
		ConversationID: conversationID, InputText: inputText, OutputText: stringValue(ioValues["output_text"]),
		ModelProvider: firstNonBlank(stringValue(ioValues["model_provider"]), candidateField(candidate, "provider"), "eval"),
		ModelName:     firstNonBlank(stringValue(ioValues["model_name"]), candidateField(candidate, "model"), "experiment"),
		AgentName:     firstNonBlank(stringValue(ioValues["agent_name"]), candidateField(candidate, "agent")),
		AgentVersion:  firstNonBlank(stringValue(ioValues["agent_version"]), candidateVersion(candidate)),
		Usage:         usage, Tags: map[string]string{"experiment.run_id": ref.ExperimentID, "task_id": ref.TestCaseID},
		Metadata: map[string]any{"experiment_run_id": ref.ExperimentID, "task_id": ref.TestCaseID, "trial_id": t.TrialID, "attempt": ref.Attempt},
	}
	if err := t.client.RecordGeneration(ctx, generationID, opts); err != nil {
		return err
	}
	t.mu.Lock()
	t.generationExported = true
	t.mu.Unlock()
	return nil
}

func (t *Trial) AcceptedScores() int { t.mu.Lock(); defer t.mu.Unlock(); return t.accepted }
func (t *Trial) Succeed() *Trial     { t.mu.Lock(); t.Status = TrialStatusPassed; t.mu.Unlock(); return t }
func (t *Trial) Fail(errorText string) *Trial {
	t.mu.Lock()
	t.Status, t.Error = TrialStatusFailed, errorText
	t.mu.Unlock()
	return t
}

func (t *Trial) testCaseSnapshot() *agento11y.TestCaseSnapshot {
	if t.testCase == nil {
		return nil
	}
	artifactRefs := make([]ExperimentArtifactRef, 0, len(t.testCase.ArtifactRefs))
	for _, ref := range t.testCase.ArtifactRefs {
		if ref.ArtifactID != "" && ref.Name != "" && ref.Kind != "" {
			artifactRefs = append(artifactRefs, ref)
		}
	}
	return &agento11y.TestCaseSnapshot{
		TestCaseID: t.testCase.TestCaseID, SuiteID: t.Ref.SuiteID, SuiteVersion: t.Ref.SuiteVersion,
		Name: t.testCase.Name, Description: t.testCase.Description, Tags: append([]string(nil), t.testCase.Tags...),
		Category: t.testCase.Category, Input: objectValue(t.testCase.Input), Expected: objectValue(t.testCase.Expected),
		Metadata: cloneMap(t.testCase.Metadata), ArtifactRefs: artifactRefs,
	}
}

func (t *Trial) identityAttributes() []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("agento11y.eval.schema.version", "experiments-otel-2026-06"),
		attribute.String("gen_ai.operation.name", "invoke_agent"),
		attribute.String("test.suite.run.id", t.Ref.ExperimentID),
		attribute.String("test.suite.name", firstNonBlank(t.Ref.SuiteName, experimentName(t.experiment))),
		attribute.String("test.suite.run.status", "in_progress"),
		attribute.String("test.case.id", t.Ref.TestCaseID),
		attribute.String("test.case.name", firstNonBlank(t.Ref.TestCaseName, t.Ref.TestCaseID)),
		attribute.String("test.case.run.id", t.TrialID),
		attribute.Int("test.case.run.attempt", t.Ref.Attempt),
	}
	if t.Ref.SuiteID != "" {
		attrs = append(attrs, attribute.String("test.suite.id", t.Ref.SuiteID))
	}
	if t.Ref.SuiteVersion != "" {
		attrs = append(attrs, attribute.String("test.suite.version", t.Ref.SuiteVersion))
	}
	if t.candidate != nil {
		if t.candidate.AgentName != "" {
			attrs = append(attrs, attribute.String("gen_ai.agent.name", t.candidate.AgentName))
		}
		if t.candidate.AgentVersion != "" {
			attrs = append(attrs, attribute.String("gen_ai.agent.version", t.candidate.AgentVersion))
		}
		if t.candidate.ModelProvider != "" {
			attrs = append(attrs, attribute.String("gen_ai.provider.name", t.candidate.ModelProvider))
		}
		if t.candidate.ModelName != "" {
			attrs = append(attrs, attribute.String("gen_ai.request.model", t.candidate.ModelName))
		}
	}
	return attrs
}

func (t *Trial) emitEvaluationEvent(name string, value ScoreValue, evaluator Evaluator, opts ScoreOptions) {
	if t.span == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.evaluation.name", name),
		attribute.String("gen_ai.evaluation.evaluator.id", evaluator.EvaluatorID),
		attribute.String("gen_ai.evaluation.evaluator.version", evaluator.Version),
		attribute.String("gen_ai.evaluation.evaluator.type", string(evaluator.Kind)),
	}
	if value.Number != nil {
		attrs = append(attrs, attribute.Float64("gen_ai.evaluation.score.value", *value.Number))
	}
	if value.Bool != nil {
		if *value.Bool {
			attrs = append(attrs, attribute.Float64("gen_ai.evaluation.score.value", 1))
		} else {
			attrs = append(attrs, attribute.Float64("gen_ai.evaluation.score.value", 0))
		}
	}
	if opts.Passed != nil {
		label := "fail"
		if *opts.Passed {
			label = "pass"
		}
		attrs = append(attrs, attribute.String("gen_ai.evaluation.score.label", label))
	}
	if value.String != nil && opts.Passed == nil {
		attrs = append(attrs, attribute.String("gen_ai.evaluation.score.label", *value.String))
	}
	if opts.Explanation != "" {
		attrs = append(attrs, attribute.String("gen_ai.evaluation.explanation", t.client.redactText(opts.Explanation)))
	}
	if opts.GenerationID != "" {
		attrs = append(attrs, attribute.String("gen_ai.response.id", opts.GenerationID))
	}
	if evaluator.ReferenceSetID != "" {
		attrs = append(attrs, attribute.String("gen_ai.evaluation.reference_set.id", evaluator.ReferenceSetID))
	}
	if evaluator.ReferenceSetVersion != "" {
		attrs = append(attrs, attribute.String("gen_ai.evaluation.reference_set.version", evaluator.ReferenceSetVersion))
	}
	t.span.AddEvent("gen_ai.evaluation.result", trace.WithAttributes(attrs...))
}

func (t *Trial) endSpan(err error) {
	if t.span == nil {
		return
	}
	if err != nil {
		safe := t.client.redactText(err.Error())
		t.span.SetStatus(codes.Error, safe)
		t.span.SetAttributes(attribute.String("gen_ai.evaluation.explanation", safe))
	} else {
		t.span.SetStatus(codes.Ok, "")
	}
	if t.ConversationID != "" {
		t.span.SetAttributes(attribute.String("gen_ai.conversation.id", t.ConversationID))
	}
	t.span.End()
	t.span = nil
	t.spanContext = nil
}

func coerceScoreValue(value any) (ScoreValue, error) {
	switch typed := value.(type) {
	case ScoreValue:
		return typed, nil
	case bool:
		return BoolScoreValue(typed), nil
	case string:
		return StringScoreValue(typed), nil
	case int:
		return NumberScoreValue(float64(typed)), nil
	case int32:
		return NumberScoreValue(float64(typed)), nil
	case int64:
		return NumberScoreValue(float64(typed)), nil
	case float32:
		return NumberScoreValue(float64(typed)), nil
	case float64:
		return NumberScoreValue(typed), nil
	default:
		return ScoreValue{}, fmt.Errorf("unsupported score value type %T", value)
	}
}

func artifactContent(opts ArtifactOptions) ([]byte, string, string, error) {
	switch {
	case opts.Path != "":
		content, err := os.ReadFile(opts.Path)
		if err != nil {
			return nil, "", "", err
		}
		mediaType := opts.MIME
		if mediaType == "" {
			mediaType = mime.TypeByExtension(filepath.Ext(opts.Path))
		}
		if mediaType == "" {
			mediaType = "application/octet-stream"
		}
		return content, firstNonBlank(opts.Kind, artifactKind(mediaType)), mediaType, nil
	case len(opts.Content) > 0:
		return opts.Content, firstNonBlank(opts.Kind, artifactKind(opts.MIME)), firstNonBlank(opts.MIME, "application/octet-stream"), nil
	case opts.Data != nil:
		content, err := json.Marshal(opts.Data)
		return content, firstNonBlank(opts.Kind, "json"), firstNonBlank(opts.MIME, "application/json"), err
	case opts.Text != "":
		return []byte(opts.Text), firstNonBlank(opts.Kind, "text"), firstNonBlank(opts.MIME, "text/plain"), nil
	default:
		return nil, "", "", errors.New("artifact requires Path, Content, Data, or Text")
	}
}

func artifactKind(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case mediaType == "application/json":
		return "json"
	case mediaType == "text/markdown":
		return "markdown"
	case mediaType == "application/pdf":
		return "pdf"
	case mediaType == "text/csv":
		return "csv"
	case strings.HasPrefix(mediaType, "text/"):
		return "text"
	default:
		return "binary"
	}
}

func objectValue(value any) any {
	if value == nil {
		return map[string]any{}
	}
	if _, ok := value.(map[string]any); ok {
		return value
	}
	return map[string]any{"value": value}
}

func mapIf(condition bool, key string, value any) map[string]any {
	if !condition {
		return nil
	}
	return map[string]any{key: value}
}

func mergeMaps(inputs ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, input := range inputs {
		maps.Copy(out, input)
	}
	return out
}

func cloneSuite(in *TestSuite) *TestSuite {
	if in == nil {
		return nil
	}
	out := *in
	out.Tags = append([]string(nil), in.Tags...)
	out.TestCases = in.Cases()
	return &out
}

func cloneCandidate(in *Candidate) *Candidate {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneTestCasePtr(in *TestCase) *TestCase {
	if in == nil {
		return nil
	}
	out := cloneTestCase(*in)
	return &out
}

func cloneInt(in *int) *int {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}
func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}
func randomIdentity() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
func candidateField(c *Candidate, field string) string {
	if c == nil {
		return ""
	}
	if field == "provider" {
		return c.ModelProvider
	}
	if field == "model" {
		return c.ModelName
	}
	return c.AgentName
}
func candidateVersion(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.AgentVersion
}
func experimentName(e *Experiment) string {
	if e == nil {
		return ""
	}
	return e.Name
}
