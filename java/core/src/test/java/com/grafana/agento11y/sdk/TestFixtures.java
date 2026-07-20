package com.grafana.agento11y.sdk;

import io.opentelemetry.api.GlobalOpenTelemetry;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;

final class TestFixtures {
    private TestFixtures() {
    }

    static GenerationStart startFixture() {
        GenerationStart start = new GenerationStart()
                .setId("gen-fixture-1")
                .setConversationId("conv-fixture-1")
                .setAgentName("agent-fixture")
                .setAgentVersion("v1.2.3")
                .setMode(GenerationMode.STREAM)
                .setOperationName("streamText")
                .setModel(new ModelRef().setProvider("anthropic").setName("claude-sonnet-4-5"))
                .setMaxTokens(1024L)
                .setTemperature(0.7)
                .setTopP(0.9)
                .setToolChoice("auto")
                .setThinkingEnabled(true)
                .setSystemPrompt("be concise")
                .setStartedAt(Instant.parse("2026-02-11T12:00:00Z"));
        start.getMetadata().put("agento11y.gen_ai.request.thinking.budget_tokens", 4096L);
        return start;
    }

    static GenerationResult resultFixture() {
        GenerationResult result = new GenerationResult()
                .setId("gen-fixture-1")
                .setConversationId("conv-fixture-1")
                .setAgentName("agent-fixture")
                .setAgentVersion("v1.2.3")
                .setMode(GenerationMode.STREAM)
                .setOperationName("streamText")
                .setModel(new ModelRef().setProvider("anthropic").setName("claude-sonnet-4-5"))
                .setResponseId("resp-fixture")
                .setResponseModel("claude-sonnet-4-5-20260201")
                .setSystemPrompt("be concise")
                .setMaxTokens(256L)
                .setTemperature(0.25)
                .setTopP(0.85)
                .setToolChoice("required")
                .setThinkingEnabled(false)
                .setUsage(new TokenUsage().setInputTokens(120).setOutputTokens(80))
                .setStopReason("stop")
                .setCompletedAt(Instant.parse("2026-02-11T12:00:01Z"));
        result.getMetadata().put("agento11y.gen_ai.request.thinking.budget_tokens", 2048L);

        result.getInput().add(new Message()
                .setRole(MessageRole.USER)
                .setParts(List.of(MessagePart.text("hello"))));

        result.getOutput().add(new Message()
                .setRole(MessageRole.ASSISTANT)
                .setParts(List.of(
                        MessagePart.thinking("think"),
                        MessagePart.toolCall(new ToolCall().setId("tool-call-1").setName("weather").setInputJson("{\"city\":\"Paris\"}".getBytes())))));

        result.getOutput().add(new Message()
                .setRole(MessageRole.TOOL)
                .setParts(List.of(
                        MessagePart.toolResult(new ToolResultPart()
                                .setToolCallId("tool-call-1")
                                .setName("weather")
                                .setContent("18C")
                                .setContentJson("{\"temp_c\":18}".getBytes())))));

        result.getTools().add(new ToolDefinition()
                .setName("weather")
                .setDescription("Get weather")
                .setType("function")
                .setInputSchemaJson("{\"type\":\"object\"}".getBytes()));

        return result;
    }

    static Agento11yClient newClient(CapturingExporter exporter) {
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.HTTP)
                        .setBatchSize(1)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));
        return new Agento11yClient(config);
    }

    static void waitFor(BooleanSupplier check, Duration timeout) throws InterruptedException {
        long deadline = System.nanoTime() + timeout.toNanos();
        while (System.nanoTime() < deadline) {
            if (check.getAsBoolean()) {
                return;
            }
            Thread.sleep(10);
        }
        throw new AssertionError("timed out waiting for condition");
    }

    @FunctionalInterface
    interface BooleanSupplier {
        boolean getAsBoolean();
    }

    static final class CapturingExporter implements GenerationExporter {
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

        List<List<Generation>> getRequests() {
            return requests;
        }
    }
}
