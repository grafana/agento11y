package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.DoubleExemplarData;
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
import org.junit.jupiter.api.Test;

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

    @Test
    void toolExecutionMetricsCarryTraceExemplar() {
        ExemplarHarness h = newHarness();
        try (Agento11yClient client = h.newClient()) {
            ToolExecutionRecorder recorder = client.startToolExecution(
                    new ToolExecutionStart()
                            .setToolName("weather")
                            .setAgentName("test-agent"));
            recorder.setResult(new ToolExecutionResult().setResult("sunny"));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
        }

        SpanData span = h.findSpan(op -> "execute_tool".equals(op));
        assertExemplarTraceId(h.metricReader, Agento11yClient.METRIC_OPERATION_DURATION, span.getTraceId());
        h.shutdown();
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
            Agento11yClientConfig config = new Agento11yClientConfig()
                    .setTracer(tracerProvider.get("test"))
                    .setMeter(meterProvider.get("test"))
                    .setGenerationExporter(new TestFixtures.CapturingExporter())
                    .setGenerationExport(new GenerationExportConfig()
                            .setBatchSize(100)
                            .setQueueSize(100)
                            .setFlushInterval(Duration.ofMinutes(10))
                            .setMaxRetries(0));
            return new Agento11yClient(config);
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
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
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
