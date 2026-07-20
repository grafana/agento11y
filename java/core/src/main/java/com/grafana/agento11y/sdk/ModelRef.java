package com.grafana.agento11y.sdk;

/** Provider/model identity. */
public final class ModelRef {
    private String provider = "";
    private String name = "";

    public String getProvider() {
        return provider;
    }

    public ModelRef setProvider(String provider) {
        this.provider = provider == null ? "" : provider;
        return this;
    }

    public String getName() {
        return name;
    }

    public ModelRef setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public ModelRef copy() {
        return new ModelRef().setProvider(provider).setName(name);
    }
}
