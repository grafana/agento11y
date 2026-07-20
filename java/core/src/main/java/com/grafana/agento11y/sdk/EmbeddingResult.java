package com.grafana.agento11y.sdk;

import java.util.ArrayList;
import java.util.List;

/** Result fields set before an embedding span is finalized. */
public final class EmbeddingResult {
    private int inputCount;
    private long inputTokens;
    private final List<String> inputTexts = new ArrayList<>();
    private String responseModel = "";
    private Long dimensions;

    public int getInputCount() {
        return inputCount;
    }

    public EmbeddingResult setInputCount(int inputCount) {
        this.inputCount = inputCount;
        return this;
    }

    public long getInputTokens() {
        return inputTokens;
    }

    public EmbeddingResult setInputTokens(long inputTokens) {
        this.inputTokens = inputTokens;
        return this;
    }

    public List<String> getInputTexts() {
        return inputTexts;
    }

    public EmbeddingResult setInputTexts(List<String> inputTexts) {
        this.inputTexts.clear();
        if (inputTexts != null) {
            this.inputTexts.addAll(inputTexts);
        }
        return this;
    }

    public String getResponseModel() {
        return responseModel;
    }

    public EmbeddingResult setResponseModel(String responseModel) {
        this.responseModel = responseModel == null ? "" : responseModel;
        return this;
    }

    public Long getDimensions() {
        return dimensions;
    }

    public EmbeddingResult setDimensions(Long dimensions) {
        this.dimensions = dimensions;
        return this;
    }

    public EmbeddingResult copy() {
        return new EmbeddingResult()
                .setInputCount(inputCount)
                .setInputTokens(inputTokens)
                .setInputTexts(inputTexts)
                .setResponseModel(responseModel)
                .setDimensions(dimensions);
    }
}
