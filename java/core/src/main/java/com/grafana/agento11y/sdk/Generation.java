package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Normalized generation exported by the SDK runtime. */
public final class Generation extends GenerationResult {
    private String traceId = "";
    private String spanId = "";

    public String getTraceId() {
        return traceId;
    }

    public Generation setTraceId(String traceId) {
        this.traceId = traceId == null ? "" : traceId;
        return this;
    }

    public String getSpanId() {
        return spanId;
    }

    public Generation setSpanId(String spanId) {
        this.spanId = spanId == null ? "" : spanId;
        return this;
    }

    @Override
    public Generation setId(String id) {
        super.setId(id);
        return this;
    }

    @Override
    public Generation setConversationId(String conversationId) {
        super.setConversationId(conversationId);
        return this;
    }

    @Override
    public Generation setConversationTitle(String conversationTitle) {
        super.setConversationTitle(conversationTitle);
        return this;
    }

    @Override
    public Generation setUserId(String userId) {
        super.setUserId(userId);
        return this;
    }

    @Override
    public Generation setAgentName(String agentName) {
        super.setAgentName(agentName);
        return this;
    }

    @Override
    public Generation setAgentVersion(String agentVersion) {
        super.setAgentVersion(agentVersion);
        return this;
    }

    @Override
    public Generation setEffectiveVersion(String effectiveVersion) {
        super.setEffectiveVersion(effectiveVersion);
        return this;
    }

    @Override
    public Generation setMode(GenerationMode mode) {
        super.setMode(mode);
        return this;
    }

    @Override
    public Generation setOperationName(String operationName) {
        super.setOperationName(operationName);
        return this;
    }

    @Override
    public Generation setModel(ModelRef model) {
        super.setModel(model);
        return this;
    }

    @Override
    public Generation setResponseId(String responseId) {
        super.setResponseId(responseId);
        return this;
    }

    @Override
    public Generation setResponseModel(String responseModel) {
        super.setResponseModel(responseModel);
        return this;
    }

    @Override
    public Generation setSystemPrompt(String systemPrompt) {
        super.setSystemPrompt(systemPrompt);
        return this;
    }

    @Override
    public Generation setStartedAt(Instant startedAt) {
        super.setStartedAt(startedAt);
        return this;
    }

    @Override
    public Generation setCompletedAt(Instant completedAt) {
        super.setCompletedAt(completedAt);
        return this;
    }

    public Generation copy() {
        Generation out = new Generation();
        out.setId(getId());
        out.setConversationId(getConversationId());
        out.setConversationTitle(getConversationTitle());
        out.setUserId(getUserId());
        out.setAgentName(getAgentName());
        out.setAgentVersion(getAgentVersion());
        out.setMode(getMode());
        out.setOperationName(getOperationName());
        out.setModel(getModel().copy());
        out.setResponseId(getResponseId());
        out.setResponseModel(getResponseModel());
        out.setSystemPrompt(getSystemPrompt());
        out.setMaxTokens(getMaxTokens());
        out.setTemperature(getTemperature());
        out.setTopP(getTopP());
        out.setToolChoice(getToolChoice());
        out.setThinkingEnabled(getThinkingEnabled());
        out.setParentGenerationIds(getParentGenerationIds());
        out.setEffectiveVersion(getEffectiveVersion());
        out.setUsage(getUsage().copy());
        out.setStopReason(getStopReason());
        out.setStartedAt(getStartedAt());
        out.setCompletedAt(getCompletedAt());
        out.setCallError(getCallError());
        out.setTraceId(traceId);
        out.setSpanId(spanId);

        for (Message message : getInput()) {
            out.getInput().add(message == null ? new Message() : message.copy());
        }
        for (Message message : getOutput()) {
            out.getOutput().add(message == null ? new Message() : message.copy());
        }
        for (ToolDefinition tool : getTools()) {
            out.getTools().add(tool == null ? new ToolDefinition() : tool.copy());
        }
        out.getTags().putAll(getTags());
        out.getMetadata().putAll(getMetadata());
        for (Artifact artifact : getArtifacts()) {
            out.getArtifacts().add(artifact == null ? new Artifact() : artifact.copy());
        }
        return out;
    }
}
