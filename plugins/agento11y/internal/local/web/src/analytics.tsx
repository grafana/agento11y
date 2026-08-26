import type React from 'react';
import { Fragment, useMemo, useState } from 'react';
import { AnalyticsPage } from './analytics-page';
import { ChartXLabels, ChartYAxis, TimeRangePicker, type WorkspaceAggregate, WorkspaceFacet } from './conversations';
import {
  bucketTokenUsage,
  type CostEstimate,
  cacheInputHitPercent,
  chartGrid,
  chartTooltipLeft,
  conversationCost,
  conversationCostEstimateByModel,
  conversationTime,
  durationBetweenSeconds,
  ESTIMATED_COST_TOOLTIP,
  formatBucketLabel,
  formatCost,
  formatDuration,
  formatTokens,
  modelDot,
  shortModel,
  splitWorkspacePath,
  type TimeSpan,
  TOKEN_SERIES,
  timeRangeOption,
  tokenPointCost,
  tokenPointTime,
  useModelPrices,
} from './formatters';
import { ACTIVE_PILL_BG, Notice, PANEL_BG, SurfaceCard } from './notices';
import { conversationPath, conversationsPath, isPlainLeftClick } from './routing';
import { agentHosts, Icon, iconBtn, ModelPill } from './shell';
import type {
  ConversationMetricsAggregate,
  ConversationSummary,
  ModelPrices,
  TokenBucketKey,
  TokenBuckets,
  TokenUsagePoint,
} from './types';

export type AnalyticsUnit = 'cost' | 'tokens';

export interface AnalyticsViewProps {
  conversations: ConversationSummary[];
  previousConversations: ConversationSummary[];
  aggregate?: ConversationMetricsAggregate | null;
  previousAggregate?: ConversationMetricsAggregate | null;
  facetConversations: ConversationSummary[];
  totalConversations: number | null;
  previousTotalConversations: number | null;
  facetTotalConversations: number | null;
  tokenPoints: TokenUsagePoint[];
  tokenIntervalMs: number;
  heatmapPoints: TokenUsagePoint[];
  loading: boolean;
  tokenLoading: boolean;
  heatmapLoading: boolean;
  error: string | null;
  previousError: string | null;
  facetError: string | null;
  tokenError: string | null;
  heatmapError: string | null;
  unit: AnalyticsUnit;
  onUnitChange: (v: AnalyticsUnit) => void;
  timeRange: string;
  onTimeRangeChange: (v: string) => void;
  workspace: string | null;
  onWorkspaceChange: (v: string | null) => void;
  hiddenSeries: ReadonlySet<TokenBucketKey>;
  onToggleSeries: (k: TokenBucketKey) => void;
  onRefresh: () => void;
  refreshing: boolean;
  onOpenConversation: (c: { id: string }) => void;
  onOpenWorkspace: (path: string) => void;
  onOpenBucket: (span: TimeSpan) => void;
  now?: number;
  prices?: ModelPrices | null;
}

interface ModelAggregate {
  model: string;
  tokens: number;
  cost: number | null;
}

interface ModelUsageSource {
  token_buckets: TokenBuckets;
  token_buckets_by_model?: Record<string, TokenBuckets>;
  models?: string[];
}

interface WorkspaceRow extends WorkspaceAggregate {}

interface SparkValue {
  key: number;
  value: number;
  costStatus?: 'no-usage' | 'unknown' | 'partial' | 'complete';
  tone?: string;
}

interface KpiCardProps {
  label: React.ReactNode;
  value: string;
  delta?: string | null;
  deltaColor?: string;
  bars: SparkValue[];
  color: string;
  peakColor: string;
  sub: React.ReactNode;
}

interface PanelHeaderProps {
  title: string;
  meta?: React.ReactNode;
}

const EMPTY_BUCKETS: TokenBuckets = {
  fresh_input: 0,
  cache_read: 0,
  cache_write: 0,
  output: 0,
  reasoning: 0,
};
const WORKSPACE_GRID = 'minmax(96px, 1fr) minmax(56px, 110px) 52px 52px 56px';
const MODEL_GRID = 'minmax(72px, 1fr) 70px 68px 56px';
const SHAPE_GRID = '82px minmax(0, 1fr) 26px';
const SESSION_GRID = '26px minmax(0, 1fr) 130px 84px 88px 128px 88px';
const DAY_NAMES = ['Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat', 'Sun'] as const;
const HEAT_COLORS = ['var(--heat-0)', 'var(--heat-1)', 'var(--heat-2)', 'var(--heat-3)', 'var(--heat-4)'] as const;

function tokenTotal(buckets: TokenBuckets | null | undefined): number {
  if (!buckets) return 0;
  return TOKEN_SERIES.reduce((sum, series) => sum + (buckets[series.key] || 0), 0);
}

function sumTokens(conversations: readonly ConversationSummary[]): number {
  return conversations.reduce((sum, conversation) => sum + tokenTotal(conversation.token_buckets), 0);
}

function sumCosts(conversations: readonly ConversationSummary[], prices: ModelPrices | null): CostEstimate {
  let total = 0;
  let priced = 0;
  let complete = true;
  for (const conversation of conversations) {
    const estimate = conversationCostEstimateByModel(conversation, prices);
    if (!estimate.complete) complete = false;
    if (estimate.value == null) continue;
    total += estimate.value;
    priced++;
  }
  return { value: priced > 0 ? total : null, complete };
}

function formatCostEstimate(estimate: CostEstimate): string {
  if (estimate.value == null) return '—';
  if (estimate.complete) return formatCost(estimate.value);
  if (estimate.value > 0 && estimate.value < 0.01) {
    return `≥$${estimate.value.toFixed(4).replace(/0+$/, '')}`;
  }
  return `≥${formatCost(estimate.value)}`;
}

function costEstimateTitle(estimate: CostEstimate): string {
  return estimate.complete
    ? ESTIMATED_COST_TOOLTIP
    : `${ESTIMATED_COST_TOOLTIP} Unpriced or unlabeled model usage is excluded.`;
}

function chartCostDescription(estimate: CostEstimate & { hasUsage: boolean }): string {
  if (!estimate.hasUsage) return 'no token usage';
  if (estimate.value == null) return 'estimated cost unavailable';
  if (estimate.complete) return `estimated cost ${formatCost(estimate.value)}`;
  return `estimated cost ${formatCostEstimate(estimate)}; unpriced usage excluded`;
}

function aggregateWorkspaces(
  conversations: readonly ConversationSummary[],
  prices: ModelPrices | null,
): WorkspaceRow[] {
  const rows = new Map<string, WorkspaceRow>();
  for (const conversation of conversations) {
    const path = conversation.workspace || '';
    let row = rows.get(path);
    if (!row) {
      row = {
        path,
        count: 0,
        cost: null,
        costComplete: true,
        tokens: 0,
        dur: 0,
        last: 0,
      };
      rows.set(path, row);
    }
    row.count++;
    row.tokens += tokenTotal(conversation.token_buckets);
    const estimate = conversationCostEstimateByModel(conversation, prices);
    if (!estimate.complete) row.costComplete = false;
    if (estimate.value != null) row.cost = (row.cost || 0) + estimate.value;
    const duration = durationBetweenSeconds(conversation.started_at, conversation.last_activity);
    if (duration != null) row.dur += duration;
    const last = conversationTime(conversation);
    if (last != null) row.last = Math.max(row.last, last);
  }
  return [...rows.values()];
}

function aggregateModels(conversations: readonly ModelUsageSource[], prices: ModelPrices | null): ModelAggregate[] {
  const rows = new Map<string, { buckets: TokenBuckets }>();
  for (const conversation of conversations) {
    const byModel = conversation.token_buckets_by_model;
    if (byModel && Object.keys(byModel).length > 0) {
      for (const [model, buckets] of Object.entries(byModel)) {
        const modelName = model || '(unknown)';
        let row = rows.get(modelName);
        if (!row) {
          row = { buckets: { ...EMPTY_BUCKETS } };
          rows.set(modelName, row);
        }
        for (const series of TOKEN_SERIES) row.buckets[series.key] += buckets[series.key] || 0;
      }
      continue;
    }
    const model = conversation.models?.[0] || '(unknown)';
    let row = rows.get(model);
    if (!row) {
      row = { buckets: { ...EMPTY_BUCKETS } };
      rows.set(model, row);
    }
    for (const series of TOKEN_SERIES) row.buckets[series.key] += conversation.token_buckets?.[series.key] || 0;
  }
  return [...rows.entries()].map(([model, row]) => ({
    model,
    tokens: tokenTotal(row.buckets),
    cost: model === '(unknown)' ? null : conversationCost({ token_buckets: row.buckets, models: [model] }, prices),
  }));
}

function sortByUnit<T extends { tokens: number; cost: number | null }>(rows: readonly T[], unit: AnalyticsUnit): T[] {
  const value = (row: T) => (unit === 'cost' ? (row.cost ?? -1) : row.tokens);
  return [...rows].sort((a, b) => value(b) - value(a));
}

