package com.grafana.agento11y.sdk.providers.openai;

import com.fasterxml.jackson.core.type.TypeReference;
import com.fasterxml.jackson.databind.MapperFeature;
import com.fasterxml.jackson.databind.ObjectMapper;
import com.fasterxml.jackson.databind.SerializationFeature;
import com.grafana.agento11y.sdk.Artifact;
import com.grafana.agento11y.sdk.ArtifactKind;
import com.grafana.agento11y.sdk.EmbeddingResult;
import com.grafana.agento11y.sdk.EmbeddingStart;
import com.grafana.agento11y.sdk.GenerationResult;
import com.grafana.agento11y.sdk.GenerationStart;
import com.grafana.agento11y.sdk.Message;
import com.grafana.agento11y.sdk.MessagePart;
import com.grafana.agento11y.sdk.MessageRole;
import com.grafana.agento11y.sdk.ModelRef;
import com.grafana.agento11y.sdk.PartMetadata;
import com.grafana.agento11y.sdk.TokenUsage;
import com.grafana.agento11y.sdk.ToolCall;
import com.grafana.agento11y.sdk.ToolDefinition;
import com.grafana.agento11y.sdk.ToolResultPart;
import com.openai.core.ObjectMappers;
import com.openai.models.chat.completions.ChatCompletion;
import com.openai.models.chat.completions.ChatCompletionChunk;
import com.openai.models.chat.completions.ChatCompletionCreateParams;
import com.openai.models.embeddings.CreateEmbeddingResponse;
import com.openai.models.embeddings.EmbeddingCreateParams;
import com.openai.models.responses.Response;
import com.openai.models.responses.ResponseCreateParams;
import com.openai.models.responses.ResponseStreamEvent;
import java.nio.charset.StandardCharsets;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.TreeMap;

final class OpenAiGenerationMapper {
    private static final String THINKING_BUDGET_METADATA_KEY = "agento11y.gen_ai.request.thinking.budget_tokens";
    private static final ObjectMapper JSON = ObjectMappers.jsonMapper();
    private static final ObjectMapper CANONICAL_JSON = new ObjectMapper()
            .configure(MapperFeature.SORT_PROPERTIES_ALPHABETICALLY, true)
            .configure(SerializationFeature.ORDER_MAP_ENTRIES_BY_KEYS, true);

    private OpenAiGenerationMapper() {
    }

