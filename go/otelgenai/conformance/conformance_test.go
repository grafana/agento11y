//go:build conformance

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/agento11y/otelhook"
	"github.com/grafana/agento11y/go/otelgenai"
	"github.com/grafana/agento11y/go/otelgenai/weavertest"
)

type expectedViolation struct {
	adviceID        string
	messageContains string
}

func (e expectedViolation) matches(violation weavertest.Violation) bool {
	return violation.ID == e.adviceID && strings.Contains(violation.Message, e.messageContains)
}

type scenario struct {
	name               string
	options            []otelgenai.Option
	emit               func(*otelgenai.Handler)
	assertRecorded     func(testing.TB, *providers)
	expectedSpans      map[string]int
	expectedMetrics    []string
	expectedViolations []expectedViolation
}

type providers struct {
	traces       *sdktrace.TracerProvider
	metrics      *sdkmetric.MeterProvider
	logs         *sdklog.LoggerProvider
	spanRecorder *tracetest.SpanRecorder
	metricReader *sdkmetric.ManualReader
	logRecorder  *recordingLogExporter
}

type recordingLogExporter struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (e *recordingLogExporter) Export(_ context.Context, records []sdklog.Record) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, record := range records {
		e.records = append(e.records, record.Clone())
	}
	return nil
}

func (*recordingLogExporter) Shutdown(context.Context) error   { return nil }
func (*recordingLogExporter) ForceFlush(context.Context) error { return nil }

func (e *recordingLogExporter) Records() []sdklog.Record {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]sdklog.Record, len(e.records))
	for i := range e.records {
		out[i] = e.records[i].Clone()
	}
	return out
}

func TestConformance(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Skip("weaver is not on PATH")
	}
	registryRef := os.Getenv("SEMCONV_GENAI_REF")
	if registryRef == "" {
		t.Fatal("SEMCONV_GENAI_REF is not set; run the conformance test through mise")
	}
	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 4*time.Minute)
	assets, err := weavertest.Setup(setupCtx, registryRef)
	cancelSetup()
	if err != nil {
		t.Fatalf("prepare Weaver inputs: %v", err)
	}

	for _, test := range conformanceScenarios() {
		t.Run(test.name, func(t *testing.T) {
			report := executeScenario(t, assets, test)
			if err := reconcileViolations(report.Violations(), test.expectedViolations); err != nil {
				t.Error(err)
			}
			if got := report.SpanOperationCounts(); !maps.Equal(got, test.expectedSpans) {
				t.Errorf("span operation counts = %v, want %v", got, test.expectedSpans)
			}
			expectedMetrics := make(map[string]struct{}, len(test.expectedMetrics))
			for _, metric := range test.expectedMetrics {
				expectedMetrics[metric] = struct{}{}
			}
			if got := report.SeenMetricNames(); !maps.Equal(got, expectedMetrics) {
				t.Errorf("metric names = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(expectedMetrics)))
			}
		})
	}

	t.Run("rejects an out-of-enum operation name", func(t *testing.T) {
		test := scenario{
			name: "invalid_operation",
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Operation = otelgenai.Operation("generateText")
				recordInvocation(handler, inv)
			},
		}
		report := executeScenario(t, assets, test)
		expected := []expectedViolation{{
			adviceID:        "undefined_enum_variant",
			messageContains: "generateText",
		}}
		if err := reconcileViolations(report.Violations(), expected); err != nil {
			t.Error(err)
		}
	})

	t.Run("file policy validation", func(t *testing.T) {
		malformed, err := json.Marshal("{malformed")
		if err != nil {
			t.Fatal(err)
		}
		samples := []weavertest.Sample{
			contentSample("malformed content", json.RawMessage(malformed)),
			contentSample("structured content", json.RawMessage(`[{"role":"user","parts":[]}]`)),
			memorySample("search_memory", "client"),
			memorySample("search_memory", "internal"),
			memorySample("memory search", "server"),
		}
		liveCheckCtx, cancelLiveCheck := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancelLiveCheck()
		findings, err := weavertest.LiveCheck(liveCheckCtx, assets, samples)
		if err != nil {
			t.Fatalf("file live-check: %v", err)
		}
		if !hasFinding(findings, "chat malformed-content", "gen_ai.input.messages", "genai_content_schema") {
			t.Errorf("findings do not contain the malformed JSON result: %#v", findings)
		}
		for _, finding := range findings {
			if finding.Span == "chat structured-content" && finding.Target == "gen_ai.input.messages" {
				t.Errorf("structured file content produced an unsupported claim: %#v", finding)
			}
			if finding.Span == "search_memory" {
				t.Errorf("valid memory span produced a finding: %#v", finding)
			}
		}
		if !hasFinding(findings, "memory search", "", "genai_span_name_format") {
			t.Errorf("findings do not reject the memory span name: %#v", findings)
		}
		if !hasFinding(findings, "memory search", "", "genai_span_kind_unexpected") {
			t.Errorf("findings do not reject the memory span kind: %#v", findings)
		}
	})
}

