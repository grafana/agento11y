import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { useState } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { HistoryDatePicker, localDateStartISO, parseLocalDate } from '../internal/local/web/src/history-date-picker';

beforeEach(() => {
  vi.useFakeTimers();
  vi.setSystemTime(new Date(2026, 3, 10, 12));
});

afterEach(() => {
  cleanup();
  vi.useRealTimers();
  vi.unstubAllEnvs();
});

function ControlledPicker({
  initial = '',
  effectiveSince = new Date(2026, 0, 10, 12).toISOString(),
  disabled = false,
}) {
  const [value, setValue] = useState(initial);
  return <HistoryDatePicker value={value} effectiveSince={effectiveSince} onChange={setValue} disabled={disabled} />;
}

describe('local date conversion', () => {
  it('parses only real calendar dates', () => {
    expect(parseLocalDate('2026-02-28')?.getDate()).toBe(28);
    expect(parseLocalDate('2026-02-29')).toBeNull();
    expect(parseLocalDate('2026-2-01')).toBeNull();
    expect(parseLocalDate('not-a-date')).toBeNull();
  });

  it('converts standard and daylight-saving dates from local midnight', () => {
    vi.stubEnv('TZ', 'America/Los_Angeles');
    expect(localDateStartISO('2026-01-15')).toBe('2026-01-15T08:00:00.000Z');
    expect(localDateStartISO('2026-07-15')).toBe('2026-07-15T07:00:00.000Z');
  });
});

describe('HistoryDatePicker', () => {
  it('shows the 90-day default and opens on the effective month', () => {
    render(<ControlledPicker />);
    const trigger = screen.getByRole('button', { name: 'History start date: Last 90 days' });
    expect(trigger.textContent).toContain('Last 90 days');
    expect(trigger.style.outline).toBe('');

    fireEvent.click(trigger);
    expect(screen.getByRole('dialog', { name: 'Choose import start date' })).toBeTruthy();
    expect(screen.getByText('January 2026')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'January 10, 2026' })).toBe(document.activeElement);
  });

  it('selects a date and closes the calendar', () => {
    render(<ControlledPicker />);
    const trigger = screen.getByRole('button', { name: 'History start date: Last 90 days' });
    fireEvent.click(trigger);
    fireEvent.click(screen.getByRole('button', { name: 'January 15, 2026' }));
    expect(screen.getByRole('button', { name: 'History start date: January 15, 2026' })).toBe(trigger);
    expect(trigger.textContent).toContain('January 15, 2026');
    expect(screen.queryByRole('dialog', { name: 'Choose import start date' })).toBeNull();
    expect(trigger).toBe(document.activeElement);
  });

  it('selects the browser current date from the footer', () => {
    vi.stubEnv('TZ', 'America/Los_Angeles');
    vi.setSystemTime(new Date('2026-04-11T06:30:00Z'));
    render(<ControlledPicker />);
    const trigger = screen.getByRole('button', { name: 'History start date: Last 90 days' });
    fireEvent.click(trigger);
    const today = screen.getByRole('button', { name: 'Today' });
    expect(today.style.color).toBe('var(--fg1)');
    fireEvent.mouseEnter(today);
    expect(today.style.background).toBe('var(--action-hover)');
    today.focus();
    fireEvent.click(today);
    expect(trigger.textContent).toContain('April 10, 2026');
    expect(screen.getByRole('dialog', { name: 'Choose import start date' })).toBeTruthy();
    expect(screen.getByText('April 2026')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'April 10, 2026' }).getAttribute('aria-pressed')).toBe('true');
    expect(today).toBe(document.activeElement);
  });

  it('disables dates after the browser current date', () => {
    render(<ControlledPicker effectiveSince="2026-04-01T12:00:00Z" />);
    fireEvent.click(screen.getByRole('button', { name: 'History start date: Last 90 days' }));
    expect((screen.getByRole('button', { name: 'April 10, 2026' }) as HTMLButtonElement).disabled).toBe(false);
    expect((screen.getByRole('button', { name: 'April 11, 2026' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Next month' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('moves focus with arrow keys and navigates months', () => {
    render(<ControlledPicker />);
    fireEvent.click(screen.getByRole('button', { name: 'History start date: Last 90 days' }));
    const january10 = screen.getByRole('button', { name: 'January 10, 2026' });
    fireEvent.keyDown(january10, { key: 'ArrowRight' });
    expect(screen.getByRole('button', { name: 'January 11, 2026' })).toBe(document.activeElement);
    fireEvent.keyDown(document.activeElement as Element, { key: 'ArrowDown' });
    expect(screen.getByRole('button', { name: 'January 18, 2026' })).toBe(document.activeElement);

    const nextMonth = screen.getByRole('button', { name: 'Next month' });
    nextMonth.focus();
    fireEvent.click(nextMonth);
    expect(screen.getByText('February 2026')).toBeTruthy();
    expect(nextMonth).toBe(document.activeElement);
    fireEvent.click(nextMonth);
    expect(screen.getByText('March 2026')).toBeTruthy();
    expect(nextMonth).toBe(document.activeElement);
  });

  it('closes on Escape from every dialog control and returns focus to the trigger', () => {
    render(<ControlledPicker initial="2026-01-10" />);
    const trigger = screen.getByRole('button', { name: 'History start date: January 10, 2026' });

    for (const controlName of ['Previous month', 'Next month', 'Today']) {
      fireEvent.keyDown(trigger, { key: 'ArrowDown' });
      const control = screen.getByRole('button', { name: controlName });
      control.focus();
      fireEvent.keyDown(control, { key: 'Escape' });
      expect(screen.queryByRole('dialog', { name: 'Choose import start date' })).toBeNull();
      expect(trigger).toBe(document.activeElement);
    }

    fireEvent.keyDown(trigger, { key: 'ArrowDown' });
    trigger.focus();
    fireEvent.keyDown(trigger, { key: 'Escape' });
    expect(screen.queryByRole('dialog', { name: 'Choose import start date' })).toBeNull();
    expect(trigger).toBe(document.activeElement);
  });

  it('closes an open dialog when the control becomes disabled', () => {
    const view = render(<ControlledPicker />);
    fireEvent.click(screen.getByRole('button', { name: 'History start date: Last 90 days' }));
    expect(screen.getByRole('dialog', { name: 'Choose import start date' })).toBeTruthy();

    view.rerender(<ControlledPicker disabled />);
    expect(screen.queryByRole('dialog', { name: 'Choose import start date' })).toBeNull();
    const trigger = screen.getByRole('button', { name: 'History start date: Last 90 days' }) as HTMLButtonElement;
    expect(trigger.disabled).toBe(true);
    expect(trigger.getAttribute('aria-expanded')).toBe('false');
  });
});
