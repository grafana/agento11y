package agento11y

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/exemplar"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func TestGenerationMetricsCarryTraceExemplar(t *testing.T) {
	spanRecorder, tp, metricReader, mp := newExemplarTestHarness(t, exemplar.AlwaysOnFilter)

	client := NewClient(Config{
		Tracer:                 tp.Tracer("agento11y-test"),
		Meter:                  mp.Meter("agento11y-test"),
		Now:                    time.Now,
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:     ModelRef{Provider: "openai", Name: "gpt-5"},
		AgentName: "test-agent",
	})
	rec.SetResult(Generation{
		Output: []Message{{Role: "assistant", Parts: []Part{{Kind: PartKindText, Text: "hello"}}}},
		Usage:  TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := spanRecorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	wantTraceID := spans[0].SpanContext().TraceID()

	assertExemplarTraceID(t, metricReader, metricOperationDuration, wantTraceID)
}

func TestEmbeddingMetricsCarryTraceExemplar(t *testing.T) {
	spanRecorder, tp, metricReader, mp := newExemplarTestHarness(t, exemplar.AlwaysOnFilter)

	client := NewClient(Config{
		Tracer:                 tp.Tracer("agento11y-test"),
		Meter:                  mp.Meter("agento11y-test"),
		Now:                    time.Now,
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, rec := client.StartEmbedding(context.Background(), EmbeddingStart{
		Model:     ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
		AgentName: "test-agent",
	})
	rec.SetResult(EmbeddingResult{InputTokens: 42, InputCount: 1})
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	spans := spanRecorder.Ended()
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
	wantTraceID := spans[0].SpanContext().TraceID()

	assertExemplarTraceID(t, metricReader, metricOperationDuration, wantTraceID)
}

func TestToolExecutionMetricsCarryTraceExemplar(t *testing.T) {
	for _, mode := range []ContentCaptureMode{
		ContentCaptureModeDefault, ContentCaptureModeFull, ContentCaptureModeNoToolContent,
		ContentCaptureModeMetadataOnly, ContentCaptureModeFullWithMetadataSpans,
	} {
		for _, kind := range []ToolExecutionKind{"", ToolExecutionKindLoadSkill, ToolExecutionKindReadSkillResource, ToolExecutionKindCommand} {
			t.Run(mode.String()+"/"+string(kind), func(t *testing.T) {
				spanRecorder, tp, metricReader, mp := newExemplarTestHarness(t, exemplar.AlwaysOnFilter)
				client := NewClient(Config{
					Tracer: tp.Tracer("agento11y-test"), Meter: mp.Meter("agento11y-test"),
					testGenerationExporter: &capturingGenerationExporter{},
				})
				t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
				_, rec := client.StartToolExecution(context.Background(), ToolExecutionStart{
					ToolName: "weather", AgentName: "test-agent", ExecutionKind: kind,
					SkillName: " review ", ContentCapture: mode, StartedAt: time.Unix(1, 0),
				})
				rec.SetResult(ToolExecutionEnd{Result: "sunny", CompletedAt: time.Unix(2, 0)})
				rec.End()
				rec.End()
				if err := rec.Err(); err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				spans := spanRecorder.Ended()
				if len(spans) != 1 {
					t.Fatalf("span count = %d, want 1", len(spans))
				}
				wantContext := spans[0].SpanContext()
				assertExemplarTraceID(t, metricReader, metricOperationDuration, wantContext.TraceID())
				histogram := collectToolDuration(t, metricReader)
				if len(histogram.DataPoints) != 1 {
					t.Fatalf("duration points = %d, want 1", len(histogram.DataPoints))
				}
				point := histogram.DataPoints[0]
				want := toolMetricBaseline("weather")
				want["gen_ai.agent.name"] = "test-agent"
				if kind != "" {
					want["gen_ai.skill.name"] = "review"
					want["agento11y.tool.execution.kind"] = string(kind)
				}
				assertToolMetricAttributes(t, point.Attributes, want)
				assertToolDuration(t, point, 1, 1)
				if len(point.Exemplars) != 1 {
					t.Fatalf("exemplars = %d, want 1", len(point.Exemplars))
				}
				ex := point.Exemplars[0]
				var spanID trace.SpanID
				copy(spanID[:], ex.SpanID)
				if spanID != wantContext.SpanID() || ex.Value != 1 || len(ex.FilteredAttributes) != 0 {
					t.Errorf("exemplar = %+v, want value 1, span %s, no filtered attributes", ex, wantContext.SpanID())
				}
			})
		}
	}
}

func TestToolExecutionSkillSeriesPreserveAggregate(t *testing.T) {
	var totals []metricdata.HistogramDataPoint[float64]
	for _, enriched := range []bool{false, true} {
		_, tp, reader, mp := newExemplarTestHarness(t, exemplar.AlwaysOffFilter)
		client := NewClient(Config{TracerProvider: tp, MeterProvider: mp, testGenerationExporter: &capturingGenerationExporter{}})
		t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
		for _, skill := range []string{"review", "security", "review"} {
			start := ToolExecutionStart{ToolName: "load", SkillName: skill, StartedAt: time.Unix(1, 0)}
			if enriched {
				start.ExecutionKind = ToolExecutionKindLoadSkill
			}
			_, rec := client.StartToolExecution(context.Background(), start)
			rec.SetResult(ToolExecutionEnd{CompletedAt: time.Unix(2, 0)})
			rec.End()
			rec.End()
			if err := rec.Err(); err != nil {
				t.Fatal(err)
			}
		}
		histogram := collectToolDuration(t, reader)
		wantPoints := 1
		if enriched {
			wantPoints = 2
		}
		if len(histogram.DataPoints) != wantPoints {
			t.Fatalf("enriched=%v: duration points = %d, want %d", enriched, len(histogram.DataPoints), wantPoints)
		}
		var total metricdata.HistogramDataPoint[float64]
		seen := map[string]bool{}
		for _, point := range histogram.DataPoints {
			want := toolMetricBaseline("load")
			count := uint64(3)
			if enriched {
				skill, _ := point.Attributes.Value("gen_ai.skill.name")
				name := skill.AsString()
				if (name != "review" && name != "security") || seen[name] {
					t.Fatalf("unexpected or duplicate skill series %q", name)
				}
				seen[name] = true
				count = 1
				if name == "review" {
					count = 2
				}
				want["gen_ai.skill.name"] = name
				want["agento11y.tool.execution.kind"] = "load_skill"
			}
			assertToolMetricAttributes(t, point.Attributes, want)
			assertToolDuration(t, point, count, 1)
			total.Count += point.Count
			total.Sum += point.Sum
			if total.BucketCounts == nil {
				total.BucketCounts = make([]uint64, len(point.BucketCounts))
			}
			for i, count := range point.BucketCounts {
				total.BucketCounts[i] += count
			}
		}
		totals = append(totals, total)
	}
	if totals[0].Count != totals[1].Count || totals[0].Sum != totals[1].Sum || !slices.Equal(totals[0].BucketCounts, totals[1].BucketCounts) {
		t.Errorf("aggregate changed: generic %+v, skills %+v", totals[0], totals[1])
	}
}

func TestSkillMetricsLeaveGenerationAndEmbeddingUnchanged(t *testing.T) {
	_, tp, reader, mp := newExemplarTestHarness(t, exemplar.AlwaysOffFilter)
	client := NewClient(Config{
		TracerProvider: tp, MeterProvider: mp, Now: func() time.Time { return time.Unix(2, 0) },
		Tags: map[string]string{"team": "sdk"}, testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	model := ModelRef{Provider: "provider", Name: "model"}
	_, tool := client.StartToolExecution(context.Background(), ToolExecutionStart{
		ToolName: "load", ExecutionKind: ToolExecutionKindLoadSkill, SkillName: "review",
		RequestProvider: model.Provider, RequestModel: model.Name, AgentName: "agent", StartedAt: time.Unix(1, 0),
	})
	tool.End()
	_, generation := client.StartGeneration(context.Background(), GenerationStart{
		Model: model, AgentName: "agent", OperationName: "chat", StartedAt: time.Unix(1, 0),
	})
	generation.SetResult(Generation{
		Output: []Message{AssistantTextMessage("hello")}, Usage: TokenUsage{InputTokens: 3, OutputTokens: 2},
	}, nil)
	generation.End()
	_, embedding := client.StartEmbedding(context.Background(), EmbeddingStart{
		Model: model, AgentName: "agent", StartedAt: time.Unix(1, 0),
	})
	embedding.SetResult(EmbeddingResult{InputCount: 1, InputTokens: 4})
	embedding.End()
	for _, err := range []error{tool.Err(), generation.Err(), embedding.Err()} {
		if err != nil {
			t.Fatal(err)
		}
	}
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatal(err)
	}
	identity := map[string]string{
		"gen_ai.provider.name": "provider", "gen_ai.request.model": "model", "gen_ai.agent.name": "agent", "agento11y.tag.team": "sdk",
	}
	inventory := map[string]int{}
	operations := map[string]bool{}
	tokens := map[string]int64{}
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			switch histogram := metric.Data.(type) {
			case metricdata.Histogram[float64]:
				if metric.Name != metricOperationDuration || metric.Unit != "s" {
					t.Fatalf("unexpected float histogram %s (%s)", metric.Name, metric.Unit)
				}
				for _, point := range histogram.DataPoints {
					op, _ := point.Attributes.Value("gen_ai.operation.name")
					operation := op.AsString()
					if operations[operation] {
						t.Fatalf("duplicate duration operation %q", operation)
					}
					operations[operation] = true
					want := maps.Clone(identity)
					want["gen_ai.operation.name"] = operation
					want["error.type"], want["error.category"] = "", ""
					if operation == "execute_tool" {
						want["gen_ai.tool.name"] = "load"
						want["gen_ai.skill.name"] = "review"
						want["agento11y.tool.execution.kind"] = "load_skill"
					}
					assertToolMetricAttributes(t, point.Attributes, want)
					assertToolDuration(t, point, 1, 1)
					inventory[metric.Name]++
				}
			case metricdata.Histogram[int64]:
				for _, point := range histogram.DataPoints {
					want := maps.Clone(identity)
					switch metric.Name {
					case metricTokenUsage:
						op, _ := point.Attributes.Value("gen_ai.operation.name")
						tokenType, _ := point.Attributes.Value("gen_ai.token.type")
						want["gen_ai.operation.name"] = op.AsString()
						want["gen_ai.token.type"] = tokenType.AsString()
						key := op.AsString() + "/" + tokenType.AsString()
						if _, duplicate := tokens[key]; duplicate {
							t.Fatalf("duplicate token series %s", key)
						}
						tokens[key] = point.Sum
					case metricToolCallsPerOperation:
						if point.Sum != 0 {
							t.Errorf("tool calls sum = %d, want 0", point.Sum)
						}
					default:
						t.Fatalf("unexpected integer histogram %s", metric.Name)
					}
					if point.Count != 1 {
						t.Errorf("%s count = %d, want 1", metric.Name, point.Count)
					}
					assertToolMetricAttributes(t, point.Attributes, want)
					inventory[metric.Name]++
				}
			default:
				t.Fatalf("unexpected instrument %s (%T)", metric.Name, metric.Data)
			}
		}
	}
	if !maps.Equal(inventory, map[string]int{metricOperationDuration: 3, metricTokenUsage: 3, metricToolCallsPerOperation: 1}) {
		t.Errorf("instrument points = %v", inventory)
	}
	if !maps.Equal(operations, map[string]bool{"execute_tool": true, "chat": true, "embeddings": true}) {
		t.Errorf("duration operations = %v", operations)
	}
	if !maps.Equal(tokens, map[string]int64{"chat/input": 3, "chat/output": 2, "embeddings/input": 4}) {
		t.Errorf("token sums = %v", tokens)
	}
}

func TestGenerationMetricsDoNotRetainRecordingContext(t *testing.T) {
	type contextKey struct{}
	type observation struct {
		spanContext trace.SpanContext
		recording   bool
		value       any
	}

	var observations []observation
	filter := func(ctx context.Context) bool {
		observations = append(observations, observation{
			spanContext: trace.SpanContextFromContext(ctx),
			recording:   trace.SpanFromContext(ctx).IsRecording(),
			value:       ctx.Value(contextKey{}),
		})
		return true
	}
	_, tp, _, mp := newExemplarTestHarness(t, filter)

	client := NewClient(Config{
		Tracer:                 tp.Tracer("agento11y-test"),
		Meter:                  mp.Meter("agento11y-test"),
		Now:                    time.Now,
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	callerCtx := context.WithValue(context.Background(), contextKey{}, "caller-owned")
	callCtx, rec := client.StartGeneration(callerCtx, GenerationStart{
		Model:     ModelRef{Provider: "openai", Name: "gpt-5"},
		AgentName: "test-agent",
	})
	wantSpanContext := trace.SpanContextFromContext(callCtx)
	rec.SetResult(Generation{
		Output: []Message{{Role: "assistant", Parts: []Part{{Kind: PartKindText, Text: "hello"}}}},
		Usage:  TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, nil)
	rec.End()

	if len(observations) == 0 {
		t.Fatal("expected the exemplar filter to observe metric contexts")
	}
	for i, got := range observations {
		if !got.spanContext.Equal(wantSpanContext) {
			t.Errorf("observation %d span context = %v, want %v", i, got.spanContext, wantSpanContext)
		}
		if got.recording {
			t.Errorf("observation %d retained a recording span", i)
		}
		if got.value != nil {
			t.Errorf("observation %d retained caller context value %v", i, got.value)
		}
	}
}

func newExemplarTestHarness(t *testing.T, filter exemplar.Filter) (*tracetest.SpanRecorder, *sdktrace.TracerProvider, *sdkmetric.ManualReader, *sdkmetric.MeterProvider) {
	t.Helper()

	spanRecorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	metricReader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(metricReader),
		sdkmetric.WithExemplarFilter(filter),
	)
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })

	return spanRecorder, tp, metricReader, mp
}

func assertExemplarTraceID(t *testing.T, metricReader *sdkmetric.ManualReader, metricName string, wantTraceID trace.TraceID) {
	t.Helper()

	var collected metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	found := false
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, m := range scopeMetrics.Metrics {
			if m.Name != metricName {
				continue
			}
			histogram, ok := m.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			for _, dp := range histogram.DataPoints {
				for _, ex := range dp.Exemplars {
					found = true
					var gotTraceID trace.TraceID
					copy(gotTraceID[:], ex.TraceID)
					if wantTraceID.IsValid() && gotTraceID != wantTraceID {
						t.Errorf("exemplar trace_id = %s, want %s", gotTraceID, wantTraceID)
					}
					if !gotTraceID.IsValid() {
						t.Error("exemplar trace_id is zero (no span context propagated)")
					}
					var gotSpanID trace.SpanID
					copy(gotSpanID[:], ex.SpanID)
					if !gotSpanID.IsValid() {
						t.Error("exemplar span_id is zero (no span context propagated)")
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("no exemplars found on %s histogram", metricName)
	}
}
