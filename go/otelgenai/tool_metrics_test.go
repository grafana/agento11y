package otelgenai_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/otelgenai"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const draftToolDuration = "gen_ai.execute_tool.duration"

var draftDurationBounds = []float64{.01, .02, .04, .08, .16, .32, .64, 1.28, 2.56, 5.12, 10.24, 20.48, 40.96, 81.92}

func draftToolPoint(t *testing.T, metrics map[string]metricdata.Metrics, name string, count uint64) metricdata.HistogramDataPoint[float64] {
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
			meter, collect := newMetricRecorder(t)
			h := otelgenai.NewHandler(meter)
			n := max(1, tc.Executions)
			for range n {
				inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: tc.Start["tool_name"],
					ExecutionKind: otelgenai.ToolExecutionKind(tc.Start["execution_kind"]), SkillName: tc.Start["skill_name"],
					StartedAt: startedAt, CompletedAt: startedAt.Add(time.Second), VerifiedSkillName: tc.End.Skill,
					VerifiedSkillResourceName: tc.End.Resource, ProcessExitCode: tc.End.Code, ProcessExitCodeIsError: tc.End.IsError}
				if tc.Error != "" {
					inv.ErrorType, inv.ErrorMessage = "tool_execution_error", tc.Error
				}
				ctx := h.Start(context.Background(), inv)
				for range max(1, tc.EndCalls) {
					h.End(ctx, inv)
				}
			}
			metrics := collect()
			legacy := draftToolPoint(t, metrics, "gen_ai.client.operation.duration", uint64(n))
			legacyError, _ := legacy.Attributes.Value("error.type")
			if (legacyError.AsString() != "") != (tc.Error != "") {
				t.Fatalf("legacy execution/exit error semantics changed: %v", legacyError)
			}
			if tc.Count == 0 {
				if _, ok := metrics[fixture.Instrument]; ok {
					t.Fatal("unexpected tool duration metric recorded")
				}
				return
			}
			p := draftToolPoint(t, metrics, fixture.Instrument, tc.Count)
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
			if !reflect.DeepEqual(p.Bounds, draftDurationBounds) {
				t.Fatalf("bounds: %v", p.Bounds)
			}
		})
	}
}

func TestToolMetricsReservedAttributesAndExtensions(t *testing.T) {
	data, err := os.ReadFile("../../conformance/tool-metrics/cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Reserved []string `json:"reserved_attributes"`
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "exec", ToolType: "function",
		ExecutionKind: otelgenai.ToolExecutionKindCommand, SkillName: "attempted", SkillResourceName: "attempt.py",
		VerifiedSkillName: "verified", VerifiedSkillResourceName: "verified.py", ProcessExitCode: new(int64(2)),
		Provider: "provider", RequestModel: "model", ResponseModel: "response", AgentName: "agent", AgentVersion: "v1",
		StartedAt: startedAt, CompletedAt: completedAt,
		MetricAttributes: []attribute.KeyValue{attribute.String("agento11y.tag.team", "sdk"), attribute.String("gen_ai.tool.name", "spoofed"), attribute.String("gen_ai.tool.type", "spoofed")},
		Attributes:       []attribute.KeyValue{attribute.String("gen_ai.skill.name", "span-only")},
	}
	for _, key := range append(fixture.Reserved, "gen_ai.conversation.id", "gen_ai.tool.call.id", "gen_ai.tool.description", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result") {
		inv.MetricAttributes = append(inv.MetricAttributes, attribute.String(key, "spoofed"))
	}
	metrics := collectMetricsWithDriver(t, inv, nil)
	p := draftToolPoint(t, metrics, draftToolDuration, 1)
	want := map[string]string{"gen_ai.tool.name": "exec", "gen_ai.tool.type": "function", "gen_ai.skill.name": "verified", "gen_ai.skill.resource.name": "verified.py",
		"agento11y.tool.execution.kind": "command", "error.type": "2", "agento11y.tag.team": "sdk", "gen_ai.provider.name": "provider", "gen_ai.request.model": "model", "gen_ai.response.model": "response", "gen_ai.agent.name": "agent", "gen_ai.agent.version": "v1"}
	for key, value := range want {
		v, ok := p.Attributes.Value(attribute.Key(key))
		if !ok || v.Type() != attribute.STRING || v.AsString() != value {
			t.Errorf("%s = %v, want %s", key, v, value)
		}
	}
	code, _ := p.Attributes.Value("process.exit.code")
	if code.Type() != attribute.INT64 || code.AsInt64() != 2 || p.Attributes.Len() != len(want)+1 {
		t.Fatalf("unexpected attrs: %v", p.Attributes)
	}
	legacy := draftToolPoint(t, metrics, "gen_ai.client.operation.duration", 1)
	v, _ := legacy.Attributes.Value("gen_ai.skill.name")
	if v.AsString() != "spoofed" {
		t.Fatal("legacy override behavior changed")
	}
}

