package agento11y

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
)

type ExperimentOptions struct {
	Client           *Client
	RunID            string
	Name             string
	Suite            *TestSuite
	Candidate        *Candidate
	DefaultEvaluator *Evaluator
	Description      string
	Tags             []string
	Metadata         map[string]any
	AutoFinalize     *bool
}

type ExperimentRun struct {
	client           *Client
	RunID            string
	Name             string
	suite            *TestSuite
	candidate        *Candidate
	defaultEvaluator Evaluator
	description      string
	tags             []string
	metadata         map[string]any
	autoFinalize     bool

	mu                   sync.Mutex
	accepted             int
	finalized            bool
	recorders            []*GenerationRecorder
	trackedIDs           []string
	activeConversationID string
	status               string
}

func NewExperimentRun(opts ExperimentOptions) *ExperimentRun {
	runID := strings.TrimSpace(opts.RunID)
	if runID == "" {
		runID = StableID("exp", opts.Name, experimentRandomHex(8))
	}
	name := opts.Name
	if name == "" {
		name = runID
	}
	defaultEvaluator := Evaluator{EvaluatorID: "sdk", Version: "0", Kind: EvaluatorKindCustom}
	if opts.DefaultEvaluator != nil {
		defaultEvaluator = opts.DefaultEvaluator.normalized()
	}
	autoFinalize := true
	if opts.AutoFinalize != nil {
		autoFinalize = *opts.AutoFinalize
	}
	return &ExperimentRun{
		client:           opts.Client,
		RunID:            runID,
		Name:             name,
		suite:            cloneTestSuite(opts.Suite),
		candidate:        cloneCandidate(opts.Candidate),
		defaultEvaluator: defaultEvaluator,
		description:      opts.Description,
		tags:             append([]string(nil), opts.Tags...),
		metadata:         cloneMetadata(opts.Metadata),
		autoFinalize:     autoFinalize,
		status:           string(ExperimentStatusRunning),
	}
}

func (r *ExperimentRun) Enter(ctx context.Context) error {
	if r == nil || r.client == nil {
		return ErrNilClient
	}
	metadata := cloneMetadata(r.metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	if r.suite != nil {
		if r.suite.SuiteID != "" {
			metadata["suite_id"] = r.suite.SuiteID
		}
		if r.suite.Version != "" {
			metadata["suite_version"] = r.suite.Version
		}
	}
	if r.candidate != nil {
		maps.Copy(metadata, r.candidate.AsMetadata())
	}
	_, err := r.client.CreateExperiment(ctx, CreateExperimentRequest{
		RunID:       r.RunID,
		Name:        r.Name,
		Source:      ExperimentSourceExternal,
		Description: r.description,
		Tags:        append([]string(nil), r.tags...),
		Metadata:    metadata,
	})
	return err
}

func WithExperiment(ctx context.Context, opts ExperimentOptions, fn func(context.Context, *ExperimentRun) error) (run *ExperimentRun, err error) {
	run = NewExperimentRun(opts)
	if err := run.Enter(ctx); err != nil {
		return run, err
	}
	defer func() {
		recovered := recover()
		if recovered == nil {
			return
		}
		if run.autoFinalize {
			finalizeCtx, cancel := experimentCleanupContext(ctx)
			_ = run.Finalize(finalizeCtx, ExperimentStatusFailed, fmt.Sprint(recovered))
			cancel()
		}
		panic(recovered)
	}()
	err = fn(run.Context(ctx), run)
	if !run.autoFinalize {
		return run, err
	}
	finalizeCtx, cancel := experimentCleanupContext(ctx)
	defer cancel()
	if err != nil {
		if finalizeErr := run.Finalize(finalizeCtx, ExperimentStatusFailed, err.Error()); finalizeErr != nil {
			return run, errors.Join(err, fmt.Errorf("finalize experiment %q: %w", run.RunID, finalizeErr))
		}
		return run, err
	}
	if finalizeErr := run.Finalize(finalizeCtx, ExperimentStatusCompleted, ""); finalizeErr != nil {
		return run, finalizeErr
	}
	return run, nil
}

func experimentCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(context.WithoutCancel(ctx), defaultExperimentRetryTimeout)
}

