package sigil

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ExperimentRunIDTag         = "experiment.run_id"
	ExperimentRunIDMetadataKey = "experiment_run_id"

	EnvExperimentID = "SIGIL_EXPERIMENT_ID"
	EnvRunID        = "SIGIL_RUN_ID"
	EnvTestCaseID   = "SIGIL_TEST_CASE_ID"
	EnvAttempt      = "SIGIL_ATTEMPT"
	EnvSuiteID      = "SIGIL_SUITE_ID"
	EnvSuiteVersion = "SIGIL_SUITE_VERSION"
	EnvTrajectoryID = "SIGIL_TRAJECTORY_ID"
)

type TrialStatus string

const (
	TrialStatusRunning TrialStatus = "running"
	TrialStatusPassed  TrialStatus = "passed"
	TrialStatusFailed  TrialStatus = "failed"
	TrialStatusErrored TrialStatus = "errored"
	TrialStatusSkipped TrialStatus = "skipped"
)

type EvaluatorKind string

const (
	EvaluatorKindLLMJudge      EvaluatorKind = "llm_judge"
	EvaluatorKindDeterministic EvaluatorKind = "deterministic"
	EvaluatorKindHuman         EvaluatorKind = "human"
	EvaluatorKindCustom        EvaluatorKind = "custom"
)

type TestCase struct {
	TestCaseID  string
	Name        string
	Description string
	Tags        []string
	Category    string
	Input       any
	Expected    any
	Weight      float64
	Metadata    map[string]any
}

type TestSuite struct {
	SuiteID     string
	Name        string
	Version     string
	Description string
	Tags        []string
	Changelog   string
	TestCases   []TestCase
}

func (s *TestSuite) Cases() []TestCase {
	if s == nil {
		return nil
	}
	return s.TestCases
}

func (s *TestSuite) Case(testCaseID string) *TestCase {
	if s == nil {
		return nil
	}
	for i := range s.TestCases {
		if s.TestCases[i].TestCaseID == testCaseID {
			return &s.TestCases[i]
		}
	}
	return nil
}

type Candidate struct {
	AgentName     string
	AgentVersion  string
	PromptVersion string
	ModelProvider string
	ModelName     string
	GitSHA        string
}

func (c Candidate) AsMetadata() map[string]any {
	out := map[string]any{}
	if c.AgentName != "" {
		out["agent_name"] = c.AgentName
	}
	if c.AgentVersion != "" {
		out["agent_version"] = c.AgentVersion
	}
	if c.PromptVersion != "" {
		out["prompt_version"] = c.PromptVersion
	}
	if c.ModelProvider != "" {
		out["model_provider"] = c.ModelProvider
	}
	if c.ModelName != "" {
		out["model_name"] = c.ModelName
	}
	if c.GitSHA != "" {
		out["git_sha"] = c.GitSHA
	}
	return out
}

type Evaluator struct {
	EvaluatorID         string
	Version             string
	Kind                EvaluatorKind
	ReferenceSetID      string
	ReferenceSetVersion string
}

func (e Evaluator) normalized() Evaluator {
	if e.EvaluatorID == "" {
		e.EvaluatorID = "sdk"
	}
	if e.Version == "" {
		e.Version = "0"
	}
	e.Kind = NormalizeEvaluatorKind(string(e.Kind))
	return e
}

func NormalizeEvaluatorKind(kind string) EvaluatorKind {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "llm_judge", "llm-judge", "llm", "judge", "rubric":
		return EvaluatorKindLLMJudge
	case "deterministic", "check", "rule", "exact", "code":
		return EvaluatorKindDeterministic
	case "human", "manual", "annotator":
		return EvaluatorKindHuman
	default:
		return EvaluatorKindCustom
	}
}

type TrialRef struct {
	ExperimentID string
	TestCaseID   string
	Attempt      int
	SuiteID      string
	SuiteVersion string
	SuiteName    string
	TestCaseName string
	TrajectoryID string
}

func (r TrialRef) RunID() string {
	return r.ExperimentID
}

