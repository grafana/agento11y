import type { CSSProperties, ReactNode } from 'react';
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react';
import { markdownURL } from './detail';
import { HistoryDatePicker, localDateStartISO } from './history-date-picker';
import type { NoticeKind } from './notices';
import {
  ACTIVE_PILL_BG,
  ModalFrame,
  Notice,
  PageHero,
  PageShell,
  PillToggle,
  Select,
  SURFACE_BG,
  SurfaceCard,
} from './notices';
import { fieldInput } from './routing';
import {
  cloneSettings,
  cloudConfigured,
  forwardChipMeta,
  guardStatusMeta,
  isThemePreference,
  Mono,
  pendingEdits,
  sameSettings,
} from './settings-model';
import { Icon } from './shell';
import type {
  ConfigResponse,
  ForwardStatus,
  HistoryAgent,
  HistoryOffer,
  HistoryPlan,
  ImportRun,
  Settings,
  Tag,
  ThemePreference,
} from './types';

// ============================================================
// History import — backfill sessions an agent recorded before
// agento11y was installed.
//
// Every agent name, label, and alias comes from
// GET /api/v1/history/agents, so registering an importer in Go makes it
// appear here with no change to this file.
// ============================================================

// ImportRunView is a run as the viewer holds it: the reply to the start
// request names only the run, and the SSE frames that follow fill in the
// counters.
export interface ImportRunView extends Partial<ImportRun> {
  run_id: string;
  agent: string;
  status: string;
}

/** The reply to POST /api/v1/history:import. */
interface ImportStartResponse {
  run_id: string;
  status?: string;
}

/** What useHistoryImport hands its callers. */
export interface HistoryImport {
  agents: HistoryAgent[];
  offers: HistoryOffer[];
  run: ImportRunView | null;
  error: string | null;
  start: (agent: string, body?: Record<string, unknown>) => Promise<ImportStartResponse | null>;
  /** Fire and forget: no caller reads the reply, and a network error is swallowed. */
  cancel: () => Promise<unknown>;
  dismiss: (agent: string) => Promise<void>;
  reloadOffers: () => Promise<void>;
}

export function importRunIsActive(run: { status?: string } | null | undefined): boolean {
  return !!run && (run.status === 'pending' || run.status === 'running');
}

// useHistoryImport owns the import state the banner and the Settings card
// both read: the registered agents, the per-agent offer, and the run in
// flight. Progress arrives on the shared SSE stream, so `run` here is
// updated by the App and passed back in.
export function useHistoryImport(liveRun: ImportRunView | null | undefined): HistoryImport {
  const [agents, setAgents] = useState<HistoryAgent[]>([]);
  const [offers, setOffers] = useState<HistoryOffer[]>([]);
  const [run, setRun] = useState<ImportRunView | null>(null);
  const [error, setError] = useState<string | null>(null);

  const loadAgents = useCallback(() => {
    return fetch('/api/v1/history/agents')
      .then((r) =>
        r.ok
          ? (r.json() as Promise<{ agents?: HistoryAgent[] } | null>)
          : Promise.reject(new Error(`HTTP ${r.status}`)),
      )
      .then((b) => setAgents(b?.agents || []))
      .catch(() => setAgents([]));
  }, []);

  const loadOffers = useCallback(() => {
    return fetch('/api/v1/history/offer')
      .then((r) =>
        r.ok
          ? (r.json() as Promise<{ offers?: HistoryOffer[] } | null>)
          : Promise.reject(new Error(`HTTP ${r.status}`)),
      )
      .then((b) => setOffers(b?.offers || []))
      .catch(() => setOffers([]));
  }, []);

  useEffect(() => {
    loadAgents();
    loadOffers();
  }, [loadAgents, loadOffers]);

  // A run event for the run this hook started replaces its state. An event
  // for another run (a second viewer tab, the CLI) is adopted too: there
  // is one import at a time, so it is the one to show.
  useEffect(() => {
    if (!liveRun) return;
    setRun(liveRun);
    if (liveRun.status === 'completed') loadOffers();
  }, [liveRun, loadOffers]);

  const start = useCallback((agent: string, body: Record<string, unknown> = {}) => {
    setError(null);
    return fetch('/api/v1/history:import', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ agent, ...body }),
    })
      .then((r) =>
        r.ok
          ? (r.json() as Promise<ImportStartResponse>)
          : r.text().then((t) => Promise.reject(new Error(t.trim() || `HTTP ${r.status}`))),
      )
      .then((b) => {
        // The server starts the run before it answers, so an SSE frame for
        // this run can arrive first. Keep it: overwriting it with "pending"
        // strands the viewer on a run that already finished.
        setRun((prev) =>
          prev && prev.run_id === b.run_id
            ? prev
            : {
                run_id: b.run_id,
                agent,
                status: b.status || 'pending',
              },
        );
        return b;
      })
      .catch((e) => {
        setError(String(e.message || e));
        return null;
      });
  }, []);

  const cancel = useCallback(() => {
    if (!run?.run_id) return Promise.resolve();
    return fetch(`/api/v1/history/runs/${encodeURIComponent(run.run_id)}:cancel`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: '{}',
    }).catch(() => {});
  }, [run]);

  const dismiss = useCallback(
    (agent: string) => {
      setError(null);
      return (
        fetch('/api/v1/history/offer:dismiss', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify(agent ? { agent } : {}),
        })
          .then((r) =>
            r.ok
              ? r.json().catch(() => ({}))
              : r.text().then((t) => Promise.reject(new Error(t.trim() || `HTTP ${r.status}`))),
          )
          .then(() => loadOffers())
          // The dismissal is written to a file, so it can fail. Saying so beats
          // a banner that comes back with no explanation.
          .catch((e) => {
            setError(`Could not dismiss the import offer: ${String(e.message || e)}`);
          })
      );
    },
    [loadOffers],
  );

  return {
    agents,
    offers,
    run,
    error,
    start,
    cancel,
    dismiss,
    reloadOffers: loadOffers,
  };
}

function formatImportTurns(offer: Pick<HistoryOffer, 'turns' | 'approx_turns'>): string {
  const turns = offer.turns || 0;
  const count = `${turns.toLocaleString()} turn${turns === 1 ? '' : 's'}`;
  return offer.approx_turns ? `about ${count}` : count;
}

interface HistoryImportBannerProps {
  history: HistoryImport;
  onOpenSettings?: (tab: string) => void;
}

// HistoryImportBanner offers a backfill when discovery found sessions the
// store does not have yet. Its text is metadata only: session counts and
// turn counts, never a prompt or a title.
export function HistoryImportBanner({ history, onOpenSettings }: HistoryImportBannerProps) {
  const offer = (history.offers || []).find((o) => o.show);
  const run = history.run;
  if (run && importRunIsActive(run)) {
    return <HistoryImportProgress run={run} onCancel={history.cancel} />;
  }
  if (!offer) return null;
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '10px 14px',
        marginBottom: 14,
        borderRadius: 2,
        border: '1px solid var(--info-border)',
        background: 'var(--info-transparent)',
      }}
    >
      <Icon name="clock" size={15} style={{ color: 'var(--info-text)', flex: 'none' }} />
      <span
        style={{
          fontSize: 10.5,
          textTransform: 'uppercase',
          letterSpacing: 0.6,
          color: 'var(--fg3)',
          flex: 'none',
        }}
      >
        Existing history
      </span>
      <span
        style={{
          fontSize: 12.5,
          color: 'var(--fg2)',
          minWidth: 0,
          overflow: 'hidden',
          textOverflow: 'ellipsis',
          whiteSpace: 'nowrap',
        }}
      >
        {offer.display_name} wrote {offer.sessions} session
        {offer.sessions === 1 ? '' : 's'} ({formatImportTurns(offer)}) to this machine in the last 90 days. Importing
        adds the ones this viewer does not have yet, and sends nothing to Grafana Cloud.
      </span>
      {history.error && (
        <span
          style={{
            fontSize: 12,
            color: 'var(--error-text)',
            flex: 'none',
          }}
        >
          {history.error}
        </span>
      )}
      <span style={{ flex: 1 }} />
      <button
        type="button"
        onClick={() => history.start(offer.agent)}
        style={{
          flex: 'none',
          background: 'var(--primary-main)',
          border: '1px solid var(--primary-main)',
          borderRadius: 2,
          color: '#fff',
          fontSize: 11.5,
          fontFamily: 'var(--fontFamily)',
          padding: '3px 9px',
          cursor: 'pointer',
        }}
      >
        Import
      </button>
      <button
        type="button"
        onClick={() => onOpenSettings?.('history')}
        style={{
          flex: 'none',
          background: 'transparent',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          color: 'var(--fg2)',
          fontSize: 11.5,
          fontFamily: 'var(--fontFamily)',
          padding: '3px 9px',
          cursor: 'pointer',
        }}
      >
        Options
      </button>
      <button
        type="button"
        onClick={() => history.dismiss('')}
        style={{
          flex: 'none',
          background: 'transparent',
          border: '1px solid transparent',
          borderRadius: 2,
          color: 'var(--fg3)',
          fontSize: 11.5,
          fontFamily: 'var(--fontFamily)',
          padding: '3px 9px',
          cursor: 'pointer',
        }}
      >
        Not now
      </button>
    </div>
  );
}

// importSessionLabel says how far a run has got, in sessions. A run that has
// not finished discovery has no total to count against, so it says what it
// is doing rather than reporting "0 of 0".
function importSessionLabel(run: ImportRunView): string {
  const done = run.sessions || 0;
  const total = run.selected || 0;
  if (total > 0) return `${done} of ${total} sessions`;
  return importRunIsActive(run) ? 'Scanning sessions…' : `${done} sessions`;
}

interface ImportProgressBarProps {
  done: number;
  total: number;
  style?: CSSProperties;
}

// ImportProgressBar shows the share of selected sessions a run has finished.
// The banner and the Settings card both draw it, so the two cannot disagree
// about what the bar measures. It moves when a session finishes, so a run
// over a few large sessions advances in visible steps rather than smoothly.
function ImportProgressBar({ done, total, style }: ImportProgressBarProps) {
  const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
  return (
    <div
      style={{
        height: 4,
        borderRadius: 999,
        background: 'var(--border-weak)',
        overflow: 'hidden',
        ...style,
      }}
    >
      <div
        style={{
          width: `${pct}%`,
          height: '100%',
          background: 'var(--primary-main)',
        }}
      />
    </div>
  );
}

interface HistoryImportProgressProps {
  run: ImportRunView;
  onCancel: () => void;
}

