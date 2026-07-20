package com.grafana.agento11y.sdk.providers.openai;

import com.grafana.agento11y.sdk.EmbeddingResult;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.ThrowingFunction;
import com.openai.models.embeddings.CreateEmbeddingResponse;
import com.openai.models.embeddings.EmbeddingCreateParams;

/** OpenAI embeddings wrappers and mappers using official OpenAI Java SDK types. */
public final class OpenAiEmbeddings {
    private OpenAiEmbeddings() {
    }

    public static CreateEmbeddingResponse create(
            Agento11yClient client,
            EmbeddingCreateParams request,
            ThrowingFunction<EmbeddingCreateParams, CreateEmbeddingResponse> providerCall,
            OpenAiOptions options) throws Exception {
        OpenAiOptions resolved = options == null ? new OpenAiOptions() : options;
        return client.withEmbedding(OpenAiGenerationMapper.embeddingsStart(request, resolved), recorder -> {
            CreateEmbeddingResponse response = providerCall.apply(request);
            recorder.setResult(fromRequestResponse(request, response));
            return response;
        });
    }

    public static EmbeddingResult fromRequestResponse(
            EmbeddingCreateParams request,
            CreateEmbeddingResponse response) {
        return OpenAiGenerationMapper.embeddingsFromRequestResponse(request, response);
    }
}
