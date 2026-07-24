// Package experiments provides the tracking-first experiment API for Go.
//
// It is intentionally separate from the original root-package experiment API:
// both remain supported, while this package follows the portable suite and
// lifecycle semantics of agento11y.experiments in Python.
package experiments

import (
	"fmt"
	"maps"
	"os"
	"strconv"
	"strings"

	agento11y "github.com/grafana/agento11y/go/agento11y"
)

type ExperimentStatus = agento11y.ExperimentStatus

const (
	ExperimentStatusRunning   = agento11y.ExperimentStatusRunning
	ExperimentStatusCompleted = agento11y.ExperimentStatusCompleted
	ExperimentStatusSucceeded = agento11y.ExperimentStatusSucceeded
	ExperimentStatusFailed    = agento11y.ExperimentStatusFailed
)

type TrialStatus = agento11y.TrialStatus

const (
	TrialStatusRunning   = agento11y.TrialStatusRunning
	TrialStatusCompleted = agento11y.TrialStatusCompleted
	TrialStatusPassed    = agento11y.TrialStatusPassed
	TrialStatusFailed    = agento11y.TrialStatusFailed
	TrialStatusErrored   = agento11y.TrialStatusErrored
	TrialStatusSkipped   = agento11y.TrialStatusSkipped
)

type EvaluatorKind = agento11y.EvaluatorKind

const (
	EvaluatorKindLLMJudge      = agento11y.EvaluatorKindLLMJudge
	EvaluatorKindDeterministic = agento11y.EvaluatorKindDeterministic
	EvaluatorKindHuman         = agento11y.EvaluatorKindHuman
	EvaluatorKindCustom        = agento11y.EvaluatorKindCustom
)

type ScoreValue = agento11y.ScoreValue
type ScoreItem = agento11y.ScoreItem
type ScoreType = agento11y.ScoreType
type ScoreSource = agento11y.ScoreSource
type ExperimentReport struct {
	Run     agento11y.Experiment
	Summary ExperimentReportSummary
	Rows    []agento11y.TestCaseResultRow
}

type ExperimentReportSummary struct {
	TestCaseCount   int
	TrialCount      int
	CompletedCount  int
	FailedCount     int
	CanceledCount   int
	PassRate        *float64
	PassAtK         map[string]float64
	PassPowerK      map[string]float64
	FinalScoreAvg   *float64
	TotalCost       *float64
	TotalTokens     *int
	PassCount       int
	PassDenominator int
	FinalScoreSum   float64
	FinalScoreCount int
	TokenCoverage   string
	CostCoverage    string
}
type TrialArtifact = agento11y.TrialArtifact
type ExperimentArtifactRef = agento11y.ExperimentArtifactRef
type TokenUsage = agento11y.TokenUsage

var (
	NumberScoreValue = agento11y.NumberScoreValue
	BoolScoreValue   = agento11y.BoolScoreValue
	StringScoreValue = agento11y.StringScoreValue
	StableID         = agento11y.StableID
)

// TestCase is one portable suite case.
type TestCase struct {
	TestCaseID   string
	Name         string
	Description  string
	Tags         []string
	Category     string
	Input        any
	Expected     any
	Weight       *float64
	Metadata     map[string]any
	ArtifactRefs []ExperimentArtifactRef
}

// ID returns the portable id alias used by YAML.
func (c TestCase) ID() string { return c.TestCaseID }

// EffectiveWeight returns the configured weight or the portable default of 1.
func (c TestCase) EffectiveWeight() float64 {
	if c.Weight == nil {
		return 1
	}
	return *c.Weight
}

// Weight returns a pointer suitable for TestCase.Weight, including explicit zero.
func Weight(value float64) *float64 { return &value }

// TestSuite is a local or stored portable test suite.
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
	out := make([]TestCase, len(s.TestCases))
	for i := range s.TestCases {
		out[i] = cloneTestCase(s.TestCases[i])
	}
	return out
}

func (s *TestSuite) Case(testCaseID string) (TestCase, bool) {
	if s == nil {
		return TestCase{}, false
	}
	for i := range s.TestCases {
		if s.TestCases[i].TestCaseID == testCaseID {
			return cloneTestCase(s.TestCases[i]), true
		}
	}
	return TestCase{}, false
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
	return agento11y.Candidate{
		AgentName: c.AgentName, AgentVersion: c.AgentVersion,
		PromptVersion: c.PromptVersion, ModelProvider: c.ModelProvider,
		ModelName: c.ModelName, GitSHA: c.GitSHA,
	}.AsMetadata()
}

type Evaluator struct {
	EvaluatorID         string
	Version             string
	Kind                EvaluatorKind
	ReferenceSetID      string
	ReferenceSetVersion string
}

func (e Evaluator) normalized() Evaluator {
	if strings.TrimSpace(e.EvaluatorID) == "" {
		e.EvaluatorID = "sdk"
	}
	if strings.TrimSpace(e.Version) == "" {
		e.Version = "0"
	}
	e.Kind = agento11y.NormalizeEvaluatorKind(string(e.Kind))
	return e
}

