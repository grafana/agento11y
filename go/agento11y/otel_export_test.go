package agento11y

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/log/logtest"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/otelgenai"
)

// newOTelTestClient builds a client in otel export mode with the experimental
// gate open, recording spans into the returned recorder.
func newOTelTestClient(t *testing.T, mutate func(*Config)) (*Client, *tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()

	t.Setenv(EnvEnableExperimentalFeatures, "true")

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	cfg := DefaultConfig()
	cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
	cfg.ContentCapture = ContentCaptureModeFull
	cfg.TracerProvider = provider
	cfg.Now = time.Now
	cfg.testGenerationExporter = &capturingGenerationExporter{}
	if mutate != nil {
		mutate(&cfg)
	}

	client := NewClient(cfg)
	t.Cleanup(func() {
		_ = client.Shutdown(context.Background())
	})
	return client, recorder, provider
}

func recordOTelGeneration(t *testing.T, client *Client, generation Generation) {
	t.Helper()

	_, recorder := client.StartStreamingGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	recorder.SetResult(generation, nil)
	recorder.End()
	if err := recorder.Err(); err != nil {
		t.Fatalf("recorder: %v", err)
	}
}

func onlySpan(t *testing.T, recorder *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	t.Helper()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}
	return ended[0]
}

func spanAttributeMapOf(span sdktrace.ReadOnlySpan) map[string]attribute.Value {
	out := make(map[string]attribute.Value, len(span.Attributes()))
	for _, kv := range span.Attributes() {
		out[string(kv.Key)] = kv.Value
	}
	return out
}

func TestOTelModeUsesTracerProviderInsteadOfDirectTracer(t *testing.T) {
	directRecorder := tracetest.NewSpanRecorder()
	directProvider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(directRecorder))
	t.Cleanup(func() { _ = directProvider.Shutdown(context.Background()) })

	client, providerRecorder, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.Tracer = directProvider.Tracer("direct-tracer")
	})
	recordOTelGeneration(t, client, Generation{Output: []Message{AssistantTextMessage("Hi!")}})

	if got := len(providerRecorder.Ended()); got != 1 {
		t.Fatalf("provider recorder holds %d spans, want 1", got)
	}
	if got := len(directRecorder.Ended()); got != 0 {
		t.Fatalf("direct tracer recorder holds %d spans, want 0", got)
	}
}

