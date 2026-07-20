package com.grafana.agento11y.sdk.providers.gemini;

import com.google.genai.types.GenerateContentResponse;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

/** Stitched summary for Gemini streaming calls. */
public final class GeminiStreamSummary {
    private String outputText = "";
    private GenerateContentResponse finalResponse;
    private Instant firstChunkAt;
    private final List<GenerateContentResponse> chunks = new ArrayList<>();

    public String getOutputText() {
        return outputText;
    }

    public GeminiStreamSummary setOutputText(String outputText) {
        this.outputText = outputText == null ? "" : outputText;
        return this;
    }

    public GenerateContentResponse getFinalResponse() {
        return finalResponse;
    }

    public GeminiStreamSummary setFinalResponse(GenerateContentResponse finalResponse) {
        this.finalResponse = finalResponse;
        return this;
    }

    public Instant getFirstChunkAt() {
        return firstChunkAt;
    }

    public GeminiStreamSummary setFirstChunkAt(Instant firstChunkAt) {
        this.firstChunkAt = firstChunkAt;
        return this;
    }

    public List<GenerateContentResponse> getChunks() {
        return chunks;
    }

    public GeminiStreamSummary setChunks(List<GenerateContentResponse> chunks) {
        this.chunks.clear();
        if (chunks != null) {
            this.chunks.addAll(chunks);
        }
        return this;
    }
}