func contentSample(name string, content json.RawMessage) weavertest.Sample {
	model := strings.ReplaceAll(name, " ", "-")
	return weavertest.Sample{Span: weavertest.SampleSpan{
		Name:   "chat " + model,
		Kind:   "client",
		Status: weavertest.SampleSpanStatus{Code: "unset", Message: ""},
		Attributes: []weavertest.SampleAttribute{
			{Name: "gen_ai.operation.name", Value: json.RawMessage(`"chat"`)},
			{Name: "gen_ai.provider.name", Value: json.RawMessage(`"openai"`)},
			{Name: "gen_ai.request.model", Value: json.RawMessage(`"` + model + `"`)},
			{Name: "gen_ai.response.id", Value: json.RawMessage(`"response"`)},
			{Name: "gen_ai.response.model", Value: json.RawMessage(`"model"`)},
			{Name: "gen_ai.response.finish_reasons", Value: json.RawMessage(`["stop"]`)},
			{Name: "gen_ai.usage.input_tokens", Value: json.RawMessage(`1`)},
			{Name: "gen_ai.usage.output_tokens", Value: json.RawMessage(`1`)},
			{Name: "server.address", Value: json.RawMessage(`"example.test"`)},
			{Name: "gen_ai.input.messages", Value: content},
		},
		SpanEvents: []weavertest.SampleSpanEvent{},
		SpanLinks:  []weavertest.SampleSpanLink{},
	}}
}

func memorySample(name, kind string) weavertest.Sample {
	attributes := []weavertest.SampleAttribute{
		{Name: "gen_ai.operation.name", Value: json.RawMessage(`"search_memory"`)},
	}
	if kind != "internal" {
		attributes = append(attributes, weavertest.SampleAttribute{
			Name: "server.address", Value: json.RawMessage(`"memory.example.test"`),
		})
	}
	return weavertest.Sample{Span: weavertest.SampleSpan{
		Name:       name,
		Kind:       kind,
		Status:     weavertest.SampleSpanStatus{Code: "unset"},
		Attributes: attributes,
		SpanEvents: []weavertest.SampleSpanEvent{},
		SpanLinks:  []weavertest.SampleSpanLink{},
	}}
}

func hasFinding(findings []weavertest.Finding, span, target, id string) bool {
	for _, finding := range findings {
		if finding.Span == span && finding.Target == target && finding.ID == id {
			return true
		}
	}
	return false
}

func conformanceScenarios() []scenario {
	return []scenario{
		{
			name: "maximal inference",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, inferenceInvocation())
			},
			assertRecorded: assertMaximalInference,
			expectedSpans:  map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "streaming inference",
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
				inv.CompletedAt = inv.StartedAt.Add(time.Second)
				ctx := handler.Start(context.Background(), inv)
				handler.RecordChunk(ctx, inv)
				handler.End(ctx, inv)
			},
			assertRecorded: assertStreamingInference,
			expectedSpans:  map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
				"gen_ai.client.operation.time_to_first_chunk",
			},
		},
		{
			name: "tool execution",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, &otelgenai.Invocation{
					Operation:       otelgenai.OperationExecuteTool,
					ToolName:        "weather",
					ToolCallID:      "call-1",
					ToolType:        "function",
					ToolDescription: "Returns the weather",
					StartedAt:       time.Now().Add(-time.Second),
				})
			},
			expectedSpans:   map[string]int{"execute_tool": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
		},
		{
			name: "default embeddings",
			emit: func(handler *otelgenai.Handler) {
				dimensions := int64(1536)
				recordInvocation(handler, &otelgenai.Invocation{
					Operation:      otelgenai.OperationEmbeddings,
					Provider:       "openai",
					RequestModel:   "text-embedding-3-small",
					ResponseModel:  "text-embedding-3-small",
					ServerAddress:  "api.openai.com",
					DimensionCount: &dimensions,
					Usage:          otelgenai.Usage{InputTokens: 12},
					StartedAt:      time.Now().Add(-time.Second),
				})
			},
			expectedSpans: map[string]int{"embeddings": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "realistic failed inference",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, failedInferenceInvocation())
			},
			assertRecorded:  assertFailedInference,
			expectedSpans:   map[string]int{"chat": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
		},
		{
			name: "fetch timeout",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, fetchTimeoutInvocation())
			},
			assertRecorded:  assertFetchTimeout,
			expectedSpans:   map[string]int{"fetch_response": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
		},
		{
			name: "failed fetch without response ID",
			emit: func(handler *otelgenai.Handler) {
				inv := fetchTimeoutInvocation()
				inv.ResponseID = ""
				recordInvocation(handler, inv)
			},
			expectedSpans:   map[string]int{"fetch_response": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
			expectedViolations: []expectedViolation{
				{adviceID: "genai_expected_attribute_missing", messageContains: "gen_ai.response.id"},
			},
		},
		{
			name: "successful fetch without response attributes",
			emit: func(handler *otelgenai.Handler) {
				inv := fetchTimeoutInvocation()
				inv.ErrorType = ""
				inv.ErrorMessage = ""
				inv.ResponseID = ""
				recordInvocation(handler, inv)
			},
			expectedSpans:   map[string]int{"fetch_response": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
			expectedViolations: []expectedViolation{
				{adviceID: "genai_expected_attribute_missing", messageContains: "gen_ai.response.id"},
				{adviceID: "genai_expected_attribute_missing", messageContains: "gen_ai.response.status"},
				{adviceID: "genai_expected_attribute_missing", messageContains: "gen_ai.response.model"},
				{adviceID: "genai_expected_attribute_missing", messageContains: "gen_ai.response.finish_reasons"},
			},
		},
		{
			name:    "span and event content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureSpanAndEvent)},
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.SystemInstructions = otelgenai.SystemInstructionsFromText("Answer briefly.")
				inv.InputMessages = []otelgenai.Message{{Role: otelgenai.RoleUser, Parts: []otelgenai.Part{otelgenai.TextPart("Hello")}}}
				finishReason := "stop"
				inv.OutputMessages = []otelgenai.Message{{
					Role:         otelgenai.RoleAssistant,
					Parts:        []otelgenai.Part{otelgenai.TextPart("Hi")},
					FinishReason: &finishReason,
				}}
				recordInvocation(handler, inv)
			},
			assertRecorded: assertContentSignals,
			expectedSpans:  map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "combined operations",
			emit: func(handler *otelgenai.Handler) {
				for _, invocation := range combinedOperationInvocations() {
					recordInvocation(handler, invocation)
				}
			},
			assertRecorded: assertCombinedOperations,
			expectedSpans: map[string]int{
				"fetch_response":  1,
				"retrieval":       1,
				"invoke_agent":    1,
				"invoke_workflow": 1,
				"create_agent":    1,
				"plan":            1,
			},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "internal inference",
			emit: func(handler *otelgenai.Handler) {
				invocation := inferenceInvocation()
				invocation.Kind = trace.SpanKindInternal
				invocation.ServerAddress = ""
				invocation.ServerPort = 0
				recordInvocation(handler, invocation)
			},
			assertRecorded: assertInternalInference,
			expectedSpans:  map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name:    "agento11y extension attributes",
			options: []otelgenai.Option{otelgenai.WithEndHook(otelhook.New())},
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Vendor = otelhook.Generation{
					ID:       "gen-1",
					Metadata: map[string]any{"source": "conformance"},
				}
				recordInvocation(handler, inv)
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
			expectedViolations: []expectedViolation{
				{adviceID: "missing_attribute", messageContains: "agento11y.record"},
				{adviceID: "missing_attribute", messageContains: "agento11y.generation.id"},
				{adviceID: "missing_attribute", messageContains: "agento11y.generation.metadata"},
			},
		},
	}
}

