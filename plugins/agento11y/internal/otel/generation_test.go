package otel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"go.opentelemetry.io/otel/attribute"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestGenerationProviderIgnoresSamplingAndLimits(t *testing.T) {
	for _, key := range []string{"OTEL_SPAN_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_ATTRIBUTE_VALUE_LENGTH_LIMIT", "OTEL_SPAN_ATTRIBUTE_COUNT_LIMIT", "OTEL_ATTRIBUTE_COUNT_LIMIT", "OTEL_SPAN_EVENT_COUNT_LIMIT", "OTEL_SPAN_LINK_COUNT_LIMIT", "OTEL_EVENT_ATTRIBUTE_COUNT_LIMIT", "OTEL_LINK_ATTRIBUTE_COUNT_LIMIT"} {
		t.Setenv(key, "1")
	}
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	exporter := tracetest.NewInMemoryExporter()
	ids := fixedGenerationIDs{}
	p := newGenerationProvider(exporter, sdkresource.Empty(), ids)
	defer func() { _ = p.shutdown(context.Background()) }()
	parent := trace.NewSpanContext(trace.SpanContextConfig{TraceID: trace.TraceID{3}, SpanID: trace.SpanID{4}, Remote: true})
	attrs := make([]attribute.KeyValue, 200)
	for i := range attrs {
		attrs[i] = attribute.String(fmt.Sprintf("key%d", i), strings.Repeat("payload", 100))
	}
	links := []trace.Link{{SpanContext: parent, Attributes: attrs}, {SpanContext: parent, Attributes: attrs}}
	ctx := trace.ContextWithRemoteSpanContext(context.Background(), parent)
	_, span := p.tp.Tracer("generation").Start(ctx, "generation", trace.WithAttributes(attrs...), trace.WithLinks(links...))
	for range 2 {
		span.AddEvent("event", trace.WithAttributes(attrs...))
	}
	span.End()
	// Also exercise NewIDs rather than just NewSpanID under the remote parent.
	_, root := p.tp.Tracer("generation").Start(context.Background(), "root")
	root.End()
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	spans := exporter.GetSpans()
	if len(spans) != 2 {
		t.Fatalf("got %d spans under always_off sampler", len(spans))
	}
	s := spans[0]
	if !s.SpanContext.IsSampled() || s.SpanContext.TraceID() != parent.TraceID() || s.SpanContext.SpanID() != ids.spanID() {
		t.Fatalf("sampling/identity not preserved: %v", s.SpanContext)
	}
	if spans[1].SpanContext.TraceID() != ids.traceID() {
		t.Fatal("supplied root IDGenerator was not used")
	}
	if len(s.Attributes) != len(attrs) || s.Attributes[0].Value.AsString() != attrs[0].Value.AsString() {
		t.Fatal("span attributes limited/truncated")
	}
	if len(s.Events) != 2 || len(s.Events[0].Attributes) != len(attrs) || s.Events[0].Attributes[0].Value.AsString() != attrs[0].Value.AsString() {
		t.Fatal("event attributes limited/truncated")
	}
	if len(s.Links) != 2 || len(s.Links[0].Attributes) != len(attrs) || s.Links[0].Attributes[0].Value.AsString() != attrs[0].Value.AsString() {
		t.Fatal("link attributes limited/truncated")
	}
}

type fixedGenerationIDs struct{}

func (fixedGenerationIDs) traceID() trace.TraceID { return trace.TraceID{1} }
func (fixedGenerationIDs) spanID() trace.SpanID   { return trace.SpanID{2} }
func (i fixedGenerationIDs) NewIDs(context.Context) (trace.TraceID, trace.SpanID) {
	return i.traceID(), i.spanID()
}
func (i fixedGenerationIDs) NewSpanID(context.Context, trace.TraceID) trace.SpanID { return i.spanID() }

