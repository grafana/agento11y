import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import type { ConversationDetail } from '../internal/local/web/src/types';
import { metricsResponse } from './fixtures';

const A = '01a1b6bf-e6c8-7627-a64e-26fbc210f69f';
const B = '9f3c2a10-4b7e-4d21-9c5a-0e8f6d1b2c3a';
const COPIED = 'Command copied. Paste it into your terminal.';

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

function detail(id: string, cwd: string, agent: 'claude-code' | 'pi' = 'pi'): ConversationDetail {
  return { id, generations: [], resume: { status: 'ready', agent, session_id: id, cwd } };
}

function command(id: string, cwd: string) {
  return `cd '${cwd}' && pi --session '${id}'`;
}

class FakeEventSource {
  static last: FakeEventSource | null = null;
  onmessage: ((event: { data: string }) => void) | null = null;
  constructor() {
    FakeEventSource.last = this;
  }
  close() {}
}

// Each detail request for a conversation takes the next queued response;
// an empty queue answers with a request that never settles.
function installFetch(queues: Record<string, Promise<Response>[]>) {
  const fetchMock = vi.fn((input: string | URL | Request) => {
    const url = String(input);
    const prefix = '/api/v1/conversations/';
    if (url.startsWith(prefix)) {
      const id = decodeURIComponent(url.slice(prefix.length));
      return queues[id]?.shift() ?? new Promise<Response>(() => {});
    }
    if (url.startsWith('/api/v1/conversations?')) {
      return Promise.resolve(jsonResponse({ conversations: [], total_conversations: 0 }));
    }
    if (url.startsWith('/api/v1/metrics/conversations?')) return Promise.resolve(jsonResponse(metricsResponse()));
    if (url.startsWith('/api/v1/metrics/tokens')) {
      return Promise.resolve(jsonResponse({ interval_seconds: 10, points: [] }));
    }
    if (url === '/api/v1/history/agents') return Promise.resolve(jsonResponse({ agents: [] }));
    if (url === '/api/v1/history/offer') return Promise.resolve(jsonResponse({ offers: [] }));
    return Promise.resolve(jsonResponse({}));
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('EventSource', FakeEventSource);
  return fetchMock;
}

function detailRequests(fetchMock: ReturnType<typeof installFetch>, id: string) {
  return fetchMock.mock.calls.filter(([input]) => String(input) === `/api/v1/conversations/${id}`).length;
}

function navigate(id: string) {
  act(() => {
    window.history.pushState({}, '', `/conversations/${id}`);
    window.dispatchEvent(new PopStateEvent('popstate'));
  });
}

// Sends one live-update event for the open conversation. The viewer
// debounces it before the quiet detail refresh starts.
async function liveUpdate(fetchMock: ReturnType<typeof installFetch>, id: string) {
  const before = detailRequests(fetchMock, id);
  act(() => {
    FakeEventSource.last?.onmessage?.({ data: JSON.stringify({ conversation_id: id, generation_id: 'g-new' }) });
  });
  await waitFor(() => expect(detailRequests(fetchMock, id)).toBe(before + 1));
}

// The message appears in the live region and in the panel.
function copiedShown() {
  return screen.queryAllByText(COPIED).length > 0;
}

function stubClipboard(writeText: (text: string) => Promise<void>) {
  const spy = vi.fn(writeText);
  Object.defineProperty(navigator, 'clipboard', { value: { writeText: spy }, configurable: true });
  return spy;
}

async function copiedText(writeText: ReturnType<typeof stubClipboard>) {
  await act(async () => {
    fireEvent.click(screen.getByRole('button', { name: 'Copy resume command' }));
  });
  return writeText.mock.calls.at(-1)?.[0];
}

beforeEach(() => {
  window.history.replaceState({}, '', `/conversations/${A}`);
});

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  Object.defineProperty(navigator, 'clipboard', { value: undefined, configurable: true });
  FakeEventSource.last = null;
  window.history.replaceState({}, '', '/');
});

describe('resume command from a direct link', () => {
  it.each([
    ['claude-code', `cd '/work/repo' && claude --resume '${A}'`],
    ['pi', `cd '/work/repo' && pi --session '${A}'`],
  ] as const)('copies the %s command without a list row', async (agent, want) => {
    installFetch({
      [A]: [Promise.resolve(jsonResponse(detail(A, '/work/repo', agent)))],
    });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await screen.findByRole('button', { name: 'Copy resume command' });
    expect(await copiedText(writeText)).toBe(want);
  });

  it('shows why a recognized session has no command', async () => {
    installFetch({
      [A]: [
        Promise.resolve(
          jsonResponse({ id: A, generations: [], resume: { status: 'unavailable', reason: 'conflicting_workspace' } }),
        ),
      ],
    });
    await act(async () => {
      render(<App />);
    });
    const copy = (await screen.findByRole('button', { name: 'Copy resume command' })) as HTMLButtonElement;
    expect(copy.disabled).toBe(true);
    expect(
      screen.getByText(
        'Resume command unavailable: the main agent recorded more than one working directory, and none of them contains all the others.',
      ),
    ).toBeTruthy();
  });
});

