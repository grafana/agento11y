import type { KeyboardEvent } from 'react';
import { useEffect, useRef, useState } from 'react';
import { fieldInput } from './routing';
import { Icon } from './shell';

interface HistoryDatePickerProps {
  value: string;
  effectiveSince?: string;
  onChange: (value: string) => void;
  disabled?: boolean;
}

const DATE_VALUE_RE = /^(\d{4})-(\d{2})-(\d{2})$/;
const WEEKDAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const dateLabel = new Intl.DateTimeFormat('en-US', {
  year: 'numeric',
  month: 'long',
  day: 'numeric',
});
const monthLabel = new Intl.DateTimeFormat('en-US', {
  year: 'numeric',
  month: 'long',
});

function localDate(year: number, month: number, day: number): Date {
  const date = new Date(0);
  date.setHours(0, 0, 0, 0);
  date.setFullYear(year, month, day);
  return date;
}

export function parseLocalDate(value: string): Date | null {
  const match = DATE_VALUE_RE.exec(value);
  if (!match) return null;
  const year = Number(match[1]);
  const month = Number(match[2]);
  const day = Number(match[3]);
  const date = localDate(year, month - 1, day);
  if (date.getFullYear() !== year || date.getMonth() !== month - 1 || date.getDate() !== day) return null;
  return date;
}

export function localDateStartISO(value: string): string | null {
  return parseLocalDate(value)?.toISOString() ?? null;
}

function localDateValue(date: Date): string {
  const year = String(date.getFullYear()).padStart(4, '0');
  const month = String(date.getMonth() + 1).padStart(2, '0');
  const day = String(date.getDate()).padStart(2, '0');
  return `${year}-${month}-${day}`;
}

function firstOfMonth(date: Date): Date {
  return localDate(date.getFullYear(), date.getMonth(), 1);
}

function dateFromSince(since: string | undefined): Date | null {
  if (!since) return null;
  const date = new Date(since);
  return Number.isNaN(date.getTime()) ? null : localDate(date.getFullYear(), date.getMonth(), date.getDate());
}

function addDays(date: Date, days: number): Date {
  return localDate(date.getFullYear(), date.getMonth(), date.getDate() + days);
}

function moveMonth(date: Date, delta: number): Date {
  const target = localDate(date.getFullYear(), date.getMonth() + delta, 1);
  const lastDay = localDate(target.getFullYear(), target.getMonth() + 1, 0).getDate();
  return localDate(target.getFullYear(), target.getMonth(), Math.min(date.getDate(), lastDay));
}

function calendarDates(month: Date): Date[] {
  const first = firstOfMonth(month);
  const start = addDays(first, -first.getDay());
  return Array.from({ length: 42 }, (_, i) => addDays(start, i));
}

function currentLocalDate(): Date {
  const now = new Date();
  return localDate(now.getFullYear(), now.getMonth(), now.getDate());
}

