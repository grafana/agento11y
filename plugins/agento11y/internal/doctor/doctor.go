// Package doctor implements `agento11y doctor`: a read-only diagnostic that
// reports the health of the two export pipelines (conversations and
// analytics), config validity, and installed host-agent plugins in one place.
//
// The command never installs, updates, or otherwise mutates host-agent
// plugin state, and never writes update-check stamps. It only reads.
//
// The conversations pipeline (generation export) and the analytics pipeline
// (OTLP metrics + traces) are independent: they use different endpoints and
// different token scopes. A user can have conversations working while
// analytics is silently dead because the OTLP endpoint is unset or the token
// lacks metrics:write/traces:write. Doctor surfaces both pipelines separately
// so that split is visible.
package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/autotag"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/updatecheck"
)

// Health is the per-section verdict. error is the only level that drives a
// non-zero exit code; warning is advisory (e.g. a host agent isn't installed)
// and ok/skipped are informational.
type Health string

const (
	HealthOK      Health = "ok"
	HealthWarn    Health = "warning"
	HealthError   Health = "error"
	HealthSkipped Health = "skipped"
)

// Value sources for a resolved env var.
const (
	sourceEnv    = "env"
	sourceConfig = "config.env"
)

// agentUserPlaceholder stands in for the auto-tag `user` value when USER_ID is
// unset. Doctor runs outside a session, so it cannot read the account a host
// agent signs in to, and printing the operating-system account name would name
// one possible outcome as the value.
const agentUserPlaceholder = "<depends on the agent>"

// trackedSuffixes are the branded alias families doctor attributes to a
// source (OS env vs config.env) under both their AGENTO11Y_* and SIGIL_*
// spellings. SnapshotEnv records their OS-env values before dotenv merge so
// Collect can tell where each effective value came from.
var trackedSuffixes = []string{
	"ENDPOINT",
	"INSECURE",
	"AUTH_TENANT_ID",
	"AUTH_TOKEN",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_AUTH_TOKEN",
	"CONTENT_CAPTURE_MODE",
	"REDACT_INPUT_MESSAGES",
	"AGENT_NAME",
	"TAGS",
	envconfig.AutoTagsSuffix,
	envconfig.AutoTagNamesSuffix,
	// USER_ID is read for the auto-tag `user` value only, so the report shows
	// the same identity the hooks resolve.
	"USER_ID",
	"AUTO_UPDATE",
	"LOCAL",
	"LOCAL_FORWARD",
	// The guard families are resolved from the snapshot, not from the process
	// env. A family missing here is never recorded by SnapshotEnv, so a
	// shell-exported value would read as unset while the hooks act on it. The
	// human row attributes GUARDS_ENABLED alone; the other two are read for
	// their effective values and to name an invalid one.
	"GUARDS_ENABLED",
	"GUARDS_FAIL_OPEN",
	"GUARDS_TIMEOUT_MS",
}

// trackedKeys is the full key set SnapshotEnv records: both spellings of the
// tracked families plus the standard (unbranded) OTel vars.
var trackedKeys = func() []string {
	keys := make([]string, 0, len(trackedSuffixes)*2+2)
	for _, suffix := range trackedSuffixes {
		keys = append(keys, envconfig.PreferredKey(suffix), envconfig.LegacyKey(suffix))
	}
	return append(keys, "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS")
}()

// Options are the parsed doctor flags.
type Options struct {
	JSON    bool
	NoColor bool
}

// Params carry the per-invocation inputs Run needs. OSEnv is the OS
// environment snapshot taken before dotenv was applied (see SnapshotEnv).
type Params struct {
	Version string
	OSEnv   map[string]string
	Stdout  io.Writer
	Stderr  io.Writer
}

// Report is the full diagnostic. Field names and the `status` strings are the
// stable contract for `--json` / support tooling.
type Report struct {
	Binary        BinarySection        `json:"agento11y"`
	Config        ConfigSection        `json:"config"`
	Conversations ConversationsSection `json:"conversations"`
	Analytics     AnalyticsSection     `json:"analytics"`
	Agents        []AgentStatus        `json:"agents"`
	// AutoUpdate is the resolved AUTO_UPDATE family. It carries provenance only:
	// updatecheck.Disabled() owns the verdict below, so the rule that decides
	// which values opt out stays in one place.
	AutoUpdate         envValue `json:"auto_update"`
	AutoUpdateDisabled bool     `json:"auto_update_disabled"`
}

// BinarySection reports the binary's build version.
type BinarySection struct {
	Version string `json:"version"`
}

// envValue is a non-secret resolved env var: endpoints and tenant IDs are safe
// to print, so Value is populated. Key is the spelling the value came from
// (AGENTO11Y_* or SIGIL_*); Conflict reports the two spellings disagree.
type envValue struct {
	Set      bool   `json:"set"`
	Value    string `json:"value,omitempty"`
	Source   string `json:"source,omitempty"`
	Key      string `json:"key,omitempty"`
	Conflict bool   `json:"conflict,omitempty"`
}

// tokenValue is a resolved secret. The value is never recorded; only presence,
// an optional non-sensitive scheme prefix (e.g. "glc_"), the selected key,
// and the conflict flag are.
type tokenValue struct {
	Set      bool   `json:"set"`
	Prefix   string `json:"prefix,omitempty"`
	Source   string `json:"source,omitempty"`
	Key      string `json:"key,omitempty"`
	Conflict bool   `json:"conflict,omitempty"`
}