func inferenceInvocation() *otelgenai.Invocation {
	maxTokens, topK, seed, choiceCount := int64(256), int64(40), int64(7), int64(2)
	temperature, topP := 0.7, 0.9
	frequencyPenalty, presencePenalty := 0.1, 0.2
	return &otelgenai.Invocation{
		Operation:        otelgenai.OperationChat,
		Provider:         "openai",
		RequestModel:     "gpt-4.1-mini",
		ResponseModel:    "gpt-4.1-mini-2025-04-14",
		ResponseID:       "resp-1",
		ResponseStatus:   "completed",
		ServerAddress:    "api.openai.com",
		ServerPort:       443,
		FinishReasons:    []string{"stop"},
		Usage:            otelgenai.Usage{InputTokens: 12, OutputTokens: 7, CacheReadInputTokens: 3, CacheWriteInputTokens: 2, ReasoningTokens: 1},
		MaxTokens:        &maxTokens,
		Temperature:      &temperature,
		TopP:             &topP,
		TopK:             &topK,
		ChoiceCount:      &choiceCount,
		OutputType:       "json",
		FrequencyPenalty: &frequencyPenalty,
		PresencePenalty:  &presencePenalty,
		StopSequences:    []string{"stop", "done"},
		Seed:             &seed,
		StartedAt:        time.Unix(1_700_000_000, 0),
	}
}

func maximalInferenceSpanAttributes() map[string]any {
	return map[string]any{
		"gen_ai.operation.name":                    "chat",
		"gen_ai.provider.name":                     "openai",
		"gen_ai.request.model":                     "gpt-4.1-mini",
		"gen_ai.request.max_tokens":                int64(256),
		"gen_ai.request.temperature":               0.7,
		"gen_ai.request.top_p":                     0.9,
		"gen_ai.request.top_k":                     int64(40),
		"gen_ai.request.choice.count":              int64(2),
		"gen_ai.request.frequency_penalty":         0.1,
		"gen_ai.request.presence_penalty":          0.2,
		"gen_ai.request.stop_sequences":            []string{"stop", "done"},
		"gen_ai.request.seed":                      int64(7),
		"gen_ai.output.type":                       "json",
		"gen_ai.response.id":                       "resp-1",
		"gen_ai.response.model":                    "gpt-4.1-mini-2025-04-14",
		"gen_ai.response.status":                   "completed",
		"gen_ai.response.finish_reasons":           []string{"stop"},
		"gen_ai.usage.input_tokens":                int64(12),
		"gen_ai.usage.output_tokens":               int64(7),
		"gen_ai.usage.cache_read.input_tokens":     int64(3),
		"gen_ai.usage.cache_creation.input_tokens": int64(2),
		"gen_ai.usage.reasoning.output_tokens":     int64(1),
		"server.address":                           "api.openai.com",
		"server.port":                              int64(443),
	}
}

