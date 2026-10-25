package otel

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// GenerationTracerProvider is deliberately separate from tool tracing. A
// generation span is the record itself: sampling or truncating it loses data.
// Nil means generation OTel export was not initialized, not a noop provider.
func (p *Providers) GenerationTracerProvider() trace.TracerProvider {
	if p == nil || p.generation == nil {
		return nil
	}
	return p.generation.tp
}

func (p *Providers) MeterProvider() metric.MeterProvider {
	if p == nil || p.mp == nil {
		return nil
	}
	return p.mp
}

// GenerationFlusher flushes generation records only, using the caller's
// context. Any export failure remains sticky for this provider's lifetime;
// tools and metrics retain their independent, non-sticky flush behavior.
func (p *Providers) GenerationFlusher() interface{ ForceFlush(context.Context) error } {
	if p == nil || p.generation == nil {
		return nil
	}
	return p.generation
}

func (p *Providers) GenerationEndpoint() string {
	if p == nil || p.generation == nil {
		return ""
	}
	return p.generation.endpoint
}

type generationProvider struct {
	endpoint string
	tp       *sdktrace.TracerProvider
	exporter *checkedTraceExporter
}

func newGenerationProvider(exporter sdktrace.SpanExporter, resource *sdkresource.Resource, ids sdktrace.IDGenerator) *generationProvider {
	checked := &checkedTraceExporter{SpanExporter: exporter}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(resource),
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		// Use raw limits: WithSpanLimits replaces zero/negative values with
		// defaults. Do not read NewSpanLimits, which imports ambient limits.
		sdktrace.WithRawSpanLimits(sdktrace.SpanLimits{
			AttributeValueLengthLimit:   -1,
			AttributeCountLimit:         -1,
			EventCountLimit:             -1,
			LinkCountLimit:              -1,
			AttributePerEventCountLimit: -1,
			AttributePerLinkCountLimit:  -1,
		}),
		sdktrace.WithBatcher(checked,
			sdktrace.WithMaxQueueSize(2048),
			sdktrace.WithMaxExportBatchSize(100),
			sdktrace.WithBlocking(),
			sdktrace.WithBatchTimeout(time.Second),
			sdktrace.WithExportTimeout(5*time.Second),
		),
	}
	if ids != nil {
		opts = append(opts, sdktrace.WithIDGenerator(ids))
	}
	return &generationProvider{tp: sdktrace.NewTracerProvider(opts...), exporter: checked}
}

func (p *generationProvider) ForceFlush(ctx context.Context) error {
	err := p.tp.ForceFlush(ctx)
	// The SDK can return nil after shutdown, even with a canceled context.
	// Never let that conceal a caller cancellation or a previous failed batch.
	return errors.Join(ctx.Err(), err, p.exporter.peekError())
}

func (p *generationProvider) shutdown(ctx context.Context) error {
	return errors.Join(p.tp.Shutdown(ctx), p.exporter.peekError())
}

func (e *checkedTraceExporter) peekError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}