func NormalizeEvaluatorKind(kind string) EvaluatorKind {
	return agento11y.NormalizeEvaluatorKind(kind)
}

const (
	EnvExperimentID = "AGENTO11Y_EXPERIMENT_ID"
	EnvTestCaseID   = "AGENTO11Y_TEST_CASE_ID"
	EnvAttempt      = "AGENTO11Y_ATTEMPT"
	EnvSuiteID      = "AGENTO11Y_SUITE_ID"
	EnvSuiteVersion = "AGENTO11Y_SUITE_VERSION"
	EnvTrajectoryID = "AGENTO11Y_TRAJECTORY_ID"
)

// TrialRef is a durable, cross-process identity for one case attempt.
// ExperimentID is canonical; RunID is accepted as a Go-side alias.
type TrialRef struct {
	ExperimentID string
	RunID        string
	TestCaseID   string
	Attempt      int
	SuiteID      string
	SuiteVersion string
	SuiteName    string
	TestCaseName string
	TrajectoryID string
}

func (r TrialRef) experimentID() string {
	if strings.TrimSpace(r.ExperimentID) != "" {
		return strings.TrimSpace(r.ExperimentID)
	}
	return strings.TrimSpace(r.RunID)
}

func (r TrialRef) normalized() TrialRef {
	r.ExperimentID = r.experimentID()
	r.RunID = r.ExperimentID
	r.TestCaseID = strings.TrimSpace(r.TestCaseID)
	if r.Attempt <= 0 {
		r.Attempt = 1
	}
	return r
}

func (r TrialRef) ToJSON() map[string]any {
	r = r.normalized()
	return map[string]any{
		"experiment_id":  r.ExperimentID,
		"test_case_id":   r.TestCaseID,
		"attempt":        r.Attempt,
		"suite_id":       r.SuiteID,
		"suite_version":  r.SuiteVersion,
		"suite_name":     r.SuiteName,
		"test_case_name": r.TestCaseName,
		"trajectory_id":  r.TrajectoryID,
	}
}

func TrialRefFromJSON(payload map[string]any) TrialRef {
	stringValue := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	attempt := 1
	switch value := payload["attempt"].(type) {
	case int:
		attempt = value
	case int64:
		attempt = int(value)
	case float64:
		attempt = int(value)
	case string:
		if parsed, err := strconv.Atoi(value); err == nil {
			attempt = parsed
		}
	}
	return TrialRef{
		ExperimentID: stringValue("experiment_id", "run_id"),
		TestCaseID:   stringValue("test_case_id"),
		Attempt:      attempt,
		SuiteID:      stringValue("suite_id"),
		SuiteVersion: stringValue("suite_version"),
		SuiteName:    stringValue("suite_name"),
		TestCaseName: stringValue("test_case_name"),
		TrajectoryID: stringValue("trajectory_id"),
	}.normalized()
}

func (r TrialRef) ToEnv() map[string]string {
	r = r.normalized()
	env := map[string]string{
		EnvExperimentID: r.ExperimentID,
		EnvTestCaseID:   r.TestCaseID,
		EnvAttempt:      strconv.Itoa(r.Attempt),
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
	experimentID := firstEnv(EnvExperimentID, agento11y.EnvExperimentID)
	testCaseID := firstEnv(EnvTestCaseID, agento11y.EnvTestCaseID)
	if experimentID == "" || testCaseID == "" {
		return nil, false
	}
	attempt, _ := strconv.Atoi(firstEnv(EnvAttempt, agento11y.EnvAttempt))
	ref := TrialRef{
		ExperimentID: experimentID,
		TestCaseID:   testCaseID,
		Attempt:      attempt,
		SuiteID:      firstEnv(EnvSuiteID, agento11y.EnvSuiteID),
		SuiteVersion: firstEnv(EnvSuiteVersion, agento11y.EnvSuiteVersion),
		TrajectoryID: firstEnv(EnvTrajectoryID, agento11y.EnvTrajectoryID),
	}.normalized()
	return &ref, true
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(os.Getenv(key)); value != "" {
			return value
		}
	}
	return ""
}

func cloneTestCase(in TestCase) TestCase {
	in.Tags = append([]string(nil), in.Tags...)
	if in.Weight != nil {
		in.Weight = Weight(*in.Weight)
	}
	in.Metadata = cloneMap(in.Metadata)
	in.ArtifactRefs = append([]ExperimentArtifactRef(nil), in.ArtifactRefs...)
	return in
}

func cloneMap(in map[string]any) map[string]any {
	return maps.Clone(in)
}

func validateSuite(suite TestSuite) error {
	if strings.TrimSpace(suite.SuiteID) == "" {
		return fmt.Errorf("suite requires a suite_id (or id)")
	}
	seen := map[string]struct{}{}
	for _, testCase := range suite.TestCases {
		id := strings.TrimSpace(testCase.TestCaseID)
		if id == "" {
			return fmt.Errorf("test case requires an id (or test_case_id)")
		}
		if _, ok := seen[id]; ok {
			return fmt.Errorf("duplicate test case id %q", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}
