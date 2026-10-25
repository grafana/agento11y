package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	generationpb "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/grafana/agento11y/plugins/agento11y/internal/emit"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/stretchr/testify/require"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestGenerationSpanSDKToLocalStore(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	s, dir := newTestServer(t)
	defer s.Close()
	t.Setenv("AGENTO11Y_PROTOCOL", "otel")
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
	var generationRequests, traceRequests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/generations:export":
			generationRequests.Add(1)
		case otlpTracesPath:
			traceRequests.Add(1)
		}
		s.ServeHTTP(w, r)
	}))
	defer server.Close()
	t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL+"/otlp")
	providers, err := otel.SetupWithOptions(t.Context(), "local-roundtrip", otel.Options{Endpoint: server.URL + "/otlp", Headers: map[string]string{}})
	require.NoError(t, err)
	require.NotNil(t, providers.GenerationTracerProvider())
	defer func() { require.NoError(t, providers.Shutdown(context.Background())) }()
	client, err := emit.NewClient(emit.ClientOptions{Providers: providers, ContentCapture: agento11y.ContentCaptureModeFull})
	require.NoError(t, err)
	defer func() { require.NoError(t, client.Shutdown(context.Background())) }()
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	start := agento11y.GenerationStart{ID: "roundtrip-gen", ConversationID: "roundtrip-conv", ConversationTitle: "Local conversation", StartedAt: started, AgentName: "codex", Model: agento11y.ModelRef{Provider: "openai", Name: "model"}}
	gen := agento11y.Generation{Output: []agento11y.Message{agento11y.AssistantTextMessage("local content retained")}, Input: []agento11y.Message{agento11y.UserTextMessage("local input")}, CompletedAt: started.Add(time.Second), Usage: agento11y.TokenUsage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}, ParentGenerationIDs: []string{"parent-gen"}}
	require.NoError(t, emit.Record(t.Context(), client, start, gen, nil, nil))
	require.NoError(t, client.Flush(t.Context()))
	require.NoError(t, client.Flush(t.Context()))
	require.Positive(t, traceRequests.Load())
	require.Zero(t, generationRequests.Load())
	data, err := os.ReadFile(filepath.Join(dir, ConversationsDir, "roundtrip-conv.jsonl"))
	require.NoError(t, err)
	var record generationRecord
	require.NoError(t, json.Unmarshal(data, &record))
	var stored generationpb.Generation
	require.NoError(t, protojson.Unmarshal(record.Generation, &stored))
	require.Equal(t, "roundtrip-gen", stored.Id)
	require.Len(t, stored.TraceId, 32)
	require.Len(t, stored.SpanId, 16)
	require.Equal(t, started, stored.StartedAt.AsTime())
	require.Equal(t, []string{"parent-gen"}, stored.ParentGenerationIds)
	require.Contains(t, string(record.Generation), "local content retained")
	reopened, err := NewStorage(dir)
	require.NoError(t, err)
	detail, err := reopened.ConversationDetail("roundtrip-conv")
	require.NoError(t, err)
	require.Equal(t, "Local conversation", detail.Title)
	require.Len(t, detail.Generations, 1)
	require.Equal(t, int64(10), detail.Generations[0].TotalTokens)
	require.Equal(t, "local input", detail.Generations[0].Input[0].Parts[0].Text)
	require.Equal(t, "local content retained", detail.Generations[0].Output[0].Parts[0].Text)
	restored := NewServer(reopened, log.New(io.Discard, "", 0), "")
	defer restored.Close()
	req := newLocalRequest("GET", "/api/v1/conversations/roundtrip-conv", nil)
	rr := httptest.NewRecorder()
	restored.ServeHTTP(rr, req)
	require.Equal(t, 200, rr.Code)
	require.Contains(t, rr.Body.String(), "local content retained")
}

