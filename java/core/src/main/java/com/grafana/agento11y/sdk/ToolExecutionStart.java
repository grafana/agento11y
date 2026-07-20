package com.grafana.agento11y.sdk;

import java.time.Instant;

/** Seed fields for a tool execution span. */
public final class ToolExecutionStart {
    private String toolName = "";
    private String toolCallId = "";
    private String toolType = "";
    private String toolDescription = "";
    private String conversationId = "";
    private String conversationTitle = "";
    private String agentName = "";
    private String agentVersion = "";
    private String requestModel = "";
    private String requestProvider = "";
    private ContentCaptureMode contentCapture = ContentCaptureMode.DEFAULT;
    private boolean includeContent;
    private Instant startedAt;

    public String getToolName() {
        return toolName;
    }

    public ToolExecutionStart setToolName(String toolName) {
        this.toolName = toolName == null ? "" : toolName;
        return this;
    }

    public String getToolCallId() {
        return toolCallId;
    }

    public ToolExecutionStart setToolCallId(String toolCallId) {
        this.toolCallId = toolCallId == null ? "" : toolCallId;
        return this;
    }

    public String getToolType() {
        return toolType;
    }

    public ToolExecutionStart setToolType(String toolType) {
        this.toolType = toolType == null ? "" : toolType;
        return this;
    }

    public String getToolDescription() {
        return toolDescription;
    }

    public ToolExecutionStart setToolDescription(String toolDescription) {
        this.toolDescription = toolDescription == null ? "" : toolDescription;
        return this;
    }

    public String getConversationId() {
        return conversationId;
    }

    public ToolExecutionStart setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getConversationTitle() {
        return conversationTitle;
    }

    public ToolExecutionStart setConversationTitle(String conversationTitle) {
        this.conversationTitle = conversationTitle == null ? "" : conversationTitle;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public ToolExecutionStart setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public ToolExecutionStart setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    /** Returns the model that requested the tool call. */
    public String getRequestModel() {
        return requestModel;
    }

    /** Sets the model that requested the tool call (e.g. "gpt-5"). */
    public ToolExecutionStart setRequestModel(String requestModel) {
        this.requestModel = requestModel == null ? "" : requestModel;
        return this;
    }

    /** Returns the provider that served the model. */
    public String getRequestProvider() {
        return requestProvider;
    }

    /** Sets the provider that served the model (e.g. "openai"). */
    public ToolExecutionStart setRequestProvider(String requestProvider) {
        this.requestProvider = requestProvider == null ? "" : requestProvider;
        return this;
    }

    public ContentCaptureMode getContentCapture() {
        return contentCapture;
    }

    public ToolExecutionStart setContentCapture(ContentCaptureMode contentCapture) {
        this.contentCapture = contentCapture == null ? ContentCaptureMode.DEFAULT : contentCapture;
        return this;
    }

    /** @deprecated Use {@link #setContentCapture(ContentCaptureMode)} instead. */
    @Deprecated
    public boolean isIncludeContent() {
        return includeContent;
    }

    /** @deprecated Use {@link #setContentCapture(ContentCaptureMode)} instead. */
    @Deprecated
    public ToolExecutionStart setIncludeContent(boolean includeContent) {
        this.includeContent = includeContent;
        return this;
    }

    public Instant getStartedAt() {
        return startedAt;
    }

    public ToolExecutionStart setStartedAt(Instant startedAt) {
        this.startedAt = startedAt;
        return this;
    }

    public ToolExecutionStart copy() {
        return new ToolExecutionStart()
                .setToolName(toolName)
                .setToolCallId(toolCallId)
                .setToolType(toolType)
                .setToolDescription(toolDescription)
                .setConversationId(conversationId)
                .setConversationTitle(conversationTitle)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setRequestModel(requestModel)
                .setRequestProvider(requestProvider)
                .setContentCapture(contentCapture)
                .setIncludeContent(includeContent)
                .setStartedAt(startedAt);
    }
}
