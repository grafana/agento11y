package agento11y

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
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
	"go.opentelemetry.io/otel/semconv/v1.41.0/genaiconv"

	"github.com/grafana/agento11y/go/otelgenai"
)

// newOTelTestClient builds a client in otel export mode with the experimental
// gate open, recording spans into the returned recorder.
func newOTelTestClient(t *testing.T, mutate func(*Config), traceOptions ...sdktrace.TracerProviderOption) (*Client, *tracetest.SpanRecorder, *sdktrace.TracerProvider) {
	t.Helper()

	t.Setenv(EnvEnableExperimentalFeatures, "true")

	recorder := tracetest.NewSpanRecorder()
	options := append([]sdktrace.TracerProviderOption{sdktrace.WithSpanProcessor(recorder)}, traceOptions...)
	provider := sdktrace.NewTracerProvider(options...)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
	})

	cfg := DefaultConfig()
	cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
	cfg.ContentCapture = ContentCaptureModeFull
	cfg.TracerProvider = provider
	cfg.Flusher = provider
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

// exceptionEventCount counts the span's recorded exceptions. A trace backend
// reads those events as operation errors, so an export failure must not add one.
func exceptionEventCount(span sdktrace.ReadOnlySpan) int {
	count := 0
	for _, event := range span.Events() {
		if event.Name == "exception" {
			count++
		}
	}
	return count
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
			name: "semconv span carries SDK attributes",
			content: Generation{
				Input:  []Message{UserTextMessage("Hello")},
				Output: []Message{AssistantTextMessage("Hi!")},
				Usage:  TokenUsage{InputTokens: 120, OutputTokens: 42, TotalTokens: 162},
				Metadata: map[string]any{
					spanAttrRequestThinkingBudget: int64(2048),
				},
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
				if got := attrs[sdkMetadataKeyName].AsString(); got != sdkName {
					t.Errorf("%s = %q, want %q", sdkMetadataKeyName, got, sdkName)
				}
				if got := attrs[spanAttrRequestThinkingBudget].AsInt64(); got != 2048 {
					t.Errorf("%s = %d, want 2048", spanAttrRequestThinkingBudget, got)
				}
				metadata := map[string]any{}
				if err := json.Unmarshal([]byte(attrs["agento11y.generation.metadata"].AsString()), &metadata); err != nil {
					t.Fatalf("unmarshal generation metadata: %v", err)
				}
				if got := metadata[sdkMetadataKeyName]; got != sdkName {
					t.Errorf("metadata[%q] = %#v, want %q", sdkMetadataKeyName, got, sdkName)
				}
				if got := metadata[spanAttrRequestThinkingBudget]; got != float64(2048) {
					t.Errorf("metadata[%q] = %#v, want 2048", spanAttrRequestThinkingBudget, got)
				}
				if attrs["agento11y.generation.id"].AsString() == "" {
					t.Error("otel-mode span carries no agento11y.generation.id")
				}
			},
		},
		{
			name: "full with metadata spans carries content on the only export",
			mutate: func(cfg *Config) {
				cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
			},
			content: Generation{
				SystemPrompt:      "SYSTEM-CONTENT",
				Input:             []Message{UserTextMessage("INPUT-CONTENT")},
				Output:            []Message{AssistantTextMessage("OUTPUT-CONTENT")},
				ConversationTitle: "TITLE-CONTENT",
			},
			check: func(t *testing.T, span sdktrace.ReadOnlySpan) {
				attrs := spanAttributeMapOf(span)
				for key, want := range map[string]string{
					"gen_ai.system_instructions":    "SYSTEM-CONTENT",
					"gen_ai.input.messages":         "INPUT-CONTENT",
					"gen_ai.output.messages":        "OUTPUT-CONTENT",
					"agento11y.conversation.title":  "TITLE-CONTENT",
					"agento11y.generation.metadata": `"agento11y.sdk.content_capture_mode":"full"`,
				} {
					if got := attrs[key].AsString(); !strings.Contains(got, want) {
						t.Errorf("%s = %q, want it to contain %q", key, got, want)
					}
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
					RedactInputMessages: new(true),
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

func TestOTelProtocolTagDestinations(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
		meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
		cfg.MeterProvider = meterProvider
		cfg.Tags = map[string]string{
			"shared":      "client",
			"client_only": "client",
		}
	})

	ctx := WithTags(context.Background(), map[string]string{
		"shared":       "context",
		"context_only": "context",
	})
	_, rec := client.StartGeneration(ctx, GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
		Tags: map[string]string{
			"shared":     "start",
			"start_only": "start",
		},
	})
	rec.SetResult(Generation{
		Tags: map[string]string{
			"shared":      "result",
			"result_only": "result",
		},
		Usage: TokenUsage{InputTokens: 10, OutputTokens: 2},
	}, nil)
	rec.End()

	attrs := spanAttributeMapOf(onlySpan(t, recorder))
	for key, want := range map[string]string{
		"shared":       "context",
		"client_only":  "client",
		"context_only": "context",
	} {
		if got := attrs[spanAttrTagPrefix+key].AsString(); got != want {
			t.Errorf("%s%s = %q, want %q", spanAttrTagPrefix, key, got, want)
		}
	}
	for _, key := range []string{"start_only", "result_only"} {
		if _, ok := attrs[spanAttrTagPrefix+key]; ok {
			t.Errorf("span carries dimension %s%s for an export-only tag", spanAttrTagPrefix, key)
		}
	}

	var tags map[string]string
	if err := json.Unmarshal([]byte(attrs["agento11y.generation.tags"].AsString()), &tags); err != nil {
		t.Fatalf("unmarshal generation tags: %v", err)
	}
	for key, want := range map[string]string{
		"shared":       "result",
		"client_only":  "client",
		"context_only": "context",
		"start_only":   "start",
		"result_only":  "result",
	} {
		if got := tags[key]; got != want {
			t.Errorf("generation tags[%q] = %q, want %q", key, got, want)
		}
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	metricCount := 0
	for _, scope := range collected.ScopeMetrics {
		if scope.Scope.Name != otelgenai.ScopeName {
			continue
		}
		for _, metric := range scope.Metrics {
			if metric.Name != "gen_ai.client.operation.duration" && metric.Name != "gen_ai.client.token.usage" {
				continue
			}
			metricCount++
			metricAttrs := histogramAttributes(t, metric)
			for key, want := range map[string]string{
				"shared":       "context",
				"client_only":  "client",
				"context_only": "context",
			} {
				if got := metricAttrs[spanAttrTagPrefix+key]; got != want {
					t.Errorf("%s %s%s = %q, want %q", metric.Name, spanAttrTagPrefix, key, got, want)
				}
			}
			for _, key := range []string{"start_only", "result_only"} {
				if _, ok := metricAttrs[spanAttrTagPrefix+key]; ok {
					t.Errorf("%s carries dimension %s%s for an export-only tag", metric.Name, spanAttrTagPrefix, key)
				}
			}
		}
	}
	if metricCount != 2 {
		t.Errorf("checked %d OTel generation metrics, want 2", metricCount)
	}
}

func TestOTelProtocolPreservesExportContractAtSpanLimit(t *testing.T) {
	limits := sdktrace.NewSpanLimits()
	limits.AttributeCountLimit = 16
	tags := map[string]string{
		"tag_01": "value", "tag_02": "value", "tag_03": "value",
		"tag_04": "value", "tag_05": "value", "tag_06": "value",
		"tag_07": "value", "tag_08": "value", "tag_09": "value",
		"tag_10": "value", "tag_11": "value", "tag_12": "value",
	}
	client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.Tags = tags
	}, sdktrace.WithRawSpanLimits(limits))

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		ID:    "generation-at-span-limit",
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("recorder: %v", err)
	}

	span := onlySpan(t, recorder)
	attrs := spanAttributeMapOf(span)
	for key, want := range map[string]string{
		"agento11y.record":   "true",
		spanAttrGenerationID: "generation-at-span-limit",
		sdkMetadataKeyName:   sdkName,
	} {
		got, ok := attrs[key]
		if !ok {
			t.Errorf("%s is absent, want %q", key, want)
			continue
		}
		if got.AsString() != want {
			t.Errorf("%s = %q, want %q", key, got.AsString(), want)
		}
	}
	var exportedTags map[string]string
	if err := json.Unmarshal([]byte(attrs["agento11y.generation.tags"].AsString()), &exportedTags); err != nil {
		t.Fatalf("unmarshal generation tags: %v", err)
	}
	for key, want := range tags {
		if got := exportedTags[key]; got != want {
			t.Errorf("generation tags[%q] = %q, want %q", key, got, want)
		}
	}
	if got := span.DroppedAttributes(); got == 0 {
		t.Fatal("span dropped no attributes; test did not reach the configured limit")
	}
	droppedDimension := false
	for key := range tags {
		if _, ok := attrs[spanAttrTagPrefix+key]; !ok {
			droppedDimension = true
			break
		}
	}
	if !droppedDimension {
		t.Error("all optional tag dimensions survived the span attribute limit")
	}
}

