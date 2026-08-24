import type React from 'react';
import { Fragment, useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { ActivityBucket, TimeBucket, TimeRangeOption, TimeSpan, TokenBucketTotals } from './formatters';
import {
  bucketActivity,
  bucketTokenUsage,
  cacheInputHitPercent,
  chartGrid,
  chartTooltipLeft,
  conversationCost,
  conversationTime,
  durationBetweenSeconds,
  ESTIMATED_COST_TOOLTIP,
  formatAgo,
  formatBucketLabel,
  formatCost,
  formatDuration,
  formatTokens,
  NO_VALUE,
  splitWorkspacePath,
  TIME_RANGES,
  TOKEN_SERIES,
  timeRangeOption,
  tokenBreakdownTitle,
  tokenPointTime,
  useModelPrices,
  workspaceLabel,
} from './formatters';
import type { SelectOption } from './notices';
import { ACTIVE_PILL_BG, Notice, PageHero, PageShell, PillToggle, Select, Stack, SurfaceCard } from './notices';
import { conversationPath, isPlainLeftClick } from './routing';
import { ConversationSearchPanel, useSearchResults } from './search';
import { HistoryImportBanner } from './settings-screen';
import { AgentCell, agentHosts, Icon, iconBtn, ModelCell } from './shell';
import type { ConversationSummary, ModelPrices, TokenBucketKey, TokenBuckets, TokenUsagePoint } from './types';

/** One bar of the activity chart: the bucket bounds plus its session count. */
interface ActivityChartBucket extends TimeBucket, ActivityBucket {}

/** One bar of the token chart: the bucket bounds plus its per-series sums. */
export interface TokenChartBucket extends TimeBucket, TokenBucketTotals {}

/** The conversation fields a caller needs to open one. */
interface OpenTarget {
  id: string;
  title?: string;
}

/** The list's sort column and direction. */
export interface ListSort {
  key: string;
  dir: 'asc' | 'desc';
}

// ============================================================
// Screen 1 — Conversations list
// ============================================================

// ChartSwitch picks which metric the single chart slot shows. It
// doubles as the chart's title: the active segment names the data.
interface ChartSwitchProps {
  value?: string;
  onChange: (value: string) => void;
}

function ChartSwitch({ value, onChange }: ChartSwitchProps) {
  const options = [
    { value: 'tokens', label: 'Tokens' },
    { value: 'activity', label: 'Sessions' },
  ];
  return <PillToggle options={options} value={value} onChange={onChange} />;
}

// ChartXLabels renders at most ~5 evenly-spaced bucket labels so the
// axis stays readable instead of becoming a wall of timestamps. Empty
// slots keep the flex columns aligned with the bars above them.
interface ChartXLabelsProps {
  data: ReadonlyArray<{ t: string }>;
  gutter?: number;
}

export function ChartXLabels({ data, gutter = 0 }: ChartXLabelsProps) {
  const step = Math.max(1, Math.ceil(data.length / 5));
  return (
    <div
      style={{
        display: 'flex',
        marginLeft: 44,
        marginRight: gutter,
        marginTop: 6,
        fontSize: 10,
        color: 'var(--fg3)',
        fontFamily: 'var(--fontFamilyMonospace)',
      }}
    >
      {data.map((d, i) => {
        const last = i === data.length - 1;
        const show = i % step === 0 || last;
        return (
          <span
            key={i}
            style={{
              flex: 1,
              textAlign: last ? 'right' : 'left',
              overflow: 'hidden',
              whiteSpace: 'nowrap',
            }}
          >
            {show ? d.t : ''}
          </span>
        );
      })}
    </div>
  );
}

// ChartYAxis renders max, midpoint, and zero labels beside a plot.
interface ChartYAxisProps {
  top: string;
  mid: string;
  height?: number;
  side?: 'left' | 'right';
  color?: string;
}

export function ChartYAxis({ top, mid, height = 130, side = 'left', color = 'var(--fg3)' }: ChartYAxisProps) {
  const label: React.CSSProperties = {
    position: 'absolute',
    left: side === 'left' ? 0 : undefined,
    right: side === 'right' ? 0 : undefined,
    width: 34,
    textAlign: side === 'left' ? 'right' : 'left',
    transform: 'translateY(-50%)',
    fontSize: 10,
    lineHeight: '10px',
    color,
    fontFamily: 'var(--fontFamilyMonospace)',
    pointerEvents: 'none',
  };
  return (
    <Fragment>
      <div style={{ ...label, top: 0 }}>{top}</div>
      <div style={{ ...label, top: height / 2 }}>{mid}</div>
      <div style={{ ...label, top: height }}>0</div>
    </Fragment>
  );
}

interface ActivityChartProps {
  data: ActivityChartBucket[];
  bucketLabel: string;
  switcher: React.ReactNode;
  selection?: TimeSpan | null;
  onBucketClick?: (bucket: ActivityChartBucket) => void;
  accent?: string;
}

function ActivityChart({
  data,
  bucketLabel,
  switcher,
  selection,
  onBucketClick,
  accent = 'var(--brand-orange)',
}: ActivityChartProps) {
  const W = 100,
    H = 32;
  const max = Math.max(1, ...data.map((d) => d.c));
  const barW = (W / Math.max(1, data.length)) * 0.7;
  const gap = (W / Math.max(1, data.length)) * 0.3;
  const [hover, setHover] = useState<number | null>(null);
  const hovered = hover === null ? null : (data[hover] ?? null);

  return (
    <SurfaceCard
      style={{
        position: 'relative',
        padding: '16px 20px 12px',
        marginBottom: 0,
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: 10,
        }}
      >
        {switcher}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            fontSize: 11,
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
          }}
        >
          <span
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: 6,
            }}
          >
            <span
              style={{
                width: 10,
                height: 10,
                background: accent,
                borderRadius: 1,
              }}
            />{' '}
            count
          </span>
          <span>{bucketLabel}</span>
        </div>
      </div>
      <div style={{ position: 'relative' }}>
        <ChartYAxis top={String(max)} mid={String(Math.round(max / 2))} />
        <div
          style={{
            marginLeft: 44,
            position: 'relative',
            borderBottom: '1px solid var(--border-medium)',
          }}
        >
          <svg
            viewBox={`0 0 ${W} ${H}`}
            preserveAspectRatio="none"
            style={{ width: '100%', height: 130, display: 'block' }}
          >
            <title>Session activity over time</title>
            {[0, 0.5].map((g) => (
              <line key={g} x1={0} x2={W} y1={H * g} y2={H * g} stroke="rgba(204,204,220,0.06)" strokeWidth="0.2" />
            ))}
            {data.map((d, i) => {
              const h = (d.c / max) * H;
              const x = i * (W / data.length) + gap / 2;
              const y = H - h;
              const isHover = hover === i;
              // Midpoint containment, not overlap: the window shifts a
              // little every render (now moves), so an overlap test can
              // light up two adjacent bars.
              const isSel =
                selection && (d.start + d.end) / 2 >= selection.start && (d.start + d.end) / 2 < selection.end;
              const dim = selection && !isSel;
              return (
                // biome-ignore lint/a11y/useSemanticElements: The bucket must stay in the SVG coordinate system.
                <g
                  key={i}
                  role="button"
                  tabIndex={0}
                  aria-label={`${d.t}: ${d.c} ${d.c === 1 ? 'session' : 'sessions'}. Filter to this time bucket.`}
                  onMouseEnter={() => setHover(i)}
                  onMouseLeave={() => setHover(null)}
                  onFocus={() => setHover(i)}
                  onBlur={() => setHover(null)}
                  onClick={onBucketClick ? () => onBucketClick(d) : undefined}
                  onKeyDown={(e) => {
                    if (onBucketClick && (e.key === 'Enter' || e.key === ' ')) {
                      e.preventDefault();
                      onBucketClick(d);
                    }
                  }}
                  style={{
                    cursor: onBucketClick ? 'pointer' : 'default',
                  }}
                >
                  <rect x={x - 0.4} y={0} width={barW + 0.8} height={H} fill="transparent" />
                  <rect
                    x={x}
                    y={y}
                    width={barW}
                    height={Math.max(h, 0.4)}
                    fill={isHover ? 'var(--brand-orange-text)' : accent}
                    opacity={isHover || isSel ? 1 : dim ? 0.3 : 0.85}
                  />
                </g>
              );
            })}
          </svg>
          {hover !== null && hovered && (
            <div
              style={{
                position: 'absolute',
                left: chartTooltipLeft(hover, data.length),
                transform: 'translate(-50%, -100%)',
                top: -4,
                background: 'var(--bg-secondary)',
                border: '1px solid var(--border-medium)',
                borderRadius: 2,
                padding: '4px 8px',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 11,
                color: 'var(--fg1)',
                whiteSpace: 'nowrap',
                pointerEvents: 'none',
                boxShadow: 'var(--shadow-z2)',
              }}
            >
              <span style={{ color: 'var(--fg3)' }}>{hovered.t}</span> · {hovered.c}{' '}
              {hovered.c === 1 ? 'session' : 'sessions'}
            </div>
          )}
        </div>
        <ChartXLabels data={data} />
      </div>
    </SurfaceCard>
  );
}

// Stacked token-usage-over-time chart. Mirrors ActivityChart's frame
// but stacks the five disjoint token series per bucket, with a
// per-model filter and a click-to-toggle legend. data comes from
// bucketTokenUsage.
interface TokenChartProps {
  data: TokenChartBucket[];
  bucketLabel: string;
  grandTotal: number;
  models: string[];
  model: string;
  onModelChange: (model: string) => void;
  hidden: ReadonlySet<TokenBucketKey>;
  onToggleSeries: (key: TokenBucketKey) => void;
  switcher: React.ReactNode;
  selection?: TimeSpan | null;
  onBucketClick?: (bucket: TokenChartBucket) => void;
}

