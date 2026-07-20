package googleadk_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	googleadk "github.com/grafana/agento11y/go-frameworks/google-adk"
	"github.com/grafana/agento11y/go/agento11y"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	metricOperationDuration = "gen_ai.client.operation.duration"
	metricTimeToFirstToken  = "gen_ai.client.time_to_first_token"
	metricToolCallsPerOp    = "gen_ai.client.tool_calls_per_operation"
	spanAttrOperationName   = "gen_ai.operation.name"
	spanAttrConversationID  = "gen_ai.conversation.id"
	spanAttrAgentName       = "gen_ai.agent.name"
	spanAttrAgentVersion    = "gen_ai.agent.version"
	spanAttrProviderName    = "gen_ai.provider.name"
	spanAttrRequestModel    = "gen_ai.request.model"
	spanAttrResponseModel   = "gen_ai.response.model"
	spanAttrToolName        = "gen_ai.tool.name"
	spanAttrToolCallID      = "gen_ai.tool.call.id"
	spanAttrToolType        = "gen_ai.tool.type"
)

func TestConformance_RunLifecyclePropagatesFrameworkMetadataAndLinksSpans(t *testing.T) {
	env := newConformanceEnv(t, googleadk.Options{
		AgentName:    "planner",
		AgentVersion: "2026.03.12",
		ExtraTags: map[string]string{
			"deployment.environment": "test",
		},
		ExtraMetadata: map[string]any{
			"team": "infra",
		},
	})

	retryAttempt := 2
	parentCtx, parent := env.Tracer.Start(context.Background(), "google-adk.run",
		trace.WithAttributes(
			attribute.String("agento11y.framework.name", "google-adk"),
			attribute.String("agento11y.framework.source", "handler"),
			attribute.String("agento11y.framework.language", "go"),
		),
	)

	if err := env.Callbacks.OnRunStart(parentCtx, googleadk.RunStartEvent{
		RunID:          "run-sync",
		ParentRunID:    "parent-run",
		SessionID:      "session-42",
		ThreadID:       "thread-7",
		EventID:        "event-42",
		ComponentName:  "planner",
		RunType:        "chat",
		RetryAttempt:   &retryAttempt,
		ModelName:      "gpt-5",
		Prompts:        []string{"Summarize release status"},
		Tags:           []string{"prod", "framework", "prod"},
		Metadata:       map[string]any{"event_payload": map[string]any{"step": "validate"}},
		InputMessages:  []agento11y.Message{agento11y.UserTextMessage("Summarize release status")},
		ConversationID: "",
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}

	if err := env.Callbacks.OnRunEnd("run-sync", googleadk.RunEndEvent{
		RunID:          "run-sync",
		OutputMessages: []agento11y.Message{agento11y.AssistantTextMessage("Release is healthy")},
		ResponseModel:  "gpt-5",
		StopReason:     "stop",
		Usage: agento11y.TokenUsage{
			InputTokens:  6,
			OutputTokens: 4,
			TotalTokens:  10,
		},
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	parentSpanContext := parent.SpanContext()
	parent.End()

	metrics := env.CollectMetrics(t)
	if len(findHistogram[float64](t, metrics, metricOperationDuration).DataPoints) == 0 {
		t.Fatalf("expected %s datapoints for sync google-adk conformance", metricOperationDuration)
	}
	requireNoHistogram(t, metrics, metricTimeToFirstToken)

	env.Shutdown(t)

	parentSpan := findSpanByName(t, env.Spans.Ended(), "google-adk.run")
	parentAttrs := spanAttrs(parentSpan)
	requireSpanAttr(t, parentAttrs, "agento11y.framework.name", "google-adk")
	requireSpanAttr(t, parentAttrs, "agento11y.framework.source", "handler")
	requireSpanAttr(t, parentAttrs, "agento11y.framework.language", "go")

	generationSpan := findSpanByOperationName(t, env.Spans.Ended(), "generateText")
	if generationSpan.Parent().SpanID() != parentSpanContext.SpanID() {
		t.Fatalf("expected generation span parent %q, got %q", parentSpanContext.SpanID().String(), generationSpan.Parent().SpanID().String())
	}

	generationAttrs := spanAttrs(generationSpan)
	requireSpanAttr(t, generationAttrs, spanAttrOperationName, "generateText")
	requireSpanAttr(t, generationAttrs, spanAttrConversationID, "session-42")
	requireSpanAttr(t, generationAttrs, spanAttrAgentName, "planner")
	requireSpanAttr(t, generationAttrs, spanAttrAgentVersion, "2026.03.12")
	requireSpanAttr(t, generationAttrs, spanAttrProviderName, "openai")
	requireSpanAttr(t, generationAttrs, spanAttrRequestModel, "gpt-5")
	requireSpanAttr(t, generationAttrs, spanAttrResponseModel, "gpt-5")

	generation := env.Export.SingleGeneration(t)
	if got := stringValue(t, generation, "conversation_id"); got != "session-42" {
		t.Fatalf("unexpected conversation_id: got %q want %q", got, "session-42")
	}
	if got := stringValue(t, generation, "trace_id"); got != generationSpan.SpanContext().TraceID().String() {
		t.Fatalf("unexpected trace_id: got %q want %q", got, generationSpan.SpanContext().TraceID().String())
	}
	if got := stringValue(t, generation, "span_id"); got != generationSpan.SpanContext().SpanID().String() {
		t.Fatalf("unexpected span_id: got %q want %q", got, generationSpan.SpanContext().SpanID().String())
	}

	tags := objectValue(t, generation, "tags")
	requireStringField(t, tags, "deployment.environment", "test")
	requireStringField(t, tags, "agento11y.framework.name", "google-adk")
	requireStringField(t, tags, "agento11y.framework.source", "handler")
	requireStringField(t, tags, "agento11y.framework.language", "go")

	metadata := objectValue(t, generation, "metadata")
	requireStringField(t, metadata, "team", "infra")
	requireStringField(t, metadata, "agento11y.framework.run_id", "run-sync")
	requireStringField(t, metadata, "agento11y.framework.thread_id", "thread-7")
	requireStringField(t, metadata, "agento11y.framework.parent_run_id", "parent-run")
	requireStringField(t, metadata, "agento11y.framework.component_name", "planner")
	requireStringField(t, metadata, "agento11y.framework.run_type", "chat")
	requireNumberField(t, metadata, "agento11y.framework.retry_attempt", 2)
	requireStringField(t, metadata, "agento11y.framework.event_id", "event-42")
	requireStringSliceField(t, metadata, "agento11y.framework.tags", []string{"prod", "framework"})

	eventPayload := objectValue(t, metadata, "event_payload")
	requireStringField(t, eventPayload, "step", "validate")
}

func TestConformance_StreamingRunTriggersGenerationExport(t *testing.T) {
	env := newConformanceEnv(t, googleadk.Options{
		AgentName:    "planner",
		AgentVersion: "2026.03.12",
	})

	if err := env.Callbacks.OnRunStart(context.Background(), googleadk.RunStartEvent{
		RunID:     "run-stream",
		SessionID: "session-stream",
		ModelName: "gemini-2.5-pro",
		RunType:   "chat",
		Stream:    true,
		Prompts:   []string{"Stream migration status"},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}
	env.Callbacks.OnRunToken("run-stream", "step ")
	env.Callbacks.OnRunToken("run-stream", "complete")

	if err := env.Callbacks.OnRunEnd("run-stream", googleadk.RunEndEvent{
		RunID:         "run-stream",
		ResponseModel: "gemini-2.5-pro",
		Usage: agento11y.TokenUsage{
			InputTokens:  3,
			OutputTokens: 2,
			TotalTokens:  5,
		},
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	metrics := env.CollectMetrics(t)
	if len(findHistogram[float64](t, metrics, metricOperationDuration).DataPoints) == 0 {
		t.Fatalf("expected %s datapoints for streaming google-adk conformance", metricOperationDuration)
	}
	if len(findHistogram[float64](t, metrics, metricTimeToFirstToken).DataPoints) == 0 {
		t.Fatalf("expected %s datapoints for streaming google-adk conformance", metricTimeToFirstToken)
	}

	env.Shutdown(t)

	generationSpan := findSpanByOperationName(t, env.Spans.Ended(), "streamText")
	requireSpanAttr(t, spanAttrs(generationSpan), spanAttrRequestModel, "gemini-2.5-pro")

	generation := env.Export.SingleGeneration(t)
	if got := stringValue(t, generation, "operation_name"); got != "streamText" {
		t.Fatalf("unexpected operation_name: got %q want %q", got, "streamText")
	}

	output := arrayValue(t, generation, "output")
	if len(output) != 1 {
		t.Fatalf("expected one streamed output message, got %d", len(output))
	}
	message := asObject(t, output[0], "output[0]")
	parts := arrayValue(t, message, "parts")
	if len(parts) != 1 {
		t.Fatalf("expected one streamed output part, got %d", len(parts))
	}
	part := asObject(t, parts[0], "output[0].parts[0]")
	requireStringField(t, part, "text", "step complete")

	metadata := objectValue(t, generation, "metadata")
	requireStringField(t, metadata, "agento11y.framework.run_id", "run-stream")
	requireStringField(t, metadata, "agento11y.framework.run_type", "chat")
}

func TestConformance_EmbeddingSupportStatus(t *testing.T) {
	err := googleadk.CheckEmbeddingsSupport()
	if err == nil {
		t.Fatalf("expected Google ADK embeddings to remain unsupported")
	}
	if !errors.Is(err, googleadk.ErrEmbeddingsUnsupported) {
		t.Fatalf("expected ErrEmbeddingsUnsupported, got %v", err)
	}
}

func TestConformance_ToolCallOutputsAndToolLifecycleStayObservable(t *testing.T) {
	env := newConformanceEnv(t, googleadk.Options{
		AgentName:    "planner",
		AgentVersion: "2026.03.12",
	})

	if err := env.Callbacks.OnRunStart(context.Background(), googleadk.RunStartEvent{
		RunID:     "run-tool-call",
		SessionID: "session-tool-call",
		ModelName: "gpt-5",
		RunType:   "chat",
		Prompts:   []string{"Look up the weather in Paris"},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}

	if err := env.Callbacks.OnRunEnd("run-tool-call", googleadk.RunEndEvent{
		RunID: "run-tool-call",
		OutputMessages: []agento11y.Message{
			{
				Role: agento11y.RoleAssistant,
				Name: "assistant",
				Parts: []agento11y.Part{
					agento11y.TextPart("Calling weather lookup."),
					agento11y.ToolCallPart(agento11y.ToolCall{
						ID:        "call-weather",
						Name:      "weather.lookup",
						InputJSON: []byte(`{"city":"Paris"}`),
					}),
				},
			},
			{
				Role: agento11y.RoleTool,
				Name: "weather.lookup",
				Parts: []agento11y.Part{
					agento11y.ToolResultPart(agento11y.ToolResult{
						ToolCallID:  "call-weather",
						Name:        "weather.lookup",
						Content:     "18C",
						ContentJSON: []byte(`{"temp_c":18}`),
					}),
				},
			},
		},
		ResponseModel: "gpt-5",
		StopReason:    "tool_calls",
		Usage: agento11y.TokenUsage{
			InputTokens:  8,
			OutputTokens: 3,
			TotalTokens:  11,
		},
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	if err := env.Callbacks.OnToolStart(context.Background(), googleadk.ToolStartEvent{
		RunID:           "tool-call-span",
		SessionID:       "session-tool-call",
		ToolCallID:      "call-weather",
		ToolName:        "weather.lookup",
		ToolType:        "function",
		ToolDescription: "Look up weather",
		Arguments:       map[string]any{"city": "Paris"},
	}); err != nil {
		t.Fatalf("tool start: %v", err)
	}
	if err := env.Callbacks.OnToolEnd("tool-call-span", googleadk.ToolEndEvent{
		Result:      map[string]any{"temp_c": 18},
		CompletedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("tool end: %v", err)
	}

	metrics := env.CollectMetrics(t)
	toolCalls := findHistogram[int64](t, metrics, metricToolCallsPerOp)
	if len(toolCalls.DataPoints) != 1 {
		t.Fatalf("expected one %s datapoint, got %d", metricToolCallsPerOp, len(toolCalls.DataPoints))
	}
	if toolCalls.DataPoints[0].Sum != 1 {
		t.Fatalf("expected %s sum=1, got %d", metricToolCallsPerOp, toolCalls.DataPoints[0].Sum)
	}

	env.Shutdown(t)

	generation := env.Export.SingleGeneration(t)
	output := arrayValue(t, generation, "output")
	if len(output) != 2 {
		t.Fatalf("expected assistant tool call plus tool result output messages, got %d", len(output))
	}

	assistant := asObject(t, output[0], "output[0]")
	assistantParts := arrayValue(t, assistant, "parts")
	if len(assistantParts) != 2 {
		t.Fatalf("expected assistant text + tool call parts, got %d", len(assistantParts))
	}
	toolCallPart := asObject(t, assistantParts[1], "output[0].parts[1]")
	toolCall := objectValue(t, toolCallPart, "tool_call")
	requireStringField(t, toolCall, "id", "call-weather")
	requireStringField(t, toolCall, "name", "weather.lookup")
	requireStringField(t, toolCall, "input_json", "eyJjaXR5IjoiUGFyaXMifQ==")

	toolMessage := asObject(t, output[1], "output[1]")
	toolParts := arrayValue(t, toolMessage, "parts")
	if len(toolParts) != 1 {
		t.Fatalf("expected one tool result part, got %d", len(toolParts))
	}
	toolResultPart := asObject(t, toolParts[0], "output[1].parts[0]")
	toolResult := objectValue(t, toolResultPart, "tool_result")
	requireStringField(t, toolResult, "tool_call_id", "call-weather")
	requireStringField(t, toolResult, "name", "weather.lookup")

	toolSpan := findSpanByOperationName(t, env.Spans.Ended(), "execute_tool")
	toolAttrs := spanAttrs(toolSpan)
	requireSpanAttr(t, toolAttrs, spanAttrToolName, "weather.lookup")
	requireSpanAttr(t, toolAttrs, spanAttrToolCallID, "call-weather")
	requireSpanAttr(t, toolAttrs, spanAttrToolType, "function")
	requireSpanAttr(t, toolAttrs, spanAttrConversationID, "session-tool-call")
}

type conformanceEnv struct {
	Client    *agento11y.Client
	Callbacks googleadk.Callbacks
	Export    *generationCaptureServer
	Spans     *tracetest.SpanRecorder
	Metrics   *sdkmetric.ManualReader
	Tracer    trace.Tracer

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
}

func newConformanceEnv(t *testing.T, opts googleadk.Options) *conformanceEnv {
	t.Helper()

	export := newGenerationCaptureServer(t)
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))

	cfg := agento11y.DefaultConfig()
	cfg.Tracer = tracerProvider.Tracer("google-adk-conformance-test")
	cfg.Meter = meterProvider.Meter("google-adk-conformance-test")
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = export.server.URL + "/api/v1/generations:export"
	cfg.GenerationExport.BatchSize = 1
	cfg.GenerationExport.QueueSize = 8
	cfg.GenerationExport.FlushInterval = time.Hour
	cfg.GenerationExport.MaxRetries = 1
	cfg.GenerationExport.InitialBackoff = time.Millisecond
	cfg.GenerationExport.MaxBackoff = 5 * time.Millisecond

	client := agento11y.NewClient(cfg)
	env := &conformanceEnv{
		Client:         client,
		Callbacks:      googleadk.NewCallbacks(client, opts),
		Export:         export,
		Spans:          spanRecorder,
		Metrics:        metricReader,
		Tracer:         tracerProvider.Tracer("google-adk-framework-test"),
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
	}
	t.Cleanup(func() {
		_ = env.close()
	})
	return env
}

func (e *conformanceEnv) Shutdown(t *testing.T) {
	t.Helper()
	if e == nil || e.Client == nil {
		return
	}
	client := e.Client
	e.Client = nil

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown client: %v", err)
	}
}

func (e *conformanceEnv) close() error {
	if e == nil {
		return nil
	}

	var closeErr error
	if e.Client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.Client.Shutdown(ctx); err != nil {
			closeErr = err
		}
		e.Client = nil
	}
	if e.meterProvider != nil {
		if err := e.meterProvider.Shutdown(context.Background()); err != nil && closeErr == nil {
			closeErr = err
		}
		e.meterProvider = nil
	}
	if e.tracerProvider != nil {
		if err := e.tracerProvider.Shutdown(context.Background()); err != nil && closeErr == nil {
			closeErr = err
		}
		e.tracerProvider = nil
	}
	if e.Export != nil && e.Export.server != nil {
		e.Export.server.Close()
		e.Export.server = nil
	}
	return closeErr
}

func (e *conformanceEnv) CollectMetrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()
	var collected metricdata.ResourceMetrics
	if err := e.Metrics.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return collected
}

type generationCaptureServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []map[string]any
}

func newGenerationCaptureServer(t *testing.T) *generationCaptureServer {
	t.Helper()
	capture := &generationCaptureServer{}
	capture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		request := map[string]any{}
		if err := json.Unmarshal(body, &request); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}

		capture.mu.Lock()
		capture.requests = append(capture.requests, request)
		capture.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(acceptanceResponse(request))
	}))
	return capture
}

