import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { SettingsConnectFlow } from '../internal/local/web/src/settings-screen';

afterEach(cleanup);

// SettingsConnectFlow replaces the credential form when nothing is saved. It
// replicates the `agento11y login` handshake: open the setup page on your
// stack, paste the block it hands back, pick what to forward.

const GOOD_BLOCK = [
  'AGENTO11Y_ENDPOINT=https://agento11y-prod-eu.grafana.net',
  'AGENTO11Y_AUTH_TENANT_ID=123456',
  'AGENTO11Y_AUTH_TOKEN=glc_token',
].join('\n');

function renderFlow(props: Partial<React.ComponentProps<typeof SettingsConnectFlow>> = {}) {
  const onConnect = vi.fn();
  const onManual = vi.fn();
  render(
    <SettingsConnectFlow
      savedStackURL=""
      configPath="/home/x/.agento11y/config.env"
      capture=""
      onConnect={onConnect}
      onManual={onManual}
      {...props}
    />,
  );
  return { onConnect, onManual };
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

  // A dropped value is named for what is wrong with it. Reporting a
  // placeholder or a broken URL as a missing key sends the user back for the
  // same block again.
  it('says a placeholder is a placeholder, not a missing key', () => {
    renderFlow();
    paste(GOOD_BLOCK.replace('glc_token', '<your token>'));
    expect(screen.getByText("Couldn't read a complete block")).toBeTruthy();
    expect(screen.getByText(/AGENTO11Y_AUTH_TOKEN is still a placeholder/)).toBeTruthy();
  });

  it('names a URL slot that does not hold a URL', () => {
    renderFlow();
    paste(GOOD_BLOCK.replace('https://agento11y-prod-eu.grafana.net', 'not-a-url'));
    expect(screen.getByText(/AGENTO11Y_ENDPOINT is not an http:\/\/ or https:\/\/ URL/)).toBeTruthy();
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

    fireEvent.click(screen.getByRole('button', { name: /Forward full content/ }));
    expect(onConnect).toHaveBeenCalledTimes(1);
    expect(onConnect.mock.calls[0]?.[1]).toBe('full');
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