func TestOTelProtocolCallErrorSetsStatus(t *testing.T) {
	for _, mode := range []ContentCaptureMode{
		ContentCaptureModeFull,
		ContentCaptureModeFullWithMetadataSpans,
	} {
		t.Run(mode.String(), func(t *testing.T) {
			client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
				cfg.ContentCapture = mode
			})

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
		})
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
		override     *bool
		experimental string
		wantOTel     bool
	}{
		{name: "explicit true with gate unset", override: new(true), wantOTel: true},
		{name: "explicit false with gate open", override: new(false), experimental: "true", wantOTel: false},
		{name: "nil with gate open", experimental: "true", wantOTel: true},
		{name: "nil with gate unset", wantOTel: false},
		{name: "nil with gate off", experimental: "false", wantOTel: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearExperimentalGate(t)
			if tc.experimental != "" {
				t.Setenv(EnvEnableExperimentalFeatures, tc.experimental)
			}

			recorder := tracetest.NewSpanRecorder()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
			t.Cleanup(func() {
				_ = provider.Shutdown(context.Background())
			})

			cfg := DefaultConfig()
			cfg.EnableExperimentalFeatures = tc.override
			cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
			cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
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
				Model:             ModelRef{Provider: "openai", Name: "gpt-5"},
				ConversationTitle: "GATE-TITLE",
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
			_, hasTitle := spanAttributeMapOf(span)["agento11y.conversation.title"]
			if hasTitle != tc.wantOTel {
				t.Errorf("conversation title present = %v, want %v", hasTitle, tc.wantOTel)
			}
		})
	}
}

