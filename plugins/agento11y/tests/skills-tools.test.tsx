import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import {
  analyticsPath,
  analyticsTabFromLocation,
  toolSessionFiltersFromLocation,
  toolSessionsPath,
} from '../internal/local/web/src/routing';
import { SkillsToolsView, type SkillsToolsViewProps } from '../internal/local/web/src/skills-tools';
import type { ToolAnalytics } from '../internal/local/web/src/types';
import { metricsResponse, response } from './fixtures';

const DATA: ToolAnalytics = {
  totals: { calls: 31, failures: 2, tools: 3, sessions: 4, duration_samples: 21 },
  rows: [
    {
      name: 'Bash',
      calls: 20,
      failures: 1,
      sessions: 3,
      duration_samples: 20,
      p50_duration_seconds: 10,
      p95_duration_seconds: 10,
    },
    {
      name: 'mcp__grafana__query',
      calls: 10,
      failures: 1,
      sessions: 2,
      duration_samples: 1,
      p50_duration_seconds: 11,
      p95_duration_seconds: 11,
    },
    { name: 'Read', calls: 1, failures: 0, sessions: 1, duration_samples: 0 },
  ],
  buckets: [
    { t: '2026-08-21T10:00:00Z', name: 'Bash', calls: 2, failures: 0 },
    { t: '2026-08-21T10:00:00Z', name: 'mcp__grafana__query', calls: 1, failures: 1 },
    { t: '2026-08-21T10:05:00Z', name: 'Read', calls: 1, failures: 0 },
  ],
  workspaces: [
    { path: '/repo', calls: 30, sessions: 3 },
    { path: '', calls: 1, sessions: 1 },
  ],
  interval_seconds: 300,
  coverage: { generation_calls: 25, projected_spans: 21, matched_calls: 15 },
};

function props(overrides: Partial<SkillsToolsViewProps> = {}): SkillsToolsViewProps {
  return {
    data: DATA,
    loading: false,
    error: null,
    timeRange: '24h',
    onTimeRangeChange: vi.fn(),
    workspace: '/repo',
    onWorkspaceChange: vi.fn(),
    window: { since: '2026-08-21T10:00:00Z', before: '2026-08-21T11:00:00Z' },
    onRefresh: vi.fn(),
    refreshing: false,
    onSelectTab: vi.fn(),
    onOpenSessions: vi.fn(),
    ...overrides,
  };
}

function stubLocalStorage(entries: [string, string][] = []) {
  const stored = new Map(entries);
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => stored.get(key) ?? null,
    setItem: (key: string, value: string) => stored.set(key, value),
    removeItem: (key: string) => stored.delete(key),
  });
  return stored;
}

function toolRowNames(): string[] {
  return [...document.querySelectorAll('[data-tool-row]')].map((row) => row.getAttribute('data-tool-row') || '');
}

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
});