func failedInferenceInvocation() *otelgenai.Invocation {
	maxTokens := int64(256)
	temperature := 0.7
	return &otelgenai.Invocation{
		Operation:     otelgenai.OperationChat,
		Provider:      "openai",
		RequestModel:  "gpt-4.1-mini",
		ServerAddress: "api.openai.com",
		ServerPort:    443,
		MaxTokens:     &maxTokens,
		Temperature:   &temperature,
		ErrorType:     "timeout",
		ErrorMessage:  "request timed out",
		StartedAt:     time.Unix(1_700_000_000, 0),
	}
}

func fetchTimeoutInvocation() *otelgenai.Invocation {
	return &otelgenai.Invocation{
		Operation:     otelgenai.OperationFetchResponse,
		Provider:      "openai",
		ResponseID:    "resp-fetch",
		ServerAddress: "api.openai.com",
		ServerPort:    443,
		ErrorType:     "timeout",
		ErrorMessage:  "request timed out",
		StartedAt:     time.Unix(1_700_000_000, 0),
	}
}

func combinedOperationInvocations() []*otelgenai.Invocation {
	startedAt := time.Unix(1_700_000_000, 0)
	return []*otelgenai.Invocation{
		{
			Operation:      otelgenai.OperationFetchResponse,
			Provider:       "openai",
			ResponseID:     "resp-fetch",
			ResponseModel:  "gpt-4.1-mini-2025-04-14",
			ResponseStatus: "completed",
			FinishReasons:  []string{"stop"},
			StreamCursor:   "cursor-1",
			ServerAddress:  "api.openai.com",
			ServerPort:     443,
			Usage:          otelgenai.Usage{InputTokens: 12, OutputTokens: 7},
			StartedAt:      startedAt,
		},
		{
			Operation:     otelgenai.OperationRetrieval,
			Provider:      "custom",
			RequestModel:  "embedder-v1",
			DataSourceID:  "knowledge-base",
			ServerAddress: "retrieval.example.test",
			ServerPort:    443,
			Attributes:    []attribute.KeyValue{attribute.Int64("gen_ai.retrieval.top_k", 3)},
			StartedAt:     startedAt,
		},
		{
			Operation:        otelgenai.OperationInvokeAgent,
			Provider:         "openai",
			RequestModel:     "gpt-4.1-mini",
			AgentName:        "assistant",
			AgentID:          "agent-1",
			AgentVersion:     "1.0",
			AgentDescription: "Helpful assistant",
			ConversationID:   "conversation-1",
			ServerAddress:    "api.openai.com",
			ServerPort:       443,
			FinishReasons:    []string{"stop"},
			Usage:            otelgenai.Usage{InputTokens: 4, OutputTokens: 2},
			StartedAt:        startedAt,
		},
		{
			Operation:      otelgenai.OperationInvokeWorkflow,
			WorkflowName:   "answer-question",
			ConversationID: "conversation-1",
			StartedAt:      startedAt,
		},
		{
			Operation:        otelgenai.OperationCreateAgent,
			Provider:         "custom",
			RequestModel:     "agent-builder-v1",
			AgentName:        "assistant",
			AgentID:          "agent-1",
			AgentVersion:     "1.0",
			AgentDescription: "Helpful assistant",
			ServerAddress:    "agents.example.test",
			ServerPort:       443,
			StartedAt:        startedAt,
		},
		{
			Operation: otelgenai.OperationPlan,
			AgentName: "assistant",
			StartedAt: startedAt,
		},
	}
}

func assertMaximalInference(t testing.TB, providers *providers) {
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:       "chat gpt-4.1-mini",
		kind:       trace.SpanKindClient,
		status:     codes.Unset,
		attributes: maximalInferenceSpanAttributes(),
	}})

	metricAttributes := map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4.1-mini",
		"gen_ai.response.model": "gpt-4.1-mini-2025-04-14",
		"server.address":        "api.openai.com",
		"server.port":           int64(443),
	}
	inputAttributes := maps.Clone(metricAttributes)
	inputAttributes["gen_ai.token.type"] = "input"
	outputAttributes := maps.Clone(metricAttributes)
	outputAttributes["gen_ai.token.type"] = "output"
	assertExactMetrics(t, providers, []expectedMetricSignal{
		{
			name:   "gen_ai.client.operation.duration",
			unit:   "s",
			points: []expectedMetricPoint{{attributes: metricAttributes, count: 1, sum: 1.0}},
		},
		{
			name: "gen_ai.client.token.usage",
			unit: "{token}",
			points: []expectedMetricPoint{
				{attributes: inputAttributes, count: 1, sum: int64(12)},
				{attributes: outputAttributes, count: 1, sum: int64(7)},
			},
		},
	})
}

