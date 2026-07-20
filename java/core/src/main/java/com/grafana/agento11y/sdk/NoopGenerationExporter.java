package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.List;

/** Generation exporter that accepts batches without sending network traffic. */
final class NoopGenerationExporter implements GenerationExporter {
    @Override
    public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
        List<ExportGenerationResult> results = new ArrayList<>();
        for (Generation generation : request.getGenerations()) {
            results.add(new ExportGenerationResult()
                    .setGenerationId(generation == null ? "" : generation.getId())
                    .setAccepted(true));
        }
        return new ExportGenerationsResponse().setResults(results);
    }
}