describe('Tools analytics component', () => {
  it('renders exact totals, thresholds, missing durations, and drilldown links', () => {
    render(<SkillsToolsView {...props()} />);

    expect(screen.getByRole('link', { name: 'Tools' })).toBeTruthy();
    expect(screen.getByText('2 · 6.5%')).toBeTruthy();
    expect(screen.getByText('21/31')).toBeTruthy();
    expect(screen.queryByText(/Source coverage:/)).toBeNull();
    expect(screen.queryByText(/Activate a segment/)).toBeNull();
    expect(screen.getByText('Calls per bucket, stacked by tool.')).toBeTruthy();
    const bash = document.querySelector<HTMLTableRowElement>('tr[data-tool-row="Bash"]');
    const grafana = document.querySelector<HTMLTableRowElement>('tr[data-tool-row="mcp__grafana__query"]');
    const read = document.querySelector<HTMLTableRowElement>('tr[data-tool-row="Read"]');
    if (!bash || !grafana || !read) throw new Error('missing tool rows');
    expect(bash.cells[4]?.style.color).toBe('var(--fg3)');
    expect(bash.cells[6]?.style.color).toBe('var(--fg2)');
    expect(grafana.cells[4]?.style.color).toBe('var(--error-text)');
    expect(grafana.cells[6]?.style.color).toBe('var(--warning-text)');
    expect(read.cells[5]?.textContent).toBe('-');
    expect(read.cells[6]?.textContent).toBe('-');

    const rowLink = screen.getByRole('link', { name: 'Bash' });
    expect(rowLink.getAttribute('href')).toBe(
      '/?tool=Bash&workspace=%2Frepo&since=2026-08-21T10%3A00%3A00Z&before=2026-08-21T11%3A00%3A00Z',
    );
    const segment = screen.getByRole('link', { name: /Bash, 2 calls/ });
    expect(segment.getAttribute('href')).toBe(
      '/?tool=Bash&workspace=%2Frepo&since=2026-08-21T10%3A00%3A00.000Z&before=2026-08-21T10%3A05%3A00.000Z',
    );
  });

  it('sorts by every data column and keeps missing durations last', () => {
    render(<SkillsToolsView {...props()} />);

    const calls = screen.getByRole('button', { name: 'Calls' });
    expect(calls.closest('th')?.getAttribute('aria-sort')).toBe('descending');
    expect(toolRowNames()).toEqual(['Bash', 'mcp__grafana__query', 'Read']);

    const tool = screen.getByRole('button', { name: 'Tool' });
    fireEvent.click(tool);
    expect(tool.closest('th')?.getAttribute('aria-sort')).toBe('ascending');
    expect(toolRowNames()).toEqual(['Bash', 'mcp__grafana__query', 'Read']);
    fireEvent.click(tool);
    expect(tool.closest('th')?.getAttribute('aria-sort')).toBe('descending');
    expect(toolRowNames()).toEqual(['Read', 'mcp__grafana__query', 'Bash']);

    for (const label of ['Share', 'Calls', 'Failed', 'p50', 'Sessions', 'p95']) {
      const header = screen.getByRole('button', { name: label });
      fireEvent.click(header);
      expect(header.closest('th')?.getAttribute('aria-sort')).toBe('descending');
    }
    fireEvent.click(screen.getByRole('button', { name: 'p95' }));
    expect(screen.getByRole('button', { name: 'p95' }).closest('th')?.getAttribute('aria-sort')).toBe('ascending');
    expect(toolRowNames()).toEqual(['Bash', 'mcp__grafana__query', 'Read']);
  });

  it('places refresh after the workspace and time range controls', () => {
    render(<SkillsToolsView {...props()} />);

    const search = screen.getByRole('textbox', { name: 'Filter tools' });
    const workspace = screen.getByTitle('Filter by workspace');
    const range = screen.getByTitle('Time range');
    const refresh = screen.getByRole('button', { name: 'Refresh tool analytics' });
    expect(search.compareDocumentPosition(workspace) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(workspace.compareDocumentPosition(range) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    expect(range.compareDocumentPosition(refresh) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });

  it('filters rows after a local debounce without fetching', async () => {
    vi.useFakeTimers();
    const fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
    render(<SkillsToolsView {...props()} />);

    const grafanaShare = document.querySelector<HTMLSpanElement>(
      'tr[data-tool-row="mcp__grafana__query"] .tools-share-fill',
    );
    expect(grafanaShare?.style.width).toBe('50%');
    fireEvent.change(screen.getByRole('textbox', { name: 'Filter tools' }), { target: { value: 'grafana' } });
    expect(document.querySelectorAll('[data-tool-row]')).toHaveLength(3);
    act(() => vi.advanceTimersByTime(149));
    expect(document.querySelectorAll('[data-tool-row]')).toHaveLength(3);
    act(() => vi.advanceTimersByTime(1));
    expect(document.querySelectorAll('[data-tool-row]')).toHaveLength(1);
    expect(grafanaShare?.style.width).toBe('50%');
    expect(screen.getAllByText('mcp__grafana__query').length).toBeGreaterThan(0);
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('keeps empty intervals and clamps chart links to the selected window', () => {
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            buckets: [
              { t: '2026-08-21T10:00:00Z', name: 'Bash', calls: 2, failures: 0 },
              { t: '2026-08-21T10:10:00Z', name: 'Read', calls: 1, failures: 0 },
            ],
          },
          window: { since: '2026-08-21T10:02:00Z', before: '2026-08-21T10:13:00Z' },
        })}
      />,
    );

    const columns = document.querySelectorAll('.tools-chart-column');
    expect(columns).toHaveLength(3);
    expect(columns[1]?.querySelector('a')).toBeNull();
    expect(screen.getByRole('link', { name: /Bash, 2 calls/ }).getAttribute('href')).toContain(
      'since=2026-08-21T10%3A02%3A00.000Z&before=2026-08-21T10%3A05%3A00.000Z',
    );
    expect(screen.getByRole('link', { name: /Read, 1 call/ }).getAttribute('href')).toContain(
      'since=2026-08-21T10%3A10%3A00.000Z&before=2026-08-21T10%3A13%3A00.000Z',
    );
  });

  it('keeps every tool as an exact chart link when the legend wraps', () => {
    const names = ['one', 'two', 'three', 'four', 'five', 'six', 'seven'];
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            buckets: names.map((name) => ({ t: '2026-08-21T10:00:00Z', name, calls: 1, failures: 0 })),
          },
        })}
      />,
    );

    for (const name of names) {
      const link = screen.getByRole('link', { name: new RegExp(`^${name}, 1 call,`) });
      expect(new URL(link.getAttribute('href') || '', 'http://localhost').searchParams.get('tool')).toBe(name);
    }
  });

  it('keeps explicit loading, error, empty, and no-match states', () => {
    const { rerender } = render(<SkillsToolsView {...props({ data: null, loading: true })} />);
    expect(document.querySelectorAll('.tools-skeleton')).toHaveLength(6);
    for (const skeleton of document.querySelectorAll('.tools-skeleton')) {
      expect(skeleton.parentElement?.classList.contains('tools-state-cell')).toBe(true);
    }
    expect(screen.getByText('Loading tool calls…')).toBeTruthy();

    rerender(<SkillsToolsView {...props({ data: null, loading: false, error: 'offline' })} />);
    expect(screen.getByText('Failed to load tool analytics')).toBeTruthy();
    expect(screen.getByText('Failed to load tool calls: offline')).toBeTruthy();

    rerender(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            totals: { calls: 0, failures: 0, tools: 0, sessions: 0, duration_samples: 0 },
            rows: [],
            buckets: [],
          },
        })}
      />,
    );
    expect(screen.getByText('No tools recorded in this range. Try a wider time range.')).toBeTruthy();
  });
});

