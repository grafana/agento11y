    const { useState, useEffect, useMemo, useCallback } = React;

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
      { value: "1h", label: "Last 1 hour", ms: 60 * 60 * 1000 },
      { value: "6h", label: "Last 6 hours", ms: 6 * 60 * 60 * 1000 },
      { value: "24h", label: "Last 24 hours", ms: 24 * 60 * 60 * 1000 },
      { value: "7d", label: "Last 7 days", ms: 7 * 24 * 60 * 60 * 1000 },
      { value: "all", label: "All", ms: null },
    ];

    function timeRangeOption(value) {
      return TIME_RANGES.find(r => r.value === value) || TIME_RANGES[1];
    }

    function conversationTime(c) {
      const t = new Date(c.last_activity || c.started_at).getTime();
      return Number.isFinite(t) ? t : null;
    }

    function formatBucketSize(ms) {
      if (!Number.isFinite(ms) || ms <= 0) return "buckets";
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
      return d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", hour12: false });
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

    function bucketActivity(conversations, rangeValue, now, { count = 12 } = {}) {
      const range = timeRangeOption(rangeValue);
      const times = conversations.map(conversationTime).filter(t => t != null);
      const end = range.ms == null
        ? Math.max(now, ...times)
        : now;
      const start = range.ms == null
        ? (times.length ? Math.min(...times) : end - 60 * 60 * 1000)
        : end - range.ms;
      const span = Math.max(end - start, 60 * 1000);
      const bucketMs = span / count;
      const buckets = [];
      for (let i = 0; i < count; i++) {
        const bucketStart = start + i * bucketMs;
        const bucketEnd = i === count - 1 ? end + 1 : bucketStart + bucketMs;
        buckets.push({
          t: formatBucketLabel(bucketStart, bucketMs),
          start: bucketStart,
          end: bucketEnd,
          c: 0,
        });
      }
      for (const t of times) {
        if (t < start || t > end) continue;
        const idx = Math.min(count - 1, Math.max(0, Math.floor((t - start) / bucketMs)));
        buckets[idx].c++;
      }
      return { buckets, bucketLabel: formatBucketSize(bucketMs) };
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
        <div style={{ display: "flex", alignItems: "center", gap: 8, userSelect: "none" }}>
          <GrafanaMark size={22}/>
          <div style={{ display: "flex", alignItems: "baseline", gap: 6 }}>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 14, letterSpacing: "0.02em", color: "var(--fg-max)", fontWeight: 500 }}>Grafana AI Observability</span>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 10.5, color: "var(--fg3)", letterSpacing: "0.08em", textTransform: "uppercase" }}>local</span>
          </div>
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

    function TopBar({ breadcrumbs = [] }) {
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
          <nav style={{ display: "flex", alignItems: "center", gap: 6, minWidth: 0, flex: 1, overflow: "hidden" }}>
            {breadcrumbs.map((b, i) => {
              const last = i === breadcrumbs.length - 1;
              return (
                <React.Fragment key={i}>
                  {i > 0 && <Icon name="cright" size={11} style={{ color: "var(--fg3)", flexShrink: 0 }}/>}
                  {last
                    ? <span style={{
                        fontFamily: b.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)",
                        fontSize: 13,
                        color: "var(--fg-max)",
                        whiteSpace: "nowrap",
                        overflow: "hidden",
                        textOverflow: "ellipsis",
                        minWidth: 0,
                      }}>{b.label}</span>
                    : b.href
                      ? <a href={b.href}
                          onClick={e => {
                            if (!isPlainLeftClick(e)) return;
                            e.preventDefault();
                            b.onClick && b.onClick(e);
                          }}
                          style={{
                            background: "transparent",
                            padding: "2px 4px", cursor: "pointer",
                            color: "var(--fg2)", fontSize: 13,
                            fontFamily: b.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)",
                            whiteSpace: "nowrap", flexShrink: 0,
                            textDecoration: "none",
                          }}
                          onMouseEnter={e => e.currentTarget.style.color = "var(--fg-max)"}
                          onMouseLeave={e => e.currentTarget.style.color = "var(--fg2)"}
                        >{b.label}</a>
                      : <button onClick={b.onClick} style={{
                          background: "transparent", border: "none",
                          padding: "2px 4px", cursor: b.onClick ? "pointer" : "default",
                          color: "var(--fg2)", fontSize: 13,
                          fontFamily: b.mono ? "var(--fontFamilyMonospace)" : "var(--fontFamily)",
                          whiteSpace: "nowrap", flexShrink: 0,
                        }}
                        onMouseEnter={e => b.onClick && (e.currentTarget.style.color = "var(--fg-max)")}
                        onMouseLeave={e => b.onClick && (e.currentTarget.style.color = "var(--fg2)")}
                        >{b.label}</button>
                  }
                </React.Fragment>
              );
            })}
          </nav>
          <a
            href="https://grafana.com/auth/sign-up/create-user/?"
            target="_blank"
            rel="noreferrer"
            style={{
              height: 30,
              display: "inline-flex", alignItems: "center", justifyContent: "center",
              padding: "0 12px",
              border: "1px solid var(--brand-orange)",
              borderRadius: 2,
              background: "var(--brand-orange)",
              color: "#111217",
              textDecoration: "none",
              fontSize: 12,
              fontWeight: 600,
              whiteSpace: "nowrap",
              flexShrink: 0,
            }}>
            Sign up for Grafana Cloud
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

    function ActivityChart({ data, bucketLabel, accent = "var(--brand-orange)" }) {
      const W = 100, H = 32;
      const max = Math.max(1, ...data.map(d => d.c));
      const barW = (W / Math.max(1, data.length)) * 0.7;
      const gap  = (W / Math.max(1, data.length)) * 0.3;
      const [hover, setHover] = useState(null);

      return (
        <div style={{ position: "relative", padding: "16px 20px 12px", background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2 }}>
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 10 }}>
            <div style={{ fontSize: 13, color: "var(--fg1)", fontWeight: 500 }}>Conversation activity</div>
            <div style={{ display: "flex", alignItems: "center", gap: 12, fontSize: 11, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>
              <span style={{ display: "inline-flex", alignItems: "center", gap: 6 }}>
                <span style={{ width: 10, height: 10, background: accent, borderRadius: 1 }}/> count
              </span>
              <span>{bucketLabel}</span>
            </div>
          </div>
          <div style={{ position: "relative" }}>
            <svg viewBox={`0 0 ${W} ${H + 8}`} preserveAspectRatio="none" style={{ width: "100%", height: 130, display: "block" }}>
              {[0, 1, 2, 3, 4].map(g => (
                <line key={g} x1={0} x2={W} y1={(H * g)/4} y2={(H * g)/4} stroke="rgba(204,204,220,0.06)" strokeWidth="0.2"/>
              ))}
              {data.map((d, i) => {
                const h = (d.c / max) * H;
                const x = i * (W / data.length) + gap/2;
                const y = H - h;
                const isHover = hover === i;
                return (
                  <g key={i} onMouseEnter={() => setHover(i)} onMouseLeave={() => setHover(null)}>
                    <rect x={x - 0.4} y={0} width={barW + 0.8} height={H} fill="transparent"/>
                    <rect x={x} y={y} width={barW} height={Math.max(h, 0.4)} fill={isHover ? "var(--brand-orange-text)" : accent} opacity={isHover ? 1 : 0.85}/>
                  </g>
                );
              })}
            </svg>
            <div style={{ display: "flex", justifyContent: "space-between", marginTop: 4, fontSize: 10, color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)" }}>
              {data.map((d, i) => <span key={i} style={{ flex: 1, textAlign: "left" }}>{d.t}</span>)}
            </div>
            {hover !== null && (
              <div style={{
                position: "absolute",
                left: `${((hover + 0.5) / data.length) * 100}%`,
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
        </div>
      );
    }

    function FilterBar({ query, onQueryChange, timeRange, onTimeRangeChange, onRefresh, refreshing }) {
      return (
        <div style={{ display: "flex", alignItems: "stretch", gap: 8, marginBottom: 16, fontSize: 13 }}>
          <div style={{
            flex: 1, display: "flex", alignItems: "center", gap: 8,
            padding: "0 10px",
            height: 32,
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
              height: 32,
              minWidth: 138,
              padding: "0 30px 0 10px",
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
            style={{ ...iconBtn, height: 32, width: 32, border: "1px solid var(--border-medium)", opacity: refreshing ? 0.5 : 1, cursor: refreshing ? "wait" : "pointer" }}
            title="Refresh">
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
          gridTemplateColumns: "90px minmax(280px, 1.4fr) 150px 100px minmax(180px, 1fr) minmax(160px, 1fr)",
          alignItems: "center",
          gap: 16,
          padding: "10px 16px",
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
            <span style={{ color: "var(--fg1)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.title || c.id}</span>
            {c.title && c.title !== c.id && (
              <span style={{ color: "var(--fg3)", fontSize: 11, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{c.id}</span>
            )}
          </div>
          <span style={{ color: "var(--fg2)" }}>
            <span style={{ color: "var(--fg1)" }}>{formatDuration(wallSec)}</span>
            <span style={{ color: "var(--fg3)", padding: "0 6px" }}>·</span>
            <span style={{ color: "var(--fg1)" }}>{c.calls} {c.calls === 1 ? "call" : "calls"}</span>
          </span>
          <span style={{ color: "var(--fg1)" }}>{formatTokens(c.total_tokens)}</span>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            {(c.agents || []).map(a => <AgentPill key={a} name={a} size="sm"/>)}
          </div>
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            {(c.models || []).map(m => <ModelPill key={m} name={m}/>)}
          </div>
        </a>
      );
    }

    function ConversationsView({ conversations, loading, error, query, setQuery, timeRange, setTimeRange, onOpen, onRefresh, refreshing }) {
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

      const activity = useMemo(() => bucketActivity(filtered, timeRange, now), [filtered, timeRange, now]);

      return (
        <div style={{ padding: 24, maxWidth: 1600, margin: "0 auto" }}>
          <FilterBar query={query} onQueryChange={setQuery} timeRange={timeRange} onTimeRangeChange={setTimeRange} onRefresh={onRefresh} refreshing={refreshing}/>
          <ActivityChart data={activity.buckets} bucketLabel={activity.bucketLabel}/>

          <div style={{
            marginTop: 18,
            border: "1px solid var(--border-weak)",
            borderRadius: 2,
            overflow: "hidden",
            background: "var(--bg-primary)",
          }}>
            <div style={{
              display: "grid",
              gridTemplateColumns: "90px minmax(280px, 1.4fr) 150px 100px minmax(180px, 1fr) minmax(160px, 1fr)",
              alignItems: "center", gap: 16,
              padding: "10px 16px 10px 19px",
              borderBottom: "1px solid var(--border-weak)",
              background: "var(--bg-secondary)",
              fontSize: 11, color: "var(--fg3)", textTransform: "uppercase", letterSpacing: "0.08em", fontWeight: 500,
            }}>
              <span>Last activity</span><span>Conversation</span><span>Duration</span><span>Tokens</span><span>Agents</span><span>Models</span>
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
            {filtered.map(c => <ConvRow key={c.id} c={c} now={now} onOpen={onOpen}/>)}
          </div>

          <div style={{
            marginTop: 14, padding: "10px 14px",
            fontSize: 11, color: "var(--fg3)",
            fontFamily: "var(--fontFamilyMonospace)",
          }}>
            {filtered.length} of {conversations.length} {conversations.length === 1 ? "conversation" : "conversations"}
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
      const labelColor = isUser ? "var(--brand-green)" : (isTool ? "var(--brand-purple)" : (isToolCall ? "var(--warning-text)" : "var(--brand-orange-text)"));
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
        return (
          <details style={{ marginTop: 2 }}>
            <summary style={{ cursor: "pointer", color: "var(--fg3)", fontSize: 11, fontFamily: "var(--fontFamilyMonospace)", textTransform: "uppercase", letterSpacing: "0.06em" }}>thinking</summary>
            <div style={{ fontSize: 12, color: "var(--fg2)", whiteSpace: "pre-wrap", marginTop: 4, fontStyle: "italic" }}>{part.thinking}</div>
          </details>
        );
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
              <pre style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "6px 8px", margin: "4px 0 0", fontSize: 12, color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all", overflowX: "auto" }}><span style={{ color: "var(--warning-text)" }}>$</span> {command}</pre>
            ) : args && (
              <pre style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "6px 8px", margin: "4px 0 0", fontSize: 11, color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all", overflowX: "auto" }}>{args}</pre>
            )}
          </div>
        );
      }
      if (kind === "tool_result" && part.tool_result) {
        const tr = part.tool_result;
        const body = tr.content || (tr.content_json ? (typeof tr.content_json === "string" ? tr.content_json : JSON.stringify(tr.content_json)) : "");
        const isErr = !!tr.is_error;
        return (
          <div style={{ marginTop: 4 }}>
            <div style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: isErr ? "var(--error-text)" : "var(--fg2)" }}>
              ← result{tr.tool_call_id ? <span style={{ color: "var(--fg3)" }}> · {tr.tool_call_id}</span> : null}{isErr ? <span style={{ color: "var(--error-text)" }}> · error</span> : null}
            </div>
            {body && (
              <pre style={{ background: "var(--bg-primary)", border: "1px solid var(--border-weak)", borderRadius: 2, padding: "6px 8px", margin: "4px 0 0", fontSize: 11, color: "var(--fg1)", whiteSpace: "pre-wrap", wordBreak: "break-all", overflowX: "auto" }}>{body}</pre>
            )}
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

    function StepCard({ step, n, expanded, onToggle }) {
      const hasError = !!step.call_error;
      const dotColor = hasError ? "var(--error-text)" : "#73BF69";
      return (
        <div style={{
          border: "1px solid var(--border-weak)",
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

    function ConversationThread({ steps }) {
      // First 4 cards default to expanded — the typical attention zone.
      const [expanded, setExpanded] = useState(() => new Set(steps.slice(0, 4).map((_, i) => i + 1)));
      const toggle = n => {
        const next = new Set(expanded);
        next.has(n) ? next.delete(n) : next.add(n);
        setExpanded(next);
      };

      const totalSec = steps.reduce((acc, s) => acc + (s.duration_seconds || 0), 0);
      const peakSec  = steps.reduce((acc, s) => Math.max(acc, s.duration_seconds || 0), 0);
      const totalTok = steps.reduce((acc, s) => acc + (s.total_tokens || 0), 0);

      return (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <div style={{ borderBottom: "1px solid var(--border-weak)", paddingBottom: 8, display: "flex", alignItems: "center", gap: 8 }}>
            <Icon name="list" size={12} style={{ color: "var(--fg3)" }}/>
            <span style={{ fontSize: 11, color: "var(--fg3)", textTransform: "uppercase", letterSpacing: "0.08em" }}>Conversation thread</span>
            <span style={{ flex: 1 }}/>
            <span style={{ fontFamily: "var(--fontFamilyMonospace)", fontSize: 11, color: "var(--fg2)" }}>
              {steps.length} {steps.length === 1 ? "call" : "calls"} · peak {formatDuration(peakSec)} · {formatTokens(totalTok)} tok · {formatDuration(totalSec)} aggregate
            </span>
          </div>
          <div>
            {steps.map((s, i) => <StepCard key={s.generation_id || i} step={s} n={i + 1} expanded={expanded.has(i + 1)} onToggle={() => toggle(i + 1)}/>)}
          </div>
        </div>
      );
    }

    function DetailStats({ conv, steps }) {
      const wallSec = durationBetweenSeconds(conv.started_at, conv.last_activity);
      const errStatus = conv.status === "err";
      const stats = [
        { icon: "clock", label: formatDuration(wallSec),                       sub: "elapsed" },
        { icon: "swap",  label: `${conv.calls} ${conv.calls === 1 ? "call" : "calls"}`, sub: "calls" },
        { icon: "bolt",  label: formatTokens(conv.total_tokens),               sub: "tok" },
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
        <div style={{ display: "flex", gap: 18, alignItems: "center", padding: "12px 24px", borderBottom: "1px solid var(--border-weak)", background: "var(--bg-primary)", flexWrap: "wrap" }}>
          {stats.map((s, i) => (
            <div key={i} style={{ display: "inline-flex", alignItems: "center", gap: 6, color: "var(--fg2)", fontSize: 12, fontFamily: "var(--fontFamilyMonospace)", whiteSpace: "nowrap" }}>
              <Icon name={s.icon} size={13} style={{ color: "var(--fg3)" }}/>
              <span style={{ color: "var(--fg1)" }}>{s.label}</span>
              <span style={{ color: "var(--fg3)" }}>{s.sub}</span>
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
            padding: "4px 10px", height: 26,
            background: "transparent", color: "var(--fg1)",
            border: "1px solid var(--border-medium)",
            borderRadius: 2, fontSize: 11, cursor: "pointer", fontFamily: "var(--fontFamily)", fontWeight: 500,
            whiteSpace: "nowrap",
          }}>
            <Icon name="download" size={11}/> Export JSON
          </button>
        </div>
      );
    }

    function TraceDetailView({ conv, detail, loading, error }) {
      return (
        <div style={{ display: "flex", flexDirection: "column", flex: 1, minHeight: 0, background: "var(--bg-canvas)" }}>
          <DetailStats conv={conv} steps={detail ? detail.generations : []}/>
          <main style={{ padding: "24px 32px", overflowY: "auto", flex: 1 }}>
            <div style={{ maxWidth: 880, margin: "0 auto" }}>
              {error && <Notice kind="error" title="Failed to load conversation">{error}</Notice>}
              {!error && loading && <div style={{ color: "var(--fg3)", fontFamily: "var(--fontFamilyMonospace)", fontSize: 12 }}>Loading…</div>}
              {!error && !loading && detail && <ConversationThread steps={detail.generations}/>}
            </div>
          </main>
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

    function App() {
      const [selectedID, setSelectedID] = useState(conversationIDFromPath);
      const [conversations, setConversations] = useState([]);
      const [loadingList, setLoadingList] = useState(true);
      const [errList, setErrList] = useState(null);
      const [query, setQuery] = useState("");
      const [timeRange, setTimeRange] = useState("6h");

      const [detail, setDetail] = useState(null);
      const [loadingDetail, setLoadingDetail] = useState(false);
      const [errDetail, setErrDetail] = useState(null);

      const view = selectedID ? "conversation" : "conversations";
      const selected = selectedID
        ? conversations.find(c => c.id === selectedID) || summaryFromDetail(detail, selectedID)
        : null;

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

      useEffect(() => { fetchList(); }, [fetchList]);

      useEffect(() => {
        const onPopState = () => setSelectedID(conversationIDFromPath());
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
        const id = setInterval(fetchList, 30_000);
        return () => clearInterval(id);
      }, [view, fetchList]);

      const openConv = (c) => {
        window.history.pushState({}, "", conversationPath(c.id));
        setSelectedID(c.id);
      };
      const goHome = () => {
        window.history.pushState({}, "", "/");
        setSelectedID(null);
      };

      const breadcrumbs = selected
        ? [
            { label: "Conversations", href: "/", onClick: goHome },
            { label: selected.title || selected.id, mono: true },
          ]
        : [
            { label: "Conversations" },
          ];

      return (
        <div style={{ minHeight: "100vh", display: "flex", flexDirection: "column" }}>
          <TopBar breadcrumbs={breadcrumbs}/>
          <div style={{ flex: 1, display: "flex", flexDirection: "column", minHeight: 0 }}>
            {view === "conversations" && (
              <ConversationsView
                conversations={conversations}
                loading={loadingList}
                error={errList}
                query={query}
                setQuery={setQuery}
                timeRange={timeRange}
                setTimeRange={setTimeRange}
                onOpen={openConv}
                onRefresh={fetchList}
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
