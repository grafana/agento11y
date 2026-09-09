package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/testkit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/trace"
)

func TestAnthropicOTelImmediateErrorPreservesRequestControls(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("provider failed before response")
	_, err := message(context.Background(), env.Client, testRequest(), func(context.Context, asdk.BetaMessageNewParams) (*asdk.BetaMessage, error) {
		return nil, providerErr
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("native error = %v, want provider error", err)
	}

	attrs := testkit.SpanAttributes(testkit.FindSpan(t, env.Spans.Ended(), "chat claude-sonnet-4-5"))
	requireOTelInt64Attr(t, attrs, "gen_ai.request.max_tokens", 512)
	if got := attrs["gen_ai.request.temperature"].AsFloat64(); got != 0.3 {
		t.Fatalf("gen_ai.request.temperature = %v, want 0.3", got)
	}
	if got := attrs["gen_ai.request.top_p"].AsFloat64(); got != 0.8 {
		t.Fatalf("gen_ai.request.top_p = %v, want 0.8", got)
	}
	requireOTelInt64Attr(t, attrs, "gen_ai.request.top_k", 40)
	requireOTelStringAttr(t, attrs, "gen_ai.output.type", "json")
	requireOTelStringAttr(t, attrs, "agento11y.gen_ai.request.tool_choice", `{"name":"weather","type":"tool"}`)
	if !attrs["agento11y.gen_ai.request.thinking.enabled"].AsBool() {
		t.Fatal("thinking was not recorded as enabled")
	}
	requireOTelInt64Attr(t, attrs, "agento11y.gen_ai.request.thinking.budget_tokens", 1024)
}

func TestAnthropicOTelEmptyStreamIsInstrumentationError(t *testing.T) {
	cases := []struct {
		name   string
		events []asdk.BetaRawMessageStreamEventUnion
	}{
		{name: "empty"},
		{name: "ping only", events: []asdk.BetaRawMessageStreamEventUnion{{Type: "ping"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testkit.ClearAmbientEnv()
			env := testkit.NewOTelEnv(t)
			stream := &testBetaMessageEventStream{events: tc.events}

			response, summary, err := messageStream(
				context.Background(),
				env.Client,
				testRequest(),
				func(context.Context, asdk.BetaMessageNewParams) betaMessageEventStream { return stream },
			)
			if err != nil || response != nil {
				t.Fatalf("native return = (%+v, %v), want (nil, nil)", response, err)
			}
			if len(summary.Events) != len(tc.events) {
				t.Fatalf("summary events = %d, want %d", len(summary.Events), len(tc.events))
			}
			if !stream.closed {
				t.Fatal("stream was not closed")
			}

			span := testkit.FindSpan(t, env.Spans.Ended(), "chat claude-sonnet-4-5")
			if span.Status().Code != codes.Error {
				t.Fatalf("span status = %v, want error", span.Status().Code)
			}
			attrs := testkit.SpanAttributes(span)
			requireOTelStringAttr(t, attrs, "error.type", "mapping_error")
		})
	}
}

func TestAnthropicProviderOTelStream(t *testing.T) {
	testkit.ClearAmbientEnv()
	env := testkit.NewOTelEnv(t)
	req := testRequest()
	summary := StreamSummary{
		Events:       hostedToolStreamEvents(t),
		FirstChunkAt: time.Now().Add(-time.Millisecond),
	}
	generation, err := FromStream(req, summary, WithAgentName("anthropic-agent"))
	if err != nil {
		t.Fatalf("map Anthropic stream: %v", err)
	}
	testkit.RecordStreamingGeneration(t, env, agento11y.GenerationStart{
		AgentName: "anthropic-agent",
		Model:     agento11y.ModelRef{Provider: "anthropic", Name: req.Model},
	}, summary.FirstChunkAt, generation, nil)
	if err := env.Client.Flush(context.Background()); err != nil {
		t.Fatalf("flush OTel generation: %v", err)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat claude-sonnet-4-5")
	if span.SpanKind() != trace.SpanKindClient {
		t.Fatalf("span kind = %v, want client", span.SpanKind())
	}
	attrs := testkit.SpanAttributes(span)
	requireOTelStringAttr(t, attrs, "gen_ai.operation.name", "chat")
	requireOTelStringAttr(t, attrs, "gen_ai.provider.name", "anthropic")
	requireOTelStringAttr(t, attrs, "gen_ai.request.model", "claude-sonnet-4-5")
	requireOTelStringAttr(t, attrs, "gen_ai.response.model", "claude-sonnet-4-5")
	requireOTelStringAttr(t, attrs, "gen_ai.response.id", "msg_ticket_stream")
	requireOTelInt64Attr(t, attrs, "gen_ai.request.top_k", 40)
	requireOTelStringAttr(t, attrs, "gen_ai.output.type", "json")
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 1220)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 40)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.cache_read.input_tokens", 1000)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.cache_creation.input_tokens", 200)
	if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 1 || got[0] != "end_turn" {
		t.Fatalf("finish reasons = %v, want [end_turn]", got)
	}

	var output []struct {
		Role         string `json:"role"`
		FinishReason string `json:"finish_reason"`
		Parts        []struct {
			Type       string         `json:"type"`
			Content    string         `json:"content"`
			Extensions map[string]any `json:"extensions"`
		} `json:"parts"`
	}
	if err := json.Unmarshal([]byte(attrs["gen_ai.output.messages"].AsString()), &output); err != nil {
		t.Fatalf("decode gen_ai.output.messages: %v", err)
	}
	if len(output) != 1 || len(output[0].Parts) != 3 {
		t.Fatalf("unexpected hosted-tool output shape: %+v", output)
	}
	if output[0].Parts[0].Type != "tool_call" || output[0].Parts[1].Type != "tool_call_response" ||
		output[0].Parts[2].Type != "text" || output[0].Parts[2].Content != "It is sunny." {
		t.Fatalf("hosted-tool output order was not preserved: %+v", output)
	}
	if output[0].FinishReason != "end_turn" {
		t.Fatalf("candidate finish reason = %q, want end_turn", output[0].FinishReason)
	}

	var metrics metricdata.ResourceMetrics
	if err := env.Metrics.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("collect OTel metrics: %v", err)
	}
	duration := findAnthropicHistogram[float64](t, metrics, "gen_ai.client.operation.duration")
	durationPoint := findAnthropicMetricPoint(t, duration, map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "anthropic",
		"gen_ai.request.model":  "claude-sonnet-4-5",
	})
	if durationPoint.Count != 1 {
		t.Fatalf("operation duration count = %d, want 1", durationPoint.Count)
	}

	usage := findAnthropicHistogram[int64](t, metrics, "gen_ai.client.token.usage")
	for tokenType, want := range map[string]int64{
		"input":          1220,
		"output":         40,
		"cache_read":     1000,
		"cache_creation": 200,
	} {
		point := findAnthropicMetricPoint(t, usage, map[string]string{
			"gen_ai.operation.name": "chat",
			"gen_ai.provider.name":  "anthropic",
			"gen_ai.request.model":  "claude-sonnet-4-5",
			"gen_ai.token.type":     tokenType,
		})
		if point.Count != 1 || point.Sum != want {
			t.Fatalf("token metric %q = count %d sum %d, want count 1 sum %d", tokenType, point.Count, point.Sum, want)
		}
	}
}

