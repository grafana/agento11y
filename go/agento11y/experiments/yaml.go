package experiments

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type portableCase struct {
	ID           string                `yaml:"id,omitempty"`
	TestCaseID   string                `yaml:"test_case_id,omitempty"`
	Name         string                `yaml:"name,omitempty"`
	Description  string                `yaml:"description,omitempty"`
	Tags         []string              `yaml:"tags,omitempty"`
	Category     string                `yaml:"category,omitempty"`
	Input        any                   `yaml:"input,omitempty"`
	Expected     any                   `yaml:"expected,omitempty"`
	Weight       *float64              `yaml:"weight,omitempty"`
	Metadata     map[string]any        `yaml:"metadata,omitempty"`
	ArtifactRefs []portableArtifactRef `yaml:"artifact_refs,omitempty"`
}

type portableArtifactRef struct {
	ArtifactID string `yaml:"artifact_id"`
	Name       string `yaml:"name,omitempty"`
	Kind       string `yaml:"kind,omitempty"`
	MIME       string `yaml:"mime,omitempty"`
}

func (c portableCase) MarshalYAML() (any, error) {
	out := map[string]any{"id": c.ID}
	if c.Name != "" {
		out["name"] = c.Name
	}
	if c.Description != "" {
		out["description"] = c.Description
	}
	if len(c.Tags) > 0 {
		out["tags"] = c.Tags
	}
	if c.Category != "" {
		out["category"] = c.Category
	}
	if c.Input != nil {
		out["input"] = c.Input
	}
	if c.Expected != nil {
		out["expected"] = c.Expected
	}
	if c.Weight != nil {
		out["weight"] = *c.Weight
	}
	if len(c.Metadata) > 0 {
		out["metadata"] = c.Metadata
	}
	if len(c.ArtifactRefs) > 0 {
		out["artifact_refs"] = c.ArtifactRefs
	}
	return out, nil
}

type portableSuite struct {
	SuiteID     string         `yaml:"suite_id,omitempty"`
	ID          string         `yaml:"id,omitempty"`
	Name        string         `yaml:"name,omitempty"`
	Version     string         `yaml:"version,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Tags        []string       `yaml:"tags,omitempty"`
	Changelog   string         `yaml:"changelog,omitempty"`
	Cases       []portableCase `yaml:"cases,omitempty"`
	TestCases   []portableCase `yaml:"test_cases,omitempty"`
}

func ParseSuite(data []byte) (*TestSuite, error) {
	var raw portableSuite
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse suite YAML: %w", err)
	}
	suiteID := raw.SuiteID
	if suiteID == "" {
		suiteID = raw.ID
	}
	cases := raw.Cases
	if cases == nil {
		cases = raw.TestCases
	}
	suite := &TestSuite{
		SuiteID: suiteID, Name: raw.Name, Version: raw.Version,
		Description: raw.Description, Tags: append([]string(nil), raw.Tags...),
		Changelog: raw.Changelog,
	}
	if suite.Version == "" {
		suite.Version = "1.0.0"
	}
	for _, rawCase := range cases {
		id := rawCase.TestCaseID
		if id == "" {
			id = rawCase.ID
		}
		weight := 1.0
		if rawCase.Weight != nil {
			weight = *rawCase.Weight
		}
		suite.TestCases = append(suite.TestCases, TestCase{
			TestCaseID: id, Name: rawCase.Name, Description: rawCase.Description,
			Tags: append([]string(nil), rawCase.Tags...), Category: rawCase.Category,
			Input: rawCase.Input, Expected: rawCase.Expected, Weight: weight,
			Metadata:     cloneMap(rawCase.Metadata),
			ArtifactRefs: fromPortableArtifactRefs(rawCase.ArtifactRefs),
		})
	}
	if err := validateSuite(*suite); err != nil {
		return nil, err
	}
	return suite, nil
}

func LoadSuite(path string) (*TestSuite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read suite YAML: %w", err)
	}
	return ParseSuite(data)
}

func MarshalSuite(suite TestSuite) ([]byte, error) {
	if err := validateSuite(suite); err != nil {
		return nil, err
	}
	raw := portableSuite{
		SuiteID: suite.SuiteID, Name: suite.Name, Version: suite.Version,
		Description: suite.Description, Tags: append([]string(nil), suite.Tags...),
		Changelog: suite.Changelog,
		Cases:     make([]portableCase, 0, len(suite.TestCases)),
	}
	for _, testCase := range suite.TestCases {
		item := portableCase{
			ID: testCase.TestCaseID, Name: testCase.Name, Description: testCase.Description,
			Tags: append([]string(nil), testCase.Tags...), Category: testCase.Category,
			Input: testCase.Input, Expected: testCase.Expected,
			Metadata:     cloneMap(testCase.Metadata),
			ArtifactRefs: toPortableArtifactRefs(testCase.ArtifactRefs),
		}
		if testCase.Weight != 1 {
			weight := testCase.Weight
			item.Weight = &weight
		}
		raw.Cases = append(raw.Cases, item)
	}
	return yaml.Marshal(raw)
}

func WriteSuite(path string, suite TestSuite) error {
	data, err := MarshalSuite(suite)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write suite YAML: %w", err)
	}
	return nil
}

func (s TestSuite) MarshalYAML() (any, error) {
	data, err := MarshalSuite(s)
	if err != nil {
		return nil, err
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return nil, err
	}
	return node.Content[0], nil
}

func (s *TestSuite) UnmarshalYAML(node *yaml.Node) error {
	data, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	parsed, err := ParseSuite(data)
	if err != nil {
		return err
	}
	*s = *parsed
	return nil
}

func (s TestSuite) ToYAML() ([]byte, error) { return MarshalSuite(s) }

func toPortableArtifactRefs(refs []ExperimentArtifactRef) []portableArtifactRef {
	out := make([]portableArtifactRef, len(refs))
	for i, ref := range refs {
		out[i] = portableArtifactRef{
			ArtifactID: ref.ArtifactID, Name: ref.Name, Kind: ref.Kind, MIME: ref.MIME,
		}
	}
	return out
}

func fromPortableArtifactRefs(refs []portableArtifactRef) []ExperimentArtifactRef {
	out := make([]ExperimentArtifactRef, len(refs))
	for i, ref := range refs {
		out[i] = ExperimentArtifactRef{
			ArtifactID: ref.ArtifactID, Name: ref.Name, Kind: ref.Kind, MIME: ref.MIME,
		}
	}
	return out
}

// Compatibility names make the portable helpers easy to discover.
func ParseTestSuiteYAML(data []byte) (*TestSuite, error)    { return ParseSuite(data) }
func LoadTestSuiteYAML(path string) (*TestSuite, error)     { return LoadSuite(path) }
func MarshalTestSuiteYAML(suite TestSuite) ([]byte, error)  { return MarshalSuite(suite) }
func WriteTestSuiteYAML(path string, suite TestSuite) error { return WriteSuite(path, suite) }
