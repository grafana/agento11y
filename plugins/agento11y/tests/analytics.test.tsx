import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  AnalyticsChart,
  type AnalyticsUnit,
  AnalyticsView,
  type AnalyticsViewProps,
} from '../internal/local/web/src/analytics';
import { App } from '../internal/local/web/src/app';
import { conversationsPath, workspaceFromLocation } from '../internal/local/web/src/routing';
import type { ConversationSummary, ModelPrices, TokenBuckets, TokenUsagePoint } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

const NOW = Date.parse('2026-04-12T12:00:00Z');
const EMPTY: TokenBuckets = { fresh_input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 };
const PRICES: ModelPrices = {
  'costly-model': { input: 10, output: 20, cache_read: 1, cache_write: 12.5 },
  'efficient-model': { input: 1, output: 2, cache_read: 0.1, cache_write: 1.25 },
};

function conversation(overrides: Partial<ConversationSummary> = {}): ConversationSummary {
  const buckets = overrides.token_buckets || { ...EMPTY, fresh_input: 100_000 };
  return {
    id: 'session-costly',
    title: 'Costly but compact',
    started_at: '2026-04-12T10:00:00Z',
    last_activity: '2026-04-12T11:00:00Z',
    calls: 8,
    input_tokens: buckets.fresh_input + buckets.cache_read + buckets.cache_write,
    output_tokens: buckets.output,
    total_tokens: Object.values(buckets).reduce((sum, value) => sum + value, 0),
    token_buckets: buckets,
    token_buckets_by_model: { 'costly-model': buckets },
    agents: ['pi'],
    models: ['costly-model'],
    status: 'ok',
    workspace: '/worktrees/costly',
    ...overrides,
  };
}

function efficientConversation(): ConversationSummary {
  const buckets = { ...EMPTY, fresh_input: 200_000 };
  return conversation({
    id: 'session-efficient',
    title: 'Large but efficient',
    started_at: '2026-04-12T09:00:00Z',
    last_activity: '2026-04-12T10:30:00Z',
    calls: 3,
    input_tokens: 200_000,
    total_tokens: 200_000,
    token_buckets: buckets,
    token_buckets_by_model: { 'efficient-model': buckets },
    models: ['efficient-model'],
    workspace: '/worktrees/efficient',
  });
}

function point(overrides: Partial<TokenUsagePoint> = {}): TokenUsagePoint {
  return {
    t: '2026-04-12T11:00:00Z',
    model: 'costly-model',
    calls: 1,
    fresh_input: 100_000,
    cache_read: 50_000,
    cache_write: 25_000,
    output: 10_000,
    reasoning: 5_000,
    ...overrides,
  };
}

function viewProps(overrides: Partial<AnalyticsViewProps> = {}): AnalyticsViewProps {
  const conversations = [conversation(), efficientConversation()];
  return {
    conversations,
    previousConversations: [],
    facetConversations: conversations,
    totalConversations: conversations.length,
    previousTotalConversations: 0,
    facetTotalConversations: conversations.length,
    tokenPoints: [
      point(),
      point({ t: '2026-04-12T10:30:00Z', model: 'efficient-model', fresh_input: 200_000, cache_read: 0 }),
    ],
    tokenIntervalMs: 3_600_000,
    heatmapPoints: [point()],
    loading: false,
    tokenLoading: false,
    heatmapLoading: false,
    error: null,
    previousError: null,
    facetError: null,
    tokenError: null,
    heatmapError: null,
    unit: 'cost',
    onUnitChange: vi.fn(),
    timeRange: '24h',
    onTimeRangeChange: vi.fn(),
    workspace: null,
    onWorkspaceChange: vi.fn(),
    hiddenSeries: new Set(),
    onToggleSeries: vi.fn(),
    onRefresh: vi.fn(),
    refreshing: false,
    onOpenConversation: vi.fn(),
    onOpenWorkspace: vi.fn(),
    onOpenBucket: vi.fn(),
    now: NOW,
    prices: PRICES,
    ...overrides,
  };
}

