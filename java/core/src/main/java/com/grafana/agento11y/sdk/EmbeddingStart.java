package com.grafana.agento11y.sdk;

import java.time.Instant;
import java.util.LinkedHashMap;
import java.util.Map;

/** Seed fields captured when embedding recording starts. */
public final class EmbeddingStart {
    private ModelRef model = new ModelRef();
    private String agentName = "";
    private String agentVersion = "";
    private Long dimensions;
    private String encodingFormat = "";
    private final Map<String, String> tags = new LinkedHashMap<>();
    private final Map<String, Object> metadata = new LinkedHashMap<>();
    private Instant startedAt;

    public ModelRef getModel() {
        return model;
    }

    public EmbeddingStart setModel(ModelRef model) {
        this.model = model == null ? new ModelRef() : model;
        return this;
    }

    public String getAgentName() {
        return agentName;
    }

    public EmbeddingStart setAgentName(String agentName) {
        this.agentName = agentName == null ? "" : agentName;
        return this;
    }

    public String getAgentVersion() {
        return agentVersion;
    }

    public EmbeddingStart setAgentVersion(String agentVersion) {
        this.agentVersion = agentVersion == null ? "" : agentVersion;
        return this;
    }

    public Long getDimensions() {
        return dimensions;
    }

    public EmbeddingStart setDimensions(Long dimensions) {
        this.dimensions = dimensions;
        return this;
    }

    public String getEncodingFormat() {
        return encodingFormat;
    }

    public EmbeddingStart setEncodingFormat(String encodingFormat) {
        this.encodingFormat = encodingFormat == null ? "" : encodingFormat;
        return this;
    }

    public Map<String, String> getTags() {
        return tags;
    }

    public EmbeddingStart setTags(Map<String, String> tags) {
        this.tags.clear();
        if (tags != null) {
            this.tags.putAll(tags);
        }
        return this;
    }

    public Map<String, Object> getMetadata() {
        return metadata;
    }

    public EmbeddingStart setMetadata(Map<String, Object> metadata) {
        this.metadata.clear();
        if (metadata != null) {
            this.metadata.putAll(metadata);
        }
        return this;
    }

    public Instant getStartedAt() {
        return startedAt;
    }

    public EmbeddingStart setStartedAt(Instant startedAt) {
        this.startedAt = startedAt;
        return this;
    }

    public EmbeddingStart copy() {
        EmbeddingStart out = new EmbeddingStart()
                .setModel(model.copy())
                .setAgentName(agentName)
                .setAgentVersion(agentVersion)
                .setDimensions(dimensions)
                .setEncodingFormat(encodingFormat)
                .setStartedAt(startedAt);
        out.getTags().putAll(tags);
        out.getMetadata().putAll(metadata);
        return out;
    }
}
