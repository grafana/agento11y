import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { App } from '../internal/local/web/src/app';
import type { LocalGuardsUpdate } from '../internal/local/web/src/settings-local-guards';
import { type HistoryImport, SettingsView } from '../internal/local/web/src/settings-screen';
import type { ConfigResponse, GuardPack, GuardsFile, Settings } from '../internal/local/web/src/types';
import { metricsResponse } from './fixtures';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  vi.useRealTimers();
  window.history.replaceState({}, '', '/');
  document.documentElement.setAttribute('data-theme', 'dark');
});

function pack(overrides: Partial<GuardPack> = {}): GuardPack {
  return {
    id: 'secrets',
    title: 'Secret redaction',
    description: 'Redact API keys, tokens, and private keys in captured tool calls.',
    kind: 'redact',
    detail: '22 common secret formats',
    preview: ['github_pat', 'openai_sk'],
    enabled: false,
    ...overrides,
  };
}

function guards(overrides: Partial<GuardsFile> = {}): GuardsFile {
  return {
    path: '~/.config/agento11y/guards.toml',
    exists: true,
    enabled: true,
    enforcing: 0,
    packs: [
      pack(),
      pack({ id: 'git', title: 'Git safety', kind: 'deny', preview: ['git reset --hard', 'git stash drop'] }),
      pack({ id: 'destructive', title: 'Destructive commands', kind: 'deny', preview: ['rm -rf', 'rm -fr'] }),
    ],
    rules: [],
    ...overrides,
  };
}

function settings(guardMode = 'failopen'): Settings {
  return {
    theme: 'dark',
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
    guards: guardMode,
    guardTimeout: '',
    debug: false,
    autoUpdate: true,
    userId: '',
    localForward: false,
  };
}

function config(settings: Settings): ConfigResponse {
  return { settings: { ...settings }, path: '/tmp/config.env', preview: '', stackUrl: '', forwardStatus: null };
}

const history: HistoryImport = {
  agents: [],
  offers: [],
  run: null,
  error: null,
  start: async () => null,
  cancel: async () => undefined,
  dismiss: async () => undefined,
  reloadOffers: async () => undefined,
};

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: Error) => void;
  const promise = new Promise<T>((done, fail) => {
    resolve = done;
    reject = fail;
  });
  return { promise, resolve, reject };
}

function setup(guardMode = 'failopen') {
  const server = {
    settings: settings(guardMode),
    guards: guards({ enabled: guardMode !== 'off' }),
    failureMode: guardMode === 'off' ? 'failopen' : guardMode,
    guardGets: [] as Promise<Response>[],
    guardPuts: [] as Promise<Response>[],
    configGets: [] as Promise<Response>[],
    configPuts: [] as Promise<Response>[],
    configPatches: [] as Promise<Response>[],
  };
  const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input);
    if (url === '/api/v1/guards') {
      if (init?.method !== 'PUT') return server.guardGets.shift() || Promise.resolve(jsonResponse(server.guards));
      const queued = server.guardPuts.shift();
      if (queued) return queued;
      const body = JSON.parse(String(init.body)) as LocalGuardsUpdate;
      if (body.enabled !== undefined) {
        server.guards.enabled = body.enabled;
        server.settings.guards = body.enabled ? server.failureMode : 'off';
      }
      server.guards.packs = server.guards.packs.map((p) => ({ ...p, enabled: body.packs?.[p.id] ?? p.enabled }));
      return Promise.resolve(jsonResponse(server.guards));
    }
    if (url === '/api/v1/config') {
      if (init?.method === 'PATCH') {
        server.settings.theme = JSON.parse(String(init.body)).theme;
        return server.configPatches.shift() || Promise.resolve(jsonResponse(config(server.settings)));
      }
      if (init?.method !== 'PUT')
        return server.configGets.shift() || Promise.resolve(jsonResponse(config(server.settings)));
      const queued = server.configPuts.shift();
      if (queued) return queued;
      server.settings = JSON.parse(String(init.body)).settings as Settings;
      server.guards.enabled = server.settings.guards !== 'off';
      return Promise.resolve(jsonResponse(config(server.settings)));
    }
    if (url === '/api/v1/config:preview') return Promise.resolve(jsonResponse({ preview: '' }));
    if (url === '/api/v1/history/agents') return Promise.resolve(jsonResponse({ agents: [] }));
    if (url === '/api/v1/history/offer') return Promise.resolve(jsonResponse({ offers: [] }));
    if (url.startsWith('/api/v1/conversations?'))
      return Promise.resolve(jsonResponse({ conversations: [], total_conversations: 0 }));
    if (url.startsWith('/api/v1/metrics/conversations?')) return Promise.resolve(jsonResponse(metricsResponse()));
    if (url.startsWith('/api/v1/metrics/tokens'))
      return Promise.resolve(jsonResponse({ interval_seconds: 10, points: [] }));
    if (url === 'https://models.dev/api.json') return Promise.resolve(jsonResponse({}));
    throw new Error(`Unexpected request: ${url}`);
  });
  vi.stubGlobal('fetch', fetchMock);
  vi.stubGlobal('EventSource', undefined);
  let publish!: (body: ConfigResponse) => void;
  const onConfig = vi.fn();
  function Host({ tab = 'local' }: { tab?: string }) {
    const [body, setBody] = useState(() => config(server.settings));
    publish = setBody;
    return (
      <SettingsView
        history={history}
        config={body}
        configError={null}
        activeSettingsTab={tab}
        onSelectTab={vi.fn()}
        onConfig={(next) => {
          onConfig(next);
          setBody(next);
        }}
      />
    );
  }
  const calls = (url: string, method = 'GET') =>
    fetchMock.mock.calls.filter(([input, init]) => String(input) === url && (init?.method || 'GET') === method);
  return {
    server,
    fetchMock,
    onConfig,
    Host,
    calls,
    poll: async () => {
      await act(async () => publish(config(server.settings)));
    },
  };
}

