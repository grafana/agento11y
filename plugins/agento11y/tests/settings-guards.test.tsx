import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsGuardsCard } from '../internal/local/web/src/settings-screen';
import type { Settings } from '../internal/local/web/src/types';

afterEach(cleanup);

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

  it('shows the saved guard mode when Cloud forwarding is on', () => {
    render(
      <SettingsGuardsCard form={settings()} savedGuards="failclosed" set={vi.fn()} status={null} localOnly={false} />,
    );

    const failClosed = screen.getByRole('button', { name: 'Fail closed' });
    expect(failClosed.hasAttribute('disabled')).toBe(false);
    expect(failClosed.getAttribute('aria-pressed')).toBe('true');
    expect(screen.getByDisplayValue('2200')).toBeTruthy();
    expect(screen.getByText('Restart a running agent if it does not use the new guard settings.')).toBeTruthy();
  });
});
