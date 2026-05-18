package com.grafana.sigil.sdk;

import io.opentelemetry.api.trace.Span;
import io.opentelemetry.api.trace.StatusCode;
import io.opentelemetry.context.Scope;
import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.Optional;
import java.util.concurrent.TimeUnit;

/** Recorder for one generation lifecycle. */
public class GenerationRecorder implements AutoCloseable {
    private final SigilClient client;
    private final GenerationStart seed;
    private final Span span;
    private final Instant startedAt;
    private final ContentCaptureMode contentCaptureMode;
    private final Scope contentCaptureScope;

    private final Object lock = new Object();
    private boolean ended;
    private Throwable callError;
    private GenerationResult result;
    private Instant firstTokenAt;
    private Throwable finalError;
    private Generation lastGeneration;

    GenerationRecorder(SigilClient client, GenerationStart seed, Span span, Instant startedAt,
                       ContentCaptureMode contentCaptureMode, Scope contentCaptureScope) {
        this.client = client;
        this.seed = seed;
        this.span = span;
        this.startedAt = startedAt;
        this.contentCaptureMode = contentCaptureMode;
        this.contentCaptureScope = contentCaptureScope;
    }

    /** Sets the mapped generation result payload. */
    public void setResult(GenerationResult result) {
        synchronized (lock) {
            this.result = result == null ? null : result.copy();
        }
    }

    /** Records a provider call error for this generation lifecycle. */
    public void setCallError(Throwable error) {
        if (error == null) {
            return;
        }
        synchronized (lock) {
            this.callError = error;
        }
    }

    /** Records when the first streamed token/chunk arrived. */
    public void setFirstTokenAt(Instant firstTokenAt) {
        if (firstTokenAt == null) {
            return;
        }
        synchronized (lock) {
            this.firstTokenAt = firstTokenAt;
        }
    }