func TestOTelProtocolAdmissionIsFixedAfterNewClient(t *testing.T) {
	cases := []struct {
		name       string
		initialEnv string
		changedEnv string
		wantOTel   bool
	}{
		{name: "open stays open", initialEnv: "true", changedEnv: "false", wantOTel: true},
		{name: "closed stays closed", initialEnv: "false", changedEnv: "true", wantOTel: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEnableExperimentalFeatures, tc.initialEnv)
			cfg := DefaultConfig()
			cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
			client := NewClient(cfg)
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			t.Setenv(EnvEnableExperimentalFeatures, tc.changedEnv)
			if got := client.otelExportEnabled(); got != tc.wantOTel {
				t.Fatalf("otelExportEnabled() after environment change = %v, want %v", got, tc.wantOTel)
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
		// The span is the only generation export in otel mode. Without span
		// content, this mode would discard generation content.
		{mode: ContentCaptureModeFullWithMetadataSpans, want: "SPAN_ONLY"},
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
		name         string
		clientMode   ContentCaptureMode
		resolverMode ContentCaptureMode
		callMode     ContentCaptureMode
		wantContent  bool
	}{
		{name: "call downgrades to metadata only", clientMode: ContentCaptureModeFull, callMode: ContentCaptureModeMetadataOnly},
		{name: "call upgrades to full", clientMode: ContentCaptureModeMetadataOnly, callMode: ContentCaptureModeFull, wantContent: true},
		{name: "call upgrades full with metadata spans", clientMode: ContentCaptureModeMetadataOnly, callMode: ContentCaptureModeFullWithMetadataSpans, wantContent: true},
		{name: "resolver upgrades full with metadata spans", clientMode: ContentCaptureModeMetadataOnly, resolverMode: ContentCaptureModeFullWithMetadataSpans, wantContent: true},
		{name: "call inherits the client mode", clientMode: ContentCaptureModeFull, wantContent: true},
		{name: "call inherits full with metadata spans", clientMode: ContentCaptureModeFullWithMetadataSpans, wantContent: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
				cfg.ContentCapture = tc.clientMode
				if tc.resolverMode != ContentCaptureModeDefault {
					cfg.ContentCaptureResolver = func(context.Context, map[string]any) ContentCaptureMode {
						return tc.resolverMode
					}
				}
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

func TestOTelCaptureModeContextResolvesForEachClient(t *testing.T) {
	otelClient, otelRecorder, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
	})
	ctx, generationRecorder := otelClient.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	generationRecorder.SetResult(Generation{Output: []Message{AssistantTextMessage("Hi!")}}, nil)

	recordTool := func(client *Client) {
		_, toolRecorder := client.StartToolExecution(ctx, ToolExecutionStart{ToolName: "weather"})
		toolRecorder.SetResult(ToolExecutionEnd{
			Arguments: map[string]any{"city": "Paris"},
			Result:    map[string]any{"temperature": 18},
		})
		toolRecorder.End()
	}

	recordTool(otelClient)
	otelAttrs := spanAttributeMapOf(onlySpan(t, otelRecorder))
	if _, ok := otelAttrs[spanAttrToolCallArguments]; !ok {
		t.Error("tool span does not carry arguments resolved for the OTel client")
	}
	if _, ok := otelAttrs[spanAttrToolCallResult]; !ok {
		t.Error("tool span does not carry a result resolved for the OTel client")
	}

	for _, protocol := range []GenerationExportProtocol{
		GenerationExportProtocolGRPC,
		GenerationExportProtocolHTTP,
	} {
		t.Run(string(protocol), func(t *testing.T) {
			client, recorder, _ := newTestClient(t, Config{
				ContentCapture: ContentCaptureModeFullWithMetadataSpans,
				GenerationExport: GenerationExportConfig{
					Protocol: protocol,
				},
			})

			recordTool(client)
			attrs := spanAttributeMapOf(onlySpan(t, recorder))
			if _, ok := attrs[spanAttrToolCallArguments]; ok {
				t.Error("tool span carries arguments resolved for the OTel client")
			}
			if _, ok := attrs[spanAttrToolCallResult]; ok {
				t.Error("tool span carries a result resolved for the OTel client")
			}
		})
	}

	generationRecorder.End()
}

func TestOTelFullWithMetadataSpansCoversToolContent(t *testing.T) {
	client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
	})

	_, rec := client.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "weather"})
	rec.SetResult(ToolExecutionEnd{
		Arguments: map[string]any{"city": "Paris"},
		Result:    map[string]any{"temperature": 18},
	})
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("tool execution: %v", err)
	}

	attrs := spanAttributeMapOf(onlySpan(t, recorder))
	if got := attrs[spanAttrToolCallArguments].AsString(); !strings.Contains(got, "Paris") {
		t.Errorf("%s = %q, want the tool arguments", spanAttrToolCallArguments, got)
	}
	if got := attrs[spanAttrToolCallResult].AsString(); !strings.Contains(got, "18") {
		t.Errorf("%s = %q, want the tool result", spanAttrToolCallResult, got)
	}
}

