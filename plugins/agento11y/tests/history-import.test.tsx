import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { type HistoryImport, HistoryImportBanner, type ImportRunView } from '../internal/local/web/src/settings-screen';
import type { HistoryOffer } from '../internal/local/web/src/types';

afterEach(cleanup);

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
