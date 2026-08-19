const {
    useState,
    useEffect,
    useMemo,
    useCallback,
    useRef,
    createContext,
    useContext,
} = React;

// ============================================================
// Formatters — all server responses ship raw numbers + RFC3339
// timestamps, the UI humanizes them so it can re-render relative
// labels without re-fetching.
// ============================================================

// NO_VALUE is what every formatter returns for a value the source did not
// record. One constant so a missing token count, duration, date, and cost
// all read the same in a table.
const NO_VALUE = "-";

function formatTokens(n) {
    if (n == null || isNaN(n)) return NO_VALUE;
    if (n < 1000) return String(n);
    if (n < 1_000_000)
        return (
            (n / 1_000).toFixed(n < 10_000 ? 1 : 1).replace(/\.0$/, "") + "k"
        );
    return (
        (n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 1).replace(/\.0$/, "") +
        "M"
    );
}

function formatDuration(seconds) {
    if (seconds == null || isNaN(seconds)) return NO_VALUE;
    if (seconds < 1) return "<1s";
    if (seconds < 60)
        return seconds.toFixed(seconds < 10 ? 2 : 1).replace(/\.0+$/, "") + "s";
    const m = Math.floor(seconds / 60);
    const s = Math.round(seconds % 60);
    if (m < 60) return s === 0 ? `${m}m` : `${m}m ${s}s`;
    const h = Math.floor(m / 60);
    const mm = m % 60;
    return mm === 0 ? `${h}h` : `${h}h ${mm}m`;
}

// formatAgo returns a complete relative-time phrase including the
// "ago" suffix where appropriate, so call sites can use it bare
// without adding their own "ago" and producing "just now ago".
function formatAgo(iso, now) {
    if (!iso) return NO_VALUE;
    const t = new Date(iso).getTime();
    if (!Number.isFinite(t)) return NO_VALUE;
    const secs = Math.max(0, Math.round((now - t) / 1000));
    if (secs < 5) return "just now";
    if (secs < 60) return `${secs}s ago`;
    const mins = Math.round(secs / 60);
    if (mins < 60) return `${mins}m ago`;
    const hours = Math.round(mins / 60);
    if (hours < 24) return `${hours}h ago`;
    const days = Math.round(hours / 24);
    return `${days}d ago`;
}

function formatTime(iso) {
    if (!iso) return NO_VALUE;
    const d = new Date(iso);
    if (isNaN(d)) return NO_VALUE;
    return d.toLocaleTimeString([], { hour12: false });
}

function durationBetweenSeconds(startISO, endISO) {
    if (!startISO || !endISO) return null;
    const s = new Date(startISO).getTime();
    const e = new Date(endISO).getTime();
    if (!Number.isFinite(s) || !Number.isFinite(e) || e < s) return null;
    return (e - s) / 1000;
}

const TIME_RANGES = [
    { value: "5m", label: "Last 5 minutes", ms: 5 * 60 * 1000 },
    { value: "15m", label: "Last 15 minutes", ms: 15 * 60 * 1000 },
    { value: "1h", label: "Last 1 hour", ms: 60 * 60 * 1000 },
    { value: "6h", label: "Last 6 hours", ms: 6 * 60 * 60 * 1000 },
    { value: "24h", label: "Last 24 hours", ms: 24 * 60 * 60 * 1000 },
    { value: "7d", label: "Last 7 days", ms: 7 * 24 * 60 * 60 * 1000 },
    { value: "90d", label: "Last 90 days", ms: 90 * 24 * 60 * 60 * 1000 },
    { value: "all", label: "All", ms: null },
];

// DEFAULT_TIME_RANGE matches the 90-day window a history import defaults
// to. A narrower default would open on an empty list right after an
// import, because everything backfilled is older than it.
const DEFAULT_TIME_RANGE = "90d";
const FEED_TIME_RANGES = TIME_RANGES.filter(
    (r) => r.value !== "5m" && r.value !== "15m",
);

// LIST_PAGE_SIZE is how many conversations one list request asks for.
// The rows are not virtualised and the server decodes only what it
// returns, so this bounds both ends. It matches the server's own
// default (conversationListLimit in server.go).
const LIST_PAGE_SIZE = 200;

function timeRangeOption(value) {
    return (
        TIME_RANGES.find((r) => r.value === value) ||
        TIME_RANGES.find((r) => r.value === DEFAULT_TIME_RANGE)
    );
}

function conversationTime(c) {
    const t = new Date(c.last_activity || c.started_at).getTime();
    return Number.isFinite(t) ? t : null;
}

function formatBucketSize(ms) {
    if (!Number.isFinite(ms) || ms <= 0) return "buckets";
    if (ms < 60000) return `${Math.round(ms / 1000)}-sec buckets`;
    const mins = Math.round(ms / 60000);
    if (mins < 60) return `${mins}-min buckets`;
    const hours = Math.round(mins / 60);
    if (hours < 24) return `${hours}-hour buckets`;
    const days = Math.round(hours / 24);
    return `${days}-day buckets`;
}

function formatBucketLabel(ts, bucketMs) {
    const d = new Date(ts);
    if (bucketMs >= 24 * 60 * 60 * 1000) {
        return d.toLocaleDateString([], { month: "short", day: "numeric" });
    }
    const time = d.toLocaleTimeString([], {
        hour: "2-digit",
        minute: "2-digit",
        hour12: false,
    });
    // 2h+ buckets mean the chart spans more than a day, so a bare
    // time is ambiguous — prefix the date.
    if (bucketMs >= 2 * 60 * 60 * 1000) {
        return (
            d.toLocaleDateString([], { month: "short", day: "numeric" }) +
            " " +
            time
        );
    }
    // Sub-minute buckets need seconds or adjacent labels collide.
    if (bucketMs < 60 * 1000) {
        return d.toLocaleTimeString([], {
            hour: "2-digit",
            minute: "2-digit",
            second: "2-digit",
            hour12: false,
        });
    }
    return time;
}

// chartTooltipLeft centers the hover tooltip on its bar but keeps it
// clear of the card edges so the first and last buckets don't clip.
function chartTooltipLeft(i, n) {
    return `${Math.min(88, Math.max(12, ((i + 0.5) / n) * 100))}%`;
}

// Per-model dot colour. New models fall back to a neutral grey
// pulled from the Saga viz palette.
const MODEL_COLORS = {
    "claude-opus-4-7": "#FF8833",
    "claude-opus-4-1": "#FF8833",
    "claude-sonnet-4": "#FF9830",
    "deepseek-v4-pro": "#5794F2",
    "gpt-5-omni": "#73BF69",
};
function modelDot(name) {
    if (!name) return "#808080";
    return MODEL_COLORS[name] || "#808080";
}

// shortModel trims the vendor prefix and dated snapshot suffix so a list
// pill reads "opus-4-8" / "haiku-4-5" instead of the full
// "claude-haiku-4-5-20251001". The dot still colours by the full id.
function shortModel(name) {
    return (name || "").replace(/^claude-/, "").replace(/-\d{8}$/, "");
}

// Token-usage chart series. The server splits each generation into
// these five non-overlapping buckets (provider-aware, see
// disjointTokenUsage in query.go), so stacking them never
// double-counts. Order is bottom-to-top in the stack.
const TOKEN_SERIES = [
    { key: "fresh_input", label: "Input", color: "var(--viz-blue)" },
    { key: "cache_read", label: "Cache read", color: "var(--viz-green)" },
    { key: "cache_write", label: "Cache write", color: "var(--viz-purple)" },
    { key: "output", label: "Output", color: "var(--viz-orange)" },
    { key: "reasoning", label: "Reasoning", color: "var(--viz-yellow)" },
];

// tokenBreakdownTitle renders disjoint token buckets as a multi-line
// native tooltip for the list's Tokens cell.
function tokenBreakdownTitle(buckets) {
    if (!buckets) return undefined;
    const lines = TOKEN_SERIES.filter((s) => buckets[s.key] > 0).map(
        (s) => `${s.label}: ${formatTokens(buckets[s.key])}`,
    );
    return lines.length ? lines.join("\n") : undefined;
}

// Prompt-input cache hit rate: cache reads divided by all prompt-input
// cache outcomes. Cache writes are misses that populated future cache
// entries, so they belong in the denominator but never the numerator.
function cacheInputHitPercent(freshInput, cacheRead, cacheWrite) {
    const fresh = Math.max(0, Number(freshInput) || 0);
    const read = Math.max(0, Number(cacheRead) || 0);
    const write = Math.max(0, Number(cacheWrite) || 0);
    const denom = fresh + read + write;
    if (denom === 0) return null;
    if (read === denom) return 100;
    return Math.min(99, Math.round((read / denom) * 100));
}

// Per-model price in USD per million tokens (input / output). Cache reads
// bill at ~0.1x input and 5-minute cache writes at ~1.25x input — the
// published Anthropic multipliers. Matched by substring so the bare model
// id (claude-opus-4-8) resolves without an exact-version table.
//
// Anthropic only — agento11y also captures OpenAI / Gemini / etc. sessions,
// and we don't carry authoritative prices for those (they drift). An
// unrecognised model returns null rather than a fabricated dollar figure:
// better to show NO_VALUE than to price a GPT/Gemini run at Claude rates.
// ponytail: add a row here when a provider's prices are known and stable;
// don't guess them.
const MODEL_PRICES = [
    { match: "fable", in: 10, out: 50 },
    { match: "opus", in: 5, out: 25 },
    { match: "sonnet", in: 3, out: 15 },
    { match: "haiku", in: 1, out: 5 },
];

function modelPrice(model) {
    const m = (model || "").toLowerCase();
    return MODEL_PRICES.find((p) => m.includes(p.match)) || null;
}

// models.dev is the authoritative, multi-provider price catalog (OpenAI,
// Anthropic, Gemini, …) — strictly better than the bundled table, and it
// carries explicit per-model cache_read / cache_write rates instead of the
// 0.1x / 1.25x assumption. We fetch it once, flatten to
// { modelId: {input, output, cache_read, cache_write} } in USD/MTok, and
// cache it in localStorage for a day. Offline or unknown id → the bundled
// Anthropic table (modelPrice) → null.
const MODELS_DEV_URL = "https://models.dev/api.json";
const PRICE_CACHE_KEY = "sigil.modelPrices.v1";
const PRICE_TTL_MS = 24 * 60 * 60 * 1000;
let modelPricesPromise = null;

function flattenModelsDev(data) {
    const map = {};
    for (const provider of Object.values(data || {})) {
        const models = provider && provider.models;
        if (!models) continue;
        for (const [id, m] of Object.entries(models)) {
            if (m && m.cost && m.cost.input != null) map[id] = m.cost;
        }
    }
    return map;
}

function loadModelPrices() {
    if (modelPricesPromise) return modelPricesPromise;
    modelPricesPromise = (async () => {
        try {
            const cached = JSON.parse(
                localStorage.getItem(PRICE_CACHE_KEY) || "null",
            );
            if (cached && cached.map && Date.now() - cached.at < PRICE_TTL_MS)
                return cached.map;
        } catch {
            /* corrupt cache, refetch */
        }
        const resp = await fetch(MODELS_DEV_URL);
        if (!resp.ok) throw new Error(`models.dev ${resp.status}`);
        const map = flattenModelsDev(await resp.json());
        try {
            localStorage.setItem(
                PRICE_CACHE_KEY,
                JSON.stringify({ at: Date.now(), map }),
            );
        } catch {
            /* quota, skip cache */
        }
        return map;
    })();
    return modelPricesPromise;
}

// useModelPrices resolves to the flattened models.dev map, or null while
// loading / when the fetch fails (the daemon may be offline). Callers must
// tolerate null and fall back to the bundled table.
function useModelPrices() {
    const [prices, setPrices] = useState(null);
    useEffect(() => {
        let alive = true;
        loadModelPrices()
            .then((m) => {
                if (alive) setPrices(m);
            })
            .catch(() => {});
        return () => {
            alive = false;
        };
    }, []);
    return prices;
}

// conversationCost prices a conversation's disjoint token buckets at its
// primary model's rates. Prefers the live models.dev catalog (exact model
// id, all providers); falls back to the bundled Anthropic table for
// brand-new Claude ids or when offline. Exact for the single-model common
// case; a mixed-model conversation is priced at models[0] (the
// orchestrator), a close approximation. Returns null when the model can't
// be priced (unknown provider, or no model recorded) so callers show NO_VALUE
// instead of a fabricated number.
// ponytail: per-model attribution would need per-generation buckets — not
// worth it until mixed-model conversations are common.
function conversationCost(c, prices) {
    const b = c && c.token_buckets;
    if (!b) return null;
    const model = (c.models || [])[0];
    let inRate, outRate, cacheReadRate, cacheWriteRate;
    const live = prices && model && prices[model];
    if (live) {
        inRate = live.input;
        outRate = live.output != null ? live.output : live.input;
        cacheReadRate =
            live.cache_read != null ? live.cache_read : live.input * 0.1;
        cacheWriteRate =
            live.cache_write != null ? live.cache_write : live.input * 1.25;
    } else {
        const p = modelPrice(model);
        if (!p) return null;
        inRate = p.in;
        outRate = p.out;
        cacheReadRate = p.in * 0.1;
        cacheWriteRate = p.in * 1.25;
    }
    return (
        ((b.fresh_input || 0) * inRate +
            (b.cache_read || 0) * cacheReadRate +
            (b.cache_write || 0) * cacheWriteRate +
            ((b.output || 0) + (b.reasoning || 0)) * outRate) /
        1e6
    );
}

function formatCost(usd) {
    if (usd == null) return NO_VALUE; // unpriced model, distinct from $0
    if (usd === 0) return "$0";
    if (usd < 0.01) return "<$0.01";
    if (usd < 1000) return "$" + usd.toFixed(2).replace(/\.00$/, "");
    return "$" + (usd / 1000).toFixed(1) + "k";
}

// List-price token math, not the provider invoice: subscriptions and
// committed-use discounts never reach this table.
const ESTIMATED_COST_TOOLTIP =
    "Estimated from token usage at published model rates. Does not include provider subscription discounts or committed-use pricing, so this can differ from the actual bill.";

function workspaceLabel(path) {
    if (!path) return "(unknown)";
    const parts = path.replace(/\/+$/, "").split("/").filter(Boolean);
    return parts.slice(-2).join("/") || path;
}

function splitWorkspacePath(path) {
    if (!path) return { dir: "", leaf: "(unknown)" };
    const clean = path.replace(/\/+$/, "");
    if (!clean) return { dir: "", leaf: "/" };
    const cut = clean.lastIndexOf("/");
    if (cut < 0) return { dir: "", leaf: clean };
    return {
        dir: clean.slice(0, cut + 1),
        leaf: clean.slice(cut + 1) || clean,
    };
}

// timeWindow computes a chart's [start, end] for a range selection.
// For "All", min/max accumulate in a loop instead of spreading into
// Math.min/Math.max: with one entry per generation the times array
// can be large enough that spread overflows the argument stack
// (RangeError).
function timeWindow(times, rangeValue, now) {
    const range = timeRangeOption(rangeValue);
    if (range.ms != null) return { start: now - range.ms, end: now };
    let minT = Infinity,
        maxT = -Infinity,
        n = 0;
    for (const t of times) {
        if (!Number.isFinite(t)) continue;
        n++;
        if (t < minT) minT = t;
        if (t > maxT) maxT = t;
    }
    const end = n ? Math.max(now, maxT) : now;
    const start = n ? minT : end - 60 * 60 * 1000;
    return { start, end };
}

// BUCKET_INTERVALS_MS is the ladder the chart and the token endpoint
// share. Every step divides the next, so a point the server aggregated
// on one step always falls inside a single bar of a coarser step.
// This list and tokenUsageIntervals in query.go must stay equal;
// TestBucketLaddersAgree checks that.
const BUCKET_INTERVALS_MS = [
    10_000,
    30_000,
    60_000,
    5 * 60_000,
    15 * 60_000,
    30 * 60_000,
    60 * 60_000,
    2 * 60 * 60_000,
    4 * 60 * 60_000,
    12 * 60 * 60_000,
    24 * 60 * 60_000,
    7 * 24 * 60 * 60_000,
];
// CHART_BUCKET_MAX caps how many bars a chart draws. The finest ladder
// step that stays under it is also what the token endpoint aggregates
// on, so the response holds one point per bar per model instead of one
// per generation. Every fixed range gives 10 to 15 bars.
const CHART_BUCKET_MAX = 16;

// chartBucketMs picks the bar width for a span. Past the top of the
// ladder it widens the last step by a whole multiple, so a decade of
// imported history draws CHART_BUCKET_MAX bars rather than one per week,
// and a server bucket still divides a bar.
function chartBucketMs(spanMs, minMs = 0) {
    const span = Number.isFinite(spanMs) && spanMs > 0 ? spanMs : 60_000;
    const floor = Number.isFinite(minMs) && minMs > 0 ? minMs : 0;
    for (const step of BUCKET_INTERVALS_MS) {
        if (step >= floor && span / step <= CHART_BUCKET_MAX) return step;
    }
    // Divide by one bar fewer than the cap, because chartGrid snaps the
    // window outwards at both ends and that can need an extra bar.
    const top = Math.max(
        BUCKET_INTERVALS_MS[BUCKET_INTERVALS_MS.length - 1],
        floor,
    );
    return top * Math.max(1, Math.ceil(span / (top * (CHART_BUCKET_MAX - 1))));
}

// chartGrid is the bucket layout both charts share: a window snapped to
// the bucket ladder (measured from the epoch, the way the server floors
// its buckets) plus the bar count that follows from it. serverIntervalMs
// is the width the token endpoint aggregated on; a bar is never finer
// than that, or a server bucket would straddle two bars and leave the
// neighbour reading empty.
function chartGrid(times, rangeValue, now, serverIntervalMs = 0) {
    const { start, end } = timeWindow(times, rangeValue, now);
    const bucketMs = chartBucketMs(end - start, serverIntervalMs);
    const gridStart = Math.floor(start / bucketMs) * bucketMs;
    const gridEnd =
        Math.ceil(Math.max(end, gridStart + bucketMs) / bucketMs) * bucketMs;
    return {
        start: gridStart,
        end: gridEnd,
        bucketMs,
        count: Math.round((gridEnd - gridStart) / bucketMs),
    };
}

// requestWindow builds the bounds the viewer sends the server: the page
// size and range for the conversation list, and the range and bucket
// interval for the token chart. A fixed range sends a `since` snapped
// to the same grid the chart draws on; "All" sends neither bound and
// lets the server pick an interval it reports back.
function requestWindow(rangeValue, pageSize, now = Date.now()) {
    const range = timeRangeOption(rangeValue);
    if (range.ms == null) return { limit: pageSize };
    const bucketMs = chartBucketMs(range.ms);
    const start = Math.floor((now - range.ms) / bucketMs) * bucketMs;
    return {
        limit: pageSize,
        since: new Date(start).toISOString(),
        intervalSec: Math.round(bucketMs / 1000),
    };
}

// bucketByTime lays out `count` equal buckets across the selected
// range and folds every in-window item into its bucket: init seeds a
// bucket's counters, add(bucket, item) accumulates one item. Pass
// `window` to share one [start, end] between charts.
function bucketByTime(
    items,
    getTime,
    rangeValue,
    now,
    { count = 12, init, add, window: win },
) {
    const times = items.map(getTime);
    const { start, end } = win || timeWindow(times, rangeValue, now);
    const span = Math.max(end - start, 60 * 1000);
    const bucketMs = span / count;
    const buckets = [];
    for (let i = 0; i < count; i++) {
        const bucketStart = start + i * bucketMs;
        // The last bucket absorbs the end instant, mirroring the clamped
        // index below, so [start, end) tests against bucket bounds agree
        // with where points were counted.
        const bucketEnd = i === count - 1 ? end + 1 : bucketStart + bucketMs;
        buckets.push({
            t: formatBucketLabel(bucketStart, bucketMs),
            start: bucketStart,
            end: bucketEnd,
            ...init(),
        });
    }
    items.forEach((item, i) => {
        const t = times[i];
        if (!Number.isFinite(t) || t < start || t > end) return;
        add(
            buckets[
                Math.min(
                    count - 1,
                    Math.max(0, Math.floor((t - start) / bucketMs)),
                )
            ],
            item,
        );
    });
    return { buckets, bucketLabel: formatBucketSize(bucketMs) };
}

function tokenPointTime(p) {
    return new Date(p.t).getTime();
}

// bucketTokenUsage sums each disjoint token series per bucket. points
// carry an RFC3339 `t` plus the five token fields.
function bucketTokenUsage(points, rangeValue, now, opts = {}) {
    let grandTotal = 0;
    const totals = {};
    for (const s of TOKEN_SERIES) totals[s.key] = 0;
    const result = bucketByTime(points, tokenPointTime, rangeValue, now, {
        ...opts,
        init: () => {
            const b = { total: 0 };
            for (const s of TOKEN_SERIES) b[s.key] = 0;
            return b;
        },
        add: (b, p) => {
            for (const s of TOKEN_SERIES) {
                const v = p[s.key] || 0;
                b[s.key] += v;
                b.total += v;
                totals[s.key] += v;
                grandTotal += v;
            }
        },
    });
    return { ...result, grandTotal, totals };
}

function bucketActivity(conversations, rangeValue, now, opts = {}) {
    return bucketByTime(conversations, conversationTime, rangeValue, now, {
        ...opts,
        init: () => ({ c: 0 }),
        add: (b) => {
            b.c += 1;
        },
    });
}

// ============================================================
// Shell primitives
// ============================================================

function Icon({ name, size = 16, style, className }) {
    const paths = {
        search: (
            <path d="M11 19a8 8 0 1 1 5.3-2L21 21M11 19a8 8 0 0 0 5.3-2L11 19Z" />
        ),
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
        refresh: (
            <path d="M3 12a9 9 0 0 1 15.5-6.3L21 8M21 3v5h-5M21 12a9 9 0 0 1-15.5 6.3L3 16M3 21v-5h5" />
        ),
        book: (
            <path d="M4 4h7a3 3 0 0 1 3 3v13a3 3 0 0 0-3-3H4V4ZM20 4h-3a3 3 0 0 0-3 3v13a3 3 0 0 1 3-3h3V4Z" />
        ),
        bookopen: (
            <path d="M12 6c-2-1.3-4.5-2-7-2v13c2.5 0 5 .7 7 2 2-1.3 4.5-2 7-2V4c-2.5 0-5 .7-7 2Zm0 0v13" />
        ),
        box: (
            <path d="M3 7.5 12 3l9 4.5v9L12 21l-9-4.5v-9Zm0 0 9 4.5m0 0 9-4.5m-9 4.5V21" />
        ),
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
        cloud: (
            <path d="M7 18a4 4 0 0 1-.5-7.97 5 5 0 0 1 9.6-1.37A3.5 3.5 0 0 1 16.5 18H7Z" />
        ),
        sparkle: (
            <path d="M12 3l1.5 5L18 9.5l-5 1.5L12 16l-1.5-5.5L5 9.5 10.5 8 12 3Z" />
        ),
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
            style={{ flexShrink: 0, display: "block", ...(style || {}) }}
        >
            {paths[name]}
        </svg>
    );
}

// GrafanaMark is the official Grafana logo (single path from
// simple-icons) rendered in the Grafana brand orange. currentColor
// wiring lets a parent override the colour without re-pasting the
// path.
function GrafanaMark({ size = 22, color = "var(--brand-orange)" }) {
    return (
        <svg
            width={size}
            height={size}
            viewBox="0 0 24 24"
            aria-label="Grafana"
            role="img"
            style={{ flexShrink: 0, display: "block", color }}
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
                display: "flex",
                alignItems: "center",
                gap: 9,
                userSelect: "none",
            }}
        >
            <GrafanaMark size={22} />
            <span
                style={{
                    fontFamily: "var(--fontFamily)",
                    fontSize: 15,
                    fontWeight: 600,
                    letterSpacing: "-0.01em",
                    color: "var(--fg-max)",
                    whiteSpace: "nowrap",
                }}
            >
                Grafana Agent Observability
            </span>
        </div>
    );
}

function ModelPill({ name, dot }) {
    const color = dot || modelDot(name);
    return (
        <span
            title={name}
            style={{
                display: "inline-flex",
                alignItems: "center",
                gap: 6,
                minWidth: 0,
                maxWidth: "100%",
                padding: "2px 8px",
                border: "1px solid var(--border-medium)",
                borderRadius: 2,
                background: "rgba(204,204,220,0.02)",
                color: "var(--fg1)",
                fontSize: 12,
                fontFamily: "var(--fontFamilyMonospace)",
                whiteSpace: "nowrap",
            }}
        >
            <span
                style={{
                    width: 7,
                    height: 7,
                    borderRadius: "50%",
                    background: color,
                    flexShrink: 0,
                }}
            />
            <span
                style={{
                    minWidth: 0,
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                }}
            >
                {shortModel(name)}
            </span>
        </span>
    );
}

