package sigil

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	evalExperimentsSuffix    = "/eval/experiments"
	defaultEvalPathPrefix    = "/api/v1"
	experimentRunsUpsertPath = "/api/v1/experiment-runs:upsert"
	experimentRunsPrefix     = "/api/v1/experiment-runs"
	scoresExportPath         = "/api/v1/scores:export"
	maxEvalResponseBytes     = 8 << 20

	envExperimentURLTemplate      = "SIGIL_EXPERIMENT_URL_TEMPLATE"
	defaultExperimentRetryTimeout = 30 * time.Second
)

type ExperimentStatus string

const (
	ExperimentStatusRunning   ExperimentStatus = "running"
	ExperimentStatusCompleted ExperimentStatus = "completed"
	ExperimentStatusSucceeded ExperimentStatus = "succeeded"
	ExperimentStatusFailed    ExperimentStatus = "failed"
	ExperimentStatusCanceled  ExperimentStatus = "canceled"
)

type ExperimentSource string

const (
	ExperimentSourceExternal   ExperimentSource = "external"
	ExperimentSourceCollection ExperimentSource = "collection"
)

type ScoreValue struct {
	Number *float64 `json:"number,omitempty"`
	Bool   *bool    `json:"bool,omitempty"`
	String *string  `json:"string,omitempty"`
}

func NumberScoreValue(v float64) ScoreValue {
	return ScoreValue{Number: &v}
}

func BoolScoreValue(v bool) ScoreValue {
	return ScoreValue{Bool: &v}
}

func StringScoreValue(v string) ScoreValue {
	return ScoreValue{String: &v}
}

type ScoreSource struct {
	Kind string `json:"kind,omitempty"`
	ID   string `json:"id,omitempty"`
}

type ScoreType string

const (
	ScoreTypeNumber ScoreType = "number"
	ScoreTypeBool   ScoreType = "bool"
	ScoreTypeString ScoreType = "string"
)

type ScoreItem struct {
	ScoreID              string         `json:"score_id"`
	GenerationID         string         `json:"generation_id"`
	EvaluatorID          string         `json:"evaluator_id"`
	EvaluatorVersion     string         `json:"evaluator_version"`
	EvaluatorKind        string         `json:"evaluator_kind,omitempty"`
	ScoreKey             string         `json:"score_key"`
	Value                ScoreValue     `json:"value"`
	ConversationID       string         `json:"conversation_id,omitempty"`
	TraceID              string         `json:"trace_id,omitempty"`
	SpanID               string         `json:"span_id,omitempty"`
	RuleID               string         `json:"rule_id,omitempty"`
	ExperimentID         string         `json:"experiment_id,omitempty"`
	TrialID              string         `json:"trial_id,omitempty"`
	TestCaseID           string         `json:"test_case_id,omitempty"`
	GraderConversationID string         `json:"grader_conversation_id,omitempty"`
	GraderGenerationID   string         `json:"grader_generation_id,omitempty"`
	GraderTraceID        string         `json:"grader_trace_id,omitempty"`
	RunID                string         `json:"-"`
	Passed               *bool          `json:"passed,omitempty"`
	Explanation          string         `json:"explanation,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            *time.Time     `json:"created_at,omitempty"`
	Source               *ScoreSource   `json:"source,omitempty"`
}

func (s ScoreItem) ResolvedExperimentID() string {
	if strings.TrimSpace(s.ExperimentID) != "" {
		return strings.TrimSpace(s.ExperimentID)
	}
	return strings.TrimSpace(s.RunID)
}

type ExportScoreResult struct {
	ScoreID  string `json:"score_id"`
	Accepted bool   `json:"accepted"`
	Status   string `json:"status,omitempty"`
	Error    string `json:"error,omitempty"`
}

type ExportScoresResponse struct {
	Results       []ExportScoreResult `json:"results"`
	Accepted      int                 `json:"accepted,omitempty"`
	Duplicates    int                 `json:"duplicates,omitempty"`
	RejectedCount int                 `json:"rejected,omitempty"`
}

func (r ExportScoresResponse) AcceptedCount() int {
	if r.Accepted != 0 {
		return r.Accepted
	}
	count := 0
	for _, result := range r.Results {
		if result.Accepted {
			count++
		}
	}
	return count
}

func (r ExportScoresResponse) DuplicateCount() int {
	if r.Duplicates != 0 {
		return r.Duplicates
	}
	count := 0
	for _, result := range r.Results {
		if result.Status == "duplicate" {
			count++
		}
	}
	return count
}

func (r ExportScoresResponse) Rejected() []ExportScoreResult {
	var out []ExportScoreResult
	for _, result := range r.Results {
		if !result.Accepted && result.Status != "duplicate" {
			out = append(out, result)
		}
	}
	return out
}

type ExperimentEvaluator struct {
	ID       string `json:"id"`
	Selector string `json:"selector"`
}

type CreateExperimentRequest struct {
	RunID        string                `json:"run_id,omitempty"`
	Name         string                `json:"name"`
	Source       ExperimentSource      `json:"source"`
	Description  string                `json:"description,omitempty"`
	Tags         []string              `json:"tags,omitempty"`
	CollectionID string                `json:"collection_id,omitempty"`
	Evaluators   []ExperimentEvaluator `json:"evaluators,omitempty"`
	Metadata     map[string]any        `json:"metadata,omitempty"`
}

type CompleteExperimentOptions struct {
	ScoreCount *int
	Error      string
	Metadata   map[string]any
}

type UpsertTrialRequest struct {
	TrialID        string         `json:"trial_id"`
	TestCaseID     string         `json:"test_case_id"`
	Attempt        int            `json:"attempt"`
	Status         string         `json:"status"`
	ConversationID string         `json:"conversation_id,omitempty"`
	TraceID        string         `json:"trace_id,omitempty"`
	SpanID         string         `json:"span_id,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type UpdateTrialRequest struct {
	Status         string   `json:"status,omitempty"`
	Error          string   `json:"error,omitempty"`
	Cost           *float64 `json:"cost,omitempty"`
	InputTokens    *int     `json:"input_tokens,omitempty"`
	OutputTokens   *int     `json:"output_tokens,omitempty"`
	DurationMillis *int     `json:"duration_ms,omitempty"`
	ConversationID string   `json:"conversation_id,omitempty"`
	TraceID        string   `json:"trace_id,omitempty"`
}

