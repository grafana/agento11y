import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsGuardsCard } from '../internal/local/web/src/settings-screen';
import type { Settings } from '../internal/local/web/src/types';

afterEach(cleanup);

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
    capture: '',
    tags: [],
    guards: 'failclosed',
    guardTimeout: '2200',
    debug: false,
    autoUpdate: false,
    userId: '',
    localForward: false,
    ...overrides,
  };
}

describe('SettingsGuardsCard', () => {
  it('shows guards as disabled and off for Local only sessions', () => {
    const set = vi.fn();
    render(<SettingsGuardsCard form={settings()} savedGuards="failclosed" set={set} status={null} localOnly={true} />);

    const off = screen.getByRole('button', { name: 'Off' });
    expect(off.hasAttribute('disabled')).toBe(true);
    expect(off.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'Fail open' }).hasAttribute('disabled')).toBe(true);
    expect(screen.getByRole('button', { name: 'Fail closed' }).hasAttribute('disabled')).toBe(true);
    expect(screen.queryByDisplayValue('2200')).toBeNull();
    expect(screen.getByText('Cloud guards are off for local sessions')).toBeTruthy();
    expect(screen.getByText(/Select Metadata only or Full above to use Cloud guards/)).toBeTruthy();
    expect(screen.getByText(/Non-local sessions still use the saved Fail closed mode/)).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Fail open' }));
    expect(set).not.toHaveBeenCalled();
  });

  it('names the saved mode when the form has an unsaved guard edit', () => {
    render(
      <SettingsGuardsCard
        form={settings({ guards: 'failopen' })}
        savedGuards="failclosed"
        set={vi.fn()}
        status={null}
        localOnly={true}
      />,
    );

    expect(screen.getByText(/Non-local sessions still use the saved Fail closed mode/)).toBeTruthy();
  });

  it('does not claim a saved mode when guards are off on disk', () => {
    render(
      <SettingsGuardsCard
        form={settings({ guards: 'failopen' })}
        savedGuards="off"
        set={vi.fn()}
        status={null}
        localOnly={true}
      />,
    );

    expect(screen.queryByText(/Non-local sessions still use/)).toBeNull();
  });

  it('does not enable Cloud guards from extra destination routes', () => {
    render(
      <SettingsGuardsCard
        form={settings({ localForward: true })}
        savedGuards="failclosed"
        set={vi.fn()}
        localOnly={false}
        status={{
          enabled: true,
          mode: 'metadata_only',
          generations: true,
          otlp: true,
          hooks: false,
          hookReason: 'primary credentials missing',
          destinations: [
            {
              name: 'extra',
              endpoint: 'https://extra.example',
              otlpEndpoint: '',
              httpGenerations: true,
              otlp: false,
              revision: 1,
            },
          ],
        }}
      />,
    );
    expect(screen.getByRole('button', { name: 'Off' }).getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByRole('button', { name: 'Fail open' }).hasAttribute('disabled')).toBe(true);
    expect(screen.getByText('primary credentials missing')).toBeTruthy();
    expect(screen.getByText(/Extra destinations do not evaluate guards/)).toBeTruthy();
  });

  it('can enable guards on a primary connection when the saved guard mode is off', () => {
    const set = vi.fn();
    render(
      <SettingsGuardsCard
        form={settings({ guards: 'off', localForward: true })}
        savedGuards="off"
        set={set}
        localOnly={false}
        status={{
          enabled: true,
          mode: 'metadata_only',
          generations: true,
          otlp: true,
          hooks: false,
          hookReason: 'AGENTO11Y_GUARDS_ENABLED is off, so no guard check is made at all',
          destinations: [
            {
              name: 'primary',
              endpoint: 'https://primary.example',
              otlpEndpoint: '',
              httpGenerations: true,
              otlp: false,
              revision: 1,
            },
          ],
        }}
      />,
    );
    const failOpen = screen.getByRole('button', { name: 'Fail open' });
    expect(failOpen.hasAttribute('disabled')).toBe(false);
    fireEvent.click(failOpen);
    expect(set).toHaveBeenCalledWith({ guards: 'failopen' });
  });

  it('shows the saved guard mode when Cloud forwarding is on', () => {
    render(
      <SettingsGuardsCard form={settings()} savedGuards="failclosed" set={vi.fn()} status={null} localOnly={false} />,
    );

    const failClosed = screen.getByRole('button', { name: 'Fail closed' });
    expect(failClosed.hasAttribute('disabled')).toBe(false);
    expect(failClosed.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByDisplayValue('2200')).toBeTruthy();
    expect(
      screen.getByText(/Fail open allows calls on guard endpoint transport failures or Cloud relay failures/),
    ).toBeTruthy();
    expect(screen.getByText(/Neither overrides an explicit local deny/)).toBeTruthy();
    expect(screen.getByText('Restart a running agent if it does not use the new guard settings.')).toBeTruthy();
  });
});
