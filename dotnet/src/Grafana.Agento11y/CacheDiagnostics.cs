namespace Grafana.Sigil;

/// <summary>Reserved generation metadata keys for cache diagnostics (Anthropic cache-diagnosis beta).</summary>
public static class CacheDiagnostics
{
    public const string MissReasonKey = "sigil.cache_diagnostics.miss_reason";
    public const string MissedInputTokensKey = "sigil.cache_diagnostics.missed_input_tokens";
    public const string PreviousMessageIdKey = "sigil.cache_diagnostics.previous_message_id";

    /// <summary>Stamps <c>sigil.cache_diagnostics.*</c> metadata. Call before <see cref="GenerationRecorder.End"/>.</summary>
    public static void SetCacheDiagnostics(
        GenerationRecorder? recorder,
        string missReason,
        long? missedInputTokens = null,
        string? previousMessageId = null
    ) => recorder?.SetCacheDiagnostics(missReason, missedInputTokens, previousMessageId);
}
