package com.grafana.agento11y.sdk;

/** Conversation rating create response envelope returned by Agent Observability. */
public final class SubmitConversationRatingResponse {
    private ConversationRating rating = new ConversationRating();
    private ConversationRatingSummary summary = new ConversationRatingSummary();

    public ConversationRating getRating() {
        return rating;
    }

    public SubmitConversationRatingResponse setRating(ConversationRating rating) {
        this.rating = rating == null ? new ConversationRating() : rating;
        return this;
    }

    public ConversationRatingSummary getSummary() {
        return summary;
    }

    public SubmitConversationRatingResponse setSummary(ConversationRatingSummary summary) {
        this.summary = summary == null ? new ConversationRatingSummary() : summary;
        return this;
    }
}
