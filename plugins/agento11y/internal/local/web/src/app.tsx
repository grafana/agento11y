import type { MouseEvent as ReactMouseEvent } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { AnalyticsOverviewContent, type AnalyticsUnit, analyticsOverviewHeroStats } from './analytics';
import { AnalyticsPage } from './analytics-page';
import { ConversationsView, GROUP_BY_OPTIONS } from './conversations';
import { TraceDetailView } from './detail';
import type { TimeSpan } from './formatters';
import {
  chartBucketMs,
  DEFAULT_TIME_RANGE,
  LIST_PAGE_SIZE,
  requestWindow,
  TIME_RANGES,
  TOKEN_SERIES,
  timeRangeOption,
} from './formatters';
import {
  type AnalyticsTab,
  analyticsPath,
  analyticsRouteActive,
  analyticsTabFromLocation,
  conversationIDFromPath,
  conversationPath,
  conversationsPath,
  IMPORT_ACTIVE_TTL_MS,
  IMPORT_REFRESH_DEBOUNCE_MS,
  REFRESH_DEBOUNCE_MS,
  settingsRouteActive,
  summaryFromDetail,
  type ToolSessionFilters,
  toolSessionFiltersFromLocation,
  toolSessionsPath,
  usePersistedState,
  workspaceFromLocation,
} from './routing';
import {
  importRunIsActive,
  SETTINGS_TAB_IDS,
  SettingsView,
  settingsPath,
  settingsTabFromLocation,
  useHistoryImport,
} from './settings-screen';
import { TopBar } from './shell';
import { SkillsToolsContent, skillsToolsHeroStats } from './skills-tools';
import type {
  ConfigResponse,
  ConversationDetail,
  ConversationListResponse,
  ConversationMetricsAggregate,
  ConversationMetricsResponse,
  ConversationSummary,
  ImportRun,
  SkillsToolsMetricsResponse,
  TokenBucketKey,
  TokenUsagePoint,
  TokenUsageResponse,
  ToolAnalytics,
} from './types';

// ============================================================
// App container — fetches from the daemon and routes between views.
// ============================================================

export interface ListSort {
  key: string;
  dir: 'asc' | 'desc';
}

interface DetailReturnState {
  view?: 'analytics' | 'conversations';
  workspace?: string | null;
  returnPath?: string;
}

const ANALYTICS_LIST_SIZE = 2000;
const ALL_WORKSPACES = 'all';
const WORKSPACE_PREFIX = 'workspace:';

function decodeAnalyticsWorkspace(value: string): string | null {
  if (value === '' || value === ALL_WORKSPACES) return null;
  return value.startsWith(WORKSPACE_PREFIX) ? value.slice(WORKSPACE_PREFIX.length) : value;
}

