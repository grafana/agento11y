import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import { ESTIMATED_COST_TOOLTIP } from '../internal/local/web/src/formatters';
import type { ConversationSummary, ModelPrices, TokenBuckets } from '../internal/local/web/src/types';
import { aggregate, EMPTY_BUCKETS, metricsResponse, response } from './fixtures';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
});

const PRICES: ModelPrices = {
  'costly-model': { input: 10, output: 20, cache_read: 1, cache_write: 12.5 },
  'a-cheap': { input: 1, output: 1 },
  'z-pricey': { input: 100, output: 100 },
};

// The price catalog is fetched once per module, so the tests seed its cache
// instead of racing the models.dev request.
function stubLocalStorage(entries: [string, string][] = []) {
  const stored = new Map<string, string>([
    ['sigil.modelPrices.v1', JSON.stringify({ at: Date.now(), map: PRICES })],
    ...entries,
  ]);
  vi.stubGlobal('localStorage', {
    getItem: (key: string) => stored.get(key) ?? null,
    setItem: (key: string, value: string) => stored.set(key, value),
    removeItem: (key: string) => stored.delete(key),
  });
  return stored;
}

function conversation(overrides: Partial<ConversationSummary> = {}): ConversationSummary {
  const buckets: TokenBuckets = overrides.token_buckets || { ...EMPTY_BUCKETS, fresh_input: 100_000 };
  return {
    id: 'session-1',
    title: 'One returned row',
    started_at: new Date(Date.now() - 2 * 60 * 60 * 1000).toISOString(),
    last_activity: new Date(Date.now() - 60 * 60 * 1000).toISOString(),
    calls: 4,
    input_tokens: buckets.fresh_input,
    output_tokens: buckets.output,
    total_tokens: Object.values(buckets).reduce((sum, value) => sum + value, 0),
    token_buckets: buckets,
    token_buckets_by_model: { 'costly-model': buckets },
    agents: ['pi'],
    models: ['costly-model'],
    status: 'ok',
    workspace: '/repo',
    ...overrides,
  };
}

function kpiCard(label: string): HTMLElement {
  const card = screen
    .getAllByText(label)
    .map((node) => node.parentElement)
    .find((parent) => parent?.textContent?.startsWith(label) && parent.querySelector('span span'));
  if (!card) throw new Error(`missing ${label} KPI tile`);
  return card as HTMLElement;
}

function lastRequest(fetchMock: ReturnType<typeof vi.fn>, pathname: string): URL | undefined {
  return fetchMock.mock.calls
    .map((call) => new URL(String(call[0]), 'http://localhost'))
    .filter((url) => url.pathname === pathname)
    .at(-1);
}