func TestOTelFullWithMetadataSpansEmbeddingCaptureInput(t *testing.T) {
	for _, tc := range []struct {
		name         string
		captureInput bool
	}{
		{name: "enabled", captureInput: true},
		{name: "disabled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
				cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
				cfg.EmbeddingCapture.CaptureInput = tc.captureInput
			})

			_, rec := client.StartEmbedding(context.Background(), EmbeddingStart{
				Model: ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
			})
			rec.SetResult(EmbeddingResult{InputCount: 1, InputTexts: []string{"EMBEDDING-CONTENT"}})
			rec.End()
			if err := rec.Err(); err != nil {
				t.Fatalf("embedding: %v", err)
			}

			got, present := spanAttributeMapOf(onlySpan(t, recorder))[spanAttrEmbeddingInputTexts]
			if present != tc.captureInput {
				t.Fatalf("%s present = %v, want %v", spanAttrEmbeddingInputTexts, present, tc.captureInput)
			}
			if tc.captureInput && !slices.Contains(got.AsStringSlice(), "EMBEDDING-CONTENT") {
				t.Errorf("%s = %v, want EMBEDDING-CONTENT", spanAttrEmbeddingInputTexts, got.AsStringSlice())
			}
		})
	}
}

func TestOTelFullWithMetadataSpansConstructionDoesNotLogCaptureMode(t *testing.T) {
	var logs bytes.Buffer
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.ContentCapture = ContentCaptureModeFullWithMetadataSpans
		cfg.Logger = log.New(&logs, "", 0)
	})
	if !client.otelExportEnabled() {
		t.Fatal("otel export is not enabled")
	}
	if got := logs.String(); strings.Contains(got, "content capture mode") {
		t.Errorf("construction log contains a content-capture message: %q", got)
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

func TestOTelFlushNeedsAFlusher(t *testing.T) {
	// The provider's lifecycle belongs to the application, so the SDK flushes
	// only what the application named. Until it does, a nil return would tell an
	// adapter its batch is delivered.
	cases := []struct {
		name   string
		config func(*Config, *sdktrace.TracerProvider)
	}{
		{
			name: "only a tracer, nothing to flush",
			config: func(cfg *Config, provider *sdktrace.TracerProvider) {
				cfg.Tracer = provider.Tracer("agento11y-test")
			},
		},
		{
			// The SDK will not reach ForceFlush by asserting the provider to it.
			name: "a flushable provider the application did not offer for flushing",
			config: func(cfg *Config, provider *sdktrace.TracerProvider) {
				cfg.TracerProvider = provider
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvEnableExperimentalFeatures, "true")

			provider := sdktrace.NewTracerProvider()
			t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

			cfg := DefaultConfig()
			cfg.GenerationExport.Protocol = GenerationExportProtocolOTel
			cfg.Now = time.Now
			tc.config(&cfg, provider)
			client := NewClient(cfg)
			t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

			if err := client.Flush(context.Background()); !errors.Is(err, ErrFlushNotVerifiable) {
				t.Fatalf("Flush err = %v, want ErrFlushNotVerifiable", err)
			}
		})
	}
}