func TestGenerationProviderStickyFailures(t *testing.T) {
	failure := errors.New("collector rejected generation")
	exporter := &failingGenerationExporter{failure: failure}
	p := newGenerationProvider(exporter, sdkresource.Empty(), nil)
	defer func() { _ = p.shutdown(context.Background()) }()
	_, span := p.tp.Tracer("generation").Start(context.Background(), "generation")
	span.End()
	// Let the background batch fail before the first explicit flush.
	deadline := time.Now().Add(5 * time.Second)
	for p.exporter.peekError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("background batch did not fail")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for range 2 {
		if err := p.ForceFlush(context.Background()); !errors.Is(err, failure) {
			t.Fatalf("lost sticky failure: %v", err)
		}
	}
	// A later successful batch must not clear evidence of a lost record.
	exporter.mu.Lock()
	exporter.failure = nil
	exporter.mu.Unlock()
	_, successful := p.tp.Tracer("generation").Start(context.Background(), "later")
	successful.End()
	if err := p.ForceFlush(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("successful export cleared sticky failure: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.ForceFlush(ctx); !errors.Is(err, failure) || !errors.Is(err, context.Canceled) {
		t.Fatalf("lost failure or cancellation: %v", err)
	}
	if err := p.ForceFlush(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("canceled flush cleared failure: %v", err)
	}
	if err := p.shutdown(context.Background()); !errors.Is(err, failure) {
		t.Fatalf("shutdown lost failure: %v", err)
	}
}

func TestGenerationProviderCanceledFlush(t *testing.T) {
	p := newGenerationProvider(tracetest.NewInMemoryExporter(), sdkresource.Empty(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := p.ForceFlush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled flush = %v", err)
	}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatalf("cancellation poisoned healthy exporter: %v", err)
	}
	if err := p.shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.ForceFlush(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled flush after shutdown = %v", err)
	}
}

func TestGenerationProviderCancellationDuringFlush(t *testing.T) {
	exporter := &canceledGenerationExporter{started: make(chan struct{})}
	p := newGenerationProvider(exporter, sdkresource.Empty(), nil)
	defer func() { _ = p.shutdown(context.Background()) }()
	_, span := p.tp.Tracer("test").Start(context.Background(), "generation")
	span.End()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.ForceFlush(ctx) }()
	select {
	case <-exporter.started:
	case <-time.After(2 * time.Second):
		t.Fatal("export never started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("flush = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("flush ignored cancellation")
	}
	deadline := time.Now().Add(time.Second)
	for p.exporter.peekError() == nil {
		if time.Now().After(deadline) {
			t.Fatal("export failure was not remembered")
		}
		time.Sleep(time.Millisecond)
	}
	for range 2 {
		if err := p.ForceFlush(context.Background()); !errors.Is(err, context.Canceled) {
			t.Fatalf("lost canceled export failure: %v", err)
		}
	}
}

type canceledGenerationExporter struct{ started chan struct{} }

func (e *canceledGenerationExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	close(e.started)
	<-ctx.Done()
	return ctx.Err()
}
func (*canceledGenerationExporter) Shutdown(context.Context) error { return nil }

type failingGenerationExporter struct {
	failure error
	mu      sync.Mutex
	batches []int
}

func (e *failingGenerationExporter) ExportSpans(_ context.Context, spans []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.batches = append(e.batches, len(spans))
	return e.failure
}
func (*failingGenerationExporter) Shutdown(context.Context) error { return nil }

func TestGenerationBatchSize(t *testing.T) {
	t.Setenv("OTEL_BSP_MAX_EXPORT_BATCH_SIZE", "512")
	t.Setenv("OTEL_BSP_MAX_QUEUE_SIZE", "1")
	exporter := &failingGenerationExporter{}
	p := newGenerationProvider(exporter, sdkresource.Empty(), nil)
	defer func() { _ = p.shutdown(context.Background()) }()
	for range 201 {
		_, s := p.tp.Tracer("test").Start(context.Background(), "generation")
		s.End()
	}
	if err := p.ForceFlush(context.Background()); err != nil {
		t.Fatal(err)
	}
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	total := 0
	for _, n := range exporter.batches {
		if n > 100 {
			t.Fatalf("batch size = %d", n)
		}
		total += n
	}
	if total != 201 {
		t.Fatalf("exported %d spans", total)
	}
}

func TestSetupGenerationConfiguration(t *testing.T) {
	cases := []struct {
		name           string
		protocol       string
		optIn          string
		endpoint       string
		override       agento11y.GenerationExportProtocol
		experimental   *bool
		wantErr        bool
		wantProvider   bool
		wantGeneration bool
	}{
		{name: "HTTP missing endpoint remains disabled"},
		{name: "OTel missing endpoint", protocol: "otel", optIn: "true", wantErr: true},
		{name: "OTel missing opt-in", protocol: "otel", endpoint: "http://localhost:4318", wantErr: true},
		{name: "OTel invalid endpoint", protocol: "otel", optIn: "true", endpoint: "not-a-url", wantErr: true},
		{name: "OTel configured", protocol: "otel", optIn: "true", endpoint: "http://localhost:4318", wantProvider: true, wantGeneration: true},
		{name: "HTTP configured", endpoint: "http://localhost:4318", wantProvider: true},
		{name: "explicit HTTP bypasses ambient OTel", protocol: "otel", override: agento11y.GenerationExportProtocolHTTP},
		{name: "explicit OTel still needs opt-in", override: agento11y.GenerationExportProtocolOTel, endpoint: "http://localhost:4318", wantErr: true},
		{name: "explicit OTel", override: agento11y.GenerationExportProtocolOTel, optIn: "true", endpoint: "http://localhost:4318", wantProvider: true, wantGeneration: true},
		{name: "unknown protocol errors even without endpoint", protocol: "typo", wantErr: true},
		{name: "resolved opt-in survives ambient removal", override: agento11y.GenerationExportProtocolOTel, experimental: new(true), endpoint: "http://localhost:4318", wantProvider: true, wantGeneration: true},
		{name: "resolved opt-out overrides ambient opt-in", protocol: "otel", optIn: "true", experimental: new(false), endpoint: "http://localhost:4318", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv(agento11y.EnvEnableExperimentalFeatures, tc.optIn)
			p, err := SetupWithOptions(context.Background(), "test", Options{Endpoint: tc.endpoint, GenerationProtocol: tc.override, EnableExperimentalFeatures: tc.experimental})
			if (err != nil) != tc.wantErr {
				t.Fatalf("setup error = %v", err)
			}
			if (p != nil) != tc.wantProvider {
				t.Fatalf("providers = %v", p)
			}
			if p != nil {
				defer func() { _ = p.Shutdown(context.Background()) }()
			}
			if (p.GenerationTracerProvider() != nil) != tc.wantGeneration || (p.GenerationFlusher() != nil) != tc.wantGeneration {
				t.Fatal("generation getters do not match configuration")
			}
			if (p.MeterProvider() != nil) != tc.wantProvider {
				t.Fatal("missing meter provider")
			}
		})
	}
}

