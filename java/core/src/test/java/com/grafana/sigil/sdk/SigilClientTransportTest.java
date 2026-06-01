package com.grafana.sigil.sdk;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.fasterxml.jackson.databind.JsonNode;
import com.google.protobuf.util.JsonFormat;
import io.grpc.Metadata;
import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.ServerCall;
import io.grpc.ServerCallHandler;
import io.grpc.ServerInterceptor;
import io.grpc.ServerInterceptors;
import io.grpc.stub.StreamObserver;
import io.opentelemetry.api.GlobalOpenTelemetry;
import java.io.IOException;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.time.Duration;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.Random;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicReference;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import sigil.v1.GenerationIngest;
import sigil.v1.GenerationIngestServiceGrpc;

class SigilClientTransportTest {
    @Test
    void exportsGenerationOverHttpRoundTripAndSetsAuthHeaders() throws Exception {
        AtomicReference<String> path = new AtomicReference<>();
        AtomicReference<Map<String, String>> headers = new AtomicReference<>();
        AtomicReference<JsonNode> payload = new AtomicReference<>();
        AtomicReference<Generation> lastGeneration = new AtomicReference<>();

        com.sun.net.httpserver.HttpServer server = com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/api/v1/generations:export", exchange -> {
            path.set(exchange.getRequestURI().getPath());

            Map<String, String> requestHeaders = new LinkedHashMap<>();
            exchange.getRequestHeaders().forEach((k, v) -> requestHeaders.put(k.toLowerCase(), String.join(",", v)));
            headers.set(requestHeaders);

            byte[] body = exchange.getRequestBody().readAllBytes();
            payload.set(Json.MAPPER.readTree(body));

            byte[] response = "{\"results\":[{\"generation_id\":\"gen-fixture-1\",\"accepted\":true}]}".getBytes();
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(202, response.length);
            try (OutputStream outputStream = exchange.getResponseBody()) {
                outputStream.write(response);
            }
        });
        server.start();

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.HTTP)
                        .setEndpoint("http://127.0.0.1:" + server.getAddress().getPort() + "/api/v1/generations:export")
                        .setAuth(new AuthConfig().setMode(AuthMode.TENANT).setTenantId("tenant-a"))
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            lastGeneration.set(recorder.lastGeneration().orElseThrow());
            client.shutdown();
        } finally {
            server.stop(0);
        }

        GenerationIngest.ExportGenerationsRequest.Builder requestBuilder = GenerationIngest.ExportGenerationsRequest.newBuilder();
        JsonFormat.parser().ignoringUnknownFields().merge(payload.get().toString(), requestBuilder);
        GenerationIngest.ExportGenerationsRequest request = requestBuilder.build();

        assertThat(path.get()).isEqualTo("/api/v1/generations:export");
        assertThat(headers.get().get("x-scope-orgid")).isEqualTo("tenant-a");
        assertThat(headers.get().get("user-agent")).isEqualTo(SdkVersion.userAgent());
        assertThat(request.getGenerationsCount()).isEqualTo(1);
        assertThat(request.getGenerations(0)).isEqualTo(ProtoMapper.toProtoGeneration(lastGeneration.get()));
    }

    static Stream<Arguments> httpUserAgentCases() {
        String defaultUserAgent = SdkVersion.userAgent();
        String override = "sigil-plugin-langchain4j/1.2.3 " + defaultUserAgent;
        // A non-blank caller User-Agent wins; a blank or whitespace-only one must
        // not blank out the default. A null header value exercises the no-header
        // path.
        return Stream.of(
                Arguments.of("no header", null, defaultUserAgent),
                Arguments.of("non-blank override", override, override),
                Arguments.of("empty falls back to default", "", defaultUserAgent),
                Arguments.of("whitespace falls back to default", "   ", defaultUserAgent));
    }

    @ParameterizedTest(name = "{0}")
    @MethodSource("httpUserAgentCases")
    void httpExportResolvesUserAgent(String name, String headerValue, String wantUserAgent) throws Exception {
        AtomicReference<Map<String, String>> headers = new AtomicReference<>();

        com.sun.net.httpserver.HttpServer server =
                com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/api/v1/generations:export", exchange -> {
            Map<String, String> requestHeaders = new LinkedHashMap<>();
            exchange.getRequestHeaders().forEach((k, v) -> requestHeaders.put(k.toLowerCase(), String.join(",", v)));
            headers.set(requestHeaders);
            exchange.getRequestBody().readAllBytes();

            byte[] response = "{\"results\":[{\"generation_id\":\"gen-fixture-1\",\"accepted\":true}]}".getBytes();
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(202, response.length);
            try (OutputStream outputStream = exchange.getResponseBody()) {
                outputStream.write(response);
            }
        });
        server.start();

        GenerationExportConfig exportConfig = new GenerationExportConfig()
                .setProtocol(GenerationExportProtocol.HTTP)
                .setEndpoint("http://127.0.0.1:" + server.getAddress().getPort() + "/api/v1/generations:export")
                .setBatchSize(1)
                .setFlushInterval(Duration.ofMinutes(10))
                .setMaxRetries(0);
        if (headerValue != null) {
            exportConfig.setHeaders(Map.of("User-Agent", headerValue));
        }

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExport(exportConfig);

        try (SigilClient client = new SigilClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            client.shutdown();
        } finally {
            server.stop(0);
        }

        assertThat(headers.get().get("user-agent")).isEqualTo(wantUserAgent);
    }

    @Test
    void exportsGenerationOverGrpcRoundTripAndHonorsHeaderOverride() throws Exception {
        List<GenerationIngest.ExportGenerationsRequest> requests = new CopyOnWriteArrayList<>();
        AtomicReference<Map<String, String>> metadata = new AtomicReference<>(new LinkedHashMap<>());
        AtomicReference<Generation> lastGeneration = new AtomicReference<>();

        GenerationIngestServiceGrpc.GenerationIngestServiceImplBase service = new GenerationIngestServiceGrpc.GenerationIngestServiceImplBase() {
            @Override
            public void exportGenerations(
                    GenerationIngest.ExportGenerationsRequest request,
                    StreamObserver<GenerationIngest.ExportGenerationsResponse> responseObserver) {
                requests.add(request);
                List<GenerationIngest.ExportGenerationResult> results = new ArrayList<>();
                for (GenerationIngest.Generation generation : request.getGenerationsList()) {
                    results.add(GenerationIngest.ExportGenerationResult.newBuilder()
                            .setGenerationId(generation.getId())
                            .setAccepted(true)
                            .build());
                }
                responseObserver.onNext(GenerationIngest.ExportGenerationsResponse.newBuilder().addAllResults(results).build());
                responseObserver.onCompleted();
            }
        };

        ServerInterceptor interceptor = new ServerInterceptor() {
            @Override
            public <ReqT, RespT> ServerCall.Listener<ReqT> interceptCall(
                    ServerCall<ReqT, RespT> call,
                    Metadata headers,
                    ServerCallHandler<ReqT, RespT> next) {
                Map<String, String> seen = new LinkedHashMap<>();
                for (String key : headers.keys()) {
                    if (key.endsWith(Metadata.BINARY_HEADER_SUFFIX)) {
                        continue;
                    }
                    Metadata.Key<String> metaKey = Metadata.Key.of(key, Metadata.ASCII_STRING_MARSHALLER);
                    seen.put(key, headers.get(metaKey));
                }
                metadata.set(seen);
                return next.startCall(call, headers);
            }
        };

        Server server = ServerBuilder.forPort(0)
                .addService(ServerInterceptors.intercept(service, interceptor))
                .build()
                .start();

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.GRPC)
                        .setEndpoint("127.0.0.1:" + server.getPort())
                        .setInsecure(true)
                        .setHeaders(Map.of("authorization", "Bearer override-token"))
                        .setAuth(new AuthConfig().setMode(AuthMode.BEARER).setBearerToken("ignored"))
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();
            lastGeneration.set(recorder.lastGeneration().orElseThrow());
            client.shutdown();
        } finally {
            server.shutdownNow();
        }

        assertThat(requests).hasSize(1);
        assertThat(requests.get(0).getGenerationsCount()).isEqualTo(1);
        assertThat(requests.get(0).getGenerations(0)).isEqualTo(ProtoMapper.toProtoGeneration(lastGeneration.get()));
        assertThat(requests.get(0).getGenerations(0).getOutput(0).getParts(0).hasThinking()).isTrue();
        assertThat(requests.get(0).getGenerations(0).getOutput(0).getParts(1).hasToolCall()).isTrue();
        assertThat(requests.get(0).getGenerations(0).getOutput(1).getParts(0).hasToolResult()).isTrue();
        assertThat(metadata.get().get("authorization")).isEqualTo("Bearer override-token");
        // grpc-java appends its own token after ours.
        assertThat(metadata.get().get("user-agent")).startsWith(SdkVersion.userAgent());
    }

    @Test
    void recordsGenerationWhenExportProtocolIsNone() throws Exception {
        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.NONE)
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            GenerationRecorder recorder = client.startGeneration(TestFixtures.startFixture());
            recorder.setResult(TestFixtures.resultFixture());
            recorder.end();

            assertThat(recorder.error()).isEmpty();
            assertThat(recorder.lastGeneration()).isPresent();

            client.flush();
            client.shutdown();
        }
    }

    @Test
    void submitsConversationRatingOverHttpAndSetsAuthHeaders() throws Exception {
        AtomicReference<String> path = new AtomicReference<>();
        AtomicReference<Map<String, String>> headers = new AtomicReference<>();
        AtomicReference<JsonNode> payload = new AtomicReference<>();

        com.sun.net.httpserver.HttpServer server = com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/api/v1/conversations/conv-1/ratings", exchange -> {
            path.set(exchange.getRequestURI().getPath());

            Map<String, String> requestHeaders = new LinkedHashMap<>();
            exchange.getRequestHeaders().forEach((k, v) -> requestHeaders.put(k.toLowerCase(), String.join(",", v)));
            headers.set(requestHeaders);

            byte[] body = exchange.getRequestBody().readAllBytes();
            payload.set(Json.MAPPER.readTree(body));

            byte[] response = """
                    {
                      "rating":{
                        "rating_id":"rat-1",
                        "conversation_id":"conv-1",
                        "rating":"CONVERSATION_RATING_VALUE_BAD",
                        "created_at":"2026-02-13T12:00:00Z"
                      },
                      "summary":{
                        "total_count":1,
                        "good_count":0,
                        "bad_count":1,
                        "latest_rating":"CONVERSATION_RATING_VALUE_BAD",
                        "latest_rated_at":"2026-02-13T12:00:00Z",
                        "has_bad_rating":true
                      }
                    }
                    """.getBytes();
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, response.length);
            try (OutputStream outputStream = exchange.getResponseBody()) {
                outputStream.write(response);
            }
        });
        server.start();

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setApi(new ApiConfig().setEndpoint("http://127.0.0.1:" + server.getAddress().getPort()))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.GRPC)
                        .setEndpoint("localhost:4317")
                        .setAuth(new AuthConfig().setMode(AuthMode.TENANT).setTenantId("tenant-a"))
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            SubmitConversationRatingResponse response = client.submitConversationRating(
                    "conv-1",
                    new SubmitConversationRatingRequest()
                            .setRatingId("rat-1")
                            .setRating(ConversationRatingValue.BAD)
                            .setComment("wrong answer")
                            .setMetadata(Map.of("channel", "assistant")));

            assertThat(response.getRating().getRatingId()).isEqualTo("rat-1");
            assertThat(response.getSummary().isHasBadRating()).isTrue();
        } finally {
            server.stop(0);
        }

        assertThat(path.get()).isEqualTo("/api/v1/conversations/conv-1/ratings");
        assertThat(headers.get().get("x-scope-orgid")).isEqualTo("tenant-a");
        assertThat(payload.get().path("rating_id").asText()).isEqualTo("rat-1");
        assertThat(payload.get().path("rating").asText()).isEqualTo("CONVERSATION_RATING_VALUE_BAD");
        assertThat(payload.get().path("comment").asText()).isEqualTo("wrong answer");
    }

    @Test
    void submitConversationRatingMapsConflictAndValidationErrors() {
        com.sun.net.httpserver.HttpServer server;
        try {
            server = com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        } catch (IOException exception) {
            throw new RuntimeException(exception);
        }
        server.createContext("/api/v1/conversations/conv-1/ratings", exchange -> {
            byte[] response = "idempotency conflict".getBytes();
            exchange.sendResponseHeaders(409, response.length);
            try (OutputStream outputStream = exchange.getResponseBody()) {
                outputStream.write(response);
            }
        });
        server.start();

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setApi(new ApiConfig().setEndpoint("http://127.0.0.1:" + server.getAddress().getPort()))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.HTTP)
                        .setEndpoint("http://127.0.0.1:" + server.getAddress().getPort() + "/api/v1/generations:export")
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            assertThatThrownBy(() -> client.submitConversationRating(
                    "conv-1",
                    new SubmitConversationRatingRequest()
                            .setRatingId("rat-1")
                            .setRating(ConversationRatingValue.GOOD)))
                    .isInstanceOf(RatingConflictException.class);

            assertThatThrownBy(() -> client.submitConversationRating(
                    "conv-1",
                    new SubmitConversationRatingRequest()
                            .setRatingId("")
                            .setRating(ConversationRatingValue.GOOD)))
                    .isInstanceOf(ValidationException.class);
        } finally {
            server.stop(0);
        }
    }

    @Test
    void submitConversationRatingAppliesBearerHeaderOverride() throws Exception {
        AtomicReference<Map<String, String>> headers = new AtomicReference<>(new LinkedHashMap<>());

        com.sun.net.httpserver.HttpServer server = com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
        server.createContext("/api/v1/conversations/conv-1/ratings", exchange -> {
            Map<String, String> requestHeaders = new LinkedHashMap<>();
            exchange.getRequestHeaders().forEach((k, v) -> requestHeaders.put(k.toLowerCase(), String.join(",", v)));
            headers.set(requestHeaders);

            byte[] response = """
                    {
                      "rating":{
                        "rating_id":"rat-1",
                        "conversation_id":"conv-1",
                        "rating":"CONVERSATION_RATING_VALUE_GOOD",
                        "created_at":"2026-02-13T12:00:00Z"
                      },
                      "summary":{
                        "total_count":1,
                        "good_count":1,
                        "bad_count":0,
                        "latest_rating":"CONVERSATION_RATING_VALUE_GOOD",
                        "latest_rated_at":"2026-02-13T12:00:00Z",
                        "has_bad_rating":false
                      }
                    }
                    """.getBytes();
            exchange.getResponseHeaders().add("Content-Type", "application/json");
            exchange.sendResponseHeaders(200, response.length);
            try (OutputStream outputStream = exchange.getResponseBody()) {
                outputStream.write(response);
            }
        });
        server.start();

        SigilClientConfig config = new SigilClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setApi(new ApiConfig().setEndpoint("127.0.0.1:" + server.getAddress().getPort()))
                .setGenerationExport(new GenerationExportConfig()
                        .setProtocol(GenerationExportProtocol.HTTP)
                        .setEndpoint("127.0.0.1:" + server.getAddress().getPort() + "/api/v1/generations:export")
                        .setInsecure(true)
                        .setAuth(new AuthConfig().setMode(AuthMode.BEARER).setBearerToken("token-a"))
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0));

        try (SigilClient client = new SigilClient(config)) {
            client.submitConversationRating(
                    "conv-1",
                    new SubmitConversationRatingRequest()
                            .setRatingId("rat-1")
                            .setRating(ConversationRatingValue.GOOD));
        } finally {
            server.stop(0);
        }

        assertThat(headers.get().get("authorization")).isEqualTo("Bearer token-a");
    }

    @Test
    void exportRoundTripSeedMatrixMatchesProtoOverHttpAndGrpc() throws Exception {
        for (int seed = 1; seed <= 5; seed++) {
            assertRoundTripMatchesProto(seed, GenerationExportProtocol.HTTP);
            assertRoundTripMatchesProto(seed, GenerationExportProtocol.GRPC);
        }
    }

    private void assertRoundTripMatchesProto(int seed, GenerationExportProtocol protocol) throws Exception {
        SeedPayload payload = payloadFromSeed(seed);
        AtomicReference<GenerationIngest.ExportGenerationsRequest> received = new AtomicReference<>();
        AtomicReference<Generation> expected = new AtomicReference<>();

        if (protocol == GenerationExportProtocol.HTTP) {
            com.sun.net.httpserver.HttpServer server = com.sun.net.httpserver.HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            server.createContext("/api/v1/generations:export", exchange -> {
                byte[] body = exchange.getRequestBody().readAllBytes();
                GenerationIngest.ExportGenerationsRequest.Builder builder = GenerationIngest.ExportGenerationsRequest.newBuilder();
                JsonFormat.parser().ignoringUnknownFields().merge(new String(body), builder);
                received.set(builder.build());

                byte[] response = "{\"results\":[{\"accepted\":true}]}".getBytes();
                exchange.getResponseHeaders().add("Content-Type", "application/json");
                exchange.sendResponseHeaders(202, response.length);
                try (OutputStream outputStream = exchange.getResponseBody()) {
                    outputStream.write(response);
                }
            });
            server.start();

            SigilClientConfig config = new SigilClientConfig()
                    .setTracer(GlobalOpenTelemetry.getTracer("test"))
                    .setGenerationExport(new GenerationExportConfig()
                            .setProtocol(GenerationExportProtocol.HTTP)
                            .setEndpoint("http://127.0.0.1:" + server.getAddress().getPort() + "/api/v1/generations:export")
                            .setBatchSize(1)
                            .setFlushInterval(Duration.ofMinutes(10))
                            .setMaxRetries(0));

            try (SigilClient client = new SigilClient(config)) {
                GenerationRecorder recorder = client.startGeneration(payload.start());
                recorder.setResult(payload.result());
                recorder.end();
                expected.set(recorder.lastGeneration().orElseThrow());
                client.shutdown();
            } finally {
                server.stop(0);
            }
        } else {
            GenerationIngestServiceGrpc.GenerationIngestServiceImplBase service = new GenerationIngestServiceGrpc.GenerationIngestServiceImplBase() {
                @Override
                public void exportGenerations(
                        GenerationIngest.ExportGenerationsRequest request,
                        StreamObserver<GenerationIngest.ExportGenerationsResponse> responseObserver) {
                    received.set(request);
                    List<GenerationIngest.ExportGenerationResult> results = new ArrayList<>();
                    for (GenerationIngest.Generation generation : request.getGenerationsList()) {
                        results.add(GenerationIngest.ExportGenerationResult.newBuilder()
                                .setGenerationId(generation.getId())
                                .setAccepted(true)
                                .build());
                    }
                    responseObserver.onNext(GenerationIngest.ExportGenerationsResponse.newBuilder().addAllResults(results).build());
                    responseObserver.onCompleted();
                }
            };

            Server server = ServerBuilder.forPort(0).addService(service).build().start();
            SigilClientConfig config = new SigilClientConfig()
                    .setTracer(GlobalOpenTelemetry.getTracer("test"))
                    .setGenerationExport(new GenerationExportConfig()
                            .setProtocol(GenerationExportProtocol.GRPC)
                            .setEndpoint("127.0.0.1:" + server.getPort())
                            .setInsecure(true)
                            .setBatchSize(1)
                            .setFlushInterval(Duration.ofMinutes(10))
                            .setMaxRetries(0));

            try (SigilClient client = new SigilClient(config)) {
                GenerationRecorder recorder = client.startGeneration(payload.start());
                recorder.setResult(payload.result());
                recorder.end();
                expected.set(recorder.lastGeneration().orElseThrow());
                client.shutdown();
            } finally {
                server.shutdownNow();
            }
        }

        assertThat(received.get()).isNotNull();
        assertThat(received.get().getGenerationsCount()).isEqualTo(1);
        assertThat(received.get().getGenerations(0)).isEqualTo(ProtoMapper.toProtoGeneration(expected.get()));
    }

    private static SeedPayload payloadFromSeed(int seed) {
        Random random = new Random(seed);
        GenerationMode mode = seed % 2 == 0 ? GenerationMode.STREAM : GenerationMode.SYNC;
        String token = Integer.toString(seed);

        GenerationStart start = new GenerationStart()
                .setId("gen-seed-" + token)
                .setConversationId("conv-seed-" + token)
                .setAgentName("agent-seed-" + token)
                .setAgentVersion("v-" + token)
                .setMode(mode)
                .setOperationName(mode == GenerationMode.STREAM ? "streamText" : "generateText")
                .setModel(new ModelRef().setProvider("provider-" + token).setName("model-" + token))
                .setMaxTokens(100L + seed)
                .setTemperature(0.1 + (seed / 100.0))
                .setTopP(0.5 + (seed / 100.0))
                .setToolChoice(seed % 2 == 0 ? "auto" : "{\"type\":\"tool\",\"name\":\"weather\"}")
                .setThinkingEnabled(seed % 2 == 0)
                .setSystemPrompt("system-" + token)
                .setTags(Map.of("seed", token))
                .setMetadata(Map.of("rand", random.nextInt(1000)));

        GenerationResult result = new GenerationResult()
                .setId(start.getId())
                .setConversationId(start.getConversationId())
                .setAgentName(start.getAgentName())
                .setAgentVersion(start.getAgentVersion())
                .setMode(start.getMode())
                .setOperationName(start.getOperationName())
                .setModel(start.getModel())
                .setResponseId("resp-" + token)
                .setResponseModel("response-model-" + token)
                .setSystemPrompt(start.getSystemPrompt())
                .setMaxTokens(200L + seed)
                .setTemperature(0.2 + (seed / 100.0))
                .setTopP(0.6 + (seed / 100.0))
                .setToolChoice(seed % 2 == 0 ? "required" : "auto")
                .setThinkingEnabled(seed % 3 == 0)
                .setUsage(new TokenUsage()
                        .setInputTokens(10 + seed)
                        .setOutputTokens(20 + seed)
                        .setTotalTokens(30 + seed))
                .setStopReason("stop-" + token)
                .setTags(Map.of("seed", token))
                .setMetadata(Map.of("nested", Map.of("n", random.nextInt(100))));

        result.getInput().add(new Message()
                .setRole(MessageRole.USER)
                .setName("user")
                .setParts(List.of(MessagePart.text("input-" + token))));

        result.getOutput().add(new Message()
                .setRole(MessageRole.ASSISTANT)
                .setName("assistant")
                .setParts(List.of(
                        MessagePart.thinking("think-" + token),
                        MessagePart.toolCall(new ToolCall()
                                .setId("call-" + token)
                                .setName("weather")
                                .setInputJson(("{\"seed\":" + seed + "}").getBytes())))));

        result.getOutput().add(new Message()
                .setRole(MessageRole.TOOL)
                .setName("tool")
                .setParts(List.of(MessagePart.toolResult(new ToolResultPart()
                        .setToolCallId("call-" + token)
                        .setName("weather")
                        .setContent("ok-" + token)
                        .setContentJson(("{\"ok\":" + seed + "}").getBytes())
                        .setError(seed % 3 == 0)))));

        result.getTools().add(new ToolDefinition()
                .setName("weather")
                .setDescription("Get weather")
                .setType("function")
                .setInputSchemaJson("{\"type\":\"object\"}".getBytes()));

        result.getArtifacts().add(new Artifact()
                .setKind(ArtifactKind.REQUEST)
                .setName("request")
                .setContentType("application/json")
                .setPayload(("{\"seed\":" + seed + "}").getBytes()));
        result.getArtifacts().add(new Artifact()
                .setKind(ArtifactKind.PROVIDER_EVENT)
                .setName("event")
                .setContentType("application/json")
                .setPayload(("{\"event\":" + seed + "}").getBytes()));

        return new SeedPayload(start, result);
    }

    private record SeedPayload(GenerationStart start, GenerationResult result) {
    }
}