func assertStreamingInference(t testing.TB, providers *providers) {
	spanAttributes := maximalInferenceSpanAttributes()
	spanAttributes["gen_ai.request.stream"] = true
	spanAttributes["gen_ai.response.time_to_first_chunk"] = 0.25
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:       "chat gpt-4.1-mini",
		kind:       trace.SpanKindClient,
		status:     codes.Unset,
		attributes: spanAttributes,
	}})

	metricAttributes := map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4.1-mini",
		"gen_ai.response.model": "gpt-4.1-mini-2025-04-14",
		"server.address":        "api.openai.com",
		"server.port":           int64(443),
	}
	inputAttributes := maps.Clone(metricAttributes)
	inputAttributes["gen_ai.token.type"] = "input"
	outputAttributes := maps.Clone(metricAttributes)
	outputAttributes["gen_ai.token.type"] = "output"
	assertExactMetrics(t, providers, []expectedMetricSignal{
		{
			name:   "gen_ai.client.operation.duration",
			unit:   "s",
			points: []expectedMetricPoint{{attributes: metricAttributes, count: 1, sum: 1.0}},
		},
		{
			name:   "gen_ai.client.operation.time_to_first_chunk",
			unit:   "s",
			points: []expectedMetricPoint{{attributes: metricAttributes, count: 1, sum: 0.25}},
		},
		{
			name: "gen_ai.client.token.usage",
			unit: "{token}",
			points: []expectedMetricPoint{
				{attributes: inputAttributes, count: 1, sum: int64(12)},
				{attributes: outputAttributes, count: 1, sum: int64(7)},
			},
		},
	})
}

func assertFailedInference(t testing.TB, providers *providers) {
	spanAttributes := map[string]any{
		"gen_ai.operation.name":      "chat",
		"gen_ai.provider.name":       "openai",
		"gen_ai.request.model":       "gpt-4.1-mini",
		"gen_ai.request.max_tokens":  int64(256),
		"gen_ai.request.temperature": 0.7,
		"server.address":             "api.openai.com",
		"server.port":                int64(443),
		"error.type":                 "timeout",
	}
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:              "chat gpt-4.1-mini",
		kind:              trace.SpanKindClient,
		status:            codes.Error,
		statusDescription: "request timed out",
		attributes:        spanAttributes,
	}})
	metricAttributes := map[string]any{
		"gen_ai.operation.name": "chat",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4.1-mini",
		"server.address":        "api.openai.com",
		"server.port":           int64(443),
		"error.type":            "timeout",
	}
	assertExactMetrics(t, providers, []expectedMetricSignal{{
		name:   "gen_ai.client.operation.duration",
		unit:   "s",
		points: []expectedMetricPoint{{attributes: metricAttributes, count: 1, sum: 1.0}},
	}})
}

func assertFetchTimeout(t testing.TB, providers *providers) {
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:              "fetch_response",
		kind:              trace.SpanKindClient,
		status:            codes.Error,
		statusDescription: "request timed out",
		attributes: map[string]any{
			"gen_ai.operation.name": "fetch_response",
			"gen_ai.provider.name":  "openai",
			"gen_ai.response.id":    "resp-fetch",
			"server.address":        "api.openai.com",
			"server.port":           int64(443),
			"error.type":            "timeout",
		},
	}})
}

