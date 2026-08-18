package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/grafana/agento11y/plugins/agento11y/internal/skills"
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

// renderHuman writes the colored (or plain) report.
func renderHuman(w io.Writer, r *Report, color bool) {
	p := palette{color: color}
	var b reportBody

	// Print the version as stamped, the way `agento11y --version` does. The
	// release ldflags already carry the `v` prefix.
	b.textf("%s %s\n\n", p.heading("agento11y doctor"), p.faint(r.Binary.Version))

	// Conversations pipeline.
	writeSection(&b, p, "Conversations (generation export)", r.Conversations.Health)
	b.kv("endpoint", describeEnv(p, r.Conversations.Endpoint))
	b.kv("tenant id", describeEnv(p, r.Conversations.TenantID))
	b.kv("auth token", describeToken(p, r.Conversations.Token))
	if r.Conversations.Probe != nil {
		b.kv("probe", describeProbeRow(p, r.Conversations.Probe))
	}
	writeMessages(&b, p, r.Conversations.Messages)
	b.textf("\n")

	// Analytics pipeline.
	writeSection(&b, p, "Analytics (OTLP metrics & traces)", r.Analytics.Health)
	b.kv("endpoint", describeEnv(p, r.Analytics.Endpoint))
	if r.Analytics.Probe != nil {
		if r.Analytics.Probe.Metrics != nil {
			b.kv("metrics probe", describeProbeRow(p, r.Analytics.Probe.Metrics))
		}
		if r.Analytics.Probe.Traces != nil {
			b.kv("traces probe", describeProbeRow(p, r.Analytics.Probe.Traces))
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
	b.kv("content capture", withTrailer(r.Config.ContentCaptureMode,
		describeSource(p, provenanceParts(r.Config.ContentModeKey, r.Config.ContentModeSource)...)))
	b.kv("prompt redaction", describeRedactInput(p, r.Config))
	b.kv("guards", describeGuards(p, r.Config))
	// Only printed when an override is set: with the family unset each adapter
	// reports its own product name, and there is no single value to show.
	if r.Config.AgentName != "" {
		b.kv("agent name", withTrailer(r.Config.AgentName,
			describeSource(p, provenanceParts(r.Config.AgentNameKey, r.Config.AgentNameSource)...)))
	}
	if len(r.Config.Tags) > 0 {
		b.kv("tags", withTrailer(formatTags(r.Config.Tags), describeSource(p, r.Config.TagsKey, r.Config.TagsSource)))
	}
	// The enabled names print even when nothing resolved, so a user who set the
	// switch can tell it was read. The values are the ones that would leave the
	// machine as metric labels; section messages name what did not resolve.
	if len(r.Config.AutoTagNames) > 0 {
		b.kv("auto tags", withTrailer(formatAutoTags(r.Config.AutoTagNames, r.Config.AutoTags),
			describeSource(p, r.Config.AutoTagsKey, r.Config.AutoTagsSource)))
	}
	b.kv("capture", describeCapture(p, r.Config.Capture))
	if r.Config.LocalForward.Set {
		b.kv("local forwarding", describeEnv(p, r.Config.LocalForward))
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
		b.kv("auto-update", withTrailer(p.faint("disabled"), describeSource(p, r.AutoUpdate.Key, r.AutoUpdate.Source)))
	}
	b.textf("\n")

	// Summary.
	writeSummary(&b, p, r)
	writeSetupHint(&b, p, r)

	_, _ = io.WriteString(w, b.flush(p))
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
	broken := brokenSections(r)
	if len(broken) == 0 {
		b.textf("%s %s\n", p.glyph(HealthOK), "no problems detected")
		return
	}
	b.textf("%s %d problem(s): %s misconfigured\n", p.glyph(HealthError), len(broken), strings.Join(broken, ", "))
}

// brokenSections names the sections in error. The agent list is advisory and
// never counts, matching Report.exitCode.
func brokenSections(r *Report) []string {
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
	return broken
}

// needsSetup reports whether this machine has setup left to do. Missing Cloud
// credentials need setup only when the resolved destination is not local.
func needsSetup(r *Report) bool {
	return len(brokenSections(r)) > 0 ||
		(!r.Conversations.configured() && r.Config.Capture.Destination != "local")
}

// writeSetupHint points the reader at the bundled setup skill. A machine with
// setup left to do gets the two-line block, because pasting it hands the work
// to the coding agent already in the terminal. A configured, healthy machine
// gets one faint line, because a paste block after every healthy check is
// noise.
func writeSetupHint(b *reportBody, p palette, r *Report) {
	b.textf("\n")
	if !needsSetup(r) {
		b.textf("%s\n", p.faint(skills.SetupCodingAgentOneLiner))
		return
	}
	// The paste line stays at full contrast, so only the introduction is faint.
	b.textf("%s\n", p.faint(skills.SetupCodingAgentHintIntro))
	b.textf("%s\n", skills.SetupCodingAgentPasteLine)
}

// describeSource renders the faint "(detail, KEY, source)" trailer every row
// shares. KEY is the env spelling that won, which is the one thing a row label
// cannot say (AGENTO11Y_* vs SIGIL_*, and the vendor-neutral
// OTEL_EXPORTER_OTLP_ENDPOINT). Empty parts are dropped, so a value assembled
// without a key still renders its source alone.
func describeSource(p palette, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, s := range parts {
		if s != "" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return p.faint("(" + strings.Join(kept, ", ") + ")")
}

// withTrailer joins a value and its trailer, and returns the value alone when
// there is nothing to qualify.
func withTrailer(value, trailer string) string {
	if trailer == "" {
		return value
	}
	return value + " " + trailer
}

// provenanceParts are the trailing "KEY, source" slots of a trailer. A row whose
// value came from a built-in default names no variable, so it reports `default`
// rather than leaving the reader unsure whether the user chose the value.
func provenanceParts(key, source string) []string {
	if key == "" {
		return []string{"default"}
	}
	return []string{key, source}
}

func describeEnv(p palette, v envValue) string {
	if !v.Set {
		return p.faint("not set")
	}
	return withTrailer(v.Value, describeSource(p, v.Key, v.Source))
}

func describeCapture(p palette, capture CaptureSection) string {
	return withTrailer(capture.Destination, describeSource(p, capture.Reason))
}

func describeToken(p palette, t tokenValue) string {
	if !t.Set {
		return p.faint("not set")
	}
	var prefix string
	if t.Prefix != "" {
		prefix = t.Prefix + "…"
	}
	return withTrailer("set", describeSource(p, prefix, t.Key, t.Source))
}

// describeRedactInput renders whether user prompts are scrubbed before export.
func describeRedactInput(p palette, c ConfigSection) string {
	out := p.faint("enabled")
	if !c.RedactInput {
		out = "disabled " + p.faint("(prompts export unredacted)")
	}
	if c.RedactInputFellBack {
		out += " " + p.faint("(invalid value, fell back)")
	}
	return out
}

// describeGuards renders the resolved guard feature flags. Guards default off,
// so a plain "disabled" is the common line; when on, the timeout and fail mode
// matter (fail-closed blocks the tool call when a guard errors or times out).
// The trailer names the GUARDS_ENABLED spelling alone. GUARDS_TIMEOUT_MS and
// GUARDS_FAIL_OPEN are separate families, and this row does not attribute them;
// an invalid value in either is named by a section message.
func describeGuards(p palette, c ConfigSection) string {
	trailer := describeSource(p, provenanceParts(c.GuardsKey, c.GuardsSource)...)
	if !c.GuardsEnabled {
		return withTrailer(p.faint("disabled"), trailer)
	}
	failMode := "fail-open"
	if !c.GuardsFailOpen {
		failMode = "fail-closed"
	}
	return withTrailer(fmt.Sprintf("enabled, timeout %dms, %s", c.GuardsTimeoutMs, failMode), trailer)
}

// describeLocalHookForward renders whether a --local session's guard checks
// reach Cloud, which is how a user learns their tool calls leave the machine
// even under a reduced content capture mode.
func describeLocalHookForward(p palette, h HookForwardSection) string {
	if h.Enabled {
		return withTrailer("forwarded to Cloud", p.faint("(tool calls, and the conversation an agent runs a preflight check on, are sent for evaluation whatever the capture mode)"))
	}
	return withTrailer(p.faint("not forwarded"), describeSource(p, h.Reason))
}

// describeProbeRow renders a probe as a row: the status plus the URL it probed,
// which is otherwise absent from the human report. A transport error has no
// status, and nothing answered, so the row reports "no response" against the
// same URL. The diagnosis is left to the section message line so it is printed
// once.
func describeProbeRow(p palette, res *ProbeResult) string {
	if res == nil {
		return p.faint("skipped")
	}
	return withTrailer(probeStatus(res), describeSource(p, res.URL))
}

// describeProbe renders a probe for message text, where the diagnosis is the
// point and no URL column exists to carry it.
func describeProbe(res *ProbeResult) string {
	if res == nil {
		return "skipped"
	}
	if res.Message != "" {
		return probeStatus(res) + ": " + res.Message
	}
	return probeStatus(res)
}

// probeStatus is the HTTP status, or "no response" for a transport error that
// never produced one.
func probeStatus(res *ProbeResult) string {
	if res.StatusCode == 0 {
		return "no response"
	}
	return fmt.Sprintf("HTTP %d", res.StatusCode)
}

func describeAgent(p palette, a AgentStatus) string {
	// Hook-only agents (cursor) are detected purely by PATH presence. Their
	// version is the agento11y binary's, so it is rendered exactly as the
	// report heading renders it.
	if a.HookBased {
		return agentNote(p, joinVersion("detected", a.Version), "", a.Note)
	}
	// The install probe never ran: a CLI-dependent agent whose binary is
	// absent, or a hook-only agent off PATH.
	if a.Health == HealthSkipped {
		return agentNote(p, p.faint("not found on PATH"), "", a.Note)
	}
	// The probe ran and errored, or the state was never set. Say the state is
	// unknown instead of picking one of the two known states; the note carries
	// the reason.
	install := a.Install.orUnknown()
	if install == InstallStateUnknown {
		return agentNote(p, "install state unknown", "", a.Note)
	}
	installed := install == InstallStateInstalled
	// A version belongs to an installed plugin. Suppress it otherwise so one
	// line can't report `plugin not installed` and a version at the same time.
	version := ""
	if installed {
		version = a.Version
	}
	// Hook-file based agent (copilot, vibe): capture doesn't depend on the CLI being
	// on PATH, so report install state with its own wording and no PATH
	// qualifiers.
	if a.notInstalledLabel != "" {
		state := a.notInstalledLabel
		if installed {
			state = "installed"
		}
		body, commit := joinAgent(state, version)
		return agentNote(p, body, commit, a.Note)
	}
	// Report install state, only claiming "on PATH" when true so config-based
	// agents installed without the CLI present aren't mislabeled. The missing-CLI
	// qualifier is a trailer part, so the version stays next to the state it
	// belongs to.
	var state, qualifier string
	switch {
	case installed && a.OnPath:
		state = "installed"
	case installed:
		state, qualifier = "installed", "CLI not on PATH"
	case a.OnPath:
		state = "on PATH, plugin not installed"
	default:
		state = "plugin not installed"
	}
	body, commit := joinAgent(state, version)
	return agentNote(p, body, commit, qualifier, a.Note)
}

// joinAgent appends a host agent plugin's own version to its state. A dotted
// number takes a `v` prefix, and a dist tag (`next`) is printed bare. A
// commit-shaped string is returned as a separate `commit <sha>` trailer part so
// it doesn't read as a version; the caller merges it with the row's other
// trailer parts, keeping one parenthesized group per row.
func joinAgent(state, version string) (body, commit string) {
	switch {
	case version == "":
		return state, ""
	case isCommitSHA(version):
		return state, "commit " + shortSHA(version)
	case version[0] >= '0' && version[0] <= '9' && strings.Contains(version, "."):
		return joinVersion(state, "v"+version), ""
	}
	return joinVersion(state, version), ""
}

// shaCutLength is git's default short-sha length: the shortest string doctor
// accepts as a sha, and how much of one it prints.
const shaCutLength = 7

// isCommitSHA reports whether a version string is a git commit sha rather than
// a version or a dist tag. Claude Code stores a short sha in the `version`
// field of its plugin store when the plugin manifest declares no version. A dot
// is not a hex digit, so a dotted version never matches.
func isCommitSHA(version string) bool {
	const hexDigits = "0123456789abcdefABCDEF"
	return len(version) >= shaCutLength && strings.TrimLeft(version, hexDigits) == ""
}

// shortSHA truncates a commit sha to the length git prints by default.
func shortSHA(sha string) string {
	return sha[:min(len(sha), shaCutLength)]
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

// agentNote appends an agent row's qualifier and note as one trailer, so a row
// that has both does not render two parenthesized groups.
func agentNote(p palette, body string, parts ...string) string {
	return withTrailer(body, describeSource(p, parts...))
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

// formatAutoTags renders the enabled auto-tag names and what they resolved to:
// "user,repo resolved user=alice@example.com, repo=grafana/agento11y". A name
// that resolved nothing appears on the left only, and the section message says
// its tag is omitted.
func formatAutoTags(names []string, tags map[string]string) string {
	enabled := strings.Join(names, ",")
	if len(tags) == 0 {
		return enabled + " (nothing resolved)"
	}
	return enabled + " resolved " + formatTags(tags)
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}
