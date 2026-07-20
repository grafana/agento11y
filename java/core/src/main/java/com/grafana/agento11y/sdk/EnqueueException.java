package com.grafana.agento11y.sdk;

/** Raised when generation enqueue fails locally. */
public final class EnqueueException extends Agento11yException {
    public EnqueueException(String message, Throwable cause) {
        super(message, cause);
    }

    public EnqueueException(String message) {
        super(message);
    }
}