const DELTA_BASELINE = 'vs previous period';

function percentageDelta(current: number | null, previous: number | null): string | null {
  if (current == null || previous == null || previous <= 0) return null;
  const value = Math.round(((current - previous) / previous) * 100);
  if (value > 0) return `+${value}% ${DELTA_BASELINE}`;
  if (value < 0) return `−${Math.abs(value)}% ${DELTA_BASELINE}`;
  return `0% ${DELTA_BASELINE}`;
}

function pointDelta(current: number | null, previous: number | null): string | null {
  if (current == null || previous == null) return null;
  const value = current - previous;
  if (value > 0) return `+${value} pt ${DELTA_BASELINE}`;
  if (value < 0) return `−${Math.abs(value)} pt ${DELTA_BASELINE}`;
  return `0 pt ${DELTA_BASELINE}`;
}

function formatInteger(value: number): string {
  return Math.round(value).toLocaleString('en-US');
}

function emptyRangeMessage(rangeLabel: string): React.ReactNode {
  return (
    <Fragment>
      No sessions in <code style={{ color: 'var(--fg1)' }}>{rangeLabel}</code>.
    </Fragment>
  );
}

function EmptyPanel({ children }: { children: React.ReactNode }) {
  return <div style={{ padding: '16px 18px', color: 'var(--fg2)', fontSize: 12 }}>{children}</div>;
}

function CostYAxis({ top, mid, side = 'left' }: { top: string; mid: string; side?: 'left' | 'right' }) {
  const label: React.CSSProperties = {
    position: 'absolute',
    left: side === 'left' ? 0 : undefined,
    right: side === 'right' ? 0 : undefined,
    width: 34,
    textAlign: side === 'left' ? 'right' : 'left',
    transform: 'translateY(-50%)',
    color: 'var(--brand-orange-text)',
    fontFamily: 'var(--fontFamilyMonospace)',
    fontSize: 10,
    lineHeight: '10px',
    pointerEvents: 'none',
  };
  return (
    <Fragment>
      <div style={{ ...label, top: 0 }}>{top}</div>
      <div style={{ ...label, top: 85 }}>{mid}</div>
      <div style={{ ...label, top: 170 }}>$0</div>
    </Fragment>
  );
}

function PanelHeader({ title, meta }: PanelHeaderProps) {
  return (
    <div
      style={{
        padding: '12px 18px',
        borderBottom: '1px solid var(--border-weak)',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        gap: 16,
        flexWrap: 'wrap',
      }}
    >
      <span style={{ fontSize: 13, fontWeight: 600, color: 'var(--fg-max)' }}>{title}</span>
      {meta != null && (
        <span style={{ fontFamily: 'var(--fontFamilyMonospace)', fontSize: 11, color: 'var(--fg3)' }}>{meta}</span>
      )}
    </div>
  );
}

function UnitToggle({ value, onChange }: { value: AnalyticsUnit; onChange: (value: AnalyticsUnit) => void }) {
  const options: { value: AnalyticsUnit; label: string }[] = [
    { value: 'cost', label: 'Cost' },
    { value: 'tokens', label: 'Tokens' },
  ];
  return (
    <fieldset
      aria-label="Measure in"
      style={{
        display: 'inline-flex',
        gap: 3,
        minWidth: 0,
        margin: 0,
        padding: 3,
        border: '1px solid var(--border-medium)',
        borderRadius: 999,
        background: PANEL_BG,
        boxShadow: 'var(--control-inset-shadow)',
      }}
    >
      {options.map((option) => {
        const active = option.value === value;
        return (
          <button
            key={option.value}
            type="button"
            aria-pressed={active}
            onClick={() => {
              if (!active) onChange(option.value);
            }}
            style={{
              height: 26,
              padding: '0 15px',
              border: 'none',
              borderRadius: 999,
              background: active ? ACTIVE_PILL_BG : 'transparent',
              color: active ? 'var(--primary-text)' : 'var(--fg2)',
              boxShadow: active ? 'inset 0 0 0 1px var(--primary-border)' : 'none',
              cursor: active ? 'default' : 'pointer',
              fontFamily: 'var(--fontFamily)',
              fontSize: 12,
              fontWeight: active ? 600 : 400,
            }}
          >
            {option.label}
          </button>
        );
      })}
    </fieldset>
  );
}

function pointSparkline(
  points: readonly TokenUsagePoint[],
  rangeValue: string,
  now: number,
  serverIntervalMs: number,
  value: (point: TokenUsagePoint) => number,
): SparkValue[] {
  const grid = chartGrid(points.map(tokenPointTime), rangeValue, now, serverIntervalMs);
  const buckets = Array.from({ length: grid.count }, (_, position) => ({
    key: grid.start + position * grid.bucketMs,
    value: 0,
  }));
  for (const point of points) {
    const time = tokenPointTime(point);
    if (!Number.isFinite(time) || time < grid.start || time >= grid.end) continue;
    const position = Math.floor((time - grid.start) / grid.bucketMs);
    const bucket = buckets[position];
    if (bucket) bucket.value += value(point);
  }
  return buckets;
}

function costSparkline(
  points: readonly TokenUsagePoint[],
  rangeValue: string,
  now: number,
  serverIntervalMs: number,
  prices: ModelPrices | null,
): SparkValue[] {
  const buckets: SparkValue[] = pointSparkline(points, rangeValue, now, serverIntervalMs, () => 0).map((bucket) => ({
    ...bucket,
    costStatus: 'no-usage' as const,
  }));
  if (buckets.length === 0) return buckets;
  const start = buckets[0]?.key || now;
  const width = buckets[1] ? buckets[1].key - start : Math.max(1, now - start);
  for (const point of points) {
    const time = tokenPointTime(point);
    if (!Number.isFinite(time) || time < start || time > now || tokenTotal(point) <= 0) continue;
    const position = Math.min(buckets.length - 1, Math.max(0, Math.floor((time - start) / width)));
    const bucket = buckets[position];
    if (!bucket) continue;
    const cost = tokenPointCost(point, prices);
    if (cost == null) {
      bucket.costStatus = bucket.costStatus === 'no-usage' ? 'unknown' : 'partial';
      continue;
    }
    bucket.value += cost;
    bucket.costStatus = bucket.costStatus === 'unknown' || bucket.costStatus === 'partial' ? 'partial' : 'complete';
  }
  return buckets;
}

function cacheSparkline(
  points: readonly TokenUsagePoint[],
  rangeValue: string,
  now: number,
  serverIntervalMs: number,
  overall: number | null,
): SparkValue[] {
  const fresh = pointSparkline(points, rangeValue, now, serverIntervalMs, (point) => point.fresh_input);
  const read = pointSparkline(points, rangeValue, now, serverIntervalMs, (point) => point.cache_read);
  const write = pointSparkline(points, rangeValue, now, serverIntervalMs, (point) => point.cache_write);
  return fresh.map((bucket, position) => {
    const pct = cacheInputHitPercent(bucket.value, read[position]?.value, write[position]?.value) || 0;
    return {
      key: bucket.key,
      value: pct,
      tone: overall != null && pct > 0 && pct + 8 < overall ? 'var(--warning-spark)' : undefined,
    };
  });
}

function KpiCard({ label, value, delta, deltaColor, bars, color, peakColor, sub }: KpiCardProps) {
  const max = Math.max(1, ...bars.map((bar) => bar.value));
  const peak = bars.reduce((best, bar) => (bar.value > best.value ? bar : best), bars[0] || { key: 0, value: 0 });
  return (
    <SurfaceCard
      style={{
        padding: '16px 18px 14px',
        display: 'flex',
        flexDirection: 'column',
        gap: 11,
        boxShadow: 'none',
      }}
    >
      <div style={{ display: 'flex', justifyContent: 'space-between', gap: 8 }}>
        <span style={{ fontSize: 11.5, color: 'var(--fg3)' }}>{label}</span>
        {delta && (
          <span
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: deltaColor || 'var(--fg2)',
              textAlign: 'right',
            }}
          >
            {delta}
          </span>
        )}
      </div>
      <div
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 32,
          fontWeight: 500,
          lineHeight: 1,
          color: 'var(--fg-max)',
          letterSpacing: '-0.01em',
        }}
      >
        {value}
      </div>
      <div style={{ display: 'flex', alignItems: 'flex-end', gap: 2, height: 32 }} aria-hidden="true">
        {bars.map((bar) => (
          <span
            key={bar.key}
            data-cost-status={bar.costStatus}
            data-spark-key={bar.key}
            style={{
              flex: 1,
              minWidth: 0,
              height: `${Math.max(3, (bar.value / max) * 100)}%`,
              background:
                bar.costStatus === 'no-usage' || bar.costStatus === 'unknown'
                  ? 'transparent'
                  : bar.tone || (bar.key === peak.key && peak.value > 0 ? peakColor : color),
              borderBottom: bar.costStatus === 'unknown' ? '1px dotted var(--fg3)' : undefined,
              opacity: bar.costStatus === 'partial' ? 0.65 : 1,
              borderRadius: '1px 1px 0 0',
            }}
          />
        ))}
      </div>
      <div style={{ fontSize: 11, color: 'var(--fg2)', minHeight: 17 }}>{sub}</div>
    </SurfaceCard>
  );
}