describe('Sessions headline numbers', () => {
  it('uses the aggregate and the matched count, not the returned page', async () => {
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(
          response(
            metricsResponse({
              matched_conversations: 3441,
              aggregate: aggregate({
                calls: 6882,
                errored: 344,
                agents: 4,
                agent_hosts: ['claude-code', 'pi'],
                workspaces: 37,
                token_buckets: { ...EMPTY_BUCKETS, fresh_input: 200_000, cache_read: 100_000 },
                token_buckets_by_model: {
                  'costly-model': { ...EMPTY_BUCKETS, fresh_input: 200_000 },
                  'other-model': { ...EMPTY_BUCKETS, cache_read: 100_000 },
                },
                models: ['costly-model', 'other-model'],
              }),
            }),
          ),
        );
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [conversation()], total_conversations: 9000 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    await waitFor(() => expect(kpiCard('Sessions').textContent).toContain('3441'));
    expect(kpiCard('Total tokens').textContent).toContain('300k');
    expect(kpiCard('Total tokens').textContent).toContain('2 models');
    expect(kpiCard('Model calls').textContent).toContain('6882');
    expect(kpiCard('Model calls').textContent).toContain('2 avg / session');
    expect(kpiCard('Errored sessions').textContent).toContain('344');
    // 200k fresh input at $10/Mtok.
    expect(kpiCard('Cost').textContent).toContain('$2');
    expect(kpiCard('Cost').textContent).toContain('unpriced usage excluded');
    expect(kpiCard('Cost').textContent).not.toContain('avg / session');
    expect(kpiCard('Input cache hit').textContent).toContain('33%');

    fireEvent.click(screen.getByTitle('Filter by agent'));
    expect(screen.getByRole('option', { name: 'claude-code' })).toBeTruthy();
    fireEvent.click(screen.getByTitle('Filter by agent'));
    fireEvent.click(screen.getByTitle('Filter by model'));
    expect(screen.getByRole('option', { name: 'other-model' })).toBeTruthy();

    const hero = screen.getByRole('heading', { name: 'Sessions' }).parentElement?.parentElement;
    expect(hero?.textContent).toContain('Workspaces37');
    expect(hero?.textContent).toContain('Agents4');

    const sampling = screen.getByText('Session row and total counts differ').parentElement;
    expect(sampling?.textContent).toContain('Rows returned: 1.');
    expect(sampling?.textContent).toContain('Sessions covered by totals: 3441.');
    expect(sampling?.textContent).toContain('lifetime cost, tokens, and duration');
  });

  it('renders no sampling notice when the page holds every match', async () => {
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse({ matched_conversations: 1 })));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [conversation()], total_conversations: 1 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    await waitFor(() => expect(kpiCard('Sessions').textContent).toContain('1'));
    expect(screen.queryByText('Session row and total counts differ')).toBeNull();
    // With zero usage and no model to price, the cost sublabel remains
    // "estimated" instead of reporting excluded usage.
    expect(kpiCard('Cost').textContent).toContain('estimated');
  });

  it('keeps an exact-scoped row at the rolling range boundary', async () => {
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse({ matched_conversations: 1 })));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        const since = new URL(url, 'http://localhost').searchParams.get('since');
        if (!since) throw new Error('missing exact lower bound');
        const edge = conversation({ id: 'session-edge', title: 'Boundary session', last_activity: since });
        // Delay the response so a newly computed rolling lower bound would
        // move past the fixed since value sent with the request.
        return new Promise<Response>((resolve) =>
          setTimeout(() => resolve(response({ conversations: [edge], total_conversations: 1 })), 5),
        );
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    await waitFor(() => expect(document.querySelector('a[href="/conversations/session-edge"]')).toBeTruthy());
    expect(screen.queryByText(/No sessions in/)).toBeNull();
  });

  it('scopes workspace deep links without dropping sibling options', async () => {
    window.history.replaceState({}, '', '/?workspace=%2Frepo');
    stubLocalStorage();
    const sibling = conversation({ id: 'session-2', title: 'Sibling workspace', workspace: '/other' });
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        const scoped = new URL(url, 'http://localhost').searchParams.get('workspace') === '/repo';
        return Promise.resolve(response(metricsResponse({ matched_conversations: scoped ? 12 : 3441 })));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        const scoped = new URL(url, 'http://localhost').searchParams.get('workspace') === '/repo';
        return Promise.resolve(
          response({ conversations: scoped ? [conversation()] : [conversation(), sibling], total_conversations: 9000 }),
        );
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await waitFor(() => expect(kpiCard('Sessions').textContent).toContain('12'));
    expect(lastRequest(fetchMock, '/api/v1/conversations')?.searchParams.get('workspace')).toBe('/repo');
    expect(lastRequest(fetchMock, '/api/v1/metrics/conversations')?.searchParams.get('workspace')).toBe('/repo');
    expect(lastRequest(fetchMock, '/api/v1/metrics/tokens')?.searchParams.get('workspace')).toBe('/repo');

    fireEvent.click(screen.getByTitle('Filter by workspace'));
    expect(screen.getByRole('option', { name: /other/ })).toBeTruthy();
  });

  it('renders no tiles and no substitute numbers when the metrics request fails', async () => {
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve({
          ok: false,
          status: 404,
          json: () => Promise.resolve({}),
          text: () => Promise.resolve('no such endpoint'),
        } as Response);
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [conversation()], total_conversations: 1 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    await screen.findByText('Failed to load session totals');
    expect(screen.getByText('no such endpoint')).toBeTruthy();
    expect(screen.queryByText('Total tokens')).toBeNull();
    expect(screen.queryByText('Input cache hit')).toBeNull();
  });

  it('shows the filter empty state when workspace and status facets match nothing', async () => {
    window.history.replaceState({}, '', '/?workspace=%2Frepo');
    stubLocalStorage();
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        const matched = new URL(url, 'http://localhost').searchParams.has('subagents') ? 92 : 3441;
        return Promise.resolve(
          response(metricsResponse({ matched_conversations: matched, aggregate: aggregate({ calls: matched }) })),
        );
      }
      if (url.startsWith('/api/v1/conversations?')) {
        const facetted = new URL(url, 'http://localhost').searchParams.has('subagents');
        return Promise.resolve(
          response({ conversations: facetted ? [] : [conversation()], total_conversations: 9000 }),
        );
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);
    await waitFor(() => expect(kpiCard('Sessions').textContent).toContain('3441'));

    fireEvent.click(screen.getByTitle('Filter by status'));
    fireEvent.click(screen.getByRole('option', { name: 'Has subagents' }));

    await waitFor(() => expect(kpiCard('Sessions').textContent).toContain('92'));
    expect(lastRequest(fetchMock, '/api/v1/conversations')?.searchParams.get('workspace')).toBe('/repo');
    expect(lastRequest(fetchMock, '/api/v1/conversations')?.searchParams.get('subagents')).toBe('1');
    expect(lastRequest(fetchMock, '/api/v1/metrics/conversations')?.searchParams.get('workspace')).toBe('/repo');
    expect(lastRequest(fetchMock, '/api/v1/metrics/conversations')?.searchParams.get('subagents')).toBe('1');
    expect(screen.getByText('No sessions match the current filters.')).toBeTruthy();
    expect(screen.queryByText('No sessions in this range.')).toBeNull();
  });
});

