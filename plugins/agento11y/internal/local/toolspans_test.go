package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

func ingestGenerationTraceSpan() *tracepb.Span {
	return &tracepb.Span{
		TraceId: bytes.Repeat([]byte{4}, 16), SpanId: bytes.Repeat([]byte{5}, 8),
		Name: "chat m1", StartTimeUnixNano: uint64(10 * time.Second), EndTimeUnixNano: uint64(12 * time.Second),
		Attributes: []*commonpb.KeyValue{
			stringKeyValue("agento11y.record", "true"),
			stringKeyValue("agento11y.generation.id", "gen-ingest"),
			stringKeyValue(spanAttrConversationID, "conv-ingest"),
			stringKeyValue(spanAttrOperationName, "chat"),
			stringKeyValue("gen_ai.provider.name", "openai"),
			stringKeyValue("gen_ai.request.model", "m1"),
		},
	}
}

func postIngestTrace(s *Server, body []byte, contentType, encoding string) *httptest.ResponseRecorder {
	req := newLocalRequest(http.MethodPost, otlpTracesPath, bytes.NewReader(body))
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Content-Encoding", encoding)
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	return rr
}

func TestServerOTLPRejectsMarkedJSON(t *testing.T) {
	for _, tc := range []struct {
		name         string
		hexIDs, gzip bool
	}{
		{name: "protojson"}, {name: "protojson gzip", gzip: true},
		{name: "standard hex IDs", hexIDs: true}, {name: "standard hex IDs gzip", hexIDs: true, gzip: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir := newTestServer(t)
			span := ingestGenerationTraceSpan()
			if tc.hexIDs {
				// protojson encodes byte fields as base64. Substituting strings
				// with hex digits mimics standard OTLP JSON on the wire.
				span.TraceId, span.SpanId = nil, nil
			}
			body := marshalTraceRequest(t, toolTraceRequest(span), wire.ContentTypeJSON)
			if tc.hexIDs {
				body = bytes.Replace(body, []byte(`"name":"chat m1"`), []byte(`"traceId":"04040404040404040404040404040404","spanId":"0505050505050505","name":"chat m1"`), 1)
			}
			encoding := ""
			if tc.gzip {
				body, encoding = gzipBytes(t, body), "gzip"
			}
			rr := postIngestTrace(s, body, wire.ContentTypeJSON, encoding)
			require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "require protobuf")
			assertConversationDirEmpty(t, s.storage)
			_, err := os.Stat(filepath.Join(dir, ToolSpansDir))
			assert.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestServerOTLPRejectsOversizedEncodedGenerationBatch(t *testing.T) {
	s, storage, dir := newTestServerStorage(t)
	opener := &countingOpener{}
	storage.openAppend = opener.open
	large := ingestGenerationTraceSpan()
	large.Attributes[1] = stringKeyValue("agento11y.generation.id", "gen-large")
	large.Attributes[2] = stringKeyValue(spanAttrConversationID, "conv-large")
	large.Attributes = append(large.Attributes, stringKeyValue("gen_ai.output.messages",
		`[{"role":"assistant","parts":[{"type":"text","content":"`+strings.Repeat("<", maxJSONLRecordBytes/6)+`"}]}]`))
	request := toolTraceRequest(ingestGenerationTraceSpan(), large, toolTraceSpan("conv-ingest", "call-1", "Bash"))
	body := marshalTraceRequest(t, request, wire.ContentTypeProto)
	require.Less(t, len(body), maxOTLPTraceBodyBytes)
	rr := postIngestTrace(s, body, wire.ContentTypeProto, "")
	require.Equal(t, http.StatusRequestEntityTooLarge, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "encoded JSONL record")
	opens, _ := opener.counts()
	assert.Zero(t, opens, "validate all conversations before appending any")
	assertConversationDirEmpty(t, storage)
	_, err := os.Stat(filepath.Join(dir, ToolSpansDir))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestServerOTLPRejectsOnlyMalformedGenerations(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*tracepb.Span)
		allInvalid bool
	}{
		{name: "short span", mutate: func(s *tracepb.Span) { s.SpanId = []byte{1} }},
		{name: "missing conversation", mutate: func(s *tracepb.Span) { s.Attributes = s.Attributes[:2] }},
		{name: "malformed messages", mutate: func(s *tracepb.Span) {
			s.Attributes = append(s.Attributes, stringKeyValue("gen_ai.input.messages", "{"))
		}},
		{name: "all invalid", allInvalid: true, mutate: func(s *tracepb.Span) { s.SpanId = []byte{1} }},
		{name: "all valid"},
	} {
		for position := range 3 {
			for _, compressed := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/position=%d/gzip=%t", tc.name, position, compressed), func(t *testing.T) {
					received := make(chan *coltracepb.ExportTraceServiceRequest, 1)
					cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						assert.Equal(t, "/v1/traces", r.URL.Path)
						data, err := io.ReadAll(r.Body)
						if err == nil && r.Header.Get("Content-Encoding") == "gzip" {
							data, err = gunzipLimited(data, maxOTLPTraceBodyBytes)
						}
						if !assert.NoError(t, err) {
							return
						}
						var request coltracepb.ExportTraceServiceRequest
						assert.NoError(t, proto.Unmarshal(data, &request))
						received <- &request
					}))
					defer cloud.Close()
					s, dir := newForwardingTestServer(t, cloud, map[string]string{
						"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
						"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
						"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL, "AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
					})
					request := new(coltracepb.ExportTraceServiceRequest)
					var rejected int64
					var wantIDs []string
					for i := range 3 {
						span := ingestGenerationTraceSpan()
						id := fmt.Sprintf("gen-%d", i)
						span.Attributes[1] = stringKeyValue("agento11y.generation.id", id)
						if tc.mutate != nil && (i == position || tc.allInvalid) {
							tc.mutate(span)
							rejected++
						} else {
							wantIDs = append(wantIDs, id)
						}
						request.ResourceSpans = append(request.ResourceSpans, toolTraceRequest(span).ResourceSpans...)
					}
					request.ResourceSpans = append(request.ResourceSpans, toolTraceRequest(toolTraceSpan("conv-ingest", "call-1", "Bash")).ResourceSpans...)
					body := marshalTraceRequest(t, request, wire.ContentTypeProto)
					encoding := ""
					if compressed {
						body, encoding = gzipBytes(t, body), "gzip"
					}
					rr := postIngestTrace(s, body, wire.ContentTypeProto, encoding)
					require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
					require.Equal(t, wire.ContentTypeProto, rr.Header().Get("Content-Type"))
					var response coltracepb.ExportTraceServiceResponse
					require.NoError(t, proto.Unmarshal(rr.Body.Bytes(), &response))
					require.Equal(t, rejected, response.GetPartialSuccess().GetRejectedSpans())
					if rejected == 0 {
						require.Nil(t, response.PartialSuccess)
					} else {
						require.NotEmpty(t, response.PartialSuccess.ErrorMessage)
					}
					if len(wantIDs) == 0 {
						assertConversationDirEmpty(t, s.storage)
					} else {
						detail, err := s.storage.ConversationDetail("conv-ingest")
						require.NoError(t, err)
						var storedIDs []string
						for _, g := range detail.Generations {
							storedIDs = append(storedIDs, g.GenerationID)
						}
						require.Equal(t, wantIDs, storedIDs)
					}
					require.Len(t, readLines(t, filepath.Join(dir, ToolSpansDir, "conv-ingest.jsonl")), 1)
					s.forward.wait()
					require.Len(t, received, 1)
					forwarded := <-received
					var forwardedIDs []string
					var tools int
					for _, resource := range forwarded.ResourceSpans {
						for _, scope := range resource.ScopeSpans {
							for _, span := range scope.Spans {
								if stringAttribute(span.Attributes, "agento11y.record") == "true" {
									forwardedIDs = append(forwardedIDs, stringAttribute(span.Attributes, "agento11y.generation.id"))
								} else {
									tools++
								}
							}
						}
					}
					require.Equal(t, wantIDs, forwardedIDs)
					require.Equal(t, 1, tools)
				})
			}
		}
	}
}

