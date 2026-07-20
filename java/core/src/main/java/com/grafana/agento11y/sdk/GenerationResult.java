package com.grafana.agento11y.sdk;

import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Result fields set before a generation is finalized. */
public class GenerationResult {
    private String id = "";
    private String conversationId = "";
    private String conversationTitle = "";
    private String userId = "";
    private String agentName = "";
    private String agentVersion = "";
    private GenerationMode mode;
    private String operationName = "";
    private ModelRef model = new ModelRef();
    private String responseId = "";
    private String responseModel = "";
    private String systemPrompt = "";
    private Long maxTokens;
    private Double temperature;
    private Double topP;
    private String toolChoice = "";
    private Boolean thinkingEnabled;
    private final List<String> parentGenerationIds = new ArrayList<>();
    private String effectiveVersion = "";
    private final List<Message> input = new ArrayList<>();
    private final List<Message> output = new ArrayList<>();
    private final List<ToolDefinition> tools = new ArrayList<>();
    private TokenUsage usage = new TokenUsage();
    private String stopReason = "";
    private Instant startedAt;
    private Instant completedAt;
    private final Map<String, String> tags = new LinkedHashMap<>();
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private final List<Artifact> artifacts = new ArrayList<>();
    private String callError = "";

    public String getId() {
        return id;
    }

    public GenerationResult setId(String id) {
        this.id = id == null ? "" : id;
        return this;
    }

    public String getConversationId() {
        return conversationId;
    }

    public GenerationResult setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getConversationTitle() {
        return conversationTitle;
    }

    public GenerationResult setConversationTitle(String conversationTitle) {
        this.conversationTitle = conversationTitle == null ? "" : conversationTitle;
        return this;
    }

    public String getUserId() {
        return userId;
    }

