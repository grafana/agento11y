package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	osdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	responses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/grafana/agento11y/go/agento11y/testkit"
)

func TestOTelChatWrapperMapsProviderResultToSpanAndMetrics(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		N:     param.NewOpt(int64(2)),
		Seed:  param.NewOpt(int64(23)),
		ResponseFormat: osdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
	}
	native := &osdk.ChatCompletion{
		ID:    "chat_otel",
		Model: "gpt-4o-mini-2026-08-01",
		Choices: []osdk.ChatCompletionChoice{
			{Index: 0, FinishReason: "stop", Message: osdk.ChatCompletionMessage{Content: "one"}},
			{Index: 1, FinishReason: "length", Message: osdk.ChatCompletionMessage{Content: "two"}},
		},
		Usage: osdk.CompletionUsage{PromptTokens: 9, CompletionTokens: 4, TotalTokens: 13},
	}

	got, err := chatCompletionsNew(context.Background(), env.Client, req, func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
		return native, nil
	})
	if err != nil || got != native {
		t.Fatalf("native return = (%p, %v), want (%p, nil)", got, err, native)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-4o-mini")
	attrs := testkit.SpanAttributes(span)
	requireOTelInt64Attr(t, attrs, "gen_ai.request.choice.count", 2)
	requireOTelInt64Attr(t, attrs, "gen_ai.request.seed", 23)
	requireOTelStringAttr(t, attrs, "gen_ai.output.type", "json")
	requireOTelStringAttr(t, attrs, "gen_ai.response.id", "chat_otel")
	if got := attrs["gen_ai.response.finish_reasons"].AsStringSlice(); len(got) != 2 || got[0] != "stop" || got[1] != "length" {
		t.Fatalf("gen_ai.response.finish_reasons = %v, want [stop length]", got)
	}
	var output []struct {
		FinishReason string `json:"finish_reason"`
	}
	if err := json.Unmarshal([]byte(attrs["gen_ai.output.messages"].AsString()), &output); err != nil {
		t.Fatalf("decode gen_ai.output.messages: %v", err)
	}
	if len(output) != 2 || output[0].FinishReason != "stop" || output[1].FinishReason != "length" {
		t.Fatalf("output candidate finish reasons = %#v", output)
	}

	metrics := collectOTelMetrics(t, env)
	duration := findOTelHistogram[float64](t, metrics, "gen_ai.client.operation.duration")
	durationPoint := findOTelPoint(t, duration.DataPoints, map[string]string{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4o-mini",
	})
	if durationPoint.Count != 1 {
		t.Fatalf("duration count = %d, want 1", durationPoint.Count)
	}
	usage := findOTelHistogram[int64](t, metrics, "gen_ai.client.token.usage")
	for tokenType, want := range map[string]int64{"input": 9, "output": 4} {
		point := findOTelPoint(t, usage.DataPoints, map[string]string{"gen_ai.token.type": tokenType})
		if point.Count != 1 || point.Sum != want {
			t.Fatalf("%s token metric = count %d sum %d, want count 1 sum %d", tokenType, point.Count, point.Sum, want)
		}
	}
}

