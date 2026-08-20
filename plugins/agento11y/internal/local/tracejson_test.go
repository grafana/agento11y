package local

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
)

func TestTraceJSONCodec(t *testing.T) {
	fixture, err := os.ReadFile("testdata/otlp-tool-trace.json")
	require.NoError(t, err)
	var batch struct {
		ResourceSpans []json.RawMessage `json:"resourceSpans"`
	}
	require.NoError(t, json.Unmarshal(fixture, &batch))
	var resource struct {
		ScopeSpans []json.RawMessage `json:"scopeSpans"`
	}
	require.NoError(t, json.Unmarshal(batch.ResourceSpans[0], &resource))
	resource.ScopeSpans = append(resource.ScopeSpans, resource.ScopeSpans[0])
	resourceJSON, err := json.Marshal(resource)
	require.NoError(t, err)
	batch.ResourceSpans = []json.RawMessage{resourceJSON, resourceJSON}
	batchJSON, err := json.Marshal(batch)
	require.NoError(t, err)
	const traceID = "01010101010101010101010101010101"
	const spanID = "0202020202020202"
	const parentID = "0303030303030303"
	for _, tc := range []struct {
		name        string
		body        string
		old         string
		replacement string
		noParent    bool
		wantStart   uint64
		wantSpans   int
		wantErr     string
	}{
		{name: "span and link IDs"},
		{name: "multiple resources and scopes", body: string(batchJSON), wantSpans: 4},
		{name: "missing parent", old: `"parentSpanId": "` + parentID + `",`, noParent: true},
		{name: "empty parent", old: `"parentSpanId": "` + parentID + `"`, replacement: `"parentSpanId": ""`, noParent: true},
		{name: "null parent", old: `"parentSpanId": "` + parentID + `"`, replacement: `"parentSpanId": null`, noParent: true},
		{name: "integer timestamp precision", old: `"10000000000"`, replacement: `1000000000000000001`, wantStart: 1000000000000000001},
		{name: "unknown fields", old: `"name": "execute_tool Bash"`, replacement: `"name": "execute_tool Bash", "futureField": {"traceId": "not-an-id"}`},
		{name: "unknown link parent", old: `"spanId": "CDCDCDCDCDCDCDCD"`, replacement: `"spanId": "CDCDCDCDCDCDCDCD", "parentSpanId": "not-an-id"`},
		{name: "empty request", body: `{}`},
		{name: "invalid JSON", body: `{`, wantErr: "EOF"},
		{name: "invalid root", body: `[]`, wantErr: "cannot unmarshal array"},
		{name: "trailing JSON", body: string(fixture) + `{}`, wantErr: "expected one"},
		{name: "trailing invalid data", body: string(fixture) + `x`, wantErr: "expected one"},
		{name: "non-string ID", old: `"traceId": "` + traceID + `"`, replacement: `"traceId": 42`, wantErr: "traceId must be a string"},
		{name: "invalid hex", old: traceID, replacement: strings.Repeat("z", 32), wantErr: "decode OTLP traceId"},
		{name: "odd hex length", old: spanID, replacement: "abc", wantErr: "odd length"},
		{name: "invalid parent", old: parentID, replacement: "bad-parent", wantErr: "decode OTLP parentSpanId"},
		{name: "invalid link", old: "CDCDCDCDCDCDCDCD", replacement: "bad-link", wantErr: "decode OTLP spanId"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := string(fixture)
			if tc.body != "" {
				body = tc.body
			}
			if tc.old != "" {
				body = strings.Replace(body, tc.old, tc.replacement, 1)
			}
			request, err := decodeTracePayload([]byte(body), wire.ContentTypeJSON)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			encoded, err := marshalTraceJSON(request)
			require.NoError(t, err)
			roundTrip, err := decodeTracePayload(encoded, wire.ContentTypeJSON)
			require.NoError(t, err)
			assert.True(t, proto.Equal(request, roundTrip))
			if tc.body == `{}` {
				assert.Empty(t, request.GetResourceSpans())
				return
			}
			count := 0
			for _, resource := range request.GetResourceSpans() {
				for _, scope := range resource.GetScopeSpans() {
					for _, span := range scope.GetSpans() {
						assert.Equal(t, bytes.Repeat([]byte{1}, 16), span.GetTraceId())
						assert.Equal(t, bytes.Repeat([]byte{2}, 8), span.GetSpanId())
						assert.Equal(t, bytes.Repeat([]byte{0xab}, 16), span.GetLinks()[0].GetTraceId())
						assert.Equal(t, bytes.Repeat([]byte{0xcd}, 8), span.GetLinks()[0].GetSpanId())
						count++
					}
				}
			}
			wantSpans := tc.wantSpans
			if wantSpans == 0 {
				wantSpans = 1
			}
			assert.Equal(t, wantSpans, count)
			span := request.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
			if tc.noParent {
				assert.Empty(t, span.GetParentSpanId())
			} else {
				assert.Equal(t, bytes.Repeat([]byte{3}, 8), span.GetParentSpanId())
			}
			wantStart := uint64(10000000000)
			if tc.wantStart != 0 {
				wantStart = tc.wantStart
			}
			assert.Equal(t, wantStart, span.GetStartTimeUnixNano())
			for _, attribute := range span.GetAttributes() {
				if attribute.GetKey() == "opaque" {
					assert.Equal(t, bytes.Repeat([]byte{1}, 16), attribute.GetValue().GetBytesValue())
				}
			}
			var document struct {
				ResourceSpans []struct {
					ScopeSpans []struct {
						Spans []struct {
							TraceID      string `json:"traceId"`
							SpanID       string `json:"spanId"`
							ParentSpanID string `json:"parentSpanId"`
							Kind         int    `json:"kind"`
							Status       struct {
								Code int `json:"code"`
							} `json:"status"`
							Links []struct {
								TraceID string `json:"traceId"`
								SpanID  string `json:"spanId"`
							} `json:"links"`
						} `json:"spans"`
					} `json:"scopeSpans"`
				} `json:"resourceSpans"`
			}
			require.NoError(t, json.Unmarshal(encoded, &document))
			wireSpan := document.ResourceSpans[0].ScopeSpans[0].Spans[0]
			assert.Equal(t, traceID, wireSpan.TraceID)
			assert.Equal(t, spanID, wireSpan.SpanID)
			assert.Equal(t, hex.EncodeToString(span.GetParentSpanId()), wireSpan.ParentSpanID)
			assert.Equal(t, 1, wireSpan.Kind)
			assert.Equal(t, 2, wireSpan.Status.Code)
			assert.Equal(t, strings.Repeat("ab", 16), wireSpan.Links[0].TraceID)
			assert.Equal(t, strings.Repeat("cd", 8), wireSpan.Links[0].SpanID)
			assert.Contains(t, string(encoded), `"bytesValue":"AQEBAQEBAQEBAQEBAQEBAQ=="`)
			assert.Contains(t, string(encoded), `"stringValue":"not-an-id"`)
			assert.NotContains(t, string(encoded), "futureField")
		})
	}
}
