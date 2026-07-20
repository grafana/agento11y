package com.grafana.agento11y.sdk.providers.openai;

import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

/** Captures OpenAI chat-completions stream chunks and optional stitched final response. */
public final class ChatCompletionsStreamSummary {
    private final List<ChatCompletionChunk> chunks = new ArrayList<>();
    private ChatCompletion finalResponse;
    private Instant firstChunkAt;

    public List<ChatCompletionChunk> getChunks() {
        return chunks;
    }

    public ChatCompletionsStreamSummary setChunks(List<ChatCompletionChunk> chunks) {
        this.chunks.clear();
        if (chunks != null) {
            this.chunks.addAll(chunks);
        }
        return this;
    }

    public ChatCompletion getFinalResponse() {
        return finalResponse;
    }

    public ChatCompletionsStreamSummary setFinalResponse(ChatCompletion finalResponse) {
        this.finalResponse = finalResponse;
        return this;
    }

    public Instant getFirstChunkAt() {
        return firstChunkAt;
    }

    public ChatCompletionsStreamSummary setFirstChunkAt(Instant firstChunkAt) {
        this.firstChunkAt = firstChunkAt;
        return this;
    }
}
