import type { MouseEvent as ReactMouseEvent } from 'react';
import { useEffect, useMemo, useState } from 'react';
import { AnalyticsPage } from './analytics-page';
import { ChartXLabels, ChartYAxis, TimeRangePicker, type WorkspaceAggregate, WorkspaceFacet } from './conversations';
import { formatBucketLabel, formatInteger, timeRangeOption } from './formatters';
import { Notice, SurfaceCard } from './notices';
import { type AnalyticsTab, isPlainLeftClick, type ToolSessionFilters, toolSessionsPath } from './routing';
import { Icon, iconBtn } from './shell';
import type { ToolAnalytics, ToolAnalyticsBucket, ToolAnalyticsRow } from './types';

const SEARCH_DEBOUNCE_MS = 150;
const SKELETON_ROWS = ['one', 'two', 'three', 'four', 'five', 'six'] as const;
const TOOL_COLORS = [
  'var(--brand-orange)',
  'var(--viz-blue)',
  'var(--viz-purple)',
  'var(--viz-green)',
  'var(--viz-yellow)',
  'var(--tool-neutral)',
] as const;

export interface SkillsToolsViewProps {
  data: ToolAnalytics | null;
  loading: boolean;
  error: string | null;
  timeRange: string;
  onTimeRangeChange: (value: string) => void;
  workspace: string | null;
  onWorkspaceChange: (value: string | null) => void;
  window: { since?: string; before: string } | null;
  onRefresh: () => void;
  refreshing: boolean;
  onSelectTab: (tab: AnalyticsTab) => void;
  onOpenSessions: (filters: ToolSessionFilters) => void;
}

function duration(value: number | undefined) {
  if (value == null || !Number.isFinite(value)) return '-';
  if (value < 1) return `${Math.round(value * 1000)}ms`;
  if (value < 10) return `${value.toFixed(1).replace(/\.0$/, '')}s`;
  if (value < 60) return `${Math.round(value)}s`;
  const minutes = Math.floor(value / 60);
  const seconds = Math.round(value % 60);
  return seconds ? `${minutes}m ${seconds}s` : `${minutes}m`;
}

function sessionFilters(
  tool: string,
  workspace: string | null,
  window: SkillsToolsViewProps['window'],
  bounds?: { since: string; before: string },
): ToolSessionFilters {
  if (!bounds) {
    return { tool, workspace, since: window?.since, before: window?.before };
  }
  const windowSince = window?.since ? Date.parse(window.since) : Number.NEGATIVE_INFINITY;
  const windowBefore = window?.before ? Date.parse(window.before) : Number.POSITIVE_INFINITY;
  const boundSince = bounds ? Date.parse(bounds.since) : windowSince;
  const boundBefore = bounds ? Date.parse(bounds.before) : windowBefore;
  const since = Math.max(windowSince, boundSince);
  const before = Math.min(windowBefore, boundBefore);
  return {
    tool,
    workspace,
    since: Number.isFinite(since) ? new Date(since).toISOString() : undefined,
    before: Number.isFinite(before) ? new Date(before).toISOString() : undefined,
  };
}

function stat(label: string, value: string, tone?: string, suffix?: string) {
  return (
    <span className="tools-stat">
      <span>{label}</span> <strong style={{ color: tone || 'var(--fg-max)' }}>{value}</strong>
      {suffix && <span> {suffix}</span>}
    </span>
  );
}

type ToolSortKey = 'name' | 'share' | 'calls' | 'failures' | 'p50' | 'p95' | 'sessions';
type SortDirection = 'asc' | 'desc';

interface ToolTableSort {
  key: ToolSortKey;
  direction: SortDirection;
}

interface ToolSortHeaderProps {
  label: string;
  sortKey: ToolSortKey;
  sort: ToolTableSort;
  onSort: (key: ToolSortKey) => void;
}

function ToolSortHeader({ label, sortKey, sort, onSort }: ToolSortHeaderProps) {
  const active = sort.key === sortKey;
  return (
    <th scope="col" aria-sort={active ? (sort.direction === 'asc' ? 'ascending' : 'descending') : undefined}>
      <button
        type="button"
        className={`tools-sort-button${active ? ' tools-sort-button-active' : ''}`}
        onClick={() => onSort(sortKey)}
        title={`Sort by ${label}`}
      >
        {label}
        {active && (
          <span className="tools-sort-indicator" aria-hidden="true">
            {sort.direction === 'asc' ? '▲' : '▼'}
          </span>
        )}
      </button>
    </th>
  );
}

