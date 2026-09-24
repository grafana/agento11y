import assert from 'node:assert/strict';
import test from 'node:test';
import { imageUsage, interactionsUsage } from '../.test-dist/providers/modality.js';

test('image usage preserves modality totals without inventing cached details', () => {
  const usage = imageUsage({
    input_tokens: 100,
    output_tokens: 10,
    input_tokens_details: { text_tokens: 40, image_tokens: 60, cached_tokens: 20 },
    output_tokens_details: { image_tokens: 10 },
  });
  assert.deepEqual(usage.inputByModality, { tokens: { text: 40, image: 60 }, complete: true });
  assert.equal(usage.cacheReadByModality, undefined);
  assert.equal(usage.outputByModality.complete, true);
});
test('interaction final usage adds thinking once and retains unknown tool usage', () => {
  const usage = interactionsUsage({
    total_input_tokens: 100,
    total_output_tokens: 10,
    total_thought_tokens: 2,
    total_tool_use_tokens: 3,
    output_tokens_by_modality: [{ modality: 'image', tokens: 10 }],
  });
  assert.equal(usage.outputTokens, 12);
  assert.deepEqual(usage.outputByModality, { tokens: { image: 10, text: 2 }, complete: true });
  assert.equal(usage.inputByModality.tokens.tool_use, 3);
});
