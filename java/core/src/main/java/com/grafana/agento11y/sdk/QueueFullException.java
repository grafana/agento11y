package com.grafana.agento11y.sdk;

/** Raised when generation queue is full. */
public final class QueueFullException extends Agento11yException {
    public QueueFullException(String message) {
        super(message);
    }
}
