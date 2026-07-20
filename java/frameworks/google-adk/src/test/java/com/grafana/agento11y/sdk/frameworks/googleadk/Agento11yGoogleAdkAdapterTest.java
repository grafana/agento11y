package com.grafana.agento11y.sdk.frameworks.googleadk;

import static org.assertj.core.api.Assertions.assertThat;

import com.grafana.agento11y.sdk.ExportGenerationResult;
import com.grafana.agento11y.sdk.ExportGenerationsRequest;
import com.grafana.agento11y.sdk.ExportGenerationsResponse;
import com.grafana.agento11y.sdk.Generation;
import com.grafana.agento11y.sdk.GenerationExporter;
import com.grafana.agento11y.sdk.GenerationMode;
import com.grafana.agento11y.sdk.GenerationRecorder;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExportProtocol;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.Message;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import com.grafana.agento11y.sdk.TokenUsage;
import com.grafana.agento11y.sdk.ToolExecutionStart;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.lang.reflect.Method;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;

class Agento11yGoogleAdkAdapterTest {
    private static Agento11yClient newClient() {
        return new Agento11yClient(
                new Agento11yClientConfig()
                        .setGenerationExport(
                                new GenerationExportConfig()
                                        .setProtocol(GenerationExportProtocol.NONE)));
    }

    @Test
    void resolveConversationUsesFrameworkIdsFirst() {
        Agento11yGoogleAdkAdapter.ConversationContext conversation = Agento11yGoogleAdkAdapter.resolveConversation(
                new Agento11yGoogleAdkAdapter.RunStartEvent()
                        .setRunId("run-1")
                        .setConversationId("conversation-1")
                        .setSessionId("session-1")
                        .setGroupId("group-1")
                        .setThreadId("thread-1"));

        assertThat(conversation.conversationId).isEqualTo("conversation-1");
        assertThat(conversation.threadId).isEqualTo("thread-1");

        conversation = Agento11yGoogleAdkAdapter.resolveConversation(
                new Agento11yGoogleAdkAdapter.RunStartEvent().setRunId("run-2").setSessionId("session-2"));
        assertThat(conversation.conversationId).isEqualTo("session-2");

        conversation = Agento11yGoogleAdkAdapter.resolveConversation(
                new Agento11yGoogleAdkAdapter.RunStartEvent().setRunId("run-3"));
        assertThat(conversation.conversationId).isEqualTo("agento11y:framework:google-adk:run-3");
    }

    @Test
    void normalizeProviderCoversKnownAndFallbackValues() {
        assertThat(Agento11yGoogleAdkAdapter.normalizeProvider("openai")).isEqualTo("openai");
        assertThat(Agento11yGoogleAdkAdapter.normalizeProvider("anthropic")).isEqualTo("anthropic");
        assertThat(Agento11yGoogleAdkAdapter.normalizeProvider("gemini")).isEqualTo("gemini");
        assertThat(Agento11yGoogleAdkAdapter.normalizeProvider("custom-provider")).isEqualTo("custom");

        assertThat(Agento11yGoogleAdkAdapter.inferProvider("gpt-5")).isEqualTo("openai");
        assertThat(Agento11yGoogleAdkAdapter.inferProvider("claude-sonnet-4-5")).isEqualTo("anthropic");
        assertThat(Agento11yGoogleAdkAdapter.inferProvider("gemini-2.5-pro")).isEqualTo("gemini");
        assertThat(Agento11yGoogleAdkAdapter.inferProvider("mistral-large")).isEqualTo("custom");
    }

