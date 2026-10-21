package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
	colmetricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestStripTraceGenerationMetadata(t *testing.T) {
	for _, contentType := range []string{wire.ContentTypeProto, wire.ContentTypeJSON} {
		for _, withContent := range []bool{false, true} {
			t.Run(contentType+"/content="+map[bool]string{false: "absent", true: "present"}[withContent], func(t *testing.T) {
				span := &tracepb.Span{Attributes: []*commonpb.KeyValue{
					stringKV("agento11y.record", "true"),
					stringKV("agento11y.generation.id", "generation-id"),
					stringKV("agento11y.generation.metadata", `{"agento11y.conversation.title":"secret title","sigil.conversation.title":"secret old title","call_error":"secret error","agento11y.sdk.content_capture_mode":"full","keep":{"a":[1,true,null]}}`),
				}}
				if withContent {
					for _, key := range []string{"gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.definitions", "gen_ai.system_instructions", "agento11y.generation.raw_artifacts"} {
						span.Attributes = append(span.Attributes, stringKV(key, "secret content"))
					}
				}
				request := traceRequestForSpans(span)
				original := proto.Clone(request)
				var body []byte
				var err error
				if contentType == wire.ContentTypeJSON {
					body, err = protojson.Marshal(request)
				} else {
					body, err = proto.Marshal(request)
				}
				require.NoError(t, err)
				out, _, err := stripTracePayload(body, contentType)
				require.NoError(t, err)
				require.True(t, proto.Equal(original, request))
				reduced, err := decodeTracePayload(out, contentType)
				require.NoError(t, err)
				attrs := reduced.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes
				require.Equal(t, "true", stringAttribute(attrs, "agento11y.record"))
				require.Equal(t, "generation-id", stringAttribute(attrs, "agento11y.generation.id"))
				var metadata map[string]any
				require.NoError(t, json.Unmarshal([]byte(stringAttribute(attrs, "agento11y.generation.metadata")), &metadata))
				require.Equal(t, map[string]any{
					"agento11y.sdk.content_capture_mode": "metadata_only",
					"keep":                               map[string]any{"a": []any{float64(1), true, nil}},
				}, metadata)
				require.NotContains(t, string(out), "secret")
			})
		}
	}
}

func TestStripTraceGenerationMetadataEmpty(t *testing.T) {
	for _, metadata := range []string{"", `{}`, `{"call_error":"secret","agento11y.conversation.title":"secret"}`} {
		t.Run(metadata, func(t *testing.T) {
			span := &tracepb.Span{Attributes: []*commonpb.KeyValue{stringKV("agento11y.record", "true"), stringKV("agento11y.generation.id", "g")}}
			if metadata != "" {
				span.Attributes = append(span.Attributes, stringKV("agento11y.generation.metadata", metadata))
			}
			require.NoError(t, stripTraceContent(traceRequestForSpans(span)))
			require.JSONEq(t, `{"agento11y.sdk.content_capture_mode":"metadata_only"}`, stringAttribute(span.Attributes, "agento11y.generation.metadata"))
		})
	}
}

func TestForwardOTLPInvalidMetadataFailsClosed(t *testing.T) {
	for _, value := range []*commonpb.AnyValue{
		{Value: &commonpb.AnyValue_StringValue{StringValue: `{"call_error":"secret"`}},
		{Value: &commonpb.AnyValue_StringValue{StringValue: `null`}},
		{Value: &commonpb.AnyValue_StringValue{StringValue: `[]`}},
		{Value: &commonpb.AnyValue_StringValue{StringValue: `"secret"`}},
		{Value: &commonpb.AnyValue_StringValue{StringValue: `{"call_error":"secret"} trailing`}},
		{Value: &commonpb.AnyValue_IntValue{IntValue: 1}},
		nil,
	} {
		t.Run(value.String(), func(t *testing.T) {
			var calls atomic.Int32
			cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
			defer cloud.Close()
			loader := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
			request := traceRequestForSpans(&tracepb.Span{Attributes: []*commonpb.KeyValue{
				stringKV("agento11y.record", "true"),
				{Key: "agento11y.generation.metadata", Value: value},
			}})
			body, err := proto.Marshal(request)
			require.NoError(t, err)
			loader.forwardOTLPWithLabel(fakeForwardConfig(t, cloud.URL, true), "traces", wire.ContentTypeProto, "", body, forwardLabelOTLPGenerations)
			require.Zero(t, calls.Load())
			require.Len(t, loader.status().Failures, 1)
			require.Equal(t, forwardLabelOTLPGenerations, loader.status().Failures[0].Label)
			require.Contains(t, loader.status().Failures[0].Detail, "agento11y.generation.metadata")
			require.NotContains(t, loader.status().Failures[0].Detail, "secret")
		})
	}
}