func TestAnthropicResponseWithErrorKeepsObservedDataAndNativeReturn(t *testing.T) {
	testkit.ClearAmbientEnv()
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("provider failed after response")
	native := &asdk.BetaMessage{
		ID:         "msg_partial_error",
		Model:      asdk.Model("claude-sonnet-4-5"),
		StopReason: asdk.BetaStopReasonMaxTokens,
		Content:    []asdk.BetaContentBlockUnion{{Type: "text", Text: "partial answer"}},
		Usage:      asdk.BetaUsage{InputTokens: 5, OutputTokens: 2},
	}

	got, err := message(context.Background(), env.Client, testRequest(), func(context.Context, asdk.BetaMessageNewParams) (*asdk.BetaMessage, error) {
		return native, providerErr
	})
	if got != native || !errors.Is(err, providerErr) {
		t.Fatalf("native return = (%p, %v), want (%p, provider error)", got, err, native)
	}
	if err := env.Client.Flush(context.Background()); err != nil {
		t.Fatalf("flush OTel generation: %v", err)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat claude-sonnet-4-5")
	attrs := testkit.SpanAttributes(span)
	requireOTelStringAttr(t, attrs, "gen_ai.response.id", "msg_partial_error")
	requireOTelInt64Attr(t, attrs, "gen_ai.request.top_k", 40)
	requireOTelStringAttr(t, attrs, "gen_ai.output.type", "json")
	if output := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(output, "partial answer") {
		t.Fatalf("gen_ai.output.messages = %q, want partial answer", output)
	}
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 5)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 2)
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", span.Status().Code)
	}
}

