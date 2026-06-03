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
	evalExperimentsSuffix = "/eval/experiments"
	defaultEvalPathPrefix = "/api/v1"
	scoresExportPath      = "/api/v1/scores:export"
	maxEvalResponseBytes  = 8 << 20

	envEvalEndpoint               = "SIGIL_EVAL_ENDPOINT"
	envEvalPathPrefix             = "SIGIL_EVAL_PATH_PREFIX"
	envEvalAuthToken              = "SIGIL_EVAL_AUTH_TOKEN"
	envExperimentURLTemplate      = "SIGIL_EXPERIMENT_URL_TEMPLATE"
	defaultExperimentRetryTimeout = 30 * time.Second
)

type ExperimentStatus string

const (
	ExperimentStatusRunning   ExperimentStatus = "running"
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

type ScoreItem struct {
	ScoreID          string         `json:"score_id"`
	GenerationID     string         `json:"generation_id"`
	EvaluatorID      string         `json:"evaluator_id"`
	EvaluatorVersion string         `json:"evaluator_version"`
	ScoreKey         string         `json:"score_key"`
	Value            ScoreValue     `json:"value"`
	ConversationID   string         `json:"conversation_id,omitempty"`
	TraceID          string         `json:"trace_id,omitempty"`
	SpanID           string         `json:"span_id,omitempty"`
	RuleID           string         `json:"rule_id,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	Passed           *bool          `json:"passed,omitempty"`
	Explanation      string         `json:"explanation,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        *time.Time     `json:"created_at,omitempty"`
	Source           *ScoreSource   `json:"source,omitempty"`
}

type ExportScoreResult struct {
	ScoreID  string `json:"score_id"`
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

type ExportScoresResponse struct {
	Results []ExportScoreResult `json:"results"`
}

func (r ExportScoresResponse) AcceptedCount() int {
	count := 0
	for _, result := range r.Results {
		if result.Accepted {
			count++
		}
	}
	return count
}

func (r ExportScoresResponse) Rejected() []ExportScoreResult {
	var out []ExportScoreResult
	for _, result := range r.Results {
		if !result.Accepted {
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

type UpdateExperimentRequest struct {
	Name        *string           `json:"name,omitempty"`
	Description *string           `json:"description,omitempty"`
	Tags        *[]string         `json:"tags,omitempty"`
	Status      *ExperimentStatus `json:"status,omitempty"`
	Metadata    map[string]any    `json:"metadata,omitempty"`
	Error       *string           `json:"error,omitempty"`
	ScoreCount  *int              `json:"score_count,omitempty"`
}

type CompleteExperimentOptions struct {
	ScoreCount *int
	Error      string
	Metadata   map[string]any
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

type ExperimentReportSummary struct {
	NConversations int     `json:"n_conversations"`
	NGenerations   int     `json:"n_generations"`
	NScores        int     `json:"n_scores"`
	PassRate       float64 `json:"pass_rate"`
	MeanScore      float64 `json:"mean_score"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	TotalTokens    int     `json:"total_tokens"`
}

type ExperimentReport struct {
	Run        Experiment              `json:"run"`
	Summary    ExperimentReportSummary `json:"summary"`
	Breakdowns map[string]any          `json:"breakdowns,omitempty"`
	Points     []map[string]any        `json:"points,omitempty"`
}

type ListExperimentScoresResponse struct {
	Items      []map[string]any `json:"items"`
	NextCursor string           `json:"next_cursor,omitempty"`
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

	args := c.evalArgs()
	endpoint, err := experimentsURL(args.endpoint, args.insecure, args.pathPrefix)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentTransportFailed, err)
	}
	var out Experiment
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, args.headers, req, &out, ErrExperimentTransportFailed, "experiment create"); err != nil {
		return nil, err
	}
	return &out, nil
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

func (c *Client) UpdateExperiment(ctx context.Context, runID string, req UpdateExperimentRequest) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	args := c.evalArgs()
	endpoint, err := experimentURL(args.endpoint, args.insecure, args.pathPrefix, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.requestEvalJSON(ctx, http.MethodPatch, endpoint, args.headers, req, &out, ErrExperimentTransportFailed, "experiment update"); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CompleteExperiment(ctx context.Context, runID string, status ExperimentStatus, opts CompleteExperimentOptions) (*Experiment, error) {
	req := UpdateExperimentRequest{Status: &status, ScoreCount: opts.ScoreCount, Metadata: opts.Metadata}
	if opts.Error != "" {
		req.Error = &opts.Error
	}
	return c.UpdateExperiment(ctx, runID, req)
}

func (c *Client) CancelExperiment(ctx context.Context, runID string) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	args := c.evalArgs()
	base, err := experimentURL(args.endpoint, args.insecure, args.pathPrefix, runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.requestEvalJSON(ctx, http.MethodPost, base+":cancel", args.headers, map[string]any{}, &out, ErrExperimentTransportFailed, "experiment cancel"); err != nil {
		return nil, err
	}
	return &out, nil
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
	payload := map[string]any{"scores": scores}
	var out ExportScoresResponse
	if err := c.requestEvalJSON(ctx, http.MethodPost, endpoint, c.config.GenerationExport.Headers, payload, &out, ErrScoreExportFailed, "score export"); err != nil {
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
	linkEndpoint := strings.TrimSpace(os.Getenv(envEvalEndpoint))
	if linkEndpoint == "" {
		linkEndpoint = c.config.API.Endpoint
	}
	base, err := baseURLFromAPIEndpoint(linkEndpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		base = ""
	}
	template := strings.TrimSpace(os.Getenv(envExperimentURLTemplate))
	if template != "" {
		out := strings.ReplaceAll(template, "{run_id}", normalized)
		return strings.ReplaceAll(out, "{base}", base)
	}
	return strings.TrimRight(base, "/") + "/a/grafana-sigil-app/evaluation/experiments/" + url.PathEscape(normalized)
}

func (c *Client) evalArgs() evalConnectionArgs {
	endpoint := strings.TrimSpace(os.Getenv(envEvalEndpoint))
	if endpoint == "" {
		endpoint = c.config.API.Endpoint
	}
	pathPrefix := strings.TrimSpace(os.Getenv(envEvalPathPrefix))
	if pathPrefix == "" {
		pathPrefix = defaultEvalPathPrefix
	}
	headers := cloneTags(c.config.GenerationExport.Headers)
	token := strings.TrimSpace(os.Getenv(envEvalAuthToken))
	if token != "" {
		if !strings.HasPrefix(strings.ToLower(token), "bearer ") {
			token = "Bearer " + token
		}
		headers = map[string]string{authorizationHeaderName: token}
	}
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

func validateScore(score ScoreItem) error {
	var missing []string
	for name, value := range map[string]string{
		"score_id":          score.ScoreID,
		"generation_id":     score.GenerationID,
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