    /** Finalizes the generation lifecycle. Safe to call multiple times. */
    public void end() {
        GenerationResult snapshotResult;
        Throwable snapshotCallError;
        Instant snapshotFirstTokenAt;

        synchronized (lock) {
            if (ended) {
                return;
            }
            ended = true;
            snapshotResult = result == null ? new GenerationResult() : result.copy();
            snapshotCallError = callError;
            snapshotFirstTokenAt = firstTokenAt;
        }

        Instant completedAt = snapshotResult.getCompletedAt() == null ? client.now() : snapshotResult.getCompletedAt();
        Generation generation;
        Throwable localError = null;
        try {
            generation = normalize(snapshotResult, completedAt, snapshotCallError);

            SigilClient.stampContentCaptureMetadata(generation, contentCaptureMode);
            if (contentCaptureMode == ContentCaptureMode.METADATA_ONLY) {
                String errorCategory = SigilClient.errorCategoryFromThrowable(snapshotCallError, false);
                SigilClient.stripContent(generation, errorCategory);
            }

            if (span.getSpanContext().isValid()) {
                generation.setTraceId(span.getSpanContext().getTraceId());
                generation.setSpanId(span.getSpanContext().getSpanId());
            }

            span.updateName(SigilClient.generationSpanName(generation.getOperationName(), generation.getModel().getName()));
            SigilClient.setGenerationSpanAttributes(span, generation);

            try {
                GenerationValidator.validate(generation);
            } catch (Throwable throwable) {
                localError = throwable;
            }

            if (localError == null) {
                try {
                    client.enqueueGeneration(generation);
                } catch (Throwable throwable) {
                    localError = throwable;
                }
            }

            boolean isMetadataOnly = contentCaptureMode == ContentCaptureMode.METADATA_ONLY;
            if (snapshotCallError != null && !isMetadataOnly) {
                span.recordException(snapshotCallError);
            }
            if (localError != null && !isMetadataOnly) {
                span.recordException(localError);
            }

            String errorType = "";
            String errorCategory = "";
            if (snapshotCallError != null) {
                errorType = "provider_call_error";
                errorCategory = SigilClient.errorCategoryFromThrowable(snapshotCallError, true);
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_TYPE, "provider_call_error");
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
                span.setStatus(StatusCode.ERROR, isMetadataOnly ? errorCategory : String.valueOf(snapshotCallError.getMessage()));
            } else if (localError instanceof ValidationException) {
                errorType = "validation_error";
                errorCategory = "sdk_error";
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_TYPE, "validation_error");
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
                span.setStatus(StatusCode.ERROR, isMetadataOnly ? errorCategory : String.valueOf(localError.getMessage()));
            } else if (localError != null) {
                errorType = "enqueue_error";
                errorCategory = "sdk_error";
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_TYPE, "enqueue_error");
                span.setAttribute(SigilClient.SPAN_ATTR_ERROR_CATEGORY, errorCategory);
                span.setStatus(StatusCode.ERROR, isMetadataOnly ? errorCategory : String.valueOf(localError.getMessage()));
            } else {
                span.setStatus(StatusCode.OK);
            }

            try (Scope metricsScope = span.makeCurrent()) {
                client.recordGenerationMetrics(generation, errorType, errorCategory, snapshotFirstTokenAt);
            }
            span.end(completedAt.toEpochMilli(), TimeUnit.MILLISECONDS);
            client.recordGeneration(generation);
        } finally {
            if (contentCaptureScope != null) {
                contentCaptureScope.close();
            }
        }

        synchronized (lock) {
            finalError = localError;
            lastGeneration = generation.copy();
        }
    }

    /**
     * Returns local SDK errors only.
     *
     * <p>This includes validation or enqueue failures, not provider call errors.</p>
     */
    public Optional<Throwable> error() {
        synchronized (lock) {
            return Optional.ofNullable(finalError);
        }
    }

    /** Returns the final normalized generation payload for debug and tests. */
    public Optional<Generation> lastGeneration() {
        synchronized (lock) {
            return Optional.ofNullable(lastGeneration == null ? null : lastGeneration.copy());
        }
    }

    @Override
    public void close() {
        end();
    }

    private Generation normalize(GenerationResult result, Instant completedAt, Throwable callError) {
        Generation generation = new Generation();

        generation.setId(firstNonBlank(result.getId(), seed.getId(), SigilClient.newID("gen")));
        generation.setConversationId(firstNonBlank(result.getConversationId(), seed.getConversationId()));
        generation.setConversationTitle(firstNonBlank(result.getConversationTitle(), seed.getConversationTitle()));
        generation.setUserId(firstNonBlank(result.getUserId(), seed.getUserId()));
        generation.setAgentName(firstNonBlank(result.getAgentName(), seed.getAgentName()));
        generation.setAgentVersion(firstNonBlank(result.getAgentVersion(), seed.getAgentVersion()));

        GenerationMode mode = result.getMode() == null ? seed.getMode() : result.getMode();
        generation.setMode(mode == null ? GenerationMode.SYNC : mode);

        String operationName = firstNonBlank(result.getOperationName(), seed.getOperationName());
        if (operationName.isBlank()) {
            operationName = SigilClient.defaultOperationName(generation.getMode());
        }
        generation.setOperationName(operationName);

        ModelRef resultModel = result.getModel() == null ? new ModelRef() : result.getModel();
        generation.setModel(new ModelRef()
                .setProvider(firstNonBlank(resultModel.getProvider(), seed.getModel().getProvider()))
                .setName(firstNonBlank(resultModel.getName(), seed.getModel().getName())));

        generation.setResponseId(result.getResponseId());
        generation.setResponseModel(result.getResponseModel());
        generation.setSystemPrompt(firstNonBlank(result.getSystemPrompt(), seed.getSystemPrompt()));
        generation.setMaxTokens(result.getMaxTokens() == null ? seed.getMaxTokens() : result.getMaxTokens());
        generation.setTemperature(result.getTemperature() == null ? seed.getTemperature() : result.getTemperature());
        generation.setTopP(result.getTopP() == null ? seed.getTopP() : result.getTopP());
        generation.setToolChoice(firstNonBlank(result.getToolChoice(), seed.getToolChoice()));
        generation.setThinkingEnabled(result.getThinkingEnabled() == null ? seed.getThinkingEnabled() : result.getThinkingEnabled());
        generation.setEffectiveVersion(firstNonBlank(result.getEffectiveVersion(), seed.getEffectiveVersion()));

        for (Message message : result.getInput()) {
            generation.getInput().add(message == null ? new Message() : message.copy());
        }
        for (Message message : result.getOutput()) {
            generation.getOutput().add(message == null ? new Message() : message.copy());
        }

        if (!result.getTools().isEmpty()) {
            for (ToolDefinition tool : result.getTools()) {
                generation.getTools().add(tool == null ? new ToolDefinition() : tool.copy());
            }
        } else {
            for (ToolDefinition tool : seed.getTools()) {
                generation.getTools().add(tool == null ? new ToolDefinition() : tool.copy());
            }
        }

        generation.setUsage((result.getUsage() == null ? new TokenUsage() : result.getUsage()).normalized());
        generation.setStopReason(result.getStopReason());
        generation.setStartedAt(result.getStartedAt() == null ? startedAt : result.getStartedAt());
        generation.setCompletedAt(completedAt);

        Map<String, String> tags = new LinkedHashMap<>(seed.getTags());
        tags.putAll(result.getTags());
        generation.setTags(tags);

        Map<String, Object> metadata = new LinkedHashMap<>(seed.getMetadata());
        metadata.putAll(result.getMetadata());
        generation.setMetadata(metadata);

        generation.setConversationTitle(firstNonBlank(
                generation.getConversationTitle(),
                SigilClient.metadataString(generation.getMetadata(), SigilClient.SPAN_ATTR_CONVERSATION_TITLE)));
        generation.setConversationTitle(normalizeResolvedString(generation.getConversationTitle()));
        if (!generation.getConversationTitle().isBlank()) {
            generation.getMetadata().put(SigilClient.SPAN_ATTR_CONVERSATION_TITLE, generation.getConversationTitle());
        }

        generation.setUserId(firstNonBlank(
                generation.getUserId(),
                SigilClient.metadataString(generation.getMetadata(), SigilClient.METADATA_USER_ID_KEY),
                SigilClient.metadataString(generation.getMetadata(), SigilClient.METADATA_LEGACY_USER_ID_KEY)));
        generation.setUserId(normalizeResolvedString(generation.getUserId()));
        if (!generation.getUserId().isBlank()) {
            generation.getMetadata().put(SigilClient.METADATA_USER_ID_KEY, generation.getUserId());
        }

        for (Artifact artifact : result.getArtifacts()) {
            generation.getArtifacts().add(artifact == null ? new Artifact() : artifact.copy());
        }

        if (callError != null) {
            generation.setCallError(firstNonBlank(result.getCallError(), String.valueOf(callError.getMessage())));
            generation.getMetadata().put("call_error", String.valueOf(callError.getMessage()));
        } else {
            generation.setCallError(result.getCallError());
        }
        generation.getMetadata().put(SigilClient.SPAN_ATTR_SDK_NAME, SigilClient.SDK_NAME);

        return generation;
    }

    private static String firstNonBlank(String... values) {
        for (String value : values) {
            if (value != null && !value.isBlank()) {
                return value;
            }
        }
        return "";
    }

    private static String normalizeResolvedString(String value) {
        return value == null ? "" : value.trim();
    }
}
