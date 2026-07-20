package com.grafana.agento11y.sdk;

import java.util.HashMap;
import java.util.Map;

/** Options for {@link Agento11yClient#executeToolCalls}. */
public final class ExecuteToolCallsOptions {
    private String conversationId = "";
    private String conversationTitle = "";
    private String agentName = "";
    private String agentVersion = "";
    private ContentCaptureMode contentCapture = ContentCaptureMode.DEFAULT;
    private String requestModel = "";
    private String requestProvider = "";
    private String toolType = "function";
    private final Map<String, String> tags = new HashMap<>();

    public String getConversationId() {
        return conversationId;
    }

    public ExecuteToolCallsOptions setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getConversationTitle() {
        return conversationTitle;
    }

    public ExecuteToolCallsOptions setConversationTitle(String conversationTitle) {
        this.conversationTitle = conversationTitle == null ? "" : conversationTitle;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public ExecuteToolCallsOptions setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public ExecuteToolCallsOptions setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public ContentCaptureMode getContentCapture() {
        return contentCapture;
    }

    public ExecuteToolCallsOptions setContentCapture(ContentCaptureMode contentCapture) {
        this.contentCapture = contentCapture == null ? ContentCaptureMode.DEFAULT : contentCapture;
        return this;
    }

    public String getRequestModel() {
        return requestModel;
    }

    public ExecuteToolCallsOptions setRequestModel(String requestModel) {
        this.requestModel = requestModel == null ? "" : requestModel;
        return this;
    }

    public String getRequestProvider() {
        return requestProvider;
    }

    public ExecuteToolCallsOptions setRequestProvider(String requestProvider) {
        this.requestProvider = requestProvider == null ? "" : requestProvider;
        return this;
    }

    public String getToolType() {
        return toolType;
    }

    public ExecuteToolCallsOptions setToolType(String toolType) {
        this.toolType = toolType == null || toolType.isBlank() ? "function" : toolType;
        return this;
    }

    /** Reserved for forward compatibility; not applied to tool spans in this release. */
    public Map<String, String> getTags() {
        return tags;
    }

    public ExecuteToolCallsOptions copy() {
        ExecuteToolCallsOptions out = new ExecuteToolCallsOptions()
                .setConversationId(conversationId)
                .setConversationTitle(conversationTitle)
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setContentCapture(contentCapture)
                .setRequestModel(requestModel)
                .setRequestProvider(requestProvider)
                .setToolType(toolType);
        out.getTags().putAll(tags);
        return out;
    }
}