func TestAnthropicProviderOTelMapsPartialStreamBeforeError(t *testing.T) {
	testkit.ClearAmbientEnv()
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("stream interrupted")
	stream := &testBetaMessageEventStream{
		events: hostedToolStreamEvents(t)[:8],
		err:    providerErr,
	}

	response, summary, err := messageStream(
		context.Background(),
		env.Client,
		testRequest(),
		func(context.Context, asdk.BetaMessageNewParams) betaMessageEventStream { return stream },
	)
	if !errors.Is(err, providerErr) {
		t.Fatalf("provider error = %v, want original error", err)
	}
	if response != nil {
		t.Fatalf("provider error returned response %+v", response)
	}
	if summary.FinalMessage == nil {
		t.Fatal("partial native accumulator was not retained")
	}
	if err := env.Client.Flush(context.Background()); err != nil {
		t.Fatalf("flush partial OTel generation: %v", err)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat claude-sonnet-4-5")
	if span.Status().Code != codes.Error {
		t.Fatalf("partial stream span status = %v, want error", span.Status().Code)
	}
	attrs := testkit.SpanAttributes(span)
	requireOTelStringAttr(t, attrs, "gen_ai.response.id", "msg_ticket_stream")
	requireOTelStringAttr(t, attrs, "gen_ai.response.model", "claude-sonnet-4-5")
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 1220)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 0)
	if got := attrs["gen_ai.output.messages"].AsString(); got == "" {
		t.Fatal("partial stream span lost accumulated output messages")
	}
}

func requireOTelStringAttr(t testing.TB, attrs map[string]attribute.Value, key, want string) {
	t.Helper()
	value, ok := attrs[key]
	if !ok || value.AsString() != want {
		t.Fatalf("span attribute %s = %q, present %v; want %q", key, value.AsString(), ok, want)
	}
}

func requireOTelInt64Attr(t testing.TB, attrs map[string]attribute.Value, key string, want int64) {
	t.Helper()
	value, ok := attrs[key]
	if !ok || value.AsInt64() != want {
		t.Fatalf("span attribute %s = %d, present %v; want %d", key, value.AsInt64(), ok, want)
	}
}

func findAnthropicHistogram[N int64 | float64](t testing.TB, metrics metricdata.ResourceMetrics, name string) metricdata.Histogram[N] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[N])
			if !ok {
				t.Fatalf("metric %s has data %T, want histogram", name, metric.Data)
			}
			return histogram
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Histogram[N]{}
}

func findAnthropicMetricPoint[N int64 | float64](t testing.TB, histogram metricdata.Histogram[N], want map[string]string) metricdata.HistogramDataPoint[N] {
	t.Helper()
	for _, point := range histogram.DataPoints {
		matches := true
		for key, expected := range want {
			value, ok := point.Attributes.Value(attribute.Key(key))
			if !ok || value.AsString() != expected {
				matches = false
				break
			}
		}
		if matches {
			return point
		}
	}
	t.Fatalf("metric point with attributes %v not found", want)
	return metricdata.HistogramDataPoint[N]{}
}