func acceptanceResponse(request map[string]any) map[string]any {
	generations, _ := request["generations"].([]any)
	results := make([]map[string]any, 0, len(generations))
	for _, raw := range generations {
		generation, ok := raw.(map[string]any)
		if !ok {
			results = append(results, map[string]any{"accepted": true})
			continue
		}
		id, _ := generation["id"].(string)
		results = append(results, map[string]any{
			"generation_id": id,
			"accepted":      true,
		})
	}
	return map[string]any{"results": results}
}

func (c *generationCaptureServer) SingleGeneration(t *testing.T) map[string]any {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.requests) != 1 {
		t.Fatalf("expected exactly one export request, got %d", len(c.requests))
	}

	generations := arrayValue(t, c.requests[0], "generations")
	if len(generations) != 1 {
		t.Fatalf("expected exactly one exported generation, got %d", len(generations))
	}

	return asObject(t, generations[0], "generations[0]")
}

func findSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func findSpanByOperationName(t *testing.T, spans []sdktrace.ReadOnlySpan, operation string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, span := range spans {
		if spanAttr := attrValue(span.Attributes(), spanAttrOperationName); spanAttr.Type() == attribute.STRING && spanAttr.AsString() == operation {
			return span
		}
	}
	t.Fatalf("span with %s=%q not found", spanAttrOperationName, operation)
	return nil
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	attrs := make(map[string]attribute.Value, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attrs[string(attr.Key)] = attr.Value
	}
	return attrs
}

