//go:build conformance

package conformance_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/grafana/agento11y/go/agento11y/otelhook"
	"github.com/grafana/agento11y/go/otelgenai"
	"github.com/grafana/agento11y/go/otelgenai/weavertest"
)

type expectedViolation struct {
	adviceID        string
	messageContains string
}

func (e expectedViolation) matches(violation weavertest.Violation) bool {
	return violation.ID == e.adviceID && strings.Contains(violation.Message, e.messageContains)
}

type scenario struct {
	name               string
	options            []otelgenai.Option
	emit               func(*otelgenai.Handler)
	expectedSpans      map[string]int
	expectedMetrics    []string
	expectedViolations []expectedViolation
}

type providers struct {
	traces  *sdktrace.TracerProvider
	metrics *sdkmetric.MeterProvider
	logs    *sdklog.LoggerProvider
}

func TestConformance(t *testing.T) {
	if _, err := exec.LookPath("weaver"); err != nil {
		t.Skip("weaver is not on PATH")
	}
	registryRef := os.Getenv("SEMCONV_GENAI_REF")
	if registryRef == "" {
		t.Fatal("SEMCONV_GENAI_REF is not set; run the conformance test through mise")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	assets, err := weavertest.Setup(ctx, registryRef)
	if err != nil {
		t.Fatalf("prepare Weaver inputs: %v", err)
	}

	for _, test := range conformanceScenarios() {
		t.Run(test.name, func(t *testing.T) {
			report := executeScenario(t, assets, test)
			if err := reconcileViolations(report.Violations(), test.expectedViolations); err != nil {
				t.Error(err)
			}
			if got := report.SpanOperationCounts(); !maps.Equal(got, test.expectedSpans) {
				t.Errorf("span operation counts = %v, want %v", got, test.expectedSpans)
			}
			expectedMetrics := make(map[string]struct{}, len(test.expectedMetrics))
			for _, metric := range test.expectedMetrics {
				expectedMetrics[metric] = struct{}{}
			}
			if got := report.SeenMetricNames(); !maps.Equal(got, expectedMetrics) {
				t.Errorf("metric names = %v, want %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(expectedMetrics)))
			}
		})
	}

	t.Run("rejects an out-of-enum operation name", func(t *testing.T) {
		test := scenario{
			name: "invalid_operation",
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Operation = otelgenai.Operation("generateText")
				recordInvocation(handler, inv)
			},
		}
		report := executeScenario(t, assets, test)
		violations := report.Violations()
		if err := reconcileViolations(violations, nil); err == nil {
			t.Fatal("out-of-enum operation passed the violation check")
		}
		if !hasViolation(violations, "undefined_enum_variant") {
			t.Errorf("violations do not include undefined_enum_variant: %v", violations)
		}
	})
}

func conformanceScenarios() []scenario {
	return []scenario{
		{
			name: "plain inference",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, inferenceInvocation())
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "streaming inference",
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Stream = true
				ctx := handler.Start(context.Background(), inv)
				handler.RecordChunk(ctx, inv)
				time.Sleep(time.Millisecond)
				handler.RecordChunk(ctx, inv)
				inv.CompletedAt = time.Now()
				handler.End(ctx, inv)
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
				"gen_ai.client.operation.time_to_first_chunk",
				"gen_ai.client.operation.time_per_output_chunk",
			},
		},
		{
			name: "tool execution",
			emit: func(handler *otelgenai.Handler) {
				recordInvocation(handler, &otelgenai.Invocation{
					Operation:       otelgenai.OperationExecuteTool,
					ToolName:        "weather",
					ToolCallID:      "call-1",
					ToolType:        "function",
					ToolDescription: "Returns the weather",
					StartedAt:       time.Now().Add(-time.Second),
				})
			},
			expectedSpans:   map[string]int{"execute_tool": 1},
			expectedMetrics: []string{"gen_ai.client.operation.duration"},
		},
		{
			name: "embeddings",
			emit: func(handler *otelgenai.Handler) {
				dimensions := int64(1536)
				recordInvocation(handler, &otelgenai.Invocation{
					Operation:      otelgenai.OperationEmbeddings,
					Provider:       "openai",
					RequestModel:   "text-embedding-3-small",
					ResponseModel:  "text-embedding-3-small",
					ServerAddress:  "api.openai.com",
					DimensionCount: &dimensions,
					Usage:          otelgenai.Usage{InputTokens: 12},
					StartedAt:      time.Now().Add(-time.Second),
				})
			},
			expectedSpans: map[string]int{"embeddings": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name: "failed inference",
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.ErrorType = "timeout"
				inv.ErrorMessage = "request timed out"
				recordInvocation(handler, inv)
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name:    "span and event content",
			options: []otelgenai.Option{otelgenai.WithCaptureMode(otelgenai.CaptureSpanAndEvent)},
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.SystemInstructions = otelgenai.SystemInstructionsFromText("Answer briefly.")
				inv.InputMessages = []otelgenai.Message{{Role: otelgenai.RoleUser, Parts: []otelgenai.Part{otelgenai.TextPart("Hello")}}}
				finishReason := "stop"
				inv.OutputMessages = []otelgenai.Message{{
					Role:         otelgenai.RoleAssistant,
					Parts:        []otelgenai.Part{otelgenai.TextPart("Hi")},
					FinishReason: &finishReason,
				}}
				recordInvocation(handler, inv)
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
		},
		{
			name:    "agento11y extension attributes",
			options: []otelgenai.Option{otelgenai.WithEndHook(otelhook.New())},
			emit: func(handler *otelgenai.Handler) {
				inv := inferenceInvocation()
				inv.Vendor = otelhook.Generation{
					ID:       "gen-1",
					Metadata: map[string]any{"source": "conformance"},
				}
				recordInvocation(handler, inv)
			},
			expectedSpans: map[string]int{"chat": 1},
			expectedMetrics: []string{
				"gen_ai.client.operation.duration",
				"gen_ai.client.token.usage",
			},
			expectedViolations: []expectedViolation{
				{adviceID: "missing_attribute", messageContains: "agento11y.record"},
				{adviceID: "missing_attribute", messageContains: "agento11y.generation.id"},
				{adviceID: "missing_attribute", messageContains: "agento11y.generation.metadata"},
			},
		},
	}
}