function ControlledAnalytics({ onChange }: { onChange: (unit: AnalyticsUnit) => void }) {
  const [unit, setUnit] = useState<AnalyticsUnit>('cost');
  return (
    <AnalyticsView
      {...viewProps()}
      unit={unit}
      onUnitChange={(next) => {
        onChange(next);
        setUnit(next);
      }}
    />
  );
}

function firstAttribute(selector: string, attribute: string): string | null {
  return document.querySelector(selector)?.getAttribute(attribute) || null;
}

function kpiCard(label: string): HTMLElement {
  const card = screen.getByText(label).parentElement?.parentElement;
  if (!card) throw new Error(`missing ${label} KPI card`);
  return card;
}

describe('analytics workspace routing', () => {
  it('keeps All and the unknown workspace as separate routes', () => {
    expect(conversationsPath()).toBe('/');
    expect(conversationsPath('')).toBe('/?workspace=');
    expect(conversationsPath('/work/a')).toBe('/?workspace=%2Fwork%2Fa');

    window.history.replaceState({}, '', '/');
    expect(workspaceFromLocation()).toBeNull();
    window.history.replaceState({}, '', '/?workspace=');
    expect(workspaceFromLocation()).toBe('');
    window.history.replaceState({}, '', '/?workspace=%2Fwork%2Fa');
    expect(workspaceFromLocation()).toBe('/work/a');
  });
});

describe('App analytics loading', () => {
  it('renders analytics and returns from a session', async () => {
    window.history.replaceState({}, '', '/analytics');
    const stored = new Map<string, string>();
    vi.stubGlobal('localStorage', {
      getItem: (key: string) => stored.get(key) ?? null,
      setItem: (key: string, value: string) => stored.set(key, value),
      removeItem: (key: string) => stored.delete(key),
    });
    const response = (body: unknown): Response =>
      ({
        ok: true,
        status: 200,
        json: () => Promise.resolve(body),
        text: () => Promise.resolve(''),
      }) as Response;
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response({ conversations: [conversation()], matched_conversations: 1 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [point()], interval_seconds: 3600 }));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [], total_conversations: 0 }));
      }
      if (url === '/api/v1/conversations/session-costly') {
        return Promise.resolve(response({ id: 'session-costly', generations: [] }));
      }
      if (url === '/api/v1/config') return Promise.resolve(response({}));
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    await waitFor(() => expect(screen.getByText('Costly but compact')).toBeTruthy());
    expect(fetchMock.mock.calls.some(([url]) => String(url).includes('interval=900'))).toBe(true);
    expect(fetchMock.mock.calls.some(([url]) => String(url).startsWith('/api/v1/metrics/tools?'))).toBe(false);

    const session = document.querySelector<HTMLAnchorElement>('a[data-session-id="session-costly"]');
    if (!session) throw new Error('missing analytics session link');
    fireEvent.click(session);
    const back = await screen.findByRole('link', { name: 'Back to Analytics' });
    expect(window.location.pathname).toBe('/conversations/session-costly');

    fireEvent.click(back);
    expect(window.location.pathname).toBe('/analytics');
    expect(screen.getByRole('heading', { name: 'Analytics' })).toBeTruthy();
  });
});

