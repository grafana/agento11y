package com.grafana.agento11y.sdk;

/**
 * Controls what content is included in exported generation payloads and OTel
 * span attributes.
 */
public enum ContentCaptureMode {
    /**
     * Uses the parent or client-level default. On config this resolves to
     * {@link #NO_TOOL_CONTENT} for backward compatibility. On GenerationStart
     * this inherits from config. On ToolExecutionStart this inherits from the
     * parent generation context, falling back to config.
     */
    DEFAULT,
    /** Exports all content. */
    FULL,
    /**
     * Exports full generation content but excludes tool execution content
     * (arguments and results) from span attributes unless explicitly opted in
     * via {@code includeContent} or a per-tool ContentCapture override. This
     * matches the pre-ContentCaptureMode SDK default behavior.
     */
    NO_TOOL_CONTENT,
    /**
     * Preserves message structure, tool names, token usage, and timing but
     * strips message text, thinking, tool arguments and results, system
     * prompts, raw artifacts, conversation titles, tool descriptions, tool
     * input schemas, and reduces error messages to their error category.
     *
     * <p>Note: user-provided Metadata and Tags are NOT stripped — callers are
     * responsible for ensuring these maps do not contain sensitive content when
     * using MetadataOnly mode.</p>
     */
    METADATA_ONLY,
    /**
     * Splits the proto and span paths for generation content. The proto
     * export keeps full content; the OTel span omits
     * {@code agento11y.conversation.title}, {@code gen_ai.tool.call.arguments},
     * {@code gen_ai.tool.call.result}, and {@code gen_ai.embeddings.input_texts}.
     *
     * <p>Use this mode when the gRPC ingest destination is private but the
     * OTel trace and metric destinations are shared and must not receive any
     * content. Tool execution and embedding spans behave identically to
     * {@link #METADATA_ONLY} (they have no separate gRPC export). Rating
     * comments are preserved.</p>
     */
    FULL_WITH_METADATA_SPANS;

    /**
     * Returns the string used in generation metadata markers.
     *
     * @throws IllegalStateException if called on {@link #DEFAULT}, which is a
     * placeholder for "inherit" and must be resolved before being stamped.
     */
    public String toMetadataValue() {
        return switch (this) {
            case FULL -> "full";
            case NO_TOOL_CONTENT -> "no_tool_content";
            case METADATA_ONLY -> "metadata_only";
            case FULL_WITH_METADATA_SPANS -> "full_with_metadata_spans";
            case DEFAULT -> throw new IllegalStateException(
                    "ContentCaptureMode.DEFAULT must be resolved before calling toMetadataValue()");
        };
    }
}