function sortToolRows(rows: ToolAnalyticsRow[], sort: ToolTableSort): ToolAnalyticsRow[] {
  const direction = sort.direction === 'asc' ? 1 : -1;
  return [...rows].sort((a, b) => {
    if (sort.key === 'name') return a.name.localeCompare(b.name) * direction;

    const value = (row: ToolAnalyticsRow): number | undefined => {
      switch (sort.key) {
        case 'share':
        case 'calls':
          return row.calls;
        case 'failures':
          return row.failures;
        case 'p50':
          return row.p50_duration_seconds;
        case 'p95':
          return row.p95_duration_seconds;
        case 'sessions':
          return row.sessions;
        default:
          return undefined;
      }
    };
    const aValue = value(a);
    const bValue = value(b);
    if (aValue == null && bValue == null) return a.name.localeCompare(b.name);
    if (aValue == null) return 1;
    if (bValue == null) return -1;
    return (aValue - bValue) * direction || a.name.localeCompare(b.name);
  });
}

interface ToolTableProps {
  rows: ToolAnalyticsRow[];
  allCalls: number;
  data: ToolAnalytics | null;
  loading: boolean;
  error: string | null;
  filtered: boolean;
  sort: ToolTableSort;
  onSort: (key: ToolSortKey) => void;
  workspace: string | null;
  window: SkillsToolsViewProps['window'];
  onOpenSessions: SkillsToolsViewProps['onOpenSessions'];
}

