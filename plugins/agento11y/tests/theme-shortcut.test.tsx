import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import type { ConfigResponse, Settings, ThemePreference } from '../internal/local/web/src/types';

function settings(theme: ThemePreference): Settings {
  return {
    theme,
    endpoint: '',
    tenantId: '',
    otlpEndpoint: '',
    tokenSet: false,
    token: '',
    tokenCleared: false,
    otlpHeaders: '',
    otlpHeadersSet: false,
    otlpHeadersCleared: false,
    capture: '',
    tags: [],
    guards: 'off',
    guardTimeout: '',
    debug: false,
    autoUpdate: true,
    userId: '',
    localForward: false,
  };
}

function config(theme: ThemePreference): ConfigResponse {
  return {
    settings: settings(theme),
    preview: `AGENTO11Y_THEME=${theme}\n`,
    path: '/tmp/config.env',
    stackUrl: '',
    forwardStatus: null,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

interface Deferred<T> {
  promise: Promise<T>;
  resolve: (value: T) => void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function installFetch(patches: Promise<Response>[] = []) {
  let patchIndex = 0;
  const fetchMock = vi.fn((input: string | URL | Request, init?: RequestInit) => {
    const url = String(input);
    if (url === '/api/v1/config' && init?.method === 'PATCH') {
      const response = patches[patchIndex++];
      if (!response) throw new Error(`Unexpected theme PATCH ${patchIndex}`);
      return response;
    }
    if (url === '/api/v1/config') return Promise.resolve(jsonResponse(config('dark')));
    if (url.startsWith('/api/v1/conversations?')) {
      return Promise.resolve(jsonResponse({ conversations: [], total_conversations: 0 }));
    }
    if (url.startsWith('/api/v1/metrics/tokens')) {
      return Promise.resolve(jsonResponse({ interval_seconds: 10, points: [] }));
    }
    if (url === '/api/v1/history/agents') return Promise.resolve(jsonResponse({ agents: [] }));
    if (url === '/api/v1/history/offer') return Promise.resolve(jsonResponse({ offers: [] }));
    if (url === 'https://models.dev/api.json') return Promise.resolve(jsonResponse({}));
    return Promise.resolve(jsonResponse({}));
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('EventSource', undefined);
  return fetchMock;
}

function pressThemeShortcut() {
  fireEvent.keyDown(window, { key: 'c' });
  fireEvent.keyDown(window, { key: 't' });
}

function patchCalls(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.filter(([, init]) => init?.method === 'PATCH');
}

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  window.history.replaceState({}, '', '/');
  document.documentElement.removeAttribute('data-system-theme');
  document.documentElement.setAttribute('data-theme', 'dark');
});

describe('App theme shortcut', () => {
  it('rolls back the optimistic theme when the PATCH fails', async () => {
    const patch = deferred<Response>();
    installFetch([patch.promise]);
    document.documentElement.setAttribute('data-theme', 'dark');
    render(<App />);
    await waitFor(() => expect(screen.getAllByText('Sessions').length).toBeGreaterThan(0));

    pressThemeShortcut();
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('light'));

    patch.resolve(jsonResponse({ error: 'write failed' }, 500));
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('dark'));
  });

  it('keeps the latest theme while queued PATCH responses arrive', async () => {
    const patches = [deferred<Response>(), deferred<Response>(), deferred<Response>()];
    const fetchMock = installFetch(patches.map((patch) => patch.promise));
    document.documentElement.setAttribute('data-theme', 'dark');
    render(<App />);
    await waitFor(() => expect(screen.getAllByText('Sessions').length).toBeGreaterThan(0));

    pressThemeShortcut();
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('light'));
    pressThemeShortcut();
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('dark'));
    pressThemeShortcut();
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('light'));
    expect(patchCalls(fetchMock)).toHaveLength(1);

    patches[0]?.resolve(jsonResponse(config('light')));
    await waitFor(() => expect(patchCalls(fetchMock)).toHaveLength(2));
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');

    patches[1]?.resolve(jsonResponse(config('dark')));
    await waitFor(() => expect(patchCalls(fetchMock)).toHaveLength(3));
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');

    patches[2]?.resolve(jsonResponse(config('light')));
    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('light'));
  });

  it('does not overwrite a dirty Settings form', async () => {
    const fetchMock = installFetch();
    window.history.replaceState({}, '', '/settings?tab=local');
    document.documentElement.setAttribute('data-theme', 'dark');
    render(<App />);
    await waitFor(() => expect(screen.getByRole('button', { name: 'Dark' })).toBeTruthy());

    fireEvent.click(screen.getAllByRole('switch')[0] as HTMLElement);
    pressThemeShortcut();

    await waitFor(() => expect(document.documentElement.getAttribute('data-theme')).toBe('dark'));
    expect(patchCalls(fetchMock)).toHaveLength(0);
  });
});
