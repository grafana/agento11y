package com.grafana.agento11y.sdk;

import java.util.Arrays;

/** Assistant tool call payload. */
public final class ToolCall {
    private String id = "";
    private String name = "";
    private byte[] inputJson = new byte[0];

    public String getId() {
        return id;
    }

    public ToolCall setId(String id) {
        this.id = id == null ? "" : id;
        return this;
    }

    public String getName() {
        return name;
    }

    public ToolCall setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public byte[] getInputJson() {
        return inputJson;
    }

    public ToolCall setInputJson(byte[] inputJson) {
        this.inputJson = inputJson == null ? new byte[0] : Arrays.copyOf(inputJson, inputJson.length);
        return this;
    }

    public ToolCall copy() {
        return new ToolCall().setId(id).setName(name).setInputJson(inputJson);
    }
}
