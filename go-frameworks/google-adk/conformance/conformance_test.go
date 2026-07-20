package conformance_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	googleadk "github.com/grafana/agento11y/go-frameworks/google-adk"
	"github.com/grafana/agento11y/go/agento11y"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const (
	spanAttrOperationName  = "gen_ai.operation.name"
	spanAttrConversationID = "gen_ai.conversation.id"
	spanAttrAgentName      = "gen_ai.agent.name"
	spanAttrAgentVersion   = "gen_ai.agent.version"
	spanAttrProviderName   = "gen_ai.provider.name"
	spanAttrRequestModel   = "gen_ai.request.model"
	spanAttrResponseModel  = "gen_ai.response.model"
	spanAttrGenerationID   = "agento11y.generation.id"

	metadataRunID       = "agento11y.framework.run_id"
	metadataThreadID    = "agento11y.framework.thread_id"
	metadataParentRunID = "agento11y.framework.parent_run_id"
	metadataRunType     = "agento11y.framework.run_type"
	metadataComponent   = "agento11y.framework.component_name"
	metadataEventID     = "agento11y.framework.event_id"
	metadataTags        = "agento11y.framework.tags"
	metadataRetry       = "agento11y.framework.retry_attempt"
)

type conformanceEnv struct {
	client         *agento11y.Client
	capture        *generationCaptureServer
	spans          *tracetest.SpanRecorder
	tracerProvider *sdktrace.TracerProvider
}

