package com.grafana.agento11y.sdk;

import io.opentelemetry.context.Context;
import io.opentelemetry.context.ContextKey;
import io.opentelemetry.context.Scope;

/** Context helpers for conversation and agent defaults. */
public final class Agento11yContext {
    private static final ContextKey<String> CONVERSATION_ID = ContextKey.named("agento11y.conversation.id");
    private static final ContextKey<String> CONVERSATION_TITLE = ContextKey.named("agento11y.conversation.title");
    private static final ContextKey<String> USER_ID = ContextKey.named("agento11y.user.id");
    private static final ContextKey<String> AGENT_NAME = ContextKey.named("agento11y.agent.name");
    private static final ContextKey<String> AGENT_VERSION = ContextKey.named("agento11y.agent.version");
    private static final ContextKey<ContentCaptureMode> CONTENT_CAPTURE_MODE = ContextKey.named("agento11y.content_capture_mode");

    private Agento11yContext() {
    }

    /**
     * Sets the conversation id in the current OTel context.
     *
     * <p>Use the returned {@link Scope} in try-with-resources to restore context automatically.</p>
     */
    public static Scope withConversationId(String conversationId) {
        return Context.current().with(CONVERSATION_ID, emptyToBlank(conversationId)).makeCurrent();
    }

    /**
     * Sets the conversation title in the current OTel context.
     *
     * <p>Use the returned {@link Scope} in try-with-resources to restore context automatically.</p>
     */
    public static Scope withConversationTitle(String conversationTitle) {
        return Context.current().with(CONVERSATION_TITLE, emptyToBlank(conversationTitle)).makeCurrent();
    }

    /**
     * Sets the user id in the current OTel context.
     *
     * <p>Use the returned {@link Scope} in try-with-resources to restore context automatically.</p>
     */
    public static Scope withUserId(String userId) {
        return Context.current().with(USER_ID, emptyToBlank(userId)).makeCurrent();
    }

    /**
     * Sets the agent name in the current OTel context.
     *
     * <p>Use the returned {@link Scope} in try-with-resources to restore context automatically.</p>
     */
    public static Scope withAgentName(String agentName) {
        return Context.current().with(AGENT_NAME, emptyToBlank(agentName)).makeCurrent();
    }

    /**
     * Sets the agent version in the current OTel context.
     *
     * <p>Use the returned {@link Scope} in try-with-resources to restore context automatically.</p>
     */
    public static Scope withAgentVersion(String agentVersion) {
        return Context.current().with(AGENT_VERSION, emptyToBlank(agentVersion)).makeCurrent();
    }

    static String conversationIdFromContext() {
        String value = Context.current().get(CONVERSATION_ID);
        return value == null ? "" : value;
    }

    static String conversationTitleFromContext() {
        String value = Context.current().get(CONVERSATION_TITLE);
        return value == null ? "" : value;
    }

    static String userIdFromContext() {
        String value = Context.current().get(USER_ID);
        return value == null ? "" : value;
    }

    static String agentNameFromContext() {
        String value = Context.current().get(AGENT_NAME);
        return value == null ? "" : value;
    }

    static String agentVersionFromContext() {
        String value = Context.current().get(AGENT_VERSION);
        return value == null ? "" : value;
    }

    static Scope withContentCaptureMode(ContentCaptureMode mode) {
        return Context.current().with(CONTENT_CAPTURE_MODE, mode).makeCurrent();
    }

    static ContentCaptureMode contentCaptureModeFromContext() {
        return Context.current().get(CONTENT_CAPTURE_MODE);
    }

    private static String emptyToBlank(String value) {
        return value == null ? "" : value;
    }
}