func TestOTelChatImmediateErrorPreservesRequestControls(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	req := osdk.ChatCompletionNewParams{
		Model:               shared.ChatModel("gpt-5"),
		MaxCompletionTokens: param.NewOpt(int64(256)),
		Temperature:         param.NewOpt(0.4),
		N:                   param.NewOpt(int64(2)),
		Seed:                param.NewOpt(int64(23)),
		ReasoningEffort:     shared.ReasoningEffortNone,
		ResponseFormat: osdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
	}
	providerErr := errors.New("provider failed before response")
	_, err := chatCompletionsNew(context.Background(), env.Client, req, func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
		return nil, providerErr
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("native error = %v, want provider error", err)
	}

	attrs := testkit.SpanAttributes(testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-5"))
	requireOTelInt64Attr(t, attrs, "gen_ai.request.max_tokens", 256)
	requireOTelInt64Attr(t, attrs, "gen_ai.request.choice.count", 2)
	requireOTelInt64Attr(t, attrs, "gen_ai.request.seed", 23)
	requireOTelStringAttr(t, attrs, "gen_ai.output.type", "json")
	if got := attrs["agento11y.gen_ai.request.thinking.enabled"].AsBool(); got {
		t.Fatal("disabled reasoning was recorded as enabled")
	}
}

func TestOTelNilResponseWithNilErrorIsRecordedAsMappingError(t *testing.T) {
	for _, tc := range []struct {
		name  string
		model string
	}{
		{name: "chat", model: "gpt-4o-mini"},
		{name: "responses", model: "gpt-5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := testkit.NewOTelEnv(t)
			switch tc.name {
			case "chat":
				response, err := chatCompletionsNew(
					context.Background(),
					env.Client,
					osdk.ChatCompletionNewParams{Model: tc.model},
					func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) { return nil, nil },
				)
				if response != nil || err != nil {
					t.Fatalf("native return = (%#v, %v), want (nil, nil)", response, err)
				}
			case "responses":
				response, err := responsesNew(
					context.Background(),
					env.Client,
					responses.ResponseNewParams{Model: tc.model},
					func(context.Context, responses.ResponseNewParams) (*responses.Response, error) { return nil, nil },
				)
				if response != nil || err != nil {
					t.Fatalf("native return = (%#v, %v), want (nil, nil)", response, err)
				}
			}

			span := testkit.FindSpan(t, env.Spans.Ended(), "chat "+tc.model)
			if span.Status().Code != codes.Error || !strings.Contains(span.Status().Description, "response is required") {
				t.Fatalf("span status = (%v, %q), want mapper validation error", span.Status().Code, span.Status().Description)
			}
			attrs := testkit.SpanAttributes(span)
			if got := attrs["error.type"].AsString(); got == "" {
				t.Fatal("nil provider response span has no error.type")
			}
		})
	}
}

func TestOTelChatResponseWithErrorKeepsObservedDataAndNativeReturn(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("provider failed after response")
	req := osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")}
	native := &osdk.ChatCompletion{
		ID:    "chat_partial_error",
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{{
			FinishReason: "length",
			Message:      osdk.ChatCompletionMessage{Content: "partial answer"},
		}},
		Usage: osdk.CompletionUsage{PromptTokens: 6, CompletionTokens: 2, TotalTokens: 8},
	}

	got, err := chatCompletionsNew(context.Background(), env.Client, req, func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
		return native, providerErr
	})
	if got != native || !errors.Is(err, providerErr) {
		t.Fatalf("native return = (%p, %v), want (%p, provider error)", got, err, native)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-4o-mini")
	attrs := testkit.SpanAttributes(span)
	requireOTelStringAttr(t, attrs, "gen_ai.response.id", "chat_partial_error")
	if output := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(output, "partial answer") {
		t.Fatalf("gen_ai.output.messages = %q, want partial answer", output)
	}
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 6)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 2)
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", span.Status().Code)
	}
}

func TestOTelResponsesFailedStatusMarksCallErrorWithoutChangingNativeReturn(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	req := responses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}
	native := &responses.Response{
		ID:     "resp_failed",
		Model:  shared.ResponsesModel("gpt-5"),
		Status: responses.ResponseStatusFailed,
		Error: responses.ResponseError{
			Code:    responses.ResponseErrorCodeServerError,
			Message: "model backend failed",
		},
	}

	got, err := responsesNew(context.Background(), env.Client, req, func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
		return native, nil
	})
	if err != nil || got != native {
		t.Fatalf("native return = (%p, %v), want (%p, nil)", got, err, native)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-5")
	if span.Status().Code != codes.Error || !strings.Contains(span.Status().Description, "model backend failed") {
		t.Fatalf("span status = (%v, %q), want failed provider response", span.Status().Code, span.Status().Description)
	}
	attrs := testkit.SpanAttributes(span)
	requireOTelStringAttr(t, attrs, "gen_ai.response.status", "failed")
	if got := attrs["error.type"].AsString(); got == "" {
		t.Fatal("failed response span has no error.type")
	}
	if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
		t.Fatal("usage-less failed response has gen_ai.usage.input_tokens")
	}
	if _, ok := attrs["gen_ai.usage.output_tokens"]; ok {
		t.Fatal("usage-less failed response has gen_ai.usage.output_tokens")
	}

	metrics := collectOTelMetrics(t, env)
	duration := findOTelHistogram[float64](t, metrics, "gen_ai.client.operation.duration")
	point := findOTelPoint(t, duration.DataPoints, map[string]string{
		"gen_ai.provider.name": "openai",
		"gen_ai.request.model": "gpt-5",
	})
	if value, ok := point.Attributes.Value("error.type"); !ok || value.AsString() == "" {
		t.Fatal("failed response duration metric has no error.type")
	}
	requireNoOTelTokenPoints(t, metrics)
}

