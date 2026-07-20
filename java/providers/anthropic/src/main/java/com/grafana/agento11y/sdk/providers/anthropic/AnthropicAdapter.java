package com.grafana.agento11y.sdk.providers.anthropic;

import com.anthropic.core.ObjectMappers;
import com.anthropic.core.http.StreamResponse;
import com.anthropic.helpers.MessageAccumulator;
import com.anthropic.models.messages.Message;
import com.anthropic.models.messages.MessageCreateParams;
import com.anthropic.models.messages.RawMessageStreamEvent;
import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.grafana.agento11y.sdk.Artifact;
import com.grafana.agento11y.sdk.ArtifactKind;
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
import com.grafana.agento11y.sdk.ThrowingFunction;
import java.nio.charset.StandardCharsets;
import java.time.Instant;
import java.util.ArrayList;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;

/** Anthropic wrappers and mappers using official Anthropic Java SDK request/response types. */
public final class AnthropicAdapter {
    private static final String THINKING_BUDGET_METADATA_KEY = "agento11y.gen_ai.request.thinking.budget_tokens";
    private static final String USAGE_SERVER_TOOL_USE_WEB_SEARCH_METADATA_KEY =
            "agento11y.gen_ai.usage.server_tool_use.web_search_requests";
    private static final String USAGE_SERVER_TOOL_USE_WEB_FETCH_METADATA_KEY =
            "agento11y.gen_ai.usage.server_tool_use.web_fetch_requests";
    private static final String USAGE_SERVER_TOOL_USE_TOTAL_METADATA_KEY =
            "agento11y.gen_ai.usage.server_tool_use.total_requests";
    private static final ObjectMapper JSON = ObjectMappers.jsonMapper();
    private static final ObjectMapper CANONICAL_JSON = new ObjectMapper()
            .configure(MapperFeature.SORT_PROPERTIES_ALPHABETICALLY, true)
            .configure(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS, true);
    private static final TypeReference<Map<String, Object>> MAP_TYPE = new TypeReference<>() {
    };

    private AnthropicAdapter() {
    }

    public static Message completion(
            Agento11yClient client,
            MessageCreateParams request,
            ThrowingFunction<MessageCreateParams, Message> providerCall,
            AnthropicOptions options) throws Exception {
        AnthropicOptions resolved = resolveOptions(options);
        return client.withGeneration(startFromRequest(request, resolved), recorder -> {
            Message response = providerCall.apply(request);
            recorder.setResult(fromRequestResponse(request, response, resolved));
            return response;
        });
    }

    public static AnthropicStreamSummary completionStream(
            Agento11yClient client,
            MessageCreateParams request,
            ThrowingFunction<MessageCreateParams, StreamResponse<RawMessageStreamEvent>> providerCall,
            AnthropicOptions options) throws Exception {
        AnthropicOptions resolved = resolveOptions(options);
        return client.withStreamingGeneration(startFromRequest(request, resolved), recorder -> {
            AnthropicStreamSummary summary = new AnthropicStreamSummary();
            MessageAccumulator accumulator = MessageAccumulator.create();
            StringBuilder outputText = new StringBuilder();

            try (StreamResponse<RawMessageStreamEvent> stream = providerCall.apply(request)) {
                stream.stream().forEach(event -> {
                    if (summary.getFirstChunkAt() == null) {
                        Instant firstChunkAt = Instant.now();
                        summary.setFirstChunkAt(firstChunkAt);
                        recorder.setFirstTokenAt(firstChunkAt);
                    }
                    summary.getEvents().add(event);
                    appendText(outputText, extractDeltaText(event));
                    try {
                        accumulator.accumulate(event);
                    } catch (Exception ignored) {
                        // Keep collecting events; mapper can still fall back to chunk-only output.
                    }
                });
            }

            if (outputText.length() > 0) {
                summary.setOutputText(outputText.toString());
            }

            try {
                summary.setFinalResponse(accumulator.message());
            } catch (Exception ignored) {
                summary.setFinalResponse(null);
            }

            if (summary.getFinalResponse() != null && summary.getOutputText().isBlank()) {
                summary.setOutputText(extractOutputText(summary.getFinalResponse()));
            }

            recorder.setResult(fromStream(request, summary, resolved));
            return summary;
        });
    }