describe('Sessions per-model cost', () => {
  it('prices a mixed-model session per model in the row and in the workspace totals', async () => {
    stubLocalStorage();
    const empty = { ...EMPTY_BUCKETS };
    const mixed = conversation({
      id: 'mixed',
      models: ['a-cheap', 'z-pricey'],
      token_buckets: { ...empty, fresh_input: 2e6 },
      token_buckets_by_model: {
        'a-cheap': { ...empty, fresh_input: 1e6 },
        'z-pricey': { ...empty, fresh_input: 1e6 },
      },
    });
    const single = conversation({
      id: 'single',
      workspace: '/other',
      models: ['costly-model'],
      token_buckets: { ...empty, fresh_input: 5e6 },
      token_buckets_by_model: { 'costly-model': { ...empty, fresh_input: 5e6 } },
    });
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = String(input);
      if (url.startsWith('/api/v1/metrics/conversations?')) {
        return Promise.resolve(response(metricsResponse({ matched_conversations: 2 })));
      }
      if (url.startsWith('/api/v1/conversations?')) {
        return Promise.resolve(response({ conversations: [mixed, single], total_conversations: 2 }));
      }
      if (url.startsWith('/api/v1/metrics/tokens')) {
        return Promise.resolve(response({ points: [], interval_seconds: 3600 }));
      }
      return Promise.resolve(response({}));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<App />);

    // $101, not the $2 that pricing 2M tokens at the alphabetically first
    // model would give.
    const costCells = await waitFor(() => {
      const cells = screen.getAllByTitle(ESTIMATED_COST_TOOLTIP);
      expect(cells.length).toBeGreaterThan(0);
      return cells;
    });
    expect(costCells.map((cell) => cell.textContent)).toContain('$101');

    // The cost sort key reads the same per-model figure, so the mixed-model
    // session outranks a $50 one it would lose to at models[0] rates.
    fireEvent.click(screen.getByTitle('Sort by cost'));
    await waitFor(() =>
      expect(
        [...document.querySelectorAll('a[href^="/conversations/"]')].map((row) => row.getAttribute('href')),
      ).toEqual(['/conversations/mixed', '/conversations/single']),
    );

    fireEvent.click(screen.getByTitle('Group sessions'));
    fireEvent.click(screen.getByRole('option', { name: 'Workspace' }));
    const groupHeader = await waitFor(() => {
      const header = screen
        .getAllByTitle('/repo')
        .map((node) => node.closest('button[aria-expanded]'))
        .find((node) => node != null);
      expect(header).toBeTruthy();
      return header as HTMLElement;
    });
    expect(groupHeader.textContent).toContain('$101');

    fireEvent.click(screen.getByTitle('Filter by workspace'));
    expect(screen.getByRole('option', { name: /All workspaces/ }).textContent).toContain('$151');
    expect(screen.getByRole('option', { name: /repo/ }).textContent).toContain('$101');
  });
});
