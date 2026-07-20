package com.grafana.agento11y.sdk;

/** Raised when conversation rating submission transport fails. */
public final class RatingTransportException extends Agento11yException {
    public RatingTransportException(String message) {
        super(message);
    }

    public RatingTransportException(String message, Throwable cause) {
        super(message, cause);
    }
}
