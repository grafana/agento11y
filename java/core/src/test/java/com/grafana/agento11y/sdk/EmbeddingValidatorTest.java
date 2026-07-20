package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThatCode;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import org.junit.jupiter.api.Test;

class EmbeddingValidatorTest {
    @Test
    void validatesEmbeddingStart() {
        assertThatCode(() -> EmbeddingValidator.validateStart(new EmbeddingStart()
                .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small"))
                .setDimensions(256L)
                .setEncodingFormat("float"))).doesNotThrowAnyException();
    }

    @Test
    void rejectsEmbeddingStartWithoutModelProvider() {
        assertThatThrownBy(() -> EmbeddingValidator.validateStart(new EmbeddingStart()
                .setModel(new ModelRef().setProvider("").setName("text-embedding-3-small"))))
                .isInstanceOf(ValidationException.class)
                .hasMessageContaining("embedding.model.provider is required");
    }

    @Test
    void validatesEmbeddingResult() {
        assertThatCode(() -> EmbeddingValidator.validateResult(new EmbeddingResult()
                .setInputCount(2)
                .setInputTokens(42)
                .setDimensions(256L)))
                .doesNotThrowAnyException();
    }

    @Test
    void rejectsEmbeddingResultWithNegativeInputCount() {
        assertThatThrownBy(() -> EmbeddingValidator.validateResult(new EmbeddingResult().setInputCount(-1)))
                .isInstanceOf(ValidationException.class)
                .hasMessageContaining("embedding.input_count must be >= 0");
    }
}
