package agento11y

import (
	"context"
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
	spanRecorder, tp, metricReader, mp := newExemplarTestHarness(t, exemplar.AlwaysOnFilter)

	client := NewClient(Config{
		Tracer:                 tp.Tracer("agento11y-test"),
		Meter:                  mp.Meter("agento11y-test"),
		Now:                    time.Now,
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, rec := client.StartToolExecution(context.Background(), ToolExecutionStart{
		ToolName:  "weather",
		AgentName: "test-agent",
	})
	rec.SetResult(ToolExecutionEnd{
		Result: "sunny",
	})
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

func TestGenerationMetricsDoNotRetainRecordingContext(t *testing.T) {
	var observations []metricContextObservation
	_, tp, _, mp := newExemplarTestHarness(t, observeMetricContexts(&observations))

	client := NewClient(Config{
		Tracer:                 tp.Tracer("agento11y-test"),
		Meter:                  mp.Meter("agento11y-test"),
		Now:                    time.Now,
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	callerCtx := context.WithValue(context.Background(), exemplarContextKey{}, "caller-owned")
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

	assertDetachedMetricContexts(t, observations, wantSpanContext)
}

// TestOTelModeGenerationMetricsDoNotRetainRecordingContext covers the otel
// export path, where otelgenai records the spec instruments from the context
// the SDK hands to Handler.End.
func TestOTelModeGenerationMetricsDoNotRetainRecordingContext(t *testing.T) {
	var observations []metricContextObservation
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		meterProvider := sdkmetric.NewMeterProvider(
			sdkmetric.WithReader(sdkmetric.NewManualReader()),
			sdkmetric.WithExemplarFilter(observeMetricContexts(&observations)),
		)
		t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
		cfg.MeterProvider = meterProvider
	})

	callerCtx := context.WithValue(context.Background(), exemplarContextKey{}, "caller-owned")
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

	assertDetachedMetricContexts(t, observations, wantSpanContext)
}

// exemplarContextKey marks a value the caller owns. A metric context that
// still carries it is a context the reservoir should not have.
type exemplarContextKey struct{}

type metricContextObservation struct {
	spanContext trace.SpanContext
	recording   bool
	value       any
}

// observeMetricContexts returns an exemplar filter that records what every
// metric context still holds. The SDK metric pipeline calls the filter inside
// Record, so appends stay on the recording goroutine.
func observeMetricContexts(observations *[]metricContextObservation) exemplar.Filter {
	return func(ctx context.Context) bool {
		*observations = append(*observations, metricContextObservation{
			spanContext: trace.SpanContextFromContext(ctx),
			recording:   trace.SpanFromContext(ctx).IsRecording(),
			value:       ctx.Value(exemplarContextKey{}),
		})
		return true
	}
}

func assertDetachedMetricContexts(
	t *testing.T,
	observations []metricContextObservation,
	wantSpanContext trace.SpanContext,
) {
	t.Helper()

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
