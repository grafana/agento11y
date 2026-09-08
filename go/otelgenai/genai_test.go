package otelgenai_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log/logtest"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/otelgenai"
)

var (
	startedAt   = time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	completedAt = startedAt.Add(1250 * time.Millisecond)
)

func newRecordingHandler(t *testing.T, opts ...otelgenai.Option) (*otelgenai.Handler, *tracetest.SpanRecorder) {
	t.Helper()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	handler := otelgenai.NewHandler(append([]otelgenai.Option{
		otelgenai.WithTracerProvider(provider),
	}, opts...)...)
	return handler, recorder
}

type failingHistogramProvider struct {
	metric.MeterProvider
	meter metric.Meter
}

func (p failingHistogramProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return p.meter
}

type failingHistogramMeter struct {
	metric.Meter
	err   error
	calls int
}

func (m *failingHistogramMeter) Float64Histogram(name string, opts ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	m.calls++
	instrument, _ := m.Meter.Float64Histogram(name, opts...)
	return instrument, m.err
}

func (m *failingHistogramMeter) Int64Histogram(name string, opts ...metric.Int64HistogramOption) (metric.Int64Histogram, error) {
	m.calls++
	instrument, _ := m.Meter.Int64Histogram(name, opts...)
	return instrument, m.err
}

func TestNewHandlerReportsInstrumentErrorsAndRecordsSpans(t *testing.T) {
	instrumentErr := errors.New("histogram constructor failed")
	noopProvider := metricnoop.NewMeterProvider()
	failingMeter := &failingHistogramMeter{
		Meter: noopProvider.Meter("test"),
		err:   instrumentErr,
	}
	meterProvider := failingHistogramProvider{
		MeterProvider: noopProvider,
		meter:         failingMeter,
	}

	testOTelErrors.reset()

	handler, recorder := newRecordingHandler(t, otelgenai.WithMeterProvider(meterProvider))
	if handler == nil {
		t.Fatal("NewHandler returned nil")
	}
	if failingMeter.calls != 4 {
		t.Errorf("histogram constructor calls = %d, want 4", failingMeter.calls)
	}
	reported := testOTelErrors.read()
	if len(reported) != 1 {
		t.Fatalf("error handler calls = %d, want 1", len(reported))
	}
	if !errors.Is(reported[0], instrumentErr) {
		t.Fatalf("reported error = %v, want wrapped %v", reported[0], instrumentErr)
	}

	inv := chatInvocation()
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)
	if got := len(recorder.Ended()); got != 1 {
		t.Fatalf("recorded %d spans, want 1", got)
	}
}

func TestInvalidInvocationCaptureReportsAndFallsBack(t *testing.T) {
	testOTelErrors.reset()

	handler, recorder := newRecordingHandler(t, otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly))
	inv := chatInvocation()
	inv.Capture = "bogus"
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	reported := testOTelErrors.read()
	if len(reported) != 1 || !strings.Contains(reported[0].Error(), "unrecognized invocation capture mode") {
		t.Fatalf("reported errors = %v, want one invalid invocation capture error", reported)
	}
	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	if _, ok := spanAttrs(ended[0])["gen_ai.input.messages"]; !ok {
		t.Error("invalid invocation capture did not fall back to the handler's span capture mode")
	}
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func testPtr[T any](value T) *T { return &value }

// chatInvocation is a completed non-streaming invocation with content in
// every content-bearing field.
func chatInvocation() *otelgenai.Invocation {
	return &otelgenai.Invocation{
		Provider:           "openai",
		RequestModel:       "gpt-5",
		ResponseModel:      "gpt-5-2026",
		ResponseID:         "resp_1",
		ConversationID:     "conv-1",
		AgentName:          "assistant",
		AgentVersion:       "1.0.0",
		SystemInstructions: otelgenai.SystemInstructionsFromText("Be concise."),
		InputMessages: []otelgenai.Message{{
			Role:  otelgenai.RoleUser,
			Parts: []otelgenai.Part{otelgenai.TextPart("Hello")},
		}},
		OutputMessages: []otelgenai.Message{{
			Role:  otelgenai.RoleAssistant,
			Parts: []otelgenai.Part{otelgenai.TextPart("Hi!")},
		}},
		ToolDefinitions: []otelgenai.ToolDefinition{{
			Name:       "weather",
			Parameters: []byte(`{"type":"object"}`),
		}},
		ToolCallArguments:  []byte(`{"city":"Paris"}`),
		ToolCallResult:     []byte(`{"temperature":18}`),
		RetrievalQueryText: "weather in Paris",
		RetrievalDocuments: []byte(`[{"id":"doc-1"}]`),
		FinishReasons:      []string{"stop"},
		Usage: otelgenai.Usage{
			Reported:     true,
			InputTokens:  120,
			OutputTokens: 42,
		},
		StartedAt:   startedAt,
		CompletedAt: completedAt,
	}
}

