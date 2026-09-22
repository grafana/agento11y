package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import java.time.Duration;
import java.util.List;
import org.junit.jupiter.api.Test;

class GenerationParentLinkTest {
    @Test
    void carriesParentGenerationIdsFromStartSeed() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder recorder =
                    client.startGeneration(TestFixtures.startFixture().setParentGenerationIds(List.of("gen-parent-1")));
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(5));
            assertThat(exporter.getRequests().get(0).get(0).getParentGenerationIds())
                    .containsExactly("gen-parent-1");
        }
    }

    @Test
    void resultParentGenerationIdsOverrideSeed() throws Exception {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (Agento11yClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder recorder = client.startGeneration(
                    TestFixtures.startFixture().setParentGenerationIds(List.of("gen-seed-parent")));
            recorder.setResult(TestFixtures.resultFixture().setParentGenerationIds(List.of("gen-result-parent")));
            recorder.end();

            TestFixtures.waitFor(() -> !exporter.getRequests().isEmpty(), Duration.ofSeconds(5));
            assertThat(exporter.getRequests().get(0).get(0).getParentGenerationIds())
                    .containsExactly("gen-result-parent");
        }
    }
}
