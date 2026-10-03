package otelgenai_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log/logtest"
	"go.opentelemetry.io/otel/trace"

	"github.com/grafana/agento11y/go/otelgenai"
)

func recordedEvents(t *testing.T, recorder *logtest.Recorder) ([]logtest.Record, logtest.Scope) {
	t.Helper()

	for scope, records := range recorder.Result() {
		if scope.Name == otelgenai.ScopeName {
			return records, scope
		}
	}
	return nil, logtest.Scope{}
}

func logAttributes(record logtest.Record) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(record.Attributes))
	for _, attr := range record.Attributes {
		out[string(attr.Key)] = attr.Value
	}
	return out
}

func logMap(value attribute.Value) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(value.AsMap()))
	for _, item := range value.AsMap() {
		out[string(item.Key)] = item.Value
	}
	return out
}

var eventContentKeys = []string{
	"gen_ai.system_instructions",
	"gen_ai.input.messages",
	"gen_ai.output.messages",
	"gen_ai.tool.definitions",
	"gen_ai.tool.call.arguments",
	"gen_ai.tool.call.result",
	"gen_ai.retrieval.query.text",
	"gen_ai.retrieval.documents",
}

func TestOperationDetailsEventGating(t *testing.T) {
	cases := []struct {
		name        string
		mode        otelgenai.CaptureMode
		emitEnv     string
		emitOption  *bool
		operation   otelgenai.Operation
		hook        otelgenai.EndHook
		want        int
		wantContent bool
	}{
		{name: "no content defaults off", mode: otelgenai.CaptureNoContent},
		{name: "span only defaults off", mode: otelgenai.CaptureSpanOnly},
		{name: "event only defaults on", mode: otelgenai.CaptureEventOnly, want: 1, wantContent: true},
		{name: "span and event defaults on", mode: otelgenai.CaptureSpanAndEvent, want: 1, wantContent: true},
		{name: "false overrides event only", mode: otelgenai.CaptureEventOnly, emitEnv: "false"},
		{name: "true overrides no content but not its content", mode: otelgenai.CaptureNoContent, emitEnv: "true", want: 1},
		{name: "option false overrides env true", mode: otelgenai.CaptureEventOnly, emitEnv: "true", emitOption: testPtr(false)},
		{name: "option true overrides env false", mode: otelgenai.CaptureNoContent, emitEnv: "false", emitOption: testPtr(true), want: 1},
		{name: "invalid value falls back to capture", mode: otelgenai.CaptureEventOnly, emitEnv: "invalid", want: 1, wantContent: true},
		{name: "custom inference operations emit", mode: otelgenai.CaptureEventOnly, operation: "custom_inference", want: 1, wantContent: true},
		{
			name:      "non-inference operations stay silent",
			mode:      otelgenai.CaptureEventOnly,
			operation: otelgenai.OperationExecuteTool,
		},
		{
			name:      "event eligibility follows the operation after hooks",
			mode:      otelgenai.CaptureEventOnly,
			operation: otelgenai.OperationExecuteTool,
			hook: otelgenai.EndHookFunc(func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
				*inv = otelgenai.Invocation{}
				return nil
			}),
			want: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(otelgenai.EnvEmitEvent, tc.emitEnv)

			recorder := logtest.NewRecorder()
			options := []otelgenai.Option{otelgenai.WithCaptureMode(tc.mode)}
			if tc.emitOption != nil {
				options = append(options, otelgenai.WithEmitEvent(*tc.emitOption))
			}
			options = append(options, otelgenai.WithLoggerProvider(recorder))
			if tc.hook != nil {
				options = append(options, otelgenai.WithEndHook(tc.hook))
			}
			handler, spans := newRecordingHandler(t, options...)
			inv := chatInvocation()
			inv.Operation = tc.operation
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)

			ended := spans.Ended()
			if len(ended) != 1 {
				t.Fatalf("recorded %d spans, want 1", len(ended))
			}
			spanAttributes := spanAttrs(ended[0])
			for _, key := range eventContentKeys {
				_, present := spanAttributes[key]
				if present != tc.mode.SpanContent() {
					t.Errorf("span carries %s = %v, want %v", key, present, tc.mode.SpanContent())
				}
			}

			records, _ := recordedEvents(t, recorder)
			if len(records) != tc.want {
				t.Fatalf("recorded %d events, want %d", len(records), tc.want)
			}
			if tc.want != 1 {
				return
			}
			if records[0].EventName != "gen_ai.client.inference.operation.details" {
				t.Errorf("event name = %q, want gen_ai.client.inference.operation.details", records[0].EventName)
			}
			attrs := logAttributes(records[0])
			if got := attrs["gen_ai.operation.name"].AsString(); got == "" {
				t.Error("event carries no gen_ai.operation.name")
			}
			for _, key := range eventContentKeys {
				_, present := attrs[key]
				if present != tc.wantContent {
					t.Errorf("event carries %s = %v, want %v", key, present, tc.wantContent)
				}
			}
		})
	}
}

