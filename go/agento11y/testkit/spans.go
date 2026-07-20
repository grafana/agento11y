package testkit

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func FindSpan(t testing.TB, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()

	for i := range spans {
		if spans[i].Name() == name {
			return spans[i]
		}
	}
	t.Fatalf("span %q not found", name)
	return nil
}

func SpanAttributes(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	if span == nil {
		return nil
	}

	attrs := make(map[string]attribute.Value, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attrs[string(attr.Key)] = attr.Value
	}
	return attrs
}
