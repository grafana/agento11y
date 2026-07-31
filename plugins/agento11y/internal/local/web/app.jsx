    const { useState, useEffect, useMemo, useCallback, useRef, createContext, useContext } = React;

    // ============================================================
    // Formatters — all server responses ship raw numbers + RFC3339
    // timestamps, the UI humanizes them so it can re-render relative
    // labels without re-fetching.
    // ============================================================

    function formatTokens(n) {
      if (n == null || isNaN(n)) return "—";
      if (n < 1000) return String(n);
      if (n < 1_000_000) return (n / 1_000).toFixed(n < 10_000 ? 1 : 1).replace(/\.0$/, "") + "k";
      return (n / 1_000_000).toFixed(n < 10_000_000 ? 1 : 1).replace(/\.0$/, "") + "M";
    }

    function formatDuration(seconds) {
      if (seconds == null || isNaN(seconds)) return "—";
      if (seconds < 1) return "<1s";
      if (seconds < 60) return seconds.toFixed(seconds < 10 ? 2 : 1).replace(/\.0+$/, "") + "s";
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
      if (!iso) return "—";
      const t = new Date(iso).getTime();
      if (!Number.isFinite(t)) return "—";
      const secs = Math.max(0, Math.round((now - t) / 1000));
      if (secs < 5)   return "just now";
      if (secs < 60)  return `${secs}s ago`;
      const mins = Math.round(secs / 60);
      if (mins < 60)  return `${mins}m ago`;
      const hours = Math.round(mins / 60);
      if (hours < 24) return `${hours}h ago`;
      const days = Math.round(hours / 24);
      return `${days}d ago`;
    }

    function formatTime(iso) {
      if (!iso) return "—";
      const d = new Date(iso);
      if (isNaN(d)) return "—";
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
      { value: "all", label: "All", ms: null },
    ];
    const FEED_TIME_RANGES = TIME_RANGES.filter(r => r.value !== "5m" && r.value !== "15m");

    function timeRangeOption(value) {
      return TIME_RANGES.find(r => r.value === value) || TIME_RANGES.find(r => r.value === "6h");
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
      const time = d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
      // 2h+ buckets mean the chart spans more than a day, so a bare
      // time is ambiguous — prefix the date.
      if (bucketMs >= 2 * 60 * 60 * 1000) {
        return d.toLocaleDateString([], { month: "short", day: "numeric" }) + " " + time;
      }
      // Sub-minute buckets need seconds or adjacent labels collide.
      if (bucketMs < 60 * 1000) {
        return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });
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
      { key: "fresh_input", label: "Input",       color: "var(--viz-blue)" },
      { key: "cache_read",  label: "Cache read",  color: "var(--viz-green)" },
      { key: "cache_write", label: "Cache write", color: "var(--viz-purple)" },
      { key: "output",      label: "Output",      color: "var(--viz-orange)" },
      { key: "reasoning",   label: "Reasoning",   color: "var(--viz-yellow)" },
    ];

    // tokenBreakdownTitle renders disjoint token buckets as a multi-line
    // native tooltip for the list's Tokens cell.
    function tokenBreakdownTitle(buckets) {
      if (!buckets) return undefined;
      const lines = TOKEN_SERIES.filter(s => buckets[s.key] > 0)
        .map(s => `${s.label}: ${formatTokens(buckets[s.key])}`);
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
    // better to show "—" than to price a GPT/Gemini run at Claude rates.
    // ponytail: add a row here when a provider's prices are known and stable;
    // don't guess them.
    const MODEL_PRICES = [
      { match: "fable",  in: 10, out: 50 },
      { match: "opus",   in: 5,  out: 25 },
      { match: "sonnet", in: 3,  out: 15 },
      { match: "haiku",  in: 1,  out: 5  },
    ];

    function modelPrice(model) {
      const m = (model || "").toLowerCase();
      return MODEL_PRICES.find(p => m.includes(p.match)) || null;
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
          const cached = JSON.parse(localStorage.getItem(PRICE_CACHE_KEY) || "null");
          if (cached && cached.map && Date.now() - cached.at < PRICE_TTL_MS) return cached.map;
        } catch { /* corrupt cache — refetch */ }
        const resp = await fetch(MODELS_DEV_URL);
        if (!resp.ok) throw new Error(`models.dev ${resp.status}`);
        const map = flattenModelsDev(await resp.json());
        try { localStorage.setItem(PRICE_CACHE_KEY, JSON.stringify({ at: Date.now(), map })); } catch { /* quota — skip cache */ }
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
        loadModelPrices().then(m => { if (alive) setPrices(m); }).catch(() => {});
        return () => { alive = false; };
      }, []);
      return prices;
    }

    // conversationCost prices a conversation's disjoint token buckets at its
    // primary model's rates. Prefers the live models.dev catalog (exact model
    // id, all providers); falls back to the bundled Anthropic table for
    // brand-new Claude ids or when offline. Exact for the single-model common
    // case; a mixed-model conversation is priced at models[0] (the
    // orchestrator), a close approximation. Returns null when the model can't
    // be priced (unknown provider, or no model recorded) so callers show "—"
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
        cacheReadRate = live.cache_read != null ? live.cache_read : live.input * 0.1;
        cacheWriteRate = live.cache_write != null ? live.cache_write : live.input * 1.25;
      } else {
        const p = modelPrice(model);
        if (!p) return null;
        inRate = p.in; outRate = p.out;
        cacheReadRate = p.in * 0.1; cacheWriteRate = p.in * 1.25;
      }
      return (
        (b.fresh_input || 0) * inRate +
        (b.cache_read || 0) * cacheReadRate +
        (b.cache_write || 0) * cacheWriteRate +
        ((b.output || 0) + (b.reasoning || 0)) * outRate
      ) / 1e6;
    }

    function formatCost(usd) {
      if (usd == null) return "—";       // unpriced model — distinct from $0
      if (usd === 0) return "$0";
      if (usd < 0.01) return "<$0.01";
      if (usd < 1000) return "$" + usd.toFixed(2).replace(/\.00$/, "");
      return "$" + (usd / 1000).toFixed(1) + "k";
    }

    // workspaceLabel shortens an absolute cwd to its last two path segments
    // (repo/branch-ish) for the sidebar; the full path stays in the title.
    function workspaceLabel(path) {
      if (!path) return "(unknown)";
      const parts = path.replace(/\/+$/, "").split("/").filter(Boolean);
      return parts.slice(-2).join("/") || path;
    }

    // timeWindow computes a chart's [start, end] for a range selection.
    // For "All", min/max accumulate in a loop instead of spreading into
    // Math.min/Math.max: with one entry per generation the times array
    // can be large enough that spread overflows the argument stack
    // (RangeError).
    function timeWindow(times, rangeValue, now) {
      const range = timeRangeOption(rangeValue);
      if (range.ms != null) return { start: now - range.ms, end: now };
      let minT = Infinity, maxT = -Infinity, n = 0;
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

    // bucketByTime lays out `count` equal buckets across the selected
    // range and folds every in-window item into its bucket: init seeds a
    // bucket's counters, add(bucket, item) accumulates one item. Pass
    // `window` to share one [start, end] between charts.
    function bucketByTime(items, getTime, rangeValue, now, { count = 12, init, add, window: win }) {
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
        buckets.push({ t: formatBucketLabel(bucketStart, bucketMs), start: bucketStart, end: bucketEnd, ...init() });
      }
      items.forEach((item, i) => {
        const t = times[i];
        if (!Number.isFinite(t) || t < start || t > end) return;
        add(buckets[Math.min(count - 1, Math.max(0, Math.floor((t - start) / bucketMs)))], item);
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
        add: b => { b.c += 1; },
      });
    }

    // ============================================================
    // Shell primitives
    // ============================================================

    function Icon({ name, size = 16, style, className }) {
      const paths = {
        search:   <path d="M11 19a8 8 0 1 1 5.3-2L21 21M11 19a8 8 0 0 0 5.3-2L11 19Z" />,
        chevron:  <path d="m6 9 6 6 6-6" />,
        cright:   <path d="m9 6 6 6-6 6" />,
        clock:    <><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></>,
        bolt:     <path d="M13 2 4 14h7l-1 8 9-12h-7l1-8Z"/>,
        coin:     <><circle cx="12" cy="12" r="9"/><path d="M9 9h5a2 2 0 0 1 0 4H9v-4Zm0 4v3m3-7v10"/></>,
        swap:     <path d="M7 7h13l-3-3M17 17H4l3 3"/>,
        refresh:  <path d="M3 12a9 9 0 0 1 15.5-6.3L21 8M21 3v5h-5M21 12a9 9 0 0 1-15.5 6.3L3 16M3 21v-5h5"/>,
        book:     <path d="M4 4h7a3 3 0 0 1 3 3v13a3 3 0 0 0-3-3H4V4ZM20 4h-3a3 3 0 0 0-3 3v13a3 3 0 0 1 3-3h3V4Z"/>,
        bookopen: <path d="M12 6c-2-1.3-4.5-2-7-2v13c2.5 0 5 .7 7 2 2-1.3 4.5-2 7-2V4c-2.5 0-5 .7-7 2Zm0 0v13"/>,
        box:      <path d="M3 7.5 12 3l9 4.5v9L12 21l-9-4.5v-9Zm0 0 9 4.5m0 0 9-4.5m-9 4.5V21"/>,
        dot:      <circle cx="12" cy="12" r="4"/>,
        download: <path d="M12 4v12m0 0-4-4m4 4 4-4M4 20h16"/>,
        copy:     <path d="M9 9h11v11H9zM4 4h11v3"/>,
        list:     <path d="M4 6h16M4 12h16M4 18h16"/>,
        wrench:   <path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>,
        alert:    <><path d="M12 9v4"/><circle cx="12" cy="16.5" r="0.6" fill="currentColor"/><path d="M10.3 4.1 2.7 17.4a2 2 0 0 0 1.7 3h15.2a2 2 0 0 0 1.7-3L13.7 4.1a2 2 0 0 0-3.4 0Z"/></>,
        empty:    <><circle cx="12" cy="12" r="9"/><path d="M8 12h8"/></>,
        extlink:  <path d="M7 17 17 7M9 7h8v8"/>,
        shield:   <path d="M12 3 5 6v6c0 4 3 6.5 7 9 4-2.5 7-5 7-9V6l-7-3Z"/>,
        shieldcheck: <><path d="M12 3 5 6v6c0 4 3 6.5 7 9 4-2.5 7-5 7-9V6l-7-3Z"/><path d="m9 12 2 2 4-4"/></>,
        plus:     <path d="M12 5v14M5 12h14"/>,
        play:     <path d="M7 5v14l11-7-11-7Z"/>,
        pen:      <path d="M12 20h9M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/>,
        trash:    <path d="M4 7h16M9 7V5h6v2M6 7l1 13h10l1-13"/>,
        close:    <path d="M6 6l12 12M18 6 6 18"/>,
        check:    <path d="m5 13 4 4L19 7"/>,
        info:     <><circle cx="12" cy="12" r="9"/><path d="M12 11v5"/><circle cx="12" cy="8" r="0.6" fill="currentColor"/></>,
        ban:      <><circle cx="12" cy="12" r="9"/><path d="m6 6 12 12"/></>,
        key:      <><circle cx="8" cy="15" r="3.2"/><path d="m10.3 12.7 8-8M16 5l2.5 2.5M13.5 7.5 16 10"/></>,
        times:    <path d="M6 6l12 12M18 6 6 18"/>,
        cloud:    <path d="M7 18a4 4 0 0 1-.5-7.97 5 5 0 0 1 9.6-1.37A3.5 3.5 0 0 1 16.5 18H7Z"/>,
        sparkle:  <path d="M12 3l1.5 5L18 9.5l-5 1.5L12 16l-1.5-5.5L5 9.5 10.5 8 12 3Z"/>,
      };
      return (
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none"
          stroke="currentColor" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round"
          className={className}
          style={{ flexShrink: 0, display: "block", ...(style || {}) }}>
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
        <svg width={size} height={size} viewBox="0 0 24 24" aria-label="Grafana" role="img" style={{ flexShrink: 0, display: "block", color }}>
          <path fill="currentColor" d="M23.02 10.59a8.578 8.578 0 0 0-.862-3.034 8.911 8.911 0 0 0-1.789-2.445c.337-1.342-.413-2.505-.413-2.505-1.292-.08-2.113.4-2.416.62-.052-.02-.102-.044-.154-.064-.22-.089-.446-.172-.677-.247-.231-.073-.47-.14-.711-.197a9.867 9.867 0 0 0-.875-.161C14.557.753 12.94 0 12.94 0c-1.804 1.145-2.147 2.744-2.147 2.744l-.018.093c-.098.029-.2.057-.298.088-.138.042-.275.094-.413.143-.138.055-.275.107-.41.166a8.869 8.869 0 0 0-1.557.87l-.063-.029c-2.497-.955-4.716.195-4.716.195-.203 2.658.996 4.33 1.235 4.636a11.608 11.608 0 0 0-.607 2.635C1.636 12.677.953 15.014.953 15.014c1.926 2.214 4.171 2.351 4.171 2.351.003-.002.006-.002.006-.005.285.509.615.994.986 1.446.156.19.32.371.488.548-.704 2.009.099 3.68.099 3.68 2.144.08 3.553-.937 3.849-1.173a9.784 9.784 0 0 0 3.164.501h.08l.055-.003.107-.002.103-.005.003.002c1.01 1.44 2.788 1.646 2.788 1.646 1.264-1.332 1.337-2.653 1.337-2.94v-.058c0-.02-.003-.039-.003-.06.265-.187.52-.387.758-.6a7.875 7.875 0 0 0 1.415-1.7c1.43.083 2.437-.885 2.437-.885-.236-1.49-1.085-2.216-1.264-2.354l-.018-.013-.016-.013a.217.217 0 0 1-.031-.02c.008-.092.016-.18.02-.27.011-.162.016-.323.016-.48v-.253l-.005-.098-.008-.135a1.891 1.891 0 0 0-.01-.13c-.003-.042-.008-.083-.013-.125l-.016-.124-.018-.122a6.215 6.215 0 0 0-2.032-3.73 6.015 6.015 0 0 0-3.222-1.46 6.292 6.292 0 0 0-.85-.048l-.107.002h-.063l-.044.003-.104.008a4.777 4.777 0 0 0-3.335 1.695c-.332.4-.592.84-.768 1.297a4.594 4.594 0 0 0-.312 1.817l.003.091c.005.055.007.11.013.164a3.615 3.615 0 0 0 .698 1.82 3.53 3.53 0 0 0 1.827 1.282c.33.098.66.14.971.137.039 0 .078 0 .114-.002l.063-.003c.02 0 .041-.003.062-.003.034-.002.065-.007.099-.01.007 0 .018-.003.028-.003l.031-.005.06-.008a1.18 1.18 0 0 0 .112-.02c.036-.008.072-.013.109-.024a2.634 2.634 0 0 0 .914-.415c.028-.02.056-.041.085-.065a.248.248 0 0 0 .039-.35.244.244 0 0 0-.309-.06l-.078.042c-.09.044-.184.083-.283.116a2.476 2.476 0 0 1-.475.096c-.028.003-.054.006-.083.006l-.083.002c-.026 0-.054 0-.08-.002l-.102-.006h-.012l-.024.006c-.016-.003-.031-.003-.044-.006-.031-.002-.06-.007-.091-.01a2.59 2.59 0 0 1-.724-.213 2.557 2.557 0 0 1-.667-.438 2.52 2.52 0 0 1-.805-1.475 2.306 2.306 0 0 1-.029-.444l.006-.122v-.023l.002-.031c.003-.021.003-.04.005-.06a3.163 3.163 0 0 1 1.352-2.29 3.12 3.12 0 0 1 .937-.43 2.946 2.946 0 0 1 .776-.101h.06l.07.002.045.003h.026l.07.005a4.041 4.041 0 0 1 1.635.49 3.94 3.94 0 0 1 1.602 1.662 3.77 3.77 0 0 1 .397 1.414l.005.076.003.075c.002.026.002.05.002.075 0 .024.003.052 0 .07v.065l-.002.073-.008.174a6.195 6.195 0 0 1-.08.639 5.1 5.1 0 0 1-.267.927 5.31 5.31 0 0 1-.624 1.13 5.052 5.052 0 0 1-3.237 2.014 4.82 4.82 0 0 1-.649.066l-.039.003h-.287a6.607 6.607 0 0 1-1.716-.265 6.776 6.776 0 0 1-3.4-2.274 6.75 6.75 0 0 1-.746-1.15 6.616 6.616 0 0 1-.714-2.596l-.005-.083-.002-.02v-.056l-.003-.073v-.096l-.003-.104v-.07l.003-.163c.008-.22.026-.45.054-.678a8.707 8.707 0 0 1 .28-1.355c.128-.444.286-.872.473-1.277a7.04 7.04 0 0 1 1.456-2.1 5.925 5.925 0 0 1 .953-.763c.169-.111.343-.213.524-.306.089-.05.182-.091.273-.135.047-.02.093-.042.138-.062a7.177 7.177 0 0 1 .714-.267l.145-.045c.049-.015.098-.026.148-.041.098-.029.197-.052.296-.076.049-.013.1-.02.15-.033l.15-.032.151-.028.076-.013.075-.01.153-.024c.057-.01.114-.013.171-.023l.169-.021c.036-.003.073-.008.106-.01l.073-.008.036-.003.042-.002c.057-.003.114-.008.171-.01l.086-.006h.023l.037-.003.145-.007a7.999 7.999 0 0 1 1.708.125 7.917 7.917 0 0 1 2.048.68 8.253 8.253 0 0 1 1.672 1.09l.09.077.089.078c.06.052.114.107.171.159.057.052.112.106.166.16.052.055.107.107.159.164a8.671 8.671 0 0 1 1.41 1.978c.012.026.028.052.04.078l.04.078.075.156c.023.051.05.1.07.153l.065.15a8.848 8.848 0 0 1 .45 1.34.19.19 0 0 0 .201.142.186.186 0 0 0 .172-.184c.01-.246.002-.532-.024-.856z"/>
        </svg>
      );
    }

    function Wordmark() {
      return (
        <div style={{ display: "flex", alignItems: "center", gap: 9, userSelect: "none" }}>
          <GrafanaMark size={22}/>
          <span style={{ fontFamily: "var(--fontFamily)", fontSize: 15, fontWeight: 600, letterSpacing: "-0.01em", color: "var(--fg-max)", whiteSpace: "nowrap" }}>Grafana Agent Observability</span>
          <span style={{
            fontFamily: "var(--fontFamily)", fontSize: 10, fontWeight: 600,
            letterSpacing: "0.07em", textTransform: "uppercase", color: "var(--fg2)",
            border: "1px solid var(--border-medium)", borderRadius: 2, padding: "2px 6px", lineHeight: 1,
          }}>Local</span>
        </div>
      );
    }

    function ModelPill({ name, dot }) {
      const color = dot || modelDot(name);
      return (
        <span title={name} style={{
          display: "inline-flex", alignItems: "center", gap: 6,
          padding: "2px 8px",
          border: "1px solid var(--border-medium)",
          borderRadius: 2,
          background: "rgba(204,204,220,0.02)",
          color: "var(--fg1)", fontSize: 12, fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap",
        }}>
          <span style={{ width: 7, height: 7, borderRadius: "50%", background: color, boxShadow: `0 0 6px ${color}66` }}/>
          {shortModel(name)}
        </span>
      );
    }

    function AgentPill({ name, size }) {
      const full = String(name || "");
      if (!full) return null;
      const sm = size === "sm";
      return (
        <span title={full} style={{
          display: "inline-flex", alignItems: "center", gap: sm ? 4 : 5,
          padding: sm ? "1px 6px" : "1px 7px",
          border: "1px solid var(--border-medium)", borderRadius: 2,
          background: "rgba(204,204,220,0.04)", color: "var(--fg1)",
          fontSize: sm ? 10 : 11, fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap",
        }}>
          <svg width={sm ? 9 : 10} height={sm ? 9 : 10} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/></svg>
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
      return [...new Set((agents || []).map(a => String(a).split("/")[0]).filter(Boolean))];
    }

    function AgentCell({ agents }) {
      const hosts = agentHosts(agents);
      return (
        <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", minWidth: 0 }}>
          {hosts.map(h => (
            <span key={h} title={h} style={{
              display: "inline-flex", alignItems: "center", gap: 5,
              padding: "1px 7px", border: "1px solid var(--border-medium)", borderRadius: 2,
              background: "rgba(204,204,220,0.04)", color: "var(--fg1)",
              fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap",
            }}>
              <svg width={10} height={10} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/></svg>
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
        <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", minWidth: 0 }}>
          {shown.map(m => <ModelPill key={m} name={m}/>)}
          {extra > 0 && (
            <span title={list.join(", ")} style={{ fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>+{extra}</span>
          )}
        </div>
      );
    }

    const iconBtn = {
      width: 28, height: 28,
      display: "inline-flex", alignItems: "center", justifyContent: "center",
      background: "transparent", border: "1px solid transparent",
      color: "var(--fg2)", cursor: "pointer", borderRadius: 2,
    };

    // NavTab is one top-nav section link (Sessions / Settings). The active
    // tab carries the brand underline bar; the others are muted and hover to
    // full white.
    function NavTab({ label, href, active, onClick }) {
      return (
        <a href={href}
          onClick={e => { if (!isPlainLeftClick(e)) return; e.preventDefault(); onClick && onClick(e); }}
          style={{
            position: "relative", display: "inline-flex", alignItems: "center",
            alignSelf: "stretch",
            padding: "0 2px",
            fontFamily: "var(--fontFamily)", fontSize: 13,
            color: active ? "var(--fg-max)" : "var(--fg2)",
            textDecoration: "none", whiteSpace: "nowrap", cursor: "pointer",
          }}
          onMouseEnter={e => { if (!active) e.currentTarget.style.color = "var(--fg-max)"; }}
          onMouseLeave={e => { if (!active) e.currentTarget.style.color = "var(--fg2)"; }}>
          {label}
          {active && <span style={{ position: "absolute", left: 0, right: 0, bottom: 0, height: 2, background: "var(--brandVertical)", borderRadius: 1 }}/>}
        </a>
      );
    }

    // HEADER_H is the sticky top-bar height. Sub-bars (breadcrumb, section
    // tabs) and the sticky left rail offset themselves by this so they sit
    // flush under the header.
    const HEADER_H = 68;

    function TopBar({ tabs = [], activeTab, trail = [] }) {
      const active = tabs.find(t => t.key === activeTab);
      return (
        <>
          <header style={{
            height: HEADER_H,
            background: "var(--bg-primary)",
            display: "flex", alignItems: "center", padding: "0 16px", gap: 20,
            position: "sticky", top: 0, zIndex: 5,
          }}>
            <Wordmark/>
            <div style={{ width: 1, height: 28, background: "var(--border-weak)", margin: "0 4px" }}/>
            <nav style={{ display: "flex", alignItems: "center", alignSelf: "stretch", gap: 18, minWidth: 0, flex: 1, overflow: "hidden" }}>
              {tabs.map(t => (
                <NavTab key={t.key} label={t.label} href={t.href} active={t.key === activeTab} onClick={t.onClick}/>
              ))}
            </nav>
            <a
              href="https://grafana.com/auth/sign-up/create-user/?"
              target="_blank"
              rel="noreferrer"
              style={{
                display: "inline-flex", alignItems: "center", gap: 5,
                color: "var(--fg2)",
                textDecoration: "none",
                fontSize: 12,
                whiteSpace: "nowrap",
                flexShrink: 0,
              }}
              onMouseEnter={e => e.currentTarget.style.color = "var(--fg-max)"}
              onMouseLeave={e => e.currentTarget.style.color = "var(--fg2)"}>
              Sign up for Grafana Cloud
              <Icon name="extlink" size={11}/>
            </a>
          </header>
          {trail.length > 0 && (
            // The breadcrumb lives on its own line under the menu so a long
            // conversation title can't push the other tabs out of view.
            <div style={{
              display: "flex", alignItems: "center", gap: 8, height: 34, padding: "0 16px",
              borderBottom: "1px solid var(--border-weak)", background: "var(--bg-primary)",
              position: "sticky", top: HEADER_H, zIndex: 4, minWidth: 0, overflow: "hidden",
            }}>
              {active && (
                <a href={active.href}
                  onClick={active.onClick ? (e => { if (!isPlainLeftClick(e)) return; e.preventDefault(); active.onClick(); }) : undefined}
                  style={{ fontSize: 13, color: "var(--fg2)", textDecoration: "none", whiteSpace: "nowrap", flexShrink: 0, cursor: "pointer" }}
                  onMouseEnter={e => e.currentTarget.style.color = "var(--fg-max)"}
                  onMouseLeave={e => e.currentTarget.style.color = "var(--fg2)"}>{active.label}</a>
              )}
              {trail.map((b, i) => (
                <React.Fragment key={i}>
                  <Icon name="cright" size={11} style={{ color: "var(--fg3)", flexShrink: 0 }}/>
                  <span style={{
                    fontFamily: b.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)",
                    fontSize: 13, color: "var(--fg-max)",
                    whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0,
                  }}>{b.label}</span>
                </React.Fragment>
              ))}
            </div>
          )}
        </>
      );
    }

    // ============================================================
    // Notices — loading, empty, error states
    // ============================================================

    function Notice({ kind = "info", title, children }) {
      const tone = {
        info:    { color: "var(--fg2)",          bg: "rgba(204,204,220,0.03)", border: "var(--border-weak)",   icon: "empty" },
        warning: { color: "var(--warning-text)", bg: "var(--warning-transparent, rgba(247,148,30,0.06))", border: "var(--warning-border, var(--border-medium))", icon: "alert" },
        error:   { color: "var(--error-text)",   bg: "rgba(209,14,92,0.06)",   border: "var(--error-border)",  icon: "alert" },
      }[kind] || {};
      return (
        <div style={{
          display: "flex", gap: 12, alignItems: "flex-start",
          padding: "16px 18px",
          border: `1px solid ${tone.border}`,
          background: tone.bg,
          borderRadius: 2,
          color: tone.color,
          fontSize: 13,
        }}>
          <Icon name={tone.icon} size={18} style={{ marginTop: 2 }}/>
          <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            {title && <div style={{ color: "var(--fg-max)", fontWeight: 500, fontSize: 13 }}>{title}</div>}
            <div style={{ color: "var(--fg2)", lineHeight: 1.5 }}>{children}</div>
          </div>
        </div>
      );
    }

    const PAGE_MAX_WIDTH = 1392;
    const SURFACE_BG = "rgba(24,27,31,0.88)";
    const HERO_BG = "var(--bg-secondary)";
    const ACTIVE_PILL_BG = "var(--action-selected, rgba(204,204,220,0.08))";
    const PANEL_BG = "rgba(17,18,23,0.42)";

    function Box({ as: Component = "div", style, children, ...props }) {
      return <Component {...props} style={style}>{children}</Component>;
    }

    function Stack({ as = "div", direction = "column", gap, align, justify, wrap, style, children, ...props }) {
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
          }}>
          {children}
        </Box>
      );
    }

    function SurfaceCard({ children, style, ...rest }) {
      return (
        <Box style={{
          position: "relative",
          overflow: "hidden",
          background: SURFACE_BG,
          border: "1px solid var(--border-weak)",
          borderRadius: 10,
          boxShadow: "0 10px 24px rgba(0,0,0,0.14)",
          ...(style || {}),
        }} {...rest}>
          {children}
        </Box>
      );
    }

    function ModalFrame({ title, desc, onClose, children, width = "min(860px, 100%)" }) {
      return (
        <div onClick={onClose} style={{ position: "fixed", inset: 0, zIndex: 70, background: "rgba(0,0,0,0.58)", display: "flex", alignItems: "flex-start", justifyContent: "center", padding: "9vh 18px 24px" }}>
          <div onClick={e => e.stopPropagation()} style={{ width, maxHeight: "82vh", overflow: "hidden", background: "var(--bg-secondary)", border: "1px solid var(--border-strong)", borderRadius: 10, boxShadow: "0 18px 54px rgba(0,0,0,0.58)", display: "flex", flexDirection: "column" }}>
            <div style={{ display: "flex", alignItems: "flex-start", justifyContent: "space-between", gap: 16, padding: "16px 18px", borderBottom: "1px solid var(--border-weak)" }}>
              <div style={{ minWidth: 0 }}>
                <div style={{ color: "var(--fg-max)", fontSize: 15, fontWeight: 600, marginBottom: desc ? 5 : 0 }}>{title}</div>
                {desc && <div style={{ color: "var(--fg3)", fontSize: 12.5, lineHeight: 1.45 }}>{desc}</div>}
              </div>
              <button type="button" onClick={onClose} style={{ border: "none", background: "transparent", color: "var(--fg3)", cursor: "pointer", padding: 4 }}>
                <Icon name="close" size={16}/>
              </button>
            </div>
            {children}
          </div>
        </div>
      );
    }

    function PageShell({ children, maxWidth = PAGE_MAX_WIDTH, style }) {
      return (
        <Box style={{ width: "100%", maxWidth, margin: "0 auto", padding: "34px 24px 96px", ...(style || {}) }}>
          {children}
        </Box>
      );
    }

    function HeroChip({ label, value, tone }) {
      return (
        <Stack gap={2} style={{ padding: "8px 10px", border: "1px solid var(--border-medium)", borderRadius: 8, background: PANEL_BG, minWidth: 108 }}>
          <Box style={{ fontSize: 10, letterSpacing: ".08em", textTransform: "uppercase", color: "var(--fg3)", fontWeight: 700 }}>{label}</Box>
          <Box style={{ fontSize: 12, color: tone || "var(--fg-max)", fontWeight: 600, whiteSpace: "nowrap" }}>{value}</Box>
        </Stack>
      );
    }

    function PageHero({ icon, kicker, title, desc, chips, actions, children, style }) {
      return (
        <Box style={{
          position: "relative",
          overflow: "hidden",
          border: "1px solid var(--border-weak)",
          borderRadius: 10,
          padding: "22px 24px",
          marginBottom: 18,
          background: HERO_BG,
          boxShadow: "0 12px 28px rgba(0,0,0,0.18)",
          ...(style || {}),
        }}>
          <Box style={{ position: "absolute", left: 0, right: 0, top: 0, height: 2, background: "var(--brandVertical)" }}/>
          <Stack direction="row" justify="space-between" gap={24} align="flex-end" wrap="wrap">
            <Box style={{ minWidth: 0, flex: "1 1 360px" }}>
              <Stack direction="row" align="center" gap={8} style={{ marginBottom: 8 }}>
                {icon && <Icon name={icon} size={15} style={{ color: "var(--brand-orange-text)" }}/>}
                <Box as="span" style={{ fontSize: 11, letterSpacing: ".12em", textTransform: "uppercase", color: "var(--fg3)", fontWeight: 700 }}>{kicker}</Box>
              </Stack>
              <h1 style={{ fontSize: 26, lineHeight: 1.15, fontWeight: 650, color: "var(--fg-max)", margin: 0, letterSpacing: "-0.03em" }}>{title}</h1>
              {desc && <Box style={{ marginTop: 8, fontSize: 13, color: "var(--fg2)", maxWidth: 680 }}>{desc}</Box>}
              {children && <Box style={{ marginTop: 10 }}>{children}</Box>}
            </Box>
            {(chips || actions) && (
              <Stack direction="row" wrap="wrap" gap={8} justify="flex-end" align="center" style={{ minWidth: 0 }}>
                {(chips || []).map(chip => <HeroChip key={chip.label} {...chip}/>)}
                {actions}
              </Stack>
            )}
          </Stack>
        </Box>
      );
    }

    function PillToggle({ options, value, onChange }) {
      return (
        <Stack direction="row" gap={3} style={{ display: "inline-flex", padding: 3, border: "1px solid var(--border-medium)", borderRadius: 999, background: PANEL_BG, overflow: "hidden", boxShadow: "inset 0 0 0 1px rgba(0,0,0,0.10)" }}>
          {options.map(o => {
            const active = o.value === value;
            return (
              <button key={o.value} type="button" onClick={() => onChange(o.value)} style={{
                padding: "5px 13px",
                borderRadius: 999,
                background: active ? ACTIVE_PILL_BG : "transparent",
                color: active ? "var(--primary-text)" : "var(--fg2)",
                border: "none",
                cursor: active ? "default" : "pointer",
                fontSize: 12,
                fontWeight: active ? 600 : 400,
                fontFamily: "var(--fontFamily)",
                boxShadow: active ? "inset 0 0 0 1px var(--primary-border)" : "none",
              }}>
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
      return <PillToggle options={options} value={value} onChange={onChange}/>;
    }

    // ChartXLabels renders at most ~5 evenly-spaced bucket labels so the
    // axis stays readable instead of becoming a wall of timestamps. Empty
    // slots keep the flex columns aligned with the bars above them.
    function ChartXLabels({ data }) {
      const step = Math.max(1, Math.ceil(data.length / 5));
      return (
        <div style={{ display: "flex", marginLeft: 44, marginTop: 6, fontSize: 10, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>
          {data.map((d, i) => {
            const last = i === data.length - 1;
            const show = i % step === 0 || last;
            return <span key={i} style={{ flex: 1, textAlign: last ? "right" : "left", overflow: "hidden", whiteSpace: "nowrap" }}>{show ? d.t : ""}</span>;
          })}
        </div>
      );
    }

    // ChartYAxis renders the three right-aligned scale labels (max, mid, 0)
    // in the 44px gutter to the left of the plot. The plot is 130px tall, so
    // the labels pin to the top, middle (65px), and baseline (130px).
    function ChartYAxis({ top, mid }) {
      const label = {
        position: "absolute", left: 0, width: 34, textAlign: "right",
        transform: "translateY(-50%)",
        fontSize: 10, lineHeight: "10px", color: "var(--fg3)",
        fontFamily: "var(--fontFamilyMonospace)", pointerEvents: "none",
      };
      return (
        <React.Fragment>
          <div style={{ ...label, top: 0 }}>{top}</div>
          <div style={{ ...label, top: 65 }}>{mid}</div>
          <div style={{ ...label, top: 130 }}>0</div>
        </React.Fragment>
      );
    }

    function ActivityChart({ data, bucketLabel, switcher, selection, onBucketClick, accent = "var(--brand-orange)" }) {
      const W = 100, H = 32;
      const max = Math.max(1, ...data.map(d => d.c));
      const barW = (W / Math.max(1, data.length)) * 0.7;
      const gap  = (W / Math.max(1, data.length)) * 0.3;
      const [hover, setHover] = useState(null);

      return (
        <SurfaceCard style={{ position: "relative", padding: "16px 20px 12px", marginBottom: 0 }}>
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 10 }}>
            {switcher}
            <div style={{ display: "flex", alignItems: "center", gap: 12, fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>
              <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                <span style={{ width: 10, height: 10, background: accent, borderRadius: 1 }}/> count
              </span>
              <span>{bucketLabel}</span>
            </div>
          </div>
          <div style={{ position: "relative" }}>
            <ChartYAxis top={String(max)} mid={String(Math.round(max / 2))}/>
            <div style={{ marginLeft: 44, position: "relative", borderBottom: "1px solid var(--border-medium)" }}>
              <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" style={{ width: "100%", height: 130, display: "block" }}>
                {[0, 0.5].map(g => (
                  <line key={g} x1={0} x2={W} y1={H * g} y2={H * g} stroke="rgba(204,204,220,0.06)" strokeWidth="0.2"/>
                ))}
                {data.map((d, i) => {
                  const h = (d.c / max) * H;
                  const x = i * (W / data.length) + gap/2;
                  const y = H - h;
                  const isHover = hover === i;
                  // Midpoint containment, not overlap: the window shifts a
                  // little every render (now moves), so an overlap test can
                  // light up two adjacent bars.
                  const isSel = selection && (d.start + d.end) / 2 >= selection.start && (d.start + d.end) / 2 < selection.end;
                  const dim = selection && !isSel;
                  return (
                    <g key={i} onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}
                      onClick={onBucketClick ? () => onBucketClick(d) : undefined}
                      style={{ cursor: onBucketClick ? "pointer" : "default" }}>
                      <rect x={x - 0.4} y={0} width={barW + 0.8} height={H} fill="transparent"/>
                      <rect x={x} y={y} width={barW} height={Math.max(h, 0.4)} fill={isHover ? "var(--brand-orange-text)" : accent} opacity={isHover || isSel ? 1 : dim ? 0.3 : 0.85}/>
                    </g>
                  );
                })}
              </svg>
              {hover !== null && (
                <div style={{
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
                }}>
                  <span style={{ color: "var(--fg3)" }}>{data[hover].t}</span> · {data[hover].c} {data[hover].c === 1 ? "session" : "sessions"}
                </div>
              )}
            </div>
            <ChartXLabels data={data}/>
          </div>
        </SurfaceCard>
      );
    }

    // Stacked token-usage-over-time chart. Mirrors ActivityChart's frame
    // but stacks the five disjoint token series per bucket, with a
    // per-model filter and a click-to-toggle legend. data comes from
    // bucketTokenUsage.
    function TokenChart({ data, bucketLabel, grandTotal, models, model, onModelChange, hidden, onToggleSeries, switcher, selection, onBucketClick }) {
      const W = 100, H = 32;
      const barW = (W / Math.max(1, data.length)) * 0.7;
      const gap  = (W / Math.max(1, data.length)) * 0.3;
      const [hover, setHover] = useState(null);
      // Only show legend entries for series that actually appear, so a
      // pure-Anthropic store doesn't carry an always-zero "Reasoning"
      // swatch. Fall back to the full set when there's no data at all.
      const present = TOKEN_SERIES.filter(s => data.some(d => d[s.key] > 0));
      const legend = present.length ? present : TOKEN_SERIES;
      // Hidden series drop out of the bars, the tooltip, and the y scale,
      // so toggling a dominant series (usually cache reads) rescales the
      // chart to show what's left.
      const visible = TOKEN_SERIES.filter(s => !hidden.has(s.key));
      const visibleTotal = d => visible.reduce((acc, s) => acc + (d[s.key] || 0), 0);
      const max = Math.max(1, ...data.map(visibleTotal));
      const empty = grandTotal === 0;

      return (
        <SurfaceCard style={{ position: "relative", padding: "16px 20px 12px", marginBottom: 0 }}>
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 10, gap: 12, flexWrap: "wrap" }}>
            {switcher}
            <div style={{ display: "flex", alignItems: "center", gap: 12, fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", flexWrap: "wrap" }}>
              {legend.map(s => {
                const off = hidden.has(s.key);
                return (
                  <button key={s.key} onClick={() => onToggleSeries(s.key)}
                    title={off ? `Show ${s.label}` : `Hide ${s.label}`}
                    style={{
                      display: "inline-flex", alignItems: "center", gap: 6,
                      background: "transparent", border: "none", padding: 0,
                      cursor: "pointer", font: "inherit",
                      color: off ? "var(--fg3)" : "inherit",
                      opacity: off ? 0.6 : 1,
                      textDecoration: off ? "line-through" : "none",
                    }}>
                    <span style={{ width: 10, height: 10, boxSizing: "border-box", background: off ? "transparent" : s.color, border: `1px solid ${off ? "var(--border-medium)" : s.color}`, borderRadius: 1 }}/> {s.label}
                  </button>
                );
              })}
              {models.length > 0 && (
                <select value={model} onChange={e => onModelChange(e.target.value)} title="Filter by model"
                  style={{ height: 24, padding: "0 6px", border: "1px solid var(--border-medium)", borderRadius: 2, background: "var(--bg-primary)", color: "var(--fg1)", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)" }}>
                  <option value="all">All models</option>
                  {models.map(m => <option key={m} value={m}>{m}</option>)}
                </select>
              )}
              <span>{bucketLabel}</span>
            </div>
          </div>
          <div style={{ position: "relative" }}>
            {!empty && visible.length > 0 && <ChartYAxis top={formatTokens(max)} mid={formatTokens(Math.round(max / 2))}/>}
            <div style={{ marginLeft: 44, position: "relative", borderBottom: "1px solid var(--border-medium)" }}>
              <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" style={{ width: "100%", height: 130, display: "block" }}>
                {[0, 0.5].map(g => (
                  <line key={g} x1={0} x2={W} y1={H * g} y2={H * g} stroke="rgba(204,204,220,0.06)" strokeWidth="0.2"/>
                ))}
                {data.map((d, i) => {
                  const x = i * (W / data.length) + gap/2;
                  const isHover = hover === i;
                  // Midpoint containment, not overlap — see ActivityChart.
                  const isSel = selection && (d.start + d.end) / 2 >= selection.start && (d.start + d.end) / 2 < selection.end;
                  const dim = selection && !isSel;
                  const barOpacity = isHover || isSel ? 1 : dim ? 0.3 : 0.85;
                  let yTop = H;
                  const segs = [];
                  for (const s of visible) {
                    const v = d[s.key] || 0;
                    if (v <= 0) continue;
                    const h = (v / max) * H;
                    yTop -= h;
                    segs.push(<rect key={s.key} x={x} y={yTop} width={barW} height={Math.max(h, 0.2)} fill={s.color} opacity={barOpacity}/>);
                  }
                  return (
                    <g key={i} onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}
                      onClick={onBucketClick ? () => onBucketClick(d) : undefined}
                      style={{ cursor: onBucketClick ? "pointer" : "default" }}>
                      <rect x={x - 0.4} y={0} width={barW + 0.8} height={H} fill="transparent"/>
                      {segs}
                    </g>
                  );
                })}
              </svg>
              {empty && (
                <div style={{ position: "absolute", top: 0, left: 0, right: 0, height: 130, display: "flex", alignItems: "center", justifyContent: "center", fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", pointerEvents: "none" }}>
                  No token usage {model !== "all" ? `for ${model} ` : ""}in this range
                </div>
              )}
              {hover !== null && visibleTotal(data[hover]) > 0 && (
                <div style={{
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
                }}>
                  <div style={{ color: "var(--fg3)", marginBottom: 4 }}>{data[hover].t} · {formatTokens(visibleTotal(data[hover]))} tok</div>
                  {visible.filter(s => data[hover][s.key] > 0).map(s => (
                    <div key={s.key} style={{ display: "flex", alignItems: "center", gap: 8 }}>
                      <span style={{ width: 8, height: 8, background: s.color, borderRadius: 1 }}/>
                      <span style={{ color: "var(--fg2)" }}>{s.label}</span>
                      <span style={{ marginLeft: "auto", color: "var(--fg1)" }}>{formatTokens(data[hover][s.key])}</span>
                    </div>
                  ))}
                </div>
              )}
            </div>
            <ChartXLabels data={data}/>
          </div>
        </SurfaceCard>
      );
    }

    function TimeRangePicker({ value, onChange, ranges = TIME_RANGES }) {
      const [open, setOpen] = useState(false);
      const selected = ranges.find(r => r.value === value) || ranges[ranges.length - 1] || TIME_RANGES[0];
      return (
        <div style={{ position: "relative", flex: "0 0 auto" }}>
          <button type="button" onClick={() => setOpen(o => !o)}
            onBlur={e => { if (!e.currentTarget.parentElement?.contains(e.relatedTarget)) setOpen(false); }}
            title="Time range"
            style={{
              height: 34,
              minWidth: 166,
              padding: "0 10px",
              border: "1px solid var(--border-medium)",
              borderRadius: 8,
              background: "rgba(24,27,31,0.78)",
              color: "var(--fg1)",
              fontSize: 13,
              fontFamily: "var(--fontFamily)",
              display: "inline-flex",
              alignItems: "center",
              justifyContent: "space-between",
              gap: 10,
              cursor: "pointer",
            }}>
            <span style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
              <Icon name="clock" size={14} style={{ color: "var(--fg3)" }}/>
              {selected.label}
            </span>
            <Icon name="chevron" size={14} style={{ color: "var(--fg3)" }}/>
          </button>
          {open && (
            <div style={{
              position: "absolute",
              top: 39,
              right: 0,
              zIndex: 30,
              minWidth: 190,
              padding: 4,
              border: "1px solid var(--border-strong)",
              borderRadius: 8,
              background: "var(--bg-secondary)",
              boxShadow: "0 12px 34px rgba(0,0,0,0.48)",
            }}>
              {ranges.map(r => {
                const active = r.value === selected.value;
                return (
                  <button key={r.value} type="button"
                    onMouseDown={e => e.preventDefault()}
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
                      background: active ? ACTIVE_PILL_BG : "transparent",
                      color: active ? "var(--primary-text)" : "var(--fg1)",
                      fontSize: 12,
                      fontFamily: "var(--fontFamily)",
                      cursor: "pointer",
                      textAlign: "left",
                    }}>
                    <span>{r.label}</span>
                    {active && <Icon name="check" size={12}/>}
                  </button>
                );
              })}
            </div>
          )}
        </div>
      );
    }

    function FilterBar({
      query, onQueryChange, inputRef,
      timeRange, onTimeRangeChange,
      agentFilter = "all", onAgentFilterChange, agentOptions = [],
      modelFilter = "all", onModelFilterChange, modelOptions = [],
      statusFilter = "all", onStatusFilterChange,
      activeFilterCount = 0, onClearFilters,
      onRefresh, refreshing,
      placeholder = "Filter by title, id, workspace, agent, model…",
      onInputKeyDown,
      rightAdornment,
    }) {
      const showTimeRange = !!timeRange && !!onTimeRangeChange;
      const showAgentFilter = !!onAgentFilterChange;
      const showModelFilter = !!onModelFilterChange;
      const showStatusFilter = !!onStatusFilterChange;
      const selectStyle = {
        height: 34,
        minWidth: 132,
        padding: "0 30px 0 11px",
        border: "1px solid var(--border-medium)",
        borderRadius: 8,
        background: "rgba(24,27,31,0.78)",
        color: "var(--fg1)",
        fontSize: 13,
        fontFamily: "var(--fontFamily)",
      };
      return (
        <Stack direction="row" align="stretch" gap={8} style={{ marginBottom: 16, fontSize: 13, flexWrap: "wrap" }}>
          <Stack direction="row" align="center" gap={8} style={{
            flex: "1 1 320px",
            padding: "0 11px",
            height: 34,
            border: "1px solid var(--border-medium)",
            borderRadius: 8,
            background: "rgba(24,27,31,0.78)",
            color: "var(--fg3)",
            boxShadow: "inset 0 0 0 1px rgba(0,0,0,0.12)",
          }}>
            <Icon name="search" size={14}/>
            <input
              ref={inputRef}
              value={query}
              onChange={e => onQueryChange(e.target.value)}
              onKeyDown={onInputKeyDown}
              placeholder={placeholder}
              style={{
                flex: 1, background: "transparent", border: "none", outline: "none",
                color: "var(--fg1)", fontSize: 13, fontFamily: "var(--fontFamily)",
              }}/>
            {rightAdornment !== undefined ? rightAdornment : (
              <span title="Press Command-K or Control-K to focus search" style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", padding: "1px 6px", border: "1px solid var(--border-weak)", borderRadius: 999 }}>⌘K</span>
            )}
          </Stack>
          {showTimeRange && <TimeRangePicker value={timeRange} onChange={onTimeRangeChange}/>}
          {showAgentFilter && (
            <select value={agentFilter} onChange={e => onAgentFilterChange(e.target.value)} title="Filter by agent" style={selectStyle}>
              <option value="all">All agents</option>
              {agentOptions.map(a => <option key={a} value={a}>{a}</option>)}
            </select>
          )}
          {showModelFilter && (
            <select value={modelFilter} onChange={e => onModelFilterChange(e.target.value)} title="Filter by model" style={{ ...selectStyle, minWidth: 150 }}>
              <option value="all">All models</option>
              {modelOptions.map(m => <option key={m} value={m}>{m}</option>)}
            </select>
          )}
          {showStatusFilter && (
            <select value={statusFilter} onChange={e => onStatusFilterChange(e.target.value)} title="Filter by status" style={selectStyle}>
              <option value="all">All status</option>
              <option value="errors">Errors</option>
              <option value="subagents">Has subagents</option>
            </select>
          )}
          {activeFilterCount > 0 && onClearFilters && (
            <button onClick={onClearFilters}
              style={{ ...iconBtn, height: 34, padding: "0 11px", border: "1px solid var(--border-medium)", borderRadius: 8, color: "var(--fg2)", gap: 6 }}
              title="Clear session filters"
              onMouseEnter={e => { e.currentTarget.style.background = "var(--action-hover)"; e.currentTarget.style.color = "var(--fg1)"; }}
              onMouseLeave={e => { e.currentTarget.style.background = "transparent"; e.currentTarget.style.color = "var(--fg2)"; }}>
              <Icon name="close" size={13}/>Clear
            </button>
          )}
          <button onClick={onRefresh} disabled={refreshing}
            style={{ ...iconBtn, height: 34, width: 34, border: "1px solid var(--border-medium)", borderRadius: 8, opacity: refreshing ? 0.5 : 1, cursor: refreshing ? "wait" : "pointer" }}
            title="Refresh"
            onMouseEnter={e => { if (!refreshing) { e.currentTarget.style.background = "var(--action-hover)"; e.currentTarget.style.color = "var(--fg1)"; } }}
            onMouseLeave={e => { e.currentTarget.style.background = "transparent"; e.currentTarget.style.color = "var(--fg2)"; }}>
            <Icon name="refresh" size={14}/>
          </button>
        </Stack>
      );
    }

    function ConvRow({ c, now, onOpen, prices }) {
      const accent = c.status === "err" ? "var(--error-main)"
        : c.status === "warn" ? "var(--warning-main)"
        : "transparent";
      const wallSec = durationBetweenSeconds(c.started_at, c.last_activity);
      return (
        <a href={conversationPath(c.id)}
           onClick={e => {
             if (!isPlainLeftClick(e)) return;
             e.preventDefault();
             onOpen(c);
           }}
           style={{
          display: "grid",
          gridTemplateColumns: CONV_GRID,
          alignItems: "center",
          gap: 16,
          padding: "12px 16px",
          borderBottom: "1px solid var(--border-weak)",
          borderLeft: `3px solid ${accent}`,
          background: "transparent",
          cursor: "pointer",
          fontFamily: "var(--fontFamilyMonospace)", fontSize: 12,
          transition: "background 80ms ease",
          textDecoration: "none",
          color: "inherit",
        }}
        onMouseEnter={e => e.currentTarget.style.background = "rgba(204,204,220,0.03)"}
        onMouseLeave={e => e.currentTarget.style.background = "transparent"}
        >
          <span style={{ color: "var(--fg2)" }}>{formatAgo(c.last_activity, now)}</span>
          <div style={{ display: "flex", flexDirection: "column", gap: 2, minWidth: 0 }}>
            <span style={{ display: "flex", alignItems: "center", gap: 7, minWidth: 0 }}>
              <span style={{ fontFamily: "var(--fontFamily)", color: "var(--fg1)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.title || c.id}</span>
              {c.subagents > 0 && (
                <span title={`${c.subagents} subagent ${c.subagents === 1 ? "step" : "steps"}`}
                  style={{ flexShrink: 0, display: "inline-flex", alignItems: "center", gap: 3, padding: "0 6px", height: 16, borderRadius: 2, background: "rgba(204,204,220,0.06)", color: "var(--fg2)", fontSize: 10, fontFamily: "var(--fontFamilyMonospace)" }}>⊂ {c.subagents}</span>
              )}
            </span>
            <span style={{ color: "var(--fg3)", fontSize: 11, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
              {c.workspace ? workspaceLabel(c.workspace) : c.id}
            </span>
          </div>
          <AgentCell agents={c.agents}/>
          <span style={{ color: "var(--fg1)" }} title="Estimated cost">{formatCost(conversationCost(c, prices))}</span>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 7 }} title={tokenBreakdownTitle(c.token_buckets)}>
            <span style={{ color: "var(--fg1)" }}>{formatTokens(c.total_tokens)}</span>
            {c.status === "err" && (
              <span style={{ display: "inline-flex", alignItems: "center", padding: "0 6px", height: 16, borderRadius: 2, background: "var(--error-transparent)", color: "var(--error-text)", fontSize: 10, letterSpacing: "0.04em" }}>ERR</span>
            )}
          </span>
          <span style={{ color: "var(--fg2)" }}>
            <span style={{ color: "var(--fg1)" }}>{formatDuration(wallSec)}</span>
            <span style={{ color: "var(--fg3)", padding: "0 6px" }}>·</span>
            <span style={{ color: "var(--fg1)" }}>{c.calls} {c.calls === 1 ? "call" : "calls"}</span>
          </span>
          <ModelCell models={c.models}/>
        </a>
      );
    }

    // Shared by ConvRow and its header so the columns stay aligned:
    // Last activity · Conversation · Agent · Cost · Tokens · Duration · Models.
    // Agent shows the host launcher only (claude-code, …) — not the per-
    // subagent rows, which were the noise; subagent presence is the ⊂N badge.
    const CONV_GRID = "84px minmax(260px, 1.7fr) 132px 80px 96px 136px 176px";

    // SortHeader is a clickable list-header cell: click sorts by the
    // column, clicking again flips the direction.
    function SortHeader({ label, sortKey, sort, onSort }) {
      const active = sort.key === sortKey;
      return (
        <button onClick={() => onSort(sortKey)} title={`Sort by ${label.toLowerCase()}`}
          style={{
            display: "inline-flex", alignItems: "center", gap: 4,
            background: "transparent", border: "none", padding: 0,
            cursor: "pointer", font: "inherit", textAlign: "left",
            fontWeight: 500, whiteSpace: "nowrap",
            color: active ? "var(--fg1)" : "inherit",
          }}>
          {label}{active && <span style={{ fontSize: 8 }}>{sort.dir === "asc" ? "▲" : "▼"}</span>}
        </button>
      );
    }

    // KpiTile is one cell of the KPI strip: a sentence-case label, a big
    // mono value (optionally tinted, with a leading status dot), an
    // optional progress bar, and a sub line.
    function KpiTile({ label, value, valueColor, sub, dot, bar }) {
      return (
        <SurfaceCard style={{ padding: "14px 16px", display: "flex", flexDirection: "column", gap: 7, minHeight: 104 }}>
          <span style={{ fontSize: 11, color: "var(--fg3)" }}>{label}</span>
          <span style={{ display: "flex", alignItems: "center", gap: 8 }}>
            {dot && <span style={{ width: 8, height: 8, borderRadius: "50%", background: dot, flexShrink: 0 }}/>}
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 24, fontWeight: 500, lineHeight: 1, color: valueColor || "var(--fg-max)" }}>{value}</span>
          </span>
          {bar != null && (
            <span style={{ display: "block", height: 4, borderRadius: 2, background: "rgba(204,204,220,0.1)", overflow: "hidden", marginTop: 1 }}>
              <span style={{ display: "block", height: "100%", width: `${bar}%`, background: "var(--viz-green)" }}/>
            </span>
          )}
          {sub != null && <span style={{ fontSize: 11, color: "var(--fg2)" }}>{sub}</span>}
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
        <div style={{ display: "grid", gridTemplateColumns: "repeat(6, 1fr)", gap: 12, marginBottom: 16 }}>
          <KpiTile label="Sessions" value={kpi.conversations} sub={kpi.conversationsSub}/>
          <KpiTile label="Total cost" value={formatCost(kpi.cost)} sub={kpi.costSub}/>
          <KpiTile label="Total tokens" value={formatTokens(kpi.tokens)} sub={`${kpi.models} ${kpi.models === 1 ? "model" : "models"}`}/>
          <KpiTile label="Input cache hit" value={kpi.cachePct == null ? "\u2014" : `${kpi.cachePct}%`} bar={kpi.cachePct == null ? 0 : kpi.cachePct}/>
          <KpiTile label="Tool calls" value={kpi.calls} sub={`${avg} avg / session`}/>
          <KpiTile label="Errored sessions" value={kpi.errConvs}
            valueColor={kpi.errConvs > 0 ? "var(--error-text)" : "var(--fg-max)"}
            dot={kpi.errConvs > 0 ? "var(--error-text)" : undefined}
            sub={`${kpi.errPct}% of sessions`}/>
        </div>
      );
    }

    // WorkspaceSidebar is the landing page's primary navigation: one row per
    // cwd in the current time range, each showing conversation count and
    // estimated cost, with an "All" row on top. Selecting one filters the
    // list, charts, and KPIs. Rows are sorted by most-recent activity so the
    // workspace you're in now sits near the top.
    // A workspace row reads like an observability leaderboard entry: name,
    // count + estimated cost, and a thin bar showing this workspace's share
    // of total spend. The "All" summary row is the full-bar reference and
    // carries a heavier label. Selection tints the bar + left edge orange.
    function WorkspaceItem({ label, title, count, cost, active, onClick, share, summary }) {
      const pct = share > 0 ? Math.max(3, Math.min(100, Math.round(share * 100))) : 0;
      return (
        <button onClick={onClick} title={title}
          style={{
            display: "flex", flexDirection: "column", gap: 6,
            width: "100%", textAlign: "left",
            padding: "9px 11px",
            border: "1px solid var(--border-weak)",
            borderLeft: `2px solid ${active ? "var(--brand-orange)" : "var(--border-weak)"}`,
            borderRadius: 8,
            background: active ? ACTIVE_PILL_BG : "rgba(24,27,31,0.68)",
            color: "inherit", cursor: "pointer", font: "inherit",
            transition: "background 80ms ease, border-color 80ms ease",
          }}
          onMouseEnter={e => { if (!active) e.currentTarget.style.background = "rgba(204,204,220,0.04)"; }}
          onMouseLeave={e => { if (!active) e.currentTarget.style.background = "rgba(24,27,31,0.68)"; }}>
          <span style={{
            fontFamily: "var(--fontFamily)", fontSize: 12.5, fontWeight: summary ? 600 : 400,
            color: active ? "var(--fg1)" : "var(--fg2)",
            overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap",
          }}>{label}</span>
          <span style={{ display: "flex", alignItems: "center", gap: 8, fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>
            <span>{count} {count === 1 ? "session" : "sessions"}</span>
            <span style={{ marginLeft: "auto", color: active ? "var(--fg1)" : "var(--fg2)" }}>{formatCost(cost)}</span>
          </span>
          <span style={{ display: "block", height: 3, borderRadius: 2, background: "rgba(204,204,220,0.08)", overflow: "hidden" }}>
            <span style={{
              display: "block", height: "100%", width: `${pct}%`,
              background: active ? "var(--brand-orange)" : "rgba(204,204,220,0.28)",
              transition: "width 220ms ease",
            }}/>
          </span>
        </button>
      );
    }

    function WorkspaceSidebar({ workspaces, selected, onSelect, totalCount, totalCost }) {
      return (
        <div style={{
          width: 248, flexShrink: 0,
          borderRight: "1px solid var(--border-weak)",
          background: "var(--bg-primary)",
          display: "flex", flexDirection: "column",
          maxHeight: `calc(100vh - ${HEADER_H}px)`, position: "sticky", top: HEADER_H,
        }}>
          {/* 34px caption slot: the first card below starts at the same
             vertical offset as the search bar in the content column. */}
          <div style={{ height: 34, padding: "14px 16px 0", display: "flex", alignItems: "flex-start", lineHeight: "14px", fontSize: 11, letterSpacing: "0.06em", color: "var(--fg3)", fontWeight: 500, textTransform: "uppercase" }}>
            Workspaces
          </div>
          <div style={{ overflowY: "auto", padding: "0 8px 12px", display: "flex", flexDirection: "column", gap: 6 }}>
            <WorkspaceItem label="All workspaces" title="All workspaces" count={totalCount} cost={totalCost}
              share={1} summary active={selected == null} onClick={() => onSelect(null)}/>
            {workspaces.map(w => (
              <WorkspaceItem key={w.path || "(unknown)"} label={workspaceLabel(w.path)} title={w.path || "(unknown)"}
                count={w.count} cost={w.cost} share={totalCost > 0 ? (w.cost || 0) / totalCost : 0}
                active={selected === w.path} onClick={() => onSelect(w.path)}/>
            ))}
          </div>
        </div>
      );
    }

    function ConversationsView({ conversations, tokenPoints, loading, error, query, setQuery, searchInputRef, timeRange, setTimeRange, tokenModel, setTokenModel, chartMetric, setChartMetric, bucketSel, setBucketSel, listSort, setListSort, onOpen, onRefresh, refreshing, onOpenSettings }) {
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
        return conversations.filter(c => {
          const t = conversationTime(c);
          return t != null && t >= from && t <= now;
        });
      }, [conversations, range.ms, now]);

      // Workspace facet, derived from the time-range set (not the search or
      // the selected workspace, so the rail stays stable while you browse).
      // Sorted by most-recent activity so the workspace you're in now floats
      // up. Selecting one narrows the list, charts, and KPIs alike.
      // ponytail: local state — resets on navigate-away. Lift to App alongside
      // bucketSel if cross-navigation persistence is wanted.
      const [workspace, setWorkspace] = useState(null);
      const workspaces = useMemo(() => {
        const map = new Map();
        for (const c of rangeFiltered) {
          const w = c.workspace || "";
          let e = map.get(w);
          if (!e) { e = { path: w, count: 0, cost: 0, last: 0 }; map.set(w, e); }
          e.count++;
          e.cost += conversationCost(c, prices) || 0;
          const t = conversationTime(c);
          if (t != null && t > e.last) e.last = t;
        }
        return [...map.values()].sort((a, b) => b.last - a.last);
      }, [rangeFiltered, prices]);
      const totalCost = useMemo(() => rangeFiltered.reduce((s, c) => s + (conversationCost(c, prices) || 0), 0), [rangeFiltered, prices]);
      // A selected workspace that vanishes from the set (range change) falls
      // back to "all" by derivation, mirroring the token-model fallback.
      const activeWorkspace = workspace != null && workspaces.some(w => w.path === workspace) ? workspace : null;
      const wsFiltered = useMemo(
        () => activeWorkspace == null ? rangeFiltered : rangeFiltered.filter(c => (c.workspace || "") === activeWorkspace),
        [rangeFiltered, activeWorkspace]
      );

      const agentOptions = useMemo(() => {
        const set = new Set();
        for (const c of wsFiltered) for (const a of agentHosts(c.agents)) set.add(a);
        return [...set].sort();
      }, [wsFiltered]);
      const activeAgentFilter = agentOptions.includes(agentFilter) ? agentFilter : "all";

      const modelFacetOptions = useMemo(() => {
        const set = new Set();
        for (const c of wsFiltered) for (const m of c.models || []) if (m) set.add(m);
        return [...set].sort();
      }, [wsFiltered]);
      const activeModelFilter = modelFacetOptions.includes(modelFilter) ? modelFilter : "all";
      const activeStatusFilter = statusFilter === "errors" || statusFilter === "subagents" ? statusFilter : "all";

      const filtered = useMemo(() => {
        return wsFiltered.filter(c => {
          if (activeAgentFilter !== "all" && !agentHosts(c.agents).includes(activeAgentFilter)) return false;
          if (activeModelFilter !== "all" && !(c.models || []).includes(activeModelFilter)) return false;
          if (activeStatusFilter === "errors" && c.status !== "err") return false;
          if (activeStatusFilter === "subagents" && !(c.subagents > 0)) return false;
          return true;
        });
      }, [wsFiltered, activeAgentFilter, activeModelFilter, activeStatusFilter]);

      const activeFilterCount = (activeAgentFilter !== "all" ? 1 : 0)
        + (activeModelFilter !== "all" ? 1 : 0)
        + (activeStatusFilter !== "all" ? 1 : 0);
      const clearFilters = useCallback(() => {
        setAgentFilter("all");
        setModelFilter("all");
        setStatusFilter("all");
      }, []);
      const clearSearch = useCallback(() => {
        setQuery("");
        setTimeout(() => {
          const el = searchInputRef && searchInputRef.current;
          if (el) el.focus();
        }, 0);
      }, [setQuery, searchInputRef]);
      const onSearchInputKey = useCallback((e) => {
        if (e.key === "Escape") {
          e.preventDefault();
          clearSearch();
          return;
        }
        if (!searchActive || searchHits.length === 0) return;
        if (e.key === "ArrowDown") {
          e.preventDefault();
          setSearchSelectedIndex(i => Math.min(searchHits.length - 1, i < 0 ? 0 : i + 1));
        } else if (e.key === "ArrowUp") {
          e.preventDefault();
          setSearchSelectedIndex(i => Math.max(-1, i - 1));
        } else if (e.key === "Enter" && searchSelectedIndex >= 0) {
          e.preventDefault();
          const hit = searchHits[searchSelectedIndex];
          if (hit) onOpen({ id: hit.id, title: hit.title });
        }
      }, [clearSearch, searchActive, searchHits, searchSelectedIndex, setSearchSelectedIndex, onOpen]);
      const searchRightAdornment = searchPhase === "loading" ? (
        <span className="sigil-spin" style={{
          width: 14, height: 14, borderRadius: "50%",
          border: "2px solid var(--border-strong)", borderTopColor: "var(--fg2)",
          display: "inline-block",
        }} aria-label="Searching"/>
      ) : searchActive ? (
        <button type="button" onClick={clearSearch}
          aria-label="Clear search"
          title="Clear search"
          style={{
            width: 22, height: 22, display: "inline-flex", alignItems: "center", justifyContent: "center",
            background: "transparent", border: "none", color: "var(--fg3)", cursor: "pointer", borderRadius: 2,
          }}
          onMouseEnter={e => { e.currentTarget.style.background = "var(--action-hover)"; e.currentTarget.style.color = "var(--fg1)"; }}
          onMouseLeave={e => { e.currentTarget.style.background = "transparent"; e.currentTarget.style.color = "var(--fg3)"; }}>
          <Icon name="times" size={14}/>
        </button>
      ) : (
        <span title="Press Command-K or Control-K to focus search" style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", padding: "1px 6px", border: "1px solid var(--border-weak)", borderRadius: 999 }}>⌘K</span>
      );

      // Token chart has its own model filter and is driven only by the
      // time range, not the text query (token points carry model, not the
      // searchable conversation fields). The selection lives in App so it
      // survives navigating into a conversation and back; a model that
      // disappears from the store falls back to "all" by derivation.
      const points = tokenPoints || [];
      const tokenModels = useMemo(
        () => Array.from(new Set(points.map(p => p.model).filter(Boolean))).sort(),
        [points]
      );
      const effectiveModel = tokenModels.includes(tokenModel) ? tokenModel : "all";
      const tokenFiltered = useMemo(
        () => effectiveModel === "all" ? points : points.filter(p => p.model === effectiveModel),
        [points, effectiveModel]
      );
      // Legend visibility is shared with the KPI strip so hiding a series
      // rescales the chart and drops it from the headline tokens in step.
      // Lives here, not in TokenChart, so both read the one set.
      const [hiddenSeries, setHiddenSeries] = useState(() => new Set());
      const toggleSeries = useCallback(key => setHiddenSeries(prev => {
        const next = new Set(prev);
        next.has(key) ? next.delete(key) : next.add(key);
        return next;
      }), []);
      // Both metrics share one window so switching the chart between
      // them doesn't shift the time axis; with per-metric windows the
      // "All" range drifts when the datasets' extents differ.
      const chartWindow = useMemo(() => {
        const times = filtered.map(conversationTime).concat(tokenFiltered.map(tokenPointTime));
        return timeWindow(times, timeRange, now);
      }, [filtered, tokenFiltered, timeRange, now]);
      const activity = useMemo(
        () => bucketActivity(filtered, timeRange, now, { window: chartWindow }),
        [filtered, timeRange, now, chartWindow]
      );
      const tokenUsage = useMemo(
        () => bucketTokenUsage(tokenFiltered, timeRange, now, { window: chartWindow }),
        [tokenFiltered, timeRange, now, chartWindow]
      );
      // Bucket drill-down from a chart bar click: the list narrows to
      // conversations active inside the picked bucket, while the charts
      // keep the full window and just highlight the selection.
      const onBucketClick = useCallback(b => {
        setBucketSel(sel => sel && sel.start === b.start && sel.end === b.end ? null : { start: b.start, end: b.end });
      }, [setBucketSel]);
      const listFiltered = useMemo(() => {
        if (!bucketSel) return filtered;
        return filtered.filter(c => {
          const endT = conversationTime(c);
          if (endT == null) return false;
          const startT = new Date(c.started_at).getTime();
          const s = Number.isFinite(startT) ? startT : endT;
          return s < bucketSel.end && endT >= bucketSel.start;
        });
      }, [filtered, bucketSel]);

      const handleSort = useCallback(key => {
        setListSort(s => s.key === key ? { key, dir: s.dir === "desc" ? "asc" : "desc" } : { key, dir: "desc" });
      }, [setListSort]);
      const sorted = useMemo(() => {
        const dir = listSort.dir === "asc" ? 1 : -1;
        const val = c => {
          if (listSort.key === "duration") {
            const d = durationBetweenSeconds(c.started_at, c.last_activity);
            return d == null ? -1 : d;
          }
          if (listSort.key === "tokens") return c.total_tokens || 0;
          if (listSort.key === "cost") return conversationCost(c, prices) || 0;
          const t = conversationTime(c);
          return t == null ? 0 : t;
        };
        return [...listFiltered].sort((a, b) => (val(a) - val(b)) * dir);
      }, [listFiltered, listSort, prices]);

      // KPI tiles read the range + workspace + search set (not the bucket
      // drill-down), computed straight off each conversation's token buckets.
      // This keeps the headline tokens, cost, and cache rate in agreement
      // with the workspace rail and the rows below — all conversation-based —
      // rather than the token-series chart, which keeps its own model filter.
      const kpi = useMemo(() => {
        let calls = 0, errConvs = 0, cost = 0, priced = 0, unpriced = 0;
        const tot = { fresh_input: 0, cache_read: 0, cache_write: 0, output: 0, reasoning: 0 };
        const models = new Set();
        for (const c of filtered) {
          calls += c.calls || 0;
          if (c.status === "err") errConvs++;
          const cc = conversationCost(c, prices);
          if (cc == null) unpriced++; else { cost += cc; priced++; }
          const b = c.token_buckets;
          if (b) for (const k in tot) tot[k] += b[k] || 0;
          for (const m of c.models || []) models.add(m);
        }
        const tokens = tot.fresh_input + tot.cache_read + tot.cache_write + tot.output + tot.reasoning;
        // Cost sub is honest about coverage: if some conversations ran on an
        // unpriced (non-Anthropic) model, say so rather than implying the
        // total covers everything.
        const costSub = unpriced > 0
          ? `${unpriced} unpriced · ${formatCost(priced ? cost / priced : 0)} avg`
          : cost ? `${formatCost(cost / Math.max(1, priced))} avg / session` : "estimated";
        return {
          conversations: filtered.length,
          conversationsSub: activeWorkspace ? "in workspace" : "active in range",
          tokens,
          cost: priced ? cost : null,  // nothing priced → "—", not a misleading $0
          costSub,
          models: models.size,
          cachePct: cacheInputHitPercent(tot.fresh_input, tot.cache_read, tot.cache_write),
          calls,
          avgCalls: filtered.length ? calls / filtered.length : 0,
          errConvs,
          errPct: filtered.length ? Math.round((errConvs / filtered.length) * 100) : 0,
        };
      }, [filtered, activeWorkspace, prices]);

      return (
        <Stack direction="row" align="flex-start" style={{ width: "100%" }}>
          {!searchActive && (
            <WorkspaceSidebar workspaces={workspaces} selected={activeWorkspace} onSelect={setWorkspace}
              totalCount={rangeFiltered.length} totalCost={totalCost}/>
          )}
          <PageShell maxWidth={1400} style={{ flex: 1, minWidth: 0 }}>
          <PageHero
            icon="list"
            kicker="Local sessions"
            title="Sessions"
            desc={searchActive
              ? "Search captured prompts, responses, and tool output across local sessions."
              : "Review captured sessions, token usage, costs, and tool-call activity from local runs."}
            chips={searchActive ? [
              { label: "Index", value: searchMode === "semantic" ? "QMD" : "FTS", tone: "var(--primary-text)" },
              { label: "Results", value: String(searchHits.length), tone: searchHits.length ? "var(--success-text)" : "var(--fg2)" },
              { label: "Status", value: searchPhase === "loading" ? "Searching" : "Ready", tone: searchPhase === "loading" ? "var(--warning-text)" : "var(--fg2)" },
            ] : [
              { label: "Range", value: range.label, tone: "var(--fg2)" },
              { label: "Workspaces", value: String(workspaces.length), tone: "var(--primary-text)" },
              { label: "Cost", value: formatCost(totalCost), tone: "var(--brand-orange-text)" },
            ]}>
            <Box style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
              {searchActive ? "Full-text search runs across all captured sessions." : "Local traces, token accounting, and model usage from this viewer."}
            </Box>
          </PageHero>
          <ForwardBanner onOpenSettings={onOpenSettings}/>
          <FilterBar
            query={query}
            onQueryChange={setQuery}
            inputRef={searchInputRef}
            timeRange={searchActive ? null : timeRange}
            onTimeRangeChange={searchActive ? null : setTimeRange}
            agentFilter={searchActive ? undefined : activeAgentFilter}
            onAgentFilterChange={searchActive ? undefined : setAgentFilter}
            agentOptions={searchActive ? [] : agentOptions}
            modelFilter={searchActive ? undefined : activeModelFilter}
            onModelFilterChange={searchActive ? undefined : setModelFilter}
            modelOptions={searchActive ? [] : modelFacetOptions}
            statusFilter={searchActive ? undefined : activeStatusFilter}
            onStatusFilterChange={searchActive ? undefined : setStatusFilter}
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
          ) : (
          <React.Fragment>
          <KpiStrip kpi={kpi}/>
          {chartMetric === "activity"
            ? <ActivityChart data={activity.buckets} bucketLabel={activity.bucketLabel}
                selection={bucketSel} onBucketClick={onBucketClick}
                switcher={<ChartSwitch value={chartMetric} onChange={setChartMetric}/>}/>
            : <TokenChart data={tokenUsage.buckets} bucketLabel={tokenUsage.bucketLabel} grandTotal={tokenUsage.grandTotal} models={tokenModels} model={effectiveModel} onModelChange={setTokenModel}
                hidden={hiddenSeries} onToggleSeries={toggleSeries}
                selection={bucketSel} onBucketClick={onBucketClick}
                switcher={<ChartSwitch value={chartMetric} onChange={setChartMetric}/>}/>}

          {bucketSel && (
            <div style={{ marginTop: 10, display: "flex", alignItems: "center", gap: 10, fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg2)" }}>
              <span>
                Showing {formatBucketLabel(bucketSel.start, bucketSel.end - bucketSel.start)} – {formatBucketLabel(bucketSel.end, bucketSel.end - bucketSel.start)}
              </span>
              <button onClick={() => setBucketSel(null)}
                style={{ background: "transparent", border: "1px solid var(--border-medium)", borderRadius: 2, color: "var(--fg2)", cursor: "pointer", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", padding: "1px 8px" }}>
                ✕ clear
              </button>
            </div>
          )}

          <SurfaceCard style={{
            marginTop: 18,
            overflow: "hidden",
          }}>
            <div style={{
              display: "grid",
              gridTemplateColumns: CONV_GRID,
              alignItems: "center", gap: 16,
              padding: "11px 16px 11px 19px",
              borderBottom: "1px solid var(--border-weak)",
              background: "var(--bg-secondary)",
              fontFamily: "var(--fontFamily)", fontSize: 12, color: "var(--fg3)", fontWeight: 500,
            }}>
              <SortHeader label="Last activity" sortKey="last_activity" sort={listSort} onSort={handleSort}/>
              <span>Session</span>
              <span>Agent</span>
              <SortHeader label="Cost" sortKey="cost" sort={listSort} onSort={handleSort}/>
              <SortHeader label="Tokens" sortKey="tokens" sort={listSort} onSort={handleSort}/>
              <SortHeader label="Duration" sortKey="duration" sort={listSort} onSort={handleSort}/>
              <span>Models</span>
            </div>

            {error && (
              <div style={{ padding: 16 }}>
                <Notice kind="error" title="Failed to load sessions">{error}</Notice>
              </div>
            )}
            {!error && loading && conversations.length === 0 && (
              <div style={{ padding: "32px 18px", color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>Loading…</div>
            )}
            {!error && !loading && conversations.length === 0 && (
              <div style={{ padding: 16 }}>
                <Notice kind="info" title="No sessions yet">
                  Run an agent against this daemon with <code style={{ color: "var(--fg1)" }}>agento11y pi --local</code> or <code style={{ color: "var(--fg1)" }}>agento11y claude --local</code>. Captured generations appear here as soon as the agent emits its first one.
                </Notice>
              </div>
            )}
            {!error && conversations.length > 0 && rangeFiltered.length === 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No sessions in <code style={{ color: "var(--fg1)" }}>{range.label}</code>.
              </div>
            )}
            {!error && filtered.length === 0 && rangeFiltered.length > 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No sessions match the current filters.
              </div>
            )}
            {!error && bucketSel && listFiltered.length === 0 && filtered.length > 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No sessions in the selected bucket.
              </div>
            )}
            {sorted.map(c => <ConvRow key={c.id} c={c} now={now} onOpen={onOpen} prices={prices}/>)}
          </SurfaceCard>

          <div style={{
            marginTop: 14, padding: "10px 14px",
            fontSize: 11, color: "var(--fg3)",
            fontFamily: "var(--fontFamilyMonospace)",
          }}>
            {sorted.length} of {filtered.length} {filtered.length === 1 ? "session" : "sessions"}
            {activeFilterCount > 0 ? ` · ${activeFilterCount} ${activeFilterCount === 1 ? "filter" : "filters"} active` : ""}
          </div>
          </React.Fragment>
          )}
          </PageShell>
        </Stack>
      );
    }

    // ============================================================
    // Screen 2 — Conversation detail
    // ============================================================

    function agentBadge(name) {
      if (!name) return "?";
      const cleaned = name.replace(/[^a-zA-Z]/g, "");
      return cleaned.slice(0, 2).toUpperCase() || "?";
    }

    // MessageBubble renders one captured message (user / assistant / tool)
    // with its visible parts. The label and accent colour come from the role;
    // unknown roles fall back to a neutral grey label.
    // partKind normalises a part to its kind, tolerating the shorthand shape
    // where the kind is implied by which field is set.
    function partKind(p) {
      return p.kind || (p.text ? "text" : p.thinking ? "thinking" : p.tool_call ? "tool_call" : p.tool_result ? "tool_result" : "unknown");
    }

    // segMeta maps a (role, kind) to its block accent and label. Each part of
    // a message renders as its own block, so the assistant's prose, its
    // reasoning, and each tool call are visually distinct instead of sharing
    // one "TOOL CALL" header. Tool call/result blocks carry their own inline
    // header (→ name / ← result), so they need no separate label.
    function segMeta(role, kind) {
      if (kind === "thinking")    return { label: "",           color: "var(--viz-blue)" };
      if (kind === "tool_call")   return { label: "",           color: "var(--warning-text)" };
      if (kind === "tool_result") return { label: "",           color: "var(--viz-purple)" };
      if (role === "user")        return { label: "USER",       color: "var(--viz-green)" };
      if (role === "tool")        return { label: "",           color: "var(--viz-purple)" };
      return { label: "ASSISTANT", color: "var(--brand-orange)" };
    }

    // partHasContent skips empties so we never draw a labelled block around
    // nothing (e.g. an empty text part, or imported thinking whose content
    // Claude Code didn't persist).
    function partHasContent(p, kind) {
      if (kind === "text") return !!(p.text || "").trim();
      if (kind === "thinking") return !!(p.thinking || "").trim();
      if (kind === "tool_call") return !!p.tool_call;
      if (kind === "tool_result") return !!p.tool_result;
      return false;
    }

    function MessageBubble({ msg }) {
      const role = msg.role || "";
      const parts = (msg.parts || []).map(p => ({ p, kind: partKind(p) })).filter(({ p, kind }) => partHasContent(p, kind));
      if (parts.length === 0) return null;
      return parts.map(({ p, kind }, i) => {
        const meta = segMeta(role, kind);
        return (
          <div key={i} style={{ borderLeft: `2px solid ${meta.color}`, padding: "6px 12px", background: "var(--bg-canvas)", borderRadius: 2, marginBottom: 6 }}>
            {meta.label && (
              <div style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, color: meta.color, letterSpacing: "0.08em", marginBottom: 4 }}>{meta.label}</div>
            )}
            <MessagePart part={p}/>
          </div>
        );
      });
    }

    // ThinkingPart collapses a thinking block to a single toggle line so
    // an empty or long chain-of-thought doesn't take over the turn. The
    // SDK doesn't record a per-part token count, so the line is just the
    // label; expanding reveals the captured text.
    function ThinkingPart({ text }) {
      const [open, setOpen] = useState(false);
      return (
        <div>
          <div onClick={() => setOpen(o => !o)} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer", color: "var(--viz-blue)", fontSize: 10, letterSpacing: "0.08em", fontFamily: "var(--fontFamilyMonospace)" }}>
            <Icon name={open ? "chevron" : "cright"} size={10} style={{ color: "var(--viz-blue)" }}/>
            REASONING
          </div>
          {open && <div style={{ fontSize: 12.5, color: "var(--fg2)", whiteSpace: "pre-wrap", marginTop: 6, fontStyle: "italic" }}>{text}</div>}
        </div>
      );
    }

    // CappedBlock renders a <pre> capped to ~208px with a bottom fade and
    // a "Show all N lines" toggle once the content runs past the cap, so a
    // single huge tool result (an ls/tree dump) can't stretch the page to
    // thousands of pixels.
    function CappedBlock({ children, lineCount, preStyle }) {
      const [open, setOpen] = useState(false);
      const base = { background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "8px 10px", margin: "4px 0 0", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, lineHeight: 1.6, color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all", ...(preStyle || {}) };
      if (lineCount <= 14 || open) {
        return <pre style={base}>{children}</pre>;
      }
      return (
        <div style={{ position: "relative" }}>
          <pre style={{ ...base, maxHeight: 208, overflow: "hidden" }}>{children}</pre>
          <div style={{ position: "absolute", left: 0, right: 0, bottom: 0, height: 96, background: "linear-gradient(to bottom, transparent, var(--bg-primary))", display: "flex", alignItems: "flex-end", justifyContent: "center", paddingBottom: 8, pointerEvents: "none" }}>
            <span onClick={() => setOpen(true)} style={{ pointerEvents: "auto", display: "inline-flex", alignItems: "center", gap: 6, height: 26, padding: "0 12px", background: "var(--bg-secondary)", border: "1px solid var(--border-medium)", borderRadius: 2, fontSize: 11, color: "var(--fg1)", cursor: "pointer" }}>
              <Icon name="chevron" size={11} style={{ color: "var(--fg3)" }}/>Show all {lineCount} lines
            </span>
          </div>
        </div>
      );
    }

    // toolCallArgPreview is the one-line essence of a tool call's input, shown
    // on the collapsed chip: the command for Bash, else the first meaningful
    // field (path/pattern/query/url), else the raw JSON — truncated.
    function toolCallArgPreview(input) {
      if (!input) return "";
      if (typeof input === "string") return input.length > 140 ? input.slice(0, 140) + "…" : input;
      for (const k of ["command", "file_path", "path", "pattern", "query", "url", "cmd", "name"]) {
        if (input[k] != null && input[k] !== "") return String(input[k]).replace(/\s+/g, " ");
      }
      try { const s = JSON.stringify(input); return s.length > 140 ? s.slice(0, 140) + "…" : s; } catch (_) { return ""; }
    }

    // ToolCallPart renders one tool call as a compact, collapse-first chip — an
    // arrow, the tool name, and a one-line preview of its input — that expands
    // on click to the full command/args.
    function ToolCallPart({ tc }) {
      const [show, setShow] = useState(false);
      const input = tc.input_json || null;
      const command = tc.name === "Bash" && input && typeof input === "object" && input.command ? input.command : "";
      const description = tc.name === "Bash" && input && typeof input === "object" && input.description ? input.description : "";
      const args = input ? (typeof input === "string" ? input : JSON.stringify(input, null, 2)) : "";
      const preview = command || toolCallArgPreview(input);
      const hasBody = !!(command || args);

      return (
        <div style={{ marginTop: 4, position: "relative" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
            <button onClick={() => hasBody && setShow(s => !s)}
              style={{ flex: 1, minWidth: 0, display: "flex", alignItems: "center", gap: 7, background: "transparent", border: "none", padding: 0, cursor: hasBody ? "pointer" : "default", textAlign: "left", fontFamily: "var(--fontFamilyMonospace)", fontSize: 11 }}>
              {hasBody && <Icon name={show ? "chevron" : "cright"} size={10} style={{ color: "var(--fg3)", flex: "none" }}/>}
              <span style={{ color: "var(--warning-text)", fontSize: 9.5, letterSpacing: "0.08em", flex: "none" }}>OUT</span>
              <span style={{ color: "var(--fg1)", fontWeight: 600, flex: "none" }}>{tc.name}</span>
              {preview && <span style={{ color: "var(--fg3)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", minWidth: 0 }}>{preview}</span>}
            </button>
          </div>
          {show && description && <div style={{ marginTop: 4, color: "var(--fg2)", fontSize: 12 }}>{description}</div>}
          {show && (command ? (
            <CappedBlock lineCount={command.split("\n").length}><span style={{ color: "var(--warning-text)" }}>$</span> {command}</CappedBlock>
          ) : args && (
            <CappedBlock lineCount={args.split("\n").length} preStyle={{ fontSize: 11 }}>{args}</CappedBlock>
          ))}
        </div>
      );
    }

    // MessagePart picks a renderer per part kind. Text and thinking are
    // wrapped pre-line so newlines from the model render naturally;
    // tool calls and tool results show a compact label + payload so the
    // viewer reads a complete turn at a glance.
    function MessagePart({ part }) {
      const kind = part.kind || (part.text ? "text" : part.thinking ? "thinking" : part.tool_call ? "tool_call" : part.tool_result ? "tool_result" : "unknown");
      if (kind === "text" && part.text) {
        return (
          <div style={{ fontSize: 13, color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-word" }}>{part.text}</div>
        );
      }
      if (kind === "thinking" && part.thinking) {
        return <ThinkingPart text={part.thinking}/>;
      }
      if (kind === "tool_call" && part.tool_call) {
        return <ToolCallPart tc={part.tool_call}/>;
      }
      if (kind === "tool_result" && part.tool_result) {
        return <ToolResultPart tr={part.tool_result}/>;
      }
      return null;
    }

    // ToolResultPart renders a tool result as a collapse-first chip matching the
    // tool call: a left arrow, line count / error flag, and a one-line preview
    // of the output, expanding on click to the full body. Errors open expanded
    // since they're the thing you came to read.
    function ToolResultPart({ tr }) {
      const body = tr.content || (tr.content_json ? (typeof tr.content_json === "string" ? tr.content_json : JSON.stringify(tr.content_json)) : "");
      const isErr = !!tr.is_error;
      const [show, setShow] = useState(isErr);
      const lineCount = body ? body.split("\n").length : 0;
      const hasBody = !!body;
      const firstLine = body ? body.split("\n").find(l => l.trim()) || "" : "";
      return (
        <div style={{ marginTop: 4 }}>
          <button onClick={() => hasBody && setShow(s => !s)}
            style={{ display: "flex", alignItems: "center", gap: 7, width: "100%", minWidth: 0, background: "transparent", border: "none", padding: 0, cursor: hasBody ? "pointer" : "default", textAlign: "left", fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: isErr ? "var(--error-text)" : "var(--fg2)" }}>
            {hasBody && <Icon name={show ? "chevron" : "cright"} size={10} style={{ color: "var(--fg3)", flex: "none" }}/>}
            <span style={{ color: "var(--viz-green)", fontSize: 9.5, letterSpacing: "0.08em", flex: "none" }}>IN</span>
            <span style={{ flex: "none" }}>result{lineCount > 0 ? <span style={{ color: "var(--fg3)" }}> · {lineCount} {lineCount === 1 ? "line" : "lines"}</span> : null}{isErr ? <span style={{ color: "var(--error-text)" }}> · error</span> : null}</span>
            {!show && firstLine && <span style={{ color: "var(--fg3)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", minWidth: 0 }}>{firstLine}</span>}
          </button>
          {show && body && <CappedBlock lineCount={lineCount} preStyle={{ fontSize: 11 }}>{body}</CappedBlock>}
        </div>
      );
    }

    // MessageThread renders the captured messages for one step. The API's
    // messages field is already ordered for display, so tool calls appear
    // before their matching results even though the raw SDK input/output split
    // stores tool results on the input side. When no content is present
    // (metadata-only capture), it shows a hint pointing at
    // SIGIL_CONTENT_CAPTURE_MODE so the empty state is self-explanatory.
    function MessageThread({ step }) {
      const input = step.input || [];
      const output = step.output || [];
      const messages = (step.messages && step.messages.length > 0) ? step.messages : input.concat(output);
      if (messages.length === 0) {
        return (
          <div style={{
            color: "var(--fg3)", fontSize: 12,
            fontFamily: "var(--fontFamilyMonospace)", marginBottom: 10,
            padding: "8px 12px",
            border: "1px dashed var(--border-weak)", borderRadius: 2,
          }}>
            No message content captured. Re-run with <code style={{ color: "var(--fg1)" }}>SIGIL_CONTENT_CAPTURE_MODE=full</code> to record prompts and responses.
          </div>
        );
      }
      return (
        <div style={{ marginBottom: 10 }}>
          {messages.map((m, i) => <MessageBubble key={`m${i}`} msg={m}/>)}
        </div>
      );
    }

    // StepTokenBar shows one step's disjoint token buckets: a thin
    // proportional stacked bar plus labeled counts in the chart's series
    // colors. Answers "did this step hit the prompt cache?" at a glance.
    function StepTokenBar({ buckets }) {
      if (!buckets) return null;
      const parts = TOKEN_SERIES.map(s => ({ ...s, v: buckets[s.key] || 0 })).filter(p => p.v > 0);
      const total = parts.reduce((acc, p) => acc + p.v, 0);
      if (total === 0) return null;
      return (
        <div style={{ marginBottom: 10 }}>
          <div style={{ display: "flex", height: 4, borderRadius: 1, overflow: "hidden", marginBottom: 6 }}>
            {parts.map(p => <span key={p.key} style={{ width: `${(p.v / total) * 100}%`, background: p.color }}/>)}
          </div>
          <div style={{ display: "flex", gap: 12, flexWrap: "wrap", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg2)" }}>
            {parts.map(p => (
              <span key={p.key} style={{ display: "inline-flex", alignItems: "center", gap: 5 }}>
                <span style={{ width: 8, height: 8, background: p.color, borderRadius: 1 }}/>
                {p.label} <span style={{ color: "var(--fg1)" }}>{formatTokens(p.v)}</span>
              </span>
            ))}
          </div>
        </div>
      );
    }

    // firstUserText returns the fresh user prompt in a step's input — a
    // user-role message with text — distinguishing a human turn from tool
    // results (role "tool"). A generation is the assistant's response to its
    // input, so the input carries the question that triggered the step.
    function firstUserText(step) {
      for (const m of (step.input || [])) {
        if (m.role !== "user") continue;
        for (const p of (m.parts || [])) {
          if (p.kind === "text" && (p.text || "").trim()) return p.text.trim();
        }
      }
      return "";
    }

    // stepGlance reduces a step to the one-line essence shown in a collapsed
    // row, role-aware: a step triggered by a human prompt leads with that
    // question (role "user"); otherwise it shows the assistant's work — the
    // tool it ran as a chip plus its leading prose, or its final answer. This
    // makes the thread scan as a real conversation: user asks → agent works →
    // agent answers.
    function stepGlance(step, i, total, skipUser) {
      const userText = skipUser ? "" : firstUserText(step);
      if (userText) return { role: "user", tool: "", text: userText, mono: false };
      const tool = (step.tools && step.tools[0]) || "";
      const prose = leadingAssistantText(step);
      let text = prose || step.tool_preview || "";
      if (!text) text = i === 0 ? "Initial prompt" : (i === total - 1 ? "Final response" : "Response");
      return { role: "assistant", tool, text, mono: !prose && !!step.tool_preview };
    }

    // StepDetailBody is the expanded content of a step: a light meta line,
    // the token split, any call error, the message thread, and a bare tool
    // preview when no rendered tool calls carry it. Shared by the thread row
    // and any other place that needs to show a step in full.
    function StepDetailBody({ step }) {
      const hasError = !!step.call_error;
      const hasReasoning = (step.messages || step.output || []).some(m => (m.parts || []).some(p => (p.kind === "thinking" || p.thinking) && (p.thinking || "").trim()));
      const reasonedNoText = step.thinking_enabled && !hasReasoning;
      return (
        <div style={{ padding: "12px 16px 16px", background: "var(--bg-canvas)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 14, marginBottom: 12, color: "var(--fg3)", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", flexWrap: "wrap" }}>
            {step.model && <span style={{ color: "var(--fg2)" }}>{step.model}</span>}
            <span>{formatTime(step.completed_at || step.started_at)}</span>
            <span>{(step.tools || []).length} {(step.tools || []).length === 1 ? "tool" : "tools"}</span>
            {step.provider && <span>{step.provider}</span>}
            {step.stop_reason && <span>{step.stop_reason}</span>}
            {hasReasoning && <span style={{ color: "var(--viz-blue)" }}>reasoning</span>}
            {reasonedNoText && <span title="The model reasoned on this step, but Claude Code does not persist reasoning text to its transcript.">reasoning · not recorded</span>}
          </div>
          <StepTokenBar buckets={step.token_buckets}/>
          {hasError && <div style={{ marginBottom: 10 }}><Notice kind="error" title="Call error">{step.call_error}</Notice></div>}
          <MessageThread step={step}/>
          {step.tool_preview && !(step.output || []).some(m => (m.parts || []).some(p => p.kind === "tool_call" || p.tool_call)) && (
            <div style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "8px 12px", marginTop: 10, fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, color: "var(--fg2)", display: "flex", alignItems: "flex-start", gap: 8 }}>
              <span style={{ color: "var(--warning-text)" }}>$</span>
              <code style={{ color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{step.tool_preview}</code>
            </div>
          )}
        </div>
      );
    }

    // StepCard is one row in the collapse-first thread list: a compact,
    // scannable line (status dot, number, tool chip, summary, duration ·
    // tokens) that expands inline to the full StepDetailBody. It lives inside
    // a shared list container, so it draws only a bottom separator — no card
    // chrome of its own. accent colours the status dot/active edge by agent.
    function StepCard({ step, n, total, accent, expanded, onToggle, active, last, innerRef, flash, turnStart }) {
      const hasError = !!step.call_error;
      const glance = stepGlance(step, n - 1, total, turnStart);
      const isUser = glance.role === "user";
      const dot = hasError ? "var(--error-text)" : (accent || "var(--viz-green)");
      const leftAccent = active ? "var(--brand-orange)" : (hasError ? "var(--error-main)" : (isUser ? "var(--viz-green)" : "transparent"));
      return (
        <div ref={innerRef} className={flash ? "sigil-step-flash" : undefined} style={{ borderBottom: last && !expanded ? "none" : "1px solid var(--border-weak)" }}>
          <div onClick={onToggle} style={{
            display: "grid", gridTemplateColumns: "auto auto 1fr auto auto", alignItems: "center", gap: 10,
            padding: "0 12px", height: 34, cursor: "pointer",
            borderLeft: `2px solid ${leftAccent}`,
            background: active ? "var(--action-selected)" : (isUser ? "rgba(115,191,105,0.04)" : "transparent"),
          }}
          onMouseEnter={e => { if (!active) e.currentTarget.style.background = isUser ? "rgba(115,191,105,0.07)" : "rgba(204,204,220,0.03)"; }}
          onMouseLeave={e => { if (!active) e.currentTarget.style.background = isUser ? "rgba(115,191,105,0.04)" : "transparent"; }}>
            {isUser
              ? <span style={{ width: 7, height: 7, borderRadius: "50%", border: "1.5px solid var(--viz-green)", boxSizing: "border-box", flexShrink: 0 }}/>
              : <span style={{ width: 7, height: 7, borderRadius: "50%", background: dot, flexShrink: 0 }}/>}
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, color: "var(--fg3)", minWidth: 16, textAlign: "right" }}>{n}</span>
            <span style={{ display: "flex", alignItems: "center", gap: 8, minWidth: 0 }}>
              {isUser && (
                <span style={{ flexShrink: 0, fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, letterSpacing: "0.06em", color: "var(--viz-green)", background: "rgba(115,191,105,0.1)", border: "1px solid rgba(115,191,105,0.25)", borderRadius: 2, padding: "0 6px", lineHeight: "16px" }}>USER</span>
              )}
              {!isUser && glance.tool && (
                <span style={{ flexShrink: 0, fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg2)", background: "rgba(204,204,220,0.06)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "0 6px", lineHeight: "16px" }}>{glance.tool}</span>
              )}
              <span style={{ minWidth: 0, fontFamily: glance.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)", fontSize: glance.mono ? 11.5 : 12.5, color: isUser ? "var(--fg-max)" : (glance.tool && glance.mono ? "var(--fg2)" : "var(--fg1)"), overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{glance.text}</span>
            </span>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", display: "flex", gap: 10, whiteSpace: "nowrap", justifyContent: "flex-end" }}>
              <span>{formatDuration(step.duration_seconds)}</span>
              <span style={{ minWidth: 42, textAlign: "right" }}>{formatTokens(step.total_tokens)}</span>
            </span>
            <Icon name={expanded ? "chevron" : "cright"} size={12} style={{ color: "var(--fg3)" }}/>
          </div>
          {expanded && <StepDetailBody step={step}/>}
        </div>
      );
    }

    // TurnHeader is the banner that opens a user turn: the prompt lifted out
    // of the step it rode in on, so the loop reads user → steps → user instead
    // of a "USER" row that hid the assistant's work. The rollup is honest about
    // the two numbers that differ in an agentic loop — billed (cumulative spend
    // across the turn) vs ctx (the live context size, which only climbs).
    function TurnHeader({ turn, last }) {
      return (
        <div style={{
          display: "flex", alignItems: "flex-start", gap: 10,
          padding: "10px 12px", borderBottom: last ? "none" : "1px solid var(--border-weak)",
          borderLeft: "2px solid var(--viz-green)", background: "rgba(115,191,105,0.06)",
        }}>
          <span style={{ flexShrink: 0, marginTop: 1, fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, letterSpacing: "0.06em", color: "var(--viz-green)", background: "rgba(115,191,105,0.1)", border: "1px solid rgba(115,191,105,0.25)", borderRadius: 2, padding: "1px 6px", lineHeight: "16px" }}>USER</span>
          <div style={{ flex: 1, minWidth: 0 }}>
            <div style={{ color: "var(--fg-max)", fontSize: 13, lineHeight: 1.5, display: "-webkit-box", WebkitLineClamp: 3, WebkitBoxOrient: "vertical", overflow: "hidden", whiteSpace: "pre-wrap", wordBreak: "break-word" }}>{turn.userText}</div>
            <div style={{ marginTop: 5, fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>
              turn {turn.index} · {turn.gens.length} {turn.gens.length === 1 ? "step" : "steps"} · <span style={{ color: "var(--fg2)" }}>{formatTokens(turn.billed)}</span> billed · <span style={{ color: "var(--fg2)" }}>{formatTokens(turn.generated)}</span> generated · ctx <span style={{ color: "var(--fg2)" }}>{formatTokens(turn.ctxIn)}</span>
            </div>
          </div>
        </div>
      );
    }

    // leadingAssistantText returns the step's first prose line — the assistant's
    // narration that precedes any tool call (e.g. "Now let me explore the
    // existing code structure."). It makes a far more legible rail label than
    // the tool form, so it wins when present.
    function leadingAssistantText(step) {
      for (const m of (step.output || [])) {
        for (const p of (m.parts || [])) {
          if (p.kind === "text") {
            const t = (p.text || "").trim();
            if (t) return t;
          }
        }
      }
      return "";
    }

    // stepRailSummary picks a one-line label for a rail row: the assistant's
    // leading prose when the step has any, else the first tool call's name +
    // preview, else a position heuristic ("Initial prompt" / "Final response").
    // mono renders the tool form in Roboto Mono; prose labels stay in Inter.
    function stepRailSummary(step, i, total) {
      const prose = leadingAssistantText(step);
      if (prose) return { label: prose, mono: false };
      const tool = (step.tools && step.tools[0]) || "";
      if (tool) {
        const preview = step.tool_preview ? ` · ${step.tool_preview}` : "";
        return { label: `${tool}${preview}`, mono: true };
      }
      if (i === 0) return { label: "Initial prompt", mono: false };
      if (i === total - 1) return { label: "Final response", mono: false };
      return { label: "Response", mono: false };
    }

    // StepRail is the sticky left navigator for long traces. Each row
    // mirrors a StepCard: number, a tool/prose summary, duration · tokens
    // (warning on the slowest step, error on a failed one), and a status
    // dot. Clicking a row expands and scrolls to that step.
    // ── Subagent forest ────────────────────────────────────────────────
    // Generations arrive as a flat chronological list. parent_generation_ids
    // links each step to the one that caused it: a same-agent edge chains one
    // agent's calls; a cross-agent edge (parent has a different agent_name) is
    // a subagent launch. We fold same-agent chains into "runs" (so a 100-call
    // subagent is one nesting level, not 100), then nest each run under the
    // generation that spawned it. Main-agent calls have no parent, so each is
    // its own top-level run — together they are the primary thread.

    const tsMs = s => { const t = s ? new Date(s).getTime() : 0; return Number.isFinite(t) ? t : 0; };

    // agentShort drops the "claude-code/" prefix; agentColor tints a subagent
    // by kind so lanes/badges are scannable. The main agent keeps the brand.
    function agentShort(name) {
      if (!name) return "main";
      const i = name.indexOf("/");
      return i === -1 ? name : name.slice(i + 1);
    }
    function isSubagent(name) { return !!name && name.indexOf("/") !== -1; }
    function agentColor(name) {
      const s = agentShort(name);
      if (!isSubagent(name)) return "var(--brand-orange)";
      if (s.includes("explore")) return "var(--viz-blue)";
      if (s.includes("general")) return "var(--viz-purple)";
      if (s.includes("fork")) return "var(--viz-green)";
      return "var(--viz-yellow)";
    }

    function buildSubagentForest(gens) {
      const byId = new Map((gens || []).map(g => [g.generation_id, g]));
      const inConvParent = g => {
        const p = (g.parent_generation_ids || [])[0];
        return p && byId.has(p) ? byId.get(p) : null;
      };
      // Run root: walk up while the parent shares this agent_name.
      const runRootId = g => {
        let cur = g; const seen = new Set();
        for (;;) {
          if (seen.has(cur.generation_id)) return cur.generation_id; // cycle guard
          seen.add(cur.generation_id);
          const p = inConvParent(cur);
          if (p && (p.agent_name || "") === (cur.agent_name || "")) { cur = p; continue; }
          return cur.generation_id;
        }
      };
      const runs = new Map();
      for (const g of (gens || [])) {
        const rid = runRootId(g);
        let run = runs.get(rid);
        if (!run) { run = { id: rid, agent: (byId.get(rid) || g).agent_name, gens: [] }; runs.set(rid, run); }
        run.gens.push(g);
      }
      const spawnedBy = new Map(); // parentGenId -> [run]
      const topRuns = [];
      for (const run of runs.values()) {
        run.gens.sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
        run.start = Math.min(...run.gens.map(g => tsMs(g.started_at) || Infinity));
        run.end = Math.max(...run.gens.map(g => tsMs(g.completed_at) || tsMs(g.started_at) || 0));
        run.totalTokens = run.gens.reduce((a, g) => a + (g.total_tokens || 0), 0);
        run.hasError = run.gens.some(g => g.call_error);
        // Only a cross-agent parent is a real spawn. A same-agent parent on a
        // run root only happens when parent_generation_ids has a cycle (the
        // run-root walk bailed on the guard); treat those as top-level so no
        // run is orphaned.
        const sp = inConvParent(byId.get(run.id));
        if (sp && (sp.agent_name || "") !== (run.agent || "")) {
          if (!spawnedBy.has(sp.generation_id)) spawnedBy.set(sp.generation_id, []);
          spawnedBy.get(sp.generation_id).push(run);
        } else {
          topRuns.push(run);
        }
      }
      for (const arr of spawnedBy.values()) arr.sort((a, b) => a.start - b.start);
      topRuns.sort((a, b) => a.start - b.start);
      const setDepth = (run, d) => {
        run.depth = d;
        run.gens.forEach(g => (spawnedBy.get(g.generation_id) || []).forEach(c => setDepth(c, d + 1)));
      };
      topRuns.forEach(r => setDepth(r, 0));
      return { runs, spawnedBy, topRuns, byId };
    }

    // flattenForest walks the forest depth-first into render rows. Each row
    // carries its nesting depth, owning run, the run-id path to it (for
    // collapse), and isRunStart (true on a subagent run's first step, where
    // the group header is drawn). Number = 1-based DFS order, stable across
    // collapse, shared by the rail and the cards.
    function flattenForest(forest) {
      const out = [];
      const seen = new Set(); // guards against cross-agent parent cycles
      const visit = (run, depth, path) => {
        if (seen.has(run.id)) return;
        seen.add(run.id);
        const rp = path.concat(run.id);
        run.gens.forEach((gen, i) => {
          out.push({ gen, depth, run, runPath: rp, isRunStart: i === 0 && depth > 0 });
          (forest.spawnedBy.get(gen.generation_id) || []).forEach(child => visit(child, depth + 1, rp));
        });
      };
      forest.topRuns.forEach(r => visit(r, 0, []));
      // Belt-and-suspenders: any run not reached above (cycle-orphaned) is
      // emitted at top level, so no generation is ever dropped from the view.
      forest.runs.forEach(r => { if (!seen.has(r.id)) visit(r, 0, []); });
      out.forEach((row, i) => { row.n = i + 1; });
      return out;
    }

    // subagentRuns returns every spawned run (depth > 0) ordered by start —
    // the timeline's non-main lanes.
    function subagentRuns(forest) {
      return [...forest.runs.values()].filter(r => r.depth > 0).sort((a, b) => a.start - b.start);
    }

    // stepTokenWork splits a step's tokens into what it actually did,
    // excluding cache_read. cache_read is just the reused context size — it
    // climbs monotonically through a run, so total_tokens is a misleading
    // "biggest step" signal (it's almost always a late step). generated is
    // what the model produced; ingested is new content the step pulled into
    // context (a big file read, fresh prompt); work is the sum.
    function stepTokenWork(gen) {
      const tb = (gen && gen.token_buckets) || {};
      const generated = (tb.output || 0) + (tb.reasoning || 0);
      const ingested = (tb.fresh_input || 0) + (tb.cache_write || 0);
      return { generated, ingested, work: generated + ingested };
    }


    // Hotspots is the "what's worth looking at" strip above the thread: a
    // clickable chip for any errors, the slowest step, and the most
    // token-hungry step. Each jumps to that step. This surfaces the outliers
    // that a per-step minimap encoding can't on a long trace.
    function Hotspots({ hot, onJump }) {
      const chip = (color, head, sub, onClick) => (
        <button onClick={onClick} title="Jump to this step" style={{
          display: "inline-flex", alignItems: "center", gap: 6, height: 24, padding: "0 9px",
          borderRadius: 12, cursor: "pointer", background: "transparent",
          border: `1px solid ${color}`, color: "var(--fg1)", fontFamily: "var(--fontFamily)", fontSize: 11.5,
        }}
        onMouseEnter={e => e.currentTarget.style.background = "rgba(204,204,220,0.05)"}
        onMouseLeave={e => e.currentTarget.style.background = "transparent"}>
          <span style={{ width: 6, height: 6, borderRadius: "50%", background: color, flexShrink: 0 }}/>
          <span style={{ color: "var(--fg2)" }}>{head}</span>
          <span style={{ fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg1)" }}>{sub}</span>
        </button>
      );
      const r = (row, extra) => `#${row.n} · ${extra}`;
      return (
        <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontSize: 11, color: "var(--fg3)" }}>Hotspots</span>
          {hot.errors.length > 0 && chip("var(--error-text)", hot.errors.length === 1 ? "error" : `${hot.errors.length} errors`, `#${hot.errors[0].n}`, () => onJump(hot.errors[0].gen.generation_id))}
          {chip("var(--warning-text)", "slowest", r(hot.slowest, formatDuration(hot.slowest.gen.duration_seconds)), () => onJump(hot.slowest.gen.generation_id))}
          {chip("var(--viz-orange)", "most generated", r(hot.mostGenerated, formatTokens(stepTokenWork(hot.mostGenerated.gen).generated)), () => onJump(hot.mostGenerated.gen.generation_id))}
          {chip("var(--viz-blue)", "most read", r(hot.mostRead, formatTokens(stepTokenWork(hot.mostRead.gen).ingested)), () => onJump(hot.mostRead.gen.generation_id))}
        </div>
      );
    }

    // TimelineView is the trace waterfall: a shared time axis with one lane
    // for the main agent and one per subagent run, each bar a generation
    // positioned by its real start/end. Concurrent subagents show as bars
    // overlapping in time — the one thing the vertical thread can't express.
    // Clicking a bar jumps to that step in the tree.
    // mergedSpan returns the total wall time covered by at least one call
    // (intervals unioned), so idle gaps between calls can be reported.
    function mergedSpan(intervals) {
      const iv = intervals.filter(x => x[1] > x[0]).sort((a, b) => a[0] - b[0]);
      let total = 0, curS = -1, curE = -1;
      for (const [s, e] of iv) {
        if (s > curE) { if (curE > curS) total += curE - curS; curS = s; curE = e; }
        else curE = Math.max(curE, e);
      }
      if (curE > curS) total += curE - curS;
      return total;
    }
    // peakConcurrency is the most calls in flight at once — the timeline's
    // headline number for parallelism.
    function peakConcurrency(intervals) {
      const ev = [];
      intervals.forEach(([s, e]) => { if (e > s) { ev.push([s, 1]); ev.push([e, -1]); } });
      ev.sort((a, b) => a[0] - b[0] || a[1] - b[1]);
      let cur = 0, peak = 0;
      for (const [, d] of ev) { cur += d; peak = Math.max(peak, cur); }
      return peak;
    }

    // TimelineTooltip is the floating card shown while hovering a step bar or
    // a turn marker. It follows the cursor (fixed-positioned, clamped to the
    // viewport) and never intercepts pointer events.
    function TimelineTooltip({ tip }) {
      const W = tip.kind === "turn" ? 300 : 268;
      const left = Math.min(tip.x + 14, window.innerWidth - W - 8);
      const top = Math.min(tip.y + 14, window.innerHeight - 180);
      const box = { position: "fixed", left, top, width: W, zIndex: 60, background: "var(--bg-canvas)", border: "1px solid var(--border-medium)", borderRadius: 4, padding: "9px 11px", boxShadow: "0 6px 22px rgba(0,0,0,0.5)", pointerEvents: "none" };
      const row = (k, v, c) => (
        <div style={{ display: "flex", justifyContent: "space-between", gap: 18 }}>
          <span style={{ color: "var(--fg3)", fontSize: 11.5 }}>{k}</span>
          <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, color: c || "var(--fg1)" }}>{v}</span>
        </div>
      );
      if (tip.kind === "turn") {
        const t = tip.turn;
        return (
          <div style={box}>
            <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
              <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontWeight: 600, fontSize: 12, color: "var(--fg-max)" }}>Turn {t.index}</span>
              <span style={{ marginLeft: "auto", fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>{t.gens.length} steps · {formatTokens(t.billed)} billed</span>
            </div>
            <div style={{ fontSize: 11.5, color: "var(--fg2)", display: "-webkit-box", WebkitLineClamp: 4, WebkitBoxOrient: "vertical", overflow: "hidden", whiteSpace: "pre-wrap", wordBreak: "break-word" }}>{t.userText}</div>
          </div>
        );
      }
      const g = tip.gen, work = stepTokenWork(g), model = (g.model || "").replace(/-(20)?\d{6,8}.*$/, "").replace(/^claude-/, "");
      return (
        <div style={box}>
          <div style={{ display: "flex", alignItems: "center", gap: 6, marginBottom: 6 }}>
            <span style={{ width: 8, height: 8, borderRadius: 2, background: agentColor(g.agent_name) }}/>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontWeight: 600, fontSize: 12, color: "var(--fg-max)" }}>{agentShort(g.agent_name)}</span>
            <span style={{ marginLeft: "auto", fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>step #{tip.n}</span>
          </div>
          <div style={{ display: "flex", flexDirection: "column", gap: 3 }}>
            {model && row("model", model)}
            {row("started", formatTime(g.started_at))}
            {row("duration", formatDuration(g.duration_seconds))}
            {row("generated", `↑ ${formatTokens(work.generated)}`, "var(--viz-orange)")}
            {row("ingested", `↓ ${formatTokens(work.ingested)}`, "var(--viz-blue)")}
            {row("billed", formatTokens(g.total_tokens))}
            {g.tools && g.tools.length > 0 && row("tools", g.tools.slice(0, 4).join(", "))}
            {tip.turnIndex && row("turn", `T${tip.turnIndex}`)}
            {g.call_error && row("status", "error", "var(--error-text)")}
          </div>
        </div>
      );
    }

    // TimelineView is the trace waterfall: one lane for the main agent and one
    // per subagent run, each step a bar positioned along a *compressed* clock —
    // long idle gaps between calls collapse to a thin dashed break so the bars
    // fill the width instead of clumping. Concurrent subagents overlap in time,
    // the one thing the vertical thread can't show. A turn ribbon on top marks
    // each user-prompt boundary (number only; the prompt text is in the hover).
    // Bar height encodes real work (gen + fresh context, excluding cache_read,
    // log-scaled); width encodes duration. Clicking a bar jumps to that step.
    function TimelineView({ forest, onSelectGen, activeGenId, turns }) {
      const [tip, setTip] = useState(null);
      const allGens = [...forest.byId.values()].sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
      if (allGens.length === 0) return null;
      const stepNo = new Map(allGens.map((g, i) => [g.generation_id, i + 1]));
      const mainAgent = (forest.topRuns[0] && forest.topRuns[0].agent) || "main";
      const mainGens = forest.topRuns.flatMap(r => r.gens).sort((a, b) => tsMs(a.started_at) - tsMs(b.started_at));
      const subs = subagentRuns(forest);
      const lanes = [{ key: "main", label: agentShort(mainAgent), agent: mainAgent, depth: 0, gens: mainGens }]
        .concat(subs.map(r => ({ key: r.id, label: agentShort(r.agent), agent: r.agent, depth: r.depth, gens: r.gens })));

      let tmin = Infinity, tmax = -Infinity, maxTok = 1;
      const intervals = [];
      for (const g of allGens) {
        const s = tsMs(g.started_at), e = tsMs(g.completed_at) || s;
        if (s) tmin = Math.min(tmin, s);
        if (e) tmax = Math.max(tmax, e);
        if (e > s) intervals.push([s, e]);
        maxTok = Math.max(maxTok, stepTokenWork(g).work);
      }
      if (!Number.isFinite(tmin) || tmax <= tmin) { tmin = 0; tmax = 1; }

      // Compressed clock: union the active windows (merging gaps under 8s),
      // lay them end-to-end, and give each idle break a fixed sliver. mapPct
      // turns a timestamp into a percentage along this compressed axis.
      const sorted = intervals.slice().sort((a, b) => a[0] - b[0]);
      const merged = [];
      for (const [s, e] of sorted) {
        const last = merged[merged.length - 1];
        if (last && s <= last[1] + 8000) last[1] = Math.max(last[1], e);
        else merged.push([s, e]);
      }
      if (merged.length === 0) merged.push([tmin, tmax]);
      const GAP_PCT = 1.4;
      const activeMs = merged.reduce((a, m) => a + (m[1] - m[0]), 0) || 1;
      const gapsPct = (merged.length - 1) * GAP_PCT;
      const segs = []; let cum = 0;
      for (const m of merged) { segs.push({ s: m[0], e: m[1], start: cum }); cum += (m[1] - m[0]); }
      const scale = (100 - gapsPct) / activeMs;
      const mapPct = t => {
        for (let i = 0; i < segs.length; i++) {
          const m = segs[i];
          if (t <= m.e + 1) { const within = Math.max(0, Math.min(m.e - m.s, t - m.s)); return m.start * scale + i * GAP_PCT + within * scale; }
        }
        return 100;
      };

      const H_MIN = 5, H_MAX = 30;
      const barH = tok => H_MIN + (H_MAX - H_MIN) * (Math.log((tok || 0) + 1) / Math.log(maxTok + 1));

      const wall = tmax - tmin;
      const active = mergedSpan(intervals);
      const idlePct = wall > 0 ? Math.max(0, Math.round((1 - active / wall) * 100)) : 0;
      const peak = peakConcurrency(intervals);

      // Turn markers: each user-prompt boundary, positioned on the compressed
      // axis. A turn runs until the next prompt; we only need its start to draw
      // the boundary, and its index to label it.
      const turnMarks = (turns || [])
        .map(t => { const g = forest.byId.get(t.startGenId); return g ? { turn: t, ms: tsMs(g.started_at) } : null; })
        .filter(Boolean).sort((a, b) => a.ms - b.ms);
      turnMarks.forEach((m, i) => { m.left = mapPct(m.ms); m.endLeft = i + 1 < turnMarks.length ? mapPct(turnMarks[i + 1].ms) : 100; });
      const turnOf = ms => { let r = null; for (const m of turnMarks) { if (m.ms <= ms) r = m.turn; else break; } return r; };
      // dashed idle breaks sit at the seam between adjacent active windows
      const breaks = segs.slice(1).map((m, i) => m.start * scale + (i + 1) * GAP_PCT - GAP_PCT / 2);

      const LANE_W = 184, LANE_H = 46;
      const stat = (v, label, color) => (
        <span style={{ display: "inline-flex", alignItems: "baseline", gap: 5 }}>
          <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 13, color: color || "var(--fg-max)" }}>{v}</span>
          <span style={{ fontSize: 11, color: "var(--fg3)" }}>{label}</span>
        </span>
      );

      return (
        <div style={{ border: "1px solid var(--border-weak)", borderRadius: 2, background: "var(--bg-primary)", overflow: "hidden" }} onMouseLeave={() => setTip(null)}>
          {/* summary strip */}
          <div style={{ display: "flex", alignItems: "center", gap: 18, padding: "10px 14px", borderBottom: "1px solid var(--border-weak)", flexWrap: "wrap" }}>
            {stat(formatDuration(wall / 1000), "wall")}
            {stat(formatDuration(active / 1000), "active")}
            {stat(`${idlePct}%`, "idle", idlePct > 50 ? "var(--warning-text)" : undefined)}
            {stat(peak === 1 ? "sequential" : `${peak}×`, peak === 1 ? "" : "peak parallel", peak > 1 ? "var(--viz-purple)" : undefined)}
            {turnMarks.length > 0 && stat(String(turnMarks.length), turnMarks.length === 1 ? "turn" : "turns")}
            {stat(String(subs.length), subs.length === 1 ? "subagent" : "subagents")}
            <span style={{ flex: 1 }}/>
            <span style={{ fontSize: 11, color: "var(--fg3)" }}>idle collapsed · height = work · width = duration</span>
          </div>
          {/* turn ribbon — numbered markers; prompt text is in the hover */}
          {turnMarks.length > 0 && (
            <div style={{ display: "flex", borderBottom: "1px solid var(--border-weak)", background: "rgba(204,204,220,0.03)" }}>
              <div style={{ width: LANE_W, flex: "none", borderRight: "1px solid var(--border-weak)", padding: "0 12px", display: "flex", alignItems: "center", fontSize: 11, color: "var(--fg3)", height: 20 }}>Turns</div>
              <div style={{ flex: 1, position: "relative", height: 20 }}>
                {turnMarks.map((m, i) => (
                  <div key={i} title={`T${m.turn.index}: ${m.turn.userText}`}
                    onMouseMove={ev => setTip({ kind: "turn", turn: m.turn, x: ev.clientX, y: ev.clientY })}
                    onMouseLeave={() => setTip(null)}
                    onClick={() => onSelectGen(m.turn.startGenId)}
                    style={{ position: "absolute", left: `${m.left}%`, width: `${Math.max(0, m.endLeft - m.left)}%`, top: 0, bottom: 0, borderLeft: "1px solid var(--border-weak)", display: "flex", alignItems: "center", paddingLeft: 4, overflow: "hidden", cursor: "pointer", background: i % 2 ? "rgba(204,204,220,0.03)" : "transparent" }}>
                    <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, color: "var(--fg3)" }}>{m.turn.index}</span>
                  </div>
                ))}
              </div>
            </div>
          )}
          {/* lanes */}
          {lanes.map((lane, li) => {
            const c = agentColor(lane.agent);
            const laneTok = lane.gens.reduce((a, g) => a + (g.total_tokens || 0), 0);
            return (
              <div key={lane.key} style={{ display: "flex", borderBottom: li === lanes.length - 1 ? "none" : "1px solid var(--border-weak)", background: li % 2 ? "rgba(204,204,220,0.015)" : "transparent" }}>
                <div style={{ width: LANE_W, flex: "none", borderRight: "1px solid var(--border-weak)", padding: "0 12px", paddingLeft: 12 + lane.depth * 14, display: "flex", flexDirection: "column", justifyContent: "center", gap: 2, minWidth: 0, height: LANE_H }}>
                  <span style={{ display: "flex", alignItems: "center", gap: 6, minWidth: 0 }}>
                    {lane.depth > 0 && <span style={{ color: "var(--fg3)", fontSize: 11 }}>↳</span>}
                    <span style={{ width: 7, height: 7, borderRadius: 2, background: c, flex: "none" }}/>
                    <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, color: "var(--fg1)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{lane.label}</span>
                  </span>
                  <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, color: "var(--fg3)", paddingLeft: lane.depth > 0 ? 17 : 0 }}>{lane.gens.length} · {formatTokens(laneTok)} tok</span>
                </div>
                <div style={{ flex: 1, position: "relative", height: LANE_H }}>
                  {turnMarks.slice(1).map((m, i) => <span key={`tn${i}`} style={{ position: "absolute", left: `${m.left}%`, top: 0, bottom: 0, width: 1, background: "var(--border-weak)" }}/>)}
                  {breaks.map((x, i) => <span key={`br${i}`} style={{ position: "absolute", left: `${x}%`, top: 0, bottom: 0, width: 0, borderLeft: "1px dashed var(--viz-purple)", opacity: 0.35 }}/>)}
                  {lane.gens.map((g, gi) => {
                    const s = tsMs(g.started_at), e = tsMs(g.completed_at) || s;
                    const left = mapPct(s);
                    const w = Math.max(0.4, mapPct(e) - left);
                    const work = stepTokenWork(g);
                    const h = barH(work.work);
                    const isActive = g.generation_id === activeGenId;
                    return (
                      <div key={g.generation_id || gi}
                        onClick={() => onSelectGen(g.generation_id)}
                        onMouseMove={ev => { const t = turnOf(s); setTip({ kind: "step", gen: g, n: stepNo.get(g.generation_id), turnIndex: t ? t.index : null, x: ev.clientX, y: ev.clientY }); }}
                        onMouseLeave={() => setTip(null)}
                        style={{
                          position: "absolute", bottom: 8, height: h,
                          left: `${left}%`, width: `${w}%`, minWidth: 3,
                          background: g.call_error ? "var(--error-main)" : c,
                          opacity: isActive || (tip && tip.gen === g) ? 1 : 0.8, borderRadius: 2, cursor: "pointer",
                          outline: isActive ? "1.5px solid var(--fg-max)" : "none", outlineOffset: 1,
                          boxShadow: isActive ? "0 0 8px rgba(255,255,255,0.25)" : "none",
                        }}/>
                    );
                  })}
                </div>
              </div>
            );
          })}
          {tip && <TimelineTooltip tip={tip}/>}
        </div>
      );
    }

    function ConversationThread({ steps }) {
      const forest = useMemo(() => buildSubagentForest(steps), [steps]);
      const rows = useMemo(() => flattenForest(forest), [forest]);
      const hasSubagents = useMemo(() => rows.some(r => r.depth > 0), [rows]);

      const [view, setView] = useState("tree"); // tree | timeline
      // Collapse-first: every step starts collapsed to one line and every
      // subagent run starts folded, so the thread opens as a clean scannable
      // list. The reader expands only what they want to read.
      const [collapsed, setCollapsed] = useState(() => new Set([...forest.runs.values()].filter(r => r.depth > 0).map(r => r.id)));
      const [expanded, setExpanded] = useState(() => new Set());
      const [activeN, setActiveN] = useState(1);
      const [flashId, setFlashId] = useState(null);
      const [hashGenID, setHashGenID] = useState(generationIDFromHash);
      const cardRefs = useRef({});
      const flashTimer = useRef(null);
      useEffect(() => () => { if (flashTimer.current) clearTimeout(flashTimer.current); }, []);

      const rowByGen = useMemo(() => new Map(rows.map(r => [r.gen.generation_id, r])), [rows]);

      // Turns: the human-readable loop boundary. A turn opens at a top-level
      // (depth 0) generation whose input carries a fresh user prompt, and runs
      // — through every step it triggers, including spawned subagents — until
      // the next prompt. The prompt is lifted into a turn header so it stops
      // masquerading as a step; the generation it lived on renders as the
      // turn's first assistant step. Rollups: billed sums what the turn spent;
      // ctxIn is the live context size at the turn's last top-level call (it
      // climbs as the transcript is re-sent, so it's a "window fill", not a
      // sum). See the steps-prototype for the model this implements.
      const turns = useMemo(() => {
        const out = [];
        let cur = null;
        rows.forEach(r => {
          const ut = r.depth === 0 ? firstUserText(r.gen) : "";
          if (ut) { cur = { startGenId: r.gen.generation_id, userText: ut, gens: [], lastTop: r.gen }; out.push(cur); }
          if (cur) { cur.gens.push(r.gen); if (r.depth === 0) cur.lastTop = r.gen; }
        });
        out.forEach((t, i) => {
          t.index = i + 1;
          t.billed = t.gens.reduce((a, g) => a + (g.total_tokens || 0), 0);
          t.generated = t.gens.reduce((a, g) => a + (((g.token_buckets || {}).output || 0) + ((g.token_buckets || {}).reasoning || 0)), 0);
          const b = t.lastTop.token_buckets || {};
          t.ctxIn = (b.fresh_input || 0) + (b.cache_read || 0) + (b.cache_write || 0);
        });
        return out;
      }, [rows]);
      const turnByStartId = useMemo(() => new Map(turns.map(t => [t.startGenId, t])), [turns]);

      const toggleStep = id => {
        setExpanded(prev => { const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n; });
      };
      const toggleRun = id => setCollapsed(prev => { const n = new Set(prev); n.has(id) ? n.delete(id) : n.add(id); return n; });

      // Jump to a step: switch to tree, open its ancestor runs and the card,
      // mark it active, then scroll + flash once layout settles.
      const jumpTo = id => {
        const row = rowByGen.get(id);
        if (!row) return;
        setView("tree");
        setCollapsed(prev => { const n = new Set(prev); row.runPath.forEach(r => n.delete(r)); return n; });
        setExpanded(prev => prev.has(id) ? prev : new Set(prev).add(id));
        setActiveN(row.n);
        setFlashId(null);
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const card = cardRefs.current[id];
          if (card) {
            // Offset by the sticky chrome (header + breadcrumb sub-bar) so the
            // card lands just under it rather than behind it. Tracks HEADER_H
            // so a header resize keeps the same tuck.
            const top = window.scrollY + card.getBoundingClientRect().top - (HEADER_H + 24);
            window.scrollTo({ top: Math.max(0, top), behavior: "smooth" });
          }
          setFlashId(id);
        }));
        if (flashTimer.current) clearTimeout(flashTimer.current);
        flashTimer.current = setTimeout(() => setFlashId(null), 1400);
      };

      // Deep links of the form /conversations/:id#generation_id open and flash
      // the matching step once the detail payload is loaded.
      useEffect(() => {
        const syncHash = () => setHashGenID(generationIDFromHash());
        window.addEventListener("hashchange", syncHash);
        window.addEventListener("popstate", syncHash);
        return () => {
          window.removeEventListener("hashchange", syncHash);
          window.removeEventListener("popstate", syncHash);
        };
      }, []);
      useEffect(() => {
        if (hashGenID && rowByGen.has(hashGenID)) jumpTo(hashGenID);
      }, [hashGenID, rowByGen]);

      const totalSec = steps.reduce((acc, s) => acc + (s.duration_seconds || 0), 0);
      const totalTok = steps.reduce((acc, s) => acc + (s.total_tokens || 0), 0);
      const nSub = subagentRuns(forest).length;
      const allOpen = expanded.size >= rows.length;
      const toggleAll = () => setExpanded(allOpen ? new Set() : new Set(rows.map(r => r.gen.generation_id)));

      // Hotspots: the few steps actually worth looking at — the slowest, the
      // most token-hungry, and any errors. Encoding every step is illegible
      // on a long trace; flagging the outliers is what surfaces "what should
      // I look at here". maxBy returns the row, not the value.
      const hot = useMemo(() => {
        if (rows.length === 0) return null;
        const pick = val => rows.reduce((a, r) => (val(r) > val(a) ? r : a), rows[0]);
        const errors = rows.filter(r => r.gen.call_error);
        const slowest = pick(r => r.gen.duration_seconds || 0);
        const mostGenerated = pick(r => stepTokenWork(r.gen).generated);
        const mostRead = pick(r => stepTokenWork(r.gen).ingested);
        // hotById matches each minimap nub / tooltip badge to its Hotspots
        // chip. Assigned weakest-first so the strongest signal wins when a
        // step is several hotspots at once: error > slowest > generated > read.
        const hotById = new Map();
        hotById.set(mostRead.gen.generation_id, { color: "var(--viz-blue)", label: "most read" });
        hotById.set(mostGenerated.gen.generation_id, { color: "var(--viz-orange)", label: "most generated" });
        hotById.set(slowest.gen.generation_id, { color: "var(--warning-text)", label: "slowest" });
        errors.forEach(r => hotById.set(r.gen.generation_id, { color: "var(--error-text)", label: "error" }));
        return { errors, slowest, mostGenerated, mostRead, hotById };
      }, [rows]);

      const hidden = row => row.runPath.some(id => collapsed.has(id));
      const headerHidden = row => row.runPath.slice(0, -1).some(id => collapsed.has(id));

      const linkBtn = { background: "transparent", border: "none", color: "var(--fg3)", cursor: "pointer", fontSize: 11, fontFamily: "var(--fontFamily)", padding: 0 };

      const header = (
        <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
          <span style={{ fontSize: 13, color: "var(--fg1)", fontWeight: 500 }}>Thread</span>
          <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>
            {steps.length} {steps.length === 1 ? "call" : "calls"}{nSub > 0 ? ` · ${nSub} subagent${nSub === 1 ? "" : "s"}` : ""} · {formatTokens(totalTok)} tok · {formatDuration(totalSec)}
          </span>
          <span style={{ flex: 1 }}/>
          {view === "tree" && <button onClick={toggleAll} style={linkBtn} onMouseEnter={e => e.currentTarget.style.color = "var(--fg1)"} onMouseLeave={e => e.currentTarget.style.color = "var(--fg3)"}>{allOpen ? "Collapse all" : "Expand all"}</button>}
          {hasSubagents && (
            <Segmented size="sm" value={view} onChange={setView} options={[{ value: "tree", label: "Steps" }, { value: "timeline", label: "Timeline" }]}/>
          )}
        </div>
      );

      if (view === "timeline") {
        return (
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            {header}
            <TimelineView forest={forest} turns={turns} onSelectGen={jumpTo} activeGenId={(rows[activeN - 1] || {}).gen ? rows[activeN - 1].gen.generation_id : null}/>
            <div style={{ fontSize: 11, color: "var(--fg3)" }}>Click a bar to open that step.</div>
          </div>
        );
      }

      // Build the visible render list (interleaving subagent chips), so each
      // item knows whether it is the last — for clean separator handling.
      const items = [];
      rows.forEach(row => {
        const turn = turnByStartId.get(row.gen.generation_id);
        if (turn && !hidden(row)) items.push({ kind: "turn", turn });
        if (row.isRunStart && !headerHidden(row)) items.push({ kind: "head", row });
        if (!hidden(row)) items.push({ kind: "step", row, turnStart: !!turn });
      });

      return (
        <div style={{ display: "flex", justifyContent: "center" }}>
          <div style={{ flex: 1, minWidth: 0, maxWidth: 940, display: "flex", flexDirection: "column", gap: 14 }}>
            {header}
            {hot && rows.length > 2 && <Hotspots hot={hot} onJump={jumpTo}/>}
            <div style={{ border: "1px solid var(--border-weak)", borderRadius: 3, overflow: "hidden", background: "var(--bg-primary)" }}>
              {items.length === 0 ? (
                <div style={{ color: "var(--fg2)", fontSize: 12, fontFamily: "var(--fontFamilyMonospace)", padding: "14px 16px" }}>
                  No turns match this filter. Clear it to see all {rows.length} steps.
                </div>
              ) : items.map((item, idx) => {
                const last = idx === items.length - 1;
                if (item.kind === "turn") {
                  const t = item.turn;
                  return <TurnHeader key={`t${t.startGenId}`} turn={t} last={last}/>;
                }
                const { row } = item;
                const id = row.gen.generation_id;
                const c = agentColor(row.run.agent);
                if (item.kind === "head") {
                  const run = row.run;
                  const isCol = collapsed.has(run.id);
                  return (
                    <div key={`h${id}`} onClick={() => toggleRun(run.id)} style={{
                      display: "flex", alignItems: "center", gap: 8,
                      height: 32, paddingRight: 12, paddingLeft: 12 + (row.depth - 1) * 16,
                      cursor: "pointer", borderBottom: last ? "none" : "1px solid var(--border-weak)",
                      borderLeft: `2px solid ${c}`, background: "rgba(204,204,220,0.025)",
                    }}>
                      <Icon name={isCol ? "cright" : "chevron"} size={12} style={{ color: "var(--fg3)" }}/>
                      <svg width={11} height={11} viewBox="0 0 24 24" fill="none" stroke={c} strokeWidth="2"><circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/></svg>
                      <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, color: "var(--fg1)" }}>{agentShort(run.agent)}</span>
                      {run.hasError && <span style={{ display: "inline-flex", alignItems: "center", height: 15, padding: "0 5px", borderRadius: 2, background: "var(--error-transparent)", color: "var(--error-text)", fontSize: 10, fontFamily: "var(--fontFamilyMonospace)" }}>error</span>}
                      <span style={{ flex: 1 }}/>
                      <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>
                        {run.gens.length} {run.gens.length === 1 ? "step" : "steps"} · {formatDuration((run.end - run.start) / 1000)} · {formatTokens(run.totalTokens)}
                      </span>
                    </div>
                  );
                }
                return (
                  <div key={`s${id}`} style={{ position: "relative", paddingLeft: row.depth * 16 }}>
                    {row.depth > 0 && <span style={{ position: "absolute", left: row.depth * 16 - 1, top: 0, bottom: 0, width: 2, background: c, opacity: 0.35 }}/>}
                    <StepCard step={row.gen} n={row.n} total={rows.length} accent={c}
                      expanded={expanded.has(id)} onToggle={() => toggleStep(id)} active={row.n === activeN} last={last}
                      innerRef={el => { cardRefs.current[id] = el; }} flash={flashId === id} turnStart={item.turnStart}/>
                  </div>
                );
              })}
            </div>
          </div>
        </div>
      );
    }

    function DetailStats({ conv, steps }) {
      const wallSec = durationBetweenSeconds(conv.started_at, conv.last_activity);
      const errStatus = conv.status === "err";

      // Cache rate from the per-step buckets: the conversation summary is
      // synthesised from the detail on a deep link and omits the aggregate
      // buckets, so the steps are the reliable source. Mirror the list KPI:
      // cache reads over cache reads + cache writes + fresh input.
      const cache = (steps || []).reduce((a, s) => {
        const b = s.token_buckets || {};
        a.read += b.cache_read || 0;
        a.write += b.cache_write || 0;
        a.fresh += b.fresh_input || 0;
        return a;
      }, { read: 0, write: 0, fresh: 0 });
      const cachePct = cacheInputHitPercent(cache.fresh, cache.read, cache.write);

      const stats = [
        { value: formatDuration(wallSec),         unit: "elapsed" },
        { value: String(conv.calls),              unit: conv.calls === 1 ? "call" : "calls" },
        { value: formatTokens(conv.total_tokens), unit: "tokens" },
        ...(cachePct != null ? [{ value: `${cachePct}%`, unit: "input cached", color: "var(--viz-green)" }] : []),
      ];
      const onExport = () => {
        const blob = new Blob([JSON.stringify({ ...conv, generations: steps }, null, 2)], { type: "application/json" });
        const url = URL.createObjectURL(blob);
        const a = document.createElement("a");
        a.href = url; a.download = `${conv.id}.json`;
        document.body.appendChild(a); a.click(); a.remove();
        setTimeout(() => URL.revokeObjectURL(url), 1000);
      };

      return (
        <div style={{ display: "flex", gap: 10, alignItems: "center", padding: "11px 24px", borderBottom: "1px solid var(--border-weak)", background: "var(--bg-primary)", flexWrap: "wrap" }}>
          {stats.map((s, i) => (
            <div key={i} style={{
              display: "inline-flex", alignItems: "baseline", gap: 6,
              paddingRight: 14,
              borderRight: i === stats.length - 1 ? "none" : "1px solid var(--border-weak)",
              whiteSpace: "nowrap",
            }}>
              <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 14, color: s.color || "var(--fg-max)" }}>{s.value}</span>
              <span style={{ fontSize: 11, color: "var(--fg3)" }}>{s.unit}</span>
            </div>
          ))}
          {errStatus && (
            <span style={{
              display: "inline-flex", alignItems: "center", gap: 6,
              padding: "3px 10px",
              background: "var(--error-transparent)",
              color: "var(--error-text)",
              border: "1px solid var(--error-border)",
              fontSize: 12, fontFamily: "var(--fontFamilyMonospace)", borderRadius: 2,
            }}>
              <Icon name="dot" size={8}/> ERR
            </span>
          )}
          {(conv.models || []).map(m => <ModelPill key={m} name={m}/>)}
          <span style={{ flex: 1 }}/>
          <button title="Download trace as JSON" onClick={onExport} style={{
            display: "inline-flex", alignItems: "center", gap: 6,
            padding: "0 11px", height: 28,
            background: "transparent", color: "var(--fg1)",
            border: "1px solid var(--border-medium)",
            borderRadius: 2, fontSize: 12, cursor: "pointer", fontFamily: "var(--fontFamily)", fontWeight: 500,
            whiteSpace: "nowrap",
          }}
          onMouseEnter={e => e.currentTarget.style.background = "var(--action-hover)"}
          onMouseLeave={e => e.currentTarget.style.background = "transparent"}>
            <Icon name="download" size={12}/> Export JSON
          </button>
        </div>
      );
    }

    function TraceDetailView({ conv, detail, loading, error }) {
      return (
        <div style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0, background: "var(--bg-canvas)" }}>
          <DetailStats conv={conv} steps={detail ? detail.generations : []}/>
          <main style={{ padding: 24 }}>
            <div style={{ maxWidth: 1392, margin: "0 auto" }}>
              {error && <Notice kind="error" title="Failed to load session">{error}</Notice>}
              {!error && loading && <div style={{ color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>Loading…</div>}
              {!error && !loading && detail && <ConversationThread steps={detail.generations}/>}
            </div>
          </main>
        </div>
      );
    }

    // ============================================================
    // Settings — edits config.env via the daemon's /api/v1/config endpoints
    // ============================================================

    // Mono renders inline code in the monospace face used across the viewer.
    function Mono({ children }) {
      return <code style={{ fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg2)" }}>{children}</code>;
    }

    // sameSettings is a field-wise deep compare for dirty tracking. Tag order
    // is significant (it survives a round-trip), so it is compared positionally.
    function sameSettings(a, b) {
      if (!a || !b) return a === b;
      if (a.endpoint !== b.endpoint || a.tenantId !== b.tenantId || a.otlpEndpoint !== b.otlpEndpoint
        || a.token !== b.token || a.tokenCleared !== b.tokenCleared) return false;
      if (a.capture !== b.capture || a.guards !== b.guards || a.guardTimeout !== b.guardTimeout
        || a.debug !== b.debug || a.autoUpdate !== b.autoUpdate || a.userId !== b.userId
        || a.localForward !== b.localForward || a.semanticSearch !== b.semanticSearch
        || a.securityFindingsExport !== b.securityFindingsExport
        || a.securityAuditSchedule !== b.securityAuditSchedule
        || a.promptGuardUrl !== b.promptGuardUrl) return false;
      const at = a.tags || [], bt = b.tags || [];
      if (at.length !== bt.length) return false;
      for (let i = 0; i < at.length; i++) {
        if (at[i].key !== bt[i].key || at[i].value !== bt[i].value) return false;
      }
      return true;
    }

    // cloneSettings deep-copies so the form and the saved snapshot never share
    // the tags array (editing one must not mutate the other).
    function cloneSettings(s) {
      return { ...s, tags: (s.tags || []).map(t => ({ ...t })) };
    }

    // GUARD_CONTENT_NOTE is the one carve-out in the capture-mode promise: a
    // chained guard check relays the content being evaluated. See
    // handleHookEvaluate in internal/local/server.go.
    const GUARD_CONTENT_NOTE = "Guards are on: tool calls, and the conversation an agent runs a preflight check on, are sent to Cloud for evaluation regardless of the capture mode.";

    // forwardBannerMeta turns the daemon's reported forwarding status into the
    // pill, accent, and sentence the banner and the settings hero both show.
    // The saved toggle is deliberately not an input: config.env and the
    // daemon's own environment can disagree, and only the daemon knows what it
    // would actually send.
    function forwardBannerMeta(st) {
      if (!st) {
        return { accent: "warning", pill: "Unknown", line: "Couldn't read the daemon's forwarding status." };
      }
      if (!st.enabled) {
        if (st.reason) return { accent: "warning", pill: "Paused", line: `Forwarding is on but paused: ${st.reason}` };
        // The hook leg is one of the legs st.enabled sums, so nothing is
        // relayed here.
        return { accent: "success", pill: "Off", line: "Cloud forwarding is off — nothing from local sessions leaves this device." };
      }
      // Guard disclosures hold whatever else the status says, so every branch
      // below that reports forwarding as on appends them. Failures are kept per
      // leg: a failing generations or OTLP leg must not hide that guard checks
      // are still shipping content, nor swallow the unchecked-allow count.
      const disclosures = [];
      if (st.hooks) disclosures.push(GUARD_CONTENT_NOTE);
      if (st.hookFailOpens > 0) disclosures.push(st.hookFailOpens === 1
        ? "1 guard check ran without a Cloud verdict and was allowed."
        : `${st.hookFailOpens} guard checks ran without a Cloud verdict and were allowed.`);
      const failures = st.failures || [];
      const failure = failures[0];
      if (failure) {
        // Name the other failing legs instead of letting the most recent one
        // stand for all of them.
        const others = [...new Set(failures.map(f => f.label))].filter(l => l !== failure.label);
        const also = others.length > 0 ? ` (also failing: ${others.join(", ")})` : "";
        return {
          accent: "error",
          pill: "Failing",
          line: [`Forwarding is on but the last attempts failed — ${failure.label}: ${failure.detail}${also}`, ...disclosures].join(" "),
        };
      }
      // An unrecognised mode must not read as the narrower one: a future mode
      // could forward more, not less.
      if (st.mode !== "full" && st.mode !== "metadata_only") {
        return { accent: "warning", pill: "On", line: [`Forwarding is on in a mode this viewer does not know (${st.mode || "unset"}).`, ...disclosures].join(" ") };
      }
      // With guards chained, only reasoning text and media are still local:
      // the guard request carries tool calls, and for a preflight check the
      // prompts and responses too, so those cannot be listed as local here.
      const metadataLine = st.hooks
        ? "Session capture forwards usage and session metadata only — reasoning text and attached media stay local."
        : "Only usage and session metadata is forwarded — prompts, responses, reasoning text, tool inputs/results, and attached media stay local.";
      const parts = [st.mode === "full"
        ? "Full session content is forwarded to your organization's Grafana Cloud."
        : metadataLine];
      if (!st.generations && st.reason) parts.push(`Generations are paused: ${st.reason}`);
      if (!st.otlp) parts.push("Traces and metrics are not forwarded.");
      parts.push(...disclosures);
      return {
        // A metadata_only forward with guards chained still ships content, so
        // it does not get the calm accent or the reassuring pill.
        accent: st.mode === "full" || st.hooks ? "warning" : "info",
        pill: st.mode === "full" ? "Full content" : (st.hooks ? "Metadata + guard content" : "Metadata only"),
        line: parts.join(" "),
      };
    }

    // ForwardBanner shows what the daemon would send to Cloud, above the
    // session list. The daemon is shared, so this is machine-wide policy for
    // every later --local session, not a property of the sessions listed below.
    //
    // It is read-only. Changing it means a full config.env write, which the
    // Cloud settings tab owns; a second writer here would silently revert a
    // config.env change made after this component mounted.
    function ForwardBanner({ onOpenSettings }) {
      const [st, setSt] = useState(null);
      const [loaded, setLoaded] = useState(false);
      useEffect(() => {
        let alive = true;
        const load = () => {
          fetch("/api/v1/config")
            .then(r => r.ok ? r.json() : Promise.reject(new Error(`HTTP ${r.status}`)))
            .then(b => { if (alive) setSt(b && b.forwardStatus ? b.forwardStatus : null); })
            .catch(() => { if (alive) setSt(null); })
            .finally(() => { if (alive) setLoaded(true); });
        };
        load();
        // The daemon re-reads config.env whenever it changes, so the posture
        // moves under an open viewer (an edit, a second tab, agento11y login).
        // A privacy disclosure that froze at mount would be the wrong default.
        const id = setInterval(load, 30_000);
        return () => { alive = false; clearInterval(id); };
      }, []);
      if (!loaded) return null;
      const meta = forwardBannerMeta(st);
      const accent = meta.accent;
      return (
        <div style={{ display: "flex", alignItems: "center", gap: 12, padding: "10px 14px", marginBottom: 14, borderRadius: 2, border: "1px solid var(--border-weak)", borderLeft: `3px solid var(--${accent}-border)`, background: `var(--${accent}-transparent)` }}>
          <Icon name="cloud" size={15} style={{ color: `var(--${accent}-text)`, flex: "none" }}/>
          <span style={{ fontSize: 10.5, textTransform: "uppercase", letterSpacing: 0.6, color: "var(--fg3)", flex: "none" }}>What leaves this machine</span>
          <span style={{ fontSize: 12.5, fontWeight: 600, color: `var(--${accent}-text)`, flex: "none" }}>{meta.pill}</span>
          <span title={meta.line} style={{ fontSize: 12.5, color: "var(--fg2)", minWidth: 0, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{meta.line}</span>
          <span style={{ flex: 1 }}/>
          <button type="button" onClick={() => onOpenSettings && onOpenSettings("cloud")} style={{
            flex: "none", background: "transparent", border: "1px solid var(--border-medium)", borderRadius: 2,
            color: "var(--fg2)", fontSize: 11.5, fontFamily: "var(--fontFamily)", padding: "3px 9px", cursor: "pointer",
          }}>Change</button>
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

    // applyForwardLocalMode writes the segmented value back onto both keys.
    // capture is only rewritten when the mode already set forwards differently,
    // so an advanced mode set in config.env survives a round-trip through the
    // toggle: turning the control off leaves capture alone, and picking
    // Metadata only again does not flatten no_tool_content to metadata_only.
    // Re-picking the value already shown is a no-op too, which matters because
    // the segmented control fires for the active option.
    function applyForwardLocalMode(set, form, mode) {
      if (mode === forwardLocalMode(form)) return;
      if (mode === "off") {
        set({ localForward: false });
        return;
      }
      const patch = { localForward: true };
      if (captureForwardMode(form.capture) !== mode) patch.capture = mode;
      set(patch);
    }

    function SettingsSegmented({ value, onChange, options }) {
      return <PillToggle options={options} value={value} onChange={onChange}/>;
    }

    function Toggle({ checked, onChange }) {
      return (
        <button role="switch" aria-checked={checked} onClick={() => onChange(!checked)} style={{
          position: "relative", width: 38, height: 22, borderRadius: 9999, border: "none",
          cursor: "pointer", padding: 0, flexShrink: 0,
          background: checked ? "var(--primary-main)" : "rgba(204,204,220,0.25)", transition: "background .15s",
        }}>
          <span style={{
            position: "absolute", top: 3, left: 3, width: 16, height: 16, borderRadius: "50%",
            background: "#fff", transform: checked ? "translateX(16px)" : "translateX(0)", transition: "transform .15s",
          }}/>
        </button>
      );
    }

    function MonoInput({ value, onChange, placeholder, width, align, type }) {
      return (
        <input type={type || "text"} value={value} placeholder={placeholder}
          onChange={e => onChange(e.target.value)}
          onFocus={e => e.currentTarget.style.borderColor = "var(--primary-border)"}
          onBlur={e => e.currentTarget.style.borderColor = "var(--border-medium)"}
          style={{
            height: 32, width: width || "auto", background: "var(--bg-canvas)",
            border: "1px solid var(--border-medium)", borderRadius: 2, color: "var(--fg1)",
            padding: "0 10px", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12,
            textAlign: align || "left", outline: "none",
          }}/>
      );
    }

    function PrimaryButton({ onClick, children }) {
      return (
        <button onClick={onClick}
          onMouseEnter={e => { e.currentTarget.style.background = "var(--primary-shade)"; e.currentTarget.style.borderColor = "var(--primary-shade)"; }}
          onMouseLeave={e => { e.currentTarget.style.background = "var(--primary-main)"; e.currentTarget.style.borderColor = "var(--primary-main)"; }}
          style={{ height: 32, padding: "0 14px", background: "var(--primary-main)", border: "1px solid var(--primary-main)", color: "#fff", borderRadius: 2, fontSize: 13, fontWeight: 500, cursor: "pointer" }}>{children}</button>
      );
    }

    function GhostButton({ onClick, children }) {
      return (
        <button onClick={onClick}
          onMouseEnter={e => e.currentTarget.style.background = "var(--action-hover)"}
          onMouseLeave={e => e.currentTarget.style.background = "transparent"}
          style={{ height: 32, padding: "0 14px", background: "transparent", border: "1px solid var(--secondary-border)", color: "var(--fg1)", borderRadius: 2, fontSize: 13, cursor: "pointer" }}>{children}</button>
      );
    }

    function SettingsCard({ children, style }) {
      return (
        <SurfaceCard style={{
          padding: "4px 20px 12px",
          marginBottom: 16,
          ...(style || {}),
        }}>
          {children}
        </SurfaceCard>
      );
    }

    function SectionLabel({ children }) {
      return (
        <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "16px 0 2px" }}>
          <span style={{ width: 18, height: 2, borderRadius: 999, background: "var(--brandVertical)" }}/>
          <span style={{ fontSize: 11, fontWeight: 700, letterSpacing: ".08em", textTransform: "uppercase", color: "var(--fg3)" }}>{children}</span>
        </div>
      );
    }

    // SettingRow is one label/help + control line inside a card. `full` stacks
    // the control under the label for wide controls (the tags editor).
    function SettingRow({ label, help, children, full }) {
      const left = (
        <div style={{ minWidth: 0 }}>
          <div style={{ fontSize: 14, fontWeight: 500, color: "var(--fg1)" }}>{label}</div>
          {help && <div style={{ fontSize: 12, lineHeight: 1.5, color: "var(--fg3)", maxWidth: 460, marginTop: 4 }}>{help}</div>}
        </div>
      );
      if (full) {
        return (
          <div style={{ padding: "16px 0", borderTop: "1px solid var(--border-weak)" }}>
            {left}
            <div style={{ marginTop: 12 }}>{children}</div>
          </div>
        );
      }
      return (
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: 32, padding: "16px 0", borderTop: "1px solid var(--border-weak)" }}>
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
        <div style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, lineHeight: 1.9, whiteSpace: "pre-wrap", wordBreak: "break-all" }}>
          {lines.map((line, i) => {
            if (line.startsWith("#")) return <div key={i} style={{ color: "var(--fg3)" }}>{line}</div>;
            const eq = line.indexOf("=");
            if (eq < 0) return <div key={i} style={{ color: "var(--fg1)" }}>{line || "\u00a0"}</div>;
            return (
              <div key={i}>
                <span style={{ color: "var(--primary-text)" }}>{line.slice(0, eq)}</span>
                <span style={{ color: "var(--fg3)" }}>=</span>
                <span style={{ color: "var(--viz-green)" }}>{line.slice(eq + 1)}</span>
              </div>
            );
          })}
        </div>
      );
    }

    function UnsavedBar({ onReset, onSave }) {
      return (
        <div style={{ position: "fixed", left: 0, right: 0, bottom: 24, display: "flex", justifyContent: "center", pointerEvents: "none", zIndex: 20 }}>
          <div style={{ pointerEvents: "auto", display: "flex", alignItems: "center", gap: 12, background: "var(--bg-secondary)", border: "1px solid var(--border-medium)", borderRadius: 2, padding: "9px 12px 9px 16px", boxShadow: "var(--shadow-z2)", animation: "sigil-barin .16s ease-out" }}>
            <span style={{ width: 7, height: 7, borderRadius: "50%", background: "var(--brand-orange)" }}/>
            <span style={{ fontSize: 13, color: "var(--fg2)" }}>Unsaved changes</span>
            <GhostButton onClick={onReset}>Reset</GhostButton>
            <PrimaryButton onClick={onSave}>Save to config.env</PrimaryButton>
          </div>
        </div>
      );
    }


    function SettingsHero({ dirty, path, forwardStatus }) {
      const forwardMeta = forwardBannerMeta(forwardStatus);
      const chips = [
        { label: "Cloud copy", value: forwardMeta.pill, tone: `var(--${forwardMeta.accent}-text)` },
        { label: "Config", value: dirty ? "Unsaved" : "Synced", tone: dirty ? "var(--brand-orange-text)" : "var(--success-text)" },
      ];
      return (
        <PageHero
          icon="wrench"
          kicker="Local viewer settings"
          title="Settings"
          desc="Set up cloud connection and local runtime options."
          chips={chips}>
          <Box style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", maxWidth: 720 }}>{path}</Box>
        </PageHero>
      );
    }

    function SettingsTabRail({ tabs, active, onChange }) {
      return (
        <div aria-label="Settings sections" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(128px, 1fr))", gap: 8, marginBottom: 16 }}>
          {tabs.map(tab => {
            const isActive = tab.id === active;
            return (
              <button key={tab.id} type="button" aria-pressed={isActive} onClick={() => onChange(tab.id)}
                style={{
                  position: "relative",
                  minHeight: 76,
                  textAlign: "left",
                  padding: "12px 12px",
                  borderRadius: 8,
                  border: `1px solid ${isActive ? "var(--primary-border)" : "var(--border-weak)"}`,
                  background: isActive ? ACTIVE_PILL_BG : "rgba(24,27,31,0.68)",
                  color: "var(--fg1)",
                  cursor: "pointer",
                  boxShadow: isActive ? "0 12px 28px rgba(0,0,0,0.20)" : "none",
                }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 8 }}>
                  <span style={{ width: 26, height: 26, display: "inline-flex", alignItems: "center", justifyContent: "center", borderRadius: 7, background: isActive ? "var(--brandVertical)" : "rgba(204,204,220,0.06)", color: isActive ? "#fff" : "var(--fg2)" }}>
                    <Icon name={tab.icon} size={14}/>
                  </span>
                  <span style={{ fontSize: 13, fontWeight: 650, color: isActive ? "var(--fg-max)" : "var(--fg1)" }}>{tab.label}</span>
                </div>
                <div style={{ fontSize: 11.5, lineHeight: 1.35, color: "var(--fg3)" }}>{tab.desc}</div>
                {isActive && <span style={{ position: "absolute", left: 12, right: 12, bottom: -1, height: 2, borderRadius: 999, background: "var(--brandVertical)" }}/>}
              </button>
            );
          })}
        </div>
      );
    }

    function SettingsPreviewPanel({ path, preview, onCopy }) {
      return (
        <div style={{ width: "min(440px, 100%)", flex: "1 1 360px", position: "sticky", top: 72 }}>
          <div style={{
            overflow: "hidden",
            background: SURFACE_BG,
            border: "1px solid var(--border-weak)",
            borderRadius: 10,
            boxShadow: "0 18px 42px rgba(0,0,0,0.22)",
          }}>
            <div style={{ display: "flex", alignItems: "center", gap: 10, padding: "12px 14px", borderBottom: "1px solid var(--border-weak)" }}>
              <span style={{ width: 28, height: 28, display: "inline-flex", alignItems: "center", justifyContent: "center", borderRadius: 8, background: "rgba(204,204,220,0.06)", color: "var(--fg2)" }}>
                <Icon name="list" size={14}/>
              </span>
              <div style={{ minWidth: 0, flex: 1 }}>
                <div style={{ fontSize: 12, fontWeight: 600, color: "var(--fg-max)" }}>config.env preview</div>
                <div style={{ fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{path}</div>
              </div>
              <button onClick={onCopy} style={{ display: "inline-flex", alignItems: "center", gap: 5, background: "transparent", border: "1px solid var(--secondary-border)", color: "var(--fg1)", borderRadius: 6, height: 28, padding: "0 9px", fontSize: 12, cursor: "pointer" }}
                onMouseEnter={e => e.currentTarget.style.background = "var(--action-hover)"}
                onMouseLeave={e => e.currentTarget.style.background = "transparent"}>
                <Icon name="copy" size={13}/>Copy
              </button>
            </div>
            <div style={{ background: "rgba(17,18,23,0.84)", padding: "14px 16px", maxHeight: "calc(100vh - 252px)", overflow: "auto" }}>
              <PreviewBody text={preview}/>
            </div>
          </div>
        </div>
      );
    }

    function Toast({ message }) {
      return (
        <div style={{ position: "fixed", top: 60, right: 20, zIndex: 30, display: "flex", alignItems: "center", gap: 8, background: "var(--bg-secondary)", border: "1px solid var(--border-medium)", borderLeft: "3px solid var(--success-border)", borderRadius: 2, padding: "10px 14px", boxShadow: "var(--shadow-z2)", animation: "sigil-tin .2s ease-out" }}>
          <Icon name="check" size={16} style={{ color: "var(--success-text)" }}/>
          <span style={{ fontSize: 13, color: "var(--fg1)" }}>{message}</span>
        </div>
      );
    }

    const FORWARD_LOCAL_OPTIONS = [
      { value: "off", label: "Off" },
      { value: "metadata_only", label: "Metadata only" },
      { value: "full", label: "Full" },
    ];
    const SETTINGS_TABS = [
      { id: "cloud", label: "Cloud", icon: "cloud", desc: "Ingest, auth, forwarding" },
      { id: "local", label: "Local", icon: "box", desc: "Tags and runtime" },
    ];
    const SETTINGS_TAB_IDS = new Set(SETTINGS_TABS.map(t => t.id));

    function settingsTabFromLocation() {
      if (typeof window === "undefined") return "cloud";
      const params = new URLSearchParams(window.location.search || "");
      const tab = params.get("tab") || "";
      return SETTINGS_TAB_IDS.has(tab) ? tab : "cloud";
    }

    function settingsPath(tab) {
      const url = new URL("/settings", typeof window !== "undefined" ? window.location.origin : "http://localhost");
      if (SETTINGS_TAB_IDS.has(tab) && tab !== "cloud") url.searchParams.set("tab", tab);
      return url.pathname + url.search;
    }

    function SettingsCloudTab({ form, set, forwardStatus }) {
      const forwardMode = forwardLocalMode(form);
      const advanced = form.capture === "no_tool_content" || form.capture === "full_with_metadata_spans";
      // The daemon prefers config.env, but it also inherits LOCAL_FORWARD into
      // its own environment at boot, so "off here, on there" is reachable until
      // an explicit false is saved.
      const daemonStillOn = !!(forwardStatus && forwardStatus.enabled) && !form.localForward;
      // Say it next to the control that sets the capture mode, not only in the
      // banner.
      const guardsChained = !!(forwardStatus && forwardStatus.hooks);
      return (
        <SettingsCard>
          <SectionLabel>Connection</SectionLabel>
          <div style={{ fontSize: 12, lineHeight: 1.5, color: "var(--fg3)", padding: "0 0 10px" }}>
            These values apply to your Grafana Cloud sessions and to optional forwarding from local mode.
          </div>
          <SettingRow label="Endpoint" help={<>Grafana AI Observability ingest URL.</>}>
            <MonoInput value={form.endpoint} onChange={v => set({ endpoint: v })} placeholder="https://agento11y-prod-….grafana.net" width={320}/>
          </SettingRow>
          <SettingRow label="Tenant ID" help={<>Your stack instance ID.</>}>
            <MonoInput value={form.tenantId} onChange={v => set({ tenantId: v })} placeholder="123456" width={200}/>
          </SettingRow>
          <SettingRow label="Auth token" help={<>Stored locally with <Mono>0600</Mono> perms. Reset to replace or remove the saved token.</>}>
            {form.tokenSet && !form.tokenCleared && form.token === "" ? (
              <div style={{ display: "inline-flex", alignItems: "center", gap: 8 }}>
                <input value="" disabled placeholder="configured" style={{ height: 32, width: 200, background: "var(--bg-canvas)", border: "1px solid var(--border-medium)", borderRadius: 2, color: "var(--fg3)", padding: "0 10px", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, cursor: "not-allowed" }}/>
                <GhostButton onClick={() => set({ tokenCleared: true, token: "" })}>Reset</GhostButton>
              </div>
            ) : (
              <MonoInput type="password" value={form.token}
                onChange={v => set({ token: v, tokenCleared: form.tokenSet && v === "" })}
                placeholder={form.tokenSet ? "new token, or blank to remove" : "glc_…"} width={260}/>
            )}
          </SettingRow>
          <SettingRow label="OTLP endpoint" help={<>For SDK traces and metrics.</>}>
            <MonoInput value={form.otlpEndpoint} onChange={v => set({ otlpEndpoint: v })} placeholder="https://otlp-gateway-….grafana.net/otlp" width={320}/>
          </SettingRow>
          <SettingRow
            label="Forward local sessions to Cloud"
            help={<>
              Also send <Mono>--local</Mono> sessions to Grafana Cloud. Needs the credentials above, and applies to every local session on this machine until you turn it off. The local viewer always keeps full content; <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Metadata only</b> forwards usage and session metadata, and <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Full</b> forwards prompts, responses, and tool I/O too. Metadata only and Full write <Mono>CONTENT_CAPTURE_MODE</Mono>, which also decides how much content your non-local Cloud sessions capture.
              {advanced && <div style={{ color: "var(--warning-text)", marginTop: 6 }}>Advanced capture mode <Mono>{form.capture}</Mono> is set in config.env. Forwarding reduces it to metadata only and <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Metadata only</b> leaves it in place; picking <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Full</b> replaces it for Cloud sessions too.</div>}
              {daemonStillOn && <div style={{ color: "var(--warning-text)", marginTop: 6 }}>The running daemon is still forwarding — <Mono>LOCAL_FORWARD</Mono> is set in its environment. Saving writes an explicit <Mono>false</Mono> to config.env, which the daemon prefers.</div>}
              {guardsChained && <div style={{ color: "var(--warning-text)", marginTop: 6 }}>{GUARD_CONTENT_NOTE} The daemon relays <Mono>--local</Mono> guard checks to Cloud so your Cloud rules still apply; turn off <Mono>GUARDS_ENABLED</Mono> or forwarding to stop it.</div>}
            </>}
          >
            <SettingsSegmented value={forwardMode} onChange={v => applyForwardLocalMode(set, form, v)} options={FORWARD_LOCAL_OPTIONS}/>
          </SettingRow>
        </SettingsCard>
      );
    }

    function SettingsTagsEditor({ tags, setTag, addTag, removeTag }) {
      return (
        <SettingsCard>
          <SectionLabel>Session tags</SectionLabel>
          <SettingRow full label="Tags" help={<>Applied to every generation as <Mono>key=value</Mono>. Empty pairs are dropped on save.</>}>
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              {tags.map((t, i) => (
                <div key={i} style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <MonoInput value={t.key} onChange={v => setTag(i, { key: v })} placeholder="key" width={200}/>
                  <span style={{ color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>=</span>
                  <MonoInput value={t.value} onChange={v => setTag(i, { value: v })} placeholder="value" width={200}/>
                  <button onClick={() => removeTag(i)} title="Remove tag" aria-label="Remove tag" style={{
                    width: 28, height: 28, display: "inline-flex", alignItems: "center", justifyContent: "center",
                    background: "transparent", border: "1px solid transparent", color: "var(--fg3)", cursor: "pointer", borderRadius: 2,
                  }}
                    onMouseEnter={e => e.currentTarget.style.color = "var(--fg1)"}
                    onMouseLeave={e => e.currentTarget.style.color = "var(--fg3)"}>
                    <Icon name="times" size={14}/>
                  </button>
                </div>
              ))}
              <button onClick={addTag} style={{
                alignSelf: "flex-start", display: "inline-flex", alignItems: "center", gap: 6,
                height: 30, padding: "0 12px", background: "transparent", border: "1px dashed var(--border-medium)",
                borderRadius: 999, color: "var(--fg2)", fontSize: 13, cursor: "pointer",
              }}
                onMouseEnter={e => e.currentTarget.style.borderColor = "var(--border-strong)"}
                onMouseLeave={e => e.currentTarget.style.borderColor = "var(--border-medium)"}>
                <Icon name="plus" size={13}/>Add tag
              </button>
            </div>
          </SettingRow>
        </SettingsCard>
      );
    }

    function SettingsLocalTab({ form, set, setTag, addTag, removeTag }) {
      return (
        <>
          <SettingsTagsEditor tags={form.tags} setTag={setTag} addTag={addTag} removeTag={removeTag}/>
          <SettingsCard>
            <SectionLabel>Runtime</SectionLabel>
            <SettingRow label="Debug logging" help={<>Write a verbose log to <Mono>~/.local/state/agento11y/logs/agento11y.log</Mono>.</>}>
              <Toggle checked={form.debug} onChange={v => set({ debug: v })}/>
            </SettingRow>
            <SettingRow label="Automatic updates" help={<>Keep host agent plugins refreshed automatically. Turn off to pin the current versions.</>}>
              <Toggle checked={form.autoUpdate} onChange={v => set({ autoUpdate: v })}/>
            </SettingRow>
          </SettingsCard>
          <SettingsCard>
            <SectionLabel>Identity · Optional</SectionLabel>
            <SettingRow label="User ID" help={<>Override the resolved user id used to attribute generations. Leave blank to auto-resolve.</>}>
              <MonoInput value={form.userId} onChange={v => set({ userId: v })} placeholder="auto" width={260}/>
            </SettingRow>
          </SettingsCard>
        </>
      );
    }

    function SettingsTabPanels({ activeSettingsTab, form, set, setTag, addTag, removeTag, forwardStatus }) {
      return (
        <>
          {activeSettingsTab === "cloud" && <SettingsCloudTab form={form} set={set} forwardStatus={forwardStatus}/>}
          {activeSettingsTab === "local" && (
            <SettingsLocalTab form={form} set={set} setTag={setTag} addTag={addTag} removeTag={removeTag}/>
          )}
        </>
      );
    }

    function SettingsView() {
      const [form, setForm] = useState(null);
      const [saved, setSaved] = useState(null);
      const [preview, setPreview] = useState("");
      const [path, setPath] = useState("~/.config/agento11y/config.env");
      // Effective forwarding posture as the daemon resolves it, which can
      // differ from the saved toggle (unusable endpoint, placeholder creds).
      const [forwardStatus, setForwardStatus] = useState(null);
      const [loading, setLoading] = useState(true);
      const [error, setError] = useState(null);
      const [toast, setToast] = useState(null);
      const [activeSettingsTab, setActiveSettingsTab] = useState(settingsTabFromLocation);
      const [visitedSettingsTabs, setVisitedSettingsTabs] = useState(() => new Set([settingsTabFromLocation()]));
      const toastTimer = useRef(null);

      const showToast = useCallback((msg) => {
        setToast(msg);
        if (toastTimer.current) clearTimeout(toastTimer.current);
        toastTimer.current = setTimeout(() => setToast(null), 2600);
      }, []);
      useEffect(() => () => { if (toastTimer.current) clearTimeout(toastTimer.current); }, []);

      // Hydrate the form from config.env on mount.
      useEffect(() => {
        let alive = true;
        setLoading(true);
        setError(null);
        fetch("/api/v1/config")
          .then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`))))
          .then(body => {
            if (!alive) return;
            setForm(cloneSettings(body.settings));
            setSaved(cloneSettings(body.settings));
            setPreview(body.preview || "");
            if (body.path) setPath(body.path);
            if (body.forwardStatus) setForwardStatus(body.forwardStatus);
          })
          .catch(e => { if (alive) setError(String(e.message || e)); })
          .finally(() => { if (alive) setLoading(false); });
        return () => { alive = false; };
      }, []);

      useEffect(() => {
        const syncTab = () => {
          const tab = settingsTabFromLocation();
          setActiveSettingsTab(tab);
          setVisitedSettingsTabs(prev => prev.has(tab) ? prev : new Set(prev).add(tab));
        };
        window.addEventListener("popstate", syncTab);
        return () => window.removeEventListener("popstate", syncTab);
      }, []);

      // Live preview: the daemon renders exactly what it would write, so the
      // panel never drifts from the file. Debounced to coalesce keystrokes.
      // Each run aborts the prior in-flight request and ignores its result, so
      // a slow older response can never overwrite a newer one.
      useEffect(() => {
        if (!form) return;
        let ignore = false;
        const controller = new AbortController();
        const t = setTimeout(() => {
          fetch("/api/v1/config:preview", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ settings: form }), signal: controller.signal })
            .then(r => r.ok ? r.json() : null)
            .then(b => { if (!ignore && b && typeof b.preview === "string") setPreview(b.preview); })
            .catch(() => {});
        }, 180);
        return () => { ignore = true; controller.abort(); clearTimeout(t); };
      }, [form]);

      const page = { maxWidth: 1360, margin: "0 auto", padding: "28px 24px 110px", width: "100%" };
      if (loading && !form) {
        return <div style={page}><Notice kind="info" title="Loading settings…">Reading config.env.</Notice></div>;
      }
      if (!form) {
        return <div style={page}><Notice kind="error" title="Failed to load settings">{error}</Notice></div>;
      }

      const dirty = !sameSettings(form, saved);
      const set = (patch) => setForm(f => ({ ...f, ...patch }));
      const setTag = (i, patch) => setForm(f => ({ ...f, tags: f.tags.map((t, j) => j === i ? { ...t, ...patch } : t) }));
      const addTag = () => setForm(f => ({ ...f, tags: [...f.tags, { key: "", value: "" }] }));
      const removeTag = (i) => setForm(f => ({ ...f, tags: f.tags.filter((_, j) => j !== i) }));
      const reset = () => setForm(cloneSettings(saved));

      const save = () => {
        setError(null);
        fetch("/api/v1/config", { method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ settings: form }) })
          .then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`))))
          .then(body => {
            setForm(cloneSettings(body.settings));
            setSaved(cloneSettings(body.settings));
            if (typeof body.preview === "string") setPreview(body.preview);
            if (body.forwardStatus) setForwardStatus(body.forwardStatus);
            showToast("Settings saved to config.env.");
          })
          .catch(e => setError(String(e.message || e)));
      };
      const copy = () => {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(preview).then(() => showToast("Copied to clipboard.")).catch(() => {});
        }
      };
      const selectTab = (tab) => {
        if (!SETTINGS_TAB_IDS.has(tab)) return;
        setActiveSettingsTab(tab);
        setVisitedSettingsTabs(prev => prev.has(tab) ? prev : new Set(prev).add(tab));
        if (typeof window !== "undefined") window.history.pushState({}, "", settingsPath(tab));
      };

      return (
        <div style={page}>
          <SettingsHero dirty={dirty} path={path} forwardStatus={forwardStatus}/>

          {error && <div style={{ marginBottom: 16 }}><Notice kind="error" title="Couldn’t save settings">{error}</Notice></div>}

          <div style={{ display: "flex", gap: 24, alignItems: "flex-start", flexWrap: "wrap" }}>
            <div style={{ flex: "999 1 560px", minWidth: 0 }}>
              <SettingsTabRail tabs={SETTINGS_TABS} active={activeSettingsTab} onChange={selectTab}/>
              <SettingsTabPanels
                activeSettingsTab={activeSettingsTab}
                form={form}
                set={set}
                setTag={setTag}
                addTag={addTag}
                removeTag={removeTag}
                forwardStatus={forwardStatus}
              />
            </div>

            <SettingsPreviewPanel path={path} preview={preview} onCopy={copy}/>
          </div>

          {dirty && <UnsavedBar onReset={reset} onSave={save}/>}
          {toast && <Toast message={toast}/>}
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
      const raw = window.location.pathname.slice(prefix.length).replace(/\/$/, "");
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
      try { return decodeURIComponent(raw); } catch { return raw; }
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
      return e.button === 0
        && !e.metaKey && !e.ctrlKey && !e.shiftKey && !e.altKey;
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
        if (start != null && (startedAt == null || start < startedAt)) startedAt = start;
        const end = conversationTime({ last_activity: g.completed_at || g.started_at });
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
      const set = useCallback(v => {
        setValue(v);
        try {
          window.localStorage.setItem(key, v);
        } catch (_) {}
      }, [key]);
      return [value, set];
    }


    // ---------- generic UI primitives ----------

    function Switch({ checked, onChange, size = "md", title }) {
      const w = size === "sm" ? 28 : 34, h = size === "sm" ? 16 : 20, knob = h - 4;
      return (
        <button type="button" role="switch" aria-checked={checked} title={title}
          onClick={e => { e.stopPropagation(); onChange(!checked); }}
          style={{
            width: w, height: h, flexShrink: 0, borderRadius: 9999, border: "none", cursor: "pointer", padding: 0,
            background: checked ? "var(--primary-main)" : "rgba(204,204,220,0.20)",
            position: "relative", transition: "background 120ms ease",
          }}>
          <span style={{ position: "absolute", top: 2, left: checked ? w - knob - 2 : 2, width: knob, height: knob, borderRadius: "50%", background: "#fff", transition: "left 120ms ease" }}/>
        </button>
      );
    }

    function Segmented({ value, onChange, options, size = "md" }) {
      return (
        <div style={{ display: "inline-flex", border: "1px solid var(--border-medium)", borderRadius: 2, overflow: "hidden" }}>
          {options.map((o, i) => {
            const active = o.value === value;
            return (
              <button key={o.value} type="button" onClick={() => onChange(o.value)} style={{
                padding: size === "sm" ? "3px 10px" : "5px 12px",
                background: active ? "var(--action-selected)" : "transparent",
                color: active ? "var(--fg-max)" : "var(--fg2)",
                border: "none", borderLeft: i > 0 ? "1px solid var(--border-medium)" : "none",
                cursor: active ? "default" : "pointer", fontSize: 12, fontWeight: active ? 500 : 400,
                fontFamily: "var(--fontFamily)", whiteSpace: "nowrap",
              }}>{o.label}</button>
            );
          })}
        </div>
      );
    }

    const BADGE_TONES = {
      block:     { bg: "var(--error-transparent)",   fg: "var(--error-text)",   bd: "var(--error-border)" },
      redact:    { bg: "var(--warning-transparent)", fg: "var(--warning-text)", bd: "var(--warning-border)" },
      regex:     { bg: "var(--info-transparent)",    fg: "var(--primary-text)", bd: "var(--primary-text)" },
      cloud:     { bg: "rgba(204,204,220,0.06)",     fg: "var(--fg2)",          bd: "var(--border-medium)" },
      preflight: { bg: "transparent",                fg: "var(--warning-text)", bd: "var(--warning-border)" },
    };
    function Badge({ tone = "cloud", children }) {
      const t = BADGE_TONES[tone] || BADGE_TONES.cloud;
      return (
        <span style={{
          display: "inline-flex", alignItems: "center", height: 16, padding: "0 6px", borderRadius: 2,
          background: t.bg, color: t.fg, border: `1px solid ${t.bd}`,
          fontSize: 9.5, letterSpacing: "0.06em", textTransform: "uppercase",
          fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap",
        }}>{children}</span>
      );
    }
    function btnStyle(kind) {
      const base = { display: "inline-flex", alignItems: "center", justifyContent: "center", gap: 6, height: 32, padding: "0 12px", borderRadius: 8, fontSize: 12.5, fontWeight: 500, fontFamily: "var(--fontFamily)", whiteSpace: "nowrap" };
      if (kind === "primary") return { ...base, background: "var(--primary-main)", color: "#fff", border: "1px solid var(--primary-border)" };
      if (kind === "danger")  return { ...base, background: "transparent", color: "var(--error-text)", border: "1px solid var(--error-border)" };
      return { ...base, background: "rgba(17,18,23,0.30)", color: "var(--fg1)", border: "1px solid var(--border-medium)" };
    }
    function Button({ kind = "secondary", icon, children, disabled, onClick, title, style }) {
      return (
        <button type="button" title={title} disabled={disabled} onClick={onClick}
          style={{ ...btnStyle(kind), opacity: disabled ? 0.45 : 1, cursor: disabled ? "not-allowed" : "pointer", ...(style || {}) }}>
          {icon && <Icon name={icon} size={13}/>}{children}
        </button>
      );
    }

    const fieldInput = {
      width: "100%", height: 34, padding: "0 10px", border: "1px solid var(--border-medium)", borderRadius: 2,
      background: "var(--bg-canvas)", color: "var(--fg1)", fontSize: 13, fontFamily: "var(--fontFamily)", outline: "none",
    };
    const monoInput = { ...fieldInput, fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 };
    const sectionLabel = { display: "block", fontSize: 11, color: "var(--fg3)", textTransform: "uppercase", letterSpacing: "0.06em", marginBottom: 7 };

    function FieldLabel({ children, hint }) {
      return (
        <label style={sectionLabel}>
          {children}
          {hint && <span style={{ textTransform: "none", letterSpacing: 0, color: "var(--fg3)", marginLeft: 8, fontSize: 11 }}>{hint}</span>}
        </label>
      );
    }

    function Section({ title, children, defaultOpen = true }) {
      const [open, setOpen] = useState(defaultOpen);
      return (
        <div style={{ border: "1px solid var(--border-weak)", borderRadius: 2, marginBottom: 12 }}>
          <button type="button" onClick={() => setOpen(o => !o)} style={{ display: "flex", alignItems: "center", gap: 8, width: "100%", padding: "10px 12px", background: "transparent", border: "none", cursor: "pointer", color: "var(--fg1)" }}>
            <Icon name={open ? "chevron" : "cright"} size={12} style={{ color: "var(--fg3)" }}/>
            <span style={{ fontSize: 12, fontWeight: 500, textTransform: "uppercase", letterSpacing: "0.06em", color: "var(--fg2)" }}>{title}</span>
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
      const terms = String(query || "").toLowerCase().split(/\s+/).filter(Boolean);
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
        return re.test(part)
          ? <mark key={i} style={wash}>{part}</mark>
          : <React.Fragment key={i}>{part}</React.Fragment>;
      });
    }

    // SearchResultRow is one ranked hit. Stays consistent with ConvRow:
    // dense mono grid, left status accent, agent/model pills, two-line
    // clamp on the snippet. The row is a real anchor so cmd/ctrl-click
    // opens in a new tab without us re-implementing the browser.
    function SearchResultRow({ hit, now, query, selected, onSelect, onOpen }) {
      // Status is "err" when any generation in the conversation
      // recorded a CallError, otherwise "ok" — see
      // searchConversationFile and the qmd-fallback summary path.
      const accent = hit.status === "err" ? "var(--error-border)" : "var(--border-medium)";
      const ago = hit.last_activity ? formatAgo(hit.last_activity, now) : "";
      const titleEl = highlightTerms(hit.title || hit.id, query);
      const snippetEl = highlightTerms(hit.snippet || "", query);
      const matchCount = hit.match_count || 0;
      return (
        <a href={conversationPath(hit.id)}
          onMouseEnter={onSelect}
          onClick={e => {
            if (!isPlainLeftClick(e)) return;
            e.preventDefault();
            onOpen(hit);
          }}
          style={{
            display: "block",
            padding: "11px 16px 12px",
            borderBottom: "1px solid var(--border-weak)",
            borderLeft: `3px solid ${accent}`,
            background: selected ? "rgba(204,204,220,0.06)" : "transparent",
            cursor: "pointer", textDecoration: "none", color: "inherit",
            transition: "background 80ms ease",
          }}
          onMouseOver={e => { if (!selected) e.currentTarget.style.background = "var(--row-hover)"; }}
          onMouseOut={e => { if (!selected) e.currentTarget.style.background = "transparent"; }}>
          <div style={{
            display: "grid",
            gridTemplateColumns: "76px minmax(0,1fr) auto",
            columnGap: 16,
            alignItems: "baseline",
          }}>
            <span style={{ color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>{ago}</span>
            <div style={{ display: "flex", alignItems: "baseline", gap: 8, flexWrap: "wrap", minWidth: 0 }}>
              <span style={{ fontFamily: "var(--fontFamily)", fontSize: 14, fontWeight: 500, color: "var(--fg1)" }}>{titleEl}</span>
              {hit.title && hit.title !== hit.id && (
                <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>{hit.id}</span>
              )}
            </div>
            <div style={{ display: "inline-flex", alignItems: "center", gap: 10, color: "var(--fg2)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>
              {(hit.agents || []).map(a => <AgentPill key={a} name={a} size="sm"/>)}
              {(hit.models || []).map(m => <ModelPill key={m} name={m}/>)}
              <span>{formatTokens(hit.total_tokens)} · {hit.calls} {hit.calls === 1 ? "call" : "calls"}</span>
            </div>
          </div>
          <div style={{
            display: "grid",
            gridTemplateColumns: "76px minmax(0,1fr)",
            columnGap: 16,
            marginTop: 7,
          }}>
            <span/>
            <div style={{
              display: "-webkit-box",
              WebkitLineClamp: 2,
              WebkitBoxOrient: "vertical",
              overflow: "hidden",
              fontFamily: "var(--fontFamilyMonospace)",
              fontSize: 12,
              color: "var(--fg2)",
              lineHeight: 1.5,
            }}>
              <span style={{
                fontFamily: "var(--fontFamilyMonospace)",
                fontSize: 11,
                color: "var(--warning-main)",
                background: "rgba(245,183,61,0.10)",
                border: "1px solid rgba(245,183,61,0.30)",
                borderRadius: 2,
                padding: "0 5px",
                marginRight: 8,
              }}>{matchCount} {matchCount === 1 ? "match" : "matches"}</span>
              {hit.snippet ? <>…{snippetEl}</> : <span style={{ color: "var(--fg3)" }}>No preview available.</span>}
            </div>
          </div>
        </a>
      );
    }

    function useSearchResults(query) {
      const [phase, setPhase] = useState("done");      // "done" | "loading"
      const [hits, setHits] = useState([]);
      const [mode, setMode] = useState("fts");
      const [error, setError] = useState(null);
      const [selectedIndex, setSelectedIndex] = useState(-1);
      const [retryNonce, setRetryNonce] = useState(0);
      const debounceRef = useRef(null);
      const abortRef = useRef(null);
      const trimmed = query.trim();

      useEffect(() => { setSelectedIndex(-1); }, [query]);

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
          fetch(`/api/v1/search?q=${encodeURIComponent(trimmed)}`, { signal: controller.signal })
            .then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`))))
            .then(body => {
              setHits(Array.isArray(body.hits) ? body.hits : []);
              setMode(body.mode || "fts");
              setPhase("done");
            })
            .catch(e => {
              if (e && e.name === "AbortError") return;
              setError(String(e.message || e));
              setPhase("done");
            });
        }, SEARCH_DEBOUNCE_MS);
        debounceRef.current = timer;
        return () => clearTimeout(timer);
      }, [trimmed, retryNonce]);

      useEffect(() => () => {
        if (debounceRef.current) clearTimeout(debounceRef.current);
        if (abortRef.current) abortRef.current.abort();
      }, []);

      const retry = useCallback(() => setRetryNonce(n => n + 1), []);
      return { phase, hits, mode, error, selectedIndex, setSelectedIndex, retry };
    }

    function ConversationSearchPanel({ query, hits, phase, mode, error, selectedIndex, setSelectedIndex, retry, now, onOpen }) {
      const showResults = !!query && !error;
      const showNoResults = showResults && phase === "done" && hits.length === 0;
      const showLoadingSkeleton = showResults && phase === "loading" && hits.length === 0;

      return (
        <SurfaceCard style={{
          overflow: "hidden",
          opacity: phase === "loading" && hits.length > 0 ? 0.55 : 1,
          transition: "opacity 120ms ease",
        }}>
          {error && (
            <div style={{
              margin: 12, padding: "12px 14px",
              border: "1px solid var(--error-border)",
              background: "var(--error-transparent)",
              borderRadius: 2,
              display: "flex", alignItems: "flex-start", gap: 11,
            }}>
              <Icon name="alert" size={16} style={{ color: "var(--error-text)", marginTop: 2 }}/>
              <div style={{ flex: 1 }}>
                <div style={{ fontSize: 14, color: "var(--fg1)" }}>Couldn't run the search.</div>
                <div style={{ fontSize: 13, color: "var(--fg2)", marginTop: 3 }}>
                  The local viewer didn't respond. Check that <span style={{ fontFamily: "var(--fontFamilyMonospace)" }}>agento11y --local</span> is running, then try again.
                </div>
              </div>
              <button type="button" onClick={retry} style={{
                height: 28, padding: "0 12px", background: "transparent",
                border: "1px solid var(--border-medium)", borderRadius: 2,
                color: "var(--fg1)", fontSize: 12, cursor: "pointer",
              }}
                onMouseEnter={e => e.currentTarget.style.background = "var(--action-hover)"}
                onMouseLeave={e => e.currentTarget.style.background = "transparent"}>Retry</button>
            </div>
          )}

          {!error && showResults && hits.length > 0 && (
            <React.Fragment>
              <div style={{
                display: "flex", alignItems: "center",
                padding: "9px 16px", borderBottom: "1px solid var(--border-weak)",
                fontFamily: "var(--fontFamilyMonospace)", fontSize: 12, color: "var(--fg3)",
              }}>
                <span>{hits.length} {hits.length === 1 ? "result" : "results"}</span>
                <span style={{ flex: 1 }}/>
                <span style={{ fontSize: 11, opacity: 0.7 }}>
                  ranked by {mode === "semantic" ? "relevance (qmd)" : "matches"}
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
                  onOpen={h => onOpen({ id: h.id, title: h.title })}
                />
              ))}
            </React.Fragment>
          )}

          {!error && showLoadingSkeleton && (
            <React.Fragment>
              {[0, 1, 2].map(i => (
                <div key={i} style={{
                  padding: "14px 16px",
                  borderBottom: i < 2 ? "1px solid var(--border-weak)" : "none",
                  borderLeft: "3px solid var(--border-medium)",
                }}>
                  <div className="sigil-shim" style={{ height: 14, width: "40%", borderRadius: 2 }}/>
                  <div className="sigil-shim" style={{ height: 10, width: "80%", borderRadius: 2, marginTop: 8 }}/>
                </div>
              ))}
            </React.Fragment>
          )}

          {!error && showNoResults && (
            <div style={{ padding: "34px 16px 36px" }}>
              <div style={{ fontSize: 14, color: "var(--fg1)" }}>
                No matches for <span style={{ fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg-max)" }}>“{query}”</span>.
              </div>
              <div style={{ fontSize: 13, color: "var(--fg3)", marginTop: 6 }}>
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
      const [conversations, setConversations] = useState([]);
      const [tokenPoints, setTokenPoints] = useState([]);
      const [loadingList, setLoadingList] = useState(true);
      const [errList, setErrList] = useState(null);
      const [query, setQuery] = useState("");
      const conversationSearchRef = useRef(null);
      const [timeRange, setTimeRange] = usePersistedState("sigil.local.timeRange", "6h",
        v => TIME_RANGES.some(r => r.value === v));
      const [tokenModel, setTokenModel] = useState("all");
      const [chartMetric, setChartMetric] = usePersistedState("sigil.local.chartMetric", "tokens",
        v => v === "tokens" || v === "activity");
      const [bucketSel, setBucketSel] = useState(null);
      const [listSort, setListSort] = useState({ key: "last_activity", dir: "desc" });

      const [detail, setDetail] = useState(null);
      const [loadingDetail, setLoadingDetail] = useState(false);
      const [errDetail, setErrDetail] = useState(null);

      const view = showSettings ? "settings"
        : (selectedID ? "conversation" : "conversations");
      const selected = selectedID
        ? conversations.find(c => c.id === selectedID) || summaryFromDetail(detail, selectedID)
        : null;

      // Changing the time range invalidates a bucket drill-down: the
      // bucket boundaries belong to the old window.
      const changeTimeRange = useCallback(v => {
        setBucketSel(null);
        setTimeRange(v);
      }, [setTimeRange]);

      const pageTitle = view === "settings" ? "Settings — agento11y local"
        : view === "conversation" && selected ? `${selected.title || selected.id} — agento11y local`
        : "agento11y — local";
      useEffect(() => { document.title = pageTitle; }, [pageTitle]);

      // fetchList is driven from three sources (mount, SSE flush, 60s
      // backstop), so a slower older response could otherwise overwrite
      // a newer one. Each call captures a monotonically increasing
      // sequence number and only applies its result if it is still the
      // latest.
      const listSeqRef = useRef(0);
      const fetchList = useCallback(() => {
        const seq = ++listSeqRef.current;
        setLoadingList(true);
        setErrList(null);
        return fetch("/api/v1/conversations")
          .then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`))))
          .then(body => {
            if (listSeqRef.current !== seq) return;
            setConversations(body.conversations || []);
          })
          .catch(e => {
            if (listSeqRef.current !== seq) return;
            setErrList(String(e.message || e));
          })
          .finally(() => {
            if (listSeqRef.current !== seq) return;
            setLoadingList(false);
          });
      }, []);

      // Token points back the usage chart. Failures are swallowed: the
      // chart is supplementary, so a hiccup here shouldn't surface an
      // error banner over the conversation list.
      const fetchTokens = useCallback(() => {
        return fetch("/api/v1/metrics/tokens")
          .then(r => r.ok ? r.json() : null)
          .then(body => { if (body) setTokenPoints(body.points || []); })
          .catch(() => {});
      }, []);

      const refreshAll = useCallback(() => {
        fetchList();
        fetchTokens();
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
          .then(r => {
            if (r.status === 404) throw new Error("Session not found in the local store.");
            if (!r.ok) return r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`)));
            return r.json();
          })
          .then(body => {
            if (selectedIDRef.current !== id) return;
            setDetail(body);
            setErrDetail(null);
          })
          .catch(e => {
            if (selectedIDRef.current !== id) return;
            // Quiet refresh failures are swallowed; the next event
            // retries and the current view stays as-is instead of
            // flashing an error banner over good content. The 60s
            // backstop only refreshes the list, so a missed detail
            // event only recovers on another targeted event or when
            // the user reopens the conversation.
            if (!quiet) setErrDetail(String(e.message || e));
          })
          .finally(() => { if (!quiet) setLoadingDetail(false); });
      }, []);
      const fetchDetail = useCallback((id) => fetchDetailCore(id, false), [fetchDetailCore]);
      const quietRefreshDetail = useCallback((id) => fetchDetailCore(id, true), [fetchDetailCore]);

      useEffect(() => { refreshAll(); }, [refreshAll]);

      useEffect(() => {
        const onPopState = () => {
          setSelectedID(conversationIDFromPath());
          setShowSettings(settingsRouteActive());
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
      const refreshAllRef = useRef(refreshAll);
      const quietRefreshDetailRef = useRef(quietRefreshDetail);
      const selectedIDRef = useRef(selectedID);
      const viewRef = useRef(view);
      useEffect(() => { refreshAllRef.current = refreshAll; }, [refreshAll]);
      useEffect(() => { quietRefreshDetailRef.current = quietRefreshDetail; }, [quietRefreshDetail]);
      useEffect(() => { selectedIDRef.current = selectedID; }, [selectedID]);
      useEffect(() => { viewRef.current = view; }, [view]);

      useEffect(() => {
        // Browsers without EventSource (vanishingly rare on modern
        // desktop browsers, but possible in some embedded webviews)
        // fall back to the 60s backstop refresh below instead of
        // throwing on the constructor.
        if (typeof EventSource === "undefined") return;
        let timer = null;
        // Debounce so a burst export (one frame per generation) does
        // not trigger one refresh per frame. We only need one list
        // refresh per burst, plus one detail refresh if any event in
        // the burst targets the open conversation.
        let burstHasOpenConv = false;
        const flush = () => {
          timer = null;
          const refreshOpen = burstHasOpenConv;
          const openID = selectedIDRef.current;
          burstHasOpenConv = false;
          // Skip refetches when the user is on a non-conversation tab;
          // the conversation list isn't rendered there, and the SSE
          // connection itself is cheap to leave running.
          const v = viewRef.current;
          if (v !== "conversations" && v !== "conversation") return;
          refreshAllRef.current();
          if (refreshOpen && openID) {
            quietRefreshDetailRef.current(openID);
          }
        };
        const es = new EventSource("/api/v1/events");
        es.onmessage = (e) => {
          let ev = {};
          try { ev = JSON.parse(e.data || "{}"); } catch (_) { /* ignore */ }
          const openID = selectedIDRef.current;
          if (openID && ev && ev.conversation_id === openID) {
            burstHasOpenConv = true;
          }
          if (timer === null) timer = setTimeout(flush, 250);
        };
        // EventSource auto-reconnects on transport errors, so a daemon
        // restart or proxy blip heals without an explicit handler.
        // Cleanup closes the stream.
        return () => {
          if (timer !== null) clearTimeout(timer);
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
      const goSettings = (tab) => {
        window.history.pushState({}, "", settingsPath(tab));
        setSelectedID(null);
        setShowSettings(true);
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
          if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && String(e.key).toLowerCase() === "k") {
            e.preventDefault();
            focusConversationSearch();
          }
        };
        window.addEventListener("keydown", onKeyDown);
        return () => window.removeEventListener("keydown", onKeyDown);
      }, [focusConversationSearch]);

      const tabs = [
        { key: "conversations", label: "Sessions", href: "/", onClick: goConversations },
        { key: "settings", label: "Settings", href: "/settings", onClick: goSettings },
      ];
      const activeTab = view === "settings" ? "settings" : "conversations";
      const trail = view === "conversation" && selected ? [{ label: selected.title || selected.id, mono: true }] : [];

      return (
        <div style={{ minHeight: "100vh", display: "flex", flexDirection: "column" }}>
          <TopBar tabs={tabs} activeTab={activeTab} trail={trail}/>
          <div style={{ flex: 1, display: "flex", flexDirection: "column", minHeight: 0 }}>
            {view === "settings" && <SettingsView/>}
            {view === "conversations" && (
              <ConversationsView
                conversations={conversations}
                tokenPoints={tokenPoints}
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
                listSort={listSort}
                setListSort={setListSort}
                onOpen={openConv}
                onRefresh={refreshAll}
                refreshing={loadingList}
                onOpenSettings={goSettings}
              />
            )}
            {view === "conversation" && selected && (
              <TraceDetailView conv={selected} detail={detail} loading={loadingDetail} error={errDetail}/>
            )}
          </div>
        </div>
      );
    }
    ReactDOM.createRoot(document.getElementById("root")).render(<App/>);