func TestToolMetricsSerializationCaptureSampling(t *testing.T) {
	for _, capture := range []otelgenai.CaptureMode{otelgenai.CaptureNoContent, otelgenai.CaptureSpanOnly, otelgenai.CaptureEventOnly, otelgenai.CaptureSpanAndEvent} {
		for _, sampled := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/sampled=%v", capture, sampled), func(t *testing.T) {
				tp := sdktrace.NewTracerProvider(sdktrace.WithSampler(map[bool]sdktrace.Sampler{true: sdktrace.AlwaysSample(), false: sdktrace.NeverSample()}[sampled]))
				t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
				inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: startedAt, CompletedAt: completedAt, ToolCallResult: json.RawMessage(`{"broken":`)}
				metrics := collectMetricsWithDriver(t, inv, nil, otelgenai.WithCaptureMode(capture), otelgenai.WithTracerProvider(tp))
				for _, name := range []string{draftToolDuration, "gen_ai.client.operation.duration"} {
					p := draftToolPoint(t, metrics, name, 1)
					if _, ok := p.Attributes.Value("error.type"); ok {
						t.Fatalf("%s classified serialization as execution error", name)
					}
				}
			})
		}
	}
}

func TestToolMetricsHooks(t *testing.T) {
	for _, panicHook := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%v", panicHook), func(t *testing.T) {
			code, benign := int64(2), false
			inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "exec", ExecutionKind: otelgenai.ToolExecutionKindCommand,
				StartedAt: startedAt, CompletedAt: completedAt, VerifiedSkillName: "old", VerifiedSkillResourceName: "old.py", ProcessExitCode: &code, ProcessExitCodeIsError: &benign}
			original := *inv
			invCopy := *inv
			inv = &invCopy
			metrics := collectMetricsWithDriver(t, inv, func(ctx context.Context, h *otelgenai.Handler, inv *otelgenai.Invocation) {
				h.End(ctx, inv)
				h.End(ctx, inv)
			},
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv.ProcessExitCode = 3
					*inv.ProcessExitCodeIsError = true
					*inv = otelgenai.Invocation{VerifiedSkillName: "new", ProcessExitCode: new(int64(5)), ProcessExitCodeIsError: new(false)}
					if panicHook {
						panic("failed verifier")
					}
					return nil
				})),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					if panicHook {
						inv.VerifiedSkillName, inv.VerifiedSkillResourceName = "unsafe", "unsafe.py"
					}
					return nil
				})),
			)
			p := draftToolPoint(t, metrics, draftToolDuration, 1)
			v, present := p.Attributes.Value("gen_ai.skill.name")
			if present == panicHook || (!panicHook && v.AsString() != "new") {
				t.Fatalf("verified name = %v (%v)", v, present)
			}
			if _, ok := p.Attributes.Value("gen_ai.skill.resource.name"); ok {
				t.Fatal("stale or failed-hook resource")
			}
			if code != 2 || benign || original.VerifiedSkillName != "old" || original.VerifiedSkillResourceName != "old.py" {
				t.Fatal("hook mutated caller-owned completion snapshot")
			}
			draftToolPoint(t, metrics, "gen_ai.client.operation.duration", 1)
		})
	}
}

func TestToolMetricsConformantIndependence(t *testing.T) {
	for _, conformant := range []bool{false, true} {
		t.Run(fmt.Sprintf("conformant=%v", conformant), func(t *testing.T) {
			meter, collect := newMetricRecorder(t)
			opts := []otelgenai.Option{meter}
			if conformant {
				opts = append(opts, otelgenai.WithConformantMetrics())
			}
			h := otelgenai.NewHandler(opts...)
			inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: startedAt, CompletedAt: completedAt}
			h.End(context.Background(), inv)
			metrics := collect()
			draftToolPoint(t, metrics, draftToolDuration, 1)
			draftToolPoint(t, metrics, "gen_ai.client.operation.duration", 1)
		})
	}
}

type toolFailMeter struct{ metric.Meter }

func (m toolFailMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	if name == draftToolDuration {
		return nil, errors.New("tool histogram failed")
	}
	return m.Meter.Float64Histogram(name, opts...)
}

func TestToolMetricsInstrumentFailure(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	testOTelErrors.reset()
	h, spans := newRecordingHandler(t, otelgenai.WithMeterProvider(failingHistogramProvider{MeterProvider: mp, meter: toolFailMeter{mp.Meter(otelgenai.ScopeName)}}))
	inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: startedAt, CompletedAt: completedAt}
	ctx := h.Start(context.Background(), inv)
	h.End(ctx, inv)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]metricdata.Metrics{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			metrics[m.Name] = m
		}
	}
	draftToolPoint(t, metrics, "gen_ai.client.operation.duration", 1)
	if _, ok := metrics[draftToolDuration]; ok {
		t.Fatal("failed histogram recorded")
	}
	if len(spans.Ended()) != 1 || len(testOTelErrors.read()) != 1 {
		t.Fatal("instrument failure disabled spans or was not reported")
	}
}

