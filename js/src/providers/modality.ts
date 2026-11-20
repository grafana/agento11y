import type { ModalityTokenCounts, TokenUsage } from '../types.js';

type Data = Record<string, unknown>;
const obj = (v: unknown): Data => (v !== null && typeof v === 'object' ? (v as Data) : {});
const number = (v: unknown): number => (typeof v === 'number' ? v : 0);

export function modalityPartition(
  raw: unknown,
  total: number,
  thinking = 0,
  openai = false,
): ModalityTokenCounts | undefined {
  if (raw == null) {
    return undefined;
  }
  const tokens: Record<string, number> = {};
  if (openai) {
    for (const m of ['text', 'image', 'audio', 'video']) {
      const n = obj(raw)[`${m}_tokens`];
      if (typeof n === 'number') {
        tokens[m] = n;
      }
    }
  } else if (Array.isArray(raw)) {
    for (const row of raw) {
      const d = obj(row),
        m = String(d.modality ?? 'unknown').toLowerCase();
      tokens[m] = (tokens[m] ?? 0) + number(d.tokenCount ?? d.tokens);
    }
  }
  if (thinking) {
    tokens.text = (tokens.text ?? 0) + thinking;
  }
  return { tokens, complete: Object.values(tokens).reduce((a, b) => a + b, 0) === total };
}

export function imageUsage(raw: unknown): TokenUsage {
  const d = obj(raw);
  const input = number(d.input_tokens),
    output = number(d.output_tokens),
    cached = number(obj(d.input_tokens_details).cached_tokens);
  return {
    inputTokens: input,
    outputTokens: output,
    totalTokens: number(d.total_tokens) || input + output,
    inputSemantics: 'inclusive',
    cacheReadInputTokens: cached,
    inputByModality: modalityPartition(d.input_tokens_details, input, 0, true),
    outputByModality: modalityPartition(d.output_tokens_details, output, 0, true),
    cacheReadByModality: modalityPartition(obj(d.input_tokens_details).cached_tokens_details, cached, 0, true),
  };
}

export function interactionsUsage(raw: unknown): TokenUsage {
  const d = obj(raw),
    thinking = number(d.total_thought_tokens),
    tool = number(d.total_tool_use_tokens);
  const input = number(d.total_input_tokens),
    output = number(d.total_output_tokens) + thinking,
    cached = number(d.total_cached_tokens);
  const inputByModality = modalityPartition(d.input_tokens_by_modality, input);
  const u: TokenUsage = {
    inputTokens: input + tool,
    outputTokens: output,
    reasoningTokens: thinking,
    totalTokens: number(d.total_tokens) || input + tool + output,
    inputSemantics: 'inclusive',
    cacheReadInputTokens: cached,
    inputByModality,
    outputByModality: modalityPartition(d.output_tokens_by_modality, output, thinking),
    cacheReadByModality: modalityPartition(d.cached_tokens_by_modality, cached),
  };
  if (tool) {
    u.inputByModality ??= { tokens: {}, complete: false };
    u.inputByModality.tokens.tool_use = tool;
  }
  return u;
}
