package otelgenai_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/grafana/agento11y/go/otelgenai"
)

// newMetricRecorder returns a handler option for an in-memory reader and a
// function that collects this package's metrics by instrument name.
func newMetricRecorder(t *testing.T) (otelgenai.Option, func() map[string]metricdata.Metrics) {
	t.Helper()

	reader := metric.NewManualReader()
	provider := metric.NewMeterProvider(metric.WithReader(reader))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	collect := func() map[string]metricdata.Metrics {
		var collected metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &collected); err != nil {
			t.Fatalf("collect: %v", err)
		}
		out := map[string]metricdata.Metrics{}
		for _, scope := range collected.ScopeMetrics {
			if scope.Scope.Name != otelgenai.ScopeName {
				continue
			}
			for _, m := range scope.Metrics {
				out[m.Name] = m
			}
		}
		return out
	}
	return otelgenai.WithMeterProvider(provider), collect
}

// collectMetricsWithDriver runs one invocation through a handler wired to an
// in-memory reader and returns its metrics, keyed by instrument name.
func collectMetricsWithDriver(
	t *testing.T,
	inv *otelgenai.Invocation,
	drive func(context.Context, *otelgenai.Handler, *otelgenai.Invocation),
	opts ...otelgenai.Option,
) map[string]metricdata.Metrics {
	t.Helper()

	meterOption, collect := newMetricRecorder(t)
	opts = append(opts, meterOption)
	handler := otelgenai.NewHandler(opts...)
	ctx := handler.Start(context.Background(), inv)
	if drive != nil {
		drive(ctx, handler, inv)
	}
	handler.End(ctx, inv)
	return collect()
}

// firstPointAttribute returns one attribute of a histogram's first data point,
// whichever numeric type the histogram carries.
func firstPointAttribute(t *testing.T, m metricdata.Metrics, key string) string {
	t.Helper()

	var attrs attribute.Set
	switch data := m.Data.(type) {
	case metricdata.Histogram[float64]:
		attrs = data.DataPoints[0].Attributes
	case metricdata.Histogram[int64]:
		attrs = data.DataPoints[0].Attributes
	default:
		t.Fatalf("%s data = %T, want a histogram", m.Name, m.Data)
	}
	value, _ := attrs.Value(attribute.Key(key))
	return value.AsString()
}

func histogramCount(t *testing.T, m metricdata.Metrics) uint64 {
	t.Helper()

	histogram, ok := m.Data.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("%s data = %T, want a float64 histogram", m.Name, m.Data)
	}
	var count uint64
	for _, point := range histogram.DataPoints {
		count += point.Count
	}
	return count
}

func tokenTypeSums(t *testing.T, m metricdata.Metrics) map[string]int64 {
	t.Helper()

	histogram, ok := m.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("%s data = %T, want an int64 histogram", m.Name, m.Data)
	}
	out := map[string]int64{}
	for _, point := range histogram.DataPoints {
		tokenType, _ := point.Attributes.Value("gen_ai.token.type")
		out[tokenType.AsString()] = point.Sum
	}
	return out
}

