import { useEffect, useState } from 'react';
import type { ModelCost, ModelPrices, TokenBucketKey, TokenBuckets, TokenUsagePoint } from './types';

// ============================================================
// Formatters — all server responses ship raw numbers + RFC3339
// timestamps, the UI humanizes them so it can re-render relative
// labels without re-fetching.
// ============================================================

// NO_VALUE is what every formatter returns for a value the source did not
// record. One constant so a missing token count, duration, date, and cost
// all read the same in a table.
export const NO_VALUE = '-';

export function formatInteger(value: number): string {
  return Math.round(value).toLocaleString('en-US');
}

export function formatTokens(n: number | null | undefined): string {
  if (n == null || Number.isNaN(Number(n))) return NO_VALUE;
  if (n < 1000) return String(n);
  if (n < 1_000_000) return `${(n / 1_000).toFixed(n < 10_000 ? 1 : 1).replace(/\.0$/, '')}k`;
  return `${(n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 1).replace(/\.0$/, '')}M`;
}

export function formatDuration(seconds: number | null | undefined): string {
  if (seconds == null || Number.isNaN(Number(seconds))) return NO_VALUE;
  if (seconds < 1) return '<1s';
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 2 : 1).replace(/\.0+$/, '')}s`;
  const m = Math.floor(seconds / 60);
  const s = Math.round(seconds % 60);
  if (m < 60) return s === 0 ? `${m}m` : `${m}m ${s}s`;
  const h = Math.floor(m / 60);
  const mm = m % 60;
  return mm === 0 ? `${h}h` : `${h}h ${mm}m`;
}

// formatAgo returns a complete relative-time phrase including the
// "ago" suffix where appropriate, so call sites can use it bare
// without adding their own "ago" and producing "just now ago".
export function formatAgo(iso: string | null | undefined, now: number): string {
  if (!iso) return NO_VALUE;
  const t = new Date(iso).getTime();
  if (!Number.isFinite(t)) return NO_VALUE;
  const secs = Math.max(0, Math.round((now - t) / 1000));
  if (secs < 5) return 'just now';
  if (secs < 60) return `${secs}s ago`;
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.round(hours / 24);
  return `${days}d ago`;
}

export function formatTime(iso: string | null | undefined): string {
  if (!iso) return NO_VALUE;
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return NO_VALUE;
  return d.toLocaleTimeString([], { hour12: false });
}

export function durationBetweenSeconds(
  startISO: string | null | undefined,
  endISO: string | null | undefined,
): number | null {
  if (!startISO || !endISO) return null;
  const s = new Date(startISO).getTime();
  const e = new Date(endISO).getTime();
  if (!Number.isFinite(s) || !Number.isFinite(e) || e < s) return null;
  return (e - s) / 1000;
}

// A selectable window. `ms` is null for "All", which has no bound.
export interface TimeRangeOption {
  value: string;
  label: string;
  ms: number | null;
}

export const TIME_RANGES: TimeRangeOption[] = [
  { value: '5m', label: 'Last 5 minutes', ms: 5 * 60 * 1000 },
  { value: '15m', label: 'Last 15 minutes', ms: 15 * 60 * 1000 },
  { value: '1h', label: 'Last 1 hour', ms: 60 * 60 * 1000 },
  { value: '6h', label: 'Last 6 hours', ms: 6 * 60 * 60 * 1000 },
  { value: '24h', label: 'Last 24 hours', ms: 24 * 60 * 60 * 1000 },
  { value: '7d', label: 'Last 7 days', ms: 7 * 24 * 60 * 60 * 1000 },
  { value: '90d', label: 'Last 90 days', ms: 90 * 24 * 60 * 60 * 1000 },
  { value: 'all', label: 'All', ms: null },
];

// DEFAULT_TIME_RANGE matches the 90-day window a history import defaults
// to. A narrower default would open on an empty list right after an
// import, because everything backfilled is older than it.
export const DEFAULT_TIME_RANGE = '90d';

// LIST_PAGE_SIZE is how many conversations one list request asks for.
// The rows are not virtualised and the server decodes only what it
// returns, so this bounds both ends. It matches the server's own
// default (conversationListLimit in server.go).
export const LIST_PAGE_SIZE = 200;

export function timeRangeOption(value: string): TimeRangeOption {
  return (
    TIME_RANGES.find((r) => r.value === value) ||
    // The DEFAULT_TIME_RANGE row is always in the table.
    (TIME_RANGES.find((r) => r.value === DEFAULT_TIME_RANGE) as TimeRangeOption)
  );
}

// The timestamps conversationTime reads; a row derived from generations may
// carry only one of them.
interface ConversationTimes {
  last_activity?: string | null;
  started_at?: string | null;
}

export function conversationTime(c: ConversationTimes): number | null {
  const t = new Date(c.last_activity || c.started_at || '').getTime();
  return Number.isFinite(t) ? t : null;
}

function formatBucketSize(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return 'buckets';
  if (ms < 60000) return `${Math.round(ms / 1000)}-sec buckets`;
  const mins = Math.round(ms / 60000);
  if (mins < 60) return `${mins}-min buckets`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours}-hour buckets`;
  const days = Math.round(hours / 24);
  return `${days}-day buckets`;
}

