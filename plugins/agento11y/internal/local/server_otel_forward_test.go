package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestReceiverOTelGenerationFailureSurvivesToolSuccess(t *testing.T) {
	for _, mode := range []string{"metadata_only", "full"} {
		t.Run(mode, func(t *testing.T) {
			clearForwardEnv(t)
			storage, err := NewStorage(t.TempDir())
			require.NoError(t, err)
			var calls atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", wire.ContentTypeProto)
				if calls.Add(1) == 1 {
					body, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1}})
					_, _ = w.Write(body)
				}
			}))
			defer cloud.Close()
			path := writeConfigEnvFile(t, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD":                "true",
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  cloud.URL,
				"AGENTO11Y_CONTENT_CAPTURE_MODE":         mode,
			})
			receiver := NewServer(storage, nil, path)
			defer receiver.Close()
			receiver.forward.client = cloud.Client()

			generationBatch := func(ids ...string) []byte {
				client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
				for _, id := range ids {
					_, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
						ID: id, ConversationID: "forward-conversation",
						Model: agento11y.ModelRef{Provider: "openai", Name: "model"},
					})
					recorder.SetResult(agento11y.Generation{Input: []agento11y.Message{agento11y.UserTextMessage("local content")}}, nil)
					recorder.End()
				}
				require.NoError(t, client.Flush(context.Background()))
				body, err := proto.Marshal(&transport.request)
				require.NoError(t, err)
				return body
			}
			send := func(body []byte) {
				request := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(gzipBytes(t, body)))
				request.Header.Set("Content-Type", wire.ContentTypeProto)
				request.Header.Set("Content-Encoding", "gzip")
				response := httptest.NewRecorder()
				receiver.ServeHTTP(response, request)
				require.Equal(t, http.StatusOK, response.Code, response.Body.String())
				receiver.forward.wait()
			}
			status := func() forwardStatus {
				response := httptest.NewRecorder()
				receiver.ServeHTTP(response, newLocalRequest(http.MethodGet, "/api/v1/config", nil))
				require.Equal(t, http.StatusOK, response.Code)
				var config configResponse
				require.NoError(t, json.Unmarshal(response.Body.Bytes(), &config))
				return config.ForwardStatus
			}

			send(generationBatch("partially-rejected", "possibly-accepted"))
			failed := status()
			require.True(t, failed.Generations)
			require.Len(t, failed.Failures, 1)
			require.Contains(t, failed.Failures[0].Detail, "rejected_spans=1")
			require.Equal(t, forwardLabelOTLPGenerations, failed.Failures[0].Label)
			require.Empty(t, failed.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			localBefore, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)

			toolBody, err := proto.Marshal(traceRequestForSpans(&tracepb.Span{
				TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{2}, 8),
				StartTimeUnixNano: 1_000_000_000, EndTimeUnixNano: 2_000_000_000,
				Attributes: []*commonpb.KeyValue{
					stringKV("gen_ai.operation.name", "execute_tool"),
					stringKV("gen_ai.tool.name", "read_file"),
					stringKV("gen_ai.conversation.id", "forward-conversation"),
				},
			}))
			require.NoError(t, err)
			send(toolBody)
			afterTool := status()
			require.Equal(t, failed.Failures, afterTool.Failures, "tool-only success must not clear a generation rejection")
			require.Equal(t, failed.Legs[forwardLabelOTLPGenerations], afterTool.Legs[forwardLabelOTLPGenerations])
			require.NotEmpty(t, afterTool.Legs[otlpForwardLabel("traces")].LastSuccessAt)
			localAfter, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)
			require.Equal(t, localBefore, localAfter)
			detail, err := storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 2)
			require.Equal(t, "local content", detail.Generations[0].Input[0].Parts[0].Text)
			require.EqualValues(t, 2, calls.Load(), "status reads must not replay a partially accepted batch")

			send(generationBatch("genuinely-accepted"))
			recovered := status()
			require.Empty(t, recovered.Failures, "generation success must clear its own warning")
			require.NotEmpty(t, recovered.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			require.EqualValues(t, 3, calls.Load())
			detail, err = storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 3)
		})
	}
}

