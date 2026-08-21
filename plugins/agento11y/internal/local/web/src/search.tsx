import type { Dispatch, SetStateAction } from 'react';
import { Fragment, useCallback, useEffect, useRef, useState } from 'react';
import { formatAgo, formatTokens } from './formatters';
import { SurfaceCard } from './notices';
import { conversationPath, highlightTerms, isPlainLeftClick, SEARCH_DEBOUNCE_MS } from './routing';
import { AgentPill, Icon, ModelPill } from './shell';
import type { SearchHit, SearchResponse } from './types';

// ============================================================
// Conversation search — the panel the top bar opens, and the fetch behind it.
// ============================================================

// SearchResultRow is one ranked hit. Stays consistent with ConvRow's dense
// mono grid and agent/model pills, and adds a two-line clamp on the snippet.
// The row is a real anchor so cmd/ctrl-click opens in a new tab without
// us re-implementing the browser.
interface SearchResultRowProps {
  hit: SearchHit;
  now: number;
  query: string;
  selected: boolean;
  onSelect: () => void;
  onOpen: (hit: SearchHit) => void;
}

function SearchResultRow({ hit, now, query, selected, onSelect, onOpen }: SearchResultRowProps) {
  const ago = hit.last_activity ? formatAgo(hit.last_activity, now) : '';
  const titleEl = highlightTerms(hit.title || hit.id, query);
  const snippetEl = highlightTerms(hit.snippet || '', query);
  const matchCount = hit.match_count || 0;
  return (
    <a
      href={conversationPath(hit.id)}
      onMouseEnter={onSelect}
      onFocus={(e) => {
        onSelect();
        if (!selected) e.currentTarget.style.background = 'var(--row-hover)';
      }}
      onBlur={(e) => {
        if (!selected) e.currentTarget.style.background = 'transparent';
      }}
      onClick={(e) => {
        if (!isPlainLeftClick(e)) return;
        e.preventDefault();
        onOpen(hit);
      }}
      style={{
        display: 'block',
        padding: '11px 16px 12px',
        borderBottom: '1px solid var(--border-weak)',
        background: selected ? 'rgba(204,204,220,0.06)' : 'transparent',
        cursor: 'pointer',
        textDecoration: 'none',
        color: 'inherit',
        transition: 'background 80ms ease',
      }}
      onMouseOver={(e) => {
        if (!selected) e.currentTarget.style.background = 'var(--row-hover)';
      }}
      onMouseOut={(e) => {
        if (!selected) e.currentTarget.style.background = 'transparent';
      }}
    >
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: '76px minmax(0,1fr) auto',
          columnGap: 16,
          alignItems: 'baseline',
        }}
      >
        <span
          style={{
            display: 'inline-flex',
            alignItems: 'baseline',
            gap: 6,
            color: 'var(--fg3)',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 12,
          }}
        >
          {/* The row carries no ERR badge, so this dot is its only
                        failure marker. Status is "err" when any generation in
                        the conversation recorded a CallError, "ok" otherwise.
                        See searchConversationFile in internal/local/search.go. */}
          {hit.status === 'err' && (
            <span
              title="Failed model call"
              style={{
                width: 5,
                height: 5,
                borderRadius: '50%',
                flex: 'none',
                background: 'var(--error-main)',
              }}
            />
          )}
          {ago}
        </span>
        <div
          style={{
            display: 'flex',
            alignItems: 'baseline',
            gap: 8,
            flexWrap: 'wrap',
            minWidth: 0,
          }}
        >
          <span
            style={{
              fontFamily: 'var(--fontFamily)',
              fontSize: 14,
              fontWeight: 500,
              color: 'var(--fg1)',
            }}
          >
            {titleEl}
          </span>
          {hit.title && hit.title !== hit.id && (
            <span
              style={{
                fontFamily: 'var(--fontFamilyMonospace)',
                fontSize: 11,
                color: 'var(--fg3)',
              }}
            >
              {hit.id}
            </span>
          )}
        </div>
        <div
          style={{
            display: 'inline-flex',
            alignItems: 'center',
            gap: 10,
            color: 'var(--fg2)',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 12,
          }}
        >
          {(hit.agents || []).map((a) => (
            <AgentPill key={a} name={a} size="sm" />
          ))}
          {(hit.models || []).map((m) => (
            <ModelPill key={m} name={m} />
          ))}
          <span>
            {formatTokens(hit.total_tokens)} · {hit.calls} {hit.calls === 1 ? 'call' : 'calls'}
          </span>
        </div>
      </div>
      <div
        style={{
          display: 'grid',
          gridTemplateColumns: '76px minmax(0,1fr)',
          columnGap: 16,
          marginTop: 7,
        }}
      >
        <span />
        <div
          style={{
            display: '-webkit-box',
            WebkitLineClamp: 2,
            WebkitBoxOrient: 'vertical',
            overflow: 'hidden',
            fontFamily: 'var(--fontFamilyMonospace)',
            fontSize: 12,
            color: 'var(--fg2)',
            lineHeight: 1.5,
          }}
        >
          <span
            style={{
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 11,
              color: 'var(--warning-main)',
              background: 'rgba(245,183,61,0.10)',
              border: '1px solid rgba(245,183,61,0.30)',
              borderRadius: 2,
              padding: '0 5px',
              marginRight: 8,
            }}
          >
            {matchCount} {matchCount === 1 ? 'match' : 'matches'}
          </span>
          {hit.snippet ? <>…{snippetEl}</> : <span style={{ color: 'var(--fg3)' }}>No preview available.</span>}
        </div>
      </div>
    </a>
  );
}