describe('resume command across detail requests', () => {
  it('keeps the newer response when a live refresh overtakes the initial load', async () => {
    const initial = deferred<Response>();
    const refresh = deferred<Response>();
    const fetchMock = installFetch({ [A]: [initial.promise, refresh.promise] });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await liveUpdate(fetchMock, A);

    await act(async () => {
      refresh.resolve(jsonResponse(detail(A, '/work/new')));
    });
    await screen.findByRole('button', { name: 'Copy resume command' });
    expect(screen.queryByText('Loading…')).toBeNull();
    await act(async () => {
      initial.resolve(jsonResponse(detail(A, '/work/old')));
    });

    expect(await copiedText(writeText)).toBe(command(A, '/work/new'));
  });

  it('shows an error from a refresh before any content, and the late initial load replaces it', async () => {
    const initial = deferred<Response>();
    const refresh = deferred<Response>();
    const fetchMock = installFetch({ [A]: [initial.promise, refresh.promise] });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await liveUpdate(fetchMock, A);

    await act(async () => {
      refresh.resolve(jsonResponse({ error: 'boom' }, 500));
    });
    expect(await screen.findByText('Failed to load session')).toBeTruthy();
    expect(screen.queryByText('Loading…')).toBeNull();
    await act(async () => {
      initial.resolve(jsonResponse(detail(A, '/work/old')));
    });
    expect(screen.queryByText('Failed to load session')).toBeNull();
    expect(await copiedText(writeText)).toBe(command(A, '/work/old'));
  });

  it('applies the initial load while a refresh is in flight, then the refresh', async () => {
    const initial = deferred<Response>();
    const refresh = deferred<Response>();
    const fetchMock = installFetch({ [A]: [initial.promise, refresh.promise] });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await liveUpdate(fetchMock, A);

    await act(async () => {
      initial.resolve(jsonResponse(detail(A, '/work/old')));
    });
    expect(screen.queryByText('Loading…')).toBeNull();
    expect(await copiedText(writeText)).toBe(command(A, '/work/old'));

    await act(async () => {
      refresh.resolve(jsonResponse(detail(A, '/work/new')));
    });
    await waitFor(() => expect(copiedShown()).toBe(false));
    expect(await copiedText(writeText)).toBe(command(A, '/work/new'));
  });

  it('applies overlapping refreshes as they arrive and ignores a failure once content is shown', async () => {
    const first = deferred<Response>();
    const second = deferred<Response>();
    const fetchMock = installFetch({
      [A]: [Promise.resolve(jsonResponse(detail(A, '/work/a'))), first.promise, second.promise],
    });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await screen.findByRole('button', { name: 'Copy resume command' });
    await liveUpdate(fetchMock, A);
    await liveUpdate(fetchMock, A);

    await act(async () => {
      first.resolve(jsonResponse(detail(A, '/work/first')));
    });
    expect(await copiedText(writeText)).toBe(command(A, '/work/first'));
    await act(async () => {
      second.resolve(jsonResponse({ error: 'transient' }, 500));
    });
    expect(screen.queryByText('Failed to load session')).toBeNull();
    expect(await copiedText(writeText)).toBe(command(A, '/work/first'));
  });

  it('ignores responses from before navigating away and back', async () => {
    const firstA = deferred<Response>();
    const onlyB = deferred<Response>();
    const secondA = deferred<Response>();
    installFetch({ [A]: [firstA.promise, secondA.promise], [B]: [onlyB.promise] });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    navigate(B);
    navigate(A);

    await act(async () => {
      secondA.resolve(jsonResponse(detail(A, '/work/current')));
    });
    await screen.findByRole('button', { name: 'Copy resume command' });
    await act(async () => {
      onlyB.resolve(jsonResponse(detail(B, '/work/b')));
      firstA.resolve(jsonResponse({ error: 'stale failure' }, 500));
    });

    expect(screen.queryByText('Failed to load session')).toBeNull();
    expect(await copiedText(writeText)).toBe(command(A, '/work/current'));
  });

  it('drops a clipboard result that settles after navigating to another session', async () => {
    const write = deferred<void>();
    installFetch({
      [A]: [Promise.resolve(jsonResponse(detail(A, '/work/a')))],
      [B]: [Promise.resolve(jsonResponse(detail(B, '/work/b')))],
    });
    const writeText = stubClipboard(() => write.promise);
    await act(async () => {
      render(<App />);
    });
    fireEvent.click(await screen.findByRole('button', { name: 'Copy resume command' }));
    expect(writeText).toHaveBeenCalledWith(command(A, '/work/a'));

    navigate(B);
    await screen.findByRole('button', { name: 'Copy resume command' });
    await act(async () => {
      write.resolve();
    });

    expect(copiedShown()).toBe(false);
    expect(screen.queryByRole('textbox', { name: 'Resume command' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Copy resume command' }).getAttribute('aria-busy')).toBeNull();
  });

  it('keeps the panel through identical live refreshes and failed ones, and closes it when the command changes', async () => {
    const fetchMock = installFetch({
      [A]: [
        Promise.resolve(jsonResponse(detail(A, '/work/a'))),
        Promise.resolve(jsonResponse(detail(A, '/work/a'))),
        Promise.resolve(jsonResponse({ error: 'transient' }, 500)),
        Promise.resolve(jsonResponse(detail(A, '/work/moved'))),
      ],
    });
    const writeText = stubClipboard(() => Promise.resolve());
    await act(async () => {
      render(<App />);
    });
    await screen.findByRole('button', { name: 'Copy resume command' });
    expect(await copiedText(writeText)).toBe(command(A, '/work/a'));
    expect(copiedShown()).toBe(true);

    await liveUpdate(fetchMock, A);
    await liveUpdate(fetchMock, A);
    await act(async () => {});
    expect(copiedShown()).toBe(true);
    expect(screen.queryByText('Failed to load session')).toBeNull();

    await liveUpdate(fetchMock, A);
    await waitFor(() => expect(copiedShown()).toBe(false));
    expect(await copiedText(writeText)).toBe(command(A, '/work/moved'));
  });
});
