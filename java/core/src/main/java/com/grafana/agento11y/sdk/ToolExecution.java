package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Normalized tool execution snapshot record. */
public final class ToolExecution {
    private String toolName = "";
    private String toolCallId = "";
    private String toolType = "";
    private String toolDescription = "";
    private String conversationId = "";
    private String agentName = "";
    private String agentVersion = "";
    private boolean includeContent;
    private Instant startedAt;
    private Instant completedAt;
    private Object arguments;
    private Object result;
    private String callError = "";

    public String getToolName() {
        return toolName;
    }

    public ToolExecution setToolName(String toolName) {
        this.toolName = toolName == null ? "" : toolName;
        return this;
    }

    public String getToolCallId() {
        return toolCallId;
    }

    public ToolExecution setToolCallId(String toolCallId) {
        this.toolCallId = toolCallId == null ? "" : toolCallId;
        return this;
    }

    public String getToolType() {
        return toolType;
    }

    public ToolExecution setToolType(String toolType) {
        this.toolType = toolType == null ? "" : toolType;
        return this;
    }

    public String getToolDescription() {
        return toolDescription;
    }

    public ToolExecution setToolDescription(String toolDescription) {
        this.toolDescription = toolDescription == null ? "" : toolDescription;
        return this;
    }

    public String getConversationId() {
        return conversationId;
    }

    public ToolExecution setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public ToolExecution setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public ToolExecution setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public boolean isIncludeContent() {
        return includeContent;
    }

    public ToolExecution setIncludeContent(boolean includeContent) {
        this.includeContent = includeContent;
        return this;
    }

    public Instant getStartedAt() {
        return startedAt;
    }

    public ToolExecution setStartedAt(Instant startedAt) {
        this.startedAt = startedAt;
        return this;
    }

    public Instant getCompletedAt() {
        return completedAt;
    }

    public ToolExecution setCompletedAt(Instant completedAt) {
        this.completedAt = completedAt;
        return this;
    }

    public Object getArguments() {
        return arguments;
    }

    public ToolExecution setArguments(Object arguments) {
        this.arguments = arguments;
        return this;
    }

    public Object getResult() {
        return result;
    }

    public ToolExecution setResult(Object result) {
        this.result = result;
        return this;
    }

    public String getCallError() {
        return callError;
    }

    public ToolExecution setCallError(String callError) {
        this.callError = callError == null ? "" : callError;
        return this;
    }

    public ToolExecution copy() {
        return new ToolExecution()
                .setToolName(toolName)
                .setToolCallId(toolCallId)
                .setToolType(toolType)
                .setToolDescription(toolDescription)
                .setConversationId(conversationId)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setIncludeContent(includeContent)
                .setStartedAt(startedAt)
                .setCompletedAt(completedAt)
                .setArguments(arguments)
                .setResult(result)
                .setCallError(callError);
    }
}