type TestCaseSnapshot struct {
	TestCaseID   string                  `json:"test_case_id"`
	SuiteID      string                  `json:"suite_id,omitempty"`
	SuiteVersion string                  `json:"suite_version,omitempty"`
	Name         string                  `json:"name,omitempty"`
	Description  string                  `json:"description,omitempty"`
	Tags         []string                `json:"tags,omitempty"`
	Category     string                  `json:"category,omitempty"`
	Input        map[string]any          `json:"input,omitempty"`
	Expected     map[string]any          `json:"expected,omitempty"`
	Metadata     map[string]any          `json:"metadata,omitempty"`
	ArtifactRefs []ExperimentArtifactRef `json:"artifact_refs,omitempty"`
}

type ExperimentArtifactRef struct {
	ArtifactID string `json:"artifact_id"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind,omitempty"`
	MIME       string `json:"mime,omitempty"`
}

type TestCaseTrial struct {
	TenantID     string            `json:"tenant_id,omitempty"`
	TrialID      string            `json:"trial_id"`
	ExperimentID string            `json:"experiment_id"`
	TestCaseID   string            `json:"test_case_id"`
	TestCase     *TestCaseSnapshot `json:"test_case,omitempty"`
	Attempt      int               `json:"attempt"`
	Status       string            `json:"status"`

	TraceID        string `json:"trace_id,omitempty"`
	SpanID         string `json:"span_id,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`

	Cost         *float64 `json:"cost,omitempty"`
	InputTokens  *int64   `json:"input_tokens,omitempty"`
	OutputTokens *int64   `json:"output_tokens,omitempty"`
	TotalTokens  *int64   `json:"total_tokens,omitempty"`
	DurationMS   *int64   `json:"duration_ms,omitempty"`

	Error       string         `json:"error,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	CompletedAt *time.Time     `json:"completed_at,omitempty"`
	CreatedAt   *time.Time     `json:"created_at,omitempty"`
	UpdatedAt   *time.Time     `json:"updated_at,omitempty"`
}

type TrialArtifactUpload struct {
	Name    string
	Kind    string
	MIME    string
	Content []byte
}

type TrialArtifact struct {
	TenantID   string     `json:"tenant_id,omitempty"`
	ArtifactID string     `json:"artifact_id"`
	ParentKind string     `json:"parent_kind,omitempty"`
	ParentID   string     `json:"parent_id,omitempty"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	MIME       string     `json:"mime,omitempty"`
	ContentRef string     `json:"content_ref,omitempty"`
	SizeBytes  *int64     `json:"size_bytes,omitempty"`
	CreatedBy  string     `json:"created_by,omitempty"`
	CreatedAt  *time.Time `json:"created_at,omitempty"`
	UpdatedAt  *time.Time `json:"updated_at,omitempty"`
}

