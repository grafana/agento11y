package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import com.fasterxml.jackson.databind.JsonNode;
import io.opentelemetry.api.OpenTelemetry;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.api.common.Attributes;
import io.opentelemetry.api.metrics.Meter;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.HistogramPointData;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import io.opentelemetry.sdk.trace.samplers.Sampler;
import java.io.IOException;
import java.lang.reflect.Proxy;
import java.nio.file.Files;
import java.nio.file.Path;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;

class ToolMetricsTest {
    private static final Instant START = Instant.parse("2026-01-01T00:00:00Z");
    private static final String DRAFT = "gen_ai.execute_tool.duration";
    private static final String LEGACY = "gen_ai.client.operation.duration";

    static Stream<Arguments> cases() throws IOException {
        Path directory = Path.of(System.getProperty("user.dir")).toAbsolutePath();
        while (directory != null) {
            Path fixture = directory.resolve("conformance/tool-metrics/cases.json");
            if (Files.isRegularFile(fixture)) {
                JsonNode root = Json.MAPPER.readTree(fixture.toFile());
                assertThat(root.path("version").asInt()).isEqualTo(1);
                assertThat(root.path("instrument").asText()).isEqualTo(DRAFT);
                assertThat(root.path("unit").asText()).isEqualTo("s");
                List<Arguments> cases = new ArrayList<>();
                root.path("cases").forEach(test -> cases.add(Arguments.of(test.path("id").asText(), test, root)));
                return cases.stream();
            }
            directory = directory.getParent();
        }
        throw new IOException("conformance/tool-metrics/cases.json not found");
    }

