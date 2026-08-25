import type { CSSProperties, ReactNode } from 'react';
import { Fragment, useCallback, useState } from 'react';
import { conversationTime } from './formatters';
import type { ConversationDetail } from './types';

// ============================================================
// Routing, storage and text helpers — the leaf module. It imports no other
// view module, so any of them can read it without an import cycle.
// ============================================================

export function conversationIDFromPath() {
  if (typeof window === 'undefined') return null;
  const prefix = '/conversations/';
  if (!window.location.pathname.startsWith(prefix)) return null;
  const raw = window.location.pathname.slice(prefix.length).replace(/\/$/, '');
  if (!raw) return null;
  try {
    return decodeURIComponent(raw);
  } catch (_) {
    return raw;
  }
}

export function conversationPath(id: string) {
  return `/conversations/${encodeURIComponent(id)}`;
}

export function conversationsPath(workspace: string | null = null) {
  if (workspace == null) return '/';
  return `/?workspace=${encodeURIComponent(workspace)}`;
}

export interface ToolSessionFilters {
  tool: string;
  workspace: string | null;
  since?: string;
  before?: string;
}

export function toolSessionsPath(filters: ToolSessionFilters) {
  const params = new URLSearchParams({ tool: filters.tool });
  if (filters.workspace != null) params.set('workspace', filters.workspace);
  if (filters.since) params.set('since', filters.since);
  if (filters.before) params.set('before', filters.before);
  return `/?${params}`;
}

export function toolSessionFiltersFromLocation(): ToolSessionFilters | null {
  if (typeof window === 'undefined' || window.location.pathname.replace(/\/$/, '') !== '') return null;
  const params = new URLSearchParams(window.location.search);
  if (!params.has('tool')) return null;
  return {
    tool: params.get('tool') ?? '',
    workspace: params.has('workspace') ? (params.get('workspace') ?? '') : null,
    since: params.get('since') || undefined,
    before: params.get('before') || undefined,
  };
}

export function workspaceFromLocation() {
  if (typeof window === 'undefined') return null;
  const params = new URLSearchParams(window.location.search);
  return params.has('workspace') ? (params.get('workspace') ?? '') : null;
}

