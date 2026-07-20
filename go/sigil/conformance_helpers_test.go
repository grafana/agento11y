package sigil_test

import (
	"context"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	sigilv1 "github.com/grafana/agento11y/go/proto/sigil/v1"
	sigil "github.com/grafana/agento11y/go/sigil"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	conformanceOperationName       = "generateText"
	conformanceStreamOperation     = "streamText"
	conformanceToolOperation       = "execute_tool"
	conformanceEmbeddingOperation  = "embeddings"
	metadataKeyConversation        = "sigil.conversation.title"
	metadataKeyContentCaptureMode  = "sigil.sdk.content_capture_mode"
	metadataKeyCanonicalUserID     = "sigil.user.id"
	metadataKeyLegacyUserID        = "user.id"
	metadataKeyThinkingBudget      = "sigil.gen_ai.request.thinking.budget_tokens"
	metadataKeySDKName             = "sigil.sdk.name"
	sdkMetadataKeyName             = metadataKeySDKName
	sdkNameGo                      = "sdk-go"
	spanAttrOperationName          = "gen_ai.operation.name"
	spanAttrGenerationID           = "sigil.generation.id"
	spanAttrConversationID         = "gen_ai.conversation.id"
	spanAttrConversationTitle      = "sigil.conversation.title"
	spanAttrUserID                 = "user.id"
	spanAttrAgentName              = "gen_ai.agent.name"
	spanAttrAgentVersion           = "gen_ai.agent.version"
	spanAttrErrorType              = "error.type"
	spanAttrErrorCategory          = "error.category"
	spanAttrProviderName           = "gen_ai.provider.name"
	spanAttrRequestModel           = "gen_ai.request.model"
	spanAttrRequestMaxTokens       = "gen_ai.request.max_tokens"
	spanAttrRequestTemperature     = "gen_ai.request.temperature"
	spanAttrRequestTopP            = "gen_ai.request.top_p"
	spanAttrRequestToolChoice      = "sigil.gen_ai.request.tool_choice"
	spanAttrRequestThinkingEnabled = "sigil.gen_ai.request.thinking.enabled"
	spanAttrRequestThinkingBudget  = metadataKeyThinkingBudget
	spanAttrEmbeddingInputCount    = "gen_ai.embeddings.input_count"
	spanAttrEmbeddingInputTexts    = "gen_ai.embeddings.input_texts"
	spanAttrEmbeddingDimCount      = "gen_ai.embeddings.dimension.count"
	spanAttrToolName               = "gen_ai.tool.name"
	spanAttrToolCallID             = "gen_ai.tool.call.id"
	spanAttrToolType               = "gen_ai.tool.type"
	spanAttrToolDescription        = "gen_ai.tool.description"
	spanAttrToolCallArguments      = "gen_ai.tool.call.arguments"
	spanAttrToolCallResult         = "gen_ai.tool.call.result"
	spanAttrResponseID             = "gen_ai.response.id"
	spanAttrResponseModel          = "gen_ai.response.model"
	spanAttrFinishReasons          = "gen_ai.response.finish_reasons"
	spanAttrInputTokens            = "gen_ai.usage.input_tokens"
	spanAttrOutputTokens           = "gen_ai.usage.output_tokens"
	spanAttrCacheReadTokens        = "gen_ai.usage.cache_read_input_tokens"
	spanAttrCacheWriteTokens       = "gen_ai.usage.cache_write_input_tokens"
	spanAttrReasoningTokens        = "gen_ai.usage.reasoning_tokens"
	metricOperationDuration        = "gen_ai.client.operation.duration"
	metricTokenUsage               = "gen_ai.client.token.usage"
	metricTimeToFirstToken         = "gen_ai.client.time_to_first_token"
	metricToolCallsPerOperation    = "gen_ai.client.tool_calls_per_operation"
	metricAttrTokenType            = "gen_ai.token.type"
	metricTokenTypeInput           = "input"
	metricTokenTypeOutput          = "output"
	metricTokenTypeCacheRead       = "cache_read"
	metricTokenTypeCacheWrite      = "cache_write"
	metricTokenTypeReasoning       = "reasoning"
)