// ConfigSection reports config.env validity and the resolved feature settings
// that every agent hook reads (content capture, guards, tags).
type ConfigSection struct {
	Path               string   `json:"path"`
	Exists             bool     `json:"exists"`
	DisallowedKeys     []string `json:"disallowed_keys,omitempty"`
	ContentCaptureMode string   `json:"content_capture_mode"`
	// ContentModeKey and ContentModeSource name where the effective mode came
	// from. They are empty when no variable supplied it, which includes the case
	// where a variable was set to a value envconfig rejected: the mode in force is
	// then the built-in one, and ContentModeFellBack plus a section message name
	// the variable to fix.
	ContentModeKey      string `json:"content_capture_key,omitempty"`
	ContentModeSource   string `json:"content_capture_source,omitempty"`
	ContentModeFellBack bool   `json:"content_mode_fell_back"`
	// RedactInput is the REDACT_INPUT_MESSAGES family: whether the user prompt is
	// scrubbed before export. RedactInputFellBack reports a rejected value, so
	// the reported state is the built-in default rather than what was set.
	RedactInput         bool `json:"redact_input_messages"`
	RedactInputFellBack bool `json:"redact_input_fell_back,omitempty"`
	GuardsEnabled       bool `json:"guards_enabled"`
	GuardsTimeoutMs     int  `json:"guards_timeout_ms"`
	GuardsFailOpen      bool `json:"guards_fail_open"`
	GuardsFellBack      bool `json:"guards_fell_back,omitempty"`
	// GuardsKey and GuardsSource name the GUARDS_ENABLED spelling that decided
	// whether guards run. The timeout and fail mode have their own families; the
	// human row reports one key, so this is the key it reports. Both are empty
	// when GUARDS_ENABLED supplied no usable value, so the row reports the
	// built-in default rather than crediting a rejected value.
	GuardsKey    string `json:"guards_key,omitempty"`
	GuardsSource string `json:"guards_source,omitempty"`
	// AgentName is the AGENT_NAME family: the value every adapter stamps on its
	// generations and guard requests instead of its own product name. Empty
	// means no override is set, so each adapter keeps its own name and the row
	// is left out.
	AgentName       string            `json:"agent_name,omitempty"`
	AgentNameKey    string            `json:"agent_name_key,omitempty"`
	AgentNameSource string            `json:"agent_name_source,omitempty"`
	Tags            map[string]string `json:"tags,omitempty"`
	TagsKey         string            `json:"tags_key,omitempty"`
	TagsSource      string            `json:"tags_source,omitempty"`
	// AutoTags are the client tags the AUTO_CODING_AGENT_TAGS family produces in
	// the directory doctor runs in, keyed by the tag key they are attached under.
	// Every one becomes a Prometheus label, and the user value is commonly an
	// email address, so the report prints them before they leave the machine. A
	// name listed in AutoTagShadowed or AutoTagUnresolved has no entry here.
	AutoTags map[string]string `json:"auto_tags,omitempty"`
	// AutoTagNames are the enabled names in AutoTagOrder, so the report can say
	// the switch is on even when nothing resolved.
	AutoTagNames []string `json:"auto_tag_names,omitempty"`
	// AutoTagUnresolved are enabled names that produced no value here (no git
	// checkout, no account name). Their tag is omitted from the export.
	AutoTagUnresolved []string `json:"auto_tag_unresolved,omitempty"`
	// AutoTagShadowed are enabled names whose tag key the TAGS family already
	// defines. The explicit tag wins, so the resolved value is not exported.
	AutoTagShadowed []string `json:"auto_tag_shadowed,omitempty"`
	// AutoTagUnknown are entries in the allowlist that name no supported value.
	// The hooks log and skip them.
	AutoTagUnknown []string `json:"auto_tag_unknown,omitempty"`
	// AutoTagsKey and AutoTagsSource attribute the on/off switch.
	// AutoTagNamesKey and AutoTagNamesSource attribute the allowlist that
	// narrowed the switch, and are empty when the session takes every name.
	AutoTagsKey        string `json:"auto_tags_key,omitempty"`
	AutoTagsSource     string `json:"auto_tags_source,omitempty"`
	AutoTagNamesKey    string `json:"auto_tag_names_key,omitempty"`
	AutoTagNamesSource string `json:"auto_tag_names_source,omitempty"`
	// Local is the resolved LOCAL family: whether `agento11y <agent>` starts in
	// local mode without --local, and where the value came from.
	Local envValue `json:"local"`
	// LocalInvalid reports that the LOCAL value is set to something outside the
	// boolean whitelist. The launcher ignores such a value, so the report has to
	// say the setting is not in effect.
	LocalInvalid bool `json:"local_invalid,omitempty"`
	// LocalForward is the resolved LOCAL_FORWARD family: whether a `--local`
	// daemon also forwards what it captures to Cloud, and where the value came
	// from. The daemon itself resolves config.env ahead of its own environment
	// (so a viewer edit takes effect without a restart), which is the opposite
	// of the shell-first precedence reported here; a conflict is called out in
	// Messages.
	LocalForward envValue `json:"local_forward"`
	// LocalHookForward is the combination users cannot infer from the two
	// settings above: whether a `--local` session's guard checks are relayed to
	// Cloud, which sends the content being evaluated whatever the capture mode
	// says. See handleHookEvaluate in internal/local/server.go.
	LocalHookForward HookForwardSection `json:"local_hook_forward"`
	Health           Health             `json:"status"`
	Messages         []string           `json:"messages,omitempty"`
}

// HookForwardSection is the resolved local-mode guard chaining posture. The
// gate itself lives in the local package so the daemon and this report cannot
// describe different rules.
//
// Its inputs are resolved config.env-first, unlike every other field here,
// because this line describes what the daemon does and the daemon prefers
// config.env over its own boot-time environment. When the two precedences
// disagree about the answer, ConfigSection.Messages says so.
type HookForwardSection struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// ConversationsSection reports the generation-export pipeline.
type ConversationsSection struct {
	Endpoint envValue     `json:"endpoint"`
	TenantID envValue     `json:"tenant_id"`
	Token    tokenValue   `json:"token"`
	Health   Health       `json:"status"`
	Messages []string     `json:"messages,omitempty"`
	Probe    *ProbeResult `json:"probe,omitempty"`
}

func (s ConversationsSection) configured() bool {
	return s.Endpoint.Set && s.TenantID.Set && s.Token.Set
}

// AnalyticsSection reports the OTLP metrics + traces pipeline.
type AnalyticsSection struct {
	Endpoint envValue        `json:"endpoint"`
	Health   Health          `json:"status"`
	Messages []string        `json:"messages,omitempty"`
	Probe    *AnalyticsProbe `json:"probe,omitempty"`
}

// InstallState is the outcome of an agent's install probe. It is tri-state
// because the probe can fail (an unreadable hook file, a CLI that errors):
// reporting such an agent as not installed would state a fact doctor never
// established.
type InstallState string

const (
	InstallStateInstalled    InstallState = "installed"
	InstallStateNotInstalled InstallState = "not_installed"
	InstallStateUnknown      InstallState = "unknown"
)

// orUnknown maps any value outside the domain, including the zero value, to
// unknown. A status built without the field must not read as a definite
// "not installed", which is the false negative the tri-state removes.
func (s InstallState) orUnknown() InstallState {
	switch s {
	case InstallStateInstalled, InstallStateNotInstalled, InstallStateUnknown:
		return s
	default:
		return InstallStateUnknown
	}
}

// MarshalJSON keeps the JSON contract inside the domain: the field is always
// one of the three states, never an empty string.
func (s InstallState) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(s.orUnknown()))
}

// AgentStatus reports one host agent's detection + install state. HookBased is
// set for agents the agento11y binary never installs (cursor): their capture is
// wired into the host's own hook settings, so install state isn't something
// doctor can read.
type AgentStatus struct {
	Name      string       `json:"name"`
	OnPath    bool         `json:"on_path"`
	Install   InstallState `json:"install_state"`
	HookBased bool         `json:"hook_based,omitempty"`
	// Version is the installed plugin's own build identifier, which for a plugin
	// whose manifest declares no version is the commit it was installed from.
	Version string `json:"version,omitempty"`
	Note    string `json:"note,omitempty"`
	Health  Health `json:"status"`

	// notInstalledLabel overrides the human "plugin not installed" wording for
	// non-plugin agents (copilot). Human-only; not part of the JSON contract.
	notInstalledLabel string
}

// ProbeResult is one HTTP probe outcome.
type ProbeResult struct {
	URL        string `json:"url,omitempty"`
	StatusCode int    `json:"status_code,omitempty"`
	OK         bool   `json:"ok"`
	Message    string `json:"message,omitempty"`
}

// AuthFailure reports a probe the endpoint answered with 401 or 403, which
// means the tenant and token were rejected as one Basic credential. It is
// exported so `agento11y login` classifies a result exactly as doctor does.
func (p *ProbeResult) AuthFailure() bool {
	return p.credentialsRejected() || p.scopeDenied()
}

// credentialsRejected reports HTTP 401: the endpoint refused the credentials.
// The status does not say which credential is wrong, so each probe names the
// candidates for its own endpoint.
func (p *ProbeResult) credentialsRejected() bool {
	return p != nil && p.StatusCode == 401
}

