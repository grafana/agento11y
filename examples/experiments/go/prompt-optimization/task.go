package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/agento11y/experiments"
)

const (
	suiteID           = "unit-inference-prometheus"
	suiteName         = "Grafana unit inference (Prometheus family)"
	taskMaxTokens     = 32
	taskDescription   = "Given a Grafana query and a compact numeric profile of the data it returned, answer with the single most appropriate Grafana visualization unit, formatted as one line: UNIT: <unit>"
	outputConstraint  = "The caller caps the response at 32 tokens and extracts the unit with the regex ^UNIT:\\s*(.+)$. The reply must be exactly the line 'UNIT: <unit>' with no reasoning, preamble, or explanation before it. Prompts that ask the model to think step by step therefore fail outright."
	naiveSystemPrompt = `You pick the Grafana visualization unit for a query.

Read the query and the profile of the data it returned, then answer with the
unit that fits best.

Answer with a single line: UNIT: <unit>`
)

//go:embed data/fixtures.jsonl
var fixturesJSONL []byte

//go:embed data/system_prompt.txt
var productionPromptFile string

var (
	unitLine          = regexp.MustCompile(`(?im)^UNIT:\s*(.+)$`)
	rateCounterTokens = []string{
		"_count", "_requests_", "_calls_", "_operations_", "_queries_", "_errors_", "_total",
	}
)

type fixture struct {
	AcceptableUnits []string    `json:"acceptableUnits"`
	DataProfile     dataProfile `json:"dataProfile"`
	DatasourceKind  string      `json:"datasourceKind"`
	ExpectedUnit    string      `json:"expectedUnit"`
	ID              string      `json:"id"`
	Intent          string      `json:"intent"`
	QueryString     string      `json:"queryString"`
	QueryType       string      `json:"queryType"`
	TimeRange       *timeRange  `json:"timeRange"`
	UnitHint        string      `json:"unitHint"`
	UserMessage     string      `json:"userMessage"`
}

type dataProfile struct {
	Average     float64 `json:"avg"`
	Maximum     float64 `json:"max"`
	Minimum     float64 `json:"min"`
	PointCount  int     `json:"pointCount"`
	Samples     [][]any `json:"samples"`
	SeriesCount int     `json:"seriesCount"`
}

type timeRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

func loadFixtures() ([]fixture, error) {
	scanner := bufio.NewScanner(bytes.NewReader(fixturesJSONL))
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	items := make([]fixture, 0, 27)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var item fixture
		if err := json.Unmarshal(scanner.Bytes(), &item); err != nil {
			return nil, fmt.Errorf("decode fixture line %d: %w", len(items)+1, err)
		}
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read embedded fixtures: %w", err)
	}
	return items, nil
}

func buildSuite(items []fixture, version string) experiments.TestSuite {
	testCases := make([]experiments.TestCase, 0, len(items))
	for _, item := range items {
		testCases = append(testCases, experiments.TestCase{
			TestCaseID: item.ID,
			Input:      userMessage(item),
			Expected:   item.ExpectedUnit,
			Metadata: map[string]any{
				"acceptable_units": item.AcceptableUnits,
				"datasource_kind":  item.DatasourceKind,
				"query":            item.QueryString,
			},
		})
	}
	return experiments.TestSuite{
		SuiteID: suiteID, Name: suiteName, Version: version, TestCases: testCases,
	}
}

func localSuiteVersion() string {
	sum := sha256.Sum256(fixturesJSONL)
	return "1.0.0+" + hex.EncodeToString(sum[:])[:12]
}

func productionSystemPrompt() string {
	return strings.TrimSpace(productionPromptFile)
}

