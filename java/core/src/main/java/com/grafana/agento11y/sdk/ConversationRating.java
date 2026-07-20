package com.grafana.agento11y.sdk;

import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.Map;

/** Conversation rating event returned by Sigil. */
public final class ConversationRating {
    private String ratingId = "";
    private String conversationId = "";
    private ConversationRatingValue rating = ConversationRatingValue.GOOD;
    private String comment = "";
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private String generationId = "";
    private String raterId = "";
    private String source = "";
    private Instant createdAt = Instant.EPOCH;

    public String getRatingId() {
        return ratingId;
    }

    public ConversationRating setRatingId(String ratingId) {
        this.ratingId = ratingId == null ? "" : ratingId;
        return this;
    }

    public String getConversationId() {
        return conversationId;
    }

    public ConversationRating setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public ConversationRatingValue getRating() {
        return rating;
    }

    public ConversationRating setRating(ConversationRatingValue rating) {
        this.rating = rating == null ? ConversationRatingValue.GOOD : rating;
        return this;
    }

    public String getComment() {
        return comment;
    }

    public ConversationRating setComment(String comment) {
        this.comment = comment == null ? "" : comment;
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public ConversationRating setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public String getGenerationId() {
        return generationId;
    }

    public ConversationRating setGenerationId(String generationId) {
        this.generationId = generationId == null ? "" : generationId;
        return this;
    }

    public String getRaterId() {
        return raterId;
    }

    public ConversationRating setRaterId(String raterId) {
        this.raterId = raterId == null ? "" : raterId;
        return this;
    }

    public String getSource() {
        return source;
    }

    public ConversationRating setSource(String source) {
        this.source = source == null ? "" : source;
        return this;
    }

    public Instant getCreatedAt() {
        return createdAt;
    }

    public ConversationRating setCreatedAt(Instant createdAt) {
        this.createdAt = createdAt == null ? Instant.EPOCH : createdAt;
        return this;
    }
}
