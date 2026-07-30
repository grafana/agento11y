package com.grafana.agento11y.sdk;

/** Agent Observability HTTP API settings used by non-ingest helper endpoints. */
public final class ApiConfig {
    private String endpoint = "http://localhost:8080";

    public String getEndpoint() {
        return endpoint;
    }

    public ApiConfig setEndpoint(String endpoint) {
        this.endpoint = endpoint == null ? "" : endpoint;
        return this;
    }

    public ApiConfig copy() {
        return new ApiConfig().setEndpoint(endpoint);
    }
}
