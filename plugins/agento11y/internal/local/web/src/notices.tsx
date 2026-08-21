import type { CSSProperties, ElementType, HTMLAttributes, ReactNode } from 'react';
import { useRef, useState } from 'react';
import { Icon } from './shell';

// ============================================================
// Notices — loading, empty, error states
// ============================================================

export type NoticeKind = 'info' | 'warning' | 'error';

interface NoticeTone {
  color: string;
  bg: string;
  border: string;
  icon: 'empty' | 'alert';
}

interface NoticeProps {
  kind?: NoticeKind;
  title?: ReactNode;
  children?: ReactNode;
}

export function Notice({ kind = 'info', title, children }: NoticeProps) {
  // The `|| {}` branch is unreachable for the three kinds above; the cast keeps
  // the tone fields non-optional for the reader below.
  const tone: NoticeTone =
    (
      {
        info: {
          color: 'var(--fg2)',
          bg: 'rgba(204,204,220,0.03)',
          border: 'var(--border-weak)',
          icon: 'empty',
        },
        warning: {
          color: 'var(--warning-text)',
          bg: 'var(--warning-transparent, rgba(247,148,30,0.06))',
          border: 'var(--warning-border, var(--border-medium))',
          icon: 'alert',
        },
        error: {
          color: 'var(--error-text)',
          bg: 'rgba(209,14,92,0.06)',
          border: 'var(--error-border)',
          icon: 'alert',
        },
      } as const
    )[kind] || ({} as NoticeTone);
  return (
    <div
      style={{
        display: 'flex',
        gap: 12,
        alignItems: 'flex-start',
        padding: '16px 18px',
        border: `1px solid ${tone.border}`,
        background: tone.bg,
        borderRadius: 2,
        color: tone.color,
        fontSize: 13,
      }}
    >
      <Icon name={tone.icon} size={18} style={{ marginTop: 2 }} />
      <div style={{ display: 'flex', flexDirection: 'column', gap: 4 }}>
        {title && (
          <div
            style={{
              color: 'var(--fg-max)',
              fontWeight: 500,
              fontSize: 13,
            }}
          >
            {title}
          </div>
        )}
        <div style={{ color: 'var(--fg2)', lineHeight: 1.5 }}>{children}</div>
      </div>
    </div>
  );
}

const PAGE_MAX_WIDTH = 1392;
export const SURFACE_BG = 'rgba(24,27,31,0.88)';
export const ACTIVE_PILL_BG = 'var(--action-selected, rgba(204,204,220,0.08))';
const PANEL_BG = 'rgba(17,18,23,0.42)';

interface BoxProps extends HTMLAttributes<HTMLElement> {
  as?: ElementType;
  style?: CSSProperties;
  children?: ReactNode;
}

function Box({ as: Component = 'div', style, children, ...props }: BoxProps) {
  return (
    <Component {...props} style={style}>
      {children}
    </Component>
  );
}

interface StackProps extends BoxProps {
  direction?: CSSProperties['flexDirection'];
  gap?: CSSProperties['gap'];
  align?: CSSProperties['alignItems'];
  justify?: CSSProperties['justifyContent'];
  wrap?: CSSProperties['flexWrap'];
}

export function Stack({
  as = 'div',
  direction = 'column',
  gap,
  align,
  justify,
  wrap,
  style,
  children,
  ...props
}: StackProps) {
  return (
    <Box
      as={as}
      {...props}
      style={{
        display: 'flex',
        flexDirection: direction,
        gap,
        alignItems: align,
        justifyContent: justify,
        flexWrap: wrap,
        ...(style || {}),
      }}
    >
      {children}
    </Box>
  );
}

interface SurfaceCardProps extends BoxProps {
  children?: ReactNode;
  style?: CSSProperties;
}

export function SurfaceCard({ children, style, ...rest }: SurfaceCardProps) {
  return (
    <Box
      style={{
        position: 'relative',
        overflow: 'hidden',
        background: SURFACE_BG,
        border: '1px solid var(--border-weak)',
        borderRadius: 8,
        boxShadow: '0 10px 24px rgba(0,0,0,0.14)',
        ...(style || {}),
      }}
      {...rest}
    >
      {children}
    </Box>
  );
}

