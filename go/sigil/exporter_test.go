package sigil

import (
	"context"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestGenerationRecorderQueueFullReturnsEnqueueError(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize: 1,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testDisableWorker:      true,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, rec1 := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
	rec1.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec1.End()
	if err := rec1.Err(); err != nil {
		t.Fatalf("unexpected error on first enqueue: %v", err)
	}

	_, rec2 := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
	rec2.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec2.End()

	if !errors.Is(rec2.Err(), ErrEnqueueFailed) {
		t.Fatalf("expected enqueue failure sentinel, got %v", rec2.Err())
	}
	if !errors.Is(rec2.Err(), ErrQueueFull) {
		t.Fatalf("expected queue full sentinel, got %v", rec2.Err())
	}
}

func TestGenerationExporterFlushesByBatchSize(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      10,
			BatchSize:      2,
			FlushInterval:  time.Hour,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	for i := 0; i < 2; i++ {
		_, rec := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
		rec.SetResult(Generation{
			Input:  []Message{UserTextMessage("hello")},
			Output: []Message{AssistantTextMessage("hi")},
		}, nil)
		rec.End()
		if err := rec.Err(); err != nil {
			t.Fatalf("unexpected enqueue error: %v", err)
		}
	}

	if err := waitForCondition(300*time.Millisecond, func() bool {
		exporter.mu.Lock()
		defer exporter.mu.Unlock()
		return len(exporter.requests) == 1 && len(exporter.requests[0].Generations) == 2
	}); err != nil {
		t.Fatalf("batch size flush not observed: %v", err)
	}
}

func TestGenerationExporterFlushesByInterval(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      10,
			BatchSize:      10,
			FlushInterval:  15 * time.Millisecond,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("unexpected enqueue error: %v", err)
	}

	if err := waitForCondition(500*time.Millisecond, func() bool {
		exporter.mu.Lock()
		defer exporter.mu.Unlock()
		return len(exporter.requests) >= 1 && len(exporter.requests[0].Generations) == 1
	}); err != nil {
		t.Fatalf("interval flush not observed: %v", err)
	}
}

func TestShutdownFlushesPendingGenerations(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      10,
			BatchSize:      10,
			FlushInterval:  time.Hour,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testGenerationExporter: exporter,
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("unexpected enqueue error: %v", err)
	}

	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.requests) != 1 {
		t.Fatalf("expected one flush on shutdown, got %d", len(exporter.requests))
	}
	if len(exporter.requests[0].Generations) != 1 {
		t.Fatalf("expected one generation in shutdown flush, got %d", len(exporter.requests[0].Generations))
	}
}

