package com.grafana.agento11y.sdk.providers.openai;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.grafana.agento11y.sdk.ExportGenerationResult;
import com.grafana.agento11y.sdk.ExportGenerationsRequest;
import com.grafana.agento11y.sdk.ExportGenerationsResponse;
import com.grafana.agento11y.sdk.EmbeddingResult;
import com.grafana.agento11y.sdk.Generation;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExporter;
import com.grafana.agento11y.sdk.GenerationMode;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import com.openai.core.ObjectMappers;
import com.openai.core.http.StreamResponse;
import com.openai.models.ReasoningEffort;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import com.openai.models.embeddings.CreateEmbeddingResponse;
import com.openai.models.embeddings.EmbeddingCreateParams;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStreamEvent;
import io.opentelemetry.api.GlobalOpenTelemetry;
import java.io.IOException;
import java.lang.reflect.Field;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;

class OpenAiConformanceTest {
    @Test
    void chatSyncWrapperSetsSyncModeAndRawArtifactsOffByDefault() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = newClient(exporter)) {
            ChatCompletion response = OpenAiChatCompletions.create(
                    client,
                    chatRequestFixture(),
                    _request -> chatResponseFixture(),
                    new OpenAiOptions());
            assertThat(response.id()).isEqualTo("chatcmpl_1");
            Generation generation = singleDebugGeneration(client);
            assertThat(generation.getMode()).isEqualTo(GenerationMode.SYNC);
            assertThat(generation.getModel().getProvider()).isEqualTo("openai");
            assertThat(generation.getUsage().getInputTokens()).isEqualTo(112L);
            assertThat(generation.getUsage().getCacheReadInputTokens()).isEqualTo(8L);
            assertThat(generation.getArtifacts()).isEmpty();
        }
    }

    @Test
    void chatStreamWrapperSetsStreamModeAndRawArtifactsOnlyWithOptIn() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = newClient(exporter)) {
            OpenAiChatCompletions.createStreaming(
                    client,
                    chatRequestFixture(),
                    _request -> new FakeStreamResponse<>(List.of(chatChunkOne(), chatChunkTwo())),
                    new OpenAiOptions().setRawArtifacts(true));
            Generation generation = singleDebugGeneration(client);
            assertThat(generation.getMode()).isEqualTo(GenerationMode.STREAM);
            assertThat(generation.getStopReason()).isEqualTo("tool_calls");
            assertThat(generation.getOutput()).isNotEmpty();
            assertThat(generation.getArtifacts()).hasSizeGreaterThanOrEqualTo(2);
            assertThat(generation.getArtifacts().stream().anyMatch(artifact -> "PROVIDER_EVENT".equals(artifact.getKind().name())))
                    .isTrue();
        }
    }

    @Test
    void responsesSyncWrapperMapsUsageAndStopReason() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = newClient(exporter)) {
            Response response = OpenAiResponses.create(
                    client,
                    responsesRequestFixture(),
                    _request -> responsesResponseFixture(),
                    new OpenAiOptions());
            assertThat(response.id()).isEqualTo("resp_1");
            Generation generation = singleDebugGeneration(client);
            assertThat(generation.getMode()).isEqualTo(GenerationMode.SYNC);
            assertThat(generation.getModel().getProvider()).isEqualTo("openai");
            assertThat(generation.getStopReason()).isEqualTo("stop");
            assertThat(generation.getUsage().getInputTokens()).isEqualTo(78L);
            assertThat(generation.getUsage().getTotalTokens()).isEqualTo(100L);
            assertThat(generation.getUsage().getCacheReadInputTokens()).isEqualTo(2L);
            assertThat(generation.getUsage().getReasoningTokens()).isEqualTo(3L);
            assertThat(generation.getArtifacts()).isEmpty();
        }
    }

    @Test
    void responsesStreamWrapperFallsBackToEventsWhenFinalResponseMissing() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = newClient(exporter)) {
            OpenAiResponses.createStreaming(
                    client,
                    responsesRequestFixture(),
                    _request -> new FakeStreamResponse<>(List.of(responseTextDeltaEvent("hello", 1), responseTextDeltaEvent(" world", 2), responseCompletedEvent())),
                    new OpenAiOptions().setRawArtifacts(true));
            Generation generation = singleDebugGeneration(client);
            assertThat(generation.getMode()).isEqualTo(GenerationMode.STREAM);
            assertThat(generation.getStopReason()).isEqualTo("stop");
            assertThat(generation.getArtifacts()).hasSizeGreaterThanOrEqualTo(2);
            assertThat(generation.getArtifacts().stream().anyMatch(artifact -> "PROVIDER_EVENT".equals(artifact.getKind().name())))
                    .isTrue();
        }
    }

    @Test
    void providerErrorsPopulateCallErrorForChatAndResponses() throws Exception {
        CapturingExporter chatExporter = new CapturingExporter();
        try (Agento11yClient client = newClient(chatExporter)) {
            assertThatThrownBy(() -> OpenAiChatCompletions.create(
                    client,
                    chatRequestFixture(),
                    _request -> {
                        throw new RuntimeException("chat provider blew up");
                    },
                    new OpenAiOptions())).isInstanceOf(RuntimeException.class).hasMessageContaining("chat provider blew up");
            assertThat(singleDebugGeneration(client).getCallError()).contains("chat provider blew up");
        }

        CapturingExporter responsesExporter = new CapturingExporter();
        try (Agento11yClient client = newClient(responsesExporter)) {
            assertThatThrownBy(() -> OpenAiResponses.create(
                    client,
                    responsesRequestFixture(),
                    _request -> {
                        throw new RuntimeException("responses provider blew up");
                    },
                    new OpenAiOptions())).isInstanceOf(RuntimeException.class).hasMessageContaining("responses provider blew up");
            assertThat(singleDebugGeneration(client).getCallError()).contains("responses provider blew up");
        }
    }

    @Test
    void wrappersTolerateMissingProviderPayloadFields() throws Exception {
        try (Agento11yClient client = newClient(new CapturingExporter())) {
            OpenAiChatCompletions.create(
                    client,
                    chatRequestFixture(),
                    _request -> json(
                            """
                            {
                              "id": "chatcmpl_malformed",
                              "choices": [],
                              "created": 1,
                              "model": "gpt-5",
                              "object": "chat.completion"
                            }
                            """,
                                    ChatCompletion.class),
                    new OpenAiOptions());
            Generation chatGeneration = singleDebugGeneration(client);
            assertThat(chatGeneration.getMode()).isEqualTo(GenerationMode.SYNC);
            assertThat(chatGeneration.getResponseId()).isEqualTo("chatcmpl_malformed");
            assertThat(chatGeneration.getResponseModel()).isEqualTo("gpt-5");
            assertThat(chatGeneration.getOutput()).isEmpty();
            assertThat(chatGeneration.getStopReason()).isEmpty();
        }

        try (Agento11yClient client = newClient(new CapturingExporter())) {
            OpenAiResponses.createStreaming(
                    client,
                    responsesRequestFixture(),
                    _request -> new FakeStreamResponse<>(List.of(
                            json(
                                    """
                                    {
                                      "type": "response.output_text.delta",
                                      "content_index": 0,
                                      "delta": 42,
                                      "item_id": "msg_1",
                                      "output_index": 0,
                                      "sequence_number": 1
                                    }
                                    """,
                                    ResponseStreamEvent.class))),
                    new OpenAiOptions());
            Generation streamGeneration = singleDebugGeneration(client);
            assertThat(streamGeneration.getMode()).isEqualTo(GenerationMode.STREAM);
            assertThat(streamGeneration.getOutput()).hasSize(1);
            assertThat(streamGeneration.getOutput().get(0).getParts()).hasSize(1);
            assertThat(streamGeneration.getOutput().get(0).getParts().get(0).getText()).isEqualTo("42");
        }
    }

    @Test
    void embeddingsWrapperDoesNotEnqueueGenerations() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = newClient(exporter)) {
            CreateEmbeddingResponse response = OpenAiEmbeddings.create(
                    client,
                    embeddingsRequestFixture(),
                    _request -> embeddingsResponseFixture(),
                    new OpenAiOptions());

            assertThat(response.model()).isEqualTo("text-embedding-3-small");
            assertThat(exporter.generations).isEmpty();
        }
    }

    @Test
    void embeddingsWrapperPropagatesProviderErrors() {
        CapturingExporter exporter = new CapturingExporter();
        assertThatThrownBy(() -> {
            try (Agento11yClient client = newClient(exporter)) {
                OpenAiEmbeddings.create(
                        client,
                        embeddingsRequestFixture(),
                        _request -> {
                            throw new RuntimeException("embedding provider blew up");
                        },
                        new OpenAiOptions());
            }
        }).isInstanceOf(RuntimeException.class).hasMessageContaining("embedding provider blew up");

        assertThat(exporter.generations).isEmpty();
    }

    @Test
    void embeddingsMapperExtractsInputCountsTokensAndDimensions() throws Exception {
        EmbeddingResult mapped = OpenAiEmbeddings.fromRequestResponse(embeddingsRequestFixture(), embeddingsResponseFixture());

        assertThat(mapped.getInputCount()).isEqualTo(2);
        assertThat(mapped.getInputTokens()).isEqualTo(7L);
        assertThat(mapped.getResponseModel()).isEqualTo("text-embedding-3-small");
        assertThat(mapped.getDimensions()).isEqualTo(3L);
        assertThat(mapped.getInputTexts()).containsExactly("alpha", "beta");
    }

    @Test
    void chatMapperLeavesThinkingUnsetWithoutReasoningConfig() throws Exception {
        ChatCompletionCreateParams request = ChatCompletionCreateParams.builder()
                .model("gpt-5")
                .addUserMessage("hello")
                .build();

        var mapped = OpenAiChatCompletions.fromRequestResponse(request, chatResponseFixture(), new OpenAiOptions());
        assertThat(mapped.getThinkingEnabled()).isNull();
    }

    private static Agento11yClient newClient(CapturingExporter exporter) {
        return new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig()
                        .setBatchSize(1)
                        .setFlushInterval(Duration.ofMinutes(10))
                        .setMaxRetries(0)));
    }

    private static ChatCompletionCreateParams chatRequestFixture() {
        return ChatCompletionCreateParams.builder()
                .model("gpt-5")
                .addSystemMessage("be concise")
                .addUserMessage("hello")
                .maxCompletionTokens(512L)
                .maxTokens(1024L)
                .temperature(0.3)
                .topP(0.8)
                .reasoningEffort(ReasoningEffort.MEDIUM)
                .build();
    }

    private static ChatCompletion chatResponseFixture() throws IOException {
        return json(
                """
                {
                  "id": "chatcmpl_1",
                  "choices": [
                    {
                      "finish_reason": "tool_calls",
                      "index": 0,
                      "message": {
                        "role": "assistant",
                        "content": "Calling tool",
                        "tool_calls": [
                          {
                            "id": "call_weather",
                            "type": "function",
                            "function": {
                              "name": "weather",
                              "arguments": "{\\"city\\":\\"Paris\\"}"
                            }
                          }
                        ]
                      }
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5-2026-02-01",
                  "object": "chat.completion",
                  "usage": {
                    "prompt_tokens": 120,
                    "completion_tokens": 42,
                    "total_tokens": 162,
                    "prompt_tokens_details": {"cached_tokens": 8},
                    "completion_tokens_details": {"reasoning_tokens": 5}
                  }
                }
                """,
                ChatCompletion.class);
    }

    private static ChatCompletionChunk chatChunkOne() throws IOException {
        return json(
                """
                {
                  "id": "chatcmpl_stream_1",
                  "choices": [
                    {
                      "delta": {
                        "content": "Calling tool",
                        "tool_calls": [
                          {
                            "index": 0,
                            "id": "call_weather",
                            "function": {
                              "name": "weather",
                              "arguments": "{\\"city\\":\\"Pa"
                            }
                          }
                        ]
                      },
                      "index": 0
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5",
                  "object": "chat.completion.chunk"
                }
                """,
                ChatCompletionChunk.class);
    }

    private static ChatCompletionChunk chatChunkTwo() throws IOException {
        return json(
                """
                {
                  "id": "chatcmpl_stream_1",
                  "choices": [
                    {
                      "delta": {
                        "content": " now.",
                        "tool_calls": [
                          {
                            "index": 0,
                            "function": {
                              "arguments": "ris\\"}"
                            }
                          }
                        ]
                      },
                      "finish_reason": "tool_calls",
                      "index": 0
                    }
                  ],
                  "created": 1,
                  "model": "gpt-5",
                  "object": "chat.completion.chunk",
                  "usage": {
                    "prompt_tokens": 20,
                    "completion_tokens": 5,
                    "total_tokens": 25
                  }
                }
                """,
                ChatCompletionChunk.class);
    }

    private static EmbeddingCreateParams embeddingsRequestFixture() {
        return EmbeddingCreateParams.builder()
                .model("text-embedding-3-small")
                .inputOfArrayOfStrings(List.of("alpha", "beta"))
                .dimensions(256L)
                .encodingFormat(EmbeddingCreateParams.EncodingFormat.FLOAT)
                .build();
    }

    private static CreateEmbeddingResponse embeddingsResponseFixture() throws IOException {
        return json(
                """
                {
                  "object": "list",
                  "model": "text-embedding-3-small",
                  "data": [
                    {
                      "index": 0,
                      "object": "embedding",
                      "embedding": [0.1, 0.2, 0.3]
                    },
                    {
                      "index": 1,
                      "object": "embedding",
                      "embedding": [0.4, 0.5, 0.6]
                    }
                  ],
                  "usage": {
                    "prompt_tokens": 7,
                    "total_tokens": 7
                  }
                }
                """,
                CreateEmbeddingResponse.class);
    }

    private static ResponseCreateParams responsesRequestFixture() {
        return ResponseCreateParams.builder()
                .model("gpt-5")
                .instructions("Be concise.")
                .input("hello")
                .maxOutputTokens(320L)
                .temperature(0.2)
                .topP(0.85)
                .build();
    }

    private static Response responsesResponseFixture() throws IOException {
        return json(
                """
                {
                  "id": "resp_1",
                  "created_at": 1,
                  "model": "gpt-5",
                  "object": "response",
                  "output": [
                    {
                      "id": "msg_1",
                      "type": "message",
                      "role": "assistant",
                      "status": "completed",
                      "content": [{"type": "output_text", "text": "world"}]
                    },
                    {
                      "id": "fc_1",
                      "type": "function_call",
                      "call_id": "call_weather",
                      "name": "weather",
                      "arguments": "{\\"city\\":\\"Paris\\"}"
                    }
                  ],
                  "parallel_tool_calls": false,
                  "tool_choice": "auto",
                  "tools": [],
                  "status": "completed",
                  "usage": {
                    "input_tokens": 80,
                    "output_tokens": 20,
                    "total_tokens": 100,
                    "input_tokens_details": {"cached_tokens": 2},
                    "output_tokens_details": {"reasoning_tokens": 3}
                  }
                }
                """,
                Response.class);
    }

    private static ResponseStreamEvent responseTextDeltaEvent(String delta, long sequenceNumber) throws IOException {
        return json(
                """
                {
                  "type": "response.output_text.delta",
                  "content_index": 0,
                  "delta": "%s",
                  "item_id": "msg_1",
                  "output_index": 0,
                  "sequence_number": %d
                }
                """.formatted(delta, sequenceNumber),
                ResponseStreamEvent.class);
    }

    private static ResponseStreamEvent responseCompletedEvent() throws IOException {
        return json(
                """
                {
                  "type": "response.completed",
                  "sequence_number": 3,
                  "response": {
                    "id": "resp_stream_1",
                    "created_at": 1,
                    "model": "gpt-5",
                    "object": "response",
                    "output": [],
                    "parallel_tool_calls": false,
                    "tool_choice": "auto",
                    "tools": [],
                    "status": "completed",
                    "usage": {
                      "input_tokens": 42,
                      "output_tokens": 14,
                      "total_tokens": 56
                    }
                  }
                }
                """,
                ResponseStreamEvent.class);
    }

    private static <T> T json(String value, Class<T> type) throws IOException {
        return ObjectMappers.jsonMapper().readValue(value, type);
    }

    private static final class FakeStreamResponse<T> implements StreamResponse<T> {
        private final List<T> values;

        private FakeStreamResponse(List<T> values) {
            this.values = values;
        }

        @Override
        public Stream<T> stream() {
            return values.stream();
        }

        @Override
        public void close() {
        }
    }

    @SuppressWarnings("unchecked")
    private static Generation singleDebugGeneration(Agento11yClient client) throws Exception {
        Field field = Agento11yClient.class.getDeclaredField("generations");
        field.setAccessible(true);
        List<Generation> generations = (List<Generation>) field.get(client);
        assertThat(generations).hasSize(1);
        return generations.get(0);
    }

    private static final class CapturingExporter implements GenerationExporter {
        private final List<Generation> generations = new CopyOnWriteArrayList<>();

        @Override
        public ExportGenerationsResponse exportGenerations(ExportGenerationsRequest request) {
            for (Generation generation : request.getGenerations()) {
                generations.add(generation.copy());
            }
            List<ExportGenerationResult> results = new ArrayList<>();
            for (Generation generation : request.getGenerations()) {
                results.add(new ExportGenerationResult().setGenerationId(generation.getId()).setAccepted(true));
            }
            return new ExportGenerationsResponse().setResults(results);
        }

        Generation singleGeneration() {
            assertThat(generations).hasSize(1);
            return generations.get(0);
        }
    }
}