export function TokenChart({
  data,
  bucketLabel,
  grandTotal,
  models,
  model,
  onModelChange,
  hidden,
  onToggleSeries,
  switcher,
  selection,
  onBucketClick,
}: TokenChartProps) {
  const W = 100,
    H = 32;
  const barW = (W / Math.max(1, data.length)) * 0.7;
  const gap = (W / Math.max(1, data.length)) * 0.3;
  const [hover, setHover] = useState<number | null>(null);
  const hovered = hover === null ? null : (data[hover] ?? null);
  // Only show legend entries for series that actually appear, so a
  // pure-Anthropic store doesn't carry an always-zero "Reasoning"
  // swatch. Fall back to the full set when there's no data at all.
  const present = TOKEN_SERIES.filter((s) => data.some((d) => d[s.key] > 0));
  const legend = present.length ? present : TOKEN_SERIES;
  // Hidden series drop out of the bars, the tooltip, and the y scale,
  // so toggling a dominant series (usually cache reads) rescales the
  // chart to show what's left.
  const visible = TOKEN_SERIES.filter((s) => !hidden.has(s.key));
  const visibleTotal = (d: TokenChartBucket) => visible.reduce((acc, s) => acc + (d[s.key] || 0), 0);
  const max = Math.max(1, ...data.map(visibleTotal));
  const empty = grandTotal === 0;

  return (
    <SurfaceCard
      style={{
        position: 'relative',
        padding: '16px 20px 12px',
        marginBottom: 0,
      }}
    >
      <div
        style={{
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          marginBottom: 10,
          gap: 12,
          flexWrap: 'wrap',
        }}
      >
        {switcher}
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 12,
            fontSize: 11,
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
            flexWrap: 'wrap',
          }}
        >
          {legend.map((s) => {
            const off = hidden.has(s.key);
            return (
              <button
                key={s.key}
                type="button"
                onClick={() => onToggleSeries(s.key)}
                title={off ? `Show ${s.label}` : `Hide ${s.label}`}
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 6,
                  background: 'transparent',
                  border: 'none',
                  padding: 0,
                  cursor: 'pointer',
                  font: 'inherit',
                  color: off ? 'var(--fg3)' : 'inherit',
                  opacity: off ? 0.6 : 1,
                  textDecoration: off ? 'line-through' : 'none',
                }}
              >
                <span
                  style={{
                    width: 10,
                    height: 10,
                    boxSizing: 'border-box',
                    background: off ? 'transparent' : s.color,
                    border: `1px solid ${off ? 'var(--border-medium)' : s.color}`,
                    borderRadius: 1,
                  }}
                />{' '}
                {s.label}
              </button>
            );
          })}
          {models.length > 0 && (
            <Select
              value={model}
              onChange={onModelChange}
              title="Filter by model"
              options={[{ value: 'all', label: 'All models' }, ...models.map((m) => ({ value: m, label: m }))]}
              trigger={{
                height: 24,
                minWidth: 108,
                padding: '0 6px',
                borderRadius: 2,
                background: 'var(--bg-primary)',
                fontSize: 11,
                fontFamily: 'var(--fontFamilyMonospace)',
              }}
              menu={{ fontFamily: 'var(--fontFamilyMonospace)' }}
            />
          )}
          <span>{bucketLabel}</span>
        </div>
      </div>
      <div style={{ position: 'relative' }}>
        {!empty && visible.length > 0 && <ChartYAxis top={formatTokens(max)} mid={formatTokens(Math.round(max / 2))} />}
        <div
          style={{
            marginLeft: 44,
            position: 'relative',
            borderBottom: '1px solid var(--border-medium)',
          }}
        >
          <svg
            viewBox={`0 0 ${W} ${H}`}
            preserveAspectRatio="none"
            style={{ width: '100%', height: 130, display: 'block' }}
          >
            <title>Token usage over time</title>
            {[0, 0.5].map((g) => (
              <line key={g} x1={0} x2={W} y1={H * g} y2={H * g} stroke="rgba(204,204,220,0.06)" strokeWidth="0.2" />
            ))}
            {data.map((d, i) => {
              const x = i * (W / data.length) + gap / 2;
              const isHover = hover === i;
              // Midpoint containment, not overlap — see ActivityChart.
              const isSel =
                selection && (d.start + d.end) / 2 >= selection.start && (d.start + d.end) / 2 < selection.end;
              const dim = selection && !isSel;
              const barOpacity = isHover || isSel ? 1 : dim ? 0.3 : 0.85;
              let yTop = H;
              const segs: React.ReactElement[] = [];
              for (const s of visible) {
                const v = d[s.key] || 0;
                if (v <= 0) continue;
                const h = (v / max) * H;
                yTop -= h;
                segs.push(
                  <rect
                    key={s.key}
                    x={x}
                    y={yTop}
                    width={barW}
                    height={Math.max(h, 0.2)}
                    fill={s.color}
                    opacity={barOpacity}
                  />,
                );
              }
              return (
                // biome-ignore lint/a11y/useSemanticElements: The bucket must stay in the SVG coordinate system.
                <g
                  key={i}
                  role="button"
                  tabIndex={0}
                  aria-label={`${d.t}: ${formatTokens(visibleTotal(d))} tokens. Filter to this time bucket.`}
                  onMouseEnter={() => setHover(i)}
                  onMouseLeave={() => setHover(null)}
                  onFocus={() => setHover(i)}
                  onBlur={() => setHover(null)}
                  onClick={onBucketClick ? () => onBucketClick(d) : undefined}
                  onKeyDown={(e) => {
                    if (onBucketClick && (e.key === 'Enter' || e.key === ' ')) {
                      e.preventDefault();
                      onBucketClick(d);
                    }
                  }}
                  style={{
                    cursor: onBucketClick ? 'pointer' : 'default',
                  }}
                >
                  <rect x={x - 0.4} y={0} width={barW + 0.8} height={H} fill="transparent" />
                  {segs}
                </g>
              );
            })}
          </svg>
          {empty && (
            <div
              style={{
                position: 'absolute',
                top: 0,
                left: 0,
                right: 0,
                height: 130,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'center',
                fontSize: 11,
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                pointerEvents: 'none',
              }}
            >
              No token usage {model !== 'all' ? `for ${model} ` : ''}in this range
            </div>
          )}
          {hover !== null && hovered && visibleTotal(hovered) > 0 && (
            <div
              style={{
                position: 'absolute',
                left: chartTooltipLeft(hover, data.length),
                transform: 'translate(-50%, -100%)',
                top: -4,
                background: 'var(--bg-secondary)',
                border: '1px solid var(--border-medium)',
                borderRadius: 2,
                padding: '6px 8px',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 11,
                color: 'var(--fg1)',
                whiteSpace: 'nowrap',
                pointerEvents: 'none',
                boxShadow: 'var(--shadow-z2)',
                zIndex: 1,
              }}
            >
              <div style={{ color: 'var(--fg3)', marginBottom: 4 }}>
                {hovered.t} · {formatTokens(visibleTotal(hovered))} tok
              </div>
              {visible
                .filter((s) => hovered[s.key] > 0)
                .map((s) => (
                  <div
                    key={s.key}
                    style={{
                      display: 'flex',
                      alignItems: 'center',
                      gap: 8,
                    }}
                  >
                    <span
                      style={{
                        width: 8,
                        height: 8,
                        background: s.color,
                        borderRadius: 1,
                      }}
                    />
                    <span style={{ color: 'var(--fg2)' }}>{s.label}</span>
                    <span
                      style={{
                        marginLeft: 'auto',
                        color: 'var(--fg1)',
                      }}
                    >
                      {formatTokens(hovered[s.key])}
                    </span>
                  </div>
                ))}
            </div>
          )}
        </div>
        <ChartXLabels data={data} />
      </div>
    </SurfaceCard>
  );
}

interface TimeRangePickerProps {
  value?: string;
  onChange: (value: string) => void;
  ranges?: TimeRangeOption[];
}

