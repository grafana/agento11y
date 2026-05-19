package com.grafana.sigil.sdk;

/** Reserved generation metadata keys for cache diagnostics (Anthropic cache-diagnosis beta). */
public final class CacheDiagnostics {
    public static final String MISS_REASON_KEY = "sigil.cache_diagnostics.miss_reason";
    public static final String MISSED_INPUT_TOKENS_KEY = "sigil.cache_diagnostics.missed_input_tokens";
    public static final String PREVIOUS_MESSAGE_ID_KEY = "sigil.cache_diagnostics.previous_message_id";

    private CacheDiagnostics() {}

    /**
     * Stamps {@code sigil.cache_diagnostics.*} metadata on a recorder. Call before {@link GenerationRecorder#end()}.
     *
     * @param missedInputTokens pass {@code null} to omit; otherwise written as a decimal string
     * @param previousMessageId pass {@code null} or blank to omit
     */
    public static void setCacheDiagnostics(
            GenerationRecorder recorder,
            String missReason,
            Long missedInputTokens,
            String previousMessageId) {
        if (recorder == null) {
            return;
        }
        recorder.setCacheDiagnostics(missReason, missedInputTokens, previousMessageId);
    }
}
