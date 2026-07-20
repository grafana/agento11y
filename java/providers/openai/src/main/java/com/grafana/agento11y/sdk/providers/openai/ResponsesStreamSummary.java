package com.grafana.agento11y.sdk.providers.openai;

import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseStreamEvent;
import java.time.Instant;
import java.util.ArrayList;
import java.util.List;

/** Captures OpenAI Responses stream events and optional stitched final response. */
public final class ResponsesStreamSummary {
    private final List<ResponseStreamEvent> events = new ArrayList<>();
    private Response finalResponse;
    private Instant firstChunkAt;

    public List<ResponseStreamEvent> getEvents() {
        return events;
    }

    public ResponsesStreamSummary setEvents(List<ResponseStreamEvent> events) {
        this.events.clear();
        if (events != null) {
            this.events.addAll(events);
        }
        return this;
    }

    public Response getFinalResponse() {
        return finalResponse;
    }

    public ResponsesStreamSummary setFinalResponse(Response finalResponse) {
        this.finalResponse = finalResponse;
        return this;
    }

    public Instant getFirstChunkAt() {
        return firstChunkAt;
    }

    public ResponsesStreamSummary setFirstChunkAt(Instant firstChunkAt) {
        this.firstChunkAt = firstChunkAt;
        return this;
    }
}