export function formatBucketLabel(ts: number, bucketMs: number): string {
  const d = new Date(ts);
  if (bucketMs >= 24 * 60 * 60 * 1000) {
    return d.toLocaleDateString([], { month: 'short', day: 'numeric' });
  }
  const time = d.toLocaleTimeString([], {
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  });
  // 2h+ buckets mean the chart spans more than a day, so a bare
  // time is ambiguous — prefix the date.
  if (bucketMs >= 2 * 60 * 60 * 1000) {
    return `${d.toLocaleDateString([], { month: 'short', day: 'numeric' })} ${time}`;
  }
  // Sub-minute buckets need seconds or adjacent labels collide.
  if (bucketMs < 60 * 1000) {
    return d.toLocaleTimeString([], {
      hour: '2-digit',
      minute: '2-digit',
      second: '2-digit',
      hour12: false,
    });
  }
  return time;
}

// chartTooltipLeft centers the hover tooltip on its bar but keeps it
// clear of the card edges so the first and last buckets don't clip.
export function chartTooltipLeft(i: number, n: number): string {
  return `${Math.min(88, Math.max(12, ((i + 0.5) / n) * 100))}%`;
}

// Per-model dot colour. New models fall back to a neutral grey
// pulled from the Saga viz palette.
const MODEL_COLORS: Record<string, string> = {
  'claude-opus-4-7': 'var(--model-opus)',
  'claude-opus-4-1': 'var(--model-opus)',
  'claude-sonnet-4': 'var(--model-sonnet)',
  'deepseek-v4-pro': 'var(--model-deepseek)',
  'gpt-5-omni': 'var(--model-gpt)',
};
export function modelDot(name: string | null | undefined): string {
  if (!name) return 'var(--model-fallback)';
  return MODEL_COLORS[name] || 'var(--model-fallback)';
}

// shortModel trims the vendor prefix and dated snapshot suffix so a list
// pill reads "opus-4-8" / "haiku-4-5" instead of the full
// "claude-haiku-4-5-20251001". The dot still colours by the full id.
export function shortModel(name: string | null | undefined): string {
  return (name || '').replace(/^claude-/, '').replace(/-\d{8}$/, '');
}

// One stacked series of the token chart, keyed by its disjoint bucket.
interface TokenSeries {
  key: TokenBucketKey;
  label: string;
  color: string;
}

// Token-usage chart series. The server splits each generation into
// these five non-overlapping buckets (provider-aware, see
// disjointTokenUsage in query.go), so stacking them never
// double-counts. Order is bottom-to-top in the stack.
export const TOKEN_SERIES: TokenSeries[] = [
  { key: 'fresh_input', label: 'Input', color: 'var(--viz-blue)' },
  { key: 'cache_read', label: 'Cache read', color: 'var(--viz-green)' },
  { key: 'cache_write', label: 'Cache write', color: 'var(--viz-purple)' },
  { key: 'output', label: 'Output', color: 'var(--viz-orange)' },
  { key: 'reasoning', label: 'Reasoning', color: 'var(--viz-yellow)' },
];

// tokenBreakdownTitle renders disjoint token buckets as a multi-line
// native tooltip for the list's Tokens cell.
export function tokenBreakdownTitle(buckets: TokenBuckets | null | undefined): string | undefined {
  if (!buckets) return undefined;
  const lines = TOKEN_SERIES.filter((s) => buckets[s.key] > 0).map(
    (s) => `${s.label}: ${formatTokens(buckets[s.key])}`,
  );
  return lines.length ? lines.join('\n') : undefined;
}

