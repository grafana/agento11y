package com.grafana.agento11y.sdk.providers.gemini;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.google.genai.JsonSerializable;
import com.google.genai.types.Content;
import com.google.genai.types.EmbedContentConfig;
import com.google.genai.types.EmbedContentResponse;
import com.google.genai.types.GenerateContentConfig;
import com.google.genai.types.GenerateContentParameters;
import com.google.genai.types.GenerateContentResponse;
import com.grafana.agento11y.sdk.Artifact;
import com.grafana.agento11y.sdk.ArtifactKind;
import com.grafana.agento11y.sdk.EmbeddingResult;
import com.grafana.agento11y.sdk.EmbeddingStart;
import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.PartMetadata;
import com.grafana.agento11y.sdk.Agento11yClient;
import com.grafana.agento11y.sdk.TokenUsage;
import com.grafana.agento11y.sdk.ToolCall;
import com.grafana.agento11y.sdk.ToolDefinition;
import com.grafana.agento11y.sdk.ToolResultPart;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Gemini wrappers and mappers using official Google GenAI Java SDK request/response types. */
public final class GeminiAdapter {
    private static final String THINKING_BUDGET_METADATA_KEY = "agento11y.gen_ai.request.thinking.budget_tokens";
    private static final String THINKING_LEVEL_METADATA_KEY = "agento11y.gen_ai.request.thinking.level";
    private static final String USAGE_TOOL_USE_PROMPT_TOKENS_METADATA_KEY = "agento11y.gen_ai.usage.tool_use_prompt_tokens";
    private static final ObjectMapper JSON = new ObjectMapper();
    private static final ObjectMapper CANONICAL_JSON = new ObjectMapper()
            .configure(MapperFeature.SORT_PROPERTIES_ALPHABETICALLY, true)
            .configure(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS, true);
    private static final TypeReference<Map<String, Object>> MAP_TYPE = new TypeReference<>() {
    };

    private GeminiAdapter() {
    }

    @FunctionalInterface
    public interface ThrowingGenerateContentCall<T> {
        T apply(String model, List<Content> contents, GenerateContentConfig config) throws Exception;
    }

    @FunctionalInterface
    public interface ThrowingEmbedContentCall<T> {
        T apply(String model, List<String> contents, EmbedContentConfig config) throws Exception;
    }

    public static GenerateContentResponse completion(
            Agento11yClient client,
            String model,
            List<Content> contents,
            GenerateContentConfig config,
            ThrowingGenerateContentCall<GenerateContentResponse> providerCall,
            GeminiOptions options) throws Exception {
        RequestContext requestContext = buildRequest(model, contents, config);
        GeminiOptions resolved = resolveOptions(options);

        return client.withGeneration(startFromRequest(requestContext.request(), resolved), recorder -> {
            GenerateContentResponse response = providerCall.apply(requestContext.model(), requestContext.contents(), requestContext.config());
            recorder.setResult(fromRequestResponse(requestContext.request(), response, resolved));
            return response;
        });
    }

    public static GeminiStreamSummary completionStream(
            Agento11yClient client,
            String model,
            List<Content> contents,
            GenerateContentConfig config,
            ThrowingGenerateContentCall<? extends Iterable<GenerateContentResponse>> providerCall,
            GeminiOptions options) throws Exception {
        RequestContext requestContext = buildRequest(model, contents, config);
        GeminiOptions resolved = resolveOptions(options);

        return client.withStreamingGeneration(startFromRequest(requestContext.request(), resolved), recorder -> {
            GeminiStreamSummary summary = new GeminiStreamSummary();
            StringBuilder outputText = new StringBuilder();

            Iterable<GenerateContentResponse> stream = providerCall.apply(
                    requestContext.model(), requestContext.contents(), requestContext.config());
            try {
                for (GenerateContentResponse chunk : stream) {
                    if (summary.getFirstChunkAt() == null) {
                        Instant firstChunkAt = Instant.now();
                        summary.setFirstChunkAt(firstChunkAt);
                        recorder.setFirstTokenAt(firstChunkAt);
                    }
                    summary.getChunks().add(chunk);
                    summary.setFinalResponse(chunk);
                    appendText(outputText, extractChunkText(chunk));
                }
            } finally {
                if (stream instanceof AutoCloseable closeable) {
                    closeable.close();
                }
            }

            if (outputText.length() > 0) {
                summary.setOutputText(outputText.toString());
            }

            if (summary.getFinalResponse() != null && summary.getOutputText().isBlank()) {
                try {
                    summary.setOutputText(summary.getFinalResponse().text());
                } catch (Exception ignored) {
                    summary.setOutputText("");
                }
            }

            recorder.setResult(fromStream(requestContext.request(), summary, resolved));
            return summary;
        });
    }