    @Test
    void buildFrameworkMetadataIncludesCanonicalLineageKeys() {
        Map<String, Object> metadata = Agento11yGoogleAdkAdapter.buildFrameworkMetadata(new Agento11yGoogleAdkAdapter.MetadataInput()
                .setBase(Map.of("team", "infra"))
                .setEvent(Map.of("phase", "plan"))
                .setRunId("run-1")
                .setThreadId("thread-1")
                .setParentRunId("parent-1")
                .setComponentName("planner")
                .setRunType("chat")
                .setTags(List.of("prod", "framework"))
                .setRetryAttempt(3)
                .setEventId("event-1"));

        assertThat(metadata)
                .containsEntry(Agento11yGoogleAdkAdapter.META_RUN_ID, "run-1")
                .containsEntry(Agento11yGoogleAdkAdapter.META_RUN_TYPE, "chat")
                .containsEntry(Agento11yGoogleAdkAdapter.META_THREAD_ID, "thread-1")
                .containsEntry(Agento11yGoogleAdkAdapter.META_PARENT_RUN_ID, "parent-1")
                .containsEntry(Agento11yGoogleAdkAdapter.META_COMPONENT_NAME, "planner")
                .containsEntry(Agento11yGoogleAdkAdapter.META_RETRY_ATTEMPT, 3)
                .containsEntry(Agento11yGoogleAdkAdapter.META_EVENT_ID, "event-1")
                .containsEntry("team", "infra")
                .containsEntry("phase", "plan");
    }

    @Test
    void adapterRunAndToolLifecycleCompletesWithoutRecorderErrors() {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setAgentName("adk-agent")
                    .setAgentVersion("1.0.0")
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-sync")
                    .setSessionId("session-42")
                    .setParentRunId("parent-run")
                    .setEventId("event-42")
                    .setRunType("chat")
                    .setModelName("gpt-5")
                    .addPrompt("hello")
                    .putMetadata("team", "infra"));
            adapter.onRunEnd("run-sync", new Agento11yGoogleAdkAdapter.RunEndEvent().setResponseModel("gpt-5").setStopReason("stop"));