// scopeDenied reports HTTP 403: the credentials authenticated, but the token
// is not allowed to write.
func (p *ProbeResult) scopeDenied() bool {
	return p != nil && p.StatusCode == 403
}

// NoResponse reports a probe that never got an HTTP response at all: a DNS
// failure, a refused connection, or a timeout. Exported for the same reason
// as AuthFailure.
func (p *ProbeResult) NoResponse() bool {
	return p != nil && p.StatusCode == 0
}

// unreachable reports a probe outcome that means a configured pipeline can't
// deliver: a transport error (no HTTP response, e.g. DNS failure, connection
// refused, or timeout) or a 5xx server error. Every other failing status is
// classified on its own: 401 and 403 by AuthFailure, 3xx by redirected, 404 by
// routeMissing, and the rest by accepted. `agento11y login` is deliberately
// stricter: it reports every non-success status before it writes those values
// to disk.
func (p *ProbeResult) unreachable() bool {
	return p != nil && (p.NoResponse() || p.StatusCode >= 500)
}

// accepted reports the statuses that prove a pipeline can deliver. Ingest
// answers the export POST itself: the generation-export route answers 202 and
// the OTLP gateway answers 200. A 405 also counts, because a method-restricted
// route still proves the request reached the service and passed auth.
func (p *ProbeResult) accepted() bool {
	return p != nil && ((p.StatusCode >= 200 && p.StatusCode < 300) || p.StatusCode == http.StatusMethodNotAllowed)
}

// routeMissing reports a 404. The host answered, but the export route is not
// registered there, so the endpoint points at some other service. Every host
// serves a 404 for an unknown path, which is why a reachable endpoint that
// answers one is still a broken pipeline.
func (p *ProbeResult) routeMissing() bool {
	return p != nil && p.StatusCode == http.StatusNotFound
}

// redirected reports a 3xx, which means the URL points at something other than
// an ingest endpoint. See probeClient for why the probe never follows it.
func (p *ProbeResult) redirected() bool {
	return p != nil && p.StatusCode >= 300 && p.StatusCode < 400
}

// AnalyticsProbe holds the per-signal OTLP probe results.
type AnalyticsProbe struct {
	Metrics *ProbeResult `json:"metrics,omitempty"`
	Traces  *ProbeResult `json:"traces,omitempty"`
}

// Test seams. Production points at the default implementations; tests swap
// these to avoid shelling out or hitting the network.
var (
	collectAgents        = defaultCollectAgents
	probeConversationsFn = defaultProbeConversations
	probeOTLPFn          = defaultProbeOTLP
)

// SnapshotEnv records the OS-env values of the tracked keys. Call it before
// dotenv.ApplyEnv so Collect can attribute each effective value to the OS
// environment vs config.env.
func SnapshotEnv() map[string]string {
	m := make(map[string]string, len(trackedKeys))
	for _, k := range trackedKeys {
		if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
			m[k] = v
		}
	}
	return m
}

// Run parses flags, collects the report, renders it, and returns the exit
// code: 0 healthy, 1 when any section is broken, 2 on a flag error.
func Run(ctx context.Context, args []string, p Params) int {
	opts, err := parseFlags(args, p.Stderr)
	if err != nil {
		return 2
	}
	report := Collect(ctx, opts, p)
	if opts.JSON {
		if err := renderJSON(p.Stdout, report); err != nil {
			_, _ = fmt.Fprintf(p.Stderr, "agento11y: doctor: %v\n", err)
			return 2
		}
	} else {
		renderHuman(p.Stdout, report, !opts.NoColor)
	}
	return report.exitCode()
}

func parseFlags(args []string, stderr io.Writer) (Options, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: agento11y doctor [--json] [--no-color]")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Report the health of the conversations and analytics export pipelines,")
		_, _ = fmt.Fprintln(stderr, "config validity, and installed host-agent plugins.")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "  --json       emit a stable JSON report (for support tooling)")
		_, _ = fmt.Fprintln(stderr, "  --no-color   disable ANSI colors")
	}
	var opts Options
	fs.BoolVar(&opts.JSON, "json", false, "emit a JSON report")
	fs.BoolVar(&opts.NoColor, "no-color", false, "disable ANSI colors")
	// Probing is unconditional, so --probe and its --online alias do nothing.
	// They stay accepted, and out of the usage text, so the scripts and runbooks
	// that pass them keep working.
	var ignored bool
	fs.BoolVar(&ignored, "probe", false, "accepted for backwards compatibility; probes always run")
	fs.BoolVar(&ignored, "online", false, "accepted for backwards compatibility; probes always run")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return Options{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	return opts, nil
}

// Collect builds the report. It reads config.env and the env snapshot but
// performs no mutations, and it probes the configured endpoints over the
// network. A config-only verdict cannot tell a working endpoint from one that
// drops every export, so the probes are not optional.
func Collect(ctx context.Context, opts Options, p Params) *Report {
	fileEnv := dotenv.LoadDotenv(dotenv.FilePath(), nil)
	osEnv := p.OSEnv
	if osEnv == nil {
		osEnv = map[string]string{}
	}

	r := &Report{}
	r.Binary = BinarySection{Version: normalizeVersion(p.Version)}
	r.Conversations = collectConversations(osEnv, fileEnv)
	r.Analytics = collectAnalytics(osEnv, fileEnv, r.Conversations.configured())
	r.Config = collectConfig(osEnv, fileEnv)
	r.Agents = collectAgents(ctx, r.Binary.Version)
	r.AutoUpdate = resolveFamily("AUTO_UPDATE", osEnv, fileEnv).envValue()
	r.AutoUpdateDisabled = updatecheck.Disabled()

	runProbes(ctx, r, osEnv, fileEnv)
	return r
}

// exitCode is 1 when any pipeline or config section is broken, else 0. The
// agent section is informational and never fails the command.
func (r *Report) exitCode() int {
	healths := []Health{r.Conversations.Health, r.Analytics.Health, r.Config.Health}
	if slices.Contains(healths, HealthError) {
		return 1
	}
	return 0
}

// apiURLHint is the one correction every wrong-endpoint message ends with, so
// a user who pasted a stack URL or an app-page URL reads the same fix.
// Connection is what the app and the docs call the page that holds the URL.
const apiURLHint = "Copy the API URL from the Agent Observability app's Connection page " +
	"(e.g. https://agento11y-prod-<region>.grafana.net)."

// grafanaAppPath is where Grafana serves the Agent Observability app pages.
// login.go prints this URL after a successful login, so it is one of the two
// app URLs a user pastes into the endpoint variable.
const grafanaAppPath = "/a/grafana-agento11y-app"

// grafanaCloudDomains are the domains Grafana serves Cloud stacks and Agent
// Observability cells from. The host shape check applies only inside them: a
// self-hosted Sigil or a test collector can use any hostname, so warning about
// those would be noise.
var grafanaCloudDomains = []string{".grafana.net", ".grafana-dev.net", ".grafana-ops.net"}

// apiHostPrefixes are the first-label prefixes of every real API host:
// sigil-prod-<region>, agento11y-prod-<region>, sigil-dev-001,
// sigil-dev-<region>, agento11y-dev-001, sigil-ops-001, sigilai. A prefix
// check, not a region list, which would go stale with every new cell.
var apiHostPrefixes = []string{"sigil", "agento11y"}

