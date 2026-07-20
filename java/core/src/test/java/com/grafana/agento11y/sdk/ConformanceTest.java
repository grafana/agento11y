package com.grafana.agento11y.sdk;

import static org.assertj.core.api.Assertions.assertThat;

import com.fasterxml.jackson.databind.JsonNode;
import com.sun.net.httpserver.HttpServer;
import io.grpc.Server;
import io.grpc.ServerBuilder;
import io.grpc.stub.StreamObserver;
import io.opentelemetry.api.common.AttributeKey;
import io.opentelemetry.context.Scope;
import io.opentelemetry.sdk.metrics.SdkMeterProvider;
import io.opentelemetry.sdk.metrics.data.MetricData;
import io.opentelemetry.sdk.testing.exporter.InMemoryMetricReader;
import io.opentelemetry.sdk.testing.exporter.InMemorySpanExporter;
import io.opentelemetry.sdk.trace.SdkTracerProvider;
import io.opentelemetry.sdk.trace.data.SpanData;
import io.opentelemetry.sdk.trace.export.SimpleSpanProcessor;
import java.io.OutputStream;
import java.net.InetSocketAddress;
import java.nio.charset.StandardCharsets;
import java.time.Duration;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.concurrent.atomic.AtomicReference;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;
import org.junit.jupiter.params.ParameterizedTest;
import org.junit.jupiter.params.provider.Arguments;
import org.junit.jupiter.params.provider.MethodSource;
import agento11y.v1.GenerationIngest;
import agento11y.v1.GenerationIngestServiceGrpc;

class ConformanceTest {
    @Test
    void syncRoundtripSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationStart start = new GenerationStart()
                    .setId("gen-roundtrip")
                    .setConversationId("conv-roundtrip")
                    .setConversationTitle("Roundtrip conversation")
                    .setUserId("user-roundtrip")
                    .setAgentName("agent-roundtrip")
                    .setAgentVersion("v-roundtrip")
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setMaxTokens(256L)
                    .setTemperature(0.2)
                    .setTopP(0.9)
                    .setToolChoice("required")
                    .setThinkingEnabled(false);
            start.getTools().add(new ToolDefinition()
                    .setName("weather")
                    .setDescription("Get weather")
                    .setType("function"));
            start.getTags().put("tenant", "dev");
            start.getMetadata().put("trace", "roundtrip");

            GenerationRecorder recorder = env.client.startGeneration(start);

            GenerationResult result = new GenerationResult()
                    .setResponseId("resp-roundtrip")
                    .setResponseModel("gpt-5-2026")
                    .setUsage(new TokenUsage()
                            .setInputTokens(12)
                            .setOutputTokens(7)
                            .setTotalTokens(19)
                            .setCacheReadInputTokens(2)
                            .setCacheWriteInputTokens(1)
                            .setReasoningTokens(4))
                    .setStopReason("stop");
            result.getTags().put("region", "eu");
            result.getMetadata().put("result", "ok");
            result.getInput().add(new Message()
                    .setRole(MessageRole.USER)
                    .setParts(List.of(MessagePart.text("hello"))));
            result.getOutput().add(new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(List.of(
                            MessagePart.thinking("reasoning"),
                            MessagePart.toolCall(new ToolCall()
                                    .setId("call-1")
                                    .setName("weather")
                                    .setInputJson("{\"city\":\"Paris\"}".getBytes(StandardCharsets.UTF_8))))));
            result.getOutput().add(new Message()
                    .setRole(MessageRole.TOOL)
                    .setParts(List.of(
                            MessagePart.toolResult(new ToolResultPart()
                                    .setToolCallId("call-1")
                                    .setName("weather")
                                    .setContent("sunny")
                                    .setContentJson("{\"temp_c\":18}".getBytes(StandardCharsets.UTF_8))))));
            result.getArtifacts().add(new Artifact()
                    .setKind(ArtifactKind.REQUEST)
                    .setName("request")
                    .setContentType("application/json")
                    .setPayload("{\"prompt\":\"hello\"}".getBytes(StandardCharsets.UTF_8)));
            result.getArtifacts().add(new Artifact()
                    .setKind(ArtifactKind.RESPONSE)
                    .setName("response")
                    .setContentType("application/json")
                    .setPayload("{\"text\":\"sunny\"}".getBytes(StandardCharsets.UTF_8)));