func TestSpecMetrics(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		options        []otelgenai.Option
		extendedTokens bool
		mutate         func(inv *otelgenai.Invocation)
		drive          func(context.Context, *otelgenai.Handler, *otelgenai.Invocation)
		check          func(t *testing.T, metrics map[string]metricdata.Metrics)
	}{
		{
			name: "duration and token usage",
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				duration, ok := metrics["gen_ai.client.operation.duration"]
				if !ok {
					t.Fatal("gen_ai.client.operation.duration not recorded")
				}
				histogram, ok := duration.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data = %T, want a float64 histogram", duration.Data)
				}
				if got := histogram.DataPoints[0].Sum; got != 1.25 {
					t.Errorf("duration sum = %v, want 1.25", got)
				}
				operation, _ := histogram.DataPoints[0].Attributes.Value("gen_ai.operation.name")
				if got := operation.AsString(); got != "chat" {
					t.Errorf("duration gen_ai.operation.name = %q, want chat", got)
				}

				usage, ok := metrics["gen_ai.client.token.usage"]
				if !ok {
					t.Fatal("gen_ai.client.token.usage not recorded")
				}
				sums := tokenTypeSums(t, usage)
				if sums["input"] != 120 || sums["output"] != 42 {
					t.Errorf("token sums = %v, want input 120 and output 42", sums)
				}
			},
		},
		{
			name:           "cache and reasoning token types",
			extendedTokens: true,
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage.CacheReadInputTokens = 12
				inv.Usage.CacheWriteInputTokens = 7
				inv.Usage.ReasoningTokens = 5
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				sums := tokenTypeSums(t, metrics["gen_ai.client.token.usage"])
				if sums["cache_read"] != 12 || sums["cache_write"] != 7 || sums["reasoning"] != 5 {
					t.Errorf("token sums = %v, want cache_read 12, cache_write 7, reasoning 5", sums)
				}
			},
		},
		{
			name: "default handler records only spec token types and dimensions",
			mutate: func(inv *otelgenai.Invocation) {
				inv.AgentName = "assistant"
				inv.AgentVersion = "1.0.0"
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250_000_000)
				inv.Usage.CacheReadInputTokens = 12
				inv.Usage.CacheWriteInputTokens = 7
				inv.Usage.ReasoningTokens = 5
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				sums := tokenTypeSums(t, metrics["gen_ai.client.token.usage"])
				if len(sums) != 2 || sums["input"] != 120 || sums["output"] != 42 {
					t.Errorf("token sums = %v, want only input 120 and output 42", sums)
				}
				for _, name := range []string{
					"gen_ai.client.operation.duration",
					"gen_ai.client.token.usage",
					"gen_ai.client.operation.time_to_first_chunk",
				} {
					m := metrics[name]
					for _, key := range []string{"gen_ai.agent.name", "gen_ai.agent.version"} {
						if got := firstPointAttribute(t, m, key); got != "" {
							t.Errorf("%s %s = %q, want no agent identity dimension", name, key, got)
						}
					}
				}
			},
		},
		{
			name: "streaming records time to first chunk",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250_000_000)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				ttfc, ok := metrics["gen_ai.client.operation.time_to_first_chunk"]
				if !ok {
					t.Fatal("gen_ai.client.operation.time_to_first_chunk not recorded")
				}
				histogram, ok := ttfc.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("ttfc data = %T, want a float64 histogram", ttfc.Data)
				}
				if got := histogram.DataPoints[0].Sum; got != 0.25 {
					t.Errorf("time to first chunk = %v, want 0.25", got)
				}
				// The spec instrument replaces the SDK's pre-spec
				// gen_ai.client.time_to_first_token, which must not be recorded
				// alongside it.
				if _, ok := metrics["gen_ai.client.time_to_first_token"]; ok {
					t.Error("gen_ai.client.time_to_first_token recorded in otel mode")
				}
			},
		},
		{
			name: "the first live chunk records ttfc once",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.StartedAt = time.Now().UTC().Add(-time.Second)
				inv.CompletedAt = time.Time{}
			},
			drive: func(ctx context.Context, handler *otelgenai.Handler, inv *otelgenai.Invocation) {
				handler.RecordChunk(ctx, inv)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				ttfc, ok := metrics["gen_ai.client.operation.time_to_first_chunk"]
				if !ok {
					t.Fatal("gen_ai.client.operation.time_to_first_chunk not recorded")
				}
				if got := histogramCount(t, ttfc); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1 after End", got)
				}
				if _, ok := metrics["gen_ai.client.operation.time_per_output_chunk"]; ok {
					t.Error("the first chunk recorded time_per_output_chunk")
				}
			},
		},
		{
			name: "a hook rebuild keeps duration and first-chunk samples",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{
						Provider:      inv.Provider,
						RequestModel:  inv.RequestModel,
						ResponseModel: inv.ResponseModel,
						Stream:        inv.Stream,
						Usage:         inv.Usage,
					}
					return nil
				}),
			)},
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if got := histogramCount(t, metrics["gen_ai.client.operation.duration"]); got != 1 {
					t.Errorf("operation.duration count = %d, want 1", got)
				}
				if got := histogramCount(t, metrics["gen_ai.client.operation.time_to_first_chunk"]); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1", got)
				}
			},
		},
		{
			// Stream gates the time-to-first-chunk fallback, and the rebuilt
			// invocation drops it.
			name: "a hook rebuild that drops the stream flag keeps the first-chunk sample",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{
						Provider:     inv.Provider,
						RequestModel: inv.RequestModel,
						Usage:        inv.Usage,
					}
					return nil
				}),
			)},
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if got := histogramCount(t, metrics["gen_ai.client.operation.time_to_first_chunk"]); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1", got)
				}
			},
		},
		{
			name: "a hook rebuild does not duplicate the live first-chunk metric",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{
						Provider:         inv.Provider,
						RequestModel:     inv.RequestModel,
						ResponseModel:    inv.ResponseModel,
						Stream:           inv.Stream,
						Usage:            inv.Usage,
						StartedAt:        inv.StartedAt,
						CompletedAt:      inv.CompletedAt,
						FirstChunkAt:     inv.FirstChunkAt,
						MetricAttributes: inv.MetricAttributes,
					}
					return nil
				}),
			)},
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.StartedAt = time.Now().UTC().Add(-time.Second)
				inv.CompletedAt = time.Time{}
			},
			drive: func(ctx context.Context, handler *otelgenai.Handler, inv *otelgenai.Invocation) {
				handler.RecordChunk(ctx, inv)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if got := histogramCount(t, metrics["gen_ai.client.operation.time_to_first_chunk"]); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1", got)
				}
			},
		},
		{
			name: "an existing first chunk timestamp sets the live metric",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.StartedAt = time.Now().UTC().Add(-time.Second)
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
				inv.CompletedAt = time.Time{}
			},
			drive: func(ctx context.Context, handler *otelgenai.Handler, inv *otelgenai.Invocation) {
				handler.RecordChunk(ctx, inv)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				ttfc, ok := metrics["gen_ai.client.operation.time_to_first_chunk"]
				if !ok {
					t.Fatal("gen_ai.client.operation.time_to_first_chunk not recorded")
				}
				histogram, ok := ttfc.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("ttfc data = %T, want a float64 histogram", ttfc.Data)
				}
				if got := histogram.DataPoints[0].Sum; got != 0.25 {
					t.Errorf("time to first chunk = %v, want 0.25 from FirstChunkAt", got)
				}
			},
		},
		{
			name: "RecordChunk marks the invocation as streaming",
			mutate: func(inv *otelgenai.Invocation) {
				inv.StartedAt = time.Now().UTC().Add(-time.Second)
				inv.CompletedAt = time.Time{}
			},
			drive: func(ctx context.Context, handler *otelgenai.Handler, inv *otelgenai.Invocation) {
				handler.RecordChunk(ctx, inv)
				if !inv.Stream {
					t.Error("RecordChunk left Stream false")
				}
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if got := histogramCount(t, metrics["gen_ai.client.operation.time_to_first_chunk"]); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1", got)
				}
			},
		},
		{
			name: "the second live chunk records one inter-chunk gap",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.StartedAt = time.Now().UTC().Add(-time.Second)
				inv.CompletedAt = time.Time{}
			},
			drive: func(ctx context.Context, handler *otelgenai.Handler, inv *otelgenai.Invocation) {
				handler.RecordChunk(ctx, inv)
				handler.RecordChunk(ctx, inv)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if got := histogramCount(t, metrics["gen_ai.client.operation.time_to_first_chunk"]); got != 1 {
					t.Errorf("time_to_first_chunk count = %d, want 1", got)
				}
				perChunk, ok := metrics["gen_ai.client.operation.time_per_output_chunk"]
				if !ok {
					t.Fatal("gen_ai.client.operation.time_per_output_chunk not recorded")
				}
				if got := histogramCount(t, perChunk); got != 1 {
					t.Errorf("time_per_output_chunk count = %d, want 1", got)
				}
			},
		},
		{
			name: "non-streaming records no first chunk",
			mutate: func(inv *otelgenai.Invocation) {
				inv.FirstChunkAt = inv.StartedAt.Add(250_000_000)
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if _, ok := metrics["gen_ai.client.operation.time_to_first_chunk"]; ok {
					t.Error("time_to_first_chunk recorded for a non-streaming invocation")
				}
			},
		},
		{
			name: "tool calls per operation is not emitted",
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if _, ok := metrics["gen_ai.client.tool_calls_per_operation"]; ok {
					t.Error("the dropped tool-call instrument is still emitted")
				}
			},
		},
		{
			name: "usage a provider never returned records no tokens",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{}
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if _, ok := metrics["gen_ai.client.token.usage"]; ok {
					t.Error("token usage recorded for a provider that reported none")
				}
				if _, ok := metrics["gen_ai.client.operation.duration"]; !ok {
					t.Error("duration is gated on usage being reported")
				}
			},
		},
		{
			name: "counts without the reported flag are still recorded",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{InputTokens: 120, OutputTokens: 42}
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				sums := tokenTypeSums(t, metrics["gen_ai.client.token.usage"])
				if sums["input"] != 120 || sums["output"] != 42 {
					t.Errorf("token sums = %v, want input 120 and output 42", sums)
				}
			},
		},
		{
			name: "an all-zero usage the provider did return is recorded as reported",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{Reported: true}
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				// Every count is zero, so no bucket lands on the instrument,
				// but the invocation still counts as having usage.
				if _, ok := metrics["gen_ai.client.token.usage"]; ok {
					t.Error("a zero-valued usage recorded a token bucket")
				}
			},
		},
		{
			name: "empty provider is omitted from duration",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationExecuteTool
				inv.Provider = ""
				inv.ToolName = "weather"
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				duration := metrics["gen_ai.client.operation.duration"]
				histogram, ok := duration.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data = %T, want a float64 histogram", duration.Data)
				}
				if _, present := histogram.DataPoints[0].Attributes.Value("gen_ai.provider.name"); present {
					t.Error("duration carries an empty gen_ai.provider.name")
				}
			},
		},
		{
			name: "token usage carries error type",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ErrorType = "timeout"
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				usage := metrics["gen_ai.client.token.usage"]
				histogram, ok := usage.Data.(metricdata.Histogram[int64])
				if !ok {
					t.Fatalf("token usage data = %T, want an int64 histogram", usage.Data)
				}
				for _, point := range histogram.DataPoints {
					errorType, _ := point.Attributes.Value("error.type")
					if got := errorType.AsString(); got != "timeout" {
						t.Errorf("token usage error.type = %q, want timeout", got)
					}
				}
			},
		},
		{
			name: "workflow records no client metrics",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationInvokeWorkflow
				inv.WorkflowName = "nightly"
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				if len(metrics) != 0 {
					t.Errorf("workflow metrics = %v, want none", metrics)
				}
			},
		},
		{
			name: "response model and server address reach every instrument",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ResponseModel = "gpt-5-2026-01-01"
				inv.ServerAddress = "api.openai.com"
				inv.ServerPort = 443
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				duration, ok := metrics["gen_ai.client.operation.duration"]
				if !ok {
					t.Fatal("gen_ai.client.operation.duration not recorded")
				}
				histogram, ok := duration.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data = %T, want a float64 histogram", duration.Data)
				}
				attrs := histogram.DataPoints[0].Attributes
				model, _ := attrs.Value("gen_ai.response.model")
				if got := model.AsString(); got != "gpt-5-2026-01-01" {
					t.Errorf("gen_ai.response.model = %q, want gpt-5-2026-01-01", got)
				}
				address, _ := attrs.Value("server.address")
				if got := address.AsString(); got != "api.openai.com" {
					t.Errorf("server.address = %q, want api.openai.com", got)
				}
				port, _ := attrs.Value("server.port")
				if got := port.AsInt64(); got != 443 {
					t.Errorf("server.port = %d, want 443", got)
				}
			},
		},
		{
			name: "an error with no type classifies as _OTHER",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ErrorMessage = "provider returned 500"
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				duration, ok := metrics["gen_ai.client.operation.duration"]
				if !ok {
					t.Fatal("gen_ai.client.operation.duration not recorded")
				}
				histogram, ok := duration.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("duration data = %T, want a float64 histogram", duration.Data)
				}
				errorType, _ := histogram.DataPoints[0].Attributes.Value("error.type")
				if got := errorType.AsString(); got != "_OTHER" {
					t.Errorf("error.type = %q, want _OTHER", got)
				}
			},
		},
		{
			name: "caller metric attributes reach every instrument",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250_000_000)
				inv.MetricAttributes = []attribute.KeyValue{attribute.String("vendor.tag.repo", "agento11y")}
			},
			check: func(t *testing.T, metrics map[string]metricdata.Metrics) {
				for _, name := range []string{
					"gen_ai.client.operation.duration",
					"gen_ai.client.token.usage",
					"gen_ai.client.operation.time_to_first_chunk",
				} {
					m, ok := metrics[name]
					if !ok {
						t.Fatalf("%s not recorded", name)
					}
					if got := firstPointAttribute(t, m, "vendor.tag.repo"); got != "agento11y" {
						t.Errorf("%s vendor.tag.repo = %q, want agento11y", name, got)
					}
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			inv := chatInvocation()
			if tc.mutate != nil {
				tc.mutate(inv)
			}
			opts := append([]otelgenai.Option(nil), tc.options...)
			if tc.extendedTokens {
				opts = append(opts, otelgenai.WithExtendedTokenTypes())
			}
			tc.check(t, collectMetricsWithDriver(t, inv, tc.drive, opts...))
		})
	}
}