// Prompt-input cache hit rate: cache reads divided by all prompt-input
// cache outcomes. Cache writes are misses that populated future cache
// entries, so they belong in the denominator but never the numerator.
export function cacheInputHitPercent(
  freshInput: number | null | undefined,
  cacheRead: number | null | undefined,
  cacheWrite: number | null | undefined,
): number | null {
  const fresh = Math.max(0, Number(freshInput) || 0);
  const read = Math.max(0, Number(cacheRead) || 0);
  const write = Math.max(0, Number(cacheWrite) || 0);
  const denom = fresh + read + write;
  if (denom === 0) return null;
  if (read === denom) return 100;
  return Math.min(99, Math.round((read / denom) * 100));
}

// A row of the bundled Anthropic table: USD per million tokens, matched by
// substring against the model id.
interface BundledModelPrice {
  match: string;
  in: number;
  out: number;
}

// Per-model price in USD per million tokens (input / output). Cache reads
// bill at ~0.1x input and 5-minute cache writes at ~1.25x input — the
// published Anthropic multipliers. Matched by substring so the bare model
// id (claude-opus-4-8) resolves without an exact-version table.
//
// Anthropic only — agento11y also captures OpenAI / Gemini / etc. sessions,
// and we don't carry authoritative prices for those (they drift). An
// unrecognised model returns null rather than a fabricated dollar figure:
// better to show NO_VALUE than to price a GPT/Gemini run at Claude rates.
// ponytail: add a row here when a provider's prices are known and stable;
// don't guess them.
const MODEL_PRICES: BundledModelPrice[] = [
  { match: 'fable', in: 10, out: 50 },
  { match: 'opus', in: 5, out: 25 },
  { match: 'sonnet', in: 3, out: 15 },
  { match: 'haiku', in: 1, out: 5 },
];

function modelPrice(model: string | null | undefined): BundledModelPrice | null {
  const m = (model || '').toLowerCase();
  return MODEL_PRICES.find((p) => m.includes(p.match)) || null;
}

export function maximumEstimatedTokenRate(prices: ModelPrices | null | undefined): number {
  let maximum = Math.max(...MODEL_PRICES.flatMap((price) => [price.in, price.out, price.in * 1.25]));
  for (const price of Object.values(prices || {})) {
    const input = price.input;
    if (input == null || !Number.isFinite(input)) continue;
    for (const rate of [
      input,
      price.output ?? input,
      price.cache_read ?? input * 0.1,
      price.cache_write ?? input * 1.25,
    ]) {
      if (Number.isFinite(rate)) maximum = Math.max(maximum, rate);
    }
  }
  return maximum;
}

// Cursor hosted Grok SKUs are `cursor-grok-4.6-high-fast`. models.dev (and
// the Cloud catalog) key the same model as `grok-4.6`. Keep this suffix
// list in sync with canonicalizeCursorModel in the cursor mapper.
const CURSOR_GROK_EFFORT_SUFFIXES: string[] = [
  '-xhigh-fast',
  '-extra-high-fast',
  '-high-fast',
  '-low-fast',
  '-medium-fast',
  '-extra-high',
  '-xhigh',
  '-high',
  '-medium',
  '-low',
];

export function canonicalizePriceModel(model: string | null | undefined): string {
  const trimmed = (model || '').trim();
  if (!trimmed) return trimmed;
  const lower = trimmed.toLowerCase();
  const prefix = 'cursor-';
  if (!lower.startsWith(prefix)) return trimmed;
  const rest = trimmed.slice(prefix.length);
  const restLower = lower.slice(prefix.length);
  if (!restLower.includes('grok')) return trimmed;
  for (const suffix of CURSOR_GROK_EFFORT_SUFFIXES) {
    if (restLower.endsWith(suffix)) {
      return rest.slice(0, rest.length - suffix.length);
    }
  }
  return rest;
}

export function liveModelCost(
  prices: ModelPrices | null | undefined,
  model: string | null | undefined,
): ModelCost | null {
  if (!prices || !model) return null;
  const exact = prices[model];
  if (exact) return exact;
  const canonical = canonicalizePriceModel(model);
  const fallback = prices[canonical];
  if (canonical !== model && fallback) return fallback;
  return null;
}