func TestConformance_RunLifecycleExportsFrameworkTelemetry(t *testing.T) {
	env := newConformanceEnv(t)

	adapter := googleadk.NewAgento11yAdapter(env.client, googleadk.Options{
		AgentName:    "triage-agent",
		AgentVersion: "1.2.3",
		ExtraTags: map[string]string{
			"deployment.environment": "staging",
		},
		ExtraMetadata: map[string]any{
			"team": "infra",
		},
	})

	parentCtx, parentSpan := env.tracerProvider.Tracer("google-adk-conformance").Start(context.Background(), "http.request")
	parentSpanContext := parentSpan.SpanContext()
	retryAttempt := 2

	if err := adapter.OnRunStart(parentCtx, googleadk.RunStartEvent{
		RunID:         "run-sync",
		ParentRunID:   "framework-parent",
		SessionID:     "session-42",
		ThreadID:      "thread-7",
		EventID:       "event-9",
		ComponentName: "planner",
		RunType:       "chat",
		ModelName:     "gemini-2.5-pro",
		Prompts:       []string{"Summarize system health"},
		Tags:          []string{"prod", "workflow"},
		RetryAttempt:  &retryAttempt,
		Metadata: map[string]any{
			"step": "triage",
		},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}

	if got := env.capture.requestCount(); got != 0 {
		t.Fatalf("expected no normalized generation export before run end, got %d requests", got)
	}

	if err := adapter.OnRunEnd("run-sync", googleadk.RunEndEvent{
		RunID:          "run-sync",
		OutputMessages: []agento11y.Message{agento11y.AssistantTextMessage("System health is green")},
		ResponseModel:  "gemini-2.5-pro",
		StopReason:     "stop",
		Usage: agento11y.TokenUsage{
			InputTokens:  12,
			OutputTokens: 4,
			TotalTokens:  16,
		},
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	generation := env.capture.waitForSingleGeneration(t)
	parentSpan.End()
	env.Shutdown(t)

	span := findSpanByName(t, env.spans.Ended(), "generateText gemini-2.5-pro")
	if span.Parent().SpanID() != parentSpanContext.SpanID() {
		t.Fatalf("expected generation span parent %q, got %q", parentSpanContext.SpanID().String(), span.Parent().SpanID().String())
	}

	attrs := spanAttributeMap(span)
	requireStringAttr(t, attrs, spanAttrOperationName, "generateText")
	requireStringAttr(t, attrs, spanAttrConversationID, "session-42")
	requireStringAttr(t, attrs, spanAttrAgentName, "triage-agent")
	requireStringAttr(t, attrs, spanAttrAgentVersion, "1.2.3")
	requireStringAttr(t, attrs, spanAttrProviderName, "gemini")
	requireStringAttr(t, attrs, spanAttrRequestModel, "gemini-2.5-pro")
	requireStringAttr(t, attrs, spanAttrResponseModel, "gemini-2.5-pro")
	requireStringAttr(t, attrs, spanAttrGenerationID, mustString(t, generation, "id"))

	requireStringField(t, generation, "conversation_id", "session-42")
	requireStringField(t, generation, "agent_name", "triage-agent")
	requireStringField(t, generation, "agent_version", "1.2.3")
	requireStringField(t, generation, "operation_name", "generateText")
	requireStringField(t, generation, "mode", "GENERATION_MODE_SYNC")
	requireStringField(t, generation, "response_model", "gemini-2.5-pro")
	requireStringField(t, generation, "stop_reason", "stop")
	requireStringField(t, generation, "trace_id", span.SpanContext().TraceID().String())
	requireStringField(t, generation, "span_id", span.SpanContext().SpanID().String())

	model := mustObject(t, generation, "model")
	requireStringFromMap(t, model, "provider", "gemini")
	requireStringFromMap(t, model, "name", "gemini-2.5-pro")

	tags := mustObject(t, generation, "tags")
	requireStringFromMap(t, tags, "agento11y.framework.name", "google-adk")
	requireStringFromMap(t, tags, "agento11y.framework.source", "handler")
	requireStringFromMap(t, tags, "agento11y.framework.language", "go")
	requireStringFromMap(t, tags, "deployment.environment", "staging")

	metadata := mustObject(t, generation, "metadata")
	requireStringFromMap(t, metadata, metadataRunID, "run-sync")
	requireStringFromMap(t, metadata, metadataThreadID, "thread-7")
	requireStringFromMap(t, metadata, metadataParentRunID, "framework-parent")
	requireStringFromMap(t, metadata, metadataRunType, "chat")
	requireStringFromMap(t, metadata, metadataComponent, "planner")
	requireStringFromMap(t, metadata, metadataEventID, "event-9")
	requireStringFromMap(t, metadata, "team", "infra")
	requireStringFromMap(t, metadata, "step", "triage")
	requireNumberFromMap(t, metadata, metadataRetry, 2)
	requireStringSliceFromMap(t, metadata, metadataTags, []string{"prod", "workflow"})
}

func TestConformance_StreamingRunExportsTokenDrivenGeneration(t *testing.T) {
	env := newConformanceEnv(t)

	adapter := googleadk.NewAgento11yAdapter(env.client, googleadk.Options{
		AgentName:    "stream-agent",
		AgentVersion: "9.9.9",
	})

	parentCtx, parentSpan := env.tracerProvider.Tracer("google-adk-conformance").Start(context.Background(), "grpc.request")
	parentSpanContext := parentSpan.SpanContext()

	if err := adapter.OnRunStart(parentCtx, googleadk.RunStartEvent{
		RunID:          "run-stream",
		ConversationID: "conversation-stream",
		ThreadID:       "thread-stream",
		ModelName:      "gpt-5",
		Stream:         true,
		Tags:           []string{"streaming"},
	}); err != nil {
		t.Fatalf("run start: %v", err)
	}

	adapter.OnRunToken("run-stream", "hello")
	adapter.OnRunToken("run-stream", " world")

	if err := adapter.OnRunEnd("run-stream", googleadk.RunEndEvent{
		RunID:         "run-stream",
		ResponseModel: "gpt-5",
		StopReason:    "end_turn",
	}); err != nil {
		t.Fatalf("run end: %v", err)
	}

	generation := env.capture.waitForSingleGeneration(t)
	parentSpan.End()
	env.Shutdown(t)

	span := findSpanByName(t, env.spans.Ended(), "streamText gpt-5")
	if span.Parent().SpanID() != parentSpanContext.SpanID() {
		t.Fatalf("expected streaming span parent %q, got %q", parentSpanContext.SpanID().String(), span.Parent().SpanID().String())
	}

	attrs := spanAttributeMap(span)
	requireStringAttr(t, attrs, spanAttrOperationName, "streamText")
	requireStringAttr(t, attrs, spanAttrConversationID, "conversation-stream")
	requireStringAttr(t, attrs, spanAttrAgentName, "stream-agent")
	requireStringAttr(t, attrs, spanAttrAgentVersion, "9.9.9")
	requireStringAttr(t, attrs, spanAttrProviderName, "openai")
	requireStringAttr(t, attrs, spanAttrRequestModel, "gpt-5")
	requireStringAttr(t, attrs, spanAttrResponseModel, "gpt-5")

	requireStringField(t, generation, "conversation_id", "conversation-stream")
	requireStringField(t, generation, "mode", "GENERATION_MODE_STREAM")
	requireStringField(t, generation, "operation_name", "streamText")
	requireStringField(t, generation, "response_model", "gpt-5")
	requireStringField(t, generation, "stop_reason", "end_turn")
	requireStringField(t, generation, "trace_id", span.SpanContext().TraceID().String())
	requireStringField(t, generation, "span_id", span.SpanContext().SpanID().String())

	tags := mustObject(t, generation, "tags")
	requireStringFromMap(t, tags, "agento11y.framework.name", "google-adk")
	requireStringFromMap(t, tags, "agento11y.framework.source", "handler")
	requireStringFromMap(t, tags, "agento11y.framework.language", "go")

	metadata := mustObject(t, generation, "metadata")
	requireStringFromMap(t, metadata, metadataRunID, "run-stream")
	requireStringFromMap(t, metadata, metadataThreadID, "thread-stream")
	requireStringFromMap(t, metadata, metadataRunType, "chat")
	requireStringSliceFromMap(t, metadata, metadataTags, []string{"streaming"})

	output := mustArray(t, generation, "output")
	if len(output) != 1 {
		t.Fatalf("expected one streamed output message, got %d", len(output))
	}
	outputMessage, ok := output[0].(map[string]any)
	if !ok {
		t.Fatalf("expected output message object, got %T", output[0])
	}
	requireStringFromMap(t, outputMessage, "role", "MESSAGE_ROLE_ASSISTANT")
	parts := mustArrayFromMap(t, outputMessage, "parts")
	if len(parts) != 1 {
		t.Fatalf("expected one output part, got %d", len(parts))
	}
	outputPart, ok := parts[0].(map[string]any)
	if !ok {
		t.Fatalf("expected output part object, got %T", parts[0])
	}
	requireStringFromMap(t, outputPart, "text", "hello world")
}

func newConformanceEnv(t *testing.T) *conformanceEnv {
	t.Helper()

	capture := newGenerationCaptureServer(t)
	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))

	cfg := agento11y.DefaultConfig()
	cfg.Tracer = tracerProvider.Tracer("google-adk-conformance")
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = capture.server.URL + "/api/v1/generations:export"
	cfg.GenerationExport.BatchSize = 1
	cfg.GenerationExport.QueueSize = 8
	cfg.GenerationExport.FlushInterval = time.Hour
	cfg.GenerationExport.MaxRetries = 1
	cfg.GenerationExport.InitialBackoff = time.Millisecond
	cfg.GenerationExport.MaxBackoff = 10 * time.Millisecond

	env := &conformanceEnv{
		client:         agento11y.NewClient(cfg),
		capture:        capture,
		spans:          spanRecorder,
		tracerProvider: tracerProvider,
	}
	t.Cleanup(func() {
		_ = env.close()
	})
	return env
}

func (e *conformanceEnv) Shutdown(t *testing.T) {
	t.Helper()

	if err := e.close(); err != nil {
		t.Fatalf("shutdown conformance env: %v", err)
	}
}

func (e *conformanceEnv) close() error {
	var closeErr error

	if e.client != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.client.Shutdown(ctx); err != nil {
			closeErr = err
		}
		e.client = nil
	}
	if e.tracerProvider != nil {
		if err := e.tracerProvider.Shutdown(context.Background()); err != nil && closeErr == nil {
			closeErr = err
		}
		e.tracerProvider = nil
	}
	if e.capture != nil {
		e.capture.server.Close()
		e.capture = nil
	}

	return closeErr
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

func (c *generationCaptureServer) requestCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.requests)
}