func TestOperationDetailsEventStructuredContent(t *testing.T) {
	t.Setenv(otelgenai.EnvEmitEvent, "")

	recorder := logtest.NewRecorder()
	handler, _ := newRecordingHandler(t,
		otelgenai.WithLoggerProvider(recorder),
		otelgenai.WithCaptureMode(otelgenai.CaptureSpanAndEvent),
	)
	inv := chatInvocation()
	inv.TopK = testPtr(int64(40))
	inv.ToolCallArguments = []byte(`{"city":"Paris"}`)
	inv.RetrievalDocuments = []byte(`[{"id":"doc-1","score":0.9}]`)
	inv.InputMessages = []otelgenai.Message{{
		Role: otelgenai.RoleUser,
		Parts: []otelgenai.Part{{
			Type:       otelgenai.PartTypeCompaction,
			ID:         "compaction-1",
			Content:    testPtr("summary"),
			Extensions: map[string]any{"vendor.turns": 12},
		}},
	}}
	inv.OutputMessages = []otelgenai.Message{{
		Role: otelgenai.RoleAssistant,
		Parts: []otelgenai.Part{
			{
				Type:      otelgenai.PartTypeServerToolCall,
				ID:        "server-call-1",
				Name:      "web_search",
				Arguments: []byte(`{"q":"weather"}`),
			},
			{
				Type:     otelgenai.PartTypeServerToolCallResponse,
				ID:       "server-call-1",
				Response: []byte(`{"hits":3}`),
			},
		},
	}}

	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	records, scope := recordedEvents(t, recorder)
	if len(records) != 1 {
		t.Fatalf("recorded %d events, want 1", len(records))
	}
	if scope.Version != otelgenai.Version() || scope.SchemaURL != otelgenai.SchemaURL {
		t.Errorf("scope = version %q schema %q, want %q and %q", scope.Version, scope.SchemaURL, otelgenai.Version(), otelgenai.SchemaURL)
	}
	record := records[0]
	if !record.Timestamp.Equal(completedAt) {
		t.Errorf("event timestamp = %v, want %v", record.Timestamp, completedAt)
	}
	if got := trace.SpanContextFromContext(record.Context); !got.IsValid() {
		t.Error("event context has no valid span context")
	}
	attrs := logAttributes(record)
	if got := attrs["gen_ai.request.top_k"].AsInt64(); got != 40 {
		t.Errorf("gen_ai.request.top_k = %v, want 40", got)
	}

	input := attrs["gen_ai.input.messages"].AsSlice()
	if len(input) != 1 {
		t.Fatalf("input messages = %d, want 1", len(input))
	}
	inputMessage := logMap(input[0])
	parts := inputMessage["parts"].AsSlice()
	compaction := logMap(parts[0])
	for key, want := range map[string]string{
		"type":    "compaction",
		"id":      "compaction-1",
		"content": "summary",
	} {
		if got := compaction[key].AsString(); got != want {
			t.Errorf("compaction %s = %q, want %q", key, got, want)
		}
	}
	if got := compaction["vendor.turns"].AsInt64(); got != 12 {
		t.Errorf("compaction vendor.turns = %d, want 12", got)
	}

	output := attrs["gen_ai.output.messages"].AsSlice()
	outputMessage := logMap(output[0])
	serverCall := logMap(outputMessage["parts"].AsSlice()[0])
	if _, ok := serverCall["arguments"]; ok {
		t.Error("server tool call event content carries arguments instead of server_tool_call")
	}
	payload := logMap(serverCall["server_tool_call"])
	if got := payload["q"].AsString(); got != "weather" {
		t.Errorf("server_tool_call.q = %q, want weather", got)
	}
	serverResponse := logMap(outputMessage["parts"].AsSlice()[1])
	if _, ok := serverResponse["response"]; ok {
		t.Error("server tool response event content carries response instead of server_tool_call_response")
	}
	responsePayload := logMap(serverResponse["server_tool_call_response"])
	if got := responsePayload["hits"].AsInt64(); got != 3 {
		t.Errorf("server_tool_call_response.hits = %d, want 3", got)
	}

	arguments := logMap(attrs["gen_ai.tool.call.arguments"])
	if got := arguments["city"].AsString(); got != "Paris" {
		t.Errorf("tool call arguments city = %q, want Paris", got)
	}
	documents := attrs["gen_ai.retrieval.documents"].AsSlice()
	if got := logMap(documents[0])["id"].AsString(); got != "doc-1" {
		t.Errorf("retrieval document id = %q, want doc-1", got)
	}
}