    public GenerationResult setUserId(String userId) {
        this.userId = userId == null ? "" : userId;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public GenerationResult setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public GenerationResult setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public GenerationMode getMode() {
        return mode;
    }

    public GenerationResult setMode(GenerationMode mode) {
        this.mode = mode;
        return this;
    }

    public String getOperationName() {
        return operationName;
    }

    public GenerationResult setOperationName(String operationName) {
        this.operationName = operationName == null ? "" : operationName;
        return this;
    }

    public ModelRef getModel() {
        return model;
    }

    public GenerationResult setModel(ModelRef model) {
        this.model = model == null ? new ModelRef() : model;
        return this;
    }

    public String getResponseId() {
        return responseId;
    }

    public GenerationResult setResponseId(String responseId) {
        this.responseId = responseId == null ? "" : responseId;
        return this;
    }

    public String getResponseModel() {
        return responseModel;
    }

    public GenerationResult setResponseModel(String responseModel) {
        this.responseModel = responseModel == null ? "" : responseModel;
        return this;
    }

    public String getSystemPrompt() {
        return systemPrompt;
    }

    public GenerationResult setSystemPrompt(String systemPrompt) {
        this.systemPrompt = systemPrompt == null ? "" : systemPrompt;
        return this;
    }

    public Long getMaxTokens() {
        return maxTokens;
    }

    public GenerationResult setMaxTokens(Long maxTokens) {
        this.maxTokens = maxTokens;
        return this;
    }

    public Double getTemperature() {
        return temperature;
    }

    public GenerationResult setTemperature(Double temperature) {
        this.temperature = temperature;
        return this;
    }

    public Double getTopP() {
        return topP;
    }

    public GenerationResult setTopP(Double topP) {
        this.topP = topP;
        return this;
    }

    public String getToolChoice() {
        return toolChoice;
    }

    public GenerationResult setToolChoice(String toolChoice) {
        this.toolChoice = toolChoice == null ? "" : toolChoice;
        return this;
    }

    public Boolean getThinkingEnabled() {
        return thinkingEnabled;
    }

    public GenerationResult setThinkingEnabled(Boolean thinkingEnabled) {
        this.thinkingEnabled = thinkingEnabled;
        return this;
    }

    public List<String> getParentGenerationIds() {
        return parentGenerationIds;
    }

    public GenerationResult setParentGenerationIds(List<String> parentGenerationIds) {
        this.parentGenerationIds.clear();
        if (parentGenerationIds != null) {
            this.parentGenerationIds.addAll(parentGenerationIds);
        }
        return this;
    }

    public String getEffectiveVersion() {
        return effectiveVersion;
    }

    public GenerationResult setEffectiveVersion(String effectiveVersion) {
        this.effectiveVersion = effectiveVersion == null ? "" : effectiveVersion;
        return this;
    }

    public List<Message> getInput() {
        return input;
    }

    public GenerationResult setInput(List<Message> input) {
        this.input.clear();
        if (input != null) {
            this.input.addAll(input);
        }
        return this;
    }

    public List<Message> getOutput() {
        return output;
    }

    public GenerationResult setOutput(List<Message> output) {
        this.output.clear();
        if (output != null) {
            this.output.addAll(output);
        }
        return this;
    }

    public List<ToolDefinition> getTools() {
        return tools;
    }

    public GenerationResult setTools(List<ToolDefinition> tools) {
        this.tools.clear();
        if (tools != null) {
            this.tools.addAll(tools);
        }
        return this;
    }

    public TokenUsage getUsage() {
        return usage;
    }

    public GenerationResult setUsage(TokenUsage usage) {
        this.usage = usage == null ? new TokenUsage() : usage;
        return this;
    }

    public String getStopReason() {
        return stopReason;
    }

    public GenerationResult setStopReason(String stopReason) {
        this.stopReason = stopReason == null ? "" : stopReason;
        return this;
    }

    public Instant getStartedAt() {
        return startedAt;
    }

    public GenerationResult setStartedAt(Instant startedAt) {
        this.startedAt = startedAt;
        return this;
    }

    public Instant getCompletedAt() {
        return completedAt;
    }

    public GenerationResult setCompletedAt(Instant completedAt) {
        this.completedAt = completedAt;
        return this;
    }

    public Map<String, String> getTags() {
        return tags;
    }

    public GenerationResult setTags(Map<String, String> tags) {
        this.tags.clear();
        if (tags != null) {
            this.tags.putAll(tags);
        }
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public GenerationResult setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public List<Artifact> getArtifacts() {
        return artifacts;
    }

    public GenerationResult setArtifacts(List<Artifact> artifacts) {
        this.artifacts.clear();
        if (artifacts != null) {
            this.artifacts.addAll(artifacts);
        }
        return this;
    }

    public String getCallError() {
        return callError;
    }

    public GenerationResult setCallError(String callError) {
        this.callError = callError == null ? "" : callError;
        return this;
    }

    public GenerationResult copy() {
        GenerationResult out = new GenerationResult()
                .setId(id)
                .setConversationId(conversationId)
                .setConversationTitle(conversationTitle)
                .setUserId(userId)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setMode(mode)
                .setOperationName(operationName)
                .setModel(model.copy())
                .setResponseId(responseId)
                .setResponseModel(responseModel)
                .setSystemPrompt(systemPrompt)
                .setMaxTokens(maxTokens)
                .setTemperature(temperature)
                .setTopP(topP)
                .setToolChoice(toolChoice)
                .setThinkingEnabled(thinkingEnabled)
                .setParentGenerationIds(parentGenerationIds)
                .setEffectiveVersion(effectiveVersion)
                .setUsage(usage == null ? new TokenUsage() : usage.copy())
                .setStopReason(stopReason)
                .setStartedAt(startedAt)
                .setCompletedAt(completedAt)
                .setCallError(callError);
        for (Message message : input) {
            out.getInput().add(message == null ? new Message() : message.copy());
        }
        for (Message message : output) {
            out.getOutput().add(message == null ? new Message() : message.copy());
        }
        for (ToolDefinition tool : tools) {
            out.getTools().add(tool == null ? new ToolDefinition() : tool.copy());
        }
        out.getTags().putAll(tags);
        out.getMetadata().putAll(metadata);
        for (Artifact artifact : artifacts) {
            out.getArtifacts().add(artifact == null ? new Artifact() : artifact.copy());
        }
        return out;
    }
}