            recorder.setResult(result);
            recorder.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            List<String> metricNames = env.metricNames();

            assertThat(generation.getMode()).isEqualTo(GenerationIngest.GenerationMode.GENERATION_MODE_SYNC);
            assertThat(generation.getOperationName()).isEqualTo("generateText");
            assertThat(generation.getConversationId()).isEqualTo("conv-roundtrip");
            assertThat(generation.getAgentName()).isEqualTo("agent-roundtrip");
            assertThat(generation.getAgentVersion()).isEqualTo("v-roundtrip");
            assertThat(generation.getTraceId()).isEqualTo(span.getTraceId());
            assertThat(generation.getSpanId()).isEqualTo(span.getSpanId());
            assertThat(generation.getMetadata().getFieldsMap().get("agento11y.conversation.title").getStringValue())
                    .isEqualTo("Roundtrip conversation");
            assertThat(generation.getMetadata().getFieldsMap().get("agento11y.user.id").getStringValue())
                    .isEqualTo("user-roundtrip");
            assertThat(generation.getInput(0).getParts(0).getText()).isEqualTo("hello");
            assertThat(generation.getOutput(0).getParts(0).getThinking()).isEqualTo("reasoning");
            assertThat(generation.getOutput(0).getParts(1).getToolCall().getName()).isEqualTo("weather");
            assertThat(generation.getOutput(1).getParts(0).getToolResult().getContent()).isEqualTo("sunny");
            assertThat(generation.getMaxTokens()).isEqualTo(256L);
            assertThat(generation.getTemperature()).isEqualTo(0.2d);
            assertThat(generation.getTopP()).isEqualTo(0.9d);
            assertThat(generation.getToolChoice()).isEqualTo("required");
            assertThat(generation.getThinkingEnabled()).isFalse();
            assertThat(generation.getUsage().getInputTokens()).isEqualTo(12L);
            assertThat(generation.getUsage().getOutputTokens()).isEqualTo(7L);
            assertThat(generation.getUsage().getTotalTokens()).isEqualTo(19L);
            assertThat(generation.getUsage().getCacheReadInputTokens()).isEqualTo(2L);
            assertThat(generation.getUsage().getCacheWriteInputTokens()).isEqualTo(1L);
            assertThat(generation.getUsage().getReasoningTokens()).isEqualTo(4L);
            assertThat(generation.getStopReason()).isEqualTo("stop");
            assertThat(generation.getTagsMap()).containsEntry("tenant", "dev").containsEntry("region", "eu");
            assertThat(generation.getRawArtifactsCount()).isEqualTo(2);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME))).isEqualTo("generateText");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE)))
                    .isEqualTo("Roundtrip conversation");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_USER_ID)))
                    .isEqualTo("user-roundtrip");
            assertThat(metricNames).contains(Agento11yClient.METRIC_OPERATION_DURATION, Agento11yClient.METRIC_TOKEN_USAGE);
            assertThat(metricNames).doesNotContain(Agento11yClient.METRIC_TTFT);
            assertThat(env.metricData(Agento11yClient.METRIC_TOKEN_USAGE).getHistogramData().getPoints())
                    .anySatisfy(point -> {
                        assertThat(point.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION)))
                                .isEqualTo("v-roundtrip");
                        assertThat(point.getAttributes().get(AttributeKey.stringKey(Agento11yClient.METRIC_ATTR_TOKEN_TYPE)))
                                .isEqualTo("input");
                    });
        }
    }

    @ParameterizedTest
    @MethodSource("conversationTitleCases")
    void conversationTitleSemantics(String startTitle, String contextTitle, String metadataTitle, String expected) throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1);
                Scope ignored = contextTitle.isEmpty() ? noopScope() : Agento11yContext.withConversationTitle(contextTitle)) {
            GenerationStart start = new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setConversationTitle(startTitle);
            if (!metadataTitle.isEmpty()) {
                start.getMetadata().put(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE, metadataTitle);
            }

            GenerationRecorder recorder = env.client.startGeneration(start);
            recorder.setResult(new GenerationResult());
            recorder.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            if (expected.isEmpty()) {
                assertThat(generation.getMetadata().getFieldsMap()).doesNotContainKey(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE);
                assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE))).isNull();
                return;
            }

            assertThat(generation.getMetadata().getFieldsMap().get(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE).getStringValue())
                    .isEqualTo(expected);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE)))
                    .isEqualTo(expected);
        }
    }

    @ParameterizedTest
    @MethodSource("userIdCases")
    void userIdSemantics(String startUserId, String contextUserId, String canonicalUserId, String legacyUserId, String expected)
            throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1);
                Scope ignored = contextUserId.isEmpty() ? noopScope() : Agento11yContext.withUserId(contextUserId)) {
            GenerationStart start = new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setUserId(startUserId);
            if (!canonicalUserId.isEmpty()) {
                start.getMetadata().put(Agento11yClient.METADATA_USER_ID_KEY, canonicalUserId);
            }
            if (!legacyUserId.isEmpty()) {
                start.getMetadata().put(Agento11yClient.METADATA_LEGACY_USER_ID_KEY, legacyUserId);
            }

            GenerationRecorder recorder = env.client.startGeneration(start);
            recorder.setResult(new GenerationResult());
            recorder.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            assertThat(generation.getMetadata().getFieldsMap().get(Agento11yClient.METADATA_USER_ID_KEY).getStringValue())
                    .isEqualTo(expected);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_USER_ID))).isEqualTo(expected);
        }
    }

    @ParameterizedTest
    @MethodSource("agentIdentityCases")
    void agentIdentitySemantics(
            String startName,
            String startVersion,
            String contextName,
            String contextVersion,
            String resultName,
            String resultVersion,
            String expectedName,
            String expectedVersion)
            throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1);
                Scope ignoredName = contextName.isEmpty() ? noopScope() : Agento11yContext.withAgentName(contextName);
                Scope ignoredVersion = contextVersion.isEmpty() ? noopScope() : Agento11yContext.withAgentVersion(contextVersion)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setAgentName(startName)
                    .setAgentVersion(startVersion));
            recorder.setResult(new GenerationResult()
                    .setAgentName(resultName)
                    .setAgentVersion(resultVersion));
            recorder.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            assertThat(generation.getAgentName()).isEqualTo(expectedName);
            assertThat(generation.getAgentVersion()).isEqualTo(expectedVersion);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_NAME)))
                    .isEqualTo(expectedName.isEmpty() ? null : expectedName);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION)))
                    .isEqualTo(expectedVersion.isEmpty() ? null : expectedVersion);
        }
    }

    // echo -n "1.2.3" | shasum -a 256
    private static final String EFFECTIVE_VERSION_DIGEST_1_2_3 =
            "sha256:c47f5b18b8a430e698b9fe15e51f6119984e78334bcf3f45e210d30c37ef2f9e";

    @Test
    void effectiveVersionUnsetLeavesProtoFieldAbsent() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));
            recorder.setResult(new GenerationResult());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.hasEffectiveVersion()).isFalse();
        }
    }

    @Test
    void effectiveVersionRawHashesToPinnedDigest() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("1.2.3"));
            recorder.setResult(new GenerationResult());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getEffectiveVersion()).isEqualTo(EFFECTIVE_VERSION_DIGEST_1_2_3);
        }
    }

    @Test
    void effectiveVersionWhitespaceOnlyLeavesProtoFieldAbsent() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("   "));
            recorder.setResult(new GenerationResult());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.hasEffectiveVersion()).isFalse();
        }
    }

    @Test
    void effectiveVersionSurroundingWhitespaceIsTrimmedBeforeHashing() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("  1.2.3\t\n"));
            recorder.setResult(new GenerationResult());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getEffectiveVersion()).isEqualTo(EFFECTIVE_VERSION_DIGEST_1_2_3);
        }
    }

    // echo -n "result-only" | shasum -a 256
    private static final String EFFECTIVE_VERSION_DIGEST_RESULT_ONLY =
            "sha256:f61f2b041f07a7e4a58a926df31279f4c11ebd1f716147d8ee8cbfad6a69f30e";

    @Test
    void effectiveVersionStartFallsThroughWhenResultIsEmpty() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("1.2.3"));
            recorder.setResult(new GenerationResult());
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getEffectiveVersion()).isEqualTo(EFFECTIVE_VERSION_DIGEST_1_2_3);
        }
    }

    @Test
    void effectiveVersionStartFallsThroughWhenResultIsWhitespaceOnly() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("1.2.3"));
            recorder.setResult(new GenerationResult().setEffectiveVersion("   "));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getEffectiveVersion()).isEqualTo(EFFECTIVE_VERSION_DIGEST_1_2_3);
        }
    }

    @Test
    void effectiveVersionResultWinsOverStart() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setEffectiveVersion("ignored"));
            recorder.setResult(new GenerationResult().setEffectiveVersion("result-only"));
            recorder.end();
            assertThat(recorder.error()).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getEffectiveVersion()).isEqualTo(EFFECTIVE_VERSION_DIGEST_RESULT_ONLY);
        }
    }

    @Test
    void streamingTelemetrySemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            Instant startedAt = Instant.parse("2026-03-12T09:00:00Z");
            GenerationRecorder recorder = env.client.startStreamingGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setStartedAt(startedAt));
            recorder.setFirstTokenAt(startedAt.plusMillis(250));
            recorder.setResult(new GenerationResult()
                    .setStartedAt(startedAt)
                    .setCompletedAt(startedAt.plusSeconds(1))
                    .setUsage(new TokenUsage().setInputTokens(4).setOutputTokens(3).setTotalTokens(7)));
            recorder.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            List<String> metricNames = env.metricNames();

            assertThat(generation.getMode()).isEqualTo(GenerationIngest.GenerationMode.GENERATION_MODE_STREAM);
            assertThat(generation.getOperationName()).isEqualTo("streamText");
            assertThat(span.getName()).isEqualTo("streamText gpt-5");
            assertThat(metricNames).contains(Agento11yClient.METRIC_OPERATION_DURATION, Agento11yClient.METRIC_TTFT);
        }
    }

    @Test
    void toolExecutionSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1);
                Scope ignoredTitle = Agento11yContext.withConversationTitle("Context title");
                Scope ignoredName = Agento11yContext.withAgentName("agent-context");
                Scope ignoredVersion = Agento11yContext.withAgentVersion("v-context")) {
            ToolExecutionRecorder recorder = env.client.startToolExecution(new ToolExecutionStart()
                    .setToolName("weather")
                    .setToolCallId("call-weather-1")
                    .setToolType("function")
                    .setIncludeContent(true));
            recorder.setResult(new ToolExecutionResult()
                    .setArguments(Map.of("city", "Paris"))
                    .setResult(Map.of("forecast", "sunny")));
            recorder.end();
            env.client.shutdown();

            SpanData span = env.latestSpanByNamePrefix("execute_tool ");
            List<String> metricNames = env.metricNames();

            assertThat(env.requests).isEmpty();
            assertThat(span.getName()).isEqualTo("execute_tool weather");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_NAME))).isEqualTo("weather");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_CALL_ID))).isEqualTo("call-weather-1");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_TYPE))).isEqualTo("function");
            assertThat(String.valueOf(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_CALL_ARGUMENTS))))
                    .contains("Paris");
            assertThat(String.valueOf(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_TOOL_CALL_RESULT))))
                    .contains("sunny");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_CONVERSATION_TITLE)))
                    .isEqualTo("Context title");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_NAME)))
                    .isEqualTo("agent-context");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION)))
                    .isEqualTo("v-context");
            assertThat(metricNames).contains(Agento11yClient.METRIC_OPERATION_DURATION);
            assertThat(metricNames).doesNotContain(Agento11yClient.METRIC_TTFT);
        }
    }

    @Test
    void embeddingSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1);
                Scope ignoredName = Agento11yContext.withAgentName("agent-context");
                Scope ignoredVersion = Agento11yContext.withAgentVersion("v-context")) {
            EmbeddingRecorder recorder = env.client.startEmbedding(new EmbeddingStart()
                    .setModel(new ModelRef().setProvider("openai").setName("text-embedding-3-small"))
                    .setDimensions(512L));
            recorder.setResult(new EmbeddingResult()
                    .setInputCount(2)
                    .setInputTokens(8)
                    .setInputTexts(List.of("hello", "world"))
                    .setResponseModel("text-embedding-3-small")
                    .setDimensions(512L));
            recorder.end();
            env.client.shutdown();

            SpanData span = env.latestSpan("embeddings");
            List<String> metricNames = env.metricNames();

            assertThat(env.requests).isEmpty();
            assertThat(span.getName()).isEqualTo("embeddings text-embedding-3-small");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME))).isEqualTo("embeddings");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_NAME)))
                    .isEqualTo("agent-context");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_AGENT_VERSION)))
                    .isEqualTo("v-context");
            assertThat(span.getAttributes().get(AttributeKey.longKey("gen_ai.embeddings.input_count"))).isEqualTo(2L);
            assertThat(span.getAttributes().get(AttributeKey.longKey("gen_ai.embeddings.dimension.count"))).isEqualTo(512L);
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_RESPONSE_MODEL)))
                    .isEqualTo("text-embedding-3-small");
            assertThat(metricNames).contains(Agento11yClient.METRIC_OPERATION_DURATION, Agento11yClient.METRIC_TOKEN_USAGE);
            assertThat(metricNames).doesNotContain(Agento11yClient.METRIC_TTFT, Agento11yClient.METRIC_TOOL_CALLS_PER_OPERATION);
        }
    }

    @Test
    void validationAndCallErrorSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationRecorder invalid = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("anthropic").setName("claude-sonnet-4-5")));
            invalid.setResult(new GenerationResult().setInput(List.of(new Message()
                    .setRole(MessageRole.USER)
                    .setParts(List.of(MessagePart.toolCall(new ToolCall().setName("weather")))))));
            invalid.end();

            assertThat(invalid.error()).isPresent();
            assertThat(env.requests).isEmpty();
            assertThat(env.latestGenerationSpan().getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_ERROR_TYPE)))
                    .isEqualTo("validation_error");

            GenerationRecorder callError = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));
            callError.setCallError(new IllegalStateException("provider unavailable"));
            callError.setResult(new GenerationResult());
            callError.end();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            SpanData span = env.latestGenerationSpan();
            assertThat(callError.error()).isEmpty();
            assertThat(generation.getCallError()).isEqualTo("provider unavailable");
            assertThat(generation.getMetadata().getFieldsMap().get("call_error").getStringValue()).isEqualTo("provider unavailable");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_ERROR_TYPE)))
                    .isEqualTo("provider_call_error");
        }
    }

    @Test
    void ratingSubmissionSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            SubmitConversationRatingResponse response = env.client.submitConversationRating(
                    "conv-rating",
                    new SubmitConversationRatingRequest()
                            .setRatingId("rat-1")
                            .setRating(ConversationRatingValue.BAD)
                            .setComment("wrong answer")
                            .setMetadata(Map.of("channel", "assistant")));

            assertThat(env.ratingPath.get()).isEqualTo("/api/v1/conversations/conv-rating/ratings");
            assertThat(response.getRating().getConversationId()).isEqualTo("conv-rating");
            assertThat(response.getSummary().getBadCount()).isEqualTo(1L);

            JsonNode body = env.ratingPayload.get();
            assertThat(body.get("rating_id").asText()).isEqualTo("rat-1");
            assertThat(body.get("rating").asText()).isEqualTo("CONVERSATION_RATING_VALUE_BAD");
            assertThat(body.get("comment").asText()).isEqualTo("wrong answer");
        }
    }

    @Test
    void clientTagAttributes() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1, Map.of("team", "payments"))) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5"))
                    .setTags(Map.of("call_only", "yes")));
            recorder.setResult(new GenerationResult()
                    .setUsage(new TokenUsage().setInputTokens(4).setOutputTokens(2)));
            recorder.end();
            env.client.shutdown();

            SpanData span = env.latestGenerationSpan();
            assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tag.team"))).isEqualTo("payments");
            assertThat(span.getAttributes().get(AttributeKey.stringKey("agento11y.tag.call_only"))).isNull();

            MetricData duration = env.metricData(Agento11yClient.METRIC_OPERATION_DURATION);
            boolean tagged = duration.getHistogramData().getPoints().stream()
                    .anyMatch(point -> "payments".equals(
                            point.getAttributes().get(AttributeKey.stringKey("agento11y.tag.team"))));
            assertThat(tagged).as("expected agento11y.tag.team on operation.duration data point").isTrue();
        }
    }

    @Test
    void shutdownFlushSemantics() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(10)) {
            GenerationRecorder recorder = env.client.startGeneration(new GenerationStart()
                    .setConversationId("conv-shutdown")
                    .setAgentName("agent-shutdown")
                    .setAgentVersion("v-shutdown")
                    .setModel(new ModelRef().setProvider("openai").setName("gpt-5")));
            recorder.setResult(new GenerationResult());
            recorder.end();

            assertThat(env.requests).isEmpty();
            env.client.shutdown();

            GenerationIngest.Generation generation = env.singleGeneration();
            assertThat(generation.getConversationId()).isEqualTo("conv-shutdown");
            assertThat(generation.getAgentName()).isEqualTo("agent-shutdown");
            assertThat(generation.getAgentVersion()).isEqualTo("v-shutdown");
        }
    }

    // Guards the agento11y rename: the SDK emits only agento11y.* names for
    // SDK-owned span attributes, metadata keys, and tag projections, while
    // caller-supplied metadata and tags pass through untouched even when they
    // use the legacy sigil.* namespace.
    @Test
    void noSdkOwnedLegacyAgento11yNamespace() throws Exception {
        try (ConformanceEnv env = new ConformanceEnv(1)) {
            GenerationStart start = new GenerationStart()
                    .setId("gen-legacy-namespace")
                    .setConversationId("conv-legacy-namespace")
                    .setConversationTitle("Legacy namespace check")
                    .setUserId("user-legacy")
                    .setAgentName("agent-legacy")
                    .setAgentVersion("1.0.0")
                    .setModel(new ModelRef().setProvider("anthropic").setName("claude-sonnet-4-5"))
                    .setToolChoice("auto")
                    .setThinkingEnabled(true);
            start.getTags().put("sigil.caller_tag", "caller-tag");
            start.getMetadata().put("sigil.caller_key", "caller-value");

            GenerationRecorder recorder = env.client.startGeneration(start);
            GenerationResult result = new GenerationResult()
                    .setUsage(new TokenUsage().setInputTokens(1).setOutputTokens(1).setTotalTokens(2));
            result.getOutput().add(new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(List.of(MessagePart.text("ok"))));
            recorder.setResult(result);
            recorder.end();
            env.client.shutdown();

            SpanData span = env.latestGenerationSpan();
            List<String> legacySpanAttrs = span.getAttributes().asMap().keySet().stream()
                    .map(AttributeKey::getKey)
                    .filter(key -> key.startsWith("sigil."))
                    .toList();
            assertThat(legacySpanAttrs).isEmpty();
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_GENERATION_ID)))
                    .isEqualTo("gen-legacy-namespace");
            assertThat(span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_SDK_NAME)))
                    .isEqualTo("sdk-java");

            GenerationIngest.Generation generation = env.singleGeneration();
            Map<String, com.google.protobuf.Value> metadata = generation.getMetadata().getFieldsMap();
            List<String> legacyMetadataKeys = metadata.keySet().stream()
                    .filter(key -> key.startsWith("sigil.") && !key.equals("sigil.caller_key"))
                    .toList();
            assertThat(legacyMetadataKeys).isEmpty();
            assertThat(metadata.get("sigil.caller_key").getStringValue()).isEqualTo("caller-value");
            assertThat(metadata.get(Agento11yClient.SPAN_ATTR_SDK_NAME).getStringValue()).isEqualTo("sdk-java");
            assertThat(metadata.get("agento11y.user.id").getStringValue()).isEqualTo("user-legacy");
            assertThat(generation.getTagsMap()).containsEntry("sigil.caller_tag", "caller-tag");
        }
    }

    private static Stream<Arguments> conversationTitleCases() {
        return Stream.of(
                Arguments.of("Explicit", "Context", "Meta", "Explicit"),
                Arguments.of("", "Context", "", "Context"),
                Arguments.of("", "", "Meta", "Meta"),
                Arguments.of("  Padded  ", "", "", "Padded"),
                Arguments.of("   ", "", "", ""));
    }

    private static Stream<Arguments> userIdCases() {
        return Stream.of(
                Arguments.of("explicit", "ctx", "canonical", "legacy", "explicit"),
                Arguments.of("", "ctx", "", "", "ctx"),
                Arguments.of("", "", "canonical", "", "canonical"),
                Arguments.of("", "", "", "legacy", "legacy"),
                Arguments.of("", "", "canonical", "legacy", "canonical"),
                Arguments.of("  padded  ", "", "", "", "padded"));
    }

    private static Stream<Arguments> agentIdentityCases() {
        return Stream.of(
                Arguments.of("agent-explicit", "v1.2.3", "", "", "", "", "agent-explicit", "v1.2.3"),
                Arguments.of("", "", "agent-context", "v-context", "", "", "agent-context", "v-context"),
                Arguments.of("agent-seed", "v-seed", "", "", "agent-result", "v-result", "agent-result", "v-result"),
                Arguments.of("", "", "", "", "", "", "", ""));
    }

    private static Scope noopScope() {
        return () -> {
        };
    }

    private static final class ConformanceEnv implements AutoCloseable {
        private final Server server;
        private final HttpServer ratingServer;
        private final InMemorySpanExporter spanExporter = InMemorySpanExporter.create();
        private final SdkTracerProvider tracerProvider = SdkTracerProvider.builder()
                .addSpanProcessor(SimpleSpanProcessor.create(spanExporter))
                .build();
        private final InMemoryMetricReader metricReader = InMemoryMetricReader.create();
        private final SdkMeterProvider meterProvider = SdkMeterProvider.builder()
                .registerMetricReader(metricReader)
                .build();
        private final AtomicReference<String> ratingPath = new AtomicReference<>();
        private final AtomicReference<JsonNode> ratingPayload = new AtomicReference<>();
        private final List<GenerationIngest.ExportGenerationsRequest> requests = new CopyOnWriteArrayList<>();
        private boolean closed;

        private final Agento11yClient client;

        ConformanceEnv(int batchSize) throws Exception {
            this(batchSize, null);
        }

        ConformanceEnv(int batchSize, Map<String, String> tags) throws Exception {
            GenerationIngestServiceGrpc.GenerationIngestServiceImplBase service =
                    new GenerationIngestServiceGrpc.GenerationIngestServiceImplBase() {
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
                            responseObserver.onNext(GenerationIngest.ExportGenerationsResponse.newBuilder()
                                    .addAllResults(results)
                                    .build());
                            responseObserver.onCompleted();
                        }
                    };
            server = ServerBuilder.forPort(0).addService(service).build().start();

            ratingServer = HttpServer.create(new InetSocketAddress("127.0.0.1", 0), 0);
            ratingServer.createContext("/api/v1/conversations/conv-rating/ratings", exchange -> {
                ratingPath.set(exchange.getRequestURI().getPath());
                ratingPayload.set(Json.MAPPER.readTree(exchange.getRequestBody().readAllBytes()));

                byte[] response = """
                        {
                          "rating":{
                            "rating_id":"rat-1",
                            "conversation_id":"conv-rating",
                            "rating":"CONVERSATION_RATING_VALUE_BAD",
                            "created_at":"2026-03-12T09:00:00Z"
                          },
                          "summary":{
                            "total_count":1,
                            "good_count":0,
                            "bad_count":1,
                            "latest_rating":"CONVERSATION_RATING_VALUE_BAD",
                            "latest_rated_at":"2026-03-12T09:00:00Z",
                            "has_bad_rating":true
                          }
                        }
                        """.getBytes(StandardCharsets.UTF_8);
                exchange.getResponseHeaders().add("Content-Type", "application/json");
                exchange.sendResponseHeaders(200, response.length);
                try (OutputStream outputStream = exchange.getResponseBody()) {
                    outputStream.write(response);
                }
            });
            ratingServer.start();

            Agento11yClientConfig config = new Agento11yClientConfig()
                    .setTracer(tracerProvider.get("agento11y-conformance-test"))
                    .setMeter(meterProvider.get("agento11y-conformance-test"))
                    .setApi(new ApiConfig().setEndpoint("http://127.0.0.1:" + ratingServer.getAddress().getPort()))
                    .setGenerationExport(new GenerationExportConfig()
                            .setProtocol(GenerationExportProtocol.GRPC)
                            .setEndpoint("127.0.0.1:" + server.getPort())
                            .setInsecure(true)
                            .setBatchSize(batchSize)
                            .setQueueSize(10)
                            .setFlushInterval(Duration.ofHours(1))
                            .setMaxRetries(1)
                            .setInitialBackoff(Duration.ofMillis(1))
                            .setMaxBackoff(Duration.ofMillis(2)));
            if (tags != null) {
                config.setTags(tags);
            }
            client = new Agento11yClient(config);
        }

        GenerationIngest.Generation singleGeneration() {
            assertThat(requests).hasSize(1);
            assertThat(requests.get(0).getGenerationsCount()).isEqualTo(1);
            return requests.get(0).getGenerations(0);
        }

        SpanData latestGenerationSpan() {
            List<SpanData> spans = spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> {
                        String operation = span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME));
                        return "generateText".equals(operation) || "streamText".equals(operation);
                    })
                    .toList();
            assertThat(spans).isNotEmpty();
            return spans.get(spans.size() - 1);
        }

        SpanData latestSpan(String operationName) {
            List<SpanData> spans = spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> operationName.equals(
                            span.getAttributes().get(AttributeKey.stringKey(Agento11yClient.SPAN_ATTR_OPERATION_NAME))))
                    .toList();
            assertThat(spans).isNotEmpty();
            return spans.get(spans.size() - 1);
        }

        SpanData latestSpanByNamePrefix(String prefix) {
            List<SpanData> spans = spanExporter.getFinishedSpanItems().stream()
                    .filter(span -> span.getName().startsWith(prefix))
                    .toList();
            assertThat(spans).isNotEmpty();
            return spans.get(spans.size() - 1);
        }

        List<String> metricNames() {
            return metricReader.collectAllMetrics().stream()
                    .map(MetricData::getName)
                    .toList();
        }

        MetricData metricData(String name) {
            return metricReader.collectAllMetrics().stream()
                    .filter(metric -> metric.getName().equals(name))
                    .findFirst()
                    .orElseThrow();
        }

        @Override
        public void close() {
            if (closed) {
                return;
            }
            closed = true;
            client.shutdown();
            server.shutdownNow();
            ratingServer.stop(0);
            tracerProvider.shutdown();
            meterProvider.shutdown();
        }
    }
}
