package com.grafana.agento11y.sdk;

/** Configures optional embedding input text capture on spans. */
public final class EmbeddingCaptureConfig {
    private boolean captureInput;
    private int maxInputItems = 20;
    private int maxTextLength = 1024;

    public boolean isCaptureInput() {
        return captureInput;
    }

    public EmbeddingCaptureConfig setCaptureInput(boolean captureInput) {
        this.captureInput = captureInput;
        return this;
    }

    public int getMaxInputItems() {
        return maxInputItems;
    }

    public EmbeddingCaptureConfig setMaxInputItems(int maxInputItems) {
        this.maxInputItems = maxInputItems;
        return this;
    }

    public int getMaxTextLength() {
        return maxTextLength;
    }

    public EmbeddingCaptureConfig setMaxTextLength(int maxTextLength) {
        this.maxTextLength = maxTextLength;
        return this;
    }

    public EmbeddingCaptureConfig copy() {
        return new EmbeddingCaptureConfig()
                .setCaptureInput(captureInput)
                .setMaxInputItems(maxInputItems)
                .setMaxTextLength(maxTextLength);
    }
}