func TestOTelResponsesFailedStreamMarksCallErrorWithoutChangingNativeReturn(t *testing.T) {
	cases := []struct {
		name           string
		prelude        string
		event          string
		message        string
		wantResponseID string
		wantOutput     string
	}{
		{
			name:           "response failed after partial delta",
			prelude:        `{"type":"response.output_text.delta","sequence_number":1,"item_id":"message_1","output_index":0,"content_index":0,"delta":"partial before failure"}`,
			event:          `{"type":"response.failed","sequence_number":2,"response":{"id":"resp_failed_stream","model":"gpt-5","status":"failed","error":{"code":"server_error","message":"stream response failed"},"output":[]}}`,
			message:        "stream response failed",
			wantResponseID: "resp_failed_stream",
			wantOutput:     "partial before failure",
		},
		{
			name:    "error event",
			event:   `{"type":"error","sequence_number":1,"code":"server_error","message":"stream event failed","param":""}`,
			message: "stream event failed",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.prelude != "" {
					_, _ = fmt.Fprintf(w, "data: %s\n\n", tc.prelude)
				}
				_, _ = fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", tc.event)
			}))
			defer server.Close()

			env := testkit.NewOTelEnv(t)
			provider := osdk.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"))
			req := responses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}

			response, summary, err := ResponsesNewStreaming(context.Background(), env.Client, provider, req)
			if err != nil {
				t.Fatalf("native stream error = %v, want nil", err)
			}
			if tc.wantResponseID == "" {
				if response != nil || summary.FinalResponse != nil {
					t.Fatalf("native response = %#v, summary final = %#v; want nil", response, summary.FinalResponse)
				}
			} else if response == nil || response != summary.FinalResponse || response.ID != tc.wantResponseID {
				t.Fatalf("native failed response = %#v, summary final = %#v", response, summary.FinalResponse)
			}

			span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-5")
			if span.Status().Code != codes.Error || !strings.Contains(span.Status().Description, tc.message) {
				t.Fatalf("span status = (%v, %q), want failed provider response", span.Status().Code, span.Status().Description)
			}
			attrs := testkit.SpanAttributes(span)
			if tc.wantResponseID != "" {
				requireOTelStringAttr(t, attrs, "gen_ai.response.id", tc.wantResponseID)
			}
			if tc.wantOutput != "" {
				if got := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(got, tc.wantOutput) {
					t.Fatalf("gen_ai.output.messages = %q, want observed partial output %q", got, tc.wantOutput)
				}
			}
			requireOTelStringAttr(t, attrs, "gen_ai.response.status", "failed")
			if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
				t.Fatal("usage-less failed stream has gen_ai.usage.input_tokens")
			}
			if _, ok := attrs["gen_ai.usage.output_tokens"]; ok {
				t.Fatal("usage-less failed stream has gen_ai.usage.output_tokens")
			}
			if got := attrs["error.type"].AsString(); got == "" {
				t.Fatal("failed response span has no error.type")
			}
		})
	}
}

func TestOTelChatStreamingMapsPartialResultBeforeLaterError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat_partial\",\"model\":\"gpt-4o-mini\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial answer\"}}],\"usage\":{\"prompt_tokens\":6,\"completion_tokens\":2,\"total_tokens\":8}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"error\":\"stream exploded\"}\n\n")
	}))
	defer server.Close()

	env := testkit.NewOTelEnv(t)
	provider := osdk.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"))
	req := osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")}

	response, summary, err := ChatCompletionsNewStreaming(context.Background(), env.Client, provider, req)
	if err == nil || !strings.Contains(err.Error(), "stream exploded") {
		t.Fatalf("stream error = %v, want stream exploded", err)
	}
	if response != nil || len(summary.Chunks) != 1 {
		t.Fatalf("native partial return = (%#v, %d chunks), want nil and one chunk", response, len(summary.Chunks))
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-4o-mini")
	attrs := testkit.SpanAttributes(span)
	if got := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(got, "partial answer") {
		t.Fatalf("gen_ai.output.messages = %q, want partial answer", got)
	}
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 6)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 2)
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", span.Status().Code)
	}
}

