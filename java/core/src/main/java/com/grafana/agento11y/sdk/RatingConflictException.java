package com.grafana.agento11y.sdk;

/** Raised when a rating idempotency key conflicts with a different payload. */
public final class RatingConflictException extends Agento11yException {
    public RatingConflictException(String message) {
        super(message);
    }

    public RatingConflictException(String message, Throwable cause) {
        super(message, cause);
    }
}
