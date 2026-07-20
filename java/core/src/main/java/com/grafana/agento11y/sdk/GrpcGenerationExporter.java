package com.grafana.agento11y.sdk;

import io.grpc.CallOptions;
import io.grpc.Channel;
import io.grpc.ClientCall;
import io.grpc.ClientInterceptor;
import io.grpc.ForwardingClientCall;
import io.grpc.ManagedChannel;
import io.grpc.ManagedChannelBuilder;
import io.grpc.Metadata;
import io.grpc.MethodDescriptor;
import java.util.LinkedHashMap;
import java.util.Map;
import java.util.concurrent.TimeUnit;
import agento11y.v1.GenerationIngestServiceGrpc;

/** gRPC exporter for generation ingest. */
public final class GrpcGenerationExporter implements GenerationExporter {
    private final ManagedChannel channel;
    private final GenerationIngestServiceGrpc.GenerationIngestServiceBlockingStub stub;

    public GrpcGenerationExporter(String endpoint, Map<String, String> headers, boolean insecure) {
        Endpoint parsed = parseEndpoint(endpoint);
        ManagedChannelBuilder<?> builder = ManagedChannelBuilder.forTarget(parsed.target);
        if (insecure || parsed.insecure) {
            builder = builder.usePlaintext();
        }
        // gRPC reserves the user-agent metadata key, so the User-Agent travels
        // via the channel user-agent rather than per-call metadata. grpc-java
        // appends its own token after this value.
        Map<String, String> remaining = new LinkedHashMap<>();
        String userAgent = SdkVersion.userAgent();
        if (headers != null) {
            for (Map.Entry<String, String> entry : headers.entrySet()) {
                if (entry.getKey().equalsIgnoreCase("User-Agent")) {
                    if (entry.getValue() != null && !entry.getValue().isBlank()) {
                        userAgent = entry.getValue();
                    }
                    continue;
                }
                remaining.put(entry.getKey(), entry.getValue());
            }
        }
        builder = builder.userAgent(userAgent);
        this.channel = builder.build();
        this.stub = GenerationIngestServiceGrpc
                .newBlockingStub(io.grpc.ClientInterceptors.intercept(channel, new HeadersInterceptor(remaining)));
    }

    @Override
    public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
        return ProtoMapper.fromProtoResponse(
                stub.exportGenerations(ProtoMapper.toProtoRequest(request)),
                request.getGenerations());
    }

    @Override
    public void shutdown() throws InterruptedException {
        channel.shutdown();
        channel.awaitTermination(5, TimeUnit.SECONDS);
    }

    private static Endpoint parseEndpoint(String endpoint) {
        String trimmed = endpoint == null ? "" : endpoint.trim();
        if (trimmed.isEmpty()) {
            throw new IllegalArgumentException("generation export endpoint is required");
        }
        if (trimmed.startsWith("http://")) {
            return new Endpoint(trimmed.substring("http://".length()), true);
        }
        if (trimmed.startsWith("https://")) {
            return new Endpoint(trimmed.substring("https://".length()), false);
        }
        if (trimmed.startsWith("grpc://")) {
            return new Endpoint(trimmed.substring("grpc://".length()), false);
        }
        return new Endpoint(trimmed, false);
    }

    private record Endpoint(String target, boolean insecure) {
    }

    private static final class HeadersInterceptor implements ClientInterceptor {
        private final Map<String, String> headers;

        private HeadersInterceptor(Map<String, String> headers) {
            this.headers = new LinkedHashMap<>();
            if (headers != null) {
                this.headers.putAll(headers);
            }
        }

        @Override
        public <ReqT, RespT> ClientCall<ReqT, RespT> interceptCall(
                MethodDescriptor<ReqT, RespT> method,
                CallOptions callOptions,
                Channel next) {
            return new ForwardingClientCall.SimpleForwardingClientCall<>(next.newCall(method, callOptions)) {
                @Override
                public void start(Listener<RespT> responseListener, Metadata metadata) {
                    for (Map.Entry<String, String> entry : headers.entrySet()) {
                        Metadata.Key<String> key = Metadata.Key.of(entry.getKey().toLowerCase(), Metadata.ASCII_STRING_MARSHALLER);
                        metadata.put(key, entry.getValue());
                    }
                    super.start(responseListener, metadata);
                }
            };
        }
    }
}
