import { act, cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { type HistoryImport, SettingsSecurityView } from '../internal/local/web/src/settings-screen';
import { TopBar } from '../internal/local/web/src/shell';
import type { ConfigResponse, ForwardDestination, ForwardStatus, Settings } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

function settings(overrides: Partial<Settings> = {}): Settings {
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
    capture: 'metadata_only',
    tags: [],
    guards: 'failclosed',
    guardTimeout: '1500',
    debug: false,
    autoUpdate: false,
    userId: '',
    localForward: false,
    ...overrides,
  };
}

const extra: ForwardDestination = {
  name: 'archive',
  endpoint: 'https://archive.example/generations',
  otlpEndpoint: 'https://archive.example/otlp',
  httpGenerations: true,
  otlp: true,
  revision: 7,
};
const primary: ForwardDestination = {
  name: 'primary',
  endpoint: 'https://primary.example/generations',
  otlpEndpoint: 'https://primary.example/otlp',
  httpGenerations: true,
  otlp: true,
  revision: 1,
};
function status(overrides: Partial<ForwardStatus> = {}): ForwardStatus {
  return {
    enabled: false,
    mode: 'off',
    generations: false,
    otlp: false,
    hooks: false,
    destinations: [extra],
    forwardingPath: '/tmp/agento11y/forwarding.toml',
    hookReason: 'guard forwarding needs usable primary credentials',
    ...overrides,
  };
}
function config(saved: Partial<Settings> = {}, forwarding = status()): ConfigResponse {
  return {
    settings: settings(saved),
    forwardStatus: forwarding,
    path: '/tmp/agento11y/config.env',
    preview: 'AGENTO11Y_LOCAL_FORWARD=false\n',
    stackUrl: '',
  };
}
const history: HistoryImport = {
  agents: [],
  offers: [],
  run: null,
  error: null,
  start: async () => null,
  cancel: async () => {},
  dismiss: async () => {},
  reloadOffers: async () => {},
};

function setup(initial = config()) {
  const writes: Settings[] = [];
  const fetcher = vi.fn(async (url: string, init?: RequestInit) => {
    if (url === '/api/v1/config:preview') return new Response(JSON.stringify({ preview: initial.preview }));
    if (url === '/api/v1/config' && init?.method === 'PUT') {
      const next = (JSON.parse(String(init.body)) as { settings: Settings }).settings;
      writes.push(next);
      return new Response(
        JSON.stringify({
          ...initial,
          settings: {
            ...next,
            token: '',
            otlpHeaders: '',
            tokenCleared: false,
            otlpHeadersCleared: false,
            tokenSet: !!next.token,
          },
          forwardStatus: {
            ...initial.forwardStatus,
            enabled: next.localForward,
            mode: next.localForward ? next.capture : 'off',
          },
        }),
      );
    }
    throw new Error(`Unexpected fetch ${url}`);
  });
  vi.stubGlobal('fetch', fetcher);
  let pushConfig = (_next: ConfigResponse) => {};
  let pushError = (_next: string | null) => {};
  function Host() {
    const [current, setCurrent] = useState(initial);
    const [configError, setConfigError] = useState<string | null>(null);
    const [tab, setTab] = useState('cloud');
    pushConfig = setCurrent;
    pushError = setConfigError;
    return (
      <>
        <TopBar config={configError ? null : current} />
        <SettingsSecurityView
          activeSection="settings"
          history={history}
          config={current}
          configError={configError}
          activeSettingsTab={tab}
          onSelectTab={setTab}
          onConfig={setCurrent}
        />
      </>
    );
  }
  const view = render(<Host />);
  return {
    ...view,
    writes,
    fetcher,
    poll: (next: ConfigResponse) => act(() => pushConfig(next)),
    failPoll: () => act(() => pushError('Failed to fetch config')),
    recover: (next: ConfigResponse) =>
      act(() => {
        pushConfig(next);
        pushError(null);
      }),
  };
}

function expectNoDestinationDetails() {
  expect(screen.queryByText('Forwarding destinations')).toBeNull();
  expect(screen.queryByText('Extra destination configuration')).toBeNull();
  expect(screen.queryByRole('table')).toBeNull();
  expect(screen.queryByText(/Revision \d+/)).toBeNull();
  expect(screen.queryByText('/tmp/agento11y/forwarding.toml')).toBeNull();
  expect(screen.queryByText(extra.name)).toBeNull();
  expect(screen.queryByText(extra.endpoint)).toBeNull();
  expect(screen.queryByText(extra.otlpEndpoint)).toBeNull();
}

