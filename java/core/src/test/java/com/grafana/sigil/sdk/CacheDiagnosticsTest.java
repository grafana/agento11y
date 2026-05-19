package com.grafana.sigil.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import java.util.Map;
import org.junit.jupiter.api.Test;

class CacheDiagnosticsTest {

    @Test
    void setCacheDiagnostics_stampsMetadata() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (SigilClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder rec = client.startGeneration(TestFixtures.startFixture());
            rec.setCacheDiagnostics("params_changed", 777L, "msg_xyz");
            rec.setResult(TestFixtures.resultFixture());
            rec.end();
            assertThat(rec.error()).isEmpty();
            Map<String, Object> md = rec.lastGeneration().orElseThrow().getMetadata();
            assertThat(md.get(CacheDiagnostics.MISS_REASON_KEY)).isEqualTo("params_changed");
            assertThat(md.get(CacheDiagnostics.MISSED_INPUT_TOKENS_KEY)).isEqualTo("777");
            assertThat(md.get(CacheDiagnostics.PREVIOUS_MESSAGE_ID_KEY)).isEqualTo("msg_xyz");
        }
    }

    @Test
    void cacheDiagnostics_staticHelper_delegates() {
        TestFixtures.CapturingExporter exporter = new TestFixtures.CapturingExporter();
        try (SigilClient client = TestFixtures.newClient(exporter)) {
            GenerationRecorder rec = client.startGeneration(TestFixtures.startFixture());
            CacheDiagnostics.setCacheDiagnostics(rec, "unavailable", null, null);
            rec.setResult(TestFixtures.resultFixture());
            rec.end();
            Map<String, Object> md = rec.lastGeneration().orElseThrow().getMetadata();
            assertThat(md.get(CacheDiagnostics.MISS_REASON_KEY)).isEqualTo("unavailable");
            assertThat(md.containsKey(CacheDiagnostics.MISSED_INPUT_TOKENS_KEY)).isFalse();
        }
    }
}