type SearchPhase = 'done' | 'loading';

interface SearchResultsState {
  phase: SearchPhase;
  hits: SearchHit[];
  mode: string;
  error: string | null;
  selectedIndex: number;
  setSelectedIndex: Dispatch<SetStateAction<number>>;
  retry: () => void;
}

export function useSearchResults(query: string): SearchResultsState {
  const [phase, setPhase] = useState<SearchPhase>('done'); // "done" | "loading"
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [mode, setMode] = useState('fts');
  const [error, setError] = useState<string | null>(null);
  const [selectedIndex, setSelectedIndex] = useState(-1);
  const [retryNonce, setRetryNonce] = useState(0);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const abortRef = useRef<AbortController | null>(null);
  const trimmed = query.trim();

  // query is a trigger, not a value the body reads: a new query invalidates the
  // highlighted row.
  // biome-ignore lint/correctness/useExhaustiveDependencies: query resets the selection
  useEffect(() => {
    setSelectedIndex(-1);
  }, [query]);

  // retryNonce is a trigger, not a value the body reads: bumping it re-runs the
  // same search after a failure.
  // biome-ignore lint/correctness/useExhaustiveDependencies: retryNonce re-runs the fetch
  useEffect(() => {
    if (debounceRef.current) clearTimeout(debounceRef.current);
    if (abortRef.current) abortRef.current.abort();

    if (!trimmed) {
      setPhase('done');
      setHits([]);
      setError(null);
      return undefined;
    }
    setPhase('loading');
    setError(null);
    const controller = new AbortController();
    abortRef.current = controller;
    const timer = setTimeout(() => {
      fetch(`/api/v1/search?q=${encodeURIComponent(trimmed)}`, {
        signal: controller.signal,
      })
        .then((r) => (r.ok ? r.json() : r.text().then((t) => Promise.reject(new Error(t || `HTTP ${r.status}`)))))
        .then((body: SearchResponse) => {
          setHits(Array.isArray(body.hits) ? body.hits : []);
          setMode(body.mode || 'fts');
          setPhase('done');
        })
        .catch((e) => {
          if (e && e.name === 'AbortError') return;
          setError(String(e.message || e));
          setPhase('done');
        });
    }, SEARCH_DEBOUNCE_MS);
    debounceRef.current = timer;
    return () => clearTimeout(timer);
  }, [trimmed, retryNonce]);

  useEffect(
    () => () => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      if (abortRef.current) abortRef.current.abort();
    },
    [],
  );

  const retry = useCallback(() => setRetryNonce((n) => n + 1), []);
  return { phase, hits, mode, error, selectedIndex, setSelectedIndex, retry };
}

interface ConversationSearchPanelProps {
  query: string;
  hits: SearchHit[];
  phase: SearchPhase;
  mode: string;
  error: string | null;
  selectedIndex: number;
  setSelectedIndex: Dispatch<SetStateAction<number>>;
  retry: () => void;
  now: number;
  onOpen: (conv: { id: string; title?: string }) => void;
}

