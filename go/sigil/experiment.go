package sigil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	experimentSourceExternal   = "external"
	experimentStatusRunning    = "running"
	experimentStatusSucceeded  = "succeeded"
	experimentStatusFailed     = "failed"
	experimentStatusCanceled   = "canceled"
	maxExperimentRunIDLen      = 255
	maxExperimentNameLen       = 255
	maxExperimentErrorBytes    = 4096
	maxExperimentMetadataBytes = 64 * 1024
	maxScoreIDLen              = 255
	maxScoreGenerationIDLen    = 255
	maxScoreConversationIDLen  = 255
	maxScoreEvaluatorIDLen     = 255
	maxScoreScoreKeyLen        = 255
	maxScoreExplanationBytes   = 8192
	maxScoreMetadataBytes      = 64 * 1024
)

type ExperimentEvaluator struct {
	ID       string `json:"id"`
	Selector string `json:"selector"`
}

type CreateExperimentRequest struct {
	RunID        string                `json:"run_id,omitempty"`
	Name         string                `json:"name"`
	Source       string                `json:"source"`
	CollectionID string                `json:"collection_id,omitempty"`
	Evaluators   []ExperimentEvaluator `json:"evaluators,omitempty"`
	Metadata     map[string]any        `json:"metadata,omitempty"`
}

type UpdateExperimentRequest struct {
	Name     *string        `json:"name,omitempty"`
	Status   *string        `json:"status,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
	Error    *string        `json:"error,omitempty"`
}

type Experiment struct {
	TenantID     string                `json:"tenant_id"`
	RunID        string                `json:"run_id"`
	Name         string                `json:"name"`
	Source       string                `json:"source"`
	Status       string                `json:"status"`
	CollectionID string                `json:"collection_id,omitempty"`
	Evaluators   []ExperimentEvaluator `json:"evaluators,omitempty"`
	Metadata     map[string]any        `json:"metadata,omitempty"`
	ScoreCount   int                   `json:"score_count"`
	Error        string                `json:"error,omitempty"`
	CreatedBy    string                `json:"created_by,omitempty"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
	StartedAt    *time.Time            `json:"started_at,omitempty"`
	CompletedAt  *time.Time            `json:"completed_at,omitempty"`
}

type ListExperimentsResponse struct {
	Items      []Experiment `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}

type ExperimentReport struct {
	Run        Experiment                 `json:"run"`
	Summary    ExperimentReportSummary    `json:"summary"`
	Breakdowns ExperimentReportBreakdowns `json:"breakdowns"`
	Points     []ExperimentReportPoint    `json:"points"`
}

type ExperimentReportSummary struct {
	NConversations int     `json:"n_conversations"`
	NGenerations   int     `json:"n_generations"`
	NScores        int     `json:"n_scores"`
	PassRate       float64 `json:"pass_rate"`
	MeanScore      float64 `json:"mean_score"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	TotalTokens    int64   `json:"total_tokens"`
}

type ExperimentReportBreakdowns struct {
	ByTask              []ExperimentReportBreakdown `json:"by_task"`
	ByCategory          []ExperimentReportBreakdown `json:"by_category"`
	ByEvaluator         []ExperimentReportBreakdown `json:"by_evaluator"`
	ByScoreKey          []ExperimentReportBreakdown `json:"by_score_key"`
	ByEvaluatorScoreKey []ExperimentReportBreakdown `json:"by_evaluator_score_key"`
}

type ExperimentReportBreakdown struct {
	Key          string  `json:"key"`
	Count        int     `json:"count"`
	PassRate     float64 `json:"pass_rate"`
	MeanScore    float64 `json:"mean_score"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	TotalTokens  int64   `json:"total_tokens"`
}

type ExperimentReportPoint struct {
	ConversationID   string         `json:"conversation_id"`
	GenerationID     string         `json:"generation_id"`
	ScoreID          string         `json:"score_id"`
	TaskID           string         `json:"task_id,omitempty"`
	TaskCategory     string         `json:"task_category,omitempty"`
	TrialID          string         `json:"trial_id,omitempty"`
	EvaluatorID      string         `json:"evaluator_id"`
	EvaluatorVersion string         `json:"evaluator_version,omitempty"`
	ScoreKey         string         `json:"score_key"`
	ScoreType        string         `json:"score_type"`
	Value            ScoreValue     `json:"value"`
	ValueNumber      *float64       `json:"value_number,omitempty"`
	Passed           *bool          `json:"passed,omitempty"`
	Explanation      string         `json:"explanation,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CostUSD          float64        `json:"cost_usd,omitempty"`
	Tokens           int64          `json:"tokens,omitempty"`
	CreatedAt        time.Time      `json:"created_at"`
}

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
	ConversationID   string         `json:"conversation_id,omitempty"`
	TraceID          string         `json:"trace_id,omitempty"`
	SpanID           string         `json:"span_id,omitempty"`
	EvaluatorID      string         `json:"evaluator_id"`
	EvaluatorVersion string         `json:"evaluator_version"`
	RuleID           string         `json:"rule_id,omitempty"`
	RunID            string         `json:"run_id,omitempty"`
	ScoreKey         string         `json:"score_key"`
	Value            ScoreValue     `json:"value"`
	Passed           *bool          `json:"passed,omitempty"`
	Explanation      string         `json:"explanation,omitempty"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	CreatedAt        time.Time      `json:"created_at,omitempty"`
	Source           ScoreSource    `json:"source,omitempty"`
}

type ExportScoresRequest struct {
	Scores []ScoreItem `json:"scores"`
}

type ExportScoreResult struct {
	ScoreID  string `json:"score_id"`
	Accepted bool   `json:"accepted"`
	Error    string `json:"error,omitempty"`
}

type ExportScoresResponse struct {
	Results []ExportScoreResult `json:"results"`
}

type ListExperimentScoresResponse struct {
	Items      []ScoreItem `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

