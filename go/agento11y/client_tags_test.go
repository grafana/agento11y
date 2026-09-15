package agento11y

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/grafana/agento11y/go/otelgenai"
)

const clientTagProjectKey = spanAttrTagPrefix + "project"

func TestClientTagsOnGenerationSpanAndMetrics(t *testing.T) {
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() {
		_ = meterProvider.Shutdown(context.Background())
	})

	client, recorder, _ := newTestClient(t, Config{
		Tags:  map[string]string{"project": "checkout-svc"},
		Meter: meterProvider.Meter("agento11y-test"),
		Now: func() time.Time {
			return time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
		},
	})

	_, genRec := client.StartGeneration(context.Background(), GenerationStart{
		ConversationID: "conv-tags",
		Model:          ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	genRec.SetResult(Generation{
		Usage: TokenUsage{InputTokens: 10, OutputTokens: 5},
	}, nil)
	genRec.End()

	span := onlyGenerationSpan(t, recorder.Ended())
	attrs := spanAttributeMap(span)
	if got := attrs[clientTagProjectKey].AsString(); got != "checkout-svc" {
		t.Fatalf("expected %s=checkout-svc on generation span, got %q", clientTagProjectKey, got)
	}

	collected := collectMetrics(t, metricReader)
	_ = findHistogramPointForTags(t, findHistogram(t, collected, metricOperationDuration), map[string]string{
		clientTagProjectKey: "checkout-svc",
	})
}

func TestGenerationTokenMetricsPreserveReportedZero(t *testing.T) {
	tests := []struct {
		name      string
		tokenType string
		usage     func(int64) TokenUsage
	}{
		{
			name:      "input",
			tokenType: metricTokenTypeInput,
			usage: func(value int64) TokenUsage {
				return TokenUsage{InputTokens: value, InputTokensReported: true}
			},
		},
		{
			name:      "output",
			tokenType: metricTokenTypeOutput,
			usage: func(value int64) TokenUsage {
				return TokenUsage{OutputTokens: value, OutputTokensReported: true}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			metricReader := sdkmetric.NewManualReader()
			meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
			t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
			client, _, _ := newTestClient(t, Config{
				Meter: meterProvider.Meter("agento11y-test"),
				Now:   time.Now,
			})

			for _, value := range []int64{0, 10} {
				_, rec := client.StartGeneration(context.Background(), GenerationStart{
					Model: ModelRef{Provider: "openai", Name: "gpt-5"},
				})
				rec.SetResult(Generation{Usage: test.usage(value)}, nil)
				rec.End()
				if err := rec.Err(); err != nil {
					t.Fatalf("record %s token count %d: %v", test.tokenType, value, err)
				}
			}

			collected := collectMetrics(t, metricReader)
			for _, scope := range collected.ScopeMetrics {
				for _, metric := range scope.Metrics {
					if metric.Name != metricTokenUsage {
						continue
					}
					histogram, ok := metric.Data.(metricdata.Histogram[int64])
					if !ok {
						t.Fatalf("%s data = %T, want int64 histogram", metricTokenUsage, metric.Data)
					}
					for _, point := range histogram.DataPoints {
						tokenType, _ := point.Attributes.Value(attribute.Key(metricAttrTokenType))
						if tokenType.AsString() == test.tokenType {
							if point.Count != 2 || point.Sum != 10 {
								t.Fatalf("%s token count = %d, sum = %d; want 2 and 10", test.tokenType, point.Count, point.Sum)
							}
							return
						}
					}
				}
			}
			t.Fatalf("%s token histogram point not found", test.tokenType)
		})
	}
}

func TestFetchResponseSuppressesProprietaryUsageTelemetry(t *testing.T) {
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	client, recorder, _ := newTestClient(t, Config{
		Meter: meterProvider.Meter("agento11y-test"),
		Now:   time.Now,
	})
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		OperationName: string(otelgenai.OperationFetchResponse),
		Model:         ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{Usage: TokenUsage{
		InputTokens:          7,
		OutputTokens:         3,
		TotalTokens:          10,
		CacheReadInputTokens: 2,
		ReasoningTokens:      1,
		InputSemantics:       TokenInputSemanticsInclusive,
	}}, nil)
	rec.End()

	attrs := spanAttributeMap(onlyGenerationSpan(t, recorder.Ended()))
	for _, key := range []string{
		spanAttrInputTokens,
		spanAttrOutputTokens,
		spanAttrCacheReadTokens,
		spanAttrReasoningTokens,
		attrTokenSemantics,
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("fetch_response span carries %s", key)
		}
	}
	collected := collectMetrics(t, metricReader)
	for _, scope := range collected.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == metricTokenUsage {
				t.Errorf("fetch_response records %s", metricTokenUsage)
			}
		}
	}
}

