package com.grafana.agento11y.sdk;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import java.time.Instant;
import java.util.Optional;
import java.util.concurrent.TimeUnit;

/** Recorder for one embedding call lifecycle. */
public final class EmbeddingRecorder implements AutoCloseable {
    static final EmbeddingRecorder INSTANCE_NOOP = new EmbeddingRecorder(
            null, new EmbeddingStart(), Span.getInvalid(), Instant.EPOCH, ContentCaptureMode.DEFAULT, true);

    private final Agento11yClient client;
    private final EmbeddingStart seed;
    private final Span span;
    private final Instant startedAt;
    private final ContentCaptureMode contentCaptureMode;
    private final boolean noop;

    private final Object lock = new Object();
    private boolean ended;
    private Throwable callError;
    private EmbeddingResult result;
    private Throwable finalError;

    EmbeddingRecorder(
            Agento11yClient client,
            EmbeddingStart seed,
            Span span,
            Instant startedAt,
            ContentCaptureMode contentCaptureMode) {
        this(client, seed, span, startedAt, contentCaptureMode, false);
    }

    private EmbeddingRecorder(
            Agento11yClient client,
            EmbeddingStart seed,
            Span span,
            Instant startedAt,
            ContentCaptureMode contentCaptureMode,
            boolean noop) {
        this.client = client;
        this.seed = seed;
        this.span = span;
        this.startedAt = startedAt;
        this.contentCaptureMode = contentCaptureMode;
        this.noop = noop;
    }

    /** Sets the mapped embedding result payload. */
    public void setResult(EmbeddingResult result) {
        if (noop) {
            return;
        }
        synchronized (lock) {
            this.result = result == null ? null : result.copy();
        }
    }

    /** Records a provider call error for this embedding lifecycle. */
    public void setCallError(Throwable error) {
        if (noop || error == null) {
            return;
        }
        synchronized (lock) {
            this.callError = error;
        }
    }

    /** Finalizes the embedding lifecycle. Safe to call multiple times. */
    public void end() {
        if (noop) {
            return;
        }

        Throwable snapshotCallError;
        EmbeddingResult snapshotResult;

        synchronized (lock) {
            if (ended) {
                return;
            }
            ended = true;
            snapshotCallError = callError;
            snapshotResult = result == null ? new EmbeddingResult() : result.copy();
        }

        span.updateName(Agento11yClient.embeddingSpanName(seed.getModel().getName()));
        Agento11yClient.setEmbeddingEndSpanAttributes(
                span, snapshotResult, client.getEmbeddingCaptureConfig(), contentCaptureMode);

        Throwable localError = null;
        try {
            EmbeddingValidator.validateStart(seed);
            EmbeddingValidator.validateResult(snapshotResult);
        } catch (Throwable throwable) {
            localError = throwable;
        }

        // Redact span-side error text under both stripped modes. Embeddings
        // have no proto export, so the raw provider error never escapes the
        // span path; matches the generation FULL_WITH_METADATA_SPANS contract.
        boolean redactSpanErrors =
                contentCaptureMode == ContentCaptureMode.METADATA_ONLY
                        || contentCaptureMode == ContentCaptureMode.FULL_WITH_METADATA_SPANS;

        if (snapshotCallError != null && !redactSpanErrors) {
            span.recordException(snapshotCallError);
        }
        if (localError != null && !redactSpanErrors) {
            span.recordException(localError);
        }

        String errorType = "";
        String errorCategory = "";
        if (snapshotCallError != null) {
            errorType = "provider_call_error";
            errorCategory = Agento11yClient.errorCategoryFromThrowable(snapshotCallError, true);
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_TYPE, errorType);
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
            span.setStatus(
                    StatusCode.ERROR,
                    redactSpanErrors ? errorCategory : String.valueOf(snapshotCallError.getMessage()));
        } else if (localError != null) {
            errorType = "validation_error";
            errorCategory = "sdk_error";
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_TYPE, errorType);
            span.setAttribute(Agento11yClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
            span.setStatus(
                    StatusCode.ERROR,
                    redactSpanErrors ? errorCategory : String.valueOf(localError.getMessage()));
        } else {
            span.setStatus(StatusCode.OK);
        }

        Instant completedAt = client.now();
        try (Scope metricsScope = span.makeCurrent()) {
            client.recordEmbeddingMetrics(seed, snapshotResult, startedAt, completedAt, errorType, errorCategory);
        }
        span.end(completedAt.toEpochMilli(), TimeUnit.MILLISECONDS);

        synchronized (lock) {
            finalError = localError;
        }
    }

    /** Returns local SDK errors only. Provider call errors are excluded. */
    public Optional<Throwable> error() {
        synchronized (lock) {
            return Optional.ofNullable(finalError);
        }
    }

    @Override
    public void close() {
        end();
    }
}