func TestOTelProtocolSpan(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		content Generation
		check   func(t *testing.T, span sdktrace.ReadOnlySpan)
	}{
		{
			name: "semconv span replaces the metadata span",
			content: Generation{
				Input:  []Message{UserTextMessage("Hello")},
				Output: []Message{AssistantTextMessage("Hi!")},
				Usage:  TokenUsage{InputTokens: 120, OutputTokens: 42, TotalTokens: 162},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				if got := span.Name(); got != "chat gpt-5" {
					t.Errorf("span name = %q, want %q", got, "chat gpt-5")
				}
				attrs := spanAttributeMapOf(span)
				if got := attrs["gen_ai.operation.name"].AsString(); got != "chat" {
					t.Errorf("gen_ai.operation.name = %q, want chat", got)
				}
				if !attrs["gen_ai.request.stream"].AsBool() {
					t.Error("gen_ai.request.stream is not true for a streaming generation")
				}
				if got := attrs["agento11y.gen_ai.usage.total_tokens"].AsInt64(); got != 162 {
					t.Errorf("total tokens = %d, want 162", got)
				}
				if got := attrs["gen_ai.input.messages"].AsString(); !strings.Contains(got, "Hello") {
					t.Errorf("gen_ai.input.messages = %q, want it to contain Hello", got)
				}
				if _, ok := attrs["agento11y.sdk.name"]; ok {
					t.Error("otel-mode span carries the metadata-span attributes")
				}
				if attrs["agento11y.generation.id"].AsString() == "" {
					t.Error("otel-mode span carries no agento11y.generation.id")
				}
			},
		},
		{
			name: "metadata only drops content",
			mutate: func(cfg *Config) {
				cfg.ContentCapture = ContentCaptureModeMetadataOnly
			},
			content: Generation{
				Input:  []Message{UserTextMessage("secret prompt")},
				Output: []Message{AssistantTextMessage("secret answer")},
				Usage:  TokenUsage{InputTokens: 10, OutputTokens: 5},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttributeMapOf(span)
				for _, key := range []string{
					"gen_ai.input.messages",
					"gen_ai.output.messages",
					"gen_ai.system_instructions",
					"gen_ai.tool.definitions",
				} {
					if _, ok := attrs[key]; ok {
						t.Errorf("metadata_only span carries %s", key)
					}
				}
				if attrs["agento11y.generation.id"].AsString() == "" {
					t.Error("metadata_only span lost the generation id")
				}
				if got := attrs["gen_ai.usage.input_tokens"].AsInt64(); got != 10 {
					t.Errorf("gen_ai.usage.input_tokens = %d, want 10", got)
				}
			},
		},
		{
			name: "tags ride as a document and as dimensions",
			mutate: func(cfg *Config) {
				cfg.Tags = map[string]string{"team": "sigil"}
			},
			content: Generation{Output: []Message{AssistantTextMessage("Hi!")}},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttributeMapOf(span)
				if got := attrs["agento11y.generation.tags"].AsString(); got != `{"team":"sigil"}` {
					t.Errorf("agento11y.generation.tags = %q, want the tags document", got)
				}
				if got := attrs["agento11y.tag.team"].AsString(); got != "sigil" {
					t.Errorf("agento11y.tag.team = %q, want sigil", got)
				}
			},
		},
		{
			name: "redaction runs before the span is encoded",
			mutate: func(cfg *Config) {
				cfg.GenerationSanitizer = NewSecretRedactionSanitizer(SecretRedactionOptions{
					RedactInputMessages: BoolPtr(true),
				})
			},
			content: Generation{
				Input: []Message{UserTextMessage("token ghp_0123456789abcdefghijklmnopqrstuvwxyz")},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				got := spanAttributeMapOf(span)["gen_ai.input.messages"].AsString()
				if strings.Contains(got, "ghp_0123456789abcdefghijklmnopqrstuvwxyz") {
					t.Errorf("gen_ai.input.messages = %q, want the secret redacted", got)
				}
			},
		},
		{
			name:    "usage a provider never returned is absent",
			content: Generation{Output: []Message{AssistantTextMessage("Hi!")}},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				// Zeros here would read as a call that really used no tokens.
				attrs := spanAttributeMapOf(span)
				if _, ok := attrs["gen_ai.usage.input_tokens"]; ok {
					t.Error("a generation with no usage carries gen_ai.usage.input_tokens")
				}
				if _, ok := attrs["gen_ai.usage.output_tokens"]; ok {
					t.Error("a generation with no usage carries gen_ai.usage.output_tokens")
				}
			},
		},
		{
			name: "a call error the mapper set as a field is not a span error",
			content: Generation{
				Output:    []Message{AssistantTextMessage("Hi!")},
				CallError: "provider returned 500",
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				// The other protocols report this generation as a success, and
				// an error status with no error.type would skew both.
				if got := span.Status().Code; got != codes.Unset {
					t.Errorf("status = %v, want unset", got)
				}
				if _, ok := spanAttributeMapOf(span)["error.type"]; ok {
					t.Error("span carries error.type without a failed call")
				}
			},
		},
		{
			name: "an inline data url is a blob when the mime type is undeclared",
			content: Generation{
				Input: []Message{{
					Role: RoleUser,
					Parts: []Part{{
						Kind:  PartKindMedia,
						Media: &Media{Kind: "image", URL: "data:image/png;base64,abc123"},
					}},
				}},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				got := spanAttributeMapOf(span)["gen_ai.input.messages"].AsString()
				if !strings.Contains(got, `"type":"blob"`) {
					t.Errorf("gen_ai.input.messages = %q, want an inline blob part", got)
				}
				if !strings.Contains(got, `"mime_type":"image/png"`) {
					t.Errorf("gen_ai.input.messages = %q, want the data url's mime type", got)
				}
			},
		},
		{
			name: "an inline data url is a blob when the declared mime type differs in case",
			content: Generation{
				Input: []Message{{
					Role: RoleUser,
					Parts: []Part{{
						Kind:  PartKindMedia,
						Media: &Media{Kind: "image", URL: "data:image/png;base64,abc123", MIMEType: "image/PNG"},
					}},
				}},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				got := spanAttributeMapOf(span)["gen_ai.input.messages"].AsString()
				if !strings.Contains(got, `"type":"blob"`) {
					t.Errorf("gen_ai.input.messages = %q, want an inline blob part", got)
				}
			},
		},
		{
			name: "a mislabeled data url rides as a uri",
			content: Generation{
				Input: []Message{{
					Role: RoleUser,
					Parts: []Part{{
						Kind:  PartKindMedia,
						Media: &Media{Kind: "image", URL: "data:image/png;base64,abc123", MIMEType: "image/jpeg"},
					}},
				}},
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				got := spanAttributeMapOf(span)["gen_ai.input.messages"].AsString()
				if !strings.Contains(got, `"type":"uri"`) {
					t.Errorf("gen_ai.input.messages = %q, want a uri part", got)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, recorder, _ := newOTelTestClient(t, tc.mutate)
			recordOTelGeneration(t, client, tc.content)
			tc.check(t, onlySpan(t, recorder))
		})
	}
}

