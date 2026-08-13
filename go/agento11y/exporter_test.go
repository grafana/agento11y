package agento11y

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestExportGenerationBypassesAsyncQueue(t *testing.T) {
	exporter := &capturingGenerationExporter{}
	client := NewClient(Config{
		GenerationExport: GenerationExportConfig{
			QueueSize:      1,
			MaxRetries:     0,
			InitialBackoff: time.Millisecond,
			MaxBackoff:     time.Millisecond,
		},
		Tracer:                 noop.NewTracerProvider().Tracer("test"),
		Now:                    time.Now,
		testDisableWorker:      true,
		testGenerationExporter: exporter,
	})
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})

	err := client.ExportGeneration(context.Background(), GenerationStart{
		ID: "generation-1", Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	}, Generation{
		ID:     "generation-1",
		Input:  []Message{UserTextMessage("hello")},
		Output: []Message{AssistantTextMessage("hi")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if queued := len(client.queue); queued != 0 {
		t.Fatalf("synchronous export enqueued %d generations", queued)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if len(exporter.requests) != 1 || len(exporter.requests[0].Generations) != 1 {
		t.Fatalf("unexpected synchronous exports: %#v", exporter.requests)
	}
	if got := exporter.requests[0].Generations[0].Id; got != "generation-1" {
		t.Fatalf("generation ID = %q, want generation-1", got)
	}
}

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

	for range 2 {
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

func TestResetBatchClearsReferences(t *testing.T) {
	first := 1
	second := 2
	batch := []*int{&first, &second}
	backing := batch

	batch = resetBatch(batch)

	if len(batch) != 0 {
		t.Fatalf("reset batch length = %d, want 0", len(batch))
	}
	for i, value := range backing {
		if value != nil {
			t.Errorf("backing array slot %d still retains a reference", i)
		}
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
		return strings.Contains(logSink.String(), "agento11y generation export failed")
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
	for i := range n {
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

// TestMergeGenerationExportConfigTimeouts pins the caller-wins-over-env layering
// for both export timeouts: base is the env/default layer, override the caller.
func TestMergeGenerationExportConfigTimeouts(t *testing.T) {
	testCases := []struct {
		name              string
		base              GenerationExportConfig
		override          GenerationExportConfig
		wantExportTimeout time.Duration
		wantHTTPTimeout   time.Duration
	}{
		{
			name:              "caller export timeout wins over env value",
			base:              GenerationExportConfig{ExportTimeout: 30 * time.Second},
			override:          GenerationExportConfig{ExportTimeout: 2 * time.Second},
			wantExportTimeout: 2 * time.Second,
		},
		{
			name:              "zero caller export timeout keeps env value",
			base:              GenerationExportConfig{ExportTimeout: 7 * time.Second},
			override:          GenerationExportConfig{},
			wantExportTimeout: 7 * time.Second,
		},
		{
			name:              "negative caller export timeout keeps env value",
			base:              GenerationExportConfig{ExportTimeout: 7 * time.Second},
			override:          GenerationExportConfig{ExportTimeout: -time.Second},
			wantExportTimeout: 7 * time.Second,
		},
		{
			name:              "http timeout and export timeout merge independently",
			base:              GenerationExportConfig{ExportTimeout: 30 * time.Second, HTTPTimeout: time.Second},
			override:          GenerationExportConfig{HTTPTimeout: 5 * time.Second},
			wantExportTimeout: 30 * time.Second,
			wantHTTPTimeout:   5 * time.Second,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			got := mergeGenerationExportConfig(testCase.base, testCase.override)
			if got.ExportTimeout != testCase.wantExportTimeout {
				t.Fatalf("ExportTimeout=%v, want %v", got.ExportTimeout, testCase.wantExportTimeout)
			}
			if got.HTTPTimeout != testCase.wantHTTPTimeout {
				t.Fatalf("HTTPTimeout=%v, want %v", got.HTTPTimeout, testCase.wantHTTPTimeout)
			}
		})
	}
}

// TestResolveExportTimeout covers the single resolution point used by the HTTP
// client timeout and by every per-attempt export context.
func TestResolveExportTimeout(t *testing.T) {
	testCases := []struct {
		name     string
		protocol GenerationExportProtocol
		cfg      GenerationExportConfig
		want     time.Duration
	}{
		{
			name:     "http unset uses default",
			protocol: GenerationExportProtocolHTTP,
			want:     defaultExportTimeout,
		},
		{
			name:     "grpc unset uses default",
			protocol: GenerationExportProtocolGRPC,
			want:     defaultExportTimeout,
		},
		{
			name:     "http uses export timeout when no http timeout",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{ExportTimeout: 2 * time.Second},
			want:     2 * time.Second,
		},
		{
			name:     "grpc uses export timeout",
			protocol: GenerationExportProtocolGRPC,
			cfg:      GenerationExportConfig{ExportTimeout: 2 * time.Second},
			want:     2 * time.Second,
		},
		{
			// experiments.NewClient maps ClientOptions.RetryTimeout onto
			// HTTPTimeout, so a positive RetryTimeout keeps its exact bound.
			name:     "http timeout overrides export timeout",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{ExportTimeout: 2 * time.Second, HTTPTimeout: 5 * time.Second},
			want:     5 * time.Second,
		},
		{
			name:     "http timeout overrides default",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{HTTPTimeout: 5 * time.Second},
			want:     5 * time.Second,
		},
		{
			name:     "grpc ignores http timeout",
			protocol: GenerationExportProtocolGRPC,
			cfg:      GenerationExportConfig{ExportTimeout: 2 * time.Second, HTTPTimeout: 5 * time.Second},
			want:     2 * time.Second,
		},
		{
			name:     "grpc ignores http timeout and falls back to default",
			protocol: GenerationExportProtocolGRPC,
			cfg:      GenerationExportConfig{HTTPTimeout: 5 * time.Second},
			want:     defaultExportTimeout,
		},
		{
			name:     "non-positive export timeout uses default",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{ExportTimeout: -time.Second},
			want:     defaultExportTimeout,
		},
		{
			name:     "non-positive http timeout defers to export timeout",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{ExportTimeout: 2 * time.Second, HTTPTimeout: -time.Second},
			want:     2 * time.Second,
		},
		{
			name:     "none protocol uses export timeout",
			protocol: GenerationExportProtocolNone,
			cfg:      GenerationExportConfig{ExportTimeout: 3 * time.Second, HTTPTimeout: 5 * time.Second},
			want:     3 * time.Second,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testCase.cfg
			cfg.Protocol = testCase.protocol
			if got := resolveExportTimeout(cfg); got != testCase.want {
				t.Fatalf("resolveExportTimeout=%v, want %v", got, testCase.want)
			}
		})
	}
}

// TestNewHTTPGenerationExporterTimeout pins http.Client.Timeout to the same
// resolved value the per-attempt context uses, so neither can silently outlive
// the other.
func TestNewHTTPGenerationExporterTimeout(t *testing.T) {
	testCases := []struct {
		name string
		cfg  GenerationExportConfig
		want time.Duration
	}{
		{
			name: "unset uses default",
			want: defaultExportTimeout,
		},
		{
			name: "export timeout applies to http",
			cfg:  GenerationExportConfig{ExportTimeout: 2 * time.Second},
			want: 2 * time.Second,
		},
		{
			name: "experiments retry timeout still wins",
			cfg:  GenerationExportConfig{ExportTimeout: 30 * time.Second, HTTPTimeout: 5 * time.Second},
			want: 5 * time.Second,
		},
		{
			name: "non-positive export timeout uses default",
			cfg:  GenerationExportConfig{ExportTimeout: -time.Second},
			want: defaultExportTimeout,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			cfg := testCase.cfg
			cfg.Protocol = GenerationExportProtocolHTTP
			cfg.Endpoint = "http://localhost:8080"
			cfg.Insecure = BoolPtr(true)

			exporter, err := newGenerationExporter(cfg)
			if err != nil {
				t.Fatalf("newGenerationExporter failed: %v", err)
			}
			httpExporter, ok := exporter.(*httpGenerationExporter)
			if !ok {
				t.Fatalf("unexpected exporter type %T", exporter)
			}
			if httpExporter.client.Timeout != testCase.want {
				t.Fatalf("http client timeout=%v, want %v", httpExporter.client.Timeout, testCase.want)
			}
			if got := resolveExportTimeout(cfg); got != httpExporter.client.Timeout {
				t.Fatalf("attempt timeout=%v, want http client timeout %v", got, httpExporter.client.Timeout)
			}
		})
	}
}

// TestExportAttemptDeadlines asserts every generation and workflow-step export
// attempt is bounded by the resolved timeout, including retries.
func TestExportAttemptDeadlines(t *testing.T) {
	if defaultExportTimeout != 30*time.Second {
		t.Fatalf("defaultExportTimeout=%v, want 30s", defaultExportTimeout)
	}

	testCases := []struct {
		name     string
		protocol GenerationExportProtocol
		cfg      GenerationExportConfig
		want     time.Duration
	}{
		{
			name:     "grpc default",
			protocol: GenerationExportProtocolGRPC,
			want:     defaultExportTimeout,
		},
		{
			name:     "grpc configured export timeout",
			protocol: GenerationExportProtocolGRPC,
			cfg:      GenerationExportConfig{ExportTimeout: 90 * time.Second},
			want:     90 * time.Second,
		},
		{
			name:     "grpc ignores http timeout",
			protocol: GenerationExportProtocolGRPC,
			cfg:      GenerationExportConfig{ExportTimeout: 90 * time.Second, HTTPTimeout: 5 * time.Second},
			want:     90 * time.Second,
		},
		{
			name:     "http timeout bounds http attempts",
			protocol: GenerationExportProtocolHTTP,
			cfg:      GenerationExportConfig{ExportTimeout: 90 * time.Second, HTTPTimeout: 5 * time.Second},
			want:     5 * time.Second,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var (
				mu        sync.Mutex
				deadlines []time.Duration
			)
			recordDeadline := func(ctx context.Context) {
				deadline, ok := ctx.Deadline()
				if !ok {
					t.Errorf("export attempt context has no deadline")
					return
				}
				mu.Lock()
				deadlines = append(deadlines, time.Until(deadline))
				mu.Unlock()
			}
			exporter := &capturingGenerationExporter{
				export: func(ctx context.Context, _ *agento11yv1.ExportGenerationsRequest) (*agento11yv1.ExportGenerationsResponse, error) {
					recordDeadline(ctx)
					return nil, errors.New("boom")
				},
				exportWorkflowSteps: func(ctx context.Context, _ *agento11yv1.ExportWorkflowStepsRequest) (*agento11yv1.ExportWorkflowStepsResponse, error) {
					recordDeadline(ctx)
					return nil, errors.New("boom")
				},
			}

			generationExport := testCase.cfg
			generationExport.Protocol = testCase.protocol
			generationExport.Endpoint = "localhost:4317"
			generationExport.QueueSize = 1
			generationExport.MaxRetries = 1
			generationExport.InitialBackoff = time.Millisecond
			generationExport.MaxBackoff = time.Millisecond

			client := NewClient(Config{
				GenerationExport:       generationExport,
				Tracer:                 noop.NewTracerProvider().Tracer("test"),
				Logger:                 log.New(io.Discard, "", 0),
				testDisableWorker:      true,
				testGenerationExporter: exporter,
			})
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			if err := client.exportWithRetry(&agento11yv1.ExportGenerationsRequest{
				Generations: []*agento11yv1.Generation{{Id: "gen-1"}},
			}); err == nil {
				t.Fatal("expected generation export error")
			}
			if err := client.exportWorkflowStepsWithRetry(&agento11yv1.ExportWorkflowStepsRequest{
				WorkflowSteps: []*agento11yv1.WorkflowStep{{Id: "step-1"}},
			}); err == nil {
				t.Fatal("expected workflow step export error")
			}

			mu.Lock()
			defer mu.Unlock()
			// MaxRetries=1 means two attempts per export path.
			if len(deadlines) != 4 {
				t.Fatalf("recorded %d attempt deadlines, want 4", len(deadlines))
			}
			for i, remaining := range deadlines {
				if remaining > testCase.want || remaining < testCase.want-time.Second {
					t.Fatalf("attempt %d deadline in %v, want ~%v", i, remaining, testCase.want)
				}
			}
		})
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