func TestSpanLifecycle(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name         string
		options      []otelgenai.Option
		mutate       func(inv *otelgenai.Invocation)
		wantName     string
		wantKind     trace.SpanKind
		checkStarted func(t *testing.T, span sdktrace.ReadOnlySpan)
		check        func(t *testing.T, span sdktrace.ReadOnlySpan)
	}{
		{
			name: "chat span defaults",
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Name(); got != "chat gpt-5" {
					t.Errorf("span name = %q, want %q", got, "chat gpt-5")
				}
				if got := span.SpanKind(); got != trace.SpanKindClient {
					t.Errorf("span kind = %v, want client", got)
				}
				attrs := spanAttrs(span)
				if got := attrs["gen_ai.operation.name"].AsString(); got != "chat" {
					t.Errorf("gen_ai.operation.name = %q, want chat", got)
				}
				if got := attrs["gen_ai.provider.name"].AsString(); got != "openai" {
					t.Errorf("gen_ai.provider.name = %q, want openai", got)
				}
				if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 120 {
					t.Errorf("gen_ai.usage.input_tokens = %d, want 120", got)
				}
				if _, ok := attrs["gen_ai.request.stream"]; ok {
					t.Error("non-streaming span carries gen_ai.request.stream")
				}
				// An instrumentation library leaves a successful operation
				// unset; Ok is the application's to set.
				if span.Status().Code != codes.Unset {
					t.Errorf("status = %v, want unset", span.Status().Code)
				}
			},
		},
		{
			name: "real invocation timestamps",
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if !span.StartTime().Equal(startedAt) {
					t.Errorf("span start = %v, want %v", span.StartTime(), startedAt)
				}
				if !span.EndTime().Equal(completedAt) {
					t.Errorf("span end = %v, want %v", span.EndTime(), completedAt)
				}
			},
		},
		{
			name:   "streaming sets request stream",
			mutate: func(inv *otelgenai.Invocation) { inv.Stream = true },
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if !spanAttrs(span)["gen_ai.request.stream"].AsBool() {
					t.Error("gen_ai.request.stream is not true on a streaming invocation")
				}
			},
		},
		{
			name: "error sets status and error type",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ErrorType = "provider_call_error"
				inv.ErrorMessage = "provider exploded"
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Status().Code; got != codes.Error {
					t.Errorf("status = %v, want error", got)
				}
				if got := span.Status().Description; got != "provider exploded" {
					t.Errorf("status description = %q, want %q", got, "provider exploded")
				}
				if got := spanAttrs(span)["error.type"].AsString(); got != "provider_call_error" {
					t.Errorf("error.type = %q, want provider_call_error", got)
				}
			},
		},
		{
			name:    "no content by default",
			options: nil,
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				for _, key := range []string{
					"gen_ai.input.messages",
					"gen_ai.output.messages",
					"gen_ai.system_instructions",
					"gen_ai.tool.definitions",
					"gen_ai.tool.call.arguments",
					"gen_ai.tool.call.result",
					"gen_ai.retrieval.query.text",
					"gen_ai.retrieval.documents",
				} {
					if _, ok := attrs[key]; ok {
						t.Errorf("%s present with the default capture mode", key)
					}
				}
			},
		},
		{
			name:    "span only emits content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly)},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got := attrs["gen_ai.input.messages"].AsString(); !strings.Contains(got, "Hello") {
					t.Errorf("gen_ai.input.messages = %q, want it to contain Hello", got)
				}
				if got := attrs["gen_ai.output.messages"].AsString(); !strings.Contains(got, "Hi!") {
					t.Errorf("gen_ai.output.messages = %q, want it to contain Hi!", got)
				}
				if got := attrs["gen_ai.system_instructions"].AsString(); !strings.Contains(got, "Be concise.") {
					t.Errorf("gen_ai.system_instructions = %q, want it to contain the prompt", got)
				}
				if got := attrs["gen_ai.tool.definitions"].AsString(); !strings.Contains(got, "weather") {
					t.Errorf("gen_ai.tool.definitions = %q, want it to contain weather", got)
				}
			},
		},
		{
			name:    "event only degrades to no content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureEventOnly)},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.input.messages"]; ok {
					t.Error("EVENT_ONLY put content on the span")
				}
			},
		},
		{
			name:    "span and event degrades to span content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureSpanAndEvent)},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.input.messages"]; !ok {
					t.Error("SPAN_AND_EVENT dropped span content")
				}
			},
		},
		{
			name: "no hooks means no vendor attributes",
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				for key := range spanAttrs(span) {
					if strings.HasPrefix(key, "agento11y.") {
						t.Errorf("span carries vendor attribute %q with no hook installed", key)
					}
				}
			},
		},
		{
			name: "hook attributes and content transform",
			options: []otelgenai.Option{
				otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					inv.InputMessages = []otelgenai.Message{{
						Role:  otelgenai.RoleUser,
						Parts: []otelgenai.Part{otelgenai.TextPart("[redacted]")},
					}}
					return []attribute.KeyValue{attribute.String("vendor.generation.id", "gen-1")}
				})),
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got := attrs["vendor.generation.id"].AsString(); got != "gen-1" {
					t.Errorf("vendor.generation.id = %q, want gen-1", got)
				}
				if got := attrs["gen_ai.input.messages"].AsString(); strings.Contains(got, "Hello") {
					t.Errorf("gen_ai.input.messages = %q, want the hook's rewrite", got)
				}
			},
		},
		{
			name: "hook sees the resolved capture mode",
			options: []otelgenai.Option{
				otelgenai.WithCaptureMode(otelgenai.CaptureNoContent),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(func(_ context.Context, _ *otelgenai.Invocation, capture otelgenai.CaptureMode) []attribute.KeyValue {
					return []attribute.KeyValue{attribute.Bool("vendor.span_content", capture.SpanContent())}
				})),
			},
			mutate: func(inv *otelgenai.Invocation) { inv.Capture = otelgenai.CaptureSpanOnly },
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if !attrs["vendor.span_content"].AsBool() {
					t.Error("the hook saw the handler's mode, not the invocation's override")
				}
				if _, ok := attrs["gen_ai.input.messages"]; !ok {
					t.Error("the per-invocation capture override did not reach the encoder")
				}
			},
		},
		{
			name:   "parser-only invocation capture spelling emits content",
			mutate: func(inv *otelgenai.Invocation) { inv.Capture = otelgenai.CaptureMode("span_only") },
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.input.messages"]; !ok {
					t.Error("the normalized invocation capture mode dropped content")
				}
			},
		},
		{
			name:    "invocation capture override withholds content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly)},
			mutate:  func(inv *otelgenai.Invocation) { inv.Capture = otelgenai.CaptureNoContent },
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.input.messages"]; ok {
					t.Error("the invocation asked for no content and got content")
				}
			},
		},
		{
			name: "an error with no type classifies as _OTHER",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ErrorMessage = "provider returned 500"
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Status().Code; got != codes.Error {
					t.Errorf("status = %v, want error", got)
				}
				if got := span.Status().Description; got != "provider returned 500" {
					t.Errorf("status description = %q, want the error message", got)
				}
				if got := spanAttrs(span)["error.type"].AsString(); got != "_OTHER" {
					t.Errorf("error.type = %q, want _OTHER", got)
				}
			},
		},
		{
			name: "streaming carries the time to first chunk",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := spanAttrs(span)["gen_ai.response.time_to_first_chunk"].AsFloat64(); got != 0.25 {
					t.Errorf("gen_ai.response.time_to_first_chunk = %v, want 0.25", got)
				}
			},
		},
		{
			// The rebuilt invocation drops Stream, which gates both attributes.
			name: "a hook rebuild keeps the streaming attributes",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{
						Provider:     inv.Provider,
						RequestModel: inv.RequestModel,
					}
					return nil
				}),
			)},
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(250 * time.Millisecond)
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if !attrs["gen_ai.request.stream"].AsBool() {
					t.Error("gen_ai.request.stream is not true after a hook rebuild")
				}
				if got := attrs["gen_ai.response.time_to_first_chunk"].AsFloat64(); got != 0.25 {
					t.Errorf("gen_ai.response.time_to_first_chunk = %v, want 0.25", got)
				}
			},
		},
		{
			name: "tool execution takes the conventions' internal shape",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationExecuteTool
				inv.ToolName = "weather"
				inv.ToolCallID = "call_1"
				inv.ToolType = "function"
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Name(); got != "execute_tool weather" {
					t.Errorf("span name = %q, want %q", got, "execute_tool weather")
				}
				if got := span.SpanKind(); got != trace.SpanKindInternal {
					t.Errorf("span kind = %v, want internal", got)
				}
				attrs := spanAttrs(span)
				if got := attrs["gen_ai.tool.name"].AsString(); got != "weather" {
					t.Errorf("gen_ai.tool.name = %q, want weather", got)
				}
				if got := attrs["gen_ai.tool.call.id"].AsString(); got != "call_1" {
					t.Errorf("gen_ai.tool.call.id = %q, want call_1", got)
				}
				if got := attrs["gen_ai.tool.type"].AsString(); got != "function" {
					t.Errorf("gen_ai.tool.type = %q, want function", got)
				}
			},
		},
		{
			name: "agent invocation is named for the agent",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationInvokeAgent
			},
			wantName: "invoke_agent assistant",
			wantKind: trace.SpanKindClient,
		},
		{
			name: "unnamed agent invocation does not fall back to the model",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationInvokeAgent
				inv.AgentName = ""
			},
			wantName: "invoke_agent",
			wantKind: trace.SpanKindClient,
		},
		{
			name: "text completion is named for the model",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationTextCompletion
			},
			wantName: "text_completion gpt-5",
			wantKind: trace.SpanKindClient,
		},
		{
			name: "retrieval is named for the data source",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationRetrieval
				inv.DataSourceID = "knowledge-base"
			},
			wantName: "retrieval knowledge-base",
			wantKind: trace.SpanKindClient,
			checkStarted: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				for _, key := range []string{"gen_ai.provider.name", "gen_ai.request.model"} {
					if _, ok := attrs[key]; ok {
						t.Errorf("non-sampling retrieval attribute %s was set at span start", key)
					}
				}
			},
		},
		{
			name: "fetch response never names the model",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationFetchResponse
			},
			wantName: "fetch_response",
			wantKind: trace.SpanKindClient,
		},
		{
			name: "workflow invocation is internal and named for the workflow",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationInvokeWorkflow
				inv.WorkflowName = "nightly"
			},
			wantName: "invoke_workflow nightly",
			wantKind: trace.SpanKindInternal,
		},
		{
			name: "agent creation is named for the agent",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationCreateAgent
			},
			wantName: "create_agent assistant",
			wantKind: trace.SpanKindClient,
		},
		{
			name: "planning is internal and named for the agent",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationPlan
			},
			wantName: "plan assistant",
			wantKind: trace.SpanKindInternal,
		},
		{
			name: "unnamed plan does not fall back to the model",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationPlan
				inv.AgentName = ""
			},
			wantName: "plan",
			wantKind: trace.SpanKindInternal,
		},
		{
			name: "an explicit span kind wins",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationInvokeAgent
				inv.Kind = trace.SpanKindInternal
			},
			wantName: "invoke_agent assistant",
			wantKind: trace.SpanKindInternal,
		},
		{
			name: "new request and response fields emit attributes",
			mutate: func(inv *otelgenai.Invocation) {
				inv.TopK = testPtr(int64(40))
				inv.FrequencyPenalty = testPtr(0.1)
				inv.PresencePenalty = testPtr(0.2)
				inv.StopSequences = []string{"stop", "done"}
				inv.Seed = testPtr(int64(7))
				inv.ChoiceCount = testPtr(int64(3))
				inv.OutputType = "json"
				inv.EncodingFormats = []string{"float", "base64"}
				inv.AgentID = "agent-1"
				inv.AgentDescription = "support agent"
				inv.DataSourceID = "knowledge-base"
				inv.WorkflowName = "nightly"
				inv.StreamCursor = "cursor-1"
				inv.ResponseStatus = "completed"
				inv.DimensionCount = testPtr(int64(1536))
			},
			checkStarted: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.embeddings.dimension.count"]; ok {
					t.Error("non-sampling embedding dimensions were set at span start")
				}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got := attrs["gen_ai.request.top_k"].AsInt64(); got != 40 {
					t.Errorf("gen_ai.request.top_k = %v, want 40", got)
				}
				if got := attrs["gen_ai.request.frequency_penalty"].AsFloat64(); got != 0.1 {
					t.Errorf("gen_ai.request.frequency_penalty = %v, want 0.1", got)
				}
				if got := attrs["gen_ai.request.presence_penalty"].AsFloat64(); got != 0.2 {
					t.Errorf("gen_ai.request.presence_penalty = %v, want 0.2", got)
				}
				if got := attrs["gen_ai.request.stop_sequences"].AsStringSlice(); !slices.Equal(got, []string{"stop", "done"}) {
					t.Errorf("gen_ai.request.stop_sequences = %v, want [stop done]", got)
				}
				for key, want := range map[string]int64{
					"gen_ai.request.seed":               7,
					"gen_ai.request.choice.count":       3,
					"gen_ai.embeddings.dimension.count": 1536,
				} {
					if got := attrs[key].AsInt64(); got != want {
						t.Errorf("%s = %d, want %d", key, got, want)
					}
				}
				for key, want := range map[string]string{
					"gen_ai.output.type":           "json",
					"gen_ai.agent.id":              "agent-1",
					"gen_ai.agent.description":     "support agent",
					"gen_ai.data_source.id":        "knowledge-base",
					"gen_ai.workflow.name":         "nightly",
					"gen_ai.request.stream_cursor": "cursor-1",
					"gen_ai.response.status":       "completed",
				} {
					if got := attrs[key].AsString(); got != want {
						t.Errorf("%s = %q, want %q", key, got, want)
					}
				}
				if got := attrs["gen_ai.request.encoding_formats"].AsStringSlice(); !slices.Equal(got, []string{"float", "base64"}) {
					t.Errorf("gen_ai.request.encoding_formats = %v, want [float base64]", got)
				}
			},
		},
		{
			name: "unset new fields emit no attributes",
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				for _, key := range []string{
					"gen_ai.request.top_k",
					"gen_ai.request.frequency_penalty",
					"gen_ai.request.presence_penalty",
					"gen_ai.request.stop_sequences",
					"gen_ai.request.seed",
					"gen_ai.request.choice.count",
					"gen_ai.output.type",
					"gen_ai.request.encoding_formats",
					"gen_ai.agent.id",
					"gen_ai.agent.description",
					"gen_ai.data_source.id",
					"gen_ai.workflow.name",
					"gen_ai.request.stream_cursor",
					"gen_ai.response.status",
					"gen_ai.embeddings.dimension.count",
				} {
					if _, ok := attrs[key]; ok {
						t.Errorf("unset field emitted %s", key)
					}
				}
			},
		},
		{
			name: "a completion that precedes the start is clamped",
			mutate: func(inv *otelgenai.Invocation) {
				inv.CompletedAt = inv.StartedAt.Add(-2 * time.Second)
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if span.EndTime().Before(span.StartTime()) {
					t.Errorf("span ends %s before it starts", span.StartTime().Sub(span.EndTime()))
				}
			},
		},
		{
			name: "a first chunk that precedes the start is clamped",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Stream = true
				inv.FirstChunkAt = inv.StartedAt.Add(-time.Second)
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				value, ok := spanAttrs(span)["gen_ai.response.time_to_first_chunk"]
				if !ok {
					t.Fatal("gen_ai.response.time_to_first_chunk is absent")
				}
				if got := value.AsFloat64(); got != 0 {
					t.Errorf("gen_ai.response.time_to_first_chunk = %v, want 0", got)
				}
			},
		},
		{
			name: "endpoint and tool attributes ride on the span",
			mutate: func(inv *otelgenai.Invocation) {
				inv.ServerAddress = "api.openai.com"
				inv.ServerPort = 443
				inv.ToolDescription = "looks up the weather"
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				// The conventions treat the endpoint as sampling-relevant, so
				// it has to be on the span and not only on the metrics.
				attrs := spanAttrs(span)
				if got := attrs["server.address"].AsString(); got != "api.openai.com" {
					t.Errorf("server.address = %q, want api.openai.com", got)
				}
				if got := attrs["server.port"].AsInt64(); got != 443 {
					t.Errorf("server.port = %d, want 443", got)
				}
				if got := attrs["gen_ai.tool.description"].AsString(); got != "looks up the weather" {
					t.Errorf("gen_ai.tool.description = %q, want the description", got)
				}
			},
		},
		{
			name: "counts without the reported flag reach the span",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{InputTokens: 7, OutputTokens: 3}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 7 {
					t.Errorf("gen_ai.usage.input_tokens = %d, want 7", got)
				}
				if got := attrs["gen_ai.usage.output_tokens"].AsInt64(); got != 3 {
					t.Errorf("gen_ai.usage.output_tokens = %d, want 3", got)
				}
			},
		},
		{
			name: "independently reported input zero omits unknown output",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{InputTokensReported: true}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got, ok := attrs["gen_ai.usage.input_tokens"]; !ok || got.AsInt64() != 0 {
					t.Errorf("gen_ai.usage.input_tokens = %v, present = %v, want known zero", got, ok)
				}
				if _, ok := attrs["gen_ai.usage.output_tokens"]; ok {
					t.Error("unknown output count reached the span")
				}
			},
		},
		{
			name: "independently reported output zero omits unknown input",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{OutputTokensReported: true}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				if got, ok := attrs["gen_ai.usage.output_tokens"]; !ok || got.AsInt64() != 0 {
					t.Errorf("gen_ai.usage.output_tokens = %v, present = %v, want known zero", got, ok)
				}
				if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
					t.Error("unknown input count reached the span")
				}
			},
		},
		{
			name: "fetch response omits usage attributes",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Operation = otelgenai.OperationFetchResponse
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				for _, key := range []string{
					"gen_ai.usage.input_tokens",
					"gen_ai.usage.output_tokens",
					"gen_ai.usage.cache_read.input_tokens",
					"gen_ai.usage.cache_creation.input_tokens",
					"gen_ai.usage.reasoning_tokens",
				} {
					if _, ok := attrs[key]; ok {
						t.Errorf("fetch_response carries %s", key)
					}
				}
			},
		},
		{
			name: "an all-zero usage the provider returned is emitted as zeros",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{Reported: true}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				// The Reported flag exists for this case: zeros the provider
				// really returned, which inference alone cannot tell from no
				// usage.
				attrs := spanAttrs(span)
				if _, ok := attrs["gen_ai.usage.input_tokens"]; !ok {
					t.Error("a reported all-zero usage carries no gen_ai.usage.input_tokens")
				}
				if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 0 {
					t.Errorf("gen_ai.usage.input_tokens = %d, want 0", got)
				}
			},
		},
		{
			name: "usage a provider never returned is absent",
			mutate: func(inv *otelgenai.Invocation) {
				inv.Usage = otelgenai.Usage{}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["gen_ai.usage.input_tokens"]; ok {
					t.Error("an invocation with no usage carries gen_ai.usage.input_tokens")
				}
			},
		},
		{
			name: "a hook can clear finish-time request attributes",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					inv.ConversationID = ""
					inv.ToolDescription = ""
					inv.MaxTokens = nil
					inv.AgentDescription = ""
					inv.AgentVersion = ""
					inv.AgentID = ""
					inv.StreamCursor = ""
					inv.DimensionCount = nil
					return nil
				}),
			)},
			mutate: func(inv *otelgenai.Invocation) {
				inv.ToolDescription = "sensitive tool description"
				inv.MaxTokens = testPtr(int64(100))
				inv.AgentDescription = "sensitive agent description"
				inv.AgentID = "agent-1"
				inv.StreamCursor = "cursor-1"
				inv.DimensionCount = testPtr(int64(1536))
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttrs(span)
				for _, key := range []string{
					"gen_ai.conversation.id",
					"gen_ai.tool.description",
					"gen_ai.request.max_tokens",
					"gen_ai.agent.description",
					"gen_ai.agent.version",
					"gen_ai.agent.id",
					"gen_ai.request.stream_cursor",
					"gen_ai.embeddings.dimension.count",
				} {
					if _, ok := attrs[key]; ok {
						t.Errorf("hook-cleared %s remains on the span", key)
					}
				}
			},
		},
		{
			name: "a panicking hook takes the content off the span",
			options: []otelgenai.Option{
				otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly),
				otelgenai.WithEndHook(
					otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
						// A redactor that got through one message and not the
						// next leaves the rest of the content raw.
						inv.InputMessages[0].Parts[0] = otelgenai.TextPart("REDACTED")
						panic("redactor exploded")
					}),
				),
			},
			mutate: func(inv *otelgenai.Invocation) {
				inv.InputMessages = []otelgenai.Message{{
					Role:  otelgenai.RoleUser,
					Parts: []otelgenai.Part{otelgenai.TextPart("secret one"), otelgenai.TextPart("secret two")},
				}}
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got, ok := spanAttrs(span)["gen_ai.input.messages"]; ok {
					t.Errorf("a half-redacted span carries content: %s", got.AsString())
				}
			},
		},
		{
			name: "a panicking hook drops attributes from every hook",
			options: []otelgenai.Option{
				otelgenai.WithEndHook(otelgenai.EndHookFunc(
					func(context.Context, *otelgenai.Invocation, otelgenai.CaptureMode) []attribute.KeyValue {
						return []attribute.KeyValue{attribute.String("vendor.content", "secret")}
					},
				)),
				otelgenai.WithEndHook(otelgenai.EndHookFunc(
					func(context.Context, *otelgenai.Invocation, otelgenai.CaptureMode) []attribute.KeyValue {
						panic("hook exploded")
					},
				)),
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if _, ok := spanAttrs(span)["vendor.content"]; ok {
					t.Error("an earlier hook's attribute reached the span after a later hook panicked")
				}
			},
		},
		{
			name: "a panicking hook does not reach the caller or strand the span",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(context.Context, *otelgenai.Invocation, otelgenai.CaptureMode) []attribute.KeyValue {
					panic("hook exploded")
				}),
			)},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Name(); got != "chat gpt-5" {
					t.Errorf("span name = %q, want the span to have closed normally", got)
				}
			},
		},
		{
			name: "a hook that rebuilds the invocation still closes the span",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					// A redaction hook that keeps an allowlist of fields is
					// written this way, and it must not take the span with it.
					*inv = otelgenai.Invocation{
						Provider:     inv.Provider,
						RequestModel: inv.RequestModel,
						StartedAt:    inv.StartedAt,
						CompletedAt:  inv.CompletedAt,
					}
					return nil
				}),
			)},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Name(); got != "chat gpt-5" {
					t.Errorf("span name = %q, want chat gpt-5", got)
				}
				if _, ok := spanAttrs(span)["gen_ai.input.messages"]; ok {
					t.Error("the hook cleared the messages and they reached the span anyway")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler, recorder := newRecordingHandler(t, tc.options...)
			inv := chatInvocation()
			if tc.mutate != nil {
				tc.mutate(inv)
			}
			ctx := handler.Start(context.Background(), inv)
			if tc.checkStarted != nil {
				started := recorder.Started()
				if len(started) != 1 {
					t.Fatalf("recorded %d started spans, want 1", len(started))
				}
				tc.checkStarted(t, started[0])
			}
			handler.End(ctx, inv)

			ended := recorder.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			span := ended[0]
			if tc.wantName != "" && span.Name() != tc.wantName {
				t.Errorf("span name = %q, want %q", span.Name(), tc.wantName)
			}
			if tc.wantKind != trace.SpanKindUnspecified && span.SpanKind() != tc.wantKind {
				t.Errorf("span kind = %v, want %v", span.SpanKind(), tc.wantKind)
			}
			if tc.check != nil {
				tc.check(t, span)
			}
		})
	}
}

