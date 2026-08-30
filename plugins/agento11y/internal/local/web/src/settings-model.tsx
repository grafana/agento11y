import type { ReactNode } from 'react';
import { formatAgo } from './formatters';
import type { ConfigResponse, ForwardStatus, Settings, ThemePreference } from './types';

// ============================================================
// Settings — edits config.env via the daemon's /api/v1/config endpoints
// ============================================================

// FormSettings is Settings plus the four keys sameSettings compares that the
// daemon does not send, so their runtime type is unknown here.
export interface FormSettings extends Settings {
  semanticSearch?: unknown;
  securityFindingsExport?: unknown;
  securityAuditSchedule?: unknown;
  promptGuardUrl?: unknown;
}

type ForwardAccent = 'success' | 'info' | 'warning' | 'error';

export interface GuardStatusMeta {
  accent: ForwardAccent;
  line: string;
}

interface ForwardBannerMeta {
  accent: ForwardAccent;
  pill: string;
  line: string;
}

interface ForwardChipMeta {
  kicker: string;
  value: string;
  line: string;
  color: string;
  border: string;
}

// Mono renders inline code in the monospace face used across the viewer.
export function Mono({ children }: { children?: ReactNode }) {
  return (
    <code
      style={{
        fontFamily: 'var(--fontFamilyMonospace)',
        color: 'var(--fg2)',
      }}
    >
      {children}
    </code>
  );
}

export function isThemePreference(value: unknown): value is ThemePreference {
  return value === 'dark' || value === 'light' || value === 'system';
}

// resolveThemePreference applies the runtime precedence for the document. The
// server-stamped value is the fallback before the first config response, and
// dark is used only when that attribute is absent or invalid too.
export function resolveThemePreference(
  dirtyFormTheme: unknown,
  savedTheme: unknown,
  serverStampedTheme: unknown,
): ThemePreference {
  if (isThemePreference(dirtyFormTheme)) return dirtyFormTheme;
  if (isThemePreference(savedTheme)) return savedTheme;
  return isThemePreference(serverStampedTheme) ? serverStampedTheme : 'dark';
}

export function documentThemePreference(root: HTMLElement = document.documentElement): ThemePreference {
  return resolveThemePreference(null, null, root.getAttribute('data-theme'));
}

export function applyDocumentTheme(theme: unknown, root: HTMLElement = document.documentElement): ThemePreference {
  const next = isThemePreference(theme) ? theme : 'dark';
  root.setAttribute('data-theme', next);
  return next;
}

export function toggledThemePreference(theme: unknown, systemTheme: unknown): ThemePreference {
  const current = theme === 'system' ? systemTheme : theme;
  return current === 'light' ? 'dark' : 'light';
}

export function patchThemePreference(theme: ThemePreference): Promise<ConfigResponse> {
  return fetch('/api/v1/config', {
    method: 'PATCH',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ theme }),
  }).then((response) =>
    response.ok
      ? (response.json() as Promise<ConfigResponse>)
      : response.text().then((text) => Promise.reject(new Error(text || `HTTP ${response.status}`))),
  );
}

export interface ThemeShortcutState {
  prefix: '' | 'c';
  at: number;
}

const THEME_SHORTCUT_TIMEOUT_MS = 800;

function isTextEntryTarget(target: EventTarget | null): boolean {
  const element = target as HTMLElement | null;
  if (!element || typeof element.tagName !== 'string') return false;
  const tag = element.tagName.toLowerCase();
  return tag === 'input' || tag === 'textarea' || tag === 'select' || element.isContentEditable === true;
}

export function themeShortcutToggles(
  event: Pick<KeyboardEvent, 'altKey' | 'ctrlKey' | 'key' | 'metaKey' | 'target'>,
  state: ThemeShortcutState,
  now = Date.now(),
): boolean {
  if (event.ctrlKey || event.metaKey || event.altKey || isTextEntryTarget(event.target)) {
    state.prefix = '';
    return false;
  }
  if (state.prefix && now - state.at > THEME_SHORTCUT_TIMEOUT_MS) state.prefix = '';
  const key = String(event.key || '').toLowerCase();
  if (state.prefix === 'c' && key === 't') {
    state.prefix = '';
    return true;
  }
  state.prefix = key === 'c' ? 'c' : '';
  state.at = now;
  return false;
}