interface ModalFrameProps {
  title?: string;
  desc?: ReactNode;
  onClose: () => void;
  children?: ReactNode;
  width?: CSSProperties['width'];
}

export function ModalFrame({ title, desc, onClose, children, width = 'min(860px, 100%)' }: ModalFrameProps) {
  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: The backdrop handles clicks outside the dialog.
    <div
      role="presentation"
      onClick={(e) => {
        if (e.target === e.currentTarget) onClose();
      }}
      onKeyDown={(e) => {
        if (e.key === 'Escape') onClose();
      }}
      style={{
        position: 'fixed',
        inset: 0,
        zIndex: 70,
        background: 'rgba(0,0,0,0.58)',
        display: 'flex',
        alignItems: 'flex-start',
        justifyContent: 'center',
        padding: '9vh 18px 24px',
      }}
    >
      <div
        role="dialog"
        aria-modal="true"
        aria-label={title}
        style={{
          width,
          maxHeight: '82vh',
          overflow: 'hidden',
          background: 'var(--bg-secondary)',
          border: '1px solid var(--border-strong)',
          borderRadius: 8,
          boxShadow: '0 18px 54px rgba(0,0,0,0.58)',
          display: 'flex',
          flexDirection: 'column',
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'flex-start',
            justifyContent: 'space-between',
            gap: 16,
            padding: '16px 18px',
            borderBottom: '1px solid var(--border-weak)',
          }}
        >
          <div style={{ minWidth: 0 }}>
            <div
              style={{
                color: 'var(--fg-max)',
                fontSize: 15,
                fontWeight: 600,
                marginBottom: desc ? 5 : 0,
              }}
            >
              {title}
            </div>
            {desc && (
              <div
                style={{
                  color: 'var(--fg3)',
                  fontSize: 12.5,
                  lineHeight: 1.45,
                }}
              >
                {desc}
              </div>
            )}
          </div>
          <button
            type="button"
            onClick={onClose}
            style={{
              border: 'none',
              background: 'transparent',
              color: 'var(--fg3)',
              cursor: 'pointer',
              padding: 4,
            }}
          >
            <Icon name="close" size={16} />
          </button>
        </div>
        {children}
      </div>
    </div>
  );
}

interface PageShellProps {
  children?: ReactNode;
  maxWidth?: CSSProperties['maxWidth'];
  style?: CSSProperties;
}

export function PageShell({ children, maxWidth = PAGE_MAX_WIDTH, style }: PageShellProps) {
  return (
    <Box
      style={{
        width: '100%',
        maxWidth,
        margin: '0 auto',
        padding: '34px 24px 96px',
        ...(style || {}),
      }}
    >
      {children}
    </Box>
  );
}

interface PageHeroStat {
  label: string;
  value: ReactNode;
  tone?: string;
}

interface PageHeroProps {
  title?: ReactNode;
  desc?: ReactNode;
  descStyle?: CSSProperties;
  stats?: PageHeroStat[];
  style?: CSSProperties;
}

