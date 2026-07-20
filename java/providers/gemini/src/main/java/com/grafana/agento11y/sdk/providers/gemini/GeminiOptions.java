package com.grafana.agento11y.sdk.providers.gemini;

import java.util.LinkedHashMap;
import java.util.Map;

/** Shared Gemini provider wrapper options. */
public final class GeminiOptions {
    private String conversationId = "";
    private String agentName = "";
    private String agentVersion = "";
    private final Map<String, String> tags = new LinkedHashMap<>();
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private boolean rawArtifacts;

    public String getConversationId() {
        return conversationId;
    }

    public GeminiOptions setConversationId(String conversationId) {
        this.conversationId = conversationId == null ? "" : conversationId;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public GeminiOptions setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public GeminiOptions setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public Map<String, String> getTags() {
        return tags;
    }

    public GeminiOptions setTags(Map<String, String> tags) {
        this.tags.clear();
        if (tags != null) {
            this.tags.putAll(tags);
        }
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public GeminiOptions setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public boolean isRawArtifacts() {
        return rawArtifacts;
    }

    public GeminiOptions setRawArtifacts(boolean rawArtifacts) {
        this.rawArtifacts = rawArtifacts;
        return this;
    }
}
