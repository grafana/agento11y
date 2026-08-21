import type React from 'react';
import { useState } from 'react';
import { modelDot, shortModel } from './formatters';
import { isPlainLeftClick } from './routing';
import { forwardChipMeta } from './settings-model';
import type { ConfigResponse } from './types';

// The agent-name helpers. They live here because every view that shows an
// agent uses them, and because a leaf module keeps detail.tsx and shell.tsx
// out of an import cycle.

export function agentShort(name: string | null | undefined): string {
  if (!name) return 'main';
  const slash = name.indexOf('/');
  return slash === -1 ? name : name.slice(slash + 1);
}

export function isSubagent(name: string | null | undefined): boolean {
  return !!name && name.indexOf('/') !== -1;
}

export function agentColor(name: string | null | undefined): string {
  const short = agentShort(name);
  if (!isSubagent(name)) return 'var(--brand-orange)';
  if (short.includes('explore')) return 'var(--viz-blue)';
  if (short.includes('general')) return 'var(--viz-purple)';
  if (short.includes('fork')) return 'var(--viz-green)';
  return 'var(--viz-yellow)';
}

// ============================================================
// Shell primitives
// ============================================================

interface IconProps {
  name: string;
  size?: number;
  style?: React.CSSProperties;
  className?: string;
}

export function Icon({ name, size = 16, style, className }: IconProps) {
  const paths: Record<string, React.ReactNode> = {
    search: <path d="M11 19a8 8 0 1 1 5.3-2L21 21M11 19a8 8 0 0 0 5.3-2L11 19Z" />,
    chevron: <path d="m6 9 6 6 6-6" />,
    cright: <path d="m9 6 6 6-6 6" />,
    clock: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 7v5l3 2" />
      </>
    ),
    bolt: <path d="M13 2 4 14h7l-1 8 9-12h-7l1-8Z" />,
    coin: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M9 9h5a2 2 0 0 1 0 4H9v-4Zm0 4v3m3-7v10" />
      </>
    ),
    swap: <path d="M7 7h13l-3-3M17 17H4l3 3" />,
    refresh: <path d="M3 12a9 9 0 0 1 15.5-6.3L21 8M21 3v5h-5M21 12a9 9 0 0 1-15.5 6.3L3 16M3 21v-5h5" />,
    book: <path d="M4 4h7a3 3 0 0 1 3 3v13a3 3 0 0 0-3-3H4V4ZM20 4h-3a3 3 0 0 0-3 3v13a3 3 0 0 1 3-3h3V4Z" />,
    bookopen: <path d="M12 6c-2-1.3-4.5-2-7-2v13c2.5 0 5 .7 7 2 2-1.3 4.5-2 7-2V4c-2.5 0-5 .7-7 2Zm0 0v13" />,
    box: <path d="M3 7.5 12 3l9 4.5v9L12 21l-9-4.5v-9Zm0 0 9 4.5m0 0 9-4.5m-9 4.5V21" />,
    dot: <circle cx="12" cy="12" r="4" />,
    download: <path d="M12 4v12m0 0-4-4m4 4 4-4M4 20h16" />,
    copy: <path d="M9 9h11v11H9zM4 4h11v3" />,
    list: <path d="M4 6h16M4 12h16M4 18h16" />,
    sortlines: <path d="M4 6h16M7 12h13M10 18h10" />,
    wrench: (
      <path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z" />
    ),
    alert: (
      <>
        <path d="M12 9v4" />
        <circle cx="12" cy="16.5" r="0.6" fill="currentColor" />
        <path d="M10.3 4.1 2.7 17.4a2 2 0 0 0 1.7 3h15.2a2 2 0 0 0 1.7-3L13.7 4.1a2 2 0 0 0-3.4 0Z" />
      </>
    ),
    empty: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M8 12h8" />
      </>
    ),
    extlink: <path d="M7 17 17 7M9 7h8v8" />,
    shield: <path d="M12 3 5 6v6c0 4 3 6.5 7 9 4-2.5 7-5 7-9V6l-7-3Z" />,
    shieldcheck: (
      <>
        <path d="M12 3 5 6v6c0 4 3 6.5 7 9 4-2.5 7-5 7-9V6l-7-3Z" />
        <path d="m9 12 2 2 4-4" />
      </>
    ),
    plus: <path d="M12 5v14M5 12h14" />,
    play: <path d="M7 5v14l11-7-11-7Z" />,
    pen: <path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z" />,
    trash: <path d="M4 7h16M9 7V5h6v2M6 7l1 13h10l1-13" />,
    close: <path d="M6 6l12 12M18 6 6 18" />,
    check: <path d="m5 13 4 4L19 7" />,
    info: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="M12 11v5" />
        <circle cx="12" cy="8" r="0.6" fill="currentColor" />
      </>
    ),
    ban: (
      <>
        <circle cx="12" cy="12" r="9" />
        <path d="m6 6 12 12" />
      </>
    ),
    key: (
      <>
        <circle cx="8" cy="15" r="3.2" />
        <path d="m10.3 12.7 8-8M16 5l2.5 2.5M13.5 7.5 16 10" />
      </>
    ),
    times: <path d="M6 6l12 12M18 6 6 18" />,
    cloud: <path d="M7 18a4 4 0 0 1-.5-7.97 5 5 0 0 1 9.6-1.37A3.5 3.5 0 0 1 16.5 18H7Z" />,
    sparkle: <path d="M12 3l1.5 5L18 9.5l-5 1.5L12 16l-1.5-5.5L5 9.5 10.5 8 12 3Z" />,
  };
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={1.6}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      aria-hidden="true"
      focusable="false"
      style={{ flexShrink: 0, display: 'block', ...(style || {}) }}
    >
      {paths[name]}
    </svg>
  );
}

