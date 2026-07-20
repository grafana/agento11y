package com.grafana.agento11y.sdk;

import java.util.List;

final class GenerationValidator {
    private GenerationValidator() {
    }

    static void validate(Generation generation) {
        boolean contentStripped = Agento11yClient.isContentStripped(generation);

        if (generation.getMode() != GenerationMode.SYNC && generation.getMode() != GenerationMode.STREAM) {
            throw new ValidationException("generation.mode must be one of SYNC|STREAM");
        }
        if (generation.getModel().getProvider().isBlank()) {
            throw new ValidationException("generation.model.provider is required");
        }
        if (generation.getModel().getName().isBlank()) {
            throw new ValidationException("generation.model.name is required");
        }

        validateMessages("generation.input", generation.getInput(), contentStripped);
        validateMessages("generation.output", generation.getOutput(), contentStripped);

        for (int i = 0; i < generation.getTools().size(); i++) {
            ToolDefinition tool = generation.getTools().get(i);
            if (tool == null || tool.getName().isBlank()) {
                throw new ValidationException("generation.tools[" + i + "].name is required");
            }
        }

        for (int i = 0; i < generation.getArtifacts().size(); i++) {
            Artifact artifact = generation.getArtifacts().get(i);
            if (artifact == null) {
                throw new ValidationException("generation.artifacts[" + i + "] is required");
            }
            if (artifact.getKind() == null) {
                throw new ValidationException("generation.artifacts[" + i + "].kind is invalid");
            }
            if (artifact.getRecordId().isBlank() && artifact.getPayload().length == 0) {
                throw new ValidationException("generation.artifacts[" + i + "] must provide payload or record_id");
            }
        }
    }

    private static void validateMessages(String path, List<Message> messages, boolean contentStripped) {
        for (int i = 0; i < messages.size(); i++) {
            Message message = messages.get(i);
            if (message == null || message.getRole() == null) {
                throw new ValidationException(path + "[" + i + "].role must be one of user|assistant|tool");
            }
            if (message.getParts().isEmpty()) {
                throw new ValidationException(path + "[" + i + "].parts must not be empty");
            }
            for (int j = 0; j < message.getParts().size(); j++) {
                validatePart(path, i, j, message.getRole(), message.getParts().get(j), contentStripped);
            }
        }
    }

    private static void validatePart(String path, int messageIndex, int partIndex, MessageRole role, MessagePart part, boolean contentStripped) {
        if (part == null || part.getKind() == null) {
            throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].kind is invalid");
        }

        boolean strippedTextOrThinking = contentStripped
                && (part.getKind() == MessagePartKind.TEXT || part.getKind() == MessagePartKind.THINKING);

        int payloadFieldCount = 0;
        if (!part.getText().isBlank()) {
            payloadFieldCount++;
        }
        if (!part.getThinking().isBlank()) {
            payloadFieldCount++;
        }
        if (part.getToolCall() != null) {
            payloadFieldCount++;
        }
        if (part.getToolResult() != null) {
            payloadFieldCount++;
        }
        if (payloadFieldCount != 1 && !strippedTextOrThinking) {
            throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "] must set exactly one payload field");
        }

        switch (part.getKind()) {
            case TEXT -> {
                if (part.getText().isBlank() && !contentStripped) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].text is required");
                }
            }
            case THINKING -> {
                if (role != MessageRole.ASSISTANT) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].thinking only allowed for assistant role");
                }
                if (part.getThinking().isBlank() && !contentStripped) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].thinking is required");
                }
            }
            case TOOL_CALL -> {
                if (role != MessageRole.ASSISTANT) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].tool_call only allowed for assistant role");
                }
                if (part.getToolCall() == null || part.getToolCall().getName().isBlank()) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].tool_call.name is required");
                }
            }
            case TOOL_RESULT -> {
                if (role != MessageRole.TOOL) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].tool_result only allowed for tool role");
                }
                if (part.getToolResult() == null) {
                    throw new ValidationException(path + "[" + messageIndex + "].parts[" + partIndex + "].tool_result is required");
                }
            }
        }
    }
}