func TestOTelResponsesStreamingMapsPartialResultBeforeLaterError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_partial\",\"model\":\"gpt-5\",\"status\":\"in_progress\",\"output\":[],\"usage\":{\"input_tokens\":7,\"output_tokens\":3,\"total_tokens\":10}}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"item_id\":\"message_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"partial response\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"error\":\"responses stream exploded\"}\n\n")
	}))
	defer server.Close()

	env := testkit.NewOTelEnv(t)
	provider := osdk.NewClient(option.WithAPIKey("test"), option.WithBaseURL(server.URL+"/"))
	req := responses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}

	response, summary, err := ResponsesNewStreaming(context.Background(), env.Client, provider, req)
	if err == nil || !strings.Contains(err.Error(), "responses stream exploded") {
		t.Fatalf("stream error = %v, want responses stream exploded", err)
	}
	if response != nil || len(summary.Events) != 2 || summary.FinalResponse != nil {
		t.Fatalf("native partial return = (%#v, %d events, final %#v)", response, len(summary.Events), summary.FinalResponse)
	}

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gpt-5")
	attrs := testkit.SpanAttributes(span)
	if got := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(got, "partial response") {
		t.Fatalf("gen_ai.output.messages = %q, want partial response", got)
	}
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.input_tokens", 7)
	requireOTelInt64Attr(t, attrs, "gen_ai.usage.output_tokens", 3)
	requireOTelStringAttr(t, attrs, "gen_ai.response.status", "in_progress")
	if span.Status().Code != codes.Error {
		t.Fatalf("span status = %v, want error", span.Status().Code)
	}
}

func requireOTelStringAttr(t *testing.T, attrs map[string]attribute.Value, key, want string) {
	t.Helper()
	value, ok := attrs[key]
	if !ok || value.AsString() != want {
		t.Fatalf("%s = %v, want %q", key, value, want)
	}
}

func requireOTelInt64Attr(t *testing.T, attrs map[string]attribute.Value, key string, want int64) {
	t.Helper()
	value, ok := attrs[key]
	if !ok || value.AsInt64() != want {
		t.Fatalf("%s = %v, want %d", key, value, want)
	}
}

func collectOTelMetrics(t *testing.T, env *testkit.Env) []metricdata.Metrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := env.Metrics.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect OTel metrics: %v", err)
	}
	var metrics []metricdata.Metrics
	for _, scope := range collected.ScopeMetrics {
		metrics = append(metrics, scope.Metrics...)
	}
	return metrics
}

func requireNoOTelTokenPoints(t *testing.T, metrics []metricdata.Metrics) {
	t.Helper()
	for _, metric := range metrics {
		if metric.Name != "gen_ai.client.token.usage" {
			continue
		}
		histogram, ok := metric.Data.(metricdata.Histogram[int64])
		if !ok {
			t.Fatalf("token usage data = %T, want int64 histogram", metric.Data)
		}
		if len(histogram.DataPoints) != 0 {
			t.Fatalf("usage-less response recorded %d token points", len(histogram.DataPoints))
		}
	}
}

func findOTelHistogram[N int64 | float64](t *testing.T, metrics []metricdata.Metrics, name string) metricdata.Histogram[N] {
	t.Helper()
	for _, metric := range metrics {
		if metric.Name != name {
			continue
		}
		if histogram, ok := metric.Data.(metricdata.Histogram[N]); ok {
			return histogram
		}
	}
	t.Fatalf("OTel histogram %q not found", name)
	return metricdata.Histogram[N]{}
}

func findOTelPoint[N int64 | float64](t *testing.T, points []metricdata.HistogramDataPoint[N], want map[string]string) metricdata.HistogramDataPoint[N] {
	t.Helper()
	for _, point := range points {
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
	t.Fatalf("OTel histogram point with attributes %v not found", want)
	return metricdata.HistogramDataPoint[N]{}
}