function encodeAnalyticsWorkspace(value: string | null): string {
  return value == null ? ALL_WORKSPACES : `${WORKSPACE_PREFIX}${value}`;
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

/** One frame of the /api/v1/events stream. */
interface StreamEvent {
  conversation_id?: string;
  generation_id?: string;
  import?: ImportRun;
}

export function App() {
  const [selectedID, setSelectedID] = useState(conversationIDFromPath);
  const [showSettings, setShowSettings] = useState(settingsRouteActive);
  const [showAnalytics, setShowAnalytics] = useState(analyticsRouteActive);
  const [analyticsTab, setAnalyticsTab] = useState<AnalyticsTab>(analyticsTabFromLocation);
  const [toolSessionFilters, setToolSessionFilters] = useState<ToolSessionFilters | null>(
    toolSessionFiltersFromLocation,
  );
  // The settings tab is part of the route, so it is owned here with the rest
  // of it: the header chip opens the Cloud tab from any view, including from
  // Settings itself, where pushState alone leaves the panel where it was.
  const [settingsTab, setSettingsTab] = useState(settingsTabFromLocation);
  const [conversations, setConversations] = useState<ConversationSummary[]>([]);
  // storeCount is the number of conversations the daemon holds, before
  // the list's page and range bounds. The list itself is range-scoped,
  // so only this count distinguishes an empty store from a quiet range.
  const [storeCount, setStoreCount] = useState<number | null>(null);
  const [tokenPoints, setTokenPoints] = useState<TokenUsagePoint[]>([]);
  const [tokenIntervalMs, setTokenIntervalMs] = useState(0);
  const [loadingList, setLoadingList] = useState(true);
  const [errList, setErrList] = useState<string | null>(null);
  const [query, setQuery] = useState('');
  const conversationSearchRef = useRef<HTMLInputElement | null>(null);
  const [timeRange, setTimeRange] = usePersistedState('sigil.local.timeRange', DEFAULT_TIME_RANGE, (v) =>
    TIME_RANGES.some((r) => r.value === v),
  );
  const [tokenModel, setTokenModel] = useState('all');
  const [chartMetric, setChartMetric] = usePersistedState(
    'sigil.local.chartMetric',
    'tokens',
    (v) => v === 'tokens' || v === 'activity',
  );
  const [bucketSel, setBucketSel] = useState<TimeSpan | null>(null);
  const [workspace, setWorkspace] = useState<string | null>(workspaceFromLocation);
  const [groupBy, setGroupBy] = usePersistedState('sigil.local.groupBy', 'workspace', (value) =>
    GROUP_BY_OPTIONS.some((option) => option.value === value),
  );
  const [listSort, setListSort] = useState<ListSort>({
    key: 'last_activity',
    dir: 'desc',
  });
  const [analyticsUnitValue, setAnalyticsUnit] = usePersistedState(
    'sigil.local.analyticsUnit',
    'cost',
    (value) => value === 'cost' || value === 'tokens',
  );
  const analyticsUnit = analyticsUnitValue as AnalyticsUnit;
  const [analyticsRange, setAnalyticsRange] = usePersistedState('sigil.local.analyticsRange', '24h', (value) =>
    TIME_RANGES.some((range) => range.value === value),
  );
  const [analyticsWorkspaceValue, setAnalyticsWorkspaceValue] = usePersistedState(
    'sigil.local.analyticsWorkspace',
    ALL_WORKSPACES,
    () => true,
  );
  const analyticsWorkspace = decodeAnalyticsWorkspace(analyticsWorkspaceValue);
  const [analyticsHiddenValue, setAnalyticsHiddenValue] = usePersistedState(
    'sigil.local.analyticsHiddenSeries',
    '',
    (value) =>
      value === '' ||
      value.split(',').every((key) => TOKEN_SERIES.some((series) => series.key === (key as TokenBucketKey))),
  );
  const analyticsHiddenSeries = useMemo<ReadonlySet<TokenBucketKey>>(
    () => new Set(analyticsHiddenValue.split(',').filter(Boolean) as TokenBucketKey[]),
    [analyticsHiddenValue],
  );
  const toggleAnalyticsSeries = useCallback(
    (key: TokenBucketKey) => {
      const next = new Set(analyticsHiddenSeries);
      if (next.has(key)) next.delete(key);
      else next.add(key);
      setAnalyticsHiddenValue(
        TOKEN_SERIES.map((series) => series.key)
          .filter((series) => next.has(series))
          .join(','),
      );
    },
    [analyticsHiddenSeries, setAnalyticsHiddenValue],
  );
  const [analyticsConversations, setAnalyticsConversations] = useState<ConversationSummary[]>([]);
  const [analyticsPreviousConversations, setAnalyticsPreviousConversations] = useState<ConversationSummary[]>([]);
  const [analyticsAggregate, setAnalyticsAggregate] = useState<ConversationMetricsAggregate | null>(null);
  const [analyticsPreviousAggregate, setAnalyticsPreviousAggregate] = useState<ConversationMetricsAggregate | null>(
    null,
  );
  const [analyticsFacetConversations, setAnalyticsFacetConversations] = useState<ConversationSummary[]>([]);
  const [analyticsTotalConversations, setAnalyticsTotalConversations] = useState<number | null>(null);
  const [analyticsPreviousTotalConversations, setAnalyticsPreviousTotalConversations] = useState<number | null>(null);
  const [analyticsFacetTotalConversations, setAnalyticsFacetTotalConversations] = useState<number | null>(null);
  const [analyticsTokenPoints, setAnalyticsTokenPoints] = useState<TokenUsagePoint[]>([]);
  const [analyticsTokenIntervalMs, setAnalyticsTokenIntervalMs] = useState(0);
  const [analyticsHeatmapPoints, setAnalyticsHeatmapPoints] = useState<TokenUsagePoint[]>([]);
  const [skillsTools, setSkillsTools] = useState<ToolAnalytics | null>(null);
  const [skillsToolsWindow, setSkillsToolsWindow] = useState<{ since?: string; before: string } | null>(null);
  const [loadingSkillsTools, setLoadingSkillsTools] = useState(false);
  const [errSkillsTools, setErrSkillsTools] = useState<string | null>(null);
  const [loadingAnalytics, setLoadingAnalytics] = useState(false);
  const [loadingAnalyticsTokens, setLoadingAnalyticsTokens] = useState(false);
  const [loadingAnalyticsHeatmap, setLoadingAnalyticsHeatmap] = useState(false);
  const [errAnalytics, setErrAnalytics] = useState<string | null>(null);
  const [errAnalyticsPrevious, setErrAnalyticsPrevious] = useState<string | null>(null);
  const [errAnalyticsFacets, setErrAnalyticsFacets] = useState<string | null>(null);
  const [errAnalyticsTokens, setErrAnalyticsTokens] = useState<string | null>(null);
  const [errAnalyticsHeatmap, setErrAnalyticsHeatmap] = useState<string | null>(null);

  const [detail, setDetail] = useState<ConversationDetail | null>(null);
  const [loadingDetail, setLoadingDetail] = useState(false);
  const [errDetail, setErrDetail] = useState<string | null>(null);
  // Import-run state arrives on the SSE stream as its own event kind, not
  // as a refetch hint: an import writes thousands of generations, and the
  // counters are what the user watches while it runs.
  const [importEvent, setImportEvent] = useState<ImportRun | null>(null);
  const history = useHistoryImport(importEvent);

  // config.env moves under an open viewer: a second tab, `agento11y login`,
  // a hand edit. The header chip is a privacy disclosure, so it re-reads
  // rather than freezing at mount. SettingsView hydrates from the same
  // response and writes back through applyConfig, so one poll serves both.
  //
  // Every request and every save takes the next sequence number and applies
  // its result only while it is still the latest, so neither a poll that left
  // before a save nor a poll stalled past the 30s interval can reinstate an
  // older posture. The .catch is guarded for the same reason: a superseded
  // request that fails afterwards must not drop the chip to Unknown.
  const [config, setConfig] = useState<ConfigResponse | null>(null);
  const [configErr, setConfigErr] = useState<string | null>(null);
  const configSeqRef = useRef(0);
  const applyConfig = useCallback((body: ConfigResponse) => {
    configSeqRef.current++;
    setConfig(body);
    setConfigErr(null);
  }, []);
  const loadConfig = useCallback(() => {
    const seq = ++configSeqRef.current;
    fetch('/api/v1/config')
      .then((r) => (r.ok ? r.json() : r.text().then((t) => Promise.reject(new Error(t || `HTTP ${r.status}`)))))
      .then((body: ConfigResponse) => {
        if (configSeqRef.current !== seq) return;
        setConfig(body);
        setConfigErr(null);
      })
      .catch((e) => {
        if (configSeqRef.current !== seq) return;
        setConfigErr(String(e.message || e));
      });
  }, []);
  useEffect(() => {
    loadConfig();
    const id = setInterval(loadConfig, 30_000);
    return () => clearInterval(id);
  }, [loadConfig]);

  const view: 'settings' | 'analytics' | 'conversation' | 'conversations' = showSettings
    ? 'settings'
    : showAnalytics
      ? 'analytics'
      : selectedID
        ? 'conversation'
        : 'conversations';
  const selected = selectedID
    ? conversations.find((c) => c.id === selectedID) || summaryFromDetail(detail, selectedID)
    : null;

  // Changing the time range invalidates a bucket drill-down: the
  // bucket boundaries belong to the old window.
  const changeTimeRange = useCallback(
    (v: string) => {
      if (toolSessionFilters) {
        window.history.pushState({}, '', conversationsPath(workspace));
        setToolSessionFilters(null);
      }
      setBucketSel(null);
      setTimeRange(v);
    },
    [setTimeRange, toolSessionFilters, workspace],
  );
  const clearToolSessionFilter = useCallback(() => {
    window.history.pushState({}, '', conversationsPath(workspace));
    setToolSessionFilters(null);
  }, [workspace]);

  const pageTitle =
    view === 'settings'
      ? 'Settings · agento11y local'
      : view === 'analytics'
        ? 'Analytics · agento11y local'
        : view === 'conversation' && selected
          ? `${selected.title || selected.id} · agento11y local`
          : 'agento11y · local';
  useEffect(() => {
    document.title = pageTitle;
  }, [pageTitle]);

  // Opening Settings re-reads config.env: the form hydrates from the polled
  // response, which is otherwise up to 30s old, and the panel it picks
  // depends on whether a connection is saved.
  useEffect(() => {
    if (view === 'settings') loadConfig();
  }, [view, loadConfig]);

  // fetchList is driven from four sources (mount, a range change, an SSE
  // flush, the 60s backstop), so a slower older response could otherwise
  // overwrite a newer one. Each call captures a monotonically increasing
  // sequence number and only applies its result if it is still the
  // latest.
  //
  // reset drops the current page before the request goes out. The page is
  // range-scoped, so once the range changes it covers a window the header
  // no longer names, and a wider range would keep showing the narrower
  // page as if it were the whole answer. A refresh in place (SSE flush,
  // backstop, the refresh button) keeps the rows and swaps them when the
  // response lands.
  const listSeqRef = useRef(0);
  const fetchList = useCallback(
    (reset = false) => {
      const seq = ++listSeqRef.current;
      setLoadingList(true);
      setErrList(null);
      if (reset) setConversations([]);
      const w = requestWindow(timeRange, LIST_PAGE_SIZE);
      const params = new URLSearchParams({ limit: String(w.limit) });
      if (toolSessionFilters) {
        params.set('tool', toolSessionFilters.tool);
        if (toolSessionFilters.workspace != null) params.set('workspace', toolSessionFilters.workspace);
        if (toolSessionFilters.since) params.set('since', toolSessionFilters.since);
        if (toolSessionFilters.before) params.set('before', toolSessionFilters.before);
      } else if (w.since) {
        params.set('since', w.since);
      }
      return fetch(`/api/v1/conversations?${params}`)
        .then((r) => (r.ok ? r.json() : r.text().then((t) => Promise.reject(new Error(t || `HTTP ${r.status}`)))))
        .then((body: ConversationListResponse) => {
          if (listSeqRef.current !== seq) return;
          setConversations(body.conversations || []);
          setStoreCount(Number.isFinite(body.total_conversations) ? body.total_conversations : null);
        })
        .catch((e) => {
          if (listSeqRef.current !== seq) return;
          setErrList(String(e.message || e));
        })
        .finally(() => {
          if (listSeqRef.current !== seq) return;
          setLoadingList(false);
        });
    },
    [timeRange, toolSessionFilters],
  );

  // Token points back the usage chart. The server aggregates them per
  // bucket and model over the requested range, so the payload follows
  // the number of bars, not the number of generations. A failure here is
  // swallowed: the chart is supplementary, and a hiccup shouldn't surface
  // an error banner over the conversation list.
  //
  // The response is range-specific, and this runs from mount, an SSE
  // flush, the 60s backstop and a range change. It therefore carries the
  // same sequence guard as fetchList: a slow response for the previous
  // range must not replace a fast one for the current range.
  const tokenSeqRef = useRef(0);
  const fetchTokens = useCallback(
    (reset = false) => {
      const seq = ++tokenSeqRef.current;
      // As with the list, a range change invalidates the points and the
      // interval they were aggregated on. Clearing them keeps a narrower
      // window's chart from reading as the new range's usage, which a
      // swallowed failure would otherwise leave in place until the next
      // flush or backstop.
      if (reset) {
        setTokenPoints([]);
        setTokenIntervalMs(0);
      }
      const w = requestWindow(timeRange, LIST_PAGE_SIZE);
      const params = new URLSearchParams();
      if (toolSessionFilters) {
        if (toolSessionFilters.since) params.set('since', toolSessionFilters.since);
        if (toolSessionFilters.before) params.set('before', toolSessionFilters.before);
        if (toolSessionFilters.workspace != null) params.set('workspace', toolSessionFilters.workspace);
        const since = toolSessionFilters.since ? Date.parse(toolSessionFilters.since) : Number.NaN;
        const before = toolSessionFilters.before ? Date.parse(toolSessionFilters.before) : Number.NaN;
        if (Number.isFinite(since) && Number.isFinite(before) && since < before) {
          params.set('interval', String(Math.round(chartBucketMs(before - since) / 1000)));
        }
      } else {
        if (w.since) params.set('since', w.since);
        if (w.intervalSec) params.set('interval', String(w.intervalSec));
      }
      const query = params.toString();
      return fetch(`/api/v1/metrics/tokens${query ? `?${query}` : ''}`)
        .then((r) => (r.ok ? r.json() : null))
        .then((body: TokenUsageResponse | null) => {
          if (!body || tokenSeqRef.current !== seq) return;
          setTokenPoints(body.points || []);
          // The chart never draws a bar finer than the width the server
          // aggregated on, so read it back instead of assuming the
          // requested one ("All" requests none).
          const seconds = Number(body.interval_seconds);
          setTokenIntervalMs(Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : 0);
        })
        .catch(() => {});
    },
    [timeRange, toolSessionFilters],
  );

  const analyticsSeqRef = useRef(0);
  const readJSON = useCallback(
    <T,>(response: Response): Promise<T> =>
      response.ok
        ? response.json()
        : response.text().then((text) => Promise.reject(new Error(text || `HTTP ${response.status}`))),
    [],
  );
  const fetchAnalytics = useCallback(
    (reset = false) => {
      const seq = ++analyticsSeqRef.current;
      setLoadingAnalytics(true);
      setLoadingAnalyticsTokens(true);
      setErrAnalytics(null);
      setErrAnalyticsPrevious(null);
      setErrAnalyticsFacets(null);
      setErrAnalyticsTokens(null);
      if (reset) {
        setAnalyticsConversations([]);
        setAnalyticsPreviousConversations([]);
        setAnalyticsAggregate(null);
        setAnalyticsPreviousAggregate(null);
        setAnalyticsFacetConversations([]);
        setAnalyticsTotalConversations(null);
        setAnalyticsPreviousTotalConversations(null);
        setAnalyticsFacetTotalConversations(null);
        setAnalyticsTokenPoints([]);
        setAnalyticsTokenIntervalMs(0);
      }

      const now = Date.now();
      const range = timeRangeOption(analyticsRange);
      const before = new Date(now).toISOString();
      const currentParams = new URLSearchParams({ limit: String(ANALYTICS_LIST_SIZE), before });
      const previousParams = new URLSearchParams({ limit: String(ANALYTICS_LIST_SIZE) });
      const tokenParams = new URLSearchParams({ before });
      if (range.ms != null) {
        const currentStart = now - range.ms;
        currentParams.set('since', new Date(currentStart).toISOString());
        previousParams.set('since', new Date(currentStart - range.ms).toISOString());
        previousParams.set('before', new Date(currentStart).toISOString());
        tokenParams.set('since', new Date(currentStart).toISOString());
        tokenParams.set('interval', String(Math.round(chartBucketMs(range.ms) / 1000)));
      }
      const facetParams = new URLSearchParams(currentParams);
      if (analyticsWorkspace != null) {
        currentParams.set('workspace', analyticsWorkspace);
        previousParams.set('workspace', analyticsWorkspace);
        tokenParams.set('workspace', analyticsWorkspace);
      }

      const currentRequest = fetch(`/api/v1/metrics/conversations?${currentParams}`).then((response) =>
        readJSON<ConversationMetricsResponse>(response),
      );
      const previousRequest =
        range.ms == null
          ? Promise.resolve<ConversationMetricsResponse | null>(null)
          : fetch(`/api/v1/metrics/conversations?${previousParams}`).then((response) =>
              readJSON<ConversationMetricsResponse>(response),
            );
      const facetRequest =
        analyticsWorkspace == null
          ? Promise.resolve<ConversationMetricsResponse | null>(null)
          : fetch(`/api/v1/metrics/conversations?${facetParams}`).then((response) =>
              readJSON<ConversationMetricsResponse>(response),
            );
      const tokenRequest = fetch(`/api/v1/metrics/tokens?${tokenParams}`).then((response) =>
        readJSON<TokenUsageResponse>(response),
      );

      const settle = <T,>(request: Promise<T>, apply: (value: T) => void, reject: (reason: unknown) => void) =>
        request.then(
          (value) => {
            if (analyticsSeqRef.current === seq) apply(value);
          },
          (reason) => {
            if (analyticsSeqRef.current === seq) reject(reason);
          },
        );
      const requests = [
        settle(
          currentRequest,
          (value) => {
            setAnalyticsConversations(value.conversations || []);
            setAnalyticsAggregate(value.aggregate || null);
            setAnalyticsTotalConversations(
              Number.isFinite(value.matched_conversations) ? value.matched_conversations : null,
            );
            if (analyticsWorkspace == null) {
              setAnalyticsFacetConversations(value.conversations || []);
              setAnalyticsFacetTotalConversations(
                Number.isFinite(value.matched_conversations) ? value.matched_conversations : null,
              );
            }
          },
          (reason) => setErrAnalytics(errorMessage(reason)),
        ),
        settle(
          previousRequest,
          (value) => {
            setAnalyticsPreviousConversations(value?.conversations || []);
            setAnalyticsPreviousAggregate(value?.aggregate || null);
            setAnalyticsPreviousTotalConversations(
              value && Number.isFinite(value.matched_conversations) ? value.matched_conversations : null,
            );
          },
          (reason) => setErrAnalyticsPrevious(errorMessage(reason)),
        ),
        settle(
          facetRequest,
          (value) => {
            if (!value) return;
            setAnalyticsFacetConversations(value.conversations || []);
            setAnalyticsFacetTotalConversations(
              Number.isFinite(value.matched_conversations) ? value.matched_conversations : null,
            );
          },
          (reason) => setErrAnalyticsFacets(errorMessage(reason)),
        ),
        settle(
          tokenRequest,
          (value) => {
            setAnalyticsTokenPoints(value.points || []);
            const seconds = Number(value.interval_seconds);
            setAnalyticsTokenIntervalMs(Number.isFinite(seconds) && seconds > 0 ? seconds * 1000 : 0);
            setLoadingAnalyticsTokens(false);
          },
          (reason) => {
            setErrAnalyticsTokens(errorMessage(reason));
            setLoadingAnalyticsTokens(false);
          },
        ),
      ];
      return Promise.all(requests).finally(() => {
        if (analyticsSeqRef.current !== seq) return;
        setLoadingAnalytics(false);
      });
    },
    [analyticsRange, analyticsWorkspace, readJSON],
  );

  const skillsToolsSeqRef = useRef(0);
  const fetchSkillsTools = useCallback(
    (reset = false) => {
      const seq = ++skillsToolsSeqRef.current;
      setLoadingSkillsTools(true);
      setErrSkillsTools(null);
      if (reset) {
        setSkillsTools(null);
        setSkillsToolsWindow(null);
      }
      const now = Date.now();
      const range = timeRangeOption(analyticsRange);
      const before = new Date(now).toISOString();
      const since = range.ms == null ? undefined : new Date(now - range.ms).toISOString();
      const params = new URLSearchParams({ before });
      if (since) {
        params.set('since', since);
        params.set('interval', String(Math.round(chartBucketMs(range.ms || 0) / 1000)));
      }
      if (analyticsWorkspace != null) params.set('workspace', analyticsWorkspace);
      return fetch(`/api/v1/metrics/skills-tools?${params}`)
        .then((response) => readJSON<SkillsToolsMetricsResponse>(response))
        .then((body) => {
          if (skillsToolsSeqRef.current !== seq) return;
          setSkillsTools(body.tools);
          setSkillsToolsWindow({ since, before });
        })
        .catch((error) => {
          if (skillsToolsSeqRef.current !== seq) return;
          const status = errorMessage(error);
          setErrSkillsTools(status.includes('404') ? 'A newer daemon is required.' : status);
        })
        .finally(() => {
          if (skillsToolsSeqRef.current !== seq) return;
          setLoadingSkillsTools(false);
        });
    },
    [analyticsRange, analyticsWorkspace, readJSON],
  );

  const analyticsHeatmapSeqRef = useRef(0);
  const fetchAnalyticsHeatmap = useCallback(
    (reset = false) => {
      const seq = ++analyticsHeatmapSeqRef.current;
      setLoadingAnalyticsHeatmap(true);
      setErrAnalyticsHeatmap(null);
      if (reset) setAnalyticsHeatmapPoints([]);
      const now = Date.now();
      const params = new URLSearchParams({
        since: new Date(now - 7 * 24 * 60 * 60 * 1000).toISOString(),
        before: new Date(now).toISOString(),
        interval: '900',
      });
      if (analyticsWorkspace != null) params.set('workspace', analyticsWorkspace);
      return fetch(`/api/v1/metrics/tokens?${params}`)
        .then((response) => readJSON<TokenUsageResponse>(response))
        .then((body) => {
          if (analyticsHeatmapSeqRef.current !== seq) return;
          setAnalyticsHeatmapPoints(body.points || []);
        })
        .catch((error) => {
          if (analyticsHeatmapSeqRef.current !== seq) return;
          setErrAnalyticsHeatmap(String(error.message || error));
        })
        .finally(() => {
          if (analyticsHeatmapSeqRef.current !== seq) return;
          setLoadingAnalyticsHeatmap(false);
        });
    },
    [analyticsWorkspace, readJSON],
  );

  const analyticsRefreshInFlightRef = useRef(false);
  const analyticsRefreshDirtyRef = useRef(false);
  const refreshAnalyticsRef = useRef<() => void>(() => {});
  const refreshAnalytics = useCallback(() => {
    if (analyticsRefreshInFlightRef.current) {
      analyticsRefreshDirtyRef.current = true;
      return;
    }
    analyticsRefreshInFlightRef.current = true;
    const request =
      analyticsTab === 'skills' ? fetchSkillsTools() : Promise.all([fetchAnalytics(), fetchAnalyticsHeatmap()]);
    Promise.resolve(request).finally(() => {
      analyticsRefreshInFlightRef.current = false;
      if (!analyticsRefreshDirtyRef.current) return;
      analyticsRefreshDirtyRef.current = false;
      refreshAnalyticsRef.current();
    });
  }, [analyticsTab, fetchAnalytics, fetchAnalyticsHeatmap, fetchSkillsTools]);

  // refreshAll keeps one refresh cycle in flight. An event arriving while
  // a cycle runs marks at most one follow-up refresh as due instead of
  // starting a second cycle. Without that, a slow response lets one
  // request per debounce window pile up until the browser's
  // six-connection limit stops them, and each one is a full-store read.
  const refreshInFlightRef = useRef(false);
  const refreshDirtyRef = useRef(false);
  // The follow-up runs through refreshAllRef rather than through the
  // captured refreshAll: a range change during the cycle replaces the
  // callback, and the follow-up has to request the range the header now
  // names. The effect below fills the ref on the first render, before any
  // response can land.
  const refreshAllRef = useRef<(() => void) | null>(null);
  const refreshAll = useCallback(() => {
    if (refreshInFlightRef.current) {
      refreshDirtyRef.current = true;
      return;
    }
    refreshInFlightRef.current = true;
    Promise.all([fetchList(), fetchTokens()]).finally(() => {
      refreshInFlightRef.current = false;
      if (!refreshDirtyRef.current) return;
      refreshDirtyRef.current = false;
      refreshAllRef.current?.();
    });
  }, [fetchList, fetchTokens]);

  // reloadRange refetches when the request window itself changed: mount,
  // or a range change. Both callbacks close over timeRange, so this
  // identity moves exactly then, and the effect below is the only caller
  // that discards what the previous window returned.
  const reloadRange = useCallback(() => {
    fetchList(true);
    fetchTokens(true);
  }, [fetchList, fetchTokens]);

  // fetchDetailCore is the shared fetch body for both an explicit
  // open (quiet=false: shows a spinner and clears stale content) and
  // a live-update refresh (quiet=true: updates in place, keeps the
  // current view and scroll on success, swallows transient errors).
  //
  // The success path applies the body only if the user is still on
  // the same conversation, and clears any prior error so a recovered
  // live refresh doesn't stay hidden behind a stale error banner.
  const fetchDetailCore = useCallback((id: string, quiet: boolean) => {
    if (!quiet) {
      setLoadingDetail(true);
      setErrDetail(null);
      setDetail(null);
    }
    return fetch(`/api/v1/conversations/${encodeURIComponent(id)}`)
      .then((r) => {
        if (r.status === 404) throw new Error('Session not found in the local store.');
        if (!r.ok) return r.text().then((t) => Promise.reject(new Error(t || `HTTP ${r.status}`)));
        return r.json();
      })
      .then((body: ConversationDetail) => {
        if (selectedIDRef.current !== id) return;
        setDetail(body);
        setErrDetail(null);
      })
      .catch((e) => {
        if (selectedIDRef.current !== id) return;
        // Quiet refresh failures are swallowed; the next event
        // retries and the current view stays as-is instead of
        // flashing an error banner over good content. The 60s
        // backstop only refreshes the list, so a missed detail
        // event only recovers on another targeted event or when
        // the user reopens the conversation.
        if (!quiet) setErrDetail(String(e.message || e));
      })
      .finally(() => {
        if (!quiet) setLoadingDetail(false);
      });
  }, []);
  const fetchDetail = useCallback((id: string) => fetchDetailCore(id, false), [fetchDetailCore]);
  const quietRefreshDetail = useCallback((id: string) => fetchDetailCore(id, true), [fetchDetailCore]);

  useEffect(() => {
    reloadRange();
  }, [reloadRange]);

  useEffect(() => {
    if (view === 'analytics' && analyticsTab === 'overview') fetchAnalytics(true);
  }, [view, analyticsTab, fetchAnalytics]);

  useEffect(() => {
    if (view === 'analytics' && analyticsTab === 'overview') fetchAnalyticsHeatmap(true);
  }, [view, analyticsTab, fetchAnalyticsHeatmap]);

  useEffect(() => {
    if (view === 'analytics' && analyticsTab === 'skills') fetchSkillsTools(true);
  }, [view, analyticsTab, fetchSkillsTools]);

  useEffect(() => {
    const onPopState = () => {
      setSelectedID(conversationIDFromPath());
      setShowSettings(settingsRouteActive());
      setShowAnalytics(analyticsRouteActive());
      setSettingsTab(settingsTabFromLocation());
      setAnalyticsTab(analyticsTabFromLocation());
      setToolSessionFilters(toolSessionFiltersFromLocation());
      setWorkspace(workspaceFromLocation());
    };
    window.addEventListener('popstate', onPopState);
    return () => window.removeEventListener('popstate', onPopState);
  }, []);

  useEffect(() => {
    if (!selectedID) {
      setDetail(null);
      setErrDetail(null);
      setLoadingDetail(false);
      return;
    }
    fetchDetail(selectedID);
  }, [selectedID, fetchDetail]);

  // Live updates from the daemon. One persistent SSE connection per
  // viewer; the server pushes {conversation_id, generation_id}
  // whenever a new generation is recorded so the list (and an open matching
  // conversation) refresh within ~1s without polling. Refs hold the
  // latest callbacks so the effect mounts once and doesn't drop the
  // connection on every render.
  const quietRefreshDetailRef = useRef(quietRefreshDetail);
  const selectedIDRef = useRef(selectedID);
  const viewRef = useRef(view);
  // importActiveUntilRef holds the instant the import cadence lapses. The
  // SSE handler is mounted once and cannot read the state the banner
  // renders from, so it tracks the run itself. It is a deadline rather
  // than a flag, see IMPORT_ACTIVE_TTL_MS.
  const importActiveUntilRef = useRef(0);
  useEffect(() => {
    refreshAllRef.current = refreshAll;
  }, [refreshAll]);
  useEffect(() => {
    quietRefreshDetailRef.current = quietRefreshDetail;
  }, [quietRefreshDetail]);
  useEffect(() => {
    refreshAnalyticsRef.current = refreshAnalytics;
  }, [refreshAnalytics]);
  useEffect(() => {
    selectedIDRef.current = selectedID;
  }, [selectedID]);
  useEffect(() => {
    viewRef.current = view;
  }, [view]);

  useEffect(() => {
    // Browsers without EventSource (vanishingly rare on modern
    // desktop browsers, but possible in some embedded webviews)
    // fall back to the 60s backstop refresh below instead of
    // throwing on the constructor.
    if (typeof EventSource === 'undefined') return;
    // Debounce so a burst export (one frame per generation) does
    // not trigger one refresh per frame. We only need one list
    // refresh per burst, plus one detail refresh if any event in
    // the burst targets the open conversation. The two are armed
    // separately because an import slows the list down and the open
    // conversation keeps the normal cadence.
    let listTimer: ReturnType<typeof setTimeout> | null = null;
    let detailTimer: ReturnType<typeof setTimeout> | null = null;
    // onConversations reports whether a data-backed screen is rendered.
    // Elsewhere the SSE connection itself is cheap to leave running.
    const onConversations = () => {
      const v = viewRef.current;
      return v === 'conversations' || v === 'conversation' || v === 'analytics';
    };
    const importActive = () => importActiveUntilRef.current > Date.now();
    const flushList = () => {
      listTimer = null;
      if (!onConversations()) return;
      if (viewRef.current === 'analytics') refreshAnalyticsRef.current();
      else refreshAllRef.current?.();
    };
    const flushDetail = () => {
      detailTimer = null;
      const openID = selectedIDRef.current;
      if (!openID || !onConversations()) return;
      quietRefreshDetailRef.current(openID);
    };
    const es = new EventSource('/api/v1/events');
    es.onmessage = (e) => {
      let ev: StreamEvent = {};
      try {
        ev = JSON.parse(e.data || '{}') as StreamEvent;
      } catch (_) {
        /* ignore */
      }
      if (ev?.import) {
        // Import events carry state rather than naming a conversation, so
        // they are applied directly and skip the refetch debounce.
        const active = importRunIsActive(ev.import);
        importActiveUntilRef.current = active ? Date.now() + IMPORT_ACTIVE_TTL_MS : 0;
        setImportEvent(ev.import);
        // A finished run may be followed by no further change event, and
        // the list is then as stale as the import left it. Arm one refresh
        // on any frame reporting a terminal status; the timer guard folds
        // the several a run's last updates produce into one.
        if (!active && listTimer === null) {
          listTimer = setTimeout(flushList, REFRESH_DEBOUNCE_MS);
        }
        return;
      }
      const openID = selectedIDRef.current;
      if (openID && ev && ev.conversation_id === openID && detailTimer === null) {
        detailTimer = setTimeout(flushDetail, REFRESH_DEBOUNCE_MS);
      }
      if (listTimer === null) {
        listTimer = setTimeout(flushList, importActive() ? IMPORT_REFRESH_DEBOUNCE_MS : REFRESH_DEBOUNCE_MS);
      }
    };
    // EventSource auto-reconnects on transport errors, so a daemon
    // restart or proxy blip heals without an explicit handler.
    // Cleanup closes the stream.
    return () => {
      if (listTimer !== null) clearTimeout(listTimer);
      if (detailTimer !== null) clearTimeout(detailTimer);
      es.close();
    };
  }, []);

  // Safety-net backstop: in case SSE stalls (a reverse proxy that
  // buffers, a dropped event whose burst leaves the list out of
  // date, an environment where EventSource is unavailable), refetch
  // the list at a low rate. Detail view is intentionally not on the
  // backstop — opening a step shouldn't move under the user except
  // when a live event names it. As a consequence, a dropped detail
  // event for the currently-open conversation only recovers when
  // another targeted event arrives or the user reopens it.
  useEffect(() => {
    if (view !== 'conversations' && view !== 'analytics') return;
    const id = setInterval(() => {
      if (view === 'analytics') refreshAnalyticsRef.current();
      else refreshAllRef.current?.();
    }, 60_000);
    return () => clearInterval(id);
  }, [view]);

  const openConv = (c: { id: string }) => {
    const returnState: DetailReturnState = {
      view: view === 'analytics' ? 'analytics' : 'conversations',
      workspace: view === 'conversations' ? workspace : null,
      returnPath: `${window.location.pathname}${window.location.search}`,
    };
    window.history.pushState(returnState, '', conversationPath(c.id));
    setShowSettings(false);
    setShowAnalytics(false);
    setSelectedID(c.id);
  };
  const goConversations = () => {
    window.history.pushState({}, '', conversationsPath());
    setShowSettings(false);
    setShowAnalytics(false);
    setSelectedID(null);
    setWorkspace(null);
    setToolSessionFilters(null);
  };
  const backFromDetail = () => {
    const state = window.history.state as DetailReturnState | null;
    setShowSettings(false);
    setSelectedID(null);
    if (state?.view === 'analytics') {
      const path = state.returnPath || analyticsPath('overview');
      window.history.pushState({}, '', path);
      setAnalyticsTab(path.includes('tab=skills') ? 'skills' : 'overview');
      setShowAnalytics(true);
      return;
    }
    const returnWorkspace = state?.workspace ?? null;
    const path = state?.returnPath || conversationsPath(returnWorkspace);
    window.history.pushState({}, '', path);
    setShowAnalytics(false);
    setWorkspace(returnWorkspace);
    setToolSessionFilters(toolSessionFiltersFromLocation());
  };
  const goAnalytics = () => {
    window.history.pushState({}, '', analyticsPath('overview'));
    setShowSettings(false);
    setShowAnalytics(true);
    setAnalyticsTab('overview');
    setSelectedID(null);
  };
  const selectAnalyticsTab = (tab: AnalyticsTab) => {
    window.history.pushState({}, '', analyticsPath(tab));
    setAnalyticsTab(tab);
  };
  const openToolSessions = (filters: ToolSessionFilters) => {
    window.history.pushState({}, '', toolSessionsPath(filters));
    setTimeRange(analyticsRange);
    setShowSettings(false);
    setShowAnalytics(false);
    setSelectedID(null);
    setWorkspace(filters.workspace);
    setToolSessionFilters(filters);
    setBucketSel(null);
  };
  const openAnalyticsBucket = (span: TimeSpan) => {
    window.history.pushState({}, '', conversationsPath(analyticsWorkspace));
    setTimeRange(analyticsRange);
    setWorkspace(analyticsWorkspace);
    setBucketSel(span);
    setShowSettings(false);
    setShowAnalytics(false);
    setSelectedID(null);
    setToolSessionFilters(null);
  };
  const openAnalyticsWorkspace = (path: string) => {
    window.history.pushState({}, '', conversationsPath(path));
    setTimeRange(analyticsRange);
    setWorkspace(path);
    setBucketSel(null);
    setShowSettings(false);
    setShowAnalytics(false);
    setSelectedID(null);
    setToolSessionFilters(null);
  };
  const selectConversationWorkspace = (path: string | null) => {
    const filters = toolSessionFilters ? { ...toolSessionFilters, workspace: path } : null;
    window.history.pushState({}, '', filters ? toolSessionsPath(filters) : conversationsPath(path));
    setWorkspace(path);
    setToolSessionFilters(filters);
  };
  // goSettings is also the nav tab's onClick, which passes an event, so
  // anything that is not a tab id opens the Cloud tab.
  const goSettings = (tab?: string | ReactMouseEvent<HTMLAnchorElement>) => {
    const next = typeof tab === 'string' && SETTINGS_TAB_IDS.has(tab) ? tab : 'cloud';
    window.history.pushState({}, '', settingsPath(next));
    setSelectedID(null);
    setShowAnalytics(false);
    setShowSettings(true);
    setSettingsTab(next);
  };
  const selectSettingsTab = (tab: string) => {
    if (!SETTINGS_TAB_IDS.has(tab)) return;
    window.history.pushState({}, '', settingsPath(tab));
    setSettingsTab(tab);
  };
  const focusConversationSearch = useCallback(() => {
    const focus = () => {
      const el = conversationSearchRef.current;
      if (!el) return;
      el.focus();
      if (typeof el.select === 'function') el.select();
    };
    if (viewRef.current !== 'conversations') {
      window.history.pushState({}, '', conversationsPath());
      setSelectedID(null);
      setShowSettings(false);
      setShowAnalytics(false);
      setWorkspace(null);
      setToolSessionFilters(null);
      setTimeout(focus, 0);
      return;
    }
    focus();
  }, []);
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && String(e.key).toLowerCase() === 'k') {
        e.preventDefault();
        focusConversationSearch();
      }
    };
    window.addEventListener('keydown', onKeyDown);
    return () => window.removeEventListener('keydown', onKeyDown);
  }, [focusConversationSearch]);

  const tabs = [
    {
      key: 'conversations',
      label: 'Sessions',
      href: '/',
      onClick: goConversations,
    },
    {
      key: 'analytics',
      label: 'Analytics',
      href: analyticsPath('overview'),
      onClick: goAnalytics,
    },
    {
      key: 'settings',
      label: 'Settings',
      href: '/settings',
      onClick: goSettings,
    },
  ];
  const activeTab = view === 'settings' ? 'settings' : view === 'analytics' ? 'analytics' : 'conversations';
  const detailReturnState = window.history.state as DetailReturnState | null;
  const detailReturnsToAnalytics = detailReturnState?.view === 'analytics';
  const detailBackHref =
    detailReturnState?.returnPath ||
    (detailReturnsToAnalytics ? analyticsPath('overview') : conversationsPath(detailReturnState?.workspace ?? null));

  return (
    <div
      style={{
        minHeight: '100vh',
        display: 'flex',
        flexDirection: 'column',
      }}
    >
      {/* A failed poll drops the chip to Unknown rather than leaving it
              asserting a posture the daemon can no longer confirm. */}
      <TopBar tabs={tabs} activeTab={activeTab} config={configErr ? null : config} onOpenSettings={goSettings} />
      <div
        style={{
          flex: 1,
          display: 'flex',
          flexDirection: 'column',
          minHeight: 0,
        }}
      >
        {view === 'settings' && (
          <SettingsView
            history={history}
            config={config}
            configError={configErr}
            activeSettingsTab={settingsTab}
            onSelectTab={selectSettingsTab}
            onConfig={applyConfig}
          />
        )}
        {view === 'analytics' && (
          <AnalyticsPage
            stats={
              analyticsTab === 'overview'
                ? analyticsOverviewHeroStats({
                    conversations: analyticsConversations,
                    aggregate: analyticsAggregate,
                    totalConversations: analyticsTotalConversations,
                  })
                : skillsToolsHeroStats(skillsTools)
            }
            tabs={{ active: analyticsTab, onSelect: selectAnalyticsTab }}
            style={analyticsTab === 'skills' ? { paddingBottom: 40 } : undefined}
          >
            {analyticsTab === 'overview' ? (
              <AnalyticsOverviewContent
                conversations={analyticsConversations}
                previousConversations={analyticsPreviousConversations}
                aggregate={analyticsAggregate}
                previousAggregate={analyticsPreviousAggregate}
                facetConversations={analyticsFacetConversations}
                totalConversations={analyticsTotalConversations}
                previousTotalConversations={analyticsPreviousTotalConversations}
                facetTotalConversations={analyticsFacetTotalConversations}
                tokenPoints={analyticsTokenPoints}
                tokenIntervalMs={analyticsTokenIntervalMs}
                heatmapPoints={analyticsHeatmapPoints}
                loading={loadingAnalytics}
                tokenLoading={loadingAnalyticsTokens}
                heatmapLoading={loadingAnalyticsHeatmap}
                error={errAnalytics}
                previousError={errAnalyticsPrevious}
                facetError={errAnalyticsFacets}
                tokenError={errAnalyticsTokens}
                heatmapError={errAnalyticsHeatmap}
                unit={analyticsUnit}
                onUnitChange={setAnalyticsUnit}
                timeRange={analyticsRange}
                onTimeRangeChange={setAnalyticsRange}
                workspace={analyticsWorkspace}
                onWorkspaceChange={(path) => setAnalyticsWorkspaceValue(encodeAnalyticsWorkspace(path))}
                hiddenSeries={analyticsHiddenSeries}
                onToggleSeries={toggleAnalyticsSeries}
                onRefresh={refreshAnalytics}
                refreshing={loadingAnalytics || loadingAnalyticsHeatmap}
                onOpenConversation={openConv}
                onOpenWorkspace={openAnalyticsWorkspace}
                onOpenBucket={openAnalyticsBucket}
              />
            ) : (
              <SkillsToolsContent
                data={skillsTools}
                loading={loadingSkillsTools}
                error={errSkillsTools}
                timeRange={analyticsRange}
                onTimeRangeChange={setAnalyticsRange}
                workspace={analyticsWorkspace}
                onWorkspaceChange={(path) => setAnalyticsWorkspaceValue(encodeAnalyticsWorkspace(path))}
                window={skillsToolsWindow}
                onRefresh={refreshAnalytics}
                refreshing={loadingSkillsTools}
                onOpenSessions={openToolSessions}
              />
            )}
          </AnalyticsPage>
        )}
        {view === 'conversations' && (
          <ConversationsView
            conversations={conversations}
            storeCount={storeCount}
            tokenPoints={tokenPoints}
            tokenIntervalMs={tokenIntervalMs}
            loading={loadingList}
            error={errList}
            query={query}
            setQuery={setQuery}
            searchInputRef={conversationSearchRef}
            timeRange={timeRange}
            setTimeRange={changeTimeRange}
            tokenModel={tokenModel}
            setTokenModel={setTokenModel}
            chartMetric={chartMetric}
            setChartMetric={setChartMetric}
            bucketSel={bucketSel}
            setBucketSel={setBucketSel}
            workspace={workspace}
            setWorkspace={selectConversationWorkspace}
            groupBy={groupBy}
            setGroupBy={setGroupBy}
            listSort={listSort}
            setListSort={setListSort}
            onOpen={openConv}
            onRefresh={refreshAll}
            refreshing={loadingList}
            onOpenSettings={goSettings}
            history={history}
            {...(toolSessionFilters
              ? { toolFilter: toolSessionFilters, onClearToolFilter: clearToolSessionFilter }
              : { toolFilter: null })}
          />
        )}
        {view === 'conversation' && selected && (
          <TraceDetailView
            conv={selected}
            detail={detail}
            loading={loadingDetail}
            error={errDetail}
            backHref={detailBackHref}
            backLabel={detailReturnsToAnalytics ? 'Analytics' : 'Sessions'}
            onBack={backFromDetail}
          />
        )}
      </div>
    </div>
  );
}
