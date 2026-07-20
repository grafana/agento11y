package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.HistogramPointData;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import java.time.Duration;
import java.util.Collection;
import java.util.List;
import org.junit.jupiter.api.Test;

class Agento11yClientMetricsTest {
    @Test
    void generationRecorderEmitsAllClientMetrics() {
        InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setMeter(meterProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        GenerationStart start = TestFixtures.startFixture()
                .setMode(null)
                .setOperationName("");

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder recorder = client.startStreamingGeneration(start);
            recorder.setFirstTokenAt(start.getStartedAt().plusMillis(250));

            GenerationResult result = TestFixtures.resultFixture();
            result.setMode(GenerationMode.STREAM);
            result.setOperationName("streamText");
            result.getUsage().setReasoningTokens(5);
            result.getUsage().setCacheWriteInputTokens(3);
            recorder.setResult(result);
            recorder.end();
        }

        Collection<MetricData> metrics = metricReader.collectAllMetrics();
        List<String> metricNames = metrics.stream()
                .map(MetricData::getName)
                .toList();

        assertThat(metricNames).contains(
                Agento11yClient.METRIC_OPERATION_DURATION,
                Agento11yClient.METRIC_TOKEN_USAGE,
                Agento11yClient.METRIC_TTFT,
                Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION
        );

        List<Double> expectedBuckets = List.of(
                0.01, 0.02, 0.04, 0.08, 0.16, 0.32, 0.64, 1.28,
                2.56, 5.12, 10.24, 20.48, 40.96, 81.92);

        assertThat(histogramBucketBoundaries(metrics, Agento11yClient.METRIC_OPERATION_DURATION))
                .as("operation duration buckets")
                .isEqualTo(expectedBuckets);
        assertThat(histogramBucketBoundaries(metrics, Agento11yClient.METRIC_TTFT))
                .as("time-to-first-token buckets")
                .isEqualTo(expectedBuckets);

        List<Double> expectedTokenUsageBuckets = List.of(
                1.0, 4.0, 16.0, 64.0, 256.0, 1024.0, 4096.0, 16384.0,
                65536.0, 262144.0, 1048576.0, 4194304.0, 16777216.0, 67108864.0);
        assertThat(histogramBucketBoundaries(metrics, Agento11yClient.METRIC_TOKEN_USAGE))
                .as("token usage buckets")
                .isEqualTo(expectedTokenUsageBuckets);

        tracerProvider.shutdown();
        meterProvider.shutdown();
    }

    private static List<Double> histogramBucketBoundaries(Collection<MetricData> metrics, String name) {
        MetricData metric = metrics.stream()
                .filter(m -> name.equals(m.getName()))
                .findFirst()
                .orElseThrow(() -> new AssertionError("missing metric " + name));
        HistogramPointData point = metric.getHistogramData().getPoints().iterator().next();
        return point.getBoundaries();
    }

    @Test
    void embeddingRecorderEmitsDurationAndInputTokenMetricsOnly() {
        InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setMeter(meterProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small"))
                    .setAgentName("agent-embed"));
            recorder.setResult(new EmbeddingResult()
                    .setInputCount(2)
                    .setInputTokens(42));
            recorder.end();
        }

        List<String> metricNames = metricReader.collectAllMetrics().stream()
                .map(MetricData::getName)
                .toList();

        assertThat(metricNames).contains(
                Agento11yClient.METRIC_OPERATION_DURATION,
                Agento11yClient.METRIC_TOKEN_USAGE
        );
        assertThat(metricNames).doesNotContain(
                Agento11yClient.METRIC_TTFT,
                Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION
        );

        tracerProvider.shutdown();
        meterProvider.shutdown();
    }
}
