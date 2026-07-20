package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.stub.StreamObserver;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.time.Duration;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import agento11y.v1.GenerationIngest;
import agento11y.v1.GenerationIngestServiceGrpc;

/**
 * Real-gRPC content-capture test env.
 *
 * <p>Spins up an in-process gRPC ingest server that records the exported
 * {@link GenerationIngest.Generation} payloads as they actually leave the SDK,
 * plus an {@link InMemorySpanExporter} for OTel span assertions. Use this when
 * a test needs to assert on both the proto export and the span path (the
 * proto/span split that {@link ContentCaptureMode#FULL_WITH_METADATA_SPANS}
 * introduces).
 */
final class ContentCaptureTestEnv implements AutoCloseable {

    private final Server server;
    private final InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
    private final SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
            .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
            .build();
    private final List<GenerationIngest.ExportGenerationsRequest> requests = new CopyOnWriteArrayList<>();
    private boolean closed;
    private boolean clientShutdown;

    final Agento11yClient client;

    private ContentCaptureTestEnv(Builder builder) {
        GenerationIngestServiceGrpc.GenerationIngestServiceImplBase service =
                new GenerationIngestServiceGrpc.GenerationIngestServiceImplBase() {
                    @Override
                    public void exportGenerations(
                            GenerationIngest.ExportGenerationsRequest request,
                            StreamObserver<GenerationIngest.ExportGenerationsResponse> responseObserver) {
                        requests.add(request);
                        GenerationIngest.ExportGenerationsResponse.Builder responseBuilder =
                                GenerationIngest.ExportGenerationsResponse.newBuilder();
                        for (GenerationIngest.Generation generation : request.getGenerationsList()) {
                            responseBuilder.addResults(GenerationIngest.ExportGenerationResult.newBuilder()
                                    .setGenerationId(generation.getId())
                                    .setAccepted(true)
                                    .build());
                        }
                        responseObserver.onNext(responseBuilder.build());
                        responseObserver.onCompleted();
                    }
                };
        try {
            server = ServerBuilder.forPort(0).addService(service).build().start();
        } catch (java.io.IOException e) {
            throw new RuntimeException("failed to start in-process gRPC server", e);
        }

        Agento11yClientConfig config = new Agento11yClientConfig()
                .setTracer(tracerProvider.get("agento11y-content-capture-test"))
                .setContentCapture(builder.contentCapture)
                .setContentCaptureResolver(builder.contentCaptureResolver)
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.GRPC)
                        .setEndpoint("127.0.0.1:" + server.getPort())
                        .setInsecure(true)
                        .setBatchSize(1)
                        .setQueueSize(10)
                        .setFlushInterval(Duration.ofHours(1))
                        .setMaxRetries(1)
                        .setInitialBackoff(Duration.ofMillis(1))
                        .setMaxBackoff(Duration.ofMillis(2)));
        if (builder.embeddingCapture != null) {
            config.setEmbeddingCapture(builder.embeddingCapture);
        }
        client = new Agento11yClient(config);
    }

    static Builder builder(ContentCaptureMode contentCapture) {
        return new Builder(contentCapture);
    }

    /**
     * Flushes the client and returns the only proto Generation the gRPC server
     * received. Safe to call alongside {@link #close()}; the tracer provider
     * stays alive so span assertions can run afterwards.
     */
    GenerationIngest.Generation singleGeneration() {
        flushClient();
        assertThat(requests).hasSize(1);
        assertThat(requests.get(0).getGenerationsCount()).isEqualTo(1);
        return requests.get(0).getGenerations(0);
    }

    private void flushClient() {
        if (!clientShutdown) {
            clientShutdown = true;
            client.shutdown();
        }
    }

    SpanData generationSpan() {
        return singleSpanByOperation("generateText");
    }

    SpanData streamingGenerationSpan() {
        return singleSpanByOperation("streamText");
    }

    SpanData embeddingSpan() {
        return singleSpanByOperation("embeddings");
    }

    SpanData toolSpan() {
        return singleSpanByOperation("execute_tool");
    }

    private SpanData singleSpanByOperation(String operationName) {
        List<SpanData> spans = spanExporter.getFinishedSpanItems().stream()
                .filter(span -> operationName.equals(
                        span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME))))
                .toList();
        assertThat(spans).as("span for operation %s", operationName).isNotEmpty();
        return spans.get(spans.size() - 1);
    }

    @Override
    public void close() {
        if (closed) {
            return;
        }
        closed = true;
        flushClient();
        server.shutdownNow();
        tracerProvider.shutdown();
    }

    static final class Builder {
        private final ContentCaptureMode contentCapture;
        private ContentCaptureResolver contentCaptureResolver;
        private EmbeddingCaptureConfig embeddingCapture;

        private Builder(ContentCaptureMode contentCapture) {
            this.contentCapture = contentCapture;
        }

        Builder contentCaptureResolver(ContentCaptureResolver resolver) {
            this.contentCaptureResolver = resolver;
            return this;
        }

        Builder embeddingCapture(EmbeddingCaptureConfig embeddingCapture) {
            this.embeddingCapture = embeddingCapture;
            return this;
        }

        ContentCaptureTestEnv build() {
            return new ContentCaptureTestEnv(this);
        }
    }
}
