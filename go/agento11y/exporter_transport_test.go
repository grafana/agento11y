package agento11y

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

type exportTransport int

const (
	exportTransportHTTP exportTransport = iota
	exportTransportGRPC
)

func TestSDKExportsGenerationOverHTTP_AllPropertiesRoundTrip(t *testing.T) {
	start, result := payloadFromSeed(42)
	expected, received := runSDKRoundTrip(t, exportTransportHTTP, start, result)

	if !proto.Equal(expected, received) {
		t.Fatalf("http roundtrip mismatch\nexpected=%s\nreceived=%s", protojson.Format(expected), protojson.Format(received))
	}
}

func TestSDKExportsGenerationOverGRPC_AllPropertiesRoundTrip(t *testing.T) {
	start, result := payloadFromSeed(99)
	expected, received := runSDKRoundTrip(t, exportTransportGRPC, start, result)

	if !proto.Equal(expected, received) {
		t.Fatalf("grpc roundtrip mismatch\nexpected=%s\nreceived=%s", protojson.Format(expected), protojson.Format(received))
	}
}

func TestSDKExportsWorkflowStepOverHTTP_AllPropertiesRoundTrip(t *testing.T) {
	step := workflowStepPayloadFromSeed(42)
	expected, received := runSDKWorkflowStepRoundTrip(t, exportTransportHTTP, step)

	if !proto.Equal(expected, received) {
		t.Fatalf("http workflow step roundtrip mismatch\nexpected=%s\nreceived=%s", protojson.Format(expected), protojson.Format(received))
	}
}

func TestSDKExportsWorkflowStepOverGRPC_AllPropertiesRoundTrip(t *testing.T) {
	step := workflowStepPayloadFromSeed(99)
	expected, received := runSDKWorkflowStepRoundTrip(t, exportTransportGRPC, step)

	if !proto.Equal(expected, received) {
		t.Fatalf("grpc workflow step roundtrip mismatch\nexpected=%s\nreceived=%s", protojson.Format(expected), protojson.Format(received))
	}
}

func TestSDKExportsGenerationOverGRPCAboveDefaultMessageLimit(t *testing.T) {
	ingest := &capturingIngestServer{}

	grpcServer := grpc.NewServer(
		grpc.MaxRecvMsgSize(defaultGRPCMaxReceiveMessageBytes),
		grpc.MaxSendMsgSize(defaultGRPCMaxSendMessageBytes),
	)
	agento11yv1.RegisterGenerationIngestServiceServer(grpcServer, ingest)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen grpc: %v", err)
	}
	go func() {
		_ = grpcServer.Serve(listener)
	}()
	t.Cleanup(func() {
		grpcServer.Stop()
		_ = listener.Close()
	})

	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			Protocol:                   GenerationExportProtocolGRPC,
			Endpoint:                   listener.Addr().String(),
			Insecure:                   BoolPtr(true),
			GRPCMaxSendMessageBytes:    defaultGRPCMaxSendMessageBytes,
			GRPCMaxReceiveMessageBytes: defaultGRPCMaxReceiveMessageBytes,
			PayloadMaxBytes:            8 << 20,
			BatchSize:                  1,
			QueueSize:                  10,
			FlushInterval:              time.Hour,
			MaxRetries:                 1,
			InitialBackoff:             time.Millisecond,
			MaxBackoff:                 10 * time.Millisecond,
		},
	})

	largeText := strings.Repeat("x", 5<<20)
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{
			Provider: "openai",
			Name:     "gpt-5",
		},
	})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage(largeText)},
		Output: []Message{AssistantTextMessage("ok")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("expected large grpc payload export to succeed, got %v", err)
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown client: %v", err)
	}

	request := ingest.singleRequest(t)
	if len(request.Generations) != 1 {
		t.Fatalf("expected one generation in captured request, got %d", len(request.Generations))
	}
	if got := request.Generations[0].GetInput()[0].GetParts()[0].GetText(); got != largeText {
		t.Fatalf("unexpected large input text size=%d", len(got))
	}
}

