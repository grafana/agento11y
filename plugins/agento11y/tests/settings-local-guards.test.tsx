import { cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsLocalGuardsCard } from '../internal/local/web/src/settings-local-guards';
import type { GuardPack, GuardsFile } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
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
      pack({
        id: 'git',
        title: 'Git safety',
        description: 'Block destructive git.',
        kind: 'deny',
        detail: '8 git commands',
        preview: ['git reset --hard', 'git stash drop'],
      }),
      pack({
        id: 'destructive',
        title: 'Destructive commands',
        description: 'Block recursive deletes.',
        kind: 'deny',
        detail: 'rm -rf and rm -fr',
        preview: ['rm -rf', 'rm -fr'],
      }),
    ],
    rules: [],
    ...overrides,
  };
}

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
}

describe('SettingsLocalGuardsCard', () => {
  it('hides packs until guards are enabled', async () => {
    const off = guards({ enabled: false });
    const on = guards({ enabled: true });
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === '/api/v1/guards' && init?.method === 'PUT') {
        expect(JSON.parse(String(init.body))).toEqual({ enabled: true });
        return Promise.resolve(jsonResponse(on));
      }
      return Promise.resolve(jsonResponse(off));
    });
    vi.stubGlobal('fetch', fetchMock);

    const onEnabledChange = vi.fn();
    render(<SettingsLocalGuardsCard onEnabledChange={onEnabledChange} />);
    await screen.findByRole('switch', { name: 'Enable guards' });
    expect(screen.queryByRole('switch', { name: 'Secret redaction' })).toBeNull();

    fireEvent.click(screen.getByRole('switch', { name: 'Enable guards' }));
    await screen.findByRole('switch', { name: 'Secret redaction' });
    expect(screen.getByRole('switch', { name: 'Enable guards' }).getAttribute('aria-checked')).toBe('true');
    expect(onEnabledChange).toHaveBeenCalledWith(true);
  });

  it('lists packs and toggles one on', async () => {
    const initial = guards();
    const enabled = guards({
      packs: initial.packs.map((item) => (item.id === 'secrets' ? { ...item, enabled: true } : item)),
      enforcing: 1,
    });
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === '/api/v1/guards' && init?.method === 'PUT') {
        expect(JSON.parse(String(init.body))).toEqual({ packs: { secrets: true } });
        return Promise.resolve(jsonResponse(enabled));
      }
      return Promise.resolve(jsonResponse(initial));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<SettingsLocalGuardsCard />);
    await screen.findByRole('switch', { name: 'Secret redaction' });
    expect(screen.getByRole('switch', { name: 'Git safety' }).getAttribute('aria-checked')).toBe('false');

    fireEvent.click(screen.getByRole('switch', { name: 'Secret redaction' }));
    await waitFor(() =>
      expect(screen.getByRole('switch', { name: 'Secret redaction' }).getAttribute('aria-checked')).toBe('true'),
    );
    expect(fetchMock.mock.calls.some(([, init]) => init?.method === 'PUT')).toBe(true);
  });

  it('turns every pack off when guards are disabled', async () => {
    const on = guards({
      packs: guards().packs.map((item) => ({ ...item, enabled: item.id === 'git' })),
    });
    const off = guards({
      enabled: false,
      packs: on.packs.map((item) => ({ ...item, enabled: false })),
    });
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit) => {
      if (String(input) === '/api/v1/guards' && init?.method === 'PUT') {
        expect(JSON.parse(String(init.body))).toEqual({
          enabled: false,
          packs: { secrets: false, git: false, destructive: false },
        });
        return Promise.resolve(jsonResponse(off));
      }
      return Promise.resolve(jsonResponse(on));
    });
    vi.stubGlobal('fetch', fetchMock);

    render(<SettingsLocalGuardsCard />);
    await screen.findByRole('switch', { name: 'Git safety' });
    fireEvent.click(screen.getByRole('switch', { name: 'Enable guards' }));
    await waitFor(() => expect(screen.queryByRole('switch', { name: 'Git safety' })).toBeNull());
    expect(screen.getByRole('switch', { name: 'Enable guards' }).getAttribute('aria-checked')).toBe('false');
  });

  it('expands a pack preview', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => Promise.resolve(jsonResponse(guards()))),
    );
    render(<SettingsLocalGuardsCard />);
    const view = await screen.findByRole('button', { name: 'View Git safety' });
    expect(view.textContent).toContain('view');
    expect(screen.queryByText('git stash drop')).toBeNull();
    fireEvent.click(view);
    expect(screen.getByText('git stash drop')).toBeTruthy();
    expect(view.getAttribute('aria-expanded')).toBe('true');
    fireEvent.click(view);
    expect(screen.queryByText('git stash drop')).toBeNull();
  });
});