export function generationIDFromHash() {
  const raw = (window.location.hash || '').replace(/^#/, '');
  if (!raw) return '';
  try {
    return decodeURIComponent(raw);
  } catch {
    return raw;
  }
}

// settingsRouteActive reports whether the URL is the Settings tab.
export function settingsRouteActive() {
  if (typeof window === 'undefined') return false;
  return window.location.pathname.replace(/\/$/, '') === '/settings';
}

// analyticsRouteActive reports whether the URL is the Analytics tab.
export function analyticsRouteActive() {
  if (typeof window === 'undefined') return false;
  return window.location.pathname.replace(/\/$/, '') === '/analytics';
}

export type AnalyticsTab = 'overview' | 'skills';

export function analyticsTabFromLocation(): AnalyticsTab {
  if (typeof window === 'undefined') return 'overview';
  return new URLSearchParams(window.location.search).get('tab') === 'skills' ? 'skills' : 'overview';
}

export function analyticsPath(tab: AnalyticsTab = 'overview') {
  return tab === 'skills' ? '/analytics?tab=skills&mode=tools' : '/analytics';
}

// The mouse-event fields the test reads. React's synthetic mouse events and
// the DOM's own both satisfy it.
interface ClickModifiers {
  button: number;
  metaKey: boolean;
  ctrlKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
}

// Returns true for a plain primary-button click with no modifier keys.
// Lets cmd/ctrl/shift/alt/middle-click fall through to the browser so
// anchors can open in a new tab / window / background tab as expected.
export function isPlainLeftClick(e: ClickModifiers) {
  return e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey;
}

// The list row for a conversation the range-scoped list does not carry: it is
// derived from the detail response, so it holds only what the generations say.
interface DetailSummary {
  id: string;
  title: string;
  started_at: string | null;
  last_activity: string | null;
  calls: number;
  total_tokens: number;
  agents: string[];
  models: string[];
  status: string;
}

export function summaryFromDetail(detail: ConversationDetail | null, id: string): DetailSummary {
  const generations = detail?.generations || [];
  const agents = new Set<string>();
  const models = new Set<string>();
  let startedAt: number | null = null;
  let lastActivity: number | null = null;
  let totalTokens = 0;
  let hasError = false;

  for (const g of generations) {
    if (g.agent_name) agents.add(g.agent_name);
    if (g.model) models.add(g.model);
    totalTokens += g.total_tokens || 0;
    if (g.call_error) hasError = true;

    const start = conversationTime({ last_activity: g.started_at });
    if (start != null && (startedAt == null || start < startedAt)) startedAt = start;
    const end = conversationTime({
      last_activity: g.completed_at || g.started_at,
    });
    if (end != null && (lastActivity == null || end > lastActivity)) lastActivity = end;
  }

  return {
    id,
    title: detail?.title || id,
    started_at: startedAt == null ? null : new Date(startedAt).toISOString(),
    last_activity: lastActivity == null ? null : new Date(lastActivity).toISOString(),
    calls: generations.length,
    total_tokens: totalTokens,
    agents: Array.from(agents).sort(),
    models: Array.from(models).sort(),
    status: hasError ? 'err' : 'ok',
  };
}

// usePersistedState is useState mirrored into localStorage (string
// values only, plain values — no updater functions) so viewer
// preferences survive reloads. accept guards against stale or
// foreign stored values; storage errors (private mode, disabled)
// fall back to in-memory state.
export function usePersistedState(
  key: string,
  initial: string,
  accept: (raw: string) => boolean,
): [string, (v: string) => void] {
  const [value, setValue] = useState(() => {
    try {
      const raw = window.localStorage.getItem(key);
      return raw != null && accept(raw) ? raw : initial;
    } catch (_) {
      return initial;
    }
  });
  const set = useCallback(
    (v: string) => {
      setValue(v);
      try {
        window.localStorage.setItem(key, v);
      } catch (_) {}
    },
    [key],
  );
  return [value, set];
}

// ---------- generic UI primitives ----------

export const fieldInput: CSSProperties = {
  width: '100%',
  height: 34,
  padding: '0 10px',
  border: '1px solid var(--border-medium)',
  borderRadius: 2,
  background: 'var(--bg-canvas)',
  color: 'var(--fg1)',
  fontSize: 13,
  fontFamily: 'var(--fontFamily)',
  outline: 'none',
};
// SEARCH_DEBOUNCE_MS controls how long after the last keystroke the
// viewer waits before issuing the search request. 320ms matches the
// upper end of the design handoff's 320–340ms window: snappy enough to
// feel live, slow enough to coalesce typing into one network call.
export const SEARCH_DEBOUNCE_MS = 320;

// REFRESH_DEBOUNCE_MS is how long a burst of change events is collected
// before the conversation list and the token chart refetch.
export const REFRESH_DEBOUNCE_MS = 250;
// IMPORT_REFRESH_DEBOUNCE_MS replaces it for the list and the chart while a
// history import runs. An import appends to thousands of conversations, so
// a refresh at the normal cadence shows a list that is stale again before
// it renders. The progress banner carries the detail; the list only has to
// stay roughly current. An open conversation keeps the normal cadence: it
// is what the user is reading.
export const IMPORT_REFRESH_DEBOUNCE_MS = 2000;
// IMPORT_ACTIVE_TTL_MS is how long one import frame holds the slower
// cadence. The event stream is lossy: the daemon drops frames for a
// subscriber that falls behind, and a reconnect replays nothing. So a run
// has to expire rather than latch, or one lost terminal frame leaves the
// tab slow until a reload. A running import publishes progress every
// 250ms, so this much slack cannot expire it mid-run.
export const IMPORT_ACTIVE_TTL_MS = 10_000;

// escapeRegExp escapes the special-meaning characters in a query token
// before splicing into the alternation regex that drives highlighting.
// Keeps a literal "a+b" highlighting "a+b" rather than "aab".
function escapeRegExp(s: string) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

// highlightTerms renders `text` with each whitespace-separated term in
// `query` wrapped in a `<mark>` carrying the warm-wash style. Tokens
// are lower-cased and deduped (so "rate rate" doesn't compile a
// doubled alternation), and the split keeps the original text casing
// outside the matches.
export function highlightTerms(text: string | null | undefined, query: string): ReactNode {
  if (!text) return text;
  const terms = String(query || '')
    .toLowerCase()
    .split(/\s+/)
    .filter(Boolean);
  if (terms.length === 0) return text;
  const uniq = Array.from(new Set(terms)).map(escapeRegExp);
  const re = new RegExp(`(${uniq.join('|')})`, 'ig');
  const parts = String(text).split(re);
  const wash: CSSProperties = {
    color: 'var(--fg-max)',
    fontWeight: 500,
    background: 'rgba(245,183,61,0.18)',
    boxShadow: 'inset 0 -1px 0 rgba(245,183,61,0.45)',
    borderRadius: 2,
    padding: '0 2px',
  };
  return parts.map((part, i) => {
    if (!part) return null;
    return re.test(part) ? (
      <mark key={i} style={wash}>
        {part}
      </mark>
    ) : (
      <Fragment key={i}>{part}</Fragment>
    );
  });
}