// failingSpanProcessor stands in for a span exporter that cannot deliver.
type failingSpanProcessor struct {
	sdktrace.SpanProcessor
	err error
}

func (p failingSpanProcessor) ForceFlush(context.Context) error { return p.err }

type observingSpanProcessor struct {
	sdktrace.SpanProcessor
	onEnd func()
}

func (p observingSpanProcessor) OnEnd(span sdktrace.ReadOnlySpan) {
	p.onEnd()
	p.SpanProcessor.OnEnd(span)
}

func TestOTelFlushReportsTracerProviderFailure(t *testing.T) {
	exportErr := errors.New("otlp collector unreachable")
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(failingSpanProcessor{
			SpanProcessor: sdktrace.NewSimpleSpanProcessor(tracetest.NewNoopExporter()),
			err:           exportErr,
		}))
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
		cfg.TracerProvider = provider
		cfg.Flusher = provider
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

func TestOTelRecordsGenerationAfterSpanHandoff(t *testing.T) {
	const generationID = "generation-order"

	var client *Client
	handoffObserved := false
	client, _, _ = newOTelTestClient(t, func(cfg *Config) {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(observingSpanProcessor{
			SpanProcessor: tracetest.NewSpanRecorder(),
			onEnd: func() {
				handoffObserved = true
				if client.hasRecordedGenerationID(generationID) {
					t.Error("generation was recorded before its span reached the processor")
				}
			},
		}))
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
		cfg.TracerProvider = provider
		cfg.Flusher = provider
	})

	recordOTelGeneration(t, client, Generation{
		ID:     generationID,
		Output: []Message{AssistantTextMessage("Hi!")},
	})

	if !handoffObserved {
		t.Fatal("span processor did not observe the generation")
	}
	if !client.hasRecordedGenerationID(generationID) {
		t.Error("generation was not recorded after its span reached the processor")
	}
}

