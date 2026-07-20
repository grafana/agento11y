package com.grafana.agento11y.sdk;

import java.util.LinkedHashMap;
import java.util.Map;

/** SDK input payload for submitting a conversation rating. */
public final class SubmitConversationRatingRequest {
    private String ratingId = "";
    private ConversationRatingValue rating = ConversationRatingValue.GOOD;
    private String comment = "";
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private String generationId = "";
    private String raterId = "";
    private String source = "";

    public String getRatingId() {
        return ratingId;
    }

    public SubmitConversationRatingRequest setRatingId(String ratingId) {
        this.ratingId = ratingId == null ? "" : ratingId;
        return this;
    }

    public ConversationRatingValue getRating() {
        return rating;
    }

    public SubmitConversationRatingRequest setRating(ConversationRatingValue rating) {
        this.rating = rating == null ? ConversationRatingValue.GOOD : rating;
        return this;
    }

    public String getComment() {
        return comment;
    }

    public SubmitConversationRatingRequest setComment(String comment) {
        this.comment = comment == null ? "" : comment;
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public SubmitConversationRatingRequest setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public String getGenerationId() {
        return generationId;
    }

    public SubmitConversationRatingRequest setGenerationId(String generationId) {
        this.generationId = generationId == null ? "" : generationId;
        return this;
    }

    public String getRaterId() {
        return raterId;
    }

    public SubmitConversationRatingRequest setRaterId(String raterId) {
        this.raterId = raterId == null ? "" : raterId;
        return this;
    }

    public String getSource() {
        return source;
    }

    public SubmitConversationRatingRequest setSource(String source) {
        this.source = source == null ? "" : source;
        return this;
    }
}