function AgentPill({ name, size }) {
    const full = String(name || "");
    if (!full) return null;
    const sm = size === "sm";
    return (
        <span
            title={full}
            style={{
                display: "inline-flex",
                alignItems: "center",
                gap: sm ? 4 : 5,
                padding: sm ? "1px 6px" : "1px 7px",
                border: "1px solid var(--border-medium)",
                borderRadius: 2,
                background: "rgba(204,204,220,0.04)",
                color: "var(--fg1)",
                fontSize: sm ? 10 : 11,
                fontFamily: "var(--fontFamilyMonospace)",
                whiteSpace: "nowrap",
            }}
        >
            <svg
                width={sm ? 9 : 10}
                height={sm ? 9 : 10}
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="2"
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
function agentHosts(agents) {
    return [
        ...new Set(
            (agents || []).map((a) => String(a).split("/")[0]).filter(Boolean),
        ),
    ];
}

function AgentCell({ agents }) {
    const hosts = agentHosts(agents);
    return (
        <div
            style={{
                display: "flex",
                gap: 6,
                alignItems: "center",
                flexWrap: "wrap",
                minWidth: 0,
            }}
        >
            {hosts.map((h) => (
                <span
                    key={h}
                    title={h}
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 5,
                        padding: "1px 7px",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        background: "rgba(204,204,220,0.04)",
                        color: "var(--fg1)",
                        fontSize: 11,
                        fontFamily: "var(--fontFamilyMonospace)",
                        whiteSpace: "nowrap",
                    }}
                >
                    <svg
                        width={10}
                        height={10}
                        viewBox="0 0 24 24"
                        fill="none"
                        stroke="currentColor"
                        strokeWidth="2"
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
function ModelCell({ models }) {
    const list = models || [];
    const shown = list.slice(0, 2);
    const extra = list.length - shown.length;
    return (
        <div
            style={{
                display: "flex",
                gap: 6,
                alignItems: "center",
                flexWrap: "nowrap",
                minWidth: 0,
                overflow: "hidden",
            }}
        >
            {shown.map((m) => (
                <ModelPill key={m} name={m} />
            ))}
            {extra > 0 && (
                <span
                    title={list.join(", ")}
                    style={{
                        fontSize: 11,
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                    }}
                >
                    +{extra}
                </span>
            )}
        </div>
    );
}

const iconBtn = {
    width: 28,
    height: 28,
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    background: "transparent",
    border: "1px solid transparent",
    color: "var(--fg2)",
    cursor: "pointer",
    borderRadius: 2,
};

// NavTab is one top-nav section link (Sessions / Settings). The active
// tab carries the brand underline bar; the others are muted and hover to
// full white.
function NavTab({ label, href, active, onClick }) {
    return (
        <a
            href={href}
            onClick={(e) => {
                if (!isPlainLeftClick(e)) return;
                e.preventDefault();
                onClick && onClick(e);
            }}
            style={{
                position: "relative",
                display: "inline-flex",
                alignItems: "center",
                alignSelf: "stretch",
                padding: "0 2px",
                fontFamily: "var(--fontFamily)",
                fontSize: 13,
                color: active ? "var(--fg-max)" : "var(--fg2)",
                textDecoration: "none",
                whiteSpace: "nowrap",
                cursor: "pointer",
            }}
            onMouseEnter={(e) => {
                if (!active) e.currentTarget.style.color = "var(--fg-max)";
            }}
            onMouseLeave={(e) => {
                if (!active) e.currentTarget.style.color = "var(--fg2)";
            }}
        >
            {label}
            {active && (
                <span
                    style={{
                        position: "absolute",
                        left: 0,
                        right: 0,
                        bottom: 0,
                        height: 2,
                        background: "var(--brandVertical)",
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
const HEADER_H = 68;

function TopBar({ tabs = [], activeTab, config, onOpenSettings }) {
    return (
        <>
            <header
                style={{
                    height: HEADER_H,
                    background: "var(--bg-primary)",
                    display: "flex",
                    alignItems: "center",
                    padding: "0 16px",
                    gap: 20,
                    position: "sticky",
                    top: 0,
                    zIndex: 5,
                }}
            >
                <Wordmark />
                <div
                    style={{
                        width: 1,
                        height: 28,
                        background: "var(--border-weak)",
                        margin: "0 4px",
                    }}
                />
                <nav
                    style={{
                        display: "flex",
                        alignItems: "center",
                        alignSelf: "stretch",
                        gap: 18,
                        minWidth: 0,
                        flex: 1,
                        overflow: "hidden",
                    }}
                >
                    {tabs.map((t) => (
                        <NavTab
                            key={t.key}
                            label={t.label}
                            href={t.href}
                            active={t.key === activeTab}
                            onClick={t.onClick}
                        />
                    ))}
                </nav>
                <ForwardModeChip
                    config={config}
                    onOpenSettings={onOpenSettings}
                />
            </header>
        </>
    );
}

// ============================================================
// Notices — loading, empty, error states
// ============================================================

function Notice({ kind = "info", title, children }) {
    const tone =
        {
            info: {
                color: "var(--fg2)",
                bg: "rgba(204,204,220,0.03)",
                border: "var(--border-weak)",
                icon: "empty",
            },
            warning: {
                color: "var(--warning-text)",
                bg: "var(--warning-transparent, rgba(247,148,30,0.06))",
                border: "var(--warning-border, var(--border-medium))",
                icon: "alert",
            },
            error: {
                color: "var(--error-text)",
                bg: "rgba(209,14,92,0.06)",
                border: "var(--error-border)",
                icon: "alert",
            },
        }[kind] || {};
    return (
        <div
            style={{
                display: "flex",
                gap: 12,
                alignItems: "flex-start",
                padding: "16px 18px",
                border: `1px solid ${tone.border}`,
                background: tone.bg,
                borderRadius: 2,
                color: tone.color,
                fontSize: 13,
            }}
        >
            <Icon name={tone.icon} size={18} style={{ marginTop: 2 }} />
            <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
                {title && (
                    <div
                        style={{
                            color: "var(--fg-max)",
                            fontWeight: 500,
                            fontSize: 13,
                        }}
                    >
                        {title}
                    </div>
                )}
                <div style={{ color: "var(--fg2)", lineHeight: 1.5 }}>
                    {children}
                </div>
            </div>
        </div>
    );
}

const PAGE_MAX_WIDTH = 1392;
const SURFACE_BG = "rgba(24,27,31,0.88)";
const ACTIVE_PILL_BG = "var(--action-selected, rgba(204,204,220,0.08))";
const PANEL_BG = "rgba(17,18,23,0.42)";

function Box({ as: Component = "div", style, children, ...props }) {
    return (
        <Component {...props} style={style}>
            {children}
        </Component>
    );
}

function Stack({
    as = "div",
    direction = "column",
    gap,
    align,
    justify,
    wrap,
    style,
    children,
    ...props
}) {
    return (
        <Box
            as={as}
            {...props}
            style={{
                display: "flex",
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

function SurfaceCard({ children, style, ...rest }) {
    return (
        <Box
            style={{
                position: "relative",
                overflow: "hidden",
                background: SURFACE_BG,
                border: "1px solid var(--border-weak)",
                borderRadius: 8,
                boxShadow: "0 10px 24px rgba(0,0,0,0.14)",
                ...(style || {}),
            }}
            {...rest}
        >
            {children}
        </Box>
    );
}

function ModalFrame({
    title,
    desc,
    onClose,
    children,
    width = "min(860px, 100%)",
}) {
    return (
        <div
            onClick={onClose}
            style={{
                position: "fixed",
                inset: 0,
                zIndex: 70,
                background: "rgba(0,0,0,0.58)",
                display: "flex",
                alignItems: "flex-start",
                justifyContent: "center",
                padding: "9vh 18px 24px",
            }}
        >
            <div
                onClick={(e) => e.stopPropagation()}
                style={{
                    width,
                    maxHeight: "82vh",
                    overflow: "hidden",
                    background: "var(--bg-secondary)",
                    border: "1px solid var(--border-strong)",
                    borderRadius: 8,
                    boxShadow: "0 18px 54px rgba(0,0,0,0.58)",
                    display: "flex",
                    flexDirection: "column",
                }}
            >
                <div
                    style={{
                        display: "flex",
                        alignItems: "flex-start",
                        justifyContent: "space-between",
                        gap: 16,
                        padding: "16px 18px",
                        borderBottom: "1px solid var(--border-weak)",
                    }}
                >
                    <div style={{ minWidth: 0 }}>
                        <div
                            style={{
                                color: "var(--fg-max)",
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
                                    color: "var(--fg3)",
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
                            border: "none",
                            background: "transparent",
                            color: "var(--fg3)",
                            cursor: "pointer",
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

function PageShell({ children, maxWidth = PAGE_MAX_WIDTH, style }) {
    return (
        <Box
            style={{
                width: "100%",
                maxWidth,
                margin: "0 auto",
                padding: "34px 24px 96px",
                ...(style || {}),
            }}
        >
            {children}
        </Box>
    );
}

function PageHero({ title, desc, descStyle, stats = [], style }) {
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
                borderBottom: "1px solid var(--border-weak)",
                ...(style || {}),
            }}
        >
            <Stack
                direction="row"
                align="baseline"
                gap={12}
                style={{ minWidth: 0, flex: "1 1 320px" }}
            >
                <h1
                    style={{
                        fontSize: 20,
                        lineHeight: 1.2,
                        fontWeight: 600,
                        color: "var(--fg-max)",
                        margin: 0,
                        letterSpacing: "-0.02em",
                        whiteSpace: "nowrap",
                    }}
                >
                    {title}
                </h1>
                {desc && (
                    <Box
                        as="span"
                        style={{
                            fontSize: 12.5,
                            color: "var(--fg3)",
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
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
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 12.5,
                        whiteSpace: "nowrap",
                    }}
                >
                    {stats.map((stat) => (
                        <span
                            key={stat.label}
                            style={{
                                display: "inline-flex",
                                alignItems: "baseline",
                                gap: 6,
                            }}
                        >
                            <Box
                                as="span"
                                style={{ fontSize: 11, color: "var(--fg3)" }}
                            >
                                {stat.label}
                            </Box>
                            <Box
                                as="span"
                                style={{ color: stat.tone || "var(--fg1)" }}
                            >
                                {stat.value}
                            </Box>
                        </span>
                    ))}
                </Stack>
            )}
        </Stack>
    );
}

// PillToggle's md size is for a control that carries a decision rather than
// a view preference: the forwarding mode switch, which is the first thing on
// the Cloud tab.
function PillToggle({ options, value, onChange, size = "sm" }) {
    const md = size === "md";
    return (
        <Stack
            direction="row"
            gap={3}
            style={{
                display: "inline-flex",
                padding: 3,
                border: "1px solid var(--border-medium)",
                borderRadius: 999,
                background: PANEL_BG,
                overflow: "hidden",
                boxShadow: "inset 0 0 0 1px rgba(0,0,0,0.10)",
            }}
        >
            {options.map((o) => {
                const active = o.value === value;
                return (
                    <button
                        key={o.value}
                        type="button"
                        onClick={() => onChange(o.value)}
                        style={{
                            padding: md ? "7px 16px" : "5px 13px",
                            borderRadius: 999,
                            background: active ? ACTIVE_PILL_BG : "transparent",
                            color: active
                                ? "var(--primary-text)"
                                : "var(--fg2)",
                            border: "none",
                            cursor: active ? "default" : "pointer",
                            fontSize: md ? 13 : 12,
                            fontWeight: active ? 600 : 400,
                            fontFamily: "var(--fontFamily)",
                            boxShadow: active
                                ? "inset 0 0 0 1px var(--primary-border)"
                                : "none",
                        }}
                    >
                        {o.label}
                    </button>
                );
            })}
        </Stack>
    );
}

// ============================================================
// Screen 1 — Conversations list
// ============================================================

// ChartSwitch picks which metric the single chart slot shows. It
// doubles as the chart's title: the active segment names the data.
function ChartSwitch({ value, onChange }) {
    const options = [
        { value: "tokens", label: "Tokens" },
        { value: "activity", label: "Sessions" },
    ];
    return <PillToggle options={options} value={value} onChange={onChange} />;
}

// ChartXLabels renders at most ~5 evenly-spaced bucket labels so the
// axis stays readable instead of becoming a wall of timestamps. Empty
// slots keep the flex columns aligned with the bars above them.
function ChartXLabels({ data }) {
    const step = Math.max(1, Math.ceil(data.length / 5));
    return (
        <div
            style={{
                display: "flex",
                marginLeft: 44,
                marginTop: 6,
                fontSize: 10,
                color: "var(--fg3)",
                fontFamily: "var(--fontFamilyMonospace)",
            }}
        >
            {data.map((d, i) => {
                const last = i === data.length - 1;
                const show = i % step === 0 || last;
                return (
                    <span
                        key={i}
                        style={{
                            flex: 1,
                            textAlign: last ? "right" : "left",
                            overflow: "hidden",
                            whiteSpace: "nowrap",
                        }}
                    >
                        {show ? d.t : ""}
                    </span>
                );
            })}
        </div>
    );
}

// ChartYAxis renders the three right-aligned scale labels (max, mid, 0)
// in the 44px gutter to the left of the plot. The plot is 130px tall, so
// the labels pin to the top, middle (65px), and baseline (130px).
function ChartYAxis({ top, mid }) {
    const label = {
        position: "absolute",
        left: 0,
        width: 34,
        textAlign: "right",
        transform: "translateY(-50%)",
        fontSize: 10,
        lineHeight: "10px",
        color: "var(--fg3)",
        fontFamily: "var(--fontFamilyMonospace)",
        pointerEvents: "none",
    };
    return (
        <React.Fragment>
            <div style={{ ...label, top: 0 }}>{top}</div>
            <div style={{ ...label, top: 65 }}>{mid}</div>
            <div style={{ ...label, top: 130 }}>0</div>
        </React.Fragment>
    );
}

function ActivityChart({
    data,
    bucketLabel,
    switcher,
    selection,
    onBucketClick,
    accent = "var(--brand-orange)",
}) {
    const W = 100,
        H = 32;
    const max = Math.max(1, ...data.map((d) => d.c));
    const barW = (W / Math.max(1, data.length)) * 0.7;
    const gap = (W / Math.max(1, data.length)) * 0.3;
    const [hover, setHover] = useState(null);

    return (
        <SurfaceCard
            style={{
                position: "relative",
                padding: "16px 20px 12px",
                marginBottom: 0,
            }}
        >
            <div
                style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    marginBottom: 10,
                }}
            >
                {switcher}
                <div
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 12,
                        fontSize: 11,
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                    }}
                >
                    <span
                        style={{
                            display: "inline-flex",
                            alignItems: "center",
                            gap: 6,
                        }}
                    >
                        <span
                            style={{
                                width: 10,
                                height: 10,
                                background: accent,
                                borderRadius: 1,
                            }}
                        />{" "}
                        count
                    </span>
                    <span>{bucketLabel}</span>
                </div>
            </div>
            <div style={{ position: "relative" }}>
                <ChartYAxis
                    top={String(max)}
                    mid={String(Math.round(max / 2))}
                />
                <div
                    style={{
                        marginLeft: 44,
                        position: "relative",
                        borderBottom: "1px solid var(--border-medium)",
                    }}
                >
                    <svg
                        viewBox={`0 0 ${W} ${H}`}
                        preserveAspectRatio="none"
                        style={{ width: "100%", height: 130, display: "block" }}
                    >
                        {[0, 0.5].map((g) => (
                            <line
                                key={g}
                                x1={0}
                                x2={W}
                                y1={H * g}
                                y2={H * g}
                                stroke="rgba(204,204,220,0.06)"
                                strokeWidth="0.2"
                            />
                        ))}
                        {data.map((d, i) => {
                            const h = (d.c / max) * H;
                            const x = i * (W / data.length) + gap / 2;
                            const y = H - h;
                            const isHover = hover === i;
                            // Midpoint containment, not overlap: the window shifts a
                            // little every render (now moves), so an overlap test can
                            // light up two adjacent bars.
                            const isSel =
                                selection &&
                                (d.start + d.end) / 2 >= selection.start &&
                                (d.start + d.end) / 2 < selection.end;
                            const dim = selection && !isSel;
                            return (
                                <g
                                    key={i}
                                    onMouseEnter={() => setHover(i)}
                                    onMouseLeave={() => setHover(null)}
                                    onClick={
                                        onBucketClick
                                            ? () => onBucketClick(d)
                                            : undefined
                                    }
                                    style={{
                                        cursor: onBucketClick
                                            ? "pointer"
                                            : "default",
                                    }}
                                >
                                    <rect
                                        x={x - 0.4}
                                        y={0}
                                        width={barW + 0.8}
                                        height={H}
                                        fill="transparent"
                                    />
                                    <rect
                                        x={x}
                                        y={y}
                                        width={barW}
                                        height={Math.max(h, 0.4)}
                                        fill={
                                            isHover
                                                ? "var(--brand-orange-text)"
                                                : accent
                                        }
                                        opacity={
                                            isHover || isSel
                                                ? 1
                                                : dim
                                                  ? 0.3
                                                  : 0.85
                                        }
                                    />
                                </g>
                            );
                        })}
                    </svg>
                    {hover !== null && (
                        <div
                            style={{
                                position: "absolute",
                                left: chartTooltipLeft(hover, data.length),
                                transform: "translate(-50%, -100%)",
                                top: -4,
                                background: "var(--bg-secondary)",
                                border: "1px solid var(--border-medium)",
                                borderRadius: 2,
                                padding: "4px 8px",
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 11,
                                color: "var(--fg1)",
                                whiteSpace: "nowrap",
                                pointerEvents: "none",
                                boxShadow: "var(--shadow-z2)",
                            }}
                        >
                            <span style={{ color: "var(--fg3)" }}>
                                {data[hover].t}
                            </span>{" "}
                            · {data[hover].c}{" "}
                            {data[hover].c === 1 ? "session" : "sessions"}
                        </div>
                    )}
                </div>
                <ChartXLabels data={data} />
            </div>
        </SurfaceCard>
    );
}

// Stacked token-usage-over-time chart. Mirrors ActivityChart's frame
// but stacks the five disjoint token series per bucket, with a
// per-model filter and a click-to-toggle legend. data comes from
// bucketTokenUsage.
function TokenChart({
    data,
    bucketLabel,
    grandTotal,
    models,
    model,
    onModelChange,
    hidden,
    onToggleSeries,
    switcher,
    selection,
    onBucketClick,
}) {
    const W = 100,
        H = 32;
    const barW = (W / Math.max(1, data.length)) * 0.7;
    const gap = (W / Math.max(1, data.length)) * 0.3;
    const [hover, setHover] = useState(null);
    // Only show legend entries for series that actually appear, so a
    // pure-Anthropic store doesn't carry an always-zero "Reasoning"
    // swatch. Fall back to the full set when there's no data at all.
    const present = TOKEN_SERIES.filter((s) => data.some((d) => d[s.key] > 0));
    const legend = present.length ? present : TOKEN_SERIES;
    // Hidden series drop out of the bars, the tooltip, and the y scale,
    // so toggling a dominant series (usually cache reads) rescales the
    // chart to show what's left.
    const visible = TOKEN_SERIES.filter((s) => !hidden.has(s.key));
    const visibleTotal = (d) =>
        visible.reduce((acc, s) => acc + (d[s.key] || 0), 0);
    const max = Math.max(1, ...data.map(visibleTotal));
    const empty = grandTotal === 0;

    return (
        <SurfaceCard
            style={{
                position: "relative",
                padding: "16px 20px 12px",
                marginBottom: 0,
            }}
        >
            <div
                style={{
                    display: "flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    marginBottom: 10,
                    gap: 12,
                    flexWrap: "wrap",
                }}
            >
                {switcher}
                <div
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 12,
                        fontSize: 11,
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                        flexWrap: "wrap",
                    }}
                >
                    {legend.map((s) => {
                        const off = hidden.has(s.key);
                        return (
                            <button
                                key={s.key}
                                onClick={() => onToggleSeries(s.key)}
                                title={
                                    off ? `Show ${s.label}` : `Hide ${s.label}`
                                }
                                style={{
                                    display: "inline-flex",
                                    alignItems: "center",
                                    gap: 6,
                                    background: "transparent",
                                    border: "none",
                                    padding: 0,
                                    cursor: "pointer",
                                    font: "inherit",
                                    color: off ? "var(--fg3)" : "inherit",
                                    opacity: off ? 0.6 : 1,
                                    textDecoration: off
                                        ? "line-through"
                                        : "none",
                                }}
                            >
                                <span
                                    style={{
                                        width: 10,
                                        height: 10,
                                        boxSizing: "border-box",
                                        background: off
                                            ? "transparent"
                                            : s.color,
                                        border: `1px solid ${off ? "var(--border-medium)" : s.color}`,
                                        borderRadius: 1,
                                    }}
                                />{" "}
                                {s.label}
                            </button>
                        );
                    })}
                    {models.length > 0 && (
                        <Select
                            value={model}
                            onChange={onModelChange}
                            title="Filter by model"
                            options={[
                                { value: "all", label: "All models" },
                                ...models.map((m) => ({ value: m, label: m })),
                            ]}
                            trigger={{
                                height: 24,
                                minWidth: 108,
                                padding: "0 6px",
                                borderRadius: 2,
                                background: "var(--bg-primary)",
                                fontSize: 11,
                                fontFamily: "var(--fontFamilyMonospace)",
                            }}
                            menu={{ fontFamily: "var(--fontFamilyMonospace)" }}
                        />
                    )}
                    <span>{bucketLabel}</span>
                </div>
            </div>
            <div style={{ position: "relative" }}>
                {!empty && visible.length > 0 && (
                    <ChartYAxis
                        top={formatTokens(max)}
                        mid={formatTokens(Math.round(max / 2))}
                    />
                )}
                <div
                    style={{
                        marginLeft: 44,
                        position: "relative",
                        borderBottom: "1px solid var(--border-medium)",
                    }}
                >
                    <svg
                        viewBox={`0 0 ${W} ${H}`}
                        preserveAspectRatio="none"
                        style={{ width: "100%", height: 130, display: "block" }}
                    >
                        {[0, 0.5].map((g) => (
                            <line
                                key={g}
                                x1={0}
                                x2={W}
                                y1={H * g}
                                y2={H * g}
                                stroke="rgba(204,204,220,0.06)"
                                strokeWidth="0.2"
                            />
                        ))}
                        {data.map((d, i) => {
                            const x = i * (W / data.length) + gap / 2;
                            const isHover = hover === i;
                            // Midpoint containment, not overlap — see ActivityChart.
                            const isSel =
                                selection &&
                                (d.start + d.end) / 2 >= selection.start &&
                                (d.start + d.end) / 2 < selection.end;
                            const dim = selection && !isSel;
                            const barOpacity =
                                isHover || isSel ? 1 : dim ? 0.3 : 0.85;
                            let yTop = H;
                            const segs = [];
                            for (const s of visible) {
                                const v = d[s.key] || 0;
                                if (v <= 0) continue;
                                const h = (v / max) * H;
                                yTop -= h;
                                segs.push(
                                    <rect
                                        key={s.key}
                                        x={x}
                                        y={yTop}
                                        width={barW}
                                        height={Math.max(h, 0.2)}
                                        fill={s.color}
                                        opacity={barOpacity}
                                    />,
                                );
                            }
                            return (
                                <g
                                    key={i}
                                    onMouseEnter={() => setHover(i)}
                                    onMouseLeave={() => setHover(null)}
                                    onClick={
                                        onBucketClick
                                            ? () => onBucketClick(d)
                                            : undefined
                                    }
                                    style={{
                                        cursor: onBucketClick
                                            ? "pointer"
                                            : "default",
                                    }}
                                >
                                    <rect
                                        x={x - 0.4}
                                        y={0}
                                        width={barW + 0.8}
                                        height={H}
                                        fill="transparent"
                                    />
                                    {segs}
                                </g>
                            );
                        })}
                    </svg>
                    {empty && (
                        <div
                            style={{
                                position: "absolute",
                                top: 0,
                                left: 0,
                                right: 0,
                                height: 130,
                                display: "flex",
                                alignItems: "center",
                                justifyContent: "center",
                                fontSize: 11,
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                                pointerEvents: "none",
                            }}
                        >
                            No token usage{" "}
                            {model !== "all" ? `for ${model} ` : ""}in this
                            range
                        </div>
                    )}
                    {hover !== null && visibleTotal(data[hover]) > 0 && (
                        <div
                            style={{
                                position: "absolute",
                                left: chartTooltipLeft(hover, data.length),
                                transform: "translate(-50%, -100%)",
                                top: -4,
                                background: "var(--bg-secondary)",
                                border: "1px solid var(--border-medium)",
                                borderRadius: 2,
                                padding: "6px 8px",
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 11,
                                color: "var(--fg1)",
                                whiteSpace: "nowrap",
                                pointerEvents: "none",
                                boxShadow: "var(--shadow-z2)",
                                zIndex: 1,
                            }}
                        >
                            <div
                                style={{ color: "var(--fg3)", marginBottom: 4 }}
                            >
                                {data[hover].t} ·{" "}
                                {formatTokens(visibleTotal(data[hover]))} tok
                            </div>
                            {visible
                                .filter((s) => data[hover][s.key] > 0)
                                .map((s) => (
                                    <div
                                        key={s.key}
                                        style={{
                                            display: "flex",
                                            alignItems: "center",
                                            gap: 8,
                                        }}
                                    >
                                        <span
                                            style={{
                                                width: 8,
                                                height: 8,
                                                background: s.color,
                                                borderRadius: 1,
                                            }}
                                        />
                                        <span style={{ color: "var(--fg2)" }}>
                                            {s.label}
                                        </span>
                                        <span
                                            style={{
                                                marginLeft: "auto",
                                                color: "var(--fg1)",
                                            }}
                                        >
                                            {formatTokens(data[hover][s.key])}
                                        </span>
                                    </div>
                                ))}
                        </div>
                    )}
                </div>
                <ChartXLabels data={data} />
            </div>
        </SurfaceCard>
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
function Select({
    value,
    options,
    onChange,
    title,
    trigger,
    menu,
    id,
    disabled,
    icon,
    prefix,
}) {
    const [open, setOpen] = useState(false);
    const [cursor, setCursor] = useState(0);
    const rootRef = useRef(null);
    const buttonRef = useRef(null);
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
    const close = (refocus) => {
        setOpen(false);
        if (refocus && buttonRef.current) buttonRef.current.focus();
    };
    const pick = (option) => {
        onChange(option.value);
        close(true);
    };

    const onKeyDown = (e) => {
        if (!open) {
            if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
                e.preventDefault();
                openMenu();
            }
            return;
        }
        if (e.key === "Escape") {
            e.preventDefault();
            close(true);
            return;
        }
        if (e.key === "Enter" || e.key === " ") {
            e.preventDefault();
            if (options[cursor]) pick(options[cursor]);
            return;
        }
        if (e.key === "ArrowDown") {
            e.preventDefault();
            setCursor((c) => Math.min(options.length - 1, c + 1));
            return;
        }
        if (e.key === "ArrowUp") {
            e.preventDefault();
            setCursor((c) => Math.max(0, c - 1));
            return;
        }
        if (e.key === "Home") {
            e.preventDefault();
            setCursor(0);
            return;
        }
        if (e.key === "End") {
            e.preventDefault();
            setCursor(options.length - 1);
        }
    };

    return (
        <div
            ref={rootRef}
            style={{ position: "relative", flex: "0 0 auto" }}
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
                    padding: "0 10px",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    background: "rgba(24,27,31,0.78)",
                    color: disabled ? "var(--fg3)" : "var(--fg1)",
                    fontSize: 13,
                    fontFamily: "var(--fontFamily)",
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 8,
                    cursor: disabled ? "not-allowed" : "pointer",
                    textAlign: "left",
                    opacity: disabled ? 0.6 : 1,
                    ...trigger,
                }}
            >
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 8,
                        minWidth: 0,
                        overflow: "hidden",
                    }}
                >
                    {icon && (
                        <Icon
                            name={icon}
                            size={14}
                            style={{ color: "var(--fg3)" }}
                        />
                    )}
                    {prefix && (
                        <span style={{ color: "var(--fg3)" }}>{prefix}</span>
                    )}
                    <span
                        style={{
                            minWidth: 0,
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                        }}
                    >
                        {selected ? selected.label : ""}
                    </span>
                </span>
                <Icon
                    name="chevron"
                    size={13}
                    style={{ color: "var(--fg3)", flex: "none" }}
                />
            </button>
            {open && !disabled && (
                <div
                    role="listbox"
                    tabIndex={-1}
                    style={{
                        position: "absolute",
                        top: "calc(100% + 5px)",
                        left: 0,
                        zIndex: 30,
                        minWidth: "100%",
                        maxHeight: 280,
                        overflowY: "auto",
                        padding: 4,
                        border: "1px solid var(--border-strong)",
                        borderRadius: 2,
                        background: "var(--bg-secondary)",
                        boxShadow: "0 12px 34px rgba(0,0,0,0.48)",
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
                                    width: "100%",
                                    minHeight: 30,
                                    display: "flex",
                                    alignItems: "center",
                                    justifyContent: "space-between",
                                    gap: 10,
                                    padding: "0 9px",
                                    border: "none",
                                    borderRadius: 5,
                                    background:
                                        i === cursor
                                            ? ACTIVE_PILL_BG
                                            : "transparent",
                                    color: isSelected
                                        ? "var(--primary-text)"
                                        : "var(--fg1)",
                                    fontSize: 12,
                                    fontFamily: "var(--fontFamily)",
                                    cursor: "pointer",
                                    textAlign: "left",
                                    whiteSpace: "nowrap",
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

function TimeRangePicker({ value, onChange, ranges = TIME_RANGES }) {
    const [open, setOpen] = useState(false);
    const selected =
        ranges.find((r) => r.value === value) ||
        ranges[ranges.length - 1] ||
        TIME_RANGES[0];
    return (
        <div style={{ position: "relative", flex: "0 0 auto" }}>
            <button
                type="button"
                onClick={() => setOpen((o) => !o)}
                onBlur={(e) => {
                    if (
                        !e.currentTarget.parentElement?.contains(
                            e.relatedTarget,
                        )
                    )
                        setOpen(false);
                }}
                title="Time range"
                style={{
                    height: 34,
                    minWidth: 166,
                    padding: "0 10px",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    background: "rgba(24,27,31,0.78)",
                    color: "var(--fg1)",
                    fontSize: 13,
                    fontFamily: "var(--fontFamily)",
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 10,
                    cursor: "pointer",
                }}
            >
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 8,
                    }}
                >
                    <Icon
                        name="clock"
                        size={14}
                        style={{ color: "var(--fg3)" }}
                    />
                    {selected.label}
                </span>
                <Icon
                    name="chevron"
                    size={14}
                    style={{ color: "var(--fg3)" }}
                />
            </button>
            {open && (
                <div
                    style={{
                        position: "absolute",
                        top: 39,
                        right: 0,
                        zIndex: 30,
                        minWidth: 190,
                        padding: 4,
                        border: "1px solid var(--border-strong)",
                        borderRadius: 2,
                        background: "var(--bg-secondary)",
                        boxShadow: "0 12px 34px rgba(0,0,0,0.48)",
                    }}
                >
                    {ranges.map((r) => {
                        const active = r.value === selected.value;
                        return (
                            <button
                                key={r.value}
                                type="button"
                                onMouseDown={(e) => e.preventDefault()}
                                onClick={() => {
                                    onChange(r.value);
                                    setOpen(false);
                                }}
                                style={{
                                    width: "100%",
                                    height: 30,
                                    display: "flex",
                                    alignItems: "center",
                                    justifyContent: "space-between",
                                    gap: 10,
                                    padding: "0 9px",
                                    border: "none",
                                    borderRadius: 5,
                                    background: active
                                        ? ACTIVE_PILL_BG
                                        : "transparent",
                                    color: active
                                        ? "var(--primary-text)"
                                        : "var(--fg1)",
                                    fontSize: 12,
                                    fontFamily: "var(--fontFamily)",
                                    cursor: "pointer",
                                    textAlign: "left",
                                }}
                            >
                                <span>{r.label}</span>
                                {active && <Icon name="check" size={12} />}
                            </button>
                        );
                    })}
                </div>
            )}
        </div>
    );
}

const GROUP_BY_OPTIONS = [
    { value: "workspace", label: "Workspace" },
    { value: "agent", label: "Agent" },
    { value: "model", label: "Model" },
    { value: "day", label: "Day" },
    { value: "none", label: "None" },
];

function WorkspaceFacet({
    workspaces,
    selected,
    onSelect,
    totalCount,
    totalCost,
    now,
    rangeLabel,
}) {
    const [open, setOpen] = useState(false);
    const [filter, setFilter] = useState("");
    const [cursor, setCursor] = useState(0);
    const rootRef = useRef(null);
    const buttonRef = useRef(null);
    const inputRef = useRef(null);
    const listRef = useRef(null);
    const optionRefs = useRef(new Map());
    const selectedPath = selected == null ? null : splitWorkspacePath(selected);
    const selectedParent = selectedPath?.dir
        ? selectedPath.dir === "/"
            ? "/"
            : `${splitWorkspacePath(selectedPath.dir).leaf}/`
        : "";
    const shown = useMemo(() => {
        const q = filter.trim().toLowerCase();
        return q
            ? workspaces.filter((w) =>
                  (w.path || "").toLowerCase().includes(q),
              )
            : workspaces;
    }, [workspaces, filter]);
    const selectedInRange = workspaces.some((w) => w.path === selected);
    const noMatches = filter.trim().length > 0 && shown.length === 0;
    const activeOptionId =
        cursor < 0
            ? undefined
            : cursor === 0
              ? "workspace-facet-option-all"
              : `workspace-facet-option-${cursor - 1}`;

    useEffect(() => {
        setCursor((current) =>
            noMatches ? -1 : Math.max(0, Math.min(current, shown.length)),
        );
    }, [shown.length, noMatches]);

    useEffect(() => {
        if (!open || cursor <= 0) return;
        const list = listRef.current;
        const option = optionRefs.current.get(cursor);
        if (!list || !option) return;
        const listRect = list.getBoundingClientRect();
        const optionRect = option.getBoundingClientRect();
        if (optionRect.top < listRect.top)
            list.scrollTop -= listRect.top - optionRect.top;
        else if (optionRect.bottom > listRect.bottom)
            list.scrollTop += optionRect.bottom - listRect.bottom;
    }, [open, cursor, shown.length]);

    const close = (refocus) => {
        setOpen(false);
        if (refocus && buttonRef.current) buttonRef.current.focus();
    };
    const openMenu = () => {
        const selectedIndex = workspaces.findIndex((w) => w.path === selected);
        setFilter("");
        setCursor(selectedIndex < 0 ? 0 : selectedIndex + 1);
        setOpen(true);
        setTimeout(() => inputRef.current?.focus(), 0);
    };
    const pick = (path) => {
        onSelect(path);
        close(true);
    };
    const onKeyDown = (e) => {
        if (!open) {
            if (e.key === "Enter" || e.key === " " || e.key === "ArrowDown") {
                e.preventDefault();
                openMenu();
            }
            return;
        }
        if (e.key === "Escape") {
            e.preventDefault();
            close(true);
            return;
        }
        if (e.key === "ArrowDown") {
            e.preventDefault();
            if (!noMatches)
                setCursor((current) =>
                    Math.min(shown.length, Math.max(0, current + 1)),
                );
            return;
        }
        if (e.key === "ArrowUp") {
            e.preventDefault();
            if (!noMatches)
                setCursor((current) => Math.max(0, current - 1));
            return;
        }
        if (
            (e.key === "Home" || e.key === "End") &&
            e.target === inputRef.current
        )
            return;
        if (e.key === "Home") {
            e.preventDefault();
            setCursor(noMatches ? -1 : 0);
            return;
        }
        if (e.key === "End") {
            e.preventDefault();
            setCursor(noMatches ? -1 : shown.length);
            return;
        }
        if (e.key === "Enter") {
            e.preventDefault();
            if (cursor < 0) return;
            if (cursor === 0) {
                pick(null);
                return;
            }
            const workspace = shown[cursor - 1];
            if (workspace) pick(workspace.path);
        }
    };

    const triggerCount = selectedPath
        ? `${selectedInRange ? 1 : 0}/${workspaces.length}`
        : String(workspaces.length);

    return (
        <div
            ref={rootRef}
            style={{ position: "relative", flex: "0 0 auto" }}
            onBlur={(e) => {
                if (!rootRef.current?.contains(e.relatedTarget)) setOpen(false);
            }}
            onKeyDown={onKeyDown}
        >
            <button
                ref={buttonRef}
                type="button"
                title="Filter by workspace"
                aria-haspopup="listbox"
                aria-expanded={open}
                onClick={() => (open ? close(false) : openMenu())}
                style={{
                    height: 34,
                    minWidth: 198,
                    maxWidth: 360,
                    padding: "0 10px",
                    border: `1px solid ${selectedPath ? "var(--primary-border)" : "var(--border-medium)"}`,
                    borderRadius: 2,
                    background: "rgba(24,27,31,0.78)",
                    color: "var(--fg1)",
                    fontSize: 13,
                    fontFamily: "var(--fontFamily)",
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "space-between",
                    gap: 10,
                    cursor: "pointer",
                }}
            >
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 8,
                        minWidth: 0,
                    }}
                >
                    <Icon name="box" size={14} style={{ color: "var(--fg3)" }} />
                    {selectedPath ? (
                        <React.Fragment>
                            <span
                                style={{
                                    color: "var(--fg-max)",
                                    whiteSpace: "nowrap",
                                }}
                            >
                                {selectedPath.leaf}
                            </span>
                            {selectedParent && (
                                <span
                                    style={{
                                        minWidth: 0,
                                        overflow: "hidden",
                                        textOverflow: "ellipsis",
                                        whiteSpace: "nowrap",
                                        color: "var(--fg3)",
                                        fontFamily: "var(--fontFamilyMonospace)",
                                        fontSize: 10.5,
                                    }}
                                >
                                    {selectedParent}
                                </span>
                            )}
                        </React.Fragment>
                    ) : (
                        <span style={{ whiteSpace: "nowrap" }}>All workspaces</span>
                    )}
                </span>
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 8,
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        whiteSpace: "nowrap",
                    }}
                >
                    {triggerCount}
                    <Icon name="chevron" size={14} />
                </span>
            </button>
            {open && (
                <div
                    style={{
                        position: "absolute",
                        top: "calc(100% + 5px)",
                        left: 0,
                        zIndex: 30,
                        width: 420,
                        padding: 6,
                        border: "1px solid var(--border-strong)",
                        borderRadius: 2,
                        background: "var(--bg-secondary)",
                        boxShadow: "0 12px 34px rgba(0,0,0,0.48)",
                    }}
                >
                    <div
                        style={{
                            height: 30,
                            display: "flex",
                            alignItems: "center",
                            gap: 7,
                            padding: "0 9px",
                            background: "rgba(17,18,23,0.42)",
                            borderRadius: 2,
                            color: "var(--fg3)",
                        }}
                    >
                        <Icon name="search" size={13} />
                        <input
                            ref={inputRef}
                            value={filter}
                            onChange={(e) => {
                                const next = e.target.value;
                                const q = next.trim().toLowerCase();
                                setFilter(next);
                                setCursor(
                                    q.length === 0
                                        ? 0
                                        : workspaces.some((w) =>
                                                (w.path || "")
                                                    .toLowerCase()
                                                    .includes(q),
                                            )
                                          ? 1
                                          : -1,
                                );
                            }}
                            placeholder="Filter workspaces…"
                            role="combobox"
                            aria-label="Filter workspaces"
                            aria-autocomplete="list"
                            aria-controls="workspace-facet-listbox"
                            aria-expanded={open}
                            aria-activedescendant={activeOptionId}
                            style={{
                                flex: 1,
                                minWidth: 0,
                                border: "none",
                                outline: "none",
                                background: "transparent",
                                color: "var(--fg1)",
                                fontFamily: "var(--fontFamily)",
                                fontSize: 12,
                            }}
                        />
                    </div>
                    <div
                        id="workspace-facet-listbox"
                        role="listbox"
                        aria-label="Workspaces"
                    >
                        <button
                            id="workspace-facet-option-all"
                            type="button"
                            role="option"
                            aria-selected={selected == null}
                            onMouseDown={(e) => e.preventDefault()}
                            onMouseEnter={() => setCursor(0)}
                            onClick={() => pick(null)}
                            style={{
                                width: "100%",
                                minHeight: 34,
                                display: "flex",
                                alignItems: "center",
                                justifyContent: "space-between",
                                gap: 12,
                                padding: "7px 9px",
                                border: "none",
                                borderRadius: 2,
                                background:
                                    cursor === 0 ? ACTIVE_PILL_BG : "transparent",
                                color:
                                    selected == null
                                        ? "var(--primary-text)"
                                        : "var(--fg1)",
                                cursor: "pointer",
                                fontFamily: "var(--fontFamily)",
                                fontSize: 12.5,
                                textAlign: "left",
                            }}
                        >
                            <span>All workspaces</span>
                            <span
                                style={{
                                    color: "var(--fg3)",
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 11,
                                }}
                            >
                                {totalCount} · {formatCost(totalCost)}
                            </span>
                        </button>
                        <div
                            style={{
                                height: 1,
                                margin: "5px 4px",
                                background: "var(--border-weak)",
                            }}
                        />
                        <div
                            ref={listRef}
                            style={{ maxHeight: 296, overflowY: "auto" }}
                        >
                            {shown.map((w, i) => {
                                const path = splitWorkspacePath(w.path);
                                const active = selected === w.path;
                                const shareAvailable =
                                    totalCost != null &&
                                    totalCost > 0 &&
                                    w.cost != null;
                                const share = shareAvailable
                                    ? w.cost / totalCost
                                    : null;
                                const pct =
                                    share == null ? null : Math.round(share * 100);
                                return (
                                    <button
                                        key={w.path || "(unknown)"}
                                        id={`workspace-facet-option-${i}`}
                                        ref={(node) => {
                                            if (node)
                                                optionRefs.current.set(i + 1, node);
                                            else optionRefs.current.delete(i + 1);
                                        }}
                                        type="button"
                                        role="option"
                                        aria-selected={active}
                                        title={w.path || "(unknown)"}
                                        onMouseDown={(e) => e.preventDefault()}
                                        onMouseEnter={() => setCursor(i + 1)}
                                        onClick={() => pick(w.path)}
                                        style={{
                                            width: "100%",
                                            display: "grid",
                                            gridTemplateColumns: "1fr 54px 62px 56px",
                                            alignItems: "center",
                                            gap: 10,
                                            padding: "7px 9px",
                                            border: "none",
                                            borderRadius: 2,
                                            background:
                                                cursor === i + 1
                                                    ? ACTIVE_PILL_BG
                                                    : "transparent",
                                            color: active
                                                ? "var(--primary-text)"
                                                : "var(--fg1)",
                                            cursor: "pointer",
                                            textAlign: "left",
                                        }}
                                    >
                                        <span
                                            style={{
                                                minWidth: 0,
                                                display: "flex",
                                                flexDirection: "column",
                                                gap: 2,
                                            }}
                                        >
                                            <span
                                                style={{
                                                    overflow: "hidden",
                                                    textOverflow: "ellipsis",
                                                    whiteSpace: "nowrap",
                                                    color: active
                                                        ? "var(--primary-text)"
                                                        : "var(--fg-max)",
                                                    fontFamily: "var(--fontFamily)",
                                                    fontSize: 12.5,
                                                }}
                                            >
                                                {path.leaf}
                                            </span>
                                            <span
                                                style={{
                                                    overflow: "hidden",
                                                    textOverflow: "ellipsis",
                                                    whiteSpace: "nowrap",
                                                    direction: "rtl",
                                                    textAlign: "left",
                                                    color: "var(--fg3)",
                                                    fontFamily:
                                                        "var(--fontFamilyMonospace)",
                                                    fontSize: 10,
                                                }}
                                            >
                                                {path.dir || NO_VALUE}
                                            </span>
                                        </span>
                                        <span
                                            style={{
                                                color: "var(--fg2)",
                                                fontFamily:
                                                    "var(--fontFamilyMonospace)",
                                                fontSize: 11,
                                                textAlign: "right",
                                            }}
                                        >
                                            {w.count}
                                        </span>
                                        <span
                                            style={{
                                                color: "var(--fg1)",
                                                fontFamily:
                                                    "var(--fontFamilyMonospace)",
                                                fontSize: 11,
                                                textAlign: "right",
                                            }}
                                        >
                                            {formatCost(w.cost)}
                                        </span>
                                        <span
                                            style={{
                                                minWidth: 0,
                                                display: "flex",
                                                flexDirection: "column",
                                                gap: 4,
                                                color: "var(--fg3)",
                                                fontFamily:
                                                    "var(--fontFamilyMonospace)",
                                                fontSize: 10,
                                                textAlign: "right",
                                            }}
                                        >
                                            <span>
                                                {formatAgo(
                                                    w.last
                                                        ? new Date(w.last).toISOString()
                                                        : null,
                                                    now,
                                                )}
                                            </span>
                                            <span
                                                title={
                                                    pct == null
                                                        ? "Spend share unavailable"
                                                        : `${pct}% of range spend`
                                                }
                                                aria-label={
                                                    pct == null
                                                        ? "Spend share unavailable"
                                                        : `${pct}% of range spend`
                                                }
                                                style={{
                                                    display: "block",
                                                    height: 2,
                                                    borderRadius: 2,
                                                    background:
                                                        "rgba(204,204,220,0.08)",
                                                    overflow: "hidden",
                                                }}
                                            >
                                                <span
                                                    style={{
                                                        display: "block",
                                                        width: `${Math.max(0, Math.min(100, pct || 0))}%`,
                                                        height: "100%",
                                                        background:
                                                            "var(--brand-orange)",
                                                    }}
                                                />
                                            </span>
                                        </span>
                                    </button>
                                );
                            })}
                            {shown.length === 0 && (
                                <div
                                    style={{
                                        padding: "14px 9px",
                                        color: "var(--fg3)",
                                        fontFamily: "var(--fontFamilyMonospace)",
                                        fontSize: 11,
                                    }}
                                >
                                    No matching workspaces.
                                </div>
                            )}
                        </div>
                    </div>
                    <div
                        style={{
                            display: "flex",
                            justifyContent: "space-between",
                            gap: 12,
                            padding: "7px 9px 2px",
                            color: "var(--fg3)",
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 10.5,
                        }}
                    >
                        <span>
                            {workspaces.length} workspaces · {rangeLabel}
                        </span>
                        <span>sessions · cost · share</span>
                    </div>
                </div>
            )}
        </div>
    );
}

function FilterBar({
    query,
    onQueryChange,
    inputRef,
    timeRange,
    onTimeRangeChange,
    workspaces = [],
    workspace,
    onWorkspaceChange,
    workspaceSessionCount = 0,
    workspaceTotalCost = 0,
    now,
    rangeLabel,
    groupBy,
    onGroupByChange,
    agentFilter = "all",
    onAgentFilterChange,
    agentOptions = [],
    modelFilter = "all",
    onModelFilterChange,
    modelOptions = [],
    statusFilter = "all",
    onStatusFilterChange,
    activeFilterCount = 0,
    onClearFilters,
    onRefresh,
    refreshing,
    placeholder = "Filter by title, id, workspace, agent, model…",
    onInputKeyDown,
    rightAdornment,
}) {
    const showTimeRange = !!timeRange && !!onTimeRangeChange;
    const showWorkspaceFacet = !!onWorkspaceChange;
    const showGroupBy = !!onGroupByChange;
    const showAgentFilter = !!onAgentFilterChange;
    const showModelFilter = !!onModelFilterChange;
    const showStatusFilter = !!onStatusFilterChange;
    const selectStyle = {
        height: 34,
        minWidth: 132,
        padding: "0 30px 0 11px",
        border: "1px solid var(--border-medium)",
        borderRadius: 2,
        background: "rgba(24,27,31,0.78)",
        color: "var(--fg1)",
        fontSize: 13,
        fontFamily: "var(--fontFamily)",
    };
    return (
        <Stack
            direction="row"
            align="stretch"
            gap={8}
            style={{ marginBottom: 16, fontSize: 13, flexWrap: "wrap" }}
        >
            <Stack
                direction="row"
                align="center"
                gap={8}
                style={{
                    flex: "1 1 260px",
                    padding: "0 11px",
                    height: 34,
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    background: "rgba(24,27,31,0.78)",
                    color: "var(--fg3)",
                    boxShadow: "inset 0 0 0 1px rgba(0,0,0,0.12)",
                }}
            >
                <Icon name="search" size={14} />
                <input
                    ref={inputRef}
                    value={query}
                    onChange={(e) => onQueryChange(e.target.value)}
                    onKeyDown={onInputKeyDown}
                    placeholder={placeholder}
                    style={{
                        flex: 1,
                        background: "transparent",
                        border: "none",
                        outline: "none",
                        color: "var(--fg1)",
                        fontSize: 13,
                        fontFamily: "var(--fontFamily)",
                    }}
                />
                {rightAdornment !== undefined ? (
                    rightAdornment
                ) : (
                    <span
                        title="Press Command-K or Control-K to focus search"
                        style={{
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 11,
                            color: "var(--fg3)",
                            padding: "1px 6px",
                            border: "1px solid var(--border-weak)",
                            borderRadius: 2,
                        }}
                    >
                        ⌘K
                    </span>
                )}
            </Stack>
            {showWorkspaceFacet && (
                <WorkspaceFacet
                    workspaces={workspaces}
                    selected={workspace}
                    onSelect={onWorkspaceChange}
                    totalCount={workspaceSessionCount}
                    totalCost={workspaceTotalCost}
                    now={now}
                    rangeLabel={rangeLabel}
                />
            )}
            {showTimeRange && (
                <TimeRangePicker
                    value={timeRange}
                    onChange={onTimeRangeChange}
                />
            )}
            {showGroupBy && (
                <Select
                    value={groupBy}
                    onChange={onGroupByChange}
                    title="Group sessions"
                    icon="sortlines"
                    prefix="Group by"
                    trigger={{ ...selectStyle, minWidth: 196, padding: "0 10px" }}
                    options={GROUP_BY_OPTIONS}
                />
            )}
            {showAgentFilter && (
                <Select
                    value={agentFilter}
                    onChange={onAgentFilterChange}
                    title="Filter by agent"
                    trigger={selectStyle}
                    options={[
                        { value: "all", label: "All agents" },
                        ...agentOptions.map((a) => ({ value: a, label: a })),
                    ]}
                />
            )}
            {showModelFilter && (
                <Select
                    value={modelFilter}
                    onChange={onModelFilterChange}
                    title="Filter by model"
                    trigger={{ ...selectStyle, minWidth: 150 }}
                    options={[
                        { value: "all", label: "All models" },
                        ...modelOptions.map((m) => ({ value: m, label: m })),
                    ]}
                />
            )}
            {showStatusFilter && (
                <Select
                    value={statusFilter}
                    onChange={onStatusFilterChange}
                    title="Filter by status"
                    trigger={selectStyle}
                    options={[
                        { value: "all", label: "All status" },
                        { value: "errors", label: "Errors" },
                        { value: "subagents", label: "Has subagents" },
                    ]}
                />
            )}
            {activeFilterCount > 0 && onClearFilters && (
                <button
                    onClick={onClearFilters}
                    style={{
                        ...iconBtn,
                        width: "auto",
                        height: 34,
                        padding: "0 11px",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        color: "var(--fg2)",
                        gap: 6,
                        flex: "0 0 auto",
                        whiteSpace: "nowrap",
                    }}
                    title="Clear session filters"
                    onMouseEnter={(e) => {
                        e.currentTarget.style.background =
                            "var(--action-hover)";
                        e.currentTarget.style.color = "var(--fg1)";
                    }}
                    onMouseLeave={(e) => {
                        e.currentTarget.style.background = "transparent";
                        e.currentTarget.style.color = "var(--fg2)";
                    }}
                >
                    <Icon name="close" size={13} />
                    Clear
                </button>
            )}
            <button
                onClick={onRefresh}
                disabled={refreshing}
                style={{
                    ...iconBtn,
                    height: 34,
                    width: 34,
                    flex: "0 0 34px",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    opacity: refreshing ? 0.5 : 1,
                    cursor: refreshing ? "wait" : "pointer",
                }}
                title="Refresh"
                onMouseEnter={(e) => {
                    if (!refreshing) {
                        e.currentTarget.style.background =
                            "var(--action-hover)";
                        e.currentTarget.style.color = "var(--fg1)";
                    }
                }}
                onMouseLeave={(e) => {
                    e.currentTarget.style.background = "transparent";
                    e.currentTarget.style.color = "var(--fg2)";
                }}
            >
                <Icon name="refresh" size={14} />
            </button>
        </Stack>
    );
}

function ConvRow({
    c,
    now,
    onOpen,
    prices,
    grouped = false,
    hideWorkspace = false,
}) {
    const wallSec = durationBetweenSeconds(c.started_at, c.last_activity);
    return (
        <a
            href={conversationPath(c.id)}
            onClick={(e) => {
                if (!isPlainLeftClick(e)) return;
                e.preventDefault();
                onOpen(c);
            }}
            style={{
                display: "grid",
                gridTemplateColumns: CONV_GRID,
                alignItems: "center",
                gap: 16,
                padding: grouped ? "12px 16px 12px 40px" : "12px 16px",
                borderBottom: "1px solid var(--border-weak)",
                background: "transparent",
                cursor: "pointer",
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 12,
                transition: "background 80ms ease",
                textDecoration: "none",
                color: "inherit",
            }}
            onMouseEnter={(e) =>
                (e.currentTarget.style.background = "rgba(204,204,220,0.03)")
            }
            onMouseLeave={(e) =>
                (e.currentTarget.style.background = "transparent")
            }
        >
            <span style={{ color: "var(--fg2)" }}>
                {formatAgo(c.last_activity, now)}
            </span>
            <div
                style={{
                    display: "flex",
                    flexDirection: "column",
                    gap: 2,
                    minWidth: 0,
                }}
            >
                <span
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 7,
                        minWidth: 0,
                    }}
                >
                    <span
                        style={{
                            fontFamily: "var(--fontFamily)",
                            color: "var(--fg1)",
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                        }}
                    >
                        {c.title || c.id}
                    </span>
                    {c.subagents > 0 && (
                        <span
                            title={`${c.subagents} subagent ${c.subagents === 1 ? "step" : "steps"}`}
                            style={{
                                flexShrink: 0,
                                display: "inline-flex",
                                alignItems: "center",
                                gap: 3,
                                padding: "0 6px",
                                height: 16,
                                borderRadius: 2,
                                background: "rgba(204,204,220,0.06)",
                                color: "var(--fg2)",
                                fontSize: 10,
                                fontFamily: "var(--fontFamilyMonospace)",
                            }}
                        >
                            ⊂ {c.subagents}
                        </span>
                    )}
                </span>
                {!hideWorkspace && (
                    <span
                        style={{
                            color: "var(--fg3)",
                            fontSize: 11,
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                        }}
                    >
                        {c.workspace ? workspaceLabel(c.workspace) : c.id}
                    </span>
                )}
            </div>
            <AgentCell agents={c.agents} />
            <span style={{ color: "var(--fg1)" }} title={ESTIMATED_COST_TOOLTIP}>
                {formatCost(conversationCost(c, prices))}
            </span>
            <span
                style={{ display: "inline-flex", alignItems: "center", gap: 7 }}
                title={tokenBreakdownTitle(c.token_buckets)}
            >
                <span style={{ color: "var(--fg1)" }}>
                    {formatTokens(c.total_tokens)}
                </span>
                {c.status === "err" && (
                    <span
                        style={{
                            display: "inline-flex",
                            alignItems: "center",
                            padding: "0 6px",
                            height: 16,
                            borderRadius: 2,
                            background: "var(--error-transparent)",
                            color: "var(--error-text)",
                            fontSize: 10,
                            letterSpacing: "0.04em",
                        }}
                    >
                        ERR
                    </span>
                )}
            </span>
            <span style={{ color: "var(--fg2)" }}>
                <span style={{ color: "var(--fg1)" }}>
                    {formatDuration(wallSec)}
                </span>
                <span style={{ color: "var(--fg3)", padding: "0 6px" }}>·</span>
                <span style={{ color: "var(--fg1)" }}>
                    {c.calls} {c.calls === 1 ? "call" : "calls"}
                </span>
            </span>
            <ModelCell models={c.models} />
        </a>
    );
}

// Shared by ConvRow and its header so the columns stay aligned:
// Last activity · Conversation · Agent · Estimated cost · Tokens · Duration · Models.
// Agent shows the host launcher only (claude-code, …) — not the per-
// subagent rows, which were the noise; subagent presence is the ⊂N badge.
const CONV_GRID =
    "84px minmax(260px, 1.4fr) 132px 118px 96px 136px minmax(220px, 1.2fr)";
const OPEN_GROUPS = 3;

// Use the full sorted agent or model set as one key so each session appears once.
function groupKeyFor(c, groupBy) {
    if (groupBy === "workspace") return c.workspace || "";
    if (groupBy === "agent")
        return agentHosts(c.agents).sort().join(" + ") || "(unknown)";
    if (groupBy === "model")
        return [...new Set(c.models || [])]
            .filter(Boolean)
            .sort()
            .join(" + ") || "(unknown)";
    if (groupBy === "day") {
        const t = conversationTime(c);
        if (t == null) return "(unknown)";
        const d = new Date(t);
        return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, "0")}-${String(d.getDate()).padStart(2, "0")}`;
    }
    return "";
}

function SessionGroupHeader({
    groupBy,
    label,
    open,
    onToggle,
    count,
    cost,
    tokens,
    last,
    share,
    now,
}) {
    const path = groupBy === "workspace" ? splitWorkspacePath(label) : null;
    const pct = share == null ? null : Math.round(share * 100);
    return (
        <button
            type="button"
            aria-expanded={open}
            onClick={onToggle}
            style={{
                width: "100%",
                display: "grid",
                gridTemplateColumns: "minmax(0, 1fr) auto",
                alignItems: "center",
                gap: 16,
                padding: "10px 16px",
                border: "none",
                borderBottom: "1px solid var(--border-weak)",
                background: "rgba(34,37,43,0.55)",
                color: "inherit",
                cursor: "pointer",
                textAlign: "left",
                font: "inherit",
            }}
            onMouseEnter={(e) => {
                e.currentTarget.style.background = "rgba(34,37,43,0.8)";
            }}
            onMouseLeave={(e) => {
                e.currentTarget.style.background = "rgba(34,37,43,0.55)";
            }}
        >
            <span
                style={{
                    minWidth: 0,
                    display: "flex",
                    alignItems: "center",
                    gap: 9,
                    overflow: "hidden",
                }}
            >
                <Icon
                    name={open ? "chevron" : "cright"}
                    size={13}
                    style={{ color: "var(--fg3)" }}
                />
                {path ? (
                    <span
                        title={label || "(unknown)"}
                        style={{
                            minWidth: 0,
                            flex: "1 1 auto",
                            display: "inline-flex",
                            alignItems: "baseline",
                            gap: 5,
                            overflow: "hidden",
                            whiteSpace: "nowrap",
                        }}
                    >
                        <span
                            style={{
                                minWidth: 0,
                                overflow: "hidden",
                                textOverflow: "ellipsis",
                                direction: "rtl",
                                textAlign: "left",
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 11.5,
                            }}
                        >
                            {path.dir}
                        </span>
                        <span
                            style={{
                                color: "var(--fg-max)",
                                fontFamily: "var(--fontFamily)",
                                fontSize: 13,
                                fontWeight: 600,
                            }}
                        >
                            {path.leaf}
                        </span>
                    </span>
                ) : (
                    <span
                        title={label || "(unknown)"}
                        style={{
                            minWidth: 0,
                            flex: "1 1 auto",
                            color: "var(--fg-max)",
                            fontFamily: "var(--fontFamily)",
                            fontSize: 13,
                            fontWeight: 600,
                            overflow: "hidden",
                            textOverflow: "ellipsis",
                            whiteSpace: "nowrap",
                        }}
                    >
                        {label}
                    </span>
                )}
                <span
                    style={{
                        width: 120,
                        height: 3,
                        flex: "0 0 auto",
                        borderRadius: 2,
                        background: "rgba(204,204,220,0.08)",
                        overflow: "hidden",
                    }}
                >
                    <span
                        style={{
                            display: "block",
                            height: "100%",
                            width: `${Math.max(0, Math.min(100, pct || 0))}%`,
                            background: "var(--brand-orange)",
                        }}
                    />
                </span>
                <span
                    style={{
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        whiteSpace: "nowrap",
                    }}
                >
                    {pct == null
                        ? "Spend share unavailable"
                        : `${pct}% of spend`}
                </span>
            </span>
            <span
                style={{
                    display: "flex",
                    alignItems: "center",
                    gap: 16,
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11.5,
                    whiteSpace: "nowrap",
                }}
            >
                <span style={{ color: "var(--fg2)" }}>
                    {count} {count === 1 ? "session" : "sessions"}
                </span>
                <span style={{ color: "var(--fg1)" }}>{formatCost(cost)}</span>
                <span style={{ color: "var(--fg2)" }}>{formatTokens(tokens)}</span>
                <span style={{ color: "var(--fg3)" }}>
                    {formatAgo(
                        last ? new Date(last).toISOString() : null,
                        now,
                    )}
                </span>
            </span>
        </button>
    );
}

function WorkspaceContextStrip({
    path,
    count,
    cost,
    tokens,
    last,
    share,
    now,
    onClear,
}) {
    const label = splitWorkspacePath(path);
    const pct = share == null ? null : Math.round(share * 100);
    return (
        <div
            style={{
                display: "flex",
                alignItems: "center",
                gap: 14,
                padding: "10px 16px",
                borderBottom: "1px solid var(--border-weak)",
                background: "rgba(34,37,43,0.55)",
                color: "var(--fg2)",
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 11.5,
            }}
        >
            <span
                title={path || "(unknown)"}
                style={{
                    minWidth: 0,
                    flex: "1 1 auto",
                    display: "inline-flex",
                    alignItems: "baseline",
                    gap: 5,
                    overflow: "hidden",
                    whiteSpace: "nowrap",
                }}
            >
                <span
                    style={{
                        minWidth: 0,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        direction: "rtl",
                        textAlign: "left",
                        color: "var(--fg3)",
                    }}
                >
                    {label.dir}
                </span>
                <span
                    style={{
                        flex: "0 0 auto",
                        color: "var(--fg-max)",
                        fontFamily: "var(--fontFamily)",
                        fontSize: 13,
                        fontWeight: 600,
                    }}
                >
                    {label.leaf}
                </span>
            </span>
            <span
                style={{
                    width: 120,
                    height: 3,
                    flex: "0 0 auto",
                    borderRadius: 2,
                    background: "rgba(204,204,220,0.08)",
                    overflow: "hidden",
                }}
            >
                <span
                    style={{
                        display: "block",
                        width: `${Math.max(0, Math.min(100, pct || 0))}%`,
                        height: "100%",
                        background: "var(--brand-orange)",
                    }}
                />
            </span>
            <span style={{ color: "var(--fg3)", whiteSpace: "nowrap" }}>
                {pct == null
                    ? "Range spend unavailable"
                    : `${pct}% of range spend`}
            </span>
            <span
                style={{
                    marginLeft: "auto",
                    display: "flex",
                    alignItems: "center",
                    gap: 16,
                    whiteSpace: "nowrap",
                }}
            >
                <span style={{ color: "var(--fg3)" }}>Range totals</span>
                <span>
                    {count} {count === 1 ? "session" : "sessions"}
                </span>
                <span
                    title={ESTIMATED_COST_TOOLTIP}
                    style={{ color: "var(--fg1)" }}
                >
                    {formatCost(cost)}
                </span>
                <span title="Workspace tokens in the selected range">
                    {formatTokens(tokens)}
                </span>
                <span style={{ color: "var(--fg3)" }}>
                    {formatAgo(
                        last ? new Date(last).toISOString() : null,
                        now,
                    )}
                </span>
                <button
                    type="button"
                    onClick={onClear}
                    title="Clear workspace filter"
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 5,
                        padding: "1px 8px",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        background: "transparent",
                        color: "var(--fg2)",
                        cursor: "pointer",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                    }}
                >
                    <Icon name="times" size={11} />
                    clear
                </button>
            </span>
        </div>
    );
}

// HelpTip is an info-icon disclosure. Native `title` waits on the browser
// delay; this opens after 300ms so a pass-through does not flash it.
const HELP_TIP_DELAY_MS = 300;

function HelpTip({ text, ariaLabel }) {
    const [open, setOpen] = useState(false);
    const triggerRef = useRef(null);
    const timerRef = useRef(null);
    const [pos, setPos] = useState(null);

    function clearTimer() {
        if (timerRef.current != null) {
            clearTimeout(timerRef.current);
            timerRef.current = null;
        }
    }

    function hide() {
        clearTimer();
        setOpen(false);
    }

    function show() {
        clearTimer();
        const el = triggerRef.current;
        if (!el) return;
        const r = el.getBoundingClientRect();
        const width = 300;
        const left = Math.max(
            8,
            Math.min(r.left, window.innerWidth - width - 8),
        );
        setPos({ top: r.bottom + 6, left });
        timerRef.current = setTimeout(() => setOpen(true), HELP_TIP_DELAY_MS);
    }

    useEffect(() => clearTimer, []);

    return (
        <span
            ref={triggerRef}
            role="button"
            tabIndex={0}
            aria-label={ariaLabel}
            onMouseEnter={show}
            onMouseLeave={hide}
            onFocus={show}
            onBlur={hide}
            onClick={(e) => e.stopPropagation()}
            style={{
                position: "relative",
                display: "inline-flex",
                color: "inherit",
                cursor: "help",
            }}
        >
            <Icon name="info" size={12} />
            {open && pos && (
                <span
                    role="tooltip"
                    style={{
                        position: "fixed",
                        top: pos.top,
                        left: pos.left,
                        zIndex: 80,
                        width: 300,
                        padding: "10px 12px",
                        background: "var(--bg-secondary)",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        boxShadow: "var(--shadow-z2)",
                        color: "var(--fg1)",
                        fontFamily: "var(--fontFamily)",
                        fontSize: 12,
                        fontWeight: 400,
                        lineHeight: 1.45,
                        whiteSpace: "normal",
                        pointerEvents: "none",
                    }}
                >
                    {text}
                </span>
            )}
        </span>
    );
}

// SortHeader is a clickable list-header cell: click sorts by the
// column, clicking again flips the direction.
function SortHeader({ label, sortKey, sort, onSort, tooltip }) {
    const active = sort.key === sortKey;
    return (
        <span
            style={{
                display: "inline-flex",
                alignItems: "center",
                gap: 4,
            }}
        >
            <button
                onClick={() => onSort(sortKey)}
                title={`Sort by ${label.toLowerCase()}`}
                style={{
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 4,
                    background: "transparent",
                    border: "none",
                    padding: 0,
                    cursor: "pointer",
                    font: "inherit",
                    textAlign: "left",
                    fontWeight: 500,
                    whiteSpace: "nowrap",
                    color: active ? "var(--fg1)" : "inherit",
                }}
            >
                {label}
                {active && (
                    <span style={{ fontSize: 8 }}>
                        {sort.dir === "asc" ? "▲" : "▼"}
                    </span>
                )}
            </button>
            {tooltip && (
                <HelpTip text={tooltip} ariaLabel={`${label} help`} />
            )}
        </span>
    );
}

// KpiTile is one cell of the KPI strip: a sentence-case label, a big
// mono value (optionally tinted, with a leading status dot), an
// optional progress bar, and a sub line.
function KpiTile({ label, value, valueColor, sub, dot, bar, tooltip }) {
    return (
        <SurfaceCard
            style={{
                padding: "14px 16px",
                display: "flex",
                flexDirection: "column",
                gap: 7,
                minHeight: 104,
            }}
        >
            <span
                style={{
                    fontSize: 11,
                    color: "var(--fg3)",
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 4,
                }}
            >
                {label}
                {tooltip && (
                    <HelpTip text={tooltip} ariaLabel={`${label} help`} />
                )}
            </span>
            <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
                {dot && (
                    <span
                        style={{
                            width: 8,
                            height: 8,
                            borderRadius: "50%",
                            background: dot,
                            flexShrink: 0,
                        }}
                    />
                )}
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 24,
                        fontWeight: 500,
                        lineHeight: 1,
                        color: valueColor || "var(--fg-max)",
                    }}
                >
                    {value}
                </span>
            </span>
            {bar != null && (
                <span
                    style={{
                        display: "block",
                        height: 4,
                        borderRadius: 2,
                        background: "rgba(204,204,220,0.1)",
                        overflow: "hidden",
                        marginTop: 1,
                    }}
                >
                    <span
                        style={{
                            display: "block",
                            height: "100%",
                            width: `${bar}%`,
                            background: "var(--viz-green)",
                        }}
                    />
                </span>
            )}
            {sub != null && (
                <span style={{ fontSize: 11, color: "var(--fg2)" }}>{sub}</span>
            )}
        </SurfaceCard>
    );
}

// KpiStrip surfaces the headline numbers for the in-view set: counts
// from the range + search conversations, token and cache rate from the
// chart's series (so they honour the model dropdown and legend
// toggles). "Tool calls" is the per-generation call count; "Errored
// conversations" counts conversations with a call error, since the
// list API exposes no per-tool-call breakdown.
function KpiStrip({ kpi }) {
    const avg = kpi.avgCalls.toFixed(1).replace(/\.0$/, "");
    return (
        <div
            style={{
                display: "grid",
                gridTemplateColumns: "repeat(6, 1fr)",
                gap: 12,
                marginBottom: 16,
            }}
        >
            <KpiTile
                label="Sessions"
                value={kpi.conversations}
                sub={kpi.conversationsSub}
            />
            <KpiTile
                label="Cost"
                value={formatCost(kpi.cost)}
                sub={kpi.costSub}
                tooltip={ESTIMATED_COST_TOOLTIP}
            />
            <KpiTile
                label="Total tokens"
                value={formatTokens(kpi.tokens)}
                sub={`${kpi.models} ${kpi.models === 1 ? "model" : "models"}`}
            />
            <KpiTile
                label="Input cache hit"
                value={kpi.cachePct == null ? "\u2014" : `${kpi.cachePct}%`}
                bar={kpi.cachePct == null ? 0 : kpi.cachePct}
            />
            <KpiTile
                label="Tool calls"
                value={kpi.calls}
                sub={`${avg} avg / session`}
            />
            <KpiTile
                label="Errored sessions"
                value={kpi.errConvs}
                valueColor={
                    kpi.errConvs > 0 ? "var(--error-text)" : "var(--fg-max)"
                }
                dot={kpi.errConvs > 0 ? "var(--error-text)" : undefined}
                sub={`${kpi.errPct}% of sessions`}
            />
        </div>
    );
}

function ConversationsView({
    conversations,
    storeCount,
    tokenPoints,
    tokenIntervalMs,
    loading,
    error,
    query,
    setQuery,
    searchInputRef,
    timeRange,
    setTimeRange,
    tokenModel,
    setTokenModel,
    chartMetric,
    setChartMetric,
    bucketSel,
    setBucketSel,
    workspace,
    setWorkspace,
    groupBy,
    setGroupBy,
    listSort,
    setListSort,
    onOpen,
    onRefresh,
    refreshing,
    onOpenSettings,
    history,
}) {
    const now = Date.now();
    const prices = useModelPrices();
    const range = timeRangeOption(timeRange);
    const trimmedQuery = query.trim();
    const searchActive = trimmedQuery.length > 0;
    const search = useSearchResults(query);
    const {
        phase: searchPhase,
        hits: searchHits,
        mode: searchMode,
        error: searchError,
        selectedIndex: searchSelectedIndex,
        setSelectedIndex: setSearchSelectedIndex,
        retry: retrySearch,
    } = search;
    const [agentFilter, setAgentFilter] = useState("all");
    const [modelFilter, setModelFilter] = useState("all");
    const [statusFilter, setStatusFilter] = useState("all");
    const rangeFiltered = useMemo(() => {
        if (range.ms == null) return conversations;
        const from = now - range.ms;
        return conversations.filter((c) => {
            const t = conversationTime(c);
            return t != null && t >= from && t <= now;
        });
    }, [conversations, range.ms, now]);

    // Explicit group overrides survive filters because they are keyed by group.
    // A groupBy change resets them because the keys change meaning.
    const [groupOpen, setGroupOpen] = useState(() => new Map());
    useEffect(() => setGroupOpen(new Map()), [groupBy]);
    const toggleGroup = useCallback((key, open) => {
        setGroupOpen((previous) => {
            const next = new Map(previous);
            next.set(key, !open);
            return next;
        });
    }, []);
    const workspaces = useMemo(() => {
        const map = new Map();
        for (const c of rangeFiltered) {
            const w = c.workspace || "";
            let e = map.get(w);
            if (!e) {
                e = {
                    path: w,
                    count: 0,
                    cost: 0,
                    costComplete: true,
                    tokens: 0,
                    dur: 0,
                    last: 0,
                };
                map.set(w, e);
            }
            e.count++;
            const cost = conversationCost(c, prices);
            if (cost == null) e.costComplete = false;
            else e.cost += cost;
            e.tokens += c.total_tokens || 0;
            const d = durationBetweenSeconds(c.started_at, c.last_activity);
            if (d != null) e.dur += d;
            const t = conversationTime(c);
            if (t != null && t > e.last) e.last = t;
        }
        return [...map.values()]
            .map((entry) => ({
                ...entry,
                cost: entry.costComplete ? entry.cost : null,
            }))
            .sort((a, b) => b.last - a.last);
    }, [rangeFiltered, prices]);
    const totalCost = useMemo(() => {
        // Sum priced sessions only. One unpriced model used to zero the
        // whole header to NO_VALUE even when other sessions had a real
        // dollar figure — the KPI tile already skips those.
        let cost = 0;
        let priced = 0;
        for (const conversation of rangeFiltered) {
            const value = conversationCost(conversation, prices);
            if (value == null) continue;
            cost += value;
            priced++;
        }
        return priced ? cost : null;
    }, [rangeFiltered, prices]);
    const agentCount = useMemo(() => {
        const set = new Set();
        for (const c of rangeFiltered)
            for (const a of agentHosts(c.agents)) set.add(a);
        return set.size;
    }, [rangeFiltered]);
    // Keep an out-of-range workspace selected so the page can show its empty
    // context instead of silently returning to all workspaces.
    const activeWorkspace = workspace;
    const selectedWorkspaceRangeAggregate =
        activeWorkspace == null
            ? null
            : workspaces.find((w) => w.path === activeWorkspace) || {
                  path: activeWorkspace,
                  count: 0,
                  cost: 0,
                  costComplete: true,
                  tokens: 0,
                  dur: 0,
                  last: 0,
              };
    const wsFiltered = useMemo(
        () =>
            activeWorkspace == null
                ? rangeFiltered
                : rangeFiltered.filter(
                      (c) => (c.workspace || "") === activeWorkspace,
                  ),
        [rangeFiltered, activeWorkspace],
    );

    const agentOptions = useMemo(() => {
        const set = new Set();
        for (const c of wsFiltered)
            for (const a of agentHosts(c.agents)) set.add(a);
        return [...set].sort();
    }, [wsFiltered]);
    const activeAgentFilter = agentOptions.includes(agentFilter)
        ? agentFilter
        : "all";

    const modelFacetOptions = useMemo(() => {
        const set = new Set();
        for (const c of wsFiltered)
            for (const m of c.models || []) if (m) set.add(m);
        return [...set].sort();
    }, [wsFiltered]);
    const activeModelFilter = modelFacetOptions.includes(modelFilter)
        ? modelFilter
        : "all";
    const activeStatusFilter =
        statusFilter === "errors" || statusFilter === "subagents"
            ? statusFilter
            : "all";

    const filtered = useMemo(() => {
        return wsFiltered.filter((c) => {
            if (
                activeAgentFilter !== "all" &&
                !agentHosts(c.agents).includes(activeAgentFilter)
            )
                return false;
            if (
                activeModelFilter !== "all" &&
                !(c.models || []).includes(activeModelFilter)
            )
                return false;
            if (activeStatusFilter === "errors" && c.status !== "err")
                return false;
            if (activeStatusFilter === "subagents" && !(c.subagents > 0))
                return false;
            return true;
        });
    }, [wsFiltered, activeAgentFilter, activeModelFilter, activeStatusFilter]);

    const activeFilterCount =
        (activeWorkspace != null ? 1 : 0) +
        (activeAgentFilter !== "all" ? 1 : 0) +
        (activeModelFilter !== "all" ? 1 : 0) +
        (activeStatusFilter !== "all" ? 1 : 0);
    const clearFilters = useCallback(() => {
        setWorkspace(null);
        setAgentFilter("all");
        setModelFilter("all");
        setStatusFilter("all");
    }, [setWorkspace]);
    const clearSearch = useCallback(() => {
        setQuery("");
        setTimeout(() => {
            const el = searchInputRef && searchInputRef.current;
            if (el) el.focus();
        }, 0);
    }, [setQuery, searchInputRef]);
    const onSearchInputKey = useCallback(
        (e) => {
            if (e.key === "Escape") {
                e.preventDefault();
                clearSearch();
                return;
            }
            if (!searchActive || searchHits.length === 0) return;
            if (e.key === "ArrowDown") {
                e.preventDefault();
                setSearchSelectedIndex((i) =>
                    Math.min(searchHits.length - 1, i < 0 ? 0 : i + 1),
                );
            } else if (e.key === "ArrowUp") {
                e.preventDefault();
                setSearchSelectedIndex((i) => Math.max(-1, i - 1));
            } else if (e.key === "Enter" && searchSelectedIndex >= 0) {
                e.preventDefault();
                const hit = searchHits[searchSelectedIndex];
                if (hit) onOpen({ id: hit.id, title: hit.title });
            }
        },
        [
            clearSearch,
            searchActive,
            searchHits,
            searchSelectedIndex,
            setSearchSelectedIndex,
            onOpen,
        ],
    );
    const searchRightAdornment =
        searchPhase === "loading" ? (
            <span
                className="sigil-spin"
                style={{
                    width: 14,
                    height: 14,
                    borderRadius: "50%",
                    border: "2px solid var(--border-strong)",
                    borderTopColor: "var(--fg2)",
                    display: "inline-block",
                }}
                aria-label="Searching"
            />
        ) : searchActive ? (
            <button
                type="button"
                onClick={clearSearch}
                aria-label="Clear search"
                title="Clear search"
                style={{
                    width: 22,
                    height: 22,
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "center",
                    background: "transparent",
                    border: "none",
                    color: "var(--fg3)",
                    cursor: "pointer",
                    borderRadius: 2,
                }}
                onMouseEnter={(e) => {
                    e.currentTarget.style.background = "var(--action-hover)";
                    e.currentTarget.style.color = "var(--fg1)";
                }}
                onMouseLeave={(e) => {
                    e.currentTarget.style.background = "transparent";
                    e.currentTarget.style.color = "var(--fg3)";
                }}
            >
                <Icon name="times" size={14} />
            </button>
        ) : (
            <span
                title="Press Command-K or Control-K to focus search"
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                    color: "var(--fg3)",
                    padding: "1px 6px",
                    border: "1px solid var(--border-weak)",
                    borderRadius: 2,
                }}
            >
                ⌘K
            </span>
        );

    // Token chart has its own model filter and is driven only by the
    // time range, not the text query (token points carry model, not the
    // searchable conversation fields). The selection lives in App so it
    // survives navigating into a conversation and back; a model that
    // disappears from the store falls back to "all" by derivation.
    const points = tokenPoints || [];
    const tokenModels = useMemo(
        () =>
            Array.from(
                new Set(points.map((p) => p.model).filter(Boolean)),
            ).sort(),
        [points],
    );
    const effectiveModel = tokenModels.includes(tokenModel)
        ? tokenModel
        : "all";
    const tokenFiltered = useMemo(
        () =>
            effectiveModel === "all"
                ? points
                : points.filter((p) => p.model === effectiveModel),
        [points, effectiveModel],
    );
    // Legend visibility is shared with the KPI strip so hiding a series
    // rescales the chart and drops it from the headline tokens in step.
    // Lives here, not in TokenChart, so both read the one set.
    const [hiddenSeries, setHiddenSeries] = useState(() => new Set());
    const toggleSeries = useCallback(
        (key) =>
            setHiddenSeries((prev) => {
                const next = new Set(prev);
                next.has(key) ? next.delete(key) : next.add(key);
                return next;
            }),
        [],
    );
    // Both metrics share one window so switching the chart between
    // them doesn't shift the time axis; with per-metric windows the
    // "All" range drifts when the datasets' extents differ. The window
    // is snapped to the bucket ladder the token endpoint aggregates on,
    // so each server point falls inside exactly one bar.
    //
    // A fixed range asks the server for the width it will draw on. Only
    // the "All" range needs the reported width as a floor: there the server
    // derives it from the whole store while the bars follow the visible
    // window, which a model facet can narrow.
    const serverIntervalMs = range.ms == null ? tokenIntervalMs : 0;
    const chartWindow = useMemo(() => {
        const times = filtered
            .map(conversationTime)
            .concat(tokenFiltered.map(tokenPointTime));
        return chartGrid(times, timeRange, now, serverIntervalMs);
    }, [filtered, tokenFiltered, timeRange, now, serverIntervalMs]);
    const activity = useMemo(
        () =>
            bucketActivity(filtered, timeRange, now, {
                window: chartWindow,
                count: chartWindow.count,
            }),
        [filtered, timeRange, now, chartWindow],
    );
    const tokenUsage = useMemo(
        () =>
            bucketTokenUsage(tokenFiltered, timeRange, now, {
                window: chartWindow,
                count: chartWindow.count,
            }),
        [tokenFiltered, timeRange, now, chartWindow],
    );
    // Bucket drill-down from a chart bar click: the list narrows to
    // conversations active inside the picked bucket, while the charts
    // keep the full window and just highlight the selection.
    const onBucketClick = useCallback(
        (b) => {
            setBucketSel((sel) =>
                sel && sel.start === b.start && sel.end === b.end
                    ? null
                    : { start: b.start, end: b.end },
            );
        },
        [setBucketSel],
    );
    const listFiltered = useMemo(() => {
        if (!bucketSel) return filtered;
        return filtered.filter((c) => {
            const endT = conversationTime(c);
            if (endT == null) return false;
            const startT = new Date(c.started_at).getTime();
            const s = Number.isFinite(startT) ? startT : endT;
            return s < bucketSel.end && endT >= bucketSel.start;
        });
    }, [filtered, bucketSel]);

    const handleSort = useCallback(
        (key) => {
            setListSort((s) =>
                s.key === key
                    ? { key, dir: s.dir === "desc" ? "asc" : "desc" }
                    : { key, dir: "desc" },
            );
        },
        [setListSort],
    );
    const sorted = useMemo(() => {
        const dir = listSort.dir === "asc" ? 1 : -1;
        const val = (c) => {
            if (listSort.key === "duration") {
                const d = durationBetweenSeconds(c.started_at, c.last_activity);
                return d == null ? -1 : d;
            }
            if (listSort.key === "tokens") return c.total_tokens || 0;
            if (listSort.key === "cost")
                return conversationCost(c, prices) || 0;
            const t = conversationTime(c);
            return t == null ? 0 : t;
        };
        return [...listFiltered].sort((a, b) => (val(a) - val(b)) * dir);
    }, [listFiltered, listSort, prices]);

    const grouped = useMemo(() => {
        if (groupBy === "none") return [];
        const map = new Map();
        for (const c of sorted) {
            const key = groupKeyFor(c, groupBy);
            let group = map.get(key);
            if (!group) {
                group = {
                    key,
                    rows: [],
                    count: 0,
                    cost: 0,
                    costComplete: true,
                    tokens: 0,
                    dur: 0,
                    last: 0,
                };
                map.set(key, group);
            }
            group.rows.push(c);
            group.count++;
            const cost = conversationCost(c, prices);
            if (cost == null) group.costComplete = false;
            else group.cost += cost;
            group.tokens += c.total_tokens || 0;
            const duration = durationBetweenSeconds(
                c.started_at,
                c.last_activity,
            );
            if (duration != null) group.dur += duration;
            const time = conversationTime(c);
            if (time != null && time > group.last) group.last = time;
        }
        const direction = listSort.dir === "asc" ? 1 : -1;
        const value = (group) => {
            if (listSort.key === "cost") return group.cost;
            if (listSort.key === "tokens") return group.tokens;
            if (listSort.key === "duration") return group.dur;
            return group.last;
        };
        return [...map.values()]
            .filter((group) => group.rows.length > 0)
            .sort((a, b) => (value(a) - value(b)) * direction)
            .map((group) => ({
                ...group,
                cost: group.costComplete ? group.cost : null,
            }));
    }, [sorted, groupBy, listSort, prices]);
    const groupedTotalCost = useMemo(() => {
        let cost = 0;
        for (const group of grouped) {
            if (group.cost == null) return null;
            cost += group.cost;
        }
        return cost;
    }, [grouped]);
    const collapsed = useMemo(() => {
        if (
            groupBy === "none" ||
            (groupBy === "workspace" && activeWorkspace != null)
        )
            return { groups: 0, sessions: 0 };
        let groups = 0;
        let sessions = 0;
        grouped.forEach((group, index) => {
            const open = groupOpen.has(group.key)
                ? groupOpen.get(group.key)
                : index < OPEN_GROUPS;
            if (open) return;
            groups++;
            sessions += group.count;
        });
        return { groups, sessions };
    }, [grouped, groupOpen, groupBy, activeWorkspace]);

    // KPI tiles read the range + workspace + search set (not the bucket
    // drill-down), computed straight off each conversation's token buckets.
    // This keeps headline tokens, cost, and cache rate conversation-based rather
    // than tied to the token-series chart, which has its own model filter.
    const kpi = useMemo(() => {
        let calls = 0,
            errConvs = 0,
            cost = 0,
            priced = 0,
            unpriced = 0;
        const tot = {
            fresh_input: 0,
            cache_read: 0,
            cache_write: 0,
            output: 0,
            reasoning: 0,
        };
        const models = new Set();
        for (const c of filtered) {
            calls += c.calls || 0;
            if (c.status === "err") errConvs++;
            const cc = conversationCost(c, prices);
            if (cc == null) unpriced++;
            else {
                cost += cc;
                priced++;
            }
            const b = c.token_buckets;
            if (b) for (const k in tot) tot[k] += b[k] || 0;
            for (const m of c.models || []) models.add(m);
        }
        const tokens =
            tot.fresh_input +
            tot.cache_read +
            tot.cache_write +
            tot.output +
            tot.reasoning;
        // Cost sub is honest about coverage: if some conversations ran on an
        // unpriced (non-Anthropic) model, say so rather than implying the
        // total covers everything.
        const costSub =
            unpriced > 0
                ? `${unpriced} unpriced · ${formatCost(priced ? cost / priced : 0)} avg`
                : cost
                  ? `${formatCost(cost / Math.max(1, priced))} avg / session`
                  : "estimated";
        return {
            conversations: filtered.length,
            conversationsSub:
                activeWorkspace != null ? "in workspace" : "active in range",
            tokens,
            cost: priced ? cost : null, // nothing priced, so NO_VALUE rather than a misleading $0
            costSub,
            models: models.size,
            cachePct: cacheInputHitPercent(
                tot.fresh_input,
                tot.cache_read,
                tot.cache_write,
            ),
            calls,
            avgCalls: filtered.length ? calls / filtered.length : 0,
            errConvs,
            errPct: filtered.length
                ? Math.round((errConvs / filtered.length) * 100)
                : 0,
        };
    }, [filtered, activeWorkspace, prices]);

    const emptyStore =
        !error &&
        !loading &&
        !searchActive &&
        activeWorkspace == null &&
        rangeFiltered.length === 0 &&
        (storeCount === 0 ||
            (storeCount == null && conversations.length === 0));
    const hasSessions = storeCount > 0 || conversations.length > 0;
    // Offers stay `show` until the user imports or dismisses, so a store
    // filled by live capture still gets the Settings → History hint. A
    // completed first import answers the offer and the hint stays gone.
    const importUnused = (history && history.offers
        ? history.offers
        : []
    ).some((offer) => offer.show);

    return (
        <PageShell maxWidth={1400}>
            <PageHero
                title="Sessions"
                desc={
                    searchActive
                        ? "Full-text search over prompts, responses, and tool output in all captured local sessions."
                        : "Captured sessions, token usage, costs, and tool-call activity from local runs."
                }
                stats={
                    searchActive
                        ? [
                              {
                                  label: "Index",
                                  value:
                                      searchMode === "semantic"
                                          ? "QMD"
                                          : "FTS",
                                  tone: "var(--primary-text)",
                              },
                              {
                                  label: "Results",
                                  value: String(searchHits.length),
                                  tone: searchHits.length
                                      ? "var(--success-text)"
                                      : "var(--fg3)",
                              },
                              {
                                  label: "Status",
                                  value:
                                      searchPhase === "loading"
                                          ? "Searching"
                                          : "Ready",
                                  tone:
                                      searchPhase === "loading"
                                          ? "var(--warning-text)"
                                          : undefined,
                              },
                          ]
                        : [
                              { label: "Range", value: range.label },
                              {
                                  label: "Workspaces",
                                  value: String(workspaces.length),
                              },
                              {
                                  label: "Agents",
                                  value: String(agentCount),
                              },
                          ]
                }
            />
            {!searchActive &&
                (history && importRunIsActive(history.run) ? (
                    <HistoryImportProgress
                        run={history.run}
                        onCancel={history.cancel}
                    />
                ) : hasSessions && importUnused ? (
                    <ImportHintBanner onOpenSettings={onOpenSettings} />
                ) : null)}
            <FilterBar
                query={query}
                onQueryChange={setQuery}
                inputRef={searchInputRef}
                timeRange={searchActive ? null : timeRange}
                onTimeRangeChange={searchActive ? null : setTimeRange}
                workspaces={searchActive ? [] : workspaces}
                workspace={searchActive ? undefined : activeWorkspace}
                onWorkspaceChange={searchActive ? undefined : setWorkspace}
                workspaceSessionCount={rangeFiltered.length}
                workspaceTotalCost={totalCost}
                now={now}
                rangeLabel={range.label}
                groupBy={searchActive ? undefined : groupBy}
                onGroupByChange={searchActive ? undefined : setGroupBy}
                agentFilter={searchActive ? undefined : activeAgentFilter}
                onAgentFilterChange={
                    searchActive ? undefined : setAgentFilter
                }
                agentOptions={searchActive ? [] : agentOptions}
                modelFilter={searchActive ? undefined : activeModelFilter}
                onModelFilterChange={
                    searchActive ? undefined : setModelFilter
                }
                modelOptions={searchActive ? [] : modelFacetOptions}
                statusFilter={searchActive ? undefined : activeStatusFilter}
                onStatusFilterChange={
                    searchActive ? undefined : setStatusFilter
                }
                activeFilterCount={searchActive ? 0 : activeFilterCount}
                onClearFilters={clearFilters}
                onRefresh={onRefresh}
                refreshing={refreshing}
                placeholder="Search prompts, responses, tool output, title, agent, model…"
                onInputKeyDown={onSearchInputKey}
                rightAdornment={searchRightAdornment}
            />
            {searchActive ? (
                <ConversationSearchPanel
                    query={trimmedQuery}
                    hits={searchHits}
                    phase={searchPhase}
                    mode={searchMode}
                    error={searchError}
                    selectedIndex={searchSelectedIndex}
                    setSelectedIndex={setSearchSelectedIndex}
                    retry={retrySearch}
                    now={now}
                    onOpen={onOpen}
                />
            ) : emptyStore ? (
                history && <SettingsHistoryTab history={history} />
            ) : (
                <React.Fragment>
                    <KpiStrip kpi={kpi} />
                    {chartMetric === "activity" ? (
                        <ActivityChart
                            data={activity.buckets}
                            bucketLabel={activity.bucketLabel}
                            selection={bucketSel}
                            onBucketClick={onBucketClick}
                            switcher={
                                <ChartSwitch
                                    value={chartMetric}
                                    onChange={setChartMetric}
                                />
                            }
                        />
                    ) : (
                        <TokenChart
                            data={tokenUsage.buckets}
                            bucketLabel={tokenUsage.bucketLabel}
                            grandTotal={tokenUsage.grandTotal}
                            models={tokenModels}
                            model={effectiveModel}
                            onModelChange={setTokenModel}
                            hidden={hiddenSeries}
                            onToggleSeries={toggleSeries}
                            selection={bucketSel}
                            onBucketClick={onBucketClick}
                            switcher={
                                <ChartSwitch
                                    value={chartMetric}
                                    onChange={setChartMetric}
                                />
                            }
                        />
                    )}

                    {bucketSel && (
                        <div
                            style={{
                                marginTop: 10,
                                display: "flex",
                                alignItems: "center",
                                gap: 10,
                                fontSize: 11,
                                fontFamily: "var(--fontFamilyMonospace)",
                                color: "var(--fg2)",
                            }}
                        >
                            <span>
                                Showing{" "}
                                {formatBucketLabel(
                                    bucketSel.start,
                                    bucketSel.end - bucketSel.start,
                                )}{" "}
                                to{" "}
                                {formatBucketLabel(
                                    bucketSel.end,
                                    bucketSel.end - bucketSel.start,
                                )}
                            </span>
                            <button
                                onClick={() => setBucketSel(null)}
                                style={{
                                    background: "transparent",
                                    border: "1px solid var(--border-medium)",
                                    borderRadius: 2,
                                    color: "var(--fg2)",
                                    cursor: "pointer",
                                    fontSize: 11,
                                    fontFamily:
                                        "var(--fontFamilyMonospace)",
                                    padding: "1px 8px",
                                }}
                            >
                                ✕ clear
                            </button>
                        </div>
                    )}

                    <SurfaceCard
                        style={{
                            marginTop: 18,
                            overflow: "hidden",
                        }}
                    >
                        <div
                            style={{
                                display: "grid",
                                gridTemplateColumns: CONV_GRID,
                                alignItems: "center",
                                gap: 16,
                                padding: "11px 16px",
                                borderBottom:
                                    "1px solid var(--border-weak)",
                                background: "var(--bg-secondary)",
                                fontFamily: "var(--fontFamily)",
                                fontSize: 12,
                                color: "var(--fg3)",
                                fontWeight: 500,
                            }}
                        >
                            <SortHeader
                                label="Last activity"
                                sortKey="last_activity"
                                sort={listSort}
                                onSort={handleSort}
                            />
                            <span>Session</span>
                            <span>Agent</span>
                            <SortHeader
                                label="Cost"
                                sortKey="cost"
                                sort={listSort}
                                onSort={handleSort}
                                tooltip={ESTIMATED_COST_TOOLTIP}
                            />
                            <SortHeader
                                label="Tokens"
                                sortKey="tokens"
                                sort={listSort}
                                onSort={handleSort}
                            />
                            <SortHeader
                                label="Duration"
                                sortKey="duration"
                                sort={listSort}
                                onSort={handleSort}
                            />
                            <span>Models</span>
                        </div>

                        {!error &&
                            (!loading ||
                                selectedWorkspaceRangeAggregate?.count > 0) &&
                            selectedWorkspaceRangeAggregate && (
                                <WorkspaceContextStrip
                                    path={
                                        selectedWorkspaceRangeAggregate.path
                                    }
                                    count={
                                        selectedWorkspaceRangeAggregate.count
                                    }
                                    cost={
                                        selectedWorkspaceRangeAggregate.cost
                                    }
                                    tokens={
                                        selectedWorkspaceRangeAggregate.tokens
                                    }
                                    last={
                                        selectedWorkspaceRangeAggregate.last
                                    }
                                    share={
                                        totalCost != null &&
                                        totalCost > 0 &&
                                        selectedWorkspaceRangeAggregate.cost !=
                                            null
                                            ? selectedWorkspaceRangeAggregate.cost /
                                              totalCost
                                            : null
                                    }
                                    now={now}
                                    onClear={() => setWorkspace(null)}
                                />
                            )}

                        {error && (
                            <div style={{ padding: 16 }}>
                                <Notice
                                    kind="error"
                                    title="Failed to load sessions"
                                >
                                    {error}
                                </Notice>
                            </div>
                        )}
                        {!error &&
                            loading &&
                            conversations.length === 0 && (
                                <div
                                    style={{
                                        padding: "32px 18px",
                                        color: "var(--fg3)",
                                        fontFamily:
                                            "var(--fontFamilyMonospace)",
                                        fontSize: 12,
                                    }}
                                >
                                    Loading…
                                </div>
                            )}
                        {/* The list request is range-scoped, so an empty page does not
                            mean an empty store. storeCount comes from the response and
                            decides which notice applies; null (no response yet, or an
                            older daemon) falls back to reading the page. The empty-store
                            case is handled above. */}
                        {!error &&
                            !loading &&
                            activeWorkspace == null &&
                            rangeFiltered.length === 0 &&
                            (storeCount > 0 ||
                                conversations.length > 0) && (
                                <div
                                    style={{
                                        padding: "16px 18px",
                                        color: "var(--fg2)",
                                        fontSize: 12,
                                    }}
                                >
                                    No sessions in{" "}
                                    <code style={{ color: "var(--fg1)" }}>
                                        {range.label}
                                    </code>
                                    .
                                </div>
                            )}
                        {!error &&
                            !loading &&
                            selectedWorkspaceRangeAggregate?.count === 0 && (
                                <div
                                    style={{
                                        padding: "16px 18px",
                                        color: "var(--fg2)",
                                        fontSize: 12,
                                    }}
                                >
                                    No sessions in this range.
                                </div>
                            )}
                        {!error &&
                            filtered.length === 0 &&
                            rangeFiltered.length > 0 &&
                            (activeWorkspace == null ||
                                selectedWorkspaceRangeAggregate?.count > 0) && (
                                <div
                                    style={{
                                        padding: "16px 18px",
                                        color: "var(--fg2)",
                                        fontSize: 12,
                                    }}
                                >
                                    No sessions match the current filters.
                                </div>
                            )}
                        {!error &&
                            bucketSel &&
                            listFiltered.length === 0 &&
                            filtered.length > 0 && (
                                <div
                                    style={{
                                        padding: "16px 18px",
                                        color: "var(--fg2)",
                                        fontSize: 12,
                                    }}
                                >
                                    No sessions in the selected bucket.
                                </div>
                            )}
                        {groupBy === "none"
                            ? sorted.map((c) => (
                                  <ConvRow
                                      key={c.id}
                                      c={c}
                                      now={now}
                                      onOpen={onOpen}
                                      prices={prices}
                                  />
                              ))
                            : groupBy === "workspace" &&
                                activeWorkspace != null
                              ? sorted.map((c) => (
                                    <ConvRow
                                        key={c.id}
                                        c={c}
                                        now={now}
                                        onOpen={onOpen}
                                        prices={prices}
                                        hideWorkspace
                                    />
                                ))
                              : grouped.map((group, index) => {
                                  const open = groupOpen.has(group.key)
                                      ? groupOpen.get(group.key)
                                      : index < OPEN_GROUPS;
                                  return (
                                      <React.Fragment
                                          key={`${groupBy}:${group.key}`}
                                      >
                                          <SessionGroupHeader
                                              groupBy={groupBy}
                                              label={group.key}
                                              open={open}
                                              onToggle={() =>
                                                  toggleGroup(group.key, open)
                                              }
                                              count={group.count}
                                              cost={group.cost}
                                              tokens={group.tokens}
                                              last={group.last}
                                              share={
                                                  groupedTotalCost != null &&
                                                  groupedTotalCost > 0 &&
                                                  group.cost != null
                                                      ? group.cost /
                                                        groupedTotalCost
                                                      : null
                                              }
                                              now={now}
                                          />
                                          {open &&
                                              group.rows.map((c) => (
                                                  <ConvRow
                                                      key={c.id}
                                                      c={c}
                                                      now={now}
                                                      onOpen={onOpen}
                                                      prices={prices}
                                                      grouped
                                                      hideWorkspace={
                                                          groupBy ===
                                                          "workspace"
                                                      }
                                                  />
                                              ))}
                                      </React.Fragment>
                                  );
                              })}
                        <div
                            style={{
                                padding: "11px 16px",
                                fontSize: 11,
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                            }}
                        >
                            {sorted.length} of {filtered.length}{" "}
                            {filtered.length === 1
                                ? "session"
                                : "sessions"}
                            {collapsed.groups > 0 && (
                                <React.Fragment>
                                    {" · "}
                                    {collapsed.groups} collapsed{" "}
                                    {groupBy === "workspace"
                                        ? collapsed.groups === 1
                                            ? "workspace"
                                            : "workspaces"
                                        : `${groupBy} ${collapsed.groups === 1 ? "group" : "groups"}`}{" "}
                                    {collapsed.groups === 1
                                        ? "hides"
                                        : "hide"}{" "}
                                    {collapsed.sessions}{" "}
                                    {collapsed.sessions === 1
                                        ? "session"
                                        : "sessions"}
                                </React.Fragment>
                            )}
                            {activeFilterCount > 0
                                ? ` · ${activeFilterCount} ${activeFilterCount === 1 ? "filter" : "filters"} active`
                                : ""}
                        </div>
                    </SurfaceCard>
                </React.Fragment>
            )}
        </PageShell>
    );
}

// ============================================================
// Screen 2 — Conversation detail
// ============================================================

function partKind(part) {
    return (
        part.kind ||
        (part.text
            ? "text"
            : part.thinking
              ? "thinking"
              : part.tool_call
                ? "tool_call"
                : part.tool_result
                  ? "tool_result"
                  : "unknown")
    );
}

function messageParts(messages) {
    const out = [];
    for (const message of messages || []) {
        for (const part of message.parts || []) out.push(part);
    }
    return out;
}

function resultParts(messages) {
    return messageParts(messages)
        .filter((part) => partKind(part) === "tool_result" && part.tool_result)
        .map((part) => part.tool_result);
}

function outputCalls(gen) {
    return messageParts((gen && gen.output) || [])
        .filter((part) => partKind(part) === "tool_call" && part.tool_call)
        .map((part) => part.tool_call);
}

function resolveResult(gen, next, call, used = new Set()) {
    const sameGeneration = resultParts(
        [].concat((gen && gen.output) || [], (gen && gen.input) || []),
    );
    const following = resultParts((next && next.input) || []);
    const available = (result) => !used.has(result);
    let result = null;
    if (call.id) {
        result =
            sameGeneration.find(
                (item) => available(item) && item.tool_call_id === call.id,
            ) ||
            following.find(
                (item) => available(item) && item.tool_call_id === call.id,
            ) ||
            null;
    } else {
        result =
            sameGeneration.find(
                (item) =>
                    available(item) && item.name && item.name === call.name,
            ) ||
            following.find(
                (item) =>
                    available(item) && item.name && item.name === call.name,
            ) ||
            null;
    }
    if (result) used.add(result);
    return result;
}

function resultBody(result) {
    if (!result) return "";
    if (result.content) return result.content;
    if (result.content_json == null) return "";
    if (typeof result.content_json === "string") return result.content_json;
    try {
        return JSON.stringify(result.content_json, null, 2);
    } catch (_) {
        return String(result.content_json);
    }
}

// IDE integrations and agent harnesses prepend one or more complete
// XML-ish blocks, with or without attributes: <user_info>, <rules>, and
// pi's <skill name="..." location="...">. Scan only from the start so
// markup inside the prompt does not move the split point.
// scanPreambleBlocks walks the complete blocks at the head of the text and
// reports where they end and what they were called. Names come from this
// walk rather than from a search for tags, because a skill body is full of
// angle-bracket words that are not blocks.
function scanPreambleBlocks(source) {
    const tags = [];
    let cursor = 0;
    let end = 0;
    while (cursor < source.length) {
        while (cursor < source.length && /\s/.test(source[cursor])) cursor++;
        const open = /^<([a-z_][a-z0-9_-]*)(?:\s[^>]*)?>/.exec(
            source.slice(cursor),
        );
        if (!open) break;
        const close = `</${open[1]}>`;
        const closeAt = source.indexOf(close, cursor + open[0].length);
        if (closeAt < 0) break;
        if (!tags.includes(open[1])) tags.push(open[1]);
        cursor = closeAt + close.length;
        end = cursor;
    }
    return { end, tags };
}

function splitPreamble(text) {
    const source = String(text || "");
    const scan = scanPreambleBlocks(source);
    let end = scan.end;
    if (scan.tags.length === 0) return { preamble: "", prompt: source };
    while (end < source.length && /\s/.test(source[end])) end++;
    const prompt = source.slice(end);
    if (!prompt.trim()) return { preamble: "", prompt: source };
    return { preamble: source.slice(0, end), prompt };
}

// Agent prose is markdown, and it is model output, so it is rendered the
// way the Agent Observability app plugin renders its own: markdown-to-jsx
// into a React tree, never innerHTML, with raw HTML parsing off so markup
// in the text shows up as text. The overrides below are ported from that
// plugin's MarkdownPreview, and web_test.go covers each of them.

// BlockedElement drops an element instead of rendering it. Every tag that
// can load a remote resource or run script is mapped to it, so the viewer
// cannot be made to phone home by a session it displays.
function BlockedElement() {
    return null;
}

const MARKDOWN_BLOCKED_TAGS = [
    "iframe",
    "video",
    "audio",
    "embed",
    "object",
    "source",
    "track",
    "base",
    "script",
    "svg",
    "math",
    "style",
    "link",
    "form",
    "textarea",
    "select",
    "button",
    "details",
    "dialog",
    "img",
];

// markdownURL returns the href to render, or undefined to render the link
// with none. Relative forms pass through; everything else has to parse as
// a URL in one of four schemes, which is what rejects javascript:, data:,
// and vbscript:. Tabs, newlines and backslashes go first, because browsers
// strip them before resolving a URL and they can otherwise hide a scheme.
function markdownURL(input) {
    if (!input) return undefined;
    const raw = String(input)
        .trim()
        .replace(/[\t\r\n\\]/g, "");
    if (!raw || raw.startsWith("//")) return undefined;
    if (/^[/#?]/.test(raw) || raw.startsWith("./") || raw.startsWith("../"))
        return raw;
    try {
        const parsed = new URL(raw);
        const allowed = ["http:", "https:", "mailto:", "tel:"];
        return allowed.includes(parsed.protocol)
            ? parsed.toString()
            : undefined;
    } catch (_) {
        return undefined;
    }
}

function SafeAnchor({ children, href, title }) {
    return (
        <a
            href={markdownURL(href)}
            title={title}
            target="_blank"
            rel="noopener noreferrer"
        >
            {children}
        </a>
    );
}

function ScrollableTable({ children, ...props }) {
    return (
        <div style={{ overflowX: "auto" }}>
            <table {...props}>{children}</table>
        </div>
    );
}

function TaskListCheckbox({ type, checked }) {
    if (type !== "checkbox") return null;
    return (
        <input type="checkbox" checked={Boolean(checked)} readOnly disabled />
    );
}

const MARKDOWN_OPTIONS = {
    overrides: {
        ...Object.fromEntries(
            MARKDOWN_BLOCKED_TAGS.map((tag) => [
                tag,
                { component: BlockedElement },
            ]),
        ),
        a: { component: SafeAnchor },
        table: { component: ScrollableTable },
        input: { component: TaskListCheckbox },
    },
    forceBlock: true,
    disableParsingRawHTML: true,
};

// CappedBlock keeps large argument and result payloads inside the row that
// owns them. The complete text remains available through the scroll area.
function CappedBlock({ children, maxHeight = 180, preStyle }) {
    return (
        <pre
            style={{
                maxHeight,
                overflow: "auto",
                background: "var(--bg-primary)",
                border: "1px solid var(--border-weak)",
                borderRadius: 8,
                padding: "8px 10px",
                margin: 0,
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 11.5,
                lineHeight: 1.6,
                color: "var(--fg1)",
                whiteSpace: "pre-wrap",
                wordBreak: "break-word",
                ...(preStyle || {}),
            }}
        >
            {children}
        </pre>
    );
}

function toolCallArgPreview(input) {
    if (!input) return "";
    if (typeof input === "string")
        return input.length > 140 ? input.slice(0, 140) + "…" : input;
    for (const key of [
        "command",
        "file_path",
        "path",
        "pattern",
        "query",
        "url",
        "cmd",
        "name",
    ]) {
        if (input[key] != null && input[key] !== "")
            return String(input[key]).replace(/\s+/g, " ");
    }
    try {
        const value = JSON.stringify(input);
        return value.length > 140 ? value.slice(0, 140) + "…" : value;
    } catch (_) {
        return "";
    }
}

function firstUserText(step) {
    for (const message of step.input || []) {
        if (message.role !== "user") continue;
        for (const part of message.parts || []) {
            if (partKind(part) === "text" && (part.text || "").trim())
                return part.text;
        }
    }
    return "";
}

function leadingAssistantText(step) {
    for (const message of step.output || []) {
        for (const part of message.parts || []) {
            if (partKind(part) === "text") {
                const text = (part.text || "").trim();
                if (text) return text;
            }
        }
    }
    return "";
}

const tsMs = (value) => {
    const time = value ? new Date(value).getTime() : 0;
    return Number.isFinite(time) ? time : 0;
};

function agentShort(name) {
    if (!name) return "main";
    const slash = name.indexOf("/");
    return slash === -1 ? name : name.slice(slash + 1);
}

function isSubagent(name) {
    return !!name && name.indexOf("/") !== -1;
}

function agentColor(name) {
    const short = agentShort(name);
    if (!isSubagent(name)) return "var(--brand-orange)";
    if (short.includes("explore")) return "var(--viz-blue)";
    if (short.includes("general")) return "var(--viz-purple)";
    if (short.includes("fork")) return "var(--viz-green)";
    return "var(--viz-yellow)";
}

function buildSubagentForest(gens) {
    const byId = new Map((gens || []).map((gen) => [gen.generation_id, gen]));
    const inConvParent = (gen) => {
        const parentID = (gen.parent_generation_ids || [])[0];
        return parentID && byId.has(parentID) ? byId.get(parentID) : null;
    };
    const runRootId = (gen) => {
        let current = gen;
        const seen = new Set();
        for (;;) {
            if (seen.has(current.generation_id)) return current.generation_id;
            seen.add(current.generation_id);
            const parent = inConvParent(current);
            if (
                parent &&
                (parent.agent_name || "") === (current.agent_name || "")
            ) {
                current = parent;
                continue;
            }
            return current.generation_id;
        }
    };
    const runs = new Map();
    for (const gen of gens || []) {
        const rootID = runRootId(gen);
        let run = runs.get(rootID);
        if (!run) {
            run = {
                id: rootID,
                agent: (byId.get(rootID) || gen).agent_name,
                gens: [],
            };
            runs.set(rootID, run);
        }
        run.gens.push(gen);
    }
    const spawnedBy = new Map();
    const topRuns = [];
    for (const run of runs.values()) {
        run.gens.sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
        run.start = Math.min(
            ...run.gens.map((gen) => tsMs(gen.started_at) || Infinity),
        );
        run.end = Math.max(
            ...run.gens.map(
                (gen) => tsMs(gen.completed_at) || tsMs(gen.started_at) || 0,
            ),
        );
        run.totalTokens = run.gens.reduce(
            (sum, gen) => sum + (gen.total_tokens || 0),
            0,
        );
        run.hasError = run.gens.some((gen) => gen.call_error);
        const parent = inConvParent(byId.get(run.id));
        if (parent && (parent.agent_name || "") !== (run.agent || "")) {
            if (!spawnedBy.has(parent.generation_id))
                spawnedBy.set(parent.generation_id, []);
            spawnedBy.get(parent.generation_id).push(run);
        } else {
            topRuns.push(run);
        }
    }
    for (const children of spawnedBy.values())
        children.sort((a, b) => a.start - b.start);
    topRuns.sort((a, b) => a.start - b.start);
    const depthSeen = new Set();
    const setDepth = (run, depth) => {
        if (depthSeen.has(run.id)) return;
        depthSeen.add(run.id);
        run.depth = depth;
        run.gens.forEach((gen) =>
            (spawnedBy.get(gen.generation_id) || []).forEach((child) =>
                setDepth(child, depth + 1),
            ),
        );
    };
    topRuns.forEach((run) => setDepth(run, 0));
    return { runs, spawnedBy, topRuns, byId };
}

function flattenForest(forest) {
    const out = [];
    const seen = new Set();
    const visit = (run, depth, path) => {
        if (seen.has(run.id)) return;
        seen.add(run.id);
        const runPath = path.concat(run.id);
        run.gens.forEach((gen, index) => {
            out.push({
                gen,
                depth,
                run,
                runPath,
                isRunStart: index === 0 && depth > 0,
            });
            (forest.spawnedBy.get(gen.generation_id) || []).forEach((child) =>
                visit(child, depth + 1, runPath),
            );
        });
    };
    forest.topRuns.forEach((run) => visit(run, 0, []));
    forest.runs.forEach((run) => {
        if (!seen.has(run.id)) visit(run, 0, []);
    });
    out.forEach((row, index) => {
        row.n = index + 1;
    });
    return out;
}

function stepTokenWork(gen) {
    const buckets = (gen && gen.token_buckets) || {};
    const generated = (buckets.output || 0) + (buckets.reasoning || 0);
    const ingested = (buckets.fresh_input || 0) + (buckets.cache_write || 0);
    return { generated, ingested, work: generated + ingested };
}

function mergedSpan(intervals) {
    const sorted = intervals
        .filter((interval) => interval[1] > interval[0])
        .sort((a, b) => a[0] - b[0]);
    let total = 0;
    let currentStart = -1;
    let currentEnd = -1;
    for (const [start, end] of sorted) {
        if (start > currentEnd) {
            if (currentEnd > currentStart) total += currentEnd - currentStart;
            currentStart = start;
            currentEnd = end;
        } else {
            currentEnd = Math.max(currentEnd, end);
        }
    }
    if (currentEnd > currentStart) total += currentEnd - currentStart;
    return total;
}

function argumentBody(input) {
    if (input == null) return "";
    if (typeof input === "string") return input;
    try {
        return JSON.stringify(input, null, 2);
    } catch (_) {
        return String(input);
    }
}

function summarizeSubagentRun(run, forest, nextByID, consumedResults) {
    const calls = [];
    const errors = [];
    let returned = "";
    for (const gen of run.gens) {
        const blocks = generationTranscriptBlocks(
            gen,
            nextByID.get(gen.generation_id),
            [],
            consumedResults,
        );
        for (const block of blocks) {
            if (block.kind === "work") calls.push(...block.calls);
            if (block.kind === "error") errors.push(block);
            if (block.kind === "prose" && block.text.trim())
                returned = block.text.trim();
        }
    }
    const children = run.gens.flatMap(
        (gen) => forest.spawnedBy.get(gen.generation_id) || [],
    );
    return {
        id: run.id,
        agent: run.agent,
        gens: run.gens,
        calls,
        errors,
        task:
            firstUserText(run.gens[0]) ||
            leadingAssistantText(run.gens[0]) ||
            "Subagent run",
        returned,
        durationSec: Math.max(0, (run.end - run.start) / 1000),
        failedCount: calls.filter((call) => call.failed).length + errors.length,
        childCount: children.length,
    };
}

function generationTranscriptBlocks(
    gen,
    next,
    subruns,
    consumedResults = new Set(),
) {
    const blocks = [];
    let work = null;
    let sawReasoning = false;
    let reasoningIndex = 0;
    let callIndex = 0;
    // A generation times one model call, not each batch of tool calls it
    // emitted, so only its first work block takes the duration.
    let durationTaken = false;
    const closeWork = () => {
        work = null;
    };
    const ensureWork = () => {
        if (!work) {
            work = {
                kind: "work",
                id: `work-${gen.generation_id}-${blocks.length}`,
                genIds: [gen.generation_id],
                calls: [],
                subruns: [],
                durationSec: durationTaken
                    ? 0
                    : Math.max(0, Number(gen.duration_seconds) || 0),
            };
            durationTaken = true;
            blocks.push(work);
        }
        return work;
    };

    for (const message of gen.output || []) {
        for (const part of message.parts || []) {
            const kind = partKind(part);
            if (kind === "tool_call" && part.tool_call) {
                const call = part.tool_call;
                const result = resolveResult(gen, next, call, consumedResults);
                const row = {
                    key: `${gen.generation_id}:${callIndex++}`,
                    genId: gen.generation_id,
                    id: call.id || "",
                    name: call.name || "tool",
                    input: call.input_json == null ? null : call.input_json,
                    result,
                    failed: !!(result && result.is_error),
                };
                ensureWork().calls.push(row);
                continue;
            }
            if (kind === "tool_result") continue;
            closeWork();
            if (kind === "text" && (part.text || "").trim()) {
                blocks.push({
                    kind: "prose",
                    text: part.text,
                    genId: gen.generation_id,
                });
            } else if (kind === "thinking" && (part.thinking || "").trim()) {
                sawReasoning = true;
                blocks.push({
                    kind: "reasoning",
                    id: `${gen.generation_id}:reasoning:${reasoningIndex++}`,
                    text: part.thinking,
                    genId: gen.generation_id,
                    notRecorded: false,
                });
            }
        }
    }

    if (gen.call_error) {
        closeWork();
        blocks.push({
            kind: "error",
            id: `${gen.generation_id}:error`,
            text: gen.call_error,
            genId: gen.generation_id,
        });
    }
    if (gen.thinking_enabled && !sawReasoning) {
        blocks.unshift({
            kind: "reasoning",
            id: `${gen.generation_id}:reasoning:0`,
            text: "",
            genId: gen.generation_id,
            notRecorded: true,
        });
    }
    if (subruns.length > 0) {
        let owner = null;
        for (let index = blocks.length - 1; index >= 0; index--) {
            if (blocks[index].kind === "work") {
                owner = blocks[index];
                break;
            }
        }
        if (!owner) owner = ensureWork();
        owner.subruns.push(...subruns);
    }
    return blocks;
}

function appendTranscriptBlocks(target, incoming) {
    for (const block of incoming) {
        const previous = target[target.length - 1];
        if (previous && previous.kind === "work" && block.kind === "work") {
            previous.calls.push(...block.calls);
            previous.subruns.push(...block.subruns);
            previous.genIds.push(...block.genIds);
            previous.durationSec += block.durationSec;
            continue;
        }
        target.push(block);
    }
}

function buildTranscript(steps) {
    const ordered = (steps || [])
        .slice()
        .sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
    const forest = buildSubagentForest(ordered);
    const rows = flattenForest(forest);
    const nextByID = new Map();
    for (const run of forest.runs.values()) {
        run.gens.forEach((gen, index) =>
            nextByID.set(gen.generation_id, run.gens[index + 1] || null),
        );
    }
    const previousTopLevelByAgent = new Map();
    for (const gen of ordered) {
        if (isSubagent(gen.agent_name)) continue;
        const agent = gen.agent_name || "";
        const previous = previousTopLevelByAgent.get(agent);
        if (previous) nextByID.set(previous.generation_id, gen);
        previousTopLevelByAgent.set(agent, gen);
    }
    const consumedResults = new Set();
    const subrunSummary = new Map();
    const summarizeRun = (run) => {
        if (subrunSummary.has(run.id)) return subrunSummary.get(run.id);
        const summary = summarizeSubagentRun(
            run,
            forest,
            nextByID,
            consumedResults,
        );
        subrunSummary.set(run.id, summary);
        const children = run.gens.flatMap(
            (gen) => forest.spawnedBy.get(gen.generation_id) || [],
        );
        summary.failedCount += children.reduce(
            (sum, child) => sum + summarizeRun(child).failedCount,
            0,
        );
        return summary;
    };
    for (const run of forest.runs.values()) {
        if (run.depth > 0) summarizeRun(run);
    }

    const turns = [];
    let current = null;
    const startTurn = (gen, userText) => ({
        index: turns.length + 1,
        startGenId: gen.generation_id,
        userText,
        userStartedAt: gen.started_at,
        gens: [],
        genIds: [],
        blocks: [],
    });
    const finishTurn = (turn) => {
        if (!turn || turn.gens.length === 0) return;
        const starts = turn.gens
            .map((gen) => tsMs(gen.started_at))
            .filter(Boolean);
        const ends = turn.gens
            .map((gen) => tsMs(gen.completed_at) || tsMs(gen.started_at))
            .filter(Boolean);
        turn.start = starts.length ? Math.min(...starts) : 0;
        turn.end = ends.length ? Math.max(...ends) : turn.start;
        turn.durationSec = Math.max(0, (turn.end - turn.start) / 1000);
        turn.toolCount = turn.gens.reduce(
            (sum, gen) => sum + outputCalls(gen).length,
            0,
        );
        turn.failedCount = turn.blocks.reduce((sum, block) => {
            if (block.kind === "error") return sum + 1;
            if (block.kind !== "work") return sum;
            const failedCalls = block.calls.filter(
                (call) => call.failed,
            ).length;
            const failedSubruns = block.subruns.reduce(
                (subrunSum, run) => subrunSum + run.failedCount,
                0,
            );
            return sum + failedCalls + failedSubruns;
        }, 0);
        turn.subrunCount = new Set(
            turn.gens
                .filter((gen) => isSubagent(gen.agent_name))
                .map((gen) => {
                    const row = rows.find(
                        (candidate) =>
                            candidate.gen.generation_id === gen.generation_id,
                    );
                    return row ? row.run.id : gen.generation_id;
                }),
        ).size;
        turns.push(turn);
    };

    for (const row of rows) {
        const userText = row.depth === 0 ? firstUserText(row.gen) : "";
        if (row.depth === 0 && userText) {
            finishTurn(current);
            current = startTurn(row.gen, userText);
        } else if (!current) {
            current = startTurn(row.gen, userText);
        }
        current.gens.push(row.gen);
        current.genIds.push(row.gen.generation_id);
        if (row.depth === 0) {
            const subruns = (forest.spawnedBy.get(row.gen.generation_id) || [])
                .map((run) => subrunSummary.get(run.id))
                .filter(Boolean);
            appendTranscriptBlocks(
                current.blocks,
                generationTranscriptBlocks(
                    row.gen,
                    nextByID.get(row.gen.generation_id),
                    subruns,
                    consumedResults,
                ),
            );
        }
    }
    finishTurn(current);
    return turns;
}

function buildTranscriptMetrics(steps, turns) {
    const intervals = [];
    let startMs = Infinity;
    let endMs = -Infinity;
    const histogram = new Map();
    let totalTokens = 0;
    let usageAvailable = false;
    for (const gen of steps || []) {
        const start = tsMs(gen.started_at);
        const end = tsMs(gen.completed_at) || start;
        if (start) startMs = Math.min(startMs, start);
        if (end) endMs = Math.max(endMs, end);
        if (start > 0 && end > start) intervals.push([start, end]);
        const tokens = Number(gen.total_tokens) || 0;
        totalTokens += tokens;
        if (tokens > 0) usageAvailable = true;
        for (const call of outputCalls(gen))
            histogram.set(
                call.name || "tool",
                (histogram.get(call.name || "tool") || 0) + 1,
            );
    }
    if (!Number.isFinite(startMs)) startMs = 0;
    if (!Number.isFinite(endMs)) endMs = startMs;
    const wallMs = Math.max(0, endMs - startMs);
    const workingMs = mergedSpan(intervals);
    let longestIdle = null;
    const chronological = (turns || [])
        .filter((turn) => turn.start > 0 && turn.end >= turn.start)
        .sort((a, b) => a.start - b.start);
    for (let index = 1; index < chronological.length; index++) {
        const durationMs = Math.max(
            0,
            chronological[index].start - chronological[index - 1].end,
        );
        if (
            durationMs > 0 &&
            (!longestIdle || durationMs > longestIdle.durationMs)
        ) {
            longestIdle = { durationMs, turn: chronological[index] };
        }
    }
    return {
        startMs,
        endMs,
        wallMs,
        workingMs,
        idleMs: Math.max(0, wallMs - workingMs),
        longestIdle,
        usageAvailable,
        totalTokens,
        toolHistogram: [...histogram.entries()]
            .map(([name, count]) => ({ name, count }))
            .sort((a, b) => b.count - a.count || a.name.localeCompare(b.name)),
    };
}

// promptLine is the first line of what was asked, for the sticky header.
// The agent's answer is far longer than the prompt, so by the time you are
// reading it the question has scrolled away; the header carries it along.
function promptLine(turn) {
    const prompt = splitPreamble(turn.userText).prompt || "";
    const line =
        prompt
            .split("\n")
            .map((part) => part.trim())
            .find(Boolean) || "";
    return line.length > 120 ? line.slice(0, 120) + "…" : line;
}

// The turn rule and the speaker labels stack into two sticky lines under
// the session bar, so which turn you are in, what you asked, and which
// block you are reading stay on screen however long the answer runs. Both
// rows are fixed-height, because the second one's offset is the first
// one's height.
const TURN_RULE_H = 32;
const SPEAKER_H = 26;

function TurnRule({ turn, slowest, first }) {
    const asked = promptLine(turn);
    return (
        // A sticky box can only stick inside its own parent, so this row is a
        // direct child of the turn section rather than a wrapper's child, and
        // the gap above it is its own margin.
        <div
            style={{
                position: "sticky",
                top: HEADER_H + 46,
                zIndex: 3,
                height: TURN_RULE_H,
                marginTop: first ? 0 : 20,
                display: "flex",
                alignItems: "center",
                gap: 10,
                background: "var(--bg-canvas)",
            }}
        >
            <span
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                    letterSpacing: "0.1em",
                    color: "var(--fg3)",
                    whiteSpace: "nowrap",
                }}
            >
                TURN {turn.index}
            </span>
            {asked && (
                <span
                    title={asked}
                    style={{
                        minWidth: 0,
                        flex: "0 1 auto",
                        color: "var(--fg2)",
                        fontSize: 12,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                    }}
                >
                    {asked}
                </span>
            )}
            <span
                style={{ flex: 1, height: 1, background: "var(--border-weak)" }}
            />
            <span
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                    color: slowest ? "var(--warning-text)" : "var(--fg3)",
                    whiteSpace: "nowrap",
                }}
            >
                {formatDuration(turn.durationSec)}
                {slowest
                    ? " · slowest turn"
                    : ` · ${turn.toolCount} ${turn.toolCount === 1 ? "tool" : "tools"}`}
            </span>
            {turn.failedCount > 0 && (
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 5,
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        color: "var(--error-text)",
                        whiteSpace: "nowrap",
                    }}
                >
                    <span
                        style={{
                            width: 6,
                            height: 6,
                            borderRadius: "50%",
                            background: "var(--error-text)",
                        }}
                    />
                    {turn.failedCount} failed
                </span>
            )}
        </div>
    );
}

// Who is speaking is carried by one device: a labelled rule in that
// speaker's colour, pinned under the turn rule for as long as the block is
// on screen. The two colours are the brand orange and the timeline's blue,
// so neither collides with green for a passing call, red for a failure, or
// amber for the slowest turn.
const SPEAKERS = {
    you: {
        label: "YOU",
        colour: "var(--brand-orange-text)",
        rule: "rgba(255,138,77,",
    },
    agent: {
        label: "AGENT",
        colour: "var(--viz-blue)",
        rule: "rgba(87,148,242,",
    },
};

function SpeakerLabel({ speaker, suffix }) {
    const { label, colour, rule } = SPEAKERS[speaker];
    return (
        <div
            style={{
                position: "sticky",
                top: HEADER_H + 46 + TURN_RULE_H,
                zIndex: 2,
                height: SPEAKER_H,
                display: "flex",
                alignItems: "center",
                gap: 10,
                background: "var(--bg-canvas)",
            }}
        >
            <span
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 10,
                    letterSpacing: "0.1em",
                    color: colour,
                    whiteSpace: "nowrap",
                }}
            >
                {label}
            </span>
            {suffix && (
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        color: "var(--fg3)",
                        whiteSpace: "nowrap",
                    }}
                >
                    {suffix}
                </span>
            )}
            <span
                style={{
                    flex: 1,
                    height: 1,
                    background: `linear-gradient(to right, ${rule}0.45), ${rule}0.06) 55%, transparent)`,
                }}
            />
        </div>
    );
}

function PreambleChip({ text }) {
    const [open, setOpen] = useState(false);
    const tags = scanPreambleBlocks(String(text || "")).tags;
    const lineCount = text ? text.replace(/\s+$/, "").split("\n").length : 0;
    return (
        <div style={{ marginBottom: 10 }}>
            <button
                type="button"
                onClick={() => setOpen((value) => !value)}
                aria-expanded={open}
                style={{
                    maxWidth: "100%",
                    display: "flex",
                    alignItems: "center",
                    gap: 6,
                    padding: "3px 9px",
                    background: "rgba(204,204,220,0.04)",
                    border: "1px solid var(--border-weak)",
                    borderRadius: 2,
                    color: "var(--fg3)",
                    cursor: "pointer",
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                }}
            >
                <Icon name={open ? "chevron" : "cright"} size={10} />
                <span
                    style={{
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                    }}
                >
                    preamble{tags.length > 0 ? ` · ${tags.join(", ")}` : ""} ·{" "}
                    {lineCount} {lineCount === 1 ? "line" : "lines"}
                </span>
            </button>
            {open && (
                <pre
                    style={{
                        maxHeight: 220,
                        overflow: "auto",
                        margin: "6px 0 0",
                        padding: "8px 10px",
                        background: "var(--bg-primary)",
                        border: "1px solid var(--border-weak)",
                        borderRadius: 8,
                        color: "var(--fg2)",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11.5,
                        lineHeight: 1.6,
                        whiteSpace: "pre-wrap",
                        wordBreak: "break-word",
                    }}
                >
                    {text}
                </pre>
            )}
        </div>
    );
}

function UserMessage({ turn }) {
    const split = splitPreamble(turn.userText);
    const lineCount = split.prompt ? split.prompt.split("\n").length : 0;
    const clamp = lineCount > 6;
    const [full, setFull] = useState(false);
    return (
        <div style={{ paddingBottom: 4 }}>
            <SpeakerLabel
                speaker="you"
                suffix={formatTime(turn.userStartedAt)}
            />
            <div style={{ height: 4 }} />
            {split.preamble && <PreambleChip text={split.preamble} />}
            {split.prompt ? (
                <div
                    style={{
                        fontSize: 16,
                        lineHeight: 1.55,
                        color: "var(--fg-max)",
                        whiteSpace: "pre-wrap",
                        wordBreak: "break-word",
                        ...(clamp && !full
                            ? {
                                  display: "-webkit-box",
                                  WebkitLineClamp: 4,
                                  WebkitBoxOrient: "vertical",
                                  overflow: "hidden",
                              }
                            : {}),
                    }}
                >
                    {split.prompt}
                </div>
            ) : (
                <div
                    style={{
                        color: "var(--fg3)",
                        fontSize: 12,
                        fontFamily: "var(--fontFamilyMonospace)",
                    }}
                >
                    No user message content captured.
                </div>
            )}
            {clamp && (
                <button
                    type="button"
                    onClick={() => setFull((value) => !value)}
                    style={{
                        marginTop: 7,
                        padding: 0,
                        background: "transparent",
                        border: "none",
                        color: "var(--fg3)",
                        cursor: "pointer",
                        fontSize: 11.5,
                    }}
                    onMouseEnter={(event) =>
                        (event.currentTarget.style.color = "var(--fg1)")
                    }
                    onMouseLeave={(event) =>
                        (event.currentTarget.style.color = "var(--fg3)")
                    }
                >
                    {full
                        ? "Show less"
                        : `Show full message · ${lineCount} lines`}
                </button>
            )}
        </div>
    );
}

function ProseBlock({ text }) {
    // The vendored script sets the global. If it failed to load, show the
    // text rather than nothing: reading the transcript is the whole point.
    const Markdown =
        typeof window !== "undefined" && window.MarkdownToJSX
            ? window.MarkdownToJSX.Markdown
            : null;
    if (!Markdown) {
        return (
            <div
                style={{
                    fontSize: 14.5,
                    lineHeight: 1.68,
                    color: "var(--fg1)",
                    whiteSpace: "pre-wrap",
                    wordBreak: "break-word",
                    marginBottom: 12,
                }}
            >
                {text}
            </div>
        );
    }
    return (
        <div className="sigil-md" style={{ marginBottom: 12 }}>
            <Markdown options={MARKDOWN_OPTIONS}>{String(text || "")}</Markdown>
        </div>
    );
}

function ReasoningBlock({ block, open, onToggle }) {
    if (block.notRecorded) {
        return (
            <div
                title="The model reasoned on this call, but the host did not persist the reasoning text."
                style={{
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 6,
                    paddingBottom: 10,
                    color: "var(--fg3)",
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                }}
            >
                <Icon name="info" size={10} />
                reasoning, not recorded
            </div>
        );
    }
    return (
        <div>
            <button
                type="button"
                onClick={onToggle}
                aria-expanded={open}
                style={{
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 6,
                    padding: "0 0 10px",
                    background: "transparent",
                    border: "none",
                    color: "var(--viz-blue)",
                    cursor: "pointer",
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11,
                }}
            >
                <Icon name={open ? "chevron" : "cright"} size={10} />
                Reasoning
            </button>
            {open && (
                <div
                    style={{
                        borderLeft: "2px solid var(--viz-blue)",
                        padding: "2px 0 2px 12px",
                        marginBottom: 12,
                        color: "var(--fg2)",
                        fontSize: 13,
                        lineHeight: 1.6,
                        fontStyle: "italic",
                        whiteSpace: "pre-wrap",
                        wordBreak: "break-word",
                    }}
                >
                    {block.text}
                </div>
            )}
        </div>
    );
}

function CallErrorBlock({ block, compact = false }) {
    return (
        <div
            role="alert"
            style={{
                marginBottom: compact ? 0 : 12,
                padding: compact ? "9px 10px" : "10px 12px",
                border: "1px solid var(--error-border)",
                borderRadius: 2,
                background: "rgba(209,14,92,0.05)",
                color: "var(--error-text)",
            }}
        >
            <div
                style={{
                    marginBottom: 5,
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 10,
                    letterSpacing: "0.08em",
                }}
            >
                MODEL CALL FAILED
            </div>
            <div
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: compact ? 10.5 : 11.5,
                    lineHeight: 1.6,
                    whiteSpace: "pre-wrap",
                    wordBreak: "break-word",
                }}
            >
                {block.text}
            </div>
        </div>
    );
}

function SuccessGlyph() {
    return (
        <svg
            width={12}
            height={12}
            viewBox="0 0 24 24"
            fill="none"
            stroke="var(--viz-green)"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
        >
            <path d="m5 13 4 4L19 7" />
        </svg>
    );
}

function FailureGlyph() {
    return (
        <svg
            width={12}
            height={12}
            viewBox="0 0 24 24"
            fill="none"
            stroke="var(--error-text)"
            strokeWidth="2"
            strokeLinecap="round"
            strokeLinejoin="round"
        >
            <path d="M6 6l12 12M18 6 6 18" />
        </svg>
    );
}

function ToolRow({ call, compact = false }) {
    const body = resultBody(call.result);
    const resolved = !!call.result;
    const lineCount = resolved ? (body ? body.split("\n").length : 0) : 0;
    const [open, setOpen] = useState(call.failed);
    useEffect(() => {
        if (call.failed) setOpen(true);
    }, [call.failed]);
    const args = argumentBody(call.input);
    const statusLabel = !resolved
        ? ""
        : call.failed
          ? `error · ${lineCount} ${lineCount === 1 ? "line" : "lines"}`
          : `${lineCount} ${lineCount === 1 ? "line" : "lines"}`;
    const rowHeight = compact ? 28 : 30;
    return (
        <React.Fragment>
            <button
                type="button"
                onClick={() => setOpen((value) => !value)}
                aria-expanded={open}
                style={{
                    width: "100%",
                    display: "grid",
                    gridTemplateColumns: compact
                        ? "14px 84px 1fr auto"
                        : "14px 92px 1fr auto",
                    alignItems: "center",
                    gap: compact ? 8 : 10,
                    padding: compact ? "0 10px" : "0 12px",
                    height: rowHeight,
                    cursor: "pointer",
                    border: "none",
                    borderBottom: "1px solid var(--border-weak)",
                    borderLeft: call.failed
                        ? "2px solid var(--error-main)"
                        : "2px solid transparent",
                    background: call.failed
                        ? "rgba(209,14,92,0.05)"
                        : "transparent",
                    textAlign: "left",
                    fontFamily: "var(--fontFamilyMonospace)",
                }}
                onMouseEnter={(event) =>
                    (event.currentTarget.style.background = call.failed
                        ? "rgba(209,14,92,0.09)"
                        : "rgba(204,204,220,0.03)")
                }
                onMouseLeave={(event) =>
                    (event.currentTarget.style.background = call.failed
                        ? "rgba(209,14,92,0.05)"
                        : "transparent")
                }
            >
                <span
                    style={{
                        display: "flex",
                        alignItems: "center",
                        justifyContent: "center",
                    }}
                >
                    {resolved ? (
                        call.failed ? (
                            <FailureGlyph />
                        ) : (
                            <SuccessGlyph />
                        )
                    ) : null}
                </span>
                <span
                    style={{
                        color: "var(--fg1)",
                        fontSize: compact ? 11 : 11.5,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                    }}
                >
                    {call.name}
                </span>
                <span
                    style={{
                        color: call.failed ? "var(--error-text)" : "var(--fg2)",
                        fontSize: compact ? 11 : 11.5,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                        minWidth: 0,
                    }}
                >
                    {toolCallArgPreview(call.input)}
                </span>
                <span
                    style={{
                        color: call.failed ? "var(--error-text)" : "var(--fg3)",
                        fontSize: compact ? 10.5 : 11,
                        whiteSpace: "nowrap",
                    }}
                >
                    {statusLabel}
                </span>
            </button>
            {open && (
                <div
                    style={{
                        background: "var(--bg-canvas)",
                        padding: compact
                            ? "8px 10px 10px 32px"
                            : "10px 12px 12px 36px",
                        borderBottom: "1px solid var(--border-weak)",
                    }}
                >
                    {args && (
                        <div style={{ marginBottom: resolved ? 10 : 0 }}>
                            <div
                                style={{
                                    marginBottom: 6,
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 10,
                                    letterSpacing: "0.08em",
                                    color: "var(--fg3)",
                                }}
                            >
                                ARGUMENTS
                            </div>
                            <CappedBlock>{args}</CappedBlock>
                        </div>
                    )}
                    {resolved && (
                        <div>
                            <div
                                style={{
                                    marginBottom: 6,
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 10,
                                    letterSpacing: "0.08em",
                                    color: call.failed
                                        ? "var(--error-text)"
                                        : "var(--fg3)",
                                }}
                            >
                                RESULT · {lineCount}{" "}
                                {lineCount === 1 ? "line" : "lines"}
                            </div>
                            <CappedBlock
                                preStyle={
                                    call.failed
                                        ? { color: "var(--error-text)" }
                                        : undefined
                                }
                            >
                                {body}
                            </CappedBlock>
                        </div>
                    )}
                </div>
            )}
        </React.Fragment>
    );
}

function SubagentRun({ run }) {
    const [open, setOpen] = useState(false);
    const color = agentColor(run.agent);
    const summary = String(run.task || "").replace(/\s+/g, " ");
    return (
        <div style={{ borderBottom: "1px solid var(--border-weak)" }}>
            <button
                type="button"
                onClick={() => setOpen((value) => !value)}
                aria-expanded={open}
                style={{
                    width: "100%",
                    height: 30,
                    padding: "0 12px",
                    display: "flex",
                    alignItems: "center",
                    gap: 8,
                    border: "none",
                    borderLeft: `2px solid ${color}`,
                    background: "rgba(87,148,242,0.04)",
                    cursor: "pointer",
                    textAlign: "left",
                }}
            >
                <Icon
                    name={open ? "chevron" : "cright"}
                    size={11}
                    style={{ color: "var(--fg3)" }}
                />
                <svg
                    width={11}
                    height={11}
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke={color}
                    strokeWidth="2"
                >
                    <circle cx="12" cy="8" r="4" />
                    <path d="M4 21a8 8 0 0 1 16 0" />
                </svg>
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11.5,
                        color: "var(--fg1)",
                        whiteSpace: "nowrap",
                    }}
                >
                    {agentShort(run.agent)}
                </span>
                <span
                    style={{
                        minWidth: 0,
                        flex: 1,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                        color: "var(--fg3)",
                        fontSize: 11.5,
                    }}
                >
                    subagent · {summary}
                </span>
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        color: run.failedCount
                            ? "var(--error-text)"
                            : "var(--fg3)",
                        whiteSpace: "nowrap",
                    }}
                >
                    {run.calls.length} tools · {formatDuration(run.durationSec)}
                </span>
            </button>
            {open && (
                <div
                    style={{
                        borderLeft: `2px solid ${color}`,
                        paddingLeft: 18,
                        background: "var(--bg-canvas)",
                    }}
                >
                    {run.calls.map((call) => (
                        <ToolRow key={call.key} call={call} compact />
                    ))}
                    {run.errors.map((error) => (
                        <CallErrorBlock key={error.id} block={error} compact />
                    ))}
                    {run.childCount > 0 && (
                        <div
                            style={{
                                padding: "8px 10px",
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 10.5,
                            }}
                        >
                            {run.childCount} nested{" "}
                            {run.childCount === 1 ? "run" : "runs"}
                        </div>
                    )}
                    {run.returned && (
                        <div
                            style={{
                                padding: "9px 10px 11px",
                                color: "var(--fg2)",
                                fontSize: 12.5,
                                lineHeight: 1.5,
                                whiteSpace: "pre-wrap",
                                wordBreak: "break-word",
                            }}
                        >
                            {run.returned}
                        </div>
                    )}
                </div>
            )}
        </div>
    );
}

function WorkGroup({ block, open, onToggle }) {
    const failedCount = block.calls.filter((call) => call.failed).length;
    const large = block.calls.length > 40;
    const subCount = block.subruns.length;
    return (
        <div
            style={{
                display: "flex",
                flexDirection: "column",
                gap: 4,
                marginBottom: 14,
            }}
        >
            <button
                type="button"
                onClick={onToggle}
                aria-expanded={open}
                style={{
                    width: "100%",
                    display: "flex",
                    alignItems: "center",
                    gap: 8,
                    padding: "0 0 4px",
                    border: "none",
                    background: "transparent",
                    cursor: "pointer",
                    textAlign: "left",
                }}
            >
                <Icon
                    name={open ? "chevron" : "cright"}
                    size={11}
                    style={{ color: "var(--fg3)" }}
                />
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        color: "var(--fg2)",
                        whiteSpace: "nowrap",
                    }}
                >
                    {block.calls.length}{" "}
                    {block.calls.length === 1 ? "tool" : "tools"}
                    {large && !open ? " - collapsed" : ""}
                </span>
                {block.durationSec > 0 && (
                    <span
                        style={{
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 11,
                            color: "var(--fg3)",
                            whiteSpace: "nowrap",
                        }}
                    >
                        · {formatDuration(block.durationSec)}
                    </span>
                )}
                {failedCount > 0 && (
                    <span
                        style={{
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 11,
                            color: "var(--error-text)",
                            whiteSpace: "nowrap",
                        }}
                    >
                        · {failedCount} failed
                    </span>
                )}
                {subCount > 0 && (
                    <span
                        style={{
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 11,
                            color: "var(--fg3)",
                            whiteSpace: "nowrap",
                        }}
                    >
                        · {subCount} {subCount === 1 ? "subagent" : "subagents"}
                    </span>
                )}
                <span
                    style={{
                        flex: 1,
                        height: 1,
                        background: "var(--border-weak)",
                    }}
                />
            </button>
            {open && (
                <div
                    style={{
                        border: "1px solid var(--border-weak)",
                        borderRadius: 8,
                        background: "var(--bg-canvas)",
                        overflow: "hidden",
                    }}
                >
                    {block.calls.map((call) => (
                        <ToolRow key={call.key} call={call} />
                    ))}
                    {block.subruns.map((run) => (
                        <SubagentRun key={run.id} run={run} />
                    ))}
                </div>
            )}
        </div>
    );
}

function AgentBlock({
    turn,
    openGroups,
    toggleGroup,
    openReasoning,
    toggleReasoning,
}) {
    return (
        <div style={{ marginTop: 10, paddingBottom: 2 }}>
            <SpeakerLabel speaker="agent" />
            <div style={{ height: 6 }} />
            {turn.blocks.length === 0 && (
                <div
                    style={{
                        color: "var(--fg3)",
                        fontSize: 12,
                        fontFamily: "var(--fontFamilyMonospace)",
                        paddingBottom: 10,
                    }}
                >
                    No message content captured. Re-run with{" "}
                    <code style={{ color: "var(--fg1)" }}>
                        SIGIL_CONTENT_CAPTURE_MODE=full
                    </code>{" "}
                    to record prompts and responses.
                </div>
            )}
            {turn.blocks.map((block, index) => {
                if (block.kind === "prose")
                    return (
                        <ProseBlock
                            key={`p${block.genId}-${index}`}
                            text={block.text}
                        />
                    );
                if (block.kind === "reasoning") {
                    return (
                        <ReasoningBlock
                            key={block.id}
                            block={block}
                            open={openReasoning.has(block.id)}
                            onToggle={() => toggleReasoning(block.id)}
                        />
                    );
                }
                if (block.kind === "error")
                    return <CallErrorBlock key={block.id} block={block} />;
                return (
                    <WorkGroup
                        key={block.id}
                        block={block}
                        open={openGroups.has(block.id)}
                        onToggle={() => toggleGroup(block.id)}
                    />
                );
            })}
        </div>
    );
}

// TurnPager is the thread's left rail: two dimmed controls that step to
// the turn before or after the one on screen. It sticks under the session
// bar so it stays reachable in a session with many turns.
function TurnPager({ turns, activeTurn, onJump }) {
    const index = Math.max(
        0,
        turns.findIndex((turn) => turn.index === activeTurn),
    );
    const steps = [
        {
            label: "previous",
            rotate: 180,
            turn: index > 0 ? turns[index - 1] : null,
        },
        {
            label: "next",
            rotate: 0,
            turn: index < turns.length - 1 ? turns[index + 1] : null,
        },
    ];
    return (
        <div
            style={{
                position: "sticky",
                top: HEADER_H + 46 + 28,
                display: "flex",
                flexDirection: "column",
                gap: 8,
            }}
        >
            {steps.map((step) => (
                <button
                    type="button"
                    key={step.label}
                    disabled={!step.turn}
                    onClick={() => step.turn && onJump(step.turn.startGenId)}
                    title={
                        step.turn
                            ? `Go to turn ${step.turn.index}`
                            : `No ${step.label} turn`
                    }
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 4,
                        padding: "3px 0",
                        background: "transparent",
                        border: "none",
                        textAlign: "left",
                        color: "var(--fg3)",
                        opacity: step.turn ? 1 : 0.35,
                        cursor: step.turn ? "pointer" : "default",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 10.5,
                    }}
                    onMouseEnter={(event) => {
                        if (step.turn)
                            event.currentTarget.style.color = "var(--fg1)";
                    }}
                    onMouseLeave={(event) => {
                        event.currentTarget.style.color = "var(--fg3)";
                    }}
                >
                    <Icon
                        name="chevron"
                        size={12}
                        style={{
                            transform: `rotate(${step.rotate}deg)`,
                            flex: "none",
                        }}
                    />
                    {step.label}
                </button>
            ))}
        </div>
    );
}

function ConversationThread({ turns, jumpRef }) {
    const groups = useMemo(
        () =>
            turns.flatMap((turn) =>
                turn.blocks.filter((block) => block.kind === "work"),
            ),
        [turns],
    );
    const [openGroups, setOpenGroups] = useState(
        () =>
            new Set(
                groups
                    .filter((group) => group.calls.length <= 40)
                    .map((group) => group.id),
            ),
    );
    const [openReasoning, setOpenReasoning] = useState(() => new Set());
    const [activeTurn, setActiveTurn] = useState(1);
    const [flashID, setFlashID] = useState(null);
    const [hashGenID, setHashGenID] = useState(generationIDFromHash);
    const turnRefs = useRef({});
    const flashTimer = useRef(null);
    const knownGroups = useRef(new Set(groups.map((group) => group.id)));
    const handledHash = useRef("");
    const pendingHash = useRef("");

    useEffect(() => {
        const valid = new Set(groups.map((group) => group.id));
        const previousKnown = knownGroups.current;
        setOpenGroups((previous) => {
            const next = new Set([...previous].filter((id) => valid.has(id)));
            groups.forEach((group) => {
                if (group.calls.length <= 40 && !previousKnown.has(group.id))
                    next.add(group.id);
            });
            return next;
        });
        knownGroups.current = valid;
    }, [groups]);
    useEffect(
        () => () => {
            if (flashTimer.current) clearTimeout(flashTimer.current);
        },
        [],
    );

    const turnByGen = useMemo(() => {
        const map = new Map();
        turns.forEach((turn) => turn.genIds.forEach((id) => map.set(id, turn)));
        return map;
    }, [turns]);
    const groupByGen = useMemo(() => {
        const map = new Map();
        groups.forEach((group) => {
            group.genIds.forEach((id) => map.set(id, group.id));
            group.subruns.forEach((run) =>
                run.gens.forEach((gen) => map.set(gen.generation_id, group.id)),
            );
        });
        return map;
    }, [groups]);

    const jumpTo = useCallback(
        (id) => {
            const turn = turnByGen.get(id);
            if (!turn) return;
            const groupID = groupByGen.get(id);
            if (groupID)
                setOpenGroups((previous) =>
                    previous.has(groupID)
                        ? previous
                        : new Set(previous).add(groupID),
                );
            setActiveTurn(turn.index);
            setFlashID(null);
            requestAnimationFrame(() =>
                requestAnimationFrame(() => {
                    const node = turnRefs.current[turn.startGenId];
                    if (node) {
                        const top =
                            window.scrollY +
                            node.getBoundingClientRect().top -
                            (HEADER_H + 46 + 24);
                        window.scrollTo({
                            top: Math.max(0, top),
                            behavior: "smooth",
                        });
                    }
                    setFlashID(turn.startGenId);
                }),
            );
            if (flashTimer.current) clearTimeout(flashTimer.current);
            flashTimer.current = setTimeout(() => setFlashID(null), 1400);
        },
        [groupByGen, turnByGen],
    );
    useEffect(() => {
        jumpRef.current = jumpTo;
        return () => {
            if (jumpRef.current === jumpTo) jumpRef.current = () => {};
        };
    }, [jumpRef, jumpTo]);

    useEffect(() => {
        const syncHash = () => {
            handledHash.current = "";
            pendingHash.current = "";
            setHashGenID(generationIDFromHash());
        };
        window.addEventListener("hashchange", syncHash);
        window.addEventListener("popstate", syncHash);
        return () => {
            window.removeEventListener("hashchange", syncHash);
            window.removeEventListener("popstate", syncHash);
        };
    }, []);
    useEffect(() => {
        if (!hashGenID) {
            handledHash.current = "";
            return;
        }
        if (
            handledHash.current === hashGenID ||
            pendingHash.current === hashGenID ||
            !turnByGen.has(hashGenID)
        )
            return;
        pendingHash.current = hashGenID;
        setTimeout(() => {
            pendingHash.current = "";
            handledHash.current = hashGenID;
            jumpTo(hashGenID);
        }, 0);
    }, [hashGenID, turnByGen, jumpTo]);

    // Track the turn under the session bar so the rail's previous/next
    // move relative to what the reader is looking at, not to the last jump.
    useEffect(() => {
        if (turns.length === 0) return undefined;
        let frame = 0;
        const measure = () => {
            frame = 0;
            let current = turns[0].index;
            for (const turn of turns) {
                const node = turnRefs.current[turn.startGenId];
                if (!node) continue;
                if (node.getBoundingClientRect().top - (HEADER_H + 46 + 28) > 0)
                    break;
                current = turn.index;
            }
            setActiveTurn(current);
        };
        const onScroll = () => {
            if (!frame) frame = requestAnimationFrame(measure);
        };
        measure();
        window.addEventListener("scroll", onScroll, { passive: true });
        window.addEventListener("resize", onScroll);
        return () => {
            if (frame) cancelAnimationFrame(frame);
            window.removeEventListener("scroll", onScroll);
            window.removeEventListener("resize", onScroll);
        };
    }, [turns]);

    const slowest = turns.reduce(
        (current, turn) =>
            !current || turn.durationSec > current.durationSec ? turn : current,
        null,
    );
    const toggleGroup = (id) =>
        setOpenGroups((previous) => {
            const next = new Set(previous);
            next.has(id) ? next.delete(id) : next.add(id);
            return next;
        });
    const toggleReasoning = (id) =>
        setOpenReasoning((previous) => {
            const next = new Set(previous);
            next.has(id) ? next.delete(id) : next.add(id);
            return next;
        });

    if (turns.length === 0) {
        return (
            <div
                style={{
                    color: "var(--fg2)",
                    fontSize: 12,
                    fontFamily: "var(--fontFamilyMonospace)",
                }}
            >
                No turns recorded.
            </div>
        );
    }
    return (
        <div
            style={{
                display: "flex",
                gap: 16,
                maxWidth: 968,
                margin: "0 auto",
            }}
        >
            <div style={{ width: 72, flex: "none" }}>
                <TurnPager
                    turns={turns}
                    activeTurn={activeTurn}
                    onJump={jumpTo}
                />
            </div>
            <div
                style={{
                    flex: 1,
                    minWidth: 0,
                    maxWidth: 880,
                    display: "flex",
                    flexDirection: "column",
                    gap: 8,
                }}
            >
                {turns.map((turn, index) => (
                    <section
                        key={turn.startGenId}
                        ref={(node) => {
                            turnRefs.current[turn.startGenId] = node;
                        }}
                        className={
                            flashID === turn.startGenId
                                ? "sigil-step-flash"
                                : undefined
                        }
                        style={{
                            borderRadius: 8,
                            outline:
                                activeTurn === turn.index
                                    ? "1px solid transparent"
                                    : "none",
                        }}
                    >
                        <TurnRule
                            turn={turn}
                            slowest={
                                !!slowest &&
                                slowest.startGenId === turn.startGenId
                            }
                            first={index === 0}
                        />
                        <UserMessage turn={turn} />
                        <AgentBlock
                            turn={turn}
                            openGroups={openGroups}
                            toggleGroup={toggleGroup}
                            openReasoning={openReasoning}
                            toggleReasoning={toggleReasoning}
                        />
                    </section>
                ))}
            </div>
        </div>
    );
}

function PanelSection({ title, children, aside }) {
    return (
        <section>
            <div
                style={{
                    display: "flex",
                    alignItems: "center",
                    marginBottom: 10,
                }}
            >
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 10,
                        letterSpacing: "0.1em",
                        color: "var(--fg3)",
                    }}
                >
                    {title}
                </span>
                {aside && (
                    <span
                        style={{
                            marginLeft: "auto",
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 10.5,
                            color: "var(--fg3)",
                        }}
                    >
                        {aside}
                    </span>
                )}
            </div>
            {children}
        </section>
    );
}

function TimelinePanel({ turns, metrics, onJump }) {
    const span = Math.max(1, metrics.endMs - metrics.startMs);
    const slowest = turns.reduce(
        (current, turn) =>
            !current || turn.durationSec > current.durationSec ? turn : current,
        null,
    );
    return (
        <PanelSection
            title="TIMELINE"
            aside={`${formatDuration(metrics.idleMs / 1000)} idle`}
        >
            <div style={{ display: "flex", flexDirection: "column", gap: 7 }}>
                {turns.map((turn) => {
                    const left = Math.max(
                        0,
                        ((turn.start - metrics.startMs) / span) * 100,
                    );
                    const width = Math.max(
                        1.5,
                        ((turn.end - turn.start) / span) * 100,
                    );
                    const isSlow =
                        slowest && slowest.startGenId === turn.startGenId;
                    return (
                        <button
                            type="button"
                            key={turn.startGenId}
                            onClick={() => onJump(turn.startGenId)}
                            title={`Jump to turn ${turn.index}`}
                            style={{
                                display: "grid",
                                gridTemplateColumns: "40px 1fr 52px",
                                alignItems: "center",
                                gap: 8,
                                width: "100%",
                                padding: 0,
                                border: "none",
                                background: "transparent",
                                cursor: "pointer",
                                textAlign: "left",
                            }}
                        >
                            <span
                                style={{
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 10.5,
                                    color: "var(--fg3)",
                                }}
                            >
                                T{turn.index}
                            </span>
                            <span
                                style={{
                                    position: "relative",
                                    height: 8,
                                    background: "rgba(204,204,220,0.05)",
                                    overflow: "hidden",
                                }}
                            >
                                <span
                                    style={{
                                        position: "absolute",
                                        left: `${left}%`,
                                        width: `${Math.min(100 - left, width)}%`,
                                        top: 0,
                                        bottom: 0,
                                        minWidth: 2,
                                        background: isSlow
                                            ? "var(--warning-main)"
                                            : "var(--viz-blue)",
                                    }}
                                />
                                {turn.failedCount > 0 && (
                                    <span
                                        style={{
                                            position: "absolute",
                                            left: `${Math.min(98, left + Math.max(0, width - Math.min(width, 18)))}%`,
                                            width: `${Math.min(width, 18)}%`,
                                            top: 0,
                                            bottom: 0,
                                            minWidth: 2,
                                            background: "var(--error-main)",
                                        }}
                                    />
                                )}
                            </span>
                            <span
                                style={{
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 10.5,
                                    color: "var(--fg2)",
                                    textAlign: "right",
                                }}
                            >
                                {formatDuration(turn.durationSec)}
                            </span>
                        </button>
                    );
                })}
            </div>
            <div
                style={{
                    margin: "10px 60px 0 48px",
                    height: 13,
                    borderTop: "1px solid var(--border-weak)",
                    display: "flex",
                    justifyContent: "space-between",
                    paddingTop: 3,
                }}
            >
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 9.5,
                        color: "var(--fg3)",
                    }}
                >
                    {metrics.startMs
                        ? formatTime(new Date(metrics.startMs).toISOString())
                        : NO_VALUE}
                </span>
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 9.5,
                        color: "var(--fg3)",
                    }}
                >
                    {metrics.endMs
                        ? formatTime(new Date(metrics.endMs).toISOString())
                        : NO_VALUE}
                </span>
            </div>
            <div
                style={{
                    display: "flex",
                    gap: 11,
                    marginTop: 7,
                    color: "var(--fg3)",
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 10,
                }}
            >
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 4,
                    }}
                >
                    <span
                        style={{
                            width: 6,
                            height: 6,
                            background: "var(--viz-blue)",
                        }}
                    />
                    turn
                </span>
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 4,
                    }}
                >
                    <span
                        style={{
                            width: 6,
                            height: 6,
                            background: "var(--warning-main)",
                        }}
                    />
                    slowest
                </span>
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 4,
                    }}
                >
                    <span
                        style={{
                            width: 6,
                            height: 6,
                            background: "var(--error-main)",
                        }}
                    />
                    failed
                </span>
            </div>
        </PanelSection>
    );
}

function WorthALook({ steps, turns, metrics, onJump }) {
    if (turns.length === 0) return null;
    const slowest = turns.reduce(
        (current, turn) =>
            !current || turn.durationSec > current.durationSec ? turn : current,
        null,
    );
    const failed = turns.filter((turn) => turn.failedCount > 0);
    const turnByGen = new Map();
    turns.forEach((turn) =>
        turn.genIds.forEach((id) => turnByGen.set(id, turn)),
    );
    const pickStep = (value) =>
        (steps || []).reduce(
            (current, gen) =>
                !current || value(gen) > value(current) ? gen : current,
            null,
        );
    const generated = metrics.usageAvailable
        ? pickStep((gen) => stepTokenWork(gen).generated)
        : null;
    const read = metrics.usageAvailable
        ? pickStep((gen) => stepTokenWork(gen).ingested)
        : null;
    const entries = [];
    if (failed.length > 0)
        entries.push({
            label:
                failed.reduce((sum, turn) => sum + turn.failedCount, 0) === 1
                    ? "failure"
                    : "failures",
            turn: failed[0],
            tone: "error",
        });
    if (slowest)
        entries.push({ label: "slowest turn", turn: slowest, tone: "warning" });
    if (metrics.longestIdle && metrics.longestIdle.durationMs > 0)
        entries.push({
            label: `longest idle ${formatDuration(metrics.longestIdle.durationMs / 1000)}`,
            turn: metrics.longestIdle.turn,
            tone: "neutral",
        });
    if (generated && turnByGen.has(generated.generation_id))
        entries.push({
            label: `most generated ${formatTokens(stepTokenWork(generated).generated)}`,
            turn: turnByGen.get(generated.generation_id),
            tone: "neutral",
        });
    if (read && turnByGen.has(read.generation_id))
        entries.push({
            label: `most read ${formatTokens(stepTokenWork(read).ingested)}`,
            turn: turnByGen.get(read.generation_id),
            tone: "neutral",
        });
    return (
        <PanelSection title="WORTH A LOOK">
            <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
                {entries.map((entry, index) => {
                    const border =
                        entry.tone === "error"
                            ? "var(--error-border)"
                            : entry.tone === "warning"
                              ? "var(--warning-border)"
                              : "var(--border-medium)";
                    const dot =
                        entry.tone === "error"
                            ? "var(--error-main)"
                            : entry.tone === "warning"
                              ? "var(--warning-main)"
                              : "var(--fg3)";
                    const hover =
                        entry.tone === "error"
                            ? "rgba(209,14,92,0.08)"
                            : entry.tone === "warning"
                              ? "rgba(245,183,61,0.08)"
                              : "var(--action-hover)";
                    return (
                        <button
                            type="button"
                            key={`${entry.label}-${index}`}
                            onClick={() => onJump(entry.turn.startGenId)}
                            style={{
                                width: "100%",
                                display: "flex",
                                alignItems: "center",
                                gap: 8,
                                padding: "7px 10px",
                                background: "transparent",
                                border: `1px solid ${border}`,
                                borderRadius: 2,
                                cursor: "pointer",
                                textAlign: "left",
                            }}
                            onMouseEnter={(event) =>
                                (event.currentTarget.style.background = hover)
                            }
                            onMouseLeave={(event) =>
                                (event.currentTarget.style.background =
                                    "transparent")
                            }
                        >
                            <span
                                style={{
                                    width: 6,
                                    height: 6,
                                    borderRadius: "50%",
                                    background: dot,
                                    flexShrink: 0,
                                }}
                            />
                            <span
                                style={{
                                    color: "var(--fg1)",
                                    fontSize: 12,
                                    overflow: "hidden",
                                    textOverflow: "ellipsis",
                                    whiteSpace: "nowrap",
                                }}
                            >
                                {entry.label}
                            </span>
                            <span
                                style={{
                                    marginLeft: "auto",
                                    color: "var(--fg3)",
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 11,
                                }}
                            >
                                T{entry.turn.index}
                            </span>
                        </button>
                    );
                })}
            </div>
        </PanelSection>
    );
}

function missingUsageNotice(host) {
    return host
        ? `No token usage was recorded for this ${host} session, so token counts and cost are unavailable.`
        : "No token usage was recorded for this session, so token counts and cost are unavailable.";
}

function MetricsPanel({ conv, steps, turns, metrics, onJump }) {
    const maxToolCount =
        metrics.toolHistogram.reduce(
            (max, item) => Math.max(max, item.count),
            0,
        ) || 1;
    const host = agentHosts(conv.agents)[0] || "";
    const stats = [
        { value: formatDuration(metrics.wallMs / 1000), label: "elapsed" },
        {
            value: formatDuration(metrics.workingMs / 1000),
            label: "agent working",
        },
        {
            value: String(steps.length),
            label: steps.length === 1 ? "call" : "calls",
        },
        {
            value: metrics.usageAvailable
                ? formatTokens(metrics.totalTokens)
                : "—",
            label: "tokens",
            muted: !metrics.usageAvailable,
        },
    ];
    return (
        <aside
            style={{
                width: 320,
                flex: "none",
                position: "sticky",
                top: HEADER_H + 46,
                alignSelf: "flex-start",
                maxHeight: `calc(100vh - ${HEADER_H + 46}px)`,
                overflow: "auto",
                borderLeft: "1px solid var(--border-weak)",
                background: "var(--bg-primary)",
                padding: "20px 18px 40px",
                display: "flex",
                flexDirection: "column",
                gap: 22,
            }}
        >
            <PanelSection title="SESSION">
                <div
                    style={{
                        display: "grid",
                        gridTemplateColumns: "1fr 1fr",
                        gap: 12,
                    }}
                >
                    {stats.map((stat) => (
                        <div key={stat.label}>
                            <div
                                style={{
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 18,
                                    color: stat.muted
                                        ? "var(--fg3)"
                                        : "var(--fg-max)",
                                }}
                            >
                                {stat.value}
                            </div>
                            <div
                                style={{
                                    marginTop: 2,
                                    color: "var(--fg3)",
                                    fontSize: 11,
                                }}
                            >
                                {stat.label}
                            </div>
                        </div>
                    ))}
                </div>
            </PanelSection>
            {!metrics.usageAvailable && (
                <div
                    style={{
                        display: "flex",
                        alignItems: "flex-start",
                        gap: 9,
                        padding: "10px 12px",
                        border: "1px solid var(--border-weak)",
                        borderRadius: 8,
                        background: "rgba(204,204,220,0.03)",
                        color: "var(--fg2)",
                        fontSize: 12,
                        lineHeight: 1.5,
                    }}
                >
                    <Icon
                        name="info"
                        size={14}
                        style={{ color: "var(--fg3)", marginTop: 2 }}
                    />
                    <span>{missingUsageNotice(host)}</span>
                </div>
            )}
            <TimelinePanel turns={turns} metrics={metrics} onJump={onJump} />
            <WorthALook
                steps={steps}
                turns={turns}
                metrics={metrics}
                onJump={onJump}
            />
            <PanelSection title="TOOLS USED">
                {metrics.toolHistogram.length === 0 ? (
                    <div style={{ color: "var(--fg3)", fontSize: 11.5 }}>
                        No tools recorded.
                    </div>
                ) : (
                    <div
                        style={{
                            display: "flex",
                            flexDirection: "column",
                            gap: 8,
                        }}
                    >
                        {metrics.toolHistogram.map((tool) => (
                            <div
                                key={tool.name}
                                style={{
                                    display: "flex",
                                    alignItems: "center",
                                    gap: 8,
                                }}
                            >
                                <span
                                    title={tool.name}
                                    style={{
                                        width: 88,
                                        overflow: "hidden",
                                        textOverflow: "ellipsis",
                                        whiteSpace: "nowrap",
                                        color: "var(--fg1)",
                                        fontFamily:
                                            "var(--fontFamilyMonospace)",
                                        fontSize: 11.5,
                                    }}
                                >
                                    {tool.name}
                                </span>
                                <span
                                    style={{
                                        position: "relative",
                                        flex: 1,
                                        height: 6,
                                        background: "rgba(204,204,220,0.06)",
                                    }}
                                >
                                    <span
                                        style={{
                                            position: "absolute",
                                            inset: 0,
                                            right: "auto",
                                            width: `${(tool.count / maxToolCount) * 100}%`,
                                            background:
                                                "rgba(204,204,220,0.30)",
                                        }}
                                    />
                                </span>
                                <span
                                    style={{
                                        width: 18,
                                        textAlign: "right",
                                        color: "var(--fg3)",
                                        fontFamily:
                                            "var(--fontFamilyMonospace)",
                                        fontSize: 11.5,
                                    }}
                                >
                                    {tool.count}
                                </span>
                            </div>
                        ))}
                    </div>
                )}
            </PanelSection>
        </aside>
    );
}

function TraceDetailView({ conv, detail, loading, error, onBack }) {
    const steps = detail ? detail.generations : [];
    const turns = useMemo(() => buildTranscript(steps), [detail]);
    const metrics = useMemo(
        () => buildTranscriptMetrics(steps, turns),
        [detail, turns],
    );
    const jumpRef = useRef(() => {});
    const wallSec =
        metrics.wallMs > 0
            ? metrics.wallMs / 1000
            : durationBetweenSeconds(conv.started_at, conv.last_activity);
    const buttonStyle = {
        display: "inline-flex",
        alignItems: "center",
        gap: 6,
        padding: "0 11px",
        height: 28,
        background: "transparent",
        color: "var(--fg1)",
        border: "1px solid var(--border-medium)",
        borderRadius: 2,
        fontSize: 12,
        cursor: "pointer",
        fontFamily: "var(--fontFamily)",
        fontWeight: 500,
        whiteSpace: "nowrap",
    };
    const onExport = () => {
        const blob = new Blob(
            [JSON.stringify({ ...conv, generations: steps }, null, 2)],
            { type: "application/json" },
        );
        const url = URL.createObjectURL(blob);
        const anchor = document.createElement("a");
        anchor.href = url;
        anchor.download = `${conv.id}.json`;
        document.body.appendChild(anchor);
        anchor.click();
        anchor.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
    };
    return (
        <div
            style={{
                display: "flex",
                flexDirection: "column",
                flex: 1,
                minHeight: 0,
                background: "var(--bg-canvas)",
            }}
        >
            <div
                style={{
                    display: "flex",
                    alignItems: "center",
                    gap: 10,
                    height: 46,
                    padding: "0 16px",
                    borderBottom: "1px solid var(--border-weak)",
                    background: "var(--bg-primary)",
                    position: "sticky",
                    top: HEADER_H,
                    zIndex: 4,
                    minWidth: 0,
                }}
            >
                <a
                    href="/"
                    onClick={(event) => {
                        if (!isPlainLeftClick(event)) return;
                        event.preventDefault();
                        onBack();
                    }}
                    style={{
                        fontSize: 13,
                        color: "var(--fg2)",
                        textDecoration: "none",
                        whiteSpace: "nowrap",
                        flexShrink: 0,
                        cursor: "pointer",
                    }}
                    onMouseEnter={(event) =>
                        (event.currentTarget.style.color = "var(--fg-max)")
                    }
                    onMouseLeave={(event) =>
                        (event.currentTarget.style.color = "var(--fg2)")
                    }
                >
                    Sessions
                </a>
                <Icon
                    name="cright"
                    size={11}
                    style={{ color: "var(--fg3)", flexShrink: 0 }}
                />
                <span
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 13,
                        color: "var(--fg-max)",
                        whiteSpace: "nowrap",
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        minWidth: 0,
                    }}
                >
                    {conv.title || conv.id}
                </span>
                {(conv.models || []).map((model) => (
                    <ModelPill key={model} name={model} />
                ))}
                <span
                    style={{
                        fontSize: 12,
                        color: "var(--fg3)",
                        whiteSpace: "nowrap",
                    }}
                >
                    {turns.length} {turns.length === 1 ? "turn" : "turns"} ·{" "}
                    {formatDuration(wallSec)}
                </span>
                <span style={{ flex: 1 }} />
                <button
                    type="button"
                    title="Download trace as JSON"
                    onClick={onExport}
                    style={buttonStyle}
                    onMouseEnter={(event) =>
                        (event.currentTarget.style.background =
                            "var(--action-hover)")
                    }
                    onMouseLeave={(event) =>
                        (event.currentTarget.style.background = "transparent")
                    }
                >
                    <Icon name="download" size={12} />
                    Export JSON
                </button>
            </div>
            <div style={{ display: "flex", alignItems: "flex-start", gap: 0 }}>
                <main
                    style={{ flex: 1, minWidth: 0, padding: "28px 32px 96px" }}
                >
                    {error && (
                        <Notice kind="error" title="Failed to load session">
                            {error}
                        </Notice>
                    )}
                    {!error && loading && (
                        <div
                            style={{
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 12,
                            }}
                        >
                            Loading…
                        </div>
                    )}
                    {!error && !loading && detail && (
                        <ConversationThread turns={turns} jumpRef={jumpRef} />
                    )}
                </main>
                {!error && !loading && detail && (
                    <MetricsPanel
                        conv={conv}
                        steps={steps}
                        turns={turns}
                        metrics={metrics}
                        onJump={(id) => jumpRef.current(id)}
                    />
                )}
            </div>
        </div>
    );
}

// ============================================================
// Settings — edits config.env via the daemon's /api/v1/config endpoints
// ============================================================

// Mono renders inline code in the monospace face used across the viewer.
function Mono({ children }) {
    return (
        <code
            style={{
                fontFamily: "var(--fontFamilyMonospace)",
                color: "var(--fg2)",
            }}
        >
            {children}
        </code>
    );
}

// sameSettings is a field-wise deep compare for dirty tracking. Tag order
// is significant (it survives a round-trip), so it is compared positionally.
function sameSettings(a, b) {
    if (!a || !b) return a === b;
    if (
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
    if (a.tokenSet !== b.tokenSet || a.otlpHeadersSet !== b.otlpHeadersSet)
        return false;
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
        if (at[i].key !== bt[i].key || at[i].value !== bt[i].value)
            return false;
    }
    return true;
}

// cloneSettings deep-copies so the form and the saved snapshot never share
// the tags array (editing one must not mutate the other).
function cloneSettings(s) {
    return { ...s, tags: (s.tags || []).map((t) => ({ ...t })) };
}

// pendingEdits returns the fields the form has changed and a write does not
// own, so a one-click control can leave them pending instead of committing
// them. null means there is nothing pending.
function pendingEdits(form, saved, owned) {
    if (!form || !saved) return null;
    const out = {};
    Object.keys(form).forEach((key) => {
        if (owned && Object.prototype.hasOwnProperty.call(owned, key)) return;
        const same =
            key === "tags"
                ? JSON.stringify(form.tags || []) ===
                  JSON.stringify(saved.tags || [])
                : form[key] === saved[key];
        if (!same) out[key] = form[key];
    });
    return Object.keys(out).length > 0 ? out : null;
}

// GUARD_CONTENT_NOTE is the one carve-out in the capture-mode promise: a
// chained guard check relays the content being evaluated. See
// handleHookEvaluate in internal/local/server.go.
const GUARD_CONTENT_NOTE =
    "Guards are on: tool calls, and the conversation an agent runs a preflight check on, are sent to Cloud for evaluation regardless of the capture mode.";

// forwardBannerMeta turns the daemon's reported forwarding status into the
// pill, accent, and sentence forwardChipMeta builds the header chip and the
// settings hero from. The saved toggle is deliberately not an input:
// config.env and the daemon's own environment can disagree, and only the
// daemon knows what it would actually send.
function forwardBannerMeta(st) {
    if (!st) {
        return {
            accent: "warning",
            pill: "Unknown",
            line: "Couldn't read the daemon's forwarding status.",
        };
    }
    if (!st.enabled) {
        if (st.reason)
            return {
                accent: "warning",
                pill: "Paused",
                line: `Forwarding is on but paused: ${st.reason}`,
            };
        // The hook leg is one of the legs st.enabled sums, so nothing is
        // relayed here.
        return {
            accent: "success",
            pill: "Off",
            line: "Cloud forwarding is off. Nothing from local sessions leaves this machine.",
        };
    }
    // Guard disclosures hold whatever else the status says, so every branch
    // below that reports forwarding as on appends them. Failures are kept per
    // leg: a failing generations or OTLP leg must not hide that guard checks
    // are still shipping content, nor swallow the unchecked-allow count.
    const disclosures = [];
    if (st.hooks) disclosures.push(GUARD_CONTENT_NOTE);
    if (st.hookFailOpens > 0)
        disclosures.push(
            st.hookFailOpens === 1
                ? "1 guard check ran without a Cloud verdict and was allowed."
                : `${st.hookFailOpens} guard checks ran without a Cloud verdict and were allowed.`,
        );
    const failures = st.failures || [];
    const failure = failures[0];
    if (failure) {
        // Name the other failing legs instead of letting the most recent one
        // stand for all of them.
        const others = [...new Set(failures.map((f) => f.label))].filter(
            (l) => l !== failure.label,
        );
        const also =
            others.length > 0 ? ` (also failing: ${others.join(", ")})` : "";
        return {
            accent: "error",
            pill: "Failing",
            line: [
                `Forwarding is on but the last attempts failed. ${failure.label}: ${failure.detail}${also}`,
                ...disclosures,
            ].join(" "),
        };
    }
    // An unrecognised mode must not read as the narrower one: a future mode
    // could forward more, not less.
    if (st.mode !== "full" && st.mode !== "metadata_only") {
        return {
            accent: "warning",
            pill: "On",
            line: [
                `Forwarding is on in a mode this viewer does not know (${st.mode || "unset"}).`,
                ...disclosures,
            ].join(" "),
        };
    }
    // With guards chained, only reasoning text and media are still local:
    // the guard request carries tool calls, and for a preflight check the
    // prompts and responses too, so those cannot be listed as local here.
    const metadataLine = st.hooks
        ? "Session capture forwards usage and session metadata only. Reasoning text and attached media stay local."
        : "Only usage and session metadata is forwarded. Prompts, responses, reasoning text, tool inputs and results, and attached media stay local.";
    const parts = [
        st.mode === "full"
            ? "Full session content is forwarded to your organization's Grafana Cloud."
            : metadataLine,
    ];
    if (!st.generations && st.reason)
        parts.push(`Generations are paused: ${st.reason}`);
    if (!st.otlp) parts.push("Traces and metrics are not forwarded.");
    parts.push(...disclosures);
    return {
        // A metadata_only forward with guards chained still ships content, so
        // it does not get the calm accent or the reassuring pill.
        accent: st.mode === "full" || st.hooks ? "warning" : "info",
        pill:
            st.mode === "full"
                ? "Full content"
                : st.hooks
                  ? "Metadata + guard content"
                  : "Metadata only",
        line: parts.join(" "),
    };
}

// cloudConfigured reports whether a Grafana Cloud connection is saved. Any
// one of the three is enough: forwardDisabledReason accepts an endpoint
// without credentials for a local collector.
function cloudConfigured(settings) {
    return !!(
        settings &&
        (settings.endpoint || settings.tenantId || settings.tokenSet)
    );
}

// forwardChipMeta maps the daemon's posture onto the header chip. It is not
// forwardBannerMeta's pill: the chip says Local where forwardBannerMeta says
// Off, Full where it says Full content, and it separates "no connection
// saved" from "saved, forwarding off", which the status alone cannot express
// (enabled is false for both, see resolveForwardConfig in
// internal/local/forward.go). color and border are whole var() references
// rather than an accent name, because the unconfigured state pairs --fg2
// with --border-medium and there is no --fg2-border.
function forwardChipMeta(config) {
    const st = config ? config.forwardStatus : null;
    const meta = forwardBannerMeta(st);
    const tone = (accent) => ({
        color: `var(--${accent}-text)`,
        border: `var(--${accent}-border)`,
    });
    if (!st)
        return {
            kicker: "Mode",
            value: "Unknown",
            line: meta.line,
            ...tone("warning"),
        };
    if (!st.enabled && !st.reason) {
        if (!cloudConfigured(config.settings)) {
            return {
                kicker: "Mode",
                value: "Local",
                color: "var(--fg2)",
                border: "var(--border-medium)",
                line: "Nothing from local sessions leaves this machine. No Grafana Cloud connection is configured, so every session stays in the local store.",
            };
        }
        return {
            kicker: "Mode",
            value: "Local",
            line: meta.line,
            ...tone(meta.accent),
        };
    }
    // Failing, Paused, Metadata only, Metadata + guard content and the
    // unrecognised-mode "On" keep forwardBannerMeta's pill and accent.
    return {
        kicker: "Cloud forwarding",
        value: meta.pill === "Full content" ? "Full" : meta.pill,
        line: meta.line,
        ...tone(meta.accent),
    };
}

// ForwardModeChip states what the daemon would send to Cloud, on every
// view. The daemon is shared, so this is machine-wide policy for every
// later --local session, not a property of the sessions on screen.
//
// It is read-only: changing the posture means a full config.env write,
// which the Cloud settings tab owns, so the chip navigates there instead.
// The tooltip is disclosure text and holds nothing interactive, which is
// why hover and focus are enough to open it.
function ForwardModeChip({ config, onOpenSettings }) {
    const [open, setOpen] = useState(false);
    const meta = forwardChipMeta(config);
    // The chip names itself "Mode Local", which does not say what that means.
    // The disclosure sentence is the point of the chip, so aria-describedby
    // ties it to the button: focus opens the tooltip, and a screen reader
    // then reads the sentence out.
    const tipID = "sigil-forward-chip-tip";
    return (
        <div
            style={{ position: "relative", flexShrink: 0 }}
            onMouseEnter={() => setOpen(true)}
            onMouseLeave={() => setOpen(false)}
        >
            <button
                type="button"
                onClick={() => onOpenSettings && onOpenSettings("cloud")}
                onFocus={() => setOpen(true)}
                onBlur={() => setOpen(false)}
                aria-label={`${meta.kicker}: ${meta.value}. Open the forwarding settings.`}
                aria-describedby={open ? tipID : undefined}
                onMouseEnter={(e) =>
                    (e.currentTarget.style.background = "var(--action-hover)")
                }
                onMouseLeave={(e) =>
                    (e.currentTarget.style.background = "rgba(24,27,31,0.78)")
                }
                style={{
                    display: "inline-flex",
                    alignItems: "center",
                    gap: 8,
                    height: 30,
                    padding: "0 9px 0 10px",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    background: "rgba(24,27,31,0.78)",
                    fontFamily: "var(--fontFamily)",
                    cursor: "pointer",
                    whiteSpace: "nowrap",
                }}
            >
                <Icon name="cloud" size={14} style={{ color: meta.color }} />
                <span
                    className="sigil-chip-kicker"
                    style={{
                        fontSize: 10.5,
                        textTransform: "uppercase",
                        letterSpacing: 0.6,
                        color: "var(--fg3)",
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
                <Icon name="cright" size={12} style={{ color: "var(--fg3)" }} />
            </button>
            {open && (
                <div
                    id={tipID}
                    role="note"
                    style={{
                        position: "absolute",
                        top: 38,
                        right: 0,
                        zIndex: 40,
                        width: 340,
                        padding: "12px 14px",
                        background: "var(--bg-secondary)",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        boxShadow: "var(--shadow-z2)",
                    }}
                >
                    <div
                        style={{
                            fontSize: 10.5,
                            textTransform: "uppercase",
                            letterSpacing: 0.6,
                            color: "var(--fg3)",
                            marginBottom: 6,
                        }}
                    >
                        What leaves this machine
                    </div>
                    <div
                        style={{
                            fontSize: 12.5,
                            lineHeight: 1.5,
                            color: "var(--fg2)",
                        }}
                    >
                        {meta.line}
                    </div>
                    <div
                        style={{
                            marginTop: 10,
                            paddingTop: 9,
                            borderTop: "1px solid var(--border-weak)",
                            fontSize: 11.5,
                            color: "var(--fg3)",
                        }}
                    >
                        Click to change the forwarding mode in Settings, Cloud
                        tab.
                    </div>
                </div>
            )}
        </div>
    );
}

// ============================================================
// History import — backfill sessions an agent recorded before
// agento11y was installed.
//
// Every agent name, label, and alias comes from
// GET /api/v1/history/agents, so registering an importer in Go makes it
// appear here with no change to this file.
// ============================================================

function importRunIsActive(run) {
    return !!run && (run.status === "pending" || run.status === "running");
}

// useHistoryImport owns the import state the banner and the Settings card
// both read: the registered agents, the per-agent offer, and the run in
// flight. Progress arrives on the shared SSE stream, so `run` here is
// updated by the App and passed back in.
function useHistoryImport(liveRun) {
    const [agents, setAgents] = useState([]);
    const [offers, setOffers] = useState([]);
    const [run, setRun] = useState(null);
    const [error, setError] = useState(null);

    const loadAgents = useCallback(() => {
        return fetch("/api/v1/history/agents")
            .then((r) =>
                r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`)),
            )
            .then((b) => setAgents((b && b.agents) || []))
            .catch(() => setAgents([]));
    }, []);

    const loadOffers = useCallback(() => {
        return fetch("/api/v1/history/offer")
            .then((r) =>
                r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`)),
            )
            .then((b) => setOffers((b && b.offers) || []))
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
        if (liveRun.status === "completed") loadOffers();
    }, [liveRun, loadOffers]);

    const start = useCallback((agent, body = {}) => {
        setError(null);
        return fetch("/api/v1/history:import", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ agent, ...body }),
        })
            .then((r) =>
                r.ok
                    ? r.json()
                    : r
                          .text()
                          .then((t) =>
                              Promise.reject(
                                  new Error(t.trim() || `HTTP ${r.status}`),
                              ),
                          ),
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
                              status: b.status || "pending",
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
        if (!run || !run.run_id) return Promise.resolve();
        return fetch(
            `/api/v1/history/runs/${encodeURIComponent(run.run_id)}:cancel`,
            {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: "{}",
            },
        ).catch(() => {});
    }, [run]);

    const dismiss = useCallback(
        (agent) => {
            setError(null);
            return (
                fetch("/api/v1/history/offer:dismiss", {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify(agent ? { agent } : {}),
                })
                    .then((r) =>
                        r.ok
                            ? r.json().catch(() => ({}))
                            : r
                                  .text()
                                  .then((t) =>
                                      Promise.reject(
                                          new Error(
                                              t.trim() || `HTTP ${r.status}`,
                                          ),
                                      ),
                                  ),
                    )
                    .then(() => loadOffers())
                    // The dismissal is written to a file, so it can fail. Saying so beats
                    // a banner that comes back with no explanation.
                    .catch((e) => {
                        setError(
                            `Could not dismiss the import offer: ${String(e.message || e)}`,
                        );
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

const IMPORT_HINT_KEY = "sigil.importHint.v1";

// ImportHintBanner points at Settings → History until the user dismisses
// it. Shown only when the store already has sessions and the first-use
// import was never used (the daemon still has an unanswered offer). An
// empty store gets the full import card instead. Dismiss lives in
// localStorage.
function ImportHintBanner({ onOpenSettings }) {
    const [dismissed, setDismissed] = useState(() => {
        try {
            return localStorage.getItem(IMPORT_HINT_KEY) === "1";
        } catch {
            return false;
        }
    });
    if (dismissed) return null;

    function dismiss() {
        try {
            localStorage.setItem(IMPORT_HINT_KEY, "1");
        } catch {
            /* quota / private mode */
        }
        setDismissed(true);
    }

    function openHistory(e) {
        if (!isPlainLeftClick(e)) return;
        e.preventDefault();
        if (onOpenSettings) onOpenSettings("history");
    }

    return (
        <div
            role="status"
            style={{
                display: "flex",
                alignItems: "center",
                gap: 10,
                height: 34,
                padding: "0 6px 0 0",
                marginBottom: 14,
                borderRadius: 2,
                border: "1px solid var(--border-medium)",
                borderLeft: "2px solid var(--info-text)",
                background: "rgba(24,27,31,0.78)",
                boxShadow: "inset 0 0 0 1px rgba(0,0,0,0.12)",
            }}
        >
            <span
                aria-hidden="true"
                style={{
                    width: 22,
                    height: 22,
                    marginLeft: 8,
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "center",
                    flex: "none",
                    borderRadius: 2,
                    background: "var(--info-transparent)",
                    color: "var(--info-text)",
                }}
            >
                <Icon name="clock" size={13} />
            </span>
            <span
                style={{
                    minWidth: 0,
                    fontSize: 12.5,
                    lineHeight: "34px",
                    color: "var(--fg2)",
                    whiteSpace: "nowrap",
                    overflow: "hidden",
                    textOverflow: "ellipsis",
                }}
            >
                Import previous sessions from your coding agents in{" "}
                <a
                    href={settingsPath("history")}
                    onClick={openHistory}
                    style={{
                        color: "var(--info-text)",
                        fontWeight: 500,
                        textDecoration: "none",
                    }}
                    onMouseEnter={(e) => {
                        e.currentTarget.style.textDecoration = "underline";
                        e.currentTarget.style.textUnderlineOffset = "3px";
                    }}
                    onMouseLeave={(e) => {
                        e.currentTarget.style.textDecoration = "none";
                    }}
                >
                    Settings → History
                </a>
            </span>
            <span style={{ flex: 1 }} />
            <button
                type="button"
                onClick={dismiss}
                aria-label="Dismiss import hint"
                title="Dismiss"
                style={{
                    ...iconBtn,
                    width: 22,
                    height: 22,
                    flex: "none",
                    color: "var(--fg3)",
                }}
                onMouseEnter={(e) => {
                    e.currentTarget.style.background = "var(--action-hover)";
                    e.currentTarget.style.color = "var(--fg1)";
                }}
                onMouseLeave={(e) => {
                    e.currentTarget.style.background = "transparent";
                    e.currentTarget.style.color = "var(--fg3)";
                }}
            >
                <Icon name="times" size={12} />
            </button>
        </div>
    );
}

// importSessionLabel says how far a run has got, in sessions. A run that has
// not finished discovery has no total to count against, so it says what it
// is doing rather than reporting "0 of 0".
function importSessionLabel(run) {
    const done = run.sessions || 0;
    const total = run.selected || 0;
    if (total > 0) return `${done} of ${total} sessions`;
    return importRunIsActive(run) ? "Scanning sessions…" : `${done} sessions`;
}

// ImportProgressBar shows the share of selected sessions a run has finished.
// The banner and the Settings card both draw it, so the two cannot disagree
// about what the bar measures. It moves when a session finishes, so a run
// over a few large sessions advances in visible steps rather than smoothly.
function ImportProgressBar({ done, total, style }) {
    const pct = total > 0 ? Math.min(100, Math.round((done / total) * 100)) : 0;
    return (
        <div
            style={{
                height: 4,
                borderRadius: 999,
                background: "var(--border-weak)",
                overflow: "hidden",
                ...style,
            }}
        >
            <div
                style={{
                    width: `${pct}%`,
                    height: "100%",
                    background: "var(--primary-main)",
                }}
            />
        </div>
    );
}

// HistoryImportProgress renders a run's progress as it arrives over SSE.
// Progress is counted in sessions: turn counts belong in the summary, where
// they cannot be mistaken for the number of sessions the run was given.
function HistoryImportProgress({ run, onCancel }) {
    const done = run.sessions || 0;
    const total = run.selected || 0;
    return (
        <div
            style={{
                display: "flex",
                alignItems: "center",
                gap: 12,
                padding: "10px 14px",
                marginBottom: 14,
                borderRadius: 2,
                border: "1px solid var(--info-border)",
                background: "var(--info-transparent)",
            }}
        >
            <Icon
                name="clock"
                size={15}
                style={{ color: "var(--info-text)", flex: "none" }}
            />
            <span
                style={{
                    fontSize: 10.5,
                    textTransform: "uppercase",
                    letterSpacing: 0.6,
                    color: "var(--fg3)",
                    flex: "none",
                }}
            >
                Importing {run.agent}
            </span>
            <span style={{ fontSize: 12.5, color: "var(--fg2)", flex: "none" }}>
                {importSessionLabel(run)}
                {run.failed ? ` · ${run.failed} failed turns` : ""}
            </span>
            <ImportProgressBar done={done} total={total} style={{ flex: 1 }} />
            <button
                type="button"
                onClick={onCancel}
                style={{
                    flex: "none",
                    background: "transparent",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    color: "var(--fg2)",
                    fontSize: 11.5,
                    fontFamily: "var(--fontFamily)",
                    padding: "3px 9px",
                    cursor: "pointer",
                }}
            >
                Cancel
            </button>
        </div>
    );
}

// captureForwardMode reports which of the two segmented values a
// CONTENT_CAPTURE_MODE forwards as. Everything that is not "full" is
// reduced to metadata by the forwarder, so the advanced modes
// (no_tool_content, full_with_metadata_spans) read as metadata_only.
function captureForwardMode(capture) {
    return capture === "full" ? "full" : "metadata_only";
}

// forwardLocalMode collapses the two persisted keys the forwarding control
// spans (LOCAL_FORWARD and CONTENT_CAPTURE_MODE) into the one segmented
// value the UI shows.
function forwardLocalMode(form) {
    if (!form.localForward) return "off";
    return captureForwardMode(form.capture);
}

// forwardLocalPatch returns the write the segmented value means, spanning
// both keys the control covers, so a caller can send it in the same PUT as
// the rest of the form. capture is only rewritten when the mode already set
// forwards differently, so an advanced mode set in config.env survives a
// round-trip through the toggle. null means the requested mode is the one
// already shown, which matters because the segmented control fires for the
// active option.
function forwardLocalPatch(form, mode) {
    if (mode === forwardLocalMode(form)) return null;
    if (mode === "off") return { localForward: false };
    const patch = { localForward: true };
    if (captureForwardMode(form.capture) !== mode) patch.capture = mode;
    return patch;
}

function Toggle({ checked, onChange }) {
    return (
        <button
            role="switch"
            aria-checked={checked}
            onClick={() => onChange(!checked)}
            style={{
                position: "relative",
                width: 38,
                height: 22,
                borderRadius: 9999,
                border: "none",
                cursor: "pointer",
                padding: 0,
                flexShrink: 0,
                background: checked
                    ? "var(--primary-main)"
                    : "rgba(204,204,220,0.25)",
                transition: "background .15s",
            }}
        >
            <span
                style={{
                    position: "absolute",
                    top: 3,
                    left: 3,
                    width: 16,
                    height: 16,
                    borderRadius: "50%",
                    background: "#fff",
                    transform: checked ? "translateX(16px)" : "translateX(0)",
                    transition: "transform .15s",
                }}
            />
        </button>
    );
}

function MonoInput({ value, onChange, placeholder, width, align, type }) {
    return (
        <input
            type={type || "text"}
            value={value}
            placeholder={placeholder}
            onChange={(e) => onChange(e.target.value)}
            onFocus={(e) =>
                (e.currentTarget.style.borderColor = "var(--primary-border)")
            }
            onBlur={(e) =>
                (e.currentTarget.style.borderColor = "var(--border-medium)")
            }
            style={{
                height: 32,
                width: width || "auto",
                background: "var(--bg-canvas)",
                border: "1px solid var(--border-medium)",
                borderRadius: 2,
                color: "var(--fg1)",
                padding: "0 10px",
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 12,
                textAlign: align || "left",
                outline: "none",
            }}
        />
    );
}

function PrimaryButton({ onClick, children }) {
    return (
        <button
            onClick={onClick}
            onMouseEnter={(e) => {
                e.currentTarget.style.background = "var(--primary-shade)";
                e.currentTarget.style.borderColor = "var(--primary-shade)";
            }}
            onMouseLeave={(e) => {
                e.currentTarget.style.background = "var(--primary-main)";
                e.currentTarget.style.borderColor = "var(--primary-main)";
            }}
            style={{
                height: 32,
                padding: "0 14px",
                background: "var(--primary-main)",
                border: "1px solid var(--primary-main)",
                color: "#fff",
                borderRadius: 2,
                fontSize: 13,
                fontWeight: 500,
                cursor: "pointer",
            }}
        >
            {children}
        </button>
    );
}

function GhostButton({ onClick, children }) {
    return (
        <button
            onClick={onClick}
            onMouseEnter={(e) =>
                (e.currentTarget.style.background = "var(--action-hover)")
            }
            onMouseLeave={(e) =>
                (e.currentTarget.style.background = "transparent")
            }
            style={{
                height: 32,
                padding: "0 14px",
                background: "transparent",
                border: "1px solid var(--secondary-border)",
                color: "var(--fg1)",
                borderRadius: 2,
                fontSize: 13,
                cursor: "pointer",
            }}
        >
            {children}
        </button>
    );
}

function SettingsCard({ children, style }) {
    return (
        <SurfaceCard
            style={{
                padding: "4px 20px 12px",
                marginBottom: 16,
                ...(style || {}),
            }}
        >
            {children}
        </SurfaceCard>
    );
}

function SectionLabel({ children }) {
    return (
        <div
            style={{
                display: "flex",
                alignItems: "center",
                gap: 8,
                padding: "16px 0 2px",
            }}
        >
            <span
                style={{
                    width: 18,
                    height: 2,
                    borderRadius: 999,
                    background: "var(--brand-orange)",
                }}
            />
            <span
                style={{
                    fontSize: 11,
                    fontWeight: 700,
                    letterSpacing: ".08em",
                    textTransform: "uppercase",
                    color: "var(--fg3)",
                }}
            >
                {children}
            </span>
        </div>
    );
}

// SettingRow is one label/help + control line inside a card. `full` stacks
// the control under the label for wide controls (the tags editor).
function SettingRow({ label, help, children, full }) {
    const left = (
        <div style={{ minWidth: 0 }}>
            <div style={{ fontSize: 14, fontWeight: 500, color: "var(--fg1)" }}>
                {label}
            </div>
            {help && (
                <div
                    style={{
                        fontSize: 12,
                        lineHeight: 1.5,
                        color: "var(--fg3)",
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
                    padding: "16px 0",
                    borderTop: "1px solid var(--border-weak)",
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
                display: "flex",
                justifyContent: "space-between",
                alignItems: "flex-start",
                gap: 32,
                padding: "16px 0",
                borderTop: "1px solid var(--border-weak)",
            }}
        >
            {left}
            <div style={{ flexShrink: 0 }}>{children}</div>
        </div>
    );
}

// PreviewBody renders the rendered config.env with key/value colouring:
// comments and `=` are dimmed, keys are blue, values green.
function PreviewBody({ text }) {
    const lines = (text || "").split("\n");
    if (lines.length && lines[lines.length - 1] === "") lines.pop();
    return (
        <div
            style={{
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 12,
                lineHeight: 1.9,
                whiteSpace: "pre-wrap",
                wordBreak: "break-all",
            }}
        >
            {lines.map((line, i) => {
                if (line.startsWith("#"))
                    return (
                        <div key={i} style={{ color: "var(--fg3)" }}>
                            {line}
                        </div>
                    );
                const eq = line.indexOf("=");
                if (eq < 0)
                    return (
                        <div key={i} style={{ color: "var(--fg1)" }}>
                            {line || "\u00a0"}
                        </div>
                    );
                return (
                    <div key={i}>
                        <span style={{ color: "var(--primary-text)" }}>
                            {line.slice(0, eq)}
                        </span>
                        <span style={{ color: "var(--fg3)" }}>=</span>
                        <span style={{ color: "var(--viz-green)" }}>
                            {line.slice(eq + 1)}
                        </span>
                    </div>
                );
            })}
        </div>
    );
}

function UnsavedBar({ onReset, onSave }) {
    return (
        <div
            style={{
                position: "fixed",
                left: 0,
                right: 0,
                bottom: 24,
                display: "flex",
                justifyContent: "center",
                pointerEvents: "none",
                zIndex: 20,
            }}
        >
            <div
                style={{
                    pointerEvents: "auto",
                    display: "flex",
                    alignItems: "center",
                    gap: 12,
                    background: "var(--bg-secondary)",
                    border: "1px solid var(--border-medium)",
                    borderRadius: 2,
                    padding: "9px 12px 9px 16px",
                    boxShadow: "var(--shadow-z2)",
                    animation: "sigil-barin .16s ease-out",
                }}
            >
                <span
                    style={{
                        width: 7,
                        height: 7,
                        borderRadius: "50%",
                        background: "var(--brand-orange)",
                    }}
                />
                <span style={{ fontSize: 13, color: "var(--fg2)" }}>
                    Unsaved changes
                </span>
                <GhostButton onClick={onReset}>Reset</GhostButton>
                <PrimaryButton onClick={onSave}>
                    Save to config.env
                </PrimaryButton>
            </div>
        </div>
    );
}

function SettingsHero({ dirty, path, config }) {
    // The hero stat reads from the same mapping as the header chip, so the two
    // never name one posture two ways.
    const forwardMeta = forwardChipMeta(config);
    const stats = [
        {
            label: "Cloud copy",
            value: forwardMeta.value,
            tone: forwardMeta.color,
        },
        {
            label: "Config",
            value: dirty ? "Unsaved" : "Synced",
            tone: dirty ? "var(--brand-orange-text)" : "var(--success-text)",
        },
    ];
    return (
        <PageHero
            title="Settings"
            desc={path}
            descStyle={{
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 11.5,
                maxWidth: 720,
            }}
            stats={stats}
        />
    );
}

function SettingsTabRail({ tabs, active, onChange }) {
    return (
        <div
            aria-label="Settings sections"
            style={{
                display: "grid",
                gridTemplateColumns: "repeat(auto-fit, minmax(128px, 1fr))",
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
                            textAlign: "left",
                            padding: "12px 12px",
                            borderRadius: 8,
                            border: `1px solid ${isActive ? "var(--primary-border)" : "var(--border-weak)"}`,
                            background: isActive
                                ? ACTIVE_PILL_BG
                                : "rgba(24,27,31,0.68)",
                            color: "var(--fg1)",
                            cursor: "pointer",
                            boxShadow: isActive
                                ? "0 12px 28px rgba(0,0,0,0.20)"
                                : "none",
                        }}
                    >
                        <div
                            style={{
                                display: "flex",
                                alignItems: "center",
                                gap: 8,
                                marginBottom: 8,
                            }}
                        >
                            <span
                                style={{
                                    width: 26,
                                    height: 26,
                                    display: "inline-flex",
                                    alignItems: "center",
                                    justifyContent: "center",
                                    borderRadius: 2,
                                    background: "rgba(204,204,220,0.06)",
                                    color: isActive
                                        ? "var(--brand-orange-text)"
                                        : "var(--fg2)",
                                }}
                            >
                                <Icon name={tab.icon} size={14} />
                            </span>
                            <span
                                style={{
                                    fontSize: 13,
                                    fontWeight: 600,
                                    color: isActive
                                        ? "var(--fg-max)"
                                        : "var(--fg1)",
                                }}
                            >
                                {tab.label}
                            </span>
                        </div>
                        <div
                            style={{
                                fontSize: 11.5,
                                lineHeight: 1.35,
                                color: "var(--fg3)",
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

function SettingsPreviewPanel({ path, preview, onCopy }) {
    return (
        <div
            style={{
                width: "min(440px, 100%)",
                flex: "1 1 360px",
                position: "sticky",
                top: 72,
            }}
        >
            <div
                style={{
                    overflow: "hidden",
                    background: SURFACE_BG,
                    border: "1px solid var(--border-weak)",
                    borderRadius: 8,
                    boxShadow: "0 18px 42px rgba(0,0,0,0.22)",
                }}
            >
                <div
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 10,
                        padding: "12px 14px",
                        borderBottom: "1px solid var(--border-weak)",
                    }}
                >
                    <span
                        style={{
                            width: 28,
                            height: 28,
                            display: "inline-flex",
                            alignItems: "center",
                            justifyContent: "center",
                            borderRadius: 2,
                            background: "rgba(204,204,220,0.06)",
                            color: "var(--fg2)",
                        }}
                    >
                        <Icon name="list" size={14} />
                    </span>
                    <div style={{ minWidth: 0, flex: 1 }}>
                        <div
                            style={{
                                fontSize: 12,
                                fontWeight: 600,
                                color: "var(--fg-max)",
                            }}
                        >
                            config.env preview
                        </div>
                        <div
                            style={{
                                fontSize: 11,
                                color: "var(--fg3)",
                                fontFamily: "var(--fontFamilyMonospace)",
                                overflow: "hidden",
                                textOverflow: "ellipsis",
                                whiteSpace: "nowrap",
                            }}
                        >
                            {path}
                        </div>
                    </div>
                    <button
                        onClick={onCopy}
                        style={{
                            display: "inline-flex",
                            alignItems: "center",
                            gap: 5,
                            background: "transparent",
                            border: "1px solid var(--secondary-border)",
                            color: "var(--fg1)",
                            borderRadius: 2,
                            height: 28,
                            padding: "0 9px",
                            fontSize: 12,
                            cursor: "pointer",
                        }}
                        onMouseEnter={(e) =>
                            (e.currentTarget.style.background =
                                "var(--action-hover)")
                        }
                        onMouseLeave={(e) =>
                            (e.currentTarget.style.background = "transparent")
                        }
                    >
                        <Icon name="copy" size={13} />
                        Copy
                    </button>
                </div>
                <div
                    style={{
                        background: "rgba(17,18,23,0.84)",
                        padding: "14px 16px",
                        maxHeight: "calc(100vh - 252px)",
                        overflow: "auto",
                    }}
                >
                    <PreviewBody text={preview} />
                </div>
            </div>
        </div>
    );
}

function Toast({ message }) {
    return (
        <div
            style={{
                position: "fixed",
                top: 60,
                right: 20,
                zIndex: 30,
                display: "flex",
                alignItems: "center",
                gap: 8,
                background: "var(--bg-secondary)",
                border: "1px solid var(--border-medium)",
                borderRadius: 2,
                padding: "10px 14px",
                boxShadow: "var(--shadow-z2)",
                animation: "sigil-tin .2s ease-out",
            }}
        >
            <Icon
                name="check"
                size={16}
                style={{ color: "var(--success-text)" }}
            />
            <span style={{ fontSize: 13, color: "var(--fg1)" }}>{message}</span>
        </div>
    );
}

const FORWARD_LOCAL_OPTIONS = [
    { value: "off", label: "Local only" },
    { value: "metadata_only", label: "Metadata only" },
    { value: "full", label: "Full" },
];
// Connecting turns forwarding on, so the connect flow offers the same modes
// without the off case.
const CONNECT_MODE_OPTIONS = FORWARD_LOCAL_OPTIONS.filter(
    (o) => o.value !== "off",
);
const SETTINGS_TABS = [
    {
        id: "cloud",
        label: "Cloud",
        icon: "cloud",
        desc: "Ingest, auth, forwarding",
    },
    { id: "local", label: "Local", icon: "box", desc: "Tags and runtime" },
    {
        id: "history",
        label: "History",
        icon: "clock",
        desc: "Import past sessions",
    },
];
const SETTINGS_TAB_IDS = new Set(SETTINGS_TABS.map((t) => t.id));

function settingsTabFromLocation() {
    if (typeof window === "undefined") return "cloud";
    const params = new URLSearchParams(window.location.search || "");
    const tab = params.get("tab") || "";
    return SETTINGS_TAB_IDS.has(tab) ? tab : "cloud";
}

function settingsPath(tab) {
    const url = new URL(
        "/settings",
        typeof window !== "undefined"
            ? window.location.origin
            : "http://localhost",
    );
    if (SETTINGS_TAB_IDS.has(tab) && tab !== "cloud")
        url.searchParams.set("tab", tab);
    return url.pathname + url.search;
}

// urlHost reduces an ingest or OTLP URL to the host the copy names, so a
// status line stays readable. Anything that is not an http(s) URL is shown
// as typed.
function urlHost(raw) {
    const s = String(raw || "");
    const m = s.match(/^https?:\/\/([^/]+)/);
    return m ? m[1] : s || "\u2014";
}

// HostTarget names the stack a confirmation is about. A connection can be
// saved with a token or a tenant and no endpoint, and urlHost's fallback
// would then read "will be sent to \u2014".
function HostTarget({ url }) {
    const s = String(url || "").trim();
    if (!s) return "your stack";
    return (
        <span
            style={{
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 12,
                color: "var(--fg1)",
            }}
        >
            {urlHost(s)}
        </span>
    );
}

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
function parseConnectBlock(text) {
    const fields = {
        ENDPOINT: { name: "endpoint", url: true },
        AUTH_TENANT_ID: { name: "tenantId" },
        AUTH_TOKEN: { name: "token" },
        OTEL_EXPORTER_OTLP_ENDPOINT: { name: "otlpEndpoint", url: true },
        OTEL_EXPORTER_OTLP_HEADERS: { name: "otlpHeaders" },
    };
    const out = {
        endpoint: "",
        tenantId: "",
        token: "",
        otlpEndpoint: "",
        otlpHeaders: "",
        placeholders: [],
        invalid: [],
    };
    const preferred = {};
    String(text || "")
        .split(/\r?\n/)
        .forEach((raw) => {
            const line = raw.trim().replace(/^export\s+/, "");
            if (!line || line.startsWith("#")) return;
            const eq = line.indexOf("=");
            if (eq < 1) return;
            const key = line.slice(0, eq).trim();
            const branded = /^AGENTO11Y_/.test(key);
            const suffix = key.replace(/^(?:AGENTO11Y|SIGIL)_/, "");
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
            const report = (list) => {
                out[field.name] = "";
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
function isHTTPURL(value) {
    if (!/^https?:\/\//i.test(String(value || ""))) return false;
    try {
        return !!new URL(value).host;
    } catch (_) {
        return false;
    }
}

// parseBlockValue reads one assignment's value the way internal/dotenv reads
// the same line: a quoted value ends at its closing quote, an unquoted one
// ends at a trailing ` #` comment.
function parseBlockValue(raw) {
    const v = raw.trim();
    if (v.length >= 2 && (v[0] === '"' || v[0] === "'")) {
        const end = v.indexOf(v[0], 1);
        if (end >= 0) return v.slice(1, end);
    }
    const hash = v.indexOf(" #");
    return (hash >= 0 ? v.slice(0, hash) : v).replace(/[ \t]+$/, "");
}

function looksLikePlaceholder(value) {
    const s = String(value || "");
    const open = s.indexOf("<");
    return open >= 0 && s.indexOf(">", open) >= 0;
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
function setupPageURL(raw) {
    const typed = String(raw || "").trim();
    const href = markdownURL(
        BARE_HOST_RE.test(typed) ? `https://${typed}` : typed,
    );
    if (!href || !/^https?:\/\//i.test(href)) return "";
    try {
        return (
            new URL(href).origin + "/a/grafana-agento11y-app/setup-coding-agent"
        );
    } catch (_) {
        return "";
    }
}

// ConnectStep is one numbered step of the connect flow, on the SettingRow
// rhythm: a top border, the label type, and help text under it.
function ConnectStep({ n, title, help, children }) {
    return (
        <div
            style={{
                display: "flex",
                gap: 14,
                padding: "18px 0",
                borderTop: "1px solid var(--border-weak)",
            }}
        >
            <span
                style={{
                    flex: "none",
                    width: 24,
                    height: 24,
                    borderRadius: "50%",
                    border: "1px solid var(--border-medium)",
                    display: "inline-flex",
                    alignItems: "center",
                    justifyContent: "center",
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 12,
                    color: "var(--fg2)",
                }}
            >
                {n}
            </span>
            <div style={{ minWidth: 0, flex: 1 }}>
                <div
                    style={{
                        fontSize: 14,
                        fontWeight: 500,
                        color: "var(--fg1)",
                    }}
                >
                    {title}
                </div>
                {help && (
                    <div
                        style={{
                            fontSize: 12,
                            lineHeight: 1.5,
                            color: "var(--fg3)",
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

// SettingsConnectFlow replaces the credential form when nothing is saved. It
// replicates the `agento11y login` handshake: open the setup page on your
// stack, paste the block it hands back, pick what to forward.
//
// The pasted block, token included, lives in this component's state while
// the flow is open: the textarea is controlled, and parseConnectBlock re-runs
// on every render. Only the feedback strip withholds the token value.
function SettingsConnectFlow({
    savedStackURL,
    configPath,
    capture,
    onConnect,
    onManual,
}) {
    const [stackUrl, setStackUrl] = useState(savedStackURL || "");
    const [paste, setPaste] = useState("");
    const [draftMode, setDraftMode] = useState("metadata_only");
    const [confirmFull, setConfirmFull] = useState(false);

    const setupHref = setupPageURL(stackUrl);
    const parsed = parseConnectBlock(paste);
    const ok = !!(parsed.endpoint && parsed.tenantId && parsed.token);
    const missing = [
        !parsed.endpoint && "AGENTO11Y_ENDPOINT",
        !parsed.tenantId && "AGENTO11Y_AUTH_TENANT_ID",
        !parsed.token && "AGENTO11Y_AUTH_TOKEN",
    ].filter(Boolean);
    const advanced =
        capture === "no_tool_content" || capture === "full_with_metadata_spans";
    const tone = ok ? "success" : "warning";
    // A dropped value is named for what is wrong with it. Reporting a
    // placeholder or a broken URL as a missing key sends the user back for the
    // same block again.
    const detail = ok
        ? `${urlHost(parsed.endpoint)} · tenant ${parsed.tenantId} · token found ${parsed.otlpEndpoint ? `· OTLP endpoint ${urlHost(parsed.otlpEndpoint)}` : "· no OTLP endpoint"}`
        : parsed.placeholders.length > 0
          ? `${parsed.placeholders.join(", ")} is still a placeholder. Fill it in on the setup page, then copy the block again.`
          : parsed.invalid.length > 0
            ? `${parsed.invalid.join(", ")} is not an http:// or https:// URL.`
            : `Missing ${missing.join(", ")}. Copy the whole block from the setup page.`;
    // Full forwarding asks first here too. Reaching it from a fresh install
    // takes two clicks otherwise, while the same widening in the connected
    // panel is confirmed.
    const submit = () => {
        if (draftMode === "full") {
            setConfirmFull(true);
            return;
        }
        onConnect(parsed, draftMode);
    };

    return (
        <SettingsCard style={{ padding: "4px 20px 20px" }}>
            <SectionLabel>Connect to Grafana Cloud</SectionLabel>
            <div
                style={{
                    fontSize: 12,
                    lineHeight: 1.5,
                    color: "var(--fg3)",
                    padding: "0 0 16px",
                    maxWidth: 620,
                }}
            >
                Local capture keeps working with no connection. Connecting lets
                the daemon forward sessions to your stack, and writes the same
                credentials to <Mono>config.env</Mono> as{" "}
                <Mono>agento11y login</Mono>.
            </div>

            <ConnectStep n={1} title="Create a token in your stack">
                <div
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 8,
                        marginTop: 12,
                        flexWrap: "wrap",
                    }}
                >
                    <MonoInput
                        value={stackUrl}
                        onChange={setStackUrl}
                        placeholder="https://your-stack.grafana.net"
                        width={280}
                    />
                    <a
                        href={setupHref || undefined}
                        target="_blank"
                        rel="noreferrer"
                        aria-disabled={setupHref ? undefined : "true"}
                        onClick={(e) => {
                            if (!setupHref) e.preventDefault();
                        }}
                        style={{
                            display: "inline-flex",
                            alignItems: "center",
                            gap: 6,
                            height: 32,
                            padding: "0 14px",
                            background: setupHref
                                ? "var(--primary-main)"
                                : "rgba(204,204,220,0.08)",
                            border: `1px solid ${setupHref ? "var(--primary-main)" : "transparent"}`,
                            color: setupHref ? "#fff" : "var(--fg3)",
                            borderRadius: 2,
                            fontSize: 13,
                            fontWeight: 500,
                            textDecoration: "none",
                            cursor: setupHref ? "pointer" : "not-allowed",
                        }}
                    >
                        Open setup page
                        <Icon name="extlink" size={12} />
                    </a>
                </div>
                <div
                    style={{
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 11,
                        color: "var(--fg3)",
                        marginTop: 8,
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        whiteSpace: "nowrap",
                    }}
                >
                    {setupHref ||
                        (stackUrl.trim()
                            ? "The setup link needs a stack hostname or URL."
                            : "Enter your stack URL to build the setup link.")}
                </div>
                <div
                    style={{ fontSize: 12, color: "var(--fg3)", marginTop: 10 }}
                >
                    No stack yet?{" "}
                    <a
                        href="https://grafana.com/auth/sign-up/create-user/?"
                        target="_blank"
                        rel="noreferrer"
                        style={{
                            color: "var(--brand-orange-text)",
                            display: "inline-flex",
                            alignItems: "center",
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
                        Paste the whole block. The token is stored locally in{" "}
                        <Mono>{configPath}</Mono>
                    </>
                }
            >
                <textarea
                    value={paste}
                    onChange={(e) => setPaste(e.target.value)}
                    spellCheck={false}
                    placeholder={
                        "AGENTO11Y_ENDPOINT=https://agento11y-prod-eu-west-2.grafana.net\nAGENTO11Y_AUTH_TENANT_ID=1234567890\nAGENTO11Y_AUTH_TOKEN=glc_…\nOTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-prod-eu-west-2.grafana.net/otlp"
                    }
                    onFocus={(e) =>
                        (e.currentTarget.style.borderColor =
                            "var(--primary-border)")
                    }
                    onBlur={(e) =>
                        (e.currentTarget.style.borderColor =
                            "var(--border-medium)")
                    }
                    style={{
                        marginTop: 12,
                        width: "100%",
                        height: 118,
                        resize: "vertical",
                        background: "var(--bg-canvas)",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        color: "var(--fg1)",
                        padding: 10,
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 12,
                        lineHeight: 1.7,
                        outline: "none",
                    }}
                />
                {paste.trim() !== "" && (
                    <div
                        style={{
                            display: "flex",
                            gap: 10,
                            alignItems: "flex-start",
                            marginTop: 10,
                            padding: "10px 12px",
                            border: `1px solid var(--${tone}-border)`,
                            background: `var(--${tone}-transparent)`,
                            borderRadius: 2,
                        }}
                    >
                        <Icon
                            name={ok ? "check" : "alert"}
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
                                {ok
                                    ? "Connection settings read"
                                    : "Couldn't read a complete block"}
                            </div>
                            <div
                                style={{
                                    fontFamily: "var(--fontFamilyMonospace)",
                                    fontSize: 11.5,
                                    color: "var(--fg3)",
                                    marginTop: 4,
                                    lineHeight: 1.7,
                                    wordBreak: "break-all",
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
                        The local viewer always keeps full content.{" "}
                        <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                            Metadata only
                        </b>{" "}
                        forwards usage and session metadata, and{" "}
                        <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                            Full
                        </b>{" "}
                        forwards prompts, responses, and tool I/O too.
                        {advanced && (
                            <div
                                style={{
                                    color: "var(--warning-text)",
                                    marginTop: 6,
                                }}
                            >
                                Advanced capture mode <Mono>{capture}</Mono> is
                                set in config.env. Sessions forward as metadata
                                while it is set.{" "}
                                <b
                                    style={{
                                        fontWeight: 500,
                                        color: "var(--fg2)",
                                    }}
                                >
                                    Metadata only
                                </b>{" "}
                                keeps that value;{" "}
                                <b
                                    style={{
                                        fontWeight: 500,
                                        color: "var(--fg2)",
                                    }}
                                >
                                    Full
                                </b>{" "}
                                overwrites it, for your non-local Cloud sessions
                                too.
                            </div>
                        )}
                    </>
                }
            >
                <div style={{ marginTop: 12 }}>
                    <PillToggle
                        value={draftMode}
                        onChange={setDraftMode}
                        options={CONNECT_MODE_OPTIONS}
                    />
                </div>
                <div
                    style={{
                        display: "flex",
                        alignItems: "center",
                        gap: 10,
                        marginTop: 16,
                        flexWrap: "wrap",
                    }}
                >
                    <button
                        type="button"
                        disabled={!ok}
                        onClick={submit}
                        style={{
                            height: 32,
                            padding: "0 14px",
                            borderRadius: 2,
                            fontSize: 13,
                            fontWeight: 500,
                            background: ok
                                ? "var(--primary-main)"
                                : "rgba(204,204,220,0.08)",
                            border: `1px solid ${ok ? "var(--primary-main)" : "transparent"}`,
                            color: ok ? "#fff" : "var(--fg3)",
                            cursor: ok ? "pointer" : "not-allowed",
                            fontFamily: "var(--fontFamily)",
                        }}
                    >
                        Connect
                    </button>
                    {ok && (
                        <span style={{ fontSize: 12, color: "var(--fg3)" }}>
                            Writes config.env and starts forwarding.
                        </span>
                    )}
                </div>
            </ConnectStep>

            {/* An endpoint on its own is a valid configuration (a local collector
              needs no tenant or token), and Connect cannot produce one, so the
              credential fields stay reachable from the empty state. */}
            <div
                style={{
                    fontSize: 12,
                    color: "var(--fg3)",
                    paddingTop: 16,
                    borderTop: "1px solid var(--border-weak)",
                }}
            >
                Pointing at a collector of your own?{" "}
                <button
                    type="button"
                    onClick={onManual}
                    style={{
                        background: "transparent",
                        border: "none",
                        padding: 0,
                        font: "inherit",
                        color: "var(--primary-text)",
                        cursor: "pointer",
                        textDecoration: "underline",
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
                        onConnect(parsed, "full");
                    }}
                />
            )}
        </SettingsCard>
    );
}

// ConfirmFullContentModal is the consent point for widening to full content,
// from either the connect flow or the configured panel. It names both scopes
// CONTENT_CAPTURE_MODE covers, because the modal is the last thing the user
// reads before the write.
function ConfirmFullContentModal({ endpoint, onCancel, onConfirm }) {
    return (
        <ModalFrame
            width="min(560px, 100%)"
            onClose={onCancel}
            title="Forward full session content?"
            desc="This applies to every local session on this machine and to your non-local Cloud sessions, until you change it."
        >
            <div
                style={{
                    padding: "16px 18px",
                    fontSize: 13,
                    lineHeight: 1.6,
                    color: "var(--fg2)",
                }}
            >
                The daemon will send prompts, responses, reasoning text, tool
                inputs and results, and attached media to{" "}
                <HostTarget url={endpoint} />. Metadata only forwards usage and
                session metadata instead.
            </div>
            <div
                style={{
                    display: "flex",
                    justifyContent: "flex-end",
                    gap: 10,
                    padding: "14px 18px",
                    borderTop: "1px solid var(--border-weak)",
                }}
            >
                <GhostButton onClick={onCancel}>Cancel</GhostButton>
                <button
                    type="button"
                    onClick={onConfirm}
                    style={{
                        height: 32,
                        padding: "0 14px",
                        background: "var(--warning-main)",
                        border: "1px solid var(--warning-main)",
                        color: "#111217",
                        borderRadius: 2,
                        fontSize: 13,
                        fontWeight: 500,
                        fontFamily: "var(--fontFamily)",
                        cursor: "pointer",
                    }}
                >
                    Forward full content
                </button>
            </div>
        </ModalFrame>
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
    config,
    stackUrl,
    configured,
    configPath,
    onConnect,
    onDisconnect,
    onMode,
}) {
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
    const advanced =
        form.capture === "no_tool_content" ||
        form.capture === "full_with_metadata_spans";
    // The daemon prefers config.env, but it also inherits LOCAL_FORWARD into
    // its own environment at boot, so "off here, on there" is reachable until
    // an explicit false is saved.
    const daemonStillOn =
        !!(forwardStatus && forwardStatus.enabled) && !form.localForward;
    // Say it next to the control that sets the capture mode, not only in the
    // header chip.
    const guardsChained = !!(forwardStatus && forwardStatus.hooks);
    const failures = (forwardStatus && forwardStatus.failures) || [];
    // recentFailures outlives being turned off, so a failure list alone would
    // put an error notice under the calm "forwarding is off" line.
    const failing =
        !!(forwardStatus && forwardStatus.enabled) && failures.length > 0;
    // Widening is the only direction that asks. Narrowing takes effect at once,
    // because less content leaving the machine needs no consent.
    //
    // Local only is the one mode a click on the active pill still writes: while
    // the daemon forwards from its own environment, config.env needs an
    // explicit false to override it, and there is no pending edit to save it
    // with.
    const requestMode = (mode) => {
        if (mode === "full" && forwardMode !== "full") {
            setConfirmFull(true);
            return;
        }
        onMode(mode, mode === "off" && daemonStillOn);
    };

    return (
        <SettingsCard style={{ padding: "4px 20px 20px" }}>
            <SectionLabel>Cloud forwarding</SectionLabel>
            <div
                style={{
                    fontSize: 12,
                    lineHeight: 1.5,
                    color: "var(--fg3)",
                    padding: "0 0 4px",
                    maxWidth: 620,
                }}
            >
                What the daemon sends to your stack for every{" "}
                <Mono>--local</Mono> session on this machine.
            </div>

            <div
                style={{
                    padding: "16px 0",
                    borderTop: "1px solid var(--border-weak)",
                    marginTop: 12,
                }}
            >
                <PillToggle
                    size="md"
                    value={forwardMode}
                    onChange={requestMode}
                    options={FORWARD_LOCAL_OPTIONS}
                />
                <div
                    style={{
                        fontSize: 12,
                        lineHeight: 1.5,
                        color: "var(--fg3)",
                        marginTop: 10,
                        maxWidth: 620,
                    }}
                >
                    The local viewer always keeps full content.{" "}
                    <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                        Metadata only
                    </b>{" "}
                    forwards usage and session metadata, and{" "}
                    <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Full</b>{" "}
                    forwards prompts, responses, and tool I/O too.
                    {advanced && (
                        <div
                            style={{
                                color: "var(--warning-text)",
                                marginTop: 6,
                            }}
                        >
                            Advanced capture mode <Mono>{form.capture}</Mono> is
                            set in config.env. Sessions forward as metadata
                            while it is set.{" "}
                            <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                                Metadata only
                            </b>{" "}
                            keeps that value;{" "}
                            <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                                Full
                            </b>{" "}
                            overwrites it, for your non-local Cloud sessions
                            too.
                        </div>
                    )}
                    {daemonStillOn && (
                        <div
                            style={{
                                color: "var(--warning-text)",
                                marginTop: 6,
                            }}
                        >
                            The running daemon is still forwarding:{" "}
                            <Mono>LOCAL_FORWARD</Mono> is set in its
                            environment. config.env overrides that, but only
                            once it holds an explicit <Mono>false</Mono>.{" "}
                            <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                                Local only
                            </b>{" "}
                            is already selected here, so click it to write that{" "}
                            <Mono>false</Mono>.
                        </div>
                    )}
                    {guardsChained && (
                        <div
                            style={{
                                color: "var(--warning-text)",
                                marginTop: 6,
                            }}
                        >
                            {GUARD_CONTENT_NOTE} The daemon relays{" "}
                            <Mono>--local</Mono> guard checks to Cloud so your
                            Cloud rules still apply; turn off{" "}
                            <Mono>GUARDS_ENABLED</Mono> or forwarding to stop
                            it.
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
                            display: "flex",
                            gap: 10,
                            alignItems: "flex-start",
                            marginTop: 14,
                            maxWidth: 640,
                        }}
                    >
                        <span
                            style={{
                                flex: "none",
                                width: 7,
                                height: 7,
                                borderRadius: "50%",
                                marginTop: 6,
                                background: meta.color,
                            }}
                        />
                        <div
                            style={{
                                fontSize: 12.5,
                                lineHeight: 1.55,
                                color: "var(--fg2)",
                            }}
                        >
                            {meta.line}
                        </div>
                    </div>
                )}
            </div>

            <SettingRow
                label={configured ? "Connected stack" : "Connection"}
                help={null}
            >
                <button
                    type="button"
                    onClick={() => setEditOpen((v) => !v)}
                    style={{
                        background: "transparent",
                        border: "1px solid var(--border-medium)",
                        borderRadius: 2,
                        color: "var(--fg2)",
                        fontSize: 11.5,
                        fontFamily: "var(--fontFamily)",
                        padding: "4px 10px",
                        cursor: "pointer",
                    }}
                >
                    {editOpen ? "Hide connection" : "Edit connection"}
                </button>
            </SettingRow>
            <div
                style={{
                    fontFamily: "var(--fontFamilyMonospace)",
                    fontSize: 11.5,
                    color: "var(--fg3)",
                    marginTop: -8,
                    lineHeight: 1.8,
                    wordBreak: "break-all",
                }}
            >
                <div>{form.endpoint || "no endpoint"}</div>
                <div>
                    {form.tenantId ? `tenant ${form.tenantId}` : "no tenant"}
                    {form.tokenSet && !form.tokenCleared
                        ? " · token configured (0600)"
                        : " · no token"}
                    {form.otlpEndpoint
                        ? ` · otlp ${urlHost(form.otlpEndpoint)}`
                        : ""}
                </div>
            </div>

            {editOpen && (
                <>
                    <SettingRow
                        label="Endpoint"
                        help={<>Grafana AI Observability ingest URL.</>}
                    >
                        <MonoInput
                            value={form.endpoint}
                            onChange={(v) => set({ endpoint: v })}
                            placeholder="https://agento11y-prod-….grafana.net"
                            width={320}
                        />
                    </SettingRow>
                    <SettingRow label="Tenant ID" help={null}>
                        <MonoInput
                            value={form.tenantId}
                            onChange={(v) => set({ tenantId: v })}
                            placeholder="123456"
                            width={200}
                        />
                    </SettingRow>
                    <SettingRow label="Auth token" help={null}>
                        {form.tokenSet &&
                        !form.tokenCleared &&
                        form.token === "" ? (
                            <div
                                style={{
                                    display: "inline-flex",
                                    alignItems: "center",
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
                                        background: "var(--bg-canvas)",
                                        border: "1px solid var(--border-medium)",
                                        borderRadius: 2,
                                        color: "var(--fg3)",
                                        padding: "0 10px",
                                        fontFamily:
                                            "var(--fontFamilyMonospace)",
                                        fontSize: 12,
                                        cursor: "not-allowed",
                                    }}
                                />
                                <GhostButton
                                    onClick={() =>
                                        set({ tokenCleared: true, token: "" })
                                    }
                                >
                                    Reset
                                </GhostButton>
                            </div>
                        ) : (
                            <MonoInput
                                type="password"
                                value={form.token}
                                onChange={(v) =>
                                    set({
                                        token: v,
                                        tokenCleared: form.tokenSet && v === "",
                                    })
                                }
                                placeholder={
                                    form.tokenSet
                                        ? "new token, or blank to remove"
                                        : "glc_…"
                                }
                                width={260}
                            />
                        )}
                    </SettingRow>
                    <SettingRow
                        label="OTLP endpoint"
                        help={<>For SDK traces and metrics.</>}
                    >
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
                        <SettingRow
                            label="Disconnect"
                            help={
                                <>
                                    Clears the saved credentials and stops all
                                    forwarding.
                                </>
                            }
                        >
                            <button
                                type="button"
                                onClick={() => setConfirmDisconnect(true)}
                                style={{
                                    height: 32,
                                    padding: "0 14px",
                                    background: "transparent",
                                    border: "1px solid var(--error-border)",
                                    color: "var(--error-text)",
                                    borderRadius: 2,
                                    fontSize: 13,
                                    fontFamily: "var(--fontFamily)",
                                    cursor: "pointer",
                                }}
                            >
                                Disconnect
                            </button>
                        </SettingRow>
                    )}
                </>
            )}

            {confirmFull && (
                <ConfirmFullContentModal
                    endpoint={savedEndpoint}
                    onCancel={() => setConfirmFull(false)}
                    onConfirm={() => {
                        setConfirmFull(false);
                        onMode("full");
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
                            padding: "16px 18px",
                            fontSize: 13,
                            lineHeight: 1.6,
                            color: "var(--fg2)",
                        }}
                    >
                        The endpoint, tenant ID, auth token and OTLP settings
                        for <HostTarget url={savedEndpoint} /> are deleted, so
                        your non-local Cloud sessions stop reaching the stack
                        too, until you connect again or run{" "}
                        <Mono>agento11y login</Mono>. Sessions already captured
                        stay in the local store, and{" "}
                        <Mono>CONTENT_CAPTURE_MODE</Mono> is left as it is.
                    </div>
                    <div
                        style={{
                            display: "flex",
                            justifyContent: "flex-end",
                            gap: 10,
                            padding: "14px 18px",
                            borderTop: "1px solid var(--border-weak)",
                        }}
                    >
                        <GhostButton
                            onClick={() => setConfirmDisconnect(false)}
                        >
                            Cancel
                        </GhostButton>
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
                                padding: "0 14px",
                                background: "transparent",
                                border: "1px solid var(--error-border)",
                                color: "var(--error-text)",
                                borderRadius: 2,
                                fontSize: 13,
                                fontFamily: "var(--fontFamily)",
                                cursor: "pointer",
                            }}
                        >
                            Disconnect
                        </button>
                    </div>
                </ModalFrame>
            )}
        </SettingsCard>
    );
}

function SettingsTagsEditor({ tags, setTag, addTag, removeTag }) {
    return (
        <SettingsCard>
            <SectionLabel>Session tags</SectionLabel>
            <SettingRow
                full
                label="Tags"
                help={
                    <>
                        Applied to every generation as <Mono>key=value</Mono>.
                        Empty pairs are dropped on save.
                    </>
                }
            >
                <div
                    style={{ display: "flex", flexDirection: "column", gap: 8 }}
                >
                    {tags.map((t, i) => (
                        <div
                            key={i}
                            style={{
                                display: "flex",
                                alignItems: "center",
                                gap: 8,
                            }}
                        >
                            <MonoInput
                                value={t.key}
                                onChange={(v) => setTag(i, { key: v })}
                                placeholder="key"
                                width={200}
                            />
                            <span
                                style={{
                                    color: "var(--fg3)",
                                    fontFamily: "var(--fontFamilyMonospace)",
                                }}
                            >
                                =
                            </span>
                            <MonoInput
                                value={t.value}
                                onChange={(v) => setTag(i, { value: v })}
                                placeholder="value"
                                width={200}
                            />
                            <button
                                onClick={() => removeTag(i)}
                                title="Remove tag"
                                aria-label="Remove tag"
                                style={{
                                    width: 28,
                                    height: 28,
                                    display: "inline-flex",
                                    alignItems: "center",
                                    justifyContent: "center",
                                    background: "transparent",
                                    border: "1px solid transparent",
                                    color: "var(--fg3)",
                                    cursor: "pointer",
                                    borderRadius: 2,
                                }}
                                onMouseEnter={(e) =>
                                    (e.currentTarget.style.color = "var(--fg1)")
                                }
                                onMouseLeave={(e) =>
                                    (e.currentTarget.style.color = "var(--fg3)")
                                }
                            >
                                <Icon name="times" size={14} />
                            </button>
                        </div>
                    ))}
                    <button
                        onClick={addTag}
                        style={{
                            alignSelf: "flex-start",
                            display: "inline-flex",
                            alignItems: "center",
                            gap: 6,
                            height: 30,
                            padding: "0 12px",
                            background: "transparent",
                            border: "1px dashed var(--border-medium)",
                            borderRadius: 2,
                            color: "var(--fg2)",
                            fontSize: 13,
                            cursor: "pointer",
                        }}
                        onMouseEnter={(e) =>
                            (e.currentTarget.style.borderColor =
                                "var(--border-strong)")
                        }
                        onMouseLeave={(e) =>
                            (e.currentTarget.style.borderColor =
                                "var(--border-medium)")
                        }
                    >
                        <Icon name="plus" size={13} />
                        Add tag
                    </button>
                </div>
            </SettingRow>
        </SettingsCard>
    );
}

function SettingsLocalTab({ form, set, setTag, addTag, removeTag }) {
    return (
        <>
            <SettingsTagsEditor
                tags={form.tags}
                setTag={setTag}
                addTag={addTag}
                removeTag={removeTag}
            />
            <SettingsCard>
                <SectionLabel>Runtime</SectionLabel>
                <SettingRow
                    label="Debug logging"
                    help={
                        <>
                            Write a verbose log to{" "}
                            <Mono>
                                ~/.local/state/agento11y/logs/agento11y.log
                            </Mono>
                            .
                        </>
                    }
                >
                    <Toggle
                        checked={form.debug}
                        onChange={(v) => set({ debug: v })}
                    />
                </SettingRow>
                <SettingRow
                    label="Automatic updates"
                    help={
                        <>
                            Keep host agent plugins refreshed automatically.
                            Turn off to pin the current versions.
                        </>
                    }
                >
                    <Toggle
                        checked={form.autoUpdate}
                        onChange={(v) => set({ autoUpdate: v })}
                    />
                </SettingRow>
            </SettingsCard>
            <SettingsCard>
                <SectionLabel>Identity (optional)</SectionLabel>
                <SettingRow
                    label="User ID"
                    help={
                        <>
                            Override the resolved user id used to attribute
                            generations. Leave blank to auto-resolve.
                        </>
                    }
                >
                    <MonoInput
                        value={form.userId}
                        onChange={(v) => set({ userId: v })}
                        placeholder="auto"
                        width={260}
                    />
                </SettingRow>
            </SettingsCard>
        </>
    );
}

// SettingsHistoryTab is the import surface: pick a registered agent, see
// what a 90-day import would cover, and run it. The plan request reads
// metadata only, so opening this tab never reads session content.
function SettingsHistoryTab({ history }) {
    const agents = history.agents || [];
    const [agent, setAgent] = useState("");
    const [plan, setPlan] = useState(null);
    const [planError, setPlanError] = useState(null);
    const [loadingPlan, setLoadingPlan] = useState(false);

    const selected = agent || (agents[0] && agents[0].id) || "";
    const run = history.run;
    const active = importRunIsActive(run);

    const loadPlan = useCallback((id) => {
        if (!id) return;
        setLoadingPlan(true);
        setPlanError(null);
        fetch(`/api/v1/history/plan?agent=${encodeURIComponent(id)}`)
            .then((r) =>
                r.ok
                    ? r.json()
                    : r
                          .text()
                          .then((t) =>
                              Promise.reject(
                                  new Error(t.trim() || `HTTP ${r.status}`),
                              ),
                          ),
            )
            .then((b) => setPlan(b))
            .catch((e) => {
                setPlan(null);
                setPlanError(String(e.message || e));
            })
            .finally(() => setLoadingPlan(false));
    }, []);

    useEffect(() => {
        loadPlan(selected);
    }, [selected, loadPlan]);
    // A finished run changes what is left to import, so the plan is stale.
    useEffect(() => {
        if (run && !active) loadPlan(selected);
    }, [run, active, selected, loadPlan]);

    const sessions = (plan && plan.sessions) || [];
    const turns = sessions.reduce((n, s) => n + (s.turn_count || 0), 0);
    const approx = sessions.some((s) => s.approx_turns);

    return (
        <SettingsCard>
            <SectionLabel>Import past sessions</SectionLabel>
            <div
                style={{
                    fontSize: 12,
                    lineHeight: 1.5,
                    color: "var(--fg3)",
                    padding: "0 0 10px",
                }}
            >
                Backfill sessions an agent recorded before agento11y was
                installed. The import writes to the local store on this machine.
                The daemon never relays it to Grafana Cloud, whatever{" "}
                <b style={{ fontWeight: 500, color: "var(--fg2)" }}>
                    Cloud forwarding
                </b>{" "}
                on the Cloud tab is set to.
            </div>
            <SettingRow
                label="Agent"
                help={<>Only agents with an importer are listed.</>}
            >
                <Select
                    value={selected}
                    onChange={setAgent}
                    disabled={active}
                    trigger={{
                        ...fieldInput,
                        width: 220,
                        display: "inline-flex",
                    }}
                    options={agents.map((a) => ({
                        value: a.id,
                        label: a.display_name || a.id,
                    }))}
                />
            </SettingRow>
            <SettingRow
                label="Available"
                help={
                    <>
                        Sessions active in the last 90 days. Sessions an agent
                        may still be writing are left out.
                    </>
                }
            >
                <div
                    style={{
                        fontSize: 13,
                        color: "var(--fg1)",
                        textAlign: "right",
                    }}
                >
                    {loadingPlan ? (
                        "Scanning…"
                    ) : planError ? (
                        <span style={{ color: "var(--error-text)" }}>
                            {planError}
                        </span>
                    ) : (
                        `${sessions.length} sessions · ${approx ? "about " : ""}${turns.toLocaleString()} turns`
                    )}
                    {plan && plan.since && (
                        <div
                            style={{
                                fontSize: 11,
                                color: "var(--fg3)",
                                marginTop: 2,
                            }}
                        >
                            since {plan.since}
                        </div>
                    )}
                </div>
            </SettingRow>
            <SettingRow
                label="Import"
                help={
                    <>
                        Re-running an import skips turns already recorded, so it
                        is safe to repeat.
                    </>
                }
            >
                {active ? (
                    <GhostButton onClick={history.cancel}>
                        Cancel import
                    </GhostButton>
                ) : (
                    <PrimaryButton onClick={() => history.start(selected)}>
                        Import {sessions.length} sessions
                    </PrimaryButton>
                )}
            </SettingRow>
            {history.error && (
                <div style={{ padding: "0 0 12px" }}>
                    <Notice kind="error" title="Could not start the import">
                        {history.error}
                    </Notice>
                </div>
            )}
            {run && <HistoryImportStatus run={run} />}
        </SettingsCard>
    );
}

// HistoryImportStatus reports a run in sessions while it runs, and adds the
// turn totals once it stops. A session holds many model turns, so showing
// both at once invites reading the turn count as a session count.
function HistoryImportStatus({ run }) {
    const tone = {
        completed: { kind: "info", title: "Import finished" },
        failed: { kind: "error", title: "Import failed" },
        cancelled: { kind: "warning", title: "Import cancelled" },
    }[run.status] || { kind: "info", title: "Import running" };
    const done = run.sessions || 0;
    const total = run.selected || 0;
    const detail = { fontSize: 12, color: "var(--fg3)" };
    return (
        <div style={{ padding: "0 0 12px" }}>
            <Notice kind={tone.kind} title={tone.title}>
                <div>
                    {importSessionLabel(run)}
                    {run.missing ? ` · ${run.missing} no longer on disk` : ""}
                </div>
                {importRunIsActive(run) ? (
                    <ImportProgressBar
                        done={done}
                        total={total}
                        style={{ marginTop: 6, marginBottom: 2 }}
                    />
                ) : (
                    <div style={detail}>
                        <span
                            style={{
                                display: "flex",
                                flexWrap: "wrap",
                                gap: "2px 12px",
                            }}
                        >
                            <span>
                                {(run.imported || 0).toLocaleString()} turns
                                imported
                            </span>
                            <span>
                                {(run.skipped || 0).toLocaleString()} already
                                imported
                            </span>
                            <span>
                                {(run.failed || 0).toLocaleString()} failed
                            </span>
                        </span>
                    </div>
                )}
                {run.error && <div style={{ fontSize: 12 }}>{run.error}</div>}
            </Notice>
        </div>
    );
}

function SettingsTabPanels({
    activeSettingsTab,
    form,
    set,
    savedEndpoint,
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
}) {
    return (
        <>
            {activeSettingsTab === "cloud" && (
                <SettingsCloudTab
                    form={form}
                    set={set}
                    savedEndpoint={savedEndpoint}
                    config={config}
                    stackUrl={stackUrl}
                    configured={configured}
                    configPath={configPath}
                    onConnect={onConnect}
                    onDisconnect={onDisconnect}
                    onMode={onMode}
                />
            )}
            {activeSettingsTab === "local" && (
                <SettingsLocalTab
                    form={form}
                    set={set}
                    setTag={setTag}
                    addTag={addTag}
                    removeTag={removeTag}
                />
            )}
            {activeSettingsTab === "history" && (
                <SettingsHistoryTab history={history} />
            )}
        </>
    );
}

// SettingsView edits config.env. It does not fetch it: App() polls
// /api/v1/config for the header chip, and this view hydrates from the same
// response so one poll serves both.
function SettingsView({
    history,
    config,
    configError,
    activeSettingsTab,
    onSelectTab,
    onConfig,
}) {
    const [form, setForm] = useState(null);
    const [saved, setSaved] = useState(null);
    const [preview, setPreview] = useState("");
    const [path, setPath] = useState("~/.config/agento11y/config.env");
    const [error, setError] = useState(null);
    const [toast, setToast] = useState(null);
    const toastTimer = useRef(null);

    const showToast = useCallback((msg) => {
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
        setPreview(config.preview || "");
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
            fetch("/api/v1/config:preview", {
                method: "POST",
                headers: { "Content-Type": "application/json" },
                body: JSON.stringify({ settings: form }),
                signal: controller.signal,
            })
                .then((r) => (r.ok ? r.json() : null))
                .then((b) => {
                    if (!ignore && b && typeof b.preview === "string")
                        setPreview(b.preview);
                })
                .catch(() => {});
        }, 180);
        return () => {
            ignore = true;
            controller.abort();
            clearTimeout(t);
        };
    }, [form]);

    const page = {
        maxWidth: 1360,
        margin: "0 auto",
        padding: "28px 24px 110px",
        width: "100%",
    };
    if (!form) {
        return (
            <div style={page}>
                {configError ? (
                    <Notice kind="error" title="Failed to load settings">
                        {configError}
                    </Notice>
                ) : (
                    <Notice kind="info" title="Loading settings…">
                        Reading config.env.
                    </Notice>
                )}
            </div>
        );
    }

    const dirty = !sameSettings(form, saved);
    const set = (patch) => setForm((f) => ({ ...f, ...patch }));
    // A failed poll drops the hero stat and the Cloud status line to Unknown,
    // the way it drops the header chip. The form keeps hydrating from the
    // last good response.
    const liveConfig = configError ? null : config;
    const setTag = (i, patch) =>
        setForm((f) => ({
            ...f,
            tags: f.tags.map((t, j) => (j === i ? { ...t, ...patch } : t)),
        }));
    const addTag = () =>
        setForm((f) => ({ ...f, tags: [...f.tags, { key: "", value: "" }] }));
    const removeTag = (i) =>
        setForm((f) => ({ ...f, tags: f.tags.filter((_, j) => j !== i) }));
    const reset = () => setForm(cloneSettings(saved));

    // persist writes a whole settings object and adopts the response as both
    // the form and the saved snapshot, so the unsaved-changes bar stays down.
    // Connect, Disconnect and the forwarding mode switch each write through it
    // instead of raising that bar for a choice the user already made.
    const persist = (next, msg) => {
        setError(null);
        return fetch("/api/v1/config", {
            method: "PUT",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ settings: next }),
        })
            .then((r) =>
                r.ok
                    ? r.json()
                    : r
                          .text()
                          .then((t) =>
                              Promise.reject(
                                  new Error(t || `HTTP ${r.status}`),
                              ),
                          ),
            )
            .then((body) => {
                setForm(cloneSettings(body.settings));
                setSaved(cloneSettings(body.settings));
                if (typeof body.preview === "string") setPreview(body.preview);
                onConfig(body);
                showToast(msg);
            })
            .catch((e) => setError(String(e.message || e)));
    };
    const save = () => persist(form, "Settings saved to config.env.");

    // oneClickWrite is how the Cloud controls write: the patch goes on top of
    // the saved state, not the form, so a click that names the forwarding mode
    // cannot also commit an edit staged elsewhere. A token Reset waiting in the
    // Edit connection disclosure would otherwise be written by it, deleting a
    // credential with no confirmation and a toast naming something else.
    //
    // Whatever was being edited, minus the fields the patch owns, is put back
    // on top of the response, so it stays pending in the unsaved-changes bar.
    const oneClickWrite = (patch, msg) => {
        const pending = pendingEdits(form, saved, patch);
        return persist({ ...saved, ...patch }, msg).then(() => {
            if (pending) setForm((f) => ({ ...f, ...pending }));
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
    const connect = (parsed, mode) =>
        oneClickWrite(
            {
                endpoint: parsed.endpoint,
                tenantId: parsed.tenantId,
                token: parsed.token,
                tokenCleared: false,
                otlpEndpoint: parsed.otlpEndpoint || "",
                otlpHeaders: parsed.otlpHeaders || "",
                otlpHeadersCleared: !parsed.otlpHeaders,
                localForward: true,
                ...(captureForwardMode(saved.capture) === mode
                    ? {}
                    : { capture: mode }),
            },
            "Connected. Saved to config.env.",
        );

    // disconnect clears the connection and stops forwarding. capture is left
    // alone: CONTENT_CAPTURE_MODE also governs non-local Cloud sessions, so
    // clearing it would change a setting the user did not touch.
    const disconnect = () =>
        oneClickWrite(
            {
                endpoint: "",
                tenantId: "",
                token: "",
                tokenCleared: true,
                otlpEndpoint: "",
                otlpHeaders: "",
                otlpHeadersCleared: true,
                localForward: false,
            },
            "Disconnected. Credentials cleared from config.env.",
        );

    // forceLocalOff is the Local-only click the Cloud tab sends when the daemon
    // is still forwarding from its own environment: nothing changes in the
    // form, and config.env still needs the explicit false that overrides it.
    const commitMode = (mode, forceLocalOff = false) => {
        const patch =
            forwardLocalPatch(saved, mode) ||
            (forceLocalOff && mode === "off" ? { localForward: false } : null);
        if (!patch) return;
        oneClickWrite(
            patch,
            mode === "off"
                ? "Forwarding turned off. Saved to config.env."
                : `Forwarding set to ${mode === "full" ? "full" : "metadata only"}. Saved to config.env.`,
        );
    };
    const copy = () => {
        if (navigator.clipboard && navigator.clipboard.writeText) {
            navigator.clipboard
                .writeText(preview)
                .then(() => showToast("Copied to clipboard."))
                .catch(() => {});
        }
    };
    return (
        <div style={page}>
            <SettingsHero dirty={dirty} path={path} config={liveConfig} />

            {error && (
                <div style={{ marginBottom: 16 }}>
                    <Notice kind="error" title="Couldn't save settings">
                        {error}
                    </Notice>
                </div>
            )}

            <div
                style={{
                    display: "flex",
                    gap: 24,
                    alignItems: "flex-start",
                    flexWrap: "wrap",
                }}
            >
                <div style={{ flex: "999 1 560px", minWidth: 0 }}>
                    <SettingsTabRail
                        tabs={SETTINGS_TABS}
                        active={activeSettingsTab}
                        onChange={onSelectTab}
                    />
                    <SettingsTabPanels
                        activeSettingsTab={activeSettingsTab}
                        form={form}
                        set={set}
                        savedEndpoint={saved.endpoint}
                        setTag={setTag}
                        addTag={addTag}
                        removeTag={removeTag}
                        config={liveConfig}
                        stackUrl={(config && config.stackUrl) || ""}
                        configured={cloudConfigured(saved)}
                        configPath={path}
                        onConnect={connect}
                        onDisconnect={disconnect}
                        onMode={commitMode}
                        history={history}
                    />
                </div>

                <SettingsPreviewPanel
                    path={path}
                    preview={preview}
                    onCopy={copy}
                />
            </div>

            {dirty && <UnsavedBar onReset={reset} onSave={save} />}
            {toast && <Toast message={toast} />}
        </div>
    );
}

// ============================================================
// App container — fetches from the daemon and routes between views.
// ============================================================

function conversationIDFromPath() {
    if (typeof window === "undefined") return null;
    const prefix = "/conversations/";
    if (!window.location.pathname.startsWith(prefix)) return null;
    const raw = window.location.pathname
        .slice(prefix.length)
        .replace(/\/$/, "");
    if (!raw) return null;
    try {
        return decodeURIComponent(raw);
    } catch (_) {
        return raw;
    }
}

function conversationPath(id) {
    return `/conversations/${encodeURIComponent(id)}`;
}

function conversationGenerationPath(id, generationID) {
    const url = new URL(conversationPath(id), window.location.origin);
    if (generationID) url.hash = generationID;
    return url.pathname + url.hash;
}

function generationIDFromHash() {
    const raw = (window.location.hash || "").replace(/^#/, "");
    if (!raw) return "";
    try {
        return decodeURIComponent(raw);
    } catch {
        return raw;
    }
}

// settingsRouteActive reports whether the URL is the Settings tab.
function settingsRouteActive() {
    if (typeof window === "undefined") return false;
    return window.location.pathname.replace(/\/$/, "") === "/settings";
}

// Returns true for a plain primary-button click with no modifier keys.
// Lets cmd/ctrl/shift/alt/middle-click fall through to the browser so
// anchors can open in a new tab / window / background tab as expected.
function isPlainLeftClick(e) {
    return (
        e.button === 0 && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey
    );
}

function summaryFromDetail(detail, id) {
    const generations = detail?.generations || [];
    const agents = new Set();
    const models = new Set();
    let startedAt = null;
    let lastActivity = null;
    let totalTokens = 0;
    let hasError = false;

    for (const g of generations) {
        if (g.agent_name) agents.add(g.agent_name);
        if (g.model) models.add(g.model);
        totalTokens += g.total_tokens || 0;
        if (g.call_error) hasError = true;

        const start = conversationTime({ last_activity: g.started_at });
        if (start != null && (startedAt == null || start < startedAt))
            startedAt = start;
        const end = conversationTime({
            last_activity: g.completed_at || g.started_at,
        });
        if (end != null && (lastActivity == null || end > lastActivity))
            lastActivity = end;
    }

    return {
        id,
        title: detail?.title || id,
        started_at:
            startedAt == null ? null : new Date(startedAt).toISOString(),
        last_activity:
            lastActivity == null ? null : new Date(lastActivity).toISOString(),
        calls: generations.length,
        total_tokens: totalTokens,
        agents: Array.from(agents).sort(),
        models: Array.from(models).sort(),
        status: hasError ? "err" : "ok",
    };
}

// usePersistedState is useState mirrored into localStorage (string
// values only, plain values — no updater functions) so viewer
// preferences survive reloads. accept guards against stale or
// foreign stored values; storage errors (private mode, disabled)
// fall back to in-memory state.
function usePersistedState(key, initial, accept) {
    const [value, setValue] = useState(() => {
        try {
            const raw = window.localStorage.getItem(key);
            return raw != null && accept(raw) ? raw : initial;
        } catch (_) {
            return initial;
        }
    });
    const set = useCallback(
        (v) => {
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

function Switch({ checked, onChange, size = "md", title }) {
    const w = size === "sm" ? 28 : 34,
        h = size === "sm" ? 16 : 20,
        knob = h - 4;
    return (
        <button
            type="button"
            role="switch"
            aria-checked={checked}
            title={title}
            onClick={(e) => {
                e.stopPropagation();
                onChange(!checked);
            }}
            style={{
                width: w,
                height: h,
                flexShrink: 0,
                borderRadius: 9999,
                border: "none",
                cursor: "pointer",
                padding: 0,
                background: checked
                    ? "var(--primary-main)"
                    : "rgba(204,204,220,0.20)",
                position: "relative",
                transition: "background 120ms ease",
            }}
        >
            <span
                style={{
                    position: "absolute",
                    top: 2,
                    left: checked ? w - knob - 2 : 2,
                    width: knob,
                    height: knob,
                    borderRadius: "50%",
                    background: "#fff",
                    transition: "left 120ms ease",
                }}
            />
        </button>
    );
}

const BADGE_TONES = {
    block: {
        bg: "var(--error-transparent)",
        fg: "var(--error-text)",
        bd: "var(--error-border)",
    },
    redact: {
        bg: "var(--warning-transparent)",
        fg: "var(--warning-text)",
        bd: "var(--warning-border)",
    },
    regex: {
        bg: "var(--info-transparent)",
        fg: "var(--primary-text)",
        bd: "var(--primary-text)",
    },
    cloud: {
        bg: "rgba(204,204,220,0.06)",
        fg: "var(--fg2)",
        bd: "var(--border-medium)",
    },
    preflight: {
        bg: "transparent",
        fg: "var(--warning-text)",
        bd: "var(--warning-border)",
    },
};
function Badge({ tone = "cloud", children }) {
    const t = BADGE_TONES[tone] || BADGE_TONES.cloud;
    return (
        <span
            style={{
                display: "inline-flex",
                alignItems: "center",
                height: 16,
                padding: "0 6px",
                borderRadius: 2,
                background: t.bg,
                color: t.fg,
                border: `1px solid ${t.bd}`,
                fontSize: 9.5,
                letterSpacing: "0.06em",
                textTransform: "uppercase",
                fontFamily: "var(--fontFamilyMonospace)",
                whiteSpace: "nowrap",
            }}
        >
            {children}
        </span>
    );
}
function btnStyle(kind) {
    const base = {
        display: "inline-flex",
        alignItems: "center",
        justifyContent: "center",
        gap: 6,
        height: 32,
        padding: "0 12px",
        borderRadius: 8,
        fontSize: 12.5,
        fontWeight: 500,
        fontFamily: "var(--fontFamily)",
        whiteSpace: "nowrap",
    };
    if (kind === "primary")
        return {
            ...base,
            background: "var(--primary-main)",
            color: "#fff",
            border: "1px solid var(--primary-border)",
        };
    if (kind === "danger")
        return {
            ...base,
            background: "transparent",
            color: "var(--error-text)",
            border: "1px solid var(--error-border)",
        };
    return {
        ...base,
        background: "rgba(17,18,23,0.30)",
        color: "var(--fg1)",
        border: "1px solid var(--border-medium)",
    };
}
function Button({
    kind = "secondary",
    icon,
    children,
    disabled,
    onClick,
    title,
    style,
}) {
    return (
        <button
            type="button"
            title={title}
            disabled={disabled}
            onClick={onClick}
            style={{
                ...btnStyle(kind),
                opacity: disabled ? 0.45 : 1,
                cursor: disabled ? "not-allowed" : "pointer",
                ...(style || {}),
            }}
        >
            {icon && <Icon name={icon} size={13} />}
            {children}
        </button>
    );
}

const fieldInput = {
    width: "100%",
    height: 34,
    padding: "0 10px",
    border: "1px solid var(--border-medium)",
    borderRadius: 2,
    background: "var(--bg-canvas)",
    color: "var(--fg1)",
    fontSize: 13,
    fontFamily: "var(--fontFamily)",
    outline: "none",
};
const monoInput = {
    ...fieldInput,
    fontFamily: "var(--fontFamilyMonospace)",
    fontSize: 12,
};
const sectionLabel = {
    display: "block",
    fontSize: 11,
    color: "var(--fg3)",
    textTransform: "uppercase",
    letterSpacing: "0.06em",
    marginBottom: 7,
};

function FieldLabel({ children, hint }) {
    return (
        <label style={sectionLabel}>
            {children}
            {hint && (
                <span
                    style={{
                        textTransform: "none",
                        letterSpacing: 0,
                        color: "var(--fg3)",
                        marginLeft: 8,
                        fontSize: 11,
                    }}
                >
                    {hint}
                </span>
            )}
        </label>
    );
}

function Section({ title, children, defaultOpen = true }) {
    const [open, setOpen] = useState(defaultOpen);
    return (
        <div
            style={{
                border: "1px solid var(--border-weak)",
                borderRadius: 2,
                marginBottom: 12,
            }}
        >
            <button
                type="button"
                onClick={() => setOpen((o) => !o)}
                style={{
                    display: "flex",
                    alignItems: "center",
                    gap: 8,
                    width: "100%",
                    padding: "10px 12px",
                    background: "transparent",
                    border: "none",
                    cursor: "pointer",
                    color: "var(--fg1)",
                }}
            >
                <Icon
                    name={open ? "chevron" : "cright"}
                    size={12}
                    style={{ color: "var(--fg3)" }}
                />
                <span
                    style={{
                        fontSize: 12,
                        fontWeight: 500,
                        textTransform: "uppercase",
                        letterSpacing: "0.06em",
                        color: "var(--fg2)",
                    }}
                >
                    {title}
                </span>
            </button>
            {open && <div style={{ padding: "4px 14px 16px" }}>{children}</div>}
        </div>
    );
}

// SEARCH_DEBOUNCE_MS controls how long after the last keystroke the
// viewer waits before issuing the search request. 320ms matches the
// upper end of the design handoff's 320–340ms window: snappy enough to
// feel live, slow enough to coalesce typing into one network call.
const SEARCH_DEBOUNCE_MS = 320;

// REFRESH_DEBOUNCE_MS is how long a burst of change events is collected
// before the conversation list and the token chart refetch.
const REFRESH_DEBOUNCE_MS = 250;
// IMPORT_REFRESH_DEBOUNCE_MS replaces it for the list and the chart while a
// history import runs. An import appends to thousands of conversations, so
// a refresh at the normal cadence shows a list that is stale again before
// it renders. The progress banner carries the detail; the list only has to
// stay roughly current. An open conversation keeps the normal cadence: it
// is what the user is reading.
const IMPORT_REFRESH_DEBOUNCE_MS = 2000;
// IMPORT_ACTIVE_TTL_MS is how long one import frame holds the slower
// cadence. The event stream is lossy: the daemon drops frames for a
// subscriber that falls behind, and a reconnect replays nothing. So a run
// has to expire rather than latch, or one lost terminal frame leaves the
// tab slow until a reload. A running import publishes progress every
// 250ms, so this much slack cannot expire it mid-run.
const IMPORT_ACTIVE_TTL_MS = 10_000;

// escapeRegExp escapes the special-meaning characters in a query token
// before splicing into the alternation regex that drives highlighting.
// Keeps a literal "a+b" highlighting "a+b" rather than "aab".
function escapeRegExp(s) {
    return s.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

// highlightTerms renders `text` with each whitespace-separated term in
// `query` wrapped in a `<mark>` carrying the warm-wash style. Tokens
// are lower-cased and deduped (so "rate rate" doesn't compile a
// doubled alternation), and the split keeps the original text casing
// outside the matches.
function highlightTerms(text, query) {
    if (!text) return text;
    const terms = String(query || "")
        .toLowerCase()
        .split(/\s+/)
        .filter(Boolean);
    if (terms.length === 0) return text;
    const uniq = Array.from(new Set(terms)).map(escapeRegExp);
    const re = new RegExp(`(${uniq.join("|")})`, "ig");
    const parts = String(text).split(re);
    const wash = {
        color: "var(--fg-max)",
        fontWeight: 500,
        background: "rgba(245,183,61,0.18)",
        boxShadow: "inset 0 -1px 0 rgba(245,183,61,0.45)",
        borderRadius: 2,
        padding: "0 2px",
    };
    return parts.map((part, i) => {
        if (!part) return null;
        return re.test(part) ? (
            <mark key={i} style={wash}>
                {part}
            </mark>
        ) : (
            <React.Fragment key={i}>{part}</React.Fragment>
        );
    });
}

// SearchResultRow is one ranked hit. Stays consistent with ConvRow's dense
// mono grid and agent/model pills, and adds a two-line clamp on the snippet.
// The row is a real anchor so cmd/ctrl-click opens in a new tab without
// us re-implementing the browser.
function SearchResultRow({ hit, now, query, selected, onSelect, onOpen }) {
    const ago = hit.last_activity ? formatAgo(hit.last_activity, now) : "";
    const titleEl = highlightTerms(hit.title || hit.id, query);
    const snippetEl = highlightTerms(hit.snippet || "", query);
    const matchCount = hit.match_count || 0;
    return (
        <a
            href={conversationPath(hit.id)}
            onMouseEnter={onSelect}
            onClick={(e) => {
                if (!isPlainLeftClick(e)) return;
                e.preventDefault();
                onOpen(hit);
            }}
            style={{
                display: "block",
                padding: "11px 16px 12px",
                borderBottom: "1px solid var(--border-weak)",
                background: selected ? "rgba(204,204,220,0.06)" : "transparent",
                cursor: "pointer",
                textDecoration: "none",
                color: "inherit",
                transition: "background 80ms ease",
            }}
            onMouseOver={(e) => {
                if (!selected)
                    e.currentTarget.style.background = "var(--row-hover)";
            }}
            onMouseOut={(e) => {
                if (!selected) e.currentTarget.style.background = "transparent";
            }}
        >
            <div
                style={{
                    display: "grid",
                    gridTemplateColumns: "76px minmax(0,1fr) auto",
                    columnGap: 16,
                    alignItems: "baseline",
                }}
            >
                <span
                    style={{
                        display: "inline-flex",
                        alignItems: "baseline",
                        gap: 6,
                        color: "var(--fg3)",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 12,
                    }}
                >
                    {/* The row carries no ERR badge, so this dot is its only
                        failure marker. Status is "err" when any generation in
                        the conversation recorded a CallError, "ok" otherwise.
                        See searchConversationFile in internal/local/search.go. */}
                    {hit.status === "err" && (
                        <span
                            title="Failed model call"
                            style={{
                                width: 5,
                                height: 5,
                                borderRadius: "50%",
                                flex: "none",
                                background: "var(--error-main)",
                            }}
                        />
                    )}
                    {ago}
                </span>
                <div
                    style={{
                        display: "flex",
                        alignItems: "baseline",
                        gap: 8,
                        flexWrap: "wrap",
                        minWidth: 0,
                    }}
                >
                    <span
                        style={{
                            fontFamily: "var(--fontFamily)",
                            fontSize: 14,
                            fontWeight: 500,
                            color: "var(--fg1)",
                        }}
                    >
                        {titleEl}
                    </span>
                    {hit.title && hit.title !== hit.id && (
                        <span
                            style={{
                                fontFamily: "var(--fontFamilyMonospace)",
                                fontSize: 11,
                                color: "var(--fg3)",
                            }}
                        >
                            {hit.id}
                        </span>
                    )}
                </div>
                <div
                    style={{
                        display: "inline-flex",
                        alignItems: "center",
                        gap: 10,
                        color: "var(--fg2)",
                        fontFamily: "var(--fontFamilyMonospace)",
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
                        {formatTokens(hit.total_tokens)} · {hit.calls}{" "}
                        {hit.calls === 1 ? "call" : "calls"}
                    </span>
                </div>
            </div>
            <div
                style={{
                    display: "grid",
                    gridTemplateColumns: "76px minmax(0,1fr)",
                    columnGap: 16,
                    marginTop: 7,
                }}
            >
                <span />
                <div
                    style={{
                        display: "-webkit-box",
                        WebkitLineClamp: 2,
                        WebkitBoxOrient: "vertical",
                        overflow: "hidden",
                        fontFamily: "var(--fontFamilyMonospace)",
                        fontSize: 12,
                        color: "var(--fg2)",
                        lineHeight: 1.5,
                    }}
                >
                    <span
                        style={{
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 11,
                            color: "var(--warning-main)",
                            background: "rgba(245,183,61,0.10)",
                            border: "1px solid rgba(245,183,61,0.30)",
                            borderRadius: 2,
                            padding: "0 5px",
                            marginRight: 8,
                        }}
                    >
                        {matchCount} {matchCount === 1 ? "match" : "matches"}
                    </span>
                    {hit.snippet ? (
                        <>…{snippetEl}</>
                    ) : (
                        <span style={{ color: "var(--fg3)" }}>
                            No preview available.
                        </span>
                    )}
                </div>
            </div>
        </a>
    );
}

function useSearchResults(query) {
    const [phase, setPhase] = useState("done"); // "done" | "loading"
    const [hits, setHits] = useState([]);
    const [mode, setMode] = useState("fts");
    const [error, setError] = useState(null);
    const [selectedIndex, setSelectedIndex] = useState(-1);
    const [retryNonce, setRetryNonce] = useState(0);
    const debounceRef = useRef(null);
    const abortRef = useRef(null);
    const trimmed = query.trim();

    useEffect(() => {
        setSelectedIndex(-1);
    }, [query]);

    useEffect(() => {
        if (debounceRef.current) clearTimeout(debounceRef.current);
        if (abortRef.current) abortRef.current.abort();

        if (!trimmed) {
            setPhase("done");
            setHits([]);
            setError(null);
            return undefined;
        }
        setPhase("loading");
        setError(null);
        const controller = new AbortController();
        abortRef.current = controller;
        const timer = setTimeout(() => {
            fetch(`/api/v1/search?q=${encodeURIComponent(trimmed)}`, {
                signal: controller.signal,
            })
                .then((r) =>
                    r.ok
                        ? r.json()
                        : r
                              .text()
                              .then((t) =>
                                  Promise.reject(
                                      new Error(t || `HTTP ${r.status}`),
                                  ),
                              ),
                )
                .then((body) => {
                    setHits(Array.isArray(body.hits) ? body.hits : []);
                    setMode(body.mode || "fts");
                    setPhase("done");
                })
                .catch((e) => {
                    if (e && e.name === "AbortError") return;
                    setError(String(e.message || e));
                    setPhase("done");
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

function ConversationSearchPanel({
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
}) {
    const showResults = !!query && !error;
    const showNoResults = showResults && phase === "done" && hits.length === 0;
    const showLoadingSkeleton =
        showResults && phase === "loading" && hits.length === 0;

    return (
        <SurfaceCard
            style={{
                overflow: "hidden",
                opacity: phase === "loading" && hits.length > 0 ? 0.55 : 1,
                transition: "opacity 120ms ease",
            }}
        >
            {error && (
                <div
                    style={{
                        margin: 12,
                        padding: "12px 14px",
                        border: "1px solid var(--error-border)",
                        background: "var(--error-transparent)",
                        borderRadius: 2,
                        display: "flex",
                        alignItems: "flex-start",
                        gap: 11,
                    }}
                >
                    <Icon
                        name="alert"
                        size={16}
                        style={{ color: "var(--error-text)", marginTop: 2 }}
                    />
                    <div style={{ flex: 1 }}>
                        <div style={{ fontSize: 14, color: "var(--fg1)" }}>
                            Couldn't run the search.
                        </div>
                        <div
                            style={{
                                fontSize: 13,
                                color: "var(--fg2)",
                                marginTop: 3,
                            }}
                        >
                            The local viewer didn't respond. Check that{" "}
                            <span
                                style={{
                                    fontFamily: "var(--fontFamilyMonospace)",
                                }}
                            >
                                agento11y --local
                            </span>{" "}
                            is running, then try again.
                        </div>
                    </div>
                    <button
                        type="button"
                        onClick={retry}
                        style={{
                            height: 28,
                            padding: "0 12px",
                            background: "transparent",
                            border: "1px solid var(--border-medium)",
                            borderRadius: 2,
                            color: "var(--fg1)",
                            fontSize: 12,
                            cursor: "pointer",
                        }}
                        onMouseEnter={(e) =>
                            (e.currentTarget.style.background =
                                "var(--action-hover)")
                        }
                        onMouseLeave={(e) =>
                            (e.currentTarget.style.background = "transparent")
                        }
                    >
                        Retry
                    </button>
                </div>
            )}

            {!error && showResults && hits.length > 0 && (
                <React.Fragment>
                    <div
                        style={{
                            display: "flex",
                            alignItems: "center",
                            padding: "9px 16px",
                            borderBottom: "1px solid var(--border-weak)",
                            fontFamily: "var(--fontFamilyMonospace)",
                            fontSize: 12,
                            color: "var(--fg3)",
                        }}
                    >
                        <span>
                            {hits.length}{" "}
                            {hits.length === 1 ? "result" : "results"}
                        </span>
                        <span style={{ flex: 1 }} />
                        <span style={{ fontSize: 11, opacity: 0.7 }}>
                            ranked by{" "}
                            {mode === "semantic"
                                ? "relevance (qmd)"
                                : "matches"}
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
                </React.Fragment>
            )}

            {!error && showLoadingSkeleton && (
                <React.Fragment>
                    {[0, 1, 2].map((i) => (
                        <div
                            key={i}
                            style={{
                                padding: "14px 16px",
                                borderBottom:
                                    i < 2
                                        ? "1px solid var(--border-weak)"
                                        : "none",
                            }}
                        >
                            <div
                                className="sigil-shim"
                                style={{
                                    height: 14,
                                    width: "40%",
                                    borderRadius: 2,
                                }}
                            />
                            <div
                                className="sigil-shim"
                                style={{
                                    height: 10,
                                    width: "80%",
                                    borderRadius: 2,
                                    marginTop: 8,
                                }}
                            />
                        </div>
                    ))}
                </React.Fragment>
            )}

            {!error && showNoResults && (
                <div style={{ padding: "34px 16px 36px" }}>
                    <div style={{ fontSize: 14, color: "var(--fg1)" }}>
                        No matches for{" "}
                        <span
                            style={{
                                fontFamily: "var(--fontFamilyMonospace)",
                                color: "var(--fg-max)",
                            }}
                        >
                            “{query}”
                        </span>
                        .
                    </div>
                    <div
                        style={{
                            fontSize: 13,
                            color: "var(--fg3)",
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

function App() {
    const [selectedID, setSelectedID] = useState(conversationIDFromPath);
    const [showSettings, setShowSettings] = useState(settingsRouteActive);
    // The settings tab is part of the route, so it is owned here with the rest
    // of it: the header chip opens the Cloud tab from any view, including from
    // Settings itself, where pushState alone leaves the panel where it was.
    const [settingsTab, setSettingsTab] = useState(settingsTabFromLocation);
    const [conversations, setConversations] = useState([]);
    // storeCount is the number of conversations the daemon holds, before
    // the list's page and range bounds. The list itself is range-scoped,
    // so only this count distinguishes an empty store from a quiet range.
    const [storeCount, setStoreCount] = useState(null);
    const [tokenPoints, setTokenPoints] = useState([]);
    const [tokenIntervalMs, setTokenIntervalMs] = useState(0);
    const [loadingList, setLoadingList] = useState(true);
    const [errList, setErrList] = useState(null);
    const [query, setQuery] = useState("");
    const conversationSearchRef = useRef(null);
    const [timeRange, setTimeRange] = usePersistedState(
        "sigil.local.timeRange",
        DEFAULT_TIME_RANGE,
        (v) => TIME_RANGES.some((r) => r.value === v),
    );
    const [tokenModel, setTokenModel] = useState("all");
    const [chartMetric, setChartMetric] = usePersistedState(
        "sigil.local.chartMetric",
        "tokens",
        (v) => v === "tokens" || v === "activity",
    );
    const [bucketSel, setBucketSel] = useState(null);
    const [workspace, setWorkspace] = useState(null);
    const [groupBy, setGroupBy] = usePersistedState(
        "sigil.local.groupBy",
        "workspace",
        (value) => GROUP_BY_OPTIONS.some((option) => option.value === value),
    );
    const [listSort, setListSort] = useState({
        key: "last_activity",
        dir: "desc",
    });

    const [detail, setDetail] = useState(null);
    const [loadingDetail, setLoadingDetail] = useState(false);
    const [errDetail, setErrDetail] = useState(null);
    // Import-run state arrives on the SSE stream as its own event kind, not
    // as a refetch hint: an import writes thousands of generations, and the
    // counters are what the user watches while it runs.
    const [importEvent, setImportEvent] = useState(null);
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
    const [config, setConfig] = useState(null);
    const [configErr, setConfigErr] = useState(null);
    const configSeqRef = useRef(0);
    const applyConfig = useCallback((body) => {
        configSeqRef.current++;
        setConfig(body);
        setConfigErr(null);
    }, []);
    const loadConfig = useCallback(() => {
        const seq = ++configSeqRef.current;
        fetch("/api/v1/config")
            .then((r) =>
                r.ok
                    ? r.json()
                    : r
                          .text()
                          .then((t) =>
                              Promise.reject(
                                  new Error(t || `HTTP ${r.status}`),
                              ),
                          ),
            )
            .then((body) => {
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

    const view = showSettings
        ? "settings"
        : selectedID
          ? "conversation"
          : "conversations";
    const selected = selectedID
        ? conversations.find((c) => c.id === selectedID) ||
          summaryFromDetail(detail, selectedID)
        : null;

    // Changing the time range invalidates a bucket drill-down: the
    // bucket boundaries belong to the old window.
    const changeTimeRange = useCallback(
        (v) => {
            setBucketSel(null);
            setTimeRange(v);
        },
        [setTimeRange],
    );

    const pageTitle =
        view === "settings"
            ? "Settings · agento11y local"
            : view === "conversation" && selected
              ? `${selected.title || selected.id} · agento11y local`
              : "agento11y · local";
    useEffect(() => {
        document.title = pageTitle;
    }, [pageTitle]);

    // Opening Settings re-reads config.env: the form hydrates from the polled
    // response, which is otherwise up to 30s old, and the panel it picks
    // depends on whether a connection is saved.
    useEffect(() => {
        if (view === "settings") loadConfig();
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
            // Bound the request: the server pages the list by file modification
            // time and never decodes past the page, so the cost follows the page
            // size, not the store size.
            const w = requestWindow(timeRange, LIST_PAGE_SIZE);
            const params = new URLSearchParams({ limit: String(w.limit) });
            if (w.since) params.set("since", w.since);
            return fetch(`/api/v1/conversations?${params}`)
                .then((r) =>
                    r.ok
                        ? r.json()
                        : r
                              .text()
                              .then((t) =>
                                  Promise.reject(
                                      new Error(t || `HTTP ${r.status}`),
                                  ),
                              ),
                )
                .then((body) => {
                    if (listSeqRef.current !== seq) return;
                    setConversations(body.conversations || []);
                    setStoreCount(
                        Number.isFinite(body.total_conversations)
                            ? body.total_conversations
                            : null,
                    );
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
        [timeRange],
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
            if (w.since) params.set("since", w.since);
            if (w.intervalSec) params.set("interval", String(w.intervalSec));
            const query = params.toString();
            return fetch(`/api/v1/metrics/tokens${query ? `?${query}` : ""}`)
                .then((r) => (r.ok ? r.json() : null))
                .then((body) => {
                    if (!body || tokenSeqRef.current !== seq) return;
                    setTokenPoints(body.points || []);
                    // The chart never draws a bar finer than the width the server
                    // aggregated on, so read it back instead of assuming the
                    // requested one ("All" requests none).
                    const seconds = Number(body.interval_seconds);
                    setTokenIntervalMs(
                        Number.isFinite(seconds) && seconds > 0
                            ? seconds * 1000
                            : 0,
                    );
                })
                .catch(() => {});
        },
        [timeRange],
    );

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
    const refreshAllRef = useRef(null);
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
            refreshAllRef.current();
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
    const fetchDetailCore = useCallback((id, quiet) => {
        if (!quiet) {
            setLoadingDetail(true);
            setErrDetail(null);
            setDetail(null);
        }
        return fetch(`/api/v1/conversations/${encodeURIComponent(id)}`)
            .then((r) => {
                if (r.status === 404)
                    throw new Error("Session not found in the local store.");
                if (!r.ok)
                    return r
                        .text()
                        .then((t) =>
                            Promise.reject(new Error(t || `HTTP ${r.status}`)),
                        );
                return r.json();
            })
            .then((body) => {
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
    const fetchDetail = useCallback(
        (id) => fetchDetailCore(id, false),
        [fetchDetailCore],
    );
    const quietRefreshDetail = useCallback(
        (id) => fetchDetailCore(id, true),
        [fetchDetailCore],
    );

    useEffect(() => {
        reloadRange();
    }, [reloadRange]);

    useEffect(() => {
        const onPopState = () => {
            setSelectedID(conversationIDFromPath());
            setShowSettings(settingsRouteActive());
            setSettingsTab(settingsTabFromLocation());
        };
        window.addEventListener("popstate", onPopState);
        return () => window.removeEventListener("popstate", onPopState);
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
        if (typeof EventSource === "undefined") return;
        // Debounce so a burst export (one frame per generation) does
        // not trigger one refresh per frame. We only need one list
        // refresh per burst, plus one detail refresh if any event in
        // the burst targets the open conversation. The two are armed
        // separately because an import slows the list down and the open
        // conversation keeps the normal cadence.
        let listTimer = null;
        let detailTimer = null;
        // onConversations reports whether the list or a conversation is
        // rendered. Elsewhere neither is, and the SSE connection itself is
        // cheap to leave running.
        const onConversations = () => {
            const v = viewRef.current;
            return v === "conversations" || v === "conversation";
        };
        const importActive = () => importActiveUntilRef.current > Date.now();
        const flushList = () => {
            listTimer = null;
            if (!onConversations()) return;
            refreshAllRef.current();
        };
        const flushDetail = () => {
            detailTimer = null;
            const openID = selectedIDRef.current;
            if (!openID || !onConversations()) return;
            quietRefreshDetailRef.current(openID);
        };
        const es = new EventSource("/api/v1/events");
        es.onmessage = (e) => {
            let ev = {};
            try {
                ev = JSON.parse(e.data || "{}");
            } catch (_) {
                /* ignore */
            }
            if (ev && ev.import) {
                // Import events carry state rather than naming a conversation, so
                // they are applied directly and skip the refetch debounce.
                const active = importRunIsActive(ev.import);
                importActiveUntilRef.current = active
                    ? Date.now() + IMPORT_ACTIVE_TTL_MS
                    : 0;
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
            if (
                openID &&
                ev &&
                ev.conversation_id === openID &&
                detailTimer === null
            ) {
                detailTimer = setTimeout(flushDetail, REFRESH_DEBOUNCE_MS);
            }
            if (listTimer === null) {
                listTimer = setTimeout(
                    flushList,
                    importActive()
                        ? IMPORT_REFRESH_DEBOUNCE_MS
                        : REFRESH_DEBOUNCE_MS,
                );
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
        if (view !== "conversations") return;
        const id = setInterval(() => refreshAllRef.current(), 60_000);
        return () => clearInterval(id);
    }, [view]);

    const openConv = (c) => {
        window.history.pushState({}, "", conversationPath(c.id));
        setShowSettings(false);
        setSelectedID(c.id);
    };
    const goConversations = () => {
        window.history.pushState({}, "", "/");
        setShowSettings(false);
        setSelectedID(null);
    };
    // goSettings is also the nav tab's onClick, which passes an event, so
    // anything that is not a tab id opens the Cloud tab.
    const goSettings = (tab) => {
        const next = SETTINGS_TAB_IDS.has(tab) ? tab : "cloud";
        window.history.pushState({}, "", settingsPath(next));
        setSelectedID(null);
        setShowSettings(true);
        setSettingsTab(next);
    };
    const selectSettingsTab = (tab) => {
        if (!SETTINGS_TAB_IDS.has(tab)) return;
        window.history.pushState({}, "", settingsPath(tab));
        setSettingsTab(tab);
    };
    const focusConversationSearch = useCallback(() => {
        const focus = () => {
            const el = conversationSearchRef.current;
            if (!el) return;
            el.focus();
            if (typeof el.select === "function") el.select();
        };
        if (viewRef.current !== "conversations") {
            window.history.pushState({}, "", "/");
            setSelectedID(null);
            setShowSettings(false);
            setTimeout(focus, 0);
            return;
        }
        focus();
    }, []);
    useEffect(() => {
        const onKeyDown = (e) => {
            if (
                (e.metaKey || e.ctrlKey) &&
                !e.shiftKey &&
                !e.altKey &&
                String(e.key).toLowerCase() === "k"
            ) {
                e.preventDefault();
                focusConversationSearch();
            }
        };
        window.addEventListener("keydown", onKeyDown);
        return () => window.removeEventListener("keydown", onKeyDown);
    }, [focusConversationSearch]);

    const tabs = [
        {
            key: "conversations",
            label: "Sessions",
            href: "/",
            onClick: goConversations,
        },
        {
            key: "settings",
            label: "Settings",
            href: "/settings",
            onClick: goSettings,
        },
    ];
    const activeTab = view === "settings" ? "settings" : "conversations";

    return (
        <div
            style={{
                minHeight: "100vh",
                display: "flex",
                flexDirection: "column",
            }}
        >
            {/* A failed poll drops the chip to Unknown rather than leaving it
              asserting a posture the daemon can no longer confirm. */}
            <TopBar
                tabs={tabs}
                activeTab={activeTab}
                config={configErr ? null : config}
                onOpenSettings={goSettings}
            />
            <div
                style={{
                    flex: 1,
                    display: "flex",
                    flexDirection: "column",
                    minHeight: 0,
                }}
            >
                {view === "settings" && (
                    <SettingsView
                        history={history}
                        config={config}
                        configError={configErr}
                        activeSettingsTab={settingsTab}
                        onSelectTab={selectSettingsTab}
                        onConfig={applyConfig}
                    />
                )}
                {view === "conversations" && (
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
                        setWorkspace={setWorkspace}
                        groupBy={groupBy}
                        setGroupBy={setGroupBy}
                        listSort={listSort}
                        setListSort={setListSort}
                        onOpen={openConv}
                        onRefresh={refreshAll}
                        refreshing={loadingList}
                        onOpenSettings={goSettings}
                        history={history}
                    />
                )}
                {view === "conversation" && selected && (
                    <TraceDetailView
                        conv={selected}
                        detail={detail}
                        loading={loadingDetail}
                        error={errDetail}
                        onBack={goConversations}
                    />
                )}
            </div>
        </div>
    );
}
ReactDOM.createRoot(document.getElementById("root")).render(<App />);