export interface AnalyticsChartProps {
  points: TokenUsagePoint[];
  tokenIntervalMs: number;
  unit: AnalyticsUnit;
  timeRange: string;
  hiddenSeries: ReadonlySet<TokenBucketKey>;
  onToggleSeries: (key: TokenBucketKey) => void;
  onOpenBucket: (span: TimeSpan) => void;
  now?: number;
  prices: ModelPrices | null;
  emptyMessage?: React.ReactNode;
}

export function AnalyticsChart({
  points,
  tokenIntervalMs,
  unit,
  timeRange,
  hiddenSeries,
  onToggleSeries,
  onOpenBucket,
  now,
  prices,
  emptyMessage,
}: AnalyticsChartProps) {
  const nowMs = now ?? Date.now();
  const [hoverStart, setHoverStart] = useState<number | null>(null);
  const [selection, setSelection] = useState<TimeSpan | null>(null);
  const usagePoints = useMemo(() => points.filter((point) => tokenTotal(point) > 0), [points]);
  const grid = useMemo(() => {
    const interval = timeRangeOption(timeRange).ms == null ? tokenIntervalMs : 0;
    return chartGrid(usagePoints.map(tokenPointTime), timeRange, nowMs, interval);
  }, [usagePoints, timeRange, nowMs, tokenIntervalMs]);
  const usage = useMemo(
    () => bucketTokenUsage(usagePoints, timeRange, nowMs, { window: grid, count: grid.count }),
    [usagePoints, timeRange, nowMs, grid],
  );
  const costBuckets = useMemo(() => {
    return usage.buckets.map((bucket) => {
      let value = 0;
      let priced = 0;
      let hasUsage = false;
      let complete = true;
      for (const point of usagePoints) {
        const time = tokenPointTime(point);
        if (!Number.isFinite(time) || time < bucket.start || time >= bucket.end) continue;
        if (tokenTotal(point) <= 0) continue;
        hasUsage = true;
        const cost = tokenPointCost(point, prices);
        if (cost == null) {
          complete = false;
          continue;
        }
        value += cost;
        priced++;
      }
      return {
        start: bucket.start,
        value: priced > 0 ? value : null,
        complete: hasUsage && complete,
        hasUsage,
      };
    });
  }, [usage.buckets, usagePoints, prices]);
  const present = TOKEN_SERIES.filter((series) => usage.buckets.some((bucket) => bucket[series.key] > 0));
  const legend = present.length > 0 ? present : TOKEN_SERIES;
  const visible = TOKEN_SERIES.filter((series) => !hiddenSeries.has(series.key));
  const visibleTokens = (bucket: (typeof usage.buckets)[number]) =>
    visible.reduce((sum, series) => sum + (bucket[series.key] || 0), 0);
  const maxTokens = Math.max(1, ...usage.buckets.map(visibleTokens));
  const axisCostBuckets = unit === 'tokens' ? costBuckets.filter((bucket) => bucket.complete) : costBuckets;
  const maxCost = Math.max(0, ...axisCostBuckets.map((bucket) => bucket.value ?? 0));
  const costScaleMax = maxCost > 0 ? maxCost : 1;
  const secondaryMax = unit === 'cost' ? maxTokens : costScaleMax;
  const lineSegments = (() => {
    const segments: string[] = [];
    let current: string[] = [];
    for (const [position, bucket] of usage.buckets.entries()) {
      const costBucket = costBuckets[position];
      const secondary =
        unit === 'cost'
          ? visibleTokens(bucket)
          : !costBucket?.hasUsage
            ? 0
            : costBucket.complete
              ? costBucket.value
              : null;
      if (secondary == null) {
        if (current.length > 0) segments.push(current.join(' '));
        current = [];
        continue;
      }
      const x = ((position + 0.5) / Math.max(1, usage.buckets.length)) * 100;
      const y = 32 - (secondary / secondaryMax) * 32;
      current.push(`${x},${Math.max(0, Math.min(32, y))}`);
    }
    if (current.length > 0) segments.push(current.join(' '));
    return segments;
  })();
  const hovered = hoverStart == null ? null : usage.buckets.find((bucket) => bucket.start === hoverStart) || null;
  const hoveredPosition = hovered ? usage.buckets.findIndex((bucket) => bucket.start === hovered.start) : -1;
  const hoveredCost = hoveredPosition >= 0 ? costBuckets[hoveredPosition] : undefined;
  const openBucket = (bucket: (typeof usage.buckets)[number]) => {
    const span = { start: bucket.start, end: bucket.end };
    setSelection(span);
    onOpenBucket(span);
  };
  const hasUsage = usage.grandTotal > 0;
  const title = unit === 'cost' ? 'Cost over time' : 'Tokens over time';
  const hasPricedCost = axisCostBuckets.some((bucket) => bucket.value != null);
  const hasIncompleteCost = costBuckets.some((bucket) => bucket.hasUsage && !bucket.complete);
  const formattedMaxCost = hasPricedCost ? formatCost(maxCost) : '—';
  const formattedMidCost = hasPricedCost ? formatCost(maxCost / 2) : '—';
  const leftTop = unit === 'cost' ? formattedMaxCost : formatTokens(maxTokens);
  const leftMid = unit === 'cost' ? formattedMidCost : formatTokens(maxTokens / 2);
  const rightTop = unit === 'cost' ? formatTokens(maxTokens) : formattedMaxCost;
  const rightMid = unit === 'cost' ? formatTokens(maxTokens / 2) : formattedMidCost;

  return (
    <SurfaceCard style={{ boxShadow: 'none' }} data-testid="analytics-chart" data-unit={unit}>
      <PanelHeader
        title={title}
        meta={
          <span style={{ display: 'inline-flex', alignItems: 'center', gap: 14, flexWrap: 'wrap' }}>
            {unit === 'tokens' ? (
              legend.map((series) => {
                const hidden = hiddenSeries.has(series.key);
                return (
                  <button
                    key={series.key}
                    type="button"
                    aria-pressed={!hidden}
                    onClick={() => onToggleSeries(series.key)}
                    title={hidden ? `Show ${series.label}` : `Hide ${series.label}`}
                    style={{
                      display: 'inline-flex',
                      alignItems: 'center',
                      gap: 6,
                      padding: 0,
                      border: 'none',
                      background: 'transparent',
                      color: 'inherit',
                      font: 'inherit',
                      cursor: 'pointer',
                      textDecoration: hidden ? 'line-through' : 'none',
                    }}
                  >
                    <span
                      style={{
                        width: 10,
                        height: 10,
                        border: `1px solid ${hidden ? 'var(--border-medium)' : series.color}`,
                        borderRadius: 1,
                        background: hidden ? 'transparent' : series.color,
                      }}
                    />
                    {series.label}
                  </button>
                );
              })
            ) : (
              <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
                <span style={{ width: 10, height: 10, borderRadius: 1, background: 'var(--brand-orange)' }} />
                Estimated cost{hasIncompleteCost ? ' · partial' : ''}
              </span>
            )}
            <span style={{ display: 'inline-flex', alignItems: 'center', gap: 6 }}>
              <span
                style={{
                  width: 14,
                  height: 2,
                  background: unit === 'cost' ? 'var(--viz-green)' : 'var(--brand-orange)',
                }}
              />
              {unit === 'cost' ? 'Tokens' : `Estimated cost${hasIncompleteCost ? ' · partial buckets omitted' : ''}`}
            </span>
            <span>{usage.bucketLabel}</span>
          </span>
        }
      />
      {!hasUsage && emptyMessage != null ? (
        <EmptyPanel>{emptyMessage}</EmptyPanel>
      ) : (
        <div style={{ padding: '18px 18px 12px' }}>
          <div style={{ position: 'relative' }}>
            {unit === 'cost' ? (
              <CostYAxis top={leftTop} mid={leftMid} />
            ) : (
              <ChartYAxis top={leftTop} mid={leftMid} height={170} />
            )}
            {unit === 'tokens' ? (
              <CostYAxis top={rightTop} mid={rightMid} side="right" />
            ) : (
              <ChartYAxis top={rightTop} mid={rightMid} height={170} side="right" />
            )}
            <div
              style={{
                position: 'relative',
                margin: '0 44px',
                borderBottom: '1px solid var(--border-medium)',
              }}
            >
              <svg
                viewBox="0 0 100 32"
                preserveAspectRatio="none"
                style={{ width: '100%', height: 170, display: 'block' }}
              >
                <title>{title}</title>
                {[0, 0.5].map((line) => (
                  <line
                    key={line}
                    x1={0}
                    x2={100}
                    y1={32 * line}
                    y2={32 * line}
                    stroke="var(--chart-grid)"
                    strokeWidth="0.2"
                  />
                ))}
                {usage.buckets.map((bucket, position) => {
                  const slot = 100 / Math.max(1, usage.buckets.length);
                  const width = slot * 0.7;
                  const x = position * slot + slot * 0.15;
                  const midpoint = (bucket.start + bucket.end) / 2;
                  const selected = selection != null && midpoint >= selection.start && midpoint < selection.end;
                  const dimmed = selection != null && !selected;
                  const hoveredBucket = hoverStart === bucket.start;
                  const baseOpacity = hoveredBucket || selected ? 1 : dimmed ? 0.3 : 0.9;
                  const costBucket = costBuckets[position] || { value: null, complete: false, hasUsage: false };
                  const costDescription = chartCostDescription(costBucket);
                  const segments: React.ReactElement[] = [];
                  if (unit === 'cost') {
                    const value = costBucket.value;
                    if (value != null) {
                      const height = (value / costScaleMax) * 32;
                      segments.push(
                        <rect
                          key="cost"
                          data-bar-unit="cost"
                          data-cost-status={costBucket.complete ? 'complete' : 'partial'}
                          x={x}
                          y={32 - height}
                          width={width}
                          height={Math.max(height, value > 0 ? 0.2 : 0)}
                          fill="var(--brand-orange)"
                          opacity={costBucket.complete ? (value > 0 && value === maxCost ? 1 : baseOpacity) : 0.65}
                        />,
                      );
                    }
                  } else {
                    let top = 32;
                    for (const series of visible) {
                      const value = bucket[series.key] || 0;
                      if (value <= 0) continue;
                      const height = (value / maxTokens) * 32;
                      top -= height;
                      segments.push(
                        <rect
                          key={series.key}
                          data-bar-unit="tokens"
                          data-series={series.key}
                          x={x}
                          y={top}
                          width={width}
                          height={Math.max(height, 0.2)}
                          fill={series.color}
                          opacity={baseOpacity}
                        />,
                      );
                    }
                  }
                  return (
                    // biome-ignore lint/a11y/useSemanticElements: SVG buckets must remain in the chart coordinate system.
                    <g
                      key={`${bucket.start}:${bucket.end}`}
                      role="button"
                      data-line-x={((position + 0.5) / Math.max(1, usage.buckets.length)) * 100}
                      data-cost-status={
                        !costBucket.hasUsage
                          ? 'no-usage'
                          : costBucket.complete
                            ? 'complete'
                            : costBucket.value == null
                              ? 'unknown'
                              : 'partial'
                      }
                      tabIndex={0}
                      aria-label={`${bucket.t}: ${
                        unit === 'cost'
                          ? costDescription
                          : `${formatTokens(visibleTokens(bucket))} tokens; ${costDescription}`
                      }. Open this time bucket.`}
                      onMouseEnter={() => setHoverStart(bucket.start)}
                      onMouseLeave={() => setHoverStart(null)}
                      onFocus={() => setHoverStart(bucket.start)}
                      onBlur={() => setHoverStart(null)}
                      onClick={() => openBucket(bucket)}
                      onKeyDown={(event) => {
                        if (event.key === 'Enter' || event.key === ' ') {
                          event.preventDefault();
                          openBucket(bucket);
                        }
                      }}
                      style={{ cursor: 'pointer' }}
                    >
                      <rect x={x - slot * 0.05} y={0} width={width + slot * 0.1} height={32} fill="transparent" />
                      {segments}
                    </g>
                  );
                })}
                {lineSegments.map((line) => (
                  <polyline
                    key={`${unit}:${line}`}
                    data-secondary-series={unit === 'cost' ? 'tokens' : 'cost'}
                    points={line}
                    fill="none"
                    stroke={unit === 'cost' ? 'var(--viz-green)' : 'var(--brand-orange)'}
                    strokeWidth={1.4}
                    vectorEffect="non-scaling-stroke"
                    strokeLinejoin="round"
                    strokeLinecap="round"
                    pointerEvents="none"
                  />
                ))}
              </svg>
              {hovered &&
                hoveredPosition >= 0 &&
                (unit === 'cost' ? hoveredCost?.hasUsage : visibleTokens(hovered) > 0) && (
                  <div
                    role="tooltip"
                    style={{
                      position: 'absolute',
                      left: chartTooltipLeft(hoveredPosition, usage.buckets.length),
                      transform: 'translate(-50%, -100%)',
                      top: -4,
                      padding: '6px 8px',
                      border: '1px solid var(--border-medium)',
                      borderRadius: 2,
                      background: 'var(--bg-secondary)',
                      boxShadow: 'var(--shadow-z2)',
                      color: 'var(--fg1)',
                      fontFamily: 'var(--fontFamilyMonospace)',
                      fontSize: 11,
                      whiteSpace: 'nowrap',
                      pointerEvents: 'none',
                      zIndex: 1,
                    }}
                  >
                    <div style={{ color: 'var(--fg3)', marginBottom: 4 }}>
                      {hovered.t}
                      {visibleTokens(hovered) > 0 ? ` · ${formatTokens(visibleTokens(hovered))} tok` : ''}
                    </div>
                    {unit === 'cost' && hoveredCost && (
                      <div style={{ marginBottom: visibleTokens(hovered) > 0 ? 4 : 0 }}>
                        {chartCostDescription(hoveredCost)}
                      </div>
                    )}
                    {visible
                      .filter((series) => hovered[series.key] > 0)
                      .map((series) => (
                        <div key={series.key} style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                          <span style={{ width: 8, height: 8, borderRadius: 1, background: series.color }} />
                          <span style={{ color: 'var(--fg2)' }}>{series.label}</span>
                          <span style={{ marginLeft: 'auto', color: 'var(--fg1)' }}>
                            {formatTokens(hovered[series.key])}
                          </span>
                        </div>
                      ))}
                  </div>
                )}
            </div>
            <ChartXLabels data={usage.buckets} gutter={44} />
          </div>
          <div style={{ marginTop: 8, fontSize: 11, color: 'var(--fg3)' }}>
            {unit === 'cost'
              ? 'Click a bar to filter the session list to that time bucket.'
              : 'Series are disjoint, so the stack never double-counts. Click a bar to filter the session list.'}
          </div>
        </div>
      )}
    </SurfaceCard>
  );
}

