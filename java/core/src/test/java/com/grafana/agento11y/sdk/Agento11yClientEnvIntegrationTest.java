package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.opentelemetry.api.GlobalOpenTelemetry;
import java.time.Duration;
import java.util.LinkedHashMap;
import java.util.Map;
import org.junit.jupiter.api.Test;

class Agento11yClientEnvIntegrationTest {

    private static Agento11yClientConfig baseConfig(TestFixtures.CapturingExporter exporter) {
        return new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.HTTP)
                        .setBatchSize(1)
                        .setQueueSize(100)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));
    }

    private static GenerationStart minimalStart() {
        return new GenerationStart()
                .setId("gen-1")
                .setConversationId("conv-1")
                .setMode(GenerationMode.SYNC)
                .setOperationName("chat")
                .setModel(new ModelRef().setProvider("openai").setName("gpt-4o"));
    }

    @Test
    void resolveFromEnvFillsConfigDefaults() {
        Agento11yClientConfig caller = new Agento11yClientConfig();
        Map<String, String> env = Map.of(
                "SIGIL_AGENT_NAME", "env-agent",
                "SIGIL_AGENT_VERSION", "1.2.3",
                "SIGIL_USER_ID", "user-1",
                "SIGIL_TAGS", "service=demo,team=ai");

        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(env::get, caller).config();

        assertThat(resolved.getAgentName()).isEqualTo("env-agent");
        assertThat(resolved.getAgentVersion()).isEqualTo("1.2.3");
        assertThat(resolved.getUserId()).isEqualTo("user-1");
        assertThat(resolved.getTags())
                .containsEntry("service", "demo")
                .containsEntry("team", "ai");
    }

    @Test
    void resolveFromPreferredEnvFillsConfigDefaults() {
        Agento11yClientConfig caller = new Agento11yClientConfig();
        Map<String, String> env = Map.of(
                "AGENTO11Y_AGENT_NAME", "env-agent",
                "AGENTO11Y_AGENT_VERSION", "1.2.3",
                "AGENTO11Y_USER_ID", "user-1",
                "AGENTO11Y_TAGS", "service=demo,team=ai");

        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(env::get, caller).config();

        assertThat(resolved.getAgentName()).isEqualTo("env-agent");
        assertThat(resolved.getAgentVersion()).isEqualTo("1.2.3");
        assertThat(resolved.getUserId()).isEqualTo("user-1");
        assertThat(resolved.getTags())
                .containsEntry("service", "demo")
                .containsEntry("team", "ai");
    }

    @Test
    void callerConfigOverridesEnv() {
        Agento11yClientConfig caller = new Agento11yClientConfig().setAgentName("caller-agent");
        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_AGENT_NAME", "env-agent")::get, caller).config();
        assertThat(resolved.getAgentName()).isEqualTo("caller-agent");
    }

    @Test
    void perCallSeedTagWinsOverConfigTag() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = baseConfig(exporter);
        config.setTags(Map.of("service", "demo", "team", "ai"));

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationStart start = minimalStart();
            start.getTags().put("team", "obs");
            GenerationRecorder rec = client.startGeneration(start);
            rec.setResult(TestFixtures.resultFixture());
            rec.close();
            assertThat(rec.error()).isEmpty();
            client.flush();
            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(2));
        }

        Generation captured = exporter.getRequests().get(0).get(0);
        assertThat(captured.getTags()).containsEntry("service", "demo");
        assertThat(captured.getTags()).containsEntry("team", "obs");
    }

    private static GenerationResult bareResult() {
        // Avoid TestFixtures.resultFixture() which sets identity fields the
        // recorder would prefer over the seed when finalizing the generation.
        return new GenerationResult()
                .setUsage(new TokenUsage().setInputTokens(1).setOutputTokens(1))
                .setStopReason("stop");
    }

    @Test
    void configIdentityFallsThroughToGenerationStart() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = baseConfig(exporter)
                .setAgentName("env-agent")
                .setAgentVersion("1.2.3")
                .setUserId("user-1");

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationStart start = minimalStart();
            GenerationRecorder rec = client.startGeneration(start);
            rec.setResult(bareResult());
            rec.close();
            assertThat(rec.error()).isEmpty();
            client.flush();
            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(2));
        }

        Generation captured = exporter.getRequests().get(0).get(0);
        assertThat(captured.getAgentName()).isEqualTo("env-agent");
        assertThat(captured.getAgentVersion()).isEqualTo("1.2.3");
        assertThat(captured.getUserId()).isEqualTo("user-1");
    }

    @Test
    void perCallSeedIdentityOverridesConfigDefault() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        Agento11yClientConfig config = baseConfig(exporter)
                .setAgentName("env-agent")
                .setUserId("env-user");

        try (Agento11yClient client = new Agento11yClient(config)) {
            GenerationStart start = minimalStart()
                    .setAgentName("call-agent")
                    .setUserId("call-user");
            GenerationRecorder rec = client.startGeneration(start);
            rec.setResult(bareResult());
            rec.close();
            assertThat(rec.error()).isEmpty();
            client.flush();
            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(2));
        }

        Generation captured = exporter.getRequests().get(0).get(0);
        assertThat(captured.getAgentName()).isEqualTo("call-agent");
        assertThat(captured.getUserId()).isEqualTo("call-user");
    }

    @Test
    void mergeTagsHelperPutsOverrideOnTop() {
        Map<String, String> base = new LinkedHashMap<>();
        base.put("service", "demo");
        base.put("team", "ai");
        Map<String, String> override = new LinkedHashMap<>();
        override.put("team", "obs");

        Map<String, String> merged = Agento11yClient.mergeTags(base, override);

        assertThat(merged).containsEntry("service", "demo");
        assertThat(merged).containsEntry("team", "obs");
        // The helper returns a fresh map.
        merged.put("late", "edit");
        assertThat(base).doesNotContainKey("late");
        assertThat(override).doesNotContainKey("late");
    }

    @Test
    void explicitInsecureFalseBeatsEnvTrue() {
        Agento11yClientConfig caller = new Agento11yClientConfig();
        caller.getGenerationExport().setInsecure(Boolean.FALSE);
        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(
                Map.of("SIGIL_INSECURE", "true")::get, caller).config();
        assertThat(resolved.getGenerationExport().getInsecure()).isFalse();
    }

    @Test
    void noEnvNoCallerInsecureResolvesToFalse() {
        Agento11yClientConfig resolved = Agento11yEnvConfig.resolveFromEnv(
                Map.<String, String>of()::get, new Agento11yClientConfig()).config();
        assertThat(resolved.getGenerationExport().isInsecureResolved()).isFalse();
    }
}
