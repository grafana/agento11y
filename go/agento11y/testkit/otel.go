package testkit

import (
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

const otelInstrumentationName = "github.com/grafana/agento11y/go/agento11y/testkit"

// NewOTelEnv returns an OTel-export client with in-memory span and metric recorders.
func NewOTelEnv(t testing.TB, opts ...func(*agento11y.Config)) *Env {
	t.Helper()

	ClearAmbientEnv()

	spanRecorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spanRecorder))
	metricReader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricReader))

	cfg := agento11y.DefaultConfig()
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolOTel
	cfg.EnableExperimentalFeatures = agento11y.BoolPtr(true)
	cfg.ContentCapture = agento11y.ContentCaptureModeFull
	cfg.Tracer = tracerProvider.Tracer(otelInstrumentationName)
	cfg.Meter = meterProvider.Meter(otelInstrumentationName)
	cfg.TracerProvider = tracerProvider
	cfg.MeterProvider = meterProvider
	cfg.Flusher = tracerProvider
	cfg.Now = time.Now
	for _, opt := range opts {
		opt(&cfg)
	}

	env := &Env{
		Client:         agento11y.NewClient(cfg),
		Spans:          spanRecorder,
		Metrics:        metricReader,
		tracerProvider: tracerProvider,
		meterProvider:  meterProvider,
	}
	t.Cleanup(func() {
		if err := env.close(); err != nil {
			t.Errorf("close agento11y OTel test env: %v", err)
		}
	})
	return env
}