func TestReceiverOTelGenerationForwarding(t *testing.T) {
	for _, tc := range []struct {
		mode     string
		rejected bool
	}{
		{mode: "metadata_only"},
		{mode: "full"},
		{mode: "full_with_metadata_spans"},
		{mode: "no_tool_content"},
		{mode: "metadata_only", rejected: true},
	} {
		t.Run(fmt.Sprintf("%s/rejected=%t", tc.mode, tc.rejected), func(t *testing.T) {
			clearForwardEnv(t)
			storage, err := NewStorage(t.TempDir())
			require.NoError(t, err)
			type traceDelivery struct {
				body      []byte
				persisted bool
				marker    string
			}
			delivered := make(chan traceDelivery, 2)
			var httpGenerations atomic.Int32
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				if r.URL.Path == "/v1/traces" {
					detail, err := storage.ConversationDetail("forward-conversation")
					delivered <- traceDelivery{body: body, persisted: err == nil && len(detail.Generations) == 1 && len(detail.Generations[0].Input) == 1 && len(detail.Generations[0].Input[0].Parts) == 1 && detail.Generations[0].Input[0].Parts[0].Text == "secret input", marker: r.Header.Get(ForwardMarkerHeader)}
				} else if r.URL.Path == wire.GenerationExportHTTPPath {
					httpGenerations.Add(1)
				}
				w.Header().Set("Content-Type", wire.ContentTypeProto)
				w.WriteHeader(http.StatusOK)
				if tc.rejected && r.URL.Path == "/v1/traces" {
					response, _ := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1}})
					_, _ = w.Write(response)
				}
			}))
			defer cloud.Close()
			path := writeConfigEnvFile(t, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD":                "true",
				"AGENTO11Y_PROTOCOL":                     "otel",
				"AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT":  cloud.URL,
				"AGENTO11Y_ENDPOINT":                     cloud.URL,
				"AGENTO11Y_AUTH_TENANT_ID":               "test-tenant",
				"AGENTO11Y_AUTH_TOKEN":                   "test-token",
				"AGENTO11Y_CONTENT_CAPTURE_MODE":         tc.mode,
			})
			receiver := NewServer(storage, log.New(io.Discard, "", 0), path)
			defer receiver.Close()
			receiver.forward.client = cloud.Client()
			client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
			_, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
				ID: "forward-generation", ConversationID: "forward-conversation", ConversationTitle: "secret title",
				SystemPrompt: "secret system", Model: agento11y.ModelRef{Provider: "openai", Name: "model"},
				Metadata: map[string]any{"keep": "non-content", "sigil.conversation.title": "secret legacy title"},
			})
			recorder.SetResult(agento11y.Generation{
				Input:  []agento11y.Message{agento11y.UserTextMessage("secret input")},
				Output: []agento11y.Message{agento11y.AssistantTextMessage("secret output")},
			}, nil)
			recorder.End()
			require.NoError(t, client.Flush(context.Background()))
			body, err := proto.Marshal(&transport.request)
			require.NoError(t, err)
			request := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(gzipBytes(t, body)))
			request.Header.Set("Content-Type", wire.ContentTypeProto)
			request.Header.Set("Content-Encoding", "gzip")
			response := httptest.NewRecorder()
			receiver.ServeHTTP(response, request)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			receiver.forward.wait()
			require.Len(t, delivered, 1)
			cloudCopy := <-delivered
			require.True(t, cloudCopy.persisted, "local persistence must precede Cloud delivery")
			require.Equal(t, "1", cloudCopy.marker)
			require.Zero(t, httpGenerations.Load(), "OTel ingress must not also forward generation HTTP")
			if tc.mode == "full" {
				decoded, err := gunzipLimited(cloudCopy.body, maxOTLPTraceBodyBytes)
				require.NoError(t, err)
				require.Equal(t, body, decoded)
			} else {
				require.NotContains(t, string(cloudCopy.body), "secret")
				var cloudTrace coltracepb.ExportTraceServiceRequest
				require.NoError(t, proto.Unmarshal(cloudCopy.body, &cloudTrace))
				forwarded := cloudTrace.ResourceSpans[0].ScopeSpans[0].Spans[0]
				original := transport.request.ResourceSpans[0].ScopeSpans[0].Spans[0]
				require.Equal(t, original.TraceId, forwarded.TraceId)
				require.Equal(t, original.SpanId, forwarded.SpanId)
				require.Equal(t, original.ParentSpanId, forwarded.ParentSpanId)
				attrs := forwarded.Attributes
				require.Equal(t, "forward-generation", stringAttribute(attrs, "agento11y.generation.id"))
				require.Equal(t, "true", stringAttribute(attrs, "agento11y.record"))
				require.JSONEq(t, `{"keep":"non-content","agento11y.sdk.name":"sdk-go","agento11y.sdk.content_capture_mode":"metadata_only"}`, stringAttribute(attrs, "agento11y.generation.metadata"))
			}
			detail, err := storage.ConversationDetail("forward-conversation")
			require.NoError(t, err)
			require.Equal(t, "secret title", detail.Title)
			require.Len(t, detail.Generations, 1)
			require.Equal(t, "secret output", detail.Generations[0].Output[0].Parts[0].Text)
			data, err := os.ReadFile(filepath.Join(storage.dir, ConversationsDir, "forward-conversation.jsonl"))
			require.NoError(t, err)
			require.Contains(t, string(data), "secret title")
			require.Contains(t, string(data), "secret system")
			require.Contains(t, string(data), "full")
			if tc.rejected {
				status := receiver.forward.status()
				require.Len(t, status.Failures, 1)
				require.Equal(t, forwardLabelOTLPGenerations, status.Failures[0].Label)
				require.Contains(t, status.Failures[0].Detail, "rejected_spans=1")
				require.Empty(t, status.Legs[forwardLabelOTLPGenerations].LastSuccessAt)
			} else {
				require.Empty(t, receiver.forward.status().Failures)
			}

			legacy := contentRichGeneration(t)
			envelope, err := json.Marshal(generationsRequest{Generations: []json.RawMessage{generationRawJSON(t, legacy)}})
			require.NoError(t, err)
			response = httptest.NewRecorder()
			legacyRequest := newLocalRequest(http.MethodPost, wire.GenerationExportHTTPPath, bytes.NewReader(envelope))
			legacyRequest.Header.Set("Content-Type", wire.ContentTypeJSON)
			receiver.ServeHTTP(response, legacyRequest)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			receiver.forward.wait()
			require.EqualValues(t, 1, httpGenerations.Load(), "legacy Pi/OpenCode HTTP ingress remains forwarded under OTel")
			require.Empty(t, delivered)
			if tc.rejected {
				require.Len(t, receiver.forward.status().Failures, 1, "HTTP success must not clear the OTLP rejection")
			}
		})
	}
}