// models.dev is the authoritative, multi-provider price catalog (OpenAI,
// Anthropic, Gemini, …) — strictly better than the bundled table, and it
// carries explicit per-model cache_read / cache_write rates instead of the
// 0.1x / 1.25x assumption. We fetch it once, flatten to
// { modelId: {input, output, cache_read, cache_write} } in USD/MTok, and
// cache it in localStorage for a day. Offline or unknown id → the bundled
// Anthropic table (modelPrice) → null.
const MODELS_DEV_URL = 'https://models.dev/api.json';
const PRICE_CACHE_KEY = 'sigil.modelPrices.v1';
const PRICE_TTL_MS = 24 * 60 * 60 * 1000;
let modelPricesPromise: Promise<ModelPrices> | null = null;

/** The models.dev api.json shape, down to the fields this file reads. */
interface ModelsDevProvider {
  models?: Record<string, { cost?: ModelCost } | null>;
}

function flattenModelsDev(data: Record<string, ModelsDevProvider | null> | null | undefined): ModelPrices {
  const map: ModelPrices = {};
  for (const provider of Object.values(data || {})) {
    const models = provider?.models;
    if (!models) continue;
    for (const [id, m] of Object.entries(models)) {
      if (m && m.cost && m.cost.input != null) map[id] = m.cost;
    }
  }
  return map;
}

/** The price map as it is cached in localStorage, with its fetch time. */
interface CachedModelPrices {
  at: number;
  map: ModelPrices;
}

function loadModelPrices(): Promise<ModelPrices> {
  if (modelPricesPromise) return modelPricesPromise;
  modelPricesPromise = (async () => {
    try {
      const cached: CachedModelPrices | null = JSON.parse(localStorage.getItem(PRICE_CACHE_KEY) || 'null');
      if (cached?.map && Date.now() - cached.at < PRICE_TTL_MS) return cached.map;
    } catch {
      /* corrupt cache, refetch */
    }
    const resp = await fetch(MODELS_DEV_URL);
    if (!resp.ok) throw new Error(`models.dev ${resp.status}`);
    const map = flattenModelsDev(await resp.json());
    try {
      localStorage.setItem(PRICE_CACHE_KEY, JSON.stringify({ at: Date.now(), map }));
    } catch {
      /* quota, skip cache */
    }
    return map;
  })();
  return modelPricesPromise;
}

// Returns null until prices load. Disabling the hook stops loading but keeps
// prices already loaded. Callers must use the bundled table while null.
export function useModelPrices(enabled = true): ModelPrices | null {
  const [prices, setPrices] = useState<ModelPrices | null>(null);
  useEffect(() => {
    if (!enabled) return;
    let alive = true;
    loadModelPrices()
      .then((m) => {
        if (alive) setPrices(m);
      })
      .catch(() => {});
    return () => {
      alive = false;
    };
  }, [enabled]);
  return prices;
}

// What conversationCost needs off a conversation row.
interface CostableConversation {
  token_buckets?: TokenBuckets | null;
  token_buckets_by_model?: Record<string, TokenBuckets> | null;
  models?: string[] | null;
}

// conversationCost prices all token buckets at models[0]'s rate. The server
// sorts model IDs alphabetically, so mixed-model rows select by ID rather than
// orchestration role or usage. It checks the multi-provider models.dev map by
// exact or supported canonicalized ID, then the bundled Anthropic-family
// substring table when the catalog is unavailable or has no match. It returns
// null when no model is recorded or neither source has a rate. Callers show
// NO_VALUE instead of guessing.
//
// Use conversationCostByModel for token_buckets_by_model. It prices each
// nonzero model bucket separately and falls back to conversationCost when the
// per-model map is absent.
export function conversationCost(
  c: CostableConversation | null | undefined,
  prices: ModelPrices | null,
): number | null {
  const b = c?.token_buckets;
  if (!b) return null;
  const model = (c.models || [])[0];
  let inRate: number, outRate: number, cacheReadRate: number, cacheWriteRate: number;
  const live = liveModelCost(prices, model);
  if (live) {
    // A catalog entry with no input rate prices to NaN.
    inRate = live.input ?? Number.NaN;
    outRate = live.output != null ? live.output : (live.input ?? Number.NaN);
    cacheReadRate = live.cache_read != null ? live.cache_read : (live.input ?? Number.NaN) * 0.1;
    cacheWriteRate = live.cache_write != null ? live.cache_write : (live.input ?? Number.NaN) * 1.25;
  } else {
    const p = modelPrice(model);
    if (!p) return null;
    inRate = p.in;
    outRate = p.out;
    cacheReadRate = p.in * 0.1;
    cacheWriteRate = p.in * 1.25;
  }
  return (
    ((b.fresh_input || 0) * inRate +
      (b.cache_read || 0) * cacheReadRate +
      (b.cache_write || 0) * cacheWriteRate +
      ((b.output || 0) + (b.reasoning || 0)) * outRate) /
    1e6
  );
}

