package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import com.fasterxml.jackson.databind.JsonMappingException;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.common.AttributesBuilder;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.DoubleExemplarData;
import io.opentelemetry.sdk.metrics.data.HistogramPointData;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneOffset;
import java.util.ArrayList;
import java.util.Collection;
import java.util.Collections;
import java.util.List;
import java.util.Map;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

class Agento11yClientMetricExemplarTest {
    private static final AttributeKey<String> OPERATION_NAME_KEY =
            AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME);

    @Test
    void generationMetricsCarryTraceExemplar() {
        ExemplarHarness h = newHarness();
        try (Agento11yClient client = h.newClient()) {
            GenerationRecorder recorder = client.startGeneration(
                    new GenerationStart()
                            .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                            .setAgentName("test-agent"));
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
        }

        SpanData span = h.findSpan(op -> !"execute_tool".equals(op));
        assertExemplarTraceId(h.metricReader, Agento11yClient.METRIC_OPERATION_DURATION, span.getTraceId());
        h.shutdown();
    }

    @Test
    void embeddingMetricsCarryTraceExemplar() {
        ExemplarHarness h = newHarness();
        try (Agento11yClient client = h.newClient()) {
            EmbeddingRecorder recorder = client.startEmbedding(
                    new EmbeddingStart()
                            .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small"))
                            .setAgentName("test-agent"));
            recorder.setResult(new EmbeddingResult().setInputTokens(42).setInputCount(1));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
        }

        SpanData span = h.findSpan(op -> "embeddings".equals(op));
        assertExemplarTraceId(h.metricReader, Agento11yClient.METRIC_OPERATION_DURATION, span.getTraceId());
        h.shutdown();
    }

    static Stream<Arguments> toolMetricCases() {
        Stream<Arguments> existingCases = Stream.of(ContentCaptureMode.DEFAULT, ContentCaptureMode.FULL,
                        ContentCaptureMode.NO_TOOL_CONTENT, ContentCaptureMode.METADATA_ONLY,
                        ContentCaptureMode.FULL_WITH_METADATA_SPANS)
                .flatMap(mode -> Stream.of("sampled", "unsampled", "noop")
                        .flatMap(tracing -> Stream.of(-1, 0, 2)
                                .flatMap(history -> Stream.of("success", "error", "serialization", "nonzero_exit")
                                        .map(outcome -> Arguments.of(mode, tracing, history, outcome,
                                                " code-review ", "code-review")))));
        Stream<Arguments> whitespaceCases = Stream.of(
                        Arguments.of("code-review", "code-review"),
                        Arguments.of("", ""),
                        Arguments.of("\u0000code-review\u0000", "code-review"),
                        Arguments.of("\u0000", ""),
                        Arguments.of("\bcode-review\b", "code-review"),
                        Arguments.of("\b", ""),
                        Arguments.of(" \t\r\n\f\u000b ", ""),
                        Arguments.of(" \t\r\n\f\u000bcode-review\t\n ", "code-review"),
                        Arguments.of("\u2003code-review\u2003", "code-review"),
                        Arguments.of("\u2003", ""),
                        Arguments.of("\u00a0code-review\u00a0", "code-review"),
                        Arguments.of("\u00a0", ""),
                        Arguments.of("\u0085code-review\u0085", "code-review"),
                        Arguments.of("\u0085", ""),
                        Arguments.of(" \t\u2003\u00a0\u0085code-review\u0085\u00a0\u2003\r\n", "code-review"),
                        Arguments.of(" \t\u2003\u00a0\u0085\r\n", ""),
                        Arguments.of(" \tcode \t review\n ", "code \t review"),
                        Arguments.of("\u2003code\u2003\u00a0\u0085review\u00a0", "code\u2003\u00a0\u0085review"))
                .map(skill -> Arguments.of(ContentCaptureMode.NO_TOOL_CONTENT, "sampled", -1, "success",
                        skill.get()[0], skill.get()[1]));
        return Stream.concat(existingCases, whitespaceCases);
    }

    @ParameterizedTest(name = "tool metrics: {0}, {1}, history={2}, {3}, skill=[{4}]")
    @MethodSource("toolMetricCases")
    void toolExecutionMetricsCarryTraceExemplar(ContentCaptureMode mode, String tracing, int history, String outcome,
            String skillName, String metricSkillName) {
        ExemplarHarness h = newHarness(tracing);
        Agento11yClientConfig config = h.clientConfig();
        if (tracing.equals("noop")) {
            config.setTracer(OpenTelemetry.noop().getTracer("test"));
        }
        if (history >= 0) {
            config.setDebugBufferSize(history);
        }
        config.setTags(Map.of("team", "sdk"));
        Instant startedAt = Instant.parse("2026-03-12T09:00:00Z");
        boolean nonzeroExit = outcome.equals("nonzero_exit");
        boolean failed = outcome.equals("error") || (outcome.equals("serialization") && mode == ContentCaptureMode.FULL);
        try (Agento11yClient client = new Agento11yClient(config)) {
            ToolExecutionStart start = namedToolStart(skillName, startedAt)
                    .setExecutionKind(nonzeroExit ? ToolExecutionKind.COMMAND : ToolExecutionKind.LOAD_SKILL)
                    .setContentCapture(mode);
            ToolExecutionRecorder recorder = client.startToolExecution(start);
            start.setSkillName("mutated").setExecutionKind(ToolExecutionKind.READ_SKILL_RESOURCE);
            recorder.setResult(new ToolExecutionResult()
                    .setArguments(Map.of("private", "arguments"))
                    .setResult(outcome.equals("serialization") ? new UnserializableResult() : "private result")
                    .setProcessExitCode(nonzeroExit ? 2L : null)
                    .setCompletedAt(startedAt.plusMillis(250)));
            if (outcome.equals("error")) {
                recorder.setCallError(new IllegalStateException("skill not found"));
            }
            recorder.end();
            recorder.end();
            assertThat(recorder.error().isPresent()).isEqualTo(failed);
            if (outcome.equals("error")) {
                assertThat(recorder.error().orElseThrow()).hasMessage("skill not found");
            } else if (failed) {
                assertThat(recorder.error().orElseThrow()).isInstanceOf(JsonMappingException.class);
            }
            assertThat(client.debugSnapshot().getToolExecutions()).hasSize(history > 0 ? 1 : 0);

            Collection<MetricData> metrics = h.metricReader.collectAllMetrics();
            assertThat(metrics).extracting(MetricData::getName).containsExactly(Agento11yClient.METRIC_OPERATION_DURATION);
            MetricData duration = metrics.iterator().next();
            assertThat(duration.getUnit()).isEqualTo("s");
            assertThat(duration.getDescription()).isEqualTo("GenAI operation duration.");
            assertThat(duration.getHistogramData().getPoints()).singleElement().satisfies(point -> {
                assertThat(point.getAttributes()).isEqualTo(toolMetricAttributes(metricSkillName,
                        nonzeroExit ? "command" : "load_skill", failed));
                assertHistogram(point, 1, 0.25);
                if (!tracing.equals("sampled")) {
                    assertThat(point.getExemplars()).isEmpty();
                }
            });
            if (tracing.equals("sampled")) {
                assertThat(h.spanExporter.getFinishedSpanItems()).hasSize(1);
                SpanData span = h.findSpan(op -> "execute_tool".equals(op));
                assertThat(span.getStatus().getStatusCode()).isEqualTo(failed ? StatusCode.ERROR : StatusCode.OK);
                assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tool.execution.kind"))).isNull();
                assertThat(span.getAttributes().get(AttributeKey.stringKey("gen_ai.skill.name")))
                        .isEqualTo(skillName.isBlank() ? null : skillName.trim());
                assertThat(span.getEvents()).hasSize(failed && mode != ContentCaptureMode.METADATA_ONLY
                        && mode != ContentCaptureMode.FULL_WITH_METADATA_SPANS ? 1 : 0);
                assertThat(duration.getHistogramData().getPoints().iterator().next().getExemplars())
                        .singleElement().satisfies(exemplar -> {
                            assertThat(exemplar.getSpanContext().getTraceId()).isEqualTo(span.getTraceId());
                            assertThat(exemplar.getSpanContext().getSpanId()).isEqualTo(span.getSpanId());
                            assertThat(exemplar.getValue()).isEqualTo(0.25);
                        });
            } else {
                assertThat(h.spanExporter.getFinishedSpanItems()).isEmpty();
            }
        } finally {
            h.shutdown();
        }
    }

    @Test
    void distinctSkillsSplitSeriesWithoutChangingAggregateOrOtherOperations() {
        ExemplarHarness h = newHarness();
        Instant startedAt = Instant.parse("2026-03-12T09:00:00Z");
        try (Agento11yClient client = new Agento11yClient(h.clientConfig()
                .setClock(Clock.fixed(startedAt.plusMillis(250), ZoneOffset.UTC))
                .setTags(Map.of("team", "sdk")))) {
            for (String skill : List.of("code-review", "testing", "code-review")) {
                ToolExecutionRecorder tool = client.startToolExecution(namedToolStart(skill, startedAt));
                tool.setResult(new ToolExecutionResult().setCompletedAt(startedAt.plusMillis(250)));
                tool.end();
                tool.end();
            }
            GenerationRecorder generation = client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setAgentName("test-agent").setStartedAt(startedAt));
            generation.setResult(new GenerationResult().setCompletedAt(startedAt.plusMillis(250)));
            generation.end();
            EmbeddingRecorder embedding = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("embedding-model"))
                    .setAgentName("test-agent").setStartedAt(startedAt));
            embedding.setResult(new EmbeddingResult().setInputCount(1).setInputTokens(42));
            embedding.end();

            Collection<MetricData> metrics = h.metricReader.collectAllMetrics();
            assertThat(metrics).extracting(MetricData::getName).containsExactlyInAnyOrder(
                    Agento11yClient.METRIC_OPERATION_DURATION, Agento11yClient.METRIC_TOKEN_USAGE,
                    Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION);
            MetricData duration = metrics.stream().filter(m -> m.getName().equals(Agento11yClient.METRIC_OPERATION_DURATION))
                    .findFirst().orElseThrow();
            List<HistogramPointData> tools = duration.getHistogramData().getPoints().stream()
                    .filter(point -> "execute_tool".equals(point.getAttributes().get(OPERATION_NAME_KEY))).toList();
            assertThat(tools).hasSize(2);
            assertThat(tools.stream().mapToLong(HistogramPointData::getCount).sum()).isEqualTo(3);
            assertThat(tools.stream().mapToDouble(HistogramPointData::getSum).sum()).isEqualTo(0.75);
            for (HistogramPointData point : tools) {
                String skill = point.getAttributes().get(AttributeKey.stringKey("gen_ai.skill.name"));
                assertThat(skill).isIn("code-review", "testing");
                assertThat(point.getAttributes()).isEqualTo(toolMetricAttributes(skill, "load_skill", false));
                long count = skill.equals("code-review") ? 2 : 1;
                assertHistogram(point, count, count * 0.25);
            }
            assertThat(duration.getHistogramData().getPoints()).hasSize(4);
            duration.getHistogramData().getPoints().stream().filter(point -> !tools.contains(point)).forEach(point -> {
                String operation = point.getAttributes().get(OPERATION_NAME_KEY);
                assertThat(operation).isIn("generateText", "embeddings");
                String model = operation.equals("embeddings") ? "embedding-model" : "gpt-5";
                assertThat(point.getAttributes()).isEqualTo(nonToolIdentity(model).toBuilder()
                        .put("gen_ai.operation.name", operation).put("error.type", "").put("error.category", "").build());
                assertHistogram(point, 1, 0.25);
            });
            metrics.stream().filter(m -> m.getName().equals(Agento11yClient.METRIC_TOKEN_USAGE)).forEach(metric ->
                    assertThat(metric.getHistogramData().getPoints()).singleElement().satisfies(point -> {
                        assertThat(point.getAttributes()).isEqualTo(nonToolIdentity("embedding-model").toBuilder()
                                .put("gen_ai.operation.name", "embeddings").put("gen_ai.token.type", "input").build());
                        assertThat(point.getCount()).isEqualTo(1);
                        assertThat(point.getSum()).isEqualTo(42);
                    }));
            metrics.stream().filter(m -> m.getName().equals(Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION)).forEach(metric ->
                    assertThat(metric.getHistogramData().getPoints()).singleElement().satisfies(point -> {
                        assertThat(point.getAttributes()).isEqualTo(nonToolIdentity("gpt-5"));
                        assertThat(point.getCount()).isEqualTo(1);
                        assertThat(point.getSum()).isZero();
                    }));
            assertThat(client.debugSnapshot().getToolExecutions()).isEmpty();
            assertThat(client.debugSnapshot().getGenerations()).isEmpty();
        } finally {
            h.shutdown();
        }
    }

    private static ToolExecutionStart namedToolStart(String skill, Instant startedAt) {
        return new ToolExecutionStart().setToolName("host-tool").setExecutionKind(ToolExecutionKind.LOAD_SKILL)
                .setSkillName(skill).setAgentName("test-agent").setAgentVersion("v1")
                .setRequestProvider("openai").setRequestModel("gpt-5").setStartedAt(startedAt)
                .setToolCallId("private-call").setConversationId("private-conversation")
                .setToolDescription("private tool description").setSkillDescription("private instructions")
                .setSkillSourceURI("file:///private/skill").setSkillResourceName("private.md")
                .setProcessExecutableName("private-executable").setProcessExecutablePath("/private/executable");
    }

    private static Attributes toolMetricAttributes(String skill, String kind, boolean failed) {
        AttributesBuilder attributes = nonToolIdentity("gpt-5").toBuilder().put("gen_ai.agent.version", "v1")
                .put("gen_ai.operation.name", "execute_tool").put("gen_ai.tool.name", "host-tool")
                .put("error.type", failed ? "tool_execution_error" : "").put("error.category", failed ? "sdk_error" : "")
                .put("agento11y.tool.execution.kind", kind);
        if (!skill.isEmpty()) {
            attributes.put("gen_ai.skill.name", skill);
        }
        return attributes.build();
    }

    private static Attributes nonToolIdentity(String model) {
        return Attributes.builder().put("gen_ai.provider.name", "openai").put("gen_ai.request.model", model)
                .put("gen_ai.agent.name", "test-agent").put("agento11y.tag.team", "sdk").build();
    }

    private static void assertHistogram(HistogramPointData point, long count, double sum) {
        assertThat(point.getCount()).isEqualTo(count);
        assertThat(point.getSum()).isEqualTo(sum);
        assertThat(point.getBoundaries()).isEqualTo(Agento11yClient.DURATION_BUCKETS_SECONDS);
        List<Long> buckets = new ArrayList<>(Collections.nCopies(point.getBoundaries().size() + 1, 0L));
        int bucket = 0;
        while (bucket < point.getBoundaries().size() && point.getBoundaries().get(bucket) < 0.25) {
            bucket++;
        }
        buckets.set(bucket, count);
        assertThat(point.getCounts()).isEqualTo(buckets);
    }

    public static final class UnserializableResult {
        public String getValue() {
            throw new IllegalStateException("cannot serialize private result");
        }
    }

    // -----------------------------------------------------------------------
    // Harness
    // -----------------------------------------------------------------------

    private static final class ExemplarHarness {
        final InMemorySpanExporter spanExporter;
        final SdkTracerProvider tracerProvider;
        final InMemoryMetricReader metricReader;
        final SdkMeterProvider meterProvider;

        ExemplarHarness(
                InMemorySpanExporter spanExporter,
                SdkTracerProvider tracerProvider,
                InMemoryMetricReader metricReader,
                SdkMeterProvider meterProvider) {
            this.spanExporter = spanExporter;
            this.tracerProvider = tracerProvider;
            this.metricReader = metricReader;
            this.meterProvider = meterProvider;
        }

        Agento11yClient newClient() {
            return new Agento11yClient(clientConfig());
        }

        Agento11yClientConfig clientConfig() {
            return new Agento11yClientConfig()
                    .setTracer(tracerProvider.get("test"))
                    .setMeter(meterProvider.get("test"))
                    .setGenerationExporter(new TestFixtures.CapturingExporter())
                    .setGenerationExport(new GenerationExportConfig()
                            .setBatchSize(100)
                            .setQueueSize(100)
                            .setFlushInterval(Duration.ofMinutes(10))
                            .setMaxRetries(0));
        }

        SpanData findSpan(java.util.function.Predicate<String> operationFilter) {
            return spanExporter.getFinishedSpanItems().stream()
                    .filter(s -> operationFilter.test(s.getAttributes().get(OPERATION_NAME_KEY)))
                    .findFirst()
                    .orElseThrow(() -> new AssertionError("no span matching filter"));
        }

        void shutdown() {
            tracerProvider.shutdown();
            meterProvider.shutdown();
        }
    }

    private static ExemplarHarness newHarness() {
        return newHarness("sampled");
    }

    private static ExemplarHarness newHarness(String tracing) {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
                .setSampler(tracing.equals("sampled") ? Sampler.alwaysOn() : Sampler.alwaysOff())
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();
        InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        return new ExemplarHarness(spanExporter, tracerProvider, metricReader, meterProvider);
    }

    private static void assertExemplarTraceId(InMemoryMetricReader metricReader, String metricName, String wantTraceId) {
        Collection<MetricData> allMetrics = metricReader.collectAllMetrics();
        MetricData durationMetric = allMetrics.stream()
                .filter(m -> m.getName().equals(metricName))
                .findFirst()
                .orElseThrow(() -> new AssertionError("metric " + metricName + " not found"));

        List<DoubleExemplarData> exemplars = durationMetric.getHistogramData().getPoints().stream()
                .map(HistogramPointData::getExemplars)
                .flatMap(List::stream)
                .toList();

        assertThat(exemplars).as("exemplars on %s", metricName).isNotEmpty();
        assertThat(exemplars.get(0).getSpanContext().getTraceId())
                .as("exemplar trace_id")
                .isEqualTo(wantTraceId);
    }
}