type Experiment struct {
	RunID        string                `json:"run_id"`
	Name         string                `json:"name"`
	Source       string                `json:"source"`
	Status       string                `json:"status"`
	TenantID     string                `json:"tenant_id,omitempty"`
	Description  string                `json:"description,omitempty"`
	Tags         []string              `json:"tags,omitempty"`
	CollectionID string                `json:"collection_id,omitempty"`
	Evaluators   []ExperimentEvaluator `json:"evaluators,omitempty"`
	Metadata     map[string]any        `json:"metadata,omitempty"`
	ScoreCount   int                   `json:"score_count,omitempty"`
	Error        string                `json:"error,omitempty"`
	CreatedBy    string                `json:"created_by,omitempty"`
	CreatedAt    *time.Time            `json:"created_at,omitempty"`
	UpdatedAt    *time.Time            `json:"updated_at,omitempty"`
	StartedAt    *time.Time            `json:"started_at,omitempty"`
	CompletedAt  *time.Time            `json:"completed_at,omitempty"`
}

type experimentRunResponse struct {
	Experiment
}

func (r *experimentRunResponse) UnmarshalJSON(data []byte) error {
	var envelope struct {
		Run *Experiment `json:"run"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	if envelope.Run != nil {
		r.Experiment = *envelope.Run
		return nil
	}
	var experiment Experiment
	if err := json.Unmarshal(data, &experiment); err != nil {
		return err
	}
	r.Experiment = experiment
	return nil
}

func (e *Experiment) UnmarshalJSON(data []byte) error {
	type alias Experiment
	var raw struct {
		alias
		ExperimentID string `json:"experiment_id"`
		RunID        string `json:"run_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*e = Experiment(raw.alias)
	if raw.ExperimentID != "" {
		e.RunID = raw.ExperimentID
	} else {
		e.RunID = raw.RunID
	}
	return nil
}

type ExperimentReportSummary struct {
	TestCaseCount  int                `json:"test_case_count"`
	TrialCount     int                `json:"trial_count"`
	CompletedCount int                `json:"completed_count"`
	FailedCount    int                `json:"failed_count"`
	CanceledCount  int                `json:"canceled_count"`
	PassRate       float64            `json:"pass_rate"`
	PassAtK        map[string]float64 `json:"pass_at_k,omitempty"`
	PassPowerK     map[string]float64 `json:"pass_power_k,omitempty"`
	FinalScoreAvg  float64            `json:"final_score_avg"`
	TotalCost      float64            `json:"total_cost"`
	TotalTokens    int                `json:"total_tokens"`
}

type TestCaseResultRowSummary struct {
	TrialCount     int             `json:"trial_count"`
	CompletedCount int             `json:"completed_count"`
	PassAtK        map[string]bool `json:"pass_at_k,omitempty"`
	PassPowerK     map[string]bool `json:"pass_power_k,omitempty"`
	TrialPassRate  *float64        `json:"trial_pass_rate,omitempty"`
}

type GenerationScore struct {
	TenantID             string         `json:"tenant_id,omitempty"`
	ScoreID              string         `json:"score_id"`
	GenerationID         string         `json:"generation_id,omitempty"`
	ConversationID       string         `json:"conversation_id,omitempty"`
	TraceID              string         `json:"trace_id,omitempty"`
	SpanID               string         `json:"span_id,omitempty"`
	TrialID              string         `json:"trial_id,omitempty"`
	TestCaseID           string         `json:"test_case_id,omitempty"`
	GraderConversationID string         `json:"grader_conversation_id,omitempty"`
	GraderGenerationID   string         `json:"grader_generation_id,omitempty"`
	GraderTraceID        string         `json:"grader_trace_id,omitempty"`
	EvaluatorID          string         `json:"evaluator_id"`
	EvaluatorVersion     string         `json:"evaluator_version"`
	EvaluatorDescription string         `json:"evaluator_description,omitempty"`
	RuleID               string         `json:"rule_id,omitempty"`
	ExperimentID         string         `json:"experiment_id,omitempty"`
	ScoreKey             string         `json:"score_key"`
	ScoreType            ScoreType      `json:"score_type,omitempty"`
	Value                ScoreValue     `json:"value"`
	Unit                 string         `json:"unit,omitempty"`
	Passed               *bool          `json:"passed,omitempty"`
	Explanation          string         `json:"explanation,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            *time.Time     `json:"created_at,omitempty"`
	IngestedAt           *time.Time     `json:"ingested_at,omitempty"`
	SourceKind           string         `json:"source_kind,omitempty"`
	SourceID             string         `json:"source_id,omitempty"`
	AgentName            string         `json:"agent_name,omitempty"`
	EffectiveVersion     string         `json:"effective_version,omitempty"`
}

type TestCaseTrialResult struct {
	Trial      TestCaseTrial     `json:"trial"`
	FinalScore *GenerationScore  `json:"final_score,omitempty"`
	Scores     []GenerationScore `json:"scores"`
	Artifacts  []TrialArtifact   `json:"artifacts"`
}

type TestCaseResultRow struct {
	TestCaseID       string                   `json:"test_case_id"`
	TestCaseSnapshot *TestCaseSnapshot        `json:"test_case_snapshot,omitempty"`
	Summary          TestCaseResultRowSummary `json:"summary"`
	Trials           []TestCaseTrialResult    `json:"trials"`
}

type ExperimentReport struct {
	Run     Experiment              `json:"run"`
	Summary ExperimentReportSummary `json:"summary"`
	Rows    []TestCaseResultRow     `json:"rows,omitempty"`
}

func (r *ExperimentReport) UnmarshalJSON(data []byte) error {
	var raw struct {
		Run        *Experiment             `json:"run"`
		Experiment *Experiment             `json:"experiment"`
		Summary    ExperimentReportSummary `json:"summary"`
		Rows       []TestCaseResultRow     `json:"rows"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	if raw.Experiment != nil {
		r.Run = *raw.Experiment
	} else if raw.Run != nil {
		r.Run = *raw.Run
	}
	r.Summary = raw.Summary
	r.Rows = raw.Rows
	return nil
}

type ListExperimentScoresResponse struct {
	Items      []GenerationScore `json:"items"`
	NextCursor string            `json:"next_cursor,omitempty"`
}

type experimentRetryPolicy struct {
	maxRetries     int
	initialBackoff time.Duration
	maxBackoff     time.Duration
	timeout        time.Duration
}

type evalConnectionArgs struct {
	endpoint   string
	insecure   bool
	headers    map[string]string
	pathPrefix string
}

func (c *Client) CreateExperiment(ctx context.Context, req CreateExperimentRequest) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrExperimentValidationFailed)
	}
	if req.Source == "" {
		req.Source = ExperimentSourceExternal
	}
	if req.Source != ExperimentSourceExternal {
		return nil, fmt.Errorf("%w: experiment-run ingest requires source=external", ErrExperimentValidationFailed)
	}

	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + experimentRunsUpsertPath
	var out experimentRunResponse
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, args.headers, serializeUpsertRequest(req), &out, ErrExperimentTransportFailed, "experiment create"); err != nil {
		return nil, err
	}
	return &out.Experiment, nil
}