export interface CostEstimate {
  value: number | null;
  complete: boolean;
}

export function conversationCostEstimateByModel(
  c: CostableConversation | null | undefined,
  prices: ModelPrices | null,
): CostEstimate {
  const byModel = c?.token_buckets_by_model;
  if (!byModel) {
    const value = conversationCost(c, prices);
    return { value, complete: value != null };
  }
  let sum = 0;
  let priced = 0;
  let considered = 0;
  let complete = true;
  for (const [model, token_buckets] of Object.entries(byModel)) {
    if (!Object.values(token_buckets).some((value) => value > 0)) continue;
    considered++;
    const cost = conversationCost({ token_buckets, models: model ? [model] : [] }, prices);
    if (cost == null) {
      complete = false;
      continue;
    }
    sum += cost;
    priced++;
  }
  if (considered === 0) {
    const aggregateHasUsage = c?.token_buckets && Object.values(c.token_buckets).some((value) => value > 0);
    if (aggregateHasUsage) return { value: null, complete: false };
    const fallback = conversationCost(c, prices);
    if (fallback != null) return { value: fallback, complete: true };
    for (const model of Object.keys(byModel)) {
      if (!model) continue;
      const zero = conversationCost({ token_buckets: byModel[model], models: [model] }, prices);
      if (zero != null) return { value: 0, complete: true };
    }
    return { value: null, complete: false };
  }
  return { value: priced > 0 ? sum : null, complete: complete && priced === considered };
}

export function conversationCostByModel(
  c: CostableConversation | null | undefined,
  prices: ModelPrices | null,
): number | null {
  return conversationCostEstimateByModel(c, prices).value;
}

export function tokenPointCost(point: TokenUsagePoint, prices: ModelPrices | null): number | null {
  if (!point.model) return null;
  return conversationCost({ token_buckets: point, models: [point.model] }, prices);
}

export function formatCost(usd: number | null | undefined): string {
  if (usd == null) return NO_VALUE; // unpriced model, distinct from $0
  if (usd === 0) return '$0';
  if (usd < 0.01) return '<$0.01';
  if (usd < 1000) return `$${usd.toFixed(2).replace(/\.00$/, '')}`;
  return `$${(usd / 1000).toFixed(1)}k`;
}

// List-price token math, not the provider invoice: subscriptions and
// committed-use discounts never reach this table.
export const ESTIMATED_COST_TOOLTIP =
  'Estimated from token usage at published model rates. Does not include provider subscription discounts or committed-use pricing, so this can differ from the actual bill.';

export function workspaceLabel(path: string | null | undefined): string {
  if (!path) return '(unknown)';
  const parts = path.replace(/\/+$/, '').split('/').filter(Boolean);
  return parts.slice(-2).join('/') || path;
}

export function splitWorkspacePath(path: string | null | undefined): { dir: string; leaf: string } {
  if (!path) return { dir: '', leaf: '(unknown)' };
  const clean = path.replace(/\/+$/, '');
  if (!clean) return { dir: '', leaf: '/' };
  const cut = clean.lastIndexOf('/');
  if (cut < 0) return { dir: '', leaf: clean };
  return {
    dir: clean.slice(0, cut + 1),
    leaf: clean.slice(cut + 1) || clean,
  };
}

/** A half-open [start, end] window in epoch milliseconds. */
export interface TimeSpan {
  start: number;
  end: number;
}