describe('AnalyticsView', () => {
  it('reports the Cost/Tokens switch and re-sorts workspaces, models, and heaviest sessions', () => {
    const onChange = vi.fn();
    render(<ControlledAnalytics onChange={onChange} />);

    expect(firstAttribute('[data-model-row]', 'data-model-row')).toBe('costly-model');
    expect(firstAttribute('a[href^="/?workspace="]', 'href')).toContain('costly');
    expect(firstAttribute('[data-session-id]', 'data-session-id')).toBe('session-costly');

    fireEvent.click(screen.getByRole('button', { name: 'Tokens' }));
    expect(onChange).toHaveBeenCalledWith('tokens');
    expect(firstAttribute('[data-model-row]', 'data-model-row')).toBe('efficient-model');
    expect(firstAttribute('a[href^="/?workspace="]', 'href')).toContain('efficient');
    expect(firstAttribute('[data-session-id]', 'data-session-id')).toBe('session-efficient');
  });

  it('shows a range-specific message in every data panel when the range is empty', () => {
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [],
          totalConversations: 4,
          tokenPoints: [],
          heatmapPoints: [],
        })}
      />,
    );

    for (const title of ['Cost over time', 'Workspaces', 'Models', 'Sessions', 'Heaviest sessions']) {
      const panels = screen.getAllByText(title);
      expect(
        panels.some((heading) => heading.parentElement?.parentElement?.textContent?.includes('Last 24 hours')),
      ).toBe(true);
    }
    expect(screen.getByText('No agent usage in the last 7 days.')).toBeTruthy();
  });

  it('discloses current and previous range coverage', () => {
    render(
      <AnalyticsView
        {...viewProps({
          totalConversations: 19,
          previousConversations: [conversation({ id: 'previous' })],
          previousTotalConversations: 7,
        })}
      />,
    );
    expect(screen.getByText(/Coverage: session panels use 2 returned sessions from 19 in range/)).toBeTruthy();
    expect(screen.getByText(/Token charts and trends cover all generations in range/)).toBeTruthy();
    expect(screen.getByText(/Previous-period comparisons are unavailable: 1 of 7 sessions returned/)).toBeTruthy();
    expect(screen.queryByText(/vs previous period$/)).toBeNull();
  });

  it('uses uncapped aggregates for KPI totals and comparisons', () => {
    const aggregate = {
      calls: 12,
      errored: 1,
      agents: 2,
      agent_hosts: ['claude-code', 'pi'],
      workspaces: 3,
      token_buckets: { ...EMPTY, fresh_input: 200_000, cache_read: 100_000 },
      token_buckets_by_model: {
        'costly-model': { ...EMPTY, fresh_input: 100_000 },
        unknown: { ...EMPTY, fresh_input: 100_000, cache_read: 100_000 },
      },
      models: ['costly-model', 'unknown'],
    };
    const previousAggregate = {
      calls: 6,
      errored: 0,
      agents: 1,
      agent_hosts: ['pi'],
      workspaces: 1,
      token_buckets: { ...EMPTY, fresh_input: 50_000, cache_read: 50_000 },
      token_buckets_by_model: { 'costly-model': { ...EMPTY, fresh_input: 50_000, cache_read: 50_000 } },
      models: ['costly-model'],
    };
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [conversation()],
          previousConversations: [conversation({ id: 'previous' })],
          aggregate,
          previousAggregate,
          totalConversations: 3,
          previousTotalConversations: 2,
        })}
      />,
    );

    const hero = screen.getByRole('heading', { name: 'Analytics' }).parentElement?.parentElement;
    expect(hero?.textContent).toContain('Sessions3');
    expect(hero?.textContent).toContain('Workspaces3');
    expect(hero?.textContent).toContain('Agents2');
    expect(screen.getByText('≥$1')).toBeTruthy();
    expect(screen.getByText('Partial estimate; unpriced usage is excluded.')).toBeTruthy();
    expect(kpiCard('Total tokens').textContent).toContain('300k');
    expect(kpiCard('Total tokens').textContent).toContain('100k avg / session');
    expect(kpiCard('Total tokens').textContent).toContain('2 models');
    expect(kpiCard('Input cache hit').textContent).toContain('33%');
    expect(kpiCard('Input cache hit').textContent).toContain('100k cache reads');
    expect(kpiCard('Model calls').textContent).toContain('12');
    expect(kpiCard('Model calls').textContent).toContain('4 avg / session');
    expect(kpiCard('Model calls').textContent).toContain('1 errored session');
    expect(document.querySelector('[data-model-row="unknown"]')).toBeTruthy();
    expect(
      screen.getByText(/KPI totals, model totals, token charts, and trends cover all generations in range/),
    ).toBeTruthy();
    expect(screen.getAllByText(/vs previous period$/)).toHaveLength(3);
  });

  it('refreshes without passing the click event into the fetch callback', () => {
    const onRefresh = vi.fn();
    render(<AnalyticsView {...viewProps({ onRefresh })} />);

    fireEvent.click(screen.getByRole('button', { name: 'Refresh analytics' }));
    expect(onRefresh).toHaveBeenCalledOnce();
    expect(onRefresh.mock.calls[0]).toEqual([]);
  });

  it('puts measure controls before filters and omits the duplicate range stat', () => {
    render(<AnalyticsView {...viewProps()} />);

    const hero = screen.getByRole('heading', { name: 'Analytics' }).parentElement?.parentElement;
    expect(hero?.textContent).not.toContain('Range');

    const measure = screen.getByRole('group', { name: 'Measure in' });
    const refresh = screen.getByRole('button', { name: 'Refresh analytics' });
    const workspace = screen.getByTitle('Filter by workspace');
    const range = screen.getByTitle('Time range');
    expect(measure.compareDocumentPosition(workspace) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(workspace.compareDocumentPosition(range) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(range.compareDocumentPosition(refresh) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('keeps session rows as real anchors and intercepts only plain clicks', () => {
    const onOpenConversation = vi.fn();
    render(<AnalyticsView {...viewProps({ onOpenConversation })} />);
    const link = document.querySelector<HTMLAnchorElement>('a[data-session-id="session-costly"]');
    expect(link?.getAttribute('href')).toBe('/conversations/session-costly');

    if (!link) throw new Error('missing heaviest-session link');
    fireEvent.click(link, { button: 0 });
    expect(onOpenConversation).toHaveBeenCalledWith({ id: 'session-costly' });

    onOpenConversation.mockClear();
    fireEvent.click(link, { button: 0, metaKey: true });
    expect(onOpenConversation).not.toHaveBeenCalled();
  });

  it('keeps the unknown workspace distinct in workspace links', () => {
    render(<AnalyticsView {...viewProps({ conversations: [conversation({ workspace: '' })] })} />);
    expect(document.querySelector('a[href="/?workspace="]')).toBeTruthy();
  });

  it('keeps a selected workspace visible when facet coverage is incomplete', () => {
    const selected = conversation({ workspace: '/outside/page' });
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [selected],
          totalConversations: 1,
          workspace: '/outside/page',
          facetTotalConversations: 19,
        })}
      />,
    );
    expect(screen.getByText('Options cover 2 of 19 sessions in range.')).toBeTruthy();
    expect(screen.getByText('1/3')).toBeTruthy();
  });

  it('draws the model-call trend from generation points, including calls without tokens', () => {
    const emptyPoint = { ...EMPTY, model: 'costly-model' };
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [conversation({ calls: 7 })],
          totalConversations: 1,
          tokenPoints: [
            point({ ...emptyPoint, t: new Date(NOW - 18 * 60 * 60 * 1000).toISOString(), calls: 2 }),
            point({ ...emptyPoint, t: new Date(NOW - 6 * 60 * 60 * 1000).toISOString(), calls: 5 }),
          ],
          tokenIntervalMs: 12 * 60 * 60 * 1000,
        })}
      />,
    );

    const bars = kpiCard('Model calls').querySelectorAll<HTMLElement>('[data-spark-key]');
    expect(bars).toHaveLength(2);
    expect([...bars].map((bar) => bar.style.height)).toEqual(['40%', '100%']);
  });

  it('builds usage trends from generation points', () => {
    const early = NOW - 20 * 60 * 60 * 1000;
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [conversation()],
          totalConversations: 1,
          tokenPoints: [point({ t: new Date(early).toISOString() })],
          tokenIntervalMs: 12 * 60 * 60 * 1000,
        })}
      />,
    );
    const costBar = document.querySelector('[data-spark-key][data-cost-status="complete"]');
    expect(Number(costBar?.getAttribute('data-spark-key'))).toBeLessThan(NOW - 10 * 60 * 60 * 1000);
    const sparkKeys = new Set(
      [...document.querySelectorAll('[data-spark-key]')].map((element) => element.getAttribute('data-spark-key')),
    );
    expect(sparkKeys.size).toBe(2);
  });

  it('states the comparison period on KPI deltas', () => {
    const current = conversation({ token_buckets: { ...EMPTY, fresh_input: 49, cache_read: 51 } });
    const previous = conversation({
      id: 'previous-costly',
      token_buckets: { ...EMPTY, fresh_input: 50, cache_read: 50 },
    });
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [current],
          totalConversations: 1,
          previousConversations: [previous],
          previousTotalConversations: 1,
        })}
      />,
    );
    expect(screen.getAllByText(/vs previous period$/).length).toBeGreaterThanOrEqual(3);
    expect(screen.getByText('+1 pt vs previous period')).toBeTruthy();
  });

  it('labels model rates and session-shape boundaries precisely', () => {
    const exact = conversation({
      id: 'exact-10m',
      token_buckets: { ...EMPTY, fresh_input: 10_000_000 },
      token_buckets_by_model: { 'costly-model': { ...EMPTY, fresh_input: 10_000_000 } },
    });
    render(<AnalyticsView {...viewProps({ conversations: [exact] })} />);
    expect(screen.getByText('estimated blended cost per 1M tokens')).toBeTruthy();
    expect(screen.getByText('Estimated model cost divided by all recorded tokens.')).toBeTruthy();
    expect(document.querySelector('[data-shape-bucket="over-10m"]')?.getAttribute('data-shape-count')).toBe('1');
  });

  it('exposes browser-local heatmap values to keyboard users', () => {
    const heatPoint = point();
    const { rerender } = render(<AnalyticsView {...viewProps({ unit: 'tokens', heatmapPoints: [heatPoint] })} />);
    const date = new Date(heatPoint.t);
    const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    const label = `${days[date.getDay()]} ${String(date.getHours()).padStart(2, '0')}:00 local`;
    const cell = screen.getByRole('button', { name: `${label}, 190k tokens` });
    expect(cell.getAttribute('title')).toBe(`${label} · 190k tokens`);
    cell.focus();
    expect(document.activeElement).toBe(cell);
    fireEvent.focus(cell);
    expect(screen.getByText(`${label} · 190k tokens`)).toBeTruthy();
    expect(screen.getByText('Usage')).toBeTruthy();
    expect(screen.getByText('last 7 days · browser-local time')).toBeTruthy();

    rerender(<AnalyticsView {...viewProps({ unit: 'cost', heatmapPoints: [heatPoint] })} />);
    const costCell = screen.getByRole('button', { name: `${label}, $1.66` });
    expect(costCell.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByText(`${label} · $1.66`)).toBeTruthy();
    expect(screen.queryByText(`${label} · 190k tokens`)).toBeNull();
  });

  it('shows heatmap loading and source-specific failures', () => {
    const { rerender } = render(
      <AnalyticsView
        {...viewProps({
          heatmapPoints: [],
          heatmapLoading: true,
        })}
      />,
    );
    expect(screen.getByText('Loading agent usage…')).toBeTruthy();

    rerender(<AnalyticsView {...viewProps({ heatmapPoints: [], heatmapError: 'HTTP 500', tokenError: 'HTTP 503' })} />);
    expect(screen.getByText('Failed to load agent usage: HTTP 500')).toBeTruthy();
    expect(screen.getByText('Token usage did not refresh')).toBeTruthy();
    expect(screen.getByText('HTTP 503')).toBeTruthy();
  });

  it('marks unpriced cost as unknown instead of zero', () => {
    const unknownPoint = point({ model: 'unknown' });
    const unknownConversation = conversation({
      models: ['unknown'],
      token_buckets_by_model: { unknown: unknownPoint },
    });
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [unknownConversation],
          tokenPoints: [unknownPoint],
          heatmapPoints: [unknownPoint],
          prices: {},
        })}
      />,
    );
    const date = new Date(unknownPoint.t);
    const days = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
    const label = `${days[date.getDay()]} ${String(date.getHours()).padStart(2, '0')}:00 local`;
    const unknownCell = screen.getByRole('button', { name: `${label}, —` });
    expect(unknownCell.getAttribute('data-cost-status')).toBe('unknown');
    expect(document.querySelector('g[data-cost-status="unknown"]')).toBeTruthy();
    expect(document.querySelector('[data-bar-unit="cost"]')).toBeNull();
    expect(screen.getByText('No priced model usage in this range.')).toBeTruthy();
  });

  it('keeps exact non-cost deltas when spend is incomplete', () => {
    const current = conversation({
      token_buckets_by_model: {
        'costly-model': { ...EMPTY, fresh_input: 50_000 },
        unknown: { ...EMPTY, fresh_input: 50_000 },
      },
    });
    const previous = conversation({
      id: 'previous',
      calls: 4,
      token_buckets: { ...EMPTY, fresh_input: 50_000 },
      token_buckets_by_model: { unknown: { ...EMPTY, fresh_input: 50_000 } },
    });
    render(
      <AnalyticsView
        {...viewProps({
          conversations: [current],
          facetConversations: [current],
          totalConversations: 1,
          previousConversations: [previous],
          previousTotalConversations: 1,
        })}
      />,
    );
    expect(screen.getAllByText(/vs previous period$/)).toHaveLength(3);
    expect(screen.getByText('Partial estimate; unpriced usage is excluded.')).toBeTruthy();
  });

  it('marks mixed priced and unpriced cost as a partial estimate', () => {
    const mixed = conversation({
      token_buckets_by_model: {
        'costly-model': { ...EMPTY, fresh_input: 100_000 },
        unknown: { ...EMPTY, fresh_input: 100_000 },
      },
    });
    render(<AnalyticsView {...viewProps({ conversations: [mixed] })} />);
    expect(screen.getAllByText(/≥/).length).toBeGreaterThan(0);
    expect(screen.getByText('Partial estimate; unpriced usage is excluded.')).toBeTruthy();
  });
});