func (c *Client) CreateExperiment(ctx context.Context, input CreateExperimentRequest) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalized, err := normalizeCreateExperimentRequest(input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.doSigilAPIJSON(ctx, http.MethodPost, "/api/v1/eval/experiments", normalized, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetExperiment(ctx context.Context, runID string) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID, err := normalizeRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.doSigilAPIJSON(ctx, http.MethodGet, "/api/v1/eval/experiments/"+url.PathEscape(normalizedRunID), nil, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) UpdateExperiment(ctx context.Context, runID string, input UpdateExperimentRequest) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID, err := normalizeRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	normalized, err := normalizeUpdateExperimentRequest(input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.doSigilAPIJSON(ctx, http.MethodPatch, "/api/v1/eval/experiments/"+url.PathEscape(normalizedRunID), normalized, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) CancelExperiment(ctx context.Context, runID string) (*Experiment, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID, err := normalizeRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out Experiment
	if err := c.doSigilAPIJSON(ctx, http.MethodPost, "/api/v1/eval/experiments/"+url.PathEscape(normalizedRunID)+"/cancel", nil, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListExperiments(ctx context.Context, limit int, cursor string) (*ListExperimentsResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	path := "/api/v1/eval/experiments" + paginationQuery(limit, cursor)
	var out ListExperimentsResponse
	if err := c.doSigilAPIJSON(ctx, http.MethodGet, path, nil, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetExperimentReport(ctx context.Context, runID string) (*ExperimentReport, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID, err := normalizeRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	var out ExperimentReport
	if err := c.doSigilAPIJSON(ctx, http.MethodGet, "/api/v1/eval/experiments/"+url.PathEscape(normalizedRunID)+"/report", nil, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ListExperimentScores(ctx context.Context, runID string, limit int, cursor string) (*ListExperimentScoresResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalizedRunID, err := normalizeRunID(runID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrExperimentValidationFailed, err)
	}
	path := "/api/v1/eval/experiments/" + url.PathEscape(normalizedRunID) + "/scores" + paginationQuery(limit, cursor)
	var out ListExperimentScoresResponse
	if err := c.doSigilAPIJSON(ctx, http.MethodGet, path, nil, &out, ErrExperimentTransportFailed, experimentStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ExportScores(ctx context.Context, input ExportScoresRequest) (*ExportScoresResponse, error) {
	if c == nil {
		return nil, ErrNilClient
	}
	normalized, err := normalizeExportScoresRequest(input)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrScoreValidationFailed, err)
	}
	var out ExportScoresResponse
	if err := c.doSigilAPIJSON(ctx, http.MethodPost, "/api/v1/scores:export", normalized, &out, ErrScoreTransportFailed, scoreExportStatusError); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) doSigilAPIJSON(ctx context.Context, method, path string, input any, output any, transportError error, mapStatus func(int, string) error) error {
	baseURL, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		return fmt.Errorf("%w: %v", transportError, err)
	}
	endpoint := strings.TrimRight(baseURL, "/") + path

	var body io.Reader
	if input != nil {
		payload, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("%w: marshal request: %v", transportError, err)
		}
		body = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return fmt.Errorf("%w: build request: %v", transportError, err)
	}
	if input != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for key, value := range c.config.GenerationExport.Headers {
		request.Header.Set(key, value)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%w: request failed: %v", transportError, err)
	}
	defer func() {
		_ = response.Body.Close()
	}()

	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("%w: read response: %v", transportError, err)
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if mapStatus != nil {
			return mapStatus(response.StatusCode, strings.TrimSpace(string(responseBody)))
		}
		return fmt.Errorf("%w: status %d: %s", transportError, response.StatusCode, statusErrorText(responseBody, response.StatusCode))
	}

	if output == nil {
		return nil
	}
	if err := json.Unmarshal(responseBody, output); err != nil {
		return fmt.Errorf("%w: decode response: %v", transportError, err)
	}
	return nil
}

func normalizeCreateExperimentRequest(input CreateExperimentRequest) (CreateExperimentRequest, error) {
	normalized := CreateExperimentRequest{
		RunID:        strings.TrimSpace(input.RunID),
		Name:         strings.TrimSpace(input.Name),
		Source:       strings.TrimSpace(input.Source),
		CollectionID: strings.TrimSpace(input.CollectionID),
		Evaluators:   normalizeExperimentEvaluators(input.Evaluators),
		Metadata:     input.Metadata,
	}
	if normalized.Source == "" {
		normalized.Source = experimentSourceExternal
	}
	if normalized.RunID != "" && len(normalized.RunID) > maxExperimentRunIDLen {
		return CreateExperimentRequest{}, fmt.Errorf("run_id is too long")
	}
	if normalized.Name == "" {
		return CreateExperimentRequest{}, fmt.Errorf("name is required")
	}
	if len(normalized.Name) > maxExperimentNameLen {
		return CreateExperimentRequest{}, fmt.Errorf("name is too long")
	}
	if normalized.Source != experimentSourceExternal && normalized.Source != "collection" {
		return CreateExperimentRequest{}, fmt.Errorf("source must be external or collection")
	}
	if normalized.Metadata != nil {
		if err := validateJSONSize(normalized.Metadata, maxExperimentMetadataBytes, "metadata"); err != nil {
			return CreateExperimentRequest{}, err
		}
	}
	return normalized, nil
}

func normalizeUpdateExperimentRequest(input UpdateExperimentRequest) (UpdateExperimentRequest, error) {
	normalized := UpdateExperimentRequest{
		Metadata: input.Metadata,
	}
	if input.Name != nil {
		name := strings.TrimSpace(*input.Name)
		if name == "" {
			return UpdateExperimentRequest{}, fmt.Errorf("name must not be empty")
		}
		if len(name) > maxExperimentNameLen {
			return UpdateExperimentRequest{}, fmt.Errorf("name is too long")
		}
		normalized.Name = &name
	}
	if input.Status != nil {
		status := strings.TrimSpace(*input.Status)
		switch status {
		case experimentStatusRunning, experimentStatusSucceeded, experimentStatusFailed, experimentStatusCanceled:
			normalized.Status = &status
		default:
			return UpdateExperimentRequest{}, fmt.Errorf("invalid status")
		}
	}
	if input.Error != nil {
		errText := strings.TrimSpace(*input.Error)
		if len([]byte(errText)) > maxExperimentErrorBytes {
			return UpdateExperimentRequest{}, fmt.Errorf("error is too long")
		}
		normalized.Error = &errText
	}
	if normalized.Metadata != nil {
		if err := validateJSONSize(normalized.Metadata, maxExperimentMetadataBytes, "metadata"); err != nil {
			return UpdateExperimentRequest{}, err
		}
	}
	return normalized, nil
}

func normalizeExportScoresRequest(input ExportScoresRequest) (ExportScoresRequest, error) {
	if len(input.Scores) == 0 {
		return ExportScoresRequest{}, fmt.Errorf("scores are required")
	}
	out := ExportScoresRequest{Scores: make([]ScoreItem, 0, len(input.Scores))}
	for _, item := range input.Scores {
		normalized, err := normalizeScoreItem(item)
		if err != nil {
			return ExportScoresRequest{}, err
		}
		out.Scores = append(out.Scores, normalized)
	}
	return out, nil
}

func normalizeScoreItem(item ScoreItem) (ScoreItem, error) {
	normalized := ScoreItem{
		ScoreID:          strings.TrimSpace(item.ScoreID),
		GenerationID:     strings.TrimSpace(item.GenerationID),
		ConversationID:   strings.TrimSpace(item.ConversationID),
		TraceID:          strings.TrimSpace(item.TraceID),
		SpanID:           strings.TrimSpace(item.SpanID),
		EvaluatorID:      strings.TrimSpace(item.EvaluatorID),
		EvaluatorVersion: strings.TrimSpace(item.EvaluatorVersion),
		RuleID:           strings.TrimSpace(item.RuleID),
		RunID:            strings.TrimSpace(item.RunID),
		ScoreKey:         strings.TrimSpace(item.ScoreKey),
		Value:            item.Value,
		Passed:           item.Passed,
		Explanation:      strings.TrimSpace(item.Explanation),
		Metadata:         item.Metadata,
		CreatedAt:        item.CreatedAt,
		Source: ScoreSource{
			Kind: strings.TrimSpace(item.Source.Kind),
			ID:   strings.TrimSpace(item.Source.ID),
		},
	}
	if normalized.ScoreID == "" {
		return ScoreItem{}, fmt.Errorf("score_id is required")
	}
	if len(normalized.ScoreID) > maxScoreIDLen {
		return ScoreItem{}, fmt.Errorf("score_id is too long")
	}
	if normalized.GenerationID == "" {
		return ScoreItem{}, fmt.Errorf("generation_id is required")
	}
	if len(normalized.GenerationID) > maxScoreGenerationIDLen {
		return ScoreItem{}, fmt.Errorf("generation_id is too long")
	}
	if len(normalized.ConversationID) > maxScoreConversationIDLen {
		return ScoreItem{}, fmt.Errorf("conversation_id is too long")
	}
	if normalized.EvaluatorID == "" {
		return ScoreItem{}, fmt.Errorf("evaluator_id is required")
	}
	if len(normalized.EvaluatorID) > maxScoreEvaluatorIDLen {
		return ScoreItem{}, fmt.Errorf("evaluator_id is too long")
	}
	if normalized.EvaluatorVersion == "" {
		return ScoreItem{}, fmt.Errorf("evaluator_version is required")
	}
	if normalized.ScoreKey == "" {
		return ScoreItem{}, fmt.Errorf("score_key is required")
	}
	if len(normalized.ScoreKey) > maxScoreScoreKeyLen {
		return ScoreItem{}, fmt.Errorf("score_key is too long")
	}
	if len([]byte(normalized.Explanation)) > maxScoreExplanationBytes {
		return ScoreItem{}, fmt.Errorf("explanation is too long")
	}
	if err := validateScoreValue(normalized.Value); err != nil {
		return ScoreItem{}, err
	}
	if normalized.Metadata != nil {
		if err := validateJSONSize(normalized.Metadata, maxScoreMetadataBytes, "metadata"); err != nil {
			return ScoreItem{}, err
		}
	}
	return normalized, nil
}

func normalizeRunID(runID string) (string, error) {
	normalized := strings.TrimSpace(runID)
	if normalized == "" {
		return "", fmt.Errorf("run_id is required")
	}
	if len(normalized) > maxExperimentRunIDLen {
		return "", fmt.Errorf("run_id is too long")
	}
	return normalized, nil
}

func normalizeExperimentEvaluators(evaluators []ExperimentEvaluator) []ExperimentEvaluator {
	if len(evaluators) == 0 {
		return nil
	}
	out := make([]ExperimentEvaluator, 0, len(evaluators))
	for _, evaluator := range evaluators {
		out = append(out, ExperimentEvaluator{
			ID:       strings.TrimSpace(evaluator.ID),
			Selector: strings.TrimSpace(evaluator.Selector),
		})
	}
	return out
}

func validateScoreValue(value ScoreValue) error {
	nonNil := 0
	if value.Number != nil {
		nonNil++
	}
	if value.Bool != nil {
		nonNil++
	}
	if value.String != nil {
		nonNil++
	}
	if nonNil == 0 {
		return fmt.Errorf("score value is required")
	}
	if nonNil > 1 {
		return fmt.Errorf("score value must contain exactly one type")
	}
	return nil
}

func validateJSONSize(value any, maxBytes int, name string) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("%s must be valid JSON", name)
	}
	if len(payload) > maxBytes {
		return fmt.Errorf("%s is too large", name)
	}
	return nil
}

func paginationQuery(limit int, cursor string) string {
	values := url.Values{}
	if limit > 0 {
		values.Set("limit", fmt.Sprintf("%d", limit))
	}
	if strings.TrimSpace(cursor) != "" {
		values.Set("cursor", strings.TrimSpace(cursor))
	}
	if len(values) == 0 {
		return ""
	}
	return "?" + values.Encode()
}

func experimentStatusError(statusCode int, body string) error {
	text := statusErrorText([]byte(body), statusCode)
	switch statusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrExperimentValidationFailed, text)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrExperimentConflict, text)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrExperimentNotFound, text)
	default:
		return fmt.Errorf("%w: status %d: %s", ErrExperimentTransportFailed, statusCode, text)
	}
}

func scoreExportStatusError(statusCode int, body string) error {
	text := statusErrorText([]byte(body), statusCode)
	switch statusCode {
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrScoreValidationFailed, text)
	default:
		return fmt.Errorf("%w: status %d: %s", ErrScoreTransportFailed, statusCode, text)
	}
}

func statusErrorText(body []byte, statusCode int) string {
	bodyText := strings.TrimSpace(string(body))
	if bodyText != "" {
		return bodyText
	}
	return http.StatusText(statusCode)
}