            adapter.onToolStart(new Agento11yGoogleAdkAdapter.ToolStartEvent()
                    .setRunId("tool-run")
                    .setSessionId("session-42")
                    .setToolName("lookup_customer")
                    .setArguments(Map.of("customer_id", "42")));
            adapter.onToolEnd("tool-run", new Agento11yGoogleAdkAdapter.ToolEndEvent().setResult(Map.of("status", "ok")));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-stream")
                    .setModelName("claude-sonnet-4-5")
                    .setStream(true)
                    .addPrompt("stream me"));
            adapter.onRunToken("run-stream", "hello");
            adapter.onRunToken("run-stream", " world");
            adapter.onRunEnd("run-stream", new Agento11yGoogleAdkAdapter.RunEndEvent().setResponseModel("claude-sonnet-4-5"));
        } finally {
            client.shutdown();
        }
    }

    @Test
    void adapterUsesExplicitProviderWhenConfigured() {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setProvider("gemini")
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-provider")
                    .setModelName("gpt-5"));
            adapter.onRunEnd("run-provider", new Agento11yGoogleAdkAdapter.RunEndEvent());

            GenerationRecorder rec = client.startGeneration(new GenerationStart()
                    .setMode(GenerationMode.SYNC)
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));
            rec.end();

            client.startToolExecution(new ToolExecutionStart().setToolName("noop")).end();
        } finally {
            client.shutdown();
        }
    }

    @Test
    void syncRunExportsFrameworkPayloadTagsAndMetrics() {
        try (FrameworkConformanceEnv env = new FrameworkConformanceEnv()) {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(env.client, new Agento11yGoogleAdkAdapter.Options()
                    .setAgentName("adk-agent")
                    .setAgentVersion("1.0.0")
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true)
                    .putExtraTag("team", "infra")
                    .putExtraMetadata("workspace", "sigil"));

            var parentSpan = env.tracerProvider.get("agento11y-framework-test")
                    .spanBuilder("framework.request")
                    .setAttribute(AttributeKey.stringKey("agento11y.framework.name"), "google-adk")
                    .setAttribute(AttributeKey.stringKey("agento11y.framework.source"), "handler")
                    .setAttribute(AttributeKey.stringKey("agento11y.framework.language"), "java")
                    .startSpan();
            try (Scope ignored = parentSpan.makeCurrent()) {
                adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                        .setRunId("run-sync")
                        .setSessionId("session-42")
                        .setThreadId("thread-9")
                        .setParentRunId("framework-parent-run")
                        .setComponentName("planner")
                        .setRunType("chat")
                        .setRetryAttempt(2)
                        .setEventId("event-42")
                        .setModelName("gpt-5")
                        .addTag("prod")
                        .addTag("framework")
                        .addPrompt("hello")
                        .putMetadata("phase", "plan"));
                adapter.onRunEnd("run-sync", new Agento11yGoogleAdkAdapter.RunEndEvent()
                        .setResponseModel("gpt-5")
                        .setStopReason("stop")
                        .setUsage(new TokenUsage().setInputTokens(3).setOutputTokens(2).setTotalTokens(5))
                        .addOutputMessage(new Message()
                                .setRole(MessageRole.ASSISTANT)
                                .setParts(List.of(MessagePart.text("hi")))));
            } finally {
                parentSpan.end();
            }

            env.client.flush();

            Generation generation = env.exporter.singleGeneration();
            SpanData generationSpan = env.latestGenerationSpan();

            assertThat(generation.getMode()).isEqualTo(GenerationMode.SYNC);
            assertThat(generation.getOperationName()).isEqualTo("generateText");
            assertThat(generation.getConversationId()).isEqualTo("session-42");
            assertThat(generation.getResponseModel()).isEqualTo("gpt-5");
            assertThat(generation.getTraceId()).isEqualTo(generationSpan.getTraceId());
            assertThat(generation.getSpanId()).isEqualTo(generationSpan.getSpanId());
            assertThat(generation.getTags())
                    .containsEntry("agento11y.framework.name", "google-adk")
                    .containsEntry("agento11y.framework.source", "handler")
                    .containsEntry("agento11y.framework.language", "java")
                    .containsEntry("team", "infra");
            assertThat(generation.getMetadata())
                    .containsEntry("workspace", "sigil")
                    .containsEntry("phase", "plan")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_RUN_ID, "run-sync")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_RUN_TYPE, "chat")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_THREAD_ID, "thread-9")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_PARENT_RUN_ID, "framework-parent-run")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_COMPONENT_NAME, "planner")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_RETRY_ATTEMPT, 2)
                    .containsEntry(Agento11yGoogleAdkAdapter.META_EVENT_ID, "event-42")
                    .containsEntry(Agento11yGoogleAdkAdapter.META_TAGS, List.of("prod", "framework"));
            assertThat(generation.getOutput()).hasSize(1);
            assertThat(generation.getOutput().get(0).getParts()).hasSize(1);
            assertThat(generation.getOutput().get(0).getParts().get(0).getText()).isEqualTo("hi");
            assertThat(generationSpan.getParentSpanId()).isEqualTo(parentSpan.getSpanContext().getSpanId());
            assertThat(env.metricNames())
                    .contains("gen_ai.client.operation.duration")
                    .doesNotContain("gen_ai.client.time_to_first_token");
        }
    }

    @Test
    void streamRunExportsStitchedOutputAndTtftMetric() {
        try (FrameworkConformanceEnv env = new FrameworkConformanceEnv()) {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(env.client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-stream-export")
                    .setSessionId("session-stream")
                    .setModelName("claude-sonnet-4-5")
                    .setStream(true)
                    .addPrompt("stream me"));
            adapter.onRunToken("run-stream-export", "hello");
            adapter.onRunToken("run-stream-export", " world");
            adapter.onRunEnd("run-stream-export", new Agento11yGoogleAdkAdapter.RunEndEvent()
                    .setResponseModel("claude-sonnet-4-5"));

            env.client.flush();

            Generation generation = env.exporter.singleGeneration();
            assertThat(generation.getMode()).isEqualTo(GenerationMode.STREAM);
            assertThat(generation.getOperationName()).isEqualTo("streamText");
            assertThat(generation.getResponseModel()).isEqualTo("claude-sonnet-4-5");
            assertThat(generation.getOutput()).hasSize(1);
            assertThat(generation.getOutput().get(0).getParts()).hasSize(1);
            assertThat(generation.getOutput().get(0).getParts().get(0).getText()).isEqualTo("hello world");
            assertThat(generation.getTags())
                    .containsEntry("agento11y.framework.name", "google-adk")
                    .containsEntry("agento11y.framework.source", "handler")
                    .containsEntry("agento11y.framework.language", "java");
            assertThat(env.metricNames())
                    .contains("gen_ai.client.operation.duration", "gen_ai.client.time_to_first_token");
        }
    }

    @Test
    void generationSpanTracksActiveParentSpanAndPreservesExportLineage() {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();
        Agento11yClient client = new Agento11yClient(
                new Agento11yClientConfig()
                        .setTracer(tracerProvider.get("agento11y-framework-test"))
                        .setGenerationExport(
                                new GenerationExportConfig()
                                        .setProtocol(GenerationExportProtocol.NONE)));
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            var parentSpan = tracerProvider.get("agento11y-framework-test").spanBuilder("framework.request").startSpan();
            try (Scope ignored = parentSpan.makeCurrent()) {
                adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                        .setRunId("run-lineage")
                        .setSessionId("session-lineage-42")
                        .setParentRunId("framework-parent-run")
                        .setRunType("chat")
                        .setModelName("gpt-5")
                        .addPrompt("hello"));
                adapter.onRunEnd("run-lineage", new Agento11yGoogleAdkAdapter.RunEndEvent()
                        .setResponseModel("gpt-5")
                        .setStopReason("stop"));
            } finally {
                parentSpan.end();
            }

            assertThat(client.debugSnapshot().getGenerations()).hasSize(1);
            var generation = client.debugSnapshot().getGenerations().get(0);
            var generationSpan = spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> "generateText".equals(span.getAttributes().get(io.opentelemetry.api.common.AttributeKey.stringKey("gen_ai.operation.name"))))
                    .findFirst()
                    .orElseThrow();

            assertThat(generationSpan.getParentSpanId()).isEqualTo(parentSpan.getSpanContext().getSpanId());
            assertThat(generationSpan.getTraceId()).isEqualTo(parentSpan.getSpanContext().getTraceId());
            assertThat(generation.getTraceId()).isEqualTo(generationSpan.getTraceId());
            assertThat(generation.getSpanId()).isEqualTo(generationSpan.getSpanId());
        } finally {
            client.shutdown();
            tracerProvider.close();
        }
    }

    @Test
    void adapterExplicitlyHasNoEmbeddingLifecycle() {
        List<String> publicMethodNames = Arrays.stream(Agento11yGoogleAdkAdapter.class.getMethods())
                .map(Method::getName)
                .toList();

        assertThat(publicMethodNames)
                .doesNotContain("onEmbeddingStart")
                .doesNotContain("onEmbeddingEnd")
                .doesNotContain("onEmbeddingError");
    }

    @Test
    void onRunEndDropsOutputsWhenCaptureOutputsDisabled() {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(true)
                    .setCaptureOutputs(false));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-no-output")
                    .setSessionId("session-42")
                    .setModelName("gpt-5")
                    .addPrompt("hello"));
            adapter.onRunEnd("run-no-output", new Agento11yGoogleAdkAdapter.RunEndEvent()
                    .addOutputMessage(new Message()
                            .setRole(MessageRole.ASSISTANT)
                            .setParts(List.of(MessagePart.text("should-not-export")))));

            assertThat(client.debugSnapshot().getGenerations()).hasSize(1);
            assertThat(client.debugSnapshot().getGenerations().get(0).getOutput()).isEmpty();
        } finally {
            client.shutdown();
        }
    }

    @Test
    void onToolEndDropsArgumentsWhenCaptureInputsDisabled() {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(false)
                    .setCaptureOutputs(true));

            adapter.onToolStart(new Agento11yGoogleAdkAdapter.ToolStartEvent()
                    .setRunId("tool-no-input")
                    .setSessionId("session-42")
                    .setToolName("lookup_customer")
                    .setArguments(Map.of("customer_id", "42")));
            adapter.onToolEnd("tool-no-input", new Agento11yGoogleAdkAdapter.ToolEndEvent()
                    .setResult(Map.of("status", "ok")));

            assertThat(client.debugSnapshot().getToolExecutions()).hasSize(1);
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getArguments()).isNull();
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getResult()).isEqualTo(Map.of("status", "ok"));
        } finally {
            client.shutdown();
        }
    }

    @Test
    void onRunTokenConcurrentCallbacksPreserveChunkedOutput() throws InterruptedException {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(client, new Agento11yGoogleAdkAdapter.Options()
                    .setCaptureInputs(true)
                    .setCaptureOutputs(true));

            adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-token-concurrent")
                    .setSessionId("session-42")
                    .setModelName("gpt-5")
                    .setStream(true)
                    .addPrompt("hello"));

            int workers = 8;
            int tokensPerWorker = 250;
            CountDownLatch startGate = new CountDownLatch(1);
            CountDownLatch done = new CountDownLatch(workers);
            Runnable emitTokens = () -> {
                try {
                    startGate.await(5, TimeUnit.SECONDS);
                    for (int i = 0; i < tokensPerWorker; i++) {
                        adapter.onRunToken("run-token-concurrent", "x");
                    }
                } catch (InterruptedException exception) {
                    Thread.currentThread().interrupt();
                } finally {
                    done.countDown();
                }
            };

            for (int i = 0; i < workers; i++) {
                new Thread(emitTokens).start();
            }
            startGate.countDown();
            assertThat(done.await(5, TimeUnit.SECONDS)).isTrue();

            adapter.onRunEnd("run-token-concurrent", new Agento11yGoogleAdkAdapter.RunEndEvent());
            assertThat(client.debugSnapshot().getGenerations()).hasSize(1);
            assertThat(client.debugSnapshot().getGenerations().get(0).getOutput()).hasSize(1);
            String output = client.debugSnapshot().getGenerations().get(0).getOutput()
                    .get(0).getParts().get(0).getText();
            assertThat(output).hasSize(workers * tokensPerWorker);
        } finally {
            client.shutdown();
        }
    }

    @Test
    void onRunStartDeduplicatesConcurrentCallbacks() throws InterruptedException {
        Agento11yClient client = newClient();
        try {
            AtomicInteger starts = new AtomicInteger(0);
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(
                    client,
                    new Agento11yGoogleAdkAdapter.Options(),
                    (start, stream) -> {
                        starts.incrementAndGet();
                        return stream ? client.startStreamingGeneration(start) : client.startGeneration(start);
                    },
                    client::startToolExecution);

            CountDownLatch startGate = new CountDownLatch(1);
            CountDownLatch done = new CountDownLatch(2);
            Runnable callback = () -> {
                try {
                    startGate.await(5, TimeUnit.SECONDS);
                    adapter.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                            .setRunId("run-concurrent")
                            .setModelName("gpt-5"));
                } catch (InterruptedException exception) {
                    Thread.currentThread().interrupt();
                } finally {
                    done.countDown();
                }
            };

            Thread first = new Thread(callback);
            Thread second = new Thread(callback);
            first.start();
            second.start();
            startGate.countDown();
            assertThat(done.await(5, TimeUnit.SECONDS)).isTrue();

            assertThat(starts.get()).isEqualTo(1);
            adapter.onRunEnd("run-concurrent", new Agento11yGoogleAdkAdapter.RunEndEvent());
        } finally {
            client.shutdown();
        }
    }

    @Test
    void onToolStartDeduplicatesConcurrentCallbacks() throws InterruptedException {
        Agento11yClient client = newClient();
        try {
            AtomicInteger starts = new AtomicInteger(0);
            Agento11yGoogleAdkAdapter adapter = new Agento11yGoogleAdkAdapter(
                    client,
                    new Agento11yGoogleAdkAdapter.Options(),
                    (start, stream) -> stream ? client.startStreamingGeneration(start) : client.startGeneration(start),
                    start -> {
                        starts.incrementAndGet();
                        return client.startToolExecution(start);
                    });

            CountDownLatch startGate = new CountDownLatch(1);
            CountDownLatch done = new CountDownLatch(2);
            Runnable callback = () -> {
                try {
                    startGate.await(5, TimeUnit.SECONDS);
                    adapter.onToolStart(new Agento11yGoogleAdkAdapter.ToolStartEvent()
                            .setRunId("tool-concurrent")
                            .setSessionId("session-42")
                            .setToolName("lookup_customer"));
                } catch (InterruptedException exception) {
                    Thread.currentThread().interrupt();
                } finally {
                    done.countDown();
                }
            };

            Thread first = new Thread(callback);
            Thread second = new Thread(callback);
            first.start();
            second.start();
            startGate.countDown();
            assertThat(done.await(5, TimeUnit.SECONDS)).isTrue();

            assertThat(starts.get()).isEqualTo(1);
            adapter.onToolEnd("tool-concurrent", new Agento11yGoogleAdkAdapter.ToolEndEvent());
        } finally {
            client.shutdown();
        }
    }

    @Test
    void createCallbacksProvidesOneTimeLifecycleWiring() {
        Agento11yClient client = newClient();
        try {
            Agento11yGoogleAdkAdapter.Callbacks callbacks = Agento11yGoogleAdkAdapter.createCallbacks(
                    client,
                    new Agento11yGoogleAdkAdapter.Options()
                            .setAgentName("adk-agent")
                            .setCaptureInputs(true)
                            .setCaptureOutputs(true));

            callbacks.onRunStart(new Agento11yGoogleAdkAdapter.RunStartEvent()
                    .setRunId("run-callbacks")
                    .setSessionId("session-callbacks")
                    .setModelName("gpt-5")
                    .addPrompt("hello"));
            callbacks.onRunToken("run-callbacks", "hi");
            callbacks.onRunEnd("run-callbacks", new Agento11yGoogleAdkAdapter.RunEndEvent()
                    .setResponseModel("gpt-5")
                    .setStopReason("stop"));

            callbacks.onToolStart(new Agento11yGoogleAdkAdapter.ToolStartEvent()
                    .setRunId("tool-callbacks")
                    .setSessionId("session-callbacks")
                    .setToolName("lookup_customer")
                    .setArguments(Map.of("id", "42")));
            callbacks.onToolEnd("tool-callbacks", new Agento11yGoogleAdkAdapter.ToolEndEvent()
                    .setResult(Map.of("status", "ok")));

            assertThat(client.debugSnapshot().getGenerations()).hasSize(1);
            assertThat(client.debugSnapshot().getToolExecutions()).hasSize(1);
        } finally {
            client.shutdown();
        }
    }

    private static final class FrameworkConformanceEnv implements AutoCloseable {
        private final CapturingExporter exporter = new CapturingExporter();
        private final InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        private final SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();
        private final InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        private final SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        private final Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(tracerProvider.get("agento11y-framework-test"))
                .setMeter(meterProvider.get("agento11y-framework-test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setQueueSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(0)));

        private List<String> metricNames() {
            return metricReader.collectAllMetrics().stream()
                    .map(MetricData::getName)
                    .toList();
        }

        private SpanData latestGenerationSpan() {
            return spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> {
                        String operation = span.getAttributes().get(AttributeKey.stringKey("gen_ai.operation.name"));
                        return "generateText".equals(operation) || "streamText".equals(operation);
                    })
                    .reduce((first, second) -> second)
                    .orElseThrow();
        }

        @Override
        public void close() {
            client.shutdown();
            tracerProvider.close();
            meterProvider.close();
        }
    }

    private static final class CapturingExporter implements GenerationExporter {
        private final List<Generation> generations = new ArrayList<>();

        @Override
        public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
            List<ExportGenerationResult> results = new ArrayList<>();
            for (Generation generation : request.getGenerations()) {
                generations.add(generation.copy());
                results.add(new ExportGenerationResult().setGenerationId(generation.getId()).setAccepted(true));
            }
            return new ExportGenerationsResponse().setResults(results);
        }

        private Generation singleGeneration() {
            assertThat(generations).hasSize(1);
            return generations.get(0);
        }
    }
}