// GrafanaMark is the official Grafana logo (single path from
// simple-icons) rendered in the Grafana brand orange. currentColor
// wiring lets a parent override the colour without re-pasting the
// path.
interface GrafanaMarkProps {
  size?: number;
  color?: string;
}

function GrafanaMark({ size = 22, color = 'var(--brand-orange)' }: GrafanaMarkProps) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      aria-label="Grafana"
      role="img"
      style={{ flexShrink: 0, display: 'block', color }}
    >
      <path
        fill="currentColor"
        d="M23.02 10.59a8.578 8.578 0 0 0-.862-3.034 8.911 8.911 0 0 0-1.789-2.445c.337-1.342-.413-2.505-.413-2.505-1.292-.08-2.113.4-2.416.62-.052-.02-.102-.044-.154-.064-.22-.089-.446-.172-.677-.247-.231-.073-.47-.14-.711-.197a9.867 9.867 0 0 0-.875-.161C14.557.753 12.94 0 12.94 0c-1.804 1.145-2.147 2.744-2.147 2.744l-.018.093c-.098.029-.2.057-.298.088-.138.042-.275.094-.413.143-.138.055-.275.107-.41.166a8.869 8.869 0 0 0-1.557.87l-.063-.029c-2.497-.955-4.716.195-4.716.195-.203 2.658.996 4.33 1.235 4.636a11.608 11.608 0 0 0-.607 2.635C1.636 12.677.953 15.014.953 15.014c1.926 2.214 4.171 2.351 4.171 2.351.003-.002.006-.002.006-.005.285.509.615.994.986 1.446.156.19.32.371.488.548-.704 2.009.099 3.68.099 3.68 2.144.08 3.553-.937 3.849-1.173a9.784 9.784 0 0 0 3.164.501h.08l.055-.003.107-.002.103-.005.003.002c1.01 1.44 2.788 1.646 2.788 1.646 1.264-1.332 1.337-2.653 1.337-2.94v-.058c0-.02-.003-.039-.003-.06.265-.187.52-.387.758-.6a7.875 7.875 0 0 0 1.415-1.7c1.43.083 2.437-.885 2.437-.885-.236-1.49-1.085-2.216-1.264-2.354l-.018-.013-.016-.013a.217.217 0 0 1-.031-.02c.008-.092.016-.18.02-.27.011-.162.016-.323.016-.48v-.253l-.005-.098-.008-.135a1.891 1.891 0 0 0-.01-.13c-.003-.042-.008-.083-.013-.125l-.016-.124-.018-.122a6.215 6.215 0 0 0-2.032-3.73 6.015 6.015 0 0 0-3.222-1.46 6.292 6.292 0 0 0-.85-.048l-.107.002h-.063l-.044.003-.104.008a4.777 4.777 0 0 0-3.335 1.695c-.332.4-.592.84-.768 1.297a4.594 4.594 0 0 0-.312 1.817l.003.091c.005.055.007.11.013.164a3.615 3.615 0 0 0 .698 1.82 3.53 3.53 0 0 0 1.827 1.282c.33.098.66.14.971.137.039 0 .078 0 .114-.002l.063-.003c.02 0 .041-.003.062-.003.034-.002.065-.007.099-.01.007 0 .018-.003.028-.003l.031-.005.06-.008a1.18 1.18 0 0 0 .112-.02c.036-.008.072-.013.109-.024a2.634 2.634 0 0 0 .914-.415c.028-.02.056-.041.085-.065a.248.248 0 0 0 .039-.35.244.244 0 0 0-.309-.06l-.078.042c-.09.044-.184.083-.283.116a2.476 2.476 0 0 1-.475.096c-.028.003-.054.006-.083.006l-.083.002c-.026 0-.054 0-.08-.002l-.102-.006h-.012l-.024.006c-.016-.003-.031-.003-.044-.006-.031-.002-.06-.007-.091-.01a2.59 2.59 0 0 1-.724-.213 2.557 2.557 0 0 1-.667-.438 2.52 2.52 0 0 1-.805-1.475 2.306 2.306 0 0 1-.029-.444l.006-.122v-.023l.002-.031c.003-.021.003-.04.005-.06a3.163 3.163 0 0 1 1.352-2.29 3.12 3.12 0 0 1 .937-.43 2.946 2.946 0 0 1 .776-.101h.06l.07.002.045.003h.026l.07.005a4.041 4.041 0 0 1 1.635.49 3.94 3.94 0 0 1 1.602 1.662 3.77 3.77 0 0 1 .397 1.414l.005.076.003.075c.002.026.002.05.002.075 0 .024.003.052 0 .07v.065l-.002.073-.008.174a6.195 6.195 0 0 1-.08.639 5.1 5.1 0 0 1-.267.927 5.31 5.31 0 0 1-.624 1.13 5.052 5.052 0 0 1-3.237 2.014 4.82 4.82 0 0 1-.649.066l-.039.003h-.287a6.607 6.607 0 0 1-1.716-.265 6.776 6.776 0 0 1-3.4-2.274 6.75 6.75 0 0 1-.746-1.15 6.616 6.616 0 0 1-.714-2.596l-.005-.083-.002-.02v-.056l-.003-.073v-.096l-.003-.104v-.07l.003-.163c.008-.22.026-.45.054-.678a8.707 8.707 0 0 1 .28-1.355c.128-.444.286-.872.473-1.277a7.04 7.04 0 0 1 1.456-2.1 5.925 5.925 0 0 1 .953-.763c.169-.111.343-.213.524-.306.089-.05.182-.091.273-.135.047-.02.093-.042.138-.062a7.177 7.177 0 0 1 .714-.267l.145-.045c.049-.015.098-.026.148-.041.098-.029.197-.052.296-.076.049-.013.1-.02.15-.033l.15-.032.151-.028.076-.013.075-.01.153-.024c.057-.01.114-.013.171-.023l.169-.021c.036-.003.073-.008.106-.01l.073-.008.036-.003.042-.002c.057-.003.114-.008.171-.01l.086-.006h.023l.037-.003.145-.007a7.999 7.999 0 0 1 1.708.125 7.917 7.917 0 0 1 2.048.68 8.253 8.253 0 0 1 1.672 1.09l.09.077.089.078c.06.052.114.107.171.159.057.052.112.106.166.16.052.055.107.107.159.164a8.671 8.671 0 0 1 1.41 1.978c.012.026.028.052.04.078l.04.078.075.156c.023.051.05.1.07.153l.065.15a8.848 8.848 0 0 1 .45 1.34.19.19 0 0 0 .201.142.186.186 0 0 0 .172-.184c.01-.246.002-.532-.024-.856z"
      />
    </svg>
  );
}

