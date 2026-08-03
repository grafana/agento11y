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
	"os"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
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
	"TAGS",
	"AUTO_UPDATE",
	"LOCAL",
	"LOCAL_FORWARD",
	// GUARDS_ENABLED is tracked for LocalHookForward only. Everything else
	// reads the guard settings through envconfig.ResolveGuards, which is the
	// hook process's view; the local daemon resolves this one config.env-first
	// and that comparison needs both sources attributed.
	"GUARDS_ENABLED",
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
	Probe   bool
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
	Binary             BinarySection        `json:"agento11y"`
	Config             ConfigSection        `json:"config"`
	Conversations      ConversationsSection `json:"conversations"`
	Analytics          AnalyticsSection     `json:"analytics"`
	Agents             []AgentStatus        `json:"agents"`
	AutoUpdateDisabled bool                 `json:"auto_update_disabled"`
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
	Path                string            `json:"path"`
	Exists              bool              `json:"exists"`
	DisallowedKeys      []string          `json:"disallowed_keys,omitempty"`
	ContentCaptureMode  string            `json:"content_capture_mode"`
	ContentModeFellBack bool              `json:"content_mode_fell_back"`
	GuardsEnabled       bool              `json:"guards_enabled"`
	GuardsTimeoutMs     int               `json:"guards_timeout_ms"`
	GuardsFailOpen      bool              `json:"guards_fail_open"`
	GuardsFellBack      bool              `json:"guards_fell_back,omitempty"`
	Tags                map[string]string `json:"tags,omitempty"`
	TagsSource          string            `json:"tags_source,omitempty"`
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
	Endpoint    envValue        `json:"endpoint"`
	EndpointVar string          `json:"endpoint_var,omitempty"`
	Health      Health          `json:"status"`
	Messages    []string        `json:"messages,omitempty"`
	Probe       *AnalyticsProbe `json:"probe,omitempty"`
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
	Version   string       `json:"version,omitempty"`
	Note      string       `json:"note,omitempty"`
	Health    Health       `json:"status"`

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

func (p *ProbeResult) authFailure() bool {
	return p != nil && (p.StatusCode == 401 || p.StatusCode == 403)
}

// unreachable reports a probe outcome that means a configured pipeline can't
// deliver: a transport error (no HTTP response, e.g. DNS failure, connection
// refused, or timeout) or a 5xx server error. 401/403 are handled separately
// as a scope problem (authFailure). Other 4xx are not treated as broken because
// the minimal probe body ({}) can draw a benign 400/415 from an endpoint that
// validates the body before auth.
func (p *ProbeResult) unreachable() bool {
	return p != nil && (p.StatusCode == 0 || p.StatusCode >= 500)
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
		renderHuman(p.Stdout, report, !opts.NoColor, opts.Probe)
	}
	return report.exitCode()
}

func parseFlags(args []string, stderr io.Writer) (Options, error) {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintln(stderr, "usage: agento11y doctor [--json] [--probe] [--no-color]")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "Report the health of the conversations and analytics export pipelines,")
		_, _ = fmt.Fprintln(stderr, "config validity, and installed host-agent plugins.")
		_, _ = fmt.Fprintln(stderr)
		_, _ = fmt.Fprintln(stderr, "  --json       emit a stable JSON report (for support tooling)")
		_, _ = fmt.Fprintln(stderr, "  --probe      send live requests to the endpoints and report HTTP status")
		_, _ = fmt.Fprintln(stderr, "  --no-color   disable ANSI colors")
	}
	var opts Options
	var online bool
	fs.BoolVar(&opts.JSON, "json", false, "emit a JSON report")
	fs.BoolVar(&opts.Probe, "probe", false, "send live requests to the endpoints")
	fs.BoolVar(&online, "online", false, "alias for --probe")
	fs.BoolVar(&opts.NoColor, "no-color", false, "disable ANSI colors")
	if err := fs.Parse(args); err != nil {
		return Options{}, err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return Options{}, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	opts.Probe = opts.Probe || online
	return opts, nil
}

