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

// reportBody accumulates the human report so key/value lines can be padded
// once the whole report is known. The column width is the widest key the run
// actually rendered; a constant would be overflowed by the longest keys.
type reportBody struct {
	lines    []bodyLine
	keyWidth int
}

// bodyLine is either a literal line (text) or a key/value pair padded when
// the body is flushed. isKV is the discriminator, so an empty key still
// renders its line instead of dropping it.
type bodyLine struct {
	text  string
	key   string
	value string
	isKV  bool
}

func (b *reportBody) textf(format string, args ...any) {
	b.lines = append(b.lines, bodyLine{text: fmt.Sprintf(format, args...)})
}

func (b *reportBody) kv(key, value string) {
	b.lines = append(b.lines, bodyLine{key: key, value: value, isKV: true})
	if w := len(key) + len(":"); w > b.keyWidth {
		b.keyWidth = w
	}
}

func (b *reportBody) flush(p palette) string {
	var out strings.Builder
	for _, line := range b.lines {
		if !line.isKV {
			out.WriteString(line.text)
			continue
		}
		fmt.Fprintf(&out, "  %s %s\n", p.faint(padRight(line.key+":", b.keyWidth)), line.value)
	}
	return out.String()
}

// renderHuman writes the colored (or plain) report. probed reports whether
// live probes ran; when they didn't, a hint nudges the user toward --probe
// since the section verdicts are config-only and don't test credentials.
func renderHuman(w io.Writer, r *Report, color, probed bool) {
	p := palette{color: color}
	var b reportBody

	// Print the version as stamped, the way `agento11y --version` does. The
	// release ldflags already carry the `v` prefix.
	b.textf("%s %s\n\n", p.heading("agento11y doctor"), p.faint(r.Binary.Version))

	// Conversations pipeline.
	writeSection(&b, p, "Conversations (generation export)", r.Conversations.Health)
	b.kv("endpoint", describeEnv(r.Conversations.Endpoint))
	b.kv("tenant id", describeEnv(r.Conversations.TenantID))
	b.kv("auth token", describeToken(r.Conversations.Token))
	if r.Conversations.Probe != nil {
		b.kv("probe", describeProbe(r.Conversations.Probe))
	}
	writeMessages(&b, p, r.Conversations.Messages)
	b.textf("\n")

	// Analytics pipeline.
	writeSection(&b, p, "Analytics (OTLP metrics & traces)", r.Analytics.Health)
	if r.Analytics.Endpoint.Set {
		b.kv("endpoint", fmt.Sprintf("%s %s", r.Analytics.Endpoint.Value, p.faint("("+r.Analytics.EndpointVar+", "+r.Analytics.Endpoint.Source+")")))
	} else {
		b.kv("endpoint", p.faint("not set"))
	}
	if r.Analytics.Probe != nil {
		if r.Analytics.Probe.Metrics != nil {
			b.kv("metrics probe", describeProbe(r.Analytics.Probe.Metrics))
		}
		if r.Analytics.Probe.Traces != nil {
			b.kv("traces probe", describeProbe(r.Analytics.Probe.Traces))
		}
	}
	writeMessages(&b, p, r.Analytics.Messages)
	b.textf("\n")

	// Config.
	writeSection(&b, p, "Config", r.Config.Health)
	exists := "missing"
	if r.Config.Exists {
		exists = "present"
	}
	b.kv("file", fmt.Sprintf("%s %s", r.Config.Path, p.faint("("+exists+")")))
	mode := r.Config.ContentCaptureMode
	if r.Config.ContentModeFellBack {
		mode += " " + p.faint("(invalid value, fell back)")
	}
	b.kv("content capture", mode)
	b.kv("guards", describeGuards(p, r.Config))
	if len(r.Config.Tags) > 0 {
		b.kv("tags", fmt.Sprintf("%s %s", formatTags(r.Config.Tags), p.faint("("+r.Config.TagsSource+")")))
	}
	if r.Config.Local.Set {
		localMode := describeEnv(r.Config.Local)
		if r.Config.LocalInvalid {
			localMode += " " + p.faint("(invalid value, local mode is off)")
		}
		b.kv("local mode", localMode)
	}
	if r.Config.LocalForward.Set {
		b.kv("local forwarding", describeEnv(r.Config.LocalForward))
	}
	// Only worth a line once the user has opted into forwarding: with
	// LOCAL_FORWARD unset chaining is always off, and that is already what the
	// absent line above says.
	if r.Config.LocalForward.Set {
		b.kv("local guard checks", describeLocalHookForward(p, r.Config.LocalHookForward))
	}
	writeMessages(&b, p, r.Config.Messages)
	b.textf("\n")

	// Agents.
	b.textf("%s\n", p.sectionTitle("Coding agents", HealthOK))
	for _, a := range r.Agents {
		b.kv(a.Name, describeAgent(p, a))
	}
	if r.AutoUpdateDisabled {
		b.kv("auto-update", p.faint("disabled (SIGIL_AUTO_UPDATE)"))
	}
	b.textf("\n")

	// Summary.
	writeSummary(&b, p, r)
	if !probed {
		writeProbeHint(&b, p, r)
	}

	_, _ = io.WriteString(w, b.flush(p))
}