func TestOTelProtocolCallErrorSetsStatus(t *testing.T) {
	client, recorder, _ := newOTelTestClient(t, nil)

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetCallError(errors.New("provider exploded"))
	rec.End()

	span := onlySpan(t, recorder)
	if got := span.Status().Description; got != "provider exploded" {
		t.Errorf("status description = %q, want the provider error", got)
	}
	if got := spanAttributeMapOf(span)["error.type"].AsString(); got == "" {
		t.Error("error span carries no error.type")
	}
}

func TestOTelProtocolBypassesGenerationExport(t *testing.T) {
	client, _, _ := newOTelTestClient(t, nil)
	recordOTelGeneration(t, client, Generation{Output: []Message{AssistantTextMessage("Hi!")}})

	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	exporter, ok := client.config.testGenerationExporter.(*capturingGenerationExporter)
	if !ok {
		t.Fatalf("test exporter = %T", client.config.testGenerationExporter)
	}
	if got := exporter.requestCount(); got != 0 {
		t.Errorf("exporter received %d requests, want 0", got)
	}
}

func TestOTelProtocolDropsWorkflowSteps(t *testing.T) {
	client, _, _ := newOTelTestClient(t, nil)

	err := client.EnqueueWorkflowStep(WorkflowStep{
		ID:             "step-1",
		ConversationID: "conv-1",
		StepName:       "step",
		Framework:      "langgraph",
	})
	// The client drops the step in otel mode, so the caller must not read the
	// result as "enqueued".
	if !errors.Is(err, ErrWorkflowStepEnqueueFailed) {
		t.Fatalf("EnqueueWorkflowStep err = %v, want ErrWorkflowStepEnqueueFailed", err)
	}
	if err := client.Flush(context.Background()); err != nil {
		t.Fatalf("flush: %v", err)
	}
	exporter, _ := client.config.testGenerationExporter.(*capturingGenerationExporter)
	if got := exporter.workflowStepRequestCount(); got != 0 {
		t.Errorf("exporter received %d workflow step requests, want 0", got)
	}
}

func TestOTelProtocolRequiresExperimentalGate(t *testing.T) {
	cases := []struct {
		name         string
		experimental string
		wantOTel     bool
	}{
		{name: "gate open", experimental: "true", wantOTel: true},
		{name: "gate unset", experimental: "", wantOTel: false},
		{name: "gate off", experimental: "false", wantOTel: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEnableExperimentalFeatures, tc.experimental)

			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() {
				_ = provider.Shutdown(context.Background())
			})

			cfg := DefaultConfig()
			cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
			cfg.TracerProvider = provider
			cfg.Now = time.Now
			client := NewClient(cfg)
			t.Cleanup(func() {
				_ = client.Shutdown(context.Background())
			})

			if got := client.otelExportEnabled(); got != tc.wantOTel {
				t.Fatalf("otelExportEnabled() = %v, want %v", got, tc.wantOTel)
			}
			if _, ok := client.exporter.(*noopGenerationExporter); !ok {
				t.Fatalf("exporter = %T, want *noopGenerationExporter", client.exporter)
			}

			_, rec := client.StartGeneration(context.Background(), GenerationStart{
				Model: ModelRef{Provider: "openai", Name: "gpt-5"},
			})
			rec.SetResult(Generation{Output: []Message{AssistantTextMessage("Hi!")}}, nil)
			rec.End()

			// The metadata span and the otel-mode span both carry the
			// generation id; the operation name is what separates them.
			span := onlySpan(t, recorder)
			operation := spanAttributeMapOf(span)["gen_ai.operation.name"].AsString()
			if tc.wantOTel && operation != "chat" {
				t.Errorf("gen_ai.operation.name = %q, want the spec operation", operation)
			}
			if !tc.wantOTel && operation != "generateText" {
				t.Errorf("gen_ai.operation.name = %q, want the unchanged default", operation)
			}
		})
	}
}