// isGrafanaAppPage reports a path that serves Grafana app UI instead of the
// ingest API: the plugin pages under /plugins/, or the Agent Observability app
// under /a/grafana-agento11y-app. Both are URLs users copy out of the browser
// or out of this tool's own login output. The ingest API carries neither prefix
// on any host, so the export POST is redirected to /login and every generation
// is lost. A bare /a/ does not match, because a self-hosted deployment could
// legitimately sit under it.
func isGrafanaAppPage(path string) bool {
	if strings.Contains(path, "/plugins/") {
		return true
	}
	trimmed := strings.TrimRight(path, "/")
	return trimmed == grafanaAppPath || strings.HasPrefix(trimmed, grafanaAppPath+"/")
}

// endpointShapeMessage reports a normalized endpoint URL that does not look
// like an Agent Observability API URL. fatal marks the shape that cannot work
// on any host; the rest are warnings, because a hostname heuristic can go
// stale, and the probe escalates a warned endpoint when it really does
// redirect the export POST.
func endpointShapeMessage(key, target string) (msg string, fatal bool) {
	parsed, err := url.Parse(target)
	if err != nil {
		return "", false
	}
	// The path check ignores the host, so it also covers a stack on a custom
	// domain.
	if isGrafanaAppPage(parsed.Path) {
		return key + " points at a Grafana app page, not the Agent Observability API. " + apiURLHint, true
	}
	host := strings.ToLower(parsed.Hostname())
	if !slices.ContainsFunc(grafanaCloudDomains, func(d string) bool { return strings.HasSuffix(host, d) }) {
		return "", false
	}
	label, _, _ := strings.Cut(host, ".")
	if slices.ContainsFunc(apiHostPrefixes, func(p string) bool { return strings.HasPrefix(label, p) }) {
		return "", false
	}
	if strings.HasPrefix(label, "otlp-gateway-") {
		return key + " is the OTLP gateway, which takes metrics and traces, not conversations. " +
			"That value belongs in AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT. " + apiURLHint, false
	}
	return key + " does not look like an Agent Observability API URL: " + host +
		" is a Grafana Cloud stack or another Cloud service. " + apiURLHint, false
}

func collectConversations(osEnv, fileEnv map[string]string) ConversationsSection {
	endpoint := resolveFamily("ENDPOINT", osEnv, fileEnv)
	tenant := resolveFamily("AUTH_TENANT_ID", osEnv, fileEnv)
	token := resolveFamily("AUTH_TOKEN", osEnv, fileEnv)

	sec := ConversationsSection{
		Endpoint: endpoint.envValue(),
		TenantID: tenant.envValue(),
		Token:    token.tokenValue(),
	}
	switch boolToInt(endpoint.set) + boolToInt(tenant.set) + boolToInt(token.set) {
	case 3:
		sec.Health = HealthOK
	case 0:
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"not configured — run `agento11y login` or set AGENTO11Y_ENDPOINT, AGENTO11Y_AUTH_TENANT_ID and AGENTO11Y_AUTH_TOKEN")
	default:
		sec.Health = HealthError
		var missing []string
		if !endpoint.set {
			missing = append(missing, "AGENTO11Y_ENDPOINT")
		}
		if !tenant.set {
			missing = append(missing, "AGENTO11Y_AUTH_TENANT_ID")
		}
		if !token.set {
			missing = append(missing, "AGENTO11Y_AUTH_TOKEN")
		}
		sec.Messages = append(sec.Messages, "incomplete credentials; missing "+strings.Join(missing, ", "))
	}
	// A set endpoint still has to be a URL the exporter can build a request
	// from. Checking the shape here reports a malformed endpoint without waiting
	// on the network, and gives runProbes a reason to skip it.
	var target string
	if endpoint.set {
		normalized, err := wire.NormalizeGenerationExportURL(endpoint.value, resolveInsecure(osEnv, fileEnv))
		if err != nil {
			sec.Health = HealthError
			sec.Messages = append(sec.Messages, fmt.Sprintf("%s is not a usable endpoint: %v", endpoint.key, err))
		}
		target = normalized
	}
	// A usable URL can still be the wrong URL. Run the shape check only when
	// nothing else failed, so it cannot downgrade a missing-credentials or
	// malformed-endpoint error to a warning.
	if target != "" && sec.Health == HealthOK {
		if msg, fatal := endpointShapeMessage(endpoint.key, target); msg != "" {
			sec.Health = HealthWarn
			if fatal {
				sec.Health = HealthError
			}
			sec.Messages = append(sec.Messages, msg)
		}
	}
	for _, r := range []resolved{endpoint, tenant, token} {
		if r.conflict {
			sec.Messages = append(sec.Messages, conflictMessage(r))
		}
	}
	if endpoint.legacyWon() || tenant.legacyWon() || token.legacyWon() {
		sec.Messages = append(sec.Messages,
			"configured via legacy SIGIL_* names — these keep working, but the preferred names are AGENTO11Y_*")
	}
	return sec
}

func collectAnalytics(osEnv, fileEnv map[string]string, conversationsConfigured bool) AnalyticsSection {
	brandedOTLP := resolveFamily("OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)
	stdOTLP := resolveEnv("OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)

	sec := AnalyticsSection{}
	switch {
	case brandedOTLP.set:
		sec.Endpoint = brandedOTLP.envValue()
	case stdOTLP.set:
		sec.Endpoint = stdOTLP.envValue()
	case conversationsConfigured:
		// The headline failure: conversations export fine, but analytics
		// (the Agent Observability page) stays empty because no OTLP endpoint
		// is configured. dotenv.HasCredentials passes here, so nothing else
		// the binary does today would flag this.
		sec.Health = HealthError
		sec.Messages = append(sec.Messages,
			"no OTLP endpoint set — metrics and traces will not be exported even though conversations are configured. "+
				"Set AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT (e.g. https://otlp-gateway-prod-<region>.grafana.net/otlp).")
		return sec
	default:
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, "no OTLP endpoint set; analytics export is disabled")
		return sec
	}
	if brandedOTLP.conflict {
		sec.Messages = append(sec.Messages, conflictMessage(brandedOTLP))
	}
	if brandedOTLP.legacyWon() {
		sec.Messages = append(sec.Messages,
			"configured via legacy SIGIL_* names — these keep working, but the preferred names are AGENTO11Y_*")
	}

	// The endpoint is set, but OTLP export still needs auth (unless the
	// collector is open). Mirror internal/otel: auth is an explicit
	// Authorization entry in OTEL_EXPORTER_OTLP_HEADERS, or synthesized from
	// the AUTH_TENANT_ID family + (the OTEL_AUTH_TOKEN family or the
	// AUTH_TOKEN family). Don't report ok when none of those resolve,
	// otherwise doctor shows a healthy analytics pipeline that exports
	// nothing.
	if analyticsAuthResolvable(osEnv, fileEnv) {
		sec.Health = HealthOK
	} else {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"OTLP endpoint set but no auth resolved — set AGENTO11Y_AUTH_TENANT_ID and AGENTO11Y_OTEL_AUTH_TOKEN "+
				"(or AGENTO11Y_AUTH_TOKEN), or an Authorization entry in OTEL_EXPORTER_OTLP_HEADERS. "+
				"Export will be unauthenticated unless the collector is open.")
	}
	return sec
}