describe('AnalyticsChart', () => {
  function renderChart(unit: AnalyticsUnit, overrides: Partial<React.ComponentProps<typeof AnalyticsChart>> = {}) {
    const onOpenBucket = vi.fn();
    render(
      <AnalyticsChart
        points={[point()]}
        tokenIntervalMs={3_600_000}
        unit={unit}
        timeRange="24h"
        hiddenSeries={new Set()}
        onToggleSeries={vi.fn()}
        onOpenBucket={onOpenBucket}
        now={NOW}
        prices={PRICES}
        {...overrides}
      />,
    );
    return onOpenBucket;
  }

  it('switches the title and uses one cost bar or stacked TOKEN_SERIES bars', () => {
    const costRender = renderChart('cost');
    expect(screen.getAllByText('Cost over time').length).toBe(2);
    expect(document.querySelectorAll('[data-bar-unit="cost"]')).toHaveLength(1);
    expect(document.querySelector('[data-series]')).toBeNull();
    const costLine = document.querySelector('[data-secondary-series="tokens"]');
    expect(costLine?.getAttribute('vector-effect')).toBe('non-scaling-stroke');
    cleanup();

    renderChart('tokens');
    expect(screen.getAllByText('Tokens over time').length).toBe(2);
    expect(document.querySelector('[data-series="fresh_input"]')).toBeTruthy();
    expect(document.querySelector('[data-series="cache_read"]')).toBeTruthy();
    expect(document.querySelector('[data-series="cache_write"]')).toBeTruthy();
    expect(document.querySelector('[data-series="output"]')).toBeTruthy();
    expect(document.querySelector('[data-series="reasoning"]')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Input' }).getAttribute('aria-pressed')).toBe('true');
    expect(document.querySelector('[data-secondary-series="cost"]')?.getAttribute('vector-effect')).toBe(
      'non-scaling-stroke',
    );
    expect(costRender).not.toHaveBeenCalled();
  });

  it('omits partial cost from the secondary line', () => {
    renderChart('tokens', {
      points: [point(), point({ model: 'unknown', fresh_input: 20_000 })],
    });
    const partial = document.querySelector('g[data-cost-status="partial"]');
    const partialX = partial?.getAttribute('data-line-x');
    const linePoints = [...document.querySelectorAll('[data-secondary-series="cost"]')]
      .flatMap((line) => (line.getAttribute('points') || '').split(' '))
      .filter(Boolean);
    expect(partialX).toBeTruthy();
    expect(linePoints.some((pointValue) => pointValue.startsWith(`${partialX},`))).toBe(false);
    expect(screen.getByText('Estimated cost · partial buckets omitted')).toBeTruthy();
  });

  it('labels a free model cost axis as zero', () => {
    renderChart('cost', {
      prices: { free: { input: 0, output: 0 } },
      points: [point({ model: 'free' })],
    });
    expect(screen.getAllByText('$0').length).toBeGreaterThanOrEqual(3);
    expect(screen.queryByText('<$0.01')).toBeNull();
  });

  it('keeps the first snapped bucket in a long range', () => {
    const interval = 7 * 24 * 60 * 60 * 1000;
    const start = Math.floor((NOW - 90 * 24 * 60 * 60 * 1000) / interval) * interval;
    renderChart('tokens', {
      timeRange: '90d',
      tokenIntervalMs: interval,
      points: [
        point({
          t: new Date(start).toISOString(),
          fresh_input: 321_000,
          cache_read: 0,
          cache_write: 0,
          output: 0,
          reasoning: 0,
        }),
      ],
    });
    expect(screen.getByRole('button', { name: /321k tokens.*Open this time bucket/ })).toBeTruthy();
  });

  it('ignores calls without tokens when deriving an all-range usage grid', () => {
    renderChart('tokens', { timeRange: 'all' });
    const expectedBuckets = document.querySelectorAll('g[data-line-x]').length;
    cleanup();

    renderChart('tokens', {
      timeRange: 'all',
      points: [point(), point({ ...EMPTY, calls: 1, t: '2020-01-01T00:00:00Z', model: 'old-tokenless-model' })],
    });
    expect(document.querySelectorAll('g[data-line-x]')).toHaveLength(expectedBuckets);
  });

  it.each([
    {
      name: 'complete',
      points: [point()],
      status: 'complete',
      expected: 'estimated cost $1.66',
    },
    {
      name: 'partial',
      points: [point(), point({ model: 'unknown', fresh_input: 20_000 })],
      status: 'partial',
      expected: 'estimated cost ≥$1.66; unpriced usage excluded',
    },
    {
      name: 'unknown',
      points: [point({ model: 'unknown' })],
      status: 'unknown',
      expected: 'estimated cost unavailable',
    },
  ])('shows $name cost when every token series is hidden', ({ points, status, expected }) => {
    renderChart('cost', {
      points,
      hiddenSeries: new Set(Object.keys(EMPTY) as Array<keyof TokenBuckets>),
    });
    const bucket = document.querySelector(`g[data-cost-status="${status}"]`);
    if (!bucket) throw new Error(`missing ${status} cost bucket`);

    fireEvent.mouseEnter(bucket);

    const tooltip = screen.getByRole('tooltip');
    expect(tooltip.textContent).toContain(expected);
    expect(tooltip.textContent).not.toContain('0 tok');
  });

  it('opens chart buckets with mouse and keyboard', () => {
    const onOpenBucket = renderChart('tokens');
    const bucket = screen
      .getAllByRole('button', { name: /Open this time bucket/ })
      .find((button) => button.getAttribute('aria-label')?.includes('190k'));
    if (!bucket) throw new Error('missing populated chart bucket');

    fireEvent.click(bucket);
    expect(onOpenBucket).toHaveBeenCalledTimes(1);

    fireEvent.keyDown(bucket, { key: 'Enter' });
    expect(onOpenBucket).toHaveBeenCalledTimes(2);
    expect(onOpenBucket.mock.calls[1]?.[0]).toMatchObject({ start: expect.any(Number), end: expect.any(Number) });
  });
});
