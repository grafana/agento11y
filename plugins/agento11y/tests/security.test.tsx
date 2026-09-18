import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import { securityRouteActive } from '../internal/local/web/src/routing';
import { metricsResponse } from './fixtures';

function installFetch(responses: Record<string, () => Promise<Response>> = {}) {
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = String(input);
    if (responses[url]) return responses[url]();
    let body: unknown = {};
    if (url === '/api/v1/config')
      body = { settings: { theme: 'dark', guards: 'off', tags: [] }, preview: '', path: '/tmp/config.env' };
    if (url === '/api/v1/guards') body = { enabled: false, packs: [], rules: [] };
    if (url.startsWith('/api/v1/conversations?')) body = { conversations: [], total_conversations: 0 };
    if (url.startsWith('/api/v1/metrics/conversations?')) body = metricsResponse();
    if (url.startsWith('/api/v1/metrics/tokens')) body = { interval_seconds: 10, points: [] };
    if (url === '/api/v1/history/agents') body = { agents: [] };
    if (url === '/api/v1/history/offer') body = { offers: [] };
    return Promise.resolve(new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } }));
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('EventSource', undefined);
  return fetchMock;
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
});

describe('Security page', () => {
  it.each([
    { path: '/security', title: 'Loading security…' },
    { path: '/settings', title: 'Loading settings…' },
  ])('shows section-specific config loading at $path', async ({ path, title }) => {
    installFetch({ '/api/v1/config': () => new Promise<Response>(() => {}) });
    window.history.replaceState({}, '', path);
    render(<App />);
    expect(await screen.findByText(title)).toBeTruthy();
    expect(screen.queryByRole('switch', { name: 'Enable guards' })).toBeNull();
  });

  it.each([
    { path: '/security', title: 'Failed to load security settings' },
    { path: '/settings', title: 'Failed to load settings' },
  ])('shows section-specific config errors at $path', async ({ path, title }) => {
    installFetch({ '/api/v1/config': () => Promise.resolve(new Response('config unavailable', { status: 500 })) });
    window.history.replaceState({}, '', path);
    render(<App />);
    expect(await screen.findByText(title)).toBeTruthy();
    expect(screen.getByText('config unavailable')).toBeTruthy();
    expect(screen.queryByRole('switch', { name: 'Enable guards' })).toBeNull();
  });

  it('shows pack loading separately from config loading', async () => {
    installFetch({ '/api/v1/guards': () => new Promise<Response>(() => {}) });
    window.history.replaceState({}, '', '/security');
    render(<App />);
    expect(await screen.findByText('Loading guard packs…')).toBeTruthy();
    expect(screen.queryByText('Loading security…')).toBeNull();
    expect(screen.getByRole('switch', { name: 'Enable guards' }).hasAttribute('disabled')).toBe(true);
  });

  it('keeps guard load errors on Security', async () => {
    installFetch({ '/api/v1/guards': () => Promise.resolve(new Response('cannot read guards.toml', { status: 500 })) });
    window.history.replaceState({}, '', '/security');
    render(<App />);
    expect(await screen.findByText('cannot read guards.toml')).toBeTruthy();
    expect(screen.getByRole('heading', { name: 'Security' })).toBeTruthy();
    expect(screen.queryByText('Loading guard packs…')).toBeNull();
    expect(screen.getByRole('switch', { name: 'Enable guards' }).hasAttribute('disabled')).toBe(true);
  });

  it('preserves the rules path, setup prompt, and compilation warnings', async () => {
    installFetch({
      '/api/v1/guards': () =>
        Promise.resolve(
          new Response(
            JSON.stringify({
              path: '/tmp/guards.toml',
              packs: [],
              rules: [],
              errors: ['invalid rule expression'],
            }),
          ),
        ),
    });
    window.history.replaceState({}, '', '/security');
    render(<App />);
    expect(await screen.findByText('Some rules did not compile')).toBeTruthy();
    expect(screen.getByText('invalid rule expression')).toBeTruthy();
    expect(screen.getByText('/tmp/guards.toml')).toBeTruthy();
    expect(screen.getByText('agento11y skills show setup-local-guards')).toBeTruthy();
  });

  it('keeps the Local settings route and follows its Security link without losing edits', async () => {
    const fetchMock = installFetch();
    window.history.replaceState({}, '', '/settings?tab=local');
    render(<App />);
    await screen.findByRole('switch', { name: 'Debug logging' });
    for (const text of ['Tags', 'Appearance', 'Runtime', 'Identity (optional)'])
      expect(screen.getByText(text)).toBeTruthy();
    expect(screen.queryByRole('switch', { name: 'Enable guards' })).toBeNull();
    expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/guards')).toHaveLength(0);
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(within(screen.getByText(/Manage local guards in/)).getByRole('link', { name: 'Security' }));
    expect(await screen.findByRole('heading', { name: 'Security' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Save to config.env' })).toBeNull();
    await act(async () => window.history.back());
    await screen.findByRole('switch', { name: 'Debug logging' });
    expect(window.location.search).toBe('?tab=local');
    expect(screen.getByRole('switch', { name: 'Debug logging' }).getAttribute('aria-checked')).toBe('true');
    expect(screen.getByRole('button', { name: 'Save to config.env' })).toBeTruthy();
  });
});

describe('Security navigation', () => {
  it.each(['/security', '/security/'])('recognizes direct loads at %s', async (path) => {
    installFetch();
    window.history.replaceState({}, '', path);
    expect(securityRouteActive()).toBe(true);
    render(<App />);
    await waitFor(() => expect(document.title).toBe('Security · agento11y local'));
    expect(screen.getByRole('link', { name: 'Security' }).getAttribute('aria-current')).toBe('page');
    expect(await screen.findByRole('heading', { name: 'Security' })).toBeTruthy();
    expect(await screen.findByRole('switch', { name: 'Enable guards' })).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Save to config.env' })).toBeNull();
    expect(screen.queryByText('config.env preview')).toBeNull();
  });

  it.each(['/settings', '/security/nested', '/'])('rejects unrelated routes at %s', (path) => {
    window.history.replaceState({}, '', path);
    expect(securityRouteActive()).toBe(false);
  });

  it.each(['Settings', 'Analytics', 'Sessions'])('navigates to %s and returns through history', async (name) => {
    const fetchMock = installFetch();
    window.history.replaceState({}, '', '/security');
    render(<App />);
    await waitFor(() => expect(document.title).toContain('Security'));
    fireEvent.click(screen.getByRole('link', { name }));
    await waitFor(() => expect(document.title).not.toContain('Security'));
    const reads = fetchMock.mock.calls.filter(([url]) => url === '/api/v1/config').length;
    await act(async () => window.history.back());
    await waitFor(() => expect(document.title).toBe('Security · agento11y local'));
    expect(screen.getByRole('link', { name: 'Security' }).getAttribute('aria-current')).toBe('page');
    await waitFor(() =>
      expect(fetchMock.mock.calls.filter(([url]) => url === '/api/v1/config').length).toBeGreaterThan(reads),
    );
    await act(async () => window.history.forward());
    await waitFor(() => expect(document.title).not.toContain('Security'));
    fireEvent.click(screen.getByRole('link', { name: 'Security' }));
    await waitFor(() => expect(window.location.pathname).toBe('/security'));
    expect(document.title).toBe('Security · agento11y local');
  });

  it.each([
    { ctrlKey: true },
    { metaKey: true },
    { shiftKey: true },
    { button: 1 },
  ])('preserves native modified navigation %j', async (modifiers) => {
    installFetch();
    window.history.replaceState({}, '', '/settings');
    render(<App />);
    await waitFor(() => expect(document.title).toContain('Settings'));
    const link = screen.getByRole('link', { name: 'Security' });
    expect(link.getAttribute('href')).toBe('/security');
    const pushState = vi.spyOn(window.history, 'pushState');
    expect(fireEvent.click(link, modifiers)).toBe(true);
    expect(pushState).not.toHaveBeenCalled();
    expect(document.title).toContain('Settings');
  });

  it.each([{ ctrlKey: true }, { metaKey: true }])('opens session search with %j K', async (modifier) => {
    installFetch();
    window.history.replaceState({}, '', '/security');
    render(<App />);
    await waitFor(() => expect(document.title).toContain('Security'));
    fireEvent.keyDown(window, { key: 'k', ...modifier });
    await waitFor(() => expect(window.location.pathname).toBe('/'));
    await waitFor(() => expect(document.activeElement?.tagName).toBe('INPUT'));
  });
});
