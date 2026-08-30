package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

func TestServerProjectsToolSpans(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		gzip        bool
	}{
		{name: "protobuf", contentType: wire.ContentTypeProto},
		{name: "protobuf gzip", contentType: wire.ContentTypeProto, gzip: true},
		{name: "proto JSON", contentType: wire.ContentTypeJSON},
		{name: "proto JSON gzip", contentType: wire.ContentTypeJSON, gzip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, dir := newTestServer(t)
			request := toolTraceRequest(toolTraceSpan("conv-tools", "call-1", "Bash"))
			body := marshalTraceRequest(t, request, tc.contentType)
			encoding := ""
			if tc.gzip {
				body = gzipBytes(t, body)
				encoding = "gzip"
			}

			req := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(body))
			req.Header.Set("Content-Type", tc.contentType)
			if encoding != "" {
				req.Header.Set("Content-Encoding", encoding)
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)

			path := filepath.Join(dir, ToolSpansDir, "conv-tools.jsonl")
			lines := readLines(t, path)
			require.Len(t, lines, 1)
			var got toolSpanRecord
			require.NoError(t, json.Unmarshal([]byte(lines[0]), &got))
			assert.Equal(t, "01010101010101010101010101010101", got.TraceID)
			assert.Equal(t, "0202020202020202", got.SpanID)
			assert.Equal(t, "0303030303030303", got.ParentSpanID)
			assert.Equal(t, "conv-tools", got.ConversationID)
			assert.Equal(t, "call-1", got.ToolCallID)
			assert.Equal(t, "Bash", got.ToolName)
			assert.Equal(t, "workflow-toolkit:review", got.SkillName)
			assert.True(t, got.Failed)
			assert.Equal(t, "tool_execution_error", got.ErrorType)
			assert.Equal(t, "sdk_error", got.ErrorCategory)
			assert.Equal(t, time.Unix(10, 0).UTC(), got.StartedAt)
			assert.Equal(t, time.Unix(12, 0).UTC(), got.CompletedAt)

			for _, secret := range []string{"secret arguments", "secret result", "secret description", "secret exception", "secret status", "/Users/private/source.go"} {
				assert.NotContains(t, lines[0], secret)
			}
		})
	}
}

func TestProjectToolSpansRejectsMalformedAndOversizedGzip(t *testing.T) {
	_, err := projectToolSpans([]byte("not protobuf"), wire.ContentTypeProto, "")
	assert.Error(t, err)

	body := gzipBytes(t, bytes.Repeat([]byte{0}, maxOTLPBodyBytes+1))
	_, err = projectToolSpans(body, wire.ContentTypeProto, "gzip")
	assert.ErrorContains(t, err, "exceeds")
}

