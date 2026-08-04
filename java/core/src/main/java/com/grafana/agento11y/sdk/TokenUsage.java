package com.grafana.agento11y.sdk;

/**
 * Token usage counters.
 *
 * <p>Under {@link TokenInputSemantics#INCLUSIVE}, {@code inputTokens} includes
 * both cache buckets per the OTel GenAI conventions, and
 * {@code reasoningTokens} is an explanatory sub-bucket of {@code outputTokens}.
 */
public final class TokenUsage {

    /**
     * Declares what {@code inputTokens} covers, mirroring
     * {@code agento11y.v1.TokenInputSemantics}.
     */
    public enum TokenInputSemantics {
        /** Provider-raw or legacy telemetry. */
        UNSPECIFIED,
        /** OTel GenAI contract: input includes both cache buckets. */
        INCLUSIVE,
    }

    private long inputTokens;
    private long outputTokens;
    private long totalTokens;
    private long cacheReadInputTokens;
    private long cacheWriteInputTokens;
    private long reasoningTokens;
    private TokenInputSemantics inputSemantics = TokenInputSemantics.UNSPECIFIED;

    public long getInputTokens() {
        return inputTokens;
    }

    public TokenUsage setInputTokens(long inputTokens) {
        this.inputTokens = inputTokens;
        return this;
    }

    public long getOutputTokens() {
        return outputTokens;
    }

    public TokenUsage setOutputTokens(long outputTokens) {
        this.outputTokens = outputTokens;
        return this;
    }

    public long getTotalTokens() {
        return totalTokens;
    }

    public TokenUsage setTotalTokens(long totalTokens) {
        this.totalTokens = totalTokens;
        return this;
    }

    public long getCacheReadInputTokens() {
        return cacheReadInputTokens;
    }

    public TokenUsage setCacheReadInputTokens(long cacheReadInputTokens) {
        this.cacheReadInputTokens = cacheReadInputTokens;
        return this;
    }

    public long getCacheWriteInputTokens() {
        return cacheWriteInputTokens;
    }

    public TokenUsage setCacheWriteInputTokens(long cacheWriteInputTokens) {
        this.cacheWriteInputTokens = cacheWriteInputTokens;
        return this;
    }

    public long getReasoningTokens() {
        return reasoningTokens;
    }

    public TokenUsage setReasoningTokens(long reasoningTokens) {
        this.reasoningTokens = reasoningTokens;
        return this;
    }

    /**
     * Which contract {@code inputTokens} follows. Set only by SDK adapters that
     * positively identified the provider payload shape; manual user-supplied
     * usage leaves it UNSPECIFIED.
     */
    public TokenInputSemantics getInputSemantics() {
        return inputSemantics;
    }

    public TokenUsage setInputSemantics(TokenInputSemantics inputSemantics) {
        this.inputSemantics = inputSemantics == null ? TokenInputSemantics.UNSPECIFIED : inputSemantics;
        return this;
    }

    public TokenUsage normalized() {
        TokenUsage out = copy();
        if (out.totalTokens == 0) {
            out.totalTokens = out.inputTokens + out.outputTokens;
        }
        return out;
    }

    public TokenUsage copy() {
        return new TokenUsage()
                .setInputTokens(inputTokens)
                .setOutputTokens(outputTokens)
                .setTotalTokens(totalTokens)
                .setCacheReadInputTokens(cacheReadInputTokens)
                .setCacheWriteInputTokens(cacheWriteInputTokens)
                .setReasoningTokens(reasoningTokens)
                .setInputSemantics(inputSemantics);
    }
}
