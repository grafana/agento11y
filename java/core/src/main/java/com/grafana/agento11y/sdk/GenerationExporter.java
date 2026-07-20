package com.grafana.agento11y.sdk;

/** Pluggable generation exporter transport. */
public interface GenerationExporter extends AutoCloseable {
    ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) throws Exception;

    default void shutdown() throws Exception {
    }

    @Override
    default void close() throws Exception {
        shutdown();
    }
}