// sameSettings is a field-wise deep compare for dirty tracking. Tag order
// is significant (it survives a round-trip), so it is compared positionally.
export function sameSettings(a: FormSettings | null | undefined, b: FormSettings | null | undefined): boolean {
  if (!a || !b) return a === b;
  if (
    a.theme !== b.theme ||
    a.endpoint !== b.endpoint ||
    a.tenantId !== b.tenantId ||
    a.otlpEndpoint !== b.otlpEndpoint ||
    a.token !== b.token ||
    a.tokenCleared !== b.tokenCleared
  )
    return false;
  // The server reports the two credential-present flags and no control
  // edits them, so they never make a form dirty. sameSettings still
  // compares them, because it also decides whether a polled config.env
  // differs from the saved snapshot, and a token written by
  // `agento11y login` changes nothing else.
  if (a.tokenSet !== b.tokenSet || a.otlpHeadersSet !== b.otlpHeadersSet) return false;
  if (
    a.capture !== b.capture ||
    a.guards !== b.guards ||
    a.guardTimeout !== b.guardTimeout ||
    a.debug !== b.debug ||
    a.autoUpdate !== b.autoUpdate ||
    a.userId !== b.userId ||
    a.localForward !== b.localForward ||
    a.semanticSearch !== b.semanticSearch ||
    a.securityFindingsExport !== b.securityFindingsExport ||
    a.securityAuditSchedule !== b.securityAuditSchedule ||
    a.promptGuardUrl !== b.promptGuardUrl
  )
    return false;
  const at = a.tags || [],
    bt = b.tags || [];
  if (at.length !== bt.length) return false;
  for (let i = 0; i < at.length; i++) {
    const av = at[i],
      bv = bt[i];
    if (!av || !bv) return false;
    if (av.key !== bv.key || av.value !== bv.value) return false;
  }
  return true;
}

// cloneSettings deep-copies so the form and the saved snapshot never share
// the tags array (editing one must not mutate the other).
export function cloneSettings(s: Settings): Settings {
  return { ...s, tags: (s.tags || []).map((t) => ({ ...t })) };
}

// pendingEdits returns the fields the form has changed and a write does not
// own, so a one-click control can leave them pending instead of committing
// them. null means there is nothing pending.
export function pendingEdits(
  form: Settings | null | undefined,
  saved: Settings | null | undefined,
  owned?: Partial<Settings> | null,
): Partial<Settings> | null {
  if (!form || !saved) return null;
  const out: Partial<Settings> = {};
  (Object.keys(form) as (keyof Settings)[]).forEach((key) => {
    if (owned && Object.hasOwn(owned, key)) return;
    const same =
      key === 'tags' ? JSON.stringify(form.tags || []) === JSON.stringify(saved.tags || []) : form[key] === saved[key];
    if (!same) copyField(out, form, key);
  });
  return Object.keys(out).length > 0 ? out : null;
}

// copyField keeps the key's type tied to its value across the assignment,
// which an indexed write through a union key cannot express.
function copyField<K extends keyof Settings>(out: Partial<Settings>, src: Settings, key: K): void {
  out[key] = src[key];
}

// GUARD_CONTENT_NOTE is the one carve-out in the capture-mode promise: a
// chained guard check relays the content being evaluated. See
// handleHookEvaluate in internal/local/server.go.
export const GUARD_CONTENT_NOTE =
  'Guards are on: tool calls, and the conversation an agent runs a preflight check on, are sent to Cloud for evaluation regardless of the capture mode.';