func (c *Client) GetExperiment(ctx context.Context, runID string) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	args := c.evalArgs()
	endpoint, err := experimentURL(args.endpoint, args.insecure, args.pathPrefix, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.requestEvalJSON(ctx, http.MethodGet, endpoint, args.headers, nil, &out, ErrExperimentTransportFailed, "experiment get"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CompleteExperiment(ctx context.Context, runID string, status ExperimentStatus, opts CompleteExperimentOptions) (*Experiment, error) {
	return c.FinalizeExperiment(ctx, runID, status, opts)
}

func (c *Client) FinalizeExperiment(ctx context.Context, runID string, status ExperimentStatus, opts CompleteExperimentOptions) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedStatus, err := normalizeExperimentFinalStatus(status)
	if err != nil {
		return nil, err
	}
	normalizedRunID := strings.TrimSpace(runID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: run_id is required", ErrExperimentValidationFailed)
	}
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	payload := map[string]any{
		"status": normalizedStatus,
		"source": experimentRunSource(),
	}
	if opts.ScoreCount != nil {
		payload["score_count"] = *opts.ScoreCount
	}
	if strings.TrimSpace(opts.Error) != "" {
		payload["error"] = opts.Error
	}
	var out experimentRunResponse
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) + ":finalize"
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, args.headers, payload, &out, ErrExperimentTransportFailed, "experiment finalize"); err != nil {
		return nil, err
	}
	return &out.Experiment, nil
}