func TestSDKExportRoundTripProperties(t *testing.T) {
	for seed := uint64(1); seed <= 20; seed++ {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) {
			start, result := payloadFromSeed(seed)

			httpExpected, httpReceived := runSDKRoundTrip(t, exportTransportHTTP, start, result)
			if !proto.Equal(httpExpected, httpReceived) {
				t.Fatalf("http roundtrip mismatch for seed=%d", seed)
			}

			grpcExpected, grpcReceived := runSDKRoundTrip(t, exportTransportGRPC, start, result)
			if !proto.Equal(grpcExpected, grpcReceived) {
				t.Fatalf("grpc roundtrip mismatch for seed=%d", seed)
			}
		})
	}
}

func TestSDKExportsCacheWriteTokensOnlyOnProtoField5(t *testing.T) {
	start, result := payloadFromSeed(7)
	result.Usage = TokenUsage{
		InputTokens:           100,
		OutputTokens:          50,
		CacheReadInputTokens:  10,
		CacheWriteInputTokens: 42,
	}

	for _, transport := range []exportTransport{exportTransportHTTP, exportTransportGRPC} {
		_, received := runSDKRoundTrip(t, transport, start, result)
		usage := received.GetUsage()
		if usage.GetCacheWriteInputTokens() != 42 {
			t.Fatalf("expected cache_write_input_tokens=42, got %d", usage.GetCacheWriteInputTokens())
		}
	}
}

func TestSDKExportsGenerationOverNone_NoSend(t *testing.T) {
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolNone,
			BatchSize:       1,
			QueueSize:       10,
			FlushInterval:   time.Hour,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      10 * time.Millisecond,
			PayloadMaxBytes: 4 << 20,
		},
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{
			Provider: "openai",
			Name:     "gpt-5",
		},
	})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}

	flushCtx, flushCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer flushCancel()
	if err := client.Flush(flushCtx); err != nil {
		t.Fatalf("flush: %v", err)
	}

	if rec.lastGeneration.ID == "" {
		t.Fatalf("expected recorder to capture generation result")
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer shutdownCancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func runSDKRoundTrip(t *testing.T, transport exportTransport, start GenerationStart, result Generation) (*agento11yv1.Generation, *agento11yv1.Generation) {
	t.Helper()

	ingest := &capturingIngestServer{}
	client := newTransportTestClient(t, transport, ingest)

	_, rec := client.StartGeneration(context.Background(), start)
	rec.SetResult(result, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder error: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown client: %v", err)
	}

	request := ingest.singleRequest(t)
	if len(request.Generations) != 1 {
		t.Fatalf("expected 1 generation, got %d", len(request.Generations))
	}

	expected, err := generationToProto(rec.lastGeneration)
	if err != nil {
		t.Fatalf("convert expected generation to proto: %v", err)
	}

	return expected, request.Generations[0]
}

func runSDKWorkflowStepRoundTrip(t *testing.T, transport exportTransport, step WorkflowStep) (*agento11yv1.WorkflowStep, *agento11yv1.WorkflowStep) {
	t.Helper()

	ingest := &capturingIngestServer{}
	client := newTransportTestClient(t, transport, ingest)

	if err := client.EnqueueWorkflowStep(step); err != nil {
		t.Fatalf("enqueue workflow step: %v", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Shutdown(shutdownCtx); err != nil {
		t.Fatalf("shutdown client: %v", err)
	}

	request := ingest.singleWorkflowStepRequest(t)
	if len(request.WorkflowSteps) != 1 {
		t.Fatalf("expected 1 workflow step, got %d", len(request.WorkflowSteps))
	}

	expected, err := workflowStepToProto(step)
	if err != nil {
		t.Fatalf("convert expected workflow step to proto: %v", err)
	}

	return expected, request.WorkflowSteps[0]
}

func newTransportTestClient(t *testing.T, transport exportTransport, ingest *capturingIngestServer) *Client {
	t.Helper()

	cfg := Config{
		Tracer: noop.NewTracerProvider().Tracer("test"),
		GenerationExport: GenerationExportConfig{
			BatchSize:      1,
			QueueSize:      10,
			FlushInterval:  time.Second,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     10 * time.Millisecond,
		},
	}

	switch transport {
	case exportTransportHTTP:
		httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}

			payload, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read body", http.StatusBadRequest)
				return
			}

			if r.URL.Path == "/api/v1/workflow-steps:export" {
				request := &agento11yv1.ExportWorkflowStepsRequest{}
				if err := protojson.Unmarshal(payload, request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				ingest.captureWorkflowSteps(request)

				response := workflowStepAcceptanceResponse(request)
				encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(response)
				if err != nil {
					http.Error(w, "marshal response", http.StatusInternalServerError)
					return
				}

				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write(encoded)
				return
			}

			request := &agento11yv1.ExportGenerationsRequest{}
			if err := protojson.Unmarshal(payload, request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			ingest.capture(request)

			response := acceptanceResponse(request)
			encoded, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(response)
			if err != nil {
				http.Error(w, "marshal response", http.StatusInternalServerError)
				return
			}

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write(encoded)
		}))
		t.Cleanup(httpServer.Close)

		cfg.GenerationExport.Protocol = GenerationExportProtocolHTTP
		cfg.GenerationExport.Endpoint = httpServer.URL + "/api/v1/generations:export"
	case exportTransportGRPC:
		grpcServer := grpc.NewServer()
		agento11yv1.RegisterGenerationIngestServiceServer(grpcServer, ingest)
		agento11yv1.RegisterWorkflowStepIngestServiceServer(grpcServer, ingest)

		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen grpc: %v", err)
		}

		go func() {
			_ = grpcServer.Serve(listener)
		}()

		t.Cleanup(func() {
			grpcServer.Stop()
			_ = listener.Close()
		})

		cfg.GenerationExport.Protocol = GenerationExportProtocolGRPC
		cfg.GenerationExport.Endpoint = listener.Addr().String()
		cfg.GenerationExport.Insecure = BoolPtr(true)
	default:
		t.Fatalf("unsupported transport: %v", transport)
	}

	return NewClient(cfg)
}

