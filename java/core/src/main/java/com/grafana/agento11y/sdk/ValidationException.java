package com.grafana.agento11y.sdk;

/** Raised when generation payload validation fails. */
public final class ValidationException extends Agento11yException {
    public ValidationException(String message) {
        super(message);
    }
}
