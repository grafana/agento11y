package com.grafana.agento11y.sdk;

/** Per-item generation ingest result. */
public final class ExportGenerationResult {
    private String generationId = "";
    private boolean accepted;
    private String error = "";

    public String getGenerationId() {
        return generationId;
    }

    public ExportGenerationResult setGenerationId(String generationId) {
        this.generationId = generationId == null ? "" : generationId;
        return this;
    }

    public boolean isAccepted() {
        return accepted;
    }

    public ExportGenerationResult setAccepted(boolean accepted) {
        this.accepted = accepted;
        return this;
    }

    public String getError() {
        return error;
    }

    public ExportGenerationResult setError(String error) {
        this.error = error == null ? "" : error;
        return this;
    }
}
