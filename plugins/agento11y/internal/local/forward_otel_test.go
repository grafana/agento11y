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
	"os"
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
			cfg := fakeForwardConfig(t, cloud.URL, true)
			loader.dispatchOTLP(cfg, "traces", wire.ContentTypeProto, "", body, forwardLabelOTLPGenerations)
			loader.wait()
			require.Zero(t, calls.Load())
			status := loader.destinationStatuses(cfg)[0]
			require.Len(t, status.Failures, 1)
			require.Equal(t, forwardLabelOTLPGenerations, status.Failures[0].Label)
			require.Equal(t, "preparation failed", status.Failures[0].Detail)
			require.EqualValues(t, 1, status.Legs[forwardLabelOTLPGenerations].Dropped)
			require.Zero(t, status.Legs[forwardLabelOTLPGenerations].Attempted)
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
		loader.dispatchOTLP(cfg, "traces", wire.ContentTypeProto, "gzip", body, otlpForwardLabel("traces"))
		loader.wait()
		require.Equal(t, "preparation failed", loader.destinationStatuses(cfg)[0].Failures[0].Detail)
	}
	require.Zero(t, calls.Load())
	request := traceRequestForSpans(&tracepb.Span{Attributes: []*commonpb.KeyValue{
		stringKV("gen_ai.input.messages", strings.Repeat("x", 17<<20)),
	}})
	body, err := proto.Marshal(request)
	require.NoError(t, err)
	loader.dispatchOTLP(cfg, "traces", wire.ContentTypeProto, "gzip", gzipBytes(t, body), otlpForwardLabel("traces"))
	loader.wait()
	require.EqualValues(t, 1, calls.Load(), "trace expansion above the metrics limit remains supported")
	status := loader.destinationStatuses(cfg)[0]
	require.Empty(t, status.Failures)
	require.EqualValues(t, 1, status.Legs[otlpForwardLabel("traces")].Succeeded)
}