function ToolTable({
  rows,
  allCalls,
  data,
  loading,
  error,
  filtered,
  sort,
  onSort,
  workspace,
  window,
  onOpenSessions,
}: ToolTableProps) {
  const maxCalls = Math.max(1, ...(data?.rows || []).map((row) => row.calls));
  return (
    <SurfaceCard className="tools-table-panel" style={{ marginBottom: 12 }}>
      <div className="tools-panel-header">
        <div className="tools-mode-label">
          Tools <span>{formatInteger(Math.max(0, data?.totals.tools || 0))}</span>
        </div>
        <div className="tools-panel-caption">
          {formatInteger(Math.max(0, data?.totals.failures || 0))} of{' '}
          {formatInteger(Math.max(0, data?.totals.calls || 0))} failed · duration from{' '}
          {formatInteger(Math.max(0, data?.totals.duration_samples || 0))} tool-execution spans
        </div>
      </div>
      <div className="tools-table-scroll">
        <table className="tools-table">
          <thead>
            <tr>
              <th scope="col">#</th>
              <ToolSortHeader label="Tool" sortKey="name" sort={sort} onSort={onSort} />
              <ToolSortHeader label="Share" sortKey="share" sort={sort} onSort={onSort} />
              <ToolSortHeader label="Calls" sortKey="calls" sort={sort} onSort={onSort} />
              <ToolSortHeader label="Failed" sortKey="failures" sort={sort} onSort={onSort} />
              <ToolSortHeader label="p50" sortKey="p50" sort={sort} onSort={onSort} />
              <ToolSortHeader label="p95" sortKey="p95" sort={sort} onSort={onSort} />
              <ToolSortHeader label="Sessions" sortKey="sessions" sort={sort} onSort={onSort} />
            </tr>
          </thead>
          <tbody aria-live="polite">
            {loading &&
              rows.length === 0 &&
              SKELETON_ROWS.map((row) => (
                <tr key={row}>
                  <td colSpan={8} className="tools-state-cell">
                    <span className="tools-skeleton sigil-shim" />
                  </td>
                </tr>
              ))}
            {!loading && error && (
              <tr>
                <td colSpan={8} className="tools-state-cell">
                  <Notice kind="error" title="Failed to load tool analytics">
                    {error}
                  </Notice>
                </td>
              </tr>
            )}
            {!loading && !error && rows.length === 0 && (
              <tr>
                <td colSpan={8} className="tools-state-cell">
                  {filtered
                    ? 'No tools match that filter.'
                    : 'No tools recorded in this range. Try a wider time range.'}
                </td>
              </tr>
            )}
            {rows.map((row, index) => {
              const filters = sessionFilters(row.name, workspace, window);
              const failureRate = row.calls > 0 ? row.failures / row.calls : 0;
              return (
                <tr key={row.name} data-tool-row={row.name}>
                  <td>{index + 1}</td>
                  <th scope="row">
                    <a
                      href={toolSessionsPath(filters)}
                      onClick={(event) => {
                        if (!isPlainLeftClick(event)) return;
                        event.preventDefault();
                        onOpenSessions(filters);
                      }}
                    >
                      {row.name}
                    </a>
                  </th>
                  <td>
                    <span
                      className="tools-share-track"
                      role="img"
                      aria-label={`${Math.round((row.calls / Math.max(1, allCalls)) * 100)}% of calls`}
                    >
                      <span className="tools-share-fill" style={{ width: `${(row.calls / maxCalls) * 100}%` }} />
                    </span>
                  </td>
                  <td>{formatInteger(Math.max(0, row.calls || 0))}</td>
                  <td style={{ color: failureRate > 0.05 ? 'var(--error-text)' : 'var(--fg3)' }}>
                    {formatInteger(Math.max(0, row.failures || 0))}
                  </td>
                  <td style={{ color: (row.p50_duration_seconds || 0) > 10 ? 'var(--warning-text)' : 'var(--fg2)' }}>
                    {duration(row.p50_duration_seconds)}
                  </td>
                  <td style={{ color: (row.p95_duration_seconds || 0) > 10 ? 'var(--warning-text)' : 'var(--fg2)' }}>
                    {duration(row.p95_duration_seconds)}
                  </td>
                  <td>{formatInteger(Math.max(0, row.sessions || 0))}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </SurfaceCard>
  );
}

interface ToolChartProps {
  buckets: ToolAnalyticsBucket[];
  intervalSeconds: number;
  workspace: string | null;
  window: SkillsToolsViewProps['window'];
  loading: boolean;
  error: string | null;
  onOpenSessions: SkillsToolsViewProps['onOpenSessions'];
}

function ToolChart({ buckets, intervalSeconds, workspace, window, loading, error, onOpenSessions }: ToolChartProps) {
  const intervalMs = Math.max(1, intervalSeconds) * 1000;
  const chart = useMemo(() => {
    const callsByTool = new Map<string, number>();
    for (const bucket of buckets) callsByTool.set(bucket.name, (callsByTool.get(bucket.name) || 0) + bucket.calls);
    const series = [...callsByTool.entries()]
      .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
      .map(([name], index) => ({ name, color: TOOL_COLORS[index] || TOOL_COLORS[TOOL_COLORS.length - 1] }));
    const allowed = new Set(series.map((item) => item.name));
    const byTime = new Map<number, Map<string, number>>();
    for (const bucket of buckets) {
      if (!allowed.has(bucket.name)) continue;
      const start = Date.parse(bucket.t);
      if (!Number.isFinite(start)) continue;
      let values = byTime.get(start);
      if (!values) {
        values = new Map();
        byTime.set(start, values);
      }
      values.set(bucket.name, (values.get(bucket.name) || 0) + bucket.calls);
    }
    const observed = [...byTime.keys()].sort((a, b) => a - b);
    const firstObserved = observed[0];
    if (firstObserved == null) return { series, columns: [], max: 1 };
    const requestedSince = window?.since ? Date.parse(window.since) : Number.NaN;
    const requestedBefore = window?.before ? Date.parse(window.before) : Number.NaN;
    const first = Number.isFinite(requestedSince)
      ? Math.floor(requestedSince / intervalMs) * intervalMs
      : firstObserved;
    const lastObserved = observed[observed.length - 1] ?? firstObserved;
    const end = Number.isFinite(requestedBefore)
      ? Math.ceil(requestedBefore / intervalMs) * intervalMs
      : lastObserved + intervalMs;
    const columns = [];
    for (let start = first; start < end; start += intervalMs) {
      const values = byTime.get(start) || new Map<string, number>();
      columns.push({
        start,
        values,
        total: [...values.values()].reduce((sum, value) => sum + value, 0),
      });
    }
    return { series, columns, max: Math.max(1, ...columns.map((column) => column.total)) };
  }, [buckets, intervalMs, window]);
  const intervalLabel = intervalSeconds >= 3600 ? `${intervalSeconds / 3600}-hour` : `${intervalSeconds / 60}-min`;

  return (
    <SurfaceCard className="tools-chart-panel">
      <div className="tools-chart-header">
        <div className="tools-chart-title">Tool calls over time</div>
        <ul className="tools-chart-legend" aria-label="Tool chart legend">
          {chart.series.map((series) => (
            <li key={series.name}>
              <i style={{ background: series.color }} /> {series.name}
            </li>
          ))}
          {chart.columns.length > 0 && <li>{intervalLabel} buckets</li>}
        </ul>
      </div>
      <div className="tools-chart-body">
        {loading && buckets.length === 0 ? (
          <div className="tools-chart-state">Loading tool calls…</div>
        ) : error && buckets.length === 0 ? (
          <div className="tools-chart-state">Failed to load tool calls: {error}</div>
        ) : chart.columns.length === 0 ? (
          <div className="tools-chart-state">No tool calls to chart in this range.</div>
        ) : (
          <div style={{ position: 'relative' }}>
            <ChartYAxis top={formatInteger(chart.max)} mid={formatInteger(Math.ceil(chart.max / 2))} />
            <div className="tools-chart-plot">
              {chart.columns.map((column) => (
                <div className="tools-chart-column" key={column.start}>
                  <div className="tools-chart-stack" style={{ height: `${(column.total / chart.max) * 100}%` }}>
                    {chart.series.map((series) => {
                      const calls = column.values.get(series.name) || 0;
                      if (calls === 0) return null;
                      const start = new Date(column.start).toISOString();
                      const before = new Date(column.start + intervalMs).toISOString();
                      const filters = sessionFilters(series.name, workspace, window, { since: start, before });
                      if (filters.since && filters.before && Date.parse(filters.since) >= Date.parse(filters.before)) {
                        return null;
                      }
                      return (
                        <a
                          key={series.name}
                          href={toolSessionsPath(filters)}
                          aria-label={`${series.name}, ${calls} ${calls === 1 ? 'call' : 'calls'}, ${formatBucketLabel(column.start, intervalMs)}`}
                          title={`${series.name}: ${formatInteger(calls)} ${calls === 1 ? 'call' : 'calls'}`}
                          style={{ flex: calls, background: series.color }}
                          onClick={(event: ReactMouseEvent<HTMLAnchorElement>) => {
                            if (!isPlainLeftClick(event)) return;
                            event.preventDefault();
                            onOpenSessions(filters);
                          }}
                        >
                          <span className="sr-only">
                            {series.name}, {calls} {calls === 1 ? 'call' : 'calls'}
                          </span>
                        </a>
                      );
                    })}
                  </div>
                </div>
              ))}
            </div>
            <ChartXLabels data={chart.columns.map((column) => ({ t: formatBucketLabel(column.start, intervalMs) }))} />
          </div>
        )}
      </div>
      <div className="tools-panel-footnote">Calls per bucket, stacked by tool.</div>
    </SurfaceCard>
  );
}

export type SkillsToolsContentProps = Omit<SkillsToolsViewProps, 'onSelectTab'>;

export function skillsToolsHeroStats(data: ToolAnalytics | null) {
  return [
    { label: 'Sessions with tool calls', value: formatInteger(Math.max(0, data?.totals.sessions || 0)) },
    { label: 'Tools', value: formatInteger(Math.max(0, data?.totals.tools || 0)) },
    { label: 'Calls', value: formatInteger(Math.max(0, data?.totals.calls || 0)) },
  ];
}

export function SkillsToolsContent(props: SkillsToolsContentProps) {
  const [query, setQuery] = useState('');
  const [debouncedQuery, setDebouncedQuery] = useState('');
  const [sort, setSort] = useState<ToolTableSort>({ key: 'calls', direction: 'desc' });
  useEffect(() => {
    const id = window.setTimeout(() => setDebouncedQuery(query.trim().toLowerCase()), SEARCH_DEBOUNCE_MS);
    return () => window.clearTimeout(id);
  }, [query]);

  const data = props.data;
  const rows = useMemo(
    () =>
      sortToolRows(
        (data?.rows || []).filter((row) => row.name.toLowerCase().includes(debouncedQuery)),
        sort,
      ),
    [data?.rows, debouncedQuery, sort],
  );
  const onSort = (key: ToolSortKey) => {
    setSort((current) =>
      current.key === key
        ? { key, direction: current.direction === 'desc' ? 'asc' : 'desc' }
        : { key, direction: key === 'name' ? 'asc' : 'desc' },
    );
  };
  const slowest = useMemo(
    () =>
      [...(data?.rows || [])]
        .filter((row) => row.p95_duration_seconds != null)
        .sort((a, b) => (b.p95_duration_seconds || 0) - (a.p95_duration_seconds || 0))[0],
    [data?.rows],
  );
  const failureRate = data?.totals.calls ? (data.totals.failures / data.totals.calls) * 100 : 0;
  const range = timeRangeOption(props.timeRange);
  const workspaces = useMemo<WorkspaceAggregate[]>(
    () =>
      (data?.workspaces || []).map((facet) => ({
        path: facet.path,
        count: facet.sessions,
        cost: null,
        costComplete: false,
        tokens: 0,
        dur: 0,
        last: 0,
      })),
    [data?.workspaces],
  );
  const totalSessions = workspaces.reduce((sum, workspace) => sum + workspace.count, 0);

  return (
    <>
      <div className="tools-stat-strip">
        {stat('tool calls', formatInteger(Math.max(0, data?.totals.calls || 0)))}
        {stat(
          'failed',
          `${formatInteger(Math.max(0, data?.totals.failures || 0))} · ${failureRate.toFixed(1)}%`,
          (data?.totals.failures || 0) > 0 ? 'var(--error-text)' : undefined,
        )}
        {stat('tools used', formatInteger(Math.max(0, data?.totals.tools || 0)))}
        {stat(
          'slowest p95',
          slowest ? duration(slowest.p95_duration_seconds) : '-',
          (slowest?.p95_duration_seconds || 0) > 10 ? 'var(--warning-text)' : undefined,
          slowest?.name,
        )}
        {stat(
          'timed calls',
          `${formatInteger(Math.max(0, data?.totals.duration_samples || 0))}/${formatInteger(
            Math.max(0, data?.totals.calls || 0),
          )}`,
        )}
      </div>
      <div className="tools-filter-bar">
        <label className="tools-search">
          <Icon name="search" size={14} />
          <span className="sr-only">Filter tools</span>
          <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder="Filter tools" />
        </label>
        <div className="tools-filter-spacer" />
        <WorkspaceFacet
          workspaces={workspaces}
          selected={props.workspace}
          onSelect={props.onWorkspaceChange}
          totalCount={totalSessions}
          totalCost={null}
          now={Date.now()}
          rangeLabel={range.label}
          countLabel="workspaces with tool calls"
        />
        <TimeRangePicker value={props.timeRange} onChange={props.onTimeRangeChange} />
        <button
          type="button"
          onClick={props.onRefresh}
          disabled={props.refreshing}
          title="Refresh"
          aria-label="Refresh tool analytics"
          style={{ ...iconBtn }}
          className="tools-refresh"
        >
          <Icon name="refresh" size={14} />
        </button>
      </div>
      <ToolTable
        rows={rows}
        allCalls={data?.totals.calls || 0}
        data={data}
        loading={props.loading}
        error={props.error}
        filtered={debouncedQuery.length > 0}
        sort={sort}
        onSort={onSort}
        workspace={props.workspace}
        window={props.window}
        onOpenSessions={props.onOpenSessions}
      />
      <ToolChart
        buckets={data?.buckets || []}
        intervalSeconds={data?.interval_seconds || 60}
        workspace={props.workspace}
        window={props.window}
        loading={props.loading}
        error={props.error}
        onOpenSessions={props.onOpenSessions}
      />
    </>
  );
}

export function SkillsToolsView(props: SkillsToolsViewProps) {
  const { onSelectTab, ...contentProps } = props;
  return (
    <AnalyticsPage
      stats={skillsToolsHeroStats(props.data)}
      tabs={{ active: 'skills', onSelect: onSelectTab }}
      style={{ paddingBottom: 40 }}
    >
      <SkillsToolsContent {...contentProps} />
    </AnalyticsPage>
  );
}