describe('forwarding destinations', () => {
  it('enables extra-only forwarding without a primary or destination details', async () => {
    const { writes } = setup();
    expectNoDestinationDetails();
    expect(screen.getByRole('button', { name: /Mode: Local/ })).toBeTruthy();
    expect(screen.getByText(/Cloud forwarding is off. Nothing from local sessions leaves this machine/)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Disconnect' })).toBeNull();
    // The connect flow has its own mode choice; this is the shared forwarding control.
    fireEvent.click(screen.getAllByRole('button', { name: 'Metadata only' })[1] as HTMLElement);
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.localForward).toBe(true);
    expect(writes[0]?.endpoint).toBe('');
    expect(writes[0]).not.toHaveProperty('destinations');
    expect(writes[0]).not.toHaveProperty('forwardingPath');
    expect(screen.getByRole('button', { name: 'Fail closed' }).hasAttribute('disabled')).toBe(true);
  });

  it('keeps invalid file diagnostics in the overall summary without a primary connection', () => {
    setup(config({}, status({ destinations: [], forwardingError: 'destination archive: invalid endpoint' })));
    expectNoDestinationDetails();
    expect(screen.getByText('Extra destinations are not loaded: destination archive: invalid endpoint.')).toBeTruthy();
    expect(
      screen.getByRole('button', { name: /Mode: Configuration error|Cloud forwarding: Configuration error/ }),
    ).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' })).toBeTruthy();
  });

  it('keeps aggregate partial failure details without destination counters or timestamps', () => {
    setup(
      config(
        { localForward: true },
        status({
          enabled: true,
          mode: 'metadata_only',
          generations: true,
          otlp: true,
          destinations: [
            {
              ...extra,
              legs: {
                generations: {
                  attempted: 12,
                  succeeded: 10,
                  failed: 2,
                  dropped: 3,
                  lastSuccessAt: '2026-08-22T11:58:00Z',
                  lastFailureAt: '2026-08-22T11:59:00Z',
                  lastFailureDetail: 'HTTP 429',
                },
                'otlp/traces': { attempted: 4, succeeded: 4, failed: 0, dropped: 0 },
                'otlp/metrics': { attempted: 1, succeeded: 0, failed: 1, dropped: 0 },
              },
              failures: [
                { destination: 'archive', label: 'generations', detail: 'HTTP 429', at: '2026-08-22T11:59:00Z' },
              ],
            },
          ],
        }),
      ),
    );
    expectNoDestinationDetails();
    expect(screen.queryByText(/last delivered|last failure/)).toBeNull();
    expect(screen.queryByText('2026-08-22T11:58:00Z')).toBeNull();
    expect(screen.queryByText('2026-08-22T11:59:00Z')).toBeNull();
    expect(screen.getByRole('button', { name: /Cloud forwarding: Partial failure/ })).toBeTruthy();
    expect(screen.getByText(/archive \/ generations: HTTP 429/)).toBeTruthy();
    expect(screen.getByText(/Some deliveries succeeded, but other deliveries failed or were dropped/)).toBeTruthy();
  });

  it('does not hide partial failure when only cumulative counters remain', () => {
    setup(
      config(
        { localForward: true },
        status({
          enabled: true,
          mode: 'metadata_only',
          destinations: [{ ...extra, legs: { generations: { attempted: 8, succeeded: 7, failed: 1, dropped: 2 } } }],
        }),
      ),
    );
    expectNoDestinationDetails();
    expect(screen.getByRole('button', { name: /Cloud forwarding: Partial failure/ })).toBeTruthy();
    expect(screen.getByText(/Some deliveries succeeded, but other deliveries failed or were dropped/)).toBeTruthy();
  });

  it('keeps the overall off state without showing historical destination counters', () => {
    setup(
      config(
        {},
        status({
          destinations: [{ ...extra, legs: { generations: { attempted: 5, succeeded: 4, failed: 1, dropped: 0 } } }],
        }),
      ),
    );
    expectNoDestinationDetails();
    expect(screen.getByRole('button', { name: /Mode: Local/ })).toBeTruthy();
    expect(screen.getByText(/Cloud forwarding is off. Nothing from local sessions leaves this machine/)).toBeTruthy();
    expect(screen.queryByText(/Forwarding is on/)).toBeNull();
  });

  it('keeps the primary and extra failures visible when one recipient succeeds', () => {
    setup(
      config(
        { endpoint: primary.endpoint, localForward: true },
        status({
          enabled: true,
          mode: 'metadata_only',
          generations: true,
          destinations: [
            { ...primary, legs: { generations: { attempted: 1, succeeded: 1, failed: 0, dropped: 0 } } },
            { ...extra, legs: { generations: { attempted: 1, succeeded: 0, failed: 1, dropped: 0 } } },
          ],
          failures: [
            { destination: 'primary', at: '', label: 'otlp/traces', detail: 'trace refused' },
            { destination: 'archive', at: '', label: 'generations', detail: 'generation refused' },
          ],
        }),
      ),
    );
    expect(
      screen.getByText(/primary \/ otlp\/traces: trace refused.*archive \/ generations: generation refused/),
    ).toBeTruthy();
    expect(screen.getByRole('button', { name: /Partial failure/ })).toBeTruthy();
  });

  it('requires consent naming all recipients before enabling full content', async () => {
    const { writes } = setup(config({ endpoint: primary.endpoint }, status({ destinations: [primary, extra] })));
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    expect(writes).toHaveLength(0);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    for (const destination of [primary, extra]) {
      expect(within(list).getByText(destination.name)).toBeTruthy();
      expect(within(list).getByText(destination.endpoint)).toBeTruthy();
      expect(within(list).getByText(destination.otlpEndpoint)).toBeTruthy();
    }
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(writes).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.capture).toBe('full');
    expect(writes[0]?.localForward).toBe(true);
  });

  it('names only extra recipients for extra-only full-content consent', () => {
    const { writes } = setup();
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' })[1] as HTMLElement);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('archive')).toBeTruthy();
    expect(within(list).queryByText('primary')).toBeNull();
    expect(writes).toHaveLength(0);
  });

  it('uses the saved primary, not a pending endpoint, for the one-click consent', () => {
    setup(config({ endpoint: primary.endpoint }, status({ destinations: [primary, extra] })));
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByDisplayValue(primary.endpoint), { target: { value: 'https://pending.example' } });
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(primary.endpoint)).toBeTruthy();
    expect(within(list).queryByText('https://pending.example')).toBeNull();
  });

  it.each(['before opening', 'while open'])('blocks full consent after polling fails %s', async (timing) => {
    const initial = config({ endpoint: primary.endpoint }, status({ destinations: [primary, extra] }));
    const { failPoll, recover, writes } = setup(initial);
    if (timing === 'before opening') failPoll();
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    if (timing === 'while open') failPoll();
    expect(screen.getByRole('button', { name: /Mode: Unknown/ })).toBeTruthy();
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
    const replacement = { ...extra, name: 'replacement', endpoint: 'https://replacement.example/ingest' };
    recover({ ...initial, forwardStatus: status({ destinations: [primary, replacement] }) });
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('replacement')).toBeTruthy();
    expect(within(list).queryByText('archive')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ capture: 'full', localForward: true });
  });

  it.each([
    'before opening',
    'while open',
  ])('blocks manual Save full consent after polling fails %s', async (timing) => {
    const initial = config(
      { endpoint: primary.endpoint, capture: 'full', localForward: true },
      status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
    );
    const { writes, failPoll, recover } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByDisplayValue(primary.endpoint), { target: { value: 'https://new.example/ingest' } });
    if (timing === 'before opening') failPoll();
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    if (timing === 'while open') failPoll();
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
    const replacement = { ...extra, name: 'replacement', endpoint: 'https://replacement.example/ingest' };
    recover({ ...initial, forwardStatus: status({ destinations: [primary, replacement] }) });
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('https://new.example/ingest')).toBeTruthy();
    expect(within(list).getByText(primary.otlpEndpoint)).toBeTruthy();
    expect(within(list).getByText(replacement.endpoint)).toBeTruthy();
    expect(within(list).queryByText('archive')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ endpoint: 'https://new.example/ingest', capture: 'full', localForward: true });
  });

  it.each(['before opening', 'while open'])('blocks full-content Connect after polling fails %s', async (timing) => {
    const { failPoll, recover, writes } = setup();
    fireEvent.change(screen.getByPlaceholderText(/^AGENTO11Y_ENDPOINT=/), {
      target: {
        value: [
          `AGENTO11Y_ENDPOINT=${primary.endpoint}`,
          'AGENTO11Y_AUTH_TENANT_ID=synthetic-tenant',
          'AGENTO11Y_AUTH_TOKEN=synthetic-token',
        ].join('\n'),
      },
    });
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' })[0] as HTMLElement);
    if (timing === 'before opening') failPoll();
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    if (timing === 'while open') failPoll();
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
    const replacement = { ...extra, name: 'replacement', endpoint: 'https://replacement.example/ingest' };
    recover(config({}, status({ destinations: [replacement] })));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(primary.endpoint)).toBeTruthy();
    expect(within(list).getByText(replacement.endpoint)).toBeTruthy();
    expect(within(list).queryByText('archive')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ endpoint: primary.endpoint, capture: 'full', localForward: true });
  });

  it.each(['Metadata only', 'Local only'])('keeps narrowing to %s available after a failed poll', async (mode) => {
    const { failPoll, writes } = setup(
      config(
        { endpoint: primary.endpoint, capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    failPoll();
    expect(screen.getByRole('button', { name: /Mode: Unknown/ })).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: mode }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject(
      mode === 'Local only'
        ? { localForward: false, capture: 'full' }
        : { localForward: true, capture: 'metadata_only' },
    );
  });

  it('keeps extra-only narrowing available after a failed poll', async () => {
    const { failPoll, writes } = setup(
      config({ capture: 'full', localForward: true }, status({ enabled: true, mode: 'full' })),
    );
    failPoll();
    fireEvent.click(screen.getByRole('button', { name: 'Local only' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ localForward: false, capture: 'full' });
  });

  it('blocks a full-mode Save without a consent modal during an outage', async () => {
    const initial = config(
      { endpoint: primary.endpoint, capture: 'full', localForward: true },
      status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
    );
    const { failPoll, recover, writes } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: /^Local Tags,/ }));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    failPoll();
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(screen.getByText(/Recipient status is unavailable/)).toBeTruthy();
    expect(writes).toHaveLength(0);
    recover(initial);
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ capture: 'full', localForward: true, debug: true });
  });

  it.each([
    null,
    status({ destinations: undefined }),
  ])('blocks full consent with missing recipient status %#', (forwarding) => {
    const initial = { ...config({ endpoint: primary.endpoint }), forwardStatus: forwarding };
    const { writes } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
  });

  it.each([
    'token',
    'tenant',
    'reset',
  ] as const)('blocks Full Save when a %s write can activate an undisclosed inherited HTTP route', async (credential) => {
    const initial = config(
      { tenantId: 'saved-tenant', tokenSet: credential === 'reset', capture: 'full', localForward: true },
      status({ enabled: true, mode: 'full', destinations: [extra] }),
    );
    const { writes } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    if (credential === 'token') {
      fireEvent.change(screen.getByPlaceholderText('glc_…'), { target: { value: 'synthetic-token' } });
    } else if (credential === 'tenant') {
      fireEvent.change(screen.getByDisplayValue('saved-tenant'), { target: { value: 'new-tenant' } });
    } else {
      fireEvent.click(screen.getByRole('button', { name: 'Reset' }));
    }
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    expect(
      screen.getByText(/Changing primary credentials may activate an inherited HTTP endpoint not shown here/),
    ).toBeTruthy();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
    fireEvent.change(screen.getByPlaceholderText('https://agento11y-prod-….grafana.net'), {
      target: { value: primary.endpoint },
    });
    expect(
      within(screen.getByRole('list', { name: 'Full-content recipients' })).getByText(primary.endpoint),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ endpoint: primary.endpoint, capture: 'full', localForward: true });
  });

  it.each([
    'before opening',
    'while open',
  ])('blocks Full restoring saved credentials over a changed runtime tenant %s', (timing) => {
    const initial = config(
      { tenantId: 'saved-tenant', capture: 'metadata_only', localForward: true },
      status({ enabled: true, mode: 'metadata_only', destinations: [extra] }),
    );
    const { writes, poll } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: /^Local Tags,/ }));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(screen.getByRole('button', { name: /^Cloud Ingest,/ }));
    if (timing === 'while open') fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    poll({ ...initial, settings: { ...initial.settings, tenantId: 'external-tenant' } });
    if (timing === 'before opening') fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
  });

  it.each([
    'known runtime route',
    'explicit endpoint',
  ])('allows a credential write when consent names the %s', async (source) => {
    const { writes } = setup(
      config(
        {
          endpoint: source === 'explicit endpoint' ? primary.endpoint : '',
          tenantId: 'saved-tenant',
          capture: 'full',
          localForward: true,
        },
        status({
          enabled: true,
          mode: 'full',
          destinations: source === 'known runtime route' ? [primary, extra] : [extra],
        }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByPlaceholderText('glc_…'), { target: { value: 'synthetic-token' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(
      within(screen.getByRole('list', { name: 'Full-content recipients' })).getByText(primary.endpoint),
    ).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.token).toBe('synthetic-token');
  });

  it.each([
    'metadata_only',
    'off',
  ])('allows credential changes under %s, then discloses the activated route after refresh', async (mode) => {
    const { writes, poll } = setup(
      config(
        { tenantId: 'saved-tenant', capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [extra] }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByPlaceholderText('glc_…'), { target: { value: 'synthetic-token' } });
    fireEvent.click(
      screen.getAllByRole('button', { name: mode === 'off' ? 'Local only' : 'Metadata only' }).at(-1) as HTMLElement,
    );
    await waitFor(() => expect(writes).toHaveLength(1));
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[1]).toMatchObject({ token: 'synthetic-token', localForward: mode !== 'off' });
    poll(config({ ...writes[1], token: '', tokenSet: true }, status({ destinations: [primary, extra] })));
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' }).at(-1) as HTMLElement);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(primary.endpoint)).toBeTruthy();
    expect(within(list).getByText(extra.endpoint)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(3));
  });

  it('allows full consent with a freshly reported empty recipient list', async () => {
    const { writes } = setup(config({ endpoint: primary.endpoint }, status({ destinations: [] })));
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    expect(screen.getByRole('list', { name: 'Full-content recipients' }).textContent).toContain(primary.endpoint);
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
  });

  it.each(
    (['endpoint', 'otlpEndpoint'] as const).flatMap((field) =>
      ['before opening', 'while open'].map((timing) => ({ field, timing })),
    ),
  )('blocks manual Full Save clearing managed $field $timing', async ({ field, timing }) => {
    const { writes } = setup(
      config(
        { endpoint: primary.endpoint, otlpEndpoint: primary.otlpEndpoint, capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    const input = screen.getByDisplayValue(primary[field]);
    if (timing === 'while open') {
      fireEvent.change(input, { target: { value: 'https://replacement.example' } });
      fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
      expect(screen.getByRole('list', { name: 'Full-content recipients' })).toBeTruthy();
    }
    fireEvent.change(input, { target: { value: field === 'endpoint' ? '' : '   ' } });
    if (timing === 'before opening') fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(screen.getByText(/Clearing a managed primary endpoint may activate an undisclosed fallback/)).toBeTruthy();
    expect(screen.getByText(/Click Save to config.env to save the endpoint changes/)).toBeTruthy();
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Forward full content' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    expect(writes).toHaveLength(0);
    fireEvent.change(input, { target: { value: 'https://replacement.example' } });
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('https://replacement.example')).toBeTruthy();
    expect(within(list).queryByText(primary[field])).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ [field]: 'https://replacement.example', capture: 'full' });
  });

  it.each(['before opening', 'while open'])('blocks Connect Full clearing saved OTLP %s', async (timing) => {
    const initial = config(
      timing === 'before opening' ? { otlpEndpoint: primary.otlpEndpoint } : {},
      status({ destinations: [primary, extra] }),
    );
    const { writes, poll } = setup(initial);
    const block = [
      'AGENTO11Y_ENDPOINT=https://new.example/ingest',
      'AGENTO11Y_AUTH_TENANT_ID=synthetic-tenant',
      'AGENTO11Y_AUTH_TOKEN=synthetic-token',
    ].join('\n');
    const paste = screen.getByPlaceholderText(/^AGENTO11Y_ENDPOINT=/);
    fireEvent.change(paste, { target: { value: block } });
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' })[0] as HTMLElement);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    if (timing === 'while open') {
      expect(screen.getByRole('list', { name: 'Full-content recipients' })).toBeTruthy();
      poll({ ...initial, settings: settings({ otlpEndpoint: primary.otlpEndpoint }) });
    }
    expect(screen.queryByRole('list', { name: 'Full-content recipients' })).toBeNull();
    expect(screen.getByRole('button', { name: 'Forward full content' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    expect(writes).toHaveLength(0);
    fireEvent.change(paste, { target: { value: `${block}\nOTEL_EXPORTER_OTLP_ENDPOINT=https://new.example/otlp` } });
    expect(screen.getByRole('list', { name: 'Full-content recipients' }).textContent).toContain(
      'https://new.example/otlp',
    );
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ otlpEndpoint: 'https://new.example/otlp', capture: 'full' });
  });

  it.each(['endpoint', 'otlpEndpoint'] as const)('blocks Save clearing the latest managed %s', (field) => {
    const initial = config(
      { capture: 'full', localForward: true },
      status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
    );
    const { writes, poll } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: /^Local Tags,/ }));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    poll({ ...initial, settings: { ...initial.settings, [field]: primary[field] } });
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
    expect(screen.getByText(/Clearing a managed primary endpoint may activate an undisclosed fallback/)).toBeTruthy();
  });

  it.each(
    (['endpoint', 'otlpEndpoint'] as const).flatMap((field) =>
      ['metadata_only', 'off'].map((mode) => ({ field, mode })),
    ),
  )('allows manual narrowing to $mode while clearing $field, then Full after refresh', async ({ field, mode }) => {
    const { writes, poll } = setup(
      config(
        { endpoint: primary.endpoint, otlpEndpoint: primary.otlpEndpoint, capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByDisplayValue(primary[field]), { target: { value: '' } });
    fireEvent.click(screen.getByRole('button', { name: mode === 'off' ? 'Local only' : 'Metadata only' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.[field]).toBe(primary[field]);
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    await waitFor(() => expect(writes).toHaveLength(2));
    expect(writes[1]).toMatchObject({
      [field]: '',
      localForward: mode !== 'off',
      capture: mode === 'off' ? 'full' : mode,
    });
    expect(screen.queryByRole('button', { name: 'Forward full content' })).toBeNull();
    const fallback = `https://previously-shadowed.example/${field}`;
    poll(config(writes[1], status({ destinations: [{ ...primary, [field]: fallback }, extra] })));
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' }).at(-1) as HTMLElement);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(fallback)).toBeTruthy();
    expect(within(list).queryByText(primary[field])).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(3));
    expect(writes[2]).toMatchObject({ [field]: '', localForward: true, capture: 'full' });
  });

  it('retains an effective primary OTLP route when connecting without a managed OTLP endpoint', async () => {
    const { writes } = setup(config({}, status({ destinations: [{ ...primary, endpoint: '' }, extra] })));
    fireEvent.change(screen.getByPlaceholderText(/^AGENTO11Y_ENDPOINT=/), {
      target: {
        value: [
          'AGENTO11Y_ENDPOINT=https://new.example/ingest',
          'AGENTO11Y_AUTH_TENANT_ID=synthetic-tenant',
          'AGENTO11Y_AUTH_TOKEN=synthetic-token',
        ].join('\n'),
      },
    });
    fireEvent.click(screen.getAllByRole('button', { name: 'Full' })[0] as HTMLElement);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('https://new.example/ingest')).toBeTruthy();
    expect(within(list).getByText(primary.otlpEndpoint)).toBeTruthy();
    expect(within(list).getByText(extra.endpoint)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ endpoint: 'https://new.example/ingest', otlpEndpoint: '', capture: 'full' });
  });

  it.each(
    ['endpoint', 'debug', 'clean'].flatMap((edit) =>
      ['before opening', 'while open'].map((timing) => ({ edit, timing })),
    ),
  )('names the prospective primary after an external change $timing with a $edit edit', async ({ edit, timing }) => {
    const initial = config(
      { endpoint: primary.endpoint, otlpEndpoint: primary.otlpEndpoint },
      status({ destinations: [primary, extra] }),
    );
    const { poll, writes } = setup(initial);
    if (edit === 'endpoint') {
      fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
      fireEvent.change(screen.getByDisplayValue(primary.endpoint), { target: { value: 'https://pending.example' } });
    } else if (edit === 'debug') {
      fireEvent.click(screen.getByRole('button', { name: /^Local Tags,/ }));
      fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
      fireEvent.click(screen.getByRole('button', { name: /^Cloud Ingest,/ }));
    }
    if (timing === 'while open') fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    const external = {
      ...primary,
      endpoint: 'https://external.example/ingest',
      otlpEndpoint: 'https://external.example/otlp',
    };
    poll(
      config(
        { endpoint: external.endpoint, otlpEndpoint: external.otlpEndpoint },
        status({ destinations: [external, extra] }),
      ),
    );
    if (timing === 'before opening') fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    const expected = edit === 'clean' ? external : primary;
    expect(within(list).getByText(expected.endpoint)).toBeTruthy();
    expect(within(list).getByText(expected.otlpEndpoint)).toBeTruthy();
    expect(within(list).getByText(extra.endpoint)).toBeTruthy();
    expect(within(list).queryByText(edit === 'clean' ? primary.endpoint : external.endpoint)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      endpoint: expected.endpoint,
      otlpEndpoint: expected.otlpEndpoint,
      capture: 'full',
      debug: false,
    });
    if (edit !== 'clean') expect(screen.getByRole('button', { name: 'Save to config.env' })).toBeTruthy();
  });

  it.each([
    'endpoint',
    'otlpEndpoint',
  ] as const)('confirms the primary restored by manual Save after an external %s change', async (field) => {
    const initial = config(
      { endpoint: primary.endpoint, otlpEndpoint: primary.otlpEndpoint, capture: 'full', localForward: true },
      status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
    );
    const { writes, poll } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: /^Local Tags,/ }));
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    const external = { ...primary, [field]: `https://external.example/${field}` };
    poll({
      ...initial,
      settings: { ...initial.settings, [field]: external[field] },
      forwardStatus: status({ enabled: true, mode: 'full', destinations: [external, extra] }),
    });
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(writes).toHaveLength(0);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(primary.endpoint)).toBeTruthy();
    expect(within(list).getByText(primary.otlpEndpoint)).toBeTruthy();
    expect(within(list).getByText(extra.endpoint)).toBeTruthy();
    expect(within(list).queryByText(external[field])).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      endpoint: primary.endpoint,
      otlpEndpoint: primary.otlpEndpoint,
      capture: 'full',
      debug: true,
    });
  });

  it.each([
    'endpoint',
    'otlpEndpoint',
  ] as const)('preserves the effective unmanaged primary route when editing %s', async (field) => {
    const other = field === 'endpoint' ? 'otlpEndpoint' : 'endpoint';
    const next = `https://new.example/${field}`;
    const { writes } = setup(
      config(
        { [field]: primary[field], capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    fireEvent.click(
      screen.getByRole('button', {
        name: field === 'endpoint' ? 'Edit connection' : 'Enter the connection fields by hand',
      }),
    );
    fireEvent.change(screen.getByDisplayValue(primary[field]), { target: { value: next } });
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(next)).toBeTruthy();
    expect(within(list).getByText(primary[other])).toBeTruthy();
    expect(within(list).queryByText(primary[field])).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({ [field]: next, [other]: '', capture: 'full' });
  });

  it('turns global forwarding off on disconnect without claiming extra credentials are erased', async () => {
    const { writes } = setup(
      config(
        { endpoint: primary.endpoint, localForward: true, capture: 'full', tokenSet: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.click(screen.getByRole('button', { name: 'Disconnect' }));
    expect(
      screen.getByText(
        /Extra destinations and their credentials remain in forwarding.toml. Turning forwarding on again/,
      ),
    ).toBeTruthy();
    expect(screen.getByText(/Unmanaged or inherited primary settings/)).toBeTruthy();
    expect(writes).toHaveLength(0);
    fireEvent.click(screen.getAllByRole('button', { name: 'Disconnect' })[1] as HTMLElement);
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      localForward: false,
      endpoint: '',
      tokenCleared: true,
      otlpHeadersCleared: true,
      capture: 'full',
    });
    expectNoDestinationDetails();
    expect(screen.getByRole('button', { name: /Mode: Local/ })).toBeTruthy();
    expect(screen.queryByText('Disconnected. Credentials cleared from config.env.')).toBeNull();
  });

  it('confirms a pending primary endpoint change before saving full capture', async () => {
    const { writes } = setup(
      config(
        { endpoint: primary.endpoint, capture: 'full', localForward: true },
        status({ enabled: true, mode: 'full', destinations: [primary, extra] }),
      ),
    );
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByDisplayValue(primary.endpoint), { target: { value: 'https://new.example/ingest' } });
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    expect(writes).toHaveLength(0);
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText('https://new.example/ingest')).toBeTruthy();
    expect(within(list).getByText(extra.endpoint)).toBeTruthy();
    expect(within(list).queryByText(primary.endpoint)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.endpoint).toBe('https://new.example/ingest');
  });

  it('adopts polled recipients and file diagnostics without discarding a pending primary edit', () => {
    const initial = config(
      { endpoint: primary.endpoint, localForward: true },
      status({ enabled: true, mode: 'metadata_only', destinations: [primary, extra] }),
    );
    const { poll, writes } = setup(initial);
    fireEvent.click(screen.getByRole('button', { name: 'Edit connection' }));
    fireEvent.change(screen.getByDisplayValue(primary.endpoint), { target: { value: 'https://pending.example' } });
    const replacement = {
      ...extra,
      name: 'replacement',
      endpoint: 'https://replacement.example/generations',
      otlpEndpoint: 'https://replacement.example/otlp',
      revision: 8,
      legs: { generations: { attempted: 99, succeeded: 99, failed: 0, dropped: 0 } },
    };
    poll({
      ...initial,
      forwardStatus: status({
        enabled: true,
        mode: 'metadata_only',
        destinations: [replacement],
      }),
    });
    expect(screen.getByDisplayValue('https://pending.example')).toBeTruthy();
    expectNoDestinationDetails();
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(list).getByText(primary.endpoint)).toBeTruthy();
    expect(within(list).getByText(replacement.name)).toBeTruthy();
    expect(within(list).getByText(replacement.endpoint)).toBeTruthy();
    expect(within(list).getByText(replacement.otlpEndpoint)).toBeTruthy();
    expect(within(list).queryByText(extra.name)).toBeNull();
    expect(within(list).queryByText(extra.endpoint)).toBeNull();
    expect(within(list).queryByText(extra.otlpEndpoint)).toBeNull();
    expect(within(list).queryByText('https://pending.example')).toBeNull();
    poll({
      ...initial,
      forwardStatus: status({
        enabled: true,
        mode: 'metadata_only',
        forwardingError: 'invalid replacement file',
        destinations: [primary],
      }),
    });
    expect(screen.getByDisplayValue('https://pending.example')).toBeTruthy();
    expect(screen.getByText(/Extra destinations are not loaded: invalid replacement file/)).toBeTruthy();
    const updatedList = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(within(updatedList).getByText(primary.endpoint)).toBeTruthy();
    expect(within(updatedList).getByText(primary.otlpEndpoint)).toBeTruthy();
    expect(within(updatedList).queryByText(replacement.name)).toBeNull();
    expect(within(updatedList).queryByText(replacement.endpoint)).toBeNull();
    expect(within(updatedList).queryByText(replacement.otlpEndpoint)).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expectNoDestinationDetails();
    expect(writes).toHaveLength(0);
  });

  it('keeps primary guard verdicts separate from a failing extra recipient', () => {
    setup(
      config(
        { endpoint: primary.endpoint, localForward: true },
        status({
          enabled: true,
          mode: 'metadata_only',
          hooks: true,
          hookReason: undefined,
          legs: { hooks: { lastSuccessAt: '2026-08-22T11:58:00Z' } },
          destinations: [
            primary,
            {
              ...extra,
              legs: { generations: { attempted: 1, succeeded: 0, failed: 1, dropped: 0 } },
              failures: [{ destination: 'archive', at: '', label: 'generations', detail: 'delivery failed' }],
            },
          ],
        }),
      ),
    );
    const failClosed = screen.getByRole('button', { name: 'Fail closed' });
    expect(failClosed.hasAttribute('disabled')).toBe(false);
    expect(failClosed.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByText(/Cloud returned a verdict/)).toBeTruthy();
    expect(screen.getByText(/Guards are on: tool calls/)).toBeTruthy();
    expect(screen.getByRole('button', { name: /Partial failure/ })).toBeTruthy();
  });

  it('keeps extra-only Cloud guards disabled while forwarding is active', () => {
    setup(config({ localForward: true }, status({ enabled: true, mode: 'metadata_only', generations: true })));
    expect(screen.getByRole('button', { name: 'Fail open' }).hasAttribute('disabled')).toBe(true);
    expect(screen.getByText(/Extra destinations do not evaluate guards/)).toBeTruthy();
    expect(screen.getByText('guard forwarding needs usable primary credentials')).toBeTruthy();
  });
});
