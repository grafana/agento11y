package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Aggregated conversation rating summary returned by Sigil. */
public final class ConversationRatingSummary {
    private int totalCount;
    private int goodCount;
    private int badCount;
    private ConversationRatingValue latestRating;
    private Instant latestRatedAt = Instant.EPOCH;
    private Instant latestBadAt;
    private boolean hasBadRating;

    public int getTotalCount() {
        return totalCount;
    }

    public ConversationRatingSummary setTotalCount(int totalCount) {
        this.totalCount = totalCount;
        return this;
    }

    public int getGoodCount() {
        return goodCount;
    }

    public ConversationRatingSummary setGoodCount(int goodCount) {
        this.goodCount = goodCount;
        return this;
    }

    public int getBadCount() {
        return badCount;
    }

    public ConversationRatingSummary setBadCount(int badCount) {
        this.badCount = badCount;
        return this;
    }

    public ConversationRatingValue getLatestRating() {
        return latestRating;
    }

    public ConversationRatingSummary setLatestRating(ConversationRatingValue latestRating) {
        this.latestRating = latestRating;
        return this;
    }

    public Instant getLatestRatedAt() {
        return latestRatedAt;
    }

    public ConversationRatingSummary setLatestRatedAt(Instant latestRatedAt) {
        this.latestRatedAt = latestRatedAt == null ? Instant.EPOCH : latestRatedAt;
        return this;
    }

    public Instant getLatestBadAt() {
        return latestBadAt;
    }

    public ConversationRatingSummary setLatestBadAt(Instant latestBadAt) {
        this.latestBadAt = latestBadAt;
        return this;
    }

    public boolean isHasBadRating() {
        return hasBadRating;
    }

    public ConversationRatingSummary setHasBadRating(boolean hasBadRating) {
        this.hasBadRating = hasBadRating;
        return this;
    }
}