// HistoryImportProgress renders a run's progress as it arrives over SSE.
// Progress is counted in sessions: turn counts belong in the summary, where
// they cannot be mistaken for the number of sessions the run was given.
function HistoryImportProgress({ run, onCancel }: HistoryImportProgressProps) {
  const done = run.sessions || 0;
  const total = run.selected || 0;
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 12,
        padding: '10px 14px',
        marginBottom: 14,
        borderRadius: 2,
        border: '1px solid var(--info-border)',
        background: 'var(--info-transparent)',
      }}
    >
      <Icon name="clock" size={15} style={{ color: 'var(--info-text)', flex: 'none' }} />
      <span
        style={{
          fontSize: 10.5,
          textTransform: 'uppercase',
          letterSpacing: 0.6,
          color: 'var(--fg3)',
          flex: 'none',
        }}
      >
        Importing {run.agent}
      </span>
      <span style={{ fontSize: 12.5, color: 'var(--fg2)', flex: 'none' }}>
        {importSessionLabel(run)}
        {run.failed ? ` · ${run.failed} failed turns` : ''}
      </span>
      <ImportProgressBar done={done} total={total} style={{ flex: 1 }} />
      <button
        type="button"
        onClick={onCancel}
        style={{
          flex: 'none',
          background: 'transparent',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          color: 'var(--fg2)',
          fontSize: 11.5,
          fontFamily: 'var(--fontFamily)',
          padding: '3px 9px',
          cursor: 'pointer',
        }}
      >
        Cancel
      </button>
    </div>
  );
}

// ForwardLocalMode is the segmented value the forwarding control shows;
// ForwardCaptureMode is the pair a capture mode reduces to.
type ForwardLocalMode = 'off' | 'metadata_only' | 'full';
type ForwardCaptureMode = Exclude<ForwardLocalMode, 'off'>;

// ForwardModeSettings is the pair of persisted keys the forwarding control
// spans.
interface ForwardModeSettings {
  localForward: boolean;
  capture: string;
}

// captureForwardMode reports which of the two segmented values a
// CONTENT_CAPTURE_MODE forwards as. Everything that is not "full" is
// reduced to metadata by the forwarder, so the advanced modes
// (no_tool_content, full_with_metadata_spans) read as metadata_only.
function captureForwardMode(capture: string): ForwardCaptureMode {
  return capture === 'full' ? 'full' : 'metadata_only';
}

// forwardLocalMode collapses the two persisted keys the forwarding control
// spans (LOCAL_FORWARD and CONTENT_CAPTURE_MODE) into the one segmented
// value the UI shows.
function forwardLocalMode(form: ForwardModeSettings): ForwardLocalMode {
  if (!form.localForward) return 'off';
  return captureForwardMode(form.capture);
}

// forwardLocalPatch returns the write the segmented value means, spanning
// both keys the control covers, so a caller can send it in the same PUT as
// the rest of the form. capture is only rewritten when the mode already set
// forwards differently, so an advanced mode set in config.env survives a
// round-trip through the toggle. null means the requested mode is the one
// already shown, which matters because the segmented control fires for the
// active option.
export function forwardLocalPatch(form: ForwardModeSettings, mode: string): Partial<Settings> | null {
  if (mode === forwardLocalMode(form)) return null;
  if (mode === 'off') return { localForward: false };
  const patch: Partial<Settings> = { localForward: true };
  if (captureForwardMode(form.capture) !== mode) patch.capture = mode;
  return patch;
}

interface ToggleProps {
  checked: boolean;
  onChange: (checked: boolean) => void;
}

function Toggle({ checked, onChange }: ToggleProps) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      onClick={() => onChange(!checked)}
      style={{
        position: 'relative',
        width: 38,
        height: 22,
        borderRadius: 9999,
        border: 'none',
        cursor: 'pointer',
        padding: 0,
        flexShrink: 0,
        background: checked ? 'var(--primary-main)' : 'var(--toggle-off-bg)',
        transition: 'background .15s',
      }}
    >
      <span
        style={{
          position: 'absolute',
          top: 3,
          left: 3,
          width: 16,
          height: 16,
          borderRadius: '50%',
          background: 'var(--toggle-knob-bg)',
          boxShadow: 'var(--toggle-knob-shadow)',
          transform: checked ? 'translateX(16px)' : 'translateX(0)',
          transition: 'transform .15s',
        }}
      />
    </button>
  );
}

interface MonoInputProps {
  value: string;
  onChange: (value: string) => void;
  placeholder?: string;
  width?: CSSProperties['width'];
  align?: CSSProperties['textAlign'];
  type?: string;
}

function MonoInput({ value, onChange, placeholder, width, align, type }: MonoInputProps) {
  return (
    <input
      type={type || 'text'}
      value={value}
      placeholder={placeholder}
      onChange={(e) => onChange(e.target.value)}
      onFocus={(e) => (e.currentTarget.style.borderColor = 'var(--primary-border)')}
      onBlur={(e) => (e.currentTarget.style.borderColor = 'var(--border-medium)')}
      style={{
        height: 32,
        width: width || 'auto',
        background: 'var(--mono-input-bg)',
        border: '1px solid var(--border-medium)',
        borderRadius: 2,
        color: 'var(--fg1)',
        padding: '0 10px',
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 12,
        textAlign: align || 'left',
        outline: 'none',
      }}
    />
  );
}

interface ButtonProps {
  onClick: () => void;
  children?: ReactNode;
  disabled?: boolean;
}

function PrimaryButton({ onClick, children, disabled }: ButtonProps) {
  return (
    <button
      type="button"
      disabled={disabled}
      onClick={onClick}
      onMouseEnter={(e) => {
        if (disabled) return;
        e.currentTarget.style.background = 'var(--primary-shade)';
        e.currentTarget.style.borderColor = 'var(--primary-shade)';
      }}
      onMouseLeave={(e) => {
        if (disabled) return;
        e.currentTarget.style.background = 'var(--primary-main)';
        e.currentTarget.style.borderColor = 'var(--primary-main)';
      }}
      style={{
        height: 32,
        padding: '0 14px',
        background: disabled ? 'var(--disabled-control-bg)' : 'var(--primary-main)',
        border: `1px solid ${disabled ? 'transparent' : 'var(--primary-main)'}`,
        color: disabled ? 'var(--fg3)' : '#fff',
        borderRadius: 2,
        fontSize: 13,
        fontWeight: 500,
        cursor: disabled ? 'not-allowed' : 'pointer',
      }}
    >
      {children}
    </button>
  );
}

function GhostButton({ onClick, children }: ButtonProps) {
  return (
    <button
      type="button"
      onClick={onClick}
      onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--action-hover)')}
      onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
      style={{
        height: 32,
        padding: '0 14px',
        background: 'transparent',
        border: '1px solid var(--secondary-border)',
        color: 'var(--fg1)',
        borderRadius: 2,
        fontSize: 13,
        cursor: 'pointer',
      }}
    >
      {children}
    </button>
  );
}

interface SettingsCardProps {
  children?: ReactNode;
  style?: CSSProperties;
}

function SettingsCard({ children, style }: SettingsCardProps) {
  return (
    <SurfaceCard
      style={{
        padding: '4px 20px 12px',
        marginBottom: 16,
        ...(style || {}),
      }}
    >
      {children}
    </SurfaceCard>
  );
}

interface SectionLabelProps {
  children?: ReactNode;
}

function SectionLabel({ children }: SectionLabelProps) {
  return (
    <div
      style={{
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        padding: '16px 0 2px',
      }}
    >
      <span
        style={{
          width: 18,
          height: 2,
          borderRadius: 999,
          background: 'var(--brand-orange)',
        }}
      />
      <span
        style={{
          fontSize: 11,
          fontWeight: 700,
          letterSpacing: '.08em',
          textTransform: 'uppercase',
          color: 'var(--fg3)',
        }}
      >
        {children}
      </span>
    </div>
  );
}

interface SettingRowProps {
  label: ReactNode;
  help?: ReactNode;
  children?: ReactNode;
  full?: boolean;
}

// SettingRow is one label/help + control line inside a card. `full` stacks
// the control under the label for wide controls (the tags editor).
function SettingRow({ label, help, children, full }: SettingRowProps) {
  const left = (
    <div style={{ minWidth: 0 }}>
      <div style={{ fontSize: 14, fontWeight: 500, color: 'var(--fg1)' }}>{label}</div>
      {help && (
        <div
          style={{
            fontSize: 12,
            lineHeight: 1.5,
            color: 'var(--fg3)',
            maxWidth: 460,
            marginTop: 4,
          }}
        >
          {help}
        </div>
      )}
    </div>
  );
  if (full) {
    return (
      <div
        style={{
          padding: '16px 0',
          borderTop: '1px solid var(--border-weak)',
        }}
      >
        {left}
        <div style={{ marginTop: 12 }}>{children}</div>
      </div>
    );
  }
  return (
    <div
      style={{
        display: 'flex',
        justifyContent: 'space-between',
        alignItems: 'flex-start',
        gap: 32,
        padding: '16px 0',
        borderTop: '1px solid var(--border-weak)',
      }}
    >
      {left}
      <div style={{ flexShrink: 0 }}>{children}</div>
    </div>
  );
}

interface PreviewBodyProps {
  text: string;
}

// PreviewBody renders the rendered config.env with key/value colouring:
// comments and `=` are dimmed, keys are blue, values green.
function PreviewBody({ text }: PreviewBodyProps) {
  const lines = (text || '').split('\n');
  if (lines.length && lines[lines.length - 1] === '') lines.pop();
  return (
    <div
      style={{
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 12,
        lineHeight: 1.9,
        whiteSpace: 'pre-wrap',
        wordBreak: 'break-all',
      }}
    >
      {lines.map((line, i) => {
        if (line.startsWith('#'))
          return (
            <div key={i} style={{ color: 'var(--fg3)' }}>
              {line}
            </div>
          );
        const eq = line.indexOf('=');
        if (eq < 0)
          return (
            <div key={i} style={{ color: 'var(--fg1)' }}>
              {line || '\u00a0'}
            </div>
          );
        return (
          <div key={i}>
            <span style={{ color: 'var(--primary-text)' }}>{line.slice(0, eq)}</span>
            <span style={{ color: 'var(--fg3)' }}>=</span>
            <span style={{ color: 'var(--config-value-text)' }}>{line.slice(eq + 1)}</span>
          </div>
        );
      })}
    </div>
  );
}

interface UnsavedBarProps {
  onReset: () => void;
  onSave: () => void;
}

function UnsavedBar({ onReset, onSave }: UnsavedBarProps) {
  return (
    <div
      style={{
        position: 'fixed',
        left: 0,
        right: 0,
        bottom: 24,
        display: 'flex',
        justifyContent: 'center',
        pointerEvents: 'none',
        zIndex: 20,
      }}
    >
      <div
        style={{
          pointerEvents: 'auto',
          display: 'flex',
          alignItems: 'center',
          gap: 12,
          background: 'var(--settings-bar-bg)',
          border: '1px solid var(--border-medium)',
          borderRadius: 2,
          padding: '9px 12px 9px 16px',
          boxShadow: 'var(--shadow-z2)',
          animation: 'sigil-barin .16s ease-out',
        }}
      >
        <span
          style={{
            width: 7,
            height: 7,
            borderRadius: '50%',
            background: 'var(--brand-orange)',
          }}
        />
        <span style={{ fontSize: 13, color: 'var(--fg2)' }}>Unsaved changes</span>
        <GhostButton onClick={onReset}>Reset</GhostButton>
        <PrimaryButton onClick={onSave}>Save to config.env</PrimaryButton>
      </div>
    </div>
  );
}

interface SettingsHeroProps {
  dirty: boolean;
  path: string;
}

function SettingsHero({ dirty, path }: SettingsHeroProps) {
  const stats = [
    {
      label: 'Config',
      value: dirty ? 'Unsaved' : 'Synced',
      tone: dirty ? 'var(--brand-orange-text)' : 'var(--success-text)',
    },
  ];
  return (
    <PageHero
      title="Settings"
      desc={path}
      descStyle={{
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 11.5,
        maxWidth: 720,
      }}
      stats={stats}
    />
  );
}

