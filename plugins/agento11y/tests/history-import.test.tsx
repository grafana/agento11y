import { act, cleanup, fireEvent, render, screen, waitFor } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { localDateStartISO } from '../internal/local/web/src/history-date-picker';
import {
  type HistoryImport,
  HistoryImportBanner,
  type ImportRunView,
  SettingsHistoryTab,
} from '../internal/local/web/src/settings-screen';
import type { HistoryOffer, HistoryPlan } from '../internal/local/web/src/types';

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

// The banner is the one part of the import UI a user sees without opening
// Settings, and its text is metadata only: session and turn counts, never a
// prompt or a title.

function offer(overrides: Partial<HistoryOffer> = {}): HistoryOffer {
  return {
    agent: 'claude',
    display_name: 'Claude Code',
    sessions: 12,
    turns: 480,
    approx_turns: false,
    show: true,
    ...overrides,
  };
}

function historyImport(overrides: Partial<HistoryImport> = {}): HistoryImport {
  return {
    agents: [],
    offers: [offer()],
    run: null,
    error: null,
    start: vi.fn().mockResolvedValue(null),
    cancel: vi.fn().mockResolvedValue(undefined),
    dismiss: vi.fn().mockResolvedValue(undefined),
    reloadOffers: vi.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

function run(overrides: Partial<ImportRunView> = {}): ImportRunView {
  return { run_id: 'run-1', agent: 'claude', status: 'running', ...overrides };
}

function plan(since: string, sessionCount: number): HistoryPlan {
  return {
    agent: 'claude-code',
    since,
    until: '',
    sessions: Array.from({ length: sessionCount }, (_, index) => ({
      session_id: `session-${index}`,
      title: `session-${index}`,
      workspace: '/work/repo',
      source_path: `/tmp/session-${index}.jsonl`,
      turn_count: 2,
      approx_turns: false,
      size_bytes: 100,
      started_at: '2020-01-01T00:00:00Z',
      last_activity_at: '2020-01-02T00:00:00Z',
      active: false,
    })),
    skipped: [],
    warnings: [],
  };
}

function response(body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { 'Content-Type': 'application/json' },
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function settingsHistory(overrides: Partial<HistoryImport> = {}): HistoryImport {
  return historyImport({
    agents: [{ id: 'claude-code', display_name: 'Claude Code', aliases: ['claude'] }],
    offers: [],
    ...overrides,
  });
}

function historyDateTrigger() {
  return screen.getByRole('button', { name: /^History start date:/ });
}

describe('HistoryImportBanner', () => {
  it('reports what was found in counts only', () => {
    render(<HistoryImportBanner history={historyImport()} />);
    const text = screen.getByText(/wrote 12 sessions/).textContent ?? '';
    expect(text).toContain('Claude Code wrote 12 sessions (480 turns)');
    expect(text).toContain('sends nothing to Grafana Cloud');
  });

  it('marks an estimated turn count as approximate', () => {
    render(<HistoryImportBanner history={historyImport({ offers: [offer({ approx_turns: true })] })} />);
    expect(screen.getByText(/about 480 turns/)).toBeTruthy();
  });

  it('says session in the singular for one session', () => {
    render(<HistoryImportBanner history={historyImport({ offers: [offer({ sessions: 1, turns: 1 })] })} />);
    expect(screen.getByText(/wrote 1 session \(1 turn\)/)).toBeTruthy();
  });

  it('starts the import for the offered agent', () => {
    const history = historyImport();
    render(<HistoryImportBanner history={history} />);
    fireEvent.click(screen.getByRole('button', { name: 'Import' }));
    expect(history.start).toHaveBeenCalledWith('claude');
  });

  // Dismissing with no agent silences every offer, which is what "Not now"
  // means on a banner that shows one agent at a time.
  it('dismisses every offer from Not now', () => {
    const history = historyImport();
    render(<HistoryImportBanner history={history} />);
    fireEvent.click(screen.getByRole('button', { name: 'Not now' }));
    expect(history.dismiss).toHaveBeenCalledWith('');
  });

  it('opens the history tab from Options', () => {
    const onOpenSettings = vi.fn();
    render(<HistoryImportBanner history={historyImport()} onOpenSettings={onOpenSettings} />);
    fireEvent.click(screen.getByRole('button', { name: 'Options' }));
    expect(onOpenSettings).toHaveBeenCalledWith('history');
  });

  it('renders nothing when no offer is worth showing', () => {
    const { container } = render(<HistoryImportBanner history={historyImport({ offers: [offer({ show: false })] })} />);
    expect(container.innerHTML).toBe('');
  });

  it('shows a dismissal failure next to the offer', () => {
    render(<HistoryImportBanner history={historyImport({ error: 'Could not dismiss the import offer: disk full' })} />);
    expect(screen.getByText(/disk full/)).toBeTruthy();
  });

  describe('while a run is active', () => {
    it('replaces the offer with progress counted in sessions', () => {
      const history = historyImport({ run: run({ sessions: 3, selected: 12 }) });
      render(<HistoryImportBanner history={history} />);
      expect(screen.queryByRole('button', { name: 'Import' })).toBeNull();
      expect(screen.getByText('3 of 12 sessions')).toBeTruthy();
    });

    // A run that has not finished discovery has no total to count against, so
    // it says what it is doing rather than reporting "0 of 0".
    it('says it is scanning before discovery has a total', () => {
      render(<HistoryImportBanner history={historyImport({ run: run({ status: 'pending' }) })} />);
      expect(screen.getByText('Scanning sessions…')).toBeTruthy();
    });

    it('cancels the run', () => {
      const history = historyImport({ run: run({ sessions: 1, selected: 4 }) });
      render(<HistoryImportBanner history={history} />);
      fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
      expect(history.cancel).toHaveBeenCalledTimes(1);
    });

    it('goes back to the offer once the run finished', () => {
      render(<HistoryImportBanner history={historyImport({ run: run({ status: 'completed', sessions: 12 }) })} />);
      expect(screen.getByRole('button', { name: 'Import' })).toBeTruthy();
    });
  });
});

describe('SettingsHistoryTab', () => {
  it('plans with the default or selected date and imports the echoed bound', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://localhost');
      const since = url.searchParams.get('since');
      return Promise.resolve(response(plan(since || '2020-01-10T12:00:00Z', since ? 2 : 1)));
    });
    vi.stubGlobal('fetch', fetchMock);
    const history = settingsHistory();
    render(<SettingsHistoryTab history={history} />);

    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();
    const initialURL = new URL(String(fetchMock.mock.calls[0]?.[0]), 'http://localhost');
    expect(initialURL.searchParams.get('agent')).toBe('claude-code');
    expect(initialURL.searchParams.has('since')).toBe(false);
    expect(historyDateTrigger().textContent).toContain('Last 90 days');
    fireEvent.click(screen.getByRole('button', { name: 'Import 1 sessions' }));
    expect(history.start).toHaveBeenNthCalledWith(1, 'claude-code', {});
    await waitFor(() => expect((historyDateTrigger() as HTMLButtonElement).disabled).toBe(false));

    fireEvent.click(historyDateTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'January 15, 2020' }));

    expect(await screen.findByText('2 sessions · 4 turns')).toBeTruthy();
    const selectedURL = new URL(String(fetchMock.mock.calls[1]?.[0]), 'http://localhost');
    const selectedSince = localDateStartISO('2020-01-15');
    expect(selectedURL.searchParams.get('since')).toBe(selectedSince);

    fireEvent.click(screen.getByRole('button', { name: 'Import 2 sessions' }));
    expect(history.start).toHaveBeenNthCalledWith(2, 'claude-code', { since: selectedSince });
  });

  it('waits for the effective default date before opening the calendar', async () => {
    const planned = deferred<Response>();
    vi.stubGlobal('fetch', vi.fn().mockReturnValue(planned.promise));
    render(<SettingsHistoryTab history={settingsHistory()} />);

    expect((historyDateTrigger() as HTMLButtonElement).disabled).toBe(true);
    await act(async () => planned.resolve(response(plan('2020-01-10T12:00:00Z', 1))));
    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();
    expect((historyDateTrigger() as HTMLButtonElement).disabled).toBe(false);
    fireEvent.click(historyDateTrigger());
    expect(screen.getByText('January 2020')).toBeTruthy();
  });

  it('does not clip the calendar at the settings card boundary', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(plan('2020-01-10T12:00:00Z', 1))));
    render(<SettingsHistoryTab history={settingsHistory()} />);
    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();

    fireEvent.click(historyDateTrigger());
    const dialog = screen.getByRole('dialog', { name: 'Choose import start date' });
    const clippingAncestors: HTMLElement[] = [];
    for (let ancestor = dialog.parentElement; ancestor; ancestor = ancestor.parentElement) {
      if (ancestor.style.overflow === 'hidden') clippingAncestors.push(ancestor);
    }
    expect(clippingAncestors).toEqual([]);
  });

  it('locks the controls while an import request starts', async () => {
    const started = deferred<{ run_id: string; status?: string }>();
    const start = vi.fn().mockReturnValue(started.promise);
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(response(plan('2020-01-10T12:00:00Z', 1))));
    render(<SettingsHistoryTab history={settingsHistory({ start })} />);
    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: 'Import 1 sessions' }));
    expect((screen.getByRole('button', { name: 'Claude Code' }) as HTMLButtonElement).disabled).toBe(true);
    expect((historyDateTrigger() as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Starting…' }) as HTMLButtonElement).disabled).toBe(true);

    await act(async () => started.resolve({ run_id: 'run-1', status: 'pending' }));
    expect((screen.getByRole('button', { name: 'Import 1 sessions' }) as HTMLButtonElement).disabled).toBe(false);
  });

  it('keeps the latest plan when responses arrive out of order', async () => {
    const older = deferred<Response>();
    const current = deferred<Response>();
    const fetchMock = vi
      .fn()
      .mockResolvedValueOnce(response(plan('2020-01-10T12:00:00Z', 1)))
      .mockReturnValueOnce(older.promise)
      .mockReturnValueOnce(current.promise);
    vi.stubGlobal('fetch', fetchMock);
    const history = settingsHistory();
    render(<SettingsHistoryTab history={history} />);
    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();

    fireEvent.click(historyDateTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'January 15, 2020' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    expect((screen.getByRole('button', { name: 'Import 0 sessions' }) as HTMLButtonElement).disabled).toBe(true);

    fireEvent.click(historyDateTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'January 16, 2020' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));

    await act(async () => {
      current.resolve(response(plan('2020-01-16T08:00:00Z', 3)));
    });
    expect(await screen.findByText('3 sessions · 6 turns')).toBeTruthy();

    await act(async () => {
      older.resolve(response(plan('2020-01-15T08:00:00Z', 8)));
    });
    expect(screen.queryByText('8 sessions · 16 turns')).toBeNull();
    fireEvent.click(screen.getByRole('button', { name: 'Import 3 sessions' }));
    expect(history.start).toHaveBeenCalledWith('claude-code', { since: '2020-01-16T08:00:00Z' });
  });

  it('locks the controls while an import is active', () => {
    const fetchMock = vi.fn().mockResolvedValue(response(plan('2020-01-10T12:00:00Z', 1)));
    vi.stubGlobal('fetch', fetchMock);
    render(<SettingsHistoryTab history={settingsHistory({ run: run({ agent: 'claude-code' }) })} />);

    expect((screen.getByRole('button', { name: 'Claude Code' }) as HTMLButtonElement).disabled).toBe(true);
    expect((historyDateTrigger() as HTMLButtonElement).disabled).toBe(true);
    expect(screen.queryByRole('button', { name: /Import \d+ sessions/ })).toBeNull();
    expect(screen.getByRole('button', { name: 'Cancel import' })).toBeTruthy();
  });

  it('refreshes the selected date after a run finishes', async () => {
    const fetchMock = vi.fn((input: RequestInfo | URL) => {
      const url = new URL(String(input), 'http://localhost');
      return Promise.resolve(response(plan(url.searchParams.get('since') || '2020-01-10T12:00:00Z', 1)));
    });
    vi.stubGlobal('fetch', fetchMock);
    const initialHistory = settingsHistory();
    const view = render(<SettingsHistoryTab history={initialHistory} />);
    expect(await screen.findByText('1 sessions · 2 turns')).toBeTruthy();

    fireEvent.click(historyDateTrigger());
    fireEvent.click(screen.getByRole('button', { name: 'January 15, 2020' }));
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(2));
    const selectedSince = localDateStartISO('2020-01-15');

    view.rerender(<SettingsHistoryTab history={settingsHistory({ run: run({ agent: 'claude-code' }) })} />);
    view.rerender(
      <SettingsHistoryTab history={settingsHistory({ run: run({ agent: 'claude-code', status: 'completed' }) })} />,
    );
    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(3));
    const refreshedURL = new URL(String(fetchMock.mock.calls[2]?.[0]), 'http://localhost');
    expect(refreshedURL.searchParams.get('since')).toBe(selectedSince);
  });
});
