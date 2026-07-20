package com.grafana.agento11y.sdk.providers.anthropic;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.anthropic.core.ObjectMappers;
import com.anthropic.core.http.StreamResponse;
import com.anthropic.models.messages.Message;
import com.anthropic.models.messages.MessageCreateParams;
import com.grafana.agento11y.sdk.ExportGenerationResult;
import com.grafana.agento11y.sdk.ExportGenerationsRequest;
import com.grafana.agento11y.sdk.ExportGenerationsResponse;
import com.grafana.agento11y.sdk.Generation;
import com.grafana.agento11y.sdk.GenerationExportConfig;
import com.grafana.agento11y.sdk.GenerationExporter;
import com.grafana.agento11y.sdk.GenerationMode;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.Agento11yClientConfig;
import io.opentelemetry.api.GlobalOpenTelemetry;
import java.io.IOException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import java.util.stream.Stream;
import org.junit.jupiter.api.Test;

class AnthropicConformanceTest {
    @Test
    void syncAndStreamWrappersSetAnthropicProviderAndModes() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            AnthropicAdapter.completion(client, request(), _r -> response(), new AnthropicOptions());
            AnthropicAdapter.completionStream(
                    client,
                    request(),
                    _r -> new FakeStreamResponse<>(List.of()),
                    new AnthropicOptions());
        }

        assertThat(exporter.generations).hasSize(2);
        assertThat(exporter.generations.get(0).getModel().getProvider()).isEqualTo("anthropic");
        assertThat(exporter.generations.get(0).getMode()).isEqualTo(GenerationMode.SYNC);
        assertThat(exporter.generations.get(0).getMaxTokens()).isEqualTo(256L);
        assertThat(exporter.generations.get(0).getTemperature()).isEqualTo(0.25);
        assertThat(exporter.generations.get(0).getTopP()).isEqualTo(0.9);
        assertThat(exporter.generations.get(0).getToolChoice()).contains("weather");
        assertThat(exporter.generations.get(0).getThinkingEnabled()).isTrue();
        assertThat(exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.request.thinking.budget_tokens")).isEqualTo(2048L);
        assertThat(exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.usage.server_tool_use.web_search_requests")).isEqualTo(2L);
        assertThat(exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.usage.server_tool_use.total_requests")).isEqualTo(2L);
        assertThat(exporter.generations.get(1).getMode()).isEqualTo(GenerationMode.STREAM);
    }

    @Test
    void rawArtifactsAreOffByDefaultAndEnabledByOptIn() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            AnthropicAdapter.completion(client, request(), _r -> response(), new AnthropicOptions());
            AnthropicAdapter.completionStream(
                    client,
                    request(),
                    _r -> new FakeStreamResponse<>(List.of()),
                    new AnthropicOptions());
            AnthropicAdapter.completionStream(
                    client,
                    request(),
                    _r -> new FakeStreamResponse<>(List.of()),
                    new AnthropicOptions().setRawArtifacts(true));
        }

        assertThat(exporter.generations).hasSize(3);
        assertThat(exporter.generations.get(0).getArtifacts()).isEmpty();
        assertThat(exporter.generations.get(1).getArtifacts()).isEmpty();
        assertThat(exporter.generations.get(2).getArtifacts()).hasSizeGreaterThanOrEqualTo(2);
    }

    @Test
    void providerErrorsPopulateCallError() {
        CapturingExporter exporter = new CapturingExporter();
        assertThatThrownBy(() -> {
            try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                    .setTracer(GlobalOpenTelemetry.getTracer("test"))
                    .setGenerationExporter(exporter)
                    .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {
                AnthropicAdapter.completion(
                        client,
                        request(),
                        _r -> {
                            throw new RuntimeException("anthropic failed");
                        },
                        new AnthropicOptions());
            }
        }).isInstanceOf(RuntimeException.class).hasMessageContaining("anthropic failed");

        assertThat(exporter.generations).hasSize(1);
        assertThat(exporter.generations.get(0).getCallError()).contains("anthropic failed");
    }

    @Test
    void wrappersTolerateMissingProviderPayloadFields() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            AnthropicAdapter.completion(
                    client,
                    request(),
                    _r -> ObjectMappers.jsonMapper().readValue(
                            """
                            {
                              "id": "msg_malformed",
                              "content": [],
                              "model": "claude-sonnet-4",
                              "usage": {
                                "input_tokens": 0,
                                "output_tokens": 0
                              }
                            }
                            """,
                            Message.class),
                    new AnthropicOptions());
            AnthropicAdapter.completionStream(
                    client,
                    request(),
                    _r -> new FakeStreamResponse<>(List.of()),
                    new AnthropicOptions());
        }

        assertThat(exporter.generations).hasSize(2);
        assertThat(exporter.generations.get(0).getMode()).isEqualTo(GenerationMode.SYNC);
        assertThat(exporter.generations.get(0).getResponseId()).isEqualTo("msg_malformed");
        assertThat(exporter.generations.get(0).getResponseModel()).isEqualTo("claude-sonnet-4");
        assertThat(exporter.generations.get(0).getOutput()).isEmpty();
        assertThat(exporter.generations.get(0).getStopReason()).isEmpty();
        assertThat(exporter.generations.get(1).getMode()).isEqualTo(GenerationMode.STREAM);
        assertThat(exporter.generations.get(1).getResponseModel()).isEqualTo("claude-sonnet-4");
        assertThat(exporter.generations.get(1).getOutput()).isEmpty();
    }

    @Test
    void embeddingConformanceIsExplicitlyUnsupportedWithoutPublicSurface() {
        assertThat(AnthropicAdapter.class).isNotNull();
        assertThatThrownBy(() -> Class.forName("com.grafana.agento11y.sdk.providers.anthropic.AnthropicEmbeddings"))
                .isInstanceOf(ClassNotFoundException.class);
    }

    @Test
    void mapperSetsThinkingFalseWhenDisabled() throws Exception {
        MessageCreateParams request = MessageCreateParams.builder()
                .model("claude-sonnet-4")
                .maxTokens(256)
                .thinking(com.anthropic.models.messages.ThinkingConfigDisabled.builder().build())
                .addUserMessage("hi")
                .build();

        var mapped = AnthropicAdapter.fromRequestResponse(request, response(), new AnthropicOptions());
        assertThat(mapped.getThinkingEnabled()).isFalse();
    }

    @Test
    void mapperRejectsMissingResponse() {
        assertThatThrownBy(() -> AnthropicAdapter.fromRequestResponse(request(), null, new AnthropicOptions()))
                .isInstanceOf(NullPointerException.class);
    }

    private static MessageCreateParams request() {
        return MessageCreateParams.builder()
                .model("claude-sonnet-4")
                .maxTokens(256)
                .temperature(0.25)
                .topP(0.9)
                .toolToolChoice("weather")
                .enabledThinking(2048)
                .addUserMessage("hi")
                .build();
    }

    private static Message response() throws IOException {
        return ObjectMappers.jsonMapper().readValue(
                """
                {
                  "id": "msg_1",
                  "content": [
                    {"type": "text", "text": "ok"}
                  ],
                  "model": "claude-sonnet-4",
                  "stop_reason": "end_turn",
                  "usage": {
                    "input_tokens": 11,
                    "output_tokens": 7,
                    "cache_read_input_tokens": 2,
                    "cache_creation_input_tokens": 3,
                    "server_tool_use": {
                      "web_search_requests": 2
                    }
                  }
                }
                """,
                Message.class);
    }

    private static final class FakeStreamResponse<T> implements StreamResponse<T> {
        private final List<T> events;

        private FakeStreamResponse(List<T> events) {
            this.events = events;
        }

        @Override
        public Stream<T> stream() {
            return events.stream();
        }

        @Override
        public void close() {
        }
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
    }
}