func userMessage(item fixture) string {
	if item.UserMessage != "" {
		return item.UserMessage
	}
	profile := item.DataProfile
	lines := []string{
		"<query_context>",
		"Query: " + item.QueryString,
		"Query Type: " + item.QueryType,
		"Datasource: " + firstNonEmpty(item.DatasourceKind, "unknown"),
	}
	if item.TimeRange != nil {
		lines = append(lines, "Time Range: "+item.TimeRange.From+" to "+item.TimeRange.To)
	}
	lines = append(lines,
		"Intent: "+item.Intent,
		"</query_context>",
		"",
		"<data_profile>",
		fmt.Sprintf("Series Count: %d", profile.SeriesCount),
		fmt.Sprintf("Point Count: %d", profile.PointCount),
	)
	if len(profile.Samples) > 0 {
		lines = append(lines, fmt.Sprintf(
			"Numeric Stats: min=%s, max=%s, avg=%s",
			formatFloat(profile.Minimum), formatFloat(profile.Maximum), formatFloat(profile.Average),
		))
	}
	lines = append(lines, "</data_profile>")
	if len(profile.Samples) > 0 {
		lines = append(lines, "", "<sampled_points>")
		for i, sample := range profile.Samples {
			if i == 8 {
				break
			}
			if len(sample) < 3 {
				continue
			}
			lines = append(lines, fmt.Sprintf(
				"%d. metric=%v, value=%s, time=%v",
				i+1, sample[0], formatFloat(sample[1]), sample[2],
			))
		}
		lines = append(lines, "</sampled_points>")
	}
	return strings.Join(lines, "\n")
}

func formatFloat(value any) string {
	number, ok := value.(float64)
	if !ok {
		switch typed := value.(type) {
		case int:
			number, ok = float64(typed), true
		case json.Number:
			number, _ = typed.Float64()
			ok = true
		}
	}
	if !ok || number == 0 {
		return "0"
	}
	return strings.TrimRight(strings.TrimRight(strconv.FormatFloat(number, 'f', 3, 64), "0"), ".")
}

func parseUnit(text, hint string) string {
	match := unitLine.FindStringSubmatch(text)
	if len(match) == 0 {
		if trimmed := strings.TrimSpace(hint); trimmed != "" {
			return trimmed
		}
		return "short"
	}
	value := strings.Trim(strings.TrimSpace(match[1]), "'\"")
	if value == "" || len(value) > 64 || len(strings.Fields(value)) > 1 {
		return "short"
	}
	return value
}

func normalizePrometheusRateUnit(query, unit string) string {
	if unit != "ops" && unit != "count" && unit != "short" {
		return unit
	}
	lowered := strings.ToLower(strings.TrimSpace(query))
	if !strings.Contains(lowered, "rate(") && !strings.Contains(lowered, "irate(") {
		return unit
	}
	if strings.Contains(lowered, "_bytes") || strings.Contains(lowered, "_cpu_") {
		return unit
	}
	for _, token := range rateCounterTokens {
		if strings.Contains(lowered, token) {
			return "reqps"
		}
	}
	return unit
}

func resolveUnit(text string, item fixture) string {
	hint := strings.TrimSpace(item.UnitHint)
	resolved := parseUnit(text, hint)
	if resolved == "short" && hint != "" && hint != "short" {
		resolved = hint
	}
	if strings.EqualFold(strings.TrimSpace(item.DatasourceKind), "prometheus") {
		resolved = normalizePrometheusRateUnit(item.QueryString, resolved)
	}
	return resolved
}

func scoreOutput(item fixture, modelOutput string) (float64, string) {
	predicted := resolveUnit(modelOutput, item)
	expected := item.ExpectedUnit
	acceptable := item.AcceptableUnits
	if acceptable == nil {
		acceptable = []string{expected}
	}
	verdict := "miss"
	switch {
	case strings.EqualFold(predicted, expected):
		verdict = "exact"
	case containsFold(acceptable, predicted):
		verdict = "acceptable"
	case equivalentRateUnit(predicted, expected):
		verdict = "equivalent"
	}
	value := 0.0
	if verdict != "miss" {
		value = 1.0
	}
	return value, fmt.Sprintf("expected %q, resolved %q (%s)", expected, predicted, verdict)
}

func equivalentRateUnit(left, right string) bool {
	equivalent := map[string]bool{"reqps": true, "rps": true, "ops": true}
	return equivalent[strings.ToLower(left)] && equivalent[strings.ToLower(right)]
}

func containsFold(values []string, target string) bool {
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