func (c *generationCaptureServer) waitForSingleGeneration(t *testing.T) map[string]any {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		if len(c.requests) == 1 {
			request := c.requests[0]
			c.mu.Unlock()
			generations := mustArray(t, request, "generations")
			if len(generations) != 1 {
				t.Fatalf("expected one exported generation, got %d", len(generations))
			}
			generation, ok := generations[0].(map[string]any)
			if !ok {
				t.Fatalf("expected generation object, got %T", generations[0])
			}
			return generation
		}
		c.mu.Unlock()

		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for a single generation export; got %d requests", c.requestCount())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func findSpanByName(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for _, span := range spans {
		if span.Name() == name {
			return span
		}
	}

	t.Fatalf("expected span %q, got %d spans", name, len(spans))
	return nil
}

func spanAttributeMap(span sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	attrs := make(map[attribute.Key]attribute.Value, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attrs[attr.Key] = attr.Value
	}
	return attrs
}

func requireStringAttr(t *testing.T, attrs map[attribute.Key]attribute.Value, key, want string) {
	t.Helper()

	got, ok := attrs[attribute.Key(key)]
	if !ok {
		t.Fatalf("expected span attribute %q", key)
	}
	if got.AsString() != want {
		t.Fatalf("unexpected span attribute %q: got %q want %q", key, got.AsString(), want)
	}
}