func assertCombinedOperations(t testing.TB, providers *providers) {
	assertExactSpans(t, providers, []expectedSpanSignal{
		{
			name:   "fetch_response",
			kind:   trace.SpanKindClient,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name":          "fetch_response",
				"gen_ai.provider.name":           "openai",
				"gen_ai.request.stream_cursor":   "cursor-1",
				"gen_ai.response.id":             "resp-fetch",
				"gen_ai.response.model":          "gpt-4.1-mini-2025-04-14",
				"gen_ai.response.status":         "completed",
				"gen_ai.response.finish_reasons": []string{"stop"},
				"server.address":                 "api.openai.com",
				"server.port":                    int64(443),
			},
		},
		{
			name:   "retrieval knowledge-base",
			kind:   trace.SpanKindClient,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name":  "retrieval",
				"gen_ai.provider.name":   "custom",
				"gen_ai.request.model":   "embedder-v1",
				"gen_ai.data_source.id":  "knowledge-base",
				"gen_ai.retrieval.top_k": int64(3),
				"server.address":         "retrieval.example.test",
				"server.port":            int64(443),
			},
		},
		{
			name:   "invoke_agent assistant",
			kind:   trace.SpanKindClient,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name":          "invoke_agent",
				"gen_ai.provider.name":           "openai",
				"gen_ai.request.model":           "gpt-4.1-mini",
				"gen_ai.agent.name":              "assistant",
				"gen_ai.agent.id":                "agent-1",
				"gen_ai.agent.version":           "1.0",
				"gen_ai.agent.description":       "Helpful assistant",
				"gen_ai.conversation.id":         "conversation-1",
				"gen_ai.response.finish_reasons": []string{"stop"},
				"gen_ai.usage.input_tokens":      int64(4),
				"gen_ai.usage.output_tokens":     int64(2),
				"server.address":                 "api.openai.com",
				"server.port":                    int64(443),
			},
		},
		{
			name:   "invoke_workflow answer-question",
			kind:   trace.SpanKindInternal,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name":  "invoke_workflow",
				"gen_ai.workflow.name":   "answer-question",
				"gen_ai.conversation.id": "conversation-1",
			},
		},
		{
			name:   "create_agent assistant",
			kind:   trace.SpanKindClient,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name":    "create_agent",
				"gen_ai.provider.name":     "custom",
				"gen_ai.request.model":     "agent-builder-v1",
				"gen_ai.agent.name":        "assistant",
				"gen_ai.agent.id":          "agent-1",
				"gen_ai.agent.version":     "1.0",
				"gen_ai.agent.description": "Helpful assistant",
				"server.address":           "agents.example.test",
				"server.port":              int64(443),
			},
		},
		{
			name:   "plan assistant",
			kind:   trace.SpanKindInternal,
			status: codes.Unset,
			attributes: map[string]any{
				"gen_ai.operation.name": "plan",
				"gen_ai.agent.name":     "assistant",
			},
		},
	})

	inputAttributes := map[string]any{
		"gen_ai.operation.name": "invoke_agent",
		"gen_ai.provider.name":  "openai",
		"gen_ai.request.model":  "gpt-4.1-mini",
		"gen_ai.token.type":     "input",
		"server.address":        "api.openai.com",
		"server.port":           int64(443),
	}
	outputAttributes := maps.Clone(inputAttributes)
	outputAttributes["gen_ai.token.type"] = "output"
	assertExactMetrics(t, providers, []expectedMetricSignal{
		{
			name: "gen_ai.client.operation.duration",
			unit: "s",
			points: []expectedMetricPoint{
				{attributes: map[string]any{
					"gen_ai.operation.name": "fetch_response",
					"gen_ai.provider.name":  "openai",
					"gen_ai.response.model": "gpt-4.1-mini-2025-04-14",
					"server.address":        "api.openai.com",
					"server.port":           int64(443),
				}, count: 1, sum: 1.0},
				{attributes: map[string]any{
					"gen_ai.operation.name": "retrieval",
					"gen_ai.provider.name":  "custom",
					"gen_ai.request.model":  "embedder-v1",
					"server.address":        "retrieval.example.test",
					"server.port":           int64(443),
				}, count: 1, sum: 1.0},
				{attributes: map[string]any{
					"gen_ai.operation.name": "invoke_agent",
					"gen_ai.provider.name":  "openai",
					"gen_ai.request.model":  "gpt-4.1-mini",
					"server.address":        "api.openai.com",
					"server.port":           int64(443),
				}, count: 1, sum: 1.0},
				{attributes: map[string]any{
					"gen_ai.operation.name": "create_agent",
					"gen_ai.provider.name":  "custom",
					"gen_ai.request.model":  "agent-builder-v1",
					"server.address":        "agents.example.test",
					"server.port":           int64(443),
				}, count: 1, sum: 1.0},
				{attributes: map[string]any{"gen_ai.operation.name": "plan"}, count: 1, sum: 1.0},
			},
		},
		{
			name: "gen_ai.client.token.usage",
			unit: "{token}",
			points: []expectedMetricPoint{
				{attributes: inputAttributes, count: 1, sum: int64(4)},
				{attributes: outputAttributes, count: 1, sum: int64(2)},
			},
		},
	})
}

func assertInternalInference(t testing.TB, providers *providers) {
	attributes := maximalInferenceSpanAttributes()
	delete(attributes, "server.address")
	delete(attributes, "server.port")
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:       "chat gpt-4.1-mini",
		kind:       trace.SpanKindInternal,
		status:     codes.Unset,
		attributes: attributes,
	}})
}

func assertContentSignals(t testing.TB, providers *providers) {
	spanAttributes := maximalInferenceSpanAttributes()
	spanAttributes["gen_ai.system_instructions"] = `[{"type":"text","content":"Answer briefly."}]`
	spanAttributes["gen_ai.input.messages"] = `[{"role":"user","parts":[{"type":"text","content":"Hello"}]}]`
	spanAttributes["gen_ai.output.messages"] = `[{"role":"assistant","parts":[{"type":"text","content":"Hi"}],"finish_reason":"stop"}]`
	assertExactSpans(t, providers, []expectedSpanSignal{{
		name:       "chat gpt-4.1-mini",
		kind:       trace.SpanKindClient,
		status:     codes.Unset,
		attributes: spanAttributes,
	}})

	eventAttributes := maximalInferenceSpanAttributes()
	eventAttributes["gen_ai.system_instructions"] = []any{map[string]any{
		"type": "text", "content": "Answer briefly.",
	}}
	eventAttributes["gen_ai.input.messages"] = []any{map[string]any{
		"role":  "user",
		"parts": []any{map[string]any{"type": "text", "content": "Hello"}},
	}}
	eventAttributes["gen_ai.output.messages"] = []any{map[string]any{
		"role":          "assistant",
		"parts":         []any{map[string]any{"type": "text", "content": "Hi"}},
		"finish_reason": "stop",
	}}
	assertExactLog(t, providers, "gen_ai.client.inference.operation.details", eventAttributes)
}

func recordInvocation(handler *otelgenai.Handler, inv *otelgenai.Invocation) {
	if inv.StartedAt.IsZero() {
		inv.StartedAt = time.Unix(1_700_000_000, 0)
	}
	ctx := handler.Start(context.Background(), inv)
	if inv.CompletedAt.IsZero() {
		inv.CompletedAt = inv.StartedAt.Add(time.Second)
	}
	handler.End(ctx, inv)
}

