package com.grafana.agento11y.sdk;

import java.util.Arrays;

/** Tool definition visible to the model. */
public final class ToolDefinition {
    private String name = "";
    private String description = "";
    private String type = "";
    private byte[] inputSchemaJson = new byte[0];

    public String getName() {
        return name;
    }

    public ToolDefinition setName(String name) {
        this.name = name == null ? "" : name;
        return this;
    }

    public String getDescription() {
        return description;
    }

    public ToolDefinition setDescription(String description) {
        this.description = description == null ? "" : description;
        return this;
    }

    public String getType() {
        return type;
    }

    public ToolDefinition setType(String type) {
        this.type = type == null ? "" : type;
        return this;
    }

    public byte[] getInputSchemaJson() {
        return inputSchemaJson;
    }

    public ToolDefinition setInputSchemaJson(byte[] inputSchemaJson) {
        this.inputSchemaJson = inputSchemaJson == null ? new byte[0] : Arrays.copyOf(inputSchemaJson, inputSchemaJson.length);
        return this;
    }

    public ToolDefinition copy() {
        return new ToolDefinition()
                .setName(name)
                .setDescription(description)
                .setType(type)
                .setInputSchemaJson(inputSchemaJson);
    }
}
