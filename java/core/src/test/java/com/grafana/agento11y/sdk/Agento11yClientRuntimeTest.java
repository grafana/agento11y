package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import io.opentelemetry.api.GlobalOpenTelemetry;
import io.opentelemetry.context.Scope;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;
import java.util.concurrent.TimeUnit;
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
        try (Agento11yClient client = TestFixtures.newClient(exporter, 10)) {
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
        try (Agento11yClient client = TestFixtures.newClient(exporter, 10)) {
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
        try (Agento11yClient client = TestFixtures.newClient(exporter, 10)) {
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

    @ParameterizedTest
    @NullSource
    @ValueSource(ints = {0, -1})
    void debugRetentionIsDisabledUnlessExplicitlyEnabled(Integer capacity) {
        for (boolean debug : new boolean[] {false, true}) {
            CountingExporter exporter = new CountingExporter();
            Agento11yClientConfig config = runtimeConfig(exporter).setDebug(debug);
            if (capacity != null) {
                config.setDebugBufferSize(capacity);
            }
            try (Agento11yClient client = new Agento11yClient(config)) {
                // Disabled retention must not even attempt to copy the payload.
                client.recordGeneration(null);
                client.recordToolExecution(null);
                for (int i = 0; i < 5; i++) {
                    completeGeneration(client, i);
                    completeTool(client, i);
                }
                assertThat(client.debugSnapshot().getGenerations()).isEmpty();
                assertThat(client.debugSnapshot().getToolExecutions()).isEmpty();
                assertThat(client.debugSnapshot().getQueueSize()).isEqualTo(5);
                assertThat(exporter.generations.get()).isZero();
                client.flush();
                assertThat(exporter.generations.get()).isEqualTo(5);
                assertThat(client.debugSnapshot().getQueueSize()).isZero();
            }
        }
    }

    @ParameterizedTest
    @ValueSource(ints = {1, 3})
    void retainsNewestCompletionsPerTypeAcrossWrapsWithoutAffectingExport(int capacity) {
        CountingExporter exporter = new CountingExporter();
        Agento11yClientConfig config = runtimeConfig(exporter).setDebugBufferSize(capacity);
        try (Agento11yClient client = new Agento11yClient(config)) {
            config.setDebugBufferSize(0);
            List<String> generationIds = new ArrayList<>();
            List<String> toolIds = new ArrayList<>();
            for (int i = 0; i < 12; i++) {
                completeGeneration(client, i);
                generationIds.add("gen-" + i);
                Agento11yDebugSnapshot beforeTools = client.debugSnapshot();
                assertThat(beforeTools.getGenerations()).extracting(Generation::getId)
                        .containsExactlyElementsOf(generationIds.subList(Math.max(0, i + 1 - capacity), i + 1));
                assertThat(beforeTools.getToolExecutions()).hasSize(Math.min(2 * i, capacity));
                completeTool(client, i);
                completeTool(client, 100 + i);
                toolIds.add("call-" + i);
                toolIds.add("call-" + (100 + i));
                Agento11yDebugSnapshot snapshot = client.debugSnapshot();
                assertThat(snapshot.getGenerations()).usingRecursiveComparison()
                        .isEqualTo(beforeTools.getGenerations());
                assertThat(snapshot.getToolExecutions()).extracting(ToolExecution::getToolCallId)
                        .containsExactlyElementsOf(toolIds.subList(Math.max(0, toolIds.size() - capacity), toolIds.size()));
                for (Generation generation : snapshot.getGenerations()) {
                    int index = Integer.parseInt(generation.getId().substring(4));
                    assertThat(generation.getConversationId()).isEqualTo("conv-" + (index % 3));
                    assertThat(generation.getMode()).isEqualTo(index % 2 == 0 ? GenerationMode.SYNC : GenerationMode.STREAM);
                }
                for (ToolExecution tool : snapshot.getToolExecutions()) {
                    int index = Integer.parseInt(tool.getToolCallId().substring(5));
                    assertThat(tool.getConversationId()).isEqualTo("conv-" + (index % 3));
                    assertThat(tool.getExecutionKind()).isEqualTo(ToolExecutionKind.values()[index % ToolExecutionKind.values().length]);
                }
                assertThat(snapshot.getQueueSize()).isEqualTo(i + 1);
                assertThat(exporter.generations.get()).isZero();
            }
            Agento11yDebugSnapshot retained = client.debugSnapshot();
            client.flush();
            assertThat(exporter.generations.get()).isEqualTo(12);
            assertThat(exporter.requests.get()).isEqualTo(1);
            assertThat(client.debugSnapshot().getQueueSize()).isZero();
            assertThat(client.debugSnapshot().getGenerations()).usingRecursiveComparison().isEqualTo(retained.getGenerations());
            assertThat(client.debugSnapshot().getToolExecutions()).usingRecursiveComparison().isEqualTo(retained.getToolExecutions());
        }
    }

    @Test
    void shutdownKeepsHistoryReadableAndOutstandingRecordersRetainTheirCompletionOrder() {
        CountingExporter exporter = new CountingExporter();
        try (Agento11yClient client = new Agento11yClient(runtimeConfig(exporter).setDebugBufferSize(1))) {
            GenerationRecorder outstandingGeneration = client.startGeneration(TestFixtures.startFixture().setId("outstanding"));
            ToolExecutionRecorder outstandingTool = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("outstanding").setToolCallId("outstanding"));
            completeGeneration(client, 0);
            completeTool(client, 0);
            Agento11yDebugSnapshot before = client.debugSnapshot();
            client.shutdown();
            client.shutdown();
            assertThat(exporter.generations.get()).isEqualTo(1);
            assertThat(exporter.shutdowns.get()).isEqualTo(1);
            assertThat(client.debugSnapshot().getGenerations()).usingRecursiveComparison().isEqualTo(before.getGenerations());
            assertThat(client.debugSnapshot().getToolExecutions()).usingRecursiveComparison().isEqualTo(before.getToolExecutions());
            assertThat(client.debugSnapshot().getQueueSize()).isZero();

            outstandingGeneration.end();
            outstandingGeneration.end();
            outstandingTool.end();
            outstandingTool.end();
            assertThat(outstandingGeneration.error().orElseThrow()).isInstanceOf(ClientShutdownException.class);
            assertThat(outstandingTool.error()).isEmpty();
            assertThat(client.debugSnapshot().getGenerations()).extracting(Generation::getId).containsExactly("outstanding");
            assertThat(client.debugSnapshot().getToolExecutions()).extracting(ToolExecution::getToolCallId).containsExactly("outstanding");
            assertThat(client.debugSnapshot().getQueueSize()).isZero();
            assertThat(exporter.generations.get()).isEqualTo(1);
            assertThat(before.getGenerations()).extracting(Generation::getId).containsExactly("gen-0");
            assertThat(before.getToolExecutions()).extracting(ToolExecution::getToolCallId).containsExactly("call-0");
        }
    }

    @Test
    void debugHistoryIncludesValidationAndEnqueueFailuresButNotEmbeddingsOrNoopTools() {
        CountingExporter exporter = new CountingExporter();
        Agento11yClientConfig config = runtimeConfig(exporter).setDebugBufferSize(3);
        config.getGenerationExport().setQueueSize(1);
        try (Agento11yClient client = new Agento11yClient(config)) {
            completeGeneration(client, 0);
            GenerationRecorder invalid = client.startGeneration(new GenerationStart().setId("invalid"));
            invalid.end();
            assertThat(invalid.error().orElseThrow()).isInstanceOf(ValidationException.class);
            GenerationRecorder full = client.startGeneration(TestFixtures.startFixture().setId("full"));
            full.end();
            assertThat(full.error().orElseThrow()).isInstanceOf(QueueFullException.class);
            client.startEmbedding(new EmbeddingStart().setModel(new ModelRef().setProvider("test").setName("embedding"))).end();
            client.startToolExecution(new ToolExecutionStart().setToolName(" ")).end();
            assertThat(client.debugSnapshot().getGenerations()).extracting(Generation::getId).containsExactly("gen-0", "invalid", "full");
            assertThat(client.debugSnapshot().getToolExecutions()).isEmpty();
            assertThat(client.debugSnapshot().getQueueSize()).isEqualTo(1);
            client.flush();
            assertThat(exporter.generations.get()).isEqualTo(1);
        }

        Agento11yClientConfig smallPayload = runtimeConfig(exporter).setDebugBufferSize(1);
        smallPayload.getGenerationExport().setPayloadMaxBytes(1);
        try (Agento11yClient client = new Agento11yClient(smallPayload)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.end();
            assertThat(recorder.error().orElseThrow()).isInstanceOf(EnqueueException.class);
            assertThat(client.debugSnapshot().getGenerations()).hasSize(1);
            assertThat(client.debugSnapshot().getQueueSize()).isZero();
        }
        assertThat(exporter.generations.get()).isEqualTo(1);
    }

    @Test
    void retainedCopiesAndSnapshotsAreIsolatedExceptForShallowToolPayloads() {
        try (Agento11yClient client = new Agento11yClient(runtimeConfig(new CountingExporter()).setDebugBufferSize(1))) {
            Generation source = new Generation();
            source.setId("original");
            source.setModel(new ModelRef().setProvider("provider").setName("model"));
            source.getInput().add(new Message().setRole(MessageRole.USER).setParts(List.of(MessagePart.text("original"))));
            source.getTools().add(new ToolDefinition().setName("tool").setInputSchemaJson(new byte[] {1}));
            source.getArtifacts().add(new Artifact().setPayload(new byte[] {2}));
            source.getMetadata().put("key", "original");
            source.getTags().put("tag", "original");
            Generation expected = source.copy();
            Map<String, Object> arguments = new LinkedHashMap<>(Map.of("arg", "original"));
            List<String> result = new ArrayList<>(List.of("original"));
            ToolExecution tool = new ToolExecution().setToolName("original").setArguments(arguments).setResult(result);
            client.recordGeneration(source);
            client.recordToolExecution(tool);
            mutateDebugGeneration(source);
            tool.setToolName("changed").setArguments("replacement").setResult("replacement");
            Agento11yDebugSnapshot first = client.debugSnapshot();
            assertThat(first.getGenerations().get(0)).usingRecursiveComparison().isEqualTo(expected);
            assertThat(first.getToolExecutions().get(0).getToolName()).isEqualTo("original");
            assertThat(first.getToolExecutions().get(0).getArguments()).isSameAs(arguments);
            assertThat(first.getToolExecutions().get(0).getResult()).isSameAs(result);
            assertThatThrownBy(() -> first.getGenerations().clear()).isInstanceOf(UnsupportedOperationException.class);
            assertThatThrownBy(() -> first.getToolExecutions().clear()).isInstanceOf(UnsupportedOperationException.class);
            mutateDebugGeneration(first.getGenerations().get(0));
            first.getToolExecutions().get(0).setToolName("snapshot mutation");
            arguments.put("arg", "shared mutation");
            result.add("shared mutation");
            Agento11yDebugSnapshot second = client.debugSnapshot();
            assertThat(second.getGenerations().get(0)).usingRecursiveComparison().isEqualTo(expected);
            assertThat(second.getToolExecutions().get(0).getToolName()).isEqualTo("original");
            assertThat(second.getToolExecutions().get(0).getArguments()).isSameAs(arguments);
            assertThat(second.getToolExecutions().get(0).getResult()).isSameAs(result);

            // Failed copies must not evict the existing oldest entry.
            assertThatThrownBy(() -> client.recordGeneration(null)).isInstanceOf(NullPointerException.class);
            assertThatThrownBy(() -> client.recordToolExecution(null)).isInstanceOf(NullPointerException.class);
            assertThat(client.debugSnapshot().getGenerations().get(0)).usingRecursiveComparison().isEqualTo(expected);
            assertThat(client.debugSnapshot().getToolExecutions()).hasSize(1);
            client.recordGeneration(new Generation());
            client.recordToolExecution(new ToolExecution());
            assertThat(second.getGenerations().get(0)).usingRecursiveComparison().isEqualTo(expected);
            assertThat(second.getToolExecutions().get(0).getToolName()).isEqualTo("original");
        }
    }

    @Test
    void concurrentWritersAndSnapshotsStayBoundedAndDoNotFailIteration() throws Exception {
        CountingExporter exporter = new CountingExporter();
        ExecutorService executor = Executors.newFixedThreadPool(3);
        CountDownLatch start = new CountDownLatch(1);
        CountDownLatch writersDone = new CountDownLatch(2);
        try (Agento11yClient client = new Agento11yClient(runtimeConfig(exporter).setDebugBufferSize(3))) {
            List<Future<?>> futures = new ArrayList<>();
            for (int writer = 0; writer < 2; writer++) {
                int offset = writer * 150;
                futures.add(executor.submit(() -> {
                    try {
                        start.await();
                        for (int i = 0; i < 150; i++) {
                            completeGeneration(client, offset + i);
                            completeTool(client, offset + i);
                        }
                        return null;
                    } finally {
                        writersDone.countDown();
                    }
                }));
            }
            futures.add(executor.submit(() -> {
                start.await();
                int snapshots = 0;
                while (writersDone.getCount() > 0 || snapshots < 100) {
                    Agento11yDebugSnapshot snapshot = client.debugSnapshot();
                    assertThat(snapshot.getGenerations().size()).isLessThanOrEqualTo(3);
                    assertThat(snapshot.getToolExecutions().size()).isLessThanOrEqualTo(3);
                    assertThat(snapshot.getGenerations()).extracting(Generation::getId).doesNotHaveDuplicates();
                    assertThat(snapshot.getToolExecutions()).extracting(ToolExecution::getToolCallId).doesNotHaveDuplicates();
                    snapshots++;
                }
                return null;
            }));
            start.countDown();
            for (Future<?> future : futures) {
                future.get(10, TimeUnit.SECONDS);
            }
            assertThat(client.debugSnapshot().getGenerations()).hasSize(3);
            assertThat(client.debugSnapshot().getToolExecutions()).hasSize(3);
            assertThat(client.debugSnapshot().getQueueSize()).isEqualTo(300);
            client.flush();
            assertThat(exporter.generations.get()).isEqualTo(300);
        } finally {
            start.countDown();
            executor.shutdownNow();
            assertThat(executor.awaitTermination(10, TimeUnit.SECONDS)).isTrue();
        }
    }

    private static Agento11yClientConfig runtimeConfig(GenerationExporter exporter) {
        return new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1000)
                        .setQueueSize(2000)
                        .setFlushInterval(Duration.ZERO)
                        .setMaxRetries(0));
    }

    private static void completeGeneration(Agento11yClient client, int index) {
        GenerationStart start = TestFixtures.startFixture()
                .setId("gen-" + index)
                .setConversationId("conv-" + (index % 3))
                .setMode(index % 2 == 0 ? GenerationMode.SYNC : GenerationMode.STREAM);
        GenerationRecorder recorder = index % 2 == 0 ? client.startGeneration(start) : client.startStreamingGeneration(start);
        recorder.end();
        assertThat(recorder.error()).isEmpty();
    }

    private static void completeTool(Agento11yClient client, int index) {
        ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                .setToolName("tool-" + index)
                .setToolCallId("call-" + index)
                .setConversationId("conv-" + (index % 3))
                .setExecutionKind(ToolExecutionKind.values()[index % ToolExecutionKind.values().length]));
        recorder.end();
        assertThat(recorder.error()).isEmpty();
    }

    private static void mutateDebugGeneration(Generation generation) {
        generation.setId("changed");
        generation.getModel().setName("changed");
        generation.getInput().get(0).getParts().get(0).setText("changed");
        generation.getTools().get(0).getInputSchemaJson()[0] = 9;
        generation.getArtifacts().get(0).getPayload()[0] = 9;
        generation.getMetadata().put("key", "changed");
        generation.getTags().put("tag", "changed");
    }

    private static final class CountingExporter implements GenerationExporter {
        private final AtomicInteger generations = new AtomicInteger();
        private final AtomicInteger requests = new AtomicInteger();
        private final AtomicInteger shutdowns = new AtomicInteger();

        @Override
        public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
            requests.incrementAndGet();
            generations.addAndGet(request.getGenerations().size());
            return new ExportGenerationsResponse();
        }

        @Override
        public void shutdown() {
            shutdowns.incrementAndGet();
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
        try (Agento11yClient client = TestFixtures.newClient(exporter, 10)) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName("   "));
            recorder.setResult(new ToolExecutionResult().setArguments("x"));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            assertThat(client.debugSnapshot().getToolExecutions()).isEmpty();
        }
    }
}