func TestOTelCaptureModeTranslation(t *testing.T) {
	cases := []struct {
		mode ContentCaptureMode
		want string
	}{
		{mode: ContentCaptureModeDefault, want: "SPAN_ONLY"},
		{mode: ContentCaptureModeFull, want: "SPAN_ONLY"},
		{mode: ContentCaptureModeNoToolContent, want: "SPAN_ONLY"},
		// The mode exists to keep content off the OTel destination, and in
		// otel mode that destination is the export.
		{mode: ContentCaptureModeFullWithMetadataSpans, want: "NO_CONTENT"},
		{mode: ContentCaptureModeMetadataOnly, want: "NO_CONTENT"},
	}
	for _, tc := range cases {
		t.Run(tc.mode.String(), func(t *testing.T) {
			if got := string(otelCaptureMode(tc.mode)); got != tc.want {
				t.Errorf("otelCaptureMode(%s) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}

func TestOTelCaptureModeIgnoresStandardEnv(t *testing.T) {
	// The branded mode decides content capture in otel mode. Letting the
	// conventions' variable decide would put a traces-side setting in charge of
	// the SDK's content policy, and flip it on for a client that never asked.
	t.Setenv("OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT", "NO_CONTENT")

	if got := string(otelCaptureMode(ContentCaptureModeDefault)); got != "SPAN_ONLY" {
		t.Fatalf("otelCaptureMode(default) = %q, want SPAN_ONLY", got)
	}
}

func TestOTelHandlerDisablesOperationDetailsEvents(t *testing.T) {
	t.Setenv(otelgenai.EnvEmitEvent, "true")

	tracerProvider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tracerProvider.Shutdown(context.Background()) })
	meterProvider := sdkmetric.NewMeterProvider()
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
	logs := logtest.NewRecorder()

	cfg := DefaultConfig()
	cfg.TracerProvider = tracerProvider
	cfg.MeterProvider = meterProvider
	options := append(otelHandlerOptions(cfg), otelgenai.WithLoggerProvider(logs))
	handler := otelgenai.NewHandler(options...)
	start := time.Now().Add(-time.Second)
	inv := &otelgenai.Invocation{
		Provider:     "openai",
		RequestModel: "gpt-5",
		StartedAt:    start,
		CompletedAt:  start.Add(time.Second),
	}
	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	for scope, records := range logs.Result() {
		if scope.Name == otelgenai.ScopeName && len(records) != 0 {
			t.Errorf("recorded %d operation-details events, want 0", len(records))
		}
	}
}

func TestOTelPerCallCaptureModeReachesTheSpan(t *testing.T) {
	cases := []struct {
		name        string
		clientMode  ContentCaptureMode
		callMode    ContentCaptureMode
		wantContent bool
	}{
		{name: "call downgrades to metadata only", clientMode: ContentCaptureModeFull, callMode: ContentCaptureModeMetadataOnly},
		{name: "call upgrades to full", clientMode: ContentCaptureModeMetadataOnly, callMode: ContentCaptureModeFull, wantContent: true},
		{name: "call inherits the client mode", clientMode: ContentCaptureModeFull, wantContent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
				cfg.ContentCapture = tc.clientMode
			})

			_, rec := client.StartGeneration(context.Background(), GenerationStart{
				Model:          ModelRef{Provider: "openai", Name: "gpt-5"},
				ContentCapture: tc.callMode,
			})
			rec.SetResult(Generation{Input: []Message{UserTextMessage("CALL-CONTENT")}}, nil)
			rec.End()

			got := spanAttributeMapOf(onlySpan(t, recorder))["gen_ai.input.messages"].AsString()
			if strings.Contains(got, "CALL-CONTENT") != tc.wantContent {
				t.Errorf("gen_ai.input.messages = %q, want content present = %v", got, tc.wantContent)
			}
		})
	}
}

func TestOTelExportGenerationIsRejected(t *testing.T) {
	client, recorder, _ := newOTelTestClient(t, nil)

	err := client.ExportGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	}, Generation{Output: []Message{AssistantTextMessage("Hi!")}})
	if !errors.Is(err, ErrSynchronousExportUnsupported) {
		t.Fatalf("ExportGeneration err = %v, want ErrSynchronousExportUnsupported", err)
	}
	if got := len(recorder.Ended()); got != 0 {
		t.Errorf("recorded %d spans, want 0", got)
	}
}

