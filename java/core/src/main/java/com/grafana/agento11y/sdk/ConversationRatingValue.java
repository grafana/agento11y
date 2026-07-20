package com.grafana.agento11y.sdk;

/** Allowed conversation rating values. */
public enum ConversationRatingValue {
    GOOD("CONVERSATION_RATING_VALUE_GOOD"),
    BAD("CONVERSATION_RATING_VALUE_BAD");

    private final String wireValue;

    ConversationRatingValue(String wireValue) {
        this.wireValue = wireValue;
    }

    public String wireValue() {
        return wireValue;
    }

    public static ConversationRatingValue fromWireValue(String value) {
        for (ConversationRatingValue candidate : values()) {
            if (candidate.wireValue.equals(value)) {
                return candidate;
            }
        }
        throw new IllegalArgumentException("unsupported conversation rating value: " + value);
    }
}