function Wordmark() {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 9,
        userSelect: 'none',
      }}
    >
      <GrafanaMark size={22} />
      <span
        style={{
          fontFamily: 'var(--fontFamily)',
          fontSize: 15,
          fontWeight: 600,
          letterSpacing: '-0.01em',
          color: 'var(--fg-max)',
          whiteSpace: 'nowrap',
        }}
      >
        Grafana Agent Observability
      </span>
    </div>
  );
}

interface ModelPillProps {
  name: string;
  dot?: string;
}

export function ModelPill({ name, dot }: ModelPillProps) {
  const color = dot || modelDot(name);
  return (
    <span
      title={name}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: 6,
        minWidth: 0,
        maxWidth: '100%',
        padding: '2px 8px',
        border: '1px solid var(--border-medium)',
        borderRadius: 2,
        background: 'rgba(204,204,220,0.02)',
        color: 'var(--fg1)',
        fontSize: 12,
        fontFamily: 'var(--fontFamilyMonospace)',
        whiteSpace: 'nowrap',
      }}
    >
      <span
        style={{
          width: 7,
          height: 7,
          borderRadius: '50%',
          background: color,
          flexShrink: 0,
        }}
      />
      <span
        style={{
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
        }}
      >
        {shortModel(name)}
      </span>
    </span>
  );
}