var conformanceModel = sigil.ModelRef{
	Provider: "openai",
	Name:     "gpt-5",
}

type conformanceEnv struct {
	Client  *sigil.Client
	Ingest  *fakeIngestServer
	Spans   *tracetest.SpanRecorder
	Metrics *sdkmetric.ManualReader
	Rating  *fakeRatingServer

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	grpcServer     *grpc.Server
	listener       net.Listener
	closeOnce      sync.Once
}

type conformanceEnvOption func(*conformanceEnvConfig)

type conformanceEnvConfig struct {
	config sigil.Config
}

func newConformanceEnv(t *testing.T, opts ...conformanceEnvOption) *conformanceEnv {
	t.Helper()

	ingest := &fakeIngestServer{}
	grpcServer := grpc.NewServer()
	sigilv1.RegisterGenerationIngestServiceServer(grpcServer, ingest)
	sigilv1.RegisterWorkflowStepIngestServiceServer(grpcServer, ingest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for fake ingest server: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(listener)
	}()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))
	ratingServer := newFakeRatingServer()

	cfg := conformanceEnvConfig{
		config: sigil.Config{
			Tracer: tracerProvider.Tracer("sigil-conformance-test"),
			Meter:  meterProvider.Meter("sigil-conformance-test"),
			GenerationExport: sigil.GenerationExportConfig{
				Protocol:        sigil.GenerationExportProtocolGRPC,
				Endpoint:        listener.Addr().String(),
				Insecure:        sigil.BoolPtr(true),
				BatchSize:       1,
				FlushInterval:   time.Hour,
				QueueSize:       8,
				MaxRetries:      1,
				InitialBackoff:  time.Millisecond,
				MaxBackoff:      5 * time.Millisecond,
				PayloadMaxBytes: 4 << 20,
			},
			API: sigil.APIConfig{
				Endpoint: ratingServer.URL(),
			},
		},
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	env := &conformanceEnv{
		Client:         sigil.NewClient(cfg.config),
		Ingest:         ingest,
		Spans:          spanRecorder,
		Metrics:        metricReader,
		Rating:         ratingServer,
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		grpcServer:     grpcServer,
		listener:       listener,
	}
	t.Cleanup(func() {
		_ = env.close()
	})
	return env
}

func withConformanceConfig(mutator func(*sigil.Config)) conformanceEnvOption {
	return func(cfg *conformanceEnvConfig) {
		if mutator != nil {
			mutator(&cfg.config)
		}
	}
}

func (e *conformanceEnv) Shutdown(t *testing.T) {
	t.Helper()

	if e == nil || e.Client == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := e.Client.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown conformance client: %v", err)
	}
}

func (e *conformanceEnv) close() error {
	if e == nil {
		return nil
	}

	var closeErr error
	e.closeOnce.Do(func() {
		if e.Client != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := e.Client.Shutdown(ctx); err != nil {
				closeErr = err
			}
		}
		if e.meterProvider != nil {
			if err := e.meterProvider.Shutdown(context.Background()); err != nil && closeErr == nil {
				closeErr = err
			}
		}
		if e.tracerProvider != nil {
			if err := e.tracerProvider.Shutdown(context.Background()); err != nil && closeErr == nil {
				closeErr = err
			}
		}
		if e.grpcServer != nil {
			e.grpcServer.Stop()
		}
		if e.listener != nil {
			_ = e.listener.Close()
		}
		if e.Rating != nil {
			e.Rating.Close()
		}
	})
	return closeErr
}

func (e *conformanceEnv) CollectMetrics(t *testing.T) metricdata.ResourceMetrics {
	t.Helper()

	var collected metricdata.ResourceMetrics
	if err := e.Metrics.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	return collected
}

