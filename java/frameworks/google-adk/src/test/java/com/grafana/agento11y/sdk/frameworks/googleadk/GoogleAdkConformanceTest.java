package com.grafana.agento11y.sdk.frameworks.googleadk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.grafana.agento11y.sdk.ExportGenerationResult;
import com.grafana.agento11y.sdk.ExportGenerationsRequest;
import com.grafana.agento11y.sdk.ExportGenerationsResponse;
import com.grafana.agento11y.sdk.Generation;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExporter;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.Span;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import org.junit.jupiter.api.Test;

class GoogleAdkConformanceTest {
    @Test
    void runLifecycleConformancePropagatesFrameworkMetadataAndParentSpan() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv()) {
            Span parent = env.tracerProvider.get("google-adk-framework").spanBuilder("google-adk.parent").startSpan();
            try (var scope = parent.makeCurrent()) {
                Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(env.client, new Agento11yGoogleAdkAdapter.Options()
                        .setAgentName("planner")
                        .setAgentVersion("1.0.0")
                        .setCaptureInputs(true)
                        .setCaptureOutputs(true));

                adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                        .setRunId("run-sync")
                        .setConversationId("conversation-42")
                        .setThreadId("thread-42")
                        .setParentRunId("parent-run-42")
                        .setEventId("event-42")
                        .setComponentName("planner")
                        .setRunType("chat")
                        .setRetryAttempt(2)
                        .addTag("prod")
                        .addTag("framework")
                        .setModelName("gpt-5")
                        .addPrompt("hello")
                        .putMetadata("team", "infra"));
                adapter.onRunEnd("run-sync", new Agento11yGoogleAdkAdapter.RunEndEvent()
                        .setResponseModel("gpt-5")
                        .setStopReason("stop")
                        .setUsage(new com.grafana.agento11y.sdk.TokenUsage().setInputTokens(12L).setOutputTokens(4L).setTotalTokens(16L)));
            } finally {
                parent.end();
            }

            Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            List<String> metricNames = env.metricNames();

            assertThat(generation.getTags())
                    .containsEntry("agento11y.framework.name", "google-adk")
                    .containsEntry("agento11y.framework.source", "handler")
                    .containsEntry("agento11y.framework.language", "java");
            assertThat(generation.getConversationId()).isEqualTo("conversation-42");
            assertThat(generation.getMetadata())
                    .containsEntry("agento11y.framework.run_id", "run-sync")
                    .containsEntry("agento11y.framework.run_type", "chat")
                    .containsEntry("agento11y.framework.thread_id", "thread-42")
                    .containsEntry("agento11y.framework.parent_run_id", "parent-run-42")
                    .containsEntry("agento11y.framework.component_name", "planner")
                    .containsEntry("agento11y.framework.retry_attempt", 2)
                    .containsEntry("agento11y.framework.event_id", "event-42");
            assertThat(generation.getMetadata().get("agento11y.framework.tags")).isEqualTo(List.of("prod", "framework"));
            assertThat(generation.getMetadata()).containsEntry("team", "infra");
            assertThat(span.getParentSpanContext().getSpanId()).isEqualTo(parent.getSpanContext().getSpanId());
            assertThat(metricNames).contains("gen_ai.client.operation.duration");
            assertThat(metricNames).doesNotContain("gen_ai.client.time_to_first_token");
        }
    }

    @Test
    void streamingConformanceStitchesOutputAndRecordsFirstTokenMetric() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv()) {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(env.client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-stream")
                    .setThreadId("thread-stream-42")
                    .setRunType("chat")
                    .setStream(true)
                    .setModelName("claude-sonnet-4-5")
                    .addPrompt("stream this"));
            adapter.onRunToken("run-stream", "hello");
            adapter.onRunToken("run-stream", " world");
            adapter.onRunEnd("run-stream", new Agento11yGoogleAdkAdapter.RunEndEvent().setResponseModel("claude-sonnet-4-5"));

            env.client.shutdown();

            Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            List<String> metricNames = env.metricNames();

            assertThat(generation.getMode()).isEqualTo(com.grafana.agento11y.sdk.GenerationMode.STREAM);
            assertThat(generation.getOperationName()).isEqualTo("streamText");
            assertThat(generation.getOutput()).hasSize(1);
            assertThat(generation.getOutput().get(0).getRole()).isEqualTo(MessageRole.ASSISTANT);
            assertThat(generation.getOutput().get(0).getParts()).hasSize(1);
            assertThat(generation.getOutput().get(0).getParts().get(0).getText()).isEqualTo("hello world");
            assertThat(span.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"))).isEqualTo("streamText");
            assertThat(metricNames).contains("gen_ai.client.operation.duration", "gen_ai.client.time_to_first_token");
        }
    }

    @Test
    void embeddingsConformanceUsesUnsupportedCapabilityContract() {
        assertThatThrownBy(Agento11yGoogleAdkAdapter::checkEmbeddingsSupport)
                .isInstanceOf(UnsupportedOperationException.class)
                .hasMessage(
                        "google-adk: embeddings are not supported because the Google ADK lifecycle surface does not expose a dedicated embeddings callback");
    }

    private static final class ConformanceEnv implements AutoCloseable {
        private final CapturingExporter exporter = new CapturingExporter();
        private final InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        private final InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        private final SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();
        private final SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        private final Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(tracerProvider.get("google-adk-conformance"))
                .setMeter(meterProvider.get("google-adk-conformance"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setQueueSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(0)));

        Generation singleGeneration() {
            awaitRequests();
            assertThat(exporter.requests).hasSize(1);
            assertThat(exporter.requests.get(0)).hasSize(1);
            return exporter.requests.get(0).get(0);
        }

        SpanData latestGenerationSpan() {
            List<SpanData> spans = spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> {
                        String operation = span.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"));
                        return "generateText".equals(operation) || "streamText".equals(operation);
                    })
                    .toList();
            assertThat(spans).isNotEmpty();
            return spans.get(spans.size() - 1);
        }

        List<String> metricNames() {
            return metricReader.collectAllMetrics().stream()
                    .map(metric -> metric.getName())
                    .distinct()
                    .sorted()
                    .toList();
        }

        private void awaitRequests() {
            long deadline = System.nanoTime() + Duration.ofSeconds(5).toNanos();
            while (System.nanoTime() < deadline) {
                if (!exporter.requests.isEmpty()) {
                    return;
                }
                try {
                    Thread.sleep(10L);
                } catch (InterruptedException exception) {
                    Thread.currentThread().interrupt();
                    throw new AssertionError("interrupted while waiting for export", exception);
                }
            }
            throw new AssertionError("timed out waiting for generation export");
        }

        @Override
        public void close() throws Exception {
            client.shutdown();
            meterProvider.close();
            tracerProvider.close();
        }
    }

    private static final class CapturingExporter implements GenerationExporter {
        private final List<List<Generation>> requests = new CopyOnWriteArrayList<>();

        @Override
        public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
            List<Generation> batch = new ArrayList<>();
            for (Generation generation : request.getGenerations()) {
                batch.add(generation.copy());
            }
            requests.add(batch);

            List<ExportGenerationResult> results = new ArrayList<>();
            for (Generation generation : batch) {
                results.add(new ExportGenerationResult().setGenerationId(generation.getId()).setAccepted(true));
            }
            return new ExportGenerationsResponse().setResults(results);
        }
    }
}