func (r *ExperimentRun) Context(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if r == nil {
		return ctx
	}
	return withExperimentRun(ctx, r)
}

func (r *ExperimentRun) Suite() *TestSuite {
	if r == nil {
		return nil
	}
	return cloneTestSuite(r.suite)
}

func (r *ExperimentRun) Trial(testCase TestCase, opts ...TrialOption) *Trial {
	testCaseID := strings.TrimSpace(testCase.TestCaseID)
	testCaseName := firstNonBlank(testCase.Name, testCaseID)
	return r.trial(testCaseID, testCaseName, opts...)
}

func (r *ExperimentRun) TrialID(testCaseID string, opts ...TrialOption) *Trial {
	testCaseID = strings.TrimSpace(testCaseID)
	testCaseName := testCaseID
	if r != nil && r.suite != nil {
		if existing, ok := r.suite.Case(testCaseID); ok {
			testCaseName = firstNonBlank(existing.Name, testCaseID)
		}
	}
	return r.trial(testCaseID, testCaseName, opts...)
}

func (r *ExperimentRun) WithTrial(ctx context.Context, testCase TestCase, fn func(context.Context, *Trial) error, opts ...TrialOption) (err error) {
	if fn == nil {
		return fmt.Errorf("%w: trial callback is required", ErrExperimentValidationFailed)
	}
	ctx = r.Context(ctx)
	trial := r.Trial(testCase, opts...)
	if err := trial.Start(ctx); err != nil {
		return fmt.Errorf("start trial %q: %w", trial.ref.TestCaseID, err)
	}
	defer func() {
		recovered := recover()
		trialErr := err
		if recovered != nil {
			trialErr = fmt.Errorf("trial callback panic: %v", recovered)
		}
		if endErr := trial.End(ctx, trialErr); endErr != nil {
			err = errors.Join(err, fmt.Errorf("end trial %q: %w", trial.ref.TestCaseID, endErr))
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	return fn(ctx, trial)
}

func (r *ExperimentRun) WithTrialID(ctx context.Context, testCaseID string, fn func(context.Context, *Trial) error, opts ...TrialOption) (err error) {
	if fn == nil {
		return fmt.Errorf("%w: trial callback is required", ErrExperimentValidationFailed)
	}
	ctx = r.Context(ctx)
	trial := r.TrialID(testCaseID, opts...)
	if err := trial.Start(ctx); err != nil {
		return fmt.Errorf("start trial %q: %w", trial.ref.TestCaseID, err)
	}
	defer func() {
		recovered := recover()
		trialErr := err
		if recovered != nil {
			trialErr = fmt.Errorf("trial callback panic: %v", recovered)
		}
		if endErr := trial.End(ctx, trialErr); endErr != nil {
			err = errors.Join(err, fmt.Errorf("end trial %q: %w", trial.ref.TestCaseID, endErr))
		}
		if recovered != nil {
			panic(recovered)
		}
	}()
	return fn(ctx, trial)
}

func (r *ExperimentRun) trial(testCaseID, testCaseName string, opts ...TrialOption) *Trial {
	if r == nil {
		return NewTrial(nil, TrialRef{
			TestCaseID:   strings.TrimSpace(testCaseID),
			Attempt:      1,
			TestCaseName: firstNonBlank(testCaseName, strings.TrimSpace(testCaseID)),
		}, opts...)
	}
	ref := TrialRef{
		RunID:        r.RunID,
		TestCaseID:   strings.TrimSpace(testCaseID),
		Attempt:      1,
		SuiteName:    r.Name,
		TestCaseName: firstNonBlank(testCaseName, strings.TrimSpace(testCaseID)),
	}
	if r.suite != nil {
		ref.SuiteID = r.suite.SuiteID
		ref.SuiteVersion = r.suite.Version
		ref.SuiteName = firstNonBlank(r.suite.Name, r.Name)
	}
	trialOpts := make([]TrialOption, 0, len(opts)+1)
	trialOpts = append(trialOpts, WithTrialDefaultEvaluator(r.defaultEvaluator))
	trialOpts = append(trialOpts, opts...)
	t := NewTrial(r.client, ref, trialOpts...)
	t.experiment = r
	t.candidate = r.candidate
	return t
}

func (r *ExperimentRun) Finalize(ctx context.Context, status ExperimentStatus, errorText string) error {
	if r == nil || r.client == nil {
		return ErrNilClient
	}
	r.mu.Lock()
	if r.finalized {
		r.mu.Unlock()
		return nil
	}
	scoreCount := r.accepted
	r.mu.Unlock()
	if err := r.client.Flush(ctx); err != nil {
		return err
	}
	_, err := r.client.FinalizeExperiment(ctx, r.RunID, status, CompleteExperimentOptions{ScoreCount: &scoreCount, Error: errorText})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.status = string(status)
	r.finalized = true
	r.mu.Unlock()
	return nil
}

func (r *ExperimentRun) Report(ctx context.Context) (*ExperimentReport, error) {
	if r == nil || r.client == nil {
		return nil, ErrNilClient
	}
	return r.client.GetExperimentReport(ctx, r.RunID)
}

func (r *ExperimentRun) URL() string {
	if r == nil || r.client == nil {
		return ""
	}
	return r.client.ExperimentURL(r.RunID)
}

func (r *ExperimentRun) AcceptedScores() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.accepted
}

func (r *ExperimentRun) recordAccepted(n int) {
	r.mu.Lock()
	r.accepted += n
	r.mu.Unlock()
}

func (r *ExperimentRun) TrackGenerationID(generationID string) {
	if r == nil {
		return
	}
	generationID = strings.TrimSpace(generationID)
	if generationID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !slices.Contains(r.trackedIDs, generationID) {
		r.trackedIDs = append(r.trackedIDs, generationID)
	}
}

func (r *ExperimentRun) ResetCapture(conversationID string) string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recorders = nil
	r.trackedIDs = nil
	r.activeConversationID = strings.TrimSpace(conversationID)
	return r.activeConversationID
}