type fakeIngestServer struct {
	sigilv1.UnimplementedGenerationIngestServiceServer
	sigilv1.UnimplementedWorkflowStepIngestServiceServer

	mu                   sync.Mutex
	requests             []*sigilv1.ExportGenerationsRequest
	workflowStepRequests []*sigilv1.ExportWorkflowStepsRequest
}

func (s *fakeIngestServer) ExportGenerations(_ context.Context, req *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	s.capture(req)
	return acceptanceResponse(req), nil
}

func (s *fakeIngestServer) ExportWorkflowSteps(_ context.Context, req *sigilv1.ExportWorkflowStepsRequest) (*sigilv1.ExportWorkflowStepsResponse, error) {
	s.captureWorkflowSteps(req)
	return workflowStepAcceptanceResponse(req), nil
}

func (s *fakeIngestServer) capture(req *sigilv1.ExportGenerationsRequest) {
	if req == nil {
		return
	}

	clone := proto.Clone(req)
	typed, ok := clone.(*sigilv1.ExportGenerationsRequest)
	if !ok {
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, typed)
	s.mu.Unlock()
}

func (s *fakeIngestServer) SingleGeneration(t *testing.T) *sigilv1.Generation {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.requests) != 1 {
		t.Fatalf("expected exactly one export request, got %d", len(s.requests))
	}
	if len(s.requests[0].Generations) != 1 {
		t.Fatalf("expected exactly one generation in request, got %d", len(s.requests[0].Generations))
	}
	return s.requests[0].Generations[0]
}

func (s *fakeIngestServer) SingleWorkflowStep(t *testing.T) *sigilv1.WorkflowStep {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.workflowStepRequests) != 1 {
		t.Fatalf("expected exactly one workflow step export request, got %d", len(s.workflowStepRequests))
	}
	if len(s.workflowStepRequests[0].WorkflowSteps) != 1 {
		t.Fatalf("expected exactly one workflow step in request, got %d", len(s.workflowStepRequests[0].WorkflowSteps))
	}
	return s.workflowStepRequests[0].WorkflowSteps[0]
}

func (s *fakeIngestServer) RequestCount() int {
	if s == nil {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *fakeIngestServer) Requests() []*sigilv1.ExportGenerationsRequest {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*sigilv1.ExportGenerationsRequest, len(s.requests))
	copy(out, s.requests)
	return out
}

func (s *fakeIngestServer) GenerationCount() int {
	if s == nil {
		return 0
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for _, req := range s.requests {
		total += len(req.GetGenerations())
	}
	return total
}

func (s *fakeIngestServer) captureWorkflowSteps(req *sigilv1.ExportWorkflowStepsRequest) {
	if req == nil {
		return
	}

	clone := proto.Clone(req)
	typed, ok := clone.(*sigilv1.ExportWorkflowStepsRequest)
	if !ok {
		return
	}

	s.mu.Lock()
	s.workflowStepRequests = append(s.workflowStepRequests, typed)
	s.mu.Unlock()
}

func acceptanceResponse(req *sigilv1.ExportGenerationsRequest) *sigilv1.ExportGenerationsResponse {
	response := &sigilv1.ExportGenerationsResponse{Results: make([]*sigilv1.ExportGenerationResult, len(req.GetGenerations()))}
	for i := range req.GetGenerations() {
		response.Results[i] = &sigilv1.ExportGenerationResult{
			GenerationId: req.Generations[i].GetId(),
			Accepted:     true,
		}
	}
	return response
}

func workflowStepAcceptanceResponse(req *sigilv1.ExportWorkflowStepsRequest) *sigilv1.ExportWorkflowStepsResponse {
	response := &sigilv1.ExportWorkflowStepsResponse{Results: make([]*sigilv1.ExportWorkflowStepResult, len(req.GetWorkflowSteps()))}
	for i := range req.GetWorkflowSteps() {
		response.Results[i] = &sigilv1.ExportWorkflowStepResult{
			StepId:   req.WorkflowSteps[i].GetId(),
			Accepted: true,
		}
	}
	return response
}

type fakeRatingServer struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []capturedRatingRequest
}