// Collect builds the report. It reads config.env and the env snapshot but
// performs no mutations. Network probes run only when opts.Probe is set.
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
	r.AutoUpdateDisabled = updatecheck.Disabled()

	if opts.Probe {
		runProbes(ctx, r, osEnv, fileEnv)
	}
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
	// from. Without this the offline run calls a malformed endpoint healthy and
	// only --probe, which needs the network, reports the problem.
	if endpoint.set {
		if _, err := wire.NormalizeGenerationExportURL(endpoint.value, resolveInsecure(osEnv, fileEnv)); err != nil {
			sec.Health = HealthError
			sec.Messages = append(sec.Messages, fmt.Sprintf("%s is not a usable endpoint: %v", endpoint.key, err))
		}
	}
	for _, r := range []resolved{endpoint, tenant, token} {
		if r.conflict {
			sec.Messages = append(sec.Messages, fmt.Sprintf(
				"%s and its other spelling are both set with different values; using %s", r.key, r.key))
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
		sec.EndpointVar = brandedOTLP.key
	case stdOTLP.set:
		sec.Endpoint = stdOTLP.envValue()
		sec.EndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"
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
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s and its other spelling are both set with different values; using %s", brandedOTLP.key, brandedOTLP.key))
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
// `--probe` does that against the live endpoint.
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

	// ResolveContentMode logs a line when it falls back from an invalid value,
	// so a capturing logger doubles as the fell-back signal.
	var buf bytes.Buffer
	mode := envconfig.ResolveContentMode(log.New(&buf, "", 0))
	sec.ContentCaptureMode = mode.String()
	sec.ContentModeFellBack = buf.Len() > 0

	// Guards are the shared pre-tool-call enforcement flags every agent hook
	// reads via envconfig.ResolveGuards. They default off, so surface the
	// effective values to confirm whether guards actually run and with what
	// timeout / fail mode. ResolveGuards logs on an invalid value, so a
	// capturing logger doubles as the fell-back signal, same as content mode.
	var guardBuf bytes.Buffer
	guards := envconfig.ResolveGuards(log.New(&guardBuf, "", 0))
	sec.GuardsEnabled = guards.Enabled
	sec.GuardsTimeoutMs = guards.TimeoutMs
	sec.GuardsFailOpen = guards.FailOpen
	sec.GuardsFellBack = guardBuf.Len() > 0

	// The TAGS family attaches key=value tags to every generation. They
	// aren't secret, so surface the resolved set (and where it came from) to
	// make a mis-set or forgotten tag visible.
	tags := resolveFamily("TAGS", osEnv, fileEnv)
	if tags.set {
		if parsed := envconfig.ParseExtraTags(tags.value); len(parsed) > 0 {
			sec.Tags = parsed
			sec.TagsSource = tags.source
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
			fmt.Sprintf("the CONTENT_CAPTURE_MODE value is invalid; using %s", mode))
	}
	if sec.GuardsFellBack {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"a GUARDS_* value is invalid; falling back to defaults")
	}
	if tags.conflict {
		sec.Messages = append(sec.Messages, fmt.Sprintf(
			"%s and its other spelling are both set with different values; using %s", tags.key, tags.key))
	}
	if tags.legacyWon() {
		sec.Messages = append(sec.Messages,
			"tags set via legacy SIGIL_TAGS — this keeps working, but the preferred name is AGENTO11Y_TAGS")
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
		// The launcher is the only thing that reads this family, so a user who
		// reads it as "nothing leaves this machine" is wrong about every other
		// way a session starts.
		sec.Messages = append(sec.Messages,
			"local mode covers `agento11y <agent>` launches; sessions the host agent starts on its own, such as Cursor or a plain `claude`, keep exporting to the configured endpoint")
	}
	return sec
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

func runProbes(ctx context.Context, r *Report, osEnv, fileEnv map[string]string) {
	// An endpoint the offline check already rejected has nothing to probe:
	// probing it would report the same fault a second and third time. Inside
	// configured(), HealthError can only come from that check.
	if r.Conversations.configured() && r.Conversations.Health != HealthError {
		token := resolveFamily("AUTH_TOKEN", osEnv, fileEnv).value
		res := probeConversationsFn(ctx, r.Conversations.Endpoint.Value, r.Conversations.TenantID.Value, token, resolveInsecure(osEnv, fileEnv))
		r.Conversations.Probe = res
		switch {
		case res.authFailure():
			r.Conversations.Health = HealthError
			r.Conversations.Messages = append(r.Conversations.Messages, res.Message)
		case res.unreachable():
			r.Conversations.Health = HealthError
			r.Conversations.Messages = append(r.Conversations.Messages,
				"could not reach the conversations endpoint: "+describeProbe(res))
		}
	}
	if r.Analytics.Endpoint.Set {
		probe := probeOTLPFn(ctx)
		r.Analytics.Probe = probe
		if probe != nil {
			switch {
			case probe.Metrics.authFailure() || probe.Traces.authFailure():
				r.Analytics.Health = HealthError
				r.Analytics.Messages = append(r.Analytics.Messages,
					"OTLP endpoint rejected auth (401/403) — the token is likely missing metrics:write/traces:write scope")
			case probe.Metrics.unreachable() || probe.Traces.unreachable():
				r.Analytics.Health = HealthError
				r.Analytics.Messages = append(r.Analytics.Messages,
					"could not reach the OTLP endpoint — metrics and traces will not be exported")
			}
		}
	}
}

// resolved is the effective value of an env var plus where it came from:
// key is the spelling that won (AGENTO11Y_* or SIGIL_*), and conflict reports
// that the other spelling also resolves — to a different value — under the
// same source precedence.
type resolved struct {
	set      bool
	value    string
	source   string
	key      string
	conflict bool
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
	winner.conflict = other.set && other.value != winner.value
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
