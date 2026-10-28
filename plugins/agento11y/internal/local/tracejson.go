package local

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"

	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
)

func unmarshalTraceJSON(body []byte, request *coltracepb.ExportTraceServiceRequest) error {
	body, err := rewriteTraceJSONIDs(body, hex.DecodeString, base64.StdEncoding.EncodeToString)
	if err != nil {
		return err
	}
	return (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(body, request)
}

func marshalTraceJSON(request *coltracepb.ExportTraceServiceRequest) ([]byte, error) {
	body, err := (protojson.MarshalOptions{UseEnumNumbers: true}).Marshal(request)
	if err != nil {
		return nil, err
	}
	return rewriteTraceJSONIDs(body, base64.StdEncoding.DecodeString, hex.EncodeToString)
}

// OTLP JSON uses hex IDs instead of protobuf JSON's base64 byte fields.
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding
func rewriteTraceJSONIDs(body []byte, decode func(string) ([]byte, error), encode func([]byte) string) ([]byte, error) {
	var request map[string]any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&request); err != nil {
		return nil, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("expected one OTLP trace JSON value")
	}
	for _, resource := range traceJSONObjects(request, "resourceSpans") {
		for _, scope := range traceJSONObjects(resource, "scopeSpans") {
			for _, span := range traceJSONObjects(scope, "spans") {
				objects := append([]map[string]any{span}, traceJSONObjects(span, "links")...)
				for index, object := range objects {
					keys := []string{"traceId", "spanId"}
					if index == 0 {
						keys = append(keys, "parentSpanId")
					}
					for _, key := range keys {
						value := object[key]
						if value == nil {
							continue
						}
						text, ok := value.(string)
						if !ok {
							return nil, fmt.Errorf("OTLP %s must be a string", key)
						}
						decoded, err := decode(text)
						if err != nil {
							return nil, fmt.Errorf("decode OTLP %s: %w", key, err)
						}
						object[key] = encode(decoded)
					}
				}
			}
		}
	}
	return json.Marshal(request)
}

func traceJSONObjects(parent map[string]any, key string) []map[string]any {
	values, _ := parent[key].([]any)
	objects := make([]map[string]any, 0, len(values))
	for _, value := range values {
		if object, ok := value.(map[string]any); ok {
			objects = append(objects, object)
		}
	}
	return objects
}