func TestToolMetricsExemplarsAndBuckets(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithExemplarFilter(exemplar.AlwaysOnFilter))
	defer func() { _ = mp.Shutdown(context.Background()) }()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	defer func() { _ = tp.Shutdown(context.Background()) }()
	h := otelgenai.NewHandler(otelgenai.WithMeterProvider(mp), otelgenai.WithTracerProvider(tp))
	inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: startedAt, CompletedAt: startedAt.Add(40 * time.Millisecond)}
	ctx := h.Start(context.Background(), inv)
	h.End(ctx, inv)
	var data metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	metrics := map[string]metricdata.Metrics{}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			metrics[m.Name] = m
		}
	}
	p := draftToolPoint(t, metrics, draftToolDuration, 1)
	if !reflect.DeepEqual(p.Bounds, draftDurationBounds) || p.BucketCounts[2] != 1 || p.Sum != .04 {
		t.Fatalf("buckets/sum: %#v", p)
	}
	if len(p.Exemplars) != 1 {
		t.Fatalf("exemplars = %v", p.Exemplars)
	}
	sc := spans.Ended()[0].SpanContext()
	tid, sid := sc.TraceID(), sc.SpanID()
	if !reflect.DeepEqual(p.Exemplars[0].TraceID, tid[:]) || !reflect.DeepEqual(p.Exemplars[0].SpanID, sid[:]) {
		t.Fatal("exemplar identity mismatch")
	}
}

func TestToolMetricsKeepAttemptedSpanAndSamplingIdentity(t *testing.T) {
	meter, collect := newMetricRecorder(t)
	h, spans, sampler := newToolExecutionKindHandler(t, meter)
	inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "Skill", ExecutionKind: otelgenai.ToolExecutionKindLoadSkill, SkillName: "attempted", VerifiedSkillName: "verified", StartedAt: startedAt, CompletedAt: completedAt}
	ctx := h.Start(context.Background(), inv)
	h.End(ctx, inv)
	if spans.Ended()[0].Name() != "execute_tool Skill attempted" {
		t.Fatal("verified identity renamed the span")
	}
	for _, attrs := range [][]attribute.KeyValue{sampler.calls[0].Attributes, spans.Ended()[0].Attributes()} {
		set := attribute.NewSet(attrs...)
		v, _ := set.Value("gen_ai.skill.name")
		if v.AsString() != "attempted" {
			t.Fatalf("span/sampling identity = %v", v)
		}
	}
	p := draftToolPoint(t, collect(), draftToolDuration, 1)
	v, _ := p.Attributes.Value("gen_ai.skill.name")
	if v.AsString() != "verified" {
		t.Fatalf("metric verified identity = %v", v)
	}
}

func TestToolMetricsMeterProvider(t *testing.T) {
	for _, provider := range []string{"global", "nil", "noop"} {
		t.Run(provider, func(t *testing.T) {
			reader := sdkmetric.NewManualReader()
			mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
			t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
			previous := otel.GetMeterProvider()
			otel.SetMeterProvider(mp)
			t.Cleanup(func() { otel.SetMeterProvider(previous) })
			var opts []otelgenai.Option
			switch provider {
			case "nil":
				opts = append(opts, otelgenai.WithMeterProvider(nil))
			case "noop":
				opts = append(opts, otelgenai.WithMeterProvider(metricnoop.NewMeterProvider()))
			}
			h := otelgenai.NewHandler(opts...)
			inv := &otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: startedAt, CompletedAt: completedAt}
			h.End(context.Background(), inv)
			var data metricdata.ResourceMetrics
			if err := reader.Collect(context.Background(), &data); err != nil {
				t.Fatal(err)
			}
			metrics := map[string]metricdata.Metrics{}
			for _, scope := range data.ScopeMetrics {
				for _, m := range scope.Metrics {
					metrics[m.Name] = m
				}
			}
			if provider == "noop" {
				if len(metrics) != 0 {
					t.Fatal("explicit no-op provider recorded to global provider")
				}
				return
			}
			draftToolPoint(t, metrics, draftToolDuration, 1)
		})
	}
}

func TestToolMetricsTimingAndNonTools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		inv   otelgenai.Invocation
		count uint64
		sum   float64
	}{
		{"negative", otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", StartedAt: completedAt, CompletedAt: startedAt}, 1, 0},
		{"missing-name", otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, StartedAt: startedAt, CompletedAt: completedAt}, 0, 0},
		{"blank-name", otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: " \t\n", StartedAt: startedAt, CompletedAt: completedAt}, 0, 0},
		{"missing-start", otelgenai.Invocation{Operation: otelgenai.OperationExecuteTool, ToolName: "read", CompletedAt: completedAt}, 0, 0},
		{"chat", otelgenai.Invocation{StartedAt: startedAt, CompletedAt: completedAt}, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meter, collect := newMetricRecorder(t)
			h := otelgenai.NewHandler(meter)
			h.End(context.Background(), &tc.inv)
			metrics := collect()
			if tc.count == 0 {
				if _, ok := metrics[draftToolDuration]; ok {
					t.Fatal("unexpected duration")
				}
				return
			}
			p := draftToolPoint(t, metrics, draftToolDuration, tc.count)
			if p.Sum != tc.sum {
				t.Fatalf("duration = %v, want %v", p.Sum, tc.sum)
			}
		})
	}
}
