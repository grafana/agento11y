package sigiltest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	sigil "github.com/grafana/sigil-sdk/go/sigil"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

// ClearAmbientEnv strips SIGIL_* and OTEL_* env vars so test clients aren't
// influenced by the developer's shell.
func ClearAmbientEnv() { clearAmbientEnvOnce() }

var clearAmbientEnvOnce = sync.OnceFunc(func() {
	for _, kv := range os.Environ() {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		key := kv[:idx]
		if strings.HasPrefix(key, "SIGIL_") || strings.HasPrefix(key, "OTEL_") {
			_ = os.Unsetenv(key)
		}
	}
})

type Env struct {
	Client  *sigil.Client
	Spans   *tracetest.SpanRecorder
	Metrics *sdkmetric.ManualReader

	ingest *capturingIngestServer

	tracerProvider *sdktrace.TracerProvider
	meterProvider  *sdkmetric.MeterProvider
	grpcServer     *grpc.Server
	listener       net.Listener
	closeOnce      sync.Once
}

// NewEnv creates a test environment with a fake gRPC ingest server, span
// recorder, and metric reader. The returned Client is configured to export
// generations synchronously (batch size 1) to the fake server.
//
// Optional config modifiers are applied after the base test config is built.
// Use this to inject a ContentCaptureResolver or override other Config fields
// while keeping the test exporter and tracing infrastructure.
func NewEnv(t testing.TB, opts ...func(*sigil.Config)) *Env {
	t.Helper()

	ClearAmbientEnv()

	ingest := &capturingIngestServer{}
	grpcServer := grpc.NewServer()
	sigilv1.RegisterGenerationIngestServiceServer(grpcServer, ingest)

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

	cfg := sigil.DefaultConfig()
	cfg.Tracer = tracerProvider.Tracer("sigil-provider-conformance")
	cfg.Meter = meterProvider.Meter("sigil-provider-conformance")
	cfg.GenerationExport = sigil.GenerationExportConfig{
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
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	env := &Env{
		Client:         sigil.NewClient(cfg),
		Spans:          spanRecorder,
		Metrics:        metricReader,
		ingest:         ingest,
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
		grpcServer:     grpcServer,
		listener:       listener,
	}
	t.Cleanup(func() {
		if err := env.close(); err != nil {
			t.Errorf("close sigil test env: %v", err)
		}
	})
	return env
}

func (e *Env) Shutdown(t testing.TB) {
	t.Helper()

	if err := e.close(); err != nil {
		t.Fatalf("shutdown sigil client: %v", err)
	}
}

func (e *Env) RequestCount() int {
	if e == nil || e.ingest == nil {
		return 0
	}
	return e.ingest.requestCount()
}

func (e *Env) SingleGenerationJSON(t testing.TB) map[string]any {
	t.Helper()

	req := e.singleRequest(t)
	if len(req.GetGenerations()) != 1 {
		t.Fatalf("expected exactly one generation in request, got %d", len(req.GetGenerations()))
	}

	generationJSON, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(req.GetGenerations()[0])
	if err != nil {
		t.Fatalf("marshal generation json: %v", err)
	}

	var generation map[string]any
	if err := json.Unmarshal(generationJSON, &generation); err != nil {
		t.Fatalf("decode generation json: %v", err)
	}
	return generation
}

func (e *Env) close() error {
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
	})
	return closeErr
}

func (e *Env) singleRequest(t testing.TB) *sigilv1.ExportGenerationsRequest {
	t.Helper()

	if e == nil || e.ingest == nil {
		t.Fatalf("sigil test env has no ingest server")
	}
	return e.ingest.singleRequest(t)
}

type capturingIngestServer struct {
	sigilv1.UnimplementedGenerationIngestServiceServer

	mu       sync.Mutex
	requests []*sigilv1.ExportGenerationsRequest
}

func (s *capturingIngestServer) ExportGenerations(_ context.Context, req *sigilv1.ExportGenerationsRequest) (*sigilv1.ExportGenerationsResponse, error) {
	s.capture(req)
	return acceptedResponse(req), nil
}

func (s *capturingIngestServer) capture(req *sigilv1.ExportGenerationsRequest) {
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

func (s *capturingIngestServer) requestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.requests)
}

func (s *capturingIngestServer) singleRequest(t testing.TB) *sigilv1.ExportGenerationsRequest {
	t.Helper()

	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.requests) != 1 {
		t.Fatalf("expected exactly one export request, got %d", len(s.requests))
	}
	return s.requests[0]
}

func acceptedResponse(req *sigilv1.ExportGenerationsRequest) *sigilv1.ExportGenerationsResponse {
	response := &sigilv1.ExportGenerationsResponse{Results: make([]*sigilv1.ExportGenerationResult, len(req.GetGenerations()))}
	for i := range req.GetGenerations() {
		response.Results[i] = &sigilv1.ExportGenerationResult{
			Accepted: true,
		}
	}
	return response
}

func JSONPath(t testing.TB, value any, path ...any) any {
	t.Helper()

	current := value
	for _, step := range path {
		switch typed := step.(type) {
		case string:
			node, ok := current.(map[string]any)
			if !ok {
				t.Fatalf("path step %q expected object, got %T", typed, current)
			}
			next, ok := node[typed]
			if !ok {
				t.Fatalf("path step %q missing in object keys %v", typed, mapsKeys(node))
			}
			current = next
		case int:
			node, ok := current.([]any)
			if !ok {
				t.Fatalf("path step %d expected array, got %T", typed, current)
			}
			if typed < 0 || typed >= len(node) {
				t.Fatalf("path index %d out of range for len %d", typed, len(node))
			}
			current = node[typed]
		default:
			t.Fatalf("unsupported path step type %T", step)
		}
	}
	return current
}

func mapsKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}

func StringValue(t testing.TB, value any, path ...any) string {
	t.Helper()

	resolved := JSONPath(t, value, path...)
	text, ok := resolved.(string)
	if !ok {
		t.Fatalf("path %v expected string, got %T (%v)", path, resolved, resolved)
	}
	return text
}

func FloatValue(t testing.TB, value any, path ...any) float64 {
	t.Helper()

	resolved := JSONPath(t, value, path...)
	number, ok := resolved.(float64)
	if !ok {
		t.Fatalf("path %v expected number, got %T (%v)", path, resolved, resolved)
	}
	return number
}

func RequireRequestCount(t testing.TB, env *Env, want int) {
	t.Helper()

	if env == nil {
		t.Fatalf("sigil test env is nil")
	}
	if got := env.RequestCount(); got != want {
		t.Fatalf("unexpected export request count: got %d want %d", got, want)
	}
}

func DebugJSON(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}
