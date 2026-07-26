package main

import (
	"strings"
	"testing"
)

func TestResolveUnit(t *testing.T) {
	tests := []struct {
		name   string
		output string
		item   fixture
		want   string
	}{
		{
			name: "parses exact unit line", output: "UNIT: bytes",
			item: fixture{DatasourceKind: "Prometheus"}, want: "bytes",
		},
		{
			name: "uses hint when line is absent", output: "I choose bytes",
			item: fixture{UnitHint: "s"}, want: "s",
		},
		{
			name: "uses non-short hint after invalid parsed value", output: "UNIT: too many words",
			item: fixture{UnitHint: "bytes"}, want: "bytes",
		},
		{
			name: "defaults to short without line or hint", output: "",
			item: fixture{}, want: "short",
		},
		{
			name: "normalizes prometheus counter rate", output: "UNIT: ops",
			item: fixture{
				DatasourceKind: "Prometheus",
				QueryString:    "sum(rate(http_requests_total[5m]))",
			},
			want: "reqps",
		},
		{
			name: "normalizes irate count", output: "UNIT: count",
			item: fixture{
				DatasourceKind: " prometheus ",
				QueryString:    "irate(worker_count[1m])",
			},
			want: "reqps",
		},
		{
			name: "does not normalize byte rate", output: "UNIT: ops",
			item: fixture{
				DatasourceKind: "Prometheus",
				QueryString:    "rate(network_receive_bytes_total[5m])",
			},
			want: "ops",
		},
		{
			name: "does not normalize cpu rate", output: "UNIT: short",
			item: fixture{
				DatasourceKind: "Prometheus",
				QueryString:    "rate(container_cpu_usage_seconds_total[5m])",
			},
			want: "short",
		},
		{
			name: "does not normalize seconds rate", output: "UNIT: short",
			item: fixture{
				DatasourceKind: "Prometheus",
				QueryString:    "rate(job_runtime_seconds_total[5m])",
			},
			want: "short",
		},
		{
			name: "does not normalize non-prometheus query", output: "UNIT: ops",
			item: fixture{
				DatasourceKind: "CloudWatch",
				QueryString:    "rate(requests_total[5m])",
			},
			want: "ops",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := resolveUnit(test.output, test.item); got != test.want {
				t.Errorf("resolveUnit() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestScoreOutput(t *testing.T) {
	tests := []struct {
		name        string
		item        fixture
		output      string
		wantScore   float64
		wantVerdict string
	}{
		{
			name: "exact", item: fixture{ExpectedUnit: "bytes"},
			output: "UNIT: bytes", wantScore: 1, wantVerdict: "(exact)",
		},
		{
			name:   "case insensitive acceptable",
			item:   fixture{ExpectedUnit: "bytes", AcceptableUnits: []string{"decbytes"}},
			output: "UNIT: DECBYTES", wantScore: 1, wantVerdict: "(acceptable)",
		},
		{
			name:   "equivalent request rate",
			item:   fixture{ExpectedUnit: "reqps", AcceptableUnits: []string{"reqps"}},
			output: "UNIT: rps", wantScore: 1, wantVerdict: "(equivalent)",
		},
		{
			name: "miss", item: fixture{ExpectedUnit: "s"},
			output: "UNIT: bytes", wantScore: 0, wantVerdict: "(miss)",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			score, explanation := scoreOutput(test.item, test.output)
			if score != test.wantScore {
				t.Errorf("scoreOutput() score = %v, want %v", score, test.wantScore)
			}
			if !strings.Contains(explanation, test.wantVerdict) {
				t.Errorf("scoreOutput() explanation = %q, want verdict %q", explanation, test.wantVerdict)
			}
		})
	}
}

func TestEmbeddedSuite(t *testing.T) {
	items, err := loadFixtures()
	if err != nil {
		t.Fatalf("loadFixtures() error = %v", err)
	}
	if len(items) != 27 {
		t.Fatalf("loadFixtures() count = %d, want 27", len(items))
	}
	if version := localSuiteVersion(); !strings.HasPrefix(version, "1.0.0+") {
		t.Errorf("localSuiteVersion() = %q, want digest version", version)
	}
}
