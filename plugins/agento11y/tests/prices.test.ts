import { describe, expect, it } from 'vitest';
import {
  canonicalizePriceModel,
  conversationCost,
  conversationCostByModel,
  conversationCostEstimateByModel,
  liveModelCost,
  tokenPointCost,
} from '../internal/local/web/src/formatters';
import type { ModelPrices, TokenBuckets, TokenUsagePoint } from '../internal/local/web/src/types';

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

describe('per-model conversation cost', () => {
  it('prices each model bucket at that model rate', () => {
    const mixedPrices: ModelPrices = {
      'model-a': { input: 2, output: 4 },
      'model-b': { input: 3, output: 9 },
    };
    const empty: TokenBuckets = { fresh_input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 };
    expect(
      conversationCostByModel(
        {
          models: ['model-a', 'model-b'],
          token_buckets: { ...empty, fresh_input: 3e6 },
          token_buckets_by_model: {
            'model-a': { ...empty, fresh_input: 1e6 },
            'model-b': { ...empty, fresh_input: 2e6 },
          },
        },
        mixedPrices,
      ),
    ).toBe(8);
  });

  it('differs from whole-conversation pricing when the cheaper model sorts first', () => {
    const mixedPrices: ModelPrices = {
      'a-cheap': { input: 1, output: 1 },
      'z-pricey': { input: 100, output: 100 },
    };
    const empty: TokenBuckets = { fresh_input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 };
    const conversation = {
      models: ['a-cheap', 'z-pricey'],
      token_buckets: { ...empty, fresh_input: 2e6 },
      token_buckets_by_model: {
        'a-cheap': { ...empty, fresh_input: 1e6 },
        'z-pricey': { ...empty, fresh_input: 1e6 },
      },
    };
    expect(conversationCost(conversation, mixedPrices)).toBe(2);
    expect(conversationCostByModel(conversation, mixedPrices)).toBe(101);
  });

  it('falls back to the conversation model for an older daemon', () => {
    const conversation = { models: ['grok-4.6'], token_buckets: buckets };
    expect(conversationCostByModel(conversation, prices)).toBe(conversationCost(conversation, prices));
  });

  it('marks a mixed priced and unpriced subtotal as incomplete', () => {
    expect(
      conversationCostEstimateByModel(
        {
          token_buckets: buckets,
          token_buckets_by_model: {
            'grok-4.6': buckets,
            unknown: buckets,
          },
        },
        prices,
      ),
    ).toEqual({ value: 2, complete: false });
  });

  it.each([
    ['an unknown model', { unknown: buckets }],
    ['unlabeled usage', { '': buckets }],
  ])('reports an unknown estimate for %s', (_name, token_buckets_by_model) => {
    expect(conversationCostEstimateByModel({ token_buckets: buckets, token_buckets_by_model }, prices)).toEqual({
      value: null,
      complete: false,
    });
  });

  it('keeps zero usage compatible across daemon schemas', () => {
    const zero = { ...buckets, fresh_input: 0 };
    const legacy = { models: ['grok-4.6'], token_buckets: zero };
    const split = {
      ...legacy,
      token_buckets_by_model: { 'grok-4.6': zero },
    };
    expect(conversationCostEstimateByModel(legacy, prices)).toEqual({ value: 0, complete: true });
    expect(conversationCostEstimateByModel(split, prices)).toEqual({ value: 0, complete: true });
  });

  it('rejects a positive aggregate whose model buckets are empty', () => {
    expect(
      conversationCostEstimateByModel(
        {
          models: ['grok-4.6'],
          token_buckets: buckets,
          token_buckets_by_model: { 'grok-4.6': { ...buckets, fresh_input: 0 } },
        },
        prices,
      ),
    ).toEqual({ value: null, complete: false });
  });

  it('keeps a zero-priced model complete', () => {
    expect(
      conversationCostEstimateByModel(
        {
          token_buckets: buckets,
          token_buckets_by_model: { free: buckets },
        },
        { free: { input: 0, output: 0 } },
      ),
    ).toEqual({ value: 0, complete: true });
  });
});

describe('tokenPointCost', () => {
  const point: TokenUsagePoint = { t: '2026-08-20T12:00:00Z', model: 'grok-4.6', calls: 1, ...buckets };

  it('prices the point model', () => {
    expect(tokenPointCost(point, prices)).toBe(2);
  });

  it('reports no cost when the point has no model', () => {
    const { model: _, ...withoutModel } = point;
    expect(tokenPointCost(withoutModel, prices)).toBeNull();
  });
});