export function TimeRangePicker({ value, onChange, ranges = TIME_RANGES }: TimeRangePickerProps) {
  const [open, setOpen] = useState(false);
  // TIME_RANGES is never empty, so the last fallback always resolves.
  const selected = (ranges.find((r) => r.value === value) ||
    ranges[ranges.length - 1] ||
    TIME_RANGES[0]) as TimeRangeOption;
  return (
    <div style={{ position: 'relative', flex: '0 0 auto' }}>
      <button
        type="button"
        onClick={() => setOpen((o) => !o)}
        onBlur={(e) => {
          if (!e.currentTarget.parentElement?.contains(e.relatedTarget)) setOpen(false);
        }}
        title="Time range"
        style={{
          height: 34,
          minWidth: 166,
          padding: '0 10px',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          background: 'rgba(24,27,31,0.78)',
          color: 'var(--fg1)',
          fontSize: 13,
          fontFamily: 'var(--fontFamily)',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 10,
          cursor: 'pointer',
        }}
      >
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 8,
          }}
        >
          <Icon name="clock" size={14} style={{ color: 'var(--fg3)' }} />
          {selected.label}
        </span>
        <Icon name="chevron" size={14} style={{ color: 'var(--fg3)' }} />
      </button>
      {open && (
        <div
          style={{
            position: 'absolute',
            top: 39,
            right: 0,
            zIndex: 30,
            minWidth: 190,
            padding: 4,
            border: '1px solid var(--border-strong)',
            borderRadius: 2,
            background: 'var(--bg-secondary)',
            boxShadow: '0 12px 34px rgba(0,0,0,0.48)',
          }}
        >
          {ranges.map((r) => {
            const active = r.value === selected.value;
            return (
              <button
                key={r.value}
                type="button"
                onMouseDown={(e) => e.preventDefault()}
                onClick={() => {
                  onChange(r.value);
                  setOpen(false);
                }}
                style={{
                  width: '100%',
                  height: 30,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  gap: 10,
                  padding: '0 9px',
                  border: 'none',
                  borderRadius: 5,
                  background: active ? ACTIVE_PILL_BG : 'transparent',
                  color: active ? 'var(--primary-text)' : 'var(--fg1)',
                  fontSize: 12,
                  fontFamily: 'var(--fontFamily)',
                  cursor: 'pointer',
                  textAlign: 'left',
                }}
              >
                <span>{r.label}</span>
                {active && <Icon name="check" size={12} />}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}

export const GROUP_BY_OPTIONS: SelectOption[] = [
  { value: 'workspace', label: 'Workspace' },
  { value: 'agent', label: 'Agent' },
  { value: 'model', label: 'Model' },
  { value: 'day', label: 'Day' },
  { value: 'none', label: 'None' },
];

/** A workspace's running totals while they accumulate over a range. */
interface WorkspaceTotals {
  path: string;
  count: number;
  cost: number;
  /** False once a session in the workspace ran on a model with no price. */
  costComplete: boolean;
  tokens: number;
  dur: number;
  /** Last activity, in epoch milliseconds. */
  last: number;
}

/** One row of the workspace facet: cost is null when nothing in it could be priced. */
export interface WorkspaceAggregate extends Omit<WorkspaceTotals, 'cost'> {
  cost: number | null;
}

interface WorkspaceFacetProps {
  workspaces: WorkspaceAggregate[];
  selected?: string | null;
  onSelect: (path: string | null) => void;
  totalCount: number;
  totalCost: number | null;
  now: number;
  rangeLabel: string;
}

export function WorkspaceFacet({
  workspaces,
  selected,
  onSelect,
  totalCount,
  totalCost,
  now,
  rangeLabel,
}: WorkspaceFacetProps) {
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');
  const [cursor, setCursor] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const inputRef = useRef<HTMLInputElement>(null);
  const listRef = useRef<HTMLDivElement>(null);
  const optionRefs = useRef(new Map<number, HTMLButtonElement>());
  const selectedPath = selected == null ? null : splitWorkspacePath(selected);
  const selectedParent = selectedPath?.dir
    ? selectedPath.dir === '/'
      ? '/'
      : `${splitWorkspacePath(selectedPath.dir).leaf}/`
    : '';
  const shown = useMemo(() => {
    const q = filter.trim().toLowerCase();
    return q ? workspaces.filter((w) => (w.path || '').toLowerCase().includes(q)) : workspaces;
  }, [workspaces, filter]);
  const selectedInRange = workspaces.some((w) => w.path === selected);
  const noMatches = filter.trim().length > 0 && shown.length === 0;
  const activeOptionId =
    cursor < 0 ? undefined : cursor === 0 ? 'workspace-facet-option-all' : `workspace-facet-option-${cursor - 1}`;

  useEffect(() => {
    setCursor((current) => (noMatches ? -1 : Math.max(0, Math.min(current, shown.length))));
  }, [shown.length, noMatches]);

  // shown.length is a trigger, not a value the body reads: the option the cursor
  // points at moves when the list is refiltered, so the scroll has to be redone.
  // biome-ignore lint/correctness/useExhaustiveDependencies: shown.length re-runs the scroll
  useEffect(() => {
    if (!open || cursor <= 0) return;
    const list = listRef.current;
    const option = optionRefs.current.get(cursor);
    if (!list || !option) return;
    const listRect = list.getBoundingClientRect();
    const optionRect = option.getBoundingClientRect();
    if (optionRect.top < listRect.top) list.scrollTop -= listRect.top - optionRect.top;
    else if (optionRect.bottom > listRect.bottom) list.scrollTop += optionRect.bottom - listRect.bottom;
  }, [open, cursor, shown.length]);

  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus && buttonRef.current) buttonRef.current.focus();
  };
  const openMenu = () => {
    const selectedIndex = workspaces.findIndex((w) => w.path === selected);
    setFilter('');
    setCursor(selectedIndex < 0 ? 0 : selectedIndex + 1);
    setOpen(true);
    setTimeout(() => inputRef.current?.focus(), 0);
  };
  const pick = (path: string | null) => {
    onSelect(path);
    close(true);
  };
  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (!open) {
      if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown') {
        e.preventDefault();
        openMenu();
      }
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      close(true);
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      if (!noMatches) setCursor((current) => Math.min(shown.length, Math.max(0, current + 1)));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      if (!noMatches) setCursor((current) => Math.max(0, current - 1));
      return;
    }
    if ((e.key === 'Home' || e.key === 'End') && e.target === inputRef.current) return;
    if (e.key === 'Home') {
      e.preventDefault();
      setCursor(noMatches ? -1 : 0);
      return;
    }
    if (e.key === 'End') {
      e.preventDefault();
      setCursor(noMatches ? -1 : shown.length);
      return;
    }
    if (e.key === 'Enter') {
      e.preventDefault();
      if (cursor < 0) return;
      if (cursor === 0) {
        pick(null);
        return;
      }
      const workspace = shown[cursor - 1];
      if (workspace) pick(workspace.path);
    }
  };

  const triggerCount = selectedPath ? `${selectedInRange ? 1 : 0}/${workspaces.length}` : String(workspaces.length);

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: Child controls own focus; the wrapper handles their keyboard events.
    <div
      ref={rootRef}
      role="presentation"
      style={{ position: 'relative', flex: '0 0 auto' }}
      onBlur={(e) => {
        if (!rootRef.current?.contains(e.relatedTarget)) setOpen(false);
      }}
      onKeyDown={onKeyDown}
    >
      <button
        ref={buttonRef}
        type="button"
        title="Filter by workspace"
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? close(false) : openMenu())}
        style={{
          height: 34,
          minWidth: 198,
          maxWidth: 360,
          padding: '0 10px',
          border: `1px solid ${selectedPath ? 'var(--primary-border)' : 'var(--border-medium)'}`,
          borderRadius: 2,
          background: 'rgba(24,27,31,0.78)',
          color: 'var(--fg1)',
          fontSize: 13,
          fontFamily: 'var(--fontFamily)',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 10,
          cursor: 'pointer',
        }}
      >
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 8,
            minWidth: 0,
          }}
        >
          <Icon name="box" size={14} style={{ color: 'var(--fg3)' }} />
          {selectedPath ? (
            <Fragment>
              <span
                style={{
                  color: 'var(--fg-max)',
                  whiteSpace: 'nowrap',
                }}
              >
                {selectedPath.leaf}
              </span>
              {selectedParent && (
                <span
                  style={{
                    minWidth: 0,
                    overflow: 'hidden',
                    textOverflow: 'ellipsis',
                    whiteSpace: 'nowrap',
                    color: 'var(--fg3)',
                    fontFamily: 'var(--fontFamilyMonospace)',
                    fontSize: 10.5,
                  }}
                >
                  {selectedParent}
                </span>
              )}
            </Fragment>
          ) : (
            <span style={{ whiteSpace: 'nowrap' }}>All workspaces</span>
          )}
        </span>
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 8,
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            whiteSpace: 'nowrap',
          }}
        >
          {triggerCount}
          <Icon name="chevron" size={14} />
        </span>
      </button>
      {open && (
        <div
          style={{
            position: 'absolute',
            top: 'calc(100% + 5px)',
            left: 0,
            zIndex: 30,
            width: 420,
            padding: 6,
            border: '1px solid var(--border-strong)',
            borderRadius: 2,
            background: 'var(--bg-secondary)',
            boxShadow: '0 12px 34px rgba(0,0,0,0.48)',
          }}
        >
          <div
            style={{
              height: 30,
              display: 'flex',
              alignItems: 'center',
              gap: 7,
              padding: '0 9px',
              background: 'rgba(17,18,23,0.42)',
              borderRadius: 2,
              color: 'var(--fg3)',
            }}
          >
            <Icon name="search" size={13} />
            <input
              ref={inputRef}
              value={filter}
              onChange={(e) => {
                const next = e.target.value;
                const q = next.trim().toLowerCase();
                setFilter(next);
                setCursor(
                  q.length === 0 ? 0 : workspaces.some((w) => (w.path || '').toLowerCase().includes(q)) ? 1 : -1,
                );
              }}
              placeholder="Filter workspaces…"
              role="combobox"
              aria-label="Filter workspaces"
              aria-autocomplete="list"
              aria-controls="workspace-facet-listbox"
              aria-expanded={open}
              aria-activedescendant={activeOptionId}
              style={{
                flex: 1,
                minWidth: 0,
                border: 'none',
                outline: 'none',
                background: 'transparent',
                color: 'var(--fg1)',
                fontFamily: 'var(--fontFamily)',
                fontSize: 12,
              }}
            />
          </div>
          <div id="workspace-facet-listbox" role="listbox" aria-label="Workspaces">
            <button
              id="workspace-facet-option-all"
              type="button"
              role="option"
              aria-selected={selected == null}
              onMouseDown={(e) => e.preventDefault()}
              onMouseEnter={() => setCursor(0)}
              onClick={() => pick(null)}
              style={{
                width: '100%',
                minHeight: 34,
                display: 'flex',
                alignItems: 'center',
                justifyContent: 'space-between',
                gap: 12,
                padding: '7px 9px',
                border: 'none',
                borderRadius: 2,
                background: cursor === 0 ? ACTIVE_PILL_BG : 'transparent',
                color: selected == null ? 'var(--primary-text)' : 'var(--fg1)',
                cursor: 'pointer',
                fontFamily: 'var(--fontFamily)',
                fontSize: 12.5,
                textAlign: 'left',
              }}
            >
              <span>All workspaces</span>
              <span
                style={{
                  color: 'var(--fg3)',
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 11,
                }}
              >
                {totalCount} · {formatCost(totalCost)}
              </span>
            </button>
            <div
              style={{
                height: 1,
                margin: '5px 4px',
                background: 'var(--border-weak)',
              }}
            />
            <div ref={listRef} style={{ maxHeight: 296, overflowY: 'auto' }}>
              {shown.map((w, i) => {
                const path = splitWorkspacePath(w.path);
                const active = selected === w.path;
                const shareAvailable = totalCost != null && totalCost > 0 && w.cost != null;
                const share = shareAvailable && w.cost != null && totalCost != null ? w.cost / totalCost : null;
                const pct = share == null ? null : Math.round(share * 100);
                return (
                  <button
                    key={w.path || '(unknown)'}
                    id={`workspace-facet-option-${i}`}
                    ref={(node) => {
                      if (node) optionRefs.current.set(i + 1, node);
                      else optionRefs.current.delete(i + 1);
                    }}
                    type="button"
                    role="option"
                    aria-selected={active}
                    title={w.path || '(unknown)'}
                    onMouseDown={(e) => e.preventDefault()}
                    onMouseEnter={() => setCursor(i + 1)}
                    onClick={() => pick(w.path)}
                    style={{
                      width: '100%',
                      display: 'grid',
                      gridTemplateColumns: '1fr 54px 62px 56px',
                      alignItems: 'center',
                      gap: 10,
                      padding: '7px 9px',
                      border: 'none',
                      borderRadius: 2,
                      background: cursor === i + 1 ? ACTIVE_PILL_BG : 'transparent',
                      color: active ? 'var(--primary-text)' : 'var(--fg1)',
                      cursor: 'pointer',
                      textAlign: 'left',
                    }}
                  >
                    <span
                      style={{
                        minWidth: 0,
                        display: 'flex',
                        flexDirection: 'column',
                        gap: 2,
                      }}
                    >
                      <span
                        style={{
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap',
                          color: active ? 'var(--primary-text)' : 'var(--fg-max)',
                          fontFamily: 'var(--fontFamily)',
                          fontSize: 12.5,
                        }}
                      >
                        {path.leaf}
                      </span>
                      <span
                        style={{
                          overflow: 'hidden',
                          textOverflow: 'ellipsis',
                          whiteSpace: 'nowrap',
                          direction: 'rtl',
                          textAlign: 'left',
                          color: 'var(--fg3)',
                          fontFamily: 'var(--fontFamilyMonospace)',
                          fontSize: 10,
                        }}
                      >
                        {path.dir || NO_VALUE}
                      </span>
                    </span>
                    <span
                      style={{
                        color: 'var(--fg2)',
                        fontFamily: 'var(--fontFamilyMonospace)',
                        fontSize: 11,
                        textAlign: 'right',
                      }}
                    >
                      {w.count}
                    </span>
                    <span
                      style={{
                        color: 'var(--fg1)',
                        fontFamily: 'var(--fontFamilyMonospace)',
                        fontSize: 11,
                        textAlign: 'right',
                      }}
                    >
                      {formatCost(w.cost)}
                    </span>
                    <span
                      style={{
                        minWidth: 0,
                        display: 'flex',
                        flexDirection: 'column',
                        gap: 4,
                        color: 'var(--fg3)',
                        fontFamily: 'var(--fontFamilyMonospace)',
                        fontSize: 10,
                        textAlign: 'right',
                      }}
                    >
                      <span>{formatAgo(w.last ? new Date(w.last).toISOString() : null, now)}</span>
                      <span
                        role="img"
                        title={pct == null ? 'Spend share unavailable' : `${pct}% of range spend`}
                        aria-label={pct == null ? 'Spend share unavailable' : `${pct}% of range spend`}
                        style={{
                          display: 'block',
                          height: 2,
                          borderRadius: 2,
                          background: 'rgba(204,204,220,0.08)',
                          overflow: 'hidden',
                        }}
                      >
                        <span
                          style={{
                            display: 'block',
                            width: `${Math.max(0, Math.min(100, pct || 0))}%`,
                            height: '100%',
                            background: 'var(--brand-orange)',
                          }}
                        />
                      </span>
                    </span>
                  </button>
                );
              })}
              {shown.length === 0 && (
                <div
                  style={{
                    padding: '14px 9px',
                    color: 'var(--fg3)',
                    fontFamily: 'var(--fontFamilyMonospace)',
                    fontSize: 11,
                  }}
                >
                  No matching workspaces.
                </div>
              )}
            </div>
          </div>
          <div
            style={{
              display: 'flex',
              justifyContent: 'space-between',
              gap: 12,
              padding: '7px 9px 2px',
              color: 'var(--fg3)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 10.5,
            }}
          >
            <span>
              {workspaces.length} workspaces · {rangeLabel}
            </span>
            <span>sessions · cost · share</span>
          </div>
        </div>
      )}
    </div>
  );
}

interface FilterBarProps {
  query: string;
  onQueryChange: (value: string) => void;
  inputRef?: React.RefObject<HTMLInputElement>;
  /** null while a text search replaces the range filters. */
  timeRange?: string | null;
  onTimeRangeChange?: ((value: string) => void) | null;
  workspaces?: WorkspaceAggregate[];
  workspace?: string | null;
  onWorkspaceChange?: (path: string | null) => void;
  workspaceSessionCount?: number;
  workspaceTotalCost?: number | null;
  now: number;
  rangeLabel: string;
  groupBy?: string;
  onGroupByChange?: (value: string) => void;
  agentFilter?: string;
  onAgentFilterChange?: (value: string) => void;
  agentOptions?: string[];
  modelFilter?: string;
  onModelFilterChange?: (value: string) => void;
  modelOptions?: string[];
  statusFilter?: string;
  onStatusFilterChange?: (value: string) => void;
  activeFilterCount?: number;
  onClearFilters?: () => void;
  onRefresh?: () => void;
  refreshing?: boolean;
  placeholder?: string;
  onInputKeyDown?: (e: React.KeyboardEvent<HTMLInputElement>) => void;
  rightAdornment?: React.ReactNode;
}

