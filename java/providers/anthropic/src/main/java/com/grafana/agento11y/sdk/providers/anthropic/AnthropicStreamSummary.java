package com.grafana.agento11y.sdk.providers.anthropic;

import com.anthropic.models.messages.Message;
import com.anthropic.models.messages.RawMessageStreamEvent;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

/** Stitched summary for Anthropic streaming calls. */
public final class AnthropicStreamSummary {
    private String outputText = "";
    private Message finalResponse;
    private Instant firstChunkAt;
    private final List<RawMessageStreamEvent> events = new ArrayList<>();

    public String getOutputText() {
        return outputText;
    }

    public AnthropicStreamSummary setOutputText(String outputText) {
        this.outputText = outputText == null ? "" : outputText;
        return this;
    }

    public Message getFinalResponse() {
        return finalResponse;
    }

    public AnthropicStreamSummary setFinalResponse(Message finalResponse) {
        this.finalResponse = finalResponse;
        return this;
    }

    public Instant getFirstChunkAt() {
        return firstChunkAt;
    }

    public AnthropicStreamSummary setFirstChunkAt(Instant firstChunkAt) {
        this.firstChunkAt = firstChunkAt;
        return this;
    }

    public List<RawMessageStreamEvent> getEvents() {
        return events;
    }

    public AnthropicStreamSummary setEvents(List<RawMessageStreamEvent> events) {
        this.events.clear();
        if (events != null) {
            this.events.addAll(events);
        }
        return this;
    }
}
