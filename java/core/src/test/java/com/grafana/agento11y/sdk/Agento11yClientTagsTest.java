package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.HistogramPointData;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.time.Duration;
import java.util.Collection;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.AfterEach;
import org.junit.jupiter.api.Test;

class Agento11yClientTagsTest {
    private static final AttributeKey<String> CLIENT_TAG_PROJECT_KEY =
            AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TAG_PREFIX + "project");

    private final InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
    private final SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
            .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
            .build();
    private final InMemoryMetricReader metricReader = InMemoryMetricReader.create();
    private final SdkMeterProvider meterProvider = SdkMeterProvider.builder()
            .registerMetricReader(metricReader)
            .build();
    private final TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();

    @AfterEach
    void shutdownProviders() {
        tracerProvider.shutdown();
        meterProvider.shutdown();
    }

    private Agento11yClient newClient(Map<String, String> tags) {
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("agento11y-test"))
                .setMeter(meterProvider.get("agento11y-test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));
        if (tags != null) {
            config.setTags(tags);
        }
        return new Agento11yClient(config);
    }

    @Test
    void clientTagsOnGenerationSpanAndMetrics() {
        try (Agento11yClient client = newClient(Map.of("project", "checkout-svc"))) {
            GenerationStart start = TestFixtures.startFixture();
            GenerationRecorder recorder = client.startStreamingGeneration(start);
            recorder.setFirstTokenAt(start.getStartedAt().plusMillis(250));
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
        }

        SpanData span = singleSpan();
        assertThat(span.getAttributes().get(CLIENT_TAG_PROJECT_KEY)).isEqualTo("checkout-svc");

        Collection<MetricData> metrics = metricReader.collectAllMetrics();
        for (String metricName : List.of(
                Agento11yClient.METRIC_OPERATION_DURATION,
                Agento11yClient.METRIC_TOKEN_USAGE,
                Agento11yClient.METRIC_TTFT,
                Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION)) {
            for (HistogramPointData point : histogramPoints(metrics, metricName)) {
                assertThat(point.getAttributes().get(CLIENT_TAG_PROJECT_KEY))
                        .as("expected agento11y.tag.project on %s", metricName)
                        .isEqualTo("checkout-svc");
            }
        }
    }

    @Test
    void clientTagsOnEmbeddingAndToolSpansAndMetrics() {
        try (Agento11yClient client = newClient(Map.of("project", "embed-tools"))) {
            EmbeddingRecorder embedding = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            embedding.setResult(new EmbeddingResult().setInputTokens(1));
            embedding.end();

            ToolExecutionRecorder tool = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("weather"));
            tool.setResult(new ToolExecutionResult().setResult("sunny"));
            tool.end();
        }

        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        assertThat(spans).hasSize(2);
        for (SpanData span : spans) {
            assertThat(span.getAttributes().get(CLIENT_TAG_PROJECT_KEY))
                    .as("expected agento11y.tag.project on span %s", span.getName())
                    .isEqualTo("embed-tools");
        }

        Collection<MetricData> metrics = metricReader.collectAllMetrics();
        // Embedding duration + tool duration share the operation.duration
        // instrument; embedding token usage is the token.usage instrument.
        for (String metricName : List.of(Agento11yClient.METRIC_OPERATION_DURATION, Agento11yClient.METRIC_TOKEN_USAGE)) {
            for (HistogramPointData point : histogramPoints(metrics, metricName)) {
                assertThat(point.getAttributes().get(CLIENT_TAG_PROJECT_KEY))
                        .as("expected agento11y.tag.project on %s", metricName)
                        .isEqualTo("embed-tools");
            }
        }
    }

    @Test
    void clientTagsAreNormalizedAndSorted() {
        Attributes attributes = Agento11yClient.tagAttributes(Map.of(
                " z ", " last ",
                "   ", "discard",
                " a ", ""));

        assertThat(attributes.size()).isEqualTo(2);
        assertThat(attributes.get(AttributeKey.stringKey("agento11y.tag.a"))).isEmpty();
        assertThat(attributes.get(AttributeKey.stringKey("agento11y.tag.z"))).isEqualTo("last");

        try (Agento11yClient client = newClient(Map.of(" z ", " last ", "   ", "discard", " a ", ""))) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
        }

        SpanData span = singleSpan();
        assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tag.a"))).isEmpty();
        assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tag.z"))).isEqualTo("last");
        span.getAttributes().forEach((key, value) ->
                assertThat(key.getKey().trim()).isEqualTo(key.getKey()));
    }

    @Test
    void emptyClientTagsAreNoOp() {
        try (Agento11yClient client = newClient(null)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            EmbeddingRecorder embedding = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            embedding.setResult(new EmbeddingResult().setInputTokens(1));
            embedding.end();

            ToolExecutionRecorder tool = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("weather"));
            tool.setResult(new ToolExecutionResult().setResult("sunny"));
            tool.end();
        }

        for (SpanData span : spanExporter.getFinishedSpanItems()) {
            span.getAttributes().forEach((key, value) ->
                    assertThat(key.getKey())
                            .as("unexpected tag attribute on span %s", span.getName())
                            .doesNotStartWith(Agento11yClient.SPAN_ATTR_TAG_PREFIX));
        }

        for (MetricData metric : metricReader.collectAllMetrics()) {
            for (HistogramPointData point : metric.getHistogramData().getPoints()) {
                point.getAttributes().forEach((key, value) ->
                        assertThat(key.getKey())
                                .as("unexpected tag attribute on metric %s", metric.getName())
                                .doesNotStartWith(Agento11yClient.SPAN_ATTR_TAG_PREFIX));
            }
        }
    }

    @Test
    void perCallGenerationTagsStayExportOnly() {
        try (Agento11yClient client = newClient(null)) {
            GenerationStart start = TestFixtures.startFixture()
                    .setTags(Map.of("call_only", "yes"));
            GenerationRecorder recorder = client.startGeneration(start);
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            client.flush();
        }

        SpanData span = singleSpan();
        assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tag.call_only"))).isNull();

        for (MetricData metric : metricReader.collectAllMetrics()) {
            for (HistogramPointData point : metric.getHistogramData().getPoints()) {
                assertThat(point.getAttributes().get(AttributeKey.stringKey("agento11y.tag.call_only")))
                        .as("per-call tag must not appear on metric %s", metric.getName())
                        .isNull();
            }
        }

        assertThat(exporter.getRequests()).hasSize(1);
        Generation generation = exporter.getRequests().get(0).get(0);
        assertThat(generation.getTags()).containsEntry("call_only", "yes");
    }

    private SpanData singleSpan() {
        List<SpanData> spans = spanExporter.getFinishedSpanItems();
        assertThat(spans).hasSize(1);
        return spans.get(0);
    }

    private static List<HistogramPointData> histogramPoints(Collection<MetricData> metrics, String name) {
        MetricData metric = metrics.stream()
                .filter(m -> name.equals(m.getName()))
                .findFirst()
                .orElseThrow(() -> new AssertionError("missing metric " + name));
        List<HistogramPointData> points = List.copyOf(metric.getHistogramData().getPoints());
        assertThat(points).as("expected data points for %s", name).isNotEmpty();
        return points;
    }
}