func TestForwardOTLPExpandedLimit(t *testing.T) {
	var calls atomic.Int32
	cloud := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer cloud.Close()
	loader := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
	cfg := fakeForwardConfig(t, cloud.URL, true)
	oversize := gzipBytes(t, bytes.Repeat([]byte("x"), maxOTLPTraceBodyBytes+1))
	for _, body := range [][]byte{oversize, []byte("not gzip")} {
		loader.forwardOTLP(cfg, "traces", wire.ContentTypeProto, "gzip", body)
		require.Contains(t, loader.status().Failures[0].Detail, "gunzip")
	}
	require.Zero(t, calls.Load())
	request := traceRequestForSpans(&tracepb.Span{Attributes: []*commonpb.KeyValue{
		stringKV("gen_ai.input.messages", strings.Repeat("x", 17<<20)),
	}})
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	loader.forwardOTLP(cfg, "traces", wire.ContentTypeProto, "gzip", gzipBytes(t, body))
	require.EqualValues(t, 1, calls.Load(), "trace expansion above the metrics limit remains supported")
	require.Empty(t, loader.status().Failures)
}

func TestForwardOTLPResponse(t *testing.T) {
	for _, leg := range []struct{ signal, label string }{
		{"traces", otlpForwardLabel("traces")},
		{"metrics", otlpForwardLabel("metrics")},
		{"traces", forwardLabelOTLPGenerations},
	} {
		signal := leg.signal
		for _, contentType := range []string{wire.ContentTypeProto, wire.ContentTypeJSON} {
			for _, rejected := range []int64{0, 2} {
				t.Run(fmt.Sprintf("%s/%s/rejected=%d", leg.label, contentType, rejected), func(t *testing.T) {
					var response proto.Message
					if signal == "traces" {
						response = &coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: rejected, ErrorMessage: "warning"}}
					} else {
						response = &colmetricspb.ExportMetricsServiceResponse{PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{RejectedDataPoints: rejected, ErrorMessage: "warning"}}
					}
					var body []byte
					var err error
					if contentType == wire.ContentTypeJSON {
						body, err = protojson.Marshal(response)
					} else {
						body, err = proto.Marshal(response)
					}
					require.NoError(t, err)
					var calls atomic.Int32
					cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						_, _ = io.Copy(io.Discard, r.Body)
						w.Header().Set("Content-Type", contentType)
						_, _ = w.Write(body)
					}))
					defer cloud.Close()
					loader := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
					cfg := fakeForwardConfig(t, cloud.URL, false)
					label := leg.label
					loader.enqueue(label, func() { loader.forwardOTLPWithLabel(cfg, signal, wire.ContentTypeProto, "", []byte("payload"), label) })
					loader.wait()
					for range 3 {
						status := loader.status()
						if rejected > 0 {
							require.Len(t, status.Failures, 1)
							require.Equal(t, label, status.Failures[0].Label)
							require.Contains(t, status.Failures[0].Detail, "OTLP partial success: rejected_")
							require.Empty(t, status.Legs[label].LastSuccessAt)
						} else {
							require.Empty(t, status.Failures)
							require.NotEmpty(t, status.Legs[label].LastSuccessAt)
						}
					}
					loader.wait()
					require.EqualValues(t, 1, calls.Load(), "partially accepted batches must not replay")
				})
			}
		}
	}
}

