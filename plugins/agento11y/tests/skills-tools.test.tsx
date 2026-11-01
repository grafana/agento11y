import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import * as conversationsModule from '../internal/local/web/src/conversations';
import {
  analyticsPath,
  analyticsTabFromLocation,
  type ExecutionSessionFilters,
  executionSessionFiltersFromLocation,
  executionSessionsPath,
} from '../internal/local/web/src/routing';
import {
  filterToolAnalytics,
  isMcpToolName,
  SkillsToolsView,
  type SkillsToolsViewProps,
} from '../internal/local/web/src/skills-tools';
import type { ConversationSummary, SkillAnalytics, ToolAnalytics } from '../internal/local/web/src/types';
import { EMPTY_BUCKETS, metricsResponse, response } from './fixtures';

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

const SKILLS: SkillAnalytics = {
  ...DATA,
  totals: { calls: 3, failures: 1, skills: 2, sessions: 2, duration_samples: 1 },
  rows: [
    {
      name: 'org:Review',
      calls: 2,
      failures: 1,
      sessions: 2,
      duration_samples: 1,
      p50_duration_seconds: 0,
      p95_duration_seconds: 0,
    },
    { name: 'org:review', calls: 1, failures: 0, sessions: 1, duration_samples: 0 },
  ],
  buckets: [{ t: '2026-08-21T10:00:00Z', name: 'org:Review', calls: 2, failures: 1 }],
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
    expect(screen.getByText('Sessions with tool calls')).toBeTruthy();
    fireEvent.click(screen.getByTitle('Filter by workspace'));
    expect(screen.getByRole('option', { name: /All workspaces with tool calls/ })).toBeTruthy();
    expect(screen.getByText(/2 workspaces with tool calls/)).toBeTruthy();
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

  it('renders one deep-link row for a case-folded tool aggregate', () => {
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            totals: { ...DATA.totals, calls: 130066, tools: 1 },
            rows: DATA.rows.slice(0, 1).map((row) => ({ ...row, calls: 130066 })),
          },
        })}
      />,
    );

    expect(document.querySelectorAll('[data-tool-row]')).toHaveLength(1);
    expect(screen.getByRole('link', { name: 'Bash' }).getAttribute('href')).toContain('tool=Bash');
    const bash = document.querySelector<HTMLTableRowElement>('tr[data-tool-row="Bash"]');
    expect(bash?.cells[3]?.textContent).toBe('130,066');
  });

  it('pins grouped counts when the browser locale is Dutch', () => {
    vi.spyOn(Number.prototype, 'toLocaleString').mockImplementation(function (this: number, locales, options) {
      return new Intl.NumberFormat(locales || 'nl-NL', options).format(Number(this));
    });
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            totals: { ...DATA.totals, calls: 200102 },
            rows: DATA.rows.slice(0, 1).map((row) => ({ ...row, calls: 200102 })),
          },
        })}
      />,
    );

    const bash = document.querySelector<HTMLTableRowElement>('tr[data-tool-row="Bash"]');
    if (!bash) throw new Error('missing Bash row');
    expect(bash.cells[3]?.textContent).toBe('200,102');
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

  it('treats mcp__ and MCP: names as MCP tools', () => {
    expect(isMcpToolName('mcp__grafana__query')).toBe(true);
    expect(isMcpToolName('MCP:browser_cdp')).toBe(true);
    expect(isMcpToolName('mcp:browser_navigate')).toBe(true);
    expect(isMcpToolName('Bash')).toBe(false);
    expect(isMcpToolName('Read')).toBe(false);
  });

  it('keeps unique session totals when filtering to overlapping MCP tools', () => {
    const filtered = filterToolAnalytics(
      {
        ...DATA,
        totals: { calls: 12, failures: 0, tools: 3, sessions: 4, duration_samples: 2 },
        rows: [
          { name: 'mcp__a', calls: 5, failures: 0, sessions: 3, duration_samples: 1 },
          { name: 'mcp__b', calls: 5, failures: 0, sessions: 3, duration_samples: 1 },
          { name: 'Bash', calls: 2, failures: 0, sessions: 2, duration_samples: 0 },
        ],
        buckets: [],
      },
      true,
    );
    expect(filtered?.totals).toEqual({
      calls: 10,
      failures: 0,
      tools: 2,
      sessions: 4,
      duration_samples: 2,
    });
  });

  it('limits the table, chart, and totals to MCP tools', () => {
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            rows: [
              ...DATA.rows,
              {
                name: 'MCP:browser_cdp',
                calls: 5,
                failures: 0,
                sessions: 1,
                duration_samples: 2,
              },
            ],
            buckets: [...DATA.buckets, { t: '2026-08-21T10:00:00Z', name: 'MCP:browser_cdp', calls: 3, failures: 0 }],
          },
        })}
      />,
    );

    fireEvent.click(screen.getByRole('checkbox', { name: 'Only MCP tools' }));

    expect(toolRowNames()).toEqual(['mcp__grafana__query', 'MCP:browser_cdp']);
    expect(screen.queryByRole('link', { name: 'Bash' })).toBeNull();
    expect(screen.queryByRole('link', { name: 'Read' })).toBeNull();
    expect(screen.getByRole('link', { name: /mcp__grafana__query, 1 call/ })).toBeTruthy();
    expect(screen.getByRole('link', { name: /MCP:browser_cdp, 3 calls/ })).toBeTruthy();
    expect(screen.queryByRole('link', { name: /Bash, 2 calls/ })).toBeNull();
    expect(screen.getByText('tool calls').parentElement?.textContent).toContain('15');
    expect(screen.getByText('tools used').parentElement?.textContent).toContain('2');
  });

  it('says when the range has tools but no MCP tools', () => {
    render(
      <SkillsToolsView
        {...props({
          data: {
            ...DATA,
            rows: DATA.rows.filter((row) => row.name === 'Bash'),
            buckets: DATA.buckets.filter((bucket) => bucket.name === 'Bash'),
            totals: { calls: 20, failures: 1, tools: 1, sessions: 3, duration_samples: 20 },
          },
        })}
      />,
    );

    fireEvent.click(screen.getByRole('checkbox', { name: 'Only MCP tools' }));
    expect(screen.getByText('No MCP tools in this range.')).toBeTruthy();
    expect(screen.getByText('No tool calls to chart in this range.')).toBeTruthy();
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

describe('Skills analytics component', () => {
  const skills = SKILLS;

  it('uses skill identity for labels and exact links without applying MCP state', () => {
    render(
      <SkillsToolsView
        {...props({
          mode: 'skills',
          data: skills,
          mcpToolsOnly: true,
          window: { since: '2026-08-21T10:02:00Z', before: '2026-08-21T10:03:00Z' },
        })}
      />,
    );
    expect(screen.getByRole('link', { name: 'Skills' }).getAttribute('aria-current')).toBe('page');
    expect(screen.getByText('Sessions with skill-associated calls')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Skill' })).toBeTruthy();
    expect(screen.getByText('Skill-associated calls over time')).toBeTruthy();
    expect(screen.queryByRole('checkbox', { name: 'Only MCP tools' })).toBeNull();
    expect(screen.getByText(/Counts every recorded tool call marked with a nonblank skill name/)).toBeTruthy();
    expect(screen.getByText(/tool-call durations/i)).toBeTruthy();
    expect(screen.getByText(/Name search filters the table only/)).toBeTruthy();
    const link = screen.getByRole('link', { name: 'org:Review' });
    expect(new URL(link.getAttribute('href') || '', 'http://localhost').searchParams.get('skill')).toBe('org:Review');
    const segment = new URL(
      screen.getByRole('link', { name: /org:Review, 2 calls/ }).getAttribute('href') || '',
      'http://localhost',
    );
    expect(segment.searchParams.get('skill')).toBe('org:Review');
    expect(segment.searchParams.get('since')).toBe('2026-08-21T10:02:00.000Z');
    expect(segment.searchParams.get('before')).toBe('2026-08-21T10:03:00.000Z');
    expect(segment.searchParams.get('workspace')).toBe('/repo');
    const rows = document.querySelectorAll<HTMLTableRowElement>('[data-skill-row]');
    expect(rows).toHaveLength(2);
    expect(rows[0]?.cells[5]?.textContent).toBe('0ms');
    expect(rows[1]?.cells[5]?.textContent).toBe('-');
  });

  it('resets table search and sorting on a mode change while preserving MCP selection', async () => {
    const { rerender } = render(<SkillsToolsView {...props()} />);
    fireEvent.click(screen.getByRole('checkbox', { name: 'Only MCP tools' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Filter tools' }), { target: { value: 'grafana' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tool' }));
    rerender(<SkillsToolsView {...props({ mode: 'skills', data: skills })} />);
    expect(screen.getByRole('textbox', { name: 'Filter skills' }).getAttribute('value')).toBe('');
    expect(screen.getByRole('button', { name: 'Calls' }).closest('th')?.getAttribute('aria-sort')).toBe('descending');
    expect(screen.getByRole('link', { name: 'org:Review' })).toBeTruthy();
    rerender(<SkillsToolsView {...props()} />);
    expect((screen.getByRole('checkbox', { name: 'Only MCP tools' }) as HTMLInputElement).checked).toBe(true);
    expect(screen.getByRole('textbox', { name: 'Filter tools' }).getAttribute('value')).toBe('');
  });

  it('distinguishes loading, request failure, unsupported capability, empty data, and name mismatch', async () => {
    const { rerender } = render(<SkillsToolsView {...props({ mode: 'skills', data: null, loading: true })} />);
    expect(screen.getByText('Loading skill-associated calls…')).toBeTruthy();
    expect(screen.queryByText(/No skill-associated calls were recorded/)).toBeNull();
    rerender(<SkillsToolsView {...props({ mode: 'skills', data: null, error: 'offline' })} />);
    expect(screen.getByText('Failed to load skill analytics')).toBeTruthy();
    rerender(<SkillsToolsView {...props({ mode: 'skills', data: null, unsupported: true })} />);
    expect(screen.getByText('A newer daemon is required.')).toBeTruthy();
    expect(screen.queryByText(/No skill-associated calls were recorded/)).toBeNull();
    rerender(
      <SkillsToolsView
        {...props({
          mode: 'skills',
          data: {
            ...skills,
            rows: [],
            buckets: [],
            totals: { calls: 0, failures: 0, skills: 0, sessions: 0, duration_samples: 0 },
          },
        })}
      />,
    );
    expect(
      screen.getByText('No skill-associated calls were recorded in this range. Try a wider time range.'),
    ).toBeTruthy();
    rerender(<SkillsToolsView {...props({ mode: 'skills', data: skills })} />);
    fireEvent.change(screen.getByRole('textbox', { name: 'Filter skills' }), { target: { value: 'missing' } });
    await screen.findByText('No skills match that filter.');
    expect(screen.getByRole('link', { name: /org:Review, 2 calls/ })).toBeTruthy();
  });
});

describe('Execution analytics integration', () => {
  it('keeps skill row and chart anchors native for modified clicks and intercepts plain clicks', () => {
    const onOpenSessions = vi.fn();
    render(<SkillsToolsView {...props({ mode: 'skills', data: SKILLS, onOpenSessions })} />);
    for (const link of [
      screen.getByRole('link', { name: 'org:Review' }),
      screen.getByRole('link', { name: /org:Review, 2 calls/ }),
    ]) {
      link.addEventListener('click', (event) => event.preventDefault());
      for (const modifiers of [
        { ctrlKey: true },
        { metaKey: true },
        { shiftKey: true },
        { altKey: true },
        { button: 1 },
      ]) {
        fireEvent.click(link, modifiers);
      }
      expect(onOpenSessions).not.toHaveBeenCalled();
      fireEvent.click(link);
      expect(onOpenSessions).toHaveBeenCalledWith(
        expect.objectContaining({ kind: 'skill', name: 'org:Review', workspace: '/repo' }),
      );
      onOpenSessions.mockClear();
    }
  });

  it.each(['tool', 'skill'] as const)('round-trips exact %s identities, blank names, and workspace scope', (kind) => {
    for (const name of ['', 'org:Review', 'org:review', ' /path/to/SKILL.md + 雪 & ? ']) {
      const filters: ExecutionSessionFilters = {
        kind,
        name,
        workspace: '',
        since: '2026-08-21T10:02:00Z',
        before: '2026-08-21T10:03:00Z',
      };
      window.history.replaceState({}, '', executionSessionsPath(filters));
      expect(executionSessionFiltersFromLocation()).toEqual(filters);
      expect(new URLSearchParams(window.location.search).has(kind === 'skill' ? 'tool' : 'skill')).toBe(false);
    }
    window.history.replaceState({}, '', '/conversations/detail?skill=Review');
    expect(executionSessionFiltersFromLocation()).toBeNull();
  });

  it('shares one combined request across Tools and Skills, retaining MCP selection but resetting table state', async () => {
    window.history.replaceState({}, '', analyticsPath('skills'));
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/skills-tools?'))
        return Promise.resolve(response({ tools: DATA, skills: SKILLS }));
      if (url.startsWith('/api/v1/conversations?'))
        return Promise.resolve(response({ conversations: [], total_conversations: 0 }));
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await screen.findByRole('link', { name: 'org:Review' });
    expect(screen.getByText('skills recorded').parentElement?.textContent).toContain('2');
    fireEvent.click(screen.getByRole('link', { name: 'Tools' }));
    await screen.findByRole('link', { name: 'Bash' });
    fireEvent.click(screen.getByRole('checkbox', { name: 'Only MCP tools' }));
    fireEvent.change(screen.getByRole('textbox', { name: 'Filter tools' }), { target: { value: 'grafana' } });
    fireEvent.click(screen.getByRole('button', { name: 'Tool' }));
    fireEvent.click(screen.getByRole('link', { name: 'Skills' }));
    expect(screen.getByRole('link', { name: 'org:Review' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Calls' }).closest('th')?.getAttribute('aria-sort')).toBe('descending');
    expect((screen.getByRole('textbox', { name: 'Filter skills' }) as HTMLInputElement).value).toBe('');
    fireEvent.click(screen.getByRole('link', { name: 'Tools' }));
    expect((screen.getByRole('checkbox', { name: 'Only MCP tools' }) as HTMLInputElement).checked).toBe(true);
    expect(screen.queryByRole('link', { name: 'Bash' })).toBeNull();
    expect(screen.getByRole('link', { name: 'mcp__grafana__query' })).toBeTruthy();
    expect(
      fetchMock.mock.calls.filter(([url]) => String(url).startsWith('/api/v1/metrics/skills-tools?')),
    ).toHaveLength(1);
  });

  it('renders sparse All-range Skills through the requested upper bound with one combined request', async () => {
    const before = '2026-09-21T10:00:00.000Z';
    vi.spyOn(Date, 'now').mockReturnValue(Date.parse(before));
    window.history.replaceState({}, '', analyticsPath('tools'));
    stubLocalStorage([['sigil.local.analyticsRange', 'all']]);
    const row = { name: 'Read', calls: 2, failures: 0, sessions: 0, duration_samples: 0 };
    const tools: ToolAnalytics = {
      totals: { calls: 2, failures: 0, tools: 1, sessions: 0, duration_samples: 0 },
      rows: [row],
      buckets: [
        { t: '2026-08-21T10:00:00Z', name: 'Read', calls: 1, failures: 0 },
        { t: '2026-09-21T08:00:00Z', name: 'Read', calls: 1, failures: 0 },
      ],
      workspaces: [],
      interval_seconds: 7200,
      coverage: { generation_calls: 0, projected_spans: 2, matched_calls: 0 },
    };
    const skills: SkillAnalytics = {
      ...tools,
      totals: { calls: 1, failures: 0, skills: 1, sessions: 0, duration_samples: 0 },
      rows: [{ ...row, name: 'org:Review', calls: 1 }],
      buckets: [{ t: '2026-08-21T10:00:00Z', name: 'org:Review', calls: 1, failures: 0 }],
      coverage: { generation_calls: 0, projected_spans: 1, matched_calls: 0 },
    };
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      if (String(input).startsWith('/api/v1/metrics/skills-tools?'))
        return Promise.resolve(response({ tools, skills }));
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await screen.findByRole('link', { name: 'Read' });
    expect(document.querySelectorAll('.tools-chart-column')).toHaveLength(372);
    fireEvent.click(screen.getByRole('link', { name: 'Skills' }));
    expect(screen.getByRole('link', { name: 'org:Review' })).toBeTruthy();
    expect(screen.getByRole('heading', { name: 'Analytics' })).toBeTruthy();
    const columns = document.querySelectorAll('.tools-chart-column');
    expect(columns).toHaveLength(372);
    expect([...columns].filter((column) => column.querySelector('a') == null)).toHaveLength(371);
    const segment = new URL(
      screen.getByRole('link', { name: /org:Review, 1 call,/ }).getAttribute('href') || '',
      'http://local',
    );
    expect(segment.searchParams.get('since')).toBe('2026-08-21T10:00:00.000Z');
    expect(segment.searchParams.get('before')).toBe('2026-08-21T12:00:00.000Z');
    const requests = fetchMock.mock.calls
      .map(([url]) => new URL(String(url), 'http://local'))
      .filter((url) => url.pathname === '/api/v1/metrics/skills-tools');
    expect(requests).toHaveLength(1);
    expect(requests[0]?.searchParams.get('before')).toBe(before);
    expect(requests[0]?.searchParams.has('since')).toBe(false);
    expect(requests[0]?.searchParams.has('interval')).toBe(false);
  });

  it('shows unsupported Skills for a tools-only daemon without hiding Tools', async () => {
    window.history.replaceState({}, '', analyticsPath('skills'));
    stubLocalStorage();
    vi.stubGlobal(
      'fetch',
      vi.fn((input: RequestInfo | URL) => {
        if (String(input).startsWith('/api/v1/metrics/skills-tools?'))
          return Promise.resolve(response({ tools: DATA }));
        return Promise.resolve(response({}));
      }),
    );
    render(<App />);
    await screen.findByText('A newer daemon is required.');
    expect(screen.queryByText(/No skill-associated calls were recorded/)).toBeNull();
    fireEvent.click(screen.getByRole('link', { name: 'Tools' }));
    expect(await screen.findByRole('link', { name: 'Bash' })).toBeTruthy();
  });

  it.each([
    'tool',
    'skill',
  ] as const)('does not re-filter exact %s results with a stale client bucket', async (kind) => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(response({}))),
    );
    render(
      <conversationsModule.ConversationsView
        conversations={[
          {
            id: 'outside-bucket',
            title: 'Lifetime outside the bucket',
            started_at: '2020-01-01T00:00:00Z',
            last_activity: '2020-01-02T00:00:00Z',
            calls: 1,
            input_tokens: 0,
            output_tokens: 0,
            total_tokens: 0,
            token_buckets: EMPTY_BUCKETS,
            agents: [],
            models: [],
            status: 'ok',
            workspace: '/repo',
          },
        ]}
        workspaceFacetConversations={[]}
        storeCount={1}
        aggregate={null}
        matchedConversations={1}
        metricsLoading={false}
        metricsError={null}
        tokenPoints={[]}
        tokenIntervalMs={60000}
        loading={false}
        error={null}
        query=""
        setQuery={vi.fn()}
        timeRange="24h"
        setTimeRange={vi.fn()}
        tokenModel="all"
        setTokenModel={vi.fn()}
        chartMetric="activity"
        setChartMetric={vi.fn()}
        bucketSel={{ start: Date.parse('2026-08-21T10:00:00Z'), end: Date.parse('2026-08-21T11:00:00Z') }}
        setBucketSel={vi.fn()}
        workspace="/repo"
        setWorkspace={vi.fn()}
        groupBy="none"
        setGroupBy={vi.fn()}
        agentFilter="all"
        setAgentFilter={vi.fn()}
        modelFilter="all"
        setModelFilter={vi.fn()}
        statusFilter="all"
        setStatusFilter={vi.fn()}
        listSort={{ key: 'last_activity', dir: 'desc' }}
        setListSort={vi.fn()}
        onOpen={vi.fn()}
        onRefresh={vi.fn()}
        refreshing={false}
        onOpenSettings={vi.fn()}
        executionSessionFilters={{
          kind,
          name: 'org:Review',
          workspace: '/repo',
          since: '2026-08-21T10:02:00Z',
          before: '2026-08-21T10:03:00Z',
        }}
        onClearExecutionFilter={vi.fn()}
      />,
    );
    expect(screen.getByRole('link', { name: /Lifetime outside the bucket/ })).toBeTruthy();
    expect(screen.queryByText('No sessions in the selected bucket.')).toBeNull();
    expect(screen.queryByText(/^Showing .* to/)).toBeNull();
    expect(screen.getByText('Tokens in exact range')).toBeTruthy();
    expect(screen.queryByRole('button', { name: /Filter to this time bucket/ })).toBeNull();
    await act(async () => {});
  });

  it.each([
    'tool',
    'skill',
  ] as const)('restores exact %s session scope through history, detail, and workspace changes', async (kind) => {
    window.history.replaceState({}, '', '/');
    stubLocalStorage([['sigil.local.groupBy', 'none']]);
    const listed: ConversationSummary = {
      id: 'lifetime-session',
      title: 'Lifetime summary',
      started_at: '2020-01-01T00:00:00Z',
      last_activity: '2030-01-01T00:00:00Z',
      calls: 1,
      input_tokens: 0,
      output_tokens: 0,
      total_tokens: 0,
      token_buckets: EMPTY_BUCKETS,
      agents: [],
      models: [],
      status: 'ok',
      workspace: '/repo',
    };
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      const params = new URL(url, 'http://local').searchParams;
      const echo = params.has('skill') ? { applied_skill: params.get('skill') } : {};
      if (url.startsWith('/api/v1/conversations?'))
        return Promise.resolve(response({ conversations: [listed], total_conversations: 1, ...echo }));
      if (url.startsWith('/api/v1/metrics/conversations?'))
        return Promise.resolve(response({ ...metricsResponse({ matched_conversations: 1 }), ...echo }));
      if (url === '/api/v1/conversations/lifetime-session')
        return Promise.resolve(response({ id: listed.id, generations: [] }));
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);
    render(<App />);
    await screen.findByText('Lifetime summary');
    fireEvent.click(screen.getByTitle('Filter by status'));
    fireEvent.click(screen.getByRole('option', { name: 'Has subagents' }));
    fireEvent.change(screen.getByPlaceholderText('Search prompts, responses, tool output, title, agent, model…'), {
      target: { value: 'stale search' },
    });
    const filters: ExecutionSessionFilters = {
      kind,
      name: 'org:Review',
      workspace: '/repo',
      since: '2026-08-21T10:02:00Z',
      before: '2026-08-21T10:03:00Z',
    };
    const path = executionSessionsPath(filters);
    act(() => {
      window.history.pushState({}, '', path);
      window.dispatchEvent(new PopStateEvent('popstate'));
    });
    expect(
      (screen.getByPlaceholderText('Search prompts, responses, tool output, title, agent, model…') as HTMLInputElement)
        .value,
    ).toBe('');
    expect(screen.getByRole('button', { name: `Clear ${kind} filter` })).toBeTruthy();
    expect(screen.getByText(/Summary tiles show in-period generation usage/)).toBeTruthy();
    expect(screen.getByText(/not usage attributed to the selected/)).toBeTruthy();
    const assertRequests = async (workspace: string | null) => {
      await waitFor(() => {
        for (const endpoint of ['/api/v1/conversations', '/api/v1/metrics/conversations', '/api/v1/metrics/tokens']) {
          const request = fetchMock.mock.calls
            .map(([url]) => new URL(String(url), 'http://local'))
            .filter((url) => url.pathname === endpoint)
            .at(-1);
          expect(request?.searchParams.get('workspace')).toBe(workspace);
          expect(request?.searchParams.get('since')).toBe(filters.since);
          expect(request?.searchParams.get('before')).toBe(filters.before);
          if (endpoint !== '/api/v1/metrics/tokens') expect(request?.searchParams.get(kind)).toBe(filters.name);
          else expect(request?.searchParams.has(kind)).toBe(false);
          for (const facet of ['agent', 'model', 'status', 'subagents'])
            expect(request?.searchParams.has(facet)).toBe(false);
        }
      });
    };
    await assertRequests('/repo');
    fireEvent.click(await screen.findByRole('link', { name: /Lifetime summary/ }));
    const back = await screen.findByRole('link', { name: 'Back to Sessions' });
    expect(back.getAttribute('href')).toBe(path);
    fireEvent.click(back);
    expect(executionSessionFiltersFromLocation()).toEqual(filters);
    expect(await screen.findByRole('link', { name: /Lifetime summary/ })).toBeTruthy();
    fireEvent.click(screen.getByTitle('Filter by workspace'));
    fireEvent.click(screen.getByRole('option', { name: /All workspaces/ }));
    expect(executionSessionFiltersFromLocation()).toEqual({ ...filters, workspace: null });
    await assertRequests(null);
  });
});

const SKILL_SESSION: ConversationSummary = {
  id: 'skill-session',
  title: 'Confirmed skill session',
  started_at: '2026-08-21T10:00:00Z',
  last_activity: '2026-08-21T10:30:00Z',
  calls: 1,
  input_tokens: 0,
  output_tokens: 0,
  total_tokens: 0,
  token_buckets: EMPTY_BUCKETS,
  agents: [],
  models: [],
  status: 'ok',
  workspace: '/repo',
};
const SESSION_LIST = { conversations: [SKILL_SESSION], total_conversations: 1 };
const SESSION_METRICS = metricsResponse({
  aggregate: { ...metricsResponse().aggregate, calls: 98765 },
  matched_conversations: 1,
});

function deferredSessions(filters: Partial<ExecutionSessionFilters> = {}) {
  window.history.replaceState(
    {},
    '',
    executionSessionsPath({ kind: 'skill', name: 'org:Review', workspace: null, ...filters }),
  );
  stubLocalStorage([['sigil.local.groupBy', 'none']]);
  const pending: Array<{
    url: URL;
    resolve: (value: Response) => void;
    reject: (reason: Error) => void;
  }> = [];
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://local');
      if (url.pathname === '/api/v1/conversations' || url.pathname === '/api/v1/metrics/conversations')
        return new Promise<Response>((resolve, reject) => pending.push({ url, resolve, reject }));
      return Promise.resolve(response({}));
    }),
  );
  render(<App />);
  return pending;
}

