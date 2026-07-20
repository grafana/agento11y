package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// renderJSON writes the stable machine-readable report. encoding/json never
// emits color codes, and the token value is absent from the Report type, so
// this output is safe to hand to support tooling.
func renderJSON(w io.Writer, r *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// palette renders styled text when color is on, plain text otherwise. When
// color is on it uses lipgloss, which itself drops color codes on a non-TTY
// writer, so captured/redirected output is plain regardless.
type palette struct {
	color bool
}

var (
	orangeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FF671D"))
	okStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#73BF69"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF9830"))
	errStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#F2495C"))
	faintStyle  = lipgloss.NewStyle().Faint(true)
)

func (p palette) heading(s string) string { return p.apply(orangeStyle, s) }
func (p palette) faint(s string) string   { return p.apply(faintStyle, s) }

// sectionTitle colors a section header by its health: green when passing, red
// when failing, orange for warnings.
func (p palette) sectionTitle(s string, h Health) string {
	switch h {
	case HealthOK:
		return p.apply(okStyle, s)
	case HealthWarn:
		return p.apply(warnStyle, s)
	case HealthError:
		return p.apply(errStyle, s)
	default:
		return p.heading(s)
	}
}

func (p palette) apply(style lipgloss.Style, s string) string {
	if !p.color {
		return s
	}
	return style.Render(s)
}

// glyph returns the status marker for a health level.
func (p palette) glyph(h Health) string {
	switch h {
	case HealthOK:
		return p.apply(okStyle, "✓")
	case HealthWarn:
		return p.apply(warnStyle, "!")
	case HealthError:
		return p.apply(errStyle, "✗")
	default:
		return p.faint("·")
	}
}

// renderHuman writes the colored (or plain) report. probed reports whether
// live probes ran; when they didn't, a hint nudges the user toward --probe
// since the section verdicts are config-only and don't test credentials.
func renderHuman(w io.Writer, r *Report, color, probed bool) {
	p := palette{color: color}
	var b strings.Builder

	fmt.Fprintf(&b, "%s %s\n\n", p.heading("agento11y doctor"), p.faint("v"+r.Binary.Version))

	// Conversations pipeline.
	writeSection(&b, p, "Conversations (generation export)", r.Conversations.Health)
	writeKV(&b, p, "endpoint", describeEnv(r.Conversations.Endpoint))
	writeKV(&b, p, "tenant id", describeEnv(r.Conversations.TenantID))
	writeKV(&b, p, "auth token", describeToken(r.Conversations.Token))
	if r.Conversations.Probe != nil {
		writeKV(&b, p, "probe", describeProbe(r.Conversations.Probe))
	}
	writeMessages(&b, p, r.Conversations.Messages)
	b.WriteString("\n")

	// Analytics pipeline.
	writeSection(&b, p, "Analytics (OTLP metrics & traces)", r.Analytics.Health)
	if r.Analytics.Endpoint.Set {
		writeKV(&b, p, "endpoint", fmt.Sprintf("%s %s", r.Analytics.Endpoint.Value, p.faint("("+r.Analytics.EndpointVar+", "+r.Analytics.Endpoint.Source+")")))
	} else {
		writeKV(&b, p, "endpoint", p.faint("not set"))
	}
	if r.Analytics.Probe != nil {
		if r.Analytics.Probe.Metrics != nil {
			writeKV(&b, p, "metrics probe", describeProbe(r.Analytics.Probe.Metrics))
		}
		if r.Analytics.Probe.Traces != nil {
			writeKV(&b, p, "traces probe", describeProbe(r.Analytics.Probe.Traces))
		}
	}
	writeMessages(&b, p, r.Analytics.Messages)
	b.WriteString("\n")

	// Config.
	writeSection(&b, p, "Config", r.Config.Health)
	exists := "missing"
	if r.Config.Exists {
		exists = "present"
	}
	writeKV(&b, p, "file", fmt.Sprintf("%s %s", r.Config.Path, p.faint("("+exists+")")))
	mode := r.Config.ContentCaptureMode
	if r.Config.ContentModeFellBack {
		mode += " " + p.faint("(invalid value, fell back)")
	}
	writeKV(&b, p, "content capture", mode)
	writeKV(&b, p, "guards", describeGuards(p, r.Config))
	if len(r.Config.Tags) > 0 {
		writeKV(&b, p, "tags", fmt.Sprintf("%s %s", formatTags(r.Config.Tags), p.faint("("+r.Config.TagsSource+")")))
	}
	writeMessages(&b, p, r.Config.Messages)
	b.WriteString("\n")

	// Agents.
	fmt.Fprintf(&b, "%s\n", p.sectionTitle("Coding agents", HealthOK))
	for _, a := range r.Agents {
		writeKV(&b, p, a.Name, describeAgent(p, a))
	}
	if r.AutoUpdateDisabled {
		writeKV(&b, p, "auto-update", p.faint("disabled (SIGIL_AUTO_UPDATE)"))
	}
	b.WriteString("\n")

	// Summary.
	writeSummary(&b, p, r)
	if !probed {
		writeProbeHint(&b, p, r)
	}

	_, _ = io.WriteString(w, b.String())
}

// writeProbeHint nudges toward --probe when the report is config-only and
// there is something to probe. Without it, the section verdicts reflect only
// that credentials are present, not that they work.
func writeProbeHint(b *strings.Builder, p palette, r *Report) {
	if !r.Conversations.configured() && !r.Analytics.Endpoint.Set {
		return
	}
	fmt.Fprintf(b, "\n%s\n", p.faint("Verdicts above check configuration only. Run `agento11y doctor --probe` to test credentials against the endpoints."))
}

func writeSection(b *strings.Builder, p palette, title string, h Health) {
	fmt.Fprintf(b, "%s %s\n", p.glyph(h), p.sectionTitle(title, h))
}

func writeKV(b *strings.Builder, p palette, key, value string) {
	fmt.Fprintf(b, "  %s %s\n", p.faint(padRight(key+":", 16)), value)
}

func writeMessages(b *strings.Builder, p palette, messages []string) {
	for _, m := range messages {
		fmt.Fprintf(b, "  %s %s\n", p.glyph(HealthWarn), m)
	}
}

func writeSummary(b *strings.Builder, p palette, r *Report) {
	var broken []string
	if r.Conversations.Health == HealthError {
		broken = append(broken, "conversations")
	}
	if r.Analytics.Health == HealthError {
		broken = append(broken, "analytics")
	}
	if r.Config.Health == HealthError {
		broken = append(broken, "config")
	}
	if len(broken) == 0 {
		fmt.Fprintf(b, "%s %s\n", p.glyph(HealthOK), "no problems detected")
		return
	}
	fmt.Fprintf(b, "%s %s\n", p.glyph(HealthError),
		fmt.Sprintf("%d problem(s): %s misconfigured", len(broken), strings.Join(broken, ", ")))
}

func describeEnv(v envValue) string {
	if !v.Set {
		return "not set"
	}
	return fmt.Sprintf("%s (%s)", v.Value, v.Source)
}

func describeToken(t tokenValue) string {
	if !t.Set {
		return "not set"
	}
	if t.Prefix != "" {
		return fmt.Sprintf("set (%s…, %s)", t.Prefix, t.Source)
	}
	return fmt.Sprintf("set (%s)", t.Source)
}

// describeGuards renders the resolved guard feature flags. Guards default off,
// so a plain "disabled" is the common line; when on, the timeout and fail mode
// matter (fail-closed blocks the tool call when a guard errors or times out).
func describeGuards(p palette, c ConfigSection) string {
	var out string
	if c.GuardsEnabled {
		failMode := "fail-open"
		if !c.GuardsFailOpen {
			failMode = "fail-closed"
		}
		out = "enabled " + p.faint(fmt.Sprintf("(timeout %dms, %s)", c.GuardsTimeoutMs, failMode))
	} else {
		out = p.faint("disabled")
	}
	if c.GuardsFellBack {
		out += " " + p.faint("(invalid value, fell back)")
	}
	return out
}

func describeProbe(p *ProbeResult) string {
	if p == nil {
		return "skipped"
	}
	status := "no response"
	if p.StatusCode != 0 {
		status = fmt.Sprintf("HTTP %d", p.StatusCode)
	}
	if p.Message != "" {
		return fmt.Sprintf("%s — %s", status, p.Message)
	}
	return status
}

func describeAgent(p palette, a AgentStatus) string {
	// Hook-only agents (cursor) are detected purely by PATH presence.
	if a.HookBased {
		return agentNote(p, joinAgent("detected", a.Version), a.Note)
	}
	// The install probe never ran: a CLI-dependent agent whose binary is
	// absent, or a hook-only agent off PATH.
	if a.Health == HealthSkipped {
		return agentNote(p, p.faint("not found on PATH"), a.Note)
	}
	// Hook-file based agent (copilot): capture doesn't depend on the CLI being
	// on PATH, so report install state with its own wording and no PATH
	// qualifiers.
	if a.notInstalledLabel != "" {
		state := a.notInstalledLabel
		if a.Installed {
			state = "installed"
		}
		return agentNote(p, joinAgent(state, a.Version), a.Note)
	}
	// The probe ran. Report install state, only claiming "on PATH" when true so
	// config-based agents installed without the CLI present aren't mislabeled.
	var state string
	switch {
	case a.Installed && a.OnPath:
		state = "installed"
	case a.Installed:
		state = "installed " + p.faint("(CLI not on PATH)")
	case a.OnPath:
		state = "on PATH, plugin not installed"
	default:
		state = "plugin not installed"
	}
	return agentNote(p, joinAgent(state, a.Version), a.Note)
}

func joinAgent(state, version string) string {
	if version != "" {
		return state + " v" + version
	}
	return state
}

func agentNote(p palette, body, note string) string {
	if note == "" {
		return body
	}
	return body + " " + p.faint("("+note+")")
}

// formatTags renders a tag map as "k=v, k=v" with keys sorted so the line is
// deterministic across runs.
func formatTags(tags map[string]string) string {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
