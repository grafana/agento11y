package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.context.Scope;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicInteger;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.EnumSource;
import org.junit.jupiter.params.provider.NullSource;
import org.junit.jupiter.params.provider.ValueSource;

class Agento11yClientRuntimeTest {
    @Test
    void exportsByBatchSizeAndFlushes() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(2));
            assertThat(exporter.getRequests()).hasSize(1);
            assertThat(exporter.getRequests().get(0)).hasSize(1);
            assertThat(exporter.getRequests().get(0).get(0).getMode()).isEqualTo(GenerationMode.STREAM);
        }
    }

    @Test
    void shutdownFlushesPendingBatch() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        Agento11yClient client = new Agento11yClient(config);
        GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
        recorder.setResult(TestFixtures.resultFixture());
        recorder.end();

        assertThat(exporter.getRequests()).isEmpty();
        client.shutdown();
        assertThat(exporter.getRequests()).hasSize(1);
    }

    @Test
    void flushesByIntervalWhenBatchNotReached() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMillis(20))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(2));
            assertThat(exporter.getRequests()).hasSize(1);
            assertThat(exporter.getRequests().get(0)).hasSize(1);
        }
    }

    @Test
    void retriesFailedExportAndEventuallySucceeds() {
        AtomicInteger attempts = new AtomicInteger();
        List<List<Generation>> successfulRequests = new CopyOnWriteArrayList<>();
        GenerationExporter flakyExporter = request -> {
            int attempt = attempts.incrementAndGet();
            if (attempt < 3) {
                throw new RuntimeException("temporary export failure");
            }
            List<Generation> copied = new ArrayList<>();
            for (Generation generation : request.getGenerations()) {
                copied.add(generation.copy());
            }
            successfulRequests.add(copied);

            List<ExportGenerationResult> results = new ArrayList<>();
            for (Generation generation : request.getGenerations()) {
                results.add(new ExportGenerationResult().setGenerationId(generation.getId()).setAccepted(true));
            }
            return new ExportGenerationsResponse().setResults(results);
        };

        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(flakyExporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(10)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(2)
                        .setInitialBackoff(Duration.ofMillis(1))
                        .setMaxBackoff(Duration.ofMillis(2)));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            client.flush();

            assertThat(recorder.error()).isEmpty();
            assertThat(attempts.get()).isEqualTo(3);
            assertThat(successfulRequests).hasSize(1);
            assertThat(successfulRequests.get(0)).hasSize(1);
        }
    }

    @Test
    void returnsQueueFullAsLocalError() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(100)
                        .setQueueSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder first = client.startGeneration(TestFixtures.startFixture());
            first.setResult(TestFixtures.resultFixture());
            first.end();
            assertThat(first.error()).isEmpty();

            GenerationRecorder second = client.startGeneration(TestFixtures.startFixture());
            second.setResult(TestFixtures.resultFixture().setId("gen-fixture-2"));
            second.end();
            assertThat(second.error()).isPresent();
            assertThat(second.error().orElseThrow()).isInstanceOf(QueueFullException.class);
        }
    }

    @Test
    void payloadMaxBytesGuardrailReturnsLocalError() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setQueueSize(10)
                        .setPayloadMaxBytes(128)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow()).isInstanceOf(EnqueueException.class);
        }
    }

    @Test
    void providerCallErrorDoesNotPopulateLocalRecorderError() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.setCallError(new RuntimeException("provider blew up"));
            recorder.end();

            assertThat(recorder.error()).isEmpty();
            assertThat(recorder.lastGeneration().orElseThrow().getCallError()).contains("provider blew up");
            assertThat(recorder.lastGeneration().orElseThrow().getMetadata().get("agento11y.sdk.name")).isEqualTo("sdk-java");
        }
    }

    @Test
    void sdkMetadataOverridesConflictingSeedAndResultValues() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationStart start = TestFixtures.startFixture();
            start.getMetadata().put("agento11y.sdk.name", "seed-value");

            GenerationResult result = TestFixtures.resultFixture();
            result.getMetadata().put("agento11y.sdk.name", "result-value");

            GenerationRecorder recorder = client.startGeneration(start);
            recorder.setResult(result);
            recorder.end();

            assertThat(recorder.error()).isEmpty();
            Generation generation = recorder.lastGeneration().orElseThrow();
            assertThat(generation.getMetadata().get("agento11y.sdk.name")).isEqualTo("sdk-java");
        }
    }

    @Test
    void contextDefaultsApplyAndExplicitFieldsOverride() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            try (Scope ignoredConversation = Agento11yContext.withConversationId("ctx-conv");
                    Scope ignoredAgent = Agento11yContext.withAgentName("ctx-agent");
                    Scope ignoredVersion = Agento11yContext.withAgentVersion("ctx-ver")) {
                GenerationStart start = TestFixtures.startFixture()
                        .setConversationId("")
                        .setAgentName("")
                        .setAgentVersion("");

                GenerationRecorder recorder = client.startGeneration(start);
                recorder.setResult(TestFixtures.resultFixture()
                        .setConversationId("")
                        .setAgentName("")
                        .setAgentVersion(""));
                recorder.end();

                Generation generation = recorder.lastGeneration().orElseThrow();
                assertThat(generation.getConversationId()).isEqualTo("ctx-conv");
                assertThat(generation.getAgentName()).isEqualTo("ctx-agent");
                assertThat(generation.getAgentVersion()).isEqualTo("ctx-ver");
            }

            GenerationRecorder explicit = client.startGeneration(TestFixtures.startFixture().setConversationId("explicit"));
            explicit.setResult(TestFixtures.resultFixture());
            explicit.end();
            assertThat(explicit.lastGeneration().orElseThrow().getConversationId()).isEqualTo("conv-fixture-1");
        }
    }

    @Test
    void embeddingRecorderDoesNotEnqueueGenerationAndProviderErrorIsNotLocalError() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            recorder.setCallError(new RuntimeException("embedding provider blew up"));
            recorder.end();

            assertThat(recorder.error()).isEmpty();
            assertThat(exporter.getRequests()).isEmpty();
        }
    }

    @Test
    void embeddingRecorderValidationErrorReturnsLocalError() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            EmbeddingRecorder recorder = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            recorder.setResult(new EmbeddingResult().setInputCount(-1));
            recorder.end();

            assertThat(recorder.error()).isPresent();
            assertThat(recorder.error().orElseThrow()).isInstanceOf(ValidationException.class);
            assertThat(exporter.getRequests()).isEmpty();
        }
    }

    @Test
    void recorderEndIsIdempotentForGenerationAndTool() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder generation = client.startGeneration(TestFixtures.startFixture());
            generation.setResult(TestFixtures.resultFixture());
            generation.end();
            generation.end();

            ToolExecutionRecorder tool = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("weather")
                    .setToolCallId("call-1"));
            tool.setResult(new ToolExecutionResult().setArguments(java.util.Map.of("city", "Paris")).setResult("18C"));
            tool.end();
            tool.end();

            EmbeddingRecorder embedding = client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small")));
            embedding.setResult(new EmbeddingResult().setInputCount(1));
            embedding.end();
            embedding.end();

            Agento11yDebugSnapshot snapshot = client.debugSnapshot();
            assertThat(snapshot.getGenerations()).hasSize(1);
            assertThat(snapshot.getToolExecutions()).hasSize(1);
        }
    }

    @ParameterizedTest
    @EnumSource(ToolExecutionKind.class)
    void toolExecutionKindSurvivesStartCompletionAndSnapshotCopies(ToolExecutionKind executionKind) {
        ToolExecutionStart start = new ToolExecutionStart()
                .setToolName("tool")
                .setExecutionKind(executionKind)
                .setSkillName("review")
                .setSkillDescription("Review changes")
                .setSkillSourceURI("file:///skills/review/SKILL.md")
                .setSkillResourceName("references/checklist.md")
                .setProcessExecutableName("git")
                .setProcessExecutablePath("/usr/bin/git");
        ToolExecutionStart startCopy = start.copy();
        assertThat(startCopy).isNotSameAs(start).usingRecursiveComparison().isEqualTo(start);

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            ToolExecutionRecorder recorder = client.startToolExecution(start);
            start.setExecutionKind(null)
                    .setSkillName("changed")
                    .setSkillDescription("changed")
                    .setSkillSourceURI("changed")
                    .setSkillResourceName("changed")
                    .setProcessExecutableName("changed")
                    .setProcessExecutablePath("changed");
            recorder.end();

            ToolExecution execution = client.debugSnapshot().getToolExecutions().get(0);
            assertThat(execution.getExecutionKind()).isEqualTo(executionKind);
            assertThat(execution.getSkillName()).isEqualTo(startCopy.getSkillName());
            assertThat(execution.getSkillDescription()).isEqualTo(startCopy.getSkillDescription());
            assertThat(execution.getSkillSourceURI()).isEqualTo(startCopy.getSkillSourceURI());
            assertThat(execution.getSkillResourceName()).isEqualTo(startCopy.getSkillResourceName());
            assertThat(execution.getProcessExecutableName()).isEqualTo(startCopy.getProcessExecutableName());
            assertThat(execution.getProcessExecutablePath()).isEqualTo(startCopy.getProcessExecutablePath());
            assertThat(execution.getProcessExitCode()).isNull();

            ToolExecution copy = execution.copy();
            assertThat(copy).isNotSameAs(execution).usingRecursiveComparison().isEqualTo(execution);
            execution.setExecutionKind(null)
                    .setSkillName("changed")
                    .setSkillDescription("changed")
                    .setSkillSourceURI("changed")
                    .setSkillResourceName("changed")
                    .setProcessExecutableName("changed")
                    .setProcessExecutablePath("changed");
            assertThat(client.debugSnapshot().getToolExecutions().get(0))
                    .isNotSameAs(execution).usingRecursiveComparison().isEqualTo(copy);
        }
    }

    @ParameterizedTest
    @NullSource
    @ValueSource(longs = {0L, 17L, -1L})
    void toolExitCodeSurvivesResultCompletionAndSnapshotCopies(Long exitCode) {
        ToolExecutionResult result = new ToolExecutionResult().setProcessExitCode(exitCode);
        ToolExecutionResult resultCopy = result.copy();
        assertThat(resultCopy).isNotSameAs(result).usingRecursiveComparison().isEqualTo(result);

        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName("command"));
            recorder.setResult(result);
            result.setProcessExitCode(99L);
            recorder.end();

            ToolExecution execution = client.debugSnapshot().getToolExecutions().get(0);
            assertThat(execution.getProcessExitCode()).isEqualTo(exitCode);
            ToolExecution copy = execution.copy();
            assertThat(copy).isNotSameAs(execution).usingRecursiveComparison().isEqualTo(execution);
            execution.setProcessExitCode(99L);
            assertThat(copy.getProcessExitCode()).isEqualTo(exitCode);
            assertThat(client.debugSnapshot().getToolExecutions().get(0).getProcessExitCode()).isEqualTo(exitCode);
            assertThat(recorder.error()).isEmpty();
        }
    }

    @Test
    void startAndFlushRejectAfterShutdown() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClient client = TestFixtures.newClient(exporter);
        client.shutdown();

        assertThatThrownBy(() -> client.startGeneration(TestFixtures.startFixture()))
                .isInstanceOf(ClientShutdownException.class);
        assertThatThrownBy(client::flush)
                .isInstanceOf(ClientShutdownException.class);
    }

    @Test
    void emptyToolNameReturnsNoopRecorder() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName("   "));
            recorder.setResult(new ToolExecutionResult().setArguments("x"));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            assertThat(client.debugSnapshot().getToolExecutions()).isEmpty();
        }
    }
}
