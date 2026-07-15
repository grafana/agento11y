package sigil

import (
	"context"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

func TestContextTagsOnGenerationSpanMetricsAndExport(t *testing.T) {
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	exporter := &capturingGenerationExporter{}
	client, recorder, _ := newTestClient(t, Config{
		Meter:                  meterProvider.Meter("sigil-test"),
		testGenerationExporter: exporter,
		Now: func() time.Time {
			return time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
		},
	})

	ctx := WithTag(context.Background(), "origin", "workspace/open-in-sidebar")
	_, genRec := client.StartGeneration(ctx, GenerationStart{
		ConversationID: "conv-ctx-tags",
		Model:          ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	genRec.SetResult(Generation{
		Usage: TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, nil)
	genRec.End()

	originTagKey := spanAttrTagPrefix + "origin"

	// Trace: context tag present on the generation span as sigil.tag.origin.
	span := onlyGenerationSpan(t, recorder.Ended())
	if got := spanAttributeMap(span)[originTagKey].AsString(); got != "workspace/open-in-sidebar" {
		t.Fatalf("expected %s on generation span, got %q", originTagKey, got)
	}

	// Metrics: context tag becomes a metric dimension.
	collected := collectMetrics(t, metricReader)
	_ = findHistogramPointForTags(t, findHistogram(t, collected, metricOperationDuration), map[string]string{
		originTagKey: "workspace/open-in-sidebar",
	})

	// Export: context tag travels on the exported generation's tags.
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	if len(exporter.requests) != 1 || len(exporter.requests[0].Generations) != 1 {
		t.Fatalf("expected one exported generation, got %#v", exporter.requests)
	}
	if got := exporter.requests[0].Generations[0].GetTags()["origin"]; got != "workspace/open-in-sidebar" {
		t.Fatalf("expected origin on export tags, got %#v", exporter.requests[0].Generations[0].GetTags())
	}
}

func TestWithTagAccumulatesAndOverrides(t *testing.T) {
	ctx := WithTag(context.Background(), "origin", "a")
	ctx = WithTag(ctx, "surface", "sidebar")
	ctx = WithTag(ctx, "origin", "b") // override

	got := TagsFromContext(ctx)
	if got["origin"] != "b" {
		t.Fatalf("expected origin override to b, got %q", got["origin"])
	}
	if got["surface"] != "sidebar" {
		t.Fatalf("expected surface=sidebar, got %q", got["surface"])
	}

	// Mutating the returned copy must not affect the context.
	got["origin"] = "mutated"
	if TagsFromContext(ctx)["origin"] != "b" {
		t.Fatalf("TagsFromContext must return a copy")
	}
}

func TestContextTagEmptyKeyIsNoOp(t *testing.T) {
	ctx := WithTag(context.Background(), "", "value")
	if TagsFromContext(ctx) != nil {
		t.Fatalf("empty key must be a no-op")
	}
}