func executeScenario(t *testing.T, assets weavertest.Assets, test scenario) weavertest.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	weaver, err := weavertest.Start(ctx, assets)
	if errors.Is(err, weavertest.ErrNotInstalled) {
		t.Skip("weaver is not on PATH")
	}
	if err != nil {
		t.Fatalf("start Weaver: %v", err)
	}
	defer weaver.Close()

	providers, err := newProviders(ctx, weaver.Endpoint())
	if err != nil {
		t.Fatalf("create OTLP providers: %v", err)
	}
	defer providers.shutdown()

	options := []otelgenai.Option{
		otelgenai.WithTracerProvider(providers.traces),
		otelgenai.WithMeterProvider(providers.metrics),
		otelgenai.WithLoggerProvider(providers.logs),
		otelgenai.WithConformantMetrics(),
	}
	options = append(options, test.options...)
	handler := otelgenai.NewHandler(options...)
	test.emit(handler)

	if err := providers.forceFlush(ctx); err != nil {
		t.Fatalf("flush OTLP telemetry: %v", err)
	}
	if test.assertRecorded != nil {
		test.assertRecorded(t, &providers)
	}
	report, err := weaver.End(ctx)
	if err != nil {
		t.Fatalf("stop Weaver: %v", err)
	}
	dumpReport(t, test.name, report)
	return report
}

func newProviders(ctx context.Context, endpoint string) (providers, error) {
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return providers{}, err
	}
	spanRecorder := tracetest.NewSpanRecorder()
	traces := sdktrace.NewTracerProvider(
		sdktrace.WithSyncer(traceExporter),
		sdktrace.WithSpanProcessor(spanRecorder),
	)

	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	if err != nil {
		_ = traces.Shutdown(ctx)
		return providers{}, err
	}
	reader := sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(time.Hour))
	metricReader := sdkmetric.NewManualReader()
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader), sdkmetric.WithReader(metricReader))

	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpoint(endpoint), otlploggrpc.WithInsecure())
	if err != nil {
		_ = traces.Shutdown(ctx)
		_ = metrics.Shutdown(ctx)
		return providers{}, err
	}
	logRecorder := &recordingLogExporter{}
	logs := sdklog.NewLoggerProvider(
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)),
		sdklog.WithProcessor(sdklog.NewSimpleProcessor(logRecorder)),
	)
	return providers{
		traces:       traces,
		metrics:      metrics,
		logs:         logs,
		spanRecorder: spanRecorder,
		metricReader: metricReader,
		logRecorder:  logRecorder,
	}, nil
}

func (p providers) forceFlush(ctx context.Context) error {
	return errors.Join(
		p.traces.ForceFlush(ctx),
		p.metrics.ForceFlush(ctx),
		p.logs.ForceFlush(ctx),
	)
}

func (p providers) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.logs.Shutdown(ctx)
	_ = p.metrics.Shutdown(ctx)
	_ = p.traces.Shutdown(ctx)
}

type expectedSpanSignal struct {
	name              string
	kind              trace.SpanKind
	status            codes.Code
	statusDescription string
	attributes        map[string]any
}

type expectedMetricSignal struct {
	name   string
	unit   string
	points []expectedMetricPoint
}

type expectedMetricPoint struct {
	attributes map[string]any
	count      uint64
	sum        any
}

func assertExactSpans(t testing.TB, providers *providers, want []expectedSpanSignal) {
	t.Helper()
	got := providers.spanRecorder.Ended()
	sort.Slice(got, func(i, j int) bool { return got[i].Name() < got[j].Name() })
	sort.Slice(want, func(i, j int) bool { return want[i].name < want[j].name })
	if len(got) != len(want) {
		t.Fatalf("recorded spans = %d, want %d", len(got), len(want))
	}
	for i, expected := range want {
		span := got[i]
		if span.Name() != expected.name || span.SpanKind() != expected.kind || span.Status().Code != expected.status || span.Status().Description != expected.statusDescription {
			t.Errorf("span %d identity = (%q, %v, %v, %q), want (%q, %v, %v, %q)", i, span.Name(), span.SpanKind(), span.Status().Code, span.Status().Description, expected.name, expected.kind, expected.status, expected.statusDescription)
		}
		attributes := make(map[string]any, len(span.Attributes()))
		for _, attr := range span.Attributes() {
			attributes[string(attr.Key)] = normalizedAttributeValue(attr.Value)
		}
		if !reflect.DeepEqual(attributes, expected.attributes) {
			t.Errorf("span %q attributes = %#v, want %#v", span.Name(), attributes, expected.attributes)
		}
	}
}

func assertExactMetrics(t testing.TB, providers *providers, want []expectedMetricSignal) {
	t.Helper()
	var resource metricdata.ResourceMetrics
	if err := providers.metricReader.Collect(context.Background(), &resource); err != nil {
		t.Fatalf("collect in-memory metrics: %v", err)
	}
	got := map[string]metricdata.Metrics{}
	for _, scope := range resource.ScopeMetrics {
		for _, metric := range scope.Metrics {
			got[metric.Name] = metric
		}
	}
	if len(got) != len(want) {
		t.Fatalf("metric names = %v, want %v", slices.Sorted(maps.Keys(got)), metricNames(want))
	}
	for _, expected := range want {
		metric, ok := got[expected.name]
		if !ok {
			t.Errorf("metric %s is missing", expected.name)
			continue
		}
		if metric.Unit != expected.unit {
			t.Errorf("metric %s unit = %q, want %q", expected.name, metric.Unit, expected.unit)
		}
		points := metricPoints(t, metric)
		sortMetricPoints(points)
		sortMetricPoints(expected.points)
		if !reflect.DeepEqual(points, expected.points) {
			t.Errorf("metric %s points = %#v, want %#v", expected.name, points, expected.points)
		}
	}
}

