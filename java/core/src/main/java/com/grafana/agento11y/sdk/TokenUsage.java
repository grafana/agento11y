package com.grafana.agento11y.sdk;

/**
 * Token usage counters.
 *
 * <p>{@code inputTokens} is fresh, non-cached input. Cache-inclusive provider
 * adapters subtract {@code cacheReadInputTokens} before setting it.
 * {@code reasoningTokens} is an explanatory sub-bucket and may overlap with
 * {@code outputTokens} depending on provider semantics.
 */
public final class TokenUsage {
    private long inputTokens;
    private long outputTokens;
    private long totalTokens;
    private long cacheReadInputTokens;
    private long cacheWriteInputTokens;
    private long reasoningTokens;
    private boolean inputIsDisjoint;

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
     * Marks that this usage already follows the disjoint contract (fresh input,
     * additive cache buckets) because an SDK-owned adapter produced it. Consumers
     * must not re-derive fresh input when true. Manual usage leaves it false.
     */
    public boolean getInputIsDisjoint() {
        return inputIsDisjoint;
    }

    public TokenUsage setInputIsDisjoint(boolean inputIsDisjoint) {
        this.inputIsDisjoint = inputIsDisjoint;
        return this;
    }

    public TokenUsage normalized() {
        TokenUsage out = copy();
        if (out.totalTokens == 0) {
            out.totalTokens = out.inputTokens + out.outputTokens + out.cacheReadInputTokens + out.cacheWriteInputTokens;
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
                .setInputIsDisjoint(inputIsDisjoint);
    }
}