// recordingSampler captures the attributes a head sampler is given at Start.
type recordingSampler struct {
	attributes []attribute.KeyValue
}

func (s *recordingSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	s.attributes = append(s.attributes, p.Attributes...)
	return sdktrace.SamplingResult{Decision: sdktrace.RecordAndSample}
}

func (s *recordingSampler) Description() string { return "recordingSampler" }

func TestOTelStartCarriesRequestAttributes(t *testing.T) {
	// A head sampler decides at Start, so the request fields the caller already
	// declared have to be on the span from that instant. Writing them at End is
	// too late for the decision.
	sampler := &recordingSampler{}
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		provider := sdktrace.NewTracerProvider(sdktrace.WithSampler(sampler))
		t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
		cfg.TracerProvider = provider
	})

	maxTokens := int64(512)
	temperature := 0.25
	topP := 0.75
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model:       ModelRef{Provider: "openai", Name: "gpt-5"},
		MaxTokens:   &maxTokens,
		Temperature: &temperature,
		TopP:        &topP,
	})
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("Hi!")}}, nil)
	rec.End()

	got := make(map[string]attribute.Value, len(sampler.attributes))
	for _, kv := range sampler.attributes {
		got[string(kv.Key)] = kv.Value
	}
	want := map[string]attribute.Value{
		"gen_ai.request.max_tokens":  attribute.IntValue(512),
		"gen_ai.request.temperature": attribute.Float64Value(0.25),
		"gen_ai.request.top_p":       attribute.Float64Value(0.75),
	}
	for key, wantValue := range want {
		if got[key] != wantValue {
			t.Errorf("sampler saw %s = %v, want %v", key, got[key], wantValue)
		}
	}
}

