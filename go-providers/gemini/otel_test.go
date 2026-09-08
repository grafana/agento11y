package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y/testkit"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	geminiOTelOperation       = "gen_ai.operation.name"
	geminiOTelProvider        = "gen_ai.provider.name"
	geminiOTelMaxTokens       = "gen_ai.request.max_tokens"
	geminiOTelTemperature     = "gen_ai.request.temperature"
	geminiOTelTopP            = "gen_ai.request.top_p"
	geminiOTelTopK            = "gen_ai.request.top_k"
	geminiOTelChoiceCount     = "gen_ai.request.choice.count"
	geminiOTelSeed            = "gen_ai.request.seed"
	geminiOTelOutputType      = "gen_ai.output.type"
	geminiOTelFinishReasons   = "gen_ai.response.finish_reasons"
	geminiOTelOutputMessages  = "gen_ai.output.messages"
	geminiOTelInputTokens     = "gen_ai.usage.input_tokens"
	geminiOTELOutputTokens    = "gen_ai.usage.output_tokens"
	geminiOTelReasoningTokens = "gen_ai.usage.reasoning.output_tokens"
	geminiOTelTokenUsage      = "gen_ai.client.token.usage"
	geminiOTelTokenType       = "gen_ai.token.type"
)

func TestGeminiOTelImmediateErrorPreservesRequestControls(t *testing.T) {
	temperature, topP, topK := float32(0.4), float32(0.8), float32(40)
	seed := int32(7)
	thinkingBudget := int32(2048)
	config := &genai.GenerateContentConfig{
		MaxOutputTokens:  256,
		Temperature:      &temperature,
		TopP:             &topP,
		TopK:             &topK,
		CandidateCount:   3,
		Seed:             &seed,
		ResponseMIMEType: "application/json",
		ToolConfig: &genai.ToolConfig{FunctionCallingConfig: &genai.FunctionCallingConfig{
			Mode: genai.FunctionCallingConfigModeAny,
		}},
		ThinkingConfig: &genai.ThinkingConfig{ThinkingBudget: &thinkingBudget},
	}
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("provider failed before response")
	_, err := generateContent(context.Background(), env.Client, "gemini-2.5-pro", nil, config, func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return nil, providerErr
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("native error = %v, want provider error", err)
	}

	attrs := testkit.SpanAttributes(testkit.FindSpan(t, env.Spans.Ended(), "chat gemini-2.5-pro"))
	requireGeminiOTelInt(t, attrs, geminiOTelMaxTokens, 256)
	requireGeminiOTelFloat(t, attrs, geminiOTelTemperature, 0.4)
	requireGeminiOTelFloat(t, attrs, geminiOTelTopP, 0.8)
	requireGeminiOTelInt(t, attrs, geminiOTelTopK, 40)
	requireGeminiOTelInt(t, attrs, geminiOTelChoiceCount, 3)
	requireGeminiOTelInt(t, attrs, geminiOTelSeed, 7)
	requireGeminiOTelString(t, attrs, geminiOTelOutputType, "json")
	requireGeminiOTelString(t, attrs, "agento11y.gen_ai.request.tool_choice", "any")
	if !attrs["agento11y.gen_ai.request.thinking.enabled"].AsBool() {
		t.Fatal("thinking was not recorded as enabled")
	}
	requireGeminiOTelInt(t, attrs, "agento11y.gen_ai.request.thinking.budget_tokens", 2048)
}

func TestGeminiOTelImmediateErrorThinkingLevelEnablesThinking(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	config := &genai.GenerateContentConfig{ThinkingConfig: &genai.ThinkingConfig{
		ThinkingLevel:   genai.ThinkingLevelHigh,
		IncludeThoughts: false,
	}}
	providerErr := errors.New("provider failed before response")
	_, err := generateContent(context.Background(), env.Client, "gemini-2.5-pro", nil, config, func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
		return nil, providerErr
	})
	if !errors.Is(err, providerErr) {
		t.Fatalf("native error = %v, want provider error", err)
	}

	attrs := testkit.SpanAttributes(testkit.FindSpan(t, env.Spans.Ended(), "chat gemini-2.5-pro"))
	if !attrs["agento11y.gen_ai.request.thinking.enabled"].AsBool() {
		t.Fatal("thinking level was not recorded as enabled when thought parts were excluded")
	}
}

