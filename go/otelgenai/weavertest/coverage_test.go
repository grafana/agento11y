package weavertest

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCoverageModelPreservesLegacyMaps(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Skip("weaver is not on PATH")
	}
	root := t.TempDir()
	const registry = `groups:
  - id: registry.test
    type: attribute_group
    brief: Test attributes.
    attributes:
      - id: gen_ai.tool.name
        type: string
        brief: Tool name.
        stability: development
      - id: gen_ai.agent.name
        type: string
        brief: Agent name.
        stability: development
  - id: gen_ai.execute_tool.internal
    type: span
    brief: Tool execution.
    span_kind: internal
    stability: development
    attributes:
      - ref: gen_ai.tool.name
        requirement_level: required
  - id: metric.gen_ai.execute_tool.duration
    type: metric
    metric_name: gen_ai.execute_tool.duration
    brief: Tool duration.
    instrument: histogram
    unit: s
    stability: development
    attributes:
      - ref: gen_ai.tool.name
        requirement_level: required
      - ref: gen_ai.agent.name
        requirement_level:
          conditionally_required: When applicable.
`
	if err := os.WriteFile(filepath.Join(root, "registry.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatal(err)
	}
	_, config, err := writeInputs(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	output := t.TempDir()
	if err := generateCoverageModel(ctx, root, filepath.Join(filepath.Dir(config), "templates"), output); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(output, "coverage-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	var model map[string]any
	if err := json.Unmarshal(contents, &model); err != nil {
		t.Fatal(err)
	}
	wantMetrics := map[string]any{"gen_ai.execute_tool.duration": map[string]any{"attributes": map[string]any{
		"gen_ai.tool.name": "required", "gen_ai.agent.name": "conditionally_required_conditional",
	}}}
	if !reflect.DeepEqual(model["metrics"], wantMetrics) {
		t.Fatalf("legacy metric shape changed: %#v", model["metrics"])
	}
	wantSpans := map[string]any{"gen_ai.execute_tool.internal": map[string]any{
		"kind": "internal", "attributes": map[string]any{"gen_ai.tool.name": "required"},
	}}
	if !reflect.DeepEqual(model["spans"], wantSpans) {
		t.Fatalf("legacy span shape changed: %#v", model["spans"])
	}
	for _, key := range []string{"tool_execution_kinds", "tool_metric_refinements", "events", "entities"} {
		if value, ok := model[key].(map[string]any); !ok || len(value) != 0 {
			t.Errorf("%s = %#v, want empty map", key, model[key])
		}
	}
	base, ok := model["tool_metric_base"].(map[string]any)
	if !ok || base["name"] != "gen_ai.execute_tool.duration" || base["instrument"] != "histogram" || base["unit"] != "s" {
		t.Fatalf("separate metric definition = %#v", model["tool_metric_base"])
	}
}
