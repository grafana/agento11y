package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import com.fasterxml.jackson.databind.JsonNode;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.Map;
import org.junit.jupiter.api.Test;

class ToolLoopTest {

    @Test
    void happyPathTwoTools() throws Exception {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));

        try (Agento11yClient client = new Agento11yClient(config)) {
            List<Message> messages = new ArrayList<>();
            Message m = new Message().setRole(MessageRole.ASSISTANT);
            m.getParts()
                    .add(MessagePart.toolCall(
                            new ToolCall().setId("c1").setName("weather").setInputJson(
                                    "{\"city\":\"Paris\"}".getBytes(StandardCharsets.UTF_8))));
            m.getParts()
                    .add(MessagePart.toolCall(
                            new ToolCall().setId("c2").setName("math").setInputJson(
                                    "{\"a\":1,\"b\":2}".getBytes(StandardCharsets.UTF_8))));
            messages.add(m);

            ExecuteToolCallsOptions opts = new ExecuteToolCallsOptions()
                    .setConversationId("conv-loop")
                    .setAgentName("agent-x")
                    .setAgentVersion("1.0.0")
                    .setRequestModel("gpt-test")
                    .setRequestProvider("openai");

            List<Message> out = client.executeToolCalls(
                    messages,
                    (name, bytes) -> {
                        if ("weather".equals(name)) {
                            return Map.of("temp_c", 18);
                        }
                        try {
                            return Json.MAPPER.readValue(bytes, Object.class);
                        } catch (Exception e) {
                            throw new RuntimeException(e);
                        }
                    },
                    opts);

            assertThat(out).hasSize(2);
            assertThat(out.get(0).getRole()).isEqualTo(MessageRole.TOOL);
            assertThat(out.get(0).getName()).isEqualTo("weather");
            ToolResultPart tr0 = out.get(0).getParts().get(0).getToolResult();
            assertThat(tr0.getToolCallId()).isEqualTo("c1");
            JsonNode node = Json.MAPPER.readTree(tr0.getContentJson());
            assertThat(node.get("temp_c").asInt()).isEqualTo(18);
        }

        long weatherSpans =
                spanExporter.getFinishedSpanItems().stream().filter(s -> s.getName().contains("weather")).count();
        long mathSpans =
                spanExporter.getFinishedSpanItems().stream().filter(s -> s.getName().contains("math")).count();
        assertThat(weatherSpans).isEqualTo(1);
        assertThat(mathSpans).isEqualTo(1);

        provider.shutdown();
    }

    @Test
    void convenienceLoopNeverInfersExecutionKindOrExitCode() {
        InMemorySpanExporter spans = InMemorySpanExporter.create();
        try (SdkTracerProvider provider = SdkTracerProvider.builder()
                    .addSpanProcessor(SimpleSpanProcessor.create(spans)).build();
                Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                        .setTracer(provider.get("test"))
                        .setGenerationExporter(new TestFixtures.CapturingExporter()))) {
            List<String> names = List.of("load_skill", "read_skill_resource", "run_skill_script", "exec_command");
            for (String name : names) {
                Message message = new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(
                        MessagePart.toolCall(new ToolCall().setId(name).setName(name).setInputJson(("""
                                {"execution_kind":"command","skill_name":"review","skill_resource_name":"check.py",
                                 "process_executable_name":"python3","process_exit_code":0}
                                """).getBytes(StandardCharsets.UTF_8)))));
                client.executeToolCalls(List.of(message), (tool, bytes) -> Map.of("process_exit_code", 0), null);
            }
            assertThat(spans.getFinishedSpanItems()).hasSize(names.size());
            for (int i = 0; i < names.size(); i++) {
                SpanData span = spans.getFinishedSpanItems().get(i);
                assertThat(span.getName()).isEqualTo("execute_tool " + names.get(i));
                assertThat(span.getAttributes().asMap().keySet()).extracting(AttributeKey::getKey)
                        .noneMatch(key -> key.startsWith("gen_ai.skill.") || key.startsWith("process.")
                                || key.equals("agento11y.skill.name"));
                ToolExecution execution = client.debugSnapshot().getToolExecutions().get(i);
                assertThat(execution.getExecutionKind()).isNull();
                assertThat(execution.getProcessExitCode()).isNull();
            }
        }
    }

    @Test
    void executorErrorMarksSpanError() throws Exception {
        InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        SdkTracerProvider provider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(provider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));

        try (Agento11yClient client = new Agento11yClient(config)) {
            Message m = new Message().setRole(MessageRole.ASSISTANT);
            m.getParts()
                    .add(MessagePart.toolCall(
                            new ToolCall().setId("c1").setName("boom").setInputJson("{}".getBytes(StandardCharsets.UTF_8))));

            List<Message> out = client.executeToolCalls(
                    List.of(m), (n, b) -> { throw new IllegalStateException("tool failed"); }, new ExecuteToolCallsOptions());

            assertThat(out).hasSize(1);
            assertThat(out.get(0).getParts().get(0).getToolResult().isError()).isTrue();
            assertThat(out.get(0).getParts().get(0).getToolResult().getContent()).contains("tool failed");
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getExecutionKind()).isNull();
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getProcessExitCode()).isNull();
        }

        SpanData boom = spanExporter.getFinishedSpanItems().stream()
                .filter(s -> s.getName().contains("boom"))
                .findFirst()
                .orElseThrow();
        assertThat(boom.getStatus().getStatusCode()).isEqualTo(StatusCode.ERROR);

        provider.shutdown();
    }

    @Test
    void noToolParts() throws Exception {
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));
        try (Agento11yClient client = new Agento11yClient(config)) {
            Message m = new Message().setRole(MessageRole.ASSISTANT);
            m.getParts().add(MessagePart.text("hi"));
            assertThat(client.executeToolCalls(List.of(m), (a, b) -> null, null)).isEmpty();
        }
        tracerProvider.shutdown();
    }

    @Test
    void nullMessages() throws Exception {
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));
        try (Agento11yClient client = new Agento11yClient(config)) {
            assertThat(client.executeToolCalls(null, (a, b) -> null, null)).isEmpty();
        }
        tracerProvider.shutdown();
    }

    @Test
    void skipsBlankToolName() throws Exception {
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));
        try (Agento11yClient client = new Agento11yClient(config)) {
            Message m = new Message().setRole(MessageRole.ASSISTANT);
            m.getParts()
                    .add(MessagePart.toolCall(
                            new ToolCall().setId("x").setName("   ").setInputJson("{}".getBytes(StandardCharsets.UTF_8))));
            assertThat(client.executeToolCalls(List.of(m), (a, b) -> 1, null)).isEmpty();
        }
        tracerProvider.shutdown();
    }

    @Test
    void nullExecutorThrows() throws Exception {
        SdkTracerProvider tracerProvider = SdkTracerProvider.builder().build();
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1));
        try (Agento11yClient client = new Agento11yClient(config)) {
            org.junit.jupiter.api.Assertions.assertThrows(
                    IllegalArgumentException.class, () -> client.executeToolCalls(List.of(), null, null));
        }
        tracerProvider.shutdown();
    }
}