func TestGeminiProviderToOTel(t *testing.T) {
	t.Run("text candidates controls and usage", func(t *testing.T) {
		env := testkit.NewOTelEnv(t)
		temperature := float32(0.25)
		topP := float32(0.8)
		topK := float32(40)
		config := &genai.GenerateContentConfig{
			MaxOutputTokens: 256,
			Temperature:     &temperature,
			TopP:            &topP,
			TopK:            &topK,
		}
		expectedResponse := &genai.GenerateContentResponse{
			ResponseID:   "gemini-response",
			ModelVersion: "gemini-2.5-pro-001",
			Candidates: []*genai.Candidate{
				{Index: 0, FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("first answer", genai.RoleModel)},
				{Index: 1, FinishReason: genai.FinishReasonMaxTokens, Content: genai.NewContentFromText("second answer", genai.RoleModel)},
			},
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:     10,
				CandidatesTokenCount: 20,
				ThoughtsTokenCount:   80,
				TotalTokenCount:      110,
			},
		}

		response, err := generateContent(
			context.Background(), env.Client, "gemini-2.5-pro",
			[]*genai.Content{genai.NewContentFromText("question", genai.RoleUser)}, config,
			func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return expectedResponse, nil
			},
		)
		if err != nil {
			t.Fatalf("generate content: %v", err)
		}
		if response != expectedResponse {
			t.Fatalf("native response pointer was not preserved")
		}
		flushGeminiOTel(t, env)

		span := testkit.FindSpan(t, env.Spans.Ended(), "chat gemini-2.5-pro")
		attrs := testkit.SpanAttributes(span)
		requireGeminiOTelString(t, attrs, geminiOTelProvider, "gcp.gemini")
		requireGeminiOTelString(t, attrs, geminiOTelOperation, "chat")
		requireGeminiOTelInt(t, attrs, geminiOTelMaxTokens, 256)
		requireGeminiOTelFloat(t, attrs, geminiOTelTemperature, 0.25)
		requireGeminiOTelFloat(t, attrs, geminiOTelTopP, 0.8)
		requireGeminiOTelInt(t, attrs, geminiOTelTopK, 40)
		requireGeminiOTelInt(t, attrs, geminiOTelInputTokens, 10)
		requireGeminiOTelInt(t, attrs, geminiOTELOutputTokens, 100)
		requireGeminiOTelInt(t, attrs, geminiOTelReasoningTokens, 80)
		if got := attrs[geminiOTelFinishReasons].AsStringSlice(); len(got) != 2 || got[0] != "STOP" || got[1] != "MAX_TOKENS" {
			t.Fatalf("finish reasons = %v, want [STOP MAX_TOKENS]", got)
		}

		var output []struct {
			Parts []struct {
				Content string `json:"content"`
			} `json:"parts"`
			FinishReason string `json:"finish_reason"`
		}
		if err := json.Unmarshal([]byte(attrs[geminiOTelOutputMessages].AsString()), &output); err != nil {
			t.Fatalf("decode output messages: %v", err)
		}
		if len(output) != 2 || output[0].Parts[0].Content != "first answer" || output[0].FinishReason != "STOP" ||
			output[1].Parts[0].Content != "second answer" || output[1].FinishReason != "MAX_TOKENS" {
			t.Fatalf("unexpected candidate output messages: %#v", output)
		}

		metrics := collectGeminiOTelMetrics(t, env)
		requireGeminiTokenPoint(t, metrics, "input", 10)
		requireGeminiTokenPoint(t, metrics, "output", 100)
		requireGeminiTokenPoint(t, metrics, "reasoning", 80)
	})

	t.Run("non-text output uses generate content", func(t *testing.T) {
		env := testkit.NewOTelEnv(t)
		config := &genai.GenerateContentConfig{ResponseModalities: []string{"IMAGE"}}
		_, err := generateContent(
			context.Background(), env.Client, "gemini-2.5-flash-image",
			[]*genai.Content{genai.NewContentFromText("draw a cat", genai.RoleUser)}, config,
			func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
				return &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
					FinishReason: genai.FinishReasonStop,
					Content:      genai.NewContentFromText("generated", genai.RoleModel),
				}}}, nil
			},
		)
		if err != nil {
			t.Fatalf("generate content: %v", err)
		}
		flushGeminiOTel(t, env)
		span := testkit.FindSpan(t, env.Spans.Ended(), "generate_content gemini-2.5-flash-image")
		attrs := testkit.SpanAttributes(span)
		requireGeminiOTelString(t, attrs, geminiOTelProvider, "gcp.gemini")
		requireGeminiOTelString(t, attrs, geminiOTelOperation, "generate_content")
	})
}

func TestGeminiResponseErrorRecordsPartialResult(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("request interrupted")
	partial := &genai.GenerateContentResponse{
		ResponseID:   "partial-response",
		ModelVersion: "gemini-2.5-pro-001",
		Candidates: []*genai.Candidate{{
			Index:   0,
			Content: genai.NewContentFromText("partial answer", genai.RoleModel),
		}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     4,
			CandidatesTokenCount: 3,
			TotalTokenCount:      7,
		},
	}

	response, err := generateContent(
		context.Background(), env.Client, "gemini-2.5-pro", nil, nil,
		func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
			return partial, providerErr
		},
	)
	if response != partial || !errors.Is(err, providerErr) {
		t.Fatalf("native return = (%p, %v), want (%p, %v)", response, err, partial, providerErr)
	}
	flushGeminiOTel(t, env)

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gemini-2.5-pro")
	attrs := testkit.SpanAttributes(span)
	requireGeminiOTelString(t, attrs, "gen_ai.response.id", "partial-response")
	if got := attrs[geminiOTelOutputMessages].AsString(); !strings.Contains(got, "partial answer") {
		t.Fatalf("partial output messages = %q", got)
	}
	if span.Status().Code != codes.Error || attrs["error.type"].AsString() == "" {
		t.Fatalf("error telemetry = status %v error.type %q", span.Status().Code, attrs["error.type"].AsString())
	}
}