func (c *Client) ExportScores(ctx context.Context, scores []ScoreItem) (*ExportScoresResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if len(scores) == 0 {
		return &ExportScoresResponse{}, nil
	}
	for _, score := range scores {
		if err := validateScore(score); err != nil {
			return nil, err
		}
	}

	base, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreExportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + scoresExportPath
	payloadScores := make([]map[string]any, 0, len(scores))
	for _, score := range scores {
		payloadScores = append(payloadScores, serializeScore(score))
	}
	payload := map[string]any{"scores": payloadScores}
	var out ExportScoresResponse
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, c.config.GenerationExport.Headers, payload, &out, ErrScoreExportFailed, "score export"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpsertTrial(ctx context.Context, experimentID string, req UpsertTrialRequest) (*TestCaseTrial, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID := strings.TrimSpace(experimentID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: experiment_id is required for trial create", ErrExperimentValidationFailed)
	}
	if strings.TrimSpace(req.TrialID) == "" {
		return nil, fmt.Errorf("%w: trial_id is required", ErrExperimentValidationFailed)
	}
	if strings.TrimSpace(req.TestCaseID) == "" {
		return nil, fmt.Errorf("%w: test_case_id is required", ErrExperimentValidationFailed)
	}
	if req.Attempt == 0 {
		req.Attempt = 1
	}
	if strings.TrimSpace(req.Status) == "" {
		req.Status = string(TrialStatusRunning)
	}
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) + "/trials"
	var out TestCaseTrial
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, args.headers, req, &out, ErrExperimentTransportFailed, "test case trial create"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateTrial(ctx context.Context, experimentID, trialID string, req UpdateTrialRequest) (*TestCaseTrial, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID := strings.TrimSpace(experimentID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: experiment_id is required for trial update", ErrExperimentValidationFailed)
	}
	normalizedTrialID := strings.TrimSpace(trialID)
	if normalizedTrialID == "" {
		return nil, fmt.Errorf("%w: trial_id is required", ErrExperimentValidationFailed)
	}
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) + "/trials/" + url.PathEscape(normalizedTrialID)
	var out TestCaseTrial
	if err := c.requestEvalJSON(ctx, http.MethodPatch, endpoint, args.headers, req, &out, ErrExperimentTransportFailed, "test case trial update"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UploadTrialArtifact(ctx context.Context, experimentID, trialID string, artifact TrialArtifactUpload) (*TrialArtifact, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID := strings.TrimSpace(experimentID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: experiment_id is required", ErrExperimentValidationFailed)
	}
	normalizedTrialID := strings.TrimSpace(trialID)
	if normalizedTrialID == "" {
		return nil, fmt.Errorf("%w: trial_id is required", ErrExperimentValidationFailed)
	}
	name := strings.TrimSpace(artifact.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrExperimentValidationFailed)
	}
	kind := strings.TrimSpace(artifact.Kind)
	if kind == "" {
		return nil, fmt.Errorf("%w: kind is required", ErrExperimentValidationFailed)
	}
	if len(artifact.Content) == 0 {
		return nil, fmt.Errorf("%w: content is required", ErrExperimentValidationFailed)
	}
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	values := url.Values{"name": []string{name}, "kind": []string{kind}, "mime": []string{strings.TrimSpace(artifact.MIME)}}
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) + "/trials/" + url.PathEscape(normalizedTrialID) + "/artifacts:upload?" + values.Encode()
	headers := cloneTags(args.headers)
	if headers == nil {
		headers = map[string]string{}
	}
	contentType := strings.TrimSpace(artifact.MIME)
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	headers["Content-Type"] = contentType
	var out TrialArtifact
	if err := c.requestEvalBytesJSON(ctx, http.MethodPost, endpoint, headers, artifact.Content, &out, ErrExperimentTransportFailed, "trial artifact upload"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListExperimentScores(ctx context.Context, runID string, limit int, cursor string) (*ListExperimentScoresResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if limit <= 0 {
		limit = 50
	}
	args := c.evalArgs()
	base, err := experimentURL(args.endpoint, args.insecure, args.pathPrefix, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	values := url.Values{"limit": []string{fmt.Sprintf("%d", limit)}}
	if strings.TrimSpace(cursor) != "" {
		values.Set("cursor", strings.TrimSpace(cursor))
	}
	var out ListExperimentScoresResponse
	if err := c.requestEvalJSON(ctx, http.MethodGet, base+"/scores?"+values.Encode(), args.headers, nil, &out, ErrExperimentTransportFailed, "experiment scores list"); err != nil {
		return nil, err
	}
	if out.NextCursor == "0" {
		out.NextCursor = ""
	}
	return &out, nil
}

func (c *Client) GetExperimentReport(ctx context.Context, runID string) (*ExperimentReport, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	args := c.evalArgs()
	base, err := experimentURL(args.endpoint, args.insecure, args.pathPrefix, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out ExperimentReport
	if err := c.requestEvalJSON(ctx, http.MethodGet, base+"/report", args.headers, nil, &out, ErrExperimentTransportFailed, "experiment report"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ExperimentURL(runID string) string {
	if c == nil {
		return ""
	}
	normalized := strings.TrimSpace(runID)
	base, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		base = ""
	}
	template := strings.TrimSpace(os.Getenv(envExperimentURLTemplate))
	if template != "" {
		out := strings.ReplaceAll(template, "{run_id}", normalized)
		return strings.ReplaceAll(out, "{base}", base)
	}
	return strings.TrimRight(base, "/") + "/a/grafana-sigil-app/offline-experiments/experiments/" + url.PathEscape(normalized)
}

func (c *Client) evalArgs() evalConnectionArgs {
	endpoint := c.config.API.Endpoint
	pathPrefix := defaultEvalPathPrefix
	headers := cloneTags(c.config.GenerationExport.Headers)
	return evalConnectionArgs{
		endpoint:   endpoint,
		insecure:   insecureValue(c.config.GenerationExport.Insecure),
		headers:    headers,
		pathPrefix: pathPrefix,
	}
}

func (c *Client) experimentRetryPolicy() experimentRetryPolicy {
	cfg := c.config.GenerationExport
	policy := experimentRetryPolicy{
		maxRetries:     cfg.MaxRetries,
		initialBackoff: cfg.InitialBackoff,
		maxBackoff:     cfg.MaxBackoff,
		timeout:        defaultExperimentRetryTimeout,
	}
	if policy.maxRetries < 0 {
		policy.maxRetries = 0
	}
	if policy.initialBackoff < 0 {
		policy.initialBackoff = 0
	}
	if policy.maxBackoff < 0 {
		policy.maxBackoff = 0
	}
	return policy
}

func (c *Client) requestEvalJSON(ctx context.Context, method, endpoint string, headers map[string]string, payload any, out any, transportSentinel error, label string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := c.experimentRetryPolicy()
	var data []byte
	if payload != nil {
		var err error
		data, err = json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("%w: marshal %s request: %v", transportSentinel, label, err)
		}
	}

	attempt := 0
	backoff := policy.initialBackoff
	for {
		reqCtx := ctx
		var cancel context.CancelFunc
		if policy.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, policy.timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(data))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("%w: build %s request: %v", transportSentinel, label, err)
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}

		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if attempt < policy.maxRetries {
				if sleepBackoff(ctx, backoff) != nil {
					return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
				}
				attempt++
				backoff = nextBackoff(backoff, policy)
				continue
			}
			return fmt.Errorf("%w: %s request: %v", transportSentinel, label, err)
		}

		body, readErr := readLimitedBody(httpResp.Body)
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s response: %v", transportSentinel, label, readErr)
		}
		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			if len(strings.TrimSpace(string(body))) == 0 {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w: decode %s response: %v", transportSentinel, label, err)
			}
			return nil
		}

		bodyText := responseErrorText(body, httpResp.StatusCode)
		switch httpResp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			if label == "score export" {
				return fmt.Errorf("%w: %s", ErrScoreValidationFailed, bodyText)
			}
			return fmt.Errorf("%w: %s", ErrExperimentValidationFailed, bodyText)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrExperimentNotFound, bodyText)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrExperimentConflict, bodyText)
		}
		if (httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= http.StatusInternalServerError) && attempt < policy.maxRetries {
			if sleepBackoff(ctx, backoff) != nil {
				return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
			}
			attempt++
			backoff = nextBackoff(backoff, policy)
			continue
		}
		return fmt.Errorf("%w: status %d: %s", transportSentinel, httpResp.StatusCode, bodyText)
	}
}