func metricNames(metrics []expectedMetricSignal) []string {
	names := make([]string, 0, len(metrics))
	for _, metric := range metrics {
		names = append(names, metric.name)
	}
	sort.Strings(names)
	return names
}

func metricPoints(t testing.TB, metric metricdata.Metrics) []expectedMetricPoint {
	t.Helper()
	var points []expectedMetricPoint
	switch data := metric.Data.(type) {
	case metricdata.Histogram[float64]:
		for _, point := range data.DataPoints {
			points = append(points, expectedMetricPoint{attributes: normalizedAttributeSet(point.Attributes), count: point.Count, sum: point.Sum})
		}
	case metricdata.Histogram[int64]:
		for _, point := range data.DataPoints {
			points = append(points, expectedMetricPoint{attributes: normalizedAttributeSet(point.Attributes), count: point.Count, sum: point.Sum})
		}
	default:
		t.Fatalf("metric %s data = %T, want histogram", metric.Name, metric.Data)
	}
	return points
}

func sortMetricPoints(points []expectedMetricPoint) {
	sort.Slice(points, func(i, j int) bool {
		left, _ := json.Marshal(points[i].attributes)
		right, _ := json.Marshal(points[j].attributes)
		return string(left) < string(right)
	})
}

func normalizedAttributeSet(set attribute.Set) map[string]any {
	out := map[string]any{}
	for _, attr := range set.ToSlice() {
		out[string(attr.Key)] = normalizedAttributeValue(attr.Value)
	}
	return out
}

func normalizedAttributeValue(value attribute.Value) any {
	switch value.Type() {
	case attribute.BOOL:
		return value.AsBool()
	case attribute.INT64:
		return value.AsInt64()
	case attribute.FLOAT64:
		return value.AsFloat64()
	case attribute.STRING:
		return value.AsString()
	case attribute.BOOLSLICE:
		return value.AsBoolSlice()
	case attribute.INT64SLICE:
		return value.AsInt64Slice()
	case attribute.FLOAT64SLICE:
		return value.AsFloat64Slice()
	case attribute.STRINGSLICE:
		return value.AsStringSlice()
	case attribute.BYTESLICE:
		return value.AsByteSlice()
	case attribute.SLICE:
		items := value.AsSlice()
		out := make([]any, 0, len(items))
		for _, item := range items {
			out = append(out, normalizedAttributeValue(item))
		}
		return out
	case attribute.MAP:
		out := map[string]any{}
		for _, item := range value.AsMap() {
			out[string(item.Key)] = normalizedAttributeValue(item.Value)
		}
		return out
	default:
		return nil
	}
}

func assertExactLog(t testing.TB, providers *providers, name string, want map[string]any) {
	t.Helper()
	records := providers.logRecorder.Records()
	if len(records) != 1 {
		t.Fatalf("recorded logs = %d, want 1", len(records))
	}
	if records[0].EventName() != name {
		t.Errorf("event name = %q, want %q", records[0].EventName(), name)
	}
	attributes := map[string]any{}
	records[0].WalkAttributes(func(attr attribute.KeyValue) bool {
		attributes[string(attr.Key)] = normalizedAttributeValue(attr.Value)
		return true
	})
	if !reflect.DeepEqual(attributes, want) {
		t.Errorf("event %s attributes = %#v, want %#v", name, attributes, want)
	}
}

func reconcileViolations(violations []weavertest.Violation, expected []expectedViolation) error {
	var problems []string
	for _, violation := range violations {
		matched := false
		for _, allowed := range expected {
			matched = matched || allowed.matches(violation)
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("unexpected [%s] %s", violation.ID, violation.Message))
		}
	}
	for _, allowed := range expected {
		matched := false
		for _, violation := range violations {
			matched = matched || allowed.matches(violation)
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("allowlisted violation was not reported: [%s] %s", allowed.adviceID, allowed.messageContains))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func dumpReport(t *testing.T, name string, report weavertest.Report) {
	t.Helper()
	contents, err := json.MarshalIndent(report.Raw, "", "  ")
	if err != nil {
		t.Errorf("encode Weaver report: %v", err)
		return
	}
	if err := os.MkdirAll("weaver_reports", 0o755); err != nil {
		t.Errorf("create report directory: %v", err)
		return
	}
	filename := strings.ReplaceAll(name, " ", "_") + ".json"
	path := filepath.Join("weaver_reports", filename)
	if err := os.WriteFile(path, append(contents, '\n'), 0o644); err != nil {
		t.Errorf("write Weaver report: %v", err)
		return
	}
	t.Logf("Weaver report: %s", path)
}
