package otelgenai

import (
	"context"
	"errors"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"
)

// durationBucketsSeconds is the conventions' bucket advice for the duration
// instruments.
var durationBucketsSeconds = []float64{
	0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
	2.56, 5.12, 10.24, 20.48, 40.96, 81.92,
}

// tokenUsageBuckets is the conventions' bucket advice for token usage: powers
// of 4 from 1 to ~67M tokens.
var tokenUsageBuckets = []float64{
	1, 4, 16, 64, 256, 1024, 4096, 16384,
	65536, 262144, 1048576, 4194304, 16777216, 67108864,
}

// These token types are outside the registry's input/output pair. An
// instrumentation opts into the series with WithExtendedTokenTypes when it
// needs cache and reasoning breakdowns.
const (
	TokenTypeCacheRead  = "cache_read"
	TokenTypeCacheWrite = "cache_write"
	TokenTypeReasoning  = "reasoning"
)

// instruments holds the spec metric instruments. The genaiconv constructors
// append their own unit and description to the bucket advice passed here.
type instruments struct {
	operationDuration  genaiconv.ClientOperationDuration
	tokenUsage         genaiconv.ClientTokenUsage
	timeToFirstChunk   genaiconv.ClientOperationTimeToFirstChunk
	timePerOutputChunk genaiconv.ClientOperationTimePerOutputChunk
	extendedTokenTypes bool
}

func newInstruments(meter metric.Meter) (instruments, error) {
	var out instruments
	errs := make([]error, 0, 4)
	var err error
	out.operationDuration, err = genaiconv.NewClientOperationDuration(
		meter,
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
	errs = append(errs, err)
	out.tokenUsage, err = genaiconv.NewClientTokenUsage(
		meter,
		metric.WithExplicitBucketBoundaries(tokenUsageBuckets...),
	)
	errs = append(errs, err)
	out.timeToFirstChunk, err = genaiconv.NewClientOperationTimeToFirstChunk(
		meter,
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
	errs = append(errs, err)
	out.timePerOutputChunk, err = genaiconv.NewClientOperationTimePerOutputChunk(
		meter,
		metric.WithExplicitBucketBoundaries(durationBucketsSeconds...),
	)
	errs = append(errs, err)
	return out, errors.Join(errs...)
}

// record emits the spec instruments for one completed invocation. extra
// carries the caller's own dimensions.
func (i instruments) record(ctx context.Context, inv *Invocation, extra []attribute.KeyValue) {
	// A workflow is not a client call, so it stays off the client instruments.
	// The conventions give it gen_ai.invoke_workflow.duration, which this
	// package does not record, so an invoke_workflow invocation reports a span
	// and no duration on any instrument.
	if inv.operation() == OperationInvokeWorkflow {
		return
	}

	if duration, ok := inv.duration(); ok {
		durationAttrs := extra
		if errorType := inv.errorType(); errorType != "" {
			durationAttrs = append(append([]attribute.KeyValue(nil), extra...),
				semconv.ErrorTypeKey.String(errorType))
		}
		i.operationDuration.RecordSet(ctx, duration, metricAttributeSet(inv, durationAttrs...))
	}

	if !inv.ttfcRecorded {
		if ttfc, ok := inv.timeToFirstChunk(); ok {
			i.timeToFirstChunk.RecordSet(ctx, ttfc, metricAttributeSet(inv, extra...))
		}
	}

	// Skip unreported usage for the same reason the span omits it: a count the
	// provider never returned is not a zero, and summing it would understate
	// the average.
	if !inv.Usage.reported() {
		return
	}
	type tokenBucket struct {
		tokenType string
		value     int64
	}
	buckets := []tokenBucket{
		{string(genaiconv.TokenTypeInput), inv.Usage.InputTokens},
		{string(genaiconv.TokenTypeOutput), inv.Usage.OutputTokens},
	}
	if i.extendedTokenTypes {
		buckets = append(buckets,
			tokenBucket{TokenTypeCacheRead, inv.Usage.CacheReadInputTokens},
			tokenBucket{TokenTypeCacheWrite, inv.Usage.CacheWriteInputTokens},
			tokenBucket{TokenTypeReasoning, inv.Usage.ReasoningTokens},
		)
	}
	for _, bucket := range buckets {
		if bucket.value == 0 {
			continue
		}
		attrs := append([]attribute.KeyValue(nil), extra...)
		if errorType := inv.errorType(); errorType != "" {
			attrs = append(attrs, semconv.ErrorTypeKey.String(errorType))
		}
		attrs = append(attrs, semconv.GenAITokenTypeKey.String(bucket.tokenType))
		i.tokenUsage.RecordSet(ctx, bucket.value, metricAttributeSet(inv, attrs...))
	}
}

func metricAttributeSet(inv *Invocation, attrs ...attribute.KeyValue) attribute.Set {
	attrs = append(attrs, semconv.GenAIOperationNameKey.String(string(inv.operation())))
	if inv.Provider != "" {
		attrs = append(attrs, semconv.GenAIProviderNameKey.String(inv.Provider))
	}
	return attribute.NewSet(attrs...)
}

func (i instruments) recordTimeToFirstChunk(ctx context.Context, inv *Invocation, seconds float64) {
	i.timeToFirstChunk.RecordSet(ctx, seconds, metricAttributeSet(inv, metricAttributes(inv)...))
}

func (i instruments) recordTimePerOutputChunk(ctx context.Context, inv *Invocation, seconds float64) {
	i.timePerOutputChunk.RecordSet(ctx, seconds, metricAttributeSet(inv, metricAttributes(inv)...))
}