func TestServerToolSpanProjectionIsBestEffortAndForwardingPreservesInput(t *testing.T) {
	type capture struct {
		body            []byte
		contentType     string
		contentEncoding string
	}
	for _, tc := range []struct {
		name string
		body func(*testing.T) []byte
	}{
		{name: "malformed", body: func(*testing.T) []byte { return []byte("not protobuf") }},
		{name: "projection size limit", body: func(t *testing.T) []byte {
			return gzipBytes(t, bytes.Repeat([]byte{0}, maxOTLPBodyBytes+1))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan capture, 1)
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				received <- capture{body: body, contentType: r.Header.Get("Content-Type"), contentEncoding: r.Header.Get("Content-Encoding")}
				w.WriteHeader(http.StatusOK)
			}))
			defer cloud.Close()
			srv, dir := newForwardingTestServer(t, cloud, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD":               "true",
				"AGENTO11Y_ENDPOINT":                    cloud.URL,
				"AGENTO11Y_AUTH_TENANT_ID":              "t",
				"AGENTO11Y_AUTH_TOKEN":                  "k",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL,
				"AGENTO11Y_CONTENT_CAPTURE_MODE":        "full",
			})
			body := tc.body(t)
			req := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(body))
			req.Header.Set("Content-Type", wire.ContentTypeProto)
			if tc.name == "projection size limit" {
				req.Header.Set("Content-Encoding", "gzip")
			}
			rr := httptest.NewRecorder()
			srv.ServeHTTP(rr, req)
			require.Equal(t, http.StatusOK, rr.Code)
			srv.forward.wait()

			got := <-received
			assert.Equal(t, body, got.body)
			assert.Equal(t, wire.ContentTypeProto, got.contentType)
			assert.Equal(t, req.Header.Get("Content-Encoding"), got.contentEncoding)
			_, err := os.Stat(filepath.Join(dir, ToolSpansDir))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestServerSkipsUnsafeToolSpanConversationID(t *testing.T) {
	srv, dir := newTestServer(t)
	body := marshalTraceRequest(t, toolTraceRequest(toolTraceSpan("../escape", "call-1", "Bash")), wire.ContentTypeProto)
	req := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", wire.ContentTypeProto)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)
	_, err := os.Stat(filepath.Join(dir, ToolSpansDir))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(filepath.Join(dir, "escape.jsonl"))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestToolSpanCacheUsesLastDeliveryAndPreservesReusedCallIDs(t *testing.T) {
	storage := newStorage(t)
	first := toolSpanRecord{
		TraceID: "trace-1", SpanID: "span-1", ConversationID: "conv-cache", ToolCallID: "same", ToolName: "Read",
	}
	duplicate := first
	duplicate.ToolName = "Bash"
	other := first
	other.TraceID, other.SpanID, other.ToolName = "trace-2", "span-2", "Write"
	changed, err := storage.appendToolSpans([]toolSpanRecord{first, duplicate, other})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-cache"}, changed)

	files, err := storage.toolSpanFiles()
	require.NoError(t, err)
	require.Len(t, files, 1)
	entry, err := storage.toolSpans.get(files[0])
	require.NoError(t, err)
	persistedDuplicate := duplicate
	persistedDuplicate.DeliveryOrder = 1
	persistedOther := other
	persistedOther.DeliveryOrder = 2
	assert.Equal(t, []toolSpanRecord{persistedDuplicate, persistedOther}, entry.spans)

	cached, err := storage.toolSpans.get(files[0])
	require.NoError(t, err)
	assert.Same(t, entry, cached)

	third := other
	third.TraceID, third.SpanID, third.ToolName = "trace-3", "span-3", "Edit"
	_, err = storage.appendToolSpans([]toolSpanRecord{third})
	require.NoError(t, err)
	files, err = storage.toolSpanFiles()
	require.NoError(t, err)
	refreshed, err := storage.toolSpans.get(files[0])
	require.NoError(t, err)
	assert.NotSame(t, entry, refreshed)
	assert.Len(t, refreshed.spans, 3)
}

func TestToolSpansDedupeGloballyAcrossConversationChangesAndRestart(t *testing.T) {
	storage := newStorage(t)
	first := toolSpanRecord{
		TraceID: "same-trace", SpanID: "same-span", ConversationID: "conv-old", ToolName: "Read",
		StartedAt: time.Unix(10, 0).UTC(),
	}
	changed, err := storage.appendToolSpans([]toolSpanRecord{first})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-old"}, changed)
	oldPath := filepath.Join(storage.Dir(), ToolSpansDir, "conv-old.jsonl")
	require.NoError(t, os.Chtimes(oldPath, time.Unix(1, 0), time.Unix(1, 0)))

	last := first
	last.ConversationID = "conv-new"
	last.ToolName = "Bash"
	changed, err = storage.appendToolSpans([]toolSpanRecord{first, last})
	require.NoError(t, err)
	assert.Equal(t, []string{"conv-new"}, changed, "the final batch occurrence determines its persisted conversation")
	// Updating the old conversation file later must not make its stale copy of
	// the duplicate identity win merely because the file is now newer.
	_, err = storage.appendToolSpans([]toolSpanRecord{{
		TraceID: "other-trace", SpanID: "other-span", ConversationID: "conv-old", ToolName: "Write",
		StartedAt: time.Unix(11, 0).UTC(),
	}})
	require.NoError(t, err)

	restarted, err := NewStorage(storage.Dir())
	require.NoError(t, err)
	observations, err := restarted.toolObservations()
	require.NoError(t, err)
	require.Len(t, observations, 2)
	assert.Equal(t, "conv-new", observations[0].ConversationID)
	assert.Equal(t, "Bash", observations[0].Name)
	assert.Equal(t, "conv-old", observations[1].ConversationID)
	assert.Equal(t, "Write", observations[1].Name)
}

func TestAppendToolSpansEmptyIsNoop(t *testing.T) {
	storage := newStorage(t)
	require.NoError(t, os.WriteFile(filepath.Join(storage.Dir(), ToolSpansDir), []byte("not a directory"), 0o600))

	changed, err := storage.appendToolSpans(nil)
	require.NoError(t, err)
	assert.Empty(t, changed)
}

func TestAppendToolSpansStopsWhenDeliveryOrderIsUnreadable(t *testing.T) {
	first := toolSpanRecord{
		TraceID: "same-trace", SpanID: "same-span", ConversationID: "conv-old", ToolName: "Read",
	}
	for _, tc := range []struct {
		name    string
		prepare func(*testing.T, *Storage)
		record  toolSpanRecord
	}{
		{
			name: "sidecar list",
			prepare: func(t *testing.T, storage *Storage) {
				require.NoError(t, os.WriteFile(filepath.Join(storage.Dir(), ToolSpansDir), nil, 0o600))
			},
			record: first,
		},
		{
			name: "existing sidecar",
			prepare: func(t *testing.T, storage *Storage) {
				_, err := storage.appendToolSpans([]toolSpanRecord{first})
				require.NoError(t, err)
				broken := filepath.Join(storage.Dir(), ToolSpansDir, "conv-broken.jsonl")
				if err := os.Symlink(storage.Dir(), broken); err != nil {
					t.Skipf("create unreadable sidecar: %v", err)
				}
			},
			record: toolSpanRecord{
				TraceID: "same-trace", SpanID: "same-span", ConversationID: "conv-new", ToolName: "Bash",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			storage := newStorage(t)
			tc.prepare(t, storage)
			appendAttempted := false
			storage.openAppend = func(string) (io.WriteCloser, error) {
				appendAttempted = true
				return nil, errors.New("unexpected append")
			}

			changed, err := storage.appendToolSpans([]toolSpanRecord{tc.record})
			assert.Empty(t, changed)
			assert.ErrorContains(t, err, "read tool span order")
			assert.False(t, appendAttempted)
		})
	}
}

func TestAppendToolSpansAttemptsEveryConversationAndJoinsErrors(t *testing.T) {
	storage := newStorage(t)
	storage.openAppend = func(path string) (io.WriteCloser, error) {
		switch filepath.Base(path) {
		case "conv-a.jsonl":
			return nil, errors.New("conv-a unavailable")
		case "conv-c.jsonl":
			return nil, errors.New("conv-c unavailable")
		default:
			return openAppendFile(path)
		}
	}
	records := []toolSpanRecord{
		{TraceID: "trace-a", SpanID: "span-a", ConversationID: "conv-a", ToolName: "Read"},
		{TraceID: "trace-b", SpanID: "span-b", ConversationID: "conv-b", ToolName: "Bash"},
		{TraceID: "trace-c", SpanID: "span-c", ConversationID: "conv-c", ToolName: "Edit"},
	}

	changed, err := storage.appendToolSpans(records)
	assert.Equal(t, []string{"conv-b"}, changed)
	assert.ErrorContains(t, err, "conv-a unavailable")
	assert.ErrorContains(t, err, "conv-c unavailable")
	assert.Len(t, readLines(t, filepath.Join(storage.Dir(), ToolSpansDir, "conv-b.jsonl")), 1)
}

func toolTraceSpan(conversationID, callID, toolName string) *tracepb.Span {
	attributes := []*commonpb.KeyValue{
		stringKeyValue(spanAttrOperationName, "execute_tool"),
		stringKeyValue(spanAttrConversationID, conversationID),
		stringKeyValue(spanAttrToolCallID, callID),
		stringKeyValue(spanAttrToolName, toolName),
		stringKeyValue(spanAttrErrorType, "tool_execution_error"),
		stringKeyValue(spanAttrErrorCategory, "sdk_error"),
		stringKeyValue(spanAttrSkillName, "workflow-toolkit:review"),
		stringKeyValue("gen_ai.tool.description", "secret description"),
		stringKeyValue("gen_ai.tool.call.arguments", "secret arguments"),
		stringKeyValue("gen_ai.tool.call.result", "secret result"),
		stringKeyValue("code.file.path", "/Users/private/source.go"),
	}
	return &tracepb.Span{
		TraceId:           bytes.Repeat([]byte{1}, 16),
		SpanId:            bytes.Repeat([]byte{2}, 8),
		ParentSpanId:      bytes.Repeat([]byte{3}, 8),
		Name:              "execute_tool " + toolName,
		StartTimeUnixNano: uint64(10 * time.Second),
		EndTimeUnixNano:   uint64(12 * time.Second),
		Attributes:        attributes,
		Status:            &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "secret status"},
		Events: []*tracepb.Span_Event{{
			Name: "exception", Attributes: []*commonpb.KeyValue{stringKeyValue("exception.message", "secret exception")},
		}},
	}
}

func toolTraceRequest(spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
	}}}
}

func stringKeyValue(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func marshalTraceRequest(t *testing.T, request *coltracepb.ExportTraceServiceRequest, contentType string) []byte {
	t.Helper()
	var body []byte
	var err error
	if strings.Contains(contentType, "json") {
		body, err = protojson.Marshal(request)
	} else {
		body, err = proto.Marshal(request)
	}
	require.NoError(t, err)
	return body
}