func TestForwardOTLPResponse(t *testing.T) {
	for _, leg := range []struct{ signal, label string }{
		{"traces", otlpForwardLabel("traces")},
		{"metrics", otlpForwardLabel("metrics")},
		{"traces", forwardLabelOTLPGenerations},
	} {
		signal := leg.signal
		for _, contentType := range []string{wire.ContentTypeProto, wire.ContentTypeJSON} {
			rejectedField := "rejected_spans"
			if signal == "metrics" {
				rejectedField = "rejected_data_points"
			}
			for _, tc := range []struct {
				name      string
				rejected  int64
				body      []byte
				wantError string
			}{
				{name: "warning_only"},
				{name: "rejected", rejected: 2, wantError: "OTLP partial success: " + rejectedField + "=2"},
				{name: "malformed", body: []byte("{secret remote diagnostic"), wantError: "invalid OTLP " + signal + " response"},
				{name: "oversize", body: bytes.Repeat([]byte("x"), (1<<20)+1), wantError: "delivery failed"},
			} {
				t.Run(fmt.Sprintf("%s/%s/%s", leg.label, contentType, tc.name), func(t *testing.T) {
					var response proto.Message
					if signal == "traces" {
						response = &coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: tc.rejected, ErrorMessage: "secret remote diagnostic"}}
					} else {
						response = &colmetricspb.ExportMetricsServiceResponse{PartialSuccess: &colmetricspb.ExportMetricsPartialSuccess{RejectedDataPoints: tc.rejected, ErrorMessage: "secret remote diagnostic"}}
					}
					var body []byte
					var err error
					if contentType == wire.ContentTypeJSON {
						body, err = protojson.Marshal(response)
					} else {
						body, err = proto.Marshal(response)
					}
					require.NoError(t, err)
					if tc.body != nil {
						body = tc.body
					}
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
					loader.dispatchOTLP(cfg, signal, wire.ContentTypeProto, "", []byte("payload"), label)
					loader.wait()
					for range 3 {
						status := loader.destinationStatuses(cfg)[0]
						require.EqualValues(t, 1, status.Legs[label].Attempted)
						if tc.wantError != "" {
							require.Len(t, status.Failures, 1)
							require.Equal(t, label, status.Failures[0].Label)
							require.Equal(t, tc.wantError, status.Failures[0].Detail)
							require.NotContains(t, status.Failures[0].Detail, "secret")
							require.EqualValues(t, 1, status.Legs[label].Failed)
							require.Empty(t, status.Legs[label].LastSuccessAt)
						} else {
							require.Empty(t, status.Failures)
							require.EqualValues(t, 1, status.Legs[label].Succeeded)
							require.NotEmpty(t, status.Legs[label].LastSuccessAt)
						}
					}
					loader.wait()
					require.EqualValues(t, 1, calls.Load(), "delivery outcomes must not replay batches")
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
		sidecar, signal           string
		wantSend                  bool
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
		{name: "OTel HTTP extra without primary", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "http", signal: "traces", wantReason: "OTLP base endpoint"},
		{name: "OTel HTTP extra with refused primary", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4319"}, sidecar: "http", signal: "traces", wantReason: "loop back"},
		{name: "HTTP OTLP extra without primary", extra: map[string]string{"AGENTO11Y_PROTOCOL": "", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "otlp", signal: "generations", wantReason: "ENDPOINT"},
		{name: "HTTP matching extra", extra: map[string]string{"AGENTO11Y_PROTOCOL": "", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "http", signal: "generations", wantGenerations: true, wantSend: true},
		{name: "OTel matching extra with refused primary", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4319"}, sidecar: "otlp", signal: "traces", wantGenerations: true, wantSend: true},
		{name: "OTel matching extra preserves endpoint error", extra: map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "otlp", signal: "traces", wantReason: "OTLP base endpoint", wantSend: true},
		{name: "OTel matching extra preserves opt-in error", extra: map[string]string{"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4319"}, sidecar: "otlp", signal: "traces", wantReason: "ENABLE_EXPERIMENTAL_FEATURES", wantSend: true},
		{name: "HTTP extra preserves protocol error", extra: map[string]string{"AGENTO11Y_PROTOCOL": "unsupported", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "http", signal: "generations", wantReason: "expected http or otel", wantSend: true},
		{name: "matching extra global off", extra: map[string]string{"AGENTO11Y_LOCAL_FORWARD": "false", "AGENTO11Y_PROTOCOL": "", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": ""}, sidecar: "http", signal: "generations"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			t.Setenv("SIGIL_ENABLE_EXPERIMENTAL_FEATURES", "")
			lines := maps.Clone(base)
			maps.Copy(lines, tc.extra)
			path := writeConfigEnvFile(t, lines)
			if tc.sidecar != "" {
				fields := forwardDestinationFields{Name: "extra"}
				if tc.sidecar == "http" {
					fields.Endpoint, fields.TenantID, fields.Token = "https://extra.example.test", "synthetic", "synthetic"
				} else {
					fields.OTLPEndpoint = "https://extra.example.test"
				}
				require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(path), forwardingFile), encodeForwardDestinations(t, fields), 0o600))
			}
			loader := newForwardLoader(path, nil)
			cfg := loader.load()
			status := loader.status()
			require.Equal(t, tc.wantGenerations, status.Generations)
			require.Equal(t, tc.wantHTTP, cfg.genURL != "")
			if tc.wantReason == "" {
				require.Empty(t, status.Reason)
			} else {
				require.Contains(t, status.Reason, tc.wantReason)
			}
			if cfg.generationError != "" {
				require.Equal(t, cfg.generationError, status.Reason)
			}
			if tc.sidecar == "" {
				return
			}
			require.Len(t, status.Destinations, 1)
			require.Equal(t, tc.sidecar == "http", status.Destinations[0].HTTPGenerations)
			require.Equal(t, tc.sidecar == "otlp", status.Destinations[0].OTLP)

			storage, err := NewStorage(t.TempDir())
			require.NoError(t, err)
			receiver := NewServer(storage, nil, path)
			t.Cleanup(receiver.Close)
			calls := make(chan string, 8)
			receiver.forward.client = &http.Client{Transport: dispatchRoundTripper(func(r *http.Request) (*http.Response, error) {
				calls <- r.URL.String()
				response := httptest.NewRecorder()
				if r.URL.Path == wire.GenerationExportHTTPPath {
					writeAcceptedGenerationResponse(response, r)
				} else {
					response.Header().Set("Content-Type", wire.ContentTypeProto)
				}
				return response.Result(), nil
			})}
			body, conversation := destinationHTTPBatch(t, "generation"), "conv-1"
			wantURL := "https://extra.example.test" + wire.GenerationExportHTTPPath
			if tc.signal == "traces" {
				body, conversation = destinationTraceBatch(t, "generation"), "otel-destinations"
				wantURL = "https://extra.example.test/v1/traces"
			}
			sendDestinationIngress(t, receiver, tc.signal, body, false)
			detail, err := storage.ConversationDetail(conversation)
			require.NoError(t, err)
			require.Len(t, detail.Generations, 1)
			if tc.wantSend {
				require.Len(t, calls, 1)
				require.Equal(t, wantURL, <-calls)
			} else {
				require.Empty(t, calls, "local acceptance must not convert the generation transport")
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
