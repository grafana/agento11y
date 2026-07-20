    const { useState, useEffect, useMemo, useCallback, useRef } = React;

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

    function Icon({ name, size = 16, style }) {
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
        dot:      <circle cx="12" cy="12" r="4"/>,
        download: <path d="M12 4v12m0 0-4-4m4 4 4-4M4 20h16"/>,
        copy:     <path d="M9 9h11v11H9zM4 4h11v3"/>,
        list:     <path d="M4 6h16M4 12h16M4 18h16"/>,
        wrench:   <path d="M14.7 6.3a1 1 0 0 0 0 1.4l1.6 1.6a1 1 0 0 0 1.4 0l3.77-3.77a6 6 0 0 1-7.94 7.94l-6.91 6.91a2.12 2.12 0 0 1-3-3l6.91-6.91a6 6 0 0 1 7.94-7.94l-3.76 3.76z"/>,
        alert:    <><path d="M12 9v4"/><circle cx="12" cy="16.5" r="0.6" fill="currentColor"/><path d="M10.3 4.1 2.7 17.4a2 2 0 0 0 1.7 3h15.2a2 2 0 0 0 1.7-3L13.7 4.1a2 2 0 0 0-3.4 0Z"/></>,
        empty:    <><circle cx="12" cy="12" r="9"/><path d="M8 12h8"/></>,
        extlink:  <path d="M7 17 17 7M9 7h8v8"/>,
        info:     <><circle cx="12" cy="12" r="9"/><path d="M12 11v5"/><circle cx="12" cy="7.6" r="0.6" fill="currentColor"/></>,
        plus:     <path d="M12 5v14M5 12h14"/>,
        times:    <path d="M6 6l12 12M18 6 6 18"/>,
        check:    <path d="M5 12l4.5 4.5L19 7"/>,
      };
      return (
        <svg width={size} height={size} viewBox="0 0 24 24" fill="none"
          stroke="currentColor" strokeWidth={1.6} strokeLinecap="round" strokeLinejoin="round"
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
          <span style={{ fontFamily: "var(--fontFamily)", fontSize: 15, fontWeight: 600, letterSpacing: "-0.01em", color: "var(--fg-max)", whiteSpace: "nowrap" }}>Grafana AI Observability</span>
          <span style={{
            fontFamily: "var(--fontFamily)", fontSize: 10, fontWeight: 600,
            letterSpacing: "0.07em", textTransform: "uppercase", color: "var(--fg2)",
            border: "1px solid var(--border-medium)", borderRadius: 2, padding: "2px 6px", lineHeight: 1,
          }}>Local</span>
        </div>
      );
    }

    function AgentPill({ name, size = "md" }) {
      const small = size === "sm";
      return (
        <span style={{
          display: "inline-flex", alignItems: "center", gap: small ? 5 : 6,
          padding: small ? "1px 6px" : "2px 8px",
          border: "1px solid var(--border-medium)",
          borderRadius: 2,
          background: "rgba(204,204,220,0.04)",
          color: "var(--fg1)",
          fontSize: small ? 11 : 12,
          fontFamily: "var(--fontFamilyMonospace)",
          whiteSpace: "nowrap",
        }}>
          <svg width={10} height={10} viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="8" r="4"/><path d="M4 21a8 8 0 0 1 16 0"/></svg>
          {name}
        </span>
      );
    }

    function ModelPill({ name, dot }) {
      const color = dot || modelDot(name);
      return (
        <span style={{
          display: "inline-flex", alignItems: "center", gap: 6,
          padding: "2px 8px",
          border: "1px solid var(--border-medium)",
          borderRadius: 2,
          background: "rgba(204,204,220,0.02)",
          color: "var(--fg1)", fontSize: 12, fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap",
        }}>
          <span style={{ width: 7, height: 7, borderRadius: "50%", background: color, boxShadow: `0 0 6px ${color}66` }}/>
          {name}
        </span>
      );
    }

    const iconBtn = {
      width: 28, height: 28,
      display: "inline-flex", alignItems: "center", justifyContent: "center",
      background: "transparent", border: "1px solid transparent",
      color: "var(--fg2)", cursor: "pointer", borderRadius: 2,
    };

    // NavTab is a top-level header nav link. The current section gets a 2px
    // brand underline bar and white text; other tabs render as faint links
    // that brighten on hover and navigate on a plain left click (modifier
    // clicks fall through so the anchor opens a new tab).
    function NavTab({ label, href, onClick, state }) {
      const current = state === "current";
      return (
        <a href={href}
          onClick={e => {
            if (!isPlainLeftClick(e)) return;
            e.preventDefault();
            onClick && onClick(e);
          }}
          onMouseEnter={e => { if (!current) e.currentTarget.style.color = "var(--fg-max)"; }}
          onMouseLeave={e => { if (!current) e.currentTarget.style.color = "var(--fg2)"; }}
          style={{
            position: "relative", display: "inline-flex", alignItems: "center",
            height: "100%", padding: "0 2px",
            fontFamily: "var(--fontFamily)", fontSize: 13,
            color: current ? "var(--fg-max)" : "var(--fg2)",
            textDecoration: "none", whiteSpace: "nowrap", flexShrink: 0,
            transition: "color .12s",
          }}>
          {label}
          {current && <span style={{ position: "absolute", left: 0, right: 0, bottom: 0, height: 2, background: "var(--brandVertical)", borderRadius: 1 }}/>}
        </a>
      );
    }

    function TopBar({ tabs = [], trail = [] }) {
      return (
        <header style={{
          height: 48,
          borderBottom: "1px solid var(--border-weak)",
          background: "var(--bg-primary)",
          display: "flex", alignItems: "center", padding: "0 16px", gap: 16,
          position: "sticky", top: 0, zIndex: 5,
        }}>
          <Wordmark/>
          <div style={{ width: 1, height: 20, background: "var(--border-weak)", margin: "0 4px" }}/>
          <nav style={{ display: "flex", alignItems: "center", alignSelf: "stretch", gap: 14, minWidth: 0, flex: 1, overflow: "hidden" }}>
            {tabs.map((t, i) => <NavTab key={i} {...t}/>)}
            {trail.map((b, i) => (
              <React.Fragment key={"trail-" + i}>
                <Icon name="cright" size={11} style={{ color: "var(--fg3)", flexShrink: 0 }}/>
                <span style={{
                  fontFamily: b.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)",
                  fontSize: 13, color: "var(--fg-max)",
                  whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", minWidth: 0,
                }}>{b.label}</span>
              </React.Fragment>
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
      );
    }

    // ============================================================
    // Notices — loading, empty, error states
    // ============================================================

    function Notice({ kind = "info", title, children }) {
      const tone = {
        info:  { color: "var(--fg2)",        bg: "rgba(204,204,220,0.03)", border: "var(--border-weak)",   icon: "empty" },
        error: { color: "var(--error-text)", bg: "rgba(209,14,92,0.06)",   border: "var(--error-border)",  icon: "alert" },
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

    // ============================================================
    // Screen 1 — Conversations list
    // ============================================================

    // ChartSwitch picks which metric the single chart slot shows. It
    // doubles as the chart's title: the active segment names the data.
    function ChartSwitch({ value, onChange }) {
      const opts = [
        { value: "tokens", label: "Tokens" },
        { value: "activity", label: "Conversations" },
      ];
      return (
        <div style={{ display: "inline-flex", border: "1px solid var(--border-medium)", borderRadius: 2, overflow: "hidden" }}>
          {opts.map((o, i) => {
            const active = o.value === value;
            return (
              <button key={o.value} onClick={() => onChange(o.value)} style={{
                padding: "4px 12px",
                background: active ? "var(--action-selected)" : "transparent",
                color: active ? "var(--fg-max)" : "var(--fg2)",
                border: "none", borderLeft: i > 0 ? "1px solid var(--border-medium)" : "none",
                cursor: active ? "default" : "pointer",
                fontSize: 12, fontWeight: active ? 500 : 400, fontFamily: "var(--fontFamily)",
              }}>{o.label}</button>
            );
          })}
        </div>
      );
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
        <div style={{ position: "relative", padding: "16px 20px 12px", background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2 }}>
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
                  <span style={{ color: "var(--fg3)" }}>{data[hover].t}</span> · {data[hover].c} {data[hover].c === 1 ? "conversation" : "conversations"}
                </div>
              )}
            </div>
            <ChartXLabels data={data}/>
          </div>
        </div>
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
        <div style={{ position: "relative", padding: "16px 20px 12px", background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2 }}>
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
        </div>
      );
    }

    function FilterBar({ query, onQueryChange, timeRange, onTimeRangeChange, onRefresh, refreshing }) {
      return (
        <div style={{ display: "flex", alignItems: "stretch", gap: 8, marginBottom: 16, fontSize: 13 }}>
          <div style={{
            flex: 1, display: "flex", alignItems: "center", gap: 8,
            padding: "0 11px",
            height: 34,
            border: "1px solid var(--border-medium)",
            borderRadius: 2,
            background: "var(--bg-primary)",
            color: "var(--fg3)",
          }}>
            <Icon name="search" size={14}/>
            <input
              value={query}
              onChange={e => onQueryChange(e.target.value)}
              placeholder="Filter by id, agent, model…"
              style={{
                flex: 1, background: "transparent", border: "none", outline: "none",
                color: "var(--fg1)", fontSize: 13, fontFamily: "var(--fontFamily)",
              }}/>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)", padding: "1px 5px", border: "1px solid var(--border-weak)", borderRadius: 2 }}>⌘K</span>
          </div>
          <select
            value={timeRange}
            onChange={e => onTimeRangeChange(e.target.value)}
            title="Time range"
            style={{
              height: 34,
              minWidth: 150,
              padding: "0 30px 0 11px",
              border: "1px solid var(--border-medium)",
              borderRadius: 2,
              background: "var(--bg-primary)",
              color: "var(--fg1)",
              fontSize: 13,
              fontFamily: "var(--fontFamily)",
            }}>
            {TIME_RANGES.map(r => <option key={r.value} value={r.value}>{r.label}</option>)}
          </select>
          <button onClick={onRefresh} disabled={refreshing}
            style={{ ...iconBtn, height: 34, width: 34, border: "1px solid var(--border-medium)", opacity: refreshing ? 0.5 : 1, cursor: refreshing ? "wait" : "pointer" }}
            title="Refresh"
            onMouseEnter={e => { if (!refreshing) { e.currentTarget.style.background = "var(--action-hover)"; e.currentTarget.style.color = "var(--fg1)"; } }}
            onMouseLeave={e => { e.currentTarget.style.background = "transparent"; e.currentTarget.style.color = "var(--fg2)"; }}>
            <Icon name="refresh" size={14}/>
          </button>
        </div>
      );
    }

    function ConvRow({ c, now, onOpen }) {
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
          gridTemplateColumns: "110px minmax(280px, 1.6fr) 150px 110px 130px 200px",
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
            <span style={{ fontFamily: "var(--fontFamily)", color: "var(--fg1)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.title || c.id}</span>
            {c.title && c.title !== c.id && (
              <span style={{ color: "var(--fg3)", fontSize: 11, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.id}</span>
            )}
          </div>
          <span style={{ color: "var(--fg2)" }}>
            <span style={{ color: "var(--fg1)" }}>{formatDuration(wallSec)}</span>
            <span style={{ color: "var(--fg3)", padding: "0 6px" }}>·</span>
            <span style={{ color: "var(--fg1)" }}>{c.calls} {c.calls === 1 ? "call" : "calls"}</span>
          </span>
          <span style={{ display: "inline-flex", alignItems: "center", gap: 7 }} title={tokenBreakdownTitle(c.token_buckets)}>
            <span style={{ color: "var(--fg1)" }}>{formatTokens(c.total_tokens)}</span>
            {c.status === "err" && (
              <span style={{ display: "inline-flex", alignItems: "center", padding: "0 6px", height: 16, borderRadius: 2, background: "var(--error-transparent)", color: "var(--error-text)", fontSize: 10, letterSpacing: "0.04em" }}>ERR</span>
            )}
          </span>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            {(c.agents || []).map(a => <AgentPill key={a} name={a} size="sm"/>)}
          </div>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            {(c.models || []).map(m => <ModelPill key={m} name={m}/>)}
          </div>
        </a>
      );
    }

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
        <div style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "14px 16px", display: "flex", flexDirection: "column", gap: 7 }}>
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
        </div>
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
        <div style={{ display: "grid", gridTemplateColumns: "repeat(5, 1fr)", gap: 12, marginBottom: 16 }}>
          <KpiTile label="Conversations" value={kpi.conversations} sub={kpi.conversationsSub}/>
          <KpiTile label="Total tokens" value={formatTokens(kpi.tokens)} sub={`${kpi.models} ${kpi.models === 1 ? "model" : "models"}`}/>
          <KpiTile label="Cache hit rate" value={kpi.cachePct == null ? "\u2014" : `${kpi.cachePct}%`} bar={kpi.cachePct == null ? 0 : kpi.cachePct}/>
          <KpiTile label="Tool calls" value={kpi.calls} sub={`${avg} avg / conversation`}/>
          <KpiTile label="Errored conversations" value={kpi.errConvs}
            valueColor={kpi.errConvs > 0 ? "var(--error-text)" : "var(--fg-max)"}
            dot={kpi.errConvs > 0 ? "var(--error-text)" : undefined}
            sub={`${kpi.errPct}% of conversations`}/>
        </div>
      );
    }

    function ConversationsView({ conversations, tokenPoints, loading, error, query, setQuery, timeRange, setTimeRange, tokenModel, setTokenModel, chartMetric, setChartMetric, bucketSel, setBucketSel, listSort, setListSort, onOpen, onRefresh, refreshing }) {
      const now = Date.now();
      const range = timeRangeOption(timeRange);
      const rangeFiltered = useMemo(() => {
        if (range.ms == null) return conversations;
        const from = now - range.ms;
        return conversations.filter(c => {
          const t = conversationTime(c);
          return t != null && t >= from && t <= now;
        });
      }, [conversations, range.ms, now]);
      const filtered = useMemo(() => {
        if (!query) return rangeFiltered;
        const q = query.toLowerCase();
        return rangeFiltered.filter(c =>
          c.id.toLowerCase().includes(q)
          || (c.title || "").toLowerCase().includes(q)
          || (c.agents || []).some(a => a.toLowerCase().includes(q))
          || (c.models || []).some(m => m.toLowerCase().includes(q))
        );
      }, [rangeFiltered, query]);

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
      // Distinct models that actually produced tokens inside the visible
      // window. tokenModels spans the whole store (so the dropdown can
      // still offer every model), but the headline count must agree with
      // the windowed token total beside it — otherwise a 6h view reports
      // models that only appear in older conversations.
      const windowModelCount = useMemo(() => {
        const seen = new Set();
        for (const p of points) {
          if (!p.model) continue;
          const t = tokenPointTime(p);
          if (!Number.isFinite(t) || t < chartWindow.start || t > chartWindow.end) continue;
          seen.add(p.model);
        }
        return seen.size;
      }, [points, chartWindow]);

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
          const t = conversationTime(c);
          return t == null ? 0 : t;
        };
        return [...listFiltered].sort((a, b) => (val(a) - val(b)) * dir);
      }, [listFiltered, listSort]);

      // KPI tiles read the range + search set (not the bucket drill-down).
      // Conversation, tool-call and error counts come from that set; token
      // and cache numbers come from the same series the chart draws, so the
      // headline tokens always match the chart below, including its model
      // dropdown and legend toggles. Conversation rows carry no per-model
      // token breakdown, so that can only be honoured via the token series.
      const kpi = useMemo(() => {
        let calls = 0, errConvs = 0;
        for (const c of filtered) {
          calls += c.calls || 0;
          if (c.status === "err") errConvs++;
        }
        // Sum only the series the chart is currently showing, so hiding one
        // in the legend pulls it out of the headline total too.
        let tokens = 0;
        for (const s of TOKEN_SERIES) {
          if (!hiddenSeries.has(s.key)) tokens += tokenUsage.totals[s.key] || 0;
        }
        const fresh = tokenUsage.totals.fresh_input || 0;
        const cacheRead = tokenUsage.totals.cache_read || 0;
        const cacheDenom = fresh + cacheRead;
        // Hiding either side of the ratio makes the rate meaningless, so
        // blank it out the way the old chart header did. Otherwise only
        // report a flat 100% when there is literally no fresh input; cap at
        // 99% so 99.99% (a few fresh tokens against a huge cache) doesn't
        // round up and read as a perfect cache.
        const cacheHidden = hiddenSeries.has("fresh_input") || hiddenSeries.has("cache_read");
        const cachePct = cacheHidden || cacheDenom === 0 ? null
          : cacheRead === cacheDenom ? 100
          : Math.min(99, Math.round((cacheRead / cacheDenom) * 100));
        return {
          conversations: filtered.length,
          conversationsSub: query ? "matching filter" : "active in range",
          tokens,
          models: effectiveModel === "all" ? windowModelCount : 1,
          cachePct,
          calls,
          avgCalls: filtered.length ? calls / filtered.length : 0,
          errConvs,
          errPct: filtered.length ? Math.round((errConvs / filtered.length) * 100) : 0,
        };
      }, [filtered, tokenUsage, windowModelCount, effectiveModel, query, hiddenSeries]);

      return (
        <div style={{ padding: 24, maxWidth: 1600, margin: "0 auto" }}>
          <FilterBar query={query} onQueryChange={setQuery} timeRange={timeRange} onTimeRangeChange={setTimeRange} onRefresh={onRefresh} refreshing={refreshing}/>
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

          <div style={{
            marginTop: 18,
            border: "1px solid var(--border-weak)",
            borderRadius: 2,
            overflow: "hidden",
            background: "var(--bg-primary)",
          }}>
            <div style={{
              display: "grid",
              gridTemplateColumns: "110px minmax(280px, 1.6fr) 150px 110px 130px 200px",
              alignItems: "center", gap: 16,
              padding: "11px 16px 11px 19px",
              borderBottom: "1px solid var(--border-weak)",
              background: "var(--bg-secondary)",
              fontFamily: "var(--fontFamily)", fontSize: 12, color: "var(--fg3)", fontWeight: 500,
            }}>
              <SortHeader label="Last activity" sortKey="last_activity" sort={listSort} onSort={handleSort}/>
              <span>Conversation</span>
              <SortHeader label="Duration" sortKey="duration" sort={listSort} onSort={handleSort}/>
              <SortHeader label="Tokens" sortKey="tokens" sort={listSort} onSort={handleSort}/>
              <span>Agents</span><span>Models</span>
            </div>

            {error && (
              <div style={{ padding: 16 }}>
                <Notice kind="error" title="Failed to load conversations">{error}</Notice>
              </div>
            )}
            {!error && loading && conversations.length === 0 && (
              <div style={{ padding: "32px 18px", color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>Loading…</div>
            )}
            {!error && !loading && conversations.length === 0 && (
              <div style={{ padding: 16 }}>
                <Notice kind="info" title="No conversations yet">
                  Run an agent against this daemon with <code style={{ color: "var(--fg1)" }}>sigil pi --local</code> or <code style={{ color: "var(--fg1)" }}>sigil claude --local</code>. Captured generations appear here as soon as the agent emits its first one.
                </Notice>
              </div>
            )}
            {!error && conversations.length > 0 && rangeFiltered.length === 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No conversations in <code style={{ color: "var(--fg1)" }}>{range.label}</code>.
              </div>
            )}
            {!error && filtered.length === 0 && rangeFiltered.length > 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No matches for <code style={{ color: "var(--fg1)" }}>{query}</code>.
              </div>
            )}
            {!error && bucketSel && listFiltered.length === 0 && filtered.length > 0 && (
              <div style={{ padding: "16px 18px", color: "var(--fg2)", fontSize: 12 }}>
                No conversations in the selected bucket.
              </div>
            )}
            {sorted.map(c => <ConvRow key={c.id} c={c} now={now} onOpen={onOpen}/>)}
          </div>

          <div style={{
            marginTop: 14, padding: "10px 14px",
            fontSize: 11, color: "var(--fg3)",
            fontFamily: "var(--fontFamilyMonospace)",
          }}>
            {sorted.length} of {filtered.length} {filtered.length === 1 ? "conversation" : "conversations"}
          </div>
        </div>
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
    function MessageBubble({ msg }) {
      const isUser = msg.role === "user";
      const isTool = msg.role === "tool";
      const parts = msg.parts || [];
      const isToolCall = !isUser && !isTool && parts.some(p => (p.kind === "tool_call") || p.tool_call);
      // Role accents come straight from Grafana's brand secondary
      // palette: green for the user (input side), purple for tool
      // results, orange for the assistant so the primary brand colour
      // attaches to the agent's own output.
      const labelColor = isUser ? "var(--viz-green)" : (isTool ? "var(--viz-purple)" : (isToolCall ? "var(--warning-text)" : "var(--brand-orange)"));
      const label = isTool ? "TOOL RESULT" : (isToolCall ? "TOOL CALL" : ((msg.role || "").toUpperCase() || "MESSAGE"));
      return (
        <div style={{
          borderLeft: `2px solid ${labelColor}`,
          padding: "6px 12px",
          background: "var(--bg-canvas)",
          borderRadius: 2,
          marginBottom: 6,
        }}>
          <div style={{
            fontFamily: "var(--fontFamilyMonospace)", fontSize: 10,
            color: labelColor, letterSpacing: "0.08em", marginBottom: 4,
          }}>{label}</div>
          {parts.length === 0 && (
            <div style={{ color: "var(--fg3)", fontSize: 12, fontStyle: "italic" }}>(no parts captured)</div>
          )}
          {parts.map((p, i) => <MessagePart key={i} part={p}/>)}
        </div>
      );
    }

    // ThinkingPart collapses a thinking block to a single toggle line so
    // an empty or long chain-of-thought doesn't take over the turn. The
    // SDK doesn't record a per-part token count, so the line is just the
    // label; expanding reveals the captured text.
    function ThinkingPart({ text }) {
      const [open, setOpen] = useState(false);
      return (
        <div style={{ marginTop: 4 }}>
          <div onClick={() => setOpen(o => !o)} style={{ display: "inline-flex", alignItems: "center", gap: 6, cursor: "pointer", color: "var(--fg3)", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)" }}>
            <Icon name={open ? "chevron" : "cright"} size={10} style={{ color: "var(--fg3)" }}/>
            Thinking
          </div>
          {open && <div style={{ fontSize: 12, color: "var(--fg2)", whiteSpace: "pre-wrap", marginTop: 4, fontStyle: "italic" }}>{text}</div>}
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
        const tc = part.tool_call;
        const input = tc.input_json || null;
        const command = tc.name === "Bash" && input && typeof input === "object" && input.command ? input.command : "";
        const description = tc.name === "Bash" && input && typeof input === "object" && input.description ? input.description : "";
        const args = input ? (typeof input === "string" ? input : JSON.stringify(input, null, 2)) : "";
        return (
          <div style={{ marginTop: 4 }}>
            <div style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--warning-text)" }}>
              → {tc.name}{tc.id ? <span style={{ color: "var(--fg3)" }}> · {tc.id}</span> : null}
            </div>
            {description && <div style={{ marginTop: 4, color: "var(--fg2)", fontSize: 12 }}>{description}</div>}
            {command ? (
              <CappedBlock lineCount={command.split("\n").length}><span style={{ color: "var(--warning-text)" }}>$</span> {command}</CappedBlock>
            ) : args && (
              <CappedBlock lineCount={args.split("\n").length} preStyle={{ fontSize: 11 }}>{args}</CappedBlock>
            )}
          </div>
        );
      }
      if (kind === "tool_result" && part.tool_result) {
        const tr = part.tool_result;
        const body = tr.content || (tr.content_json ? (typeof tr.content_json === "string" ? tr.content_json : JSON.stringify(tr.content_json)) : "");
        const isErr = !!tr.is_error;
        const lineCount = body ? body.split("\n").length : 0;
        return (
          <div style={{ marginTop: 4 }}>
            <div style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: isErr ? "var(--error-text)" : "var(--fg2)" }}>
              ← result{tr.tool_call_id ? <span style={{ color: "var(--fg3)" }}> · {tr.tool_call_id}</span> : null}{lineCount > 0 ? <span style={{ color: "var(--fg3)" }}> · {lineCount} {lineCount === 1 ? "line" : "lines"}</span> : null}{isErr ? <span style={{ color: "var(--error-text)" }}> · error</span> : null}
            </div>
            {body && <CappedBlock lineCount={lineCount} preStyle={{ fontSize: 11 }}>{body}</CappedBlock>}
          </div>
        );
      }
      return null;
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

    function StepCard({ step, n, expanded, onToggle, innerRef, flash }) {
      const hasError = !!step.call_error;
      const dotColor = hasError ? "var(--error-text)" : "#73BF69";
      return (
        <div ref={innerRef} className={flash ? "sigil-step-flash" : undefined} style={{
          border: "1px solid var(--border-weak)",
          borderLeft: hasError ? "2px solid var(--error-main)" : "1px solid var(--border-weak)",
          borderRadius: 2,
          background: "var(--bg-primary)",
          marginBottom: 12,
        }}>
          <div onClick={onToggle} style={{
            display: "flex", alignItems: "center", gap: 12,
            padding: "10px 14px",
            cursor: "pointer",
            borderBottom: expanded ? "1px solid var(--border-weak)" : "none",
          }}>
            <span style={{
              display: "inline-flex", alignItems: "center", gap: 6,
              padding: "2px 8px",
              background: "rgba(204,204,220,0.06)",
              borderRadius: 2,
              fontFamily: "var(--fontFamilyMonospace)",
              fontSize: 10, color: "var(--fg2)", textTransform: "uppercase", letterSpacing: "0.08em",
            }}>Step {n}</span>
            <span style={{ display: "inline-flex", alignItems: "center", gap: 6, color: "var(--fg1)", fontSize: 12, minWidth: 0 }}>
              <span style={{ width: 7, height: 7, borderRadius: "50%", background: dotColor, flexShrink: 0 }}/>
              {step.agent_name && <AgentPill name={step.agent_name} size="sm"/>}
              {step.model && (
                <span style={{ fontFamily: "var(--fontFamilyMonospace)", color: "var(--fg2)", whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>{step.model}</span>
              )}
            </span>
            {hasError && (
              <span style={{ display: "inline-flex", alignItems: "center", height: 16, padding: "0 6px", borderRadius: 2, background: "var(--error-transparent)", color: "var(--error-text)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, letterSpacing: "0.04em" }}>error</span>
            )}
            <span style={{ flex: 1 }}/>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg2)", display: "flex", gap: 12, whiteSpace: "nowrap", flexShrink: 0 }}>
              <span>{formatDuration(step.duration_seconds)}</span>
              <span>{formatTokens(step.total_tokens)} tok</span>
            </span>
            <Icon name={expanded ? "chevron" : "cright"} size={13} style={{ color: "var(--fg3)" }}/>
          </div>
          {expanded && (
            <div style={{ padding: "12px 14px 14px" }}>
              <div style={{ display: "flex", alignItems: "center", gap: 12, marginBottom: 10, color: "var(--fg2)", fontSize: 12, flexWrap: "wrap" }}>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 6, fontFamily: "var(--fontFamilyMonospace)" }}>
                  <span style={{ width: 18, height: 18, borderRadius: 2, background: "rgba(115,191,105,0.18)", color: "#73BF69", display: "inline-flex", alignItems: "center", justifyContent: "center", fontSize: 10 }}>{agentBadge(step.agent_name)}</span>
                  <span style={{ color: "var(--fg2)" }}>{formatTime(step.completed_at || step.started_at)}</span>
                </span>
                <span style={{ display: "inline-flex", alignItems: "center", gap: 6, color: "var(--fg2)", fontFamily: "var(--fontFamilyMonospace)" }}>
                  <Icon name="wrench" size={11}/>
                  tools · {(step.tools || []).length}
                </span>
                {step.provider && (
                  <span style={{ color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>provider · {step.provider}</span>
                )}
              </div>

              <StepTokenBar buckets={step.token_buckets}/>

              {hasError && (
                <div style={{ marginBottom: 10 }}>
                  <Notice kind="error" title="Call error">{step.call_error}</Notice>
                </div>
              )}

              <MessageThread step={step}/>

              {step.tool_preview && !(step.output || []).some(m => (m.parts || []).some(p => p.kind === "tool_call" || p.tool_call)) && (
                <div style={{
                  background: "var(--bg-canvas)",
                  border: "1px solid var(--border-weak)",
                  borderRadius: 2,
                  padding: "8px 12px",
                  marginTop: 10,
                  fontFamily: "var(--fontFamilyMonospace)", fontSize: 12,
                  color: "var(--fg2)",
                  display: "flex", alignItems: "flex-start", gap: 8,
                }}>
                  <span style={{ color: "var(--warning-text)" }}>$</span>
                  <code style={{ color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all" }}>{step.tool_preview}</code>
                </div>
              )}
            </div>
          )}
        </div>
      );
    }

    // stepRailSummary picks a one-line label for a rail row: the first
    // tool call's name + preview when the step ran a tool, otherwise
    // "Initial prompt" for the opening step and "Final response" for a
    // trailing text-only step. mono renders the tool form in Roboto Mono;
    // prose labels stay in Inter. There is no "initial/final" flag in the
    // data, so the position heuristic is the best we can do.
    function stepRailSummary(step, i, total) {
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
    function StepRail({ steps, activeStep, peakIdx, onSelect }) {
      if (!steps || steps.length === 0) return null;
      return (
        <aside style={{ flex: "none", width: 248, position: "sticky", top: 72 }}>
          <div style={{ fontSize: 11, color: "var(--fg3)", fontWeight: 500, padding: "0 4px 10px", display: "flex", alignItems: "center", justifyContent: "space-between" }}>
            <span>Steps</span>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)" }}>{steps.length}</span>
          </div>
          <div style={{ border: "1px solid var(--border-weak)", borderRadius: 2, overflow: "hidden", background: "var(--bg-primary)" }}>
            {steps.map((s, i) => {
              const n = i + 1;
              const active = n === activeStep;
              const hasError = !!s.call_error;
              const summary = stepRailSummary(s, i, steps.length);
              const isPeak = i === peakIdx;
              const subColor = hasError ? "var(--error-text)" : (isPeak ? "var(--warning-text)" : "var(--fg3)");
              return (
                <div key={s.generation_id || i} onClick={() => onSelect(n)} style={{
                  display: "grid", gridTemplateColumns: "24px 1fr auto", alignItems: "center", gap: 8,
                  padding: "9px 11px",
                  borderBottom: i === steps.length - 1 ? "none" : "1px solid var(--border-weak)",
                  borderLeft: active ? "2px solid var(--brand-orange)" : "2px solid transparent",
                  background: active ? "var(--action-selected)" : "transparent",
                  cursor: "pointer",
                }}
                onMouseEnter={e => { if (!active) e.currentTarget.style.background = "rgba(204,204,220,0.03)"; }}
                onMouseLeave={e => { if (!active) e.currentTarget.style.background = "transparent"; }}>
                  <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg3)" }}>{n}</span>
                  <span style={{ display: "flex", flexDirection: "column", gap: 1, minWidth: 0 }}>
                    <span style={{ fontFamily: summary.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)", fontSize: 12, color: active ? "var(--fg-max)" : "var(--fg1)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{summary.label}</span>
                    <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10, color: subColor, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                      {formatDuration(s.duration_seconds)} · {formatTokens(s.total_tokens)}{isPeak ? " · peak" : ""}
                    </span>
                  </span>
                  <span style={{ width: 6, height: 6, borderRadius: "50%", background: hasError ? "var(--error-text)" : "var(--viz-green)" }}/>
                </div>
              );
            })}
          </div>
        </aside>
      );
    }

    function ConversationThread({ steps }) {
      // First 4 cards default to expanded — the typical attention zone.
      const [expanded, setExpanded] = useState(() => new Set(steps.slice(0, 4).map((_, i) => i + 1)));
      const [activeStep, setActiveStep] = useState(1);
      const [flashStep, setFlashStep] = useState(null);
      const cardRefs = useRef({});
      const flashTimer = useRef(null);

      useEffect(() => () => { if (flashTimer.current) clearTimeout(flashTimer.current); }, []);

      // Toggling a card header is also a focus signal, so keep the rail's
      // active marker in sync with it — otherwise the highlight stays on
      // the last rail-clicked step while the user works elsewhere.
      const toggle = n => {
        setActiveStep(n);
        const next = new Set(expanded);
        next.has(n) ? next.delete(n) : next.add(n);
        setExpanded(next);
      };

      // Rail click: expand the step, mark it active, smooth-scroll its card
      // just below the sticky top bar, and trigger a brief orange glow. The
      // page scrolls on the window (no ancestor has a definite height, so
      // the inner main never becomes a scroll container), and 72px clears
      // the 48px header plus a gap, matching the rail's sticky offset.
      // Clearing flashStep before re-setting it restarts the animation when
      // the same step is clicked twice; the nested rAF lets the expand
      // reflow settle so the scroll target is measured against the final
      // layout.
      const selectStep = n => {
        setExpanded(prev => prev.has(n) ? prev : new Set(prev).add(n));
        setActiveStep(n);
        setFlashStep(null);
        requestAnimationFrame(() => requestAnimationFrame(() => {
          const card = cardRefs.current[n];
          if (card) {
            const top = window.scrollY + card.getBoundingClientRect().top - 72;
            window.scrollTo({ top: Math.max(0, top), behavior: "smooth" });
          }
          setFlashStep(n);
        }));
        if (flashTimer.current) clearTimeout(flashTimer.current);
        flashTimer.current = setTimeout(() => setFlashStep(null), 1400);
      };

      const totalSec = steps.reduce((acc, s) => acc + (s.duration_seconds || 0), 0);
      const peakSec  = steps.reduce((acc, s) => Math.max(acc, s.duration_seconds || 0), 0);
      const totalTok = steps.reduce((acc, s) => acc + (s.total_tokens || 0), 0);
      let peakIdx = -1;
      steps.forEach((s, i) => { if (peakSec > 0 && peakIdx === -1 && (s.duration_seconds || 0) === peakSec) peakIdx = i; });

      return (
        <div style={{ display: "flex", alignItems: "flex-start", gap: 24 }}>
          <StepRail steps={steps} activeStep={activeStep} peakIdx={peakIdx} onSelect={selectStep}/>
          <div style={{ flex: 1, minWidth: 0, maxWidth: 920, display: "flex", flexDirection: "column", gap: 12 }}>
            <div style={{ borderBottom: "1px solid var(--border-weak)", paddingBottom: 9, display: "flex", alignItems: "center", gap: 8 }}>
              <Icon name="list" size={13} style={{ color: "var(--fg3)" }}/>
              <span style={{ fontSize: 13, color: "var(--fg1)", fontWeight: 500 }}>Conversation thread</span>
              <span style={{ flex: 1 }}/>
              <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg2)" }}>
                {steps.length} {steps.length === 1 ? "call" : "calls"} · peak {formatDuration(peakSec)} · {formatTokens(totalTok)} tok · {formatDuration(totalSec)} aggregate
              </span>
            </div>
            <div>
              {steps.map((s, i) => <StepCard key={s.generation_id || i} step={s} n={i + 1} expanded={expanded.has(i + 1)} onToggle={() => toggle(i + 1)} innerRef={el => { cardRefs.current[i + 1] = el; }} flash={flashStep === i + 1}/>)}
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
      // buckets, so the steps are the reliable source. Mirror the list
      // KPI: cache reads over cache reads + fresh input, capped at 99% so a
      // near-perfect cache doesn't round up to a misleading 100%.
      const cache = (steps || []).reduce((a, s) => {
        const b = s.token_buckets || {};
        a.read += b.cache_read || 0;
        a.fresh += b.fresh_input || 0;
        return a;
      }, { read: 0, fresh: 0 });
      const cacheDenom = cache.read + cache.fresh;
      const cachePct = cacheDenom === 0 ? null
        : cache.read === cacheDenom ? 100
        : Math.min(99, Math.round((cache.read / cacheDenom) * 100));

      const stats = [
        { value: formatDuration(wallSec),         unit: "elapsed" },
        { value: String(conv.calls),              unit: conv.calls === 1 ? "call" : "calls" },
        { value: formatTokens(conv.total_tokens), unit: "tokens" },
        ...(cachePct != null ? [{ value: `${cachePct}%`, unit: "cached", color: "var(--viz-green)" }] : []),
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
              {error && <Notice kind="error" title="Failed to load conversation">{error}</Notice>}
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
        || a.debug !== b.debug || a.autoUpdate !== b.autoUpdate || a.userId !== b.userId) return false;
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

    function Segmented({ value, onChange, options }) {
      return (
        <div style={{ display: "inline-flex", padding: 3, gap: 3, background: "var(--bg-canvas)", border: "1px solid var(--border-medium)", borderRadius: 2 }}>
          {options.map(o => {
            const active = o.value === value;
            return (
              <button key={o.value} onClick={() => onChange(o.value)} style={{
                padding: "6px 14px", borderRadius: 2, fontSize: 13, border: "none", cursor: "pointer",
                background: active ? "var(--secondary-main)" : "transparent",
                color: active ? "var(--fg-max)" : "var(--fg2)",
                fontWeight: active ? 500 : 400, transition: "background .12s, color .12s",
              }}>{o.label}</button>
            );
          })}
        </div>
      );
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

    function SettingsCard({ children }) {
      return <div style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "4px 20px 10px", marginBottom: 16 }}>{children}</div>;
    }

    function SectionLabel({ children }) {
      return <div style={{ padding: "16px 0 2px", fontSize: 11, fontWeight: 600, letterSpacing: ".06em", textTransform: "uppercase", color: "var(--fg3)" }}>{children}</div>;
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

    function Toast({ message }) {
      return (
        <div style={{ position: "fixed", top: 60, right: 20, zIndex: 30, display: "flex", alignItems: "center", gap: 8, background: "var(--bg-secondary)", border: "1px solid var(--border-medium)", borderLeft: "3px solid var(--success-border)", borderRadius: 2, padding: "10px 14px", boxShadow: "var(--shadow-z2)", animation: "sigil-tin .2s ease-out" }}>
          <Icon name="check" size={16} style={{ color: "var(--success-text)" }}/>
          <span style={{ fontSize: 13, color: "var(--fg1)" }}>{message}</span>
        </div>
      );
    }

    const CAPTURE_OPTIONS = [{ value: "metadata_only", label: "Metadata only" }, { value: "full", label: "Full" }];
    const GUARD_OPTIONS = [{ value: "off", label: "Disabled" }, { value: "failopen", label: "Fail-open" }, { value: "failclosed", label: "Fail-closed" }];

    function SettingsView() {
      const [form, setForm] = useState(null);
      const [saved, setSaved] = useState(null);
      const [preview, setPreview] = useState("");
      const [path, setPath] = useState("~/.config/agento11y/config.env");
      const [loading, setLoading] = useState(true);
      const [error, setError] = useState(null);
      const [toast, setToast] = useState(null);
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
          })
          .catch(e => { if (alive) setError(String(e.message || e)); })
          .finally(() => { if (alive) setLoading(false); });
        return () => { alive = false; };
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

      const page = { maxWidth: 1300, margin: "0 auto", padding: "28px 24px 110px", width: "100%" };
      if (loading && !form) {
        return <div style={page}><Notice kind="info" title="Loading settings…">Reading config.env.</Notice></div>;
      }
      if (!form) {
        return <div style={page}><Notice kind="error" title="Failed to load settings">{error}</Notice></div>;
      }

      const dirty = !sameSettings(form, saved);
      const captureUnset = form.capture === "";
      const advanced = form.capture === "no_tool_content" || form.capture === "full_with_metadata_spans";
      const guardsOn = form.guards !== "off";
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
            showToast("Settings saved to config.env.");
          })
          .catch(e => setError(String(e.message || e)));
      };
      const copy = () => {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(preview).then(() => showToast("Copied to clipboard.")).catch(() => {});
        }
      };

      return (
        <div style={page}>
          <h1 style={{ fontSize: 20, fontWeight: 500, color: "var(--fg-max)", margin: "0 0 20px" }}>Settings</h1>

          {error && <div style={{ marginBottom: 16 }}><Notice kind="error" title="Couldn’t save settings">{error}</Notice></div>}

          <div style={{ display: "flex", gap: 24, alignItems: "flex-start" }}>
            <div style={{ flex: "1 1 0", minWidth: 0 }}>
              <SettingsCard>
                <SectionLabel>Connection</SectionLabel>
                <div style={{ fontSize: 12, lineHeight: 1.5, color: "var(--fg3)", padding: "0 0 10px" }}>
                  These values apply to your Grafana Cloud sessions.
                </div>
                <SettingRow label="Endpoint" help={<>Grafana AI Observability ingest URL.</>}>
                  <MonoInput value={form.endpoint} onChange={v => set({ endpoint: v })} placeholder="https://sigil-prod-….grafana.net" width={320}/>
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
                  label="Content capture mode"
                  help={<>
                    What content sigil sends to Grafana Cloud for each generation. <Mono>--local</Mono> sessions always capture full content on this machine.
                    {captureUnset && <div style={{ color: "var(--fg3)", marginTop: 6 }}>Not set: Grafana Cloud sessions capture metadata only. Pick a mode to pin it.</div>}
                    {advanced && <div style={{ color: "var(--warning-text)", marginTop: 6 }}>Advanced mode <Mono>{form.capture}</Mono> is set in config.env and will be preserved.</div>}
                  </>}
                >
                  <Segmented value={form.capture} onChange={v => set({ capture: v })} options={CAPTURE_OPTIONS}/>
                </SettingRow>
              </SettingsCard>

              <SettingsCard>
                <SectionLabel>Tags</SectionLabel>
                <SettingRow full label="Session tags" help={<>Applied to every generation as <Mono>key=value</Mono>. Empty pairs are dropped on save.</>}>
                  <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
                    {form.tags.map((t, i) => (
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
                      borderRadius: 2, color: "var(--fg2)", fontSize: 13, cursor: "pointer",
                    }}
                      onMouseEnter={e => e.currentTarget.style.borderColor = "var(--border-strong)"}
                      onMouseLeave={e => e.currentTarget.style.borderColor = "var(--border-medium)"}>
                      <Icon name="plus" size={13}/>Add tag
                    </button>
                  </div>
                </SettingRow>
              </SettingsCard>

              <SettingsCard>
                <SectionLabel>Guards</SectionLabel>
                <SettingRow label="Pre-tool-use guards" help={<>Run safety checks before each tool call. <b style={{ fontWeight: 500, color: "var(--fg2)" }}>Fail-open</b> allows the action if a check errors or times out; <b style={{ fontWeight: 500, color: "var(--fg2)" }}>fail-closed</b> blocks it.</>}>
                  <Segmented value={form.guards} onChange={v => set({ guards: v })} options={GUARD_OPTIONS}/>
                </SettingRow>
                {guardsOn && (
                  <SettingRow label="Guard timeout" help={<>Max time for a guard check to respond. Clear the field to use the default of 1500 ms.</>}>
                    <div style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                      <MonoInput value={form.guardTimeout} onChange={v => set({ guardTimeout: v })} placeholder="1500" width={110} align="right"/>
                      <span style={{ fontSize: 12, color: "var(--fg3)" }}>ms</span>
                    </div>
                  </SettingRow>
                )}
              </SettingsCard>

              <SettingsCard>
                <SectionLabel>Runtime</SectionLabel>
                <SettingRow label="Debug logging" help={<>Write a verbose log to <Mono>~/.local/state/sigil/logs/sigil.log</Mono>.</>}>
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
            </div>

            <div style={{ width: 440, flexShrink: 0, position: "sticky", top: 72 }}>
              <div style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2 }}>
                <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "10px 14px", borderBottom: "1px solid var(--border-weak)" }}>
                  <span style={{ fontSize: 12, color: "var(--fg2)", fontFamily: "var(--fontFamilyMonospace)", flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{path}</span>
                  <button onClick={copy} style={{ display: "inline-flex", alignItems: "center", gap: 5, background: "transparent", border: "1px solid var(--secondary-border)", color: "var(--fg1)", borderRadius: 2, height: 26, padding: "0 8px", fontSize: 12, cursor: "pointer" }}
                    onMouseEnter={e => e.currentTarget.style.background = "var(--action-hover)"}
                    onMouseLeave={e => e.currentTarget.style.background = "transparent"}>
                    <Icon name="copy" size={13}/>Copy
                  </button>
                </div>
                <div style={{ background: "var(--bg-canvas)", padding: "14px 16px", maxHeight: "calc(100vh - 220px)", overflow: "auto" }}>
                  <PreviewBody text={preview}/>
                </div>
              </div>
            </div>
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

    // settingsRouteActive reports whether the URL is the Settings tab. It is
    // the only non-conversation route; every other path is the conversations
    // section (the list, or a detail when conversationIDFromPath matches).
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

    function App() {
      const [selectedID, setSelectedID] = useState(conversationIDFromPath);
      const [showSettings, setShowSettings] = useState(settingsRouteActive);
      const [conversations, setConversations] = useState([]);
      const [tokenPoints, setTokenPoints] = useState([]);
      const [loadingList, setLoadingList] = useState(true);
      const [errList, setErrList] = useState(null);
      const [query, setQuery] = useState("");
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

      const view = showSettings ? "settings" : (selectedID ? "conversation" : "conversations");
      const selected = selectedID
        ? conversations.find(c => c.id === selectedID) || summaryFromDetail(detail, selectedID)
        : null;

      // Changing the time range invalidates a bucket drill-down: the
      // bucket boundaries belong to the old window.
      const changeTimeRange = useCallback(v => {
        setBucketSel(null);
        setTimeRange(v);
      }, [setTimeRange]);

      const pageTitle = view === "settings"
        ? "Settings — sigil local"
        : view === "conversation" && selected
          ? `${selected.title || selected.id} — sigil local`
          : "sigil — local";
      useEffect(() => { document.title = pageTitle; }, [pageTitle]);

      const fetchList = useCallback(() => {
        setLoadingList(true);
        setErrList(null);
        return fetch("/api/v1/conversations")
          .then(r => r.ok ? r.json() : r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`))))
          .then(body => {
            setConversations(body.conversations || []);
          })
          .catch(e => setErrList(String(e.message || e)))
          .finally(() => setLoadingList(false));
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

      const fetchDetail = useCallback((id) => {
        setLoadingDetail(true);
        setErrDetail(null);
        setDetail(null);
        return fetch(`/api/v1/conversations/${encodeURIComponent(id)}`)
          .then(r => {
            if (r.status === 404) throw new Error("Conversation not found in the local store.");
            if (!r.ok) return r.text().then(t => Promise.reject(new Error(t || `HTTP ${r.status}`)));
            return r.json();
          })
          .then(setDetail)
          .catch(e => setErrDetail(String(e.message || e)))
          .finally(() => setLoadingDetail(false));
      }, []);

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

      // Auto-refresh the list every 30s so newly-recorded generations
      // surface without an explicit reload. Detail view is intentionally
      // not auto-refreshed — opening a step shouldn't move under the
      // user.
      useEffect(() => {
        if (view !== "conversations") return;
        const id = setInterval(refreshAll, 30_000);
        return () => clearInterval(id);
      }, [view, refreshAll]);

      const openConv = (c) => {
        window.history.pushState({}, "", conversationPath(c.id));
        setShowSettings(false);
        setSelectedID(c.id);
      };
      const goHome = () => {
        window.history.pushState({}, "", "/");
        setShowSettings(false);
        setSelectedID(null);
      };
      const goSettings = () => {
        window.history.pushState({}, "", "/settings");
        setSelectedID(null);
        setShowSettings(true);
      };

      const tabs = [
        { label: "Conversations", href: "/", onClick: goHome, state: view === "conversations" ? "current" : "link" },
        { label: "Settings", href: "/settings", onClick: goSettings, state: view === "settings" ? "current" : "link" },
      ];
      const trail = view === "conversation" && selected
        ? [{ label: selected.title || selected.id, mono: true }]
        : [];

      return (
        <div style={{ minHeight: "100vh", display: "flex", flexDirection: "column" }}>
          <TopBar tabs={tabs} trail={trail}/>
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