// analyticsAuthResolvable reports whether the OTLP exporter would have a
// credential: an explicit Authorization header, or a tenant + token pair that
// internal/otel turns into Basic auth. It does not validate the credential —
// the probe does that against the live endpoint.
func analyticsAuthResolvable(osEnv, fileEnv map[string]string) bool {
	if headersHaveAuthorization(resolveEnv("OTEL_EXPORTER_OTLP_HEADERS", osEnv, fileEnv).value) {
		return true
	}
	tenant := resolveFamily("AUTH_TENANT_ID", osEnv, fileEnv)
	token := resolveFamily("OTEL_AUTH_TOKEN", osEnv, fileEnv)
	if !token.set {
		token = resolveFamily("AUTH_TOKEN", osEnv, fileEnv)
	}
	return tenant.set && token.set
}

// headersHaveAuthorization parses the OTEL_EXPORTER_OTLP_HEADERS value
// (comma-separated key=value pairs) and reports whether it carries an
// Authorization entry with a non-empty value, matching how internal/otel
// parses headers: it drops pairs whose key or value is empty after trimming,
// so an Authorization with no value exports unauthenticated.
func headersHaveAuthorization(raw string) bool {
	for pair := range strings.SplitSeq(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(key), "Authorization") && strings.TrimSpace(value) != "" {
			return true
		}
	}
	return false
}