func requireSpanAttr(t *testing.T, attrs map[string]attribute.Value, key string, want string) {
	t.Helper()
	value, ok := attrs[key]
	if !ok {
		t.Fatalf("missing span attr %q", key)
	}
	if value.Type() != attribute.STRING {
		t.Fatalf("span attr %q has type %v, want string", key, value.Type())
	}
	if got := value.AsString(); got != want {
		t.Fatalf("unexpected span attr %q: got %q want %q", key, got, want)
	}
}

func attrValue(attrs []attribute.KeyValue, key string) attribute.Value {
	for _, attr := range attrs {
		if string(attr.Key) == key {
			return attr.Value
		}
	}
	return attribute.Value{}
}

func stringValue(t *testing.T, object map[string]any, key string) string {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q", key)
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("expected %q to be string, got %T", key, value)
	}
	return text
}

func objectValue(t *testing.T, object map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q", key)
	}
	return asObject(t, value, key)
}

func arrayValue(t *testing.T, object map[string]any, key string) []any {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q", key)
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("expected %q to be array, got %T", key, value)
	}
	return items
}

func asObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected %s to be object, got %T", label, value)
	}
	return object
}

func requireStringField(t *testing.T, object map[string]any, key string, want string) {
	t.Helper()
	if got := stringValue(t, object, key); got != want {
		t.Fatalf("unexpected %q: got %q want %q", key, got, want)
	}
}

