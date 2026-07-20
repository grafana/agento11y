package com.grafana.agento11y.sdk.providers.openai;

import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.ThrowingFunction;
import com.openai.core.http.StreamResponse;
import com.openai.helpers.ChatCompletionAccumulator;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import java.time.Instant;

/** OpenAI chat-completions wrappers and mappers using official OpenAI Java SDK types. */
public final class OpenAiChatCompletions {
    private OpenAiChatCompletions() {
    }

    public static ChatCompletion create(
            Agento11yClient client,
            ChatCompletionCreateParams request,
            ThrowingFunction<ChatCompletionCreateParams, ChatCompletion> providerCall,
            OpenAiOptions options) throws Exception {
        OpenAiOptions resolved = options == null ? new OpenAiOptions() : options;
        return client.withGeneration(OpenAiGenerationMapper.chatCompletionsStart(request, resolved), recorder -> {
            ChatCompletion response = providerCall.apply(request);
            recorder.setResult(fromRequestResponse(request, response, resolved));
            return response;
        });
    }

    public static ChatCompletionsStreamSummary createStreaming(
            Agento11yClient client,
            ChatCompletionCreateParams request,
            ThrowingFunction<ChatCompletionCreateParams, StreamResponse<ChatCompletionChunk>> providerCall,
            OpenAiOptions options) throws Exception {
        OpenAiOptions resolved = options == null ? new OpenAiOptions() : options;
        return client.withStreamingGeneration(OpenAiGenerationMapper.chatCompletionsStart(request, resolved), recorder -> {
            ChatCompletionsStreamSummary summary = new ChatCompletionsStreamSummary();
            ChatCompletionAccumulator accumulator = ChatCompletionAccumulator.create();

            try (StreamResponse<ChatCompletionChunk> stream = providerCall.apply(request)) {
                stream.stream().forEach(chunk -> {
                    if (summary.getFirstChunkAt() == null) {
                        Instant firstChunkAt = Instant.now();
                        summary.setFirstChunkAt(firstChunkAt);
                        recorder.setFirstTokenAt(firstChunkAt);
                    }
                    summary.getChunks().add(chunk);
                    try {
                        accumulator.accumulate(chunk);
                    } catch (Exception ignored) {
                        // Keep collecting chunks; mapping can still fall back to chunk-only stitching.
                    }
                });
            }

            try {
                summary.setFinalResponse(accumulator.chatCompletion());
            } catch (Exception ignored) {
                summary.setFinalResponse(null);
            }

            recorder.setResult(fromStream(request, summary, resolved));
            return summary;
        });
    }

    public static GenerationResult fromRequestResponse(
            ChatCompletionCreateParams request,
            ChatCompletion response,
            OpenAiOptions options) {
        return OpenAiGenerationMapper.chatCompletionsFromRequestResponse(request, response, options);
    }

    public static GenerationResult fromStream(
            ChatCompletionCreateParams request,
            ChatCompletionsStreamSummary summary,
            OpenAiOptions options) {
        return OpenAiGenerationMapper.chatCompletionsFromStream(request, summary, options);
    }
}