// forwardBannerMeta turns the daemon's reported forwarding status into the
// pill, accent, and sentence forwardChipMeta builds the header chip and the
// settings hero from. The saved toggle is deliberately not an input:
// config.env and the daemon's own environment can disagree, and only the
// daemon knows what it would actually send.
function forwardBannerMeta(st: ForwardStatus | null | undefined): ForwardBannerMeta {
  if (!st) {
    return {
      accent: 'warning',
      pill: 'Unknown',
      line: "Couldn't read the daemon's forwarding status.",
    };
  }
  if (!st.enabled) {
    if (st.reason)
      return {
        accent: 'warning',
        pill: 'Paused',
        line: `Forwarding is on but paused: ${st.reason}`,
      };
    // The hook leg is one of the legs st.enabled sums, so nothing is
    // relayed here.
    return {
      accent: 'success',
      pill: 'Off',
      line: 'Cloud forwarding is off. Nothing from local sessions leaves this machine.',
    };
  }
  // Guard disclosures hold whatever else the status says, so every branch
  // below that reports forwarding as on appends them. Failures are kept per
  // leg: a failing generations or OTLP leg must not hide that guard checks
  // are still shipping content, nor swallow the unchecked-allow count.
  const disclosures = [];
  if (st.hooks) disclosures.push(GUARD_CONTENT_NOTE);
  if ((st.hookFailOpens ?? 0) > 0)
    disclosures.push(
      st.hookFailOpens === 1
        ? '1 guard check ran without a Cloud verdict and was allowed.'
        : `${st.hookFailOpens} guard checks ran without a Cloud verdict and were allowed.`,
    );
  const failures = st.failures || [];
  const failure = failures[0];
  if (failure) {
    // Name the other failing legs instead of letting the most recent one
    // stand for all of them.
    const others = [...new Set(failures.map((f) => f.label))].filter((l) => l !== failure.label);
    const also = others.length > 0 ? ` (also failing: ${others.join(', ')})` : '';
    return {
      accent: 'error',
      pill: 'Failing',
      line: [
        `Forwarding is on but the last attempts failed. ${failure.label}: ${failure.detail}${also}`,
        ...disclosures,
      ].join(' '),
    };
  }
  // An unrecognised mode must not read as the narrower one: a future mode
  // could forward more, not less.
  if (st.mode !== 'full' && st.mode !== 'metadata_only') {
    return {
      accent: 'warning',
      pill: 'On',
      line: [`Forwarding is on in a mode this viewer does not know (${st.mode || 'unset'}).`, ...disclosures].join(' '),
    };
  }
  // With guards chained, only reasoning text and media are still local:
  // the guard request carries tool calls, and for a preflight check the
  // prompts and responses too, so those cannot be listed as local here.
  const metadataLine = st.hooks
    ? 'Session capture forwards usage and session metadata only. Reasoning text and attached media stay local.'
    : 'Only usage and session metadata is forwarded. Prompts, responses, reasoning text, tool inputs and results, and attached media stay local.';
  const parts = [
    st.mode === 'full' ? "Full session content is forwarded to your organization's Grafana Cloud." : metadataLine,
  ];
  if (!st.generations && st.reason) parts.push(`Generations are paused: ${st.reason}`);
  if (!st.otlp) parts.push('Traces and metrics are not forwarded.');
  parts.push(...disclosures);
  return {
    // A metadata_only forward with guards chained still ships content, so
    // it does not get the calm accent or the reassuring pill.
    accent: st.mode === 'full' || st.hooks ? 'warning' : 'info',
    pill: st.mode === 'full' ? 'Full content' : st.hooks ? 'Metadata + guard content' : 'Metadata only',
    line: parts.join(' '),
  };
}

function timestampIsAfter(left: string, right: string): boolean {
  const leftMillis = Date.parse(left);
  const rightMillis = Date.parse(right);
  if (leftMillis !== rightMillis) return leftMillis > rightMillis;

  // Date.parse drops the digits after milliseconds.
  const nanoseconds = (timestamp: string) => {
    const fraction = timestamp.match(/\.(\d+)(?:Z|[+-]\d{2}:\d{2})$/)?.[1] ?? '';
    return Number(fraction.padEnd(9, '0').slice(0, 9));
  };
  return nanoseconds(left) > nanoseconds(right);
}