function WorkspacesPanel({
  rows,
  unit,
  onOpen,
  empty,
}: {
  rows: WorkspaceRow[];
  unit: AnalyticsUnit;
  onOpen: (path: string) => void;
  empty: React.ReactNode;
}) {
  const sorted = sortByUnit(rows, unit).slice(0, 6);
  const costComplete = rows.every((row) => row.costComplete);
  const total = rows.reduce((sum, row) => sum + (unit === 'cost' ? row.cost || 0 : row.tokens), 0);
  const max = Math.max(1, ...sorted.map((row) => (unit === 'cost' ? row.cost || 0 : row.tokens)));
  return (
    <SurfaceCard style={{ boxShadow: 'none', minWidth: 0 }}>
      <PanelHeader
        title="Workspaces"
        meta={`sorted by ${unit}${unit === 'cost' && !costComplete ? ' · partial estimate' : ''}`}
      />
      {sorted.length === 0 ? (
        <EmptyPanel>{empty}</EmptyPanel>
      ) : (
        <div style={{ padding: '0 18px 14px' }}>
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: WORKSPACE_GRID,
              gap: 10,
              padding: '11px 0 9px',
              borderBottom: '1px solid var(--border-weak)',
              color: 'var(--fg3)',
              fontSize: 11,
            }}
          >
            <span>Workspace</span>
            <span>Share</span>
            <span style={{ textAlign: 'right' }}>Sessions</span>
            <span style={{ textAlign: 'right' }}>Tokens</span>
            <span style={{ textAlign: 'right' }}>Cost</span>
          </div>
          {sorted.map((row) => {
            const path = splitWorkspacePath(row.path);
            const value = unit === 'cost' ? row.cost || 0 : row.tokens;
            const share = total > 0 ? Math.round((value / total) * 100) : 0;
            return (
              <a
                key={row.path || '(unknown)'}
                href={conversationsPath(row.path)}
                onClick={(event) => {
                  if (!isPlainLeftClick(event)) return;
                  event.preventDefault();
                  onOpen(row.path);
                }}
                style={{
                  display: 'grid',
                  gridTemplateColumns: WORKSPACE_GRID,
                  alignItems: 'center',
                  gap: 10,
                  padding: '11px 0',
                  borderBottom: '1px solid var(--border-weak)',
                  color: 'inherit',
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 12,
                  textDecoration: 'none',
                }}
                onMouseEnter={(event) => (event.currentTarget.style.background = 'var(--row-hover)')}
                onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
              >
                <span
                  title={row.path || '(unknown)'}
                  style={{
                    minWidth: 0,
                    display: 'flex',
                    alignItems: 'baseline',
                    gap: 5,
                    overflow: 'hidden',
                    whiteSpace: 'nowrap',
                  }}
                >
                  <span
                    style={{
                      flex: '0 1 auto',
                      minWidth: 0,
                      overflow: 'hidden',
                      textOverflow: 'clip',
                      direction: 'rtl',
                      textAlign: 'left',
                      color: 'var(--fg3)',
                      fontSize: 11,
                    }}
                  >
                    {path.dir}
                  </span>
                  <span
                    style={{
                      flex: '0 0 auto',
                      color: 'var(--fg-max)',
                      fontFamily: 'var(--fontFamily)',
                      fontSize: 12.5,
                      fontWeight: 600,
                    }}
                  >
                    {path.leaf}
                  </span>
                </span>
                <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
                  <span
                    style={{
                      flex: 1,
                      height: 6,
                      borderRadius: 2,
                      overflow: 'hidden',
                      background: 'var(--bar-track)',
                    }}
                  >
                    <span
                      style={{
                        display: 'block',
                        width: `${Math.max(0, Math.min(100, (value / max) * 100))}%`,
                        height: '100%',
                        background: unit === 'cost' ? 'var(--brand-orange)' : 'var(--viz-green)',
                      }}
                    />
                  </span>
                  <span style={{ width: 26, textAlign: 'right', color: 'var(--fg3)', fontSize: 10.5 }}>
                    {unit === 'cost' && !costComplete ? '—' : `${share}%`}
                  </span>
                </span>
                <span style={{ textAlign: 'right', color: 'var(--fg2)' }}>{row.count}</span>
                <span style={{ textAlign: 'right', color: unit === 'tokens' ? 'var(--fg-max)' : 'var(--fg1)' }}>
                  {formatTokens(row.tokens)}
                </span>
                <span
                  title={costEstimateTitle({ value: row.cost, complete: row.costComplete })}
                  style={{ textAlign: 'right', color: unit === 'cost' ? 'var(--fg-max)' : 'var(--fg1)' }}
                >
                  {formatCostEstimate({ value: row.cost, complete: row.costComplete })}
                </span>
              </a>
            );
          })}
        </div>
      )}
    </SurfaceCard>
  );
}

