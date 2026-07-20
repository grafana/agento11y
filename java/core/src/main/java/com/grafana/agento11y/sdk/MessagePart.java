package com.grafana.agento11y.sdk;

/** Typed message part. */
public final class MessagePart {
    private MessagePartKind kind = MessagePartKind.TEXT;
    private String text = "";
    private String thinking = "";
    private ToolCall toolCall;
    private ToolResultPart toolResult;
    private PartMetadata metadata = new PartMetadata();

    public MessagePartKind getKind() {
        return kind;
    }

    public MessagePart setKind(MessagePartKind kind) {
        this.kind = kind == null ? MessagePartKind.TEXT : kind;
        return this;
    }

    public String getText() {
        return text;
    }

    public MessagePart setText(String text) {
        this.text = text == null ? "" : text;
        return this;
    }

    public String getThinking() {
        return thinking;
    }

    public MessagePart setThinking(String thinking) {
        this.thinking = thinking == null ? "" : thinking;
        return this;
    }

    public ToolCall getToolCall() {
        return toolCall;
    }

    public MessagePart setToolCall(ToolCall toolCall) {
        this.toolCall = toolCall;
        return this;
    }

    public ToolResultPart getToolResult() {
        return toolResult;
    }

    public MessagePart setToolResult(ToolResultPart toolResult) {
        this.toolResult = toolResult;
        return this;
    }

    public PartMetadata getMetadata() {
        return metadata;
    }

    public MessagePart setMetadata(PartMetadata metadata) {
        this.metadata = metadata == null ? new PartMetadata() : metadata;
        return this;
    }

    public MessagePart copy() {
        return new MessagePart()
                .setKind(kind)
                .setText(text)
                .setThinking(thinking)
                .setToolCall(toolCall == null ? null : toolCall.copy())
                .setToolResult(toolResult == null ? null : toolResult.copy())
                .setMetadata(metadata == null ? new PartMetadata() : metadata.copy());
    }

    public static MessagePart text(String text) {
        return new MessagePart().setKind(MessagePartKind.TEXT).setText(text);
    }

    public static MessagePart thinking(String thinking) {
        return new MessagePart().setKind(MessagePartKind.THINKING).setThinking(thinking);
    }

    public static MessagePart toolCall(ToolCall toolCall) {
        return new MessagePart().setKind(MessagePartKind.TOOL_CALL).setToolCall(toolCall);
    }

    public static MessagePart toolResult(ToolResultPart toolResult) {
        return new MessagePart().setKind(MessagePartKind.TOOL_RESULT).setToolResult(toolResult);
    }
}