    public static EmbedContentResponse embedContent(
            Agento11yClient client,
            String model,
            List<String> contents,
            EmbedContentConfig config,
            ThrowingEmbedContentCall<EmbedContentResponse> providerCall,
            GeminiOptions options) throws Exception {
        GeminiOptions resolved = resolveOptions(options);
        String requestModel = model == null ? "" : model;
        List<String> requestContents = contents == null ? List.of() : List.copyOf(contents);

        EmbeddingStart start = embeddingStartFromRequest(requestModel, config, resolved);
        return client.withEmbedding(start, recorder -> {
            EmbedContentResponse response = providerCall.apply(requestModel, requestContents, config);
            recorder.setResult(embeddingFromResponse(requestModel, requestContents, config, response));
            return response;
        });
    }

    public static GenerationResult fromRequestResponse(
            String model,
            List<Content> contents,
            GenerateContentConfig config,
            GenerateContentResponse response,
            GeminiOptions options) {
        RequestContext requestContext = buildRequest(model, contents, config);
        return fromRequestResponse(requestContext.request(), response, options);
    }

    public static GenerationResult fromStream(
            String model,
            List<Content> contents,
            GenerateContentConfig config,
            GeminiStreamSummary summary,
            GeminiOptions options) {
        RequestContext requestContext = buildRequest(model, contents, config);
        return fromStream(requestContext.request(), summary, options);
    }

    public static EmbeddingResult embeddingFromResponse(
            String model,
            List<String> contents,
            EmbedContentConfig config,
            EmbedContentResponse response) {
        Long requestedDimensions = embeddingRequestedDimensions(config);
        EmbeddingResult result = new EmbeddingResult()
                .setInputCount(embeddingInputCount(contents))
                .setInputTexts(embeddingInputTexts(contents));

        if (response == null) {
            result.setDimensions(requestedDimensions);
            return result;
        }

        long inputTokens = 0L;
        if (response.embeddings().isPresent()) {
            for (var embedding : response.embeddings().orElse(List.of())) {
                if (embedding == null) {
                    continue;
                }
                var statistics = embedding.statistics().orElse(null);
                if (statistics != null && statistics.tokenCount().isPresent()) {
                    inputTokens += statistics.tokenCount().orElse(0f).longValue();
                }
                if (result.getDimensions() == null && embedding.values().isPresent()) {
                    List<Float> values = embedding.values().orElse(List.of());
                    if (!values.isEmpty()) {
                        result.setDimensions((long) values.size());
                    }
                }
            }
        }
        result.setInputTokens(inputTokens);
        if (result.getDimensions() == null) {
            result.setDimensions(requestedDimensions);
        }
        return result;
    }

