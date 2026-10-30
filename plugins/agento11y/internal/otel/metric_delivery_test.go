package otel

import (
	"context"
	"errors"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

type fakeMetricExporter struct {
	sdkmetric.Exporter
	err error
}

func (f *fakeMetricExporter) Export(context.Context, *metricdata.ResourceMetrics) error {
	return f.err
}

func tokenBatch(at time.Time, temporality metricdata.Temporality) *metricdata.ResourceMetrics {
	return &metricdata.ResourceMetrics{ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: "gen_ai.client.token.usage",
		Data: metricdata.Histogram[int64]{
			Temporality: temporality,
			DataPoints:  []metricdata.HistogramDataPoint[int64]{{Time: at, Count: 1, Sum: 53}},
		},
	}}}}}
}

// A successful cumulative export delivers everything recorded before it was
// collected, and a later failed export takes nothing back. A delta export
// carries only its own interval, so it proves nothing about earlier values.
func TestDeliveryTrackingExporter(t *testing.T) {
	collected := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	inner := &fakeMetricExporter{}
	exporter := &deliveryTrackingExporter{Exporter: inner}
	if got := exporter.lastDelivered(); !got.IsZero() {
		t.Fatalf("deliveredAt = %v before any export, want zero", got)
	}

	if err := exporter.Export(t.Context(), tokenBatch(collected, metricdata.CumulativeTemporality)); err != nil {
		t.Fatal(err)
	}
	if got := exporter.lastDelivered(); !got.Equal(collected) {
		t.Fatalf("deliveredAt = %v after a successful export, want %v", got, collected)
	}

	inner.err = errors.New("rejected")
	if err := exporter.Export(t.Context(), tokenBatch(collected.Add(time.Second), metricdata.CumulativeTemporality)); err == nil {
		t.Fatal("failed export returned nil")
	}
	if got := exporter.lastDelivered(); !got.Equal(collected) {
		t.Fatalf("deliveredAt = %v after a failed export, want it kept at %v", got, collected)
	}

	inner.err = nil
	if err := exporter.Export(t.Context(), tokenBatch(collected.Add(2*time.Second), metricdata.DeltaTemporality)); err != nil {
		t.Fatal(err)
	}
	if got := exporter.lastDelivered(); !got.Equal(collected) {
		t.Fatalf("deliveredAt = %v after a delta export, want it kept at %v", got, collected)
	}

	var nilProviders *Providers
	if got := nilProviders.MetricsDeliveredAt(); !got.IsZero() {
		t.Fatalf("nil providers deliveredAt = %v, want zero", got)
	}
}
