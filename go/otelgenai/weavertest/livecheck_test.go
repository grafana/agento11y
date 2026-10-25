package weavertest

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestToolSpanPolicy(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Skip("weaver is not on PATH")
	}
	root := t.TempDir()
	var registry strings.Builder
	registry.WriteString("groups:\n  - id: registry.test\n    type: attribute_group\n    brief: Policy test attributes.\n    attributes:\n")
	for _, key := range []string{"gen_ai.operation.name", "gen_ai.tool.name", "gen_ai.agent.name", "gen_ai.skill.name", "gen_ai.skill.resource.name", "process.executable.name", "error.type"} {
		registry.WriteString("      - id: " + key + "\n        type: string\n        brief: Policy test attribute.\n        stability: development\n")
	}
	if err := os.WriteFile(filepath.Join(root, "registry.yaml"), []byte(registry.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	policies, config, err := writeInputs(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, executionKinds := range []bool{false, true} {
		name := "legacy"
		if executionKinds {
			name = "execution_kinds"
		}
		t.Run(name, func(t *testing.T) {
			attributes := map[string]string{"gen_ai.operation.name": "required", "gen_ai.tool.name": "required"}
			if executionKinds {
				attributes["gen_ai.agent.name"] = "recommended"
			}
			model := map[string]any{
				"spans": map[string]any{"gen_ai.execute_tool.internal": map[string]any{
					"kind": "internal", "attributes": attributes,
				}},
				"enums": map[string]any{"gen_ai.operation.name": []string{"execute_tool"}},
			}
			if executionKinds {
				model["tool_execution_kinds"] = map[string]any{"gen_ai.execute_tool.load_skill.internal": map[string]any{}}
			}
			data, err := json.Marshal(model)
			if err != nil {
				t.Fatal(err)
			}
			advice := filepath.Join(root, "coverage-model.json")
			if err := os.WriteFile(advice, data, 0o644); err != nil {
				t.Fatal(err)
			}
			assets := Assets{Registry: root, Policies: policies, Config: config, AdviceData: advice}
			var samples []Sample
			var want []string
			for _, tc := range []struct {
				name               string
				attrs              map[string]string
				executionKindsOnly bool
				invalid            bool
			}{
				{name: "execute_tool tool"},
				{name: "execute_tool tool", attrs: map[string]string{"gen_ai.skill.name": "review"}},
				{name: "execute_tool tool review", attrs: map[string]string{"gen_ai.skill.name": "review"}, executionKindsOnly: true},
				{name: "execute_tool tool review notes.md", attrs: map[string]string{"gen_ai.skill.name": "review", "gen_ai.skill.resource.name": "notes.md"}, executionKindsOnly: true},
				{name: "execute_tool tool notes.md", attrs: map[string]string{"gen_ai.skill.resource.name": "notes.md"}, executionKindsOnly: true},
				{name: "execute_tool tool bash", attrs: map[string]string{"process.executable.name": "bash"}, executionKindsOnly: true},
				{name: "execute_tool tool bash", attrs: map[string]string{"gen_ai.skill.name": "review", "process.executable.name": "bash"}, executionKindsOnly: true},
				{name: "execute_tool wrong", invalid: true},
				{name: "execute_tool tool arbitrary", invalid: true},
				{name: "execute_tool tool wrong", attrs: map[string]string{"gen_ai.skill.name": "review"}, invalid: true},
				{name: "execute_tool tool notes.md review", attrs: map[string]string{"gen_ai.skill.name": "review", "gen_ai.skill.resource.name": "notes.md"}, invalid: true},
				{name: "execute_tool", invalid: true},
			} {
				attrs := map[string]string{"gen_ai.operation.name": "execute_tool", "gen_ai.tool.name": "tool"}
				maps.Copy(attrs, tc.attrs)
				sample := Sample{Span: SampleSpan{Name: tc.name, Kind: "internal", Status: SampleSpanStatus{Code: "unset"}, SpanEvents: []SampleSpanEvent{}, SpanLinks: []SampleSpanLink{}}}
				for key, value := range attrs {
					encoded, err := json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
					sample.Span.Attributes = append(sample.Span.Attributes, SampleAttribute{Name: key, Value: encoded})
				}
				samples = append(samples, sample)
				if tc.invalid || (tc.executionKindsOnly && !executionKinds) {
					want = append(want, tc.name)
				}
			}
			findings, err := LiveCheck(ctx, assets, samples)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int{}
			for _, finding := range findings {
				if finding.ID != "genai_span_name_format" {
					t.Errorf("unexpected finding: %+v", finding)
				}
				got[finding.Span]++
			}
			expected := map[string]int{}
			for _, span := range want {
				expected[span]++
			}
			if !maps.Equal(got, expected) {
				t.Errorf("name violations = %v, want %v", got, expected)
			}
		})
	}
}

func TestViolationsCollectsEverySpanLocation(t *testing.T) {
	finding := func(id string) map[string]any {
		return map[string]any{
			"id":      id,
			"level":   LevelViolation,
			"context": map[string]any{"case": id},
		}
	}
	result := func(id string) map[string]any {
		return map[string]any{"all_advice": []any{finding(id)}}
	}
	report, err := json.Marshal(map[string]any{
		"samples": []any{map[string]any{
			"span": map[string]any{
				"name":              "chat model",
				"live_check_result": result("span"),
				"attributes": []any{map[string]any{
					"name":              "gen_ai.operation.name",
					"live_check_result": result("span_attribute"),
				}},
				"span_events": []any{map[string]any{
					"name":              "gen_ai.client.inference.operation.details",
					"live_check_result": result("event"),
					"attributes": []any{map[string]any{
						"name":              "gen_ai.input.messages",
						"value":             "{malformed",
						"live_check_result": result("event_attribute"),
					}},
				}},
				"span_links": []any{map[string]any{
					"live_check_result": result("link"),
					"attributes": []any{map[string]any{
						"name":              "gen_ai.output.messages",
						"value":             map[string]any{"structured": true},
						"live_check_result": result("link_attribute"),
					}},
				}},
			},
		}},
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}

	findings, err := violations(report)
	if err != nil {
		t.Fatalf("violations: %v", err)
	}
	got := make(map[string]string, len(findings))
	for _, finding := range findings {
		if finding.Span != "chat model" || finding.Level != LevelViolation {
			t.Errorf("finding identity = %#v", finding)
		}
		got[finding.ID] = finding.Target
	}
	want := map[string]string{
		"span":           "",
		"span_attribute": "gen_ai.operation.name",
		"event":          "event[0]:gen_ai.client.inference.operation.details",
		"event_attribute": "event[0]:gen_ai.client.inference.operation.details/" +
			"gen_ai.input.messages",
		"link":           "link[0]",
		"link_attribute": "link[0]/gen_ai.output.messages",
	}
	if !maps.Equal(got, want) {
		t.Fatalf("finding targets = %#v, want %#v", got, want)
	}
}