func requireNumberField(t *testing.T, object map[string]any, key string, want float64) {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q", key)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("expected %q to be number, got %T", key, value)
	}
	if number != want {
		t.Fatalf("unexpected %q: got %v want %v", key, number, want)
	}
}

func requireStringSliceField(t *testing.T, object map[string]any, key string, want []string) {
	t.Helper()
	value, ok := object[key]
	if !ok {
		t.Fatalf("missing %q", key)
	}
	items, ok := value.([]any)
	if !ok {
		t.Fatalf("expected %q to be string array, got %T", key, value)
	}
	if len(items) != len(want) {
		t.Fatalf("unexpected %q length: got %d want %d", key, len(items), len(want))
	}
	for i := range want {
		text, ok := items[i].(string)
		if !ok {
			t.Fatalf("expected %q[%d] to be string, got %T", key, i, items[i])
		}
		if text != want[i] {
			t.Fatalf("unexpected %q[%d]: got %q want %q", key, i, text, want[i])
		}
	}
}

func findHistogram[N int64 | float64](t *testing.T, metrics metricdata.ResourceMetrics, name string) metricdata.Histogram[N] {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[N])
			if !ok {
				t.Fatalf("metric %q did not contain expected histogram data", name)
			}
			return histogram
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Histogram[N]{}
}

func requireNoHistogram(t *testing.T, metrics metricdata.ResourceMetrics, name string) {
	t.Helper()
	for _, scopeMetrics := range metrics.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name == name {
				t.Fatalf("did not expect metric %q", name)
			}
		}
	}
}