function expectNoSkillData() {
  expect(screen.queryByRole('link', { name: /Confirmed skill session/ })).toBeNull();
  expect(screen.queryByText('98765')).toBeNull();
  expect(screen.queryByText(/No sessions match this exact/)).toBeNull();
}

async function confirmRequests(
  pending: ReturnType<typeof deferredSessions>,
  echo: Record<string, unknown> = { applied_skill: 'org:Review' },
) {
  await act(async () => {
    for (const request of pending) {
      request.resolve(
        response({
          ...(request.url.pathname === '/api/v1/conversations' ? SESSION_LIST : SESSION_METRICS),
          ...echo,
        }),
      );
    }
  });
}

function navigateSkill(filters: Partial<ExecutionSessionFilters>) {
  act(() => {
    window.history.pushState(
      {},
      '',
      executionSessionsPath({ kind: 'skill', name: 'org:Review', workspace: null, ...filters }),
    );
    window.dispatchEvent(new PopStateEvent('popstate'));
  });
}

describe('Skill Sessions daemon confirmations', () => {
  const invalidEchoes = [
    ['missing', {}],
    ['null', { applied_skill: null }],
    ['wrong case', { applied_skill: 'org:review' }],
    ['wrong namespace', { applied_skill: 'Review' }],
    ['wrong whitespace', { applied_skill: ' org:Review ' }],
  ] as const;

  it.each(
    invalidEchoes.flatMap(([label, echo]) =>
      ['list', 'metrics', 'both'].map((endpoint) => ({ label, echo, endpoint })),
    ),
  )('rejects $label confirmation from $endpoint', async ({ echo, endpoint }) => {
    const pending = deferredSessions();
    await act(async () => {
      for (const request of pending) {
        const list = request.url.pathname === '/api/v1/conversations';
        const invalid = endpoint === 'both' || endpoint === (list ? 'list' : 'metrics');
        request.resolve(
          response({
            ...(list ? SESSION_LIST : SESSION_METRICS),
            ...(invalid ? echo : { applied_skill: 'org:Review' }),
          }),
        );
      }
    });
    expect(screen.getByText('A newer daemon is required.')).toBeTruthy();
    expectNoSkillData();
    expect(screen.queryByText(/Failed to load session/)).toBeNull();
  });

  it.each(['', ' /path/to/SKILL.md + 雪 & ? ', 'org:Review'])('accepts only the exact raw echo %j', async (name) => {
    const pending = deferredSessions({ name });
    for (const request of pending) {
      expect(request.url.searchParams.has('skill')).toBe(true);
      expect(request.url.searchParams.get('skill')).toBe(name);
    }
    await confirmRequests(pending, { applied_skill: name });
    expect(screen.getByRole('link', { name: /Confirmed skill session/ })).toBeTruthy();
    expect(screen.getByText('98765')).toBeTruthy();
    expect(screen.queryByText('A newer daemon is required.')).toBeNull();
  });

  it.each([
    {},
    { applied_skill: null },
    { applied_skill: ' ' },
  ])('does not treat blank skill as absent: %j', async (echo) => {
    const pending = deferredSessions({ name: '' });
    await confirmRequests(pending, echo);
    expect(screen.getByText('A newer daemon is required.')).toBeTruthy();
    expectNoSkillData();
  });

  it.each(['list', 'metrics'])('waits for both endpoints when only %s confirms', async (first) => {
    const pending = deferredSessions();
    const list = pending.filter((request) => request.url.pathname === '/api/v1/conversations');
    const metrics = pending.filter((request) => request.url.pathname === '/api/v1/metrics/conversations');
    expectNoSkillData();
    await confirmRequests(first === 'list' ? list : metrics);
    expectNoSkillData();
    await confirmRequests(first === 'list' ? metrics : list);
    expect(screen.getByRole('link', { name: /Confirmed skill session/ })).toBeTruthy();
    expect(screen.getByText('98765')).toBeTruthy();
  });

  it.each(['list', 'metrics'])('settles genuine %s failures as errors and recovers on refresh', async (endpoint) => {
    const original = conversationsModule.ConversationsView;
    const renders: Array<Parameters<typeof original>[0]> = [];
    vi.spyOn(conversationsModule, 'ConversationsView').mockImplementation((props) => {
      renders.push(props);
      return original(props);
    });
    const pending = deferredSessions();
    await act(async () => {
      for (const request of pending) {
        const list = request.url.pathname === '/api/v1/conversations';
        if (endpoint === (list ? 'list' : 'metrics')) request.reject(new Error('offline'));
        else request.resolve(response({ ...(list ? SESSION_LIST : SESSION_METRICS), applied_skill: 'org:Review' }));
      }
    });
    expect(screen.getByText('offline')).toBeTruthy();
    expect(screen.queryByText('A newer daemon is required.')).toBeNull();
    expectNoSkillData();
    expect(renders.at(-1)?.refreshing).toBe(false);
    expect(renders.at(-1)?.loading).toBe(false);
    expect(renders.at(-1)?.metricsLoading).toBe(false);
    expect(screen.queryByText('Loading…')).toBeNull();
    expect(screen.queryByText('Total tokens')).toBeNull();

    const start = pending.length;
    fireEvent.click(screen.getByTitle('Refresh'));
    expectNoSkillData();
    const refresh = pending.slice(start);
    expect(refresh).toHaveLength(2);
    const retry = refresh.filter(
      (request) =>
        request.url.pathname === (endpoint === 'list' ? '/api/v1/conversations' : '/api/v1/metrics/conversations'),
    );
    await confirmRequests(refresh.filter((request) => !retry.includes(request)));
    expectNoSkillData();
    await confirmRequests(retry);
    expect(screen.getByRole('link', { name: /Confirmed skill session/ })).toBeTruthy();
    expect(screen.getByText('98765')).toBeTruthy();
    expect(screen.queryByText('offline')).toBeNull();
    expect(screen.queryByText('Loading…')).toBeNull();
  });

  it('hides prior-scope data on the first render before reset effects run', async () => {
    const original = conversationsModule.ConversationsView;
    const renders: Array<Parameters<typeof original>[0]> = [];
    vi.spyOn(conversationsModule, 'ConversationsView').mockImplementation((props) => {
      renders.push(props);
      return original(props);
    });
    const pending = deferredSessions();
    await confirmRequests(pending);
    for (const filters of [
      { name: 'org:review' },
      { workspace: '/repo' },
      { workspace: '/repo', since: '2026-08-21T10:02:00Z', before: '2026-08-21T10:03:00Z' },
    ]) {
      const start = pending.length;
      renders.length = 0;
      navigateSkill(filters);
      const first = renders[0];
      expect(first?.conversations).toEqual([]);
      expect(first?.workspaceFacetConversations).toEqual([]);
      expect(first?.aggregate).toBeNull();
      expect(first?.matchedConversations).toBeNull();
      expect(first?.loading).toBe(true);
      expect(first?.metricsLoading).toBe(true);
      await confirmRequests(pending.slice(start), { applied_skill: filters.name ?? 'org:Review' });
      expect(screen.getByText('98765')).toBeTruthy();
    }
    const start = pending.length;
    renders.length = 0;
    fireEvent.click(screen.getByTitle('Filter by status'));
    fireEvent.click(screen.getByRole('option', { name: 'Has subagents' }));
    expect(renders[0]?.conversations).toEqual([]);
    expect(renders[0]?.workspaceFacetConversations).toEqual([]);
    expect(renders[0]?.aggregate).toBeNull();
    expect(renders[0]?.matchedConversations).toBeNull();
    await confirmRequests(pending.slice(start));
    renders.length = 0;
    const toolStart = pending.length;
    navigateSkill({ kind: 'tool', name: 'Bash' });
    expect(renders[0]?.conversations).toEqual([]);
    expect(renders[0]?.workspaceFacetConversations).toEqual([]);
    expect(renders[0]?.aggregate).toBeNull();
    expect(renders[0]?.matchedConversations).toBeNull();
    await confirmRequests(pending.slice(toolStart), {});
    expect(screen.getByText('98765')).toBeTruthy();
    renders.length = 0;
    fireEvent.click(screen.getByRole('button', { name: 'Clear tool filter' }));
    expect(renders[0]?.conversations).toEqual([]);
    expect(renders[0]?.workspaceFacetConversations).toEqual([]);
    expect(renders[0]?.aggregate).toBeNull();
    expect(renders[0]?.matchedConversations).toBeNull();
  });

  it('hides old confirmations across filter, workspace, time, and facet scopes and ignores stale responses', async () => {
    const pending = deferredSessions();
    await confirmRequests(pending);
    expect(screen.getByText('98765')).toBeTruthy();
    let start = pending.length;
    navigateSkill({ name: 'org:review' });
    expectNoSkillData();
    const obsolete = pending.slice(start);
    start = pending.length;
    navigateSkill({ workspace: '/repo', since: '2026-08-21T10:02:00Z', before: '2026-08-21T10:03:00Z' });
    expectNoSkillData();
    const current = pending.slice(start);
    await confirmRequests(current);
    expect(screen.getByText('98765')).toBeTruthy();
    await confirmRequests(obsolete, { applied_skill: null });
    expect(screen.queryByText('A newer daemon is required.')).toBeNull();
    expect(screen.getByText('98765')).toBeTruthy();
    start = pending.length;
    fireEvent.click(screen.getByTitle('Filter by status'));
    fireEvent.click(screen.getByRole('option', { name: 'Has subagents' }));
    expectNoSkillData();
    await confirmRequests(pending.slice(start));
    expect(screen.getByText('98765')).toBeTruthy();
    start = pending.length;
    fireEvent.click(screen.getByTitle('Filter by workspace'));
    fireEvent.click(screen.getByRole('option', { name: /All workspaces/ }));
    expectNoSkillData();
    await confirmRequests(pending.slice(start));
    expect(screen.getByText('98765')).toBeTruthy();
  });

  it.each([
    {},
    { applied_skill: null },
    { applied_skill: 'org:review' },
  ])('ignores unconfirmed workspace facet rows: %j', async (echo) => {
    const pending = deferredSessions({ workspace: '/repo' });
    await confirmRequests(pending.filter((request) => request.url.searchParams.has('workspace')));
    const facet = pending.filter((request) => !request.url.searchParams.has('workspace'));
    await act(async () => {
      for (const request of facet)
        request.resolve(
          response({
            conversations: [{ ...SKILL_SESSION, workspace: '/unconfirmed-secret' }],
            total_conversations: 1,
            ...echo,
          }),
        );
    });
    expect(screen.getByText('98765')).toBeTruthy();
    fireEvent.click(screen.getByTitle('Filter by workspace'));
    expect(screen.queryByRole('option', { name: /unconfirmed-secret/ })).toBeNull();
    expect(document.body.textContent).not.toContain('/unconfirmed-secret');
  });

  it('waits for both main confirmations before exposing even confirmed workspace facets', async () => {
    const pending = deferredSessions({ workspace: '/repo' });
    const facet = pending.find((request) => !request.url.searchParams.has('workspace'));
    if (!facet) throw new Error('missing facet request');
    await act(async () =>
      facet.resolve(
        response({
          conversations: [{ ...SKILL_SESSION, workspace: '/confirmed-facet' }],
          total_conversations: 1,
          applied_skill: 'org:Review',
        }),
      ),
    );
    fireEvent.click(screen.getByTitle('Filter by workspace'));
    expect(screen.queryByRole('option', { name: /confirmed-facet/ })).toBeNull();
    await confirmRequests(
      pending.filter((request) => request.url.pathname === '/api/v1/conversations' && request !== facet),
    );
    expect(screen.queryByRole('option', { name: /confirmed-facet/ })).toBeNull();
    expectNoSkillData();
    await confirmRequests(pending.filter((request) => request.url.pathname === '/api/v1/metrics/conversations'));
    expect(screen.getByRole('option', { name: /confirmed-facet/ })).toBeTruthy();
    expect(screen.getByText('98765')).toBeTruthy();
  });

  it.each(['list', 'metrics'])('a bad %s refresh hides previous rows and KPIs immediately', async (endpoint) => {
    const pending = deferredSessions();
    await confirmRequests(pending);
    expect(screen.getByText('98765')).toBeTruthy();
    const start = pending.length;
    fireEvent.click(screen.getByTitle('Refresh'));
    expect(screen.getByText('98765')).toBeTruthy();
    expect(screen.getByRole('link', { name: /Confirmed skill session/ })).toBeTruthy();
    const refresh = pending.slice(start);
    const bad = refresh.filter(
      (request) =>
        request.url.pathname === (endpoint === 'list' ? '/api/v1/conversations' : '/api/v1/metrics/conversations'),
    );
    await confirmRequests(bad, {});
    expect(screen.getByText('A newer daemon is required.')).toBeTruthy();
    expectNoSkillData();
    await confirmRequests(refresh.filter((request) => !bad.includes(request)));
    expectNoSkillData();
  });

  it('accepts confirmed empty results as no matches, not an unsupported daemon', async () => {
    const pending = deferredSessions();
    await act(async () => {
      for (const request of pending)
        request.resolve(
          response({
            ...(request.url.pathname === '/api/v1/conversations'
              ? { conversations: [], total_conversations: 0 }
              : metricsResponse()),
            applied_skill: 'org:Review',
          }),
        );
    });
    expect(
      screen.getByText('No sessions match this exact skill, workspace, time range, and active session filters.'),
    ).toBeTruthy();
    expect(screen.queryByText('A newer daemon is required.')).toBeNull();
  });

  it('keeps old-daemon exact Tool Sessions usable without any echoes', async () => {
    const pending = deferredSessions({ kind: 'tool', name: 'Bash' });
    await confirmRequests(pending, {});
    expect(screen.getByRole('link', { name: /Confirmed skill session/ })).toBeTruthy();
    expect(screen.getByText('98765')).toBeTruthy();
    expect(screen.queryByText('A newer daemon is required.')).toBeNull();
  });
});