    static GenerationStart chatCompletionsStart(ChatCompletionCreateParams request, OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        ChatRequestMapping requestMapping = mapChatRequest(toMap(request));
        String modelName = firstNonBlank(requestMapping.model, "");

        return new GenerationStart()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(modelName))
                .setSystemPrompt(requestMapping.systemPrompt)
                .setTools(requestMapping.tools)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));
    }

    static GenerationResult chatCompletionsFromRequestResponse(
            ChatCompletionCreateParams request,
            ChatCompletion response,
            OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        Map<String, Object> requestPayload = toMap(request);
        Map<String, Object> responsePayload = toMap(response);

        ChatRequestMapping requestMapping = mapChatRequest(requestPayload);
        String responseId = asString(responsePayload.get("id"));
        String responseModel = firstNonBlank(asString(responsePayload.get("model")), requestMapping.model);

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(requestMapping.model))
                .setResponseId(responseId)
                .setResponseModel(responseModel)
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setUsage(mapChatUsage(asMap(responsePayload.get("usage"))))
                .setStopReason(normalizeChatStopReason(firstChoiceFinishReason(responsePayload)))
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(mapChatResponseOutput(responsePayload));
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "openai.chat.request", request));
            result.getArtifacts().add(toArtifact(ArtifactKind.RESPONSE, "openai.chat.response", response));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "openai.chat.tools", requestMapping.tools));
            }
        }

        return result;
    }

    static GenerationResult chatCompletionsFromStream(
            ChatCompletionCreateParams request,
            ChatCompletionsStreamSummary summary,
            OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        if (summary.getFinalResponse() != null) {
            GenerationResult mapped = chatCompletionsFromRequestResponse(request, summary.getFinalResponse(), resolved);
            if (resolved.isRawArtifacts() && !summary.getChunks().isEmpty()) {
                mapped.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "openai.chat.stream_events", summary.getChunks()));
            }
            return mapped;
        }

        Map<String, Object> requestPayload = toMap(request);
        ChatRequestMapping requestMapping = mapChatRequest(requestPayload);

        String responseId = "";
        String responseModel = requestMapping.model;
        String stopReason = "";
        TokenUsage usage = new TokenUsage();
        StringBuilder outputText = new StringBuilder();
        Map<Integer, ToolCallAccumulator> streamedToolCalls = new TreeMap<>();

        for (ChatCompletionChunk chunk : summary.getChunks()) {
            Map<String, Object> chunkMap = toMap(chunk);
            responseId = firstNonBlank(responseId, getString(chunkMap, "id"));
            responseModel = firstNonBlank(responseModel, getString(chunkMap, "model"));

            Map<String, Object> usagePayload = asMap(getFirst(chunkMap, "usage"));
            if (!usagePayload.isEmpty()) {
                usage = mapChatUsage(usagePayload);
            }

            for (Map<String, Object> choice : asMapList(getFirst(chunkMap, "choices"))) {
                String finishReason = getString(choice, "finish_reason", "finishReason");
                if (!finishReason.isBlank()) {
                    stopReason = normalizeChatStopReason(finishReason);
                }

                Map<String, Object> delta = asMap(getFirst(choice, "delta"));
                String content = getString(delta, "content");
                if (!content.isBlank()) {
                    outputText.append(content);
                }

                for (Map<String, Object> toolCall : asMapList(getFirst(delta, "tool_calls", "toolCalls"))) {
                    int index = asInt(getFirst(toolCall, "index"), streamedToolCalls.size());
                    ToolCallAccumulator accumulator = streamedToolCalls.computeIfAbsent(index, key -> new ToolCallAccumulator());
                    String toolCallId = getString(toolCall, "id");
                    if (!toolCallId.isBlank()) {
                        accumulator.id = toolCallId;
                    }
                    Map<String, Object> function = asMap(getFirst(toolCall, "function"));
                    String name = getString(function, "name");
                    if (!name.isBlank()) {
                        accumulator.name = name;
                    }
                    String arguments = getString(function, "arguments");
                    if (!arguments.isBlank()) {
                        accumulator.arguments.append(arguments);
                    }
                }
            }
        }

        List<Message> outputMessages = new ArrayList<>();
        if (!outputText.isEmpty()) {
            outputMessages.add(new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(List.of(MessagePart.text(outputText.toString()))));
        }
        streamedToolCalls.entrySet().stream()
                .sorted(Comparator.comparingInt(Map.Entry::getKey))
                .forEach(entry -> {
                    ToolCallAccumulator call = entry.getValue();
                    MessagePart part = MessagePart.toolCall(new ToolCall()
                            .setId(call.id)
                            .setName(call.name)
                            .setInputJson(jsonStringBytes(call.arguments.toString())));
                    part.setMetadata(new PartMetadata().setProviderType("tool_call"));
                    outputMessages.add(new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(part)));
                });

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(requestMapping.model))
                .setResponseId(responseId)
                .setResponseModel(responseModel)
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setUsage(usage)
                .setStopReason(stopReason)
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(outputMessages);
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "openai.chat.request", request));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "openai.chat.tools", requestMapping.tools));
            }
            if (!summary.getChunks().isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "openai.chat.stream_events", summary.getChunks()));
            }
        }

        return result;
    }

    static GenerationStart responsesStart(ResponseCreateParams request, OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        ResponsesRequestMapping requestMapping = mapResponsesRequest(toMap(request));

        return new GenerationStart()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(requestMapping.model))
                .setSystemPrompt(requestMapping.systemPrompt)
                .setTools(requestMapping.tools)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));
    }

    static EmbeddingStart embeddingsStart(EmbeddingCreateParams request, OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        EmbeddingStart start = new EmbeddingStart()
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef()
                        .setProvider("openai")
                        .setName(request == null || request.model() == null ? "" : request.model().asString()))
                .setMetadata(new LinkedHashMap<>(resolved.getMetadata()))
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        if (request != null) {
            request.dimensions().ifPresent(start::setDimensions);
            request.encodingFormat().ifPresent(value -> start.setEncodingFormat(value.asString()));
        }
        return start;
    }

    static EmbeddingResult embeddingsFromRequestResponse(
            EmbeddingCreateParams request,
            CreateEmbeddingResponse response) {
        EmbeddingResult result = new EmbeddingResult()
                .setInputCount(embeddingInputCount(request == null ? null : request.input()))
                .setInputTexts(embeddingInputTexts(request == null ? null : request.input()));

        if (response == null) {
            if (request != null) {
                request.dimensions().ifPresent(result::setDimensions);
            }
            return result;
        }

        if (response.usage() != null) {
            result.setInputTokens(response.usage().promptTokens());
        }
        result.setResponseModel(firstNonBlank(
                response.model(),
                request == null || request.model() == null ? "" : request.model().asString()));

        if (response.data() != null && !response.data().isEmpty() && response.data().get(0) != null) {
            List<Float> embedding = response.data().get(0).embedding();
            if (embedding != null && !embedding.isEmpty()) {
                result.setDimensions((long) embedding.size());
            }
        }
        if (result.getDimensions() == null && request != null) {
            request.dimensions().ifPresent(result::setDimensions);
        }
        return result;
    }

    static GenerationResult responsesFromRequestResponse(
            ResponseCreateParams request,
            Response response,
            OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        Map<String, Object> requestPayload = toMap(request);
        Map<String, Object> responsePayload = toMap(response);

        ResponsesRequestMapping requestMapping = mapResponsesRequest(requestPayload);
        String responseId = asString(responsePayload.get("id"));
        String responseModel = firstNonBlank(asString(responsePayload.get("model")), requestMapping.model);

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(requestMapping.model))
                .setResponseId(responseId)
                .setResponseModel(responseModel)
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setUsage(mapResponsesUsage(asMap(responsePayload.get("usage"))))
                .setStopReason(normalizeResponsesStopReason(responsePayload))
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(mapResponsesOutput(asList(responsePayload.get("output"))));
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "openai.responses.request", request));
            result.getArtifacts().add(toArtifact(ArtifactKind.RESPONSE, "openai.responses.response", response));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "openai.responses.tools", requestMapping.tools));
            }
        }

        return result;
    }

    static GenerationResult responsesFromStream(
            ResponseCreateParams request,
            ResponsesStreamSummary summary,
            OpenAiOptions options) {
        OpenAiOptions resolved = resolveOptions(options);
        if (summary.getFinalResponse() != null) {
            GenerationResult mapped = responsesFromRequestResponse(request, summary.getFinalResponse(), resolved);
            if (resolved.isRawArtifacts() && !summary.getEvents().isEmpty()) {
                mapped.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "openai.responses.stream_events", summary.getEvents()));
            }
            return mapped;
        }

        Map<String, Object> requestPayload = toMap(request);
        ResponsesRequestMapping requestMapping = mapResponsesRequest(requestPayload);

        String responseId = "";
        String responseModel = requestMapping.model;
        String stopReason = "";
        TokenUsage usage = new TokenUsage();
        StringBuilder outputText = new StringBuilder();
        List<Message> outputMessages = new ArrayList<>();

        for (ResponseStreamEvent event : summary.getEvents()) {
            Map<String, Object> eventPayload = toMap(event);
            String eventType = getString(eventPayload, "type");
            if (eventType.isBlank()) {
                for (Map.Entry<String, Object> entry : eventPayload.entrySet()) {
                    Map<String, Object> nested = asMap(entry.getValue());
                    String nestedType = getString(nested, "type");
                    if (!nestedType.isBlank()) {
                        eventPayload = nested;
                        eventType = nestedType;
                        break;
                    }
                }
            }

            Map<String, Object> eventResponse = asMap(getFirst(eventPayload, "response"));
            if (!eventResponse.isEmpty()) {
                responseId = firstNonBlank(responseId, getString(eventResponse, "id"));
                responseModel = firstNonBlank(responseModel, getString(eventResponse, "model"));
                Map<String, Object> usagePayload = asMap(getFirst(eventResponse, "usage"));
                if (!usagePayload.isEmpty()) {
                    usage = mapResponsesUsage(usagePayload);
                }
                String normalized = normalizeResponsesStopReason(eventResponse);
                if (!normalized.isBlank()) {
                    stopReason = normalized;
                }
            }

            switch (eventType) {
                case "response.output_text.delta", "response.refusal.delta" -> {
                    String delta = getString(eventPayload, "delta");
                    if (!delta.isBlank()) {
                        outputText.append(delta);
                    }
                }
                case "response.output_text.done" -> {
                    if (outputText.isEmpty()) {
                        String text = getString(eventPayload, "text");
                        if (!text.isBlank()) {
                            outputText.append(text);
                        }
                    }
                }
                case "response.refusal.done" -> {
                    if (outputText.isEmpty()) {
                        String refusal = getString(eventPayload, "refusal");
                        if (!refusal.isBlank()) {
                            outputText.append(refusal);
                        }
                    }
                }
                case "response.output_item.done", "response.output_item.added" -> {
                    outputMessages.addAll(mapResponsesOutput(List.of(getFirst(eventPayload, "item"))));
                }
                case "response.completed" -> {
                    if (stopReason.isBlank()) {
                        stopReason = "stop";
                    }
                }
                case "response.incomplete" -> {
                    if (stopReason.isBlank()) {
                        Map<String, Object> details = asMap(getFirst(eventPayload, "incomplete_details", "incompleteDetails"));
                        stopReason = firstNonBlank(getString(details, "reason"), "incomplete");
                    }
                }
                case "response.failed" -> {
                    if (stopReason.isBlank()) {
                        stopReason = "failed";
                    }
                }
                case "response.cancelled" -> {
                    if (stopReason.isBlank()) {
                        stopReason = "cancelled";
                    }
                }
                default -> {
                    // no-op
                }
            }
        }

        if (!outputText.isEmpty()) {
            outputMessages.add(0, new Message()
                    .setRole(MessageRole.ASSISTANT)
                    .setParts(List.of(MessagePart.text(outputText.toString()))));
        }

        GenerationResult result = new GenerationResult()
                .setConversationId(resolved.getConversationId())
                .setAgentName(resolved.getAgentName())
                .setAgentVersion(resolved.getAgentVersion())
                .setModel(new ModelRef().setProvider("openai").setName(requestMapping.model))
                .setResponseId(responseId)
                .setResponseModel(responseModel)
                .setSystemPrompt(requestMapping.systemPrompt)
                .setMaxTokens(requestMapping.maxTokens)
                .setTemperature(requestMapping.temperature)
                .setTopP(requestMapping.topP)
                .setToolChoice(requestMapping.toolChoice)
                .setThinkingEnabled(requestMapping.thinkingEnabled)
                .setUsage(usage)
                .setStopReason(stopReason)
                .setMetadata(metadataWithThinkingBudget(resolved.getMetadata(), requestMapping.thinkingBudget))
                .setTags(new LinkedHashMap<>(resolved.getTags()));

        result.getInput().addAll(requestMapping.input);
        result.getOutput().addAll(outputMessages);
        result.getTools().addAll(requestMapping.tools);

        if (resolved.isRawArtifacts()) {
            result.getArtifacts().add(toArtifact(ArtifactKind.REQUEST, "openai.responses.request", request));
            if (!requestMapping.tools.isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.TOOLS, "openai.responses.tools", requestMapping.tools));
            }
            if (!summary.getEvents().isEmpty()) {
                result.getArtifacts().add(toArtifact(ArtifactKind.PROVIDER_EVENT, "openai.responses.stream_events", summary.getEvents()));
            }
        }

        return result;
    }

    private static ChatRequestMapping mapChatRequest(Map<String, Object> payload) {
        String model = getString(payload, "model");
        Long maxTokens = firstLong(getFirst(payload, "max_completion_tokens", "maxCompletionTokens"), getFirst(payload, "max_tokens", "maxTokens"));
        Double temperature = getDouble(payload, "temperature");
        Double topP = getDouble(payload, "top_p", "topP");
        String toolChoice = canonicalToolChoice(getFirst(payload, "tool_choice", "toolChoice"));

        Boolean thinkingEnabled = payload.containsKey("reasoning_effort") || payload.containsKey("reasoningEffort") ? Boolean.TRUE : null;
        Long thinkingBudget = null;

        List<Message> input = new ArrayList<>();
        List<String> systemPrompts = new ArrayList<>();
        for (Map<String, Object> message : asMapList(getFirst(payload, "messages"))) {
            String role = getString(message, "role").toLowerCase();
            String text = extractText(getFirst(message, "content"));
            if ("system".equals(role) || "developer".equals(role)) {
                if (!text.isBlank()) {
                    systemPrompts.add(text);
                }
                continue;
            }

            if ("tool".equals(role)) {
                ToolResultPart toolResult = new ToolResultPart()
                        .setToolCallId(firstNonBlank(getString(message, "tool_call_id", "toolCallId"), getString(message, "id"))
                        )
                        .setName(getString(message, "name"))
                        .setContent(text)
                        .setContentJson(jsonStringBytes(text));
                MessagePart part = MessagePart.toolResult(toolResult);
                part.setMetadata(new PartMetadata().setProviderType("tool_result"));
                input.add(new Message().setRole(MessageRole.TOOL).setParts(List.of(part)));
                continue;
            }

            if (!text.isBlank()) {
                input.add(new Message()
                        .setRole("assistant".equals(role) ? MessageRole.ASSISTANT : MessageRole.USER)
                        .setName(getString(message, "name"))
                        .setParts(List.of(MessagePart.text(text))));
            }
        }

        List<ToolDefinition> tools = mapTools(getFirst(payload, "tools"));

        return new ChatRequestMapping(
                model,
                String.join("\n\n", systemPrompts),
                input,
                tools,
                maxTokens,
                temperature,
                topP,
                toolChoice,
                thinkingEnabled,
                thinkingBudget);
    }

    private static List<Message> mapChatResponseOutput(Map<String, Object> payload) {
        List<Message> output = new ArrayList<>();

        for (Map<String, Object> choice : asMapList(getFirst(payload, "choices"))) {
            Map<String, Object> message = asMap(getFirst(choice, "message"));
            if (message.isEmpty()) {
                continue;
            }

            List<MessagePart> parts = new ArrayList<>();
            String content = extractText(getFirst(message, "content"));
            if (!content.isBlank()) {
                parts.add(MessagePart.text(content));
            }

            for (Map<String, Object> toolCall : asMapList(getFirst(message, "tool_calls", "toolCalls"))) {
                Map<String, Object> function = asMap(getFirst(toolCall, "function"));
                MessagePart part = MessagePart.toolCall(new ToolCall()
                        .setId(getString(toolCall, "id"))
                        .setName(getString(function, "name"))
                        .setInputJson(jsonStringBytes(getString(function, "arguments"))));
                part.setMetadata(new PartMetadata().setProviderType("tool_call"));
                parts.add(part);
            }

            if (!parts.isEmpty()) {
                output.add(new Message().setRole(MessageRole.ASSISTANT).setParts(parts));
            }
        }

        return output;
    }

    private static ResponsesRequestMapping mapResponsesRequest(Map<String, Object> payload) {
        String model = getString(payload, "model");
        Long maxTokens = asLong(getFirst(payload, "max_output_tokens", "maxOutputTokens"));
        Double temperature = getDouble(payload, "temperature");
        Double topP = getDouble(payload, "top_p", "topP");
        String toolChoice = canonicalToolChoice(getFirst(payload, "tool_choice", "toolChoice"));
        Boolean thinkingEnabled = payload.containsKey("reasoning") ? Boolean.TRUE : null;
        Long thinkingBudget = resolveThinkingBudget(getFirst(payload, "reasoning"));

        List<Message> input = new ArrayList<>();
        List<String> systemPrompts = new ArrayList<>();

        String instructions = getString(payload, "instructions");
        if (!instructions.isBlank()) {
            systemPrompts.add(instructions);
        }

        Object rawInput = getFirst(payload, "input");
        if (rawInput instanceof String text) {
            if (!text.isBlank()) {
                input.add(new Message().setRole(MessageRole.USER).setParts(List.of(MessagePart.text(text))));
            }
        } else {
            for (Map<String, Object> item : asMapList(rawInput)) {
                String type = getString(item, "type");
                String role = getString(item, "role").toLowerCase();

                if ("message".equals(type) && ("system".equals(role) || "developer".equals(role))) {
                    String systemText = extractText(getFirst(item, "content"));
                    if (!systemText.isBlank()) {
                        systemPrompts.add(systemText);
                    }
                    continue;
                }

                if ("function_call_output".equals(type)) {
                    String content = firstNonBlank(extractText(getFirst(item, "output")), jsonValueText(getFirst(item, "output")));
                    if (!content.isBlank()) {
                        ToolResultPart toolResult = new ToolResultPart()
                                .setToolCallId(getString(item, "call_id", "callId"))
                                .setName(getString(item, "name"))
                                .setContent(content)
                                .setContentJson(jsonStringBytes(content));
                        MessagePart part = MessagePart.toolResult(toolResult);
                        part.setMetadata(new PartMetadata().setProviderType("tool_result"));
                        input.add(new Message().setRole(MessageRole.TOOL).setParts(List.of(part)));
                    }
                    continue;
                }

                if ("message".equals(type) || !role.isBlank()) {
                    String content = extractText(getFirst(item, "content"));
                    if (!content.isBlank()) {
                        MessageRole mappedRole = switch (role) {
                            case "assistant" -> MessageRole.ASSISTANT;
                            case "tool" -> MessageRole.TOOL;
                            default -> MessageRole.USER;
                        };
                        input.add(new Message().setRole(mappedRole).setParts(List.of(MessagePart.text(content))));
                    }
                }
            }
        }

        List<ToolDefinition> tools = mapTools(getFirst(payload, "tools"));

        return new ResponsesRequestMapping(
                model,
                String.join("\n\n", systemPrompts),
                input,
                tools,
                maxTokens,
                temperature,
                topP,
                toolChoice,
                thinkingEnabled,
                thinkingBudget);
    }

    private static List<Message> mapResponsesOutput(List<?> items) {
        List<Message> output = new ArrayList<>();
        for (Map<String, Object> item : asMapList(items)) {
            String type = getString(item, "type");
            switch (type) {
                case "message" -> {
                    String text = extractText(getFirst(item, "content"));
                    if (!text.isBlank()) {
                        output.add(new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(MessagePart.text(text))));
                    }
                }
                case "function_call" -> {
                    String name = getString(item, "name");
                    if (!name.isBlank()) {
                        MessagePart part = MessagePart.toolCall(new ToolCall()
                                .setId(getString(item, "call_id", "callId"))
                                .setName(name)
                                .setInputJson(jsonStringBytes(getString(item, "arguments"))));
                        part.setMetadata(new PartMetadata().setProviderType("tool_call"));
                        output.add(new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(part)));
                    }
                }
                case "function_call_output" -> {
                    String text = firstNonBlank(extractText(getFirst(item, "output")), jsonValueText(getFirst(item, "output")));
                    if (!text.isBlank()) {
                        ToolResultPart toolResult = new ToolResultPart()
                                .setToolCallId(getString(item, "call_id", "callId"))
                                .setName(getString(item, "name"))
                                .setContent(text)
                                .setContentJson(jsonStringBytes(text));
                        MessagePart part = MessagePart.toolResult(toolResult);
                        part.setMetadata(new PartMetadata().setProviderType("tool_result"));
                        output.add(new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(part)));
                    }
                }
                default -> {
                    String fallback = firstNonBlank(
                            extractText(getFirst(item, "output")),
                            getString(item, "result"),
                            getString(item, "error"));
                    if (!fallback.isBlank()) {
                        output.add(new Message().setRole(MessageRole.ASSISTANT).setParts(List.of(MessagePart.text(fallback))));
                    }
                }
            }
        }
        return output;
    }

    private static List<ToolDefinition> mapTools(Object rawTools) {
        List<ToolDefinition> tools = new ArrayList<>();
        for (Map<String, Object> tool : asMapList(rawTools)) {
            String toolType = getString(tool, "type");
            if ("function".equals(toolType)) {
                Map<String, Object> function = asMap(getFirst(tool, "function"));
                String name = firstNonBlank(getString(function, "name"), getString(tool, "name"));
                if (name.isBlank()) {
                    continue;
                }
                ToolDefinition definition = new ToolDefinition()
                        .setName(name)
                        .setDescription(firstNonBlank(getString(function, "description"), getString(tool, "description")))
                        .setType("function");
                Object parameters = function.containsKey("parameters") ? function.get("parameters") : getFirst(tool, "parameters");
                if (parameters != null) {
                    definition.setInputSchemaJson(jsonValueBytes(parameters));
                }
                tools.add(definition);
                continue;
            }

            String name = getString(tool, "name");
            if (!toolType.isBlank() && !name.isBlank()) {
                tools.add(new ToolDefinition().setName(name).setType(toolType));
            }
        }
        return tools;
    }

    private static TokenUsage mapChatUsage(Map<String, Object> usage) {
        if (usage.isEmpty()) {
            return new TokenUsage();
        }
        Map<String, Object> promptDetails = asMap(getFirst(usage, "prompt_tokens_details", "promptTokensDetails"));
        Map<String, Object> completionDetails = asMap(getFirst(usage, "completion_tokens_details", "completionTokensDetails"));
        long cacheReadInputTokens = defaultLong(asLong(getFirst(promptDetails, "cached_tokens", "cachedTokens")));
        return new TokenUsage()
                .setInputTokens(freshInputTokens(
                        defaultLong(asLong(getFirst(usage, "prompt_tokens", "promptTokens"))),
                        cacheReadInputTokens))
                .setOutputTokens(defaultLong(asLong(getFirst(usage, "completion_tokens", "completionTokens"))))
                .setTotalTokens(defaultLong(asLong(getFirst(usage, "total_tokens", "totalTokens"))))
                .setCacheReadInputTokens(cacheReadInputTokens)
                .setReasoningTokens(defaultLong(asLong(getFirst(completionDetails, "reasoning_tokens", "reasoningTokens"))))
                .setInputIsDisjoint(true);
    }

    private static TokenUsage mapResponsesUsage(Map<String, Object> usage) {
        if (usage.isEmpty()) {
            return new TokenUsage();
        }
        Map<String, Object> inputDetails = asMap(getFirst(usage, "input_tokens_details", "inputTokensDetails"));
        Map<String, Object> outputDetails = asMap(getFirst(usage, "output_tokens_details", "outputTokensDetails"));
        long cacheReadInputTokens = defaultLong(asLong(getFirst(inputDetails, "cached_tokens", "cachedTokens")));
        return new TokenUsage()
                .setInputTokens(freshInputTokens(
                        defaultLong(asLong(getFirst(usage, "input_tokens", "inputTokens"))),
                        cacheReadInputTokens))
                .setOutputTokens(defaultLong(asLong(getFirst(usage, "output_tokens", "outputTokens"))))
                .setTotalTokens(defaultLong(asLong(getFirst(usage, "total_tokens", "totalTokens"))))
                .setCacheReadInputTokens(cacheReadInputTokens)
                .setReasoningTokens(defaultLong(asLong(getFirst(outputDetails, "reasoning_tokens", "reasoningTokens"))))
                .setInputIsDisjoint(true);
    }

    private static long freshInputTokens(long rawInputTokens, long cacheReadInputTokens) {
        return Math.max(rawInputTokens - cacheReadInputTokens, 0L);
    }

    private static String firstChoiceFinishReason(Map<String, Object> responsePayload) {
        List<Map<String, Object>> choices = asMapList(getFirst(responsePayload, "choices"));
        if (choices.isEmpty()) {
            return "";
        }
        return getString(choices.get(0), "finish_reason", "finishReason");
    }

    private static String normalizeChatStopReason(String raw) {
        String normalized = raw == null ? "" : raw.trim().toLowerCase();
        if (normalized.isBlank()) {
            return "";
        }
        return switch (normalized) {
            case "stop", "length", "tool_calls", "content_filter", "function_call" -> normalized;
            default -> normalized;
        };
    }

    private static String normalizeResponsesStopReason(Map<String, Object> responsePayload) {
        String status = getString(responsePayload, "status").toLowerCase();
        if (status.isBlank()) {
            return "";
        }
        if ("completed".equals(status)) {
            return "stop";
        }
        if ("incomplete".equals(status)) {
            Map<String, Object> details = asMap(getFirst(responsePayload, "incomplete_details", "incompleteDetails"));
            return firstNonBlank(getString(details, "reason"), "incomplete");
        }
        return status;
    }

    private static String canonicalToolChoice(Object value) {
        if (value == null) {
            return null;
        }
        if (value instanceof String text) {
            String normalized = text.trim().toLowerCase();
            return normalized.isBlank() ? null : normalized;
        }
        try {
            return CANONICAL_JSON.writeValueAsString(value);
        } catch (Exception ignored) {
            String fallback = String.valueOf(value).trim();
            return fallback.isBlank() ? null : fallback;
        }
    }

    private static Long resolveThinkingBudget(Object reasoning) {
        if (!(reasoning instanceof Map<?, ?> map)) {
            return null;
        }

        for (String key : List.of("budget_tokens", "thinking_budget", "thinkingBudget", "max_output_tokens")) {
            Long value = asLong(map.get(key));
            if (value != null) {
                return value;
            }
        }
        return null;
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

    private static Artifact toArtifact(ArtifactKind kind, String name, Object payload) {
        try {
            return new Artifact()
                    .setKind(kind)
                    .setName(name)
                    .setContentType("application/json")
                    .setPayload(JSON.writeValueAsBytes(payload));
        } catch (Exception ignored) {
            return new Artifact()
                    .setKind(kind)
                    .setName(name)
                    .setContentType("application/json")
                    .setPayload(String.valueOf(payload).getBytes(StandardCharsets.UTF_8));
        }
    }

    private static byte[] jsonValueBytes(Object value) {
        try {
            return JSON.writeValueAsBytes(value);
        } catch (Exception ignored) {
            return new byte[0];
        }
    }

    private static String jsonValueText(Object value) {
        byte[] bytes = jsonValueBytes(value);
        return bytes.length == 0 ? "" : new String(bytes, StandardCharsets.UTF_8);
    }

    private static byte[] jsonStringBytes(String value) {
        String raw = value == null ? "" : value;
        if (raw.isBlank()) {
            return new byte[0];
        }
        String trimmed = raw.trim();
        if ((trimmed.startsWith("{") && trimmed.endsWith("}")) || (trimmed.startsWith("[") && trimmed.endsWith("]"))) {
            return trimmed.getBytes(StandardCharsets.UTF_8);
        }
        return jsonValueBytes(raw);
    }

    private static String extractText(Object value) {
        if (value == null) {
            return "";
        }
        if (value instanceof String text) {
            return text.trim();
        }
        if (value instanceof List<?> list) {
            List<String> parts = new ArrayList<>();
            for (Object item : list) {
                String text = extractText(item);
                if (!text.isBlank()) {
                    parts.add(text);
                }
            }
            return String.join("\n", parts);
        }
        if (value instanceof Map<?, ?> map) {
            String text = asString(map.get("text"));
            if (!text.isBlank()) {
                return text;
            }
            String content = asString(map.get("content"));
            if (!content.isBlank()) {
                return content;
            }
            String refusal = asString(map.get("refusal"));
            if (!refusal.isBlank()) {
                return refusal;
            }
            String output = asString(map.get("output_text"));
            if (!output.isBlank()) {
                return output;
            }
            String outputValue = asString(map.get("output"));
            if (!outputValue.isBlank()) {
                return outputValue;
            }
        }
        return "";
    }

    private static int embeddingInputCount(EmbeddingCreateParams.Input input) {
        if (input == null) {
            return 0;
        }
        if (input.isString()) {
            return 1;
        }
        if (input.isArrayOfStrings()) {
            return input.asArrayOfStrings().size();
        }
        if (input.isArrayOfTokens()) {
            return 1;
        }
        if (input.isArrayOfTokenArrays()) {
            return input.asArrayOfTokenArrays().size();
        }
        return 0;
    }

    private static List<String> embeddingInputTexts(EmbeddingCreateParams.Input input) {
        if (input == null) {
            return List.of();
        }
        if (input.isString()) {
            String value = input.asString();
            return value == null || value.isBlank() ? List.of() : List.of(value);
        }
        if (input.isArrayOfStrings()) {
            List<String> values = new ArrayList<>();
            for (String value : input.asArrayOfStrings()) {
                if (value != null && !value.isBlank()) {
                    values.add(value);
                }
            }
            return values;
        }
        return List.of();
    }

    private static String firstNonBlank(String... values) {
        for (String value : values) {
            if (value != null && !value.isBlank()) {
                return value;
            }
        }
        return "";
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> toMap(Object payload) {
        if (payload == null) {
            return Map.of();
        }
        try {
            Map<String, Object> mapped = JSON.convertValue(payload, new TypeReference<Map<String, Object>>() {
            });
            Object body = mapped.get("body");
            if (body instanceof Map<?, ?> bodyMap && !bodyMap.isEmpty()) {
                return new LinkedHashMap<>((Map<String, Object>) bodyMap);
            }
            return mapped;
        } catch (Exception ignored) {
            return Map.of();
        }
    }

    @SuppressWarnings("unchecked")
    private static List<Map<String, Object>> asMapList(Object value) {
        if (!(value instanceof List<?> list)) {
            return List.of();
        }
        List<Map<String, Object>> out = new ArrayList<>();
        for (Object item : list) {
            if (item instanceof Map<?, ?> map) {
                out.add((Map<String, Object>) map);
            }
        }
        return out;
    }

    private static List<?> asList(Object value) {
        return value instanceof List<?> list ? list : List.of();
    }

    @SuppressWarnings("unchecked")
    private static Map<String, Object> asMap(Object value) {
        if (value instanceof Map<?, ?> map) {
            return (Map<String, Object>) map;
        }
        return Map.of();
    }

    private static String asString(Object value) {
        if (value == null) {
            return "";
        }
        if (value instanceof String text) {
            return text.trim();
        }
        return String.valueOf(value).trim();
    }

    private static Object getFirst(Map<String, Object> value, String... keys) {
        if (value == null) {
            return null;
        }
        for (String key : keys) {
            if (value.containsKey(key)) {
                return value.get(key);
            }
        }
        return null;
    }

    private static String getString(Map<String, Object> value, String... keys) {
        return asString(getFirst(value, keys));
    }

    private static Double getDouble(Map<String, Object> value, String... keys) {
        return asDouble(getFirst(value, keys));
    }

    private static Long asLong(Object value) {
        if (value instanceof Number number) {
            return number.longValue();
        }
        if (value instanceof String text) {
            try {
                return Long.parseLong(text.trim());
            } catch (NumberFormatException ignored) {
                return null;
            }
        }
        return null;
    }

    private static Double asDouble(Object value) {
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

    private static Long firstLong(Object primary, Object fallback) {
        Long first = asLong(primary);
        return first != null ? first : asLong(fallback);
    }

    private static long defaultLong(Long value) {
        return value == null ? 0L : value;
    }

    private static int asInt(Object value, int fallback) {
        if (value instanceof Number number) {
            return number.intValue();
        }
        if (value instanceof String text) {
            try {
                return Integer.parseInt(text.trim());
            } catch (NumberFormatException ignored) {
                return fallback;
            }
        }
        return fallback;
    }

    private static OpenAiOptions resolveOptions(OpenAiOptions options) {
        return options == null ? new OpenAiOptions() : options;
    }

    private static final class ToolCallAccumulator {
        private String id = "";
        private String name = "";
        private final StringBuilder arguments = new StringBuilder();
    }

    private record ChatRequestMapping(
            String model,
            String systemPrompt,
            List<Message> input,
            List<ToolDefinition> tools,
            Long maxTokens,
            Double temperature,
            Double topP,
            String toolChoice,
            Boolean thinkingEnabled,
            Long thinkingBudget) {
    }

    private record ResponsesRequestMapping(
            String model,
            String systemPrompt,
            List<Message> input,
            List<ToolDefinition> tools,
            Long maxTokens,
            Double temperature,
            Double topP,
            String toolChoice,
            Boolean thinkingEnabled,
            Long thinkingBudget) {
    }
}