// timeWindow computes a chart's [start, end] for a range selection.
// For "All", min/max accumulate in a loop instead of spreading into
// Math.min/Math.max: with one entry per generation the times array
// can be large enough that spread overflows the argument stack
// (RangeError).
function timeWindow(times: ReadonlyArray<number | null>, rangeValue: string, now: number): TimeSpan {
  const range = timeRangeOption(rangeValue);
  if (range.ms != null) return { start: now - range.ms, end: now };
  let minT = Infinity,
    maxT = -Infinity,
    n = 0;
  for (const t of times) {
    if (t == null || !Number.isFinite(t)) continue;
    n++;
    if (t < minT) minT = t;
    if (t > maxT) maxT = t;
  }
  const end = n ? Math.max(now, maxT) : now;
  const start = n ? minT : end - 60 * 60 * 1000;
  return { start, end };
}

// BUCKET_INTERVALS_MS is the ladder the chart and the token endpoint
// share. Every step divides the next, so a point the server aggregated
// on one step always falls inside a single bar of a coarser step.
// This list and tokenUsageIntervals in query.go must stay equal;
// TestBucketLaddersAgree checks that.
const BUCKET_INTERVALS_MS = [
  10_000,
  30_000,
  60_000,
  5 * 60_000,
  15 * 60_000,
  30 * 60_000,
  60 * 60_000,
  2 * 60 * 60_000,
  4 * 60 * 60_000,
  12 * 60 * 60_000,
  24 * 60 * 60_000,
  7 * 24 * 60 * 60_000,
];
// CHART_BUCKET_MAX caps how many bars a chart draws. The finest ladder
// step that stays under it is also what the token endpoint aggregates
// on, so the response holds one point per bar per model instead of one
// per generation. Every fixed range gives 10 to 15 bars.
const CHART_BUCKET_MAX = 16;

// chartBucketMs picks the bar width for a span. Past the top of the
// ladder it widens the last step by a whole multiple, so a decade of
// imported history draws CHART_BUCKET_MAX bars rather than one per week,
// and a server bucket still divides a bar.
export function chartBucketMs(spanMs: number, minMs = 0): number {
  const span = Number.isFinite(spanMs) && spanMs > 0 ? spanMs : 60_000;
  const floor = Number.isFinite(minMs) && minMs > 0 ? minMs : 0;
  for (const step of BUCKET_INTERVALS_MS) {
    if (step >= floor && span / step <= CHART_BUCKET_MAX) return step;
  }
  // Divide by one bar fewer than the cap, because chartGrid snaps the
  // window outwards at both ends and that can need an extra bar.
  const top = Math.max(BUCKET_INTERVALS_MS[BUCKET_INTERVALS_MS.length - 1] ?? 0, floor);
  return top * Math.max(1, Math.ceil(span / (top * (CHART_BUCKET_MAX - 1))));
}

/** A bucket layout: the snapped window, the bar width, and the bar count. */
interface ChartGrid extends TimeSpan {
  bucketMs: number;
  count: number;
}

// chartGrid is the bucket layout both charts share: a window snapped to
// the bucket ladder (measured from the epoch, the way the server floors
// its buckets) plus the bar count that follows from it. serverIntervalMs
// is the width the token endpoint aggregated on; a bar is never finer
// than that, or a server bucket would straddle two bars and leave the
// neighbour reading empty.
export function chartGrid(
  times: ReadonlyArray<number | null>,
  rangeValue: string,
  now: number,
  serverIntervalMs = 0,
): ChartGrid {
  const { start, end } = timeWindow(times, rangeValue, now);
  const bucketMs = chartBucketMs(end - start, serverIntervalMs);
  const gridStart = Math.floor(start / bucketMs) * bucketMs;
  const gridEnd = Math.ceil(Math.max(end, gridStart + bucketMs) / bucketMs) * bucketMs;
  return {
    start: gridStart,
    end: gridEnd,
    bucketMs,
    count: Math.round((gridEnd - gridStart) / bucketMs),
  };
}

/** The query bounds the viewer sends the server. */
interface RequestWindow {
  limit: number;
  since?: string;
  intervalSec?: number;
}

// requestWindow builds the bounds the viewer sends the server: the page
// size and range for the conversation list, and the range and bucket
// interval for the token chart. A fixed range sends a `since` snapped
// to the same grid the chart draws on; "All" sends neither bound and
// lets the server pick an interval it reports back.
export function requestWindow(rangeValue: string, pageSize: number, now = Date.now()): RequestWindow {
  const range = timeRangeOption(rangeValue);
  if (range.ms == null) return { limit: pageSize };
  const bucketMs = chartBucketMs(range.ms);
  const start = Math.floor((now - range.ms) / bucketMs) * bucketMs;
  return {
    limit: pageSize,
    since: new Date(start).toISOString(),
    intervalSec: Math.round(bucketMs / 1000),
  };
}