func payloadFromSeed(seed uint64) (GenerationStart, Generation) {
	rnd := rand.New(rand.NewSource(int64(seed)))
	mode := GenerationModeSync
	if seed%2 == 0 {
		mode = GenerationModeStream
	}

	startedAt := time.Unix(int64(seed%1_000_000), int64(seed%1000)*int64(time.Millisecond)).UTC()
	completedAt := startedAt.Add(250 * time.Millisecond)

	start := GenerationStart{
		ID:             "gen-" + randomASCII(rnd, 10),
		ConversationID: "conv-" + randomASCII(rnd, 8),
		AgentName:      "agent-" + randomASCII(rnd, 8),
		AgentVersion:   "v-" + randomASCII(rnd, 6),
		Mode:           mode,
		Model: ModelRef{
			Provider: "provider-" + randomASCII(rnd, 5),
			Name:     "model-" + randomASCII(rnd, 5),
		},
		SystemPrompt: "system-" + randomASCII(rnd, 10),
		Tools: []ToolDefinition{
			{Name: "tool-" + randomASCII(rnd, 5), Description: "desc-" + randomASCII(rnd, 6), Type: "function", InputSchema: []byte(`{"type":"object"}`), Deferred: seed%2 == 0},
		},
		MaxTokens:       int64Ptr(int64(rnd.Intn(1024) + 1)),
		Temperature:     float64Ptr(float64(rnd.Intn(100)) / 100),
		TopP:            float64Ptr(float64(rnd.Intn(100)) / 100),
		ToolChoice:      stringPtr("auto"),
		ThinkingEnabled: boolPtr(seed%2 == 0),
		Tags: map[string]string{
			"seed": fmt.Sprintf("%d", seed),
			"env":  "test",
		},
		Metadata: map[string]any{
			"seed":    seed % 10000,
			"enabled": seed%2 == 0,
			"nested":  map[string]any{"n": rnd.Intn(100)},
		},
		StartedAt: startedAt,
	}

	result := Generation{
		ID:             start.ID,
		ConversationID: start.ConversationID,
		AgentName:      start.AgentName,
		AgentVersion:   start.AgentVersion,
		Mode:           start.Mode,
		OperationName:  defaultOperationNameForMode(start.Mode),
		TraceID:        randomHex(rnd, 32),
		SpanID:         randomHex(rnd, 16),
		Model:          start.Model,
		ResponseID:     "resp-" + randomASCII(rnd, 8),
		ResponseModel:  "response-model-" + randomASCII(rnd, 5),
		SystemPrompt:   start.SystemPrompt,
		Input: []Message{
			{Role: RoleUser, Name: "user", Parts: []Part{TextPart("input-" + randomASCII(rnd, 8))}},
		},
		Output: []Message{
			{Role: RoleAssistant, Name: "assistant", Parts: []Part{ThinkingPart("think-" + randomASCII(rnd, 6)), ToolCallPart(ToolCall{ID: "call-" + randomASCII(rnd, 5), Name: "tool", InputJSON: []byte(`{"x":1}`)})}},
			{Role: RoleTool, Name: "tool", Parts: []Part{ToolResultPart(ToolResult{ToolCallID: "call-" + randomASCII(rnd, 5), Name: "tool", Content: "ok", ContentJSON: []byte(`{"ok":true}`), IsError: seed%3 == 0})}},
		},
		Tools:           start.Tools,
		MaxTokens:       int64Ptr(*start.MaxTokens),
		Temperature:     float64Ptr(*start.Temperature),
		TopP:            float64Ptr(*start.TopP),
		ToolChoice:      stringPtr(*start.ToolChoice),
		ThinkingEnabled: boolPtr(*start.ThinkingEnabled),
		Usage: TokenUsage{
			InputTokens:           int64(rnd.Intn(1000)),
			OutputTokens:          int64(rnd.Intn(1000)),
			TotalTokens:           int64(rnd.Intn(2000) + 1),
			CacheReadInputTokens:  int64(rnd.Intn(100)),
			CacheWriteInputTokens: int64(rnd.Intn(100)),
			ReasoningTokens:       int64(rnd.Intn(100)),
		},
		StopReason:  "stop-" + randomASCII(rnd, 4),
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		Tags: map[string]string{
			"seed": fmt.Sprintf("%d", seed),
			"env":  "test",
		},
		Metadata: map[string]any{
			"seed":    seed % 10000,
			"enabled": seed%2 == 0,
			"nested":  map[string]any{"n": rnd.Intn(100)},
		},
		Artifacts: []Artifact{
			{Kind: ArtifactKindRequest, Name: "request", ContentType: "application/json", Payload: []byte(`{"request":true}`), RecordID: "rec-" + randomASCII(rnd, 6), URI: "sigil://artifact/" + randomASCII(rnd, 6)},
			{Kind: ArtifactKindProviderEvent, Name: "event", ContentType: "application/json", Payload: []byte(`{"event":true}`)},
		},
	}

	return start, result
}

