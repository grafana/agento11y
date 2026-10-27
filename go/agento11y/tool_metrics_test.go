package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/otelgenai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func toolMetricsCollect(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Metrics{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func toolMetricsPoint(t *testing.T, metrics map[string]metricdata.Metrics, name string, count uint64) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	m := metrics[name]
	h, ok := m.Data.(metricdata.Histogram[float64])
	if !ok || len(h.DataPoints) != 1 {
		t.Fatalf("%s = %#v, want one histogram series", name, m)
	}
	p := h.DataPoints[0]
	if p.Count != count {
		t.Fatalf("%s count = %d, want %d", name, p.Count, count)
	}
	return p
}

func toolMetricsClient(t *testing.T, cfg Config) (*Client, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	cfg.Meter = mp.Meter("tool-metrics-test")
	cfg.GenerationExport.Protocol = GenerationExportProtocolNone
	cfg.testDisableWorker = true
	c := NewClient(cfg)
	t.Cleanup(func() { _ = c.Shutdown(context.Background()) })
	return c, reader
}

func TestToolMetricsSharedCases(t *testing.T) {
	data, err := os.ReadFile("../../conformance/tool-metrics/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Instrument string   `json:"instrument"`
		Unit       string   `json:"unit"`
		Reserved   []string `json:"reserved_attributes"`
		Cases      []struct {
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
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	for _, tc := range fixture.Cases {
		t.Run(tc.ID, func(t *testing.T) {
			c, reader := toolMetricsClient(t, Config{})
			start := time.Unix(100, 0)
			n := max(1, tc.Executions)
			for range n {
				_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{
					ToolName: tc.Start["tool_name"], ExecutionKind: ToolExecutionKind(tc.Start["execution_kind"]),
					SkillName: tc.Start["skill_name"], StartedAt: start,
				})
				r.SetResult(ToolExecutionEnd{CompletedAt: start.Add(time.Second), VerifiedSkillName: tc.End.Skill,
					VerifiedSkillResourceName: tc.End.Resource, ProcessExitCode: tc.End.Code, ProcessExitCodeIsError: tc.End.IsError})
				if tc.Error != "" {
					r.SetExecError(errors.New(tc.Error))
				}
				for range max(1, tc.EndCalls) {
					r.End()
				}
			}
			metrics := toolMetricsCollect(t, reader)
			legacy := toolMetricsPoint(t, metrics, metricOperationDuration, uint64(n))
			legacyError, _ := legacy.Attributes.Value("error.type")
			if (legacyError.AsString() != "") != (tc.Error != "") {
				t.Fatalf("legacy execution/exit error semantics changed: %v", legacyError)
			}
			p := toolMetricsPoint(t, metrics, fixture.Instrument, tc.Count)
			if metrics[fixture.Instrument].Unit != fixture.Unit || p.Sum != float64(n) {
				t.Fatalf("unit/sum: %#v", metrics[fixture.Instrument])
			}
			got := map[string]any{}
			for _, kv := range p.Attributes.ToSlice() {
				if kv.Value.Type() == attribute.INT64 {
					got[string(kv.Key)] = float64(kv.Value.AsInt64())
				} else {
					if kv.Value.Type() != attribute.STRING {
						t.Fatalf("unexpected type: %v", kv)
					}
					got[string(kv.Key)] = kv.Value.AsString()
				}
			}
			if !reflect.DeepEqual(got, tc.Attributes) {
				t.Fatalf("attributes = %#v, want %#v", got, tc.Attributes)
			}
			if !reflect.DeepEqual(p.Bounds, durationBucketsSeconds) {
				t.Fatalf("bounds: %v", p.Bounds)
			}
		})
	}
}

func TestToolMetricsSerializationCaptureSampling(t *testing.T) {
	for _, mode := range []ContentCaptureMode{ContentCaptureModeFull, ContentCaptureModeNoToolContent, ContentCaptureModeMetadataOnly, ContentCaptureModeFullWithMetadataSpans} {
		for _, sampled := range []bool{true, false} {
			t.Run(fmt.Sprintf("mode=%d/sampled=%v", mode, sampled), func(t *testing.T) {
				spans := tracetest.NewSpanRecorder()
				tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans), sdktrace.WithSampler(map[bool]sdktrace.Sampler{true: sdktrace.AlwaysSample(), false: sdktrace.NeverSample()}[sampled]))
				t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
				c, reader := toolMetricsClient(t, Config{TracerProvider: tp, ContentCapture: mode})
				_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "read", IncludeContent: true, StartedAt: time.Unix(100, 0)})
				r.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(101, 0), Result: make(chan int)})
				r.End()
				metrics := toolMetricsCollect(t, reader)
				p := toolMetricsPoint(t, metrics, metricToolDuration, 1)
				if _, ok := p.Attributes.Value("error.type"); ok {
					t.Fatal("serialization became an execution error")
				}
				legacy := toolMetricsPoint(t, metrics, metricOperationDuration, 1)
				v, _ := legacy.Attributes.Value("error.type")
				wantError := mode == ContentCaptureModeFull || mode == ContentCaptureModeNoToolContent
				if (r.Err() != nil) != wantError || (v.AsString() != "") != wantError {
					t.Fatalf("legacy serialization semantics changed: %v, %v", r.Err(), v)
				}
				if sampled {
					want := codes.Ok
					if wantError {
						want = codes.Error
					}
					if spans.Ended()[0].Status().Code != want {
						t.Fatal("span status changed")
					}
				}
			})
		}
	}
}