func TestClientTagsOnEmbeddingAndToolSpans(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{
		Tags: map[string]string{"project": "embed-tools"},
	})

	_, embedRec := client.StartEmbedding(context.Background(), EmbeddingStart{
		Model: ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
	})
	embedRec.SetResult(EmbeddingResult{InputTokens: 1})
	embedRec.End()

	embedSpan := onlyEmbeddingSpan(t, recorder.Ended())
	if got := spanAttributeMap(embedSpan)[clientTagProjectKey].AsString(); got != "embed-tools" {
		t.Fatalf("expected %s on embedding span, got %q", clientTagProjectKey, got)
	}

	_, toolRec := client.StartToolExecution(context.Background(), ToolExecutionStart{
		ToolName: "weather",
	})
	toolRec.SetResult(ToolExecutionEnd{Result: map[string]any{"temp": 72}})
	toolRec.End()

	toolSpan := onlyToolSpan(t, recorder.Ended())
	if got := spanAttributeMap(toolSpan)[clientTagProjectKey].AsString(); got != "embed-tools" {
		t.Fatalf("expected %s on tool span, got %q", clientTagProjectKey, got)
	}
}

func TestClientTagsEmptyIsNoOpOnSpan(t *testing.T) {
	client, recorder, _ := newTestClient(t, Config{})

	_, genRec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	genRec.SetResult(Generation{}, nil)
	genRec.End()

	span := onlyGenerationSpan(t, recorder.Ended())
	attrs := spanAttributeMap(span)
	if _, ok := attrs[clientTagProjectKey]; ok {
		t.Fatalf("did not expect %s when client tags are unset", clientTagProjectKey)
	}
}

func TestPerCallGenerationTagsStayExportOnly(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client, recorder, _ := newTestClient(t, Config{
		testGenerationExporter: exporter,
	})

	_, genRec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
		Tags:  map[string]string{"call_only": "yes"},
	})
	genRec.SetResult(Generation{}, nil)
	genRec.End()

	span := onlyGenerationSpan(t, recorder.Ended())
	if _, ok := spanAttributeMap(span)[spanAttrTagPrefix+"call_only"]; ok {
		t.Fatalf("per-call tag must not appear on span without client-level SIGIL_TAGS")
	}

	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if len(exporter.requests) != 1 || len(exporter.requests[0].Generations) != 1 {
		t.Fatalf("expected one exported generation, got %#v", exporter.requests)
	}
	if got := exporter.requests[0].Generations[0].GetTags()["call_only"]; got != "yes" {
		t.Fatalf("expected call_only=yes on export tags, got %#v", exporter.requests[0].Generations[0].GetTags())
	}
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return collected
}

func findHistogramPointForTags(t *testing.T, histogram metricdata.Histogram[float64], want map[string]string) metricdata.HistogramDataPoint[float64] {
	t.Helper()
	for _, point := range histogram.DataPoints {
		if histogramPointMatchesForTags(point.Attributes, want) {
			return point
		}
	}
	t.Fatalf("expected histogram point with attrs %v", want)
	return metricdata.HistogramDataPoint[float64]{}
}

func histogramPointMatchesForTags(attrs attribute.Set, want map[string]string) bool {
	for key, expected := range want {
		value, ok := (&attrs).Value(attribute.Key(key))
		if !ok || value.AsString() != expected {
			return false
		}
	}
	return true
}

func findHistogram(t *testing.T, collected metricdata.ResourceMetrics, metricName string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != metricName {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q is not a float64 histogram", metricName)
			}
			return histogram
		}
	}
	t.Fatalf("metric %q not found", metricName)
	return metricdata.Histogram[float64]{}
}