export function ConversationSearchPanel({
  query,
  hits,
  phase,
  mode,
  error,
  selectedIndex,
  setSelectedIndex,
  retry,
  now,
  onOpen,
}: ConversationSearchPanelProps) {
  const showResults = !!query && !error;
  const showNoResults = showResults && phase === 'done' && hits.length === 0;
  const showLoadingSkeleton = showResults && phase === 'loading' && hits.length === 0;

  return (
    <SurfaceCard
      style={{
        overflow: 'hidden',
        opacity: phase === 'loading' && hits.length > 0 ? 0.55 : 1,
        transition: 'opacity 120ms ease',
      }}
    >
      {error && (
        <div
          style={{
            margin: 12,
            padding: '12px 14px',
            border: '1px solid var(--error-border)',
            background: 'var(--error-transparent)',
            borderRadius: 2,
            display: 'flex',
            alignItems: 'flex-start',
            gap: 11,
          }}
        >
          <Icon name="alert" size={16} style={{ color: 'var(--error-text)', marginTop: 2 }} />
          <div style={{ flex: 1 }}>
            <div style={{ fontSize: 14, color: 'var(--fg1)' }}>Couldn't run the search.</div>
            <div
              style={{
                fontSize: 13,
                color: 'var(--fg2)',
                marginTop: 3,
              }}
            >
              The local viewer didn't respond. Check that{' '}
              <span
                style={{
                  fontFamily: 'var(--fontFamilyMonospace)',
                }}
              >
                agento11y --local
              </span>{' '}
              is running, then try again.
            </div>
          </div>
          <button
            type="button"
            onClick={retry}
            style={{
              height: 28,
              padding: '0 12px',
              background: 'transparent',
              border: '1px solid var(--border-medium)',
              borderRadius: 2,
              color: 'var(--fg1)',
              fontSize: 12,
              cursor: 'pointer',
            }}
            onMouseEnter={(e) => (e.currentTarget.style.background = 'var(--action-hover)')}
            onMouseLeave={(e) => (e.currentTarget.style.background = 'transparent')}
          >
            Retry
          </button>
        </div>
      )}

      {!error && showResults && hits.length > 0 && (
        <Fragment>
          <div
            style={{
              display: 'flex',
              alignItems: 'center',
              padding: '9px 16px',
              borderBottom: '1px solid var(--border-weak)',
              fontFamily: 'var(--fontFamilyMonospace)',
              fontSize: 12,
              color: 'var(--fg3)',
            }}
          >
            <span>
              {hits.length} {hits.length === 1 ? 'result' : 'results'}
            </span>
            <span style={{ flex: 1 }} />
            <span style={{ fontSize: 11, opacity: 0.7 }}>
              ranked by {mode === 'semantic' ? 'relevance (qmd)' : 'matches'}
            </span>
          </div>
          {hits.map((hit, i) => (
            <SearchResultRow
              key={hit.id}
              hit={hit}
              now={now}
              query={query}
              selected={selectedIndex === i}
              onSelect={() => setSelectedIndex(i)}
              onOpen={(h) => onOpen({ id: h.id, title: h.title })}
            />
          ))}
        </Fragment>
      )}

      {!error &&
        showLoadingSkeleton &&
        [0, 1, 2].map((i) => (
          <div
            key={i}
            style={{
              padding: '14px 16px',
              borderBottom: i < 2 ? '1px solid var(--border-weak)' : 'none',
            }}
          >
            <div
              className="sigil-shim"
              style={{
                height: 14,
                width: '40%',
                borderRadius: 2,
              }}
            />
            <div
              className="sigil-shim"
              style={{
                height: 10,
                width: '80%',
                borderRadius: 2,
                marginTop: 8,
              }}
            />
          </div>
        ))}

      {!error && showNoResults && (
        <div style={{ padding: '34px 16px 36px' }}>
          <div style={{ fontSize: 14, color: 'var(--fg1)' }}>
            No matches for{' '}
            <span
              style={{
                fontFamily: 'var(--fontFamilyMonospace)',
                color: 'var(--fg-max)',
              }}
            >
              “{query}”
            </span>
            .
          </div>
          <div
            style={{
              fontSize: 13,
              color: 'var(--fg3)',
              marginTop: 6,
            }}
          >
            Check spelling, broaden terms, or search fewer words.
          </div>
        </div>
      )}
    </SurfaceCard>
  );
}
