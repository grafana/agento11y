package otelgenai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/log"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

const operationDetailsEventName = "gen_ai.client.inference.operation.details"

func (h *Handler) emitOperationDetails(
	ctx context.Context,
	span trace.Span,
	inv *Invocation,
	capture CaptureMode,
	emit bool,
) {
	if !emit {
		return
	}
	if span != nil && span.SpanContext().IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, span.SpanContext())
	}

	var record log.Record
	record.SetEventName(operationDetailsEventName)
	if !inv.CompletedAt.IsZero() {
		record.SetTimestamp(inv.CompletedAt)
	}
	attrs := attributesToLog(h.requestAttributes(inv))
	attrs = append(attrs, attributesToLog(h.responseAttributes(inv))...)
	attrs = append(attrs, eventContentAttributes(inv, capture)...)
	record.AddAttributes(attrs...)
	h.logger.Emit(ctx, record)
}

func (h *Handler) shouldEmitEvent(capture CaptureMode) bool {
	if h.emitEventOverride != nil {
		return *h.emitEventOverride
	}
	return capture.EventContent()
}

func isInferenceOperation(operation Operation) bool {
	switch operation {
	case OperationEmbeddings,
		OperationExecuteTool,
		OperationInvokeAgent,
		OperationRetrieval,
		OperationFetchResponse,
		OperationInvokeWorkflow,
		OperationCreateAgent,
		OperationPlan:
		return false
	default:
		return true
	}
}

func attributesToLog(attrs []attribute.KeyValue) []log.KeyValue {
	out := make([]log.KeyValue, 0, len(attrs))
	for _, attr := range attrs {
		out = append(out, log.KeyValueFromAttribute(attr))
	}
	return out
}

func eventContentAttributes(inv *Invocation, capture CaptureMode) []log.KeyValue {
	if !capture.EventContent() {
		return nil
	}

	var attrs []log.KeyValue
	encode := func(key attribute.Key, encoder func() (string, error), populated bool) {
		if !populated {
			return
		}
		payload, err := encoder()
		if err != nil {
			otel.Handle(fmt.Errorf("otelgenai: encoding %s for event: %w", key, err))
		}
		if payload == "" {
			return
		}
		value, err := structuredLogValue(payload)
		if err != nil {
			otel.Handle(fmt.Errorf("otelgenai: structuring %s for event: %w", key, err))
			return
		}
		attrs = append(attrs, log.KeyValue{Key: string(key), Value: value})
	}

	encode(semconv.GenAISystemInstructionsKey, func() (string, error) {
		return encodeSystemInstructions(inv.SystemInstructions)
	}, len(inv.SystemInstructions) > 0)
	encode(semconv.GenAIInputMessagesKey, func() (string, error) {
		return encodeMessages(inv.InputMessages, false)
	}, len(inv.InputMessages) > 0)
	encode(semconv.GenAIOutputMessagesKey, func() (string, error) {
		return encodeMessages(inv.OutputMessages, true)
	}, len(inv.OutputMessages) > 0)
	encode(semconv.GenAIToolDefinitionsKey, func() (string, error) {
		return encodeToolDefinitions(inv.ToolDefinitions)
	}, len(inv.ToolDefinitions) > 0)
	encode(semconv.GenAIToolCallArgumentsKey, func() (string, error) {
		payload, err := rawJSONField(inv.ToolCallArguments, "tool call arguments")
		return string(payload), err
	}, len(inv.ToolCallArguments) > 0)
	encode(semconv.GenAIToolCallResultKey, func() (string, error) {
		payload, err := rawJSONField(inv.ToolCallResult, "tool call result")
		return string(payload), err
	}, len(inv.ToolCallResult) > 0)
	if inv.RetrievalQueryText != "" {
		attrs = append(attrs, log.String(string(semconv.GenAIRetrievalQueryTextKey), inv.RetrievalQueryText))
	}
	encode(semconv.GenAIRetrievalDocumentsKey, func() (string, error) {
		payload, err := rawJSONField(inv.RetrievalDocuments, "retrieval documents")
		return string(payload), err
	}, len(inv.RetrievalDocuments) > 0)
	return attrs
}

func structuredLogValue(payload string) (log.Value, error) {
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return log.Value{}, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		if err == nil {
			return log.Value{}, errors.New("multiple JSON values")
		}
		return log.Value{}, err
	}
	return logValue(value)
}

func logValue(value any) (log.Value, error) {
	switch value := value.(type) {
	case nil:
		return log.Value{}, nil
	case bool:
		return log.BoolValue(value), nil
	case string:
		return log.StringValue(value), nil
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return log.Int64Value(integer), nil
		}
		decimal, err := value.Float64()
		if err != nil {
			return log.Value{}, err
		}
		return log.Float64Value(decimal), nil
	case []any:
		items := make([]log.Value, 0, len(value))
		for _, item := range value {
			converted, err := logValue(item)
			if err != nil {
				return log.Value{}, err
			}
			items = append(items, converted)
		}
		return log.SliceValue(items...), nil
	case map[string]any:
		items := make([]log.KeyValue, 0, len(value))
		for _, key := range slices.Sorted(maps.Keys(value)) {
			converted, err := logValue(value[key])
			if err != nil {
				return log.Value{}, err
			}
			items = append(items, log.KeyValue{Key: key, Value: converted})
		}
		return log.MapValue(items...), nil
	default:
		return log.Value{}, fmt.Errorf("unsupported JSON value %T", value)
	}
}
