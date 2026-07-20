package com.grafana.agento11y.sdk.devex;

import static org.assertj.core.api.Assertions.assertThat;

import com.grafana.agento11y.sdk.GenerationMode;
import org.junit.jupiter.api.Test;

class DevexEmitterTest {
    @Test
    void buildTagEnvelopeIncludesRequiredContractFields() {
        DevexEmitter.TagEnvelope envelope = DevexEmitter.buildTagEnvelope("openai", GenerationMode.SYNC, 2, 1);

        assertThat(envelope.tags()).containsEntry("agento11y.devex.language", "java");
        assertThat(envelope.tags()).containsEntry("agento11y.devex.provider", "openai");
        assertThat(envelope.tags()).containsEntry("agento11y.devex.source", "provider_wrapper");
        assertThat(envelope.tags()).containsEntry("agento11y.devex.mode", "SYNC");

        assertThat(envelope.metadata()).containsEntry("turn_index", 2);
        assertThat(envelope.metadata()).containsEntry("conversation_slot", 1);
        assertThat(envelope.metadata()).containsEntry("emitter", "sdk-traffic");
        assertThat(envelope.metadata()).containsKey("agent_persona");
        assertThat(envelope.metadata()).containsKey("provider_shape");
    }

    @Test
    void sourceTagForUsesCoreCustomForMistral() {
        assertThat(DevexEmitter.sourceTagFor("mistral")).isEqualTo("core_custom");
        assertThat(DevexEmitter.sourceTagFor("gemini")).isEqualTo("provider_wrapper");
    }

    @Test
    void chooseModeUsesStreamThreshold() {
        assertThat(DevexEmitter.chooseMode(10, 30)).isEqualTo(GenerationMode.STREAM);
        assertThat(DevexEmitter.chooseMode(30, 30)).isEqualTo(GenerationMode.SYNC);
    }

    @Test
    void resolveThreadRotatesConversationAndResetsTurnAtThreshold() {
        DevexEmitter.SourceState state = new DevexEmitter.SourceState(1);

        DevexEmitter.ThreadState first = DevexEmitter.resolveThread(state, 3, "openai", 0);
        assertThat(first.turn).isEqualTo(0);
        String firstConversationID = first.conversationId;
        assertThat(firstConversationID).isNotBlank();

        first.turn = 3;
        DevexEmitter.ThreadState rotated = DevexEmitter.resolveThread(state, 3, "openai", 0);
        assertThat(rotated.turn).isEqualTo(0);
        assertThat(rotated.conversationId).isNotEqualTo(firstConversationID);
    }
}