func TestSetupGenerationCanceledContext(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p, err := SetupWithOptions(ctx, "test", Options{Endpoint: "http://localhost:4318", GenerationProtocol: agento11y.GenerationExportProtocolOTel})
	if p != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled setup = (%v, %v)", p, err)
	}
}

func TestSetupGenerationExplicitEndpointReplacesInvalidAmbient(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
	t.Setenv("AGENTO11Y_PROTOCOL", "otel")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "invalid-ambient-url")
	p, err := SetupWithOptions(context.Background(), "test", Options{Endpoint: "http://localhost:4318"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	if p.GenerationTracerProvider() == nil {
		t.Fatal("explicit endpoint was not used")
	}
}

func TestSetupGenerationExportsFullWithMetadataSpans(t *testing.T) {
	envconfig.PinAliasEnvBlank(t)
	t.Setenv("AGENTO11Y_PROTOCOL", "otel")
	t.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")
	t.Setenv("OTEL_TRACES_SAMPLER", "always_off")
	capture := newOTLPCapture(t)
	defer capture.server.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", capture.server.URL+"/otlp")
	p, err := Setup(context.Background(), "generation-test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Shutdown(context.Background()) }()
	client := agento11y.NewClient(agento11y.Config{
		GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolOTel},
		ContentCapture:   envconfig.ResolveContentMode(nil),
		TracerProvider:   p.GenerationTracerProvider(),
		MeterProvider:    p.MeterProvider(),
		Flusher:          p.GenerationFlusher(),
	})
	defer func() { _ = client.Shutdown(context.Background()) }()
	_, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{Model: agento11y.ModelRef{Provider: "openai", Name: "test"}})
	rec.SetResult(agento11y.Generation{Input: []agento11y.Message{agento11y.UserTextMessage("FULL-CONTENT")}}, nil)
	rec.End()
	if err := client.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	capture.mu.Lock()
	defer capture.mu.Unlock()
	count := 0
	for _, req := range capture.traceReqs {
		for _, rs := range req.ResourceSpans {
			if findAttr(rs.Resource.Attributes, "service.instance.id") != "generation-test" {
				t.Fatal("generation resource lost instance identity")
			}
			for _, ss := range rs.ScopeSpans {
				for _, span := range ss.Spans {
					count++
					if !strings.Contains(findAttr(span.Attributes, "gen_ai.input.messages"), "FULL-CONTENT") {
						t.Fatal("full_with_metadata_spans was not upgraded to full")
					}
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("got %d generation spans", count)
	}
}

func TestGenerationFlushIsSeparateFromToolsAndMetrics(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(fmt.Sprintf("partial=%t", partial), func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv(agento11y.EnvEnableExperimentalFeatures, "true")
			var requests atomic.Int64
			server := newTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/traces") {
					return
				}
				requests.Add(1)
				if !partial {
					http.Error(w, "denied", http.StatusForbidden)
					return
				}
				body, err := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1}})
				if err != nil {
					t.Error(err)
					return
				}
				w.Header().Set("Content-Type", "application/x-protobuf")
				_, _ = w.Write(body)
			}))
			defer server.Close()
			p, err := SetupWithOptions(context.Background(), "test", Options{Endpoint: server.URL, GenerationProtocol: agento11y.GenerationExportProtocolOTel})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = p.Shutdown(context.Background()) }()
			_, span := p.GenerationTracerProvider().Tracer("test").Start(context.Background(), "generation")
			span.End()
			if err := p.GenerationFlusher().ForceFlush(context.Background()); err == nil {
				t.Fatal("generation export failed but flush claimed success")
			}
			if err := p.ForceFlush(); err != nil {
				t.Fatalf("generation failure contaminated tools/metrics flush: %v", err)
			}
			if err := p.GenerationFlusher().ForceFlush(context.Background()); err == nil {
				t.Fatal("tools/metrics flush cleared generation failure")
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("trace requests=%d, want 1 without retries", got)
			}
		})
	}
}
