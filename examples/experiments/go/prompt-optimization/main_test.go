package main

import "testing"

func TestExperimentComparisonURL(t *testing.T) {
	got, err := experimentComparisonURL("https://example.grafana.net/custom/path")
	if err != nil {
		t.Fatalf("experimentComparisonURL() error = %v", err)
	}
	want := "https://example.grafana.net/a/grafana-agento11y-app/offline-experiments/experiments/"
	if got != want {
		t.Errorf("experimentComparisonURL() = %q, want %q", got, want)
	}
}