func collectConfig(osEnv, fileEnv map[string]string) ConfigSection {
	path := dotenv.FilePath()
	sec := ConfigSection{Path: path, Health: HealthOK}
	if _, err := os.Stat(path); err == nil {
		sec.Exists = true
	}
	sec.DisallowedKeys = disallowedKeys(path)

	// ResolveContentModeValue logs a line when it falls back from an invalid
	// value, so a capturing logger doubles as the fell-back signal.
	contentMode := resolveFamily("CONTENT_CAPTURE_MODE", osEnv, fileEnv)
	var buf bytes.Buffer
	mode := envconfig.ResolveContentModeValue(log.New(&buf, "", 0), contentMode.key, contentMode.value)
	sec.ContentCaptureMode = mode.String()
	sec.ContentModeFellBack = buf.Len() > 0
	// A rejected value did not supply the mode in force, so the row credits no
	// variable for it and reports the built-in default instead.
	if !sec.ContentModeFellBack {
		sec.ContentModeKey = contentMode.key
		sec.ContentModeSource = contentMode.source
	}

	// snapshotLookup resolves an alias family from the pre-merge snapshot every
	// row is attributed with, so the values reported here are the ones the hooks
	// read rather than whatever the dotenv merge left in this process.
	snapshotLookup := func(suffix string) (value, key string, ok bool) {
		r := resolveFamily(suffix, osEnv, fileEnv)
		return r.value, r.key, r.set
	}

	// The hooks resolve this flag against a discarded logger unless
	// AGENTO11Y_DEBUG is set, so doctor is where a rejected value becomes
	// visible. Same capturing-logger trick as content mode.
	var redactBuf bytes.Buffer
	sec.RedactInput = envconfig.ResolveRedactInputWith(log.New(&redactBuf, "", 0), snapshotLookup)
	sec.RedactInputFellBack = redactBuf.Len() > 0

	// Guards are the shared pre-tool-call enforcement flags every agent hook
	// reads via envconfig.ResolveGuards. They default off, so surface the
	// effective values to confirm whether guards actually run and with what
	// timeout / fail mode. The lookup resolves them from the same pre-merge
	// snapshot every other row is attributed with, while the parsing and the
	// defaults stay in envconfig, shared with the hooks. No logger is needed:
	// invalidGuardKeys names the rejected variables.
	guardsEnabled := resolveFamily("GUARDS_ENABLED", osEnv, fileEnv)
	guardsTimeout := resolveFamily("GUARDS_TIMEOUT_MS", osEnv, fileEnv)
	guardsFailOpen := resolveFamily("GUARDS_FAIL_OPEN", osEnv, fileEnv)
	guards := envconfig.ResolveGuardsWith(nil, snapshotLookup)
	sec.GuardsEnabled = guards.Enabled
	sec.GuardsTimeoutMs = guards.TimeoutMs
	sec.GuardsFailOpen = guards.FailOpen
	invalidGuards := invalidGuardKeys(guardsEnabled, guardsTimeout, guardsFailOpen)
	sec.GuardsFellBack = len(invalidGuards) > 0
	// Same rule as the mode above: a rejected GUARDS_ENABLED value did not decide
	// whether guards run, so the row names no variable for it.
	if _, ok := envconfig.ParseBoolValue(guardsEnabled.value); ok {
		sec.GuardsKey = guardsEnabled.key
		sec.GuardsSource = guardsEnabled.source
	}

	// The AGENT_NAME family renames the agent every hook reports. A rule or
	// dashboard filtered on the product name stops matching once it is set, so
	// the effective value and its source have to be visible here.
	agentName := resolveFamily("AGENT_NAME", osEnv, fileEnv)
	if agentName.set {
		sec.AgentName = agentName.value
		sec.AgentNameKey = agentName.key
		sec.AgentNameSource = agentName.source
	}

	// The TAGS family attaches key=value tags to every generation. They
	// aren't secret, so surface the resolved set (and where it came from) to
	// make a mis-set or forgotten tag visible.
	tags := resolveFamily("TAGS", osEnv, fileEnv)
	if tags.set {
		if parsed := envconfig.ParseExtraTags(tags.value); len(parsed) > 0 {
			sec.Tags = parsed
			sec.TagsKey = tags.key
			sec.TagsSource = tags.source
		}
	}

	// AUTO_CODING_AGENT_TAGS resolves the session's user, repository, and branch
	// and attaches them as client tags, which become metric labels. The values
	// are resolved here, in the directory doctor runs in, so a user can read the
	// exact strings — a work email, a private repository name — before enabling
	// the switch for real sessions.
	//
	// The same pre-merge snapshot every other row is attributed with backs the
	// lookup, so the switch, the allowlist, the reported user, and the explicit
	// tags all match what the hooks read.
	autoTags := resolveFamily(envconfig.AutoTagsSuffix, osEnv, fileEnv)
	autoTagNames := resolveFamily(envconfig.AutoTagNamesSuffix, osEnv, fileEnv)
	userID := resolveFamily("USER_ID", osEnv, fileEnv)
	// A switch value that is not a boolean leaves the mechanism off, so name the
	// variable to fix rather than reporting silence.
	_, autoTagsValid := envconfig.ParseBoolValue(autoTags.value)
	autoSel := autotag.Select(snapshotLookup, nil)
	sec.AutoTagUnknown = autoSel.Unknown
	if len(autoSel.Enabled) > 0 {
		sec.AutoTagsKey = autoTags.key
		sec.AutoTagsSource = autoTags.source
		if autoSel.NamesSet {
			sec.AutoTagNamesKey = autoTagNames.key
			sec.AutoTagNamesSource = autoTagNames.source
		}
		// `user` is the one name doctor cannot resolve the way a session will.
		// With USER_ID unset, Claude Code and Cursor attach the account they are
		// signed in to, which doctor has no hook payload to read, and only an
		// agent that supplies no identity falls back to the operating-system
		// account name. Feeding a placeholder through the same tier the adapters
		// fill keeps the row from presenting one of those outcomes as the value;
		// the message below names both.
		in := autotag.Inputs{Lookup: snapshotLookup}
		if !userID.set {
			in.UserID = agentUserPlaceholder
		}
		res := autotag.Describe(autoSel.Enabled, in)
		// The row reports the tags that reach the client, so a shadowed name
		// shows no value: the explicit tag supplies it, and the message below
		// says so.
		sec.AutoTags = res.Tags
		for _, name := range envconfig.AutoTagOrder {
			if !autoSel.Enabled[name] {
				continue
			}
			sec.AutoTagNames = append(sec.AutoTagNames, string(name))
			if _, ok := res.Values[name]; !ok {
				sec.AutoTagUnresolved = append(sec.AutoTagUnresolved, string(name))
			}
		}
		for _, name := range res.Shadowed {
			sec.AutoTagShadowed = append(sec.AutoTagShadowed, string(name))
		}
	}

	// LOCAL turns every launcher run into a local session, so report the
	// effective value and where it came from.
	localMode := resolveFamily("LOCAL", osEnv, fileEnv)
	localOn, localValid := envconfig.ParseBoolValue(localMode.value)
	sec.Local = localMode.envValue()
	sec.LocalInvalid = localMode.set && !localValid

	// LOCAL_FORWARD sends a copy of every --local session to Cloud, so make the
	// effective value and its source visible.
	forward := resolveFamily("LOCAL_FORWARD", osEnv, fileEnv)
	sec.LocalForward = forward.envValue()

	// Guard chaining needs the forwarding opt-in, guards, a Cloud endpoint,
	// and real credentials. Each of those is reported separately above and in
	// the conversations section; the combination is what decides whether tool
	// calls from a --local session are sent to Cloud, so state it outright.
	//
	// The daemon owns this decision and resolves config.env ahead of its own
	// environment, so the inputs are resolved that way here too. Reporting the
	// shell-first answer would let doctor print "not forwarded" while the daemon
	// relays every tool call.
	hookReason := hookForwardReason(daemonFamily, osEnv, fileEnv)
	sec.LocalHookForward = HookForwardSection{Enabled: hookReason == "", Reason: hookReason}
	if shellReason := hookForwardReason(resolveFamily, osEnv, fileEnv); (shellReason == "") != (hookReason == "") {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"config.env and the environment disagree about local guard chaining; the line above reports the daemon's answer, which prefers config.env")
	}

	if len(sec.DisallowedKeys) > 0 {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"config.env has keys agento11y ignores: "+strings.Join(sec.DisallowedKeys, ", "))
	}
	if sec.ContentModeFellBack {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			fmt.Sprintf("the %s value is invalid; using %s", contentMode.key, mode))
	}
	if sec.RedactInputFellBack {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"the REDACT_INPUT_MESSAGES value is invalid; prompt redaction stays on")
	}
	// One message per rejected variable: the row names the GUARDS_ENABLED
	// spelling alone, so this is the only place a reader learns which guard value
	// is broken.
	for _, key := range invalidGuards {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			fmt.Sprintf("the %s value is invalid; guards use the default", key))
	}
	if agentName.conflict {
		sec.Messages = append(sec.Messages, conflictMessage(agentName))
	}
	if agentName.legacyWon() {
		sec.Messages = append(sec.Messages,
			"agent name set via legacy SIGIL_AGENT_NAME — this keeps working, but the preferred name is AGENTO11Y_AGENT_NAME")
	}
	// A slash separates a base name from a subagent suffix, so a name that
	// contains one makes every generation of the run read as a subagent step.
	if strings.Contains(agentName.value, "/") {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"the %s value %q contains a slash; a slash marks a subagent generation, so every generation of this run is counted as a subagent",
			agentName.key, agentName.value))
	}
	if tags.conflict {
		sec.Messages = append(sec.Messages, conflictMessage(tags))
	}
	if tags.legacyWon() {
		sec.Messages = append(sec.Messages,
			"tags set via legacy SIGIL_TAGS — this keeps working, but the preferred name is AGENTO11Y_TAGS")
	}
	// The auto-tag row lists what resolved. These messages cover what did not,
	// which the row cannot show: an allowlist entry the parser rejected, an
	// allowlist that turns nothing on, a list set while the switch is off, a name
	// that found no value in this directory, and a name an explicit tag
	// overrides.
	if len(sec.AutoTagUnknown) > 0 {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s has unsupported names %s; supported: %s",
			autoTagNames.key, strings.Join(sec.AutoTagUnknown, ", "), autotag.SupportedNames()))
	}
	if autoSel.On && len(autoSel.Enabled) == 0 {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s names no supported value, so no automatic tags are attached", autoTagNames.key))
	}
	if !autoSel.On && autoSel.NamesSet {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s is set but %s is off, so no automatic tags are attached",
			autoTagNames.key, envconfig.PreferredKey(envconfig.AutoTagsSuffix)))
	}
	if autoTags.set && !autoTagsValid {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"the %s value is not a boolean; automatic tags stay off, and the names go in %s",
			autoTags.key, envconfig.PreferredKey(envconfig.AutoTagNamesSuffix)))
	}
	if len(sec.AutoTagUnresolved) > 0 {
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"these auto tags resolved no value in this directory and are left off: %s",
			strings.Join(sec.AutoTagUnresolved, ", ")))
	}
	if len(sec.AutoTagShadowed) > 0 {
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"these auto tags are also set in %s, which wins: %s",
			tags.key, strings.Join(sec.AutoTagShadowed, ", ")))
	}
	// The placeholder is only in the row when `user` is enabled, USER_ID is
	// unset, and no explicit tag shadows the name, which is exactly when the
	// value the session attaches depends on which agent runs it.
	if sec.AutoTags[autotag.KeyUser] == agentUserPlaceholder {
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"the auto tag user depends on the coding agent: Claude Code and Cursor attach the account they are signed in to, and the others attach the operating-system account name; set %s to pin one value",
			envconfig.PreferredKey("USER_ID")))
	}
	if autoTags.conflict {
		sec.Messages = append(sec.Messages, conflictMessage(autoTags))
	}
	if autoTagNames.conflict {
		sec.Messages = append(sec.Messages, conflictMessage(autoTagNames))
	}
	if forward.set && forward.source == sourceEnv && strings.TrimSpace(fileEnv[envconfig.PreferredKey("LOCAL_FORWARD")]+fileEnv[envconfig.LegacyKey("LOCAL_FORWARD")]) != "" {
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s is set in the environment and in config.env; the local daemon uses the config.env value", forward.key))
	}
	if sec.LocalInvalid {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"the %s value is not a boolean; local mode stays off", localMode.key))
	}
	if localMode.legacyWon() {
		sec.Messages = append(sec.Messages,
			"local mode set via legacy SIGIL_LOCAL — this keeps working, but the preferred name is AGENTO11Y_LOCAL")
	}
	if localOn {
		// Launchers and hooks both read this family, but LOCAL_FORWARD still
		// copies local captures to Cloud, so "nothing leaves this machine" is
		// still the wrong reading.
		sec.Messages = append(sec.Messages,
			"local mode sends `agento11y <agent>` launches and agento11y hooks to the local viewer; Cloud forwarding still requires AGENTO11Y_LOCAL_FORWARD")
	}
	return sec
}

// invalidGuardKeys names the GUARDS_* variables whose values envconfig rejects,
// in the order the guards row prints them. The envconfig parsers the hooks
// resolve with decide validity, so doctor cannot call a value good that a hook
// throws away. The row attributes GUARDS_ENABLED alone, so these names are the
// only place a reader learns which guard value is broken.
func invalidGuardKeys(enabled, timeout, failOpen resolved) []string {
	isBool := func(raw string) bool { _, ok := envconfig.ParseBoolValue(raw); return ok }
	isTimeout := func(raw string) bool { _, ok := envconfig.ParseIntValue(raw); return ok }

	var keys []string
	for _, family := range []struct {
		value resolved
		valid func(string) bool
	}{
		{enabled, isBool},
		{timeout, isTimeout},
		{failOpen, isBool},
	} {
		if family.value.set && !family.valid(family.value.value) {
			keys = append(keys, family.value.key)
		}
	}
	return keys
}