/** The bounds every bucket carries, before init()'s counters are merged in. */
export interface TimeBucket extends TimeSpan {
  t: string;
}

/** Bucket layout options a caller may share between charts. */
interface BucketWindowOptions {
  count?: number;
  window?: TimeSpan;
}

interface BucketByTimeOptions<I, B> extends BucketWindowOptions {
  init: () => B;
  add: (bucket: TimeBucket & B, item: I) => void;
}

interface BucketByTimeResult<B> {
  buckets: (TimeBucket & B)[];
  bucketLabel: string;
}

// bucketByTime lays out `count` equal buckets across the selected
// range and folds every in-window item into its bucket: init seeds a
// bucket's counters, add(bucket, item) accumulates one item. Pass
// `window` to share one [start, end] between charts.
function bucketByTime<I, B extends object>(
  items: readonly I[],
  getTime: (item: I) => number | null,
  rangeValue: string,
  now: number,
  { count = 12, init, add, window: win }: BucketByTimeOptions<I, B>,
): BucketByTimeResult<B> {
  const times = items.map(getTime);
  const { start, end } = win || timeWindow(times, rangeValue, now);
  const span = Math.max(end - start, 60 * 1000);
  const bucketMs = span / count;
  const buckets: (TimeBucket & B)[] = [];
  for (let i = 0; i < count; i++) {
    const bucketStart = start + i * bucketMs;
    // The last bucket absorbs the end instant, mirroring the clamped
    // index below, so [start, end) tests against bucket bounds agree
    // with where points were counted.
    const bucketEnd = i === count - 1 ? end + 1 : bucketStart + bucketMs;
    buckets.push({
      t: formatBucketLabel(bucketStart, bucketMs),
      start: bucketStart,
      end: bucketEnd,
      ...init(),
    });
  }
  items.forEach((item, i) => {
    const t = times[i] ?? Number.NaN;
    if (!Number.isFinite(t) || t < start || t > end) return;
    const bucket = buckets[Math.min(count - 1, Math.max(0, Math.floor((t - start) / bucketMs)))];
    if (bucket) add(bucket, item);
  });
  return { buckets, bucketLabel: formatBucketSize(bucketMs) };
}

export function tokenPointTime(p: { t: string }): number {
  return new Date(p.t).getTime();
}

/** One token bucket: the five disjoint series plus their sum. */
export interface TokenBucketTotals extends TokenBuckets {
  total: number;
}

interface TokenUsageBuckets extends BucketByTimeResult<TokenBucketTotals> {
  grandTotal: number;
  totals: TokenBuckets;
}

// bucketTokenUsage sums each disjoint token series per bucket. points
// carry an RFC3339 `t` plus the five token fields.
export function bucketTokenUsage(
  points: readonly TokenUsagePoint[],
  rangeValue: string,
  now: number,
  opts: BucketWindowOptions = {},
): TokenUsageBuckets {
  let grandTotal = 0;
  // Seeded by the loop below, one entry per TOKEN_SERIES key.
  const totals = {} as TokenBuckets;
  for (const s of TOKEN_SERIES) totals[s.key] = 0;
  const result = bucketByTime(points, tokenPointTime, rangeValue, now, {
    ...opts,
    init: () => {
      const b = { total: 0 } as TokenBucketTotals;
      for (const s of TOKEN_SERIES) b[s.key] = 0;
      return b;
    },
    add: (b, p) => {
      for (const s of TOKEN_SERIES) {
        const v = p[s.key] || 0;
        b[s.key] += v;
        b.total += v;
        totals[s.key] += v;
        grandTotal += v;
      }
    },
  });
  return { ...result, grandTotal, totals };
}

/** One activity bucket: the conversation count. */
export interface ActivityBucket {
  c: number;
}

export function bucketActivity(
  conversations: readonly ConversationTimes[],
  rangeValue: string,
  now: number,
  opts: BucketWindowOptions = {},
): BucketByTimeResult<ActivityBucket> {
  return bucketByTime(conversations, conversationTime, rangeValue, now, {
    ...opts,
    init: () => ({ c: 0 }),
    add: (b) => {
      b.c += 1;
    },
  });
}
