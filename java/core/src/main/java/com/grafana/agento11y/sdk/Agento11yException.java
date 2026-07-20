package com.grafana.agento11y.sdk;

/** Base SDK runtime exception. */
public class Agento11yException extends RuntimeException {
    public Agento11yException(String message) {
        super(message);
    }

    public Agento11yException(String message, Throwable cause) {
        super(message, cause);
    }
}