func (r *ExperimentRun) ProducedGenerationIDs() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var ids []string
	for _, recorder := range r.recorders {
		if recorder == nil {
			continue
		}
		recorder.mu.Lock()
		id := recorder.lastGeneration.ID
		recorder.mu.Unlock()
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	for _, id := range r.trackedIDs {
		if id != "" && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func (r *ExperimentRun) ActiveConversationID() string {
	if r == nil {
		return ""
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.activeConversationID
}

func (r *ExperimentRun) captureRecorder(recorder *GenerationRecorder) {
	if r == nil || recorder == nil {
		return
	}
	r.mu.Lock()
	r.recorders = append(r.recorders, recorder)
	r.mu.Unlock()
}

func (r *ExperimentRun) prepareGeneration(start GenerationStart) GenerationStart {
	r.mu.Lock()
	conversationID := strings.TrimSpace(start.ConversationID)
	if conversationID == "" {
		conversationID = strings.TrimSpace(r.activeConversationID)
	}
	if conversationID == "" {
		conversationID = StableID("conv", r.RunID, experimentRandomHex(8))
	}
	r.activeConversationID = conversationID
	r.mu.Unlock()
	start.ConversationID = conversationID

	tags := cloneTags(start.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[ExperimentRunIDTag] = r.RunID
	start.Tags = tags

	metadata := cloneMetadata(start.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[ExperimentRunIDMetadataKey] = r.RunID
	start.Metadata = metadata

	if start.AgentName == "" && r.candidate != nil {
		start.AgentName = r.candidate.AgentName
	}
	if start.AgentVersion == "" && r.candidate != nil {
		start.AgentVersion = r.candidate.AgentVersion
	}
	return start
}

func prepareGenerationForExperimentRunID(start GenerationStart, runID string) GenerationStart {
	seed := cloneGenerationStart(start)
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return seed
	}
	tags := cloneTags(seed.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[ExperimentRunIDTag] = runID
	seed.Tags = tags
	metadata := cloneMetadata(seed.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[ExperimentRunIDMetadataKey] = runID
	seed.Metadata = metadata
	return seed
}
