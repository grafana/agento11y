package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import org.junit.jupiter.api.Test;

class Agento11yClientValidationTest {
    @Test
    void executionKindAndSkillProcessFieldsDefaultToAbsentAndNormalizeNullStrings() {
        ToolExecutionStart start = new ToolExecutionStart();
        ToolExecution execution = new ToolExecution();
        assertThat(start.getExecutionKind()).isNull();
        assertThat(execution.getExecutionKind()).isNull();
        assertThat(execution.getProcessExitCode()).isNull();
        assertThat(new ToolExecutionResult().getProcessExitCode()).isNull();
        assertThat(start.getSkillName()).isEmpty();
        assertThat(start.getSkillDescription()).isEmpty();
        assertThat(start.getSkillSourceURI()).isEmpty();
        assertThat(start.getSkillResourceName()).isEmpty();
        assertThat(start.getProcessExecutableName()).isEmpty();
        assertThat(start.getProcessExecutablePath()).isEmpty();
        assertThat(execution.getSkillName()).isEmpty();
        assertThat(execution.getSkillDescription()).isEmpty();
        assertThat(execution.getSkillSourceURI()).isEmpty();
        assertThat(execution.getSkillResourceName()).isEmpty();
        assertThat(execution.getProcessExecutableName()).isEmpty();
        assertThat(execution.getProcessExecutablePath()).isEmpty();

        ToolExecutionStart nullStart = new ToolExecutionStart()
                .setExecutionKind(ToolExecutionKind.LOAD_SKILL).setExecutionKind(null)
                .setSkillName(null)
                .setSkillDescription(null)
                .setSkillSourceURI(null)
                .setSkillResourceName(null)
                .setProcessExecutableName(null)
                .setProcessExecutablePath(null);
        assertThat(nullStart.copy()).usingRecursiveComparison().isEqualTo(start);
        ToolExecution nullExecution = new ToolExecution()
                .setExecutionKind(ToolExecutionKind.COMMAND).setExecutionKind(null)
                .setSkillName(null)
                .setSkillDescription(null)
                .setSkillSourceURI(null)
                .setSkillResourceName(null)
                .setProcessExecutableName(null)
                .setProcessExecutablePath(null)
                .setProcessExitCode(0L).setProcessExitCode(null);
        assertThat(nullExecution.copy()).usingRecursiveComparison().isEqualTo(execution);
        assertThat(new ToolExecutionResult().setProcessExitCode(0L).setProcessExitCode(null).copy().getProcessExitCode())
                .isNull();
    }

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
