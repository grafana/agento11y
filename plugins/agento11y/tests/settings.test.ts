import { describe, expect, it } from 'vitest';
import {
  cloudConfigured,
  type FormSettings,
  forwardChipMeta,
  pendingEdits,
  sameSettings,
} from '../internal/local/web/src/settings-model';
import {
  forwardLocalPatch,
  looksLikePlaceholder,
  parseConnectBlock,
  setupPageURL,
} from '../internal/local/web/src/settings-screen';
import type { ConfigResponse, ForwardStatus, Settings } from '../internal/local/web/src/types';

// The pure functions behind the Cloud settings panel. parseConnectBlock
// re-implements what validatePastedBlock checks in internal/login/login.go, so
// the two grammars can drift; these cases are the ones the Go side pins in
// login_test.go.

function settings(overrides: Partial<Settings> = {}): Settings {
  return {
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
    guards: '',
    guardTimeout: '',
    debug: false,
    autoUpdate: false,
    userId: '',
    localForward: false,
    ...overrides,
  };
}

function config(saved: Partial<Settings>, forwardStatus: Partial<ForwardStatus> | null): ConfigResponse {
  return {
    settings: settings(saved),
    preview: '',
    path: '',
    stackUrl: '',
    forwardStatus: forwardStatus
      ? { enabled: false, mode: 'off', generations: false, otlp: false, hooks: false, ...forwardStatus }
      : null,
  };
}

const SETUP_PATH = '/a/grafana-agento11y-app/setup-coding-agent';

describe('parseConnectBlock', () => {
  // A whole block, in the shape the setup page hands out: an `export ` prefix,
  // a comment line, a quoted value with a trailing comment, and the two
  // OTEL_EXPORTER_OTLP_ variables under their raw keys.
  it('reads a block copied out of the setup page', () => {
    expect(
      parseConnectBlock(
        [
          '# copied from Grafana',
          'export AGENTO11Y_ENDPOINT=https://agento11y-prod-eu.grafana.net',
          'AGENTO11Y_AUTH_TENANT_ID="123456" # instance id',
          'AGENTO11Y_AUTH_TOKEN=glc_token',
          'OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway.grafana.net/otlp',
          'OTEL_EXPORTER_OTLP_HEADERS=Authorization=Basic c2VjcmV0',
        ].join('\n'),
      ),
    ).toEqual({
      endpoint: 'https://agento11y-prod-eu.grafana.net',
      tenantId: '123456',
      token: 'glc_token',
      otlpEndpoint: 'https://otlp-gateway.grafana.net/otlp',
      otlpHeaders: 'Authorization=Basic c2VjcmV0',
      placeholders: [],
      invalid: [],
    });
  });

  it('drops a trailing comment rather than keeping it in the value', () => {
    expect(parseConnectBlock('AGENTO11Y_ENDPOINT=https://x # from Grafana').endpoint).toBe('https://x');
    expect(parseConnectBlock('AGENTO11Y_ENDPOINT="https://x" # from Grafana').endpoint).toBe('https://x');
  });

  it('prefers AGENTO11Y_ over SIGIL_ whichever order the two appear in', () => {
    for (const lines of [
      ['SIGIL_AUTH_TOKEN=glc_old', 'AGENTO11Y_AUTH_TOKEN=glc_new'],
      ['AGENTO11Y_AUTH_TOKEN=glc_new', 'SIGIL_AUTH_TOKEN=glc_old'],
    ]) {
      expect(parseConnectBlock(lines.join('\n')).token).toBe('glc_new');
    }
    expect(parseConnectBlock('SIGIL_AUTH_TOKEN=glc_old').token).toBe('glc_old');
  });

  // A placeholder is not a value: a block copied before the token existed must
  // not read as complete. The key is reported, so the panel can say what is
  // wrong with it instead of calling it missing.
  it('reports a placeholder as its own kind of problem', () => {
    const parsed = parseConnectBlock(
      ['AGENTO11Y_ENDPOINT=https://x', 'AGENTO11Y_AUTH_TENANT_ID=123456', 'AGENTO11Y_AUTH_TOKEN=<your token>'].join(
        '\n',
      ),
    );
    expect(parsed.token).toBe('');
    expect(parsed.placeholders).toEqual(['AGENTO11Y_AUTH_TOKEN']);
    expect(parsed.invalid).toEqual([]);
    expect(looksLikePlaceholder('<your token>')).toBe(true);
    expect(looksLikePlaceholder('glc_token')).toBe(false);
  });

  // A URL slot that is not an http(s) URL is reported too, the way requireURL
  // rejects the same value in the CLI form.
  it('reports a URL slot that does not hold a URL', () => {
    const parsed = parseConnectBlock(
      [
        'AGENTO11Y_ENDPOINT=not-a-url',
        'AGENTO11Y_AUTH_TENANT_ID=123456',
        'AGENTO11Y_AUTH_TOKEN=glc_token',
        'OTEL_EXPORTER_OTLP_ENDPOINT=otlp-gateway.grafana.net',
      ].join('\n'),
    );
    expect(parsed.endpoint).toBe('');
    expect(parsed.otlpEndpoint).toBe('');
    expect(parsed.invalid).toEqual(['AGENTO11Y_ENDPOINT', 'OTEL_EXPORTER_OTLP_ENDPOINT']);
  });
});

