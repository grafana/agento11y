package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.trace.SpanKind;
import io.opentelemetry.context.Context;
import io.opentelemetry.context.Scope;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.data.LinkData;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import io.opentelemetry.sdk.trace.samplers.SamplingResult;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import java.time.Duration;
import java.util.List;
import java.util.Optional;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import org.junit.jupiter.params.provider.NullSource;
import org.junit.jupiter.params.provider.ValueSource;

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

    static Stream<Arguments> toolSamplerExecutionKinds() {
        return Stream.of(
                Arguments.of(null, " check.py ", "execute_tool tool"),
                Arguments.of(ToolExecutionKind.LOAD_SKILL, " check.py ", "execute_tool tool review"),
                Arguments.of(ToolExecutionKind.READ_SKILL_RESOURCE, " check.py ", "execute_tool tool review check.py"),
                Arguments.of(ToolExecutionKind.COMMAND, " check.py ", "execute_tool tool review check.py"),
                Arguments.of(ToolExecutionKind.READ_SKILL_RESOURCE, "\u2003", "execute_tool tool"),
                Arguments.of(ToolExecutionKind.COMMAND, "\u2003", "execute_tool tool python3"));
    }

    @ParameterizedTest
    @MethodSource("toolSamplerExecutionKinds")
    void toolSamplerCanSelectExecutionKindsWithoutPrivateContent(ToolExecutionKind executionKind,
                                                                String resource, String expectedName) {
        InMemorySpanExporter spans = InMemorySpanExporter.create();
        AtomicInteger sampled = new AtomicInteger();
        Sampler sampler = new Sampler() {
            @Override
            public SamplingResult shouldSample(Context parent, String traceId, String name, SpanKind kind,
                                               Attributes attributes, List<LinkData> links) {
                sampled.incrementAndGet();
                assertThat(kind).isEqualTo(SpanKind.INTERNAL);
                assertThat(attributes.get(AttributeKey.stringKey("gen_ai.tool.name"))).isEqualTo("tool");
                assertThat(attributes.get(AttributeKey.stringKey("gen_ai.skill.name")))
                        .isEqualTo(executionKind == null ? null : "review");
                assertThat(attributes.get(AttributeKey.stringKey("gen_ai.skill.resource.name")))
                        .isEqualTo(!resource.isBlank() && (executionKind == ToolExecutionKind.READ_SKILL_RESOURCE
                                || executionKind == ToolExecutionKind.COMMAND) ? "check.py" : null);
                assertThat(attributes.get(AttributeKey.stringKey("process.executable.name")))
                        .isEqualTo(executionKind == ToolExecutionKind.COMMAND ? "python3" : null);
                assertThat(attributes.get(AttributeKey.longKey("process.exit.code"))).isNull();
                for (String key : List.of("gen_ai.skill.description", "gen_ai.skill.source.uri",
                        "process.executable.path", "gen_ai.tool.description", "agento11y.skill.name")) {
                    assertThat(attributes.get(AttributeKey.stringKey(key))).as(key).isNull();
                }
                return expectedName.equals(name) ? SamplingResult.recordAndSample() : SamplingResult.drop();
            }

            @Override
            public String getDescription() {
                return "select tool execution kinds at creation";
            }
        };
        try (SdkTracerProvider provider = SdkTracerProvider.builder().setSampler(sampler)
                    .addSpanProcessor(SimpleSpanProcessor.create(spans)).build();
                Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                        .setTracer(provider.get("test"))
                        .setGenerationExporter(new TestFixtures.CapturingExporter()))) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                    .setToolName(" tool ")
                    .setExecutionKind(executionKind)
                    .setSkillName(" review ")
                    .setSkillResourceName(resource)
                    .setProcessExecutableName(" python3 ")
                    .setSkillDescription("private description")
                    .setSkillSourceURI("file:///private/skills")
                    .setProcessExecutablePath("/private/python3")
                    .setToolDescription("private tool description")
                    .setContentCapture(ContentCaptureMode.FULL));
            recorder.setResult(new ToolExecutionResult().setProcessExitCode(0L));
            recorder.end();
            assertThat(sampled.get()).isEqualTo(1);
            assertThat(spans.getFinishedSpanItems()).singleElement().satisfies(span ->
                    assertThat(span.getName()).isEqualTo(expectedName));
        }
    }

    static Stream<Arguments> skillSourceURIs() {
        return Stream.of(
                Arguments.of("file:///opt/my%20skills/review?token=secret#section", "file:///opt/my%20skills/review"),
                Arguments.of("https://user:secret@skills.example:8443/a%2Fb?q=secret#part", "https://skills.example:8443/a%2Fb"),
                Arguments.of("https://user:secret@[::1]:8443/review?secret#part", "https://[::1]:8443/review"),
                Arguments.of("s3://user:secret@skills/review?version=secret#part", "s3://skills/review"),
                Arguments.of("file:/opt/skills/review/SKILL.md", "file:///opt/skills/review/SKILL.md"),
                Arguments.of("file://user:secret@localhost/opt/my%20skills?secret#part", "file://localhost/opt/my%20skills"),
                Arguments.of("gs://skills-bucket/a%20b?secret#part", "gs://skills-bucket/a%20b"),
                Arguments.of("https://skills.example", "https://skills.example"),
                Arguments.of("https://skills.example:65535/review", "https://skills.example:65535/review"),
                Arguments.of("  https://skills.example/review  ", "https://skills.example/review"),
                Arguments.of("https://skills.example/a b", null),
                Arguments.of("https://skills.example/a\u2003b", null),
                Arguments.of("https://skills.example/a\u00a0b", null),
                Arguments.of("https://skills.example/a\tb", null),
                Arguments.of("https://skills.example/a\nb", null),
                Arguments.of("https://skills.example/a\u007fb", null),
                Arguments.of("https://skills.example/a%", null),
                Arguments.of("https://skills.example/a%2", null),
                Arguments.of("https://skills.example/a%GG", null),
                Arguments.of("https://skills.example/review?q=%GG", null),
                Arguments.of("https://skills.example/review#bad%", null),
                Arguments.of("https:", null),
                Arguments.of("https://", null),
                Arguments.of("https:///path", null),
                Arguments.of("https:/path", null),
                Arguments.of("https://user:secret@/path", null),
                Arguments.of("https://skills.example:invalid/review", null),
                Arguments.of("https://skills.example:-1/review", null),
                Arguments.of("https://skills.example:65536/review", null),
                Arguments.of("https://skills.example:999999999999/review", null),
                Arguments.of("https://skills.example:/review", null),
                Arguments.of("file:", null),
                Arguments.of("file:relative", null),
                Arguments.of("file://", null),
                Arguments.of("file://localhost", null),
                Arguments.of("file://localhost?secret#part", null),
                Arguments.of("s3:/path", null),
                Arguments.of("s3:///path", null),
                Arguments.of("skills/review", null),
                Arguments.of("//skills.example/review", null),
                Arguments.of("https://[broken", null),
                Arguments.of("https://skills.example/bad%path", null),
                Arguments.of(" ", null),
                Arguments.of("urn:private:instructions", null));
    }

    @ParameterizedTest
    @MethodSource("skillSourceURIs")
    void skillSourceURIIsScrubbedOnlyOnSpan(String source, String expected) {
        InMemorySpanExporter spans = InMemorySpanExporter.create();
        try (SdkTracerProvider provider = SdkTracerProvider.builder()
                    .addSpanProcessor(SimpleSpanProcessor.create(spans)).build();
                Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                        .setTracer(provider.get("test"))
                        .setGenerationExporter(new TestFixtures.CapturingExporter()))) {
            try (ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("load")
                    .setExecutionKind(ToolExecutionKind.LOAD_SKILL)
                    .setSkillSourceURI(source)
                    .setContentCapture(ContentCaptureMode.FULL))) {
                recorder.setResult(new ToolExecutionResult());
            }
            assertThat(spans.getFinishedSpanItems()).singleElement().satisfies(span ->
                    assertThat(span.getAttributes().get(AttributeKey.stringKey("gen_ai.skill.source.uri"))).isEqualTo(expected));
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getSkillSourceURI()).isEqualTo(source);
        }
    }

    @ParameterizedTest
    @NullSource
    @ValueSource(longs = {0, 7, -1, Long.MIN_VALUE, Long.MAX_VALUE})
    void commandExitCodeIsNullableLongAndIndependentOfCallError(Long code) {
        InMemorySpanExporter spans = InMemorySpanExporter.create();
        try (SdkTracerProvider provider = SdkTracerProvider.builder()
                    .addSpanProcessor(SimpleSpanProcessor.create(spans)).build();
                Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                        .setTracer(provider.get("test"))
                        .setGenerationExporter(new TestFixtures.CapturingExporter()))) {
            for (boolean failed : List.of(false, true)) {
                ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                        .setToolName("exec").setExecutionKind(ToolExecutionKind.COMMAND));
                recorder.setResult(new ToolExecutionResult().setProcessExitCode(code));
                if (failed) {
                    recorder.setCallError(new IllegalStateException("command failed"));
                }
                recorder.end();
                recorder.end();
            }
            assertThat(spans.getFinishedSpanItems()).hasSize(2);
            for (SpanData span : spans.getFinishedSpanItems()) {
                assertThat(span.getAttributes().get(AttributeKey.longKey("process.exit.code"))).isEqualTo(code);
            }
            assertThat(spans.getFinishedSpanItems().get(0).getStatus().getStatusCode()).isEqualTo(StatusCode.OK);
            assertThat(spans.getFinishedSpanItems().get(1).getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);
            assertThat(spans.getFinishedSpanItems().get(1).getEvents()).hasSize(1);
            assertThat(client.debugSnapshot().getToolExecutions()).allSatisfy(execution ->
                    assertThat(execution.getProcessExitCode()).isEqualTo(code));
        }
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