func TestToolMetricsResultSnapshotReplacement(t *testing.T) {
	c, reader := toolMetricsClient(t, Config{})
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "exec", ExecutionKind: ToolExecutionKindCommand, StartedAt: time.Unix(100, 0)})
	r.SetResult(ToolExecutionEnd{VerifiedSkillName: "old", VerifiedSkillResourceName: "old.py", ProcessExitCode: new(int64(3)), ProcessExitCodeIsError: new(true)})
	code, benign := int64(2), false
	end := ToolExecutionEnd{VerifiedSkillName: "new", ProcessExitCode: &code, ProcessExitCodeIsError: &benign, CompletedAt: time.Unix(101, 0)}
	copyEnd := end
	r.SetResult(copyEnd)
	code, benign = 7, true
	end.VerifiedSkillName = "mutated"
	r.End()
	r.End()
	p := toolMetricsPoint(t, toolMetricsCollect(t, reader), metricToolDuration, 1)
	want := map[attribute.Key]attribute.Value{"gen_ai.skill.name": attribute.StringValue("new"), "process.exit.code": attribute.Int64Value(2)}
	for k, v := range want {
		got, _ := p.Attributes.Value(k)
		if got != v {
			t.Fatalf("%s = %v, want %v", k, got, v)
		}
	}
	for _, k := range []attribute.Key{"error.type", "gen_ai.skill.resource.name"} {
		if _, ok := p.Attributes.Value(k); ok {
			t.Fatalf("stale %s", k)
		}
	}
}

type optionalToolFailMeter struct{ metric.Meter }

func (m optionalToolFailMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	if name == metricToolDuration {
		return nil, errors.New("optional histogram failed")
	}
	return m.Meter.Float64Histogram(name, opts...)
}

func TestToolMetricsInstrumentFailure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	c := NewClient(Config{Meter: optionalToolFailMeter{mp.Meter("test")}, GenerationExport: GenerationExportConfig{Protocol: GenerationExportProtocolNone}, testDisableWorker: true})
	defer func() { _ = c.Shutdown(context.Background()) }()
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "read"})
	r.End()
	metrics := toolMetricsCollect(t, reader)
	toolMetricsPoint(t, metrics, metricOperationDuration, 1)
	if _, ok := metrics[metricToolDuration]; ok {
		t.Fatal("failed instrument recorded")
	}
}

func TestToolMetricsExemplarAndBuckets(t *testing.T) {
	spans, tp, reader, mp := newExemplarTestHarness(t, exemplar.AlwaysOnFilter)
	c := NewClient(Config{TracerProvider: tp, MeterProvider: mp, GenerationExport: GenerationExportConfig{Protocol: GenerationExportProtocolNone}, testDisableWorker: true})
	defer func() { _ = c.Shutdown(context.Background()) }()
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "read", StartedAt: time.Unix(100, 0)})
	r.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(100, 0).Add(40 * time.Millisecond)})
	r.End()
	p := toolMetricsPoint(t, toolMetricsCollect(t, reader), metricToolDuration, 1)
	if p.BucketCounts[2] != 1 || p.Sum != .04 {
		t.Fatalf("boundary bucket = %v, sum %v", p.BucketCounts, p.Sum)
	}
	assertExemplarTraceID(t, reader, metricToolDuration, spans.Ended()[0].SpanContext().TraceID())
}