func (r TrialRef) ToJSON() map[string]any {
	attempt := r.Attempt
	if attempt == 0 {
		attempt = 1
	}
	return map[string]any{
		"experiment_id":  r.ExperimentID,
		"test_case_id":   r.TestCaseID,
		"attempt":        attempt,
		"suite_id":       r.SuiteID,
		"suite_version":  r.SuiteVersion,
		"suite_name":     r.SuiteName,
		"test_case_name": r.TestCaseName,
		"trajectory_id":  r.TrajectoryID,
	}
}

func TrialRefFromJSON(payload map[string]any) TrialRef {
	attempt := 1
	switch raw := payload["attempt"].(type) {
	case int:
		attempt = raw
	case float64:
		attempt = int(raw)
	case string:
		if parsed, err := strconv.Atoi(raw); err == nil {
			attempt = parsed
		}
	}
	return TrialRef{
		ExperimentID: strings.TrimSpace(firstString(payload["experiment_id"], payload["run_id"])),
		TestCaseID:   strings.TrimSpace(firstString(payload["test_case_id"])),
		Attempt:      attempt,
		SuiteID:      strings.TrimSpace(firstString(payload["suite_id"])),
		SuiteVersion: strings.TrimSpace(firstString(payload["suite_version"])),
		SuiteName:    strings.TrimSpace(firstString(payload["suite_name"])),
		TestCaseName: strings.TrimSpace(firstString(payload["test_case_name"])),
		TrajectoryID: strings.TrimSpace(firstString(payload["trajectory_id"])),
	}
}

func (r TrialRef) ToEnv() map[string]string {
	attempt := r.Attempt
	if attempt == 0 {
		attempt = 1
	}
	env := map[string]string{
		EnvExperimentID: r.ExperimentID,
		EnvTestCaseID:   r.TestCaseID,
		EnvAttempt:      strconv.Itoa(attempt),
	}
	if r.SuiteID != "" {
		env[EnvSuiteID] = r.SuiteID
	}
	if r.SuiteVersion != "" {
		env[EnvSuiteVersion] = r.SuiteVersion
	}
	if r.TrajectoryID != "" {
		env[EnvTrajectoryID] = r.TrajectoryID
	}
	return env
}

func TrialRefFromEnv() (*TrialRef, bool) {
	experimentID := strings.TrimSpace(firstNonBlank(os.Getenv(EnvExperimentID), os.Getenv(EnvRunID)))
	testCaseID := strings.TrimSpace(os.Getenv(EnvTestCaseID))
	if experimentID == "" || testCaseID == "" {
		return nil, false
	}
	attempt := 1
	if parsed, err := strconv.Atoi(os.Getenv(EnvAttempt)); err == nil && parsed > 0 {
		attempt = parsed
	}
	return &TrialRef{
		ExperimentID: experimentID,
		TestCaseID:   testCaseID,
		Attempt:      attempt,
		SuiteID:      strings.TrimSpace(os.Getenv(EnvSuiteID)),
		SuiteVersion: strings.TrimSpace(os.Getenv(EnvSuiteVersion)),
		TrajectoryID: strings.TrimSpace(os.Getenv(EnvTrajectoryID)),
	}, true
}

type ExperimentOptions struct {
	Client              *Client
	ExperimentID        string
	RunID               string
	Name                string
	Suite               *TestSuite
	Candidate           *Candidate
	DefaultEvaluator    *Evaluator
	Description         string
	Tags                []string
	Metadata            map[string]any
	AutoFinalize        *bool
	UseExperimentalOTel bool
}