function FilterBar({
  query,
  onQueryChange,
  inputRef,
  timeRange,
  onTimeRangeChange,
  workspaces = [],
  workspace,
  onWorkspaceChange,
  workspaceSessionCount = 0,
  workspaceTotalCost = 0,
  now,
  rangeLabel,
  groupBy,
  onGroupByChange,
  agentFilter = 'all',
  onAgentFilterChange,
  agentOptions = [],
  modelFilter = 'all',
  onModelFilterChange,
  modelOptions = [],
  statusFilter = 'all',
  onStatusFilterChange,
  activeFilterCount = 0,
  onClearFilters,
  onRefresh,
  refreshing,
  placeholder = 'Filter by title, id, workspace, agent, model…',
  onInputKeyDown,
  rightAdornment,
}: FilterBarProps) {
  const showTimeRange = !!timeRange && !!onTimeRangeChange;
  const showWorkspaceFacet = !!onWorkspaceChange;
  const showGroupBy = !!onGroupByChange;
  const showAgentFilter = !!onAgentFilterChange;
  const showModelFilter = !!onModelFilterChange;
  const showStatusFilter = !!onStatusFilterChange;
  const selectStyle: React.CSSProperties = {
    height: 34,
    minWidth: 132,
    padding: '0 30px 0 11px',
    border: '1px solid var(--border-medium)',
    borderRadius: 2,
    background: 'rgba(24,27,31,0.78)',
    color: 'var(--fg1)',
    fontSize: 13,
    fontFamily: 'var(--fontFamily)',
  };
  return (
    <Stack direction="row" align="stretch" gap={8} style={{ marginBottom: 16, fontSize: 13, flexWrap: 'wrap' }}>
      <Stack
        direction="row"
        align="center"
        gap={8}
        style={{
          flex: '1 1 260px',
          padding: '0 11px',
          height: 34,
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          background: 'rgba(24,27,31,0.78)',
          color: 'var(--fg3)',
          boxShadow: 'inset 0 0 0 1px rgba(0,0,0,0.12)',
        }}
      >
        <Icon name="search" size={14} />
        <input
          ref={inputRef}
          value={query}
          onChange={(e) => onQueryChange(e.target.value)}
          onKeyDown={onInputKeyDown}
          placeholder={placeholder}
          style={{
            flex: 1,
            background: 'transparent',
            border: 'none',
            outline: 'none',
            color: 'var(--fg1)',
            fontSize: 13,
            fontFamily: 'var(--fontFamily)',
          }}
        />
        {rightAdornment !== undefined ? (
          rightAdornment
        ) : (
          <span
            title="Press Command-K or Control-K to focus search"
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: 'var(--fg3)',
              padding: '1px 6px',
              border: '1px solid var(--border-weak)',
              borderRadius: 2,
            }}
          >
            ⌘K
          </span>
        )}
      </Stack>
      {showWorkspaceFacet && (
        <WorkspaceFacet
          workspaces={workspaces}
          selected={workspace}
          onSelect={onWorkspaceChange}
          totalCount={workspaceSessionCount}
          totalCost={workspaceTotalCost}
          now={now}
          rangeLabel={rangeLabel}
        />
      )}
      {showTimeRange && <TimeRangePicker value={timeRange} onChange={onTimeRangeChange} />}
      {showGroupBy && (
        <Select
          value={groupBy}
          onChange={onGroupByChange}
          title="Group sessions"
          icon="sortlines"
          prefix="Group by"
          trigger={{ ...selectStyle, minWidth: 196, padding: '0 10px' }}
          options={GROUP_BY_OPTIONS}
        />
      )}
      {showAgentFilter && (
        <Select
          value={agentFilter}
          onChange={onAgentFilterChange}
          title="Filter by agent"
          trigger={selectStyle}
          options={[{ value: 'all', label: 'All agents' }, ...agentOptions.map((a) => ({ value: a, label: a }))]}
        />
      )}
      {showModelFilter && (
        <Select
          value={modelFilter}
          onChange={onModelFilterChange}
          title="Filter by model"
          trigger={{ ...selectStyle, minWidth: 150 }}
          options={[{ value: 'all', label: 'All models' }, ...modelOptions.map((m) => ({ value: m, label: m }))]}
        />
      )}
      {showStatusFilter && (
        <Select
          value={statusFilter}
          onChange={onStatusFilterChange}
          title="Filter by status"
          trigger={selectStyle}
          options={[
            { value: 'all', label: 'All status' },
            { value: 'errors', label: 'Errors' },
            { value: 'subagents', label: 'Has subagents' },
          ]}
        />
      )}
      {activeFilterCount > 0 && onClearFilters && (
        <button
          type="button"
          onClick={onClearFilters}
          style={{
            ...iconBtn,
            width: 'auto',
            height: 34,
            padding: '0 11px',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            color: 'var(--fg2)',
            gap: 6,
            flex: '0 0 auto',
            whiteSpace: 'nowrap',
          }}
          title="Clear session filters"
          onMouseEnter={(e) => {
            e.currentTarget.style.background = 'var(--action-hover)';
            e.currentTarget.style.color = 'var(--fg1)';
          }}
          onMouseLeave={(e) => {
            e.currentTarget.style.background = 'transparent';
            e.currentTarget.style.color = 'var(--fg2)';
          }}
        >
          <Icon name="close" size={13} />
          Clear
        </button>
      )}
      <button
        type="button"
        onClick={onRefresh}
        disabled={refreshing}
        style={{
          ...iconBtn,
          height: 34,
          width: 34,
          flex: '0 0 34px',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          opacity: refreshing ? 0.5 : 1,
          cursor: refreshing ? 'wait' : 'pointer',
        }}
        title="Refresh"
        onMouseEnter={(e) => {
          if (!refreshing) {
            e.currentTarget.style.background = 'var(--action-hover)';
            e.currentTarget.style.color = 'var(--fg1)';
          }
        }}
        onMouseLeave={(e) => {
          e.currentTarget.style.background = 'transparent';
          e.currentTarget.style.color = 'var(--fg2)';
        }}
      >
        <Icon name="refresh" size={14} />
      </button>
    </Stack>
  );
}

interface ConvRowProps {
  c: ConversationSummary;
  now: number;
  onOpen: (c: ConversationSummary) => void;
  prices: ModelPrices | null;
  grouped?: boolean;
  hideWorkspace?: boolean;
}

export function ConvRow({ c, now, onOpen, prices, grouped = false, hideWorkspace = false }: ConvRowProps) {
  const wallSec = durationBetweenSeconds(c.started_at, c.last_activity);
  return (
    <a
      href={conversationPath(c.id)}
      onClick={(e) => {
        if (!isPlainLeftClick(e)) return;
        e.preventDefault();
        onOpen(c);
      }}
      style={{
        display: 'grid',
        gridTemplateColumns: CONV_GRID,
        alignItems: 'center',
        gap: 16,
        padding: grouped ? '12px 16px 12px 40px' : '12px 16px',
        borderBottom: '1px solid var(--border-weak)',
        background: 'transparent',
        cursor: 'pointer',
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 12,
        transition: 'background 80ms ease',
        textDecoration: 'none',
        color: 'inherit',
      }}
      onMouseEnter={(e) => (e.currentTarget.style.background = 'rgba(204,204,220,0.03)')}
      onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
    >
      <span style={{ color: 'var(--fg2)' }}>{formatAgo(c.last_activity, now)}</span>
      <div
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: 2,
          minWidth: 0,
        }}
      >
        <span
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 7,
            minWidth: 0,
          }}
        >
          <span
            style={{
              fontFamily: 'var(--fontFamily)',
              color: 'var(--fg1)',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {c.title || c.id}
          </span>
          {(c.subagents ?? 0) > 0 && (
            <span
              title={`${c.subagents} subagent ${c.subagents === 1 ? 'step' : 'steps'}`}
              style={{
                flexShrink: 0,
                display: 'inline-flex',
                alignItems: 'center',
                gap: 3,
                padding: '0 6px',
                height: 16,
                borderRadius: 2,
                background: 'rgba(204,204,220,0.06)',
                color: 'var(--fg2)',
                fontSize: 10,
                fontFamily: 'var(--fontFamilyMonospace)',
              }}
            >
              ⊂ {c.subagents}
            </span>
          )}
        </span>
        {!hideWorkspace && (
          <span
            style={{
              color: 'var(--fg3)',
              fontSize: 11,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {c.workspace ? workspaceLabel(c.workspace) : c.id}
          </span>
        )}
      </div>
      <AgentCell agents={c.agents} />
      <span style={{ color: 'var(--fg1)' }} title={ESTIMATED_COST_TOOLTIP}>
        {formatCost(conversationCost(c, prices))}
      </span>
      <span
        style={{ display: 'inline-flex', alignItems: 'center', gap: 7 }}
        title={tokenBreakdownTitle(c.token_buckets)}
      >
        <span style={{ color: 'var(--fg1)' }}>{formatTokens(c.total_tokens)}</span>
        {c.status === 'err' && (
          <span
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              padding: '0 6px',
              height: 16,
              borderRadius: 2,
              background: 'var(--error-transparent)',
              color: 'var(--error-text)',
              fontSize: 10,
              letterSpacing: '0.04em',
            }}
          >
            ERR
          </span>
        )}
      </span>
      <span style={{ color: 'var(--fg2)' }}>
        <span style={{ color: 'var(--fg1)' }}>{formatDuration(wallSec)}</span>
        <span style={{ color: 'var(--fg3)', padding: '0 6px' }}>·</span>
        <span style={{ color: 'var(--fg1)' }}>
          {c.calls} {c.calls === 1 ? 'call' : 'calls'}
        </span>
      </span>
      <ModelCell models={c.models} />
    </a>
  );
}

// Shared by ConvRow and its header so the columns stay aligned:
// Last activity · Conversation · Agent · Estimated cost · Tokens · Duration · Models.
// Agent shows the host launcher only (claude-code, …) — not the per-
// subagent rows, which were the noise; subagent presence is the ⊂N badge.
const CONV_GRID = '84px minmax(260px, 1.4fr) 132px 118px 96px 136px minmax(220px, 1.2fr)';
const OPEN_GROUPS = 3;