function ModelsPanel({ rows, unit, empty }: { rows: ModelAggregate[]; unit: AnalyticsUnit; empty: React.ReactNode }) {
  const sorted = sortByUnit(rows, unit).slice(0, 8);
  return (
    <SurfaceCard style={{ boxShadow: 'none', minWidth: 0 }}>
      <PanelHeader title="Models" meta="estimated blended cost per 1M tokens" />
      {sorted.length === 0 ? (
        <EmptyPanel>{empty}</EmptyPanel>
      ) : (
        <div style={{ padding: '0 18px 14px' }}>
          <div
            style={{
              display: 'grid',
              gridTemplateColumns: MODEL_GRID,
              gap: 10,
              padding: '11px 0 9px',
              borderBottom: '1px solid var(--border-weak)',
              color: 'var(--fg3)',
              fontSize: 11,
            }}
          >
            <span>Model</span>
            <span style={{ textAlign: 'right' }}>Tokens</span>
            <span style={{ textAlign: 'right' }}>Cost</span>
            <span style={{ textAlign: 'right' }}>Per 1M</span>
          </div>
          {sorted.map((row) => (
            <div
              key={row.model}
              data-model-row={row.model}
              style={{
                display: 'grid',
                gridTemplateColumns: MODEL_GRID,
                alignItems: 'center',
                gap: 10,
                padding: '12px 0',
                borderBottom: '1px solid var(--border-weak)',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 12,
              }}
            >
              <span
                title={row.model}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 7,
                  minWidth: 0,
                  overflow: 'hidden',
                  color: 'var(--fg1)',
                }}
              >
                <span
                  style={{ width: 7, height: 7, borderRadius: '50%', background: modelDot(row.model), flexShrink: 0 }}
                />
                <span style={{ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' }}>
                  {shortModel(row.model)}
                </span>
              </span>
              <span style={{ textAlign: 'right', color: unit === 'tokens' ? 'var(--fg-max)' : 'var(--fg1)' }}>
                {formatTokens(row.tokens)}
              </span>
              <span
                title={ESTIMATED_COST_TOOLTIP}
                style={{ textAlign: 'right', color: unit === 'cost' ? 'var(--fg-max)' : 'var(--fg1)' }}
              >
                {formatCost(row.cost)}
              </span>
              <span style={{ textAlign: 'right', color: 'var(--fg2)' }}>
                {row.cost != null && row.tokens > 0 ? formatCost((row.cost / row.tokens) * 1_000_000) : '—'}
              </span>
            </div>
          ))}
          <div style={{ paddingTop: 12, color: 'var(--fg3)', fontSize: 11.5, lineHeight: 1.5 }}>
            Estimated model cost divided by all recorded tokens.
          </div>
        </div>
      )}
    </SurfaceCard>
  );
}

function SessionShapePanel({ conversations, empty }: { conversations: ConversationSummary[]; empty: React.ReactNode }) {
  const buckets = [
    { key: 'under-500k', label: '< 500k', min: 0, max: 500_000 },
    { key: '500k-1m', label: '500k – 1M', min: 500_000, max: 1_000_000 },
    { key: '1m-2m', label: '1M – 2M', min: 1_000_000, max: 2_000_000 },
    { key: '2m-5m', label: '2M – 5M', min: 2_000_000, max: 5_000_000 },
    { key: '5m-10m', label: '5M – 10M', min: 5_000_000, max: 10_000_000 },
    { key: 'over-10m', label: '10M or more', min: 10_000_000, max: Number.POSITIVE_INFINITY },
  ].map((bucket) => ({
    ...bucket,
    count: conversations.filter((conversation) => {
      const tokens = tokenTotal(conversation.token_buckets);
      return tokens >= bucket.min && tokens < bucket.max;
    }).length,
  }));
  const max = Math.max(1, ...buckets.map((bucket) => bucket.count));
  const durations = conversations
    .map((conversation) => durationBetweenSeconds(conversation.started_at, conversation.last_activity))
    .filter((duration): duration is number => duration != null)
    .sort((a, b) => a - b);
  const percentile = (p: number) =>
    durations[Math.min(durations.length - 1, Math.max(0, Math.ceil(durations.length * p) - 1))];
  return (
    <SurfaceCard style={{ boxShadow: 'none', minWidth: 0 }}>
      <PanelHeader title="Sessions" meta="tokens / session" />
      {conversations.length === 0 ? (
        <EmptyPanel>{empty}</EmptyPanel>
      ) : (
        <div style={{ padding: '14px 18px 16px', display: 'flex', flexDirection: 'column', gap: 11 }}>
          {buckets.map((bucket) => (
            <div
              key={bucket.key}
              data-shape-bucket={bucket.key}
              data-shape-count={bucket.count}
              style={{
                display: 'grid',
                gridTemplateColumns: SHAPE_GRID,
                alignItems: 'center',
                gap: 10,
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 11.5,
              }}
            >
              <span style={{ color: 'var(--fg2)' }}>{bucket.label}</span>
              <span style={{ height: 14, overflow: 'hidden', background: 'var(--shape-track)' }}>
                <span
                  style={{
                    display: 'block',
                    width: `${(bucket.count / max) * 100}%`,
                    height: '100%',
                    background: bucket.key === 'over-10m' ? 'var(--shape-hot-fill)' : 'var(--shape-fill)',
                  }}
                />
              </span>
              <span style={{ textAlign: 'right', color: 'var(--fg1)' }}>{bucket.count}</span>
            </div>
          ))}
          <div
            style={{
              marginTop: 5,
              paddingTop: 12,
              borderTop: '1px solid var(--border-weak)',
              display: 'flex',
              justifyContent: 'space-between',
              color: 'var(--fg3)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
            }}
          >
            <span>median {formatDuration(percentile(0.5))}</span>
            <span>p90 {formatDuration(percentile(0.9))}</span>
          </div>
        </div>
      )}
    </SurfaceCard>
  );
}

