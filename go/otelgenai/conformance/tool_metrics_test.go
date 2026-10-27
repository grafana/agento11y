//go:build conformance && tool_metrics_draft

package conformance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/otelgenai"
	"github.com/grafana/agento11y/go/otelgenai/weavertest"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	lognoop "go.opentelemetry.io/otel/log/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const draftMetricName = "gen_ai.execute_tool.duration"
const draftMetricRef = "fb436c99dbe30d445c1fa952d67fdb74ce35dd4d"

var draftMetricBounds = []float64{.01, .02, .04, .08, .16, .32, .64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

var draftRefinementIDs = map[string]string{
	"load_skill":          "gen_ai.execute_tool.load_skill.duration",
	"read_skill_resource": "gen_ai.execute_tool.read_skill_resource",
	"command":             "gen_ai.execute_tool.command",
}

type draftMetricRule struct {
	Type        json.RawMessage `json:"type"`
	Requirement json.RawMessage `json:"requirement_level"`
}

type draftMetricDefinition struct {
	Name       string                     `json:"name"`
	Instrument string                     `json:"instrument"`
	Unit       string                     `json:"unit"`
	ValueType  string                     `json:"value_type"`
	Attributes map[string]draftMetricRule `json:"attributes"`
}

type draftMetricCoverage struct {
	Metrics map[string]struct {
		Attributes map[string]string `json:"attributes"`
	} `json:"metrics"`
	Base        draftMetricDefinition            `json:"tool_metric_base"`
	Refinements map[string]draftMetricDefinition `json:"tool_metric_refinements"`
}

type draftMetricCase struct {
	ID    string            `json:"id"`
	Start map[string]string `json:"start"`
	End   struct {
		Skill    string `json:"verified_skill_name"`
		Resource string `json:"verified_skill_resource_name"`
		Code     *int64 `json:"process_exit_code"`
		IsError  *bool  `json:"process_exit_code_is_error"`
	} `json:"end"`
	Error      string         `json:"error"`
	EndCalls   int            `json:"end_calls"`
	Executions int            `json:"executions"`
	Count      uint64         `json:"count"`
	Attributes map[string]any `json:"attributes"`
}

func TestToolMetricsDraftConformance(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Fatal("weaver is required; run test:go:tool-metrics-draft through mise")
	}
	if os.Getenv("SEMCONV_GENAI_REF") != draftMetricRef {
		t.Fatal("draft metric registry ref must be " + draftMetricRef)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	assets, err := weavertest.Setup(ctx, draftMetricRef)
	if err != nil {
		t.Fatal(err)
	}
	coverage := loadDraftMetricCoverage(t, assets)
	t.Run("extension allowlist is exact", func(t *testing.T) {
		valid := weavertest.Violation{ID: "missing_attribute", Message: "Attribute 'agento11y.tool.execution.kind' does not exist in the registry.", Context: map[string]any{"attribute_key": "agento11y.tool.execution.kind"}, SignalName: draftMetricName, SignalType: "metric"}
		if !allowedDraftMetricExtension(valid) {
			t.Fatal("approved extension rejected")
		}
		for _, mutate := range []func(*weavertest.Violation){
			func(v *weavertest.Violation) { v.ID = "type_mismatch" },
			func(v *weavertest.Violation) { v.Message = "another error" },
			func(v *weavertest.Violation) { v.SignalName = "another.metric" },
			func(v *weavertest.Violation) { v.SignalType = "span" },
			func(v *weavertest.Violation) { v.Context = map[string]any{"attribute_key": "unknown.extension"} },
			func(v *weavertest.Violation) {
				v.Context = map[string]any{"attribute_key": "agento11y.tool.execution.kind", "extra": true}
			},
		} {
			invalid := valid
			mutate(&invalid)
			if allowedDraftMetricExtension(invalid) {
				t.Fatalf("allowlisted unrelated error: %+v", invalid)
			}
		}
	})
	data, err := os.ReadFile("../../../conformance/tool-metrics/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version    int               `json:"version"`
		Revision   string            `json:"proposal_revision"`
		Instrument string            `json:"instrument"`
		Unit       string            `json:"unit"`
		Cases      []draftMetricCase `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Version != 1 || fixture.Revision != draftMetricRef || fixture.Instrument != draftMetricName || fixture.Unit != "s" || len(fixture.Cases) == 0 {
		t.Fatal("unexpected shared fixture contract")
	}
	snapshots := map[string]metricdata.ResourceMetrics{}
	cases := map[string]draftMetricCase{}
	for _, tc := range fixture.Cases {
		cases[tc.ID] = tc
		t.Run(tc.ID, func(t *testing.T) {
			for _, conformant := range []bool{false, true} {
				snapshot := collectDraftMetricCase(t, ctx, tc, conformant)
				if err := validateDraftMetricSnapshot(snapshot, tc, coverage); err != nil {
					t.Fatal(err)
				}
				if !conformant {
					snapshots[tc.ID] = snapshot
				}
			}
		})
	}
	if t.Failed() {
		t.FailNow()
	}
	// Export the collected, validated snapshots. A second recording could hide exporter mutations or timing changes.
	var positives []metricdata.ResourceMetrics
	for _, tc := range fixture.Cases {
		if tc.Count > 0 {
			positives = append(positives, snapshots[tc.ID])
		}
	}
	report := streamDraftMetrics(t, ctx, assets, positives)
	assertDraftMetricViolations(t, report, "", "")

	negatives := []struct {
		name, source, target, weaverID string
		mutate                         func(*metricdata.Metrics, *metricdata.HistogramDataPoint[float64])
	}{
		{"numeric error type", "command_exit", "error.type", "type_mismatch", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "error.type", attribute.Int64("error.type", 2))
		}},
		{"missing tool name", "verified_load", "gen_ai.tool.name", "required_attribute_not_present", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "gen_ai.tool.name")
		}},
		{"missing known skill", "verified_load", "gen_ai.skill.name", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "gen_ai.skill.name")
		}},
		{"missing known resource", "resource_only", "gen_ai.skill.resource.name", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "gen_ai.skill.resource.name")
		}},
		{"missing known command skill", "command_exit", "gen_ai.skill.name", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "gen_ai.skill.name")
		}},
		{"missing known command resource", "command_exit", "gen_ai.skill.resource.name", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "gen_ai.skill.resource.name")
		}},
		{"unknown extension", "verified_load", "unknown.extension", "missing_attribute", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "unknown.extension", attribute.String("unknown.extension", "unexpected"))
		}},
		{"string exit code", "command_exit", "process.exit.code", "type_mismatch", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			setDraftAttribute(p, "process.exit.code", attribute.String("process.exit.code", "2"))
		}},
		{"bad unit", "verified_load", "unit", "", func(m *metricdata.Metrics, _ *metricdata.HistogramDataPoint[float64]) { m.Unit = "ms" }},
		{"bad count", "repeat_loads", "count", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) { p.Count++ }},
		{"bad sum", "repeat_loads", "sum", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) { p.Sum++ }},
		{"bad bounds", "repeat_loads", "bounds", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) { p.Bounds[0] = .009 }},
		{"bad buckets", "repeat_loads", "buckets", "", func(_ *metricdata.Metrics, p *metricdata.HistogramDataPoint[float64]) {
			p.BucketCounts[0]++
			p.BucketCounts[1]--
		}},
	}
	for _, negative := range negatives {
		t.Run(negative.name, func(t *testing.T) {
			snapshot := cloneDraftMetricSnapshot(snapshots[negative.source])
			m := &snapshot.ScopeMetrics[0].Metrics[0]
			h := m.Data.(metricdata.Histogram[float64])
			negative.mutate(m, &h.DataPoints[0])
			m.Data = h
			err := validateDraftMetricSnapshot(snapshot, cases[negative.source], coverage)
			if err == nil || !strings.Contains(err.Error(), negative.target) {
				t.Fatalf("negative control must reject %s, got %v", negative.target, err)
			}
			if strings.HasPrefix(negative.name, "missing") && !strings.Contains(err.Error(), "missing required") {
				t.Fatalf("registry rule did not reject missing attribute: %v", err)
			}
			if negative.weaverID == "type_mismatch" && !strings.Contains(err.Error(), "type_mismatch") {
				t.Fatalf("registry type did not reject attribute: %v", err)
			}
			if err := validateDraftMetricSnapshot(snapshots[negative.source], cases[negative.source], coverage); err != nil {
				t.Fatalf("negative control mutated original: %v", err)
			}
			if negative.weaverID != "" || strings.HasPrefix(negative.name, "missing") {
				report := streamDraftMetrics(t, ctx, assets, []metricdata.ResourceMetrics{snapshot})
				assertDraftMetricViolations(t, report, negative.weaverID, negative.target)
			}
		})
	}
}

func loadDraftMetricCoverage(t *testing.T, assets weavertest.Assets) draftMetricCoverage {
	t.Helper()
	paths, err := filepath.Glob(assets.AdviceData)
	if err != nil {
		t.Fatal(err)
	}
	var model draftMetricCoverage
	for _, path := range paths {
		if filepath.Base(path) != "coverage-model.json" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &model); err != nil {
			t.Fatal(err)
		}
	}
	if len(model.Refinements) != 3 {
		t.Fatalf("selected %d metric refinements, want exactly three", len(model.Refinements))
	}
	if model.Base.Name != draftMetricName || model.Base.Instrument != "histogram" || model.Base.Unit != "s" || model.Base.ValueType != "double" {
		t.Fatalf("bad base metric: %+v", model.Base)
	}
	base := model.Metrics[draftMetricName].Attributes
	if base["gen_ai.tool.name"] != "required" || base["error.type"] != "conditionally_required_conditional" || base["gen_ai.tool.type"] != "recommended_conditional" || base["gen_ai.agent.name"] != "conditionally_required_conditional" || len(base) != 4 {
		t.Fatalf("existing base coverage shape changed: %v", base)
	}
	for _, id := range draftRefinementIDs {
		definition, ok := model.Refinements[id]
		if !ok || definition.Name != model.Base.Name || definition.Instrument != model.Base.Instrument || definition.Unit != model.Base.Unit || definition.ValueType != model.Base.ValueType {
			t.Fatalf("bad refinement %s: %+v", id, definition)
		}
		if _, ok := model.Metrics[id]; ok {
			t.Fatalf("refinement %s leaked into base metrics", id)
		}
		wantAttributes := 5
		if id == draftRefinementIDs["read_skill_resource"] {
			wantAttributes = 6
		}
		if id == draftRefinementIDs["command"] {
			wantAttributes = 7
		}
		if len(definition.Attributes) != wantAttributes {
			t.Fatalf("%s has %d attributes, want %d", id, len(definition.Attributes), wantAttributes)
		}
		for key, rule := range model.Base.Attributes {
			if !reflect.DeepEqual(definition.Attributes[key], rule) {
				t.Fatalf("%s did not inherit %s", id, key)
			}
		}
	}
	for key := range model.Base.Attributes {
		if strings.HasPrefix(key, "gen_ai.skill.") || strings.HasPrefix(key, "process.") {
			t.Fatalf("refinement leaked into base: %s", key)
		}
	}
	return model
}

func collectDraftMetricCase(t *testing.T, ctx context.Context, tc draftMetricCase, conformant bool) metricdata.ResourceMetrics {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	opts := []otelgenai.Option{otelgenai.WithMeterProvider(provider), otelgenai.WithTracerProvider(tracenoop.NewTracerProvider()), otelgenai.WithLoggerProvider(lognoop.NewLoggerProvider())}
	if conformant {
		opts = append(opts, otelgenai.WithConformantMetrics())
	}
	handler := otelgenai.NewHandler(opts...)
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range max(1, tc.Executions) {
		inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: tc.Start["tool_name"], ToolType: "function", AgentName: "draft-agent",
			Provider: "provider", RequestModel: "request", ResponseModel: "response", AgentVersion: "v1", MetricAttributes: []attribute.KeyValue{attribute.String("agento11y.tag.team", "sdk")},
			ExecutionKind: otelgenai.ToolExecutionKind(tc.Start["execution_kind"]), SkillName: tc.Start["skill_name"],
			VerifiedSkillName: tc.End.Skill, VerifiedSkillResourceName: tc.End.Resource, ProcessExitCode: tc.End.Code, ProcessExitCodeIsError: tc.End.IsError,
			StartedAt: start, CompletedAt: start.Add(time.Duration(i+1) * 20 * time.Millisecond)}
		if tc.Error != "" {
			inv.ErrorType, inv.ErrorMessage = "tool_execution_error", tc.Error
		}
		callCtx := handler.Start(ctx, inv)
		for range max(1, tc.EndCalls) {
			handler.End(callCtx, inv)
		}
	}
	var snapshot metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &snapshot); err != nil {
		t.Fatal(err)
	}
	return cloneDraftMetricSnapshot(snapshot)
}

func cloneDraftMetricSnapshot(source metricdata.ResourceMetrics) metricdata.ResourceMetrics {
	out := metricdata.ResourceMetrics{Resource: source.Resource}
	for _, scope := range source.ScopeMetrics {
		copyScope := scope
		copyScope.Metrics = nil
		for _, metric := range scope.Metrics {
			if metric.Name != draftMetricName {
				continue
			}
			if h, ok := metric.Data.(metricdata.Histogram[float64]); ok {
				h.DataPoints = slices.Clone(h.DataPoints)
				for i := range h.DataPoints {
					p := &h.DataPoints[i]
					p.Bounds, p.BucketCounts, p.Exemplars = slices.Clone(p.Bounds), slices.Clone(p.BucketCounts), slices.Clone(p.Exemplars)
					for j := range p.Exemplars {
						p.Exemplars[j].FilteredAttributes = slices.Clone(p.Exemplars[j].FilteredAttributes)
					}
				}
				metric.Data = h
			}
			copyScope.Metrics = append(copyScope.Metrics, metric)
		}
		if len(copyScope.Metrics) > 0 {
			out.ScopeMetrics = append(out.ScopeMetrics, copyScope)
		}
	}
	return out
}

func validateDraftMetricSnapshot(snapshot metricdata.ResourceMetrics, tc draftMetricCase, model draftMetricCoverage) error {
	var metrics []metricdata.Metrics
	for _, scope := range snapshot.ScopeMetrics {
		metrics = append(metrics, scope.Metrics...)
	}
	if tc.Count == 0 {
		if len(metrics) != 0 {
			return fmt.Errorf("unexpected tool duration metric emitted")
		}
		return nil
	}
	if len(metrics) != 1 {
		return fmt.Errorf("got %d draft metrics, want one", len(metrics))
	}
	m := metrics[0]
	if m.Name != model.Base.Name || m.Unit != model.Base.Unit {
		return fmt.Errorf("metric name/unit = %s/%s", m.Name, m.Unit)
	}
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 || h.Temporality != metricdata.CumulativeTemporality {
		return fmt.Errorf("want cumulative Float64Histogram with one series, got %T", m.Data)
	}
	p := h.DataPoints[0]
	if p.Count != tc.Count {
		return fmt.Errorf("count = %d, want %d", p.Count, tc.Count)
	}
	var sum float64
	buckets := make([]uint64, len(draftMetricBounds)+1)
	for i := range max(1, tc.Executions) {
		value := float64(i+1) * .02
		sum += value
		index := sortDraftBucket(value)
		buckets[index]++
	}
	if math.Abs(p.Sum-sum) > 1e-12 {
		return fmt.Errorf("sum = %g, want %g", p.Sum, sum)
	}
	if !slices.Equal(p.Bounds, draftMetricBounds) {
		return fmt.Errorf("bounds = %v", p.Bounds)
	}
	if !slices.Equal(p.BucketCounts, buckets) {
		return fmt.Errorf("buckets = %v, want %v", p.BucketCounts, buckets)
	}
	if err := validateDraftMetricRules(p.Attributes, tc, model.Base); err != nil {
		return fmt.Errorf("base: %w", err)
	}
	if id := draftRefinementIDs[tc.Start["execution_kind"]]; id != "" {
		if err := validateDraftMetricRules(p.Attributes, tc, model.Refinements[id]); err != nil {
			return fmt.Errorf("%s: %w", id, err)
		}
	}
	want := []attribute.KeyValue{attribute.String("gen_ai.tool.type", "function"), attribute.String("gen_ai.agent.name", "draft-agent"), attribute.String("gen_ai.provider.name", "provider"), attribute.String("gen_ai.request.model", "request"), attribute.String("gen_ai.response.model", "response"), attribute.String("gen_ai.agent.version", "v1"), attribute.String("agento11y.tag.team", "sdk")}
	for key, value := range tc.Attributes {
		switch value := value.(type) {
		case string:
			want = append(want, attribute.String(key, value))
		case float64:
			want = append(want, attribute.Int64(key, int64(value)))
		default:
			return fmt.Errorf("unsupported fixture attribute %s", key)
		}
	}
	expected := attribute.NewSet(want...)
	if !p.Attributes.Equals(&expected) {
		return fmt.Errorf("exact typed attributes = %v, want %v", p.Attributes, expected)
	}
	return nil
}

func sortDraftBucket(value float64) int {
	for i, bound := range draftMetricBounds {
		if value <= bound {
			return i
		}
	}
	return len(draftMetricBounds)
}

func validateDraftMetricRules(attrs attribute.Set, tc draftMetricCase, definition draftMetricDefinition) error {
	for key, rule := range definition.Attributes {
		required := false
		var level string
		if json.Unmarshal(rule.Requirement, &level) == nil {
			switch level {
			case "required":
				required = true
			case "recommended", "opt_in":
			default:
				return fmt.Errorf("unknown requirement %s: %s", key, level)
			}
		} else {
			var conditional map[string]string
			if err := json.Unmarshal(rule.Requirement, &conditional); err != nil {
				return err
			}
			for level, wording := range conditional {
				if level == "recommended" {
					if wording != "If available." {
						return fmt.Errorf("unknown recommendation %s: %s", key, wording)
					}
					continue
				}
				if level != "conditionally_required" {
					return fmt.Errorf("unknown requirement %s", level)
				}
				// Only fixture inputs establish availability. Missing output labels cannot turn these conditions off.
				switch wording {
				case "If the operation ended in an error.":
					required = tc.Error != "" || (tc.Start["execution_kind"] == "command" && tc.End.Code != nil && *tc.End.Code != 0 && (tc.End.IsError == nil || *tc.End.IsError))
				case "When applicable.":
					required = true
				case "If the skill exists and its name is available.", "If the command is associated with an existing skill and its name is available.":
					required = strings.TrimSpace(tc.End.Skill) != ""
				case "If the skill resource exists and its name is available.", "If the command executes an existing skill resource and its name is available.":
					required = strings.TrimSpace(tc.End.Resource) != ""
				case "If the tool reports a single command or process exit code.":
					required = tc.End.Code != nil
				default:
					return fmt.Errorf("unknown condition %s: %s", key, wording)
				}
			}
		}
		value, present := attrs.Value(attribute.Key(key))
		if required && !present {
			return fmt.Errorf("missing required %s (%s)", key, rule.Requirement)
		}
		if !present {
			continue
		}
		var scalar string
		if json.Unmarshal(rule.Type, &scalar) != nil {
			var enum struct {
				Members []struct {
					Value any `json:"value"`
				} `json:"members"`
			}
			if err := json.Unmarshal(rule.Type, &enum); err != nil || len(enum.Members) == 0 {
				return fmt.Errorf("unsupported type %s: %s", key, rule.Type)
			}
			// Registry enums are open. Their member values still determine the attribute's scalar type.
			for _, member := range enum.Members {
				if _, ok := member.Value.(string); !ok {
					return fmt.Errorf("unsupported enum type %s", key)
				}
			}
			scalar = "string"
		}
		if (scalar == "string" && value.Type() != attribute.STRING) || (scalar == "int" && value.Type() != attribute.INT64) {
			return fmt.Errorf("type_mismatch %s: got %v, want %s", key, value.Type(), scalar)
		}
		if scalar != "string" && scalar != "int" {
			return fmt.Errorf("unsupported scalar %s: %s", key, scalar)
		}
	}
	return nil
}

func setDraftAttribute(p *metricdata.HistogramDataPoint[float64], key string, replacement ...attribute.KeyValue) {
	attrs := p.Attributes.ToSlice()
	attrs = slices.DeleteFunc(attrs, func(kv attribute.KeyValue) bool { return string(kv.Key) == key })
	p.Attributes = attribute.NewSet(append(attrs, replacement...)...)
}

func streamDraftMetrics(t *testing.T, ctx context.Context, assets weavertest.Assets, snapshots []metricdata.ResourceMetrics) weavertest.Report {
	t.Helper()
	runner, err := weavertest.Start(ctx, assets)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	exporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(runner.Endpoint()), otlpmetricgrpc.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exporter.Shutdown(context.Background()) }()
	for _, snapshot := range snapshots {
		if err := exporter.Export(ctx, &snapshot); err != nil {
			t.Fatal(err)
		}
	}
	report, err := runner.End(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if names := report.SeenMetricNames(); len(names) != 1 {
		t.Fatalf("Weaver did not receive only the draft instrument: %v", names)
	} else if _, ok := names[draftMetricName]; !ok {
		t.Fatal("Weaver did not receive the draft instrument")
	}
	if len(report.SpanOperationCounts()) != 0 {
		t.Fatal("draft profile received spans")
	}
	assertDraftMetricReceived(t, report, snapshots)
	return report
}

func assertDraftMetricReceived(t *testing.T, report weavertest.Report, snapshots []metricdata.ResourceMetrics) {
	t.Helper()
	var want, got []string
	encode := func(name, instrument, unit any, points []any) string {
		data, err := json.Marshal(map[string]any{"name": name, "instrument": instrument, "unit": unit, "data_points": points})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, snapshot := range snapshots {
		for _, scope := range snapshot.ScopeMetrics {
			for _, metric := range scope.Metrics {
				h := metric.Data.(metricdata.Histogram[float64])
				var points []any
				for _, p := range h.DataPoints {
					var attrs []any
					for _, kv := range p.Attributes.ToSlice() {
						typ := "string"
						var value any = kv.Value.AsString()
						if kv.Value.Type() == attribute.INT64 {
							typ, value = "int", kv.Value.AsInt64()
						}
						attrs = append(attrs, map[string]any{"name": string(kv.Key), "type": typ, "value": value})
					}
					points = append(points, map[string]any{"attributes": attrs, "count": p.Count, "sum": p.Sum, "explicit_bounds": p.Bounds, "bucket_counts": p.BucketCounts})
				}
				want = append(want, encode(metric.Name, "histogram", metric.Unit, points))
			}
		}
	}
	samples, _ := report.Raw["samples"].([]any)
	for _, raw := range samples {
		sample, _ := raw.(map[string]any)
		if _, present := sample["span"]; present {
			t.Fatal("draft profile received a span")
		}
		metric, ok := sample["metric"].(map[string]any)
		if !ok {
			continue
		}
		var points []any
		dataPoints, _ := metric["data_points"].([]any)
		for _, raw := range dataPoints {
			point, _ := raw.(map[string]any)
			var attrs []any
			attributes, _ := point["attributes"].([]any)
			for _, raw := range attributes {
				attr, _ := raw.(map[string]any)
				attrs = append(attrs, map[string]any{"name": attr["name"], "type": attr["type"], "value": attr["value"]})
			}
			points = append(points, map[string]any{"attributes": attrs, "count": point["count"], "sum": point["sum"], "explicit_bounds": point["explicit_bounds"], "bucket_counts": point["bucket_counts"]})
		}
		got = append(got, encode(metric["name"], metric["instrument"], metric["unit"], points))
	}
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("Weaver did not receive the same histogram snapshots:\ngot %v\nwant %v", got, want)
	}
}

func allowedDraftMetricExtension(v weavertest.Violation) bool {
	context, ok := v.Context.(map[string]any)
	if !ok || len(context) != 1 || v.ID != "missing_attribute" || v.SignalType != "metric" || v.SignalName != draftMetricName {
		return false
	}
	key, _ := context["attribute_key"].(string)
	switch key {
	case "agento11y.tool.execution.kind", "gen_ai.provider.name", "gen_ai.request.model", "gen_ai.response.model", "gen_ai.agent.version", "agento11y.tag.team":
		return v.Message == fmt.Sprintf("Attribute '%s' does not exist in the registry.", key)
	default:
		return false
	}
}

func assertDraftMetricViolations(t *testing.T, report weavertest.Report, wantID, target string) {
	t.Helper()
	found := false
	allowedCount, expectedCount := 0, 0
	for _, violation := range report.Violations() {
		encoded, _ := json.Marshal(violation)
		context, _ := violation.Context.(map[string]any)
		if wantID != "" && violation.ID == wantID && context["attribute_key"] == target && violation.SignalName == draftMetricName && violation.SignalType == "metric" {
			found = true
			expectedCount += violation.Count
			continue
		}
		if allowedDraftMetricExtension(violation) {
			allowedCount += violation.Count
			continue
		}
		t.Errorf("unexpected Weaver violation: %s", encoded)
	}
	if wantID != "" && !found {
		t.Errorf("Weaver did not reject %s with %s", target, wantID)
	}
	if target != "" && wantID == "" {
		t.Logf("%s: fixture-aware local rule rejected the missing identity; Weaver has no fixture availability context", target)
	}
	levels := map[string]int{}
	var walk func(any)
	walk = func(value any) {
		switch value := value.(type) {
		case map[string]any:
			if result, ok := value["live_check_result"].(map[string]any); ok {
				advice, _ := result["all_advice"].([]any)
				for _, item := range advice {
					finding, _ := item.(map[string]any)
					level, _ := finding["level"].(string)
					levels[level]++
				}
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(report.Raw)
	t.Logf("Weaver advice levels=%v; allowlisted extension violations=%d, expected negative violations=%d; Development not_stable advice is improvement-level", levels, allowedCount, expectedCount)
}