// Use the full sorted agent or model set as one key so each session appears once.
function groupKeyFor(c: ConversationSummary, groupBy: string): string {
  if (groupBy === 'workspace') return c.workspace || '';
  if (groupBy === 'agent') return agentHosts(c.agents).sort().join(' + ') || '(unknown)';
  if (groupBy === 'model') return [...new Set(c.models || [])].filter(Boolean).sort().join(' + ') || '(unknown)';
  if (groupBy === 'day') {
    const t = conversationTime(c);
    if (t == null) return '(unknown)';
    const d = new Date(t);
    return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`;
  }
  return '';
}

interface SessionGroupHeaderProps {
  groupBy: string;
  label: string;
  open?: boolean;
  onToggle: () => void;
  count: number;
  cost: number | null;
  tokens: number;
  /** Last activity, in epoch milliseconds. */
  last: number;
  share: number | null;
  now: number;
}

function SessionGroupHeader({
  groupBy,
  label,
  open,
  onToggle,
  count,
  cost,
  tokens,
  last,
  share,
  now,
}: SessionGroupHeaderProps) {
  const path = groupBy === 'workspace' ? splitWorkspacePath(label) : null;
  const pct = share == null ? null : Math.round(share * 100);
  return (
    <button
      type="button"
      aria-expanded={open}
      onClick={onToggle}
      style={{
        width: '100%',
        display: 'grid',
        gridTemplateColumns: 'minmax(0, 1fr) auto',
        alignItems: 'center',
        gap: 16,
        padding: '10px 16px',
        border: 'none',
        borderBottom: '1px solid var(--border-weak)',
        background: 'rgba(34,37,43,0.55)',
        color: 'inherit',
        cursor: 'pointer',
        textAlign: 'left',
        font: 'inherit',
      }}
      onMouseEnter={(e) => {
        e.currentTarget.style.background = 'rgba(34,37,43,0.8)';
      }}
      onMouseLeave={(e) => {
        e.currentTarget.style.background = 'rgba(34,37,43,0.55)';
      }}
    >
      <span
        style={{
          minWidth: 0,
          display: 'flex',
          alignItems: 'center',
          gap: 9,
          overflow: 'hidden',
        }}
      >
        <Icon name={open ? 'chevron' : 'cright'} size={13} style={{ color: 'var(--fg3)' }} />
        {path ? (
          <span
            title={label || '(unknown)'}
            style={{
              minWidth: 0,
              flex: '1 1 auto',
              display: 'inline-flex',
              alignItems: 'baseline',
              gap: 5,
              overflow: 'hidden',
              whiteSpace: 'nowrap',
            }}
          >
            <span
              style={{
                minWidth: 0,
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                direction: 'rtl',
                textAlign: 'left',
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 11.5,
              }}
            >
              {path.dir}
            </span>
            <span
              style={{
                color: 'var(--fg-max)',
                fontFamily: 'var(--fontFamily)',
                fontSize: 13,
                fontWeight: 600,
              }}
            >
              {path.leaf}
            </span>
          </span>
        ) : (
          <span
            title={label || '(unknown)'}
            style={{
              minWidth: 0,
              flex: '1 1 auto',
              color: 'var(--fg-max)',
              fontFamily: 'var(--fontFamily)',
              fontSize: 13,
              fontWeight: 600,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {label}
          </span>
        )}
        <span
          style={{
            width: 120,
            height: 3,
            flex: '0 0 auto',
            borderRadius: 2,
            background: 'rgba(204,204,220,0.08)',
            overflow: 'hidden',
          }}
        >
          <span
            style={{
              display: 'block',
              height: '100%',
              width: `${Math.max(0, Math.min(100, pct || 0))}%`,
              background: 'var(--brand-orange)',
            }}
          />
        </span>
        <span
          style={{
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            whiteSpace: 'nowrap',
          }}
        >
          {pct == null ? 'Spend share unavailable' : `${pct}% of spend`}
        </span>
      </span>
      <span
        style={{
          display: 'flex',
          alignItems: 'center',
          gap: 16,
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11.5,
          whiteSpace: 'nowrap',
        }}
      >
        <span style={{ color: 'var(--fg2)' }}>
          {count} {count === 1 ? 'session' : 'sessions'}
        </span>
        <span style={{ color: 'var(--fg1)' }}>{formatCost(cost)}</span>
        <span style={{ color: 'var(--fg2)' }}>{formatTokens(tokens)}</span>
        <span style={{ color: 'var(--fg3)' }}>{formatAgo(last ? new Date(last).toISOString() : null, now)}</span>
      </span>
    </button>
  );
}

interface WorkspaceContextStripProps {
  path: string;
  count: number;
  cost: number | null;
  tokens: number;
  /** Last activity, in epoch milliseconds. */
  last: number;
  share: number | null;
  now: number;
  onClear: () => void;
}

function WorkspaceContextStrip({ path, count, cost, tokens, last, share, now, onClear }: WorkspaceContextStripProps) {
  const label = splitWorkspacePath(path);
  const pct = share == null ? null : Math.round(share * 100);
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 14,
        padding: '10px 16px',
        borderBottom: '1px solid var(--border-weak)',
        background: 'rgba(34,37,43,0.55)',
        color: 'var(--fg2)',
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 11.5,
      }}
    >
      <span
        title={path || '(unknown)'}
        style={{
          minWidth: 0,
          flex: '1 1 auto',
          display: 'inline-flex',
          alignItems: 'baseline',
          gap: 5,
          overflow: 'hidden',
          whiteSpace: 'nowrap',
        }}
      >
        <span
          style={{
            minWidth: 0,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            direction: 'rtl',
            textAlign: 'left',
            color: 'var(--fg3)',
          }}
        >
          {label.dir}
        </span>
        <span
          style={{
            flex: '0 0 auto',
            color: 'var(--fg-max)',
            fontFamily: 'var(--fontFamily)',
            fontSize: 13,
            fontWeight: 600,
          }}
        >
          {label.leaf}
        </span>
      </span>
      <span
        style={{
          width: 120,
          height: 3,
          flex: '0 0 auto',
          borderRadius: 2,
          background: 'rgba(204,204,220,0.08)',
          overflow: 'hidden',
        }}
      >
        <span
          style={{
            display: 'block',
            width: `${Math.max(0, Math.min(100, pct || 0))}%`,
            height: '100%',
            background: 'var(--brand-orange)',
          }}
        />
      </span>
      <span style={{ color: 'var(--fg3)', whiteSpace: 'nowrap' }}>
        {pct == null ? 'Range spend unavailable' : `${pct}% of range spend`}
      </span>
      <span
        style={{
          marginLeft: 'auto',
          display: 'flex',
          alignItems: 'center',
          gap: 16,
          whiteSpace: 'nowrap',
        }}
      >
        <span style={{ color: 'var(--fg3)' }}>Range totals</span>
        <span>
          {count} {count === 1 ? 'session' : 'sessions'}
        </span>
        <span title={ESTIMATED_COST_TOOLTIP} style={{ color: 'var(--fg1)' }}>
          {formatCost(cost)}
        </span>
        <span title="Workspace tokens in the selected range">{formatTokens(tokens)}</span>
        <span style={{ color: 'var(--fg3)' }}>{formatAgo(last ? new Date(last).toISOString() : null, now)}</span>
        <button
          type="button"
          onClick={onClear}
          title="Clear workspace filter"
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 5,
            padding: '1px 8px',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            background: 'transparent',
            color: 'var(--fg2)',
            cursor: 'pointer',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
          }}
        >
          <Icon name="times" size={11} />
          clear
        </button>
      </span>
    </div>
  );
}

// HelpTip is an info-icon disclosure. Native `title` waits on the browser
// delay; this opens after 300ms so a pass-through does not flash it.
const HELP_TIP_DELAY_MS = 300;

interface HelpTipProps {
  text: React.ReactNode;
  ariaLabel: string;
}

/** Where the fixed tooltip sits, in viewport coordinates. */
interface HelpTipPosition {
  top: number;
  left: number;
}

function HelpTip({ text, ariaLabel }: HelpTipProps) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const timerRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const [pos, setPos] = useState<HelpTipPosition | null>(null);

  function clearTimer() {
    if (timerRef.current != null) {
      clearTimeout(timerRef.current);
      timerRef.current = null;
    }
  }

  function hide() {
    clearTimer();
    setOpen(false);
  }

  function show() {
    clearTimer();
    const el = triggerRef.current;
    if (!el) return;
    const r = el.getBoundingClientRect();
    const width = 300;
    const left = Math.max(8, Math.min(r.left, window.innerWidth - width - 8));
    setPos({ top: r.bottom + 6, left });
    timerRef.current = setTimeout(() => setOpen(true), HELP_TIP_DELAY_MS);
  }

  useEffect(() => {
    function onScrollOrResize() {
      if (timerRef.current != null) {
        clearTimeout(timerRef.current);
        timerRef.current = null;
      }
      setOpen(false);
    }
    // Capture so an inner pane (the session list) dismisses the fixed
    // tooltip instead of leaving it at stale viewport coordinates.
    window.addEventListener('scroll', onScrollOrResize, true);
    window.addEventListener('resize', onScrollOrResize);
    return () => {
      window.removeEventListener('scroll', onScrollOrResize, true);
      window.removeEventListener('resize', onScrollOrResize);
      onScrollOrResize();
    };
  }, []);

  return (
    <button
      ref={triggerRef}
      type="button"
      aria-label={ariaLabel}
      onMouseEnter={show}
      onMouseLeave={hide}
      onFocus={show}
      onBlur={hide}
      onClick={(e) => e.stopPropagation()}
      style={{
        position: 'relative',
        display: 'inline-flex',
        color: 'inherit',
        cursor: 'help',
        background: 'transparent',
        border: 'none',
        padding: 0,
        font: 'inherit',
      }}
    >
      <Icon name="info" size={12} />
      {open && pos && (
        <span
          role="tooltip"
          style={{
            position: 'fixed',
            top: pos.top,
            left: pos.left,
            zIndex: 80,
            width: 300,
            padding: '10px 12px',
            background: 'var(--bg-secondary)',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            boxShadow: 'var(--shadow-z2)',
            color: 'var(--fg1)',
            fontFamily: 'var(--fontFamily)',
            fontSize: 12,
            fontWeight: 400,
            lineHeight: 1.45,
            whiteSpace: 'normal',
            pointerEvents: 'none',
          }}
        >
          {text}
        </span>
      )}
    </button>
  );
}

// SortHeader is a clickable list-header cell: click sorts by the
// column, clicking again flips the direction.
interface SortHeaderProps {
  label: string;
  sortKey: string;
  sort: ListSort;
  onSort: (key: string) => void;
  tooltip?: string;
}

function SortHeader({ label, sortKey, sort, onSort, tooltip }: SortHeaderProps) {
  const active = sort.key === sortKey;
  return (
    <span
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 4,
      }}
    >
      <button
        type="button"
        onClick={() => onSort(sortKey)}
        title={`Sort by ${label.toLowerCase()}`}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 4,
          background: 'transparent',
          border: 'none',
          padding: 0,
          cursor: 'pointer',
          font: 'inherit',
          textAlign: 'left',
          fontWeight: 500,
          whiteSpace: 'nowrap',
          color: active ? 'var(--fg1)' : 'inherit',
        }}
      >
        {label}
        {active && <span style={{ fontSize: 8 }}>{sort.dir === 'asc' ? '▲' : '▼'}</span>}
      </button>
      {tooltip && <HelpTip text={tooltip} ariaLabel={`${label} help`} />}
    </span>
  );
}

// KpiTile is one cell of the KPI strip: a sentence-case label, a big
// mono value (optionally tinted, with a leading status dot), an
// optional progress bar, and a sub line.
interface KpiTileProps {
  label: string;
  value: React.ReactNode;
  valueColor?: string;
  sub?: React.ReactNode;
  dot?: string;
  /** Fill percentage of the optional progress bar. */
  bar?: number;
  tooltip?: string;
}

function KpiTile({ label, value, valueColor, sub, dot, bar, tooltip }: KpiTileProps) {
  return (
    <SurfaceCard
      style={{
        padding: '14px 16px',
        display: 'flex',
        flexDirection: 'column',
        gap: 7,
        minHeight: 104,
      }}
    >
      <span
        style={{
          fontSize: 11,
          color: 'var(--fg3)',
          display: 'inline-flex',
          alignItems: 'center',
          gap: 4,
        }}
      >
        {label}
        {tooltip && <HelpTip text={tooltip} ariaLabel={`${label} help`} />}
      </span>
      <span style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
        {dot && (
          <span
            style={{
              width: 8,
              height: 8,
              borderRadius: '50%',
              background: dot,
              flexShrink: 0,
            }}
          />
        )}
        <span
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 24,
            fontWeight: 500,
            lineHeight: 1,
            color: valueColor || 'var(--fg-max)',
          }}
        >
          {value}
        </span>
      </span>
      {bar != null && (
        <span
          style={{
            display: 'block',
            height: 4,
            borderRadius: 2,
            background: 'rgba(204,204,220,0.1)',
            overflow: 'hidden',
            marginTop: 1,
          }}
        >
          <span
            style={{
              display: 'block',
              height: '100%',
              width: `${bar}%`,
              background: 'var(--viz-green)',
            }}
          />
        </span>
      )}
      {sub != null && <span style={{ fontSize: 11, color: 'var(--fg2)' }}>{sub}</span>}
    </SurfaceCard>
  );
}

// KpiStrip surfaces the headline numbers for the in-view set: counts
// from the range + search conversations, token and cache rate from the
// chart's series (so they honour the model dropdown and legend
// toggles). "Model calls" is the per-generation call count. "Errored
// conversations" counts conversations with a call error.
/** The headline numbers the KPI strip renders, derived from the in-view set. */
interface KpiSummary {
  conversations: number;
  conversationsSub: string;
  tokens: number;
  cost: number | null;
  costSub: string;
  models: number;
  cachePct: number | null;
  calls: number;
  avgCalls: number;
  errConvs: number;
  errPct: number;
}

interface KpiStripProps {
  kpi: KpiSummary;
}

function KpiStrip({ kpi }: KpiStripProps) {
  const avg = kpi.avgCalls.toFixed(1).replace(/\.0$/, '');
  return (
    <div
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(6, 1fr)',
        gap: 12,
        marginBottom: 16,
      }}
    >
      <KpiTile label="Sessions" value={kpi.conversations} sub={kpi.conversationsSub} />
      <KpiTile label="Cost" value={formatCost(kpi.cost)} sub={kpi.costSub} tooltip={ESTIMATED_COST_TOOLTIP} />
      <KpiTile
        label="Total tokens"
        value={formatTokens(kpi.tokens)}
        sub={`${kpi.models} ${kpi.models === 1 ? 'model' : 'models'}`}
      />
      <KpiTile
        label="Input cache hit"
        value={kpi.cachePct == null ? '\u2014' : `${kpi.cachePct}%`}
        bar={kpi.cachePct == null ? 0 : kpi.cachePct}
      />
      <KpiTile label="Model calls" value={kpi.calls} sub={`${avg} avg / session`} />
      <KpiTile
        label="Errored sessions"
        value={kpi.errConvs}
        valueColor={kpi.errConvs > 0 ? 'var(--error-text)' : 'var(--fg-max)'}
        dot={kpi.errConvs > 0 ? 'var(--error-text)' : undefined}
        sub={`${kpi.errPct}% of sessions`}
      />
    </div>
  );
}

/** A session group in the list: the rows plus the totals its header shows. */
interface SessionGroupTotals {
  key: string;
  rows: ConversationSummary[];
  count: number;
  cost: number;
  /** False once a session in the group ran on a model with no price. */
  costComplete: boolean;
  tokens: number;
  dur: number;
  /** Last activity, in epoch milliseconds. */
  last: number;
}

/** A finished group: cost is null when nothing in it could be priced. */
interface SessionGroup extends Omit<SessionGroupTotals, 'cost'> {
  cost: number | null;
}

/** How many groups, and how many sessions inside them, are collapsed. */
interface CollapsedGroups {
  groups: number;
  sessions: number;
}

interface ConversationsViewProps {
  conversations: ConversationSummary[];
  /** Conversations the daemon holds before the list's range bounds; null from an older daemon. */
  storeCount: number | null;
  tokenPoints: TokenUsagePoint[] | null;
  tokenIntervalMs: number;
  loading: boolean;
  error: string | null;
  query: string;
  setQuery: (value: string) => void;
  searchInputRef?: React.RefObject<HTMLInputElement>;
  timeRange: string;
  setTimeRange: (value: string) => void;
  tokenModel: string;
  setTokenModel: (value: string) => void;
  chartMetric: string;
  setChartMetric: (value: string) => void;
  bucketSel: TimeSpan | null;
  setBucketSel: React.Dispatch<React.SetStateAction<TimeSpan | null>>;
  workspace: string | null;
  setWorkspace: (path: string | null) => void;
  groupBy: string;
  setGroupBy: (value: string) => void;
  listSort: ListSort;
  setListSort: React.Dispatch<React.SetStateAction<ListSort>>;
  onOpen: (c: OpenTarget) => void;
  onRefresh: () => void;
  refreshing: boolean;
  onOpenSettings: (tab: string) => void;
  /** The import state the banner renders, as useHistoryImport reports it. */
  history?: React.ComponentProps<typeof HistoryImportBanner>['history'] | null;
}

export function ConversationsView({
  conversations,
  storeCount,
  tokenPoints,
  tokenIntervalMs,
  loading,
  error,
  query,
  setQuery,
  searchInputRef,
  timeRange,
  setTimeRange,
  tokenModel,
  setTokenModel,
  chartMetric,
  setChartMetric,
  bucketSel,
  setBucketSel,
  workspace,
  setWorkspace,
  groupBy,
  setGroupBy,
  listSort,
  setListSort,
  onOpen,
  onRefresh,
  refreshing,
  onOpenSettings,
  history,
}: ConversationsViewProps) {
  const now = Date.now();
  const prices = useModelPrices();
  const range = timeRangeOption(timeRange);
  const trimmedQuery = query.trim();
  const searchActive = trimmedQuery.length > 0;
  const search = useSearchResults(query);
  const {
    phase: searchPhase,
    hits: searchHits,
    mode: searchMode,
    error: searchError,
    selectedIndex: searchSelectedIndex,
    setSelectedIndex: setSearchSelectedIndex,
    retry: retrySearch,
  } = search;
  const [agentFilter, setAgentFilter] = useState('all');
  const [modelFilter, setModelFilter] = useState('all');
  const [statusFilter, setStatusFilter] = useState('all');
  const rangeFiltered = useMemo(() => {
    if (range.ms == null) return conversations;
    const from = now - range.ms;
    return conversations.filter((c) => {
      const t = conversationTime(c);
      return t != null && t >= from && t <= now;
    });
  }, [conversations, range.ms, now]);

  // Explicit group overrides survive filters because they are keyed by group.
  // A groupBy change resets them because the keys change meaning.
  const [groupOpen, setGroupOpen] = useState<Map<string, boolean>>(() => new Map());
  // biome-ignore lint/correctness/useExhaustiveDependencies: groupBy clears the overrides
  useEffect(() => setGroupOpen(new Map()), [groupBy]);
  const toggleGroup = useCallback((key: string, open: boolean | undefined) => {
    setGroupOpen((previous) => {
      const next = new Map(previous);
      next.set(key, !open);
      return next;
    });
  }, []);
  const workspaces = useMemo<WorkspaceAggregate[]>(() => {
    const map = new Map<string, WorkspaceTotals>();
    for (const c of rangeFiltered) {
      const w = c.workspace || '';
      let e = map.get(w);
      if (!e) {
        e = {
          path: w,
          count: 0,
          cost: 0,
          costComplete: true,
          tokens: 0,
          dur: 0,
          last: 0,
        };
        map.set(w, e);
      }
      e.count++;
      const cost = conversationCost(c, prices);
      if (cost == null) e.costComplete = false;
      else e.cost += cost;
      e.tokens += c.total_tokens || 0;
      const d = durationBetweenSeconds(c.started_at, c.last_activity);
      if (d != null) e.dur += d;
      const t = conversationTime(c);
      if (t != null && t > e.last) e.last = t;
    }
    return [...map.values()]
      .map((entry) => ({
        ...entry,
        // Keep the priced sum when a workspace mixes priced and
        // unpriced sessions. Null only when nothing in it can be
        // priced, so share bars match totalCost.
        cost: entry.costComplete || entry.cost > 0 ? entry.cost : null,
      }))
      .sort((a, b) => b.last - a.last);
  }, [rangeFiltered, prices]);
  const totalCost = useMemo(() => {
    // Sum priced sessions only. One unpriced model used to zero the
    // whole header to NO_VALUE even when other sessions had a real
    // dollar figure — the KPI tile already skips those.
    let cost = 0;
    let priced = 0;
    for (const conversation of rangeFiltered) {
      const value = conversationCost(conversation, prices);
      if (value == null) continue;
      cost += value;
      priced++;
    }
    return priced ? cost : null;
  }, [rangeFiltered, prices]);
  const agentCount = useMemo(() => {
    const set = new Set<string>();
    for (const c of rangeFiltered) for (const a of agentHosts(c.agents)) set.add(a);
    return set.size;
  }, [rangeFiltered]);
  // Keep an out-of-range workspace selected so the page can show its empty
  // context instead of silently returning to all workspaces.
  const activeWorkspace = workspace;
  const selectedWorkspaceRangeAggregate =
    activeWorkspace == null
      ? null
      : workspaces.find((w) => w.path === activeWorkspace) || {
          path: activeWorkspace,
          count: 0,
          cost: 0,
          costComplete: true,
          tokens: 0,
          dur: 0,
          last: 0,
        };
  const wsFiltered = useMemo(
    () =>
      activeWorkspace == null ? rangeFiltered : rangeFiltered.filter((c) => (c.workspace || '') === activeWorkspace),
    [rangeFiltered, activeWorkspace],
  );

  const agentOptions = useMemo(() => {
    const set = new Set<string>();
    for (const c of wsFiltered) for (const a of agentHosts(c.agents)) set.add(a);
    return [...set].sort();
  }, [wsFiltered]);
  const activeAgentFilter = agentOptions.includes(agentFilter) ? agentFilter : 'all';

  const modelFacetOptions = useMemo(() => {
    const set = new Set<string>();
    for (const c of wsFiltered) for (const m of c.models || []) if (m) set.add(m);
    return [...set].sort();
  }, [wsFiltered]);
  const activeModelFilter = modelFacetOptions.includes(modelFilter) ? modelFilter : 'all';
  const activeStatusFilter = statusFilter === 'errors' || statusFilter === 'subagents' ? statusFilter : 'all';

  const filtered = useMemo(() => {
    return wsFiltered.filter((c) => {
      if (activeAgentFilter !== 'all' && !agentHosts(c.agents).includes(activeAgentFilter)) return false;
      if (activeModelFilter !== 'all' && !(c.models || []).includes(activeModelFilter)) return false;
      if (activeStatusFilter === 'errors' && c.status !== 'err') return false;
      if (activeStatusFilter === 'subagents' && !((c.subagents ?? 0) > 0)) return false;
      return true;
    });
  }, [wsFiltered, activeAgentFilter, activeModelFilter, activeStatusFilter]);

  const activeFilterCount =
    (activeWorkspace != null ? 1 : 0) +
    (activeAgentFilter !== 'all' ? 1 : 0) +
    (activeModelFilter !== 'all' ? 1 : 0) +
    (activeStatusFilter !== 'all' ? 1 : 0);
  const clearFilters = useCallback(() => {
    setWorkspace(null);
    setAgentFilter('all');
    setModelFilter('all');
    setStatusFilter('all');
  }, [setWorkspace]);
  const clearSearch = useCallback(() => {
    setQuery('');
    setTimeout(() => {
      const el = searchInputRef?.current;
      if (el) el.focus();
    }, 0);
  }, [setQuery, searchInputRef]);
  const onSearchInputKey = useCallback(
    (e: React.KeyboardEvent<HTMLInputElement>) => {
      if (e.key === 'Escape') {
        e.preventDefault();
        clearSearch();
        return;
      }
      if (!searchActive || searchHits.length === 0) return;
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        setSearchSelectedIndex((i) => Math.min(searchHits.length - 1, i < 0 ? 0 : i + 1));
      } else if (e.key === 'ArrowUp') {
        e.preventDefault();
        setSearchSelectedIndex((i) => Math.max(-1, i - 1));
      } else if (e.key === 'Enter' && searchSelectedIndex >= 0) {
        e.preventDefault();
        const hit = searchHits[searchSelectedIndex];
        if (hit) onOpen({ id: hit.id, title: hit.title });
      }
    },
    [clearSearch, searchActive, searchHits, searchSelectedIndex, setSearchSelectedIndex, onOpen],
  );
  const searchRightAdornment =
    searchPhase === 'loading' ? (
      <span
        className="sigil-spin"
        style={{
          width: 14,
          height: 14,
          borderRadius: '50%',
          border: '2px solid var(--border-strong)',
          borderTopColor: 'var(--fg2)',
          display: 'inline-block',
        }}
        role="status"
        aria-label="Searching"
      />
    ) : searchActive ? (
      <button
        type="button"
        onClick={clearSearch}
        aria-label="Clear search"
        title="Clear search"
        style={{
          width: 22,
          height: 22,
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          background: 'transparent',
          border: 'none',
          color: 'var(--fg3)',
          cursor: 'pointer',
          borderRadius: 2,
        }}
        onMouseEnter={(e) => {
          e.currentTarget.style.background = 'var(--action-hover)';
          e.currentTarget.style.color = 'var(--fg1)';
        }}
        onMouseLeave={(e) => {
          e.currentTarget.style.background = 'transparent';
          e.currentTarget.style.color = 'var(--fg3)';
        }}
      >
        <Icon name="times" size={14} />
      </button>
    ) : (
      <span
        title="Press Command-K or Control-K to focus search"
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11,
          color: 'var(--fg3)',
          padding: '1px 6px',
          border: '1px solid var(--border-weak)',
          borderRadius: 2,
        }}
      >
        ⌘K
      </span>
    );

  // Token chart has its own model filter and is driven only by the
  // time range, not the text query (token points carry model, not the
  // searchable conversation fields). The selection lives in App so it
  // survives navigating into a conversation and back; a model that
  // disappears from the store falls back to "all" by derivation.
  const points = tokenPoints || [];
  const tokenUsagePoints = useMemo(
    () => points.filter((point) => TOKEN_SERIES.some((series) => point[series.key] > 0)),
    [points],
  );
  const tokenModels = useMemo(
    () =>
      Array.from(
        new Set(tokenUsagePoints.map((point) => point.model).filter((model): model is string => Boolean(model))),
      ).sort(),
    [tokenUsagePoints],
  );
  const effectiveModel = tokenModels.includes(tokenModel) ? tokenModel : 'all';
  const tokenFiltered = useMemo(
    () =>
      effectiveModel === 'all' ? tokenUsagePoints : tokenUsagePoints.filter((point) => point.model === effectiveModel),
    [tokenUsagePoints, effectiveModel],
  );
  // Legend visibility is shared with the KPI strip so hiding a series
  // rescales the chart and drops it from the headline tokens in step.
  // Lives here, not in TokenChart, so both read the one set.
  const [hiddenSeries, setHiddenSeries] = useState<Set<TokenBucketKey>>(() => new Set());
  const toggleSeries = useCallback(
    (key: TokenBucketKey) =>
      setHiddenSeries((prev) => {
        const next = new Set(prev);
        next.has(key) ? next.delete(key) : next.add(key);
        return next;
      }),
    [],
  );
  // Both metrics share one window so switching the chart between
  // them doesn't shift the time axis; with per-metric windows the
  // "All" range drifts when the datasets' extents differ. The window
  // is snapped to the bucket ladder the token endpoint aggregates on,
  // so each server point falls inside exactly one bar.
  //
  // A fixed range asks the server for the width it will draw on. Only
  // the "All" range needs the reported width as a floor: there the server
  // derives it from the whole store while the bars follow the visible
  // window, which a model facet can narrow.
  const serverIntervalMs = range.ms == null ? tokenIntervalMs : 0;
  const chartWindow = useMemo(() => {
    const times = filtered.map(conversationTime).concat(tokenFiltered.map(tokenPointTime));
    return chartGrid(times, timeRange, now, serverIntervalMs);
  }, [filtered, tokenFiltered, timeRange, now, serverIntervalMs]);
  const activity = useMemo(
    () =>
      bucketActivity(filtered, timeRange, now, {
        window: chartWindow,
        count: chartWindow.count,
      }),
    [filtered, timeRange, now, chartWindow],
  );
  const tokenUsage = useMemo(
    () =>
      bucketTokenUsage(tokenFiltered, timeRange, now, {
        window: chartWindow,
        count: chartWindow.count,
      }),
    [tokenFiltered, timeRange, now, chartWindow],
  );
  // Bucket drill-down from a chart bar click: the list narrows to
  // conversations active inside the picked bucket, while the charts
  // keep the full window and just highlight the selection.
  const onBucketClick = useCallback(
    (b: TimeSpan) => {
      setBucketSel((sel) =>
        sel && sel.start === b.start && sel.end === b.end ? null : { start: b.start, end: b.end },
      );
    },
    [setBucketSel],
  );
  const listFiltered = useMemo(() => {
    if (!bucketSel) return filtered;
    return filtered.filter((c) => {
      const endT = conversationTime(c);
      if (endT == null) return false;
      const startT = new Date(c.started_at).getTime();
      const s = Number.isFinite(startT) ? startT : endT;
      return s < bucketSel.end && endT >= bucketSel.start;
    });
  }, [filtered, bucketSel]);

  const handleSort = useCallback(
    (key: string) => {
      setListSort((s) => (s.key === key ? { key, dir: s.dir === 'desc' ? 'asc' : 'desc' } : { key, dir: 'desc' }));
    },
    [setListSort],
  );
  const sorted = useMemo(() => {
    const dir = listSort.dir === 'asc' ? 1 : -1;
    const val = (c: ConversationSummary) => {
      if (listSort.key === 'duration') {
        const d = durationBetweenSeconds(c.started_at, c.last_activity);
        return d == null ? -1 : d;
      }
      if (listSort.key === 'tokens') return c.total_tokens || 0;
      if (listSort.key === 'cost') return conversationCost(c, prices) || 0;
      const t = conversationTime(c);
      return t == null ? 0 : t;
    };
    return [...listFiltered].sort((a, b) => (val(a) - val(b)) * dir);
  }, [listFiltered, listSort, prices]);

  const grouped = useMemo<SessionGroup[]>(() => {
    if (groupBy === 'none') return [];
    const map = new Map<string, SessionGroupTotals>();
    for (const c of sorted) {
      const key = groupKeyFor(c, groupBy);
      let group = map.get(key);
      if (!group) {
        group = {
          key,
          rows: [],
          count: 0,
          cost: 0,
          costComplete: true,
          tokens: 0,
          dur: 0,
          last: 0,
        };
        map.set(key, group);
      }
      group.rows.push(c);
      group.count++;
      const cost = conversationCost(c, prices);
      if (cost == null) group.costComplete = false;
      else group.cost += cost;
      group.tokens += c.total_tokens || 0;
      const duration = durationBetweenSeconds(c.started_at, c.last_activity);
      if (duration != null) group.dur += duration;
      const time = conversationTime(c);
      if (time != null && time > group.last) group.last = time;
    }
    const direction = listSort.dir === 'asc' ? 1 : -1;
    const value = (group: SessionGroupTotals) => {
      if (listSort.key === 'cost') return group.cost;
      if (listSort.key === 'tokens') return group.tokens;
      if (listSort.key === 'duration') return group.dur;
      return group.last;
    };
    return [...map.values()]
      .filter((group) => group.rows.length > 0)
      .sort((a, b) => (value(a) - value(b)) * direction)
      .map((group) => ({
        ...group,
        cost: group.costComplete || group.cost > 0 ? group.cost : null,
      }));
  }, [sorted, groupBy, listSort, prices]);
  const groupedTotalCost = useMemo(() => {
    let cost = 0;
    let priced = 0;
    for (const group of grouped) {
      if (group.cost == null) continue;
      cost += group.cost;
      priced++;
    }
    return priced ? cost : null;
  }, [grouped]);
  const collapsed = useMemo<CollapsedGroups>(() => {
    if (groupBy === 'none' || (groupBy === 'workspace' && activeWorkspace != null)) return { groups: 0, sessions: 0 };
    let groups = 0;
    let sessions = 0;
    grouped.forEach((group, index) => {
      const open = groupOpen.has(group.key) ? groupOpen.get(group.key) : index < OPEN_GROUPS;
      if (open) return;
      groups++;
      sessions += group.count;
    });
    return { groups, sessions };
  }, [grouped, groupOpen, groupBy, activeWorkspace]);

  // KPI tiles read the range + workspace + search set (not the bucket
  // drill-down), computed straight off each conversation's token buckets.
  // This keeps headline tokens, cost, and cache rate conversation-based rather
  // than tied to the token-series chart, which has its own model filter.
  const kpi = useMemo(() => {
    let calls = 0,
      errConvs = 0,
      cost = 0,
      priced = 0,
      unpriced = 0;
    const tot: TokenBuckets = {
      fresh_input: 0,
      cache_read: 0,
      cache_write: 0,
      output: 0,
      reasoning: 0,
    };
    const models = new Set<string>();
    for (const c of filtered) {
      calls += c.calls || 0;
      if (c.status === 'err') errConvs++;
      const cc = conversationCost(c, prices);
      if (cc == null) unpriced++;
      else {
        cost += cc;
        priced++;
      }
      const b = c.token_buckets;
      if (b) for (const k in tot) tot[k as TokenBucketKey] += b[k as TokenBucketKey] || 0;
      for (const m of c.models || []) models.add(m);
    }
    const tokens = tot.fresh_input + tot.cache_read + tot.cache_write + tot.output + tot.reasoning;
    // Cost sub is honest about coverage: if some conversations ran on an
    // unpriced (non-Anthropic) model, say so rather than implying the
    // total covers everything.
    const costSub =
      unpriced > 0
        ? `${unpriced} unpriced · ${formatCost(priced ? cost / priced : 0)} avg`
        : cost
          ? `${formatCost(cost / Math.max(1, priced))} avg / session`
          : 'estimated';
    return {
      conversations: filtered.length,
      conversationsSub: activeWorkspace != null ? 'in workspace' : 'active in range',
      tokens,
      cost: priced ? cost : null, // nothing priced, so NO_VALUE rather than a misleading $0
      costSub,
      models: models.size,
      cachePct: cacheInputHitPercent(tot.fresh_input, tot.cache_read, tot.cache_write),
      calls,
      avgCalls: filtered.length ? calls / filtered.length : 0,
      errConvs,
      errPct: filtered.length ? Math.round((errConvs / filtered.length) * 100) : 0,
    };
  }, [filtered, activeWorkspace, prices]);

  return (
    <PageShell maxWidth={1400}>
      <PageHero
        title="Sessions"
        desc={
          searchActive
            ? 'Full-text search over prompts, responses, and tool output in all captured local sessions.'
            : 'Captured sessions, token usage, costs, and tool-call activity from local runs.'
        }
        stats={
          searchActive
            ? [
                {
                  label: 'Index',
                  value: searchMode === 'semantic' ? 'QMD' : 'FTS',
                  tone: 'var(--primary-text)',
                },
                {
                  label: 'Results',
                  value: String(searchHits.length),
                  tone: searchHits.length ? 'var(--success-text)' : 'var(--fg3)',
                },
                {
                  label: 'Status',
                  value: searchPhase === 'loading' ? 'Searching' : 'Ready',
                  tone: searchPhase === 'loading' ? 'var(--warning-text)' : undefined,
                },
              ]
            : [
                { label: 'Range', value: range.label },
                {
                  label: 'Workspaces',
                  value: String(workspaces.length),
                },
                {
                  label: 'Agents',
                  value: String(agentCount),
                },
              ]
        }
      />
      {history && <HistoryImportBanner history={history} onOpenSettings={onOpenSettings} />}
      <FilterBar
        query={query}
        onQueryChange={setQuery}
        inputRef={searchInputRef}
        timeRange={searchActive ? null : timeRange}
        onTimeRangeChange={searchActive ? null : setTimeRange}
        workspaces={searchActive ? [] : workspaces}
        workspace={searchActive ? undefined : activeWorkspace}
        onWorkspaceChange={searchActive ? undefined : setWorkspace}
        workspaceSessionCount={rangeFiltered.length}
        workspaceTotalCost={totalCost}
        now={now}
        rangeLabel={range.label}
        groupBy={searchActive ? undefined : groupBy}
        onGroupByChange={searchActive ? undefined : setGroupBy}
        agentFilter={searchActive ? undefined : activeAgentFilter}
        onAgentFilterChange={searchActive ? undefined : setAgentFilter}
        agentOptions={searchActive ? [] : agentOptions}
        modelFilter={searchActive ? undefined : activeModelFilter}
        onModelFilterChange={searchActive ? undefined : setModelFilter}
        modelOptions={searchActive ? [] : modelFacetOptions}
        statusFilter={searchActive ? undefined : activeStatusFilter}
        onStatusFilterChange={searchActive ? undefined : setStatusFilter}
        activeFilterCount={searchActive ? 0 : activeFilterCount}
        onClearFilters={clearFilters}
        onRefresh={onRefresh}
        refreshing={refreshing}
        placeholder="Search prompts, responses, tool output, title, agent, model…"
        onInputKeyDown={onSearchInputKey}
        rightAdornment={searchRightAdornment}
      />
      {searchActive ? (
        <ConversationSearchPanel
          query={trimmedQuery}
          hits={searchHits}
          phase={searchPhase}
          mode={searchMode}
          error={searchError}
          selectedIndex={searchSelectedIndex}
          setSelectedIndex={setSearchSelectedIndex}
          retry={retrySearch}
          now={now}
          onOpen={onOpen}
        />
      ) : (
        <Fragment>
          <KpiStrip kpi={kpi} />
          {chartMetric === 'activity' ? (
            <ActivityChart
              data={activity.buckets}
              bucketLabel={activity.bucketLabel}
              selection={bucketSel}
              onBucketClick={onBucketClick}
              switcher={<ChartSwitch value={chartMetric} onChange={setChartMetric} />}
            />
          ) : (
            <TokenChart
              data={tokenUsage.buckets}
              bucketLabel={tokenUsage.bucketLabel}
              grandTotal={tokenUsage.grandTotal}
              models={tokenModels}
              model={effectiveModel}
              onModelChange={setTokenModel}
              hidden={hiddenSeries}
              onToggleSeries={toggleSeries}
              selection={bucketSel}
              onBucketClick={onBucketClick}
              switcher={<ChartSwitch value={chartMetric} onChange={setChartMetric} />}
            />
          )}

          {bucketSel && (
            <div
              style={{
                marginTop: 10,
                display: 'flex',
                alignItems: 'center',
                gap: 10,
                fontSize: 11,
                fontFamily: 'var(--fontFamilyMonospace)',
                color: 'var(--fg2)',
              }}
            >
              <span>
                Showing {formatBucketLabel(bucketSel.start, bucketSel.end - bucketSel.start)} to{' '}
                {formatBucketLabel(bucketSel.end, bucketSel.end - bucketSel.start)}
              </span>
              <button
                type="button"
                onClick={() => setBucketSel(null)}
                style={{
                  background: 'transparent',
                  border: '1px solid var(--border-medium)',
                  borderRadius: 2,
                  color: 'var(--fg2)',
                  cursor: 'pointer',
                  fontSize: 11,
                  fontFamily: 'var(--fontFamilyMonospace)',
                  padding: '1px 8px',
                }}
              >
                ✕ clear
              </button>
            </div>
          )}

          <SurfaceCard
            style={{
              marginTop: 18,
              overflow: 'hidden',
            }}
          >
            <div
              style={{
                display: 'grid',
                gridTemplateColumns: CONV_GRID,
                alignItems: 'center',
                gap: 16,
                padding: '11px 16px',
                borderBottom: '1px solid var(--border-weak)',
                background: 'var(--bg-secondary)',
                fontFamily: 'var(--fontFamily)',
                fontSize: 12,
                color: 'var(--fg3)',
                fontWeight: 500,
              }}
            >
              <SortHeader label="Last activity" sortKey="last_activity" sort={listSort} onSort={handleSort} />
              <span>Session</span>
              <span>Agent</span>
              <SortHeader
                label="Cost"
                sortKey="cost"
                sort={listSort}
                onSort={handleSort}
                tooltip={ESTIMATED_COST_TOOLTIP}
              />
              <SortHeader label="Tokens" sortKey="tokens" sort={listSort} onSort={handleSort} />
              <SortHeader label="Duration" sortKey="duration" sort={listSort} onSort={handleSort} />
              <span>Models</span>
            </div>

            {!error &&
              (!loading || (selectedWorkspaceRangeAggregate?.count ?? 0) > 0) &&
              selectedWorkspaceRangeAggregate && (
                <WorkspaceContextStrip
                  path={selectedWorkspaceRangeAggregate.path}
                  count={selectedWorkspaceRangeAggregate.count}
                  cost={selectedWorkspaceRangeAggregate.cost}
                  tokens={selectedWorkspaceRangeAggregate.tokens}
                  last={selectedWorkspaceRangeAggregate.last}
                  share={
                    totalCost != null && totalCost > 0 && selectedWorkspaceRangeAggregate.cost != null
                      ? selectedWorkspaceRangeAggregate.cost / totalCost
                      : null
                  }
                  now={now}
                  onClear={() => setWorkspace(null)}
                />
              )}

            {error && (
              <div style={{ padding: 16 }}>
                <Notice kind="error" title="Failed to load sessions">
                  {error}
                </Notice>
              </div>
            )}
            {!error && loading && conversations.length === 0 && (
              <div
                style={{
                  padding: '32px 18px',
                  color: 'var(--fg3)',
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 12,
                }}
              >
                Loading…
              </div>
            )}
            {/* The list request is range-scoped, so an empty page does not
                            mean an empty store. storeCount comes from the response and
                            decides which notice applies; null (no response yet, or an
                            older daemon) falls back to reading the page. */}
            {!error &&
              !loading &&
              activeWorkspace == null &&
              rangeFiltered.length === 0 &&
              (storeCount === 0 || (storeCount == null && conversations.length === 0)) && (
                <div style={{ padding: 16 }}>
                  <Notice kind="info" title="No sessions yet">
                    Run an agent against this daemon with{' '}
                    <code style={{ color: 'var(--fg1)' }}>agento11y pi --local</code> or{' '}
                    <code style={{ color: 'var(--fg1)' }}>agento11y claude --local</code>. Captured generations appear
                    here as soon as the agent emits its first one.
                  </Notice>
                </div>
              )}
            {!error &&
              !loading &&
              activeWorkspace == null &&
              rangeFiltered.length === 0 &&
              ((storeCount ?? 0) > 0 || conversations.length > 0) && (
                <div
                  style={{
                    padding: '16px 18px',
                    color: 'var(--fg2)',
                    fontSize: 12,
                  }}
                >
                  No sessions in <code style={{ color: 'var(--fg1)' }}>{range.label}</code>.
                </div>
              )}
            {!error && !loading && selectedWorkspaceRangeAggregate?.count === 0 && (
              <div
                style={{
                  padding: '16px 18px',
                  color: 'var(--fg2)',
                  fontSize: 12,
                }}
              >
                No sessions in this range.
              </div>
            )}
            {!error &&
              filtered.length === 0 &&
              rangeFiltered.length > 0 &&
              (activeWorkspace == null || (selectedWorkspaceRangeAggregate?.count ?? 0) > 0) && (
                <div
                  style={{
                    padding: '16px 18px',
                    color: 'var(--fg2)',
                    fontSize: 12,
                  }}
                >
                  No sessions match the current filters.
                </div>
              )}
            {!error && bucketSel && listFiltered.length === 0 && filtered.length > 0 && (
              <div
                style={{
                  padding: '16px 18px',
                  color: 'var(--fg2)',
                  fontSize: 12,
                }}
              >
                No sessions in the selected bucket.
              </div>
            )}
            {groupBy === 'none'
              ? sorted.map((c) => <ConvRow key={c.id} c={c} now={now} onOpen={onOpen} prices={prices} />)
              : groupBy === 'workspace' && activeWorkspace != null
                ? sorted.map((c) => (
                    <ConvRow key={c.id} c={c} now={now} onOpen={onOpen} prices={prices} hideWorkspace />
                  ))
                : grouped.map((group, index) => {
                    const open = groupOpen.has(group.key) ? groupOpen.get(group.key) : index < OPEN_GROUPS;
                    return (
                      <Fragment key={`${groupBy}:${group.key}`}>
                        <SessionGroupHeader
                          groupBy={groupBy}
                          label={group.key}
                          open={open}
                          onToggle={() => toggleGroup(group.key, open)}
                          count={group.count}
                          cost={group.cost}
                          tokens={group.tokens}
                          last={group.last}
                          share={
                            groupedTotalCost != null && groupedTotalCost > 0 && group.cost != null
                              ? group.cost / groupedTotalCost
                              : null
                          }
                          now={now}
                        />
                        {open &&
                          group.rows.map((c) => (
                            <ConvRow
                              key={c.id}
                              c={c}
                              now={now}
                              onOpen={onOpen}
                              prices={prices}
                              grouped
                              hideWorkspace={groupBy === 'workspace'}
                            />
                          ))}
                      </Fragment>
                    );
                  })}
            <div
              style={{
                padding: '11px 16px',
                fontSize: 11,
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
              }}
            >
              {sorted.length} of {filtered.length} {filtered.length === 1 ? 'session' : 'sessions'}
              {collapsed.groups > 0 && (
                <Fragment>
                  {' · '}
                  {collapsed.groups} collapsed{' '}
                  {groupBy === 'workspace'
                    ? collapsed.groups === 1
                      ? 'workspace'
                      : 'workspaces'
                    : `${groupBy} ${collapsed.groups === 1 ? 'group' : 'groups'}`}{' '}
                  {collapsed.groups === 1 ? 'hides' : 'hide'} {collapsed.sessions}{' '}
                  {collapsed.sessions === 1 ? 'session' : 'sessions'}
                </Fragment>
              )}
              {activeFilterCount > 0
                ? ` · ${activeFilterCount} ${activeFilterCount === 1 ? 'filter' : 'filters'} active`
                : ''}
            </div>
          </SurfaceCard>
        </Fragment>
      )}
    </PageShell>
  );
}