func TestHookPanicRestoresCallerAndLifecycleState(t *testing.T) {
	sensitiveAttrs := []attribute.KeyValue{
		attribute.String("caller.sensitive", "synthetic-original"),
		attribute.String("caller.removed", "synthetic-removed"),
		attribute.String("caller.safe", "keep"),
	}
	cases := []struct {
		name   string
		attrs  []attribute.KeyValue
		redact func([]attribute.KeyValue) []attribute.KeyValue
		want   []attribute.KeyValue
	}{
		{
			name:  "first hook panic",
			attrs: []attribute.KeyValue{attribute.String("caller.safe", "keep")},
			want:  []attribute.KeyValue{attribute.String("caller.safe", "keep")},
		},
		{name: "first hook panic with nil attributes"},
		{
			name:  "earlier redaction and deletion",
			attrs: sensitiveAttrs,
			redact: func(attrs []attribute.KeyValue) []attribute.KeyValue {
				attrs[0] = attribute.String("caller.sensitive", "[redacted]")
				copy(attrs[1:], attrs[2:])
				return attrs[:2]
			},
			want: []attribute.KeyValue{
				attribute.String("caller.sensitive", "[redacted]"),
				attribute.String("caller.safe", "keep"),
			},
		},
		{
			name:   "earlier deletion to nil",
			attrs:  sensitiveAttrs,
			redact: func([]attribute.KeyValue) []attribute.KeyValue { return nil },
		},
		{
			name:   "earlier deletion reuses backing slices",
			attrs:  sensitiveAttrs,
			redact: func(attrs []attribute.KeyValue) []attribute.KeyValue { return attrs[:0] },
			want:   []attribute.KeyValue{},
		},
	}
	for _, tc := range cases {
		for _, mutation := range []string{"in place", "replace invocation"} {
			t.Run(tc.name+"/"+mutation, func(t *testing.T) {
				checkInvocationAttrs := func(inv *otelgenai.Invocation) {
					t.Helper()
					for name, attrs := range map[string][]attribute.KeyValue{
						"span": inv.Attributes, "metric": inv.MetricAttributes,
					} {
						if !slices.Equal(attrs, tc.want) || (tc.want == nil && attrs != nil) {
							t.Errorf("invocation %s attributes = %v, want %v", name, attrs, tc.want)
						}
					}
				}
				meterOption, collect := newMetricRecorder(t)
				options := []otelgenai.Option{meterOption, otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly)}
				if tc.redact != nil {
					options = append(options, otelgenai.WithEndHook(otelgenai.EndHookFunc(
						func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
							inv.Attributes = tc.redact(inv.Attributes)
							inv.MetricAttributes = tc.redact(inv.MetricAttributes)
							return []attribute.KeyValue{attribute.String("hook.before_panic", "discard")}
						},
					)))
				}
				var realSpan trace.SpanContext
				var laterHookSawRealSpan bool
				options = append(options,
					otelgenai.WithEndHook(otelgenai.EndHookFunc(
						func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
							checkInvocationAttrs(inv)
							if len(inv.Attributes) > 0 {
								inv.Attributes[0] = attribute.String("caller.sensitive", "corrupt")
								inv.MetricAttributes[0] = attribute.String("caller.sensitive", "corrupt")
							}
							inv.Attributes = append(inv.Attributes, attribute.String("caller.added", "discard"))
							inv.MetricAttributes = append(inv.MetricAttributes, attribute.String("caller.added", "discard"))
							if mutation == "replace invocation" {
								*inv = otelgenai.Invocation{
									Attributes:       inv.Attributes,
									MetricAttributes: inv.MetricAttributes,
									Usage:            inv.Usage,
									InputMessages:    inv.InputMessages,
								}
							}
							panic("hook exploded")
						},
					)),
					otelgenai.WithEndHook(otelgenai.EndHookFunc(
						func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
							checkInvocationAttrs(inv)
							spanContext := inv.Span().SpanContext()
							laterHookSawRealSpan = inv.Span().IsRecording() && realSpan.IsValid() &&
								spanContext.TraceID() == realSpan.TraceID() &&
								spanContext.SpanID() == realSpan.SpanID()
							return []attribute.KeyValue{attribute.String("hook.after_panic", "discard")}
						},
					)),
				)
				handler, recorder := newRecordingHandler(t, options...)
				inv := chatInvocation()
				inv.Stream = true
				inv.FirstChunkAt = startedAt.Add(250 * time.Millisecond)
				if tc.attrs != nil {
					inv.Attributes = append(make([]attribute.KeyValue, 0, 4), tc.attrs...)
					inv.MetricAttributes = append(make([]attribute.KeyValue, 0, 4), tc.attrs...)
				}
				ctx := handler.Start(context.Background(), inv)
				realSpan = inv.Span().SpanContext()
				handler.End(ctx, inv)
				handler.End(ctx, inv)

				checkInvocationAttrs(inv)
				if !laterHookSawRealSpan {
					t.Error("a later hook did not receive the real invocation span")
				}
				ended := recorder.Ended()
				if len(ended) != 1 {
					t.Fatalf("recorded %d spans, want 1", len(ended))
				}
				if !ended[0].StartTime().Equal(startedAt) || !ended[0].EndTime().Equal(completedAt) {
					t.Error("hook panic changed span timestamps")
				}
				attrs := spanAttrs(ended[0])
				if !attrs["gen_ai.request.stream"].AsBool() || attrs["gen_ai.response.time_to_first_chunk"].AsFloat64() != 0.25 {
					t.Error("hook panic changed streaming state")
				}
				for _, key := range []string{"hook.before_panic", "hook.after_panic", "gen_ai.input.messages"} {
					if _, ok := attrs[key]; ok {
						t.Errorf("%s survived a hook panic", key)
					}
				}
				checkExportedAttrs := func(source string, attrs map[string]string) {
					t.Helper()
					for _, kv := range tc.want {
						if got := attrs[string(kv.Key)]; got != kv.Value.AsString() {
							t.Errorf("%s %s = %q, want %q", source, kv.Key, got, kv.Value.AsString())
						}
					}
					for key, value := range attrs {
						if strings.HasPrefix(key, "caller.") && !slices.ContainsFunc(tc.want, func(kv attribute.KeyValue) bool {
							return string(kv.Key) == key
						}) {
							t.Errorf("%s has unexpected %s = %q", source, key, value)
						}
					}
				}
				spanValues := make(map[string]string)
				for key, value := range attrs {
					spanValues[key] = value.AsString()
				}
				checkExportedAttrs("span", spanValues)
				metrics := collect()
				for _, name := range []string{
					"gen_ai.client.operation.duration",
					"gen_ai.client.operation.time_to_first_chunk",
					"gen_ai.client.token.usage",
				} {
					points := metricPointAttributes(t, metrics[name])
					if len(points) == 0 {
						t.Errorf("%s has no data points", name)
					}
					for _, point := range points {
						checkExportedAttrs(name, point)
					}
				}
				if got := histogramCount(t, metrics["gen_ai.client.operation.duration"]); got != 1 {
					t.Errorf("duration count = %d, want 1", got)
				}
			})
		}
	}
}

