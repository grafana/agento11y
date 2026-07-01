package sigil

import (
	"os"
	"strconv"
	"strings"
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
	return cloneTestCases(s.TestCases)
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
	RunID        string
	TestCaseID   string
	Attempt      int
	SuiteID      string
	SuiteVersion string
	SuiteName    string
	TestCaseName string
	TrajectoryID string
}

func (r TrialRef) ToJSON() map[string]any {
	attempt := r.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	return map[string]any{
		"experiment_id":  r.RunID,
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
	if attempt <= 0 {
		attempt = 1
	}
	return TrialRef{
		RunID:        strings.TrimSpace(firstString(payload["experiment_id"], payload["run_id"])),
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
	if attempt <= 0 {
		attempt = 1
	}
	env := map[string]string{
		EnvExperimentID: r.RunID,
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
		RunID:        experimentID,
		TestCaseID:   testCaseID,
		Attempt:      attempt,
		SuiteID:      strings.TrimSpace(os.Getenv(EnvSuiteID)),
		SuiteVersion: strings.TrimSpace(os.Getenv(EnvSuiteVersion)),
		TrajectoryID: strings.TrimSpace(os.Getenv(EnvTrajectoryID)),
	}, true
}
