package com.grafana.agento11y.sdk.providers.gemini;

import static org.assertj.core.api.Assertions.assertThat;
import static org.assertj.core.api.Assertions.assertThatThrownBy;

import com.google.genai.types.Content;
import com.google.genai.types.EmbedContentConfig;
import com.google.genai.types.EmbedContentResponse;
import com.google.genai.types.GenerateContentConfig;
import com.google.genai.types.GenerateContentResponse;
import com.google.genai.types.Part;
import com.grafana.agento11y.sdk.EmbeddingResult;
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
import java.time.Duration;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.CopyOnWriteArrayList;
import org.junit.jupiter.api.Test;

class GeminiConformanceTest {
    @Test
    void syncAndStreamWrappersSetGeminiProviderAndModes() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            GeminiAdapter.completion(client, model(), contents(), config(), (_m, _c, _cfg) -> response(), new GeminiOptions());
            GeminiAdapter.completionStream(client, model(), contents(), config(), (_m, _c, _cfg) -> List.of(response()), new GeminiOptions());
        }

        assertThat(exporter.generations).hasSize(2);
        assertThat(exporter.generations.get(0).getModel().getProvider()).isEqualTo("gemini");
        assertThat(exporter.generations.get(0).getMode()).isEqualTo(GenerationMode.SYNC);
        assertThat(exporter.generations.get(0).getMaxTokens()).isEqualTo(512L);
        assertThat(exporter.generations.get(0).getTemperature()).isEqualTo(0.2);
        assertThat(exporter.generations.get(0).getTopP()).isEqualTo(0.75);
        assertThat(exporter.generations.get(0).getToolChoice()).isEqualTo("auto");
        assertThat(exporter.generations.get(0).getThinkingEnabled()).isTrue();
        assertThat(exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.request.thinking.budget_tokens")).isEqualTo(1536L);
        Object thinkingLevel = exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.request.thinking.level");
        if (thinkingLevel != null) {
            assertThat(thinkingLevel).isEqualTo("high");
        }
        assertThat(exporter.generations.get(0).getMetadata().get("agento11y.gen_ai.usage.tool_use_prompt_tokens")).isEqualTo(5L);
        assertThat(exporter.generations.get(1).getMode()).isEqualTo(GenerationMode.STREAM);
    }

    @Test
    void rawArtifactsAreOffByDefaultAndEnabledByOptIn() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            GeminiAdapter.completion(client, model(), contents(), config(), (_m, _c, _cfg) -> response(), new GeminiOptions());
            GeminiAdapter.completionStream(client, model(), contents(), config(), (_m, _c, _cfg) -> List.of(response()), new GeminiOptions());
            GeminiAdapter.completionStream(
                    client,
                    model(),
                    contents(),
                    config(),
                    (_m, _c, _cfg) -> List.of(response()),
                    new GeminiOptions().setRawArtifacts(true));
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
                GeminiAdapter.completion(
                        client,
                        model(),
                        contents(),
                        config(),
                        (_m, _c, _cfg) -> {
                            throw new RuntimeException("gemini failed");
                        },
                        new GeminiOptions());
            }
        }).isInstanceOf(RuntimeException.class).hasMessageContaining("gemini failed");

        assertThat(exporter.generations).hasSize(1);
        assertThat(exporter.generations.get(0).getCallError()).contains("gemini failed");
    }

    @Test
    void wrappersTolerateMissingProviderPayloadFields() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            GeminiAdapter.completion(
                    client,
                    model(),
                    contents(),
                    config(),
                    (_m, _c, _cfg) -> GenerateContentResponse.fromJson(
                            """
                            {
                              "responseId": "resp_malformed",
                              "modelVersion": "gemini-2.5-pro-001",
                              "candidates": []
                            }
                            """),
                    new GeminiOptions());
            GeminiAdapter.completionStream(
                    client,
                    model(),
                    contents(),
                    config(),
                    (_m, _c, _cfg) -> List.of(GenerateContentResponse.fromJson(
                            """
                            {
                              "modelVersion": "gemini-2.5-pro-001"
                            }
                            """)),
                    new GeminiOptions());
        }

        assertThat(exporter.generations).hasSize(2);
        assertThat(exporter.generations.get(0).getMode()).isEqualTo(GenerationMode.SYNC);
        assertThat(exporter.generations.get(0).getResponseId()).isEqualTo("resp_malformed");
        assertThat(exporter.generations.get(0).getResponseModel()).isEqualTo("gemini-2.5-pro-001");
        assertThat(exporter.generations.get(0).getOutput()).isEmpty();
        assertThat(exporter.generations.get(0).getStopReason()).isEmpty();
        assertThat(exporter.generations.get(1).getMode()).isEqualTo(GenerationMode.STREAM);
        assertThat(exporter.generations.get(1).getResponseModel()).isEqualTo("gemini-2.5-pro-001");
        assertThat(exporter.generations.get(1).getOutput()).isEmpty();
    }

    @Test
    void embeddingWrapperDoesNotEnqueueGenerations() throws Exception {
        CapturingExporter exporter = new CapturingExporter();
        try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                .setTracer(GlobalOpenTelemetry.getTracer("test"))
                .setGenerationExporter(exporter)
                .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {

            EmbedContentResponse response = GeminiAdapter.embedContent(
                    client,
                    "gemini-embedding-001",
                    List.of("alpha", "beta"),
                    embedConfig(),
                    (_m, _c, _cfg) -> embedResponse(),
                    new GeminiOptions());
            assertThat(response.embeddings()).isPresent();
        }

        assertThat(exporter.generations).isEmpty();
    }

    @Test
    void embeddingWrapperPropagatesProviderErrors() {
        CapturingExporter exporter = new CapturingExporter();
        assertThatThrownBy(() -> {
            try (Agento11yClient client = new Agento11yClient(new Agento11yClientConfig()
                    .setTracer(GlobalOpenTelemetry.getTracer("test"))
                    .setGenerationExporter(exporter)
                    .setGenerationExport(new GenerationExportConfig().setBatchSize(1).setFlushInterval(Duration.ofMinutes(10)).setMaxRetries(0)))) {
                GeminiAdapter.embedContent(
                        client,
                        "gemini-embedding-001",
                        List.of("alpha"),
                        embedConfig(),
                        (_m, _c, _cfg) -> {
                            throw new RuntimeException("gemini embedding failed");
                        },
                        new GeminiOptions());
            }
        }).isInstanceOf(RuntimeException.class).hasMessageContaining("gemini embedding failed");

        assertThat(exporter.generations).isEmpty();
    }

    @Test
    void embeddingMapperExtractsUsageAndDimensions() {
        EmbeddingResult result = GeminiAdapter.embeddingFromResponse(
                "gemini-embedding-001",
                List.of("alpha", "beta"),
                embedConfig(),
                embedResponse());

        assertThat(result.getInputCount()).isEqualTo(2);
        assertThat(result.getInputTokens()).isEqualTo(7L);
        assertThat(result.getDimensions()).isEqualTo(3L);
        assertThat(result.getInputTexts()).containsExactly("alpha", "beta");
    }

    @Test
    void mapperSetsThinkingFalseWhenIncludeThoughtsFalse() {
        var mapped = GeminiAdapter.fromRequestResponse(
                model(),
                contents(),
                GenerateContentConfig.fromJson("""
                        {
                          "maxOutputTokens": 512,
                          "temperature": 0.2,
                          "topP": 0.75,
                          "thinkingConfig": {
                            "includeThoughts": false,
                            "thinkingBudget": 1536
                          }
                        }
                        """),
                response(),
                new GeminiOptions());
        assertThat(mapped.getThinkingEnabled()).isFalse();
    }

    private static String model() {
        return "gemini-2.5-pro";
    }

    private static List<Content> contents() {
        return List.of(Content.builder()
                .role("user")
                .parts(Part.fromText("hi"))
                .build());
    }

    private static GenerateContentConfig config() {
        return GenerateContentConfig.fromJson("""
                {
                  "maxOutputTokens": 512,
                  "temperature": 0.2,
                  "topP": 0.75,
                  "toolConfig": {
                    "functionCallingConfig": {
                      "mode": "AUTO"
                    }
                  },
                  "thinkingConfig": {
                    "includeThoughts": true,
                    "thinkingBudget": 1536,
                    "thinkingLevel": "HIGH"
                  }
                }
                """);
    }

    private static GenerateContentResponse response() {
        return GenerateContentResponse.fromJson("""
                {
                  "responseId": "resp_1",
                  "modelVersion": "gemini-2.5-pro-001",
                  "candidates": [
                    {
                      "finishReason": "STOP",
                      "content": {
                        "role": "model",
                        "parts": [
                          {"text": "ok"}
                        ]
                      }
                    }
                  ],
                  "usageMetadata": {
                    "promptTokenCount": 10,
                    "candidatesTokenCount": 5,
                    "totalTokenCount": 15,
                    "cachedContentTokenCount": 2,
                    "thoughtsTokenCount": 3,
                    "toolUsePromptTokenCount": 5
                  }
                }
                """);
    }

    private static EmbedContentConfig embedConfig() {
        return EmbedContentConfig.fromJson("""
                {
                  "outputDimensionality": 256
                }
                """);
    }

    private static EmbedContentResponse embedResponse() {
        return EmbedContentResponse.fromJson("""
                {
                  "embeddings": [
                    {
                      "values": [0.1, 0.2, 0.3],
                      "statistics": {"tokenCount": 4}
                    },
                    {
                      "values": [0.4, 0.5, 0.6],
                      "statistics": {"tokenCount": 3}
                    }
                  ]
                }
                """);
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