interface AgentPillProps {
  name: string;
  size?: 'sm' | 'md';
}

export function AgentPill({ name, size }: AgentPillProps) {
  const full = String(name || '');
  if (!full) return null;
  const sm = size === 'sm';
  return (
    <span
      title={full}
      style={{
        display: 'inline-flex',
        alignItems: 'center',
        gap: sm ? 4 : 5,
        padding: sm ? '1px 6px' : '1px 7px',
        border: '1px solid var(--border-medium)',
        borderRadius: 2,
        background: 'rgba(204,204,220,0.04)',
        color: 'var(--fg1)',
        fontSize: sm ? 10 : 11,
        fontFamily: 'var(--fontFamilyMonospace)',
        whiteSpace: 'nowrap',
      }}
    >
      <svg
        width={sm ? 9 : 10}
        height={sm ? 9 : 10}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        aria-hidden="true"
        focusable="false"
      >
        <circle cx="12" cy="8" r="4" />
        <path d="M4 21a8 8 0 0 1 16 0" />
      </svg>
      {agentShort(full)}
    </span>
  );
}

// agentHosts reduces the agents list to its distinct host launchers —
// the part before the first "/". "claude-code", "claude-code/explore" and
// "claude-code/general-purpose" all collapse to "claude-code"; the
// subagent breakdown lives in the ⊂N badge and the detail view. Built to
// distinguish hosts once we capture cursor / codex / copilot / opencode /
// pi sessions alongside claude-code.
export function agentHosts(agents?: string[] | null): string[] {
  // split always yields a first element, which the index signature cannot know.
  return [...new Set((agents || []).map((a) => String(a).split('/')[0] as string).filter(Boolean))];
}

interface AgentCellProps {
  agents?: string[] | null;
}

export function AgentCell({ agents }: AgentCellProps) {
  const hosts = agentHosts(agents);
  return (
    <div
      style={{
        display: 'flex',
        gap: 6,
        alignItems: 'center',
        flexWrap: 'wrap',
        minWidth: 0,
      }}
    >
      {hosts.map((h) => (
        <span
          key={h}
          title={h}
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 5,
            padding: '1px 7px',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            background: 'rgba(204,204,220,0.04)',
            color: 'var(--fg1)',
            fontSize: 11,
            fontFamily: 'var(--fontFamilyMonospace)',
            whiteSpace: 'nowrap',
          }}
        >
          <svg
            width={10}
            height={10}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="2"
            aria-hidden="true"
            focusable="false"
          >
            <circle cx="12" cy="8" r="4" />
            <path d="M4 21a8 8 0 0 1 16 0" />
          </svg>
          {h}
        </span>
      ))}
    </div>
  );
}

