package com.grafana.agento11y.sdk;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import java.time.Instant;
import java.util.Optional;
import java.util.concurrent.TimeUnit;

/** Recorder for one tool execution lifecycle. */
public class ToolExecutionRecorder implements AutoCloseable {
    private final Agento11yClient client;
    private final ToolExecutionStart seed;
    private final Span span;
    private final Instant startedAt;
    private final ContentCaptureMode contentCaptureMode;
    private final boolean includeContent;
    // Tools have no proto export; under both stripped modes the span must
    // not echo raw provider exception text via recordException events or the
    // status description.
    private final boolean redactSpanErrors;

    private final Object lock = new Object();
    private boolean ended;
    private Throwable callError;
    private ToolExecutionResult result;
    private Throwable finalError;

    ToolExecutionRecorder(Agento11yClient client, ToolExecutionStart seed, Span span, Instant startedAt,
                          ContentCaptureMode contentCaptureMode, boolean legacyIncludeContent) {
        this.client = client;
        this.seed = seed;
        this.span = span;
        this.startedAt = startedAt;
        this.contentCaptureMode = contentCaptureMode;
        this.redactSpanErrors = contentCaptureMode == ContentCaptureMode.METADATA_ONLY
                || contentCaptureMode == ContentCaptureMode.FULL_WITH_METADATA_SPANS;
        this.includeContent = switch (contentCaptureMode) {
            case METADATA_ONLY, FULL_WITH_METADATA_SPANS -> false;
            case FULL -> true;
            default -> legacyIncludeContent;
        };
    }

    protected ToolExecutionRecorder() {
        this.client = null;
        this.seed = null;
        this.span = null;
        this.startedAt = null;
        this.contentCaptureMode = ContentCaptureMode.NO_TOOL_CONTENT;
        this.includeContent = false;
        this.redactSpanErrors = false;
    }

    /** Sets tool execution arguments/result payload. */
    public void setResult(ToolExecutionResult result) {
        synchronized (lock) {
            this.result = result == null ? null : result.copy();
        }
    }

    /** Records a tool execution error for this lifecycle. */
    public void setCallError(Throwable error) {
        if (error == null) {
            return;
        }
        synchronized (lock) {
            this.callError = error;
        }
    }

    /** Finalizes the tool execution lifecycle. Safe to call multiple times. */
    public void end() {
        ToolExecutionResult snapshotResult;
        Throwable snapshotCallError;

        synchronized (lock) {
            if (ended) {
                return;
            }
            ended = true;
            snapshotResult = result == null ? new ToolExecutionResult() : result.copy();
            snapshotCallError = callError;
        }

        Instant completedAt = snapshotResult.getCompletedAt() == null ? client.now() : snapshotResult.getCompletedAt();

        ToolExecution execution = new ToolExecution()
                .setToolName(seed.getToolName())
                .setToolCallId(seed.getToolCallId())
                .setToolType(seed.getToolType())
                .setToolDescription(seed.getToolDescription())
                .setConversationId(seed.getConversationId())
                .setAgentName(seed.getAgentName())
                .setAgentVersion(seed.getAgentVersion())
                .setIncludeContent(includeContent)
                .setStartedAt(startedAt)
                .setCompletedAt(completedAt)
                .setArguments(snapshotResult.getArguments())
                .setResult(snapshotResult.getResult())
                .setCallError(snapshotCallError == null ? "" : String.valueOf(snapshotCallError.getMessage()));

        if (includeContent) {
            try {
                if (snapshotResult.getArguments() != null) {
                    span.setAttribute(Agento11yClient.SPAN_ATTR_TOOL_CALL_ARGUMENTS, Json.MAPPER.writeValueAsString(snapshotResult.getArguments()));
                }
                if (snapshotResult.getResult() != null) {
                    span.setAttribute(Agento11yClient.SPAN_ATTR_TOOL_CALL_RESULT, Json.MAPPER.writeValueAsString(snapshotResult.getResult()));
                }
            } catch (Exception exception) {
                snapshotCallError = snapshotCallError == null ? exception : snapshotCallError;
            }
        }

        if (snapshotCallError != null) {
            String errorCategory = Agento11yClient.errorCategoryFromThrowable(snapshotCallError, true);
            if (!redactSpanErrors) {
                span.recordException(snapshotCallError);
            }
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_TYPE, "tool_execution_error");
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
            span.setStatus(
                    StatusCode.ERROR,
                    redactSpanErrors ? errorCategory : String.valueOf(snapshotCallError.getMessage()));
        } else {
            span.setStatus(StatusCode.OK);
        }

        try (Scope metricsScope = span.makeCurrent()) {
            client.recordToolExecutionMetrics(seed, startedAt, completedAt, snapshotCallError);
        }
        span.end(completedAt.toEpochMilli(), TimeUnit.MILLISECONDS);
        client.recordToolExecution(execution);

        synchronized (lock) {
            finalError = snapshotCallError;
        }
    }

    /** Returns local SDK errors from tool span finalization. */
    public Optional<Throwable> error() {
        synchronized (lock) {
            return Optional.ofNullable(finalError);
        }
    }

    @Override
    public void close() {
        end();
    }

    private static final class NoopToolExecutionRecorder extends ToolExecutionRecorder {
        private static final NoopToolExecutionRecorder INSTANCE = new NoopToolExecutionRecorder();

        @Override
        public void setResult(ToolExecutionResult result) {
        }

        @Override
        public void setCallError(Throwable error) {
        }

        @Override
        public void end() {
        }

        @Override
        public Optional<Throwable> error() {
            return Optional.empty();
        }

        @Override
        public void close() {
        }
    }

    static final ToolExecutionRecorder INSTANCE_NOOP = NoopToolExecutionRecorder.INSTANCE;
}