func (c *Client) requestEvalBytesJSON(ctx context.Context, method, endpoint string, headers map[string]string, payload []byte, out any, transportSentinel error, label string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	policy := c.experimentRetryPolicy()
	attempt := 0
	backoff := policy.initialBackoff
	for {
		reqCtx := ctx
		var cancel context.CancelFunc
		if policy.timeout > 0 {
			reqCtx, cancel = context.WithTimeout(ctx, policy.timeout)
		}
		req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bytes.NewReader(payload))
		if err != nil {
			if cancel != nil {
				cancel()
			}
			return fmt.Errorf("%w: build %s request: %v", transportSentinel, label, err)
		}
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		httpResp, err := http.DefaultClient.Do(req)
		if err != nil {
			if cancel != nil {
				cancel()
			}
			if attempt < policy.maxRetries {
				if sleepBackoff(ctx, backoff) != nil {
					return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
				}
				attempt++
				backoff = nextBackoff(backoff, policy)
				continue
			}
			return fmt.Errorf("%w: %s request: %v", transportSentinel, label, err)
		}
		body, readErr := readLimitedBody(httpResp.Body)
		_ = httpResp.Body.Close()
		if cancel != nil {
			cancel()
		}
		if readErr != nil {
			return fmt.Errorf("%w: read %s response: %v", transportSentinel, label, readErr)
		}
		if httpResp.StatusCode >= http.StatusOK && httpResp.StatusCode < http.StatusMultipleChoices {
			if len(strings.TrimSpace(string(body))) == 0 || out == nil {
				return nil
			}
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("%w: decode %s response: %v", transportSentinel, label, err)
			}
			return nil
		}
		bodyText := responseErrorText(body, httpResp.StatusCode)
		switch httpResp.StatusCode {
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", ErrExperimentValidationFailed, bodyText)
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", ErrExperimentNotFound, bodyText)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", ErrExperimentConflict, bodyText)
		}
		if (httpResp.StatusCode == http.StatusTooManyRequests || httpResp.StatusCode >= http.StatusInternalServerError) && attempt < policy.maxRetries {
			if sleepBackoff(ctx, backoff) != nil {
				return fmt.Errorf("%w: %v", transportSentinel, ctx.Err())
			}
			attempt++
			backoff = nextBackoff(backoff, policy)
			continue
		}
		return fmt.Errorf("%w: status %d: %s", transportSentinel, httpResp.StatusCode, bodyText)
	}
}

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
	if experimentID := score.ResolvedExperimentID(); experimentID != "" {
		out["experiment_id"] = experimentID
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

func experimentsURL(endpoint string, insecure bool, pathPrefix string) (string, error) {
	base, err := baseURLFromAPIEndpoint(endpoint, insecure)
	if err != nil {
		return "", err
	}
	prefix := strings.Trim(strings.TrimSpace(pathPrefix), "/")
	if prefix == "" {
		prefix = strings.Trim(defaultEvalPathPrefix, "/")
	}
	return strings.TrimRight(base, "/") + "/" + prefix + evalExperimentsSuffix, nil
}

func experimentURL(endpoint string, insecure bool, pathPrefix string, runID string) (string, error) {
	normalized := strings.TrimSpace(runID)
	if normalized == "" {
		return "", errors.New("run_id is required")
	}
	base, err := experimentsURL(endpoint, insecure, pathPrefix)
	if err != nil {
		return "", err
	}
	return base + "/" + url.PathEscape(normalized), nil
}

func readLimitedBody(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, int64(maxEvalResponseBytes)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxEvalResponseBytes {
		return nil, errors.New("response too large")
	}
	return raw, nil
}

func responseErrorText(body []byte, status int) string {
	text := strings.TrimSpace(string(body))
	if text != "" {
		return text
	}
	return fmt.Sprintf("status %d", status)
}

func sleepBackoff(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func nextBackoff(current time.Duration, policy experimentRetryPolicy) time.Duration {
	next := current * 2
	if current <= 0 {
		next = policy.initialBackoff
	}
	if policy.maxBackoff > 0 && next > policy.maxBackoff {
		return policy.maxBackoff
	}
	return next
}
