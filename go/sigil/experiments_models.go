package sigil

import (
	"encoding/json"
	"strings"
	"time"
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
	RunID                string         `json:"-"`
	TrialID              string         `json:"trial_id,omitempty"`
	TestCaseID           string         `json:"test_case_id,omitempty"`
	GraderConversationID string         `json:"grader_conversation_id,omitempty"`
	GraderGenerationID   string         `json:"grader_generation_id,omitempty"`
	GraderTraceID        string         `json:"grader_trace_id,omitempty"`
	Passed               *bool          `json:"passed,omitempty"`
	Explanation          string         `json:"explanation,omitempty"`
	Metadata             map[string]any `json:"metadata,omitempty"`
	CreatedAt            *time.Time     `json:"created_at,omitempty"`
	Source               *ScoreSource   `json:"source,omitempty"`
}

func (s ScoreItem) ResolvedRunID() string {
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
	RunID       string           `json:"run_id,omitempty"`
	Name        string           `json:"name"`
	Source      ExperimentSource `json:"source"`
	Description string           `json:"description,omitempty"`
	Tags        []string         `json:"tags,omitempty"`
	Metadata    map[string]any   `json:"metadata,omitempty"`
}

type CompleteExperimentOptions struct {
	ScoreCount *int
	Error      string
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
	Input        any                     `json:"input,omitempty"`
	Expected     any                     `json:"expected,omitempty"`
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
		ExperimentID string          `json:"experiment_id"`
		RunID        string          `json:"run_id"`
		Source       json.RawMessage `json:"source"`
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
	if len(raw.Source) == 0 || string(raw.Source) == "null" {
		return nil
	}
	var source string
	if err := json.Unmarshal(raw.Source, &source); err == nil {
		e.Source = source
		return nil
	}
	var sourceObject struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal(raw.Source, &sourceObject); err != nil {
		return err
	}
	e.Source = firstNonBlank(sourceObject.Kind, sourceObject.ID)
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