func TestOTelRecordAfterShutdownReportsClientShutdown(t *testing.T) {
	// Shutdown force-flushed the provider already, so a record that arrives
	// after it missed that flush. The queue path reports the same case as
	// ErrClientShutdown, and a nil here would tell the caller the record is
	// handled.
	reader := sdkmetric.NewManualReader()
	client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
		meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
		t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })
		cfg.MeterProvider = meterProvider
	})
	if err := client.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("Hi!")}}, nil)
	rec.End()

	if err := rec.Err(); !errors.Is(err, ErrClientShutdown) {
		t.Fatalf("recorder Err = %v, want ErrClientShutdown", err)
	}

	// The span keeps the generation: the caller owns the provider and may still
	// flush it, so dropping the payload here would lose a record that can still
	// arrive.
	span := onlySpan(t, recorder)
	attrs := spanAttributeMapOf(span)
	if _, ok := attrs["agento11y.generation.id"]; !ok {
		t.Error("a post-shutdown record carries no agento11y.generation.id")
	}
	// The model call succeeded, so the conventions' failure fields stay clear
	// and the drop travels on the SDK's own attribute.
	if got, ok := attrs["error.type"]; ok {
		t.Errorf("a successful call carries error.type = %v after a failed export", got.AsString())
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("status = %v, want unset", got)
	}
	if got := attrs["agento11y.export.error"].AsString(); !strings.Contains(got, "shutting down") {
		t.Errorf("agento11y.export.error = %q, want the shutdown reason", got)
	}
	if got := exceptionEventCount(span); got != 0 {
		t.Errorf("span carries %d exception events after a failed export, want 0", got)
	}

	// The conventions say a successful operation carries no error.type, so
	// filtering the duration histogram on it has to stay a filter for failed
	// model calls rather than for our own export trouble.
	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}
	for _, scope := range collected.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name != "gen_ai.client.operation.duration" {
				continue
			}
			if got := histogramAttributes(t, m)["error.type"]; got != "" {
				t.Errorf("%s carries error.type = %q after a failed export", m.Name, got)
			}
		}
	}
}

func TestOTelProtocolDropsValidatorRejectedRecords(t *testing.T) {
	// A record the SDK's validator refuses is never enqueued on the other
	// protocols. In otel mode the span is the export, so it has to close
	// without the id and the content that would make it a generation.
	client, recorder, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.Tags = map[string]string{"team": "sigil"}
	})

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
	if got := attrs[sdkMetadataKeyName].AsString(); got != sdkName {
		t.Errorf("%s = %q, want %q", sdkMetadataKeyName, got, sdkName)
	}
	if got := attrs[spanAttrTagPrefix+"team"].AsString(); got != "sigil" {
		t.Errorf("%steam = %q, want sigil", spanAttrTagPrefix, got)
	}
	if _, ok := attrs["agento11y.generation.id"]; ok {
		t.Error("a rejected record carries agento11y.generation.id, so the backend would store it")
	}
	if _, ok := attrs["agento11y.record"]; ok {
		t.Error("a rejected record carries agento11y.record")
	}
	if got, ok := attrs["gen_ai.input.messages"]; ok {
		t.Errorf("a rejected record carries content: %s", got.AsString())
	}
	// The validator refused our own payload; the model call still succeeded, so
	// the drop reads as an export failure rather than a failed generation.
	if got, ok := attrs["error.type"]; ok {
		t.Errorf("a rejected record carries error.type = %v", got.AsString())
	}
	if got := span.Status().Code; got != codes.Unset {
		t.Errorf("status = %v, want unset", got)
	}
	if got := attrs["agento11y.export.error"].AsString(); got == "" {
		t.Error("a rejected record carries no agento11y.export.error, so the drop is invisible")
	}
	if got := exceptionEventCount(span); got != 0 {
		t.Errorf("a rejected record carries %d exception events, want 0", got)
	}
}

