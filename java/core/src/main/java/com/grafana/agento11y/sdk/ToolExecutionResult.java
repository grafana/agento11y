package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Completion payload for tool execution spans. */
public final class ToolExecutionResult {
    private Object arguments;
    private Object result;
    private Instant completedAt;

    public Object getArguments() {
        return arguments;
    }

    public ToolExecutionResult setArguments(Object arguments) {
        this.arguments = arguments;
        return this;
    }

    public Object getResult() {
        return result;
    }

    public ToolExecutionResult setResult(Object result) {
        this.result = result;
        return this;
    }

    public Instant getCompletedAt() {
        return completedAt;
    }

    public ToolExecutionResult setCompletedAt(Instant completedAt) {
        this.completedAt = completedAt;
        return this;
    }

    public ToolExecutionResult copy() {
        return new ToolExecutionResult().setArguments(arguments).setResult(result).setCompletedAt(completedAt);
    }
}
