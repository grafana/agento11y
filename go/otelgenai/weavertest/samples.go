package weavertest

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"strconv"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Sample is one entry of the JSON array Weaver's live-check reads from
// `--input-source <file> --input-format json`. Weaver's ingester is an
// externally tagged enum, so the span sits under a "span" key.
type Sample struct {
	Span SampleSpan `json:"span"`
}

// SampleSpan is Weaver's sample_span schema. Weaver's deserializer
// requires name, kind, and, when status is present, its message; it
// defaults the rest. Every field is written anyway, so what live-check
// reads is what the fixture holds and not a Weaver default.
type SampleSpan struct {
	Name       string            `json:"name"`
	Kind       string            `json:"kind"`
	Status     SampleSpanStatus  `json:"status"`
	Attributes []SampleAttribute `json:"attributes"`
	SpanEvents []SampleSpanEvent `json:"span_events"`
	SpanLinks  []SampleSpanLink  `json:"span_links"`
}

type SampleSpanStatus struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type SampleSpanEvent struct {
	Name       string            `json:"name"`
	Attributes []SampleAttribute `json:"attributes"`
}

type SampleSpanLink struct {
	Attributes []SampleAttribute `json:"attributes"`
}

// SampleAttribute is one attribute. Weaver infers the semantic type from
// the JSON type of Value, so the encoding of the value decides whether
// live-check reports a type_mismatch.
type SampleAttribute struct {
	Name  string          `json:"name"`
	Value json.RawMessage `json:"value"`
}

// spanKinds maps OTLP span kinds to Weaver's spelling. Weaver does not define
// SPAN_KIND_UNSPECIFIED. The OTLP protobuf permits receivers to interpret
// SPAN_KIND_UNSPECIFIED as SPAN_KIND_INTERNAL, so this package maps both values
// to "internal".
var spanKinds = map[tracepb.Span_SpanKind]string{
	tracepb.Span_SPAN_KIND_UNSPECIFIED: "internal",
	tracepb.Span_SPAN_KIND_INTERNAL:    "internal",
	tracepb.Span_SPAN_KIND_SERVER:      "server",
	tracepb.Span_SPAN_KIND_CLIENT:      "client",
	tracepb.Span_SPAN_KIND_PRODUCER:    "producer",
	tracepb.Span_SPAN_KIND_CONSUMER:    "consumer",
}

var statusCodes = map[tracepb.Status_StatusCode]string{
	tracepb.Status_STATUS_CODE_UNSET: "unset",
	tracepb.Status_STATUS_CODE_OK:    "ok",
	tracepb.Status_STATUS_CODE_ERROR: "error",
}

// SampleFromSpan converts an OTLP span into the Weaver sample schema. It omits
// resource attributes because Weaver live-check evaluates only span attributes.
// The GenAI registry does not define resource attributes such as service.name.
func SampleFromSpan(span *tracepb.Span) (Sample, error) {
	kind, ok := spanKinds[span.GetKind()]
	if !ok {
		return Sample{}, fmt.Errorf("span %q: unknown OTLP span kind %v", span.GetName(), span.GetKind())
	}
	code, ok := statusCodes[span.GetStatus().GetCode()]
	if !ok {
		return Sample{}, fmt.Errorf("span %q: unknown OTLP status code %v", span.GetName(), span.GetStatus().GetCode())
	}

	attributes, err := sampleAttributes(span.GetAttributes())
	if err != nil {
		return Sample{}, fmt.Errorf("span %q: %w", span.GetName(), err)
	}

	events := make([]SampleSpanEvent, 0, len(span.GetEvents()))
	for _, event := range span.GetEvents() {
		eventAttrs, err := sampleAttributes(event.GetAttributes())
		if err != nil {
			return Sample{}, fmt.Errorf("span %q event %q: %w", span.GetName(), event.GetName(), err)
		}
		events = append(events, SampleSpanEvent{Name: event.GetName(), Attributes: eventAttrs})
	}

	links := make([]SampleSpanLink, 0, len(span.GetLinks()))
	for _, link := range span.GetLinks() {
		linkAttrs, err := sampleAttributes(link.GetAttributes())
		if err != nil {
			return Sample{}, fmt.Errorf("span %q link: %w", span.GetName(), err)
		}
		links = append(links, SampleSpanLink{Attributes: linkAttrs})
	}

	return Sample{Span: SampleSpan{
		Name:       span.GetName(),
		Kind:       kind,
		Status:     SampleSpanStatus{Code: code, Message: span.GetStatus().GetMessage()},
		Attributes: attributes,
		SpanEvents: events,
		SpanLinks:  links,
	}}, nil
}

func sampleAttributes(attrs []*commonpb.KeyValue) ([]SampleAttribute, error) {
	out := make([]SampleAttribute, 0, len(attrs))
	for _, attr := range attrs {
		value, err := encodeAnyValue(attr.GetValue())
		if err != nil {
			return nil, fmt.Errorf("attribute %q: %w", attr.GetKey(), err)
		}
		out = append(out, SampleAttribute{Name: attr.GetKey(), Value: value})
	}
	return out, nil
}

// encodeAnyValue renders an OTLP value as the JSON Weaver types it from.
func encodeAnyValue(value *commonpb.AnyValue) (json.RawMessage, error) {
	switch v := value.GetValue().(type) {
	case nil:
		// An attribute with no value at all. Weaver types a JSON null as
		// None and reports nothing about it, so encoding one would record
		// the attribute as conformant. Fail instead, for the same reason
		// the kvlist branch below does.
		return nil, fmt.Errorf("attribute has no value")
	case *commonpb.AnyValue_StringValue:
		return json.Marshal(v.StringValue)
	case *commonpb.AnyValue_BoolValue:
		return json.Marshal(v.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return json.RawMessage(strconv.FormatInt(v.IntValue, 10)), nil
	case *commonpb.AnyValue_DoubleValue:
		return encodeDouble(v.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return json.Marshal(base64.StdEncoding.EncodeToString(v.BytesValue))
	case *commonpb.AnyValue_ArrayValue:
		items := make([]json.RawMessage, 0, len(v.ArrayValue.GetValues()))
		for _, item := range v.ArrayValue.GetValues() {
			encoded, err := encodeAnyValue(item)
			if err != nil {
				return nil, err
			}
			items = append(items, encoded)
		}
		return json.Marshal(items)
	case *commonpb.AnyValue_KvlistValue:
		// OTLP allows map values. The GenAI registry defines no map attributes,
		// and Weaver's sample schema cannot represent map values. Return an error
		// rather than omit the attribute from the verdict.
		return nil, fmt.Errorf("kvlist attribute values are not supported by the Weaver sample schema")
	default:
		return nil, fmt.Errorf("unknown OTLP value type %T", v)
	}
}

// encodeDouble always writes a fractional part. Weaver infers the type from
// JSON, so an integral double written as `2` becomes an int and hides a
// type_mismatch against a declared int. Weaver 0.25.1 reports no mismatch when
// an int is used for a declared double.
func encodeDouble(value float64) (json.RawMessage, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, fmt.Errorf("double value %v has no JSON encoding", value)
	}
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if value == math.Trunc(value) {
		text += ".0"
	}
	return json.RawMessage(text), nil
}