function master() {
  return screen.getByRole('switch', { name: 'Enable guards' });
}
function save() {
  return screen.getByRole('button', { name: 'Save to config.env' });
}
function checked(name: string) {
  return screen.getByRole('switch', { name }).getAttribute('aria-checked');
}
async function ready() {
  await waitFor(() => expect(master().hasAttribute('disabled')).toBe(false));
}

describe('SettingsLocalGuardsCard in SettingsView', () => {
  it('hides packs until guards are enabled', async () => {
    const { Host, calls } = setup('off');
    render(<Host />);
    await ready();
    expect(screen.queryByRole('switch', { name: 'Secret redaction' })).toBeNull();
    fireEvent.click(master());
    await screen.findByRole('switch', { name: 'Secret redaction' });
    await ready();
    expect(checked('Enable guards')).toBe('true');
    expect(JSON.parse(String(calls('/api/v1/guards', 'PUT')[0]?.[1]?.body))).toEqual({ enabled: true });
    expect(screen.queryByText('Unsaved changes')).toBeNull();
  });

  it('lists packs and toggles one on', async () => {
    const { Host, calls } = setup();
    render(<Host />);
    await ready();
    expect(checked('Git safety')).toBe('false');
    fireEvent.click(screen.getByRole('switch', { name: 'Secret redaction' }));
    await waitFor(() => expect(checked('Secret redaction')).toBe('true'));
    await ready();
    expect(JSON.parse(String(calls('/api/v1/guards', 'PUT')[0]?.[1]?.body))).toEqual({ packs: { secrets: true } });
  });

  it('turns every pack off when guards are disabled', async () => {
    const { Host, calls, server } = setup();
    server.guards.packs = server.guards.packs.map((p) => ({ ...p, enabled: p.id === 'git' }));
    render(<Host />);
    await ready();
    fireEvent.click(master());
    await waitFor(() => expect(checked('Enable guards')).toBe('false'));
    await ready();
    expect(screen.queryByRole('switch', { name: 'Git safety' })).toBeNull();
    expect(JSON.parse(String(calls('/api/v1/guards', 'PUT')[0]?.[1]?.body))).toEqual({
      enabled: false,
      packs: { secrets: false, git: false, destructive: false },
    });
  });

  it('expands a pack preview', async () => {
    const { Host } = setup();
    render(<Host />);
    const view = await screen.findByRole('button', { name: 'View Git safety' });
    expect(screen.queryByText('git stash drop')).toBeNull();
    fireEvent.click(view);
    expect(screen.getByText('git stash drop')).toBeTruthy();
    expect(view.getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(view);
    expect(screen.queryByText('git stash drop')).toBeNull();
  });

  it('does not mutate before the initial guard GET completes', async () => {
    const { Host, server, calls } = setup('off');
    const initial = deferred<Response>();
    server.guardGets.push(initial.promise);
    render(<Host />);
    expect(master().hasAttribute('disabled')).toBe(true);
    fireEvent.click(master());
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(0);
    await act(async () => initial.resolve(jsonResponse(server.guards)));
    await ready();
    fireEvent.click(master());
    await ready();
    expect(checked('Enable guards')).toBe('true');
  });

  it.each([
    'success',
    'failure',
  ] as const)('ignores a stale initial GET %s after a newer poll and PUT', async (outcome) => {
    const { Host, server, poll } = setup('off');
    const initial = deferred<Response>();
    server.guardGets.push(initial.promise);
    render(<Host />);
    await poll();
    await ready();
    fireEvent.click(master());
    await ready();
    await act(async () => {
      if (outcome === 'success') initial.resolve(jsonResponse(guards({ enabled: false, packs: [] })));
      else initial.reject(new Error('old GET failed'));
    });
    expect(checked('Enable guards')).toBe('true');
    expect(screen.getByRole('switch', { name: 'Git safety' })).toBeTruthy();
    expect(screen.queryByText('old GET failed')).toBeNull();
  });

  it.each([
    'failopen',
    'failclosed',
  ])('keeps dirty edits and %s when Save follows a successful enable with config GET pending', async (mode) => {
    const { Host, server, calls } = setup('off');
    server.failureMode = mode;
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    render(<Host />);
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(screen.getByRole('button', { name: 'Light' }));
    fireEvent.click(master());
    await waitFor(() => expect(calls('/api/v1/config')).toHaveLength(1));
    expect(server.settings.guards).toBe(mode);
    expect(checked('Enable guards')).toBe('true');
    expect(save().hasAttribute('disabled')).toBe(true);
    fireEvent.click(save());
    expect(calls('/api/v1/config', 'PUT')).toHaveLength(0);
    await act(async () => refresh.resolve(jsonResponse(config(server.settings))));
    await ready();
    expect(checked('Debug logging')).toBe('true');
    expect(screen.getByRole('button', { name: 'Light' }).getAttribute('aria-pressed')).toBe('true');
    fireEvent.click(save());
    await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
    expect(JSON.parse(String(calls('/api/v1/config', 'PUT')[0]?.[1]?.body)).settings).toMatchObject({
      guards: mode,
      debug: true,
      theme: 'light',
    });
    await waitFor(() => expect(screen.queryByText('Unsaved changes')).toBeNull());
  });

  it.each([false, true])('adopts external enable, disable, and pack changes with dirty=%s', async (dirty) => {
    const { Host, server, poll, calls } = setup('off');
    render(<Host />);
    await ready();
    if (dirty) fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    server.settings.guards = 'failclosed';
    server.guards = guards({ packs: [pack({ enabled: true })] });
    await poll();
    await waitFor(() => expect(checked('Secret redaction')).toBe('true'));
    expect(checked('Enable guards')).toBe('true');
    server.guards.packs = [pack({ enabled: false })];
    await poll();
    await waitFor(() => expect(checked('Secret redaction')).toBe('false'));
    server.settings.guards = 'off';
    server.guards.enabled = false;
    await poll();
    expect(checked('Enable guards')).toBe('false');
    expect(screen.queryByRole('switch', { name: 'Secret redaction' })).toBeNull();
    expect(checked('Debug logging')).toBe(String(dirty));
    if (dirty) {
      fireEvent.click(save());
      await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
      expect(server.settings).toMatchObject({ guards: 'off', debug: true });
    } else expect(screen.queryByText('Unsaved changes')).toBeNull();
  });

  it.each([
    'failopen',
    'failclosed',
  ])('saves an unrelated dirty edit with externally enabled %s guards', async (mode) => {
    const { Host, server, poll, calls } = setup('off');
    render(<Host />);
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    server.settings.guards = mode;
    server.guards.enabled = true;
    await poll();
    fireEvent.click(save());
    await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
    expect(server.settings).toMatchObject({ guards: mode, debug: true });
  });

  it('uses config enabled state even if the pack GET returns an older enabled value', async () => {
    const { Host, server, poll } = setup();
    render(<Host />);
    await ready();
    server.settings.guards = 'off';
    server.guardGets.push(Promise.resolve(jsonResponse(guards({ enabled: true }))));
    await poll();
    expect(checked('Enable guards')).toBe('false');
  });

  it('does not let a pending pack refresh undo a pack PUT', async () => {
    const { Host, server, poll, calls } = setup();
    render(<Host />);
    await ready();
    const refresh = deferred<Response>();
    server.guardGets.push(refresh.promise);
    await poll();
    fireEvent.click(screen.getByRole('switch', { name: 'Secret redaction' }));
    await ready();
    await act(async () => refresh.resolve(jsonResponse(guards())));
    expect(checked('Secret redaction')).toBe('true');
    const reads = calls('/api/v1/guards');
    expect(reads[1]?.[1]?.signal?.aborted).toBe(true);
  });

  it('keeps the last good state on PUT failure and allows retry', async () => {
    const { Host, server, calls } = setup('off');
    server.guardPuts.push(Promise.resolve(new Response('disk is read-only', { status: 500 })));
    render(<Host />);
    await ready();
    fireEvent.click(master());
    expect(await screen.findByText('disk is read-only')).toBeTruthy();
    await ready();
    expect(checked('Enable guards')).toBe('false');
    expect(calls('/api/v1/config')).toHaveLength(0);
    fireEvent.click(master());
    await ready();
    expect(checked('Enable guards')).toBe('true');
  });

  it('keeps mutations disabled after an initial GET error until a poll recovers', async () => {
    const { Host, server, poll, calls } = setup('off');
    server.guardGets.push(Promise.resolve(new Response('cannot read packs', { status: 500 })));
    render(<Host />);
    expect(await screen.findByText('cannot read packs')).toBeTruthy();
    expect(master().hasAttribute('disabled')).toBe(true);
    fireEvent.click(master());
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(0);
    await poll();
    await ready();
    expect(screen.queryByText('cannot read packs')).toBeNull();
  });

  it('keeps Save paused after a failed config refresh and recovers on the next poll', async () => {
    const { Host, server, poll, calls } = setup('off');
    server.failureMode = 'failclosed';
    server.configGets.push(Promise.resolve(new Response('config unavailable', { status: 500 })));
    render(<Host />);
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(master());
    expect(await screen.findByText(/Guards saved, but settings refresh failed/)).toBeTruthy();
    expect(checked('Enable guards')).toBe('true');
    expect(save().hasAttribute('disabled')).toBe(true);
    fireEvent.click(save());
    expect(calls('/api/v1/config', 'PUT')).toHaveLength(0);
    await poll();
    await ready();
    fireEvent.click(save());
    await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
    expect(server.settings).toMatchObject({ guards: 'failclosed', debug: true });
  });

  it.each([
    'success',
    'failure',
  ] as const)('prefers a newer external config poll to a delayed refresh %s', async (outcome) => {
    const { Host, server, poll, calls } = setup('off');
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    render(<Host />);
    await ready();
    fireEvent.click(master());
    await waitFor(() => expect(calls('/api/v1/config')).toHaveLength(1));
    const old = config(server.settings);
    server.settings.guards = 'off';
    server.guards.enabled = false;
    await poll();
    await act(async () => {
      if (outcome === 'success') refresh.resolve(jsonResponse(old));
      else refresh.reject(new Error('old refresh failed'));
    });
    await ready();
    expect(checked('Enable guards')).toBe('false');
    expect(screen.queryByText(/old refresh failed/)).toBeNull();
  });

  it('finishes the guard write after switching tabs and does not lose dirty edits', async () => {
    const { Host, server, calls } = setup('off');
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    const view = render(<Host />);
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(master());
    await waitFor(() => expect(calls('/api/v1/config')).toHaveLength(1));
    view.rerender(<Host tab="cloud" />);
    await act(async () => refresh.resolve(jsonResponse(config(server.settings))));
    view.rerender(<Host />);
    await ready();
    expect(checked('Enable guards')).toBe('true');
    expect(checked('Debug logging')).toBe('true');
    fireEvent.click(save());
    await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
    expect(server.settings).toMatchObject({ guards: 'failopen', debug: true });
  });

  it.each([
    { stage: 'GET', fails: false },
    { stage: 'GET', fails: true },
    { stage: 'PUT', fails: false },
    { stage: 'PUT', fails: true },
    { stage: 'refresh', fails: false },
    { stage: 'refresh', fails: true },
    { stage: 'Save', fails: false },
    { stage: 'Save', fails: true },
  ])('ignores $stage completion after unmount (fails=$fails)', async ({ stage, fails }) => {
    const { Host, server, calls, onConfig } = setup('off');
    const pending = deferred<Response>();
    if (stage === 'GET') server.guardGets.push(pending.promise);
    if (stage === 'PUT') server.guardPuts.push(pending.promise);
    if (stage === 'refresh') server.configGets.push(pending.promise);
    if (stage === 'Save') server.configPuts.push(pending.promise);
    const view = render(<Host />);
    if (stage !== 'GET') {
      await ready();
      if (stage === 'Save') {
        fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
        fireEvent.click(save());
      } else fireEvent.click(master());
      if (stage === 'refresh') await waitFor(() => expect(calls('/api/v1/config')).toHaveLength(1));
    }
    const before = onConfig.mock.calls.length;
    view.unmount();
    await act(async () => {
      if (fails) pending.reject(new Error('request failed after unmount'));
      else pending.resolve(jsonResponse(stage === 'refresh' || stage === 'Save' ? config(server.settings) : guards()));
    });
    expect(onConfig).toHaveBeenCalledTimes(before);
    if (stage !== 'refresh') expect(calls('/api/v1/config')).toHaveLength(0);
  });

  it('serializes a deferred guard PUT and preserves edits made while it is pending', async () => {
    const { Host, server, calls } = setup('off');
    const pending = deferred<Response>();
    server.guardPuts.push(pending.promise);
    render(<Host />);
    await ready();
    fireEvent.click(master());
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(screen.getByRole('button', { name: 'Light' }));
    fireEvent.click(master());
    fireEvent.click(save());
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(1);
    expect(calls('/api/v1/config', 'PUT')).toHaveLength(0);
    server.settings.guards = 'failclosed';
    server.settings.userId = 'external-user';
    server.guards.enabled = true;
    await act(async () => pending.resolve(jsonResponse(server.guards)));
    await ready();
    expect(checked('Debug logging')).toBe('true');
    expect(screen.getByDisplayValue('external-user')).toBeTruthy();
    fireEvent.click(save());
    await waitFor(() => expect(calls('/api/v1/config', 'PUT')).toHaveLength(1));
    expect(server.settings).toMatchObject({
      guards: 'failclosed',
      debug: true,
      theme: 'light',
      userId: 'external-user',
    });
  });

  it('keeps guards enabled and edits pending when config Save fails', async () => {
    const { Host, server } = setup('off');
    render(<Host />);
    await ready();
    fireEvent.click(master());
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    server.configPuts.push(Promise.resolve(new Response('config write failed', { status: 500 })));
    fireEvent.click(save());
    expect(await screen.findByText('config write failed')).toBeTruthy();
    await ready();
    expect(checked('Enable guards')).toBe('true');
    expect(checked('Debug logging')).toBe('true');
    fireEvent.click(save());
    await waitFor(() => expect(screen.queryByText('Unsaved changes')).toBeNull());
    expect(server.settings).toMatchObject({ guards: 'failopen', debug: true });
  });

  it.each([
    { savedMode: 'failopen', draft: 'Fail closed', mode: 'failclosed', recovery: 'refresh' },
    { savedMode: 'failclosed', draft: 'Fail open', mode: 'failopen', recovery: 'refresh' },
    { savedMode: 'failopen', draft: 'Off', mode: 'off', recovery: 'refresh' },
    { savedMode: 'failopen', draft: 'Fail closed', mode: 'failclosed', recovery: 'failed refresh' },
    { savedMode: 'failclosed', draft: 'Fail open', mode: 'failopen', recovery: 'failed refresh' },
    { savedMode: 'failopen', draft: 'Off', mode: 'off', recovery: 'failed refresh' },
  ])('preserves staged $mode after a pack-only PUT and $recovery', async ({ savedMode, draft, mode, recovery }) => {
    const { Host, server, poll, calls } = setup(savedMode);
    server.settings.endpoint = 'https://stack.example.test';
    server.settings.localForward = true;
    const view = render(<Host tab="cloud" />);
    fireEvent.click(screen.getByRole('button', { name: draft }));
    await act(async () => view.rerender(<Host />));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    await act(async () => fireEvent.click(screen.getByRole('switch', { name: 'Git safety' })));
    expect(save().hasAttribute('disabled')).toBe(true);
    expect(JSON.parse(String(calls('/api/v1/guards', 'PUT')[0]?.[1]?.body))).toEqual({ packs: { git: true } });
    if (recovery === 'failed refresh') {
      await act(async () => refresh.reject(new Error('config refresh failed')));
      expect(save().hasAttribute('disabled')).toBe(true);
      await poll();
    } else {
      await act(async () => refresh.resolve(jsonResponse(config(server.settings))));
    }
    expect(save().hasAttribute('disabled')).toBe(false);
    expect(checked('Debug logging')).toBe('true');
    expect(checked('Git safety')).toBe('true');
    view.rerender(<Host tab="cloud" />);
    expect(screen.getByRole('button', { name: draft }).getAttribute('aria-pressed')).toBe('true');
    await act(async () => fireEvent.click(save()));
    expect(server.settings).toMatchObject({ guards: mode, debug: true });
  });

  it.each([
    { actual: 'failopen', label: 'Fail open', recovery: 'refresh' },
    { actual: 'failclosed', label: 'Fail closed', recovery: 'refresh' },
    { actual: 'off', label: 'Off', recovery: 'refresh' },
    { actual: 'failopen', label: 'Fail open', recovery: 'failed refresh' },
    { actual: 'failclosed', label: 'Fail closed', recovery: 'failed refresh' },
    { actual: 'off', label: 'Off', recovery: 'failed refresh' },
  ])('master toggle replaces staged guards with actual $actual after $recovery', async ({
    actual,
    label,
    recovery,
  }) => {
    const { Host, server, poll } = setup(actual === 'off' ? 'failclosed' : 'off');
    server.failureMode = actual;
    server.settings.endpoint = 'https://stack.example.test';
    server.settings.localForward = true;
    const view = render(<Host tab="cloud" />);
    fireEvent.click(screen.getByRole('button', { name: actual === 'failopen' ? 'Fail closed' : 'Fail open' }));
    await act(async () => view.rerender(<Host />));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    await act(async () => fireEvent.click(master()));
    if (recovery === 'failed refresh') {
      await act(async () => refresh.reject(new Error('config refresh failed')));
      await poll();
    } else {
      await act(async () => refresh.resolve(jsonResponse(config(server.settings))));
    }
    expect(checked('Enable guards')).toBe(String(actual !== 'off'));
    expect(save().hasAttribute('disabled')).toBe(false);
    view.rerender(<Host tab="cloud" />);
    expect(screen.getByRole('button', { name: label }).getAttribute('aria-pressed')).toBe('true');
    await act(async () => fireEvent.click(save()));
    expect(server.settings).toMatchObject({ guards: actual, debug: true });
  });

  it('does not overlap a config Save with a guard mutation', async () => {
    const { Host, server, calls } = setup();
    const pending = deferred<Response>();
    server.configPuts.push(pending.promise);
    render(<Host />);
    await ready();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(save());
    expect(master().hasAttribute('disabled')).toBe(true);
    fireEvent.click(master());
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(0);
    await act(async () => pending.resolve(jsonResponse(config({ ...server.settings, debug: true }))));
    await ready();
  });
});

function themeShortcut() {
  fireEvent.keyDown(window, { key: 'c' });
  fireEvent.keyDown(window, { key: 't' });
}

async function renderApp() {
  vi.useFakeTimers();
  window.history.replaceState({}, '', '/settings?tab=local');
  await act(async () => {
    render(<App />);
  });
  expect(master().hasAttribute('disabled')).toBe(false);
}

async function appPoll() {
  await act(async () => vi.advanceTimersByTimeAsync(30_000));
}

function lastConfigPut(calls: ReturnType<typeof setup>['calls']) {
  return JSON.parse(String(calls('/api/v1/config', 'PUT').at(-1)?.[1]?.body)).settings;
}

describe('App config writes', () => {
  it('blocks shortcuts before config loads and guard writes before packs load', async () => {
    const { server, calls } = setup('off');
    const firstConfig = deferred<Response>();
    const secondConfig = deferred<Response>();
    const firstGuards = deferred<Response>();
    server.configGets.push(firstConfig.promise, secondConfig.promise);
    server.guardGets.push(firstGuards.promise);
    vi.useFakeTimers();
    window.history.replaceState({}, '', '/settings?tab=local');
    await act(async () => {
      render(<App />);
    });
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(0);
    expect(screen.queryByRole('switch', { name: 'Enable guards' })).toBeNull();
    await act(async () => secondConfig.resolve(jsonResponse(config(server.settings))));
    expect(master().hasAttribute('disabled')).toBe(true);
    fireEvent.click(master());
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(0);
    await act(async () => firstGuards.resolve(jsonResponse(server.guards)));
    await act(async () => fireEvent.click(master()));
    await act(async () => firstConfig.resolve(jsonResponse(config(settings('off')))));
    expect(checked('Enable guards')).toBe('true');
    expect(master().hasAttribute('disabled')).toBe(false);
  });

  it('pauses clean theme shortcuts during a guard write and resumes after poll recovery', async () => {
    const { server, calls } = setup('off');
    const put = deferred<Response>();
    const refresh = deferred<Response>();
    await renderApp();
    server.guardPuts.push(put.promise);
    server.configGets.push(refresh.promise);
    await act(async () => fireEvent.click(master()));
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(0);
    server.settings.guards = 'failclosed';
    server.guards.enabled = true;
    await act(async () => put.resolve(jsonResponse(server.guards)));
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(0);
    await appPoll();
    expect(master().hasAttribute('disabled')).toBe(false);
    expect(screen.queryByText('Unsaved changes')).toBeNull();
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(1);
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(checked('Enable guards')).toBe('true');
    await act(async () => refresh.resolve(jsonResponse(config(settings('off')))));
    expect(document.documentElement.getAttribute('data-theme')).toBe('light');
    expect(checked('Enable guards')).toBe('true');
  });

  it.each(['failopen', 'failclosed'])('rejects a pre-enable theme reply during guard refresh (%s)', async (mode) => {
    const { server, calls } = setup('off');
    server.failureMode = mode;
    const patch = deferred<Response>();
    server.configPatches.push(patch.promise);
    await renderApp();
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(1);
    const oldThemeReply = config(server.settings);
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    await act(async () => fireEvent.click(master()));
    expect(checked('Enable guards')).toBe('true');
    expect(save().hasAttribute('disabled')).toBe(true);
    await act(async () => patch.resolve(jsonResponse(oldThemeReply)));
    expect(save().hasAttribute('disabled')).toBe(true);
    await act(async () => refresh.resolve(jsonResponse(config(server.settings))));
    expect(master().hasAttribute('disabled')).toBe(false);
    expect(checked('Enable guards')).toBe('true');
    expect(checked('Debug logging')).toBe('true');
    await act(async () => fireEvent.click(save()));
    expect(lastConfigPut(calls)).toMatchObject({ guards: mode, debug: true });
    expect(checked('Enable guards')).toBe('true');
    expect(screen.queryByText('Unsaved changes')).toBeNull();
  });

  it.each(['during', 'after'])('rejects old theme replies and queued shortcuts %s a config PUT', async (when) => {
    const { server, calls } = setup();
    const patch = deferred<Response>();
    server.configPatches.push(patch.promise);
    await renderApp();
    await act(async () => themeShortcut());
    const oldThemeReply = config(server.settings);
    await act(async () => themeShortcut());
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(1);
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const put = deferred<Response>();
    server.configPuts.push(put.promise);
    await act(async () => fireEvent.click(save()));
    if (when === 'during') await act(async () => patch.resolve(jsonResponse(oldThemeReply)));
    server.settings = lastConfigPut(calls);
    await act(async () => put.resolve(jsonResponse(config(server.settings))));
    if (when === 'after') await act(async () => patch.resolve(jsonResponse(oldThemeReply)));
    expect(calls('/api/v1/config', 'PATCH')).toHaveLength(1);
    expect(checked('Debug logging')).toBe('true');
    expect(screen.queryByText('Unsaved changes')).toBeNull();
    expect(document.documentElement.getAttribute('data-theme')).toBe(server.settings.theme);
    fireEvent.click(screen.getByRole('switch', { name: 'Automatic updates' }));
    await act(async () => fireEvent.click(save()));
    expect(lastConfigPut(calls)).toMatchObject({ guards: 'failopen', debug: true, autoUpdate: false });
  });

  it.each([
    'success',
    'failure',
  ])('recovers a hung guard refresh on a newer poll before its late %s', async (outcome) => {
    const { server, calls } = setup('off');
    server.failureMode = 'failclosed';
    await renderApp();
    const oldPoll = deferred<Response>();
    const beforeEnable = config(server.settings);
    server.configGets.push(oldPoll.promise);
    await appPoll();
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const refresh = deferred<Response>();
    server.configGets.push(refresh.promise);
    await act(async () => fireEvent.click(master()));
    const refreshCall = calls('/api/v1/config').at(-1);
    const oldRefresh = config(server.settings);
    await act(async () => oldPoll.resolve(jsonResponse(beforeEnable)));
    expect(save().hasAttribute('disabled')).toBe(true);
    expect(master().hasAttribute('disabled')).toBe(true);
    server.configGets.push(Promise.resolve(new Response('config unavailable', { status: 500 })));
    await appPoll();
    expect(save().hasAttribute('disabled')).toBe(true);
    expect(master().hasAttribute('disabled')).toBe(true);
    await appPoll();
    expect(refreshCall?.[1]?.signal?.aborted).toBe(true);
    expect(master().hasAttribute('disabled')).toBe(false);
    expect(save().hasAttribute('disabled')).toBe(false);
    expect(checked('Enable guards')).toBe('true');
    expect(checked('Debug logging')).toBe('true');

    const nextPut = deferred<Response>();
    server.guardPuts.push(nextPut.promise);
    await act(async () => fireEvent.click(master()));
    await act(async () => {
      if (outcome === 'success') refresh.resolve(jsonResponse(oldRefresh));
      else refresh.reject(new Error('old refresh failed'));
    });
    expect(master().hasAttribute('disabled')).toBe(true);
    expect(save().hasAttribute('disabled')).toBe(true);
    expect(screen.queryByText(/old refresh failed/)).toBeNull();
    expect(calls('/api/v1/guards', 'PUT')).toHaveLength(2);
    server.settings.guards = 'off';
    server.guards.enabled = false;
    await act(async () => nextPut.resolve(jsonResponse(server.guards)));
    expect(master().hasAttribute('disabled')).toBe(false);
    expect(checked('Enable guards')).toBe('false');
    await act(async () => fireEvent.click(save()));
    expect(lastConfigPut(calls)).toMatchObject({ guards: 'off', debug: true });
  });

  it.each(['success', 'failure'])('ignores a theme PATCH %s after App unmount', async (outcome) => {
    const { server } = setup();
    const patch = deferred<Response>();
    server.configPatches.push(patch.promise);
    await renderApp();
    await act(async () => themeShortcut());
    const oldThemeReply = config(server.settings);
    cleanup();
    server.settings.theme = 'system';
    await renderApp();
    await act(async () => {
      if (outcome === 'success') patch.resolve(jsonResponse(oldThemeReply));
      else patch.reject(new Error('old theme failed'));
    });
    expect(document.documentElement.getAttribute('data-theme')).toBe('system');
    expect(screen.getByRole('button', { name: 'Match system' }).getAttribute('aria-pressed')).toBe('true');
  });
});