interface SettingsTab {
  id: string;
  label: string;
  icon: string;
  desc: string;
}

interface SettingsTabRailProps {
  tabs: SettingsTab[];
  active: string;
  onChange: (id: string) => void;
}

function SettingsTabRail({ tabs, active, onChange }: SettingsTabRailProps) {
  return (
    // biome-ignore lint/a11y/useSemanticElements: These buttons navigate sections; they do not edit one grouped field.
    <div
      role="group"
      aria-label="Settings sections"
      style={{
        display: 'grid',
        gridTemplateColumns: 'repeat(auto-fit, minmax(128px, 1fr))',
        gap: 8,
        marginBottom: 16,
      }}
    >
      {tabs.map((tab) => {
        const isActive = tab.id === active;
        return (
          <button
            key={tab.id}
            type="button"
            aria-pressed={isActive}
            onClick={() => onChange(tab.id)}
            style={{
              minHeight: 76,
              textAlign: 'left',
              padding: '12px 12px',
              borderRadius: 8,
              border: `1px solid ${isActive ? 'var(--primary-border)' : 'var(--border-weak)'}`,
              background: isActive ? ACTIVE_PILL_BG : 'var(--settings-tab-bg)',
              color: 'var(--fg1)',
              cursor: 'pointer',
              boxShadow: isActive ? 'var(--settings-tab-shadow)' : 'none',
            }}
          >
            <div
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
                marginBottom: 8,
              }}
            >
              <span
                style={{
                  width: 26,
                  height: 26,
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  borderRadius: 2,
                  background: 'var(--settings-icon-bg)',
                  color: isActive ? 'var(--brand-orange-text)' : 'var(--fg2)',
                }}
              >
                <Icon name={tab.icon} size={14} />
              </span>
              <span
                style={{
                  fontSize: 13,
                  fontWeight: 600,
                  color: isActive ? 'var(--fg-max)' : 'var(--fg1)',
                }}
              >
                {tab.label}
              </span>
            </div>
            <div
              style={{
                fontSize: 11.5,
                lineHeight: 1.35,
                color: 'var(--fg3)',
              }}
            >
              {tab.desc}
            </div>
          </button>
        );
      })}
    </div>
  );
}

interface SettingsPreviewPanelProps {
  path: string;
  preview: string;
  onCopy: () => void;
}

function SettingsPreviewPanel({ path, preview, onCopy }: SettingsPreviewPanelProps) {
  return (
    <div
      style={{
        width: 'min(440px, 100%)',
        flex: '1 1 360px',
        position: 'sticky',
        top: 72,
      }}
    >
      <div
        style={{
          overflow: 'hidden',
          background: SURFACE_BG,
          border: '1px solid var(--border-weak)',
          borderRadius: 8,
          boxShadow: 'var(--settings-preview-shadow)',
        }}
      >
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 10,
            padding: '12px 14px',
            borderBottom: '1px solid var(--border-weak)',
          }}
        >
          <span
            style={{
              width: 28,
              height: 28,
              display: 'inline-flex',
              alignItems: 'center',
              justifyContent: 'center',
              borderRadius: 2,
              background: 'var(--settings-icon-bg)',
              color: 'var(--fg2)',
            }}
          >
            <Icon name="list" size={14} />
          </span>
          <div style={{ minWidth: 0, flex: 1 }}>
            <div
              style={{
                fontSize: 12,
                fontWeight: 600,
                color: 'var(--fg-max)',
              }}
            >
              config.env preview
            </div>
            <div
              style={{
                fontSize: 11,
                color: 'var(--fg3)',
                fontFamily: 'var(--fontFamilyMonospace)',
                overflow: 'hidden',
                textOverflow: 'ellipsis',
                whiteSpace: 'nowrap',
              }}
            >
              {path}
            </div>
          </div>
          <button
            type="button"
            onClick={onCopy}
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: 5,
              background: 'transparent',
              border: '1px solid var(--secondary-border)',
              color: 'var(--fg1)',
              borderRadius: 2,
              height: 28,
              padding: '0 9px',
              fontSize: 12,
              cursor: 'pointer',
            }}
            onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--action-hover)')}
            onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
          >
            <Icon name="copy" size={13} />
            Copy
          </button>
        </div>
        <div
          style={{
            background: 'var(--inset-bg)',
            padding: '14px 16px',
            maxHeight: 'calc(100vh - 252px)',
            overflow: 'auto',
          }}
        >
          <PreviewBody text={preview} />
        </div>
      </div>
    </div>
  );
}

interface ToastProps {
  message: string;
}

function Toast({ message }: ToastProps) {
  return (
    <div
      style={{
        position: 'fixed',
        top: 60,
        right: 20,
        zIndex: 30,
        display: 'flex',
        alignItems: 'center',
        gap: 8,
        background: 'var(--bg-secondary)',
        border: '1px solid var(--border-medium)',
        borderRadius: 2,
        padding: '10px 14px',
        boxShadow: 'var(--shadow-z2)',
        animation: 'sigil-tin .2s ease-out',
      }}
    >
      <Icon name="check" size={16} style={{ color: 'var(--success-text)' }} />
      <span style={{ fontSize: 13, color: 'var(--fg1)' }}>{message}</span>
    </div>
  );
}

const FORWARD_LOCAL_OPTIONS = [
  { value: 'off', label: 'Local only' },
  { value: 'metadata_only', label: 'Metadata only' },
  { value: 'full', label: 'Full' },
];
const GUARD_OPTIONS = [
  { value: 'off', label: 'Off' },
  { value: 'failopen', label: 'Fail open' },
  { value: 'failclosed', label: 'Fail closed' },
];
const THEME_OPTIONS = [
  { value: 'dark', label: 'Dark' },
  { value: 'light', label: 'Light' },
  { value: 'system', label: 'Match system' },
];
// Connecting turns forwarding on, so the connect flow offers the same modes
// without the off case.
const CONNECT_MODE_OPTIONS = FORWARD_LOCAL_OPTIONS.filter((o) => o.value !== 'off');
const SETTINGS_TABS: SettingsTab[] = [
  {
    id: 'cloud',
    label: 'Cloud',
    icon: 'cloud',
    desc: 'Ingest, auth, forwarding',
  },
  { id: 'local', label: 'Local', icon: 'box', desc: 'Tags, appearance, runtime' },
  {
    id: 'history',
    label: 'History',
    icon: 'clock',
    desc: 'Import past sessions',
  },
];
export const SETTINGS_TAB_IDS = new Set(SETTINGS_TABS.map((t) => t.id));

export function settingsTabFromLocation(): string {
  if (typeof window === 'undefined') return 'cloud';
  const params = new URLSearchParams(window.location.search || '');
  const tab = params.get('tab') || '';
  return SETTINGS_TAB_IDS.has(tab) ? tab : 'cloud';
}

export function settingsPath(tab: string): string {
  const url = new URL('/settings', typeof window !== 'undefined' ? window.location.origin : 'http://localhost');
  if (SETTINGS_TAB_IDS.has(tab) && tab !== 'cloud') url.searchParams.set('tab', tab);
  return url.pathname + url.search;
}

// urlHost reduces an ingest or OTLP URL to the host the copy names, so a
// status line stays readable. Anything that is not an http(s) URL is shown
// as typed.
function urlHost(raw: string | null | undefined): string {
  const s = String(raw || '');
  const m = s.match(/^https?:\/\/([^/]+)/);
  return m ? (m[1] ?? s) : s || '\u2014';
}

interface HostTargetProps {
  url: string;
}

// HostTarget names the stack a confirmation is about. A connection can be
// saved with a token or a tenant and no endpoint, and urlHost's fallback
// would then read "will be sent to \u2014".
function HostTarget({ url }: HostTargetProps) {
  const s = String(url || '').trim();
  if (!s) return 'your stack';
  return (
    <span
      style={{
        fontFamily: 'var(--fontFamilyMonospace)',
        fontSize: 12,
        color: 'var(--fg1)',
      }}
    >
      {urlHost(s)}
    </span>
  );
}

// ConnectBlock is what the pasted environment block yielded: the values that
// are usable, plus the keys that were dropped and why.
interface ConnectBlock {
  endpoint: string;
  tenantId: string;
  token: string;
  otlpEndpoint: string;
  otlpHeaders: string;
  placeholders: string[];
  invalid: string[];
}

/** The ConnectBlock keys one assignment can fill in. */
type ConnectBlockField = 'endpoint' | 'tenantId' | 'token' | 'otlpEndpoint' | 'otlpHeaders';

// parseConnectBlock reads the environment block the stack's setup page hands
// out. It mirrors applyPaste in internal/login/login.go: optional `export `,
// `#` comments, quoted values, AGENTO11Y_ ahead of SIGIL_ within one block,
// and the two OTEL_EXPORTER_OTLP_ variables, of which only
// OTEL_EXPORTER_OTLP_HEADERS has no branded spelling. The grammar is
// duplicated because internal/local cannot import internal/login:
// internal/login reaches internal/local through internal/doctor.
//
// A value that is not usable is dropped from the result and its key is
// recorded, so the feedback strip can name the real cause the way
// validatePastedBlock does. `placeholders` holds the keys the setup page
// still renders as <…>, which is what a block copied before the token was
// created carries; `invalid` holds a URL slot that is not an http(s) URL.
// Reporting either as a missing key would leave the user re-copying the
// same block.
export function parseConnectBlock(text: string | null | undefined): ConnectBlock {
  const fields: Record<string, { name: ConnectBlockField; url?: boolean }> = {
    ENDPOINT: { name: 'endpoint', url: true },
    AUTH_TENANT_ID: { name: 'tenantId' },
    AUTH_TOKEN: { name: 'token' },
    OTEL_EXPORTER_OTLP_ENDPOINT: { name: 'otlpEndpoint', url: true },
    OTEL_EXPORTER_OTLP_HEADERS: { name: 'otlpHeaders' },
  };
  const out: ConnectBlock = {
    endpoint: '',
    tenantId: '',
    token: '',
    otlpEndpoint: '',
    otlpHeaders: '',
    placeholders: [],
    invalid: [],
  };
  const preferred: Partial<Record<ConnectBlockField, boolean>> = {};
  String(text || '')
    .split(/\r?\n/)
    .forEach((raw) => {
      const line = raw.trim().replace(/^export\s+/, '');
      if (!line || line.startsWith('#')) return;
      const eq = line.indexOf('=');
      if (eq < 1) return;
      const key = line.slice(0, eq).trim();
      const branded = /^AGENTO11Y_/.test(key);
      const suffix = key.replace(/^(?:AGENTO11Y|SIGIL)_/, '');
      const field = fields[suffix];
      if (!field) return;
      // The AGENTO11Y_ spelling wins over SIGIL_ however the two are ordered,
      // the way brandedPaste resolves one alias family.
      if (preferred[field.name] && !branded) return;
      const value = parseBlockValue(line.slice(eq + 1));
      if (!value) return;
      if (branded) preferred[field.name] = true;
      // The key is reported as the block spells it, which is the line the user
      // has to go back and fix.
      const report = (list: string[]) => {
        out[field.name] = '';
        if (!list.includes(key)) list.push(key);
      };
      if (looksLikePlaceholder(value)) {
        report(out.placeholders);
        return;
      }
      if (field.url && !isHTTPURL(value)) {
        report(out.invalid);
        return;
      }
      out[field.name] = value;
    });
  return out;
}