func TestStartNestsUnderCallerSpan(t *testing.T) {
	t.Parallel()

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})
	handler := otelgenai.NewHandler(otelgenai.WithTracerProvider(provider))

	parentCtx, parent := provider.Tracer("app").Start(context.Background(), "request")
	inv := chatInvocation()
	ctx := handler.Start(parentCtx, inv)
	if got := trace.SpanFromContext(ctx).SpanContext().SpanID(); !got.IsValid() {
		t.Fatal("Start returned a context without a valid span")
	}
	handler.End(ctx, inv)
	parent.End()

	for _, span := range recorder.Ended() {
		if span.Name() != "chat gpt-5" {
			continue
		}
		if got, want := span.Parent().SpanID(), parent.SpanContext().SpanID(); got != want {
			t.Fatalf("generation span parent = %s, want %s", got, want)
		}
		return
	}
	t.Fatal("no generation span recorded")
}

func TestEndWithoutStartRecordsNoSpan(t *testing.T) {
	t.Parallel()

	handler, recorder := newRecordingHandler(t)
	inv := chatInvocation()
	handler.End(context.Background(), inv)

	if got := len(recorder.Ended()); got != 0 {
		t.Fatalf("recorded %d spans, want 0", got)
	}
}

func TestRepeatedLifecycleCalls(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		options []otelgenai.Option
	}{
		{name: "no hooks"},
		{
			// A hook that rebuilds the invocation clears the unexported state
			// End uses to tell a second call from the first.
			name: "a hook that rebuilds the invocation",
			options: []otelgenai.Option{otelgenai.WithEndHook(
				otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
					*inv = otelgenai.Invocation{
						Provider:     inv.Provider,
						RequestModel: inv.RequestModel,
					}
					return nil
				}),
			)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logs := logtest.NewRecorder()
			meterOption, collectMetrics := newMetricRecorder(t)
			options := append([]otelgenai.Option{
				meterOption,
				otelgenai.WithLoggerProvider(logs),
				otelgenai.WithCaptureMode(otelgenai.CaptureEventOnly),
				otelgenai.WithEmitEvent(true),
			}, tc.options...)
			handler, spans := newRecordingHandler(t, options...)
			inv := chatInvocation()
			ctx := handler.Start(context.Background(), inv)
			// A second Start would orphan the first span, which is never exported.
			handler.Start(ctx, inv)
			handler.End(ctx, inv)
			// The span handle is nil after the first End. Events and metrics pin
			// that the second call is also a no-op.
			handler.End(ctx, inv)

			if got := len(spans.Started()); got != 1 {
				t.Errorf("started %d spans, want 1", got)
			}
			if got := len(spans.Ended()); got != 1 {
				t.Fatalf("recorded %d spans, want 1", got)
			}
			records, _ := recordedEvents(t, logs)
			if got := len(records); got != 1 {
				t.Errorf("recorded %d operation-details events, want 1", got)
			}
			metrics := collectMetrics()
			duration, ok := metrics["gen_ai.client.operation.duration"]
			if !ok {
				t.Fatal("gen_ai.client.operation.duration not recorded")
			}
			if got := histogramCount(t, duration); got != 1 {
				t.Errorf("duration count = %d, want 1", got)
			}
		})
	}
}