func TestToolMetricsKeepAttemptedSpanAndSamplingIdentity(t *testing.T) {
	sampler := &toolExecutionKindSampler{}
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler), sdktrace.WithSpanProcessor(spans))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	c, reader := toolMetricsClient(t, Config{TracerProvider: tp, AgentName: "agent", AgentVersion: "v1", Tags: map[string]string{"team": "sdk"}})
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "Skill", ToolType: "function", ExecutionKind: ToolExecutionKindLoadSkill, SkillName: "attempted", SkillDescription: "private", SkillSourceURI: "file:///private/SKILL.md", RequestProvider: "provider", RequestModel: "model", StartedAt: time.Unix(100, 0)})
	r.SetResult(ToolExecutionEnd{VerifiedSkillName: "verified", CompletedAt: time.Unix(101, 0)})
	r.End()
	if spans.Ended()[0].Name() != "execute_tool Skill attempted" {
		t.Fatal("verified identity renamed the span")
	}
	for _, attrs := range [][]attribute.KeyValue{sampler.parameters[0].Attributes, spans.Ended()[0].Attributes()} {
		set := attribute.NewSet(attrs...)
		v, _ := set.Value("gen_ai.skill.name")
		if v.AsString() != "attempted" {
			t.Fatalf("span/sampling identity = %v", v)
		}
	}
	metrics := toolMetricsCollect(t, reader)
	p := toolMetricsPoint(t, metrics, metricToolDuration, 1)
	for key, want := range map[attribute.Key]string{"gen_ai.skill.name": "verified", "gen_ai.tool.type": "function", "gen_ai.agent.name": "agent", "gen_ai.agent.version": "v1", "gen_ai.provider.name": "provider", "gen_ai.request.model": "model", "agento11y.tag.team": "sdk"} {
		got, _ := p.Attributes.Value(key)
		if got.AsString() != want {
			t.Errorf("%s = %v, want %s", key, got, want)
		}
	}
	for _, key := range []attribute.Key{"gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.name", "process.executable.path", "gen_ai.tool.call.id", "gen_ai.conversation.id"} {
		if _, ok := p.Attributes.Value(key); ok {
			t.Errorf("private metric dimension: %s", key)
		}
	}
}

func TestToolMetricsExportProtocolOwnership(t *testing.T) {
	for _, tc := range []struct {
		protocol  GenerationExportProtocol
		operation string
	}{
		{GenerationExportProtocolNone, "generate"},
		{GenerationExportProtocolOTel, "generate"},
		{GenerationExportProtocolOTel, "execute_tool"},
	} {
		t.Run(string(tc.protocol)+"/"+tc.operation, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			defer func() { _ = mp.Shutdown(context.Background()) }()
			c := NewClient(Config{EnableExperimentalFeatures: new(true), MeterProvider: mp, GenerationExport: GenerationExportConfig{Protocol: tc.protocol}, testDisableWorker: true})
			defer func() { _ = c.Shutdown(context.Background()) }()
			if tc.protocol == GenerationExportProtocolOTel && c.otelHandler == nil {
				t.Fatal("OTel handler unavailable")
			}
			_, generation := c.StartGeneration(context.Background(), GenerationStart{OperationName: tc.operation, Model: ModelRef{Provider: "openai", Name: "test"}, StartedAt: time.Unix(100, 0)})
			generation.SetResult(Generation{CompletedAt: time.Unix(101, 0)}, nil)
			generation.End()
			_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "read", StartedAt: time.Unix(100, 0)})
			r.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(101, 0)})
			r.End()
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			var count uint64
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					if m.Name != metricToolDuration {
						continue
					}
					if scope.Scope.Name == otelgenai.ScopeName {
						t.Fatal("generation handler emitted tool metric")
					}
					for _, p := range m.Data.(metricdata.Histogram[float64]).DataPoints {
						count += p.Count
					}
				}
			}
			if count != 1 {
				t.Fatalf("total tool measurements = %d, want 1", count)
			}
		})
	}
}

func TestToolMetricsConcurrentEnd(t *testing.T) {
	c, reader := toolMetricsClient(t, Config{})
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "read", StartedAt: time.Unix(100, 0)})
	r.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(101, 0)})
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(r.End)
	}
	wg.Wait()
	metrics := toolMetricsCollect(t, reader)
	toolMetricsPoint(t, metrics, metricToolDuration, 1)
	toolMetricsPoint(t, metrics, metricOperationDuration, 1)
}

func TestToolMetricsNegativeDurationAndExecutionErrors(t *testing.T) {
	c, reader := toolMetricsClient(t, Config{})
	_, r := c.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "exec", ExecutionKind: ToolExecutionKindCommand, StartedAt: time.Unix(101, 0)})
	r.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(100, 0), ProcessExitCode: new(int64(-1)), ProcessExitCodeIsError: new(false)})
	r.SetExecError(context.DeadlineExceeded)
	r.End()
	p := toolMetricsPoint(t, toolMetricsCollect(t, reader), metricToolDuration, 1)
	v, _ := p.Attributes.Value("error.type")
	if p.Sum != 0 || v.AsString() != "tool_execution_error" {
		t.Fatalf("duration/error = %v/%v", p.Sum, v)
	}
}