function HeatmapPanel({
  points,
  unit,
  prices,
  loading,
  error,
}: {
  points: TokenUsagePoint[];
  unit: AnalyticsUnit;
  prices: ModelPrices | null;
  loading: boolean;
  error: string | null;
}) {
  const [focusedCell, setFocusedCell] = useState<string | null>(null);
  const values = useMemo(() => {
    const cells = new Map<string, CostEstimate & { hasUsage: boolean }>();
    for (const point of points) {
      const date = new Date(point.t);
      if (!Number.isFinite(date.getTime())) continue;
      const day = (date.getDay() + 6) % 7;
      const hour = date.getHours();
      const key = `${day}:${hour}`;
      const current = cells.get(key) || { value: null, complete: true, hasUsage: false };
      const tokens = tokenTotal(point);
      current.hasUsage ||= tokens > 0;
      if (unit === 'cost') {
        const cost = tokenPointCost(point, prices);
        if (cost == null) current.complete = false;
        else current.value = (current.value || 0) + cost;
      } else {
        current.value = (current.value || 0) + tokens;
      }
      cells.set(key, current);
    }
    return cells;
  }, [points, unit, prices]);
  const max = Math.max(0, ...[...values.values()].map((cell) => cell.value || 0));
  const valueLabel = (cell: CostEstimate & { hasUsage: boolean }) =>
    unit === 'cost'
      ? formatCostEstimate({ value: cell.value, complete: cell.complete })
      : `${formatTokens(cell.value || 0)} tokens`;
  const focusedValue = (() => {
    if (!focusedCell) return null;
    const [dayRaw, hourRaw] = focusedCell.split(':');
    const day = Number(dayRaw);
    const hour = Number(hourRaw);
    const cell = values.get(focusedCell);
    const dayName = DAY_NAMES[day];
    if (!cell?.hasUsage || dayName == null || !Number.isFinite(hour)) return null;
    return `${dayName} ${String(hour).padStart(2, '0')}:00 local · ${valueLabel(cell)}`;
  })();
  const hours = Array.from({ length: 24 }, (_, hour) => hour);
  return (
    <SurfaceCard style={{ boxShadow: 'none', minWidth: 0 }}>
      <PanelHeader title="Usage" meta="last 7 days · browser-local time" />
      {error ? (
        <EmptyPanel>Failed to load agent usage: {error}</EmptyPanel>
      ) : loading && points.length === 0 ? (
        <EmptyPanel>Loading agent usage…</EmptyPanel>
      ) : points.length === 0 ? (
        <EmptyPanel>No agent usage in the last 7 days.</EmptyPanel>
      ) : (
        <div style={{ padding: '16px 18px' }}>
          <div style={{ display: 'flex', flexDirection: 'column', gap: 3 }}>
            {DAY_NAMES.map((day, dayPosition) => (
              <div
                key={day}
                style={{
                  display: 'grid',
                  gridTemplateColumns: '32px minmax(0, 1fr)',
                  alignItems: 'center',
                  gap: 9,
                }}
              >
                <span style={{ color: 'var(--fg3)', fontFamily: 'var(--fontFamilyMonospace)', fontSize: 10 }}>
                  {day}
                </span>
                <span style={{ display: 'flex', gap: 2 }}>
                  {hours.map((hour) => {
                    const cell = values.get(`${dayPosition}:${hour}`);
                    if (!cell?.hasUsage) {
                      return (
                        <span
                          key={`${day}:${hour}`}
                          aria-hidden="true"
                          style={{ flex: 1, height: 14, minWidth: 0, borderRadius: 1, background: HEAT_COLORS[0] }}
                        />
                      );
                    }
                    const value = cell.value || 0;
                    const level = value <= 0 || max <= 0 ? 0 : Math.min(4, Math.max(1, Math.ceil((value / max) * 4)));
                    const rendered = valueLabel(cell);
                    const hourLabel = `${day} ${String(hour).padStart(2, '0')}:00 local`;
                    const cellKey = `${dayPosition}:${hour}`;
                    const detail = `${hourLabel} · ${rendered}`;
                    const costStatus =
                      unit === 'cost'
                        ? cell.complete
                          ? 'complete'
                          : cell.value == null
                            ? 'unknown'
                            : 'partial'
                        : undefined;
                    return (
                      <button
                        key={`${day}:${hour}`}
                        type="button"
                        aria-label={`${hourLabel}, ${rendered}`}
                        aria-pressed={focusedCell === cellKey}
                        title={detail}
                        data-cost-status={costStatus}
                        onFocus={() => setFocusedCell(cellKey)}
                        onClick={() => setFocusedCell(cellKey)}
                        style={{
                          flex: 1,
                          height: 14,
                          minWidth: 0,
                          padding: 0,
                          border: costStatus === 'unknown' ? '1px dotted var(--warning-text)' : 'none',
                          borderRadius: 1,
                          background: costStatus === 'unknown' ? 'var(--warning-soft-bg)' : HEAT_COLORS[level],
                          opacity: costStatus === 'partial' ? 0.7 : 1,
                          cursor: 'pointer',
                        }}
                      />
                    );
                  })}
                </span>
              </div>
            ))}
          </div>
          <div
            style={{
              marginTop: 5,
              marginLeft: 41,
              display: 'flex',
              justifyContent: 'space-between',
              color: 'var(--fg3)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 10,
            }}
          >
            <span>00</span>
            <span>06</span>
            <span>12</span>
            <span>18</span>
            <span>23</span>
          </div>
          <div
            aria-live="polite"
            style={{
              minHeight: 16,
              marginTop: 10,
              color: 'var(--fg2)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 10.5,
            }}
          >
            {focusedValue || 'Focus or select a populated cell to inspect its value.'}
          </div>
          <div
            style={{
              marginTop: 10,
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'flex-end',
              gap: 7,
              color: 'var(--fg3)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 10.5,
            }}
          >
            <span>less</span>
            {HEAT_COLORS.map((color) => (
              <span key={color} style={{ width: 14, height: 14, borderRadius: 1, background: color }} />
            ))}
            <span>more {unit}</span>
          </div>
        </div>
      )}
    </SurfaceCard>
  );
}

function ErrorChip() {
  return (
    <span
      style={{
        height: 16,
        padding: '0 6px',
        borderRadius: 2,
        background: 'var(--error-transparent)',
        color: 'var(--error-text)',
        fontSize: 10,
        letterSpacing: '0.04em',
        flexShrink: 0,
      }}
    >
      ERR
    </span>
  );
}

function HeaviestSessionsPanel({
  conversations,
  unit,
  prices,
  totalConversations,
  returnedCurrentRange,
  previousTotalConversations,
  returnedPreviousRange,
  kpisCoverAll,
  previousKpisCoverAll,
  onOpen,
  empty,
}: {
  conversations: ConversationSummary[];
  unit: AnalyticsUnit;
  prices: ModelPrices | null;
  totalConversations: number | null;
  returnedCurrentRange: number;
  previousTotalConversations: number | null;
  returnedPreviousRange: number;
  kpisCoverAll: boolean;
  previousKpisCoverAll: boolean;
  onOpen: (conversation: { id: string }) => void;
  empty: React.ReactNode;
}) {
  const costs = new Map(
    conversations.map((conversation) => [conversation.id, conversationCostEstimateByModel(conversation, prices)]),
  );
  const sorted = [...conversations]
    .sort((a, b) => {
      const av = unit === 'cost' ? (costs.get(a.id)?.value ?? -1) : tokenTotal(a.token_buckets);
      const bv = unit === 'cost' ? (costs.get(b.id)?.value ?? -1) : tokenTotal(b.token_buckets);
      return bv - av;
    })
    .slice(0, 6);
  const totalTokens = sumTokens(conversations);
  const totalCost = sumCosts(conversations, prices);
  const coverage = totalConversations != null && totalConversations > returnedCurrentRange;
  const previousCoverage = previousTotalConversations != null && previousTotalConversations > returnedPreviousRange;
  return (
    <SurfaceCard style={{ boxShadow: 'none', minWidth: 0 }}>
      <PanelHeader title="Heaviest sessions" meta={`top ${sorted.length} of ${conversations.length} by ${unit}`} />
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: SESSION_GRID,
          alignItems: 'center',
          gap: 16,
          padding: '10px 18px',
          borderBottom: '1px solid var(--border-weak)',
          background: 'var(--bg-secondary)',
          color: 'var(--fg3)',
          fontSize: 11.5,
        }}
      >
        <span />
        <span>Session</span>
        <span>Workspace</span>
        <span style={{ textAlign: 'right' }}>Tokens</span>
        <span style={{ textAlign: 'right' }}>Duration</span>
        <span>Model</span>
        <span style={{ textAlign: 'right' }}>Cost</span>
      </div>
      {sorted.length === 0 ? (
        <EmptyPanel>{empty}</EmptyPanel>
      ) : (
        sorted.map((conversation, position) => {
          const workspace = splitWorkspacePath(conversation.workspace);
          return (
            <a
              key={conversation.id}
              data-session-id={conversation.id}
              href={conversationPath(conversation.id)}
              onClick={(event) => {
                if (!isPlainLeftClick(event)) return;
                event.preventDefault();
                onOpen({ id: conversation.id });
              }}
              style={{
                display: 'grid',
                gridTemplateColumns: SESSION_GRID,
                alignItems: 'center',
                gap: 16,
                padding: '12px 18px',
                borderBottom: '1px solid var(--border-weak)',
                color: 'inherit',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 12,
                textDecoration: 'none',
              }}
              onMouseEnter={(event) => (event.currentTarget.style.background = 'var(--row-hover)')}
              onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
            >
              <span style={{ color: 'var(--fg3)' }}>{position + 1}</span>
              <span style={{ minWidth: 0, display: 'flex', alignItems: 'center', gap: 7 }}>
                <span
                  style={{
                    minWidth: 0,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                    color: 'var(--fg1)',
                    fontFamily: 'var(--fontFamily)',
                  }}
                >
                  {conversation.title || conversation.id}
                </span>
                {conversation.status === 'err' && <ErrorChip />}
              </span>
              <span
                title={conversation.workspace || '(unknown)'}
                style={{
                  overflow: 'hidden',
                  textOverflow: 'ellipsis',
                  whiteSpace: 'nowrap',
                  color: 'var(--fg2)',
                  fontSize: 11.5,
                }}
              >
                {workspace.leaf}
              </span>
              <span style={{ textAlign: 'right', color: unit === 'tokens' ? 'var(--fg-max)' : 'var(--fg1)' }}>
                {formatTokens(tokenTotal(conversation.token_buckets))}
              </span>
              <span style={{ textAlign: 'right', color: 'var(--fg2)' }}>
                {formatDuration(durationBetweenSeconds(conversation.started_at, conversation.last_activity))}
              </span>
              <span style={{ minWidth: 0 }}>
                {conversation.models?.[0] ? <ModelPill name={conversation.models[0]} /> : '—'}
              </span>
              <span
                title={costEstimateTitle(costs.get(conversation.id) || { value: null, complete: false })}
                style={{ textAlign: 'right', color: unit === 'cost' ? 'var(--fg-max)' : 'var(--fg1)', fontSize: 13 }}
              >
                {formatCostEstimate(costs.get(conversation.id) || { value: null, complete: false })}
              </span>
            </a>
          );
        })
      )}
      <div
        style={{
          padding: '11px 18px',
          color: 'var(--fg3)',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
        }}
      >
        {conversations.length} {conversations.length === 1 ? 'session' : 'sessions'} in range ·{' '}
        {unit === 'cost' ? `${formatCostEstimate(totalCost)} total` : `${formatTokens(totalTokens)} tokens total`}
        {!totalCost.complete && totalCost.value != null && ' · Unpriced usage is excluded.'}
        {coverage && (
          <Fragment>
            {' · '}Coverage: session panels use {returnedCurrentRange} returned{' '}
            {returnedCurrentRange === 1 ? 'session' : 'sessions'} from {totalConversations} in range.{' '}
            {kpisCoverAll ? 'KPI totals, model totals, token charts, and trends' : 'Token charts and trends'} cover all
            generations in range.
          </Fragment>
        )}
        {previousCoverage && !previousKpisCoverAll && (
          <Fragment>
            {' · '}Previous-period comparisons are unavailable: {returnedPreviousRange} of {previousTotalConversations}{' '}
            sessions returned.
          </Fragment>
        )}
      </div>
    </SurfaceCard>
  );
}

