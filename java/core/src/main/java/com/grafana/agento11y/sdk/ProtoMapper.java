package com.grafana.agento11y.sdk;

import com.google.protobuf.ByteString;
import com.google.protobuf.ListValue;
import com.google.protobuf.NullValue;
import com.google.protobuf.Struct;
import com.google.protobuf.Timestamp;
import com.google.protobuf.Value;
import java.nio.charset.StandardCharsets;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Instant;
import java.util.ArrayList;
import java.util.HexFormat;
import java.util.List;
import java.util.Map;
import agento11y.v1.GenerationIngest;

final class ProtoMapper {
    private ProtoMapper() {
    }

    static GenerationIngest.ExportGenerationsRequest toProtoRequest(ExportGenerationsRequest request) {
        GenerationIngest.ExportGenerationsRequest.Builder builder = GenerationIngest.ExportGenerationsRequest.newBuilder();
        for (Generation generation : request.getGenerations()) {
            builder.addGenerations(toProtoGeneration(generation));
        }
        return builder.build();
    }

    static GenerationIngest.Generation toProtoGeneration(Generation generation) {
        GenerationIngest.Generation.Builder builder = GenerationIngest.Generation.newBuilder()
                .setId(generation.getId())
                .setConversationId(generation.getConversationId())
                .setOperationName(generation.getOperationName())
                .setMode(toProtoMode(generation.getMode()))
                .setTraceId(generation.getTraceId())
                .setSpanId(generation.getSpanId())
                .setResponseId(generation.getResponseId())
                .setResponseModel(generation.getResponseModel())
                .setSystemPrompt(generation.getSystemPrompt())
                .setStopReason(generation.getStopReason())
                .setCallError(generation.getCallError())
                .setAgentName(generation.getAgentName())
                .setAgentVersion(generation.getAgentVersion());

        builder.setModel(GenerationIngest.ModelRef.newBuilder()
                .setProvider(generation.getModel().getProvider())
                .setName(generation.getModel().getName())
                .build());

        if (generation.getStartedAt() != null) {
            builder.setStartedAt(toTimestamp(generation.getStartedAt()));
        }
        if (generation.getCompletedAt() != null) {
            builder.setCompletedAt(toTimestamp(generation.getCompletedAt()));
        }

        if (generation.getMaxTokens() != null) {
            builder.setMaxTokens(generation.getMaxTokens());
        }
        if (generation.getTemperature() != null) {
            builder.setTemperature(generation.getTemperature());
        }
        if (generation.getTopP() != null) {
            builder.setTopP(generation.getTopP());
        }
        if (!generation.getToolChoice().isBlank()) {
            builder.setToolChoice(generation.getToolChoice());
        }
        if (generation.getThinkingEnabled() != null) {
            builder.setThinkingEnabled(generation.getThinkingEnabled());
        }
        builder.addAllParentGenerationIds(generation.getParentGenerationIds());
        String trimmedEffectiveVersion = generation.getEffectiveVersion().strip();
        if (!trimmedEffectiveVersion.isEmpty()) {
            builder.setEffectiveVersion(canonicalEffectiveVersion(trimmedEffectiveVersion));
        }

        builder.putAllTags(generation.getTags());
        if (!generation.getMetadata().isEmpty()) {
            builder.setMetadata(toStruct(generation.getMetadata()));
        }

        TokenUsage usage = generation.getUsage();
        if (usage != null) {
            builder.setUsage(GenerationIngest.TokenUsage.newBuilder()
                    .setInputTokens(usage.getInputTokens())
                    .setOutputTokens(usage.getOutputTokens())
                    .setTotalTokens(usage.getTotalTokens())
                    .setCacheReadInputTokens(usage.getCacheReadInputTokens())
                    .setCacheWriteInputTokens(usage.getCacheWriteInputTokens())
                    .setReasoningTokens(usage.getReasoningTokens())
                    .build());
        }

        for (Message message : generation.getInput()) {
            builder.addInput(toProtoMessage(message));
        }
        for (Message message : generation.getOutput()) {
            builder.addOutput(toProtoMessage(message));
        }
        for (ToolDefinition tool : generation.getTools()) {
            builder.addTools(GenerationIngest.ToolDefinition.newBuilder()
                    .setName(tool.getName())
                    .setDescription(tool.getDescription())
                    .setType(tool.getType())
                    .setInputSchemaJson(ByteString.copyFrom(tool.getInputSchemaJson()))
                    .build());
        }
        for (Artifact artifact : generation.getArtifacts()) {
            builder.addRawArtifacts(GenerationIngest.Artifact.newBuilder()
                    .setKind(toProtoArtifactKind(artifact.getKind()))
                    .setName(artifact.getName())
                    .setContentType(artifact.getContentType())
                    .setPayload(ByteString.copyFrom(artifact.getPayload()))
                    .setRecordId(artifact.getRecordId())
                    .setUri(artifact.getUri())
                    .build());
        }

        return builder.build();
    }

    static ExportGenerationsResponse fromProtoResponse(GenerationIngest.ExportGenerationsResponse response, List<Generation> sent) {
        ExportGenerationsResponse out = new ExportGenerationsResponse();
        List<ExportGenerationResult> results = new ArrayList<>();
        for (int i = 0; i < response.getResultsCount(); i++) {
            GenerationIngest.ExportGenerationResult proto = response.getResults(i);
            String generationId = proto.getGenerationId();
            if (generationId.isBlank() && i < sent.size()) {
                generationId = sent.get(i).getId();
            }
            results.add(new ExportGenerationResult()
                    .setGenerationId(generationId)
                    .setAccepted(proto.getAccepted())
                    .setError(proto.getError()));
        }
        out.setResults(results);
        return out;
    }