// hookForwardReason resolves the local guard-chaining gate with the given
// precedence, so the same inputs can be read the daemon's way (config.env
// first) and the shell's way and compared.
func hookForwardReason(resolve func(suffix string, osEnv, fileEnv map[string]string) resolved, osEnv, fileEnv map[string]string) string {
	forward := resolve("LOCAL_FORWARD", osEnv, fileEnv)
	guards := resolve("GUARDS_ENABLED", osEnv, fileEnv)
	return local.HookForwardReason(
		forward.set && envconfig.ParseBool(forward.value),
		guards.set && envconfig.ParseBool(guards.value),
		resolve("ENDPOINT", osEnv, fileEnv).value,
		resolve("AUTH_TENANT_ID", osEnv, fileEnv).value,
		resolve("AUTH_TOKEN", osEnv, fileEnv).value,
	)
}

// resolveInsecure reads the INSECURE flag, which picks http over https for a
// scheme-less endpoint. The offline endpoint check and the live probe resolve
// it here so they cannot disagree about the same endpoint.
func resolveInsecure(osEnv, fileEnv map[string]string) bool {
	return envconfig.ParseBool(resolveFamily("INSECURE", osEnv, fileEnv).value)
}

// endpointKey is the endpoint variable spelling to name in a message. The
// resolved key wins, so a SIGIL_* setup reads its own name back; the preferred
// spelling is the fallback when the report carries no key.
func endpointKey(v envValue) string {
	if v.Key != "" {
		return v.Key
	}
	return envconfig.PreferredKey("ENDPOINT")
}

func runProbes(ctx context.Context, r *Report, osEnv, fileEnv map[string]string) {
	// An endpoint the offline check already rejected has nothing to probe:
	// probing it would report the same fault a second and third time. Inside
	// configured(), HealthError means a malformed endpoint or an app-page path,
	// and probing would report that fault again.
	if r.Conversations.configured() && r.Conversations.Health != HealthError {
		token := resolveFamily("AUTH_TOKEN", osEnv, fileEnv).value
		res := probeConversationsFn(ctx, r.Conversations.Endpoint.Value, r.Conversations.TenantID, token, resolveInsecure(osEnv, fileEnv))
		r.Conversations.Probe = res
		switch {
		case res.AuthFailure():
			r.Conversations.Health = HealthError
			// The row shows the status and the URL, so the diagnosis (bad token,
			// wrong tenant id, missing write scope) is carried here or nowhere in
			// the human report.
			r.Conversations.Messages = append(r.Conversations.Messages,
				"the conversations endpoint rejected the export request: "+describeProbe(res))
		case res.redirected():
			r.Conversations.Health = HealthError
			r.Conversations.Messages = append(r.Conversations.Messages,
				"the conversations endpoint redirected the export request ("+describeProbe(res)+"), so "+
					endpointKey(r.Conversations.Endpoint)+" is not an Agent Observability API URL. "+apiURLHint)
		case res.unreachable():
			r.Conversations.Health = HealthError
			r.Conversations.Messages = append(r.Conversations.Messages,
				"could not reach the conversations endpoint: "+describeProbe(res))
		case res.routeMissing():
			r.Conversations.Health = HealthError
			r.Conversations.Messages = append(r.Conversations.Messages,
				"the conversations endpoint has no generation-export route (HTTP 404), so "+
					endpointKey(r.Conversations.Endpoint)+" is not an Agent Observability API URL. "+apiURLHint)
		case !res.accepted():
			// A 400 or 415 can come from an endpoint that validates the body
			// before auth, and the probe posts a minimal {}. Warn rather than
			// fail, but never call it healthy: the real route answers 202.
			if r.Conversations.Health == HealthOK {
				r.Conversations.Health = HealthWarn
			}
			r.Conversations.Messages = append(r.Conversations.Messages,
				"the conversations endpoint answered "+describeProbe(res)+" instead of HTTP 202; exports may be dropped")
		}
	}
	if r.Analytics.Endpoint.Set {
		probe := probeOTLPFn(ctx, otlpProbeTenant(r.Conversations.TenantID, osEnv, fileEnv))
		r.Analytics.Probe = probe
		if probe != nil {
			switch {
			case probe.Metrics.AuthFailure() || probe.Traces.AuthFailure():
				r.Analytics.Health = HealthError
				r.Analytics.Messages = append(r.Analytics.Messages, otlpAuthMessages(probe)...)
			case probe.Metrics.redirected() || probe.Traces.redirected():
				r.Analytics.Health = HealthError
				r.Analytics.Messages = append(r.Analytics.Messages,
					"the OTLP endpoint redirected the export request, so it is not an OTLP ingest URL "+
						"(e.g. https://otlp-gateway-prod-<region>.grafana.net/otlp)")
			case probe.Metrics.unreachable() || probe.Traces.unreachable():
				r.Analytics.Health = HealthError
				// The row shows the status and the URL, so the cause (DNS failure,
				// refused connection, TLS error, timeout, 5xx body) is carried here or
				// nowhere in the human report.
				r.Analytics.Messages = append(r.Analytics.Messages,
					"could not reach the OTLP endpoint: "+describeProbe(firstUnreachable(probe))+
						"; metrics and traces will not be exported")
			case probe.Metrics.routeMissing() || probe.Traces.routeMissing():
				r.Analytics.Health = HealthError
				r.Analytics.Messages = append(r.Analytics.Messages,
					"the OTLP endpoint has no ingest route (HTTP 404), so it is not an OTLP ingest URL "+
						"(e.g. https://otlp-gateway-prod-<region>.grafana.net/otlp)")
			case !probe.Metrics.accepted() || !probe.Traces.accepted():
				// The probe posts JSON where the exporter posts protobuf, so a
				// collector that parses before it authenticates can answer 400 or
				// 415 while real exports still land. Warn, don't fail.
				if r.Analytics.Health == HealthOK {
					r.Analytics.Health = HealthWarn
				}
				r.Analytics.Messages = append(r.Analytics.Messages,
					"the OTLP endpoint did not accept the probe; the probe posts JSON where the exporter posts "+
						"protobuf, so this can be benign")
			}
		}
	}
}

// otlpProbeTenant is the tenant id the OTLP request authenticates with, or a
// zero value when it authenticates with something else. internal/otel builds
// Basic auth from the tenant id only when OTEL_EXPORTER_OTLP_HEADERS carries
// no Authorization entry (otel.ExporterHeaders); with an explicit header the
// tenant id never reaches the request, so a 401 must not point at it.
func otlpProbeTenant(tenant envValue, osEnv, fileEnv map[string]string) envValue {
	if headersHaveAuthorization(resolveEnv("OTEL_EXPORTER_OTLP_HEADERS", osEnv, fileEnv).value) {
		return envValue{}
	}
	return tenant
}