func TestGeminiStreamErrorRecordsPartialResult(t *testing.T) {
	env := testkit.NewOTelEnv(t)
	providerErr := errors.New("stream interrupted")
	partial := &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{{Index: 0, Content: genai.NewContentFromText("partial answer", genai.RoleModel)}},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:     4,
			CandidatesTokenCount: 3,
		},
	}

	summary, err := generateContentStream(
		context.Background(), env.Client, "gemini-2.5-pro", nil, nil,
		func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
			return func(yield func(*genai.GenerateContentResponse, error) bool) {
				if !yield(partial, nil) {
					return
				}
				yield(nil, providerErr)
			}
		},
	)
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected native stream error, got %v", err)
	}
	if len(summary.Responses) != 1 || summary.Responses[0] != partial {
		t.Fatalf("partial native stream responses were not preserved: %#v", summary.Responses)
	}
	flushGeminiOTel(t, env)

	span := testkit.FindSpan(t, env.Spans.Ended(), "chat gemini-2.5-pro")
	attrs := testkit.SpanAttributes(span)
	var output []struct {
		Parts []struct {
			Content string `json:"content"`
		} `json:"parts"`
	}
	if err := json.Unmarshal([]byte(attrs[geminiOTelOutputMessages].AsString()), &output); err != nil {
		t.Fatalf("decode partial output: %v", err)
	}
	if len(output) != 1 || output[0].Parts[0].Content != "partial answer" {
		t.Fatalf("unexpected partial output: %#v", output)
	}
	requireGeminiOTelInt(t, attrs, geminiOTelInputTokens, 4)
	requireGeminiOTelInt(t, attrs, geminiOTELOutputTokens, 3)
	if span.Status().Code != codes.Error || attrs["error.type"].AsString() == "" {
		t.Fatalf("error telemetry = status %v error.type %q", span.Status().Code, attrs["error.type"].AsString())
	}
	metrics := collectGeminiOTelMetrics(t, env)
	requireGeminiTokenPoint(t, metrics, "input", 4)
	requireGeminiTokenPoint(t, metrics, "output", 3)
}

func flushGeminiOTel(t *testing.T, env *testkit.Env) {
	t.Helper()
	if err := env.Client.Flush(context.Background()); err != nil {
		t.Fatalf("flush OTel: %v", err)
	}
}

func collectGeminiOTelMetrics(t *testing.T, env *testkit.Env) metricdata.ResourceMetrics {
	t.Helper()
	var metrics metricdata.ResourceMetrics
	if err := env.Metrics.Collect(context.Background(), &metrics); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return metrics
}

func requireGeminiTokenPoint(t *testing.T, metrics metricdata.ResourceMetrics, tokenType string, want int64) {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != geminiOTelTokenUsage {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[int64])
			if !ok {
				t.Fatalf("%s has data type %T", metric.Name, metric.Data)
			}
			for _, point := range histogram.DataPoints {
				if geminiMetricAttr(point.Attributes, geminiOTelProvider) == "gcp.gemini" &&
					geminiMetricAttr(point.Attributes, geminiOTelOperation) == "chat" &&
					geminiMetricAttr(point.Attributes, geminiOTelTokenType) == tokenType {
					if point.Count != 1 || point.Sum != want {
						t.Fatalf("%s token point count/sum = %d/%d, want 1/%d", tokenType, point.Count, point.Sum, want)
					}
					return
				}
			}
		}
	}
	t.Fatalf("missing %s token point for gcp.gemini chat", tokenType)
}

func geminiMetricAttr(attrs attribute.Set, key string) string {
	value, ok := attrs.Value(attribute.Key(key))
	if !ok {
		return ""
	}
	return value.AsString()
}

func requireGeminiOTelString(t *testing.T, attrs map[string]attribute.Value, key, want string) {
	t.Helper()
	if got := attrs[key].AsString(); got != want {
		t.Fatalf("%s = %q, want %q", key, got, want)
	}
}

func requireGeminiOTelInt(t *testing.T, attrs map[string]attribute.Value, key string, want int64) {
	t.Helper()
	if got := attrs[key].AsInt64(); got != want {
		t.Fatalf("%s = %d, want %d", key, got, want)
	}
}

func requireGeminiOTelFloat(t *testing.T, attrs map[string]attribute.Value, key string, want float64) {
	t.Helper()
	if got := attrs[key].AsFloat64(); got < want-1e-6 || got > want+1e-6 {
		t.Fatalf("%s = %v, want %v", key, got, want)
	}
}
