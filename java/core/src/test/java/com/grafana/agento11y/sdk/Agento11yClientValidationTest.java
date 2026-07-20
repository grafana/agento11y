package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import org.junit.jupiter.api.Test;

class Agento11yClientValidationTest {
    @Test
    void rejectsRolePartCompatibilityViolations() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationResult invalid = TestFixtures.resultFixture();
            invalid.getOutput().clear();
            invalid.getOutput().add(new Message()
                    .setRole(MessageRole.USER)
                    .setParts(java.util.List.of(MessagePart.thinking("bad"))));

            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(invalid);
            recorder.end();

            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow().getMessage())
                    .contains("generation.output[0].parts[0].thinking only allowed for assistant role");
        }
    }

    @Test
    void rejectsToolCallOutsideAssistantRole() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationResult invalid = TestFixtures.resultFixture();
            invalid.getOutput().clear();
            invalid.getOutput().add(new Message()
                    .setRole(MessageRole.USER)
                    .setParts(java.util.List.of(MessagePart.toolCall(new ToolCall().setName("weather")))));

            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(invalid);
            recorder.end();

            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow().getMessage())
                    .contains("generation.output[0].parts[0].tool_call only allowed for assistant role");
        }
    }

    @Test
    void rejectsToolResultOutsideToolRole() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationResult invalid = TestFixtures.resultFixture();
            invalid.getOutput().clear();
            invalid.getOutput().add(new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(java.util.List.of(MessagePart.toolResult(new ToolResultPart().setToolCallId("call-1")))));

            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(invalid);
            recorder.end();

            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow().getMessage())
                    .contains("generation.output[0].parts[0].tool_result only allowed for tool role");
        }
    }

    @Test
    void allowsConversationAndResponseFields() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationResult valid = new GenerationResult()
                    .setConversationId("conv-1")
                    .setModel(new ModelRef().setProvider("anthropic").setName("claude-sonnet-4-5"))
                    .setResponseId("resp-1")
                    .setResponseModel("claude-sonnet-4-5-20260201");
            valid.getInput().add(new Message()
                    .setRole(MessageRole.USER)
                    .setParts(java.util.List.of(MessagePart.text("hello"))));
            valid.getOutput().add(new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(java.util.List.of(MessagePart.text("hi"))));

            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(valid);
            recorder.end();

            assertThat(recorder.error()).isEmpty();
        }
    }

    @Test
    void rejectsArtifactWithoutPayloadOrRecordId() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationResult invalid = TestFixtures.resultFixture();
            invalid.getArtifacts().add(new Artifact().setKind(ArtifactKind.REQUEST));

            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(invalid);
            recorder.end();

            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow().getMessage())
                    .contains("generation.artifacts[0] must provide payload or record_id");
        }
    }
}
