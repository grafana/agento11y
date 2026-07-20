package com.grafana.agento11y.sdk;

import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Seed fields captured when generation recording starts. */
public final class GenerationStart {
    private String id = "";
    private String conversationId = "";
    private String conversationTitle = "";
    private String userId = "";
    private String agentName = "";
    private String agentVersion = "";
    private GenerationMode mode;
    private String operationName = "";
    private ModelRef model = new ModelRef();
    private String systemPrompt = "";
    private Long maxTokens;
    private Double temperature;
    private Double topP;
    private String toolChoice = "";
    private Boolean thinkingEnabled;
    private final List<String> parentGenerationIds = new ArrayList<>();
    private String effectiveVersion = "";
    private final List<ToolDefinition> tools = new ArrayList<>();
    private final Map<String, String> tags = new LinkedHashMap<>();
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private ContentCaptureMode contentCapture = ContentCaptureMode.DEFAULT;
    private Instant startedAt;

    public String getId() {
        return id;
    }

    public GenerationStart setId(String id) {
        this.id = id == null ? "" : id;
        return this;
    }

    public String getConversationId() {
        return conversationId;
    }

    public GenerationStart setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getConversationTitle() {
        return conversationTitle;
    }

    public GenerationStart setConversationTitle(String conversationTitle) {
        this.conversationTitle = conversationTitle == null ? "" : conversationTitle;
        return this;
    }

    public String getUserId() {
        return userId;
    }

    public GenerationStart setUserId(String userId) {
        this.userId = userId == null ? "" : userId;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public GenerationStart setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public GenerationStart setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public GenerationMode getMode() {
        return mode;
    }

    public GenerationStart setMode(GenerationMode mode) {
        this.mode = mode;
        return this;
    }

    public String getOperationName() {
        return operationName;
    }

    public GenerationStart setOperationName(String operationName) {
        this.operationName = operationName == null ? "" : operationName;
        return this;
    }

    public ModelRef getModel() {
        return model;
    }

    public GenerationStart setModel(ModelRef model) {
        this.model = model == null ? new ModelRef() : model;
        return this;
    }

    public String getSystemPrompt() {
        return systemPrompt;
    }

    public GenerationStart setSystemPrompt(String systemPrompt) {
        this.systemPrompt = systemPrompt == null ? "" : systemPrompt;
        return this;
    }

    public Long getMaxTokens() {
        return maxTokens;
    }

    public GenerationStart setMaxTokens(Long maxTokens) {
        this.maxTokens = maxTokens;
        return this;
    }

    public Double getTemperature() {
        return temperature;
    }

    public GenerationStart setTemperature(Double temperature) {
        this.temperature = temperature;
        return this;
    }

    public Double getTopP() {
        return topP;
    }

    public GenerationStart setTopP(Double topP) {
        this.topP = topP;
        return this;
    }

    public String getToolChoice() {
        return toolChoice;
    }

    public GenerationStart setToolChoice(String toolChoice) {
        this.toolChoice = toolChoice == null ? "" : toolChoice;
        return this;
    }

    public Boolean getThinkingEnabled() {
        return thinkingEnabled;
    }

    public GenerationStart setThinkingEnabled(Boolean thinkingEnabled) {
        this.thinkingEnabled = thinkingEnabled;
        return this;
    }

    public List<String> getParentGenerationIds() {
        return parentGenerationIds;
    }

    public GenerationStart setParentGenerationIds(List<String> parentGenerationIds) {
        this.parentGenerationIds.clear();
        if (parentGenerationIds != null) {
            this.parentGenerationIds.addAll(parentGenerationIds);
        }
        return this;
    }

    /** See {@link GenerationResult#getEffectiveVersion()}. */
    public String getEffectiveVersion() {
        return effectiveVersion;
    }

    public GenerationStart setEffectiveVersion(String effectiveVersion) {
        this.effectiveVersion = effectiveVersion == null ? "" : effectiveVersion;
        return this;
    }

    public List<ToolDefinition> getTools() {
        return tools;
    }

    public GenerationStart setTools(List<ToolDefinition> tools) {
        this.tools.clear();
        if (tools != null) {
            this.tools.addAll(tools);
        }
        return this;
    }

    public Map<String, String> getTags() {
        return tags;
    }

    public GenerationStart setTags(Map<String, String> tags) {
        this.tags.clear();
        if (tags != null) {
            this.tags.putAll(tags);
        }
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public GenerationStart setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public ContentCaptureMode getContentCapture() {
        return contentCapture;
    }

    public GenerationStart setContentCapture(ContentCaptureMode contentCapture) {
        this.contentCapture = contentCapture == null ? ContentCaptureMode.DEFAULT : contentCapture;
        return this;
    }

    public Instant getStartedAt() {
        return startedAt;
    }

    public GenerationStart setStartedAt(Instant startedAt) {
        this.startedAt = startedAt;
        return this;
    }

    public GenerationStart copy() {
        GenerationStart out = new GenerationStart()
                .setId(id)
                .setConversationId(conversationId)
                .setConversationTitle(conversationTitle)
                .setUserId(userId)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setMode(mode)
                .setOperationName(operationName)
                .setModel(model.copy())
                .setSystemPrompt(systemPrompt)
                .setMaxTokens(maxTokens)
                .setTemperature(temperature)
                .setTopP(topP)
                .setToolChoice(toolChoice)
                .setThinkingEnabled(thinkingEnabled)
                .setParentGenerationIds(parentGenerationIds)
                .setEffectiveVersion(effectiveVersion)
                .setContentCapture(contentCapture)
                .setStartedAt(startedAt);
        for (ToolDefinition tool : tools) {
            out.getTools().add(tool == null ? new ToolDefinition() : tool.copy());
        }
        out.getTags().putAll(tags);
        out.getMetadata().putAll(metadata);
        return out;
    }
}