func TestOTelSharedMetricMetadata(t *testing.T) {
	durationConvention := genaiconv.ClientOperationDuration{}
	usageConvention := genaiconv.ClientTokenUsage{}
	if got := durationConvention.Name(); metricOperationDuration != got {
		t.Fatalf("metricOperationDuration = %q, want %q", metricOperationDuration, got)
	}
	if got := usageConvention.Name(); metricTokenUsage != got {
		t.Fatalf("metricTokenUsage = %q, want %q", metricTokenUsage, got)
	}

	reader := sdkmetric.NewManualReader()
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = meterProvider.Shutdown(context.Background()) })

	const sdkScope = "agento11y-test"
	client, _, _ := newOTelTestClient(t, func(cfg *Config) {
		cfg.Meter = meterProvider.Meter(sdkScope)
		cfg.MeterProvider = meterProvider
	})

	_, generation := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	generation.SetResult(Generation{
		Usage: TokenUsage{InputTokens: 10, OutputTokens: 2},
	}, nil)
	generation.End()
	if err := generation.Err(); err != nil {
		t.Fatalf("generation: %v", err)
	}

	_, tool := client.StartToolExecution(context.Background(), ToolExecutionStart{ToolName: "weather"})
	tool.SetResult(ToolExecutionEnd{Result: map[string]any{"temperature": 18}})
	tool.End()
	if err := tool.Err(); err != nil {
		t.Fatalf("tool execution: %v", err)
	}

	_, embedding := client.StartEmbedding(context.Background(), EmbeddingStart{
		Model: ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
	})
	embedding.SetResult(EmbeddingResult{InputCount: 1, InputTokens: 3})
	embedding.End()
	if err := embedding.Err(); err != nil {
		t.Fatalf("embedding: %v", err)
	}

	var collected metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &collected); err != nil {
		t.Fatalf("collect: %v", err)
	}

	metricsByScope := make(map[string]map[string]metricdata.Metrics)
	for _, scope := range collected.ScopeMetrics {
		metrics := make(map[string]metricdata.Metrics, len(scope.Metrics))
		for _, m := range scope.Metrics {
			metrics[m.Name] = m
		}
		metricsByScope[scope.Scope.Name] = metrics
	}

	sdkMetrics, ok := metricsByScope[sdkScope]
	if !ok {
		t.Fatalf("no metrics recorded under %s", sdkScope)
	}
	otelMetrics, ok := metricsByScope[otelgenai.ScopeName]
	if !ok {
		t.Fatalf("no metrics recorded under %s", otelgenai.ScopeName)
	}

	conventions := map[string]struct {
		description string
		unit        string
	}{
		durationConvention.Name(): {
			description: durationConvention.Description(),
			unit:        durationConvention.Unit(),
		},
		usageConvention.Name(): {
			description: usageConvention.Description(),
			unit:        usageConvention.Unit(),
		},
	}
	for name, convention := range conventions {
		for scopeName, metrics := range map[string]map[string]metricdata.Metrics{
			sdkScope:            sdkMetrics,
			otelgenai.ScopeName: otelMetrics,
		} {
			m, present := metrics[name]
			if !present {
				t.Errorf("%s not recorded under %s", name, scopeName)
				continue
			}
			if m.Description != convention.description {
				t.Errorf("%s description under %s = %q, want %q", name, scopeName, m.Description, convention.description)
			}
			if m.Unit != convention.unit {
				t.Errorf("%s unit under %s = %q, want %q", name, scopeName, m.Unit, convention.unit)
			}
		}
	}

	for name, sdkMetric := range sdkMetrics {
		otelMetric, shared := otelMetrics[name]
		if !shared {
			continue
		}
		if sdkMetric.Description != otelMetric.Description {
			t.Errorf("%s descriptions differ: SDK = %q, otelgenai = %q", name, sdkMetric.Description, otelMetric.Description)
		}
		if sdkMetric.Unit != otelMetric.Unit {
			t.Errorf("%s units differ: SDK = %q, otelgenai = %q", name, sdkMetric.Unit, otelMetric.Unit)
		}
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
			InputTokens:           10,
			OutputTokens:          2,
			CacheReadInputTokens:  3,
			CacheWriteInputTokens: 4,
			InputSemantics:        TokenInputSemanticsInclusive,
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
	cacheCreation := int64(0)
	for _, point := range usageHistogram.DataPoints {
		if got, present := point.Attributes.Value("error.type"); present {
			t.Errorf("token usage carries error.type = %q", got.AsString())
		}
		tokenType, _ := point.Attributes.Value("gen_ai.token.type")
		switch tokenType.AsString() {
		case "cache_read":
			cacheRead = point.Sum
		case "cache_creation":
			cacheCreation = point.Sum
		}
	}
	if cacheRead != 3 {
		t.Errorf("cache_read token usage = %d, want 3", cacheRead)
	}
	if cacheCreation != 4 {
		t.Errorf("cache_creation token usage = %d, want 4", cacheCreation)
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
