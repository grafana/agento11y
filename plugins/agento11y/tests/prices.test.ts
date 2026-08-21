import { describe, expect, it } from 'vitest';
import { canonicalizePriceModel, conversationCost, liveModelCost } from '../internal/local/web/src/formatters';
import type { ModelPrices, TokenBuckets } from '../internal/local/web/src/types';

// Cursor records a Grok turn under an id of its own, with the reasoning effort
// and the speed tier appended. models.dev prices the xAI model, so the id has
// to be canonicalized before the lookup or the turn prices to nothing.

const prices: ModelPrices = { 'grok-4.6': { input: 2, output: 6, cache_read: 0.5 } };

const buckets: TokenBuckets = { fresh_input: 1e6, output: 0, cache_read: 0, cache_write: 0, reasoning: 0 };

describe('canonicalizePriceModel', () => {
  it('strips the effort and speed suffixes off a Cursor Grok id', () => {
    expect(canonicalizePriceModel('cursor-grok-4.6-high-fast')).toBe('grok-4.6');
    expect(canonicalizePriceModel('cursor-grok-4.6-xhigh-fast')).toBe('grok-4.6');
    expect(canonicalizePriceModel('cursor-grok-4.6-xhigh')).toBe('grok-4.6');
    expect(canonicalizePriceModel('cursor-grok-4.5-medium')).toBe('grok-4.5');
  });

  it('leaves every other id alone', () => {
    expect(canonicalizePriceModel('grok-4.6')).toBe('grok-4.6');
    expect(canonicalizePriceModel('claude-opus-4-8')).toBe('claude-opus-4-8');
    expect(canonicalizePriceModel('composer-2.5-fast')).toBe('composer-2.5-fast');
  });
});

describe('liveModelCost', () => {
  it('falls back to the canonical id when the recorded one is not in the catalog', () => {
    expect(liveModelCost(prices, 'grok-4.6')).toEqual(prices['grok-4.6']);
    expect(liveModelCost(prices, 'cursor-grok-4.6-high-fast')).toEqual(prices['grok-4.6']);
    expect(liveModelCost(prices, 'cursor-grok-4.6-xhigh-fast')).toEqual(prices['grok-4.6']);
  });

  it('returns null when the canonical id is not priced either', () => {
    expect(liveModelCost(prices, 'cursor-grok-4.5-high-fast')).toBeNull();
  });
});

describe('conversationCost', () => {
  it('prices a Cursor Grok conversation at the xAI rate', () => {
    expect(conversationCost({ models: ['grok-4.6'], token_buckets: buckets }, prices)).toBe(2);
    expect(conversationCost({ models: ['cursor-grok-4.6-high-fast'], token_buckets: buckets }, prices)).toBe(2);
  });

  it('reports no cost when the catalog is empty', () => {
    expect(conversationCost({ models: ['cursor-grok-4.6-high-fast'], token_buckets: buckets }, {})).toBeNull();
  });
});