type ResolvedAnalyticsViewProps = Omit<AnalyticsViewProps, 'prices'> & { prices: ModelPrices | null };

type AnalyticsHeroSource = Pick<AnalyticsViewProps, 'conversations' | 'aggregate' | 'totalConversations'>;

export function analyticsOverviewHeroStats(props: AnalyticsHeroSource) {
  const agents = new Set<string>();
  const workspaces = new Set<string>();
  for (const conversation of props.conversations) {
    workspaces.add(conversation.workspace || '');
    for (const agent of agentHosts(conversation.agents)) agents.add(agent);
  }
  const sessions =
    props.aggregate && props.totalConversations != null ? props.totalConversations : props.conversations.length;
  return [
    { label: 'Sessions', value: String(sessions) },
    { label: 'Workspaces', value: String(props.aggregate?.workspaces ?? workspaces.size) },
    { label: 'Agents', value: String(props.aggregate?.agents ?? agents.size) },
  ];
}

function AnalyticsContent(props: ResolvedAnalyticsViewProps) {
  const now = props.now ?? Date.now();
  const prices = props.prices;
  const range = timeRangeOption(props.timeRange);
  const selectedCurrent = props.conversations;
  const selectedPrevious = props.previousConversations;
  const facetWorkspaces = useMemo(() => {
    const rows = aggregateWorkspaces(props.facetConversations, prices);
    if (props.workspace == null || rows.some((row) => row.path === props.workspace)) return rows;
    return [...rows, ...aggregateWorkspaces(selectedCurrent, prices)];
  }, [props.facetConversations, props.workspace, selectedCurrent, prices]);
  const workspaceRows = useMemo(() => aggregateWorkspaces(selectedCurrent, prices), [selectedCurrent, prices]);
  const modelRows = useMemo(
    () => aggregateModels(props.aggregate ? [props.aggregate] : selectedCurrent, prices),
    [props.aggregate, selectedCurrent, prices],
  );
  const chartPoints = useMemo(() => props.tokenPoints.filter((point) => tokenTotal(point) > 0), [props.tokenPoints]);
  const currentCost = props.aggregate
    ? conversationCostEstimateByModel(props.aggregate, prices)
    : sumCosts(selectedCurrent, prices);
  const previousCost = props.previousAggregate
    ? conversationCostEstimateByModel(props.previousAggregate, prices)
    : sumCosts(selectedPrevious, prices);
  const currentTokens = props.aggregate ? tokenTotal(props.aggregate.token_buckets) : sumTokens(selectedCurrent);
  const previousTokens = props.previousAggregate
    ? tokenTotal(props.previousAggregate.token_buckets)
    : sumTokens(selectedPrevious);
  const currentCalls =
    props.aggregate?.calls ?? selectedCurrent.reduce((sum, conversation) => sum + (conversation.calls || 0), 0);
  const previousCalls =
    props.previousAggregate?.calls ??
    selectedPrevious.reduce((sum, conversation) => sum + (conversation.calls || 0), 0);
  const selectedTokenTotals = selectedCurrent.reduce(
    (totals, conversation) => {
      for (const series of TOKEN_SERIES) totals[series.key] += conversation.token_buckets?.[series.key] || 0;
      return totals;
    },
    { ...EMPTY_BUCKETS },
  );
  const selectedPreviousTokenTotals = selectedPrevious.reduce(
    (totals, conversation) => {
      for (const series of TOKEN_SERIES) totals[series.key] += conversation.token_buckets?.[series.key] || 0;
      return totals;
    },
    { ...EMPTY_BUCKETS },
  );
  const tokenTotals = props.aggregate?.token_buckets ?? selectedTokenTotals;
  const previousTokenTotals = props.previousAggregate?.token_buckets ?? selectedPreviousTokenTotals;
  const cachePct = cacheInputHitPercent(tokenTotals.fresh_input, tokenTotals.cache_read, tokenTotals.cache_write);
  const previousCachePct = cacheInputHitPercent(
    previousTokenTotals.fresh_input,
    previousTokenTotals.cache_read,
    previousTokenTotals.cache_write,
  );
  const currentCoverageIncomplete =
    props.totalConversations != null && props.totalConversations > props.conversations.length;
  const previousCoverageIncomplete =
    props.previousTotalConversations != null && props.previousTotalConversations > props.previousConversations.length;
  const currentTotalsComplete = props.aggregate != null || !currentCoverageIncomplete;
  const previousTotalsComplete = props.previousAggregate != null || !previousCoverageIncomplete;
  const hidePeriodDeltas =
    range.ms == null ||
    !currentTotalsComplete ||
    !previousTotalsComplete ||
    props.error != null ||
    props.previousError != null;
  const hideCostDelta = hidePeriodDeltas || !currentCost.complete || !previousCost.complete;
  const costSpark = costSparkline(chartPoints, props.timeRange, now, props.tokenIntervalMs, prices);
  const tokenSpark = pointSparkline(chartPoints, props.timeRange, now, props.tokenIntervalMs, tokenTotal);
  const callSpark = pointSparkline(
    props.tokenPoints,
    props.timeRange,
    now,
    props.tokenIntervalMs,
    (point) => point.calls,
  );
  const cacheSpark = cacheSparkline(chartPoints, props.timeRange, now, props.tokenIntervalMs, cachePct);
  const peakCost = costSpark.reduce(
    (best, value) => (value.value > best.value ? value : best),
    costSpark[0] || { key: now, value: 0 },
  );
  const costSparkBucketMs = costSpark[1]
    ? costSpark[1].key - (costSpark[0]?.key || costSpark[1].key)
    : props.tokenIntervalMs || 60 * 60 * 1000;
  const modelCount =
    props.aggregate?.models.length ??
    new Set(selectedCurrent.flatMap((conversation) => conversation.models || [])).size;
  const errored =
    props.aggregate?.errored ?? selectedCurrent.filter((conversation) => conversation.status === 'err').length;
  const currentSessionCount =
    props.aggregate && props.totalConversations != null ? props.totalConversations : selectedCurrent.length;
  const empty = props.loading && selectedCurrent.length === 0 ? 'Loading analytics…' : emptyRangeMessage(range.label);
  const chartEmpty = props.tokenError
    ? `Failed to load token usage: ${props.tokenError}`
    : props.tokenLoading
      ? 'Loading token usage…'
      : selectedCurrent.length === 0
        ? empty
        : 'No token usage in this range.';
  const facetCost = sumCosts(props.facetConversations, prices);

  return (
    <>
      {props.error && (
        <div style={{ marginBottom: 14 }}>
          <Notice kind="error" title="Failed to load current-period sessions">
            {props.error}
          </Notice>
        </div>
      )}
      {props.previousError && (
        <div style={{ marginBottom: 14 }}>
          <Notice kind="warning" title="Previous-period comparisons are unavailable">
            {props.previousError}
          </Notice>
        </div>
      )}
      {props.facetError && (
        <div style={{ marginBottom: 14 }}>
          <Notice kind="warning" title="Workspace options did not refresh">
            {props.facetError}
          </Notice>
        </div>
      )}
      {props.facetTotalConversations != null && props.facetTotalConversations > props.facetConversations.length && (
        <div style={{ marginBottom: 14 }}>
          <Notice kind="warning" title="Workspace options are incomplete">
            Options cover {props.facetConversations.length} of {props.facetTotalConversations} sessions in range.
          </Notice>
        </div>
      )}
      <div
        style={{
          display: 'flex',
          alignItems: 'stretch',
          gap: 8,
          marginBottom: 14,
          flexWrap: 'wrap',
        }}
      >
        <span style={{ alignSelf: 'center', color: 'var(--fg3)', fontSize: 11.5, whiteSpace: 'nowrap' }}>
          Measure in
        </span>
        <UnitToggle value={props.unit} onChange={props.onUnitChange} />
        <div style={{ flex: 1, minWidth: 12 }} />
        <WorkspaceFacet
          workspaces={facetWorkspaces}
          selected={props.workspace}
          onSelect={props.onWorkspaceChange}
          totalCount={props.facetConversations.length}
          totalCost={facetCost.complete ? facetCost.value : null}
          now={now}
          rangeLabel={range.label}
        />
        <TimeRangePicker value={props.timeRange} onChange={props.onTimeRangeChange} />
        <button
          type="button"
          onClick={() => props.onRefresh()}
          disabled={props.refreshing}
          title="Refresh"
          aria-label="Refresh analytics"
          style={{
            ...iconBtn,
            height: 34,
            width: 34,
            flex: '0 0 34px',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            opacity: props.refreshing ? 0.5 : 1,
            cursor: props.refreshing ? 'wait' : 'pointer',
          }}
          onMouseEnter={(event) => {
            if (!props.refreshing) {
              event.currentTarget.style.background = 'var(--action-hover)';
              event.currentTarget.style.color = 'var(--fg1)';
            }
          }}
          onMouseLeave={(event) => {
            event.currentTarget.style.background = 'transparent';
            event.currentTarget.style.color = 'var(--fg2)';
          }}
        >
          <Icon name="refresh" size={14} />
        </button>
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(4, minmax(0, 1fr))',
          gap: 12,
          marginBottom: 12,
        }}
      >
        <KpiCard
          label={
            <span title={ESTIMATED_COST_TOOLTIP} style={{ display: 'inline-flex', alignItems: 'center', gap: 4 }}>
              Estimated spend <Icon name="info" size={12} />
            </span>
          }
          value={formatCostEstimate(currentCost)}
          delta={hideCostDelta ? null : percentageDelta(currentCost.value, previousCost.value)}
          deltaColor={
            currentCost.value != null && previousCost.value != null && currentCost.value > previousCost.value
              ? 'var(--error-text)'
              : 'var(--fg2)'
          }
          bars={costSpark}
          color="var(--spark-orange)"
          peakColor="var(--brand-orange)"
          sub={
            selectedCurrent.length === 0
              ? empty
              : currentCost.value == null
                ? 'No priced model usage in this range.'
                : !currentCost.complete
                  ? 'Partial estimate; unpriced usage is excluded.'
                  : `${formatCost(currentCost.value / Math.max(1, currentSessionCount))} avg / session · peak ${formatCost(peakCost.value)} at ${formatBucketLabel(peakCost.key, costSparkBucketMs)}`
          }
        />
        <KpiCard
          label="Total tokens"
          value={formatTokens(currentTokens)}
          delta={hidePeriodDeltas ? null : percentageDelta(currentTokens, previousTokens)}
          bars={tokenSpark}
          color="var(--spark-green)"
          peakColor="var(--viz-green)"
          sub={
            selectedCurrent.length === 0
              ? empty
              : `${formatTokens(currentTokens / Math.max(1, currentSessionCount))} avg / session · ${modelCount} ${modelCount === 1 ? 'model' : 'models'}`
          }
        />
        <KpiCard
          label="Input cache hit"
          value={cachePct == null ? '—' : `${cachePct}%`}
          delta={hidePeriodDeltas ? null : pointDelta(cachePct, previousCachePct)}
          deltaColor={
            cachePct != null && previousCachePct != null && cachePct < previousCachePct
              ? 'var(--warning-text)'
              : 'var(--fg2)'
          }
          bars={cacheSpark}
          color="var(--spark-green)"
          peakColor="var(--viz-green)"
          sub={
            selectedCurrent.length === 0
              ? empty
              : `${formatTokens(tokenTotals.cache_read)} cache reads · ${formatTokens(tokenTotals.fresh_input)} fresh input`
          }
        />
        <KpiCard
          label="Model calls"
          value={formatInteger(currentCalls)}
          delta={hidePeriodDeltas ? null : percentageDelta(currentCalls, previousCalls)}
          bars={callSpark}
          color="var(--spark-neutral)"
          peakColor="var(--spark-neutral-peak)"
          sub={
            selectedCurrent.length === 0 ? (
              empty
            ) : (
              <Fragment>
                {(currentCalls / Math.max(1, currentSessionCount)).toFixed(1).replace(/\.0$/, '')} avg / session ·{' '}
                <span style={{ color: errored > 0 ? 'var(--error-text)' : 'inherit' }}>
                  {errored} errored {errored === 1 ? 'session' : 'sessions'}
                </span>
              </Fragment>
            )
          }
        />
      </div>

      <div style={{ marginBottom: 12 }}>
        {props.tokenError && props.tokenPoints.length > 0 && (
          <div style={{ marginBottom: 8 }}>
            <Notice kind="warning" title="Token usage did not refresh">
              {props.tokenError}
            </Notice>
          </div>
        )}
        <AnalyticsChart
          points={chartPoints}
          tokenIntervalMs={props.tokenIntervalMs}
          unit={props.unit}
          timeRange={props.timeRange}
          hiddenSeries={props.hiddenSeries}
          onToggleSeries={props.onToggleSeries}
          onOpenBucket={props.onOpenBucket}
          now={now}
          prices={prices}
          emptyMessage={chartEmpty}
        />
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'minmax(0, 1.35fr) minmax(0, 1fr)',
          gap: 12,
          marginBottom: 12,
        }}
      >
        <WorkspacesPanel rows={workspaceRows} unit={props.unit} onOpen={props.onOpenWorkspace} empty={empty} />
        <ModelsPanel rows={modelRows} unit={props.unit} empty={empty} />
      </div>

      <div
        style={{
          display: 'grid',
          gridTemplateColumns: 'repeat(2, minmax(0, 1fr))',
          gap: 12,
          marginBottom: 12,
        }}
      >
        <SessionShapePanel conversations={selectedCurrent} empty={empty} />
        <HeatmapPanel
          points={props.heatmapPoints}
          unit={props.unit}
          prices={prices}
          loading={props.heatmapLoading}
          error={props.heatmapError}
        />
      </div>

      <HeaviestSessionsPanel
        conversations={selectedCurrent}
        unit={props.unit}
        prices={prices}
        totalConversations={props.totalConversations}
        returnedCurrentRange={props.conversations.length}
        previousTotalConversations={props.previousTotalConversations}
        returnedPreviousRange={props.previousConversations.length}
        kpisCoverAll={props.aggregate != null}
        previousKpisCoverAll={props.previousAggregate != null}
        onOpen={props.onOpenConversation}
        empty={empty}
      />
    </>
  );
}

function AnalyticsWithModelPrices(props: AnalyticsViewProps) {
  const prices = useModelPrices();
  return <AnalyticsContent {...props} prices={prices} />;
}

export function AnalyticsOverviewContent(props: AnalyticsViewProps) {
  if (props.prices === undefined) {
    return <AnalyticsWithModelPrices {...props} />;
  }
  return <AnalyticsContent {...props} prices={props.prices} />;
}

export function AnalyticsView(props: AnalyticsViewProps) {
  return (
    <AnalyticsPage stats={analyticsOverviewHeroStats(props)}>
      <AnalyticsOverviewContent {...props} />
    </AnalyticsPage>
  );
}