type ExperimentRun struct {
	client           *Client
	ExperimentID     string
	Name             string
	Suite            *TestSuite
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
	experimentID := firstNonBlank(opts.ExperimentID, opts.RunID)
	if experimentID == "" {
		experimentID = StableID("exp", opts.Name, experimentRandomHex(8))
	}
	name := opts.Name
	if name == "" {
		name = experimentID
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
		ExperimentID:     experimentID,
		Name:             name,
		Suite:            opts.Suite,
		candidate:        opts.Candidate,
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
	if r.Suite != nil {
		if r.Suite.SuiteID != "" {
			metadata["suite_id"] = r.Suite.SuiteID
		}
		if r.Suite.Version != "" {
			metadata["suite_version"] = r.Suite.Version
		}
	}
	if r.candidate != nil {
		maps.Copy(metadata, r.candidate.AsMetadata())
	}
	_, err := r.client.CreateExperiment(ctx, CreateExperimentRequest{
		RunID:       r.ExperimentID,
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
			return run, errors.Join(err, finalizeErr)
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

func (r *ExperimentRun) Trial(testCase any, opts ...TrialOption) *Trial {
	testCaseID, testCaseName := r.resolveTestCase(testCase)
	ref := TrialRef{
		ExperimentID: r.ExperimentID,
		TestCaseID:   testCaseID,
		Attempt:      1,
		SuiteName:    r.Name,
		TestCaseName: testCaseName,
	}
	if r.Suite != nil {
		ref.SuiteID = r.Suite.SuiteID
		ref.SuiteVersion = r.Suite.Version
		ref.SuiteName = firstNonBlank(r.Suite.Name, r.Name)
	}
	t := NewTrial(r.client, ref, opts...)
	t.experiment = r
	t.candidate = r.candidate
	t.defaultEvaluator = r.defaultEvaluator
	return t
}

func (r *ExperimentRun) resolveTestCase(testCase any) (string, string) {
	switch tc := testCase.(type) {
	case TestCase:
		return tc.TestCaseID, firstNonBlank(tc.Name, tc.TestCaseID)
	case *TestCase:
		if tc == nil {
			return "", ""
		}
		return tc.TestCaseID, firstNonBlank(tc.Name, tc.TestCaseID)
	case string:
		if r != nil && r.Suite != nil {
			if existing := r.Suite.Case(tc); existing != nil {
				return tc, firstNonBlank(existing.Name, tc)
			}
		}
		return tc, tc
	default:
		id := fmt.Sprint(testCase)
		return id, id
	}
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
	r.status = string(status)
	r.mu.Unlock()
	_, err := r.client.FinalizeExperiment(ctx, r.ExperimentID, status, CompleteExperimentOptions{ScoreCount: &scoreCount, Error: errorText})
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.finalized = true
	r.mu.Unlock()
	return nil
}

func (r *ExperimentRun) Report(ctx context.Context) (*ExperimentReport, error) {
	if r == nil || r.client == nil {
		return nil, ErrNilClient
	}
	return r.client.GetExperimentReport(ctx, r.ExperimentID)
}

func (r *ExperimentRun) URL() string {
	if r == nil || r.client == nil {
		return ""
	}
	return r.client.ExperimentURL(r.ExperimentID)
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
		conversationID = StableID("conv", r.ExperimentID, experimentRandomHex(8))
	}
	r.activeConversationID = conversationID
	r.mu.Unlock()
	start.ConversationID = conversationID

	tags := cloneTags(start.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[ExperimentRunIDTag] = r.ExperimentID
	start.Tags = tags

	metadata := cloneMetadata(start.Metadata)
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata[ExperimentRunIDMetadataKey] = r.ExperimentID
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

type TrialOption func(*Trial)

func WithTrialAttempt(attempt int) TrialOption {
	return func(t *Trial) {
		if attempt > 0 {
			t.ref.Attempt = attempt
			t.trialID = StableID("trial", t.ref.ExperimentID, t.ref.TestCaseID, t.ref.Attempt)
			t.generationID = StableID("gen", t.ref.ExperimentID, t.ref.TestCaseID, t.ref.Attempt)
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
	if ref.Attempt == 0 {
		ref.Attempt = 1
	}
	t := &Trial{
		client:           client,
		ref:              ref,
		defaultEvaluator: Evaluator{EvaluatorID: "sdk", Version: "0", Kind: EvaluatorKindCustom},
		metadata:         map[string]any{},
		trialID:          StableID("trial", ref.ExperimentID, ref.TestCaseID, ref.Attempt),
		status:           TrialStatusRunning,
		generationID:     StableID("gen", ref.ExperimentID, ref.TestCaseID, ref.Attempt),
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
	t.started = time.Now()
	return t.createTrial(ctx)
}

func (t *Trial) End(ctx context.Context, err error) error {
	if t == nil {
		return ErrNilClient
	}
	if err != nil && t.status == TrialStatusRunning {
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
	_, flushErr := t.Flush(cleanupCtx)
	finalizeErr := t.finalizeTrial(cleanupCtx)
	if flushErr != nil {
		return flushErr
	}
	return finalizeErr
}

func (t *Trial) createTrial(ctx context.Context) error {
	if t.trialCreated {
		return nil
	}
	metadata := map[string]any{}
	if t.ref.TestCaseName != "" {
		metadata["test_case_name"] = t.ref.TestCaseName
	}
	if len(metadata) == 0 {
		metadata = nil
	}
	_, err := t.client.UpsertTrial(ctx, t.ref.ExperimentID, UpsertTrialRequest{
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
	_, err := t.client.UpdateTrial(ctx, t.ref.ExperimentID, t.trialID, req)
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
		t.generationExported = true
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
	}
	if opts.OutputTokens != nil {
		t.io["output_tokens"] = *opts.OutputTokens
	}
	if t.hasRecordedGenerationData() {
		t.hasGeneration = true
		if t.conversationID == "" {
			t.conversationID = StableID("conv", t.ref.ExperimentID, t.ref.TestCaseID, t.ref.Attempt)
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
	scoreID := StableID("score", t.ref.ExperimentID, t.trialID, scoreKey, ev.EvaluatorID)
	metadata := map[string]any{
		"task_id":  t.ref.TestCaseID,
		"trial_id": t.trialID,
		"attempt":  t.ref.Attempt,
	}
	maps.Copy(metadata, t.metadata)
	maps.Copy(metadata, opts.Metadata)
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
		ExperimentID:         t.ref.ExperimentID,
		TestCaseID:           t.ref.TestCaseID,
		GraderConversationID: opts.GraderConversationID,
		GraderGenerationID:   opts.GraderGenerationID,
		GraderTraceID:        opts.GraderTraceID,
		Passed:               opts.Passed,
		Explanation:          opts.Explanation,
		Metadata:             metadata,
		Source:               &ScoreSource{Kind: "experiment", ID: t.ref.ExperimentID},
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
	record, err := t.client.UploadTrialArtifact(ctx, t.ref.ExperimentID, t.trialID, TrialArtifactUpload{
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
	if t.generationExported || t.generationBound || !t.hasRecordedGenerationData() {
		return nil
	}
	caseInput := ""
	if t.experiment != nil && t.experiment.Suite != nil {
		if tc := t.experiment.Suite.Case(t.ref.TestCaseID); tc != nil && tc.Input != nil {
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
		Tags:           map[string]string{"experiment.run_id": t.ref.ExperimentID, "task_id": t.ref.TestCaseID},
		Metadata: map[string]any{
			"experiment_run_id": t.ref.ExperimentID,
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

func StableID(prefix string, parts ...any) string {
	values := make([]string, len(parts))
	for i, part := range parts {
		if part != nil {
			values[i] = fmt.Sprint(part)
		}
	}
	digest := sha1.Sum([]byte(strings.Join(values, "\x1f")))
	return prefix + "-" + hex.EncodeToString(digest[:])[:16]
}

func acceptedOrError(response *ExportScoresResponse) (int, error) {
	if response == nil {
		return 0, fmt.Errorf("%w: empty response", ErrScoreExportFailed)
	}
	rejected := response.Rejected()
	if len(rejected) == 0 {
		return response.AcceptedCount() + response.DuplicateCount(), nil
	}
	parts := make([]string, len(rejected))
	for i, result := range rejected {
		detail := result.Error
		if detail == "" {
			detail = "rejected"
		}
		parts[i] = result.ScoreID + ": " + detail
	}
	return 0, fmt.Errorf("%w: rejected %d score(s): %s", ErrScoreExportFailed, len(rejected), strings.Join(parts, "; "))
}

func experimentRandomHex(n int) string {
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func firstNonBlank(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstString(values ...any) string {
	for _, value := range values {
		switch typed := value.(type) {
		case string:
			if typed != "" {
				return typed
			}
		case fmt.Stringer:
			if typed.String() != "" {
				return typed.String()
			}
		}
	}
	return ""
}

func textMessages(role Role, text string) []Message {
	if text == "" {
		return nil
	}
	if role == RoleAssistant {
		return []Message{AssistantTextMessage(text)}
	}
	return []Message{UserTextMessage(text)}
}

func candidateModelProvider(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.ModelProvider
}

func candidateModelName(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.ModelName
}

func candidateAgentName(c *Candidate) string {
	if c == nil {
		return ""
	}
	return c.AgentName
}