export function HistoryDatePicker({ value, effectiveSince, onChange, disabled }: HistoryDatePickerProps) {
  const [open, setOpen] = useState(false);
  const [visibleMonth, setVisibleMonth] = useState(() => firstOfMonth(new Date()));
  const [focusedDate, setFocusedDate] = useState('');
  const rootRef = useRef<HTMLDivElement>(null);
  const triggerRef = useRef<HTMLButtonElement>(null);
  const gridRef = useRef<HTMLTableElement>(null);
  const focusDateRef = useRef(false);
  const today = currentLocalDate();
  const todayValue = localDateValue(today);
  const selectedDate = parseLocalDate(value);

  useEffect(() => {
    if (!open || !focusedDate || !focusDateRef.current) return;
    focusDateRef.current = false;
    gridRef.current?.querySelector<HTMLElement>(`[data-date="${focusedDate}"]`)?.focus();
  }, [open, focusedDate]);

  useEffect(() => {
    if (disabled) setOpen(false);
  }, [disabled]);

  const close = (returnFocus: boolean) => {
    focusDateRef.current = false;
    setOpen(false);
    if (returnFocus) triggerRef.current?.focus();
  };

  const setFocusedDateAndFocus = (date: Date) => {
    focusDateRef.current = true;
    setFocusedDate(localDateValue(date));
  };

  const openCalendar = () => {
    const effective = selectedDate || dateFromSince(effectiveSince) || today;
    const initial = effective.getTime() > today.getTime() ? today : effective;
    setVisibleMonth(firstOfMonth(initial));
    setFocusedDateAndFocus(initial);
    setOpen(true);
  };

  const pick = (date: Date) => {
    if (date.getTime() > today.getTime()) return;
    onChange(localDateValue(date));
    close(true);
  };

  const selectToday = () => {
    const current = currentLocalDate();
    onChange(localDateValue(current));
    setVisibleMonth(firstOfMonth(current));
    setFocusedDate(localDateValue(current));
  };

  const moveFocus = (date: Date, days: number) => {
    const next = addDays(date, days);
    if (next.getTime() > today.getTime()) return;
    setFocusedDateAndFocus(next);
    setVisibleMonth(firstOfMonth(next));
  };

  const navigateMonth = (delta: number) => {
    const current = parseLocalDate(focusedDate) || visibleMonth;
    const next = moveMonth(current, delta);
    const bounded = next.getTime() > today.getTime() ? today : next;
    setVisibleMonth(firstOfMonth(bounded));
    setFocusedDate(localDateValue(bounded));
  };

  const handleDateKeyDown = (event: KeyboardEvent<HTMLButtonElement>, date: Date) => {
    if (event.key === 'ArrowLeft') {
      event.preventDefault();
      moveFocus(date, -1);
    } else if (event.key === 'ArrowRight') {
      event.preventDefault();
      moveFocus(date, 1);
    } else if (event.key === 'ArrowUp') {
      event.preventDefault();
      moveFocus(date, -7);
    } else if (event.key === 'ArrowDown') {
      event.preventDefault();
      moveFocus(date, 7);
    }
  };

  const nextMonthDisabled =
    visibleMonth.getFullYear() === today.getFullYear() && visibleMonth.getMonth() === today.getMonth();
  const dates = calendarDates(visibleMonth);

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: Child controls own focus; the wrapper closes the calendar.
    <div
      ref={rootRef}
      role="presentation"
      style={{ position: 'relative', width: 220 }}
      onBlur={(event) => {
        if (!rootRef.current?.contains(event.relatedTarget)) setOpen(false);
      }}
    >
      <button
        ref={triggerRef}
        type="button"
        aria-label={`History start date: ${selectedDate ? dateLabel.format(selectedDate) : 'Last 90 days'}`}
        aria-haspopup="dialog"
        aria-expanded={open && !disabled}
        disabled={disabled}
        onClick={() => (open ? close(false) : openCalendar())}
        onKeyDown={(event) => {
          if (open && event.key === 'Escape') {
            event.preventDefault();
            close(false);
            return;
          }
          if (!open && (event.key === 'Enter' || event.key === ' ' || event.key === 'ArrowDown')) {
            event.preventDefault();
            openCalendar();
          }
        }}
        style={{
          ...fieldInput,
          outline: undefined,
          width: 220,
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 8,
          textAlign: 'left',
          cursor: disabled ? 'not-allowed' : 'pointer',
          opacity: disabled ? 0.6 : 1,
        }}
      >
        <span style={{ display: 'inline-flex', alignItems: 'center', gap: 8 }}>
          <Icon name="clock" size={14} style={{ color: 'var(--fg3)' }} />
          {selectedDate ? dateLabel.format(selectedDate) : 'Last 90 days'}
        </span>
        <Icon name="chevron" size={13} style={{ color: 'var(--fg3)' }} />
      </button>

      {open && !disabled && (
        <div
          role="dialog"
          aria-label="Choose import start date"
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return;
            event.preventDefault();
            event.stopPropagation();
            close(true);
          }}
          style={{
            position: 'absolute',
            zIndex: 30,
            top: 38,
            right: 0,
            width: 286,
            padding: 12,
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            background: 'var(--bg-secondary)',
            boxShadow: 'var(--shadow-z2)',
          }}
        >
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              justifyContent: 'space-between',
              marginBottom: 10,
            }}
          >
            <button type="button" aria-label="Previous month" onClick={() => navigateMonth(-1)} style={navButtonStyle}>
              <Icon name="cright" size={14} style={{ transform: 'rotate(180deg)' }} />
            </button>
            <div aria-live="polite" style={{ fontSize: 13, fontWeight: 600, color: 'var(--fg1)' }}>
              {monthLabel.format(visibleMonth)}
            </div>
            <button
              type="button"
              aria-label="Next month"
              disabled={nextMonthDisabled}
              onClick={() => navigateMonth(1)}
              style={{ ...navButtonStyle, opacity: nextMonthDisabled ? 0.35 : 1 }}
            >
              <Icon name="cright" size={14} />
            </button>
          </div>

          <table
            ref={gridRef}
            aria-label={monthLabel.format(visibleMonth)}
            style={{ width: '100%', tableLayout: 'fixed', borderCollapse: 'separate', borderSpacing: 2 }}
          >
            <thead aria-hidden="true" style={{ color: 'var(--fg3)', fontSize: 10, fontWeight: 400 }}>
              <tr>
                {WEEKDAYS.map((day) => (
                  <th key={day} scope="col" style={{ height: 18, fontWeight: 400 }}>
                    {day}
                  </th>
                ))}
              </tr>
            </thead>
            <tbody>
              {Array.from({ length: 6 }, (_, week) => (
                <tr key={localDateValue(dates[week * 7] as Date)}>
                  {dates.slice(week * 7, week * 7 + 7).map((date) => {
                    const dateValue = localDateValue(date);
                    const future = dateValue > todayValue;
                    const selected = dateValue === value;
                    const outside = date.getMonth() !== visibleMonth.getMonth();
                    return (
                      <td key={dateValue} style={{ padding: 0 }}>
                        <button
                          type="button"
                          data-date={dateValue}
                          aria-label={dateLabel.format(date)}
                          aria-current={dateValue === todayValue ? 'date' : undefined}
                          aria-pressed={selected}
                          tabIndex={dateValue === focusedDate ? 0 : -1}
                          disabled={future}
                          onClick={() => pick(date)}
                          onKeyDown={(event) => handleDateKeyDown(event, date)}
                          style={{
                            width: '100%',
                            height: 30,
                            border: selected ? '1px solid var(--primary-border)' : '1px solid transparent',
                            borderRadius: 2,
                            background: selected ? 'var(--date-selected-bg)' : 'transparent',
                            color: future
                              ? 'var(--date-disabled-fg)'
                              : outside
                                ? 'var(--fg3)'
                                : selected
                                  ? 'var(--primary-text)'
                                  : 'var(--fg1)',
                            cursor: future ? 'not-allowed' : 'pointer',
                            fontSize: 12,
                            fontFamily: 'var(--fontFamily)',
                          }}
                        >
                          {date.getDate()}
                        </button>
                      </td>
                    );
                  })}
                </tr>
              ))}
            </tbody>
          </table>

          <div style={{ display: 'flex', marginTop: 10 }}>
            <button
              type="button"
              onClick={selectToday}
              onMouseEnter={(event) => (event.currentTarget.style.background = 'var(--action-hover)')}
              onMouseLeave={(event) => (event.currentTarget.style.background = 'transparent')}
              style={footerButtonStyle}
            >
              Today
            </button>
          </div>
        </div>
      )}
    </div>
  );
}

const footerButtonStyle = {
  flex: 1,
  height: 30,
  border: '1px solid var(--secondary-border)',
  borderRadius: 2,
  background: 'transparent',
  color: 'var(--fg1)',
  cursor: 'pointer',
  fontSize: 12,
  fontWeight: 500,
  fontFamily: 'var(--fontFamily)',
};

const navButtonStyle = {
  width: 28,
  height: 28,
  padding: 0,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  border: '1px solid transparent',
  borderRadius: 2,
  background: 'transparent',
  color: 'var(--fg2)',
  cursor: 'pointer',
};