describe('Tools analytics routing and App fetching', () => {
  it('parses final Analytics and exact Sessions URLs', () => {
    expect(analyticsPath('skills')).toBe('/analytics?tab=skills&mode=tools');
    window.history.replaceState({}, '', analyticsPath('skills'));
    expect(analyticsTabFromLocation()).toBe('skills');

    const path = toolSessionsPath({
      tool: 'mcp__grafana__query',
      workspace: '',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    });
    window.history.replaceState({}, '', path);
    expect(toolSessionFiltersFromLocation()).toEqual({
      tool: 'mcp__grafana__query',
      workspace: '',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    });
  });

  it('opens Tools directly with one tools request and keeps the saved Overview unit', async () => {
    window.history.replaceState({}, '', analyticsPath('skills'));
    const stored = stubLocalStorage([['sigil.local.analyticsUnit', 'tokens']]);
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/skills-tools?')) return Promise.resolve(response({ tools: DATA }));
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [], total_conversations: 0 }));
      }
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse()));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 60 }));
      }
      if (url === '/api/v1/config') return Promise.resolve(response({}));
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await screen.findByRole('link', { name: 'Bash' });
    const toolsRequests = fetchMock.mock.calls.filter(([url]) =>
      String(url).startsWith('/api/v1/metrics/skills-tools?'),
    );
    expect(toolsRequests).toHaveLength(1);
    expect(fetchMock.mock.calls.some(([url]) => String(url).startsWith('/api/v1/metrics/conversations?'))).toBe(false);
    expect(fetchMock.mock.calls.some(([url]) => String(url).startsWith('/api/v1/metrics/tools?'))).toBe(false);
    expect(screen.queryByText('Measure in')).toBeNull();
    expect(screen.getAllByRole('heading', { name: 'Analytics' })).toHaveLength(1);
    expect(screen.getAllByRole('navigation', { name: 'Analytics views' })).toHaveLength(1);
    expect(stored.get('sigil.local.analyticsUnit')).toBe('tokens');

    const overviewTab = screen.getByRole('link', { name: 'Overview' });
    overviewTab.focus();
    fireEvent.click(overviewTab);
    await screen.findByText('Measure in');
    expect(document.activeElement).toBe(overviewTab);
    expect(screen.getAllByRole('heading', { name: 'Analytics' })).toHaveLength(1);
    expect(screen.getAllByRole('navigation', { name: 'Analytics views' })).toHaveLength(1);
  });

  it('ignores an older tools response after the range changes', async () => {
    window.history.replaceState({}, '', analyticsPath('skills'));
    stubLocalStorage();
    const pending: Array<(value: Response) => void> = [];
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/skills-tools?')) {
        return new Promise<Response>((resolve) => pending.push(resolve));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [], total_conversations: 0 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 60 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await waitFor(() => expect(pending).toHaveLength(1));
    fireEvent.click(screen.getByRole('button', { name: 'Last 24 hours' }));
    fireEvent.click(screen.getByRole('button', { name: 'Last 1 hour' }));
    await waitFor(() => expect(pending).toHaveLength(2));

    act(() => pending[1]?.(response({ tools: DATA })));
    await screen.findByRole('link', { name: 'Bash' });
    act(() =>
      pending[0]?.(
        response({
          tools: {
            ...DATA,
            rows: [{ name: 'stale', calls: 1, failures: 0, sessions: 1, duration_samples: 0 }],
          },
        }),
      ),
    );
    await waitFor(() => expect(screen.queryByRole('link', { name: 'stale' })).toBeNull());
    expect(screen.getByRole('link', { name: 'Bash' })).toBeTruthy();
  });

  it('preserves exact tool and time filters when the workspace changes', async () => {
    const filters = {
      tool: 'Bash',
      workspace: '/repo',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    };
    window.history.replaceState({}, '', toolSessionsPath(filters));
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [], total_conversations: 0 }));
      }
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse()));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 60 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(([url]) => {
          const parsed = new URL(String(url), 'http://localhost');
          return parsed.pathname === '/api/v1/conversations' && parsed.searchParams.get('workspace') === '/repo';
        }),
      ).toBe(true),
    );
    const listRequestCount = fetchMock.mock.calls.filter(([url]) =>
      String(url).startsWith('/api/v1/conversations?'),
    ).length;
    const tokenRequestCount = fetchMock.mock.calls.filter(([url]) =>
      String(url).startsWith('/api/v1/metrics/tokens'),
    ).length;

    fireEvent.click(screen.getByTitle('Filter by workspace'));
    fireEvent.click(screen.getByRole('option', { name: /All workspaces/ }));

    expect(toolSessionFiltersFromLocation()).toEqual({ ...filters, workspace: null });
    expect(screen.getByRole('button', { name: 'Clear tool filter' })).toBeTruthy();
    expect(screen.getByText('Tokens in exact range')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Last 24 hours' })).toBeNull();
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/conversations?')).length,
      ).toBeGreaterThan(listRequestCount),
    );
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/metrics/tokens')).length,
      ).toBeGreaterThan(tokenRequestCount),
    );

    const listRequest = fetchMock.mock.calls
      .map(([url]) => new URL(String(url), 'http://localhost'))
      .filter((url) => url.pathname === '/api/v1/conversations')
      .at(-1);
    expect(listRequest?.searchParams.get('tool')).toBe(filters.tool);
    expect(listRequest?.searchParams.get('since')).toBe(filters.since);
    expect(listRequest?.searchParams.get('before')).toBe(filters.before);
    expect(listRequest?.searchParams.has('workspace')).toBe(false);

    const tokenRequest = fetchMock.mock.calls
      .map(([url]) => new URL(String(url), 'http://localhost'))
      .filter((url) => url.pathname === '/api/v1/metrics/tokens')
      .at(-1);
    expect(tokenRequest?.searchParams.get('since')).toBe(filters.since);
    expect(tokenRequest?.searchParams.get('before')).toBe(filters.before);
    expect(tokenRequest?.searchParams.has('workspace')).toBe(false);
  });

  it('navigates a plain row click to exact Sessions without old facets and preserves modified clicks', async () => {
    const stored = stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/skills-tools?')) return Promise.resolve(response({ tools: DATA }));
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [], total_conversations: 1 }));
      }
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse()));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 60 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await screen.findByRole('heading', { name: 'Sessions' });
    fireEvent.click(screen.getByTitle('Filter by status'));
    fireEvent.click(screen.getByRole('option', { name: 'Has subagents' }));
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.some(([url]) => {
          const parsed = new URL(String(url), 'http://localhost');
          return parsed.pathname === '/api/v1/conversations' && parsed.searchParams.get('subagents') === '1';
        }),
      ).toBe(true),
    );
    fireEvent.click(screen.getByRole('link', { name: 'Analytics' }));
    fireEvent.click(await screen.findByRole('link', { name: 'Tools' }));

    const link = await screen.findByRole('link', { name: 'Bash' });
    const drilldown = new URL(link.getAttribute('href') || '', 'http://localhost');
    const expectedSince = drilldown.searchParams.get('since');
    const expectedBefore = drilldown.searchParams.get('before');
    const expectedWorkspace = drilldown.searchParams.get('workspace');
    link.addEventListener('click', (event) => {
      if (event.ctrlKey) event.preventDefault();
    });
    fireEvent.click(link, { ctrlKey: true });
    expect(window.location.pathname).toBe('/analytics');
    fireEvent.click(link);
    expect(window.location.pathname).toBe('/');
    expect(new URLSearchParams(window.location.search).get('tool')).toBe('Bash');
    expect(stored.get('sigil.local.timeRange')).toBe('24h');
    await waitFor(() => {
      const exactPaths = fetchMock.mock.calls
        .map(([url]) => new URL(String(url), 'http://localhost'))
        .filter((url) => url.searchParams.get('tool') === 'Bash')
        .map((url) => url.pathname);
      expect(exactPaths).toContain('/api/v1/conversations');
      expect(exactPaths).toContain('/api/v1/metrics/conversations');
    });
    const exactRequests = fetchMock.mock.calls
      .map(([url]) => new URL(String(url), 'http://localhost'))
      .filter((url) => url.searchParams.get('tool') === 'Bash');
    for (const request of exactRequests) {
      expect(request.searchParams.has('agent')).toBe(false);
      expect(request.searchParams.has('model')).toBe(false);
      expect(request.searchParams.has('status')).toBe(false);
      expect(request.searchParams.has('subagents')).toBe(false);
    }
    expect(screen.getByText(/Tool/).parentElement?.textContent).toContain('Bash');
    expect(document.body.textContent).toContain(`${expectedSince} to ${expectedBefore}`);
    expect(screen.getByText('Tokens in exact range')).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Last 24 hours' })).toBeNull();
    await waitFor(() =>
      expect(
        fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/metrics/tokens')).length,
      ).toBeGreaterThan(1),
    );
    const tokenRequests = fetchMock.mock.calls
      .map(([url]) => new URL(String(url), 'http://localhost'))
      .filter((url) => url.pathname === '/api/v1/metrics/tokens')
      .map((url) => url.toString());
    const expectedTokenRequest = new URL('/api/v1/metrics/tokens', 'http://localhost');
    expectedTokenRequest.searchParams.set('since', expectedSince || '');
    expectedTokenRequest.searchParams.set('before', expectedBefore || '');
    if (expectedWorkspace != null) expectedTokenRequest.searchParams.set('workspace', expectedWorkspace);
    expectedTokenRequest.searchParams.set('interval', '7200');
    expect(tokenRequests).toContain(expectedTokenRequest.toString());
    fireEvent.click(screen.getByRole('button', { name: 'Clear tool filter' }));
    expect(await screen.findByRole('button', { name: 'Last 24 hours' })).toBeTruthy();
  });
});