func workflowStepPayloadFromSeed(seed uint64) WorkflowStep {
	rnd := rand.New(rand.NewSource(int64(seed)))
	startedAt := time.Unix(int64(seed%1_000_000), int64(seed%1000)*int64(time.Millisecond)).UTC()
	completedAt := startedAt.Add(250 * time.Millisecond)

	return WorkflowStep{
		ID:             "wfs-" + randomASCII(rnd, 10),
		ConversationID: "conv-" + randomASCII(rnd, 8),
		StepName:       "step-" + randomASCII(rnd, 6),
		Framework:      "framework-" + randomASCII(rnd, 5),
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		InputState: map[string]any{
			"seed":    seed % 10000,
			"enabled": seed%2 == 0,
			"nested":  map[string]any{"n": rnd.Intn(100)},
		},
		OutputState: map[string]any{
			"route": "answer-" + randomASCII(rnd, 5),
		},
		Error: "error-" + randomASCII(rnd, 4),
		Tags: map[string]string{
			"seed": fmt.Sprintf("%d", seed),
			"env":  "test",
		},
		LinkedGenerationIDs: []string{"gen-" + randomASCII(rnd, 8), "gen-" + randomASCII(rnd, 8)},
		ParentStepIDs:       []string{"wfs-parent-" + randomASCII(rnd, 5)},
		AgentName:           "agent-" + randomASCII(rnd, 8),
		AgentVersion:        "v-" + randomASCII(rnd, 6),
		TraceID:             randomHex(rnd, 32),
		SpanID:              randomHex(rnd, 16),
		Metadata: map[string]any{
			"run_id": "run-" + randomASCII(rnd, 8),
			"seed":   seed % 10000,
		},
	}
}

func randomASCII(rnd *rand.Rand, n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	bytes := make([]byte, n)
	for i := range bytes {
		bytes[i] = alphabet[rnd.Intn(len(alphabet))]
	}
	return string(bytes)
}

func randomHex(rnd *rand.Rand, n int) string {
	const alphabet = "0123456789abcdef"
	bytes := make([]byte, n)
	for i := range bytes {
		bytes[i] = alphabet[rnd.Intn(len(alphabet))]
	}
	return string(bytes)
}