func inferenceInvocation() *otelgenai.Invocation {
	return &otelgenai.Invocation{
		Operation:       otelgenai.OperationChat,
		Provider:        "openai",
		RequestModel:    "gpt-4.1-mini",
		ResponseModel:   "gpt-4.1-mini-2025-04-14",
		ResponseID:      "resp-1",
		ServerAddress:   "api.openai.com",
		FinishReasons:   []string{"stop"},
		Usage:           otelgenai.Usage{InputTokens: 12, OutputTokens: 7},
		StartedAt:       time.Now().Add(-time.Second),
		InputMessages:   nil,
		OutputMessages:  nil,
		ToolDefinitions: nil,
	}
}

func recordInvocation(handler *otelgenai.Handler, inv *otelgenai.Invocation) {
	ctx := handler.Start(context.Background(), inv)
	inv.CompletedAt = time.Now()
	handler.End(ctx, inv)
}

func executeScenario(t *testing.T, assets weavertest.Assets, test scenario) weavertest.Report {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	weaver, err := weavertest.Start(ctx, assets)
	if errors.Is(err, weavertest.ErrNotInstalled) {
		t.Skip("weaver is not on PATH")
	}
	if err != nil {
		t.Fatalf("start Weaver: %v", err)
	}
	defer weaver.Close()

	providers, err := newProviders(ctx, weaver.Endpoint())
	if err != nil {
		t.Fatalf("create OTLP providers: %v", err)
	}
	defer providers.shutdown()

	options := []otelgenai.Option{
		otelgenai.WithTracerProvider(providers.traces),
		otelgenai.WithMeterProvider(providers.metrics),
		otelgenai.WithLoggerProvider(providers.logs),
		otelgenai.WithConformantMetrics(),
	}
	options = append(options, test.options...)
	handler := otelgenai.NewHandler(options...)
	test.emit(handler)

	if err := providers.forceFlush(ctx); err != nil {
		t.Fatalf("flush OTLP telemetry: %v", err)
	}
	report, err := weaver.End(ctx)
	if err != nil {
		t.Fatalf("stop Weaver: %v", err)
	}
	dumpReport(t, test.name, report)
	return report
}

func newProviders(ctx context.Context, endpoint string) (providers, error) {
	traceExporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(endpoint), otlptracegrpc.WithInsecure())
	if err != nil {
		return providers{}, err
	}
	traces := sdktrace.NewTracerProvider(sdktrace.WithSyncer(traceExporter))

	metricExporter, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(endpoint), otlpmetricgrpc.WithInsecure())
	if err != nil {
		_ = traces.Shutdown(ctx)
		return providers{}, err
	}
	reader := sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(time.Hour))
	metrics := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	logExporter, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpoint(endpoint), otlploggrpc.WithInsecure())
	if err != nil {
		_ = traces.Shutdown(ctx)
		_ = metrics.Shutdown(ctx)
		return providers{}, err
	}
	logs := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(logExporter)))
	return providers{traces: traces, metrics: metrics, logs: logs}, nil
}

func (p providers) forceFlush(ctx context.Context) error {
	return errors.Join(
		p.traces.ForceFlush(ctx),
		p.metrics.ForceFlush(ctx),
		p.logs.ForceFlush(ctx),
	)
}

func (p providers) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.logs.Shutdown(ctx)
	_ = p.metrics.Shutdown(ctx)
	_ = p.traces.Shutdown(ctx)
}

func reconcileViolations(violations []weavertest.Violation, expected []expectedViolation) error {
	var problems []string
	for _, violation := range violations {
		matched := false
		for _, allowed := range expected {
			matched = matched || allowed.matches(violation)
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("unexpected [%s] %s", violation.ID, violation.Message))
		}
	}
	for _, allowed := range expected {
		matched := false
		for _, violation := range violations {
			matched = matched || allowed.matches(violation)
		}
		if !matched {
			problems = append(problems, fmt.Sprintf("allowlisted violation was not reported: [%s] %s", allowed.adviceID, allowed.messageContains))
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

func hasViolation(violations []weavertest.Violation, adviceID string) bool {
	for _, violation := range violations {
		if violation.ID == adviceID {
			return true
		}
	}
	return false
}

func dumpReport(t *testing.T, name string, report weavertest.Report) {
	t.Helper()
	contents, err := json.MarshalIndent(report.Raw, "", "  ")
	if err != nil {
		t.Errorf("encode Weaver report: %v", err)
		return
	}
	if err := os.MkdirAll("weaver_reports", 0o755); err != nil {
		t.Errorf("create report directory: %v", err)
		return
	}
	filename := strings.ReplaceAll(name, " ", "_") + ".json"
	path := filepath.Join("weaver_reports", filename)
	if err := os.WriteFile(path, append(contents, '\n'), 0o644); err != nil {
		t.Errorf("write Weaver report: %v", err)
		return
	}
	t.Logf("Weaver report: %s", path)
}
