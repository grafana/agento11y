package com.grafana.agento11y.sdk;

/** Validation helpers for embedding recording payloads. */
public final class EmbeddingValidator {
    private EmbeddingValidator() {
    }

    public static void validateStart(EmbeddingStart start) {
        EmbeddingStart value = start == null ? new EmbeddingStart() : start;

        if (value.getModel() == null || value.getModel().getProvider().trim().isEmpty()) {
            throw new ValidationException("embedding.model.provider is required");
        }
        if (value.getModel().getName().trim().isEmpty()) {
            throw new ValidationException("embedding.model.name is required");
        }
        if (value.getDimensions() != null && value.getDimensions() <= 0) {
            throw new ValidationException("embedding.dimensions must be > 0");
        }
        if (!value.getEncodingFormat().isEmpty() && value.getEncodingFormat().trim().isEmpty()) {
            throw new ValidationException("embedding.encoding_format must not be blank");
        }
    }

    public static void validateResult(EmbeddingResult result) {
        EmbeddingResult value = result == null ? new EmbeddingResult() : result;

        if (value.getInputCount() < 0) {
            throw new ValidationException("embedding.input_count must be >= 0");
        }
        if (value.getInputTokens() < 0) {
            throw new ValidationException("embedding.input_tokens must be >= 0");
        }
        if (value.getDimensions() != null && value.getDimensions() <= 0) {
            throw new ValidationException("embedding.dimensions must be > 0");
        }
    }
}
