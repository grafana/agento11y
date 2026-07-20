package com.grafana.agento11y.sdk;

/** Raised when runtime APIs are called after shutdown. */
public final class ClientShutdownException extends Agento11yException {
    public ClientShutdownException(String message) {
        super(message);
    }
}
