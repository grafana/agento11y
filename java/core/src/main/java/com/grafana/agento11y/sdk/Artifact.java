package com.grafana.agento11y.sdk;

import java.util.Arrays;

/** Optional raw provider artifact. */
public final class Artifact {
    private ArtifactKind kind = ArtifactKind.REQUEST;
    private String name = "";
    private String contentType = "";
    private byte[] payload = new byte[0];
    private String recordId = "";
    private String uri = "";

    public ArtifactKind getKind() {
        return kind;
    }

    public Artifact setKind(ArtifactKind kind) {
        this.kind = kind == null ? ArtifactKind.REQUEST : kind;
        return this;
    }

    public String getName() {
        return name;
    }

    public Artifact setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public String getContentType() {
        return contentType;
    }

    public Artifact setContentType(String contentType) {
        this.contentType = contentType == null ? "" : contentType;
        return this;
    }

    public byte[] getPayload() {
        return payload;
    }

    public Artifact setPayload(byte[] payload) {
        this.payload = payload == null ? new byte[0] : Arrays.copyOf(payload, payload.length);
        return this;
    }

    public String getRecordId() {
        return recordId;
    }

    public Artifact setRecordId(String recordId) {
        this.recordId = recordId == null ? "" : recordId;
        return this;
    }

    public String getUri() {
        return uri;
    }

    public Artifact setUri(String uri) {
        this.uri = uri == null ? "" : uri;
        return this;
    }

    public Artifact copy() {
        return new Artifact()
                .setKind(kind)
                .setName(name)
                .setContentType(contentType)
                .setPayload(payload)
                .setRecordId(recordId)
                .setUri(uri);
    }
}
