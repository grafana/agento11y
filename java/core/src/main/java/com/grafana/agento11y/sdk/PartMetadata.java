package com.grafana.agento11y.sdk;

/** Provider-specific message part metadata. */
public final class PartMetadata {
    private String providerType = "";

    public String getProviderType() {
        return providerType;
    }

    public PartMetadata setProviderType(String providerType) {
        this.providerType = providerType == null ? "" : providerType;
        return this;
    }

    public PartMetadata copy() {
        return new PartMetadata().setProviderType(providerType);
    }
}