// otlpAuthMessages diagnoses the OTLP signals whose auth failed. The rows show
// the status and the URL, so the diagnosis (bad token, wrong tenant id, missing
// write scope) is carried here or nowhere in the human report. Metrics and
// traces authenticate with the same credentials, so a failure both share is
// described once; each signal gets its own line only when the two failed
// differently.
func otlpAuthMessages(p *AnalyticsProbe) []string {
	rejected := func(subject string, res *ProbeResult) string {
		return "the OTLP endpoint rejected the " + subject + ": " + describeProbe(res)
	}
	if p.Metrics.AuthFailure() && p.Traces.AuthFailure() &&
		p.Metrics.StatusCode == p.Traces.StatusCode && p.Metrics.Message == p.Traces.Message {
		return []string{rejected("metrics and traces exports", p.Metrics)}
	}
	var msgs []string
	for _, signal := range []struct {
		name string
		res  *ProbeResult
	}{{"metrics", p.Metrics}, {"traces", p.Traces}} {
		if signal.res.AuthFailure() {
			msgs = append(msgs, rejected(signal.name+" export", signal.res))
		}
	}
	return msgs
}

// firstUnreachable returns the signal probe that could not deliver, so the
// message names one concrete failure instead of both.
func firstUnreachable(p *AnalyticsProbe) *ProbeResult {
	if p.Metrics.unreachable() {
		return p.Metrics
	}
	return p.Traces
}

// resolved is the effective value of an env var plus where it came from:
// key is the spelling that won (AGENTO11Y_* or SIGIL_*), and conflict reports
// that the other spelling also resolves — to a different value — under the
// same source precedence. otherKey and otherSource describe that losing
// spelling, so a message can name the variable to edit instead of calling it
// "the other spelling"; both are empty unless conflict is set.
type resolved struct {
	set         bool
	value       string
	source      string
	key         string
	conflict    bool
	otherKey    string
	otherSource string
}

func (r resolved) envValue() envValue {
	return envValue{Set: r.set, Value: r.value, Source: r.source, Key: r.key, Conflict: r.conflict}
}

func (r resolved) tokenValue() tokenValue {
	return tokenValue{Set: r.set, Prefix: tokenPrefix(r.value), Source: r.source, Key: r.key, Conflict: r.conflict}
}

// legacyWon reports that the value came from the legacy SIGIL_* spelling.
func (r resolved) legacyWon() bool {
	return r.set && strings.HasPrefix(r.key, "SIGIL_")
}

// conflictMessage describes an alias family whose two spellings hold different
// values: which variable each value came from, which one is in force, and
// which one to delete. Both spellings name the same setting, so a reader who
// is only told the winner cannot tell what the other value even was.
func conflictMessage(r resolved) string {
	var b strings.Builder
	if r.source == r.otherSource {
		fmt.Fprintf(&b, "%s and %s are both set in %s, to different values; using %s",
			r.key, r.otherKey, sourceLabel(r.source), r.key)
	} else {
		fmt.Fprintf(&b, "%s is set in %s and %s in %s, to different values; using %s",
			r.key, sourceLabel(r.source), r.otherKey, sourceLabel(r.otherSource), r.key)
	}
	if r.legacyWon() {
		// The preferred spelling loses only across sources: resolveFamily picks it
		// whenever both spellings come from the same place.
		b.WriteString(", because the environment outranks config.env")
	}
	legacyKey, legacySource := r.key, r.source
	if !r.legacyWon() {
		legacyKey, legacySource = r.otherKey, r.otherSource
	}
	fmt.Fprintf(&b, ". %s is the old name for the same setting: %s.", legacyKey, clearHint(legacySource))
	return b.String()
}

// clearHint names the action that clears a variable, which depends on where it
// is set: an exported variable is unset, a config.env line is deleted.
func clearHint(source string) string {
	if source == sourceConfig {
		return "remove it from config.env"
	}
	return "unset it"
}

// sourceLabel spells a source for prose. The rows print the bare `env` and
// `config.env` labels, which a sentence cannot reuse as-is.
func sourceLabel(source string) string {
	if source == sourceConfig {
		return "config.env"
	}
	return "the environment"
}

// resolveEnv mirrors dotenv.ApplyEnv precedence for one exact key: a
// non-empty OS-env value wins over config.env. The OS-env snapshot must
// predate the dotenv merge.
func resolveEnv(key string, osEnv, fileEnv map[string]string) resolved {
	if v, ok := osEnv[key]; ok && strings.TrimSpace(v) != "" {
		return resolved{set: true, value: strings.TrimSpace(v), source: sourceEnv, key: key}
	}
	if v, ok := fileEnv[key]; ok && strings.TrimSpace(v) != "" {
		return resolved{set: true, value: strings.TrimSpace(v), source: sourceConfig, key: key}
	}
	return resolved{}
}

// resolveFamily mirrors dotenv.ApplyEnv's alias-family precedence — shell
// preferred > shell legacy > file preferred > file legacy — and reports the
// selected key, its source, and whether the two spellings disagree.
func resolveFamily(suffix string, osEnv, fileEnv map[string]string) resolved {
	preferred := resolveEnv(envconfig.PreferredKey(suffix), osEnv, fileEnv)
	legacy := resolveEnv(envconfig.LegacyKey(suffix), osEnv, fileEnv)

	winner, other := preferred, legacy
	// Source precedence outranks spelling precedence: a shell legacy value
	// beats a file preferred value.
	if !preferred.set || (legacy.set && legacy.source == sourceEnv && preferred.source == sourceConfig) {
		if legacy.set {
			winner, other = legacy, preferred
		}
	}
	if !winner.set {
		return resolved{}
	}
	if other.set && other.value != winner.value {
		winner.conflict = true
		winner.otherKey, winner.otherSource = other.key, other.source
	}
	return winner
}

// daemonFamily resolves an alias family the way the local daemon does — file
// preferred > file legacy > shell preferred > shell legacy — which is the
// inverse of resolveFamily's source precedence. The daemon's own environment
// was populated from config.env at boot, so preferring the file is what lets a
// config.env edit reach a running daemon; anything describing daemon behavior
// has to read it the same way.
func daemonFamily(suffix string, osEnv, fileEnv map[string]string) resolved {
	for _, env := range []struct {
		values map[string]string
		source string
	}{{fileEnv, sourceConfig}, {osEnv, sourceEnv}} {
		for _, key := range []string{envconfig.PreferredKey(suffix), envconfig.LegacyKey(suffix)} {
			if v, ok := env.values[key]; ok && strings.TrimSpace(v) != "" {
				return resolved{set: true, value: strings.TrimSpace(v), source: env.source, key: key}
			}
		}
	}
	return resolved{}
}

// tokenPrefix returns the non-sensitive scheme marker of a token (everything
// up to and including the first underscore, e.g. "glc_"), or "" when there is
// none. It never returns the secret part.
func tokenPrefix(token string) string {
	token = strings.TrimSpace(token)
	if i := strings.IndexByte(token, '_'); i > 0 && i <= 8 {
		return token[:i+1]
	}
	return ""
}

// disallowedKeys lists keys in config.env that agento11y's dotenv loader ignores.
// It mirrors dotenv.LoadDotenv's line handling so the same lines are parsed,
// but reports the rejected keys the loader silently drops.
func disallowedKeys(path string) []string {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()

	var bad []string
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "export "); ok {
			line = strings.TrimSpace(after)
		}
		key, _, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || dotenv.AllowedDotenvKey(key) || seen[key] {
			continue
		}
		seen[key] = true
		bad = append(bad, key)
	}
	return bad
}

func normalizeVersion(v string) string {
	if strings.TrimSpace(v) == "" {
		return "dev"
	}
	return v
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