// ModelCell shows a conversation's models as compact pills, capped at two
// with a "+N" overflow, so a multi-model run never wraps into the next
// column. The full list is in the title.
interface ModelCellProps {
  models?: string[] | null;
}

export function ModelCell({ models }: ModelCellProps) {
  const list = models || [];
  const shown = list.slice(0, 2);
  const extra = list.length - shown.length;
  return (
    <div
      style={{
        display: 'flex',
        gap: 6,
        alignItems: 'center',
        flexWrap: 'nowrap',
        minWidth: 0,
        overflow: 'hidden',
      }}
    >
      {shown.map((m) => (
        <ModelPill key={m} name={m} />
      ))}
      {extra > 0 && (
        <span
          title={list.join(', ')}
          style={{
            fontSize: 11,
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
          }}
        >
          +{extra}
        </span>
      )}
    </div>
  );
}

export const iconBtn: React.CSSProperties = {
  width: 28,
  height: 28,
  display: 'inline-flex',
  alignItems: 'center',
  justifyContent: 'center',
  background: 'transparent',
  border: '1px solid transparent',
  color: 'var(--fg2)',
  cursor: 'pointer',
  borderRadius: 2,
};

// NavTab is one top-nav section link (Sessions / Settings). The active
// tab carries the brand underline bar; the others are muted and hover to
// full white.
interface NavTabProps {
  label: React.ReactNode;
  href: string;
  active?: boolean;
  onClick?: (e: React.MouseEvent<HTMLAnchorElement>) => void;
}

function NavTab({ label, href, active, onClick }: NavTabProps) {
  return (
    <a
      href={href}
      onClick={(e) => {
        if (!isPlainLeftClick(e)) return;
        e.preventDefault();
        onClick?.(e);
      }}
      style={{
        position: 'relative',
        display: 'inline-flex',
        alignItems: 'center',
        alignSelf: 'stretch',
        padding: '0 2px',
        fontFamily: 'var(--fontFamily)',
        fontSize: 13,
        color: active ? 'var(--fg-max)' : 'var(--fg2)',
        textDecoration: 'none',
        whiteSpace: 'nowrap',
        cursor: 'pointer',
      }}
      onMouseEnter={(e) => {
        if (!active) e.currentTarget.style.color = 'var(--fg-max)';
      }}
      onMouseLeave={(e) => {
        if (!active) e.currentTarget.style.color = 'var(--fg2)';
      }}
    >
      {label}
      {active && (
        <span
          style={{
            position: 'absolute',
            left: 0,
            right: 0,
            bottom: 0,
            height: 2,
            background: 'var(--brandVertical)',
            borderRadius: 1,
          }}
        />
      )}
    </a>
  );
}

// HEADER_H is the sticky top-bar height. Sub-bars (breadcrumb, section
// tabs) and the sticky left rail offset themselves by this so they sit
// flush under the header.
export const HEADER_H = 68;

interface TopBarTab {
  key: string;
  label: React.ReactNode;
  href: string;
  onClick?: (e: React.MouseEvent<HTMLAnchorElement>) => void;
}

interface TopBarProps {
  tabs?: TopBarTab[];
  activeTab?: string;
  config: ConfigResponse | null;
  onOpenSettings?: (tab: string) => void;
}

export function TopBar({ tabs = [], activeTab, config, onOpenSettings }: TopBarProps) {
  return (
    <header
      style={{
        height: HEADER_H,
        background: 'var(--bg-primary)',
        display: 'flex',
        alignItems: 'center',
        padding: '0 16px',
        gap: 20,
        position: 'sticky',
        top: 0,
        zIndex: 5,
      }}
    >
      <Wordmark />
      <div
        style={{
          width: 1,
          height: 28,
          background: 'var(--border-weak)',
          margin: '0 4px',
        }}
      />
      <nav
        style={{
          display: 'flex',
          alignItems: 'center',
          alignSelf: 'stretch',
          gap: 18,
          minWidth: 0,
          flex: 1,
          overflow: 'hidden',
        }}
      >
        {tabs.map((t) => (
          <NavTab key={t.key} label={t.label} href={t.href} active={t.key === activeTab} onClick={t.onClick} />
        ))}
      </nav>
      <ForwardModeChip config={config} onOpenSettings={onOpenSettings} />
    </header>
  );
}