type ingestFailingWriter struct {
	writeErr, closeErr error
	short              bool
}

func (w ingestFailingWriter) Write(data []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.short {
		return len(data) - 1, nil
	}
	return len(data), nil
}
func (w ingestFailingWriter) Close() error { return w.closeErr }

func TestServerOTLPGenerationStorageFailuresAreDetectable(t *testing.T) {
	for _, mode := range []string{"open", "write", "short write", "close"} {
		t.Run(mode, func(t *testing.T) {
			s, storage, dir := newTestServerStorage(t)
			storage.openAppend = func(string) (io.WriteCloser, error) {
				err := errors.New("injected storage failure")
				switch mode {
				case "open":
					return nil, err
				case "write":
					return ingestFailingWriter{writeErr: err}, nil
				case "short write":
					return ingestFailingWriter{short: true}, nil
				default:
					return ingestFailingWriter{closeErr: err}, nil
				}
			}
			sub := s.hub.subscribe()
			defer s.hub.unsubscribe(sub)
			request := toolTraceRequest(ingestGenerationTraceSpan(), toolTraceSpan("conv-ingest", "call-1", "Bash"))
			rr := postIngestTrace(s, marshalTraceRequest(t, request, wire.ContentTypeProto), wire.ContentTypeProto, "")
			require.Equal(t, http.StatusInternalServerError, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Body.String(), "persist generation")
			assert.Empty(t, sub.ch, "failed persistence must not notify success")
			_, err := os.Stat(filepath.Join(dir, ToolSpansDir))
			assert.ErrorIs(t, err, os.ErrNotExist, "tool projection must follow generation persistence")
		})
	}
}

func TestServerOTLPGenerationsAndCompanionToolsCoexist(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", compressed), func(t *testing.T) {
			received := make(chan string, 4)
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.URL.Path
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusOK)
			}))
			defer cloud.Close()
			s, dir := newForwardingTestServer(t, cloud, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud.URL,
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": cloud.URL, "AGENTO11Y_CONTENT_CAPTURE_MODE": "full",
			})
			sub := s.hub.subscribe()
			defer s.hub.unsubscribe(sub)
			request := toolTraceRequest(ingestGenerationTraceSpan(), toolTraceSpan("conv-ingest", "call-1", "Bash"))
			body := marshalTraceRequest(t, request, wire.ContentTypeProto)
			encoding := ""
			if compressed {
				body, encoding = gzipBytes(t, body), "gzip"
			}
			rr := postIngestTrace(s, body, wire.ContentTypeProto, encoding)
			require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
			assert.Empty(t, rr.Body.String())
			assert.Len(t, readLines(t, filepath.Join(dir, ConversationsDir, "conv-ingest.jsonl")), 1)
			assert.Len(t, readLines(t, filepath.Join(dir, ToolSpansDir, "conv-ingest.jsonl")), 1)
			info, err := os.Stat(filepath.Join(dir, ConversationsDir, "conv-ingest.jsonl"))
			require.NoError(t, err)
			assert.WithinDuration(t, time.Unix(12, 0), info.ModTime(), time.Second)
			require.Len(t, sub.ch, 2)
			assert.Equal(t, changeEvent{ConversationID: "conv-ingest", GenerationID: "gen-ingest"}, <-sub.ch)
			restarted, err := NewStorage(dir)
			require.NoError(t, err)
			detail, err := restarted.ConversationDetail("conv-ingest")
			require.NoError(t, err)
			require.Len(t, detail.Generations, 1)
			assert.Equal(t, "gen-ingest", detail.Generations[0].GenerationID)
			s.forward.wait()
			require.Len(t, received, 1, "OTel records must not also forward through HTTP generations")
			assert.Equal(t, "/v1/traces", <-received)
		})
	}
}

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
			span := toolTraceSpan("conv-tools", "call-1", "Bash")
			span.Attributes = append(span.Attributes, intKeyValue("process.exit.code", 0))
			request := toolTraceRequest(span)
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
			assert.Equal(t, "scripts/check.py", got.SkillResourceName)
			assert.Equal(t, "python3", got.ProcessExecutableName)
			require.NotNil(t, got.ProcessExitCode)
			assert.Equal(t, int64(0), *got.ProcessExitCode)
			assert.True(t, got.Failed)
			assert.Equal(t, "tool_execution_error", got.ErrorType)
			assert.Equal(t, "sdk_error", got.ErrorCategory)
			assert.Equal(t, time.Unix(10, 0).UTC(), got.StartedAt)
			assert.Equal(t, time.Unix(12, 0).UTC(), got.CompletedAt)

			for _, secret := range []string{"secret arguments", "secret result", "secret description", "secret exception", "secret status", "/Users/private/source.go", "secret skill description", "https://private.example/skill", "/private/bin/python3"} {
				assert.NotContains(t, lines[0], secret)
			}
		})
	}
}

func TestProjectToolSpansSkillNamespaces(t *testing.T) {
	for _, contentType := range []string{wire.ContentTypeJSON, wire.ContentTypeProto} {
		for _, tc := range []struct {
			name  string
			attrs []*commonpb.KeyValue
			want  string
		}{
			{name: "absent"},
			{name: "legacy", attrs: []*commonpb.KeyValue{stringKeyValue("agento11y.skill.name", " legacy ")}, want: "legacy"},
			{name: "standard", attrs: []*commonpb.KeyValue{stringKeyValue("gen_ai.skill.name", " standard ")}, want: "standard"},
			{name: "conflicting", attrs: []*commonpb.KeyValue{stringKeyValue("agento11y.skill.name", "legacy"), stringKeyValue("gen_ai.skill.name", " standard ")}, want: "standard"},
			{name: "empty standard", attrs: []*commonpb.KeyValue{stringKeyValue("gen_ai.skill.name", ""), stringKeyValue("agento11y.skill.name", " legacy ")}, want: "legacy"},
			{name: "blank standard", attrs: []*commonpb.KeyValue{stringKeyValue("gen_ai.skill.name", " \t\n"), stringKeyValue("agento11y.skill.name", " legacy ")}, want: "legacy"},
			{name: "blank legacy", attrs: []*commonpb.KeyValue{stringKeyValue("gen_ai.skill.name", " \t"), stringKeyValue("agento11y.skill.name", " \n")}},
			{name: "non-string standard", attrs: []*commonpb.KeyValue{intKeyValue("gen_ai.skill.name", 1), stringKeyValue("agento11y.skill.name", " legacy ")}, want: "legacy"},
		} {
			t.Run(contentType+"/"+tc.name, func(t *testing.T) {
				span := toolTraceSpan("conv-names", "call-1", "Read")
				span.Attributes = []*commonpb.KeyValue{
					stringKeyValue("gen_ai.operation.name", "execute_tool"),
					stringKeyValue("gen_ai.conversation.id", "conv-names"),
					stringKeyValue("gen_ai.tool.name", "Read"),
				}
				span.Attributes = append(span.Attributes, tc.attrs...)
				records, err := projectToolSpans(marshalTraceRequest(t, toolTraceRequest(span), contentType), contentType, "")
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, tc.want, records[0].SkillName)
			})
		}
	}
}

func TestToolSpanSkillAndProcessFieldsJSONLRoundTrip(t *testing.T) {
	for _, contentType := range []string{wire.ContentTypeJSON, wire.ContentTypeProto} {
		for _, tc := range []struct {
			name string
			attr *commonpb.KeyValue
			want *int64
		}{
			{name: "absent"},
			{name: "zero", attr: intKeyValue("process.exit.code", 0), want: int64Pointer(0)},
			{name: "negative", attr: intKeyValue("process.exit.code", -1), want: int64Pointer(-1)},
			{name: "nonzero", attr: intKeyValue("process.exit.code", 23), want: int64Pointer(23)},
			{name: "min int64", attr: intKeyValue("process.exit.code", -1<<63), want: int64Pointer(-1 << 63)},
			{name: "max int64", attr: intKeyValue("process.exit.code", 1<<63-1), want: int64Pointer(1<<63 - 1)},
			{name: "string is not integer", attr: stringKeyValue("process.exit.code", "0")},
			{name: "bool is not integer", attr: &commonpb.KeyValue{Key: "process.exit.code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: true}}}},
			{name: "double is not integer", attr: &commonpb.KeyValue{Key: "process.exit.code", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_DoubleValue{DoubleValue: 0}}}},
			{name: "missing value", attr: &commonpb.KeyValue{Key: "process.exit.code"}},
		} {
			t.Run(contentType+"/"+tc.name, func(t *testing.T) {
				span := toolTraceSpan("conv-roundtrip", "call-1", "Bash")
				if tc.attr != nil {
					span.Attributes = append(span.Attributes, tc.attr)
				}
				records, err := projectToolSpans(marshalTraceRequest(t, toolTraceRequest(span), contentType), contentType, "")
				require.NoError(t, err)
				require.Len(t, records, 1)
				assert.Equal(t, tc.want, records[0].ProcessExitCode)
				assert.Equal(t, "workflow-toolkit:review", records[0].SkillName)
				assert.Equal(t, "scripts/check.py", records[0].SkillResourceName)
				assert.Equal(t, "python3", records[0].ProcessExecutableName)

				storage := newStorage(t)
				_, err = storage.appendToolSpans(records)
				require.NoError(t, err)
				path := filepath.Join(storage.Dir(), ToolSpansDir, "conv-roundtrip.jsonl")
				lines := readLines(t, path)
				require.Len(t, lines, 1)
				var fields map[string]json.RawMessage
				require.NoError(t, json.Unmarshal([]byte(lines[0]), &fields))
				assert.JSONEq(t, `"workflow-toolkit:review"`, string(fields["skill_name"]))
				assert.JSONEq(t, `"scripts/check.py"`, string(fields["skill_resource_name"]))
				assert.JSONEq(t, `"python3"`, string(fields["process_executable_name"]))
				if tc.want == nil {
					assert.NotContains(t, fields, "process_exit_code")
				} else {
					var code int64
					require.NoError(t, json.Unmarshal(fields["process_exit_code"], &code))
					assert.Equal(t, *tc.want, code)
				}
				for _, private := range []string{"description", "uri", "path", "secret", "https://private.example/skill", "/private/bin/python3"} {
					assert.NotContains(t, lines[0], private)
				}

				restarted, err := NewStorage(storage.Dir())
				require.NoError(t, err)
				files, err := restarted.toolSpanFiles()
				require.NoError(t, err)
				require.Len(t, files, 1)
				entry, err := restarted.toolSpans.get(files[0])
				require.NoError(t, err)
				records[0].DeliveryOrder = 1
				assert.Equal(t, records, entry.spans)
			})
		}
	}
}

func TestOldToolSpanSidecarsKeepOptionalFieldsAbsent(t *testing.T) {
	storage := newStorage(t)
	path := filepath.Join(storage.Dir(), ToolSpansDir, "conv-old.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	old := []byte("{\"trace_id\":\"trace-1\",\"span_id\":\"span-1\",\"conversation_id\":\"conv-old\",\"tool_name\":\"Bash\",\"skill_name\":\"legacy\"}\n" +
		"{\"trace_id\":\"trace-2\",\"span_id\":\"span-2\",\"conversation_id\":\"conv-old\",\"tool_name\":\"Read\"}\n")
	require.NoError(t, os.WriteFile(path, old, 0o600))
	files, err := storage.toolSpanFiles()
	require.NoError(t, err)
	require.Len(t, files, 1)
	entry, err := storage.toolSpans.get(files[0])
	require.NoError(t, err)
	require.Len(t, entry.spans, 2)
	assert.Equal(t, "legacy", entry.spans[0].SkillName)
	assert.Empty(t, entry.spans[1].SkillName)
	for _, record := range entry.spans {
		assert.Empty(t, record.SkillResourceName)
		assert.Empty(t, record.ProcessExecutableName)
		assert.Nil(t, record.ProcessExitCode)
		encoded, err := json.Marshal(record)
		require.NoError(t, err)
		for _, key := range []string{"skill_resource_name", "process_executable_name", "process_exit_code"} {
			assert.NotContains(t, string(encoded), key)
		}
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, old, data, "reading old sidecars must not backfill or rewrite them")
	_, err = storage.appendToolSpans([]toolSpanRecord{{TraceID: "trace-3", SpanID: "span-3", ConversationID: "conv-old", ToolName: "Write"}})
	require.NoError(t, err)
	data, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.HasPrefix(data, old), "appending must leave old records unchanged")
}

func int64Pointer(value int64) *int64 { return new(value) }

func TestProjectToolSpansRejectsMalformedAndOversizedGzip(t *testing.T) {
	_, err := projectToolSpans([]byte("not protobuf"), wire.ContentTypeProto, "")
	assert.Error(t, err)

	body := gzipBytes(t, bytes.Repeat([]byte{0}, maxOTLPTraceBodyBytes+1))
	_, err = projectToolSpans(body, wire.ContentTypeProto, "gzip")
	assert.ErrorContains(t, err, "exceeds")
}

func TestServerRejectsUndecodableTracesBeforeForwarding(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   func(*testing.T) []byte
		status int
	}{
		{name: "malformed", body: func(*testing.T) []byte { return []byte("not protobuf") }, status: http.StatusBadRequest},
		{name: "projection size limit", body: func(t *testing.T) []byte {
			return gzipBytes(t, bytes.Repeat([]byte{0}, maxOTLPTraceBodyBytes+1))
		}, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan struct{}, 1)
			cloud := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- struct{}{}
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
			require.Equal(t, tc.status, rr.Code)
			srv.forward.wait()
			assert.Empty(t, received)
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
		stringKeyValue(spanAttrSkillName, " workflow-toolkit:review "),
		stringKeyValue("gen_ai.skill.resource.name", " scripts/check.py "),
		stringKeyValue("process.executable.name", " python3 "),
		stringKeyValue("gen_ai.skill.description", "secret skill description"),
		stringKeyValue("gen_ai.skill.source.uri", "https://private.example/skill"),
		stringKeyValue("process.executable.path", "/private/bin/python3"),
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

func intKeyValue(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
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