type capturedRatingRequest struct {
	Method  string
	Path    string
	Headers http.Header
	Body    []byte
}

func newFakeRatingServer() *fakeRatingServer {
	s := &fakeRatingServer{}
	s.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		s.mu.Lock()
		s.requests = append(s.requests, capturedRatingRequest{
			Method:  req.Method,
			Path:    req.URL.Path,
			Headers: req.Header.Clone(),
			Body:    append([]byte(nil), body...),
		})
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"rating":{"rating_id":"rat-1","conversation_id":"conv-1","rating":"CONVERSATION_RATING_VALUE_GOOD","created_at":"2026-03-12T11:00:00Z"},"summary":{"total_count":1,"good_count":1,"bad_count":0,"latest_rating":"CONVERSATION_RATING_VALUE_GOOD","latest_rated_at":"2026-03-12T11:00:00Z","has_bad_rating":false}}`))
	}))
	return s
}

func (s *fakeRatingServer) URL() string {
	if s == nil || s.server == nil {
		return ""
	}
	return s.server.URL
}

func (s *fakeRatingServer) Close() {
	if s != nil && s.server != nil {
		s.server.Close()
	}
}

func (s *fakeRatingServer) SingleRequest(t *testing.T) capturedRatingRequest {
	t.Helper()

	requests := s.Requests()
	if len(requests) != 1 {
		t.Fatalf("expected exactly one rating request, got %d", len(requests))
	}

	return requests[0]
}

func (s *fakeRatingServer) Requests() []capturedRatingRequest {
	if s == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]capturedRatingRequest, len(s.requests))
	for i := range s.requests {
		out[i] = capturedRatingRequest{
			Method:  s.requests[i].Method,
			Path:    s.requests[i].Path,
			Headers: s.requests[i].Headers.Clone(),
			Body:    append([]byte(nil), s.requests[i].Body...),
		}
	}
	return out
}

func findSpan(t *testing.T, spans []sdktrace.ReadOnlySpan, operationName string) sdktrace.ReadOnlySpan {
	t.Helper()

	var matched sdktrace.ReadOnlySpan
	for _, span := range spans {
		attrs := spanAttrs(span)
		if got, ok := attrs[spanAttrOperationName]; ok && got.AsString() == operationName {
			if matched != nil {
				t.Fatalf("expected exactly one span with %s=%q", spanAttrOperationName, operationName)
			}
			matched = span
		}
	}
	if matched == nil {
		t.Fatalf("expected span with %s=%q", spanAttrOperationName, operationName)
	}
	return matched
}

func spanAttrs(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	attrs := make(map[string]attribute.Value, len(span.Attributes()))
	for _, attr := range span.Attributes() {
		attrs[string(attr.Key)] = attr.Value
	}
	return attrs
}

func requireSpanAttr(t *testing.T, attrs map[string]attribute.Value, key, want string) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("expected span attribute %q=%q, attribute missing", key, want)
	}
	if got.AsString() != want {
		t.Fatalf("unexpected span attribute %q: got %q want %q", key, got.AsString(), want)
	}
}

func requireSpanAttrAbsent(t *testing.T, attrs map[string]attribute.Value, key string) {
	t.Helper()

	if _, ok := attrs[key]; ok {
		t.Fatalf("did not expect span attribute %q to be present", key)
	}
}

func requireSpanBoolAttr(t *testing.T, attrs map[string]attribute.Value, key string, want bool) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("expected span attribute %q=%t, attribute missing", key, want)
	}
	if got.AsBool() != want {
		t.Fatalf("unexpected span attribute %q: got %t want %t", key, got.AsBool(), want)
	}
}

func requireSpanInt64Attr(t *testing.T, attrs map[string]attribute.Value, key string, want int64) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("expected span attribute %q=%d, attribute missing", key, want)
	}
	if got.AsInt64() != want {
		t.Fatalf("unexpected span attribute %q: got %d want %d", key, got.AsInt64(), want)
	}
}

func requireSpanFloat64Attr(t *testing.T, attrs map[string]attribute.Value, key string, want float64) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("expected span attribute %q=%v, attribute missing", key, want)
	}
	if math.Abs(got.AsFloat64()-want) > 1e-9 {
		t.Fatalf("unexpected span attribute %q: got %v want %v", key, got.AsFloat64(), want)
	}
}

func requireSpanStringSliceAttr(t *testing.T, attrs map[string]attribute.Value, key string, want []string) {
	t.Helper()

	got, ok := attrs[key]
	if !ok {
		t.Fatalf("expected span attribute %q=%v, attribute missing", key, want)
	}
	gotSlice := got.AsStringSlice()
	if len(gotSlice) != len(want) {
		t.Fatalf("unexpected span attribute %q length: got %d want %d", key, len(gotSlice), len(want))
	}
	for i := range want {
		if gotSlice[i] != want[i] {
			t.Fatalf("unexpected span attribute %q[%d]: got %q want %q", key, i, gotSlice[i], want[i])
		}
	}
}

func requireSpanAttrPresent(t *testing.T, attrs map[string]attribute.Value, key string) {
	t.Helper()

	if _, ok := attrs[key]; !ok {
		t.Fatalf("expected span attribute %q to be present", key)
	}
}

func findHistogram[N int64 | float64](t *testing.T, collected metricdata.ResourceMetrics, name string) metricdata.Histogram[N] {
	t.Helper()

	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[N])
			if !ok {
				t.Fatalf("metric %q is not the expected histogram type", name)
			}
			return histogram
		}
	}

	t.Fatalf("expected histogram %q", name)
	return metricdata.Histogram[N]{}
}

func requireNoHistogram(t *testing.T, collected metricdata.ResourceMetrics, name string) {
	t.Helper()

	for _, scopeMetrics := range collected.ScopeMetrics {
		for _, metric := range scopeMetrics.Metrics {
			if metric.Name == name {
				t.Fatalf("did not expect histogram %q to be present", name)
			}
		}
	}
}

func requireHistogramPointWithAttrs[N int64 | float64](t *testing.T, histogram metricdata.Histogram[N], want map[string]string) metricdata.HistogramDataPoint[N] {
	t.Helper()

	return findHistogramPoint(t, histogram, want)
}

func findHistogramPoint[N int64 | float64](t *testing.T, histogram metricdata.Histogram[N], want map[string]string) metricdata.HistogramDataPoint[N] {
	t.Helper()

	for _, point := range histogram.DataPoints {
		if histogramPointMatches(point.Attributes, want) {
			return point
		}
	}

	t.Fatalf("expected histogram point with attrs %v", want)
	return metricdata.HistogramDataPoint[N]{}
}

func histogramPointMatches(attrs attribute.Set, want map[string]string) bool {
	for key, expected := range want {
		value, ok := (&attrs).Value(attribute.Key(key))
		if !ok || value.AsString() != expected {
			return false
		}
	}
	return true
}

func requireProtoMetadata(t *testing.T, generation *sigilv1.Generation, key, want string) {
	t.Helper()

	got, ok := protoMetadataString(generation, key)
	if !ok {
		t.Fatalf("expected generation metadata %q=%q, key missing", key, want)
	}
	if got != want {
		t.Fatalf("unexpected generation metadata %q: got %q want %q", key, got, want)
	}
}

func requireProtoMetadataAbsent(t *testing.T, generation *sigilv1.Generation, key string) {
	t.Helper()

	if _, ok := protoMetadataString(generation, key); ok {
		t.Fatalf("did not expect generation metadata %q to be present", key)
	}
}

func protoMetadataString(generation *sigilv1.Generation, key string) (string, bool) {
	if generation == nil || generation.GetMetadata() == nil {
		return "", false
	}

	value, ok := generation.GetMetadata().AsMap()[key]
	if !ok {
		return "", false
	}
	asString, ok := value.(string)
	if !ok {
		return "", false
	}
	return asString, true
}