    public static GenerationResult fromRequestResponse(
            MessageCreateParams request,
            Message response,
            AnthropicOptions options) {
        AnthropicOptions resolved = resolveOptions(options);
        RequestMapping requestMapping = mapRequest(request);
        Map<String, Object> responsePayload = toMap(response);
        TokenUsage usage = mapUsage(response);
        LinkedHashMap<String, Object> metadata = metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget);
        metadata.putAll(anthropicUsageMetadata(responsePayload));

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("anthropic").setName(requestMapping.model))
                .setResponseId(response.id())
                .setResponseModel(firstNonBlank(response.model().asString(), requestMapping.model))
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setStopReason(response.stopReason().map(stopReason -> stopReason.asString()).orElse(""))
                .setUsage(usage)
                .setMetadata(metadata)
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(mapResponseOutput(responsePayload));
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "anthropic.messages.request", request));
            result.getArtifacts().add(toArtifact(ArtifactKind.RESPONSE, "anthropic.messages.response", response));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "anthropic.messages.tools", requestMapping.tools));
            }
        }

        return result;
    }

    public static GenerationResult fromStream(
            MessageCreateParams request,
            AnthropicStreamSummary summary,
            AnthropicOptions options) {
        AnthropicOptions resolved = resolveOptions(options);
        if (summary.getFinalResponse() != null) {
            GenerationResult mapped = fromRequestResponse(request, summary.getFinalResponse(), resolved);
            if (resolved.isRawArtifacts()) {
                mapped.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "anthropic.messages.stream_events", summary.getEvents()));
            }
            return mapped;
        }

        RequestMapping requestMapping = mapRequest(request);
        LinkedHashMap<String, Object> metadata = metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget);
        metadata.putAll(anthropicStreamUsageMetadata(summary));
        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("anthropic").setName(requestMapping.model))
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
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "anthropic.messages.request", request));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "anthropic.messages.tools", requestMapping.tools));
            }
            result.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "anthropic.messages.stream_events", summary.getEvents()));
        }

        return result;
    }

    private static GenerationStart startFromRequest(MessageCreateParams request, AnthropicOptions options) {
        RequestMapping mapped = mapRequest(request);
        return new GenerationStart()
                .setConversationId(options.getConversationId())
                .setAgentName(options.getAgentName())
                .setAgentVersion(options.getAgentVersion())
                .setModel(new ModelRef().setProvider("anthropic").setName(mapped.model))
                .setSystemPrompt(mapped.systemPrompt)
                .setTools(mapped.tools)
                .setMaxTokens(mapped.maxTokens)
                .setTemperature(mapped.temperature)
                .setTopP(mapped.topP)
                .setToolChoice(mapped.toolChoice)
                .setThinkingEnabled(mapped.thinkingEnabled)
                .setMetadata(metadataWithThinkingBudget(options.getMetadata(), mapped.thinkingBudget))
                .setTags(new LinkedHashMap<>(options.getTags()));
    }

    private static RequestMapping mapRequest(MessageCreateParams request) {
        Map<String, Object> requestPayload = toMap(request);
        String model = request.model().asString();
        String systemPrompt = asSystemPrompt(first(requestPayload, "system"));
        Long maxTokens = request.maxTokens();
        Double temperature = request.temperature().orElse(null);
        Double topP = request.topP().orElse(null);
        Object toolChoiceRaw = request.toolChoice().orElse(null);
        Object thinkingRaw = request.thinking().orElse(null);

        List<Map<String, Object>> messagePayload = new ArrayList<>();
        for (Object message : request.messages()) {
            messagePayload.add(toMap(message));
        }

        List<Map<String, Object>> toolPayload = new ArrayList<>();
        for (Object tool : request.tools().orElse(List.of())) {
            toolPayload.add(toMap(tool));
        }

        List<com.grafana.agento11y.sdk.Message> input = mapInputMessages(messagePayload);
        List<ToolDefinition> tools = mapTools(toolPayload);

        return new RequestMapping(
                model,
                systemPrompt,
                maxTokens,
                temperature,
                topP,
                canonicalToolChoice(toolChoiceRaw),
                resolveThinkingEnabled(thinkingRaw),
                resolveThinkingBudget(thinkingRaw),
                input,
                tools);
    }

    private static List<com.grafana.agento11y.sdk.Message> mapInputMessages(List<Map<String, Object>> messages) {
        List<com.grafana.agento11y.sdk.Message> out = new ArrayList<>();
        for (Map<String, Object> payload : messages) {
            String role = asString(first(payload, "role"));
            if ("system".equalsIgnoreCase(role)) {
                continue;
            }

            List<MessagePart> parts = mapContentParts(first(payload, "content"));
            if (parts.isEmpty()) {
                continue;
            }

            out.add(new com.grafana.agento11y.sdk.Message()
                    .setRole(normalizeRole(role))
                    .setName(asString(first(payload, "name")))
                    .setParts(parts));
        }
        return out;
    }

    private static List<com.grafana.agento11y.sdk.Message> mapResponseOutput(Map<String, Object> responsePayload) {
        List<com.grafana.agento11y.sdk.Message> out = new ArrayList<>();

        List<Map<String, Object>> blocks = asMapList(first(responsePayload, "content"));
        List<MessagePart> assistantParts = new ArrayList<>();
        for (Map<String, Object> block : blocks) {
            String type = asString(first(block, "type"));
            if ("text".equalsIgnoreCase(type) || "thinking".equalsIgnoreCase(type) || "redacted_thinking".equalsIgnoreCase(type)) {
                String text = extractThinkingText(block);
                if (!text.isBlank()) {
                    if ("thinking".equalsIgnoreCase(type)) {
                        MessagePart part = MessagePart.thinking(text);
                        part.setMetadata(new PartMetadata().setProviderType("thinking"));
                        assistantParts.add(part);
                    } else if ("redacted_thinking".equalsIgnoreCase(type)) {
                        MessagePart part = MessagePart.thinking(text);
                        part.setMetadata(new PartMetadata().setProviderType("redacted_thinking"));
                        assistantParts.add(part);
                    } else {
                        MessagePart part = MessagePart.text(text);
                        part.setMetadata(new PartMetadata().setProviderType("text"));
                        assistantParts.add(part);
                    }
                }
                continue;
            }
            if ("tool_use".equalsIgnoreCase(type)
                    || "server_tool_use".equalsIgnoreCase(type)
                    || "mcp_tool_use".equalsIgnoreCase(type)) {
                ToolCall toolCall = new ToolCall()
                        .setId(asString(first(block, "id")))
                        .setName(asString(first(block, "name")))
                        .setInputJson(jsonBytes(first(block, "input")));
                MessagePart part = MessagePart.toolCall(toolCall);
                part.setMetadata(new PartMetadata().setProviderType(type.toLowerCase()));
                assistantParts.add(part);
            }
        }

        if (assistantParts.isEmpty()) {
            String outputText = extractOutputTextFromMap(responsePayload);
            if (!outputText.isBlank()) {
                assistantParts.add(MessagePart.text(outputText));
            }
        }

        if (!assistantParts.isEmpty()) {
            out.add(new com.grafana.agento11y.sdk.Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(assistantParts));
        }

        return out;
    }

    private static List<MessagePart> mapContentParts(Object content) {
        List<MessagePart> parts = new ArrayList<>();

        if (content instanceof String text) {
            if (!text.isBlank()) {
                parts.add(MessagePart.text(text));
            }
            return parts;
        }

        for (Map<String, Object> block : asMapList(content)) {
            String type = asString(first(block, "type"));
            if ("text".equalsIgnoreCase(type) || "thinking".equalsIgnoreCase(type) || "redacted_thinking".equalsIgnoreCase(type)) {
                String text = extractThinkingText(block);
                if (!text.isBlank()) {
                    if ("thinking".equalsIgnoreCase(type)) {
                        MessagePart part = MessagePart.thinking(text);
                        part.setMetadata(new PartMetadata().setProviderType("thinking"));
                        parts.add(part);
                    } else if ("redacted_thinking".equalsIgnoreCase(type)) {
                        MessagePart part = MessagePart.thinking(text);
                        part.setMetadata(new PartMetadata().setProviderType("redacted_thinking"));
                        parts.add(part);
                    } else {
                        MessagePart part = MessagePart.text(text);
                        part.setMetadata(new PartMetadata().setProviderType("text"));
                        parts.add(part);
                    }
                }
                continue;
            }

            if ("tool_use".equalsIgnoreCase(type)
                    || "server_tool_use".equalsIgnoreCase(type)
                    || "mcp_tool_use".equalsIgnoreCase(type)) {
                ToolCall call = new ToolCall()
                        .setId(asString(first(block, "id")))
                        .setName(asString(first(block, "name")))
                        .setInputJson(jsonBytes(first(block, "input")));
                MessagePart part = MessagePart.toolCall(call);
                part.setMetadata(new PartMetadata().setProviderType(type.toLowerCase()));
                parts.add(part);
                continue;
            }

            if ("tool_result".equalsIgnoreCase(type)) {
                ToolResultPart result = new ToolResultPart()
                        .setToolCallId(asString(first(block, "tool_use_id", "toolUseId")))
                        .setName(asString(first(block, "name")))
                        .setContent(contentToText(first(block, "content")))
                        .setContentJson(jsonBytes(first(block, "content")))
                        .setError(asBoolean(first(block, "is_error", "isError")));
                MessagePart part = MessagePart.toolResult(result);
                part.setMetadata(new PartMetadata().setProviderType("tool_result"));
                parts.add(part);
            }
        }

        if (parts.isEmpty() && content != null) {
            String fallback = contentToText(content);
            if (!fallback.isBlank()) {
                parts.add(MessagePart.text(fallback));
            }
        }

        return parts;
    }

    private static List<ToolDefinition> mapTools(List<Map<String, Object>> toolsPayload) {
        List<ToolDefinition> out = new ArrayList<>();
        for (Map<String, Object> tool : toolsPayload) {
            ToolDefinition definition = new ToolDefinition()
                    .setType(asString(first(tool, "type")))
                    .setName(asString(first(tool, "name")))
                    .setDescription(asString(first(tool, "description")));

            Object schema = first(tool, "input_schema", "inputSchema");
            if (schema != null) {
                definition.setInputSchemaJson(jsonBytes(schema));
            }

            if (!definition.getName().isBlank()) {
                out.add(definition);
            }
        }
        return out;
    }

    private static TokenUsage mapUsage(Message response) {
        var usage = response.usage();
        long input = usage.inputTokens();
        long output = usage.outputTokens();
        long total = input + output;
        long cacheRead = usage.cacheReadInputTokens().orElse(0L);
        long cacheWrite = usage.cacheCreationInputTokens().orElse(0L);

        return new TokenUsage()
                .setInputTokens(input)
                .setOutputTokens(output)
                .setTotalTokens(total == 0 ? input + output : total)
                .setCacheReadInputTokens(cacheRead)
                .setCacheWriteInputTokens(cacheWrite);
    }

    private static String extractDeltaText(RawMessageStreamEvent event) {
        Map<String, Object> payload = toMap(event);
        Map<String, Object> blockDelta = asMap(first(payload, "content_block_delta", "contentBlockDelta"));
        Map<String, Object> delta = asMap(first(blockDelta, "delta"));
        String text = asString(first(delta, "text"));
        if (!text.isBlank()) {
            return text;
        }

        Map<String, Object> nested = asMap(first(payload, "delta"));
        return asString(first(nested, "text"));
    }

    private static String extractOutputText(Message message) {
        return extractOutputTextFromMap(toMap(message));
    }

    private static String extractOutputTextFromMap(Map<String, Object> responsePayload) {
        StringBuilder text = new StringBuilder();
        for (Map<String, Object> block : asMapList(first(responsePayload, "content"))) {
            appendText(text, extractThinkingText(block));
        }
        return text.toString();
    }

    private static String extractThinkingText(Map<String, Object> block) {
        String thinking = asString(first(block, "thinking", "text", "data"));
        return thinking;
    }

    private static void appendText(StringBuilder builder, String value) {
        if (value != null && !value.isBlank()) {
            builder.append(value);
        }
    }

    private static AnthropicOptions resolveOptions(AnthropicOptions options) {
        return options == null ? new AnthropicOptions() : options;
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
            return JSON.convertValue(value, MAP_TYPE);
        } catch (IllegalArgumentException ignored) {
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

    private static String asSystemPrompt(Object value) {
        if (value == null) {
            return "";
        }
        if (value instanceof String text) {
            return text;
        }
        if (value instanceof List<?> list) {
            StringBuilder out = new StringBuilder();
            for (Object block : list) {
                appendText(out, asString(first(asMap(block), "text")));
            }
            if (out.length() > 0) {
                return out.toString();
            }
        }
        return asString(value);
    }

    private static String contentToText(Object value) {
        if (value == null) {
            return "";
        }
        if (value instanceof String text) {
            return text;
        }
        if (value instanceof List<?> list) {
            StringBuilder builder = new StringBuilder();
            for (Object entry : list) {
                Map<String, Object> block = asMap(entry);
                String text = asString(first(block, "text"));
                if (!text.isBlank()) {
                    appendText(builder, text);
                }
            }
            if (builder.length() > 0) {
                return builder.toString();
            }
        }
        try {
            return JSON.writeValueAsString(value);
        } catch (Exception ignored) {
            return asString(value);
        }
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

    private static MessageRole normalizeRole(String role) {
        if (role == null) {
            return MessageRole.USER;
        }
        return switch (role.trim().toLowerCase()) {
            case "assistant" -> MessageRole.ASSISTANT;
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
        if (value == null) {
            return null;
        }
        String className = value.getClass().getSimpleName().toLowerCase();
        if (className.contains("disabled")) {
            return Boolean.FALSE;
        }
        if (className.contains("enabled")) {
            return Boolean.TRUE;
        }
        String rendered = value.toString().toLowerCase();
        if (rendered.contains("disabled")) {
            return Boolean.FALSE;
        }
        if (rendered.contains("enabled") || rendered.contains("adaptive")) {
            return Boolean.TRUE;
        }
        if (value instanceof String text) {
            String normalized = text.trim().toLowerCase();
            return switch (normalized) {
                case "enabled" -> Boolean.TRUE;
                case "disabled" -> Boolean.FALSE;
                default -> null;
            };
        }
        Map<String, Object> payload = asMap(value);
        if (payload.isEmpty()) {
            payload = toMap(value);
        }
        String type = asString(first(payload, "type"));
        if (type.isBlank()) {
            type = asString(first(payload, "_type"));
        }
        if (!type.isBlank()) {
            return switch (type.toLowerCase()) {
                case "enabled", "adaptive" -> Boolean.TRUE;
                case "disabled" -> Boolean.FALSE;
                default -> null;
            };
        }
        return null;
    }

    private static Long resolveThinkingBudget(Object value) {
        if (value == null) {
            return null;
        }
        Map<String, Object> payload = asMap(value);
        if (payload.isEmpty()) {
            payload = toMap(value);
        }
        Long budget = asLong(first(payload, "budget_tokens", "budgetTokens", "thinking_budget", "thinkingBudget", "_budgetTokens"));
        if (budget == 0) {
            int marker = renderedBudget(value.toString());
            if (marker > 0) {
                budget = (long) marker;
            }
        }
        return budget == 0 ? null : budget;
    }

    private static int renderedBudget(String rendered) {
        int idx = rendered.toLowerCase().indexOf("budget");
        if (idx < 0) {
            return 0;
        }
        StringBuilder digits = new StringBuilder();
        for (int i = idx; i < rendered.length(); i++) {
            char ch = rendered.charAt(i);
            if (Character.isDigit(ch)) {
                digits.append(ch);
            } else if (digits.length() > 0) {
                break;
            }
        }
        if (digits.length() == 0) {
            return 0;
        }
        try {
            return Integer.parseInt(digits.toString());
        } catch (NumberFormatException ignored) {
            return 0;
        }
    }

    private static LinkedHashMap<String, Object> metadataWithThinkingBudget(Map<String, Object> metadata, Long thinkingBudget) {
        LinkedHashMap<String, Object> out = new LinkedHashMap<>();
        if (metadata != null) {
            out.putAll(metadata);
        }
        if (thinkingBudget != null) {
            out.put(THINKING_BUDGET_METADATA_KEY, thinkingBudget);
        }
        return out;
    }

    private static LinkedHashMap<String, Object> anthropicUsageMetadata(Map<String, Object> responsePayload) {
        LinkedHashMap<String, Object> out = new LinkedHashMap<>();
        Map<String, Object> usage = asMap(first(responsePayload, "usage"));
        Map<String, Object> serverToolUse = asMap(first(usage, "server_tool_use", "serverToolUse"));
        long webSearchRequests = asLong(first(serverToolUse, "web_search_requests", "webSearchRequests"));
        long webFetchRequests = asLong(first(serverToolUse, "web_fetch_requests", "webFetchRequests"));
        long totalRequests = webSearchRequests + webFetchRequests;
        if (totalRequests <= 0) {
            return out;
        }
        if (webSearchRequests > 0) {
            out.put(USAGE_SERVER_TOOL_USE_WEB_SEARCH_METADATA_KEY, webSearchRequests);
        }
        if (webFetchRequests > 0) {
            out.put(USAGE_SERVER_TOOL_USE_WEB_FETCH_METADATA_KEY, webFetchRequests);
        }
        out.put(USAGE_SERVER_TOOL_USE_TOTAL_METADATA_KEY, totalRequests);
        return out;
    }

    private static LinkedHashMap<String, Object> anthropicStreamUsageMetadata(AnthropicStreamSummary summary) {
        for (int i = summary.getEvents().size() - 1; i >= 0; i--) {
            Map<String, Object> payload = toMap(summary.getEvents().get(i));
            if (!"message_delta".equalsIgnoreCase(asString(first(payload, "type")))) {
                continue;
            }
            LinkedHashMap<String, Object> usageMetadata = anthropicUsageMetadata(payload);
            if (!usageMetadata.isEmpty()) {
                return usageMetadata;
            }
        }
        return new LinkedHashMap<>();
    }

    private static String firstNonBlank(String... values) {
        for (String value : values) {
            if (value != null && !value.isBlank()) {
                return value;
            }
        }
        return "";
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
            List<com.grafana.agento11y.sdk.Message> input,
            List<ToolDefinition> tools) {
    }
}