func TestGenerationSpanSDKOpaqueBatchPersists(t *testing.T) {
	client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
	plain := []byte(`{"ok":true}`)
	duplicate := []byte(`{"x":1,"x":2,"nested":[{"n":9007199254740993,"n":1e+02}]}`)
	whitespace := append([]byte(" \n"), append(bytes.Clone(duplicate), '\t')...)
	generationSpanRecordPayloads(t, client, "before", "opaque-batch", plain, plain, plain)
	generationSpanRecordPayloads(t, client, "duplicate", "opaque-batch", duplicate, duplicate, duplicate)
	generationSpanRecordPayloads(t, client, "base64", "opaque-batch", whitespace, whitespace, whitespace)
	generationSpanRecordPayloads(t, client, "after", "opaque-batch", plain, plain, plain)
	require.NoError(t, client.Flush(t.Context()))
	s, dir := newTestServer(t)
	defer s.Close()
	wire, err := proto.Marshal(&transport.request)
	require.NoError(t, err)
	resp := postBytes(t, s, "/otlp/v1/traces", "application/x-protobuf", wire)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, 200, resp.StatusCode, "%s", body)
	data, err := os.ReadFile(filepath.Join(dir, ConversationsDir, "opaque-batch.jsonl"))
	require.NoError(t, err)
	decoder := json.NewDecoder(bytes.NewReader(data))
	for i, want := range [][]byte{plain, duplicate, whitespace, plain} {
		var record generationRecord
		require.NoError(t, decoder.Decode(&record))
		var stored generationpb.Generation
		require.NoError(t, protojson.Unmarshal(record.Generation, &stored))
		require.Equal(t, []string{"before", "duplicate", "base64", "after"}[i], stored.Id)
		generationSpanRequirePayloads(t, &stored, want, want, want)
	}
	var extra generationRecord
	require.ErrorIs(t, decoder.Decode(&extra), io.EOF)
	reopened, err := NewStorage(dir)
	require.NoError(t, err)
	listed, total, err := reopened.ListConversations(ConversationListOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, total)
	require.Len(t, listed, 1)
	require.Equal(t, 4, listed[0].Calls)
}

func TestGenerationSpanSDKCallErrorPersists(t *testing.T) {
	for _, tc := range []struct {
		name        string
		callError   error
		wantStatus  string
		wantMessage string
	}{
		{name: "empty description", callError: errors.New(""), wantStatus: "err"},
		{name: "nonempty description", callError: errors.New("provider failed"), wantStatus: "err", wantMessage: "provider failed"},
		{name: "success", wantStatus: "ok"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, _, _, transport := generationSpanTestClient(t, agento11y.ContentCaptureModeFull)
			_, rec := client.StartGeneration(t.Context(), agento11y.GenerationStart{
				ID: "call-error", ConversationID: "call-error-conversation", Model: agento11y.ModelRef{Provider: "custom", Name: "model"},
			})
			rec.SetCallError(tc.callError)
			rec.End()
			require.NoError(t, rec.Err())
			require.NoError(t, client.Flush(t.Context()))
			span := generationSpanFind(t, &transport.request, "call-error")
			if tc.callError != nil {
				require.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
				require.Equal(t, tc.callError.Error(), span.GetStatus().GetMessage())
			} else {
				require.NotEqual(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
			}
			s, dir := newTestServer(t)
			defer s.Close()
			wire, err := proto.Marshal(&transport.request)
			require.NoError(t, err)
			resp := postBytes(t, s, "/otlp/v1/traces", "application/x-protobuf", wire)
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			data, err := os.ReadFile(filepath.Join(dir, ConversationsDir, "call-error-conversation.jsonl"))
			require.NoError(t, err)
			var record generationRecord
			require.NoError(t, json.Unmarshal(data, &record))
			var stored generationpb.Generation
			require.NoError(t, protojson.Unmarshal(record.Generation, &stored))
			if tc.callError != nil {
				require.NotEmpty(t, stored.CallError)
				require.Equal(t, stored.CallError, stored.Metadata.Fields["call_error"].GetStringValue())
				if tc.wantMessage != "" {
					require.Equal(t, tc.wantMessage, stored.CallError)
				}
			} else {
				require.Empty(t, stored.CallError)
				require.NotContains(t, stored.GetMetadata().GetFields(), "call_error")
			}
			reopened, err := NewStorage(dir)
			require.NoError(t, err)
			listed, _, err := reopened.ListConversations(ConversationListOptions{})
			require.NoError(t, err)
			require.Len(t, listed, 1)
			require.Equal(t, tc.wantStatus, listed[0].Status)
			detail, err := reopened.ConversationDetail("call-error-conversation")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 1)
			require.Equal(t, stored.CallError, detail.Generations[0].CallError)
		})
	}
}