// writeProbeHint nudges toward --probe when the report is config-only and
// there is something to probe. Without it, the section verdicts reflect only
// that credentials are present, not that they work.
func writeProbeHint(b *reportBody, p palette, r *Report) {
	if !r.Conversations.configured() && !r.Analytics.Endpoint.Set {
		return
	}
	b.textf("\n%s\n", p.faint("Verdicts above check configuration only. Run `agento11y doctor --probe` to test credentials against the endpoints."))
}

func writeSection(b *reportBody, p palette, title string, h Health) {
	b.textf("%s %s\n", p.glyph(h), p.sectionTitle(title, h))
}

func writeMessages(b *reportBody, p palette, messages []string) {
	for _, m := range messages {
		b.textf("  %s %s\n", p.glyph(HealthWarn), m)
	}
}

func writeSummary(b *reportBody, p palette, r *Report) {
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
		b.textf("%s %s\n", p.glyph(HealthOK), "no problems detected")
		return
	}
	b.textf("%s %d problem(s): %s misconfigured\n", p.glyph(HealthError), len(broken), strings.Join(broken, ", "))
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

// describeLocalHookForward renders whether a --local session's guard checks
// reach Cloud, which is how a user learns their tool calls leave the machine
// even under a reduced content capture mode.
func describeLocalHookForward(p palette, h HookForwardSection) string {
	if h.Enabled {
		return "local hook evaluation reaches Cloud " + p.faint("(tool calls, and the conversation an agent runs a preflight check on, are sent for evaluation whatever the capture mode)")
	}
	return p.faint("not forwarded (" + h.Reason + ")")
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
	// Hook-only agents (cursor) are detected purely by PATH presence. Their
	// version is the agento11y binary's, so it is rendered exactly as the
	// report heading renders it.
	if a.HookBased {
		return agentNote(p, joinVersion("detected", a.Version), a.Note)
	}
	// The install probe never ran: a CLI-dependent agent whose binary is
	// absent, or a hook-only agent off PATH.
	if a.Health == HealthSkipped {
		return agentNote(p, p.faint("not found on PATH"), a.Note)
	}
	// The probe ran and errored, or the state was never set. Say the state is
	// unknown instead of picking one of the two known states; the note carries
	// the reason.
	install := a.Install.orUnknown()
	if install == InstallStateUnknown {
		return agentNote(p, "install state unknown", a.Note)
	}
	installed := install == InstallStateInstalled
	// Hook-file based agent (copilot, vibe): capture doesn't depend on the CLI being
	// on PATH, so report install state with its own wording and no PATH
	// qualifiers.
	if a.notInstalledLabel != "" {
		state := a.notInstalledLabel
		if installed {
			state = "installed"
		}
		return agentNote(p, joinAgent(state, a.Version), a.Note)
	}
	// Report install state, only claiming "on PATH" when true so config-based
	// agents installed without the CLI present aren't mislabeled.
	var state string
	switch {
	case installed && a.OnPath:
		state = "installed"
	case installed:
		state = "installed " + p.faint("(CLI not on PATH)")
	case a.OnPath:
		state = "on PATH, plugin not installed"
	default:
		state = "plugin not installed"
	}
	return agentNote(p, joinAgent(state, a.Version), a.Note)
}

// joinAgent appends a host agent's own version to its state, prefixed with
// `v` when it is a bare number. Some agents report a dist-tag instead of a
// number (opencode and pi return the tail of an npm spec, so a `@next`
// install reports `next`), and `vnext` would be wrong.
func joinAgent(state, version string) string {
	if version != "" && version[0] >= '0' && version[0] <= '9' {
		version = "v" + version
	}
	return joinVersion(state, version)
}

// joinVersion appends a version to a state as-is. The agento11y binary version
// goes through here, so the heading and cursor's line print the same string
// the build stamped, and the `v` prefix stays in the ldflags alone.
func joinVersion(state, version string) string {
	if version == "" {
		return state
	}
	return state + " " + version
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