// isHTTPURL applies requireURL's rule from internal/login: a usable endpoint
// parses as a URL and carries an http or https scheme.
function isHTTPURL(value: string): boolean {
  if (!/^https?:\/\//i.test(String(value || ''))) return false;
  try {
    return !!new URL(value).host;
  } catch (_) {
    return false;
  }
}

// parseBlockValue reads one assignment's value the way internal/dotenv reads
// the same line: a quoted value ends at its closing quote, an unquoted one
// ends at a trailing ` #` comment.
function parseBlockValue(raw: string): string {
  const v = raw.trim();
  if (v.length >= 2 && (v[0] === '"' || v[0] === "'")) {
    const end = v.indexOf(v[0], 1);
    if (end >= 0) return v.slice(1, end);
  }
  const hash = v.indexOf(' #');
  return (hash >= 0 ? v.slice(0, hash) : v).replace(/[ \t]+$/, '');
}

export function looksLikePlaceholder(value: string | null | undefined): boolean {
  const s = String(value || '');
  const open = s.indexOf('<');
  return open >= 0 && s.indexOf('>', open) >= 0;
}

// BARE_HOST_RE matches a value typed without a scheme, so a stack pasted as
// mystack.grafana.net or localhost:3000 still builds a link. It whitelists
// what a host can look like rather than listing what a scheme can, so
// javascript:alert(1), mailto:a@b.c and /settings keep the meaning they have
// and setupPageURL goes on rejecting them.
const BARE_HOST_RE = /^[a-z0-9][a-z0-9.-]*(:\d+)?([/?#]|$)/i;

// setupPageURL builds the setup-page link on the user's own stack, or "" when
// the typed value cannot carry one. Only the scheme and host survive: a URL
// copied from a Grafana address bar carries a path and an ?orgId=N, and
// appending the app path to those gives a 404. markdownURL alone is not
// enough either, because it passes relative paths and mailto:, and the link
// would then point at this daemon.
export function setupPageURL(raw: string | null | undefined): string {
  const typed = String(raw || '').trim();
  const href = markdownURL(BARE_HOST_RE.test(typed) ? `https://${typed}` : typed);
  if (!href || !/^https?:\/\//i.test(href)) return '';
  try {
    return `${new URL(href).origin}/a/grafana-agento11y-app/setup-coding-agent`;
  } catch (_) {
    return '';
  }
}

interface ConnectStepProps {
  n: number;
  title: ReactNode;
  help?: ReactNode;
  children?: ReactNode;
}

// ConnectStep is one numbered step of the connect flow, on the SettingRow
// rhythm: a top border, the label type, and help text under it.
function ConnectStep({ n, title, help, children }: ConnectStepProps) {
  return (
    <div
      style={{
        display: 'flex',
        gap: 14,
        padding: '18px 0',
        borderTop: '1px solid var(--border-weak)',
      }}
    >
      <span
        style={{
          flex: 'none',
          width: 24,
          height: 24,
          borderRadius: '50%',
          border: '1px solid var(--border-medium)',
          display: 'inline-flex',
          alignItems: 'center',
          justifyContent: 'center',
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 12,
          color: 'var(--fg2)',
        }}
      >
        {n}
      </span>
      <div style={{ minWidth: 0, flex: 1 }}>
        <div
          style={{
            fontSize: 14,
            fontWeight: 500,
            color: 'var(--fg1)',
          }}
        >
          {title}
        </div>
        {help && (
          <div
            style={{
              fontSize: 12,
              lineHeight: 1.5,
              color: 'var(--fg3)',
              marginTop: 4,
              maxWidth: 520,
            }}
          >
            {help}
          </div>
        )}
        {children}
      </div>
    </div>
  );
}

interface SettingsConnectFlowProps {
  savedStackURL: string;
  configPath: string;
  capture: string;
  onConnect: (parsed: ConnectBlock, mode: string) => void;
  onManual: () => void;
}

// SettingsConnectFlow replaces the credential form when nothing is saved. It
// replicates the `agento11y login` handshake: open the setup page on your
// stack, paste the block it hands back, pick what to forward.
//
// The pasted block, token included, lives in this component's state while
// the flow is open: the textarea is controlled, and parseConnectBlock re-runs
// on every render. Only the feedback strip withholds the token value.
export function SettingsConnectFlow({
  savedStackURL,
  configPath,
  capture,
  onConnect,
  onManual,
}: SettingsConnectFlowProps) {
  const [stackUrl, setStackUrl] = useState(savedStackURL || '');
  const [paste, setPaste] = useState('');
  const [draftMode, setDraftMode] = useState('metadata_only');
  const [confirmFull, setConfirmFull] = useState(false);

  const setupHref = setupPageURL(stackUrl);
  const parsed = parseConnectBlock(paste);
  const ok = !!(parsed.endpoint && parsed.tenantId && parsed.token);
  const missing = [
    !parsed.endpoint && 'AGENTO11Y_ENDPOINT',
    !parsed.tenantId && 'AGENTO11Y_AUTH_TENANT_ID',
    !parsed.token && 'AGENTO11Y_AUTH_TOKEN',
  ].filter(Boolean);
  const advanced = capture === 'no_tool_content' || capture === 'full_with_metadata_spans';
  const tone = ok ? 'success' : 'warning';
  // A dropped value is named for what is wrong with it. Reporting a
  // placeholder or a broken URL as a missing key sends the user back for the
  // same block again.
  const detail = ok
    ? `${urlHost(parsed.endpoint)} · tenant ${parsed.tenantId} · token found ${parsed.otlpEndpoint ? `· OTLP endpoint ${urlHost(parsed.otlpEndpoint)}` : '· no OTLP endpoint'}`
    : parsed.placeholders.length > 0
      ? `${parsed.placeholders.join(', ')} is still a placeholder. Fill it in on the setup page, then copy the block again.`
      : parsed.invalid.length > 0
        ? `${parsed.invalid.join(', ')} is not an http:// or https:// URL.`
        : `Missing ${missing.join(', ')}. Copy the whole block from the setup page.`;
  // Full forwarding asks first here too. Reaching it from a fresh install
  // takes two clicks otherwise, while the same widening in the connected
  // panel is confirmed.
  const submit = () => {
    if (draftMode === 'full') {
      setConfirmFull(true);
      return;
    }
    onConnect(parsed, draftMode);
  };

  return (
    <SettingsCard style={{ padding: '4px 20px 20px' }}>
      <SectionLabel>Connect to Grafana Cloud</SectionLabel>
      <div
        style={{
          fontSize: 12,
          lineHeight: 1.5,
          color: 'var(--fg3)',
          padding: '0 0 16px',
          maxWidth: 620,
        }}
      >
        Local capture keeps working with no connection. Connecting lets the daemon forward sessions to your stack, and
        writes the same credentials to <Mono>config.env</Mono> as <Mono>agento11y login</Mono>.
      </div>

      <ConnectStep n={1} title="Create a token in your stack">
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 8,
            marginTop: 12,
            flexWrap: 'wrap',
          }}
        >
          <MonoInput value={stackUrl} onChange={setStackUrl} placeholder="https://your-stack.grafana.net" width={280} />
          <a
            href={setupHref || undefined}
            target="_blank"
            rel="noreferrer"
            aria-disabled={setupHref ? undefined : 'true'}
            onClick={(e) => {
              if (!setupHref) e.preventDefault();
            }}
            style={{
              display: 'inline-flex',
              alignItems: 'center',
              gap: 6,
              height: 32,
              padding: '0 14px',
              background: setupHref ? 'var(--primary-main)' : 'var(--disabled-control-bg)',
              border: `1px solid ${setupHref ? 'var(--primary-main)' : 'transparent'}`,
              color: setupHref ? '#fff' : 'var(--fg3)',
              borderRadius: 2,
              fontSize: 13,
              fontWeight: 500,
              textDecoration: 'none',
              cursor: setupHref ? 'pointer' : 'not-allowed',
            }}
          >
            Open setup page
            <Icon name="extlink" size={12} />
          </a>
        </div>
        <div
          style={{
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 11,
            color: 'var(--fg3)',
            marginTop: 8,
            overflow: 'hidden',
            textOverflow: 'ellipsis',
            whiteSpace: 'nowrap',
          }}
        >
          {setupHref ||
            (stackUrl.trim()
              ? 'The setup link needs a stack hostname or URL.'
              : 'Enter your stack URL to build the setup link.')}
        </div>
        <div style={{ fontSize: 12, color: 'var(--fg3)', marginTop: 10 }}>
          No stack yet?{' '}
          <a
            href="https://grafana.com/auth/sign-up/create-user/?"
            target="_blank"
            rel="noreferrer"
            style={{
              color: 'var(--brand-orange-text)',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 5,
            }}
          >
            Sign up for Grafana Cloud
            <Icon name="extlink" size={11} />
          </a>
        </div>
      </ConnectStep>

      <ConnectStep
        n={2}
        title="Paste the connection settings"
        help={
          <>
            Paste the whole block. The token is stored locally in <Mono>{configPath}</Mono>
          </>
        }
      >
        <textarea
          value={paste}
          onChange={(e) => setPaste(e.target.value)}
          spellCheck={false}
          placeholder={
            'AGENTO11Y_ENDPOINT=https://agento11y-prod-eu-west-2.grafana.net\nAGENTO11Y_AUTH_TENANT_ID=1234567890\nAGENTO11Y_AUTH_TOKEN=glc_…\nOTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-eu-west-2.grafana.net/otlp'
          }
          onFocus={(e) => (e.currentTarget.style.borderColor = 'var(--primary-border)')}
          onBlur={(e) => (e.currentTarget.style.borderColor = 'var(--border-medium)')}
          style={{
            marginTop: 12,
            width: '100%',
            height: 118,
            resize: 'vertical',
            background: 'var(--bg-canvas)',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            color: 'var(--fg1)',
            padding: 10,
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 12,
            lineHeight: 1.7,
            outline: 'none',
          }}
        />
        {paste.trim() !== '' && (
          <div
            style={{
              display: 'flex',
              gap: 10,
              alignItems: 'flex-start',
              marginTop: 10,
              padding: '10px 12px',
              border: `1px solid var(--${tone}-border)`,
              background: `var(--${tone}-transparent)`,
              borderRadius: 2,
            }}
          >
            <Icon
              name={ok ? 'check' : 'alert'}
              size={15}
              style={{
                color: `var(--${tone}-text)`,
                marginTop: 2,
              }}
            />
            <div style={{ minWidth: 0 }}>
              <div
                style={{
                  fontSize: 12.5,
                  color: `var(--${tone}-text)`,
                }}
              >
                {ok ? 'Connection settings read' : "Couldn't read a complete block"}
              </div>
              <div
                style={{
                  fontFamily: 'var(--fontFamilyMonospace)',
                  fontSize: 11.5,
                  color: 'var(--fg3)',
                  marginTop: 4,
                  lineHeight: 1.7,
                  wordBreak: 'break-all',
                }}
              >
                {detail}
              </div>
            </div>
          </div>
        )}
      </ConnectStep>

      <ConnectStep
        n={3}
        title="Choose what to forward"
        help={
          <>
            The local viewer always keeps full content.{' '}
            <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Metadata only</b> forwards usage and session metadata,
            and <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Full</b> forwards prompts, responses, and tool I/O
            too.
            {advanced && (
              <div
                style={{
                  color: 'var(--warning-text)',
                  marginTop: 6,
                }}
              >
                Advanced capture mode <Mono>{capture}</Mono> is set in config.env. Sessions forward as metadata while it
                is set.{' '}
                <b
                  style={{
                    fontWeight: 500,
                    color: 'var(--fg2)',
                  }}
                >
                  Metadata only
                </b>{' '}
                keeps that value;{' '}
                <b
                  style={{
                    fontWeight: 500,
                    color: 'var(--fg2)',
                  }}
                >
                  Full
                </b>{' '}
                overwrites it, for your non-local Cloud sessions too.
              </div>
            )}
          </>
        }
      >
        <div style={{ marginTop: 12 }}>
          <PillToggle value={draftMode} onChange={setDraftMode} options={CONNECT_MODE_OPTIONS} />
        </div>
        <div
          style={{
            display: 'flex',
            alignItems: 'center',
            gap: 10,
            marginTop: 16,
            flexWrap: 'wrap',
          }}
        >
          <button
            type="button"
            disabled={!ok}
            onClick={submit}
            style={{
              height: 32,
              padding: '0 14px',
              borderRadius: 2,
              fontSize: 13,
              fontWeight: 500,
              background: ok ? 'var(--primary-main)' : 'var(--disabled-control-bg)',
              border: `1px solid ${ok ? 'var(--primary-main)' : 'transparent'}`,
              color: ok ? '#fff' : 'var(--fg3)',
              cursor: ok ? 'pointer' : 'not-allowed',
              fontFamily: 'var(--fontFamily)',
            }}
          >
            Connect
          </button>
          {ok && <span style={{ fontSize: 12, color: 'var(--fg3)' }}>Writes config.env and starts forwarding.</span>}
        </div>
      </ConnectStep>

      {/* An endpoint on its own is a valid configuration (a local collector
              needs no tenant or token), and Connect cannot produce one, so the
              credential fields stay reachable from the empty state. */}
      <div
        style={{
          fontSize: 12,
          color: 'var(--fg3)',
          paddingTop: 16,
          borderTop: '1px solid var(--border-weak)',
        }}
      >
        Pointing at a collector of your own?{' '}
        <button
          type="button"
          onClick={onManual}
          style={{
            background: 'transparent',
            border: 'none',
            padding: 0,
            font: 'inherit',
            color: 'var(--primary-text)',
            cursor: 'pointer',
            textDecoration: 'underline',
          }}
        >
          Enter the connection fields by hand
        </button>
        .
      </div>

      {confirmFull && (
        <ConfirmFullContentModal
          endpoint={parsed.endpoint}
          onCancel={() => setConfirmFull(false)}
          onConfirm={() => {
            setConfirmFull(false);
            onConnect(parsed, 'full');
          }}
        />
      )}
    </SettingsCard>
  );
}

interface ConfirmFullContentModalProps {
  endpoint: string;
  onCancel: () => void;
  onConfirm: () => void;
}

// ConfirmFullContentModal is the consent point for widening to full content,
// from either the connect flow or the configured panel. It names both scopes
// CONTENT_CAPTURE_MODE covers, because the modal is the last thing the user
// reads before the write.
function ConfirmFullContentModal({ endpoint, onCancel, onConfirm }: ConfirmFullContentModalProps) {
  return (
    <ModalFrame
      width="min(560px, 100%)"
      onClose={onCancel}
      title="Forward full session content?"
      desc="This applies to every local session on this machine and to your non-local Cloud sessions, until you change it."
    >
      <div
        style={{
          padding: '16px 18px',
          fontSize: 13,
          lineHeight: 1.6,
          color: 'var(--fg2)',
        }}
      >
        The daemon will send prompts, responses, reasoning text, tool inputs and results, and attached media to{' '}
        <HostTarget url={endpoint} />. Metadata only forwards usage and session metadata instead.
      </div>
      <div
        style={{
          display: 'flex',
          justifyContent: 'flex-end',
          gap: 10,
          padding: '14px 18px',
          borderTop: '1px solid var(--border-weak)',
        }}
      >
        <GhostButton onClick={onCancel}>Cancel</GhostButton>
        <button
          type="button"
          onClick={onConfirm}
          style={{
            height: 32,
            padding: '0 14px',
            background: 'var(--warning-main)',
            border: '1px solid var(--warning-main)',
            color: '#111217',
            borderRadius: 2,
            fontSize: 13,
            fontWeight: 500,
            fontFamily: 'var(--fontFamily)',
            cursor: 'pointer',
          }}
        >
          Forward full content
        </button>
      </div>
    </ModalFrame>
  );
}

interface SettingsCloudTabProps {
  form: Settings;
  set: (patch: Partial<Settings>) => void;
  savedEndpoint: string;
  savedGuards: string;
  config: ConfigResponse | null;
  stackUrl: string;
  configured: boolean;
  configPath: string;
  onConnect: (parsed: ConnectBlock, mode: string) => void;
  onDisconnect: () => void;
  onMode: (mode: string, forceLocalOff?: boolean) => void;
}

interface SettingsGuardsCardProps {
  form: Settings;
  savedGuards: string;
  set: (patch: Partial<Settings>) => void;
  status: ForwardStatus | null;
  localOnly: boolean;
}

export function SettingsGuardsCard({ form, savedGuards, set, status, localOnly }: SettingsGuardsCardProps) {
  const guardsConfigured = form.guards === 'failopen' || form.guards === 'failclosed';
  const savedGuardsConfigured = savedGuards === 'failopen' || savedGuards === 'failclosed';
  const guardsOn = guardsConfigured && !localOnly;
  const timeout = form.guardTimeout.trim();
  const invalidTimeout = timeout !== '' && !/^[1-9]\d*$/.test(timeout);
  const meta = guardStatusMeta(status, Date.now());

  return (
    <SettingsCard>
      <SectionLabel>Guards</SectionLabel>
      <div
        style={{
          fontSize: 12,
          lineHeight: 1.5,
          color: 'var(--fg3)',
          padding: '0 0 4px',
          maxWidth: 620,
        }}
      >
        A guard check runs before a tool call and is evaluated by your Cloud rules.
      </div>
      <SettingRow
        label="Fail mode"
        help="Fail open allows the call when Cloud cannot answer. Fail closed blocks it. Off skips guard checks."
      >
        <PillToggle
          size="md"
          value={guardsOn ? form.guards : 'off'}
          onChange={(guards) => set({ guards })}
          options={GUARD_OPTIONS}
          disabled={localOnly}
        />
      </SettingRow>
      {guardsOn && (
        <SettingRow label="Timeout" help="Maximum time to wait for a guard verdict.">
          <div style={{ textAlign: 'right' }}>
            <div style={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-end', gap: 8 }}>
              <MonoInput
                value={form.guardTimeout}
                onChange={(guardTimeout) => set({ guardTimeout })}
                placeholder="1500"
                width={100}
                align="right"
              />
              <Mono>ms</Mono>
            </div>
            {invalidTimeout && (
              <div style={{ color: 'var(--warning-text)', fontSize: 12, lineHeight: 1.5, marginTop: 6 }}>
                Enter a positive integer.
              </div>
            )}
          </div>
        </SettingRow>
      )}
      {localOnly ? (
        <div style={{ padding: '14px 0 0' }}>
          <Notice kind="info" title="Cloud guards are off for local sessions">
            Select Metadata only or Full above to use Cloud guards.
            {savedGuardsConfigured && (
              <>
                {' '}
                Non-local sessions still use the saved {savedGuards === 'failclosed' ? 'Fail closed' : 'Fail open'}{' '}
                mode.
              </>
            )}
          </Notice>
        </div>
      ) : (
        <div style={{ fontSize: 12, lineHeight: 1.5, color: 'var(--fg3)', padding: '12px 0 0' }}>
          Restart a running agent if it does not use the new guard settings.
        </div>
      )}
      {guardsOn && (
        <div
          style={{
            display: 'flex',
            gap: 10,
            alignItems: 'flex-start',
            padding: '14px 0 4px',
            maxWidth: 640,
          }}
        >
          <span
            style={{
              flex: 'none',
              width: 7,
              height: 7,
              borderRadius: '50%',
              marginTop: 6,
              background: `var(--${meta.accent}-text)`,
            }}
          />
          <div style={{ fontSize: 12.5, lineHeight: 1.55, color: 'var(--fg2)' }}>{meta.line}</div>
        </div>
      )}
    </SettingsCard>
  );
}

// savedEndpoint is the endpoint on disk, which is not form.endpoint while the
// Edit connection disclosure holds an unsaved one. onMode and onDisconnect
// write on top of the saved state, so the consent copy names savedEndpoint:
// the stack the write actually keeps forwarding to, or clears.
function SettingsCloudTab({
  form,
  set,
  savedEndpoint,
  savedGuards,
  config,
  stackUrl,
  configured,
  configPath,
  onConnect,
  onDisconnect,
  onMode,
}: SettingsCloudTabProps) {
  const [editOpen, setEditOpen] = useState(false);
  const [manual, setManual] = useState(false);
  const [confirmFull, setConfirmFull] = useState(false);
  const [confirmDisconnect, setConfirmDisconnect] = useState(false);
  const forwardStatus = config ? config.forwardStatus : null;

  if (!configured && !manual) {
    return (
      <SettingsConnectFlow
        savedStackURL={stackUrl}
        configPath={configPath}
        capture={form.capture}
        onConnect={onConnect}
        onManual={() => {
          setManual(true);
          setEditOpen(true);
        }}
      />
    );
  }

  const forwardMode = forwardLocalMode(form);
  const meta = forwardChipMeta(config);
  const advanced = form.capture === 'no_tool_content' || form.capture === 'full_with_metadata_spans';
  // The daemon prefers config.env, but it also inherits LOCAL_FORWARD into
  // its own environment at boot, so "off here, on there" is reachable until
  // an explicit false is saved.
  const daemonStillOn = !!forwardStatus?.enabled && !form.localForward;
  const localOnly = forwardMode === 'off' && !daemonStillOn;
  const failures = forwardStatus?.failures || [];
  // recentFailures outlives being turned off, so a failure list alone would
  // put an error notice under the calm "forwarding is off" line.
  const failing = !!forwardStatus?.enabled && failures.length > 0;
  // Widening is the only direction that asks. Narrowing takes effect at once,
  // because less content leaving the machine needs no consent.
  //
  // Local only is the one mode a click on the active pill still writes: while
  // the daemon forwards from its own environment, config.env needs an
  // explicit false to override it, and there is no pending edit to save it
  // with.
  const requestMode = (mode: string) => {
    if (mode === 'full' && forwardMode !== 'full') {
      setConfirmFull(true);
      return;
    }
    onMode(mode, mode === 'off' && daemonStillOn);
  };

  const forwardingCard = (
    <SettingsCard style={{ padding: '4px 20px 20px' }}>
      <SectionLabel>Cloud forwarding</SectionLabel>
      <div
        style={{
          fontSize: 12,
          lineHeight: 1.5,
          color: 'var(--fg3)',
          padding: '0 0 4px',
          maxWidth: 620,
        }}
      >
        What the daemon sends to your stack for every <Mono>--local</Mono> session on this machine.
      </div>

      <div
        style={{
          padding: '16px 0',
          borderTop: '1px solid var(--border-weak)',
          marginTop: 12,
        }}
      >
        <PillToggle size="md" value={forwardMode} onChange={requestMode} options={FORWARD_LOCAL_OPTIONS} />
        <div
          style={{
            fontSize: 12,
            lineHeight: 1.5,
            color: 'var(--fg3)',
            marginTop: 10,
            maxWidth: 620,
          }}
        >
          The local viewer always keeps full content.{' '}
          <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Metadata only</b> forwards usage and session metadata, and{' '}
          <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Full</b> forwards prompts, responses, and tool I/O too.
          {advanced && (
            <div
              style={{
                color: 'var(--warning-text)',
                marginTop: 6,
              }}
            >
              Advanced capture mode <Mono>{form.capture}</Mono> is set in config.env. Sessions forward as metadata while
              it is set. <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Metadata only</b> keeps that value;{' '}
              <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Full</b> overwrites it, for your non-local Cloud
              sessions too.
            </div>
          )}
          {daemonStillOn && (
            <div
              style={{
                color: 'var(--warning-text)',
                marginTop: 6,
              }}
            >
              The running daemon is still forwarding: <Mono>LOCAL_FORWARD</Mono> is set in its environment. config.env
              overrides that, but only once it holds an explicit <Mono>false</Mono>.{' '}
              <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Local only</b> is already selected here, so click it
              to write that <Mono>false</Mono>.
            </div>
          )}
        </div>
        {failing ? (
          <div style={{ marginTop: 14 }}>
            <Notice kind="error">{meta.line}</Notice>
          </div>
        ) : (
          <div
            style={{
              display: 'flex',
              gap: 10,
              alignItems: 'flex-start',
              marginTop: 14,
              maxWidth: 640,
            }}
          >
            <span
              style={{
                flex: 'none',
                width: 7,
                height: 7,
                borderRadius: '50%',
                marginTop: 6,
                background: meta.color,
              }}
            />
            <div
              style={{
                fontSize: 12.5,
                lineHeight: 1.55,
                color: 'var(--fg2)',
              }}
            >
              {meta.line}
            </div>
          </div>
        )}
      </div>

      <SettingRow label={configured ? 'Connected stack' : 'Connection'} help={null}>
        <button
          type="button"
          onClick={() => setEditOpen((v) => !v)}
          style={{
            background: 'transparent',
            border: '1px solid var(--border-medium)',
            borderRadius: 2,
            color: 'var(--fg2)',
            fontSize: 11.5,
            fontFamily: 'var(--fontFamily)',
            padding: '4px 10px',
            cursor: 'pointer',
          }}
        >
          {editOpen ? 'Hide connection' : 'Edit connection'}
        </button>
      </SettingRow>
      <div
        style={{
          fontFamily: 'var(--fontFamilyMonospace)',
          fontSize: 11.5,
          color: 'var(--fg3)',
          marginTop: -8,
          lineHeight: 1.8,
          wordBreak: 'break-all',
        }}
      >
        <div>{form.endpoint || 'no endpoint'}</div>
        <div>
          {form.tenantId ? `tenant ${form.tenantId}` : 'no tenant'}
          {form.tokenSet && !form.tokenCleared ? ' · token configured (0600)' : ' · no token'}
          {form.otlpEndpoint ? ` · otlp ${urlHost(form.otlpEndpoint)}` : ''}
        </div>
      </div>

      {editOpen && (
        <>
          <SettingRow label="Endpoint" help={<>Grafana AI Observability ingest URL.</>}>
            <MonoInput
              value={form.endpoint}
              onChange={(v) => set({ endpoint: v })}
              placeholder="https://agento11y-prod-….grafana.net"
              width={320}
            />
          </SettingRow>
          <SettingRow label="Tenant ID" help={null}>
            <MonoInput value={form.tenantId} onChange={(v) => set({ tenantId: v })} placeholder="123456" width={200} />
          </SettingRow>
          <SettingRow label="Auth token" help={null}>
            {form.tokenSet && !form.tokenCleared && form.token === '' ? (
              <div
                style={{
                  display: 'inline-flex',
                  alignItems: 'center',
                  gap: 8,
                }}
              >
                <input
                  value=""
                  disabled
                  placeholder="configured"
                  style={{
                    height: 32,
                    width: 200,
                    background: 'var(--bg-canvas)',
                    border: '1px solid var(--border-medium)',
                    borderRadius: 2,
                    color: 'var(--fg3)',
                    padding: '0 10px',
                    fontFamily: 'var(--fontFamilyMonospace)',
                    fontSize: 12,
                    cursor: 'not-allowed',
                  }}
                />
                <GhostButton onClick={() => set({ tokenCleared: true, token: '' })}>Reset</GhostButton>
              </div>
            ) : (
              <MonoInput
                type="password"
                value={form.token}
                onChange={(v) =>
                  set({
                    token: v,
                    tokenCleared: form.tokenSet && v === '',
                  })
                }
                placeholder={form.tokenSet ? 'new token, or blank to remove' : 'glc_…'}
                width={260}
              />
            )}
          </SettingRow>
          <SettingRow label="OTLP endpoint" help={<>For SDK traces and metrics.</>}>
            <MonoInput
              value={form.otlpEndpoint}
              onChange={(v) => set({ otlpEndpoint: v })}
              placeholder="https://otlp-gateway-….grafana.net/otlp"
              width={320}
            />
          </SettingRow>
          {/* Nothing to disconnect from in the by-hand branch, which is
                  reached with no connection saved. */}
          {configured && (
            <SettingRow label="Disconnect" help={<>Clears the saved credentials and stops all forwarding.</>}>
              <button
                type="button"
                onClick={() => setConfirmDisconnect(true)}
                style={{
                  height: 32,
                  padding: '0 14px',
                  background: 'transparent',
                  border: '1px solid var(--error-border)',
                  color: 'var(--error-text)',
                  borderRadius: 2,
                  fontSize: 13,
                  fontFamily: 'var(--fontFamily)',
                  cursor: 'pointer',
                }}
              >
                Disconnect
              </button>
            </SettingRow>
          )}
        </>
      )}
    </SettingsCard>
  );

  return (
    <>
      {forwardingCard}
      <SettingsGuardsCard
        form={form}
        savedGuards={savedGuards}
        set={set}
        status={forwardStatus}
        localOnly={localOnly}
      />

      {confirmFull && (
        <ConfirmFullContentModal
          endpoint={savedEndpoint}
          onCancel={() => setConfirmFull(false)}
          onConfirm={() => {
            setConfirmFull(false);
            onMode('full');
          }}
        />
      )}

      {confirmDisconnect && (
        <ModalFrame
          width="min(560px, 100%)"
          onClose={() => setConfirmDisconnect(false)}
          title="Disconnect from Grafana Cloud?"
          desc="Forwarding stops and the saved credentials are removed from config.env."
        >
          <div
            style={{
              padding: '16px 18px',
              fontSize: 13,
              lineHeight: 1.6,
              color: 'var(--fg2)',
            }}
          >
            The endpoint, tenant ID, auth token and OTLP settings for <HostTarget url={savedEndpoint} /> are deleted, so
            your non-local Cloud sessions stop reaching the stack too, until you connect again or run{' '}
            <Mono>agento11y login</Mono>. Sessions already captured stay in the local store, and{' '}
            <Mono>CONTENT_CAPTURE_MODE</Mono> is left as it is.
          </div>
          <div
            style={{
              display: 'flex',
              justifyContent: 'flex-end',
              gap: 10,
              padding: '14px 18px',
              borderTop: '1px solid var(--border-weak)',
            }}
          >
            <GhostButton onClick={() => setConfirmDisconnect(false)}>Cancel</GhostButton>
            <button
              type="button"
              onClick={() => {
                setConfirmDisconnect(false);
                setEditOpen(false);
                setManual(false);
                onDisconnect();
              }}
              style={{
                height: 32,
                padding: '0 14px',
                background: 'transparent',
                border: '1px solid var(--error-border)',
                color: 'var(--error-text)',
                borderRadius: 2,
                fontSize: 13,
                fontFamily: 'var(--fontFamily)',
                cursor: 'pointer',
              }}
            >
              Disconnect
            </button>
          </div>
        </ModalFrame>
      )}
    </>
  );
}

interface SettingsTagsEditorProps {
  tags: Tag[];
  setTag: (index: number, patch: Partial<Tag>) => void;
  addTag: () => void;
  removeTag: (index: number) => void;
}

function SettingsTagsEditor({ tags, setTag, addTag, removeTag }: SettingsTagsEditorProps) {
  return (
    <SettingsCard>
      <SectionLabel>Session tags</SectionLabel>
      <SettingRow
        full
        label="Tags"
        help={
          <>
            Applied to every generation as <Mono>key=value</Mono>. Empty pairs are dropped on save.
          </>
        }
      >
        <div style={{ display: 'flex', flexDirection: 'column', gap: 8 }}>
          {tags.map((t, i) => (
            <div
              key={i}
              style={{
                display: 'flex',
                alignItems: 'center',
                gap: 8,
              }}
            >
              <MonoInput value={t.key} onChange={(v) => setTag(i, { key: v })} placeholder="key" width={200} />
              <span
                style={{
                  color: 'var(--fg3)',
                  fontFamily: 'var(--fontFamilyMonospace)',
                }}
              >
                =
              </span>
              <MonoInput value={t.value} onChange={(v) => setTag(i, { value: v })} placeholder="value" width={200} />
              <button
                type="button"
                onClick={() => removeTag(i)}
                title="Remove tag"
                aria-label="Remove tag"
                style={{
                  width: 28,
                  height: 28,
                  display: 'inline-flex',
                  alignItems: 'center',
                  justifyContent: 'center',
                  background: 'transparent',
                  border: '1px solid transparent',
                  color: 'var(--fg3)',
                  cursor: 'pointer',
                  borderRadius: 2,
                }}
                onMouseEnter={(e) => (e.currentTarget.style.color = 'var(--fg1)')}
                onMouseLeave={(e) => (e.currentTarget.style.color = 'var(--fg3)')}
              >
                <Icon name="times" size={14} />
              </button>
            </div>
          ))}
          <button
            type="button"
            onClick={addTag}
            style={{
              alignSelf: 'flex-start',
              display: 'inline-flex',
              alignItems: 'center',
              gap: 6,
              height: 30,
              padding: '0 12px',
              background: 'transparent',
              border: '1px dashed var(--border-medium)',
              borderRadius: 2,
              color: 'var(--fg2)',
              fontSize: 13,
              cursor: 'pointer',
            }}
            onMouseEnter={(e) => (e.currentTarget.style.borderColor = 'var(--border-strong)')}
            onMouseLeave={(e) => (e.currentTarget.style.borderColor = 'var(--border-medium)')}
          >
            <Icon name="plus" size={13} />
            Add tag
          </button>
        </div>
      </SettingRow>
    </SettingsCard>
  );
}

interface SettingsAppearanceCardProps {
  theme: ThemePreference;
  onChange: (theme: ThemePreference) => void;
}

export function SettingsAppearanceCard({ theme, onChange }: SettingsAppearanceCardProps) {
  return (
    <SettingsCard>
      <SectionLabel>Appearance</SectionLabel>
      <SettingRow
        label="Theme"
        help={
          <>
            Applies to this viewer only. <Mono>Match system</Mono> follows your OS setting and switches without a
            reload. Press <Mono>c</Mono>, then <Mono>t</Mono> to switch between dark and light.
          </>
        }
      >
        <PillToggle
          size="md"
          value={theme}
          onChange={(value) => {
            if (isThemePreference(value)) onChange(value);
          }}
          options={THEME_OPTIONS}
        />
      </SettingRow>
    </SettingsCard>
  );
}

interface SettingsLocalTabProps {
  form: Settings;
  set: (patch: Partial<Settings>) => void;
  setTag: (index: number, patch: Partial<Tag>) => void;
  addTag: () => void;
  removeTag: (index: number) => void;
}

function SettingsLocalTab({ form, set, setTag, addTag, removeTag }: SettingsLocalTabProps) {
  return (
    <>
      <SettingsTagsEditor tags={form.tags} setTag={setTag} addTag={addTag} removeTag={removeTag} />
      <SettingsAppearanceCard theme={form.theme} onChange={(theme) => set({ theme })} />
      <SettingsCard>
        <SectionLabel>Runtime</SectionLabel>
        <SettingRow
          label="Debug logging"
          help={
            <>
              Write a verbose log to <Mono>~/.local/state/agento11y/logs/agento11y.log</Mono>.
            </>
          }
        >
          <Toggle checked={form.debug} onChange={(v) => set({ debug: v })} />
        </SettingRow>
        <SettingRow
          label="Automatic updates"
          help={<>Keep host agent plugins refreshed automatically. Turn off to pin the current versions.</>}
        >
          <Toggle checked={form.autoUpdate} onChange={(v) => set({ autoUpdate: v })} />
        </SettingRow>
      </SettingsCard>
      <SettingsCard>
        <SectionLabel>Identity (optional)</SectionLabel>
        <SettingRow
          label="User ID"
          help={<>Override the resolved user id used to attribute generations. Leave blank to auto-resolve.</>}
        >
          <MonoInput value={form.userId} onChange={(v) => set({ userId: v })} placeholder="auto" width={260} />
        </SettingRow>
      </SettingsCard>
    </>
  );
}

interface SettingsHistoryTabProps {
  history: HistoryImport;
}

type HistoryPlanRequest =
  | { key: string; status: 'idle' | 'loading' }
  | { key: string; status: 'ready'; plan: HistoryPlan }
  | { key: string; status: 'error'; error: string };

function historyPlanKey(agent: string, sinceDate: string): string {
  return `${agent}\u0000${sinceDate}`;
}

// Planning reads metadata only, so opening this tab never reads session content.
export function SettingsHistoryTab({ history }: SettingsHistoryTabProps) {
  const agents = history.agents || [];
  const [agent, setAgent] = useState('');
  const [sinceDate, setSinceDate] = useState('');
  const [starting, setStarting] = useState(false);
  const [planRequest, setPlanRequest] = useState<HistoryPlanRequest>({ key: '', status: 'idle' });
  const planSeqRef = useRef(0);

  const selected = agent || agents[0]?.id || '';
  const selectionKey = historyPlanKey(selected, sinceDate);
  const currentPlanRequest = planRequest.key === selectionKey ? planRequest : null;
  const plan = currentPlanRequest?.status === 'ready' ? currentPlanRequest.plan : null;
  const planError = currentPlanRequest?.status === 'error' ? currentPlanRequest.error : null;
  const loadingPlan = !!selected && (!currentPlanRequest || currentPlanRequest.status === 'loading');
  const run = history.run;
  const active = importRunIsActive(run);
  const controlsLocked = active || starting;
  const refreshedRunRef = useRef(run && !active ? run.run_id : '');

  const loadPlan = useCallback((id: string, date: string) => {
    const seq = ++planSeqRef.current;
    const key = historyPlanKey(id, date);
    if (!id) {
      setPlanRequest({ key, status: 'idle' });
      return;
    }
    setPlanRequest({ key, status: 'loading' });
    const params = new URLSearchParams({ agent: id });
    const since = localDateStartISO(date);
    if (since) params.set('since', since);
    fetch(`/api/v1/history/plan?${params}`)
      .then((r) =>
        r.ok
          ? (r.json() as Promise<HistoryPlan>)
          : r.text().then((t) => Promise.reject(new Error(t.trim() || `HTTP ${r.status}`))),
      )
      .then((body) => {
        if (planSeqRef.current === seq) setPlanRequest({ key, status: 'ready', plan: body });
      })
      .catch((error) => {
        if (planSeqRef.current !== seq) return;
        setPlanRequest({ key, status: 'error', error: String(error.message || error) });
      });
  }, []);

  useEffect(() => {
    loadPlan(selected, sinceDate);
  }, [selected, sinceDate, loadPlan]);
  useEffect(() => {
    if (!run || active || refreshedRunRef.current === run.run_id) return;
    refreshedRunRef.current = run.run_id;
    loadPlan(selected, sinceDate);
  }, [run, active, selected, sinceDate, loadPlan]);

  const sessions = plan?.sessions || [];
  const turns = sessions.reduce((n, session) => n + (session.turn_count || 0), 0);
  const approx = sessions.some((session) => session.approx_turns);
  const importDisabled = !selected || loadingPlan || !plan || !!planError || starting;
  const datePickerDisabled = controlsLocked || !selected || (!sinceDate && loadingPlan);

  return (
    <SettingsCard style={{ overflow: 'visible' }}>
      <SectionLabel>Import past sessions</SectionLabel>
      <div
        style={{
          fontSize: 12,
          lineHeight: 1.5,
          color: 'var(--fg3)',
          padding: '0 0 10px',
        }}
      >
        Backfill sessions an agent recorded before agento11y was installed. The import writes to the local store on this
        machine. The daemon never relays it to Grafana Cloud, whatever{' '}
        <b style={{ fontWeight: 500, color: 'var(--fg2)' }}>Cloud forwarding</b> on the Cloud tab is set to.
      </div>
      <SettingRow label="Agent" help={<>Only agents with an importer are listed.</>}>
        <Select
          value={selected}
          onChange={setAgent}
          disabled={controlsLocked}
          trigger={{
            ...fieldInput,
            width: 220,
            display: 'inline-flex',
          }}
          options={agents.map((item) => ({
            value: item.id,
            label: item.display_name || item.id,
          }))}
        />
      </SettingRow>
      <SettingRow label="Start date">
        <HistoryDatePicker
          value={sinceDate}
          effectiveSince={plan?.since}
          onChange={setSinceDate}
          disabled={datePickerDisabled}
        />
      </SettingRow>
      <SettingRow
        label="Available"
        help={
          <>
            Sessions active on or after the start date are included. Each matching session is imported in full,
            including turns before that date. Sessions still in progress are left out.
          </>
        }
      >
        <div
          style={{
            fontSize: 13,
            color: 'var(--fg1)',
            textAlign: 'right',
          }}
        >
          {loadingPlan ? (
            'Scanning…'
          ) : planError ? (
            <span style={{ color: 'var(--error-text)' }}>{planError}</span>
          ) : (
            `${sessions.length} sessions · ${approx ? 'about ' : ''}${turns.toLocaleString()} turns`
          )}
        </div>
      </SettingRow>
      <SettingRow
        label="Import"
        help={<>Re-running an import skips turns already recorded, so it is safe to repeat.</>}
      >
        {active ? (
          <GhostButton onClick={history.cancel}>Cancel import</GhostButton>
        ) : (
          <PrimaryButton
            disabled={importDisabled}
            onClick={() => {
              if (!plan) return;
              setStarting(true);
              const body = sinceDate ? { since: plan.since } : {};
              void history.start(selected, body).finally(() => setStarting(false));
            }}
          >
            {starting ? 'Starting…' : `Import ${sessions.length} sessions`}
          </PrimaryButton>
        )}
      </SettingRow>
      {history.error && (
        <div style={{ padding: '0 0 12px' }}>
          <Notice kind="error" title="Could not start the import">
            {history.error}
          </Notice>
        </div>
      )}
      {run && <HistoryImportStatus run={run} />}
    </SettingsCard>
  );
}

interface HistoryImportStatusProps {
  run: ImportRunView;
}

/** The Notice a run's status maps onto. */
interface ImportStatusTone {
  kind: NoticeKind;
  title: string;
}

// HistoryImportStatus reports a run in sessions while it runs, and adds the
// turn totals once it stops. A session holds many model turns, so showing
// both at once invites reading the turn count as a session count.
function HistoryImportStatus({ run }: HistoryImportStatusProps) {
  // The daemon's status set is open, so the lookup is keyed by string and
  // the `||` branch covers a status this viewer does not know.
  const tone: ImportStatusTone = (
    {
      completed: { kind: 'info', title: 'Import finished' },
      failed: { kind: 'error', title: 'Import failed' },
      cancelled: { kind: 'warning', title: 'Import cancelled' },
    } as Record<string, ImportStatusTone>
  )[run.status] || { kind: 'info', title: 'Import running' };
  const done = run.sessions || 0;
  const total = run.selected || 0;
  const detail = { fontSize: 12, color: 'var(--fg3)' };
  return (
    <div style={{ padding: '0 0 12px' }}>
      <Notice kind={tone.kind} title={tone.title}>
        <div>
          {importSessionLabel(run)}
          {run.missing ? ` · ${run.missing} no longer on disk` : ''}
        </div>
        {importRunIsActive(run) ? (
          <ImportProgressBar done={done} total={total} style={{ marginTop: 6, marginBottom: 2 }} />
        ) : (
          <div style={detail}>
            <span
              style={{
                display: 'flex',
                flexWrap: 'wrap',
                gap: '2px 12px',
              }}
            >
              <span>{(run.imported || 0).toLocaleString()} turns imported</span>
              <span>{(run.skipped || 0).toLocaleString()} already imported</span>
              <span>{(run.failed || 0).toLocaleString()} failed</span>
            </span>
          </div>
        )}
        {run.error && <div style={{ fontSize: 12 }}>{run.error}</div>}
      </Notice>
    </div>
  );
}

interface SettingsTabPanelsProps {
  activeSettingsTab: string;
  form: Settings;
  set: (patch: Partial<Settings>) => void;
  savedEndpoint: string;
  savedGuards: string;
  setTag: (index: number, patch: Partial<Tag>) => void;
  addTag: () => void;
  removeTag: (index: number) => void;
  config: ConfigResponse | null;
  stackUrl: string;
  configured: boolean;
  configPath: string;
  onConnect: (parsed: ConnectBlock, mode: string) => void;
  onDisconnect: () => void;
  onMode: (mode: string, forceLocalOff?: boolean) => void;
  history: HistoryImport;
}

function SettingsTabPanels({
  activeSettingsTab,
  form,
  set,
  savedEndpoint,
  savedGuards,
  setTag,
  addTag,
  removeTag,
  config,
  stackUrl,
  configured,
  configPath,
  onConnect,
  onDisconnect,
  onMode,
  history,
}: SettingsTabPanelsProps) {
  return (
    <>
      {activeSettingsTab === 'cloud' && (
        <SettingsCloudTab
          form={form}
          set={set}
          savedEndpoint={savedEndpoint}
          savedGuards={savedGuards}
          config={config}
          stackUrl={stackUrl}
          configured={configured}
          configPath={configPath}
          onConnect={onConnect}
          onDisconnect={onDisconnect}
          onMode={onMode}
        />
      )}
      {activeSettingsTab === 'local' && (
        <SettingsLocalTab form={form} set={set} setTag={setTag} addTag={addTag} removeTag={removeTag} />
      )}
      {activeSettingsTab === 'history' && <SettingsHistoryTab history={history} />}
    </>
  );
}

interface SettingsViewProps {
  history: HistoryImport;
  config: ConfigResponse | null;
  configError: string | null;
  activeSettingsTab: string;
  onSelectTab: (tab: string) => void;
  onConfig: (config: ConfigResponse) => void;
  onThemePreview?: (theme: ThemePreference | null) => void;
}

// SettingsView edits config.env. It does not fetch it: App() polls
// /api/v1/config for the header chip, and this view hydrates from the same
// response so one poll serves both.
export function SettingsView({
  history,
  config,
  configError,
  activeSettingsTab,
  onSelectTab,
  onConfig,
  onThemePreview,
}: SettingsViewProps) {
  const [form, setForm] = useState<Settings | null>(null);
  const [saved, setSaved] = useState<Settings | null>(null);
  const [preview, setPreview] = useState('');
  const [path, setPath] = useState('~/.config/agento11y/config.env');
  const [error, setError] = useState<string | null>(null);
  const [toast, setToast] = useState<string | null>(null);
  const toastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const showToast = useCallback((msg: string) => {
    setToast(msg);
    if (toastTimer.current) clearTimeout(toastTimer.current);
    toastTimer.current = setTimeout(() => setToast(null), 2600);
  }, []);
  useEffect(
    () => () => {
      if (toastTimer.current) clearTimeout(toastTimer.current);
    },
    [],
  );

  // Hydrate from the polled config, and re-hydrate while nothing is edited:
  // `agento11y login`, a second tab or a hand edit can add a connection under
  // an open panel, and this view chooses between the connect flow and the
  // connected panel from `saved`. An unsaved edit wins over the poll, so
  // typed input is never discarded.
  useEffect(() => {
    if (!config) return;
    if (form && !sameSettings(form, saved)) return;
    if (form && sameSettings(config.settings, saved)) return;
    setForm(cloneSettings(config.settings));
    setSaved(cloneSettings(config.settings));
    setPreview(config.preview || '');
    if (config.path) setPath(config.path);
  }, [config, form, saved]);

  // Live preview: the daemon renders exactly what it would write, so the
  // panel never drifts from the file. Debounced to coalesce keystrokes.
  // Each run aborts the prior in-flight request and ignores its result, so
  // a slow older response can never overwrite a newer one.
  useEffect(() => {
    if (!form) return;
    let ignore = false;
    const controller = new AbortController();
    const t = setTimeout(() => {
      fetch('/api/v1/config:preview', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ settings: form }),
        signal: controller.signal,
      })
        .then((r) => (r.ok ? r.json() : null))
        .then((b) => {
          if (!ignore && b && typeof b.preview === 'string') setPreview(b.preview);
        })
        .catch(() => {});
    }, 180);
    return () => {
      ignore = true;
      controller.abort();
      clearTimeout(t);
    };
  }, [form]);

  const dirty = !!form && !sameSettings(form, saved);
  const previewTheme = dirty && form ? form.theme : null;
  // The App owns the document attribute. Publishing in a layout effect keeps
  // an optimistic selection, Reset, and a dirty non-theme edit in sync before
  // the browser paints the corresponding form state.
  useLayoutEffect(() => {
    onThemePreview?.(previewTheme);
  }, [previewTheme, onThemePreview]);
  useLayoutEffect(() => () => onThemePreview?.(null), [onThemePreview]);

  const pageStyle = { paddingBottom: 110 };
  if (!form) {
    return (
      <PageShell maxWidth={1400} style={pageStyle}>
        {configError ? (
          <Notice kind="error" title="Failed to load settings">
            {configError}
          </Notice>
        ) : (
          <Notice kind="info" title="Loading settings…">
            Reading config.env.
          </Notice>
        )}
      </PageShell>
    );
  }

  // Past the early return above `form` is set, and `saved` is set with it:
  // the two are only ever assigned together.
  const set = (patch: Partial<Settings>) => setForm((f) => (f ? { ...f, ...patch } : f));
  // A failed poll drops the hero stat and the Cloud status line to Unknown,
  // the way it drops the header chip. The form keeps hydrating from the
  // last good response.
  const liveConfig = configError ? null : config;
  const setTag = (i: number, patch: Partial<Tag>) =>
    setForm((f) =>
      f
        ? {
            ...f,
            tags: f.tags.map((t, j) => (j === i ? { ...t, ...patch } : t)),
          }
        : f,
    );
  const addTag = () => setForm((f) => (f ? { ...f, tags: [...f.tags, { key: '', value: '' }] } : f));
  const removeTag = (i: number) => setForm((f) => (f ? { ...f, tags: f.tags.filter((_, j) => j !== i) } : f));
  const reset = () => {
    // A dirty form deliberately ignores hydration from config polls, but
    // Reset means "adopt what is saved now", not the snapshot from when the
    // edit began.
    const latest = config?.settings || (saved as Settings);
    setForm(cloneSettings(latest));
    setSaved(cloneSettings(latest));
    if (config && typeof config.preview === 'string') setPreview(config.preview);
    if (config?.path) setPath(config.path);
  };

  // persist writes a whole settings object and adopts the response as both
  // the form and the saved snapshot, so the unsaved-changes bar stays down.
  // Connect, Disconnect and the forwarding mode switch each write through it
  // instead of raising that bar for a choice the user already made.
  const persist = (next: Settings, msg: string) => {
    setError(null);
    return fetch('/api/v1/config', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ settings: next }),
    })
      .then((r) =>
        r.ok
          ? (r.json() as Promise<ConfigResponse>)
          : r.text().then((t) => Promise.reject(new Error(t || `HTTP ${r.status}`))),
      )
      .then((body) => {
        setForm(cloneSettings(body.settings));
        setSaved(cloneSettings(body.settings));
        if (typeof body.preview === 'string') setPreview(body.preview);
        onConfig(body);
        showToast(msg);
      })
      .catch((e) => setError(String(e.message || e)));
  };
  const save = () => persist(form, 'Settings saved to config.env.');

  // oneClickWrite is how the Cloud controls write: the patch goes on top of
  // the saved state, not the form, so a click that names the forwarding mode
  // cannot also commit an edit staged elsewhere. A token Reset waiting in the
  // Edit connection disclosure would otherwise be written by it, deleting a
  // credential with no confirmation and a toast naming something else.
  //
  // Whatever was being edited, minus the fields the patch owns, is put back
  // on top of the response, so it stays pending in the unsaved-changes bar.
  const oneClickWrite = (patch: Partial<Settings>, msg: string) => {
    const pending = pendingEdits(form, saved, patch);
    return persist({ ...(saved as Settings), ...patch }, msg).then(() => {
      if (pending) setForm((f) => (f ? { ...f, ...pending } : f));
    });
  };

  // connect writes every value the pasted block carried, plus the mode picked
  // in step 3, in one request. The pasted CONTENT_CAPTURE_MODE is not read:
  // step 3 is the control for it. capture follows the rule forwardLocalPatch
  // applies: it is rewritten only when the mode on disk forwards
  // differently.
  //
  // OTLP headers carry their own copy of the credential, so a block without
  // them clears the stored value rather than leaving the previous stack's
  // header to authenticate the new one's traces.
  const connect = (parsed: ConnectBlock, mode: string) =>
    oneClickWrite(
      {
        endpoint: parsed.endpoint,
        tenantId: parsed.tenantId,
        token: parsed.token,
        tokenCleared: false,
        otlpEndpoint: parsed.otlpEndpoint || '',
        otlpHeaders: parsed.otlpHeaders || '',
        otlpHeadersCleared: !parsed.otlpHeaders,
        localForward: true,
        ...(captureForwardMode((saved as Settings).capture) === mode ? {} : { capture: mode }),
      },
      'Connected. Saved to config.env.',
    );

  // disconnect clears the connection and stops forwarding. capture is left
  // alone: CONTENT_CAPTURE_MODE also governs non-local Cloud sessions, so
  // clearing it would change a setting the user did not touch.
  const disconnect = () =>
    oneClickWrite(
      {
        endpoint: '',
        tenantId: '',
        token: '',
        tokenCleared: true,
        otlpEndpoint: '',
        otlpHeaders: '',
        otlpHeadersCleared: true,
        localForward: false,
      },
      'Disconnected. Credentials cleared from config.env.',
    );

  // forceLocalOff is the Local-only click the Cloud tab sends when the daemon
  // is still forwarding from its own environment: nothing changes in the
  // form, and config.env still needs the explicit false that overrides it.
  const commitMode = (mode: string, forceLocalOff = false) => {
    const patch =
      forwardLocalPatch(saved as Settings, mode) || (forceLocalOff && mode === 'off' ? { localForward: false } : null);
    if (!patch) return;
    oneClickWrite(
      patch,
      mode === 'off'
        ? 'Forwarding turned off. Saved to config.env.'
        : `Forwarding set to ${mode === 'full' ? 'full' : 'metadata only'}. Saved to config.env.`,
    );
  };
  const copy = () => {
    if (navigator.clipboard?.writeText) {
      navigator.clipboard
        .writeText(preview)
        .then(() => showToast('Copied to clipboard.'))
        .catch(() => {});
    }
  };
  return (
    <PageShell maxWidth={1400} style={pageStyle}>
      <SettingsHero dirty={dirty} path={path} />

      {error && (
        <div style={{ marginBottom: 16 }}>
          <Notice kind="error" title="Couldn't save settings">
            {error}
          </Notice>
        </div>
      )}

      <div
        style={{
          display: 'flex',
          gap: 24,
          alignItems: 'flex-start',
          flexWrap: 'wrap',
        }}
      >
        <div style={{ flex: '999 1 560px', minWidth: 0 }}>
          <SettingsTabRail tabs={SETTINGS_TABS} active={activeSettingsTab} onChange={onSelectTab} />
          <SettingsTabPanels
            activeSettingsTab={activeSettingsTab}
            form={form}
            set={set}
            savedEndpoint={(saved as Settings).endpoint}
            savedGuards={(saved as Settings).guards}
            setTag={setTag}
            addTag={addTag}
            removeTag={removeTag}
            config={liveConfig}
            stackUrl={config?.stackUrl || ''}
            configured={cloudConfigured(saved)}
            configPath={path}
            onConnect={connect}
            onDisconnect={disconnect}
            onMode={commitMode}
            history={history}
          />
        </div>

        <SettingsPreviewPanel path={path} preview={preview} onCopy={copy} />
      </div>

      {dirty && <UnsavedBar onReset={reset} onSave={save} />}
      {toast && <Toast message={toast} />}
    </PageShell>
  );
}