// guardStatusMeta reports the most recent outcome of the Cloud guard leg.
export function guardStatusMeta(st: ForwardStatus | null | undefined, now: number): GuardStatusMeta {
  if (!st) {
    return {
      accent: 'warning',
      line: "Couldn't read the daemon's guard status.",
    };
  }
  if (st.hookReason) {
    return { accent: 'info', line: st.hookReason };
  }
  if (!st.enabled) {
    return {
      accent: 'info',
      line: 'Cloud forwarding is off, so this daemon does not relay guard checks.',
    };
  }

  const leg = st.legs?.hooks;
  if (!leg?.lastSuccessAt && !leg?.lastFailureAt) {
    return {
      accent: 'info',
      line: 'The daemon has not recorded a Cloud guard verdict or evaluation failure since it started.',
    };
  }

  const failureIsLatest =
    !!leg.lastFailureAt && (!leg.lastSuccessAt || timestampIsAfter(leg.lastFailureAt, leg.lastSuccessAt));
  if (failureIsLatest) {
    const lastVerdict = leg.lastSuccessAt ? ` The last Cloud verdict was ${formatAgo(leg.lastSuccessAt, now)}.` : '';
    return {
      accent: 'error',
      line: `The latest guard evaluation got no Cloud verdict ${formatAgo(leg.lastFailureAt, now)}: ${leg.lastFailureDetail || 'Unknown error'}.${lastVerdict}`,
    };
  }

  const earlierFailure = leg.lastFailureAt
    ? ` The previous evaluation got no verdict ${formatAgo(leg.lastFailureAt, now)}: ${leg.lastFailureDetail || 'Unknown error'}.`
    : '';
  return {
    accent: 'success',
    line: `Cloud returned a verdict ${formatAgo(leg.lastSuccessAt, now)}.${earlierFailure}`,
  };
}

// cloudConfigured reports whether a Grafana Cloud connection is saved. Any
// one of the three is enough: forwardDisabledReason accepts an endpoint
// without credentials for a local collector.
export function cloudConfigured(settings: Settings | null | undefined): boolean {
  return !!(settings && (settings.endpoint || settings.tenantId || settings.tokenSet));
}

// forwardChipMeta maps the daemon's posture onto the header chip. It is not
// forwardBannerMeta's pill: the chip says Local where forwardBannerMeta says
// Off, Full where it says Full content, and it separates "no connection
// saved" from "saved, forwarding off", which the status alone cannot express
// (enabled is false for both, see resolveForwardConfig in
// internal/local/forward.go). color and border are whole var() references
// rather than an accent name, because the unconfigured state pairs --fg2
// with --border-medium and there is no --fg2-border.
export function forwardChipMeta(config: ConfigResponse | null | undefined): ForwardChipMeta {
  const st = config ? config.forwardStatus : null;
  const meta = forwardBannerMeta(st);
  const tone = (accent: ForwardAccent) => ({
    color: `var(--${accent}-text)`,
    border: `var(--${accent}-border)`,
  });
  if (!st)
    return {
      kicker: 'Mode',
      value: 'Unknown',
      line: meta.line,
      ...tone('warning'),
    };
  if (!st.enabled && !st.reason) {
    if (!cloudConfigured(config?.settings)) {
      return {
        kicker: 'Mode',
        value: 'Local',
        color: 'var(--fg2)',
        border: 'var(--border-medium)',
        line: 'Nothing from local sessions leaves this machine. No Grafana Cloud connection is configured, so every session stays in the local store.',
      };
    }
    return {
      kicker: 'Mode',
      value: 'Local',
      line: meta.line,
      ...tone(meta.accent),
    };
  }
  // Failing, Paused, Metadata only, Metadata + guard content and the
  // unrecognised-mode "On" keep forwardBannerMeta's pill and accent.
  return {
    kicker: 'Cloud forwarding',
    value: meta.pill === 'Full content' ? 'Full' : meta.pill,
    line: meta.line,
    ...tone(meta.accent),
  };
}