func TestCheckOTLPForwardResponse(t *testing.T) {
	for _, tc := range []struct {
		name, body, contentType string
		wantError               bool
	}{
		{name: "empty protobuf", contentType: wire.ContentTypeProto},
		{name: "empty JSON", contentType: wire.ContentTypeJSON},
		{name: "JSON success", body: `{}`, contentType: wire.ContentTypeJSON},
		{name: "JSON snake case rejection", body: `{"partial_success":{"rejected_spans":"1"}}`, contentType: wire.ContentTypeJSON, wantError: true},
		{name: "malformed JSON", body: `{`, contentType: wire.ContentTypeJSON, wantError: true},
		{name: "malformed protobuf", body: `invalid`, contentType: wire.ContentTypeProto, wantError: true},
		{name: "oversize response", body: strings.Repeat("x", (1<<20)+1), contentType: wire.ContentTypeProto, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkOTLPForwardResponse(strings.NewReader(tc.body), tc.contentType, "traces")
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
		})
	}
	require.ErrorContains(t, checkOTLPForwardResponse(forwardErrorReader{}, wire.ContentTypeProto, "traces"), "read OTLP response")
}

func TestForwardStatusGenerationTransport(t *testing.T) {
	base := map[string]string{
		"AGENTO11Y_LOCAL_FORWARD":                "true",
		"AGENTO11Y_PROTOCOL":                     "otel",
		"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  "https://otlp.example.test",
		"OTEL_EXPORTER_OTLP_HEADERS":             "Authorization=Bearer gateway-only",
	}
	for _, tc := range []struct {
		name                      string
		extra                     map[string]string
		wantGenerations, wantHTTP bool
		wantReason                string
	}{
		{name: "OTel gateway only", wantGenerations: true},
		{name: "OTel dedicated token only", extra: map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "", "AGENTO11Y_AUTH_TENANT_ID": "tenant", "AGENTO11Y_OTEL_AUTH_TOKEN": "otlp-token"}, wantGenerations: true},
		{name: "OTel bare endpoint", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://bare.example.test"}, wantGenerations: true},
		{name: "legacy protocol alias", extra: map[string]string{"AGENTO11Y_PROTOCOL": "", "SIGIL_PROTOCOL": "otel"}, wantGenerations: true},
		{name: "OTel with legacy HTTP credentials", extra: map[string]string{"AGENTO11Y_ENDPOINT": "https://http.example.test", "AGENTO11Y_AUTH_TENANT_ID": "tenant", "AGENTO11Y_AUTH_TOKEN": "token"}, wantGenerations: true, wantHTTP: true},
		{name: "OTel missing OTLP despite HTTP credentials", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "", "AGENTO11Y_ENDPOINT": "https://http.example.test", "AGENTO11Y_AUTH_TENANT_ID": "tenant", "AGENTO11Y_AUTH_TOKEN": "token"}, wantHTTP: true, wantReason: "OTLP base endpoint"},
		{name: "OTel missing opt-in", extra: map[string]string{"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": ""}, wantReason: "ENABLE_EXPERIMENTAL_FEATURES"},
		{name: "legacy opt-in is ignored", extra: map[string]string{"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "", "SIGIL_ENABLE_EXPERIMENTAL_FEATURES": "true"}, wantReason: "ENABLE_EXPERIMENTAL_FEATURES"},
		{name: "OTel loopback route", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4319"}, wantReason: "loop back"},
		{name: "OTel invalid route", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://%"}, wantReason: "OTLP base endpoint"},
		{name: "unsupported protocol", extra: map[string]string{"AGENTO11Y_PROTOCOL": "unsupported"}, wantReason: "expected http or otel"},
		{name: "HTTP default still requires HTTP credentials", extra: map[string]string{"AGENTO11Y_PROTOCOL": ""}, wantReason: "ENDPOINT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			t.Setenv("SIGIL_ENABLE_EXPERIMENTAL_FEATURES", "")
			lines := maps.Clone(base)
			maps.Copy(lines, tc.extra)
			loader := newForwardLoader(writeConfigEnvFile(t, lines), nil)
			cfg := loader.load()
			status := loader.status()
			require.Equal(t, tc.wantGenerations, status.Generations)
			require.Equal(t, tc.wantHTTP, cfg.genURL != "")
			if tc.wantReason == "" {
				require.Empty(t, status.Reason)
			} else {
				require.Contains(t, status.Reason, tc.wantReason)
			}
		})
	}
	t.Run("file transport overrides boot environment", func(t *testing.T) {
		clearForwardEnv(t)
		t.Setenv("AGENTO11Y_PROTOCOL", "http")
		t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
		loader := newForwardLoader(writeConfigEnvFile(t, base), nil)
		require.True(t, loader.status().Generations)
		require.Empty(t, loader.status().Reason)
	})
}

type forwardErrorReader struct{}

func (forwardErrorReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }
