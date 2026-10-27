package entry

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	colmetricpb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	metricpb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type agentOTLPCapture struct {
	server     *httptest.Server
	mu         sync.Mutex
	traceReqs  []*coltracepb.ExportTraceServiceRequest
	metricReqs []*colmetricpb.ExportMetricsServiceRequest
	errors     []error
}

func newAgentOTLPCapture(t *testing.T) *agentOTLPCapture {
	t.Helper()
	c := &agentOTLPCapture{}
	c.server = newLoopbackServer(t, http.HandlerFunc(c.serveHTTP))
	return c
}

func (c *agentOTLPCapture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	reject := func(status int, err error) {
		c.mu.Lock()
		c.errors = append(c.errors, fmt.Errorf("%s: %w", r.URL.Path, err))
		c.mu.Unlock()
		http.Error(w, err.Error(), status)
	}
	var message proto.Message
	switch r.URL.Path {
	case "/v1/traces":
		message = &coltracepb.ExportTraceServiceRequest{}
	case "/v1/metrics":
		message = &colmetricpb.ExportMetricsServiceRequest{}
	default:
		reject(http.StatusNotFound, errors.New("unknown OTLP route"))
		return
	}
	if r.Method != http.MethodPost {
		reject(http.StatusMethodNotAllowed, errors.New("OTLP requires POST"))
		return
	}
	var reader io.Reader = r.Body
	switch r.Header.Get("Content-Encoding") {
	case "", "identity":
	case "gzip":
		gz, err := gzip.NewReader(r.Body)
		if err != nil {
			reject(http.StatusBadRequest, fmt.Errorf("decode gzip: %w", err))
			return
		}
		defer gz.Close()
		reader = gz
	default:
		reject(http.StatusUnsupportedMediaType, errors.New("unsupported content encoding"))
		return
	}
	body, err := io.ReadAll(reader)
	if err != nil {
		reject(http.StatusBadRequest, fmt.Errorf("read OTLP body: %w", err))
		return
	}
	if err := proto.Unmarshal(body, message); err != nil {
		reject(http.StatusBadRequest, fmt.Errorf("decode protobuf: %w", err))
		return
	}
	c.mu.Lock()
	switch request := message.(type) {
	case *coltracepb.ExportTraceServiceRequest:
		c.traceReqs = append(c.traceReqs, request)
	case *colmetricpb.ExportMetricsServiceRequest:
		c.metricReqs = append(c.metricReqs, request)
	}
	c.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

func (c *agentOTLPCapture) snapshot() ([]*coltracepb.ExportTraceServiceRequest, []*colmetricpb.ExportMetricsServiceRequest, []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	traces := make([]*coltracepb.ExportTraceServiceRequest, len(c.traceReqs))
	for i, request := range c.traceReqs {
		traces[i] = proto.Clone(request).(*coltracepb.ExportTraceServiceRequest)
	}
	metrics := make([]*colmetricpb.ExportMetricsServiceRequest, len(c.metricReqs))
	for i, request := range c.metricReqs {
		metrics[i] = proto.Clone(request).(*colmetricpb.ExportMetricsServiceRequest)
	}
	return traces, metrics, append([]error(nil), c.errors...)
}

func (c *agentOTLPCapture) validate(agent string) error {
	wantGenerations, provider, tool := 1, "openai", "Bash"
	switch agent {
	case "claude-code":
		wantGenerations, provider, tool = 2, "anthropic", "Bash"
	case "codex":
	default:
		return fmt.Errorf("unsupported agent %q", agent)
	}
	traces, metrics, errs := c.snapshot()
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("OTLP receiver: %w", err)
	}
	identity := map[string]string{
		"gen_ai.agent.name": agent, "gen_ai.provider.name": provider, "gen_ai.request.model": fakeModelName,
		"agento11y.tag.user": fixtureEmail, "agento11y.tag.repo": fixtureRepo, "agento11y.tag.git.branch": fixtureBranch,
	}
	generations, tools := 0, 0
	seen := map[string]bool{}
	for _, request := range traces {
		if err := agentOTLPNoContent(request); err != nil {
			return fmt.Errorf("trace export: %w", err)
		}
		for _, resource := range request.GetResourceSpans() {
			for _, scope := range resource.GetScopeSpans() {
				for _, span := range scope.GetSpans() {
					if err := agentOTLPWantAttrs(span.GetAttributes(), identity); err != nil {
						return fmt.Errorf("span %q: %w", span.GetName(), err)
					}
					if len(span.GetTraceId()) != 16 || len(span.GetSpanId()) != 8 || bytes.Equal(span.GetTraceId(), make([]byte, 16)) || bytes.Equal(span.GetSpanId(), make([]byte, 8)) {
						return fmt.Errorf("span %q has invalid trace/span ID", span.GetName())
					}
					if span.StartTimeUnixNano == 0 || span.EndTimeUnixNano < span.StartTimeUnixNano {
						return fmt.Errorf("span %q has invalid timestamps", span.Name)
					}
					attrs := agentOTLPAttrs(span.Attributes)
					switch attrs["gen_ai.operation.name"] {
					case "execute_tool":
						if err := agentOTLPWantAttrs(span.Attributes, map[string]string{
							"gen_ai.tool.name": tool, "gen_ai.tool.call.id": fakeToolCallID, "gen_ai.tool.type": "function",
						}); err != nil {
							return fmt.Errorf("tool span: %w", err)
						}
						tools++
					case "generateText", "streamText", "chat":
						id := attrs["agento11y.generation.id"]
						if id == "" || seen[id] {
							return fmt.Errorf("missing or duplicate generation ID %q", id)
						}
						seen[id] = true
						if attrs["user.id"] != fixtureEmail {
							return fmt.Errorf("generation %q identity user.id = %q, want %q", id, attrs["user.id"], fixtureEmail)
						}
						generations++
					default:
						return fmt.Errorf("unexpected span operation %q", attrs["gen_ai.operation.name"])
					}
				}
			}
		}
	}
	if generations != wantGenerations || tools != 1 {
		return fmt.Errorf("span counts: generations=%d tools=%d, want %d and 1", generations, tools, wantGenerations)
	}
	peakTokenCounts := map[string]uint64{}
	for _, request := range metrics {
		if err := agentOTLPNoContent(request); err != nil {
			return fmt.Errorf("metric export: %w", err)
		}
		for _, resource := range request.GetResourceMetrics() {
			for _, scope := range resource.GetScopeMetrics() {
				for _, metric := range scope.GetMetrics() {
					if metric.GetName() != "gen_ai.client.token.usage" {
						continue
					}
					histogram := metric.GetHistogram()
					if histogram == nil || len(histogram.DataPoints) == 0 {
						return errors.New("token usage requires histogram datapoints")
					}
					for _, point := range histogram.DataPoints {
						// user.id identifies spans, not metric series. The user tag is a metric dimension.
						if err := agentOTLPWantAttrs(point.GetAttributes(), identity); err != nil {
							return fmt.Errorf("token usage datapoint: %w", err)
						}
						tokenType := agentOTLPAttrs(point.Attributes)["gen_ai.token.type"]
						value := float64(0)
						switch tokenType {
						case "input":
							value = 10
						case "output":
							value = 5
						default:
							return fmt.Errorf("unexpected token type %q", tokenType)
						}
						if agent == "codex" {
							value *= 2
						}
						if point.Count == 0 || point.Count > uint64(wantGenerations) || point.Sum == nil || point.GetSum() != value*float64(point.Count) || point.Flags != 0 {
							return fmt.Errorf("incorrect %s token usage count/sum: count=%d sum=%v", tokenType, point.Count, point.Sum)
						}
						peakTokenCounts[tokenType] = max(peakTokenCounts[tokenType], point.Count)
					}
				}
			}
		}
	}
	// ForceFlush and Shutdown can both export the same cumulative series, so summing exports would double-count observations.
	if peakTokenCounts["input"] != uint64(wantGenerations) || peakTokenCounts["output"] != uint64(wantGenerations) {
		return fmt.Errorf("missing input/output token usage observations: counts=%v, want %d each", peakTokenCounts, wantGenerations)
	}
	return nil
}

func agentOTLPAttrs(attrs []*commonpb.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		out[attr.GetKey()] = attr.GetValue().GetStringValue()
	}
	return out
}

func agentOTLPWantAttrs(attrs []*commonpb.KeyValue, want map[string]string) error {
	seen := map[string]bool{}
	for _, attr := range attrs {
		if seen[attr.GetKey()] {
			return fmt.Errorf("duplicate attribute %q", attr.GetKey())
		}
		seen[attr.GetKey()] = true
	}
	got := agentOTLPAttrs(attrs)
	for key, value := range want {
		if got[key] != value {
			return fmt.Errorf("attribute %s = %q, want %q", key, got[key], value)
		}
	}
	return nil
}

func agentOTLPContentKey(key string) bool {
	switch key {
	case "gen_ai.system_instructions", "gen_ai.input.messages", "gen_ai.output.messages", "gen_ai.tool.definitions",
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result", "gen_ai.tool.description", "gen_ai.embeddings.input_texts",
		"gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.path",
		"gen_ai.retrieval.query.text", "gen_ai.retrieval.documents", "agento11y.generation.raw_artifacts",
		"agento11y.conversation.title", "sigil.conversation.title", "call_error", "exception.message", "exception.stacktrace":
		return true
	}
	return false
}

func agentOTLPNoContent(message proto.Message) error {
	var walk func(protoreflect.Message) error
	walk = func(m protoreflect.Message) error {
		if m.Descriptor().FullName() == "opentelemetry.proto.common.v1.KeyValue" {
			key := m.Get(m.Descriptor().Fields().ByName("key")).String()
			if agentOTLPContentKey(key) {
				return fmt.Errorf("content attribute %q", key)
			}
		}
		var err error
		m.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			check := func(v protoreflect.Value) error {
				switch field.Kind() {
				case protoreflect.MessageKind:
					return walk(v.Message())
				case protoreflect.StringKind:
					return agentOTLPCheckText(v.String(), 0)
				case protoreflect.BytesKind:
					return agentOTLPCheckText(string(v.Bytes()), 0)
				default:
					return nil
				}
			}
			if field.IsList() {
				list := value.List()
				for i := 0; i < list.Len() && err == nil; i++ {
					err = check(list.Get(i))
				}
			} else {
				err = check(value)
			}
			return err == nil
		})
		return err
	}
	return walk(message.ProtoReflect())
}

func agentOTLPCheckText(text string, depth int) error {
	for _, forbidden := range []string{"Run echo", fakeToolOutput, "Done.", "glc_abcdefghijklmnopqrstuvwxyz"} {
		if strings.Contains(text, forbidden) {
			return fmt.Errorf("leaked ordinary content %q", forbidden)
		}
	}
	if depth >= 8 {
		return errors.New("cannot inspect deeply encoded content")
	}
	if raw, err := base64.StdEncoding.DecodeString(text); err == nil && len(raw) > 0 {
		if err := agentOTLPCheckText(string(raw), depth+1); err != nil {
			return err
		}
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) != nil {
		return nil
	}
	var walk func(any) error
	walk = func(value any) error {
		switch v := value.(type) {
		case string:
			return agentOTLPCheckText(v, depth+1)
		case []any:
			for _, item := range v {
				if err := walk(item); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, item := range v {
				if agentOTLPContentKey(key) {
					return fmt.Errorf("content payload key %q", key)
				}
				if err := agentOTLPCheckText(key, depth+1); err != nil {
					return err
				}
				if err := walk(item); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return walk(decoded)
}

func TestAgentOTLPCaptureReceiver(t *testing.T) {
	for _, signal := range []string{"traces", "metrics"} {
		for _, compressed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/gzip=%t", signal, compressed), func(t *testing.T) {
				traces, metrics := agentOTLPFixture("claude-code")
				var message proto.Message = traces
				if signal == "metrics" {
					message = metrics
				}
				body, err := proto.Marshal(message)
				require.NoError(t, err)
				encoding := ""
				if compressed {
					body, encoding = agentOTLPGzip(t, body), "gzip"
				}
				capture := newAgentOTLPCapture(t)
				agentOTLPPost(t, capture, "/v1/"+signal, encoding, body, http.StatusOK)
				gotTraces, gotMetrics, errs := capture.snapshot()
				require.Empty(t, errs)
				if signal == "traces" {
					require.Empty(t, gotMetrics)
					require.Len(t, gotTraces, 1)
					require.True(t, proto.Equal(traces, gotTraces[0]))
					gotTraces[0].ResourceSpans = nil
				} else {
					require.Empty(t, gotTraces)
					require.Len(t, gotMetrics, 1)
					require.True(t, proto.Equal(metrics, gotMetrics[0]))
					gotMetrics[0].ResourceMetrics = nil
				}
				gotTraces, gotMetrics, _ = capture.snapshot()
				if signal == "traces" {
					require.True(t, proto.Equal(traces, gotTraces[0]), "snapshot must not alias stored protobufs")
				} else {
					require.True(t, proto.Equal(metrics, gotMetrics[0]), "snapshot must not alias stored protobufs")
				}
			})
		}
	}
}

func TestAgentOTLPCaptureRejectsRequests(t *testing.T) {
	for _, route := range []string{"/v1/traces", "/v1/metrics"} {
		for _, tc := range []struct {
			name, encoding string
			body           []byte
			status         int
		}{
			{name: "malformed protobuf", body: []byte{0xff}, status: http.StatusBadRequest},
			{name: "malformed gzip", encoding: "gzip", body: []byte("invalid"), status: http.StatusBadRequest},
			{name: "gzipped malformed protobuf", encoding: "gzip", body: agentOTLPGzip(t, []byte{0xff}), status: http.StatusBadRequest},
			{name: "truncated gzip", encoding: "gzip", body: agentOTLPGzip(t, nil)[:12], status: http.StatusBadRequest},
			{name: "unsupported encoding", encoding: "br", status: http.StatusUnsupportedMediaType},
		} {
			t.Run(route+"/"+tc.name, func(t *testing.T) {
				capture := newAgentOTLPCapture(t)
				agentOTLPPost(t, capture, route, tc.encoding, tc.body, tc.status)
				traces, metrics, errs := capture.snapshot()
				require.Empty(t, traces)
				require.Empty(t, metrics)
				require.Len(t, errs, 1)
				require.ErrorContains(t, capture.validate("claude-code"), "OTLP receiver")
			})
		}
	}
	for _, route := range []string{"/wrong", "/v1/logs", "/api/v1/generations:export"} {
		t.Run("unknown route "+route, func(t *testing.T) {
			capture := newAgentOTLPCapture(t)
			agentOTLPPost(t, capture, route, "", nil, http.StatusNotFound)
			traces, metrics, errs := capture.snapshot()
			require.Empty(t, traces)
			require.Empty(t, metrics)
			require.Len(t, errs, 1)
			require.ErrorContains(t, errs[0], "unknown OTLP route")
		})
	}
}

func agentOTLPGzip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, err := writer.Write(body)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func agentOTLPPost(t *testing.T, capture *agentOTLPCapture, path, encoding string, body []byte, status int) {
	t.Helper()
	request, err := http.NewRequest(http.MethodPost, capture.server.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Content-Type", "application/x-protobuf")
	request.Header.Set("Content-Encoding", encoding)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Do(request)
	require.NoError(t, err)
	defer response.Body.Close()
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.Equal(t, status, response.StatusCode)
}

func TestAgentOTLPCaptureConcurrentSnapshots(t *testing.T) {
	capture := newAgentOTLPCapture(t)
	traces, metrics := agentOTLPFixture("codex")
	traceBody, err := proto.Marshal(traces)
	require.NoError(t, err)
	metricBody, err := proto.Marshal(metrics)
	require.NoError(t, err)
	const requests = 8
	results := make(chan error, requests)
	client := &http.Client{Timeout: 5 * time.Second}
	for i := range requests {
		go func() {
			path, body := "/v1/traces", traceBody
			if i%2 != 0 {
				path, body = "/v1/metrics", metricBody
			}
			response, err := client.Post(capture.server.URL+path, "application/x-protobuf", bytes.NewReader(body))
			if err == nil {
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("HTTP status %d", response.StatusCode)
				}
			}
			traces, metrics, _ := capture.snapshot()
			for _, request := range traces {
				request.ResourceSpans = nil
			}
			for _, request := range metrics {
				request.ResourceMetrics = nil
			}
			results <- err
		}()
	}
	for range requests {
		require.NoError(t, <-results)
	}
	gotTraces, gotMetrics, errs := capture.snapshot()
	require.Empty(t, errs)
	require.Len(t, gotTraces, requests/2)
	require.Len(t, gotMetrics, requests/2)
	for _, request := range gotTraces {
		require.True(t, proto.Equal(traces, request))
	}
	for _, request := range gotMetrics {
		require.True(t, proto.Equal(metrics, request))
	}
}

func TestAgentOTLPNoContent(t *testing.T) {
	value := func(text string) *commonpb.AnyValue {
		return &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: text}}
	}
	for _, tc := range []struct {
		name    string
		message proto.Message
		wantErr string
	}{
		{name: "safe metadata", message: &commonpb.KeyValue{Key: "agento11y.generation.metadata", Value: value(`{"agento11y.sdk.content_capture_mode":"metadata_only"}`)}},
		{name: "empty content payload", message: &commonpb.KeyValue{Key: "gen_ai.input.messages", Value: value("")}, wantErr: "content attribute"},
		{name: "event", message: &tracepb.Span_Event{Name: "custom", Attributes: []*commonpb.KeyValue{{Key: "custom", Value: value(fakeToolOutput)}}}, wantErr: "ordinary content"},
		{name: "status", message: &tracepb.Status{Message: "Done."}, wantErr: "ordinary content"},
		{name: "scope", message: &commonpb.InstrumentationScope{Name: "Run echo"}, wantErr: "ordinary content"},
		{name: "metric exemplar", message: &metricpb.Exemplar{FilteredAttributes: []*commonpb.KeyValue{{Key: "custom", Value: value(fakeToolOutput)}}}, wantErr: "ordinary content"},
		{name: "nested JSON", message: &commonpb.KeyValue{Key: "custom", Value: value(`{"nested":{"agento11y.conversation.title":"ordinary title"}}`)}, wantErr: "content payload key"},
		{name: "base64", message: &commonpb.KeyValue{Key: "custom", Value: value(base64.StdEncoding.EncodeToString([]byte(fakeToolOutput)))}, wantErr: "ordinary content"},
		{name: "JSON base64", message: &commonpb.KeyValue{Key: "custom", Value: value(`{"payload":"` + base64.StdEncoding.EncodeToString([]byte("Done.")) + `"}`)}, wantErr: "ordinary content"},
		{name: "bytes", message: &commonpb.AnyValue{Value: &commonpb.AnyValue_BytesValue{BytesValue: []byte("glc_abcdefghijklmnopqrstuvwxyz")}}, wantErr: "ordinary content"},
		{name: "nested attributes", message: &commonpb.AnyValue{Value: &commonpb.AnyValue_KvlistValue{KvlistValue: &commonpb.KeyValueList{Values: []*commonpb.KeyValue{{Key: "gen_ai.tool.call.result", Value: value("ordinary result")}}}}}, wantErr: "content attribute"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := agentOTLPNoContent(tc.message)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.wantErr)
			}
		})
	}
}

func agentOTLPFixture(agent string) (*coltracepb.ExportTraceServiceRequest, *colmetricpb.ExportMetricsServiceRequest) {
	count, provider, tool, input, output := 1, "openai", "Bash", float64(20), float64(10)
	if agent == "claude-code" {
		count, provider, tool, input, output = 2, "anthropic", "Bash", 10, 5
	}
	attr := func(key, value string) *commonpb.KeyValue {
		return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
	}
	identity := func() []*commonpb.KeyValue {
		return []*commonpb.KeyValue{
			attr("gen_ai.agent.name", agent), attr("gen_ai.provider.name", provider), attr("gen_ai.request.model", fakeModelName),
			attr("agento11y.tag.user", fixtureEmail), attr("agento11y.tag.repo", fixtureRepo), attr("agento11y.tag.git.branch", fixtureBranch),
		}
	}
	var spans []*tracepb.Span
	for i := range count + 1 {
		span := &tracepb.Span{
			Name: "fixture", TraceId: bytes.Repeat([]byte{1}, 16), SpanId: bytes.Repeat([]byte{byte(i + 1)}, 8),
			StartTimeUnixNano: 100, EndTimeUnixNano: 200, Attributes: identity(),
		}
		if i < count {
			span.Attributes = append(span.Attributes, attr("gen_ai.operation.name", "generateText"), attr("agento11y.generation.id", fmt.Sprintf("gen-%d", i)), attr("user.id", fixtureEmail))
		} else {
			span.Attributes = append(span.Attributes, attr("gen_ai.operation.name", "execute_tool"), attr("gen_ai.tool.name", tool), attr("gen_ai.tool.call.id", fakeToolCallID), attr("gen_ai.tool.type", "function"))
		}
		spans = append(spans, span)
	}
	traces := &coltracepb.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}}}}}
	var points []*metricpb.HistogramDataPoint
	for _, token := range []struct {
		name  string
		value float64
	}{{"input", input}, {"output", output}} {
		sum := token.value * float64(count)
		points = append(points, &metricpb.HistogramDataPoint{
			Attributes: append(identity(), attr("gen_ai.token.type", token.name)), Count: uint64(count), Sum: &sum,
		})
	}
	metrics := &colmetricpb.ExportMetricsServiceRequest{ResourceMetrics: []*metricpb.ResourceMetrics{{ScopeMetrics: []*metricpb.ScopeMetrics{{Metrics: []*metricpb.Metric{{
		Name: "gen_ai.client.token.usage", Data: &metricpb.Metric_Histogram{Histogram: &metricpb.Histogram{DataPoints: points}},
	}}}}}}}
	return traces, metrics
}