func TestOTelFlushNeedsATracerProvider(t *testing.T) {
	// With only a tracer the client cannot force-flush anything, and a nil
	// return would tell an adapter its batch is delivered.
	t.Setenv(EnvEnableExperimentalFeatures, "true")

	provider := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	cfg := DefaultConfig()
	cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
	cfg.Tracer = provider.Tracer("agento11y-test")
	cfg.Now = time.Now
	client := NewClient(cfg)
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	if err := client.Flush(context.Background()); !errors.Is(err, ErrFlushNotVerifiable) {
		t.Fatalf("Flush err = %v, want ErrFlushNotVerifiable", err)
	}
}

// failingSpanProcessor stands in for a span exporter that cannot deliver.
type failingSpanProcessor struct {
	sdktrace.SpanProcessor
	err error
}

func (p failingSpanProcessor) ForceFlush(context.Context) error { return p.err }

func TestOTelFlushReportsTracerProviderFailure(t *testing.T) {
	exportErr := errors.New("otlp collector unreachable")
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(failingSpanProcessor{
			SpanProcessor: sdktrace.NewSimpleSpanProcessor(tracetest.NewNoopExporter()),
			err:           exportErr,
		}))
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
		cfg.TracerProvider = provider
	})

	recordOTelGeneration(t, client, Generation{Output: []Message{AssistantTextMessage("Hi!")}})

	// Adapters treat a nil Flush as proof the batch is delivered and then
	// delete their own copy of it, so a failing exporter has to reach them.
	if err := client.Flush(context.Background()); !errors.Is(err, exportErr) {
		t.Fatalf("Flush err = %v, want the exporter failure", err)
	}
	// Shutdown flushes what the provider still holds, under the same rule.
	if err := client.Shutdown(context.Background()); !errors.Is(err, exportErr) {
		t.Fatalf("Shutdown err = %v, want the exporter failure", err)
	}
}

func TestOTelProtocolDropsValidatorRejectedRecords(t *testing.T) {
	// A record the SDK's validator refuses is never enqueued on the other
	// protocols. In otel mode the span is the export, so it has to close
	// without the id and the content that would make it a generation.
	client, recorder, _ := newOTelTestClient(t, nil)

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{
		Input: []Message{{
			Role: RoleUser,
			Parts: []Part{
				TextPart("PROMPT TEXT"),
				{Kind: PartKindMedia, Media: &Media{Kind: "image"}},
			},
		}},
	}, nil)
	rec.End()

	if rec.Err() == nil {
		t.Fatal("recorder reported no error for a generation the validator refuses")
	}

	span := onlySpan(t, recorder)
	attrs := spanAttributeMapOf(span)
	if _, ok := attrs["agento11y.generation.id"]; ok {
		t.Error("a rejected record carries agento11y.generation.id, so the backend would store it")
	}
	if got, ok := attrs["gen_ai.input.messages"]; ok {
		t.Errorf("a rejected record carries content: %s", got.AsString())
	}
	if got := attrs["error.type"].AsString(); got == "" {
		t.Error("a rejected record carries no error.type")
	}
	if got := span.Status().Description; got == "" {
		t.Error("a rejected record closes with an empty status description")
	}
}

