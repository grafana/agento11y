import type { GenerationRecorder } from './types.js';

/** Reserved generation metadata keys (Sigil docs/guides/cache-diagnostics.md). */
export const CACHE_DIAGNOSTICS_MISS_REASON_KEY = 'sigil.cache_diagnostics.miss_reason';
export const CACHE_DIAGNOSTICS_MISSED_INPUT_TOKENS_KEY = 'sigil.cache_diagnostics.missed_input_tokens';
export const CACHE_DIAGNOSTICS_PREVIOUS_MESSAGE_ID_KEY = 'sigil.cache_diagnostics.previous_message_id';

export type CacheDiagnosticsOptions = {
  missedInputTokens?: number;
  previousMessageId?: string;
};

/**
 * Stamp `sigil.cache_diagnostics.*` metadata on a generation recorder.
 * Call before `end()`, typically after the provider response is available.
 */
export function setCacheDiagnostics(
  rec: GenerationRecorder | null | undefined,
  missReason: string,
  opts?: CacheDiagnosticsOptions,
): void {
  if (rec === null || rec === undefined) {
    return;
  }
  rec.setCacheDiagnostics(missReason, opts);
}