// TestFlushReportsPriorIntervalFailure pins the Flush() contract: an
// interval-driven flush that failed (with its error only logged) must
// surface that error on the next explicit Flush call. Without this,
// hooks that use Flush as a durability checkpoint silently treat data
// loss as success and delete their on-disk retry state.
func TestFlushReportsPriorIntervalFailure(t *testing.T) {
	wantErr := errors.New("boom")
	exporter := &capturingGenerationExporter{err: wantErr}
	// Use a synchronized buffer so we can poll the worker's log output
	// from the test goroutine without racing the logger.
	logSink := &syncBuffer{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      10,
			BatchSize:      100,
			FlushInterval:  10 * time.Millisecond,
			MaxRetries:     1,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Logger:                 log.New(logSink, "", 0),
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
	rec.SetResult(Generation{
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("unexpected enqueue error: %v", err)
	}

	// Wait for the worker to log the failed export. The log line is
	// emitted from the same block that records pendingErr, so seeing
	// it proves pendingErr is set before we call Flush.
	if err := waitForCondition(500*time.Millisecond, func() bool {
		return strings.Contains(logSink.String(), "sigil generation export failed")
	}); err != nil {
		t.Fatalf("interval-driven export never reported failure: %v\nlog so far: %q", err, logSink.String())
	}

	// Clear the injected failure so the explicit Flush itself has nothing
	// to send — we want to assert it surfaces the prior async failure.
	exporter.setExportErr(nil)

	err := client.Flush(context.Background())
	if err == nil {
		t.Fatal("expected Flush to surface prior interval failure, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("Flush err = %v, want it to wrap %v", err, wantErr)
	}

	// A second Flush has no pending error to surface and nothing queued.
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("expected clean Flush after pending error consumed, got %v", err)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestFlushDrainsQueuedGenerations pins that an explicit Flush exports
// every generation enqueued before the call, including items still on
// the channel that the worker hadn't pulled into the batch yet. Without
// the drain step the worker's select can service flushReq first, see
// an empty batch, and return nil while items linger on c.queue.
func TestFlushDrainsQueuedGenerations(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      200,
			BatchSize:      200,
			FlushInterval:  time.Hour, // disable the interval timer
			MaxRetries:     0,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	const n = 50
	for i := 0; i < n; i++ {
		_, rec := client.StartGeneration(context.Background(), GenerationStart{Model: ModelRef{Provider: "openai", Name: "gpt-5"}})
		rec.SetResult(Generation{
			Input:  []Message{UserTextMessage("hello")},
			Output: []Message{AssistantTextMessage("hi")},
		}, nil)
		rec.End()
		if err := rec.Err(); err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}

	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	total := 0
	for _, r := range exporter.requests {
		total += len(r.Generations)
	}
	if total != n {
		t.Fatalf("Flush exported %d generations across %d requests; want %d", total, len(exporter.requests), n)
	}
}

func TestMergeGenerationExportConfigInsecure(t *testing.T) {
	testCases := []struct {
		name             string
		baseInsecure     *bool
		overrideInsecure *bool
		wantInsecure     *bool
	}{
		{
			name:             "override unset preserves base",
			baseInsecure:     BoolPtr(true),
			overrideInsecure: nil,
			wantInsecure:     BoolPtr(true),
		},
		{
			name:             "override false replaces base true",
			baseInsecure:     BoolPtr(true),
			overrideInsecure: BoolPtr(false),
			wantInsecure:     BoolPtr(false),
		},
		{
			name:             "override true replaces base false",
			baseInsecure:     BoolPtr(false),
			overrideInsecure: BoolPtr(true),
			wantInsecure:     BoolPtr(true),
		},
		{
			name:             "both nil remains nil",
			baseInsecure:     nil,
			overrideInsecure: nil,
			wantInsecure:     nil,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			base := GenerationExportConfig{Insecure: testCase.baseInsecure}
			override := GenerationExportConfig{Insecure: testCase.overrideInsecure}
			got := mergeGenerationExportConfig(base, override)
			if (got.Insecure == nil) != (testCase.wantInsecure == nil) {
				t.Fatalf("insecure=%v, want %v", got.Insecure, testCase.wantInsecure)
			}
			if got.Insecure != nil && *got.Insecure != *testCase.wantInsecure {
				t.Fatalf("insecure=%v, want %v", *got.Insecure, *testCase.wantInsecure)
			}
		})
	}
}

func TestMergeGenerationExportConfigGRPCMessageLimits(t *testing.T) {
	base := GenerationExportConfig{
		GRPCMaxSendMessageBytes:    2 << 20,
		GRPCMaxReceiveMessageBytes: 3 << 20,
	}
	override := GenerationExportConfig{
		GRPCMaxSendMessageBytes:    8 << 20,
		GRPCMaxReceiveMessageBytes: 9 << 20,
	}
	got := mergeGenerationExportConfig(base, override)

	if got.GRPCMaxSendMessageBytes != 8<<20 {
		t.Fatalf("expected grpc max send 8MiB, got %d", got.GRPCMaxSendMessageBytes)
	}
	if got.GRPCMaxReceiveMessageBytes != 9<<20 {
		t.Fatalf("expected grpc max receive 9MiB, got %d", got.GRPCMaxReceiveMessageBytes)
	}
}

func TestNewHTTPGenerationExporterUsesEndpointScheme(t *testing.T) {
	testCases := []struct {
		name     string
		endpoint string
		insecure bool
		wantURL  string
	}{
		{
			name:     "explicit http endpoint remains http",
			endpoint: "http://localhost:8080/api/v1/generations:export",
			insecure: false,
			wantURL:  "http://localhost:8080/api/v1/generations:export",
		},
		{
			name:     "host endpoint uses insecure flag when no scheme",
			endpoint: "localhost:8080/api/v1/generations:export",
			insecure: true,
			wantURL:  "http://localhost:8080/api/v1/generations:export",
		},
		{
			name:     "missing path appends default ingest path",
			endpoint: "http://localhost:8080",
			insecure: true,
			wantURL:  "http://localhost:8080/api/v1/generations:export",
		},
		{
			name:     "trailing slash treated as missing path",
			endpoint: "http://localhost:8080/",
			insecure: true,
			wantURL:  "http://localhost:8080/api/v1/generations:export",
		},
		{
			name:     "https with no path appends default ingest path",
			endpoint: "https://stack.grafana.net",
			insecure: false,
			wantURL:  "https://stack.grafana.net/api/v1/generations:export",
		},
		{
			name:     "custom path is preserved",
			endpoint: "http://localhost:8080/custom/ingest",
			insecure: true,
			wantURL:  "http://localhost:8080/custom/ingest",
		},
		{
			name:     "uppercase scheme normalized to lowercase",
			endpoint: "HTTPS://stack.grafana.net",
			insecure: false,
			wantURL:  "https://stack.grafana.net/api/v1/generations:export",
		},
		{
			name:     "query string preserved when path appended",
			endpoint: "http://localhost:8080?token=abc",
			insecure: true,
			wantURL:  "http://localhost:8080/api/v1/generations:export?token=abc",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			exporter, err := newHTTPGenerationExporter(GenerationExportConfig{
				Endpoint: testCase.endpoint,
				Insecure: BoolPtr(testCase.insecure),
			})
			if err != nil {
				t.Fatalf("newHTTPGenerationExporter failed: %v", err)
			}
			httpExporter, ok := exporter.(*httpGenerationExporter)
			if !ok {
				t.Fatalf("unexpected exporter type %T", exporter)
			}
			if httpExporter.endpoint != testCase.wantURL {
				t.Fatalf("endpoint=%q, want %q", httpExporter.endpoint, testCase.wantURL)
			}
		})
	}
}

func waitForCondition(timeout time.Duration, condition func() bool) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.New("condition timed out")
}
