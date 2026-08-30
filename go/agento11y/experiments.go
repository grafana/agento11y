package agento11y

import (
	"context"
	"fmt"
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

	envExperimentURLTemplatePreferred = "AGENTO11Y_EXPERIMENT_URL_TEMPLATE"
	envExperimentURLTemplate          = "SIGIL_EXPERIMENT_URL_TEMPLATE"
	defaultExperimentRetryTimeout     = 30 * time.Second
)

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

// TriggerTrialEvaluation queues a stored tenant evaluator against the
// conversation bound to a trial. The trial must exist, belong to a running
// external experiment, and already have a conversation.
//
// A queued or claimed evaluation comes back with a non-terminal status; poll
// GetTrialEvaluation until Status.Terminal() is true. The evaluation is keyed by
// trial, conversation, evaluator, and resolved evaluator version, so triggering
// the same combination returns the existing evaluation instead of running it
// twice, and requeues it once it has failed.
//
// Experimental: Config.EnableExperimentalFeatures takes precedence over
// AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES. A closed gate returns
// ErrExperimentalFeatureDisabled.
func (c *Client) TriggerTrialEvaluation(ctx context.Context, experimentID, trialID string, req TriggerTrialEvaluationRequest) (*TrialEvaluation, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if err := c.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		return nil, err
	}
	normalizedRunID := strings.TrimSpace(experimentID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: experiment_id is required for trial evaluation", ErrExperimentValidationFailed)
	}
	normalizedTrialID := strings.TrimSpace(trialID)
	if normalizedTrialID == "" {
		return nil, fmt.Errorf("%w: trial_id is required", ErrExperimentValidationFailed)
	}
	req.EvaluatorID = strings.TrimSpace(req.EvaluatorID)
	if req.EvaluatorID == "" {
		return nil, fmt.Errorf("%w: evaluator_id is required", ErrExperimentValidationFailed)
	}
	req.EvaluatorVersion = strings.TrimSpace(req.EvaluatorVersion)
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) +
		"/trials/" + escapeTrialPathSegment(normalizedTrialID) + ":evaluate"
	var out TrialEvaluation
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, args.headers, req, &out, ErrExperimentTransportFailed, "trial evaluation trigger"); err != nil {
		return nil, err
	}
	if err := validateTrialEvaluation(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetTrialEvaluation reads durable status for a triggered trial evaluation.
//
// Experimental: Config.EnableExperimentalFeatures takes precedence over
// AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES. A closed gate returns
// ErrExperimentalFeatureDisabled.
func (c *Client) GetTrialEvaluation(ctx context.Context, experimentID, trialID, evaluationID string) (*TrialEvaluation, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	if err := c.RequireExperimental(FeatureCloudTrialEvaluation); err != nil {
		return nil, err
	}
	normalizedRunID := strings.TrimSpace(experimentID)
	if normalizedRunID == "" {
		return nil, fmt.Errorf("%w: experiment_id is required for trial evaluation", ErrExperimentValidationFailed)
	}
	normalizedTrialID := strings.TrimSpace(trialID)
	if normalizedTrialID == "" {
		return nil, fmt.Errorf("%w: trial_id is required", ErrExperimentValidationFailed)
	}
	normalizedEvaluationID := strings.TrimSpace(evaluationID)
	if normalizedEvaluationID == "" {
		return nil, fmt.Errorf("%w: evaluation_id is required", ErrExperimentValidationFailed)
	}
	args := c.evalArgs()
	base, err := baseURLFromAPIEndpoint(args.endpoint, args.insecure)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	endpoint := strings.TrimRight(base, "/") + experimentRunsPrefix + "/" + url.PathEscape(normalizedRunID) +
		"/trials/" + escapeTrialPathSegment(normalizedTrialID) +
		"/evaluations/" + url.PathEscape(normalizedEvaluationID)
	var out TrialEvaluation
	if err := c.requestEvalJSON(ctx, http.MethodGet, endpoint, args.headers, nil, &out, ErrExperimentTransportFailed, "trial evaluation status"); err != nil {
		return nil, err
	}
	if err := validateTrialEvaluation(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// escapeTrialPathSegment escapes a trial id for the trial evaluation routes.
// url.PathEscape leaves ":" alone, and the ingest router reads the ":evaluate"
// verb off the raw path segment before it decodes the id, so an unescaped colon
// in a trial id would change which route the request reaches.
func escapeTrialPathSegment(value string) string {
	return strings.ReplaceAll(url.PathEscape(value), ":", "%3A")
}

// validateTrialEvaluation rejects a response the SDK cannot act on. Status is a
// named string type, so an unrecognized terminal state would otherwise read as
// non-terminal and poll until the caller's deadline. A missing evaluation ID
// would turn the next status read into a validation error that blames the
// caller's arguments.
func validateTrialEvaluation(out *TrialEvaluation) error {
	switch out.Status {
	case TrialEvaluationStatusQueued, TrialEvaluationStatusClaimed,
		TrialEvaluationStatusSuccess, TrialEvaluationStatusFailed:
	default:
		return fmt.Errorf("%w: unsupported evaluation status %q", ErrExperimentTransportFailed, string(out.Status))
	}
	if strings.TrimSpace(out.EvaluationID) == "" {
		return fmt.Errorf("%w: evaluation response carries no evaluation_id", ErrExperimentTransportFailed)
	}
	return nil
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
	template := firstNonBlank(os.Getenv(envExperimentURLTemplatePreferred), os.Getenv(envExperimentURLTemplate))
	if template != "" {
		out := strings.ReplaceAll(template, "{run_id}", normalized)
		return strings.ReplaceAll(out, "{base}", base)
	}
	return strings.TrimRight(base, "/") + "/a/grafana-agento11y-app/offline-experiments/experiments/" + url.PathEscape(normalized)
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
		timeout:        c.config.ExperimentRetryTimeout,
	}
	if policy.timeout <= 0 {
		policy.timeout = defaultExperimentRetryTimeout
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