    @ParameterizedTest(name = "tool metrics: {0}")
    @MethodSource("cases")
    void sharedMatrix(String id, JsonNode test, JsonNode fixture) {
        JsonNode start = test.path("start");
        JsonNode end = test.path("end");
        ToolExecutionStart seed = new ToolExecutionStart()
                .setToolName(start.path("tool_name").asText())
                .setExecutionKind(kind(start.path("execution_kind").asText()))
                .setSkillName(start.path("skill_name").asText())
                .setStartedAt(START);
        ToolExecutionResult result = new ToolExecutionResult()
                .setVerifiedSkillName(end.path("verified_skill_name").asText())
                .setVerifiedSkillResourceName(end.path("verified_skill_resource_name").asText())
                .setProcessExitCode(end.path("process_exit_code").isIntegralNumber()
                        ? end.path("process_exit_code").longValue() : null)
                .setProcessExitCodeIsError(end.has("process_exit_code_is_error")
                        ? end.path("process_exit_code_is_error").booleanValue() : null)
                .setCompletedAt(START.plusMillis(320));
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig())) {
            int executions = test.path("executions").asInt(1);
            for (int i = 0; i < executions; i++) {
                ToolExecutionRecorder recorder = client.startToolExecution(seed);
                recorder.setResult(result);
                if (test.has("error")) {
                    recorder.setCallError(new IllegalStateException(test.path("error").asText()));
                }
                for (int j = 0; j < test.path("end_calls").asInt(1); j++) {
                    recorder.end();
                }
            }
            HistogramPointData legacy = h.point(LEGACY);
            assertThat(legacy.getCount()).isEqualTo(executions);
            assertThat(legacy.getSum()).isEqualTo(0.32 * executions);
            assertThat(legacy.getAttributes().get(AttributeKey.stringKey("error.type")))
                    .isEqualTo(test.has("error") ? "tool_execution_error" : "");
            MetricData metric = h.metric(DRAFT);
            assertThat(metric.getUnit()).isEqualTo("s");
            HistogramPointData point = h.point(DRAFT);
            assertThat(point.getCount()).isEqualTo(test.path("count").asLong());
            assertThat(point.getSum()).isEqualTo(legacy.getSum());
            Map<String, Object> expected = new LinkedHashMap<>();
            test.path("attributes").fields().forEachRemaining(entry -> {
                JsonNode value = entry.getValue();
                Object nativeValue = value.isIntegralNumber() ? value.longValue() : value.asText();
                // The shared fixture's tool_error denotes an explicit execution error.
                if (entry.getKey().equals("error.type") && "tool_error".equals(nativeValue)) {
                    nativeValue = "tool_execution_error";
                }
                expected.put(entry.getKey(), nativeValue);
            });
            assertThat(asMap(point.getAttributes())).containsExactlyInAnyOrderEntriesOf(expected);
            fixture.path("reserved_attributes").forEach(key -> {
                if (!expected.containsKey(key.asText())) {
                    assertThat(asMap(point.getAttributes())).doesNotContainKey(key.asText());
                }
            });
            assertThat(h.spans.getFinishedSpanItems()).hasSize(executions);
        }
    }

    @Test
    void toolMetricsAreEmittedByDefault() {
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig())) {
            finish(client, new ToolExecutionResult());
            assertThat(h.point(DRAFT).getCount()).isEqualTo(1);
            assertThat(h.point(LEGACY).getCount()).isEqualTo(1);
        }
    }

    static Stream<Arguments> serializationCases() {
        return Stream.of(ContentCaptureMode.values()).flatMap(mode -> Stream.of("sampled", "drop", "noop")
                .flatMap(tracing -> Stream.of(false, true).flatMap(explicitError -> Stream.of(false, true)
                        .map(legacyInclude -> Arguments.of(mode, tracing, explicitError, legacyInclude)))));
    }

    @ParameterizedTest(name = "serialization: {0}, {1}, execution error={2}, legacy content={3}")
    @MethodSource("serializationCases")
    @SuppressWarnings("deprecation")
    void serializationDoesNotBecomeExecutionError(ContentCaptureMode mode, String tracing,
                                                  boolean explicitError, boolean legacyInclude) {
        try (Harness h = new Harness(tracing);
                Agento11yClient client = h.client(new Agento11yClientConfig())) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("Skill").setExecutionKind(ToolExecutionKind.LOAD_SKILL)
                    .setSkillName("attempted").setContentCapture(mode).setIncludeContent(legacyInclude).setStartedAt(START));
            recorder.setResult(new ToolExecutionResult().setVerifiedSkillName("verified")
                    .setArguments(new Unserializable()).setResult(new Unserializable())
                    .setCompletedAt(START.plusSeconds(1)));
            if (explicitError) {
                recorder.setCallError(new IllegalStateException("execution failed"));
            }
            recorder.end();
            recorder.end();
            assertThat(h.point(DRAFT).getCount()).isEqualTo(1);
            assertThat(h.point(DRAFT).getAttributes().get(AttributeKey.stringKey("gen_ai.skill.name"))).isEqualTo("verified");
            assertThat(h.point(DRAFT).getAttributes().get(AttributeKey.stringKey("error.type")))
                    .isEqualTo(explicitError ? "tool_execution_error" : null);
            boolean legacyError = explicitError || mode == ContentCaptureMode.FULL
                    || (legacyInclude && (mode == ContentCaptureMode.DEFAULT || mode == ContentCaptureMode.NO_TOOL_CONTENT));
            assertThat(recorder.error().isPresent()).isEqualTo(legacyError);
            assertThat(h.point(LEGACY).getAttributes().get(AttributeKey.stringKey("error.type")))
                    .isEqualTo(legacyError ? "tool_execution_error" : "");
            if (tracing.equals("sampled")) {
                assertThat(h.spans.getFinishedSpanItems()).hasSize(1);
                var span = h.spans.getFinishedSpanItems().get(0);
                assertThat(span.getName()).contains("attempted");
                assertThat(span.getStatus().getStatusCode().name()).isEqualTo(legacyError ? "ERROR" : "OK");
                assertThat(span.getAttributes().get(AttributeKey.stringKey("gen_ai.skill.name"))).isEqualTo("attempted");
            } else {
                assertThat(h.spans.getFinishedSpanItems()).isEmpty();
            }
        }
    }

    public static final class Unserializable {
        public String getValue() {
            throw new IllegalStateException("cannot serialize");
        }
    }

    @Test
    void completionCopiesReplacementAndSnapshotsAreIsolated() {
        ToolExecutionResult input = new ToolExecutionResult().setVerifiedSkillName("first")
                .setVerifiedSkillResourceName("first.md").setProcessExitCode(2L).setProcessExitCodeIsError(false)
                .setCompletedAt(START.plusSeconds(1));
        ToolExecutionResult copied = input.copy();
        input.setVerifiedSkillName("mutated").setVerifiedSkillResourceName("mutated.md")
                .setProcessExitCode(3L).setProcessExitCodeIsError(true);
        assertThat(copied.getVerifiedSkillName()).isEqualTo("first");
        assertThat(copied.getVerifiedSkillResourceName()).isEqualTo("first.md");
        assertThat(copied.getProcessExitCode()).isEqualTo(2);
        assertThat(copied.getProcessExitCodeIsError()).isFalse();
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig())) {
            ToolExecutionStart seed = new ToolExecutionStart().setToolName("exec")
                    .setExecutionKind(ToolExecutionKind.COMMAND).setStartedAt(START);
            ToolExecutionRecorder recorder = client.startToolExecution(seed);
            seed.setExecutionKind(null);
            recorder.setResult(input);
            recorder.setResult(copied);
            copied.setVerifiedSkillName("late").setVerifiedSkillResourceName("late.md")
                    .setProcessExitCode(4L).setProcessExitCodeIsError(true);
            recorder.end();
            recorder.setResult(input);
            recorder.end();
            var attrs = asMap(h.point(DRAFT).getAttributes());
            assertThat(attrs).containsEntry("gen_ai.skill.name", "first")
                    .containsEntry("gen_ai.skill.resource.name", "first.md").containsEntry("process.exit.code", 2L)
                    .doesNotContainKey("error.type");
            ToolExecution snapshot = client.debugSnapshot().getToolExecutions().get(0);
            ToolExecution copy = snapshot.copy();
            snapshot.setVerifiedSkillName("debug mutation").setVerifiedSkillResourceName("debug.md")
                    .setProcessExitCodeIsError(true);
            assertThat(copy.getVerifiedSkillName()).isEqualTo("first");
            assertThat(copy.getVerifiedSkillResourceName()).isEqualTo("first.md");
            assertThat(copy.getProcessExitCodeIsError()).isFalse();
            ToolExecution fresh = client.debugSnapshot().getToolExecutions().get(0);
            assertThat(fresh.getVerifiedSkillName()).isEqualTo("first");
            assertThat(fresh.getVerifiedSkillResourceName()).isEqualTo("first.md");
            assertThat(fresh.getProcessExitCodeIsError()).isFalse();
            ToolExecutionRecorder cleared = client.startToolExecution(new ToolExecutionStart()
                    .setToolName("cleared").setExecutionKind(ToolExecutionKind.COMMAND).setStartedAt(START));
            cleared.setResult(input);
            cleared.setResult(null);
            cleared.end();
            HistogramPointData clearedPoint = h.metric(DRAFT).getHistogramData().getPoints().stream()
                    .filter(p -> "cleared".equals(p.getAttributes().get(AttributeKey.stringKey("gen_ai.tool.name"))))
                    .findFirst().orElseThrow();
            assertThat(asMap(clearedPoint.getAttributes())).doesNotContainKeys(
                    "gen_ai.skill.name", "gen_ai.skill.resource.name", "process.exit.code", "error.type");
        }
    }

    @Test
    void attributesAreAllowlistedAndTagsCannotSpoofReservedKeys() {
        Map<String, String> tags = new LinkedHashMap<>();
        for (String key : List.of("gen_ai.skill.name", "gen_ai.skill.resource.name", "agento11y.tool.execution.kind",
                "error.type", "process.exit.code", "gen_ai.skill.description", "gen_ai.skill.source.uri",
                "process.executable.name", "process.executable.path", "gen_ai.tool.name")) {
            tags.put(key, "spoofed");
        }
        tags.put("region", "test");
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig()
                .setTags(tags).setAgentName("agent").setAgentVersion("v1"))) {
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName(" exec ")
                    .setToolType(" function ").setToolDescription("private description").setToolCallId("private-call")
                    .setConversationId("private-conversation").setSkillDescription("private skill")
                    .setSkillSourceURI("file:///private").setProcessExecutableName("private-executable")
                    .setProcessExecutablePath("/private/path").setSkillName("attempted").setSkillResourceName("attempted.md")
                    .setRequestProvider("provider").setRequestModel("model").setStartedAt(START));
            recorder.setResult(new ToolExecutionResult().setVerifiedSkillName("ignored")
                    .setVerifiedSkillResourceName("ignored.md").setProcessExitCode(2L).setCompletedAt(START.plusSeconds(1)));
            recorder.end();
            Map<String, Object> attrs = asMap(h.point(DRAFT).getAttributes());
            assertThat(attrs).containsEntry("gen_ai.tool.name", "exec").containsEntry("gen_ai.tool.type", "function")
                    .containsEntry("gen_ai.provider.name", "provider").containsEntry("gen_ai.request.model", "model")
                    .containsEntry("gen_ai.agent.name", "agent").containsEntry("gen_ai.agent.version", "v1")
                    .containsEntry("agento11y.tool.execution.kind", "generic").containsEntry("agento11y.tag.region", "test");
            assertThat(attrs).doesNotContainKeys("gen_ai.skill.name", "gen_ai.skill.resource.name", "process.exit.code", "error.type",
                    "gen_ai.skill.description", "gen_ai.skill.source.uri", "process.executable.name", "process.executable.path",
                    "gen_ai.tool.description", "gen_ai.conversation.id", "gen_ai.tool.call.id", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result");
            assertThat(h.point(LEGACY).getAttributes().get(AttributeKey.stringKey("gen_ai.skill.name"))).isNull();
        }
    }

    @Test
    void bucketsClampingAndExemplarsMatchLegacy() {
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig())) {
            List<Double> durations = new ArrayList<>(Agento11yClient.DURATION_BUCKETS_SECONDS);
            durations.add(-1d);
            durations.add(100d);
            for (double duration : durations) {
                finish(client, new ToolExecutionResult().setCompletedAt(START.plusNanos((long) (duration * 1_000_000_000L))));
            }
            var collected = h.reader.collectAllMetrics();
            HistogramPointData draft = collected.stream().filter(m -> m.getName().equals(DRAFT))
                    .findFirst().orElseThrow().getHistogramData().getPoints().iterator().next();
            HistogramPointData legacy = collected.stream().filter(m -> m.getName().equals(LEGACY))
                    .findFirst().orElseThrow().getHistogramData().getPoints().iterator().next();
            assertThat(draft.getBoundaries()).isEqualTo(Agento11yClient.DURATION_BUCKETS_SECONDS);
            List<Long> expectedCounts = new ArrayList<>();
            expectedCounts.add(2L); // zero (clamped) plus the first boundary
            for (int i = 1; i <= Agento11yClient.DURATION_BUCKETS_SECONDS.size(); i++) {
                expectedCounts.add(1L);
            }
            assertThat(draft.getCounts()).isEqualTo(expectedCounts).isEqualTo(legacy.getCounts());
            assertThat(draft.getCount()).isEqualTo(durations.size());
            assertThat(draft.getSum()).isEqualTo(legacy.getSum());
            assertThat(draft.getExemplars()).isNotEmpty();
            List<String> traceIds = h.spans.getFinishedSpanItems().stream().map(s -> s.getTraceId()).toList();
            draft.getExemplars().forEach(e -> assertThat(traceIds).contains(e.getSpanContext().getTraceId()));
            assertThat(legacy.getExemplars()).isNotEmpty();
        }
    }

    @Test
    void toolInstrumentCreationFailureDoesNotDisableLegacy() {
        try (Harness h = new Harness()) {
            AtomicInteger attempts = new AtomicInteger();
            Meter delegate = h.meter.get("test");
            Meter failing = (Meter) Proxy.newProxyInstance(Meter.class.getClassLoader(), new Class<?>[] {Meter.class},
                    (proxy, method, args) -> {
                        if (method.getName().equals("histogramBuilder") && DRAFT.equals(args[0])) {
                            attempts.incrementAndGet();
                            throw new IllegalStateException("tool instrument unavailable");
                        }
                        return method.invoke(delegate, args);
                    });
            try (Agento11yClient client = h.client(new Agento11yClientConfig()
                    .setMeter(failing))) {
                finish(client, new ToolExecutionResult());
                assertThat(attempts.get()).isEqualTo(1);
                assertThat(h.metric(DRAFT)).isNull();
                assertThat(h.point(LEGACY).getCount()).isEqualTo(1);
                assertThat(client.debugSnapshot().getToolExecutions()).hasSize(1);
                assertThat(h.spans.getFinishedSpanItems()).hasSize(1);
            }
        }
    }

    @Test
    void signedExitCodesAndMissingToolName() {
        try (Harness h = new Harness(); Agento11yClient client = h.client(new Agento11yClientConfig())) {
            ToolExecutionRecorder noop = client.startToolExecution(new ToolExecutionStart().setToolName(" "));
            noop.end();
            assertThat(h.metric(DRAFT)).isNull();
            assertThat(h.metric(LEGACY)).isNull();
            ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName("exec")
                    .setExecutionKind(ToolExecutionKind.COMMAND).setStartedAt(START));
            recorder.setResult(new ToolExecutionResult().setProcessExitCode(-2L).setCompletedAt(START.plusSeconds(1)));
            recorder.end();
            assertThat(asMap(h.point(DRAFT).getAttributes())).containsEntry("process.exit.code", -2L).containsEntry("error.type", "-2");
            assertThat(h.point(LEGACY).getAttributes().get(AttributeKey.stringKey("error.type"))).isEmpty();
            assertThat(h.spans.getFinishedSpanItems().get(0).getStatus().getStatusCode().name()).isEqualTo("OK");
        }
    }

    private static ToolExecutionKind kind(String value) {
        return switch (value) {
            case "load_skill" -> ToolExecutionKind.LOAD_SKILL;
            case "read_skill_resource" -> ToolExecutionKind.READ_SKILL_RESOURCE;
            case "command" -> ToolExecutionKind.COMMAND;
            default -> null;
        };
    }

    private static Map<String, Object> asMap(Attributes attributes) {
        Map<String, Object> result = new LinkedHashMap<>();
        attributes.forEach((key, value) -> result.put(key.getKey(), value));
        return result;
    }

    private static void finish(Agento11yClient client, ToolExecutionResult result) {
        ToolExecutionRecorder recorder = client.startToolExecution(new ToolExecutionStart().setToolName("read").setStartedAt(START));
        recorder.setResult(result.getCompletedAt() == null ? result.copy().setCompletedAt(START.plusSeconds(1)) : result);
        recorder.end();
    }

    private static final class Harness implements AutoCloseable {
        final InMemoryMetricReader reader = InMemoryMetricReader.create();
        final SdkMeterProvider meter = SdkMeterProvider.builder().registerMetricReader(reader).build();
        final InMemorySpanExporter spans = InMemorySpanExporter.create();
        final SdkTracerProvider tracer;
        final String tracing;

        Harness() {
            this("sampled");
        }

        Harness(String tracing) {
            this.tracing = tracing;
            tracer = SdkTracerProvider.builder().setSampler(tracing.equals("drop") ? Sampler.alwaysOff() : Sampler.alwaysOn())
                    .addSpanProcessor(SimpleSpanProcessor.create(spans)).build();
        }

        Agento11yClient client(Agento11yClientConfig config) {
            config = config.copy();
            if (config.getMeter() == null) {
                config.setMeter(meter.get("test"));
            }
            return new Agento11yClient(config.setGenerationExporter(new TestFixtures.CapturingExporter())
                    .setGenerationExport(new GenerationExportConfig().setProtocol(GenerationExportProtocol.NONE))
                    .setTracer(tracing.equals("noop") ? OpenTelemetry.noop().getTracer("test") : tracer.get("test")));
        }

        MetricData metric(String name) {
            return reader.collectAllMetrics().stream().filter(m -> m.getName().equals(name)).findFirst().orElse(null);
        }

        HistogramPointData point(String name) {
            MetricData metric = metric(name);
            assertThat(metric).as("metric %s", name).isNotNull();
            assertThat(metric.getHistogramData().getPoints()).hasSize(1);
            return metric.getHistogramData().getPoints().iterator().next();
        }

        @Override
        public void close() {
            tracer.close();
            meter.close();
        }
    }
}
