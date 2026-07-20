package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.List;

/** Generation export response payload. */
public final class ExportGenerationsResponse {
    private final List<ExportGenerationResult> results = new ArrayList<>();

    public List<ExportGenerationResult> getResults() {
        return results;
    }

    public ExportGenerationsResponse setResults(List<ExportGenerationResult> results) {
        this.results.clear();
        if (results != null) {
            this.results.addAll(results);
        }
        return this;
    }
}