func TestWithCaptureMode(t *testing.T) {
	cases := []struct {
		name        string
		env         string
		optIn       bool
		mode        otelgenai.CaptureMode
		wantContent bool
	}{
		{name: "canonical spelling", mode: otelgenai.CaptureSpanOnly, wantContent: true},
		{name: "lowercase spelling", mode: "span_only", wantContent: true},
		{name: "span and event includes span content", mode: otelgenai.CaptureSpanAndEvent, wantContent: true},
		{name: "unrecognized mode emits no content", mode: "bogus", wantContent: false},
		{
			name:        "the zero value leaves the environment in charge",
			env:         "SPAN_ONLY",
			optIn:       true,
			mode:        otelgenai.CaptureUnset,
			wantContent: true,
		},
		{
			name:        "an explicit mode overrides the environment",
			env:         "SPAN_ONLY",
			optIn:       true,
			mode:        otelgenai.CaptureNoContent,
			wantContent: false,
		},
		{
			name:        "the environment needs the experimental opt-in",
			env:         "SPAN_ONLY",
			mode:        otelgenai.CaptureUnset,
			wantContent: false,
		},
		{
			name:        "an explicit mode needs no opt-in",
			mode:        otelgenai.CaptureSpanOnly,
			wantContent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(otelgenai.EnvCaptureMessageContent, tc.env)
			optIn := ""
			if tc.optIn {
				optIn = "gen_ai_latest_experimental"
			}
			t.Setenv(otelgenai.EnvSemconvStabilityOptIn, optIn)

			handler, recorder := newRecordingHandler(t, otelgenai.WithCaptureMode(tc.mode))
			inv := chatInvocation()
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)

			ended := recorder.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			_, gotContent := spanAttrs(ended[0])["gen_ai.input.messages"]
			if gotContent != tc.wantContent {
				t.Errorf("content on span = %v, want %v", gotContent, tc.wantContent)
			}
		})
	}
}