// ForwardModeChip states what the daemon would send to Cloud, on every
// view. The daemon is shared, so this is machine-wide policy for every
// later --local session, not a property of the sessions on screen.
//
// It is read-only: changing the posture means a full config.env write,
// which the Cloud settings tab owns, so the chip navigates there instead.
// The tooltip is disclosure text and holds nothing interactive, which is
// why hover and focus are enough to open it.
interface ForwardModeChipProps {
  config: ConfigResponse | null;
  onOpenSettings?: (tab: string) => void;
}

function ForwardModeChip({ config, onOpenSettings }: ForwardModeChipProps) {
  const [open, setOpen] = useState(false);
  const meta = forwardChipMeta(config);
  // The chip names itself "Mode Local", which does not say what that means.
  // The disclosure sentence is the point of the chip, so aria-describedby
  // ties it to the button: focus opens the tooltip, and a screen reader
  // then reads the sentence out.
  const tipID = 'sigil-forward-chip-tip';
  return (
    <div style={{ position: 'relative', flexShrink: 0 }}>
      <button
        type="button"
        onClick={() => onOpenSettings?.('cloud')}
        onFocus={() => setOpen(true)}
        onBlur={() => setOpen(false)}
        aria-label={`${meta.kicker}: ${meta.value}. Open the forwarding settings.`}
        aria-describedby={open ? tipID : undefined}
        onMouseEnter={(e) => {
          setOpen(true);
          e.currentTarget.style.background = 'var(--action-hover)';
        }}
        onMouseLeave={(e) => {
          setOpen(false);
          e.currentTarget.style.background = 'rgba(24,27,31,0.78)';
        }}
        style={{
          display: 'inline-flex',
          alignItems: 'center',
          gap: 8,
          height: 30,
          padding: '0 9px 0 10px',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          background: 'rgba(24,27,31,0.78)',
          fontFamily: 'var(--fontFamily)',
          cursor: 'pointer',
          whiteSpace: 'nowrap',
        }}
      >
        <Icon name="cloud" size={14} style={{ color: meta.color }} />
        <span
          className="sigil-chip-kicker"
          style={{
            fontSize: 10.5,
            textTransform: 'uppercase',
            letterSpacing: 0.6,
            color: 'var(--fg3)',
          }}
        >
          {meta.kicker}
        </span>
        <span
          style={{
            fontSize: 12.5,
            fontWeight: 600,
            color: meta.color,
          }}
        >
          {meta.value}
        </span>
        <Icon name="cright" size={12} style={{ color: 'var(--fg3)' }} />
      </button>
      {open && (
        <div
          id={tipID}
          role="note"
          style={{
            position: 'absolute',
            top: 38,
            right: 0,
            zIndex: 40,
            width: 340,
            padding: '12px 14px',
            background: 'var(--bg-secondary)',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            boxShadow: 'var(--shadow-z2)',
          }}
        >
          <div
            style={{
              fontSize: 10.5,
              textTransform: 'uppercase',
              letterSpacing: 0.6,
              color: 'var(--fg3)',
              marginBottom: 6,
            }}
          >
            What leaves this machine
          </div>
          <div
            style={{
              fontSize: 12.5,
              lineHeight: 1.5,
              color: 'var(--fg2)',
            }}
          >
            {meta.line}
          </div>
          <div
            style={{
              marginTop: 10,
              paddingTop: 9,
              borderTop: '1px solid var(--border-weak)',
              fontSize: 11.5,
              color: 'var(--fg3)',
            }}
          >
            Click to change the forwarding mode in Settings, Cloud tab.
          </div>
        </div>
      )}
    </div>
  );
}
