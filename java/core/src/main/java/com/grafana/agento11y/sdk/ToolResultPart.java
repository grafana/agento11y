package com.grafana.agento11y.sdk;

import java.util.Arrays;

/** Tool role result payload. */
public final class ToolResultPart {
    private String toolCallId = "";
    private String name = "";
    private String content = "";
    private byte[] contentJson = new byte[0];
    private boolean error;

    public String getToolCallId() {
        return toolCallId;
    }

    public ToolResultPart setToolCallId(String toolCallId) {
        this.toolCallId = toolCallId == null ? "" : toolCallId;
        return this;
    }

    public String getName() {
        return name;
    }

    public ToolResultPart setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public String getContent() {
        return content;
    }

    public ToolResultPart setContent(String content) {
        this.content = content == null ? "" : content;
        return this;
    }

    public byte[] getContentJson() {
        return contentJson;
    }

    public ToolResultPart setContentJson(byte[] contentJson) {
        this.contentJson = contentJson == null ? new byte[0] : Arrays.copyOf(contentJson, contentJson.length);
        return this;
    }

    public boolean isError() {
        return error;
    }

    public ToolResultPart setError(boolean error) {
        this.error = error;
        return this;
    }

    public ToolResultPart copy() {
        return new ToolResultPart()
                .setToolCallId(toolCallId)
                .setName(name)
                .setContent(content)
                .setContentJson(contentJson)
                .setError(error);
    }
}