describe('setupPageURL', () => {
  // Only the scheme and host of the typed stack URL survive: a URL copied from
  // a Grafana address bar carries a path and often an ?orgId=N, and the app
  // path replaces whatever path it came with.
  it('keeps the origin and replaces the path', () => {
    expect(setupPageURL('https://mystack.grafana.net/?orgId=1')).toBe(`https://mystack.grafana.net${SETUP_PATH}`);
    expect(setupPageURL('https://mystack.grafana.net/a/other/page')).toBe(`https://mystack.grafana.net${SETUP_PATH}`);
    expect(setupPageURL('https://MyStack.Grafana.net/')).toBe(`https://mystack.grafana.net${SETUP_PATH}`);
    expect(setupPageURL('http://localhost:3000')).toBe(`http://localhost:3000${SETUP_PATH}`);
  });

  // A host typed without a scheme gets https://, so the button works on a value
  // pasted from a browser tab or typed by hand. A scheme already there is kept,
  // which is what keeps javascript: and mailto: out below.
  it('adds https:// to a bare host', () => {
    expect(setupPageURL('mystack.grafana.net')).toBe(`https://mystack.grafana.net${SETUP_PATH}`);
    expect(setupPageURL('mystack.grafana.net/?orgId=1')).toBe(`https://mystack.grafana.net${SETUP_PATH}`);
    expect(setupPageURL('localhost:3000')).toBe(`https://localhost:3000${SETUP_PATH}`);
  });

  it('builds no link from a value that is not a stack', () => {
    for (const bad of ['', '   ', '/settings', './settings', 'my stack', 'javascript:alert(1)', 'mailto:a@b.c']) {
      expect(setupPageURL(bad), `must not build a link from ${JSON.stringify(bad)}`).toBe('');
    }
  });
});

// The mode patch spans two keys, and rewrites the capture mode only when the
// one on disk forwards differently, so an advanced mode survives the switch.
describe('forwardLocalPatch', () => {
  it('spans both keys and leaves an already-matching capture mode alone', () => {
    expect(forwardLocalPatch({ localForward: true, capture: 'full' }, 'off')).toEqual({ localForward: false });
    expect(forwardLocalPatch({ localForward: false, capture: 'no_tool_content' }, 'metadata_only')).toEqual({
      localForward: true,
    });
    expect(forwardLocalPatch({ localForward: false, capture: 'no_tool_content' }, 'full')).toEqual({
      localForward: true,
      capture: 'full',
    });
    expect(forwardLocalPatch({ localForward: true, capture: 'full' }, 'metadata_only')).toEqual({
      localForward: true,
      capture: 'metadata_only',
    });
  });
});

// The chip separates "no connection saved" from "saved, forwarding off", which
// the status alone cannot express: enabled is false for both.
describe('forwardChipMeta', () => {
  const off: Partial<ForwardStatus> = { enabled: false, mode: 'off' };

  it('reads an unavailable status as unknown', () => {
    expect(forwardChipMeta(null).value).toBe('Unknown');
    expect(forwardChipMeta(config({}, null)).value).toBe('Unknown');
  });

  it('tells an unconfigured daemon apart from one with forwarding turned off', () => {
    const local = forwardChipMeta(config({}, off));
    expect(local.value).toBe('Local');
    expect(local.color).toBe('var(--fg2)');

    const savedOff = forwardChipMeta(config({ tokenSet: true }, off));
    expect(savedOff.value).toBe('Local');
    expect(savedOff.color).toBe('var(--success-text)');
  });

  it('names the mode the daemon reports', () => {
    expect(
      forwardChipMeta(config({ tokenSet: true }, { enabled: true, mode: 'metadata_only', generations: true })).value,
    ).toBe('Metadata only');
    expect(forwardChipMeta(config({ tokenSet: true }, { enabled: true, mode: 'full', generations: true })).value).toBe(
      'Full',
    );
    expect(
      forwardChipMeta(
        config(
          { tokenSet: true },
          {
            enabled: true,
            mode: 'full',
            failures: [{ at: '', label: 'generations', detail: 'connection refused' }],
          },
        ),
      ).value,
    ).toBe('Failing');
    expect(forwardChipMeta(config({ tokenSet: true }, { enabled: false, reason: 'no credentials' })).value).toBe(
      'Paused',
    );
  });
});

describe('cloudConfigured', () => {
  it('counts any one of the three as a saved connection', () => {
    expect(cloudConfigured(null)).toBe(false);
    expect(cloudConfigured(settings())).toBe(false);
    expect(cloudConfigured(settings({ endpoint: 'https://x' }))).toBe(true);
    expect(cloudConfigured(settings({ tenantId: '1' }))).toBe(true);
    expect(cloudConfigured(settings({ tokenSet: true }))).toBe(true);
  });
});

// A one-click Cloud write sends the saved state plus its own patch, and puts
// the edits it does not own back on the form, so a staged token Reset is
// neither written nor lost.
describe('pendingEdits', () => {
  const saved = settings({ endpoint: 'https://x', tags: [{ key: 'a', value: '1' }] });
  const edited = settings({
    endpoint: 'https://x',
    tokenCleared: true,
    tags: [{ key: 'a', value: '2' }],
  });

  it('returns the changed fields the write does not own', () => {
    expect(pendingEdits(edited, saved, { localForward: false })).toEqual({
      tokenCleared: true,
      tags: [{ key: 'a', value: '2' }],
    });
    expect(pendingEdits(edited, saved, { tokenCleared: false, token: '' })).toEqual({
      tags: [{ key: 'a', value: '2' }],
    });
  });

  it('returns null when the form matches what is saved', () => {
    expect(pendingEdits(saved, saved, null)).toBeNull();
  });
});

// A token written by `agento11y login` changes nothing else in the settings, so
// the flag has to count as a difference or the panel never re-hydrates.
describe('sameSettings', () => {
  it('treats tokenSet as a difference', () => {
    const base: FormSettings = settings();
    expect(sameSettings(base, settings({ tokenSet: true }))).toBe(false);
  });
});
