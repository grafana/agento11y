package com.grafana.agento11y.sdk.providers.openai;

import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.ThrowingFunction;
import com.openai.core.http.StreamResponse;
import com.openai.helpers.ResponseAccumulator;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStreamEvent;
import java.time.Instant;

/** OpenAI Responses wrappers and mappers using official OpenAI Java SDK types. */
public final class OpenAiResponses {
    private OpenAiResponses() {
    }

    public static Response create(
            Agento11yClient client,
            ResponseCreateParams request,
            ThrowingFunction<ResponseCreateParams, Response> providerCall,
            OpenAiOptions options) throws Exception {
        OpenAiOptions resolved = options == null ? new OpenAiOptions() : options;
        return client.withGeneration(OpenAiGenerationMapper.responsesStart(request, resolved), recorder -> {
            Response response = providerCall.apply(request);
            recorder.setResult(fromRequestResponse(request, response, resolved));
            return response;
        });
    }

    public static ResponsesStreamSummary createStreaming(
            Agento11yClient client,
            ResponseCreateParams request,
            ThrowingFunction<ResponseCreateParams, StreamResponse<ResponseStreamEvent>> providerCall,
            OpenAiOptions options) throws Exception {
        OpenAiOptions resolved = options == null ? new OpenAiOptions() : options;
        return client.withStreamingGeneration(OpenAiGenerationMapper.responsesStart(request, resolved), recorder -> {
            ResponsesStreamSummary summary = new ResponsesStreamSummary();
            ResponseAccumulator accumulator = ResponseAccumulator.create();

            try (StreamResponse<ResponseStreamEvent> stream = providerCall.apply(request)) {
                stream.stream().forEach(event -> {
                    if (summary.getFirstChunkAt() == null) {
                        Instant firstChunkAt = Instant.now();
                        summary.setFirstChunkAt(firstChunkAt);
                        recorder.setFirstTokenAt(firstChunkAt);
                    }
                    summary.getEvents().add(event);
                    try {
                        accumulator.accumulate(event);
                    } catch (Exception ignored) {
                        // Keep collecting events; mapper can still stitch from raw stream events.
                    }
                });
            }

            try {
                summary.setFinalResponse(accumulator.response());
            } catch (Exception ignored) {
                summary.setFinalResponse(null);
            }

            recorder.setResult(fromStream(request, summary, resolved));
            return summary;
        });
    }

    public static GenerationResult fromRequestResponse(
            ResponseCreateParams request,
            Response response,
            OpenAiOptions options) {
        return OpenAiGenerationMapper.responsesFromRequestResponse(request, response, options);
    }

    public static GenerationResult fromStream(
            ResponseCreateParams request,
            ResponsesStreamSummary summary,
            OpenAiOptions options) {
        return OpenAiGenerationMapper.responsesFromStream(request, summary, options);
    }
}