describe('Tools analytics routing and App fetching', () => {
  it('parses final Analytics and exact Sessions URLs', () => {
    expect(analyticsPath('tools')).toBe('/analytics?tab=skills&mode=tools');
    expect(analyticsPath('skills')).toBe('/analytics?tab=skills&mode=skills');
    for (const [path, tab] of [
      ['/analytics', 'overview'],
      ['/analytics?tab=skills', 'tools'],
      ['/analytics?tab=skills&mode=tools', 'tools'],
      ['/analytics?tab=skills&mode=skills', 'skills'],
    ]) {
      window.history.replaceState({}, '', path);
      expect(analyticsTabFromLocation()).toBe(tab);
    }

    const path = executionSessionsPath({
      kind: 'tool',
      name: 'mcp__grafana__query',
      workspace: '',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    });
    window.history.replaceState({}, '', path);
    expect(executionSessionFiltersFromLocation()).toEqual({
      kind: 'tool',
      name: 'mcp__grafana__query',
      workspace: '',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    });
  });

  it('opens Tools directly with one tools request and keeps the saved Overview unit', async () => {
    window.history.replaceState({}, '', analyticsPath('tools'));
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

  it.each([
    'tools',
    'skills',
  ] as const)('ignores an older combined response after the %s range changes', async (mode) => {
    window.history.replaceState({}, '', analyticsPath(mode));
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

    act(() => pending[1]?.(response({ tools: DATA, skills: SKILLS })));
    const currentName = mode === 'skills' ? 'org:Review' : 'Bash';
    await screen.findByRole('link', { name: currentName });
    const staleRows = [{ name: 'stale', calls: 1, failures: 0, sessions: 1, duration_samples: 0 }];
    act(() =>
      pending[0]?.(
        response({
          tools: { ...DATA, rows: staleRows },
          skills: { ...SKILLS, rows: staleRows },
        }),
      ),
    );
    await waitFor(() => expect(screen.queryByRole('link', { name: 'stale' })).toBeNull());
    expect(screen.getByRole('link', { name: currentName })).toBeTruthy();
  });

  it('preserves exact tool and time filters when the workspace changes', async () => {
    const filters: ExecutionSessionFilters = {
      kind: 'tool',
      name: 'Bash',
      workspace: '/repo',
      since: '2026-08-21T10:00:00Z',
      before: '2026-08-21T11:00:00Z',
    };
    window.history.replaceState({}, '', executionSessionsPath(filters));
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

    expect(executionSessionFiltersFromLocation()).toEqual({ ...filters, workspace: null });
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
    expect(listRequest?.searchParams.get('tool')).toBe(filters.name);
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
