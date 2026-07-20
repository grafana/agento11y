package com.grafana.agento11y.sdk.providers.anthropic;

import java.util.LinkedHashMap;
import java.util.Map;

/** Shared Anthropic provider wrapper options. */
public final class AnthropicOptions {
    private String conversationId = "";
    private String agentName = "";
    private String agentVersion = "";
    private final Map<String, String> tags = new LinkedHashMap<>();
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private boolean rawArtifacts;

    public String getConversationId() {
        return conversationId;
    }

    public AnthropicOptions setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public AnthropicOptions setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public AnthropicOptions setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public Map<String, String> getTags() {
        return tags;
    }

    public AnthropicOptions setTags(Map<String, String> tags) {
        this.tags.clear();
        if (tags != null) {
            this.tags.putAll(tags);
        }
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public AnthropicOptions setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public boolean isRawArtifacts() {
        return rawArtifacts;
    }

    public AnthropicOptions setRawArtifacts(boolean rawArtifacts) {
        this.rawArtifacts = rawArtifacts;
        return this;
    }
}