func TestOperationDetailsEventKeepsSiblingsOfUnrepresentableNumber(t *testing.T) {
	recorder := logtest.NewRecorder()
	handler, _ := newRecordingHandler(t,
		otelgenai.WithLoggerProvider(recorder),
		otelgenai.WithCaptureMode(otelgenai.CaptureEventOnly),
	)
	inv := chatInvocation()
	inv.Usage = otelgenai.Usage{InputTokensReported: true}
	inv.InputMessages = []otelgenai.Message{
		{
			Role: otelgenai.RoleUser,
			Parts: []otelgenai.Part{{
				Type:    otelgenai.PartTypeText,
				Content: testPtr("safe"),
				Extensions: map[string]any{"vendor.nested": map[string]any{
					"bad":  json.Number("1e10000"),
					"good": "kept",
				}},
			}},
		},
		{
			Role:  otelgenai.RoleUser,
			Parts: []otelgenai.Part{otelgenai.TextPart("unrelated message")},
		},
	}

	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	records, _ := recordedEvents(t, recorder)
	if len(records) != 1 {
		t.Fatalf("recorded %d events, want 1", len(records))
	}
	attrs := logAttributes(records[0])
	messages := attrs["gen_ai.input.messages"].AsSlice()
	if len(messages) != 2 {
		t.Fatalf("input messages = %d, want 2", len(messages))
	}
	message := logMap(messages[0])
	part := logMap(message["parts"].AsSlice()[0])
	nested := logMap(part["vendor.nested"])
	if got := nested["good"].AsString(); got != "kept" {
		t.Errorf("valid sibling = %q, want kept", got)
	}
	if _, ok := nested["bad"]; ok {
		t.Error("unrepresentable number reached the structured event")
	}
	unrelated := logMap(logMap(messages[1])["parts"].AsSlice()[0])
	if got := unrelated["content"].AsString(); got != "unrelated message" {
		t.Errorf("unrelated message = %q, want unrelated message", got)
	}
	if got, ok := attrs["gen_ai.usage.input_tokens"]; !ok || got.AsInt64() != 0 {
		t.Errorf("known input zero = %v, present = %v", got, ok)
	}
	if _, ok := attrs["gen_ai.usage.output_tokens"]; ok {
		t.Error("unknown output usage reached the structured event")
	}
}

func TestOperationDetailsEventOmitsInvalidTopLevelContentShapes(t *testing.T) {
	cases := []struct {
		name string
		key  string
		set  func(*otelgenai.Invocation)
	}{
		{
			name: "tool arguments array",
			key:  "gen_ai.tool.call.arguments",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallArguments = json.RawMessage(`["Paris"]`)
			},
		},
		{
			name: "tool result scalar",
			key:  "gen_ai.tool.call.result",
			set: func(inv *otelgenai.Invocation) {
				inv.ToolCallResult = json.RawMessage(`"sunny"`)
			},
		},
		{
			name: "retrieval documents object",
			key:  "gen_ai.retrieval.documents",
			set: func(inv *otelgenai.Invocation) {
				inv.RetrievalDocuments = json.RawMessage(`{"id":"doc-1"}`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testOTelErrors.reset()
			recorder := logtest.NewRecorder()
			handler, _ := newRecordingHandler(t,
				otelgenai.WithLoggerProvider(recorder),
				otelgenai.WithCaptureMode(otelgenai.CaptureEventOnly),
			)
			inv := chatInvocation()
			tc.set(inv)
			ctx := handler.Start(context.Background(), inv)
			handler.End(ctx, inv)

			records, _ := recordedEvents(t, recorder)
			if len(records) != 1 {
				t.Fatalf("recorded %d events, want 1", len(records))
			}
			if _, ok := logAttributes(records[0])[tc.key]; ok {
				t.Errorf("invalid %s reached the event", tc.key)
			}
			var diagnosed bool
			for _, err := range testOTelErrors.read() {
				if strings.Contains(err.Error(), tc.key) && strings.Contains(err.Error(), "expected") {
					diagnosed = true
					break
				}
			}
			if !diagnosed {
				t.Errorf("OTel errors = %v, want a diagnostic for %s", testOTelErrors.read(), tc.key)
			}
		})
	}
}

func TestOperationDetailsEventSuppressesContentAfterHookPanic(t *testing.T) {
	t.Setenv(otelgenai.EnvEmitEvent, "")

	recorder := logtest.NewRecorder()
	handler, _ := newRecordingHandler(t,
		otelgenai.WithLoggerProvider(recorder),
		otelgenai.WithCaptureMode(otelgenai.CaptureEventOnly),
		otelgenai.WithEndHook(otelgenai.EndHookFunc(
			func(_ context.Context, inv *otelgenai.Invocation, _ otelgenai.CaptureMode) []attribute.KeyValue {
				inv.InputMessages[0].Parts[0] = otelgenai.TextPart("partly redacted")
				panic("redactor exploded")
			},
		)),
	)
	inv := chatInvocation()
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	records, _ := recordedEvents(t, recorder)
	if len(records) != 1 {
		t.Fatalf("recorded %d events, want 1", len(records))
	}
	attrs := logAttributes(records[0])
	for _, key := range []string{
		"gen_ai.input.messages",
		"gen_ai.output.messages",
		"gen_ai.system_instructions",
		"gen_ai.tool.definitions",
	} {
		if _, ok := attrs[key]; ok {
			t.Errorf("event carries %s after a hook panic", key)
		}
	}
}
