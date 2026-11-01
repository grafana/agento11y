package otel

import (
	"context"
	"sync"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// deliveryTrackingExporter remembers when the newest metrics it delivered
// were collected. The exporter reports cumulative totals, so a successful
// export carries every value recorded before its collection, and a later
// failed export takes nothing back.
type deliveryTrackingExporter struct {
	sdkmetric.Exporter
	mu          sync.Mutex
	deliveredAt time.Time
}

func (e *deliveryTrackingExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	// Read rm before exporting: the reader may reuse it afterwards.
	collectedAt, cumulative := collectionTime(rm)
	if err := e.Exporter.Export(ctx, rm); err != nil {
		return err
	}
	if !cumulative {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if collectedAt.After(e.deliveredAt) {
		e.deliveredAt = collectedAt
	}
	return nil
}

func (e *deliveryTrackingExporter) lastDelivered() time.Time {
	if e == nil {
		return time.Time{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.deliveredAt
}

// collectionTime returns the newest data point time in rm, which is when it
// was collected, and whether every sum and histogram in it is cumulative. A
// delta export carries only what changed since the previous one, so it says
// nothing about values recorded before that.
func collectionTime(rm *metricdata.ResourceMetrics) (time.Time, bool) {
	var latest time.Time
	cumulative := true
	note := func(at time.Time, temporality metricdata.Temporality) {
		if at.After(latest) {
			latest = at
		}
		if temporality != metricdata.CumulativeTemporality {
			cumulative = false
		}
	}
	for _, scope := range rm.ScopeMetrics {
		for _, m := range scope.Metrics {
			switch data := m.Data.(type) {
			case metricdata.Sum[int64]:
				note(newestPoint(data.DataPoints), data.Temporality)
			case metricdata.Sum[float64]:
				note(newestPoint(data.DataPoints), data.Temporality)
			case metricdata.Histogram[int64]:
				note(newestHistogramPoint(data.DataPoints), data.Temporality)
			case metricdata.Histogram[float64]:
				note(newestHistogramPoint(data.DataPoints), data.Temporality)
			}
		}
	}
	return latest, cumulative && !latest.IsZero()
}

func newestPoint[N int64 | float64](points []metricdata.DataPoint[N]) time.Time {
	var latest time.Time
	for _, point := range points {
		if point.Time.After(latest) {
			latest = point.Time
		}
	}
	return latest
}

func newestHistogramPoint[N int64 | float64](points []metricdata.HistogramDataPoint[N]) time.Time {
	var latest time.Time
	for _, point := range points {
		if point.Time.After(latest) {
			latest = point.Time
		}
	}
	return latest
}
