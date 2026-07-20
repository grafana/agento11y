package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.List;

/** Generation export request payload. */
public final class ExportGenerationsRequest {
    private final List<Generation> generations = new ArrayList<>();

    public List<Generation> getGenerations() {
        return generations;
    }

    public ExportGenerationsRequest setGenerations(List<Generation> generations) {
        this.generations.clear();
        if (generations != null) {
            this.generations.addAll(generations);
        }
        return this;
    }
}