export function PageHero({ title, desc, descStyle, stats = [], style }: PageHeroProps) {
  return (
    <Stack
      direction="row"
      align="baseline"
      justify="space-between"
      gap={24}
      wrap="wrap"
      style={{
        paddingBottom: 14,
        marginBottom: 18,
        borderBottom: '1px solid var(--border-weak)',
        ...(style || {}),
      }}
    >
      <Stack direction="row" align="baseline" gap={12} style={{ minWidth: 0, flex: '1 1 320px' }}>
        <h1
          style={{
            fontSize: 20,
            lineHeight: 1.2,
            fontWeight: 600,
            color: 'var(--fg-max)',
            margin: 0,
            letterSpacing: '-0.02em',
            whiteSpace: 'nowrap',
          }}
        >
          {title}
        </h1>
        {desc && (
          <Box
            as="span"
            style={{
              fontSize: 12.5,
              color: 'var(--fg3)',
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
              ...(descStyle || {}),
            }}
          >
            {desc}
          </Box>
        )}
      </Stack>
      {stats.length > 0 && (
        <Stack
          direction="row"
          align="baseline"
          gap={18}
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 12.5,
            whiteSpace: 'nowrap',
          }}
        >
          {stats.map((stat) => (
            <span
              key={stat.label}
              style={{
                display: 'inline-flex',
                alignItems: 'baseline',
                gap: 6,
              }}
            >
              <Box as="span" style={{ fontSize: 11, color: 'var(--fg3)' }}>
                {stat.label}
              </Box>
              <Box as="span" style={{ color: stat.tone || 'var(--fg1)' }}>
                {stat.value}
              </Box>
            </span>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

interface PillToggleOption {
  value: string;
  label: ReactNode;
}

interface PillToggleProps {
  options: PillToggleOption[];
  value?: string;
  onChange: (value: string) => void;
  size?: 'sm' | 'md';
  disabled?: boolean;
}

// PillToggle's md size is for a control that carries a decision rather than
// a view preference: the forwarding mode switch, which is the first thing on
// the Cloud tab.
export function PillToggle({ options, value, onChange, size = 'sm', disabled = false }: PillToggleProps) {
  const md = size === 'md';
  return (
    <Stack
      direction="row"
      gap={3}
      style={{
        display: 'inline-flex',
        padding: 3,
        border: '1px solid var(--border-medium)',
        borderRadius: 999,
        background: PANEL_BG,
        overflow: 'hidden',
        boxShadow: 'inset 0 0 0 1px rgba(0,0,0,0.10)',
      }}
    >
      {options.map((o) => {
        const active = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            aria-pressed={active}
            disabled={disabled}
            onClick={() => onChange(o.value)}
            style={{
              padding: md ? '7px 16px' : '5px 13px',
              borderRadius: 999,
              background: active ? ACTIVE_PILL_BG : 'transparent',
              color: active ? 'var(--primary-text)' : 'var(--fg2)',
              border: 'none',
              cursor: disabled || active ? 'default' : 'pointer',
              opacity: disabled ? 0.55 : 1,
              fontSize: md ? 13 : 12,
              fontWeight: active ? 600 : 400,
              fontFamily: 'var(--fontFamily)',
              boxShadow: active ? 'inset 0 0 0 1px var(--primary-border)' : 'none',
            }}
          >
            {o.label}
          </button>
        );
      })}
    </Stack>
  );
}

// Select is the viewer's dropdown. A native <select> draws its popup in OS
// chrome, which on macOS punches a light widget through the dark theme, so
// every picker here uses this listbox instead. Options are
// [{ value, label }]. trigger and menu take style overrides, because the
// chart filter is a 24px mono control and the rest are 34px.
//
// Keyboard: Enter, Space, or ArrowDown opens. Arrow keys and Home/End move.
// Enter picks, Escape closes and returns focus to the button.
export interface SelectOption {
  value: string;
  label: ReactNode;
}

interface SelectProps {
  value?: string;
  options: SelectOption[];
  onChange: (value: string) => void;
  title?: string;
  trigger?: CSSProperties;
  menu?: CSSProperties;
  id?: string;
  disabled?: boolean;
  icon?: string;
  prefix?: ReactNode;
}

export function Select({ value, options, onChange, title, trigger, menu, id, disabled, icon, prefix }: SelectProps) {
  const [open, setOpen] = useState(false);
  const [cursor, setCursor] = useState(0);
  const rootRef = useRef<HTMLDivElement>(null);
  const buttonRef = useRef<HTMLButtonElement>(null);
  const index = Math.max(
    0,
    options.findIndex((o) => o.value === value),
  );
  const selected = options[index] || options[0];

  // Opening starts the cursor on the current value rather than the top, so
  // ArrowDown steps away from where the user already is.
  const openMenu = () => {
    setCursor(index);
    setOpen(true);
  };
  const close = (refocus: boolean) => {
    setOpen(false);
    if (refocus && buttonRef.current) buttonRef.current.focus();
  };
  const pick = (option: SelectOption) => {
    onChange(option.value);
    close(true);
  };

  const onKeyDown = (e: React.KeyboardEvent<HTMLButtonElement>) => {
    if (!open) {
      if (e.key === 'Enter' || e.key === ' ' || e.key === 'ArrowDown') {
        e.preventDefault();
        openMenu();
      }
      return;
    }
    if (e.key === 'Escape') {
      e.preventDefault();
      close(true);
      return;
    }
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      if (options[cursor]) pick(options[cursor]);
      return;
    }
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      setCursor((c) => Math.min(options.length - 1, c + 1));
      return;
    }
    if (e.key === 'ArrowUp') {
      e.preventDefault();
      setCursor((c) => Math.max(0, c - 1));
      return;
    }
    if (e.key === 'Home') {
      e.preventDefault();
      setCursor(0);
      return;
    }
    if (e.key === 'End') {
      e.preventDefault();
      setCursor(options.length - 1);
    }
  };

  return (
    // biome-ignore lint/a11y/noStaticElementInteractions: Child controls own focus; the wrapper closes the menu.
    <div
      ref={rootRef}
      role="presentation"
      style={{ position: 'relative', flex: '0 0 auto' }}
      onBlur={(e) => {
        if (!rootRef.current?.contains(e.relatedTarget)) setOpen(false);
      }}
    >
      <button
        ref={buttonRef}
        type="button"
        id={id}
        title={title}
        disabled={disabled}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? close(false) : openMenu())}
        onKeyDown={onKeyDown}
        style={{
          height: 34,
          minWidth: 132,
          padding: '0 10px',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          background: 'rgba(24,27,31,0.78)',
          color: disabled ? 'var(--fg3)' : 'var(--fg1)',
          fontSize: 13,
          fontFamily: 'var(--fontFamily)',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'space-between',
          gap: 8,
          cursor: disabled ? 'not-allowed' : 'pointer',
          textAlign: 'left',
          opacity: disabled ? 0.6 : 1,
          ...trigger,
        }}
      >
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 8,
            minWidth: 0,
            overflow: 'hidden',
          }}
        >
          {icon && <Icon name={icon} size={14} style={{ color: 'var(--fg3)' }} />}
          {prefix && <span style={{ color: 'var(--fg3)' }}>{prefix}</span>}
          <span
            style={{
              minWidth: 0,
              overflow: 'hidden',
              textOverflow: 'ellipsis',
              whiteSpace: 'nowrap',
            }}
          >
            {selected ? selected.label : ''}
          </span>
        </span>
        <Icon name="chevron" size={13} style={{ color: 'var(--fg3)', flex: 'none' }} />
      </button>
      {open && !disabled && (
        <div
          role="listbox"
          tabIndex={-1}
          style={{
            position: 'absolute',
            top: 'calc(100% + 5px)',
            left: 0,
            zIndex: 30,
            minWidth: '100%',
            maxHeight: 280,
            overflowY: 'auto',
            padding: 4,
            border: '1px solid var(--border-strong)',
            borderRadius: 2,
            background: 'var(--bg-secondary)',
            boxShadow: '0 12px 34px rgba(0,0,0,0.48)',
            ...menu,
          }}
        >
          {options.map((o, i) => {
            const isSelected = o.value === selected?.value;
            return (
              <button
                key={o.value}
                type="button"
                role="option"
                aria-selected={isSelected}
                onMouseDown={(e) => e.preventDefault()}
                onMouseEnter={() => setCursor(i)}
                onClick={() => pick(o)}
                style={{
                  width: '100%',
                  minHeight: 30,
                  display: 'flex',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  gap: 10,
                  padding: '0 9px',
                  border: 'none',
                  borderRadius: 5,
                  background: i === cursor ? ACTIVE_PILL_BG : 'transparent',
                  color: isSelected ? 'var(--primary-text)' : 'var(--fg1)',
                  fontSize: 12,
                  fontFamily: 'var(--fontFamily)',
                  cursor: 'pointer',
                  textAlign: 'left',
                  whiteSpace: 'nowrap',
                }}
              >
                <span>{o.label}</span>
                {isSelected && <Icon name="check" size={12} />}
              </button>
            );
          })}
        </div>
      )}
    </div>
  );
}
