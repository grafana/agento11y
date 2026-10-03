package weavertest

import (
	"encoding/json"
	"maps"
	"testing"
)

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