func TestAgentOTLPValidate(t *testing.T) {
	for _, agent := range []string{"claude-code", "codex"} {
		for _, tc := range []struct {
			name    string
			mutate  func(*coltracepb.ExportTraceServiceRequest, *colmetricpb.ExportMetricsServiceRequest)
			wantErr string
		}{
			{name: "valid without metric or tool user.id"},
			{name: "missing generation", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceSpans[0].ScopeSpans[0].Spans = r.ResourceSpans[0].ScopeSpans[0].Spans[1:]
			}, wantErr: "span counts"},
			{name: "wrong last span tag", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				spans := r.ResourceSpans[0].ScopeSpans[0].Spans
				spans[len(spans)-1].Attributes[4].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "agento11y.tag.repo"},
			{name: "wrong identity", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				attrs := r.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes
				attrs[len(attrs)-1].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "identity"},
			{name: "model tool name", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				spans := r.ResourceSpans[0].ScopeSpans[0].Spans
				attrs := spans[len(spans)-1].Attributes
				attrs[len(attrs)-3].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "exec_command"}
			}, wantErr: "gen_ai.tool.name"},
			{name: "wrong tool call ID", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				spans := r.ResourceSpans[0].ScopeSpans[0].Spans
				attrs := spans[len(spans)-1].Attributes
				attrs[len(attrs)-2].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "gen_ai.tool.call.id"},
			{name: "missing tool", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				scope := r.ResourceSpans[0].ScopeSpans[0]
				scope.Spans = scope.Spans[:len(scope.Spans)-1]
			}, wantErr: "span counts"},
			{name: "missing generation ID", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				span := r.ResourceSpans[0].ScopeSpans[0].Spans[0]
				span.Attributes = append(span.Attributes[:7], span.Attributes[8:]...)
			}, wantErr: "generation ID"},
			{name: "tool content", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				spans := r.ResourceSpans[0].ScopeSpans[0].Spans
				spans[len(spans)-1].Attributes = append(spans[len(spans)-1].Attributes, &commonpb.KeyValue{Key: "gen_ai.tool.call.arguments"})
			}, wantErr: "content attribute"},
			{name: "resource leak", mutate: func(r *coltracepb.ExportTraceServiceRequest, _ *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceSpans[0].Resource = &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "custom", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: fakeToolOutput}}}}}
			}, wantErr: "ordinary content"},
			{name: "missing metrics", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics = nil
			}, wantErr: "missing input/output"},
			{name: "wrong last datapoint tag", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Attributes[5].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "agento11y.tag.git.branch"},
			{name: "wrong last datapoint agent", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Attributes[0].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "gen_ai.agent.name"},
			{name: "wrong last datapoint model", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Attributes[2].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "gen_ai.request.model"},
			{name: "zero datapoint count", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Count = 0
			}, wantErr: "count/sum"},
			{name: "wrong token type", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Attributes[6].Value.Value = &commonpb.AnyValue_StringValue{StringValue: "wrong"}
			}, wantErr: "token type"},
			{name: "missing output observations", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				histogram := r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram()
				histogram.DataPoints = histogram.DataPoints[:1]
			}, wantErr: "missing input/output"},
			{name: "wrong token sum", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				*r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].GetHistogram().DataPoints[1].Sum++
			}, wantErr: "count/sum"},
			{name: "not a histogram", mutate: func(_ *coltracepb.ExportTraceServiceRequest, r *colmetricpb.ExportMetricsServiceRequest) {
				r.ResourceMetrics[0].ScopeMetrics[0].Metrics[0].Data = &metricpb.Metric_Sum{Sum: &metricpb.Sum{}}
			}, wantErr: "histogram datapoints"},
		} {
			t.Run(agent+"/"+tc.name, func(t *testing.T) {
				traces, metrics := agentOTLPFixture(agent)
				if tc.mutate != nil {
					tc.mutate(traces, metrics)
				}
				capture := &agentOTLPCapture{traceReqs: []*coltracepb.ExportTraceServiceRequest{traces}, metricReqs: []*colmetricpb.ExportMetricsServiceRequest{metrics}}
				err := capture.validate(agent)
				if tc.wantErr == "" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, tc.wantErr)
				}
			})
		}
	}
}
