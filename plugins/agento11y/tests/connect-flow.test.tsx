import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import {
  type HistoryImport,
  SettingsConnectFlow,
  SettingsSecurityView,
} from '../internal/local/web/src/settings-screen';
import type { ConfigResponse, Settings } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.unstubAllGlobals();
});

// SettingsConnectFlow replaces the credential form when nothing is saved. It
// replicates the `agento11y login` handshake: open the setup page on your
// stack, paste the block it hands back, pick what to forward.

const API_BLOCK = [
  'AGENTO11Y_ENDPOINT=https://agento11y-prod-eu.grafana.net',
  'AGENTO11Y_AUTH_TENANT_ID=123456',
  'AGENTO11Y_AUTH_TOKEN=glc_token',
].join('\n');
const OTLP_ENDPOINT = 'https://otlp-gateway.grafana.net/otlp';
const GOOD_BLOCK = `${API_BLOCK}\nOTEL_EXPORTER_OTLP_ENDPOINT=${OTLP_ENDPOINT}`;
const MALFORMED_OTLP_ENDPOINTS = [
  'grpc://collector.example:4317',
  'https:///collector.example',
  'https://%63ollector.example',
  'https://user:secret@collector.example',
  'https://collector.example?key=secret',
  'https://collector.example?',
  'https://collector.example#traces',
  'https://collector.example:0',
  'https://collector.example:65536',
  'https://collector.example:abc',
  'https://collector.example/otlp%zz',
  'https://collector.example\\@other.example',
  'https://collect{or.example/otlp',
  'https://collect}or.example/otlp',
  'https://collect`or.example/otlp',
  'https://collect^or.example/otlp',
  'https://collect|or.example/otlp',
];

function renderFlow(props: Partial<React.ComponentProps<typeof SettingsConnectFlow>> = {}) {
  const onConnect = vi.fn();
  const onManual = vi.fn();
  let current = {
    generationProtocol: 'otel',
    generationProtocolError: '',
    configError: null,
    savedStackURL: '',
    configPath: '/home/x/.agento11y/config.env',
    capture: '',
    destinations: [],
    onConnect,
    onManual,
    ...props,
  } satisfies React.ComponentProps<typeof SettingsConnectFlow>;
  const view = render(<SettingsConnectFlow {...current} />);
  return {
    onConnect,
    onManual,
    rerenderFlow: (patch: Partial<React.ComponentProps<typeof SettingsConnectFlow>>) => {
      current = { ...current, ...patch };
      view.rerender(<SettingsConnectFlow {...current} />);
    },
  };
}

function paste(text: string) {
  fireEvent.change(screen.getByPlaceholderText(/^AGENTO11Y_ENDPOINT=/), { target: { value: text } });
}