func requireStringField(t *testing.T, data map[string]any, key, want string) {
	t.Helper()
	requireStringFromMap(t, data, key, want)
}

func requireStringFromMap(t *testing.T, data map[string]any, key, want string) {
	t.Helper()

	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	gotString, ok := got.(string)
	if !ok {
		t.Fatalf("expected %q to be a string, got %T", key, got)
	}
	if gotString != want {
		t.Fatalf("unexpected %q: got %q want %q", key, gotString, want)
	}
}

func requireNumberFromMap(t *testing.T, data map[string]any, key string, want float64) {
	t.Helper()

	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	gotNumber, ok := got.(float64)
	if !ok {
		t.Fatalf("expected %q to be numeric, got %T", key, got)
	}
	if gotNumber != want {
		t.Fatalf("unexpected %q: got %v want %v", key, gotNumber, want)
	}
}

func requireStringSliceFromMap(t *testing.T, data map[string]any, key string, want []string) {
	t.Helper()

	raw, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	values, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected %q to be an array, got %T", key, raw)
	}
	if len(values) != len(want) {
		t.Fatalf("unexpected %q length: got %d want %d", key, len(values), len(want))
	}
	for i, expected := range want {
		got, ok := values[i].(string)
		if !ok {
			t.Fatalf("expected %q[%d] to be string, got %T", key, i, values[i])
		}
		if got != expected {
			t.Fatalf("unexpected %q[%d]: got %q want %q", key, i, got, expected)
		}
	}
}

func mustString(t *testing.T, data map[string]any, key string) string {
	t.Helper()

	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	value, ok := got.(string)
	if !ok {
		t.Fatalf("expected %q to be string, got %T", key, got)
	}
	return value
}

func mustObject(t *testing.T, data map[string]any, key string) map[string]any {
	t.Helper()

	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	value, ok := got.(map[string]any)
	if !ok {
		t.Fatalf("expected %q to be an object, got %T", key, got)
	}
	return value
}

func mustArray(t *testing.T, data map[string]any, key string) []any {
	t.Helper()

	got, ok := data[key]
	if !ok {
		t.Fatalf("expected field %q", key)
	}
	value, ok := got.([]any)
	if !ok {
		t.Fatalf("expected %q to be an array, got %T", key, got)
	}
	return value
}

func mustArrayFromMap(t *testing.T, data map[string]any, key string) []any {
	t.Helper()
	return mustArray(t, data, key)
}