    private static GenerationIngest.Message toProtoMessage(Message message) {
        GenerationIngest.Message.Builder builder = GenerationIngest.Message.newBuilder()
                .setRole(toProtoRole(message.getRole()))
                .setName(message.getName());

        for (MessagePart part : message.getParts()) {
            builder.addParts(toProtoPart(part));
        }

        return builder.build();
    }

    private static GenerationIngest.Part toProtoPart(MessagePart part) {
        GenerationIngest.Part.Builder builder = GenerationIngest.Part.newBuilder();
        if (part.getMetadata() != null && !part.getMetadata().getProviderType().isBlank()) {
            builder.setMetadata(GenerationIngest.PartMetadata.newBuilder().setProviderType(part.getMetadata().getProviderType()));
        }

        switch (part.getKind()) {
            case TEXT -> builder.setText(part.getText());
            case THINKING -> builder.setThinking(part.getThinking());
            case TOOL_CALL -> {
                ToolCall call = part.getToolCall() == null ? new ToolCall() : part.getToolCall();
                builder.setToolCall(GenerationIngest.ToolCall.newBuilder()
                        .setId(call.getId())
                        .setName(call.getName())
                        .setInputJson(ByteString.copyFrom(call.getInputJson()))
                        .build());
            }
            case TOOL_RESULT -> {
                ToolResultPart result = part.getToolResult() == null ? new ToolResultPart() : part.getToolResult();
                builder.setToolResult(GenerationIngest.ToolResult.newBuilder()
                        .setToolCallId(result.getToolCallId())
                        .setName(result.getName())
                        .setContent(result.getContent())
                        .setContentJson(ByteString.copyFrom(result.getContentJson()))
                        .setIsError(result.isError())
                        .build());
            }
        }

        return builder.build();
    }

    private static GenerationIngest.GenerationMode toProtoMode(GenerationMode mode) {
        if (mode == GenerationMode.STREAM) {
            return GenerationIngest.GenerationMode.GENERATION_MODE_STREAM;
        }
        return GenerationIngest.GenerationMode.GENERATION_MODE_SYNC;
    }

    private static GenerationIngest.MessageRole toProtoRole(MessageRole role) {
        if (role == MessageRole.ASSISTANT) {
            return GenerationIngest.MessageRole.MESSAGE_ROLE_ASSISTANT;
        }
        if (role == MessageRole.TOOL) {
            return GenerationIngest.MessageRole.MESSAGE_ROLE_TOOL;
        }
        return GenerationIngest.MessageRole.MESSAGE_ROLE_USER;
    }

    private static GenerationIngest.ArtifactKind toProtoArtifactKind(ArtifactKind kind) {
        if (kind == ArtifactKind.RESPONSE) {
            return GenerationIngest.ArtifactKind.ARTIFACT_KIND_RESPONSE;
        }
        if (kind == ArtifactKind.TOOLS) {
            return GenerationIngest.ArtifactKind.ARTIFACT_KIND_TOOLS;
        }
        if (kind == ArtifactKind.PROVIDER_EVENT) {
            return GenerationIngest.ArtifactKind.ARTIFACT_KIND_PROVIDER_EVENT;
        }
        return GenerationIngest.ArtifactKind.ARTIFACT_KIND_REQUEST;
    }

    private static String canonicalEffectiveVersion(String value) {
        try {
            MessageDigest digest = MessageDigest.getInstance("SHA-256");
            byte[] bytes = digest.digest(value.getBytes(StandardCharsets.UTF_8));
            return "sha256:" + HexFormat.of().formatHex(bytes);
        } catch (NoSuchAlgorithmException e) {
            throw new IllegalStateException("SHA-256 not available", e);
        }
    }

    private static Timestamp toTimestamp(Instant instant) {
        return Timestamp.newBuilder().setSeconds(instant.getEpochSecond()).setNanos(instant.getNano()).build();
    }

    private static Struct toStruct(Map<String, Object> metadata) {
        Struct.Builder builder = Struct.newBuilder();
        for (Map.Entry<String, Object> entry : metadata.entrySet()) {
            builder.putFields(entry.getKey(), toValue(entry.getValue()));
        }
        return builder.build();
    }

    @SuppressWarnings("unchecked")
    private static Value toValue(Object value) {
        if (value == null) {
            return Value.newBuilder().setNullValue(NullValue.NULL_VALUE).build();
        }
        if (value instanceof Boolean v) {
            return Value.newBuilder().setBoolValue(v).build();
        }
        if (value instanceof Number v) {
            return Value.newBuilder().setNumberValue(v.doubleValue()).build();
        }
        if (value instanceof CharSequence v) {
            return Value.newBuilder().setStringValue(v.toString()).build();
        }
        if (value instanceof Map<?, ?> v) {
            Struct.Builder struct = Struct.newBuilder();
            for (Map.Entry<?, ?> entry : v.entrySet()) {
                struct.putFields(String.valueOf(entry.getKey()), toValue(entry.getValue()));
            }
            return Value.newBuilder().setStructValue(struct).build();
        }
        if (value instanceof List<?> v) {
            ListValue.Builder list = ListValue.newBuilder();
            for (Object item : v) {
                list.addValues(toValue(item));
            }
            return Value.newBuilder().setListValue(list).build();
        }
        return Value.newBuilder().setStringValue(String.valueOf(value)).build();
    }
}