    private static GenerationResult fromRequestResponse(
            GenerateContentParameters request,
            GenerateContentResponse response,
            GeminiOptions options) {
        GeminiOptions resolved = resolveOptions(options);
        RequestMapping requestMapping = mapRequest(toMap(request));
        Map<String, Object> responsePayload = toMap(response);
        Map<String, Object> usagePayload = asMap(first(responsePayload, "usageMetadata", "usage_metadata"));
        LinkedHashMap<String, Object> metadata = metadataWithThinking(
                resolved.getMetadata(),
                requestMapping.thinkingBudget,
                requestMapping.thinkingLevel);
        metadata.putAll(geminiUsageMetadata(usagePayload));

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("gemini").setName(requestMapping.model))
                .setResponseId(asString(first(responsePayload, "responseId", "response_id")))
                .setResponseModel(firstNonBlank(
                        asString(first(responsePayload, "modelVersion", "model_version")),
                        requestMapping.model))
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setStopReason(normalizeStopReason(responsePayload))
                .setUsage(mapUsage(usagePayload))
                .setMetadata(metadata)
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(mapOutput(responsePayload));
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "gemini.models.generate_content.request", request));
            result.getArtifacts().add(toArtifact(ArtifactKind.RESPONSE, "gemini.models.generate_content.response", response));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "gemini.models.generate_content.tools", requestMapping.tools));
            }
        }

        return result;
    }

    private static GenerationResult fromStream(
            GenerateContentParameters request,
            GeminiStreamSummary summary,
            GeminiOptions options) {
        GeminiOptions resolved = resolveOptions(options);
        if (summary.getFinalResponse() != null) {
            GenerationResult mapped = fromRequestResponse(request, summary.getFinalResponse(), resolved);
            if (resolved.isRawArtifacts()) {
                mapped.getArtifacts().add(toArtifact(
                        ArtifactKind.PROVIDER_EVENT,
                        "gemini.models.generate_content.stream_events",
                        summary.getChunks()));
            }
            return mapped;
        }

        RequestMapping requestMapping = mapRequest(toMap(request));
        LinkedHashMap<String, Object> metadata = metadataWithThinking(
                resolved.getMetadata(),
                requestMapping.thinkingBudget,
                requestMapping.thinkingLevel);
        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("gemini").setName(requestMapping.model))
                .setResponseModel(requestMapping.model)
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setUsage(new TokenUsage())
                .setMetadata(metadata)
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        if (!summary.getOutputText().isBlank()) {
            result.getOutput().add(new com.grafana.agento11y.sdk.Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(List.of(MessagePart.text(summary.getOutputText()))));
        }
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "gemini.models.generate_content.request", request));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "gemini.models.generate_content.tools", requestMapping.tools));
            }
            result.getArtifacts().add(toArtifact(
                    ArtifactKind.PROVIDER_EVENT,
                    "gemini.models.generate_content.stream_events",
                    summary.getChunks()));
        }

        return result;
    }

    private static GenerationStart startFromRequest(GenerateContentParameters request, GeminiOptions options) {
        RequestMapping mapped = mapRequest(toMap(request));
        return new GenerationStart()
                .setConversationId(options.getConversationId())
                .setAgentName(options.getAgentName())
                .setAgentVersion(options.getAgentVersion())
                .setModel(new ModelRef().setProvider("gemini").setName(mapped.model))
                .setSystemPrompt(mapped.systemPrompt)
                .setTools(mapped.tools)
                .setMaxTokens(mapped.maxTokens)
                .setTemperature(mapped.temperature)
                .setTopP(mapped.topP)
                .setToolChoice(mapped.toolChoice)
                .setThinkingEnabled(mapped.thinkingEnabled)
                .setMetadata(metadataWithThinking(options.getMetadata(), mapped.thinkingBudget, mapped.thinkingLevel))
                .setTags(new LinkedHashMap<>(options.getTags()));
    }

    private static EmbeddingStart embeddingStartFromRequest(String model, EmbedContentConfig config, GeminiOptions options) {
        return new EmbeddingStart()
                .setAgentName(options.getAgentName())
                .setAgentVersion(options.getAgentVersion())
                .setModel(new ModelRef().setProvider("gemini").setName(model == null ? "" : model))
                .setDimensions(embeddingRequestedDimensions(config))
                .setMetadata(new LinkedHashMap<>(options.getMetadata()))
                .setTags(new LinkedHashMap<>(options.getTags()));
    }

    private static Long embeddingRequestedDimensions(EmbedContentConfig config) {
        if (config == null || config.outputDimensionality().isEmpty()) {
            return null;
        }
        Integer dimensions = config.outputDimensionality().orElse(null);
        if (dimensions == null || dimensions <= 0) {
            return null;
        }
        return dimensions.longValue();
    }

    private static int embeddingInputCount(List<String> contents) {
        if (contents == null) {
            return 0;
        }
        return contents.size();
    }

    private static List<String> embeddingInputTexts(List<String> contents) {
        if (contents == null || contents.isEmpty()) {
            return List.of();
        }
        List<String> out = new ArrayList<>(contents.size());
        for (String content : contents) {
            if (content == null || content.isBlank()) {
                continue;
            }
            out.add(content);
        }
        return out;
    }

    private static RequestContext buildRequest(String model, List<Content> contents, GenerateContentConfig config) {
        String resolvedModel = model == null ? "" : model;
        List<Content> resolvedContents = contents == null ? List.of() : List.copyOf(contents);

        GenerateContentParameters.Builder builder = GenerateContentParameters.builder()
                .model(resolvedModel)
                .contents(resolvedContents);
        if (config != null) {
            builder.config(config);
        }

        return new RequestContext(resolvedModel, resolvedContents, config, builder.build());
    }

    private static RequestMapping mapRequest(Map<String, Object> payload) {
        String model = asString(first(payload, "model"));
        List<Map<String, Object>> contents = asMapList(first(payload, "contents"));
        Map<String, Object> config = asMap(first(payload, "config"));

        Long maxTokens = asLong(first(config, "maxOutputTokens", "max_output_tokens"));
        Double temperature = asDouble(first(config, "temperature"));
        Double topP = asDouble(first(config, "topP", "top_p"));

        Object functionCallingConfig = first(
                asMap(first(config, "toolConfig", "tool_config")),
                "functionCallingConfig",
                "function_calling_config");
        String toolChoice = canonicalToolChoice(first(asMap(functionCallingConfig), "mode"));

        Object thinkingConfig = first(config, "thinkingConfig", "thinking_config");
        Boolean thinkingEnabled = resolveThinkingEnabled(thinkingConfig);
        Long thinkingBudget = resolveThinkingBudget(thinkingConfig);
        String thinkingLevel = resolveThinkingLevel(thinkingConfig);

        List<com.grafana.agento11y.sdk.Message> input = mapContents(contents);
        List<ToolDefinition> tools = mapTools(asMapList(first(config, "tools")));

        String systemPrompt = contentToText(first(config, "systemInstruction", "system_instruction"));

        return new RequestMapping(
                model,
                systemPrompt,
                maxTokens,
                temperature,
                topP,
                toolChoice,
                thinkingEnabled,
                thinkingBudget,
                thinkingLevel,
                input,
                tools);
    }

    private static List<com.grafana.agento11y.sdk.Message> mapContents(List<Map<String, Object>> contents) {
        List<com.grafana.agento11y.sdk.Message> out = new ArrayList<>();
        for (Map<String, Object> content : contents) {
            String role = asString(first(content, "role"));
            List<MessagePart> parts = mapParts(asMapList(first(content, "parts")));
            if (parts.isEmpty()) {
                String fallbackText = asString(first(content, "text"));
                if (!fallbackText.isBlank()) {
                    parts = List.of(MessagePart.text(fallbackText));
                }
            }
            if (parts.isEmpty()) {
                continue;
            }
            out.add(new com.grafana.agento11y.sdk.Message()
                    .setRole(normalizeRole(role))
                    .setParts(parts));
        }
        return out;
    }

    private static List<MessagePart> mapParts(List<Map<String, Object>> partsPayload) {
        List<MessagePart> parts = new ArrayList<>();
        for (Map<String, Object> partPayload : partsPayload) {
            String text = asString(first(partPayload, "text"));
            if (!text.isBlank()) {
                boolean thought = asBoolean(first(partPayload, "thought"));
                parts.add(thought ? MessagePart.thinking(text) : MessagePart.text(text));
                continue;
            }

            Map<String, Object> functionCall = asMap(first(partPayload, "functionCall", "function_call"));
            if (!functionCall.isEmpty()) {
                ToolCall call = new ToolCall()
                        .setId(asString(first(functionCall, "id")))
                        .setName(asString(first(functionCall, "name")))
                        .setInputJson(jsonBytes(first(functionCall, "args", "arguments")));
                MessagePart part = MessagePart.toolCall(call);
                part.setMetadata(new PartMetadata().setProviderType("function_call"));
                parts.add(part);
                continue;
            }

            Map<String, Object> functionResponse = asMap(first(partPayload, "functionResponse", "function_response"));
            if (!functionResponse.isEmpty()) {
                ToolResultPart result = new ToolResultPart()
                        .setName(asString(first(functionResponse, "name")))
                        .setContent(contentToText(first(functionResponse, "response")))
                        .setContentJson(jsonBytes(first(functionResponse, "response")));
                MessagePart part = MessagePart.toolResult(result);
                part.setMetadata(new PartMetadata().setProviderType("function_response"));
                parts.add(part);
            }
        }
        return parts;
    }

    private static List<com.grafana.agento11y.sdk.Message> mapOutput(Map<String, Object> responsePayload) {
        List<com.grafana.agento11y.sdk.Message> out = new ArrayList<>();
        for (Map<String, Object> candidate : asMapList(first(responsePayload, "candidates"))) {
            Map<String, Object> content = asMap(first(candidate, "content"));
            List<MessagePart> parts = mapParts(asMapList(first(content, "parts")));
            if (!parts.isEmpty()) {
                out.add(new com.grafana.agento11y.sdk.Message()
                        .setRole(MessageRole.ASSISTANT)
                        .setParts(parts));
            }
        }

        if (out.isEmpty()) {
            String text = extractChunkTextFromMap(responsePayload);
            if (!text.isBlank()) {
                out.add(new com.grafana.agento11y.sdk.Message()
                        .setRole(MessageRole.ASSISTANT)
                        .setParts(List.of(MessagePart.text(text))));
            }
        }

        return out;
    }

    private static List<ToolDefinition> mapTools(List<Map<String, Object>> toolsPayload) {
        List<ToolDefinition> out = new ArrayList<>();
        for (Map<String, Object> toolPayload : toolsPayload) {
            for (Map<String, Object> declaration : asMapList(first(toolPayload, "functionDeclarations", "function_declarations"))) {
                ToolDefinition definition = new ToolDefinition()
                        .setType("function")
                        .setName(asString(first(declaration, "name")))
                        .setDescription(asString(first(declaration, "description")));
                Object schema = first(declaration, "parameters", "parametersJsonSchema", "parameters_json_schema");
                if (schema != null) {
                    definition.setInputSchemaJson(jsonBytes(schema));
                }
                if (!definition.getName().isBlank()) {
                    out.add(definition);
                }
            }
        }
        return out;
    }

    private static TokenUsage mapUsage(Map<String, Object> usagePayload) {
        long input = asLong(first(usagePayload, "promptTokenCount", "prompt_token_count"));
        long output = asLong(first(usagePayload, "candidatesTokenCount", "candidates_token_count"));
        long total = asLong(first(usagePayload, "totalTokenCount", "total_token_count"));
        long cacheRead = asLong(first(usagePayload, "cachedContentTokenCount", "cached_content_token_count"));
        long toolUsePrompt = asLong(first(usagePayload, "toolUsePromptTokenCount", "tool_use_prompt_token_count"));
        long reasoning = asLong(first(usagePayload, "thoughtsTokenCount", "thoughts_token_count"));
        long freshInput = freshInputTokens(input, cacheRead);
        if (total == 0) {
            total = freshInput + output + cacheRead + toolUsePrompt + reasoning;
        }

        return new TokenUsage()
                .setInputTokens(freshInput)
                .setOutputTokens(output)
                .setTotalTokens(total)
                .setCacheReadInputTokens(cacheRead)
                .setReasoningTokens(reasoning)
                .setInputIsDisjoint(true);
    }

    private static long freshInputTokens(long rawInputTokens, long cacheReadInputTokens) {
        return Math.max(rawInputTokens - cacheReadInputTokens, 0L);
    }

    private static String normalizeStopReason(Map<String, Object> responsePayload) {
        for (Map<String, Object> candidate : asMapList(first(responsePayload, "candidates"))) {
            String finishReason = asString(first(candidate, "finishReason", "finish_reason"));
            if (!finishReason.isBlank()) {
                return finishReason;
            }
        }
        return "";
    }

    private static String extractChunkText(GenerateContentResponse response) {
        return extractChunkTextFromMap(toMap(response));
    }

    private static String extractChunkTextFromMap(Map<String, Object> payload) {
        StringBuilder out = new StringBuilder();
        for (Map<String, Object> candidate : asMapList(first(payload, "candidates"))) {
            Map<String, Object> content = asMap(first(candidate, "content"));
            for (Map<String, Object> part : asMapList(first(content, "parts"))) {
                appendText(out, asString(first(part, "text")));
            }
        }
        return out.toString();
    }

    private static void appendText(StringBuilder builder, String value) {
        if (value != null && !value.isBlank()) {
            builder.append(value);
        }
    }

    private static GeminiOptions resolveOptions(GeminiOptions options) {
        return options == null ? new GeminiOptions() : options;
    }

    private static Artifact toArtifact(ArtifactKind kind, String name, Object payload) {
        return new Artifact()
                .setKind(kind)
                .setName(name)
                .setContentType("application/json")
                .setPayload(jsonBytes(payload));
    }

    private static byte[] jsonBytes(Object payload) {
        try {
            if (payload instanceof JsonSerializable serializable) {
                return serializable.toJson().getBytes(StandardCharsets.UTF_8);
            }
            return JSON.writeValueAsBytes(payload);
        } catch (Exception ignored) {
            return String.valueOf(payload).getBytes(StandardCharsets.UTF_8);
        }
    }

    private static Map<String, Object> toMap(Object value) {
        if (value == null) {
            return Map.of();
        }
        try {
            if (value instanceof JsonSerializable serializable) {
                return JSON.readValue(serializable.toJson(), MAP_TYPE);
            }
            return JSON.convertValue(value, MAP_TYPE);
        } catch (Exception ignored) {
            return Map.of();
        }
    }

    private static Object first(Map<String, Object> payload, String... keys) {
        for (String key : keys) {
            if (payload.containsKey(key)) {
                return payload.get(key);
            }
        }
        return null;
    }

    private static Map<String, Object> asMap(Object value) {
        if (value instanceof Map<?, ?> map) {
            Map<String, Object> out = new LinkedHashMap<>();
            for (Map.Entry<?, ?> entry : map.entrySet()) {
                out.put(String.valueOf(entry.getKey()), entry.getValue());
            }
            return out;
        }
        return Map.of();
    }

    private static List<Map<String, Object>> asMapList(Object value) {
        List<Map<String, Object>> out = new ArrayList<>();
        if (value instanceof List<?> list) {
            for (Object entry : list) {
                out.add(asMap(entry));
            }
        }
        return out;
    }

    private static String asString(Object value) {
        if (value == null) {
            return "";
        }
        String stringValue = String.valueOf(value).trim();
        return "null".equalsIgnoreCase(stringValue) ? "" : stringValue;
    }

    private static Long asLong(Object value) {
        if (value == null) {
            return 0L;
        }
        if (value instanceof Number number) {
            return number.longValue();
        }
        if (value instanceof String text) {
            try {
                return Long.parseLong(text.trim());
            } catch (NumberFormatException ignored) {
                return 0L;
            }
        }
        return 0L;
    }

    private static Double asDouble(Object value) {
        if (value == null) {
            return null;
        }
        if (value instanceof Number number) {
            return number.doubleValue();
        }
        if (value instanceof String text) {
            try {
                return Double.parseDouble(text.trim());
            } catch (NumberFormatException ignored) {
                return null;
            }
        }
        return null;
    }

    private static boolean asBoolean(Object value) {
        if (value instanceof Boolean bool) {
            return bool;
        }
        if (value instanceof String text) {
            return Boolean.parseBoolean(text.trim());
        }
        return false;
    }

    private static String contentToText(Object value) {
        if (value == null) {
            return "";
        }
        if (value instanceof String text) {
            return text;
        }
        if (value instanceof Map<?, ?> map) {
            Object nested = map.containsKey("text") ? map.get("text") : map.get("output");
            if (nested != null) {
                return asString(nested);
            }
        }
        try {
            return JSON.writeValueAsString(value);
        } catch (Exception ignored) {
            return asString(value);
        }
    }

    private static MessageRole normalizeRole(String role) {
        if (role == null) {
            return MessageRole.USER;
        }
        return switch (role.trim().toLowerCase()) {
            case "assistant", "model" -> MessageRole.ASSISTANT;
            case "tool" -> MessageRole.TOOL;
            default -> MessageRole.USER;
        };
    }

    private static String canonicalToolChoice(Object value) {
        if (value == null) {
            return null;
        }
        if (value instanceof String text) {
            String normalized = text.trim().toLowerCase();
            return normalized.isEmpty() ? null : normalized;
        }
        try {
            return CANONICAL_JSON.writeValueAsString(value);
        } catch (Exception ignored) {
            String fallback = String.valueOf(value).trim();
            return fallback.isEmpty() ? null : fallback;
        }
    }

    private static Boolean resolveThinkingEnabled(Object value) {
        Map<String, Object> payload = asMap(value);
        if (payload.isEmpty()) {
            return null;
        }
        if (payload.containsKey("includeThoughts")) {
            return asBoolean(payload.get("includeThoughts"));
        }
        if (payload.containsKey("include_thoughts")) {
            return asBoolean(payload.get("include_thoughts"));
        }
        return null;
    }

    private static Long resolveThinkingBudget(Object value) {
        Map<String, Object> payload = asMap(value);
        Long budget = asLong(first(payload, "thinkingBudget", "thinking_budget"));
        return budget == 0 ? null : budget;
    }

    private static String resolveThinkingLevel(Object value) {
        Map<String, Object> payload = asMap(value);
        Object rawLevel = first(payload, "thinkingLevel", "thinking_level", "thinkinglevel");
        if (rawLevel == null) {
            for (Map.Entry<String, Object> entry : payload.entrySet()) {
                String key = entry.getKey() == null ? "" : entry.getKey().toLowerCase();
                if (key.contains("thinking") && key.contains("level")) {
                    rawLevel = entry.getValue();
                    break;
                }
            }
        }
        if (rawLevel instanceof Map<?, ?> nested) {
            Map<String, Object> nestedMap = asMap(nested);
            rawLevel = first(nestedMap, "value", "name", "level");
        }

        String normalized = asString(rawLevel).toLowerCase();
        if (normalized.startsWith("thinking_level_")) {
            normalized = normalized.substring("thinking_level_".length());
        } else if (normalized.startsWith("thinkinglevel_")) {
            normalized = normalized.substring("thinkinglevel_".length());
        } else if (normalized.startsWith("thinkinglevel")) {
            normalized = normalized.substring("thinkinglevel".length());
        }
        return switch (normalized) {
            case "", "unspecified" -> null;
            case "low" -> "low";
            case "medium" -> "medium";
            case "high" -> "high";
            case "minimal" -> "minimal";
            default -> normalized;
        };
    }

    private static LinkedHashMap<String, Object> metadataWithThinking(
            Map<String, Object> metadata,
            Long thinkingBudget,
            String thinkingLevel) {
        LinkedHashMap<String, Object> out = new LinkedHashMap<>();
        if (metadata != null) {
            out.putAll(metadata);
        }
        if (thinkingBudget != null) {
            out.put(THINKING_BUDGET_METADATA_KEY, thinkingBudget);
        }
        if (thinkingLevel != null && !thinkingLevel.isBlank()) {
            out.put(THINKING_LEVEL_METADATA_KEY, thinkingLevel);
        }
        return out;
    }

    private static LinkedHashMap<String, Object> geminiUsageMetadata(Map<String, Object> usagePayload) {
        LinkedHashMap<String, Object> out = new LinkedHashMap<>();
        long toolUsePrompt = asLong(first(usagePayload, "toolUsePromptTokenCount", "tool_use_prompt_token_count"));
        if (toolUsePrompt > 0) {
            out.put(USAGE_TOOL_USE_PROMPT_TOKENS_METADATA_KEY, toolUsePrompt);
        }
        return out;
    }

    private static String firstNonBlank(String... values) {
        for (String value : values) {
            if (value != null && !value.isBlank()) {
                return value;
            }
        }
        return "";
    }

    private record RequestContext(
            String model,
            List<Content> contents,
            GenerateContentConfig config,
            GenerateContentParameters request) {
    }

    private record RequestMapping(
            String model,
            String systemPrompt,
            Long maxTokens,
            Double temperature,
            Double topP,
            String toolChoice,
            Boolean thinkingEnabled,
            Long thinkingBudget,
            String thinkingLevel,
            List<com.grafana.agento11y.sdk.Message> input,
            List<ToolDefinition> tools) {
    }
}
