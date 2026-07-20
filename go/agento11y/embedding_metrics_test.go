package agento11y

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestStartEmbeddingRecordsDurationAndInputTokenMetrics(t *testing.T) {
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	t0 := time.Date(2026, 2, 17, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(500 * time.Millisecond)
	times := []time.Time{t0, t1}
	index := 0

	client := NewClient(Config{
		Meter: meterProvider.Meter("agento11y-test"),
		Now: func() time.Time {
			if index >= len(times) {
				return times[len(times)-1]
			}
			now := times[index]
			index++
			return now
		},
		testGenerationExporter: &capturingGenerationExporter{},
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, recorder := client.StartEmbedding(context.Background(), EmbeddingStart{
		Model:     ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
		AgentName: "agent-metrics",
	})
	recorder.SetResult(EmbeddingResult{
		InputCount:  2,
		InputTokens: 42,
	})
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("unexpected embedding recorder error: %v", err)
	}

	var collected metricdata.ResourceMetrics
	if err := metricReader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}

	durationPoints := histogramPointCountFloat64(collected, metricOperationDuration)
	if durationPoints == 0 {
		t.Fatalf("expected %s datapoints", metricOperationDuration)
	}

	tokenPoints := histogramPointCountInt64(collected, metricTokenUsage)
	if tokenPoints != 1 {
		t.Fatalf("expected 1 %s datapoint, got %d", metricTokenUsage, tokenPoints)
	}

	if ttftPoints := histogramPointCountFloat64(collected, metricTimeToFirstToken); ttftPoints != 0 {
		t.Fatalf("expected no %s datapoints for embeddings, got %d", metricTimeToFirstToken, ttftPoints)
	}
	if toolPoints := histogramPointCountInt64(collected, metricToolCallsPerOperation); toolPoints != 0 {
		t.Fatalf("expected no %s datapoints for embeddings, got %d", metricToolCallsPerOperation, toolPoints)
	}
}

func histogramPointCountFloat64(collected metricdata.ResourceMetrics, metricName string) int {
	count := 0
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != metricName {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				continue
			}
			count += len(histogram.DataPoints)
		}
	}
	return count
}

func histogramPointCountInt64(collected metricdata.ResourceMetrics, metricName string) int {
	count := 0
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != metricName {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[int64])
			if !ok {
				continue
			}
			count += len(histogram.DataPoints)
		}
	}
	return count
}