func TestEndHookReceivesEndContext(t *testing.T) {
	t.Parallel()

	type carrier struct{}

	var got any
	handler, _ := newRecordingHandler(t, otelgenai.WithEndHook(
		otelgenai.EndHookFunc(func(ctx context.Context, _ *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
			got = ctx.Value(carrier{})
			if !trace.SpanContextFromContext(ctx).IsValid() {
				t.Error("the hook's context carries no span, so a span the hook starts would not nest")
			}
			return nil
		}),
	))

	inv := chatInvocation()
	ctx := handler.Start(context.WithValue(context.Background(), carrier{}, "carried"), inv)
	handler.End(ctx, inv)

	if got != "carried" {
		t.Errorf("hook context value = %v, want the context End was called with", got)
	}
}

func TestPackageIdentity(t *testing.T) {
	t.Parallel()

	if otelgenai.ScopeName != "github.com/grafana/agento11y/go/otelgenai" {
		t.Errorf("ScopeName = %q, want the package import path", otelgenai.ScopeName)
	}
	if otelgenai.Version() == "" {
		t.Error("Version() is empty")
	}
	if otelgenai.SchemaURL != "https://opentelemetry.io/schemas/1.41.0" {
		t.Errorf("SchemaURL = %q, want the v1.41.0 schema", otelgenai.SchemaURL)
	}
}