func int64Ptr(value int64) *int64 {
	return &value
}

func float64Ptr(value float64) *float64 {
	return &value
}

func stringPtr(value string) *string {
	return &value
}

func boolPtr(value bool) *bool {
	return &value
}

type capturingIngestServer struct {
	agento11yv1.UnimplementedGenerationIngestServiceServer
	agento11yv1.UnimplementedWorkflowStepIngestServiceServer

	mu                   sync.Mutex
	requests             []*agento11yv1.ExportGenerationsRequest
	workflowStepRequests []*agento11yv1.ExportWorkflowStepsRequest
	userAgents           []string
}

func (s *capturingIngestServer) ExportGenerations(ctx context.Context, req *agento11yv1.ExportGenerationsRequest) (*agento11yv1.ExportGenerationsResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("user-agent"); len(vals) > 0 {
			s.mu.Lock()
			s.userAgents = append(s.userAgents, vals[0])
			s.mu.Unlock()
		}
	}
	s.capture(req)
	return acceptanceResponse(req), nil
}

func (s *capturingIngestServer) ExportWorkflowSteps(ctx context.Context, req *agento11yv1.ExportWorkflowStepsRequest) (*agento11yv1.ExportWorkflowStepsResponse, error) {
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if vals := md.Get("user-agent"); len(vals) > 0 {
			s.mu.Lock()
			s.userAgents = append(s.userAgents, vals[0])
			s.mu.Unlock()
		}
	}
	s.captureWorkflowSteps(req)
	return workflowStepAcceptanceResponse(req), nil
}

func (s *capturingIngestServer) singleUserAgent(t *testing.T) string {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.userAgents) != 1 {
		t.Fatalf("expected exactly one captured user-agent, got %d", len(s.userAgents))
	}
	return s.userAgents[0]
}

func (s *capturingIngestServer) capture(req *agento11yv1.ExportGenerationsRequest) {
	if req == nil {
		return
	}

	clone := proto.Clone(req)
	typed, ok := clone.(*agento11yv1.ExportGenerationsRequest)
	if !ok {
		return
	}

	s.mu.Lock()
	s.requests = append(s.requests, typed)
	s.mu.Unlock()
}

func (s *capturingIngestServer) singleRequest(t *testing.T) *agento11yv1.ExportGenerationsRequest {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 1 {
		t.Fatalf("expected exactly one export request, got %d", len(s.requests))
	}
	return s.requests[0]
}

func (s *capturingIngestServer) captureWorkflowSteps(req *agento11yv1.ExportWorkflowStepsRequest) {
	if req == nil {
		return
	}

	clone := proto.Clone(req)
	typed, ok := clone.(*agento11yv1.ExportWorkflowStepsRequest)
	if !ok {
		return
	}

	s.mu.Lock()
	s.workflowStepRequests = append(s.workflowStepRequests, typed)
	s.mu.Unlock()
}

func (s *capturingIngestServer) singleWorkflowStepRequest(t *testing.T) *agento11yv1.ExportWorkflowStepsRequest {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.workflowStepRequests) != 1 {
		t.Fatalf("expected exactly one workflow step export request, got %d", len(s.workflowStepRequests))
	}
	return s.workflowStepRequests[0]
}

func acceptanceResponse(req *agento11yv1.ExportGenerationsRequest) *agento11yv1.ExportGenerationsResponse {
	response := &agento11yv1.ExportGenerationsResponse{Results: make([]*agento11yv1.ExportGenerationResult, len(req.GetGenerations()))}
	for i := range req.GetGenerations() {
		response.Results[i] = &agento11yv1.ExportGenerationResult{GenerationId: req.Generations[i].Id, Accepted: true}
	}
	return response
}

func workflowStepAcceptanceResponse(req *agento11yv1.ExportWorkflowStepsRequest) *agento11yv1.ExportWorkflowStepsResponse {
	response := &agento11yv1.ExportWorkflowStepsResponse{Results: make([]*agento11yv1.ExportWorkflowStepResult, len(req.GetWorkflowSteps()))}
	for i := range req.GetWorkflowSteps() {
		response.Results[i] = &agento11yv1.ExportWorkflowStepResult{StepId: req.WorkflowSteps[i].Id, Accepted: true}
	}
	return response
}
