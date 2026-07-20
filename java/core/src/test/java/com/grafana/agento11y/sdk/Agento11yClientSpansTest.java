package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.context.Scope;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import java.time.Duration;
import java.util.List;
import java.util.Optional;
import org.junit.jupiter.api.Test;

class Agento11yClientSpansTest {
    @Test
    void generationSpanHasRequiredAttributesAndErrorTyping() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            GenerationResult result = TestFixtures.resultFixture();
            result.getUsage().setReasoningTokens(5);
            result.getUsage().setCacheWriteInputTokens(3);
            recorder.setResult(result);
            recorder.setCallError(new RuntimeException("provider exploded"));
            recorder.end();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        assertThat(spans).hasSize(1);
        SpanData span = spans.get(0);
        assertThat(span.getName()).startsWith("streamText ");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_SDK_NAME))).isEqualTo("sdk-java");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_PROVIDER_NAME))).isEqualTo("anthropic");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_REQUEST_MODEL))).isEqualTo("claude-sonnet-4-5");
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_REQUEST_MAX_TOKENS))).isEqualTo(256L);
        assertThat(span.getAttributes().get(AttributeKey.doubleKey(Agento11yClient.SPAN_ATTR_REQUEST_TEMPERATURE))).isEqualTo(0.25d);
        assertThat(span.getAttributes().get(AttributeKey.doubleKey(Agento11yClient.SPAN_ATTR_REQUEST_TOP_P))).isEqualTo(0.85d);
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_REQUEST_TOOL_CHOICE))).isEqualTo("required");
        assertThat(span.getAttributes().get(AttributeKey.booleanKey(Agento11yClient.SPAN_ATTR_REQUEST_THINKING_ENABLED))).isEqualTo(false);
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_REQUEST_THINKING_BUDGET))).isEqualTo(2048L);
        assertThat(span.getAttributes().get(AttributeKey.stringArrayKey(Agento11yClient.SPAN_ATTR_FINISH_REASONS))).containsExactly("stop");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_ERROR_TYPE))).isEqualTo("provider_call_error");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_ERROR_CATEGORY))).isEqualTo("sdk_error");
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_REASONING_TOKENS))).isEqualTo(5L);
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_CACHE_WRITE_TOKENS))).isEqualTo(3L);
        assertThat(span.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);

        provider.shutdown();
    }

    @Test
    void toolSpanNameAndAttributesMatchContract() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("weather")
                    .setToolCallId("call-1")
                    .setToolType("function")
                    .setToolDescription("Get weather")
                    .setRequestProvider("openai")
                    .setRequestModel("gpt-5"));
            recorder.setResult(new ToolExecutionResult().setArguments(java.util.Map.of("city", "Paris")).setResult("18C"));
            recorder.end();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        assertThat(spans).hasSize(1);
        SpanData span = spans.get(0);
        assertThat(span.getName()).isEqualTo("execute_tool weather");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_SDK_NAME))).isEqualTo("sdk-java");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME)))
                .isEqualTo(Agento11yClient.TOOL_EXECUTION_OPERATION_NAME);
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_NAME))).isEqualTo("weather");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_CALL_ID))).isEqualTo("call-1");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_PROVIDER_NAME))).isEqualTo("openai");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_REQUEST_MODEL))).isEqualTo("gpt-5");

        provider.shutdown();
    }

    @Test
    void embeddingSpanHasRequiredAttributesAndSkipsInputTextsByDefault() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small"))
                    .setAgentName("agent-embed")
                    .setAgentVersion("v-embed")
                    .setDimensions(256L)
                    .setEncodingFormat("float"));
            recorder.setResult(new EmbeddingResult()
                    .setInputCount(2)
                    .setInputTokens(42)
                    .setResponseModel("text-embedding-3-small")
                    .setDimensions(256L)
                    .setInputTexts(List.of("secret one", "secret two")));
            recorder.end();

            assertThat(recorder.error()).isEmpty();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        Optional<SpanData> embeddingSpan = spans.stream()
                .filter(span -> span.getName().startsWith("embeddings "))
                .findFirst();
        assertThat(embeddingSpan).isPresent();
        SpanData span = embeddingSpan.orElseThrow();

        assertThat(span.getName()).isEqualTo("embeddings text-embedding-3-small");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME))).isEqualTo("embeddings");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_PROVIDER_NAME))).isEqualTo("openai");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_REQUEST_MODEL))).isEqualTo("text-embedding-3-small");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_NAME))).isEqualTo("agent-embed");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION))).isEqualTo("v-embed");
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_EMBEDDING_DIM_COUNT))).isEqualTo(256L);
        assertThat(span.getAttributes().get(AttributeKey.stringArrayKey(Agento11yClient.SPAN_ATTR_REQUEST_ENCODING_FORMATS))).containsExactly("float");
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_INPUT_TOKENS))).isEqualTo(42L);
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_RESPONSE_MODEL))).isEqualTo("text-embedding-3-small");
        assertThat(span.getAttributes().get(AttributeKey.longKey(Agento11yClient.SPAN_ATTR_EMBEDDING_INPUT_COUNT))).isEqualTo(2L);
        assertThat(span.getAttributes().get(AttributeKey.stringArrayKey(Agento11yClient.SPAN_ATTR_EMBEDDING_INPUT_TEXTS))).isNull();
        assertThat(span.getStatus().getStatusCode()).isEqualTo(StatusCode.OK);
        assertThat(exporter.getRequests()).isEmpty();

        provider.shutdown();
    }

    @Test
    void embeddingSpanCapturesAndTruncatesInputTextsWhenEnabled() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(new TestFixtures.CapturingExporter())
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0))
                .setEmbeddingCapture(new EmbeddingCaptureConfig()
                        .setCaptureInput(true)
                        .setMaxInputItems(1)
                        .setMaxTextLength(5));

        try (Agento11yClient client = new Agento11yClient(config)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            recorder.setResult(new EmbeddingResult()
                    .setInputCount(2)
                    .setInputTexts(List.of("abcdefgh", "second")));
            recorder.end();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        Optional<SpanData> embeddingSpan = spans.stream()
                .filter(span -> span.getName().startsWith("embeddings "))
                .findFirst();
        assertThat(embeddingSpan).isPresent();
        SpanData span = embeddingSpan.orElseThrow();
        assertThat(span.getAttributes().get(AttributeKey.stringArrayKey(Agento11yClient.SPAN_ATTR_EMBEDDING_INPUT_TEXTS)))
                .containsExactly("ab...");

        provider.shutdown();
    }

    @Test
    void embeddingSpanMarksProviderCallErrors() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(new TestFixtures.CapturingExporter())
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            recorder.setCallError(new RuntimeException("embedding provider failed"));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        Optional<SpanData> embeddingSpan = spans.stream()
                .filter(span -> span.getName().startsWith("embeddings "))
                .findFirst();
        assertThat(embeddingSpan).isPresent();
        SpanData span = embeddingSpan.orElseThrow();
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_ERROR_TYPE))).isEqualTo("provider_call_error");
        assertThat(span.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);

        provider.shutdown();
    }

    @Test
    void embeddingSpanInheritsAgentContextWhenFieldsAreEmpty() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(new TestFixtures.CapturingExporter())
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config);
                Scope ignoredAgent = Agento11yContext.withAgentName("ctx-agent");
                Scope ignoredVersion = Agento11yContext.withAgentVersion("ctx-ver")) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            recorder.end();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        Optional<SpanData> embeddingSpan = spans.stream()
                .filter(span -> span.getName().startsWith("embeddings "))
                .findFirst();
        assertThat(embeddingSpan).isPresent();
        SpanData span = embeddingSpan.orElseThrow();
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_NAME))).isEqualTo("ctx-agent");
        assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION))).isEqualTo("ctx-ver");

        provider.shutdown();
    }
}