func TestOTelSpecMetricDimensions(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
		cfg.MeterProvider = meterProvider
		cfg.Tags = map[string]string{"repo": "agento11y"}
	})

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetCallError(errors.New("429 too many requests"))
	rec.SetResult(Generation{
		AgentName:    "assistant",
		AgentVersion: "1.0.0",
		Usage: TokenUsage{
			InputTokens:          10,
			OutputTokens:         2,
			CacheReadInputTokens: 3,
			InputSemantics:       TokenInputSemanticsInclusive,
		},
	}, nil)
	rec.End()

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}

	scopes := map[string][]string{}
	var duration, usage metricdata.Metrics
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			scopes[m.Name] = append(scopes[m.Name], scope.Scope.Name)
			if scope.Scope.Name != otelgenai.ScopeName {
				continue
			}
			switch m.Name {
			case "gen_ai.client.operation.duration":
				duration = m
			case "gen_ai.client.token.usage":
				usage = m
			}
		}
	}

	// The SDK keeps its own instruments for tool and embedding metrics. They
	// carry the same names with a different label schema, so they have to sit
	// in their own scope rather than conflict inside one.
	for name, owners := range scopes {
		if len(owners) != len(slices.Compact(slices.Sorted(slices.Values(owners)))) {
			t.Errorf("%s is registered twice in one scope: %v", name, owners)
		}
	}

	if duration.Name == "" {
		t.Fatalf("gen_ai.client.operation.duration not recorded under %s", otelgenai.ScopeName)
	}
	durationAttrs := histogramAttributes(t, duration)
	if got := durationAttrs["agento11y.tag.repo"]; got != "agento11y" {
		t.Errorf("duration agento11y.tag.repo = %q, want agento11y", got)
	}
	if got := durationAttrs["error.category"]; got != "rate_limit" {
		t.Errorf("duration error.category = %q, want rate_limit", got)
	}
	if got := durationAttrs["error.type"]; got == "" {
		t.Error("duration carries no error.type")
	}
	if got := durationAttrs[spanAttrAgentName]; got != "assistant" {
		t.Errorf("duration %s = %q, want assistant", spanAttrAgentName, got)
	}
	if got := durationAttrs[spanAttrAgentVersion]; got != "1.0.0" {
		t.Errorf("duration %s = %q, want 1.0.0", spanAttrAgentVersion, got)
	}
	usageAttrs := histogramAttributes(t, usage)
	if got := usageAttrs["agento11y.tag.repo"]; got != "agento11y" {
		t.Errorf("token usage agento11y.tag.repo = %q, want agento11y", got)
	}
	if got := usageAttrs["gen_ai.token.semantics"]; got != "inclusive" {
		t.Errorf("token usage gen_ai.token.semantics = %q, want inclusive", got)
	}
	if got := usageAttrs[spanAttrAgentName]; got != "assistant" {
		t.Errorf("token usage %s = %q, want assistant", spanAttrAgentName, got)
	}
	if got := usageAttrs[spanAttrAgentVersion]; got != "1.0.0" {
		t.Errorf("token usage %s = %q, want 1.0.0", spanAttrAgentVersion, got)
	}
	usageHistogram, ok := usage.Data.(metricdata.Histogram[int64])
	if !ok {
		t.Fatalf("token usage data = %T, want an int64 histogram", usage.Data)
	}
	cacheRead := int64(0)
	for _, point := range usageHistogram.DataPoints {
		tokenType, _ := point.Attributes.Value("gen_ai.token.type")
		if tokenType.AsString() == "cache_read" {
			cacheRead = point.Sum
		}
	}
	if cacheRead != 3 {
		t.Errorf("cache_read token usage = %d, want 3", cacheRead)
	}
}

// histogramAttributes returns the attributes of a histogram's first data
// point, whichever numeric type it carries.
func histogramAttributes(t *testing.T, m metricdata.Metrics) map[string]string {
	t.Helper()

	var set attribute.Set
	switch data := m.Data.(type) {
	case metricdata.Histogram[float64]:
		set = data.DataPoints[0].Attributes
	case metricdata.Histogram[int64]:
		set = data.DataPoints[0].Attributes
	default:
		t.Fatalf("%s data = %T, want a histogram", m.Name, m.Data)
	}
	out := make(map[string]string, set.Len())
	for _, kv := range set.ToSlice() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func TestOTelMediaPartModality(t *testing.T) {
	cases := []struct {
		name    string
		kind    string
		want    string
		wantSet bool
	}{
		{name: "empty kind"},
		{name: "image", kind: "image", want: "image", wantSet: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var out otelgenai.Part
			otelMediaPart(&out, &Media{Kind: tc.kind, URL: "https://example.test/media"})
			if !tc.wantSet {
				if out.Modality != nil {
					t.Errorf("modality = %q, want nil", *out.Modality)
				}
				return
			}
			if out.Modality == nil {
				t.Fatalf("modality = nil, want %q", tc.want)
			}
			if *out.Modality != tc.want {
				t.Errorf("modality = %q, want %q", *out.Modality, tc.want)
			}
		})
	}
}

func TestOTelProviderRegistrySpellings(t *testing.T) {
	cases := []struct{ stored, wire string }{
		{stored: "gemini", wire: "gcp.gemini"},
		{stored: "mistral", wire: "mistral_ai"},
		{stored: "openai", wire: "openai"},
	}
	for _, tc := range cases {
		t.Run(tc.stored, func(t *testing.T) {
			if got := otelProviderName(tc.stored); got != tc.wire {
				t.Errorf("otelProviderName(%q) = %q, want %q", tc.stored, got, tc.wire)
			}
		})
	}
}