describe('SettingsConnectFlow', () => {
  it('builds the setup link from a stack URL typed with no scheme', () => {
    renderFlow();
    // With no stack typed the anchor carries no href at all, which is why it is
    // found by its text rather than by the link role.
    const setupLink = () => screen.getByText('Open setup page').closest('a');
    expect(setupLink()?.getAttribute('href')).toBeNull();
    expect(setupLink()?.getAttribute('aria-disabled')).toBe('true');

    fireEvent.change(screen.getByPlaceholderText('https://your-stack.grafana.net'), {
      target: { value: 'mystack.grafana.net' },
    });
    expect(setupLink()?.getAttribute('href')).toBe(
      'https://mystack.grafana.net/a/grafana-agento11y-app/setup-coding-agent',
    );
  });

  it('starts with Connect disabled and enables it once a complete block is pasted', () => {
    const { onConnect } = renderFlow();
    const connect = screen.getByRole('button', { name: 'Connect' });
    expect(connect.hasAttribute('disabled')).toBe(true);

    paste(GOOD_BLOCK);
    expect(screen.getByText('Connection settings read')).toBeTruthy();
    expect(screen.getByText(/tenant 123456 · token found/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(false);

    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).toHaveBeenCalledTimes(1);
    const [parsed, mode] = onConnect.mock.calls[0] ?? [];
    expect(parsed?.token).toBe('glc_token');
    expect(mode).toBe('metadata_only');
  });

  it('requires OTLP for the default OTel transport even with complete API credentials', () => {
    const { onConnect } = renderFlow();
    paste(API_BLOCK);
    expect(screen.getByText(/OTel requires a valid HTTP\(S\) OTLP base endpoint/)).toBeTruthy();
    expect(screen.getByText(/set AGENTO11Y_PROTOCOL=http in config.env/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).not.toHaveBeenCalled();
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
  });

  it.each([
    'http://collector.example:4318',
    'https://localhost.attacker.example/otlp',
    'https://[2001:db8::1]:4318/otlp',
    'https://[::ffff:192.0.2.1]:443/otlp',
    "https://collect_or~!$&'()*+,;=.example/otlp",
    'https://cöllector.example/otlp',
    'https://collector.example/otlp{tenant}`',
  ])('accepts usable non-loopback OTLP destination %s', (endpoint) => {
    const { onConnect } = renderFlow();
    paste(`${API_BLOCK}\nOTEL_EXPORTER_OTLP_ENDPOINT=${endpoint}`);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).toHaveBeenCalledTimes(1);
    expect(onConnect.mock.calls[0]?.[0].otlpEndpoint).toBe(endpoint);
  });

  it('accepts API-only credentials only after explicit HTTP status arrives', () => {
    const { onConnect, rerenderFlow } = renderFlow();
    paste(`${API_BLOCK}\nAGENTO11Y_PROTOCOL=http`);
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    rerenderFlow({ generationProtocol: 'http' });
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).toHaveBeenCalledTimes(1);
    expect(onConnect.mock.calls[0]?.[0].otlpEndpoint).toBe('');
  });

  it.each([
    ['unknown', 'invalid', 'unsupported protocol', null],
    ['absent', undefined, '', null],
    ['empty', '', '', null],
    ['missing error status', 'otel', undefined, null],
    ['read error', '', 'cannot read config', null],
    ['failed polling', 'otel', '', 'HTTP 500'],
  ])('blocks Connect with %s status', (_name, generationProtocol, generationProtocolError, configError) => {
    const { onConnect } = renderFlow({ generationProtocol, generationProtocolError, configError });
    paste(GOOD_BLOCK);
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    expect(screen.getByText(/Run agento11y doctor/)).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).not.toHaveBeenCalled();
  });

  it.each([
    'http://localhost:4318/otlp',
    'http://LOCALHOST/otlp',
    'http://127.0.0.1:9999',
    'http://[::1]:4318',
    'http://[0:0:0:0:0:0:0:1]:4318',
    'http://[::ffff:127.0.0.1]:4318',
    'https://localhost:4318',
  ])('rejects a forwarding loop at %s', (endpoint) => {
    const { onConnect } = renderFlow();
    paste(`${API_BLOCK}\nOTEL_EXPORTER_OTLP_ENDPOINT=${endpoint}`);
    expect(screen.getByText(/could cause a forwarding loop/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).not.toHaveBeenCalled();
  });

  it.each(MALFORMED_OTLP_ENDPOINTS)('blocks malformed OTLP endpoint %s', (endpoint) => {
    const { onConnect } = renderFlow();
    paste(`${API_BLOCK}\nOTEL_EXPORTER_OTLP_ENDPOINT=${endpoint}`);
    expect(screen.getByText(/OTel requires a valid HTTP\(S\) OTLP base endpoint/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).not.toHaveBeenCalled();
  });

  it.each([
    { generationProtocol: 'invalid', generationProtocolError: 'unsupported' },
    { generationProtocol: undefined },
    { configError: 'poll failed' },
    { generationProtocol: 'otel' },
  ])('rechecks full-content confirmation after status changes: %j', (patch) => {
    const { onConnect, rerenderFlow } = renderFlow({ generationProtocol: 'http' });
    paste(API_BLOCK);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    rerenderFlow(patch);
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(onConnect).not.toHaveBeenCalled();
    rerenderFlow({ generationProtocol: 'http', generationProtocolError: '', configError: null });
    fireEvent.click(screen.getByRole('button', { name: 'Forward full content' }));
    expect(onConnect).toHaveBeenCalledTimes(1);
  });

  it('rechecks pasted destination changes during full-content confirmation', () => {
    const { onConnect } = renderFlow();
    paste(GOOD_BLOCK);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    paste(API_BLOCK);
    const confirm = screen.getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(onConnect).not.toHaveBeenCalled();
  });

  it('says a placeholder is a placeholder, not a missing key', () => {
    renderFlow();
    paste(GOOD_BLOCK.replace('glc_token', '<your token>'));
    expect(screen.getByText("Couldn't read a complete block")).toBeTruthy();
    expect(screen.getByText(/AGENTO11Y_AUTH_TOKEN is still a placeholder/)).toBeTruthy();
  });

  it('names a URL slot that does not hold a URL', () => {
    renderFlow();
    paste(GOOD_BLOCK.replace('https://agento11y-prod-eu.grafana.net', 'not-a-url'));
    expect(screen.getByText(/AGENTO11Y_ENDPOINT requires a valid HTTP\(S\) URL/)).toBeTruthy();
  });

  it('names the keys a partial block is missing', () => {
    renderFlow();
    paste('AGENTO11Y_ENDPOINT=https://agento11y-prod-eu.grafana.net');
    expect(screen.getByText(/Missing AGENTO11Y_AUTH_TENANT_ID, AGENTO11Y_AUTH_TOKEN/)).toBeTruthy();
  });

  // Full forwarding asks first here too. Reaching it from a fresh install
  // takes two clicks otherwise, while the same widening in the connected panel
  // is confirmed.
  it('confirms before connecting in full-content mode', () => {
    const { onConnect } = renderFlow();
    paste(GOOD_BLOCK);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(onConnect).not.toHaveBeenCalled();
    expect(screen.getByRole('list', { name: 'Full-content recipients' }).textContent).toContain(
      'agento11y-prod-eu.grafana.net',
    );

    fireEvent.click(screen.getByRole('button', { name: /Forward full content/ }));
    expect(onConnect).toHaveBeenCalledTimes(1);
    expect(onConnect.mock.calls[0]?.[1]).toBe('full');
  });

  it('names the new primary and saved extras before full-content connect', () => {
    const { onConnect } = renderFlow({
      destinations: [
        {
          name: 'backup',
          endpoint: 'https://backup.example/ingest',
          otlpEndpoint: 'https://backup.example/otlp',
          httpGenerations: true,
          otlp: true,
          revision: 1,
        },
      ],
    });
    paste(GOOD_BLOCK);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    const list = screen.getByRole('list', { name: 'Full-content recipients' });
    expect(list.textContent).toContain('primary');
    expect(list.textContent).toContain('https://agento11y-prod-eu.grafana.net');
    expect(list.textContent).toContain('backup');
    expect(list.textContent).toContain('https://backup.example/ingest');
    expect(list.textContent).toContain('https://backup.example/otlp');
    expect(onConnect).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(onConnect).not.toHaveBeenCalled();
  });

  // An endpoint on its own is a valid configuration (a local collector needs
  // no tenant or token), and Connect cannot produce one.
  it('keeps the hand-written fields reachable', () => {
    const { onManual } = renderFlow();
    fireEvent.click(screen.getByRole('button', { name: 'Enter the connection fields by hand' }));
    expect(onManual).toHaveBeenCalledTimes(1);
  });

  it('warns that an advanced capture mode is what forwards as metadata', () => {
    renderFlow({ capture: 'no_tool_content' });
    expect(screen.getByText(/Advanced capture mode/)).toBeTruthy();
    expect(screen.getByText('no_tool_content')).toBeTruthy();
  });
});

function config(overrides: Partial<ConfigResponse> = {}): ConfigResponse {
  const settings: Settings = {
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
    guards: 'off',
    guardTimeout: '',
    debug: false,
    autoUpdate: true,
    userId: '',
    localForward: false,
  };
  return {
    settings,
    path: '/tmp/config.env',
    preview: '',
    stackUrl: '',
    generationProtocol: 'otel',
    generationProtocolError: '',
    forwardStatus: { enabled: false, mode: 'off', generations: false, otlp: false, hooks: false, destinations: [] },
    ...overrides,
  };
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

function renderSettings(initial = config()) {
  const writes: Settings[] = [];
  const fetcher = vi.fn(async (input: string, init?: RequestInit) => {
    if (input === '/api/v1/config:preview') return new Response(JSON.stringify({ preview: '' }));
    if (input === '/api/v1/config' && init?.method === 'PUT') {
      const next = JSON.parse(String(init.body)).settings as Settings;
      writes.push(next);
      return new Response(JSON.stringify({ ...initial, settings: next }));
    }
    throw new Error(`Unexpected request: ${input}`);
  });
  vi.stubGlobal('fetch', fetcher);
  let props: React.ComponentProps<typeof SettingsSecurityView> = {
    activeSection: 'settings',
    history,
    config: initial,
    configError: null,
    activeSettingsTab: 'cloud',
    onSelectTab: vi.fn(),
    onConfig: vi.fn(),
  };
  const view = render(<SettingsSecurityView {...props} />);
  return {
    writes,
    update: (patch: Partial<typeof props>) => {
      props = { ...props, ...patch };
      view.rerender(<SettingsSecurityView {...props} />);
    },
  };
}

describe('Settings connection readiness prop chain', () => {
  it.each(MALFORMED_OTLP_ENDPOINTS)('does not PUT malformed OTLP endpoint %s', (endpoint) => {
    const { writes } = renderSettings();
    paste(`${API_BLOCK}\nOTEL_EXPORTER_OTLP_ENDPOINT=${endpoint}`);
    expect(screen.getByText(/OTel requires a valid HTTP\(S\) OTLP base endpoint/)).toBeTruthy();
    const connect = screen.getByRole('button', { name: 'Connect' });
    expect(connect.hasAttribute('disabled')).toBe(true);
    fireEvent.click(connect);
    expect(writes).toHaveLength(0);
  });

  it('requires OTLP by default and persists credentials without a protocol setting', async () => {
    const { writes } = renderSettings();
    paste(`${API_BLOCK}\nAGENTO11Y_PROTOCOL=http`);
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    paste(`${GOOD_BLOCK}\nAGENTO11Y_PROTOCOL=http`);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]).toMatchObject({
      endpoint: 'https://agento11y-prod-eu.grafana.net',
      otlpEndpoint: OTLP_ENDPOINT,
      token: 'glc_token',
      localForward: true,
      capture: '',
    });
    expect(writes[0]).not.toHaveProperty('generationProtocol');
    expect(writes[0]).not.toHaveProperty('protocol');
  });

  it('passes current transport changes through every Settings component', async () => {
    const { writes, update } = renderSettings();
    paste(API_BLOCK);
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    update({ config: config({ generationProtocol: 'http' }) });
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.otlpEndpoint).toBe('');
  });

  it.each([
    { generationProtocol: 'unexpected' },
    { generationProtocol: '', generationProtocolError: 'unreadable config' },
    { generationProtocol: undefined, generationProtocolError: undefined },
  ])('blocks unavailable protocol status through the full chain: %j', (status) => {
    const { writes } = renderSettings(config(status));
    paste(GOOD_BLOCK);
    expect(screen.getByText(/Run agento11y doctor/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Connect' }).hasAttribute('disabled')).toBe(true);
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    expect(writes).toHaveLength(0);
  });

  it('blocks a failed poll despite stale valid config and recovers without losing the paste', async () => {
    const stale = config();
    const { writes, update } = renderSettings(stale);
    paste(GOOD_BLOCK);
    update({ config: stale, configError: 'Failed to fetch config' });
    const connect = screen.getByRole('button', { name: 'Connect' });
    expect(connect.hasAttribute('disabled')).toBe(true);
    fireEvent.click(connect);
    expect(writes).toHaveLength(0);
    update({ config: stale, configError: null });
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    await waitFor(() => expect(writes).toHaveLength(1));
  });

  it.each([
    'poll failure',
    'transport change',
    'read error',
    'recipient status change',
  ])('rechecks full-content confirmation after %s through the full chain', (change) => {
    const initial = config({ generationProtocol: 'http' });
    const { writes, update } = renderSettings(initial);
    paste(API_BLOCK);
    fireEvent.click(screen.getByRole('button', { name: 'Full' }));
    fireEvent.click(screen.getByRole('button', { name: 'Connect' }));
    if (change === 'poll failure') update({ configError: 'Failed to fetch config' });
    if (change === 'transport change') update({ config: config() });
    if (change === 'read error') {
      update({ config: config({ generationProtocol: '', generationProtocolError: 'unreadable config' }) });
    }
    if (change === 'recipient status change') {
      update({ config: { ...initial, forwardStatus: null } });
    }
    const confirm = within(screen.getByRole('dialog')).getByRole('button', { name: 'Forward full content' });
    expect(confirm.hasAttribute('disabled')).toBe(true);
    fireEvent.click(confirm);
    expect(writes).toHaveLength(0);
  });

  it('does not block unrelated settings edits when transport status is invalid or polling failed', async () => {
    const { writes, update } = renderSettings(config({ generationProtocol: '', generationProtocolError: 'invalid' }));
    update({ activeSettingsTab: 'local', configError: 'Failed to fetch config' });
    fireEvent.click(screen.getByRole('switch', { name: 'Debug logging' }));
    fireEvent.click(screen.getByRole('button', { name: 'Save to config.env' }));
    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.debug).toBe(true);
    expect(writes[0]?.localForward).toBe(false);
  });
});
