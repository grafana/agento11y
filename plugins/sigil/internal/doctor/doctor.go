// Package doctor implements `sigil doctor`: a read-only diagnostic that
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
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"slices"
	"strings"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/dotenv"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/envconfig"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/updatecheck"
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

// trackedKeys are the env vars doctor attributes to a source (OS env vs
// config.env). SnapshotEnv records their OS-env values before dotenv merge so
// Collect can tell where each effective value came from.
var trackedKeys = []string{
	"SIGIL_ENDPOINT",
	"SIGIL_INSECURE",
	"SIGIL_AUTH_TENANT_ID",
	"SIGIL_AUTH_TOKEN",
	"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_HEADERS",
	"SIGIL_OTEL_AUTH_TOKEN",
	"SIGIL_CONTENT_CAPTURE_MODE",
	"SIGIL_TAGS",
	"SIGIL_AUTO_UPDATE",
}

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
	Sigil              SigilSection         `json:"sigil"`
	Config             ConfigSection        `json:"config"`
	Conversations      ConversationsSection `json:"conversations"`
	Analytics          AnalyticsSection     `json:"analytics"`
	Agents             []AgentStatus        `json:"agents"`
	AutoUpdateDisabled bool                 `json:"auto_update_disabled"`
}

// SigilSection reports the binary's build version.
type SigilSection struct {
	Version string `json:"version"`
}

// envValue is a non-secret resolved env var: endpoints and tenant IDs are safe
// to print, so Value is populated.
type envValue struct {
	Set    bool   `json:"set"`
	Value  string `json:"value,omitempty"`
	Source string `json:"source,omitempty"`
}

// tokenValue is a resolved secret. The value is never recorded; only presence
// and an optional non-sensitive scheme prefix (e.g. "glc_") are.
type tokenValue struct {
	Set    bool   `json:"set"`
	Prefix string `json:"prefix,omitempty"`
	Source string `json:"source,omitempty"`
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
	Health              Health            `json:"status"`
	Messages            []string          `json:"messages,omitempty"`
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

// AgentStatus reports one host agent's detection + install state. HookBased is
// set for agents the sigil binary never installs (cursor): their capture is
// wired into the host's own hook settings, so install state isn't something
// doctor can read.
type AgentStatus struct {
	Name      string `json:"name"`
	OnPath    bool   `json:"on_path"`
	Installed bool   `json:"installed"`
	HookBased bool   `json:"hook_based,omitempty"`
	Version   string `json:"version,omitempty"`
	Note      string `json:"note,omitempty"`
	Health    Health `json:"status"`

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
			_, _ = fmt.Fprintf(p.Stderr, "sigil: doctor: %v\n", err)
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
		_, _ = fmt.Fprintln(stderr, "usage: sigil doctor [--json] [--probe] [--no-color]")
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
	fileEnv := dotenv.LoadDotenv(dotenv.FilePath("sigil"), nil)
	osEnv := p.OSEnv
	if osEnv == nil {
		osEnv = map[string]string{}
	}

	r := &Report{}
	r.Sigil = SigilSection{Version: normalizeVersion(p.Version)}
	r.Conversations = collectConversations(osEnv, fileEnv)
	r.Analytics = collectAnalytics(osEnv, fileEnv, r.Conversations.configured())
	r.Config = collectConfig(osEnv, fileEnv)
	r.Agents = collectAgents(ctx, r.Sigil.Version)
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
	endpoint := resolveEnv("SIGIL_ENDPOINT", osEnv, fileEnv)
	tenant := resolveEnv("SIGIL_AUTH_TENANT_ID", osEnv, fileEnv)
	token := resolveEnv("SIGIL_AUTH_TOKEN", osEnv, fileEnv)

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
			"not configured — run `sigil login` or set SIGIL_ENDPOINT, SIGIL_AUTH_TENANT_ID and SIGIL_AUTH_TOKEN")
	default:
		sec.Health = HealthError
		var missing []string
		if !endpoint.set {
			missing = append(missing, "SIGIL_ENDPOINT")
		}
		if !tenant.set {
			missing = append(missing, "SIGIL_AUTH_TENANT_ID")
		}
		if !token.set {
			missing = append(missing, "SIGIL_AUTH_TOKEN")
		}
		sec.Messages = append(sec.Messages, "incomplete credentials; missing "+strings.Join(missing, ", "))
	}
	return sec
}

func collectAnalytics(osEnv, fileEnv map[string]string, conversationsConfigured bool) AnalyticsSection {
	sigilOTLP := resolveEnv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)
	stdOTLP := resolveEnv("OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)

	sec := AnalyticsSection{}
	switch {
	case sigilOTLP.set:
		sec.Endpoint = sigilOTLP.envValue()
		sec.EndpointVar = "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT"
	case stdOTLP.set:
		sec.Endpoint = stdOTLP.envValue()
		sec.EndpointVar = "OTEL_EXPORTER_OTLP_ENDPOINT"
	case conversationsConfigured:
		// The headline failure: conversations export fine, but analytics
		// (the AI Observability page) stays empty because no OTLP endpoint
		// is configured. dotenv.HasCredentials passes here, so nothing else
		// the binary does today would flag this.
		sec.Health = HealthError
		sec.Messages = append(sec.Messages,
			"no OTLP endpoint set — metrics and traces will not be exported even though conversations are configured. "+
				"Set SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT (e.g. https://otlp-gateway-prod-<region>.grafana.net/otlp).")
		return sec
	default:
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, "no OTLP endpoint set; analytics export is disabled")
		return sec
	}

	// The endpoint is set, but OTLP export still needs auth (unless the
	// collector is open). Mirror internal/otel: auth is an explicit
	// Authorization entry in OTEL_EXPORTER_OTLP_HEADERS, or synthesized from
	// SIGIL_AUTH_TENANT_ID + (SIGIL_OTEL_AUTH_TOKEN or SIGIL_AUTH_TOKEN). Don't
	// report ok when none of those resolve, otherwise doctor shows a healthy
	// analytics pipeline that exports nothing.
	if analyticsAuthResolvable(osEnv, fileEnv) {
		sec.Health = HealthOK
	} else {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"OTLP endpoint set but no auth resolved — set SIGIL_AUTH_TENANT_ID and SIGIL_OTEL_AUTH_TOKEN "+
				"(or SIGIL_AUTH_TOKEN), or an Authorization entry in OTEL_EXPORTER_OTLP_HEADERS. "+
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
	tenant := resolveEnv("SIGIL_AUTH_TENANT_ID", osEnv, fileEnv)
	token := resolveEnv("SIGIL_OTEL_AUTH_TOKEN", osEnv, fileEnv)
	if !token.set {
		token = resolveEnv("SIGIL_AUTH_TOKEN", osEnv, fileEnv)
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
	path := dotenv.FilePath("sigil")
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

	// SIGIL_TAGS attaches key=value tags to every generation. They aren't
	// secret, so surface the resolved set (and where it came from) to make a
	// mis-set or forgotten tag visible.
	if tags := resolveEnv("SIGIL_TAGS", osEnv, fileEnv); tags.set {
		if parsed := envconfig.ParseExtraTags(tags.value); len(parsed) > 0 {
			sec.Tags = parsed
			sec.TagsSource = tags.source
		}
	}

	if len(sec.DisallowedKeys) > 0 {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"config.env has keys sigil ignores: "+strings.Join(sec.DisallowedKeys, ", "))
	}
	if sec.ContentModeFellBack {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			fmt.Sprintf("SIGIL_CONTENT_CAPTURE_MODE is invalid; using %s", mode))
	}
	if sec.GuardsFellBack {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages,
			"a SIGIL_GUARDS_* value is invalid; falling back to defaults")
	}
	return sec
}

func runProbes(ctx context.Context, r *Report, osEnv, fileEnv map[string]string) {
	if r.Conversations.configured() {
		token := resolveEnv("SIGIL_AUTH_TOKEN", osEnv, fileEnv).value
		insecure := envconfig.ParseBool(resolveEnv("SIGIL_INSECURE", osEnv, fileEnv).value)
		res := probeConversationsFn(ctx, r.Conversations.Endpoint.Value, r.Conversations.TenantID.Value, token, insecure)
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

// resolved is the effective value of an env var plus where it came from.
type resolved struct {
	set    bool
	value  string
	source string
}

func (r resolved) envValue() envValue {
	return envValue{Set: r.set, Value: r.value, Source: r.source}
}

func (r resolved) tokenValue() tokenValue {
	return tokenValue{Set: r.set, Prefix: tokenPrefix(r.value), Source: r.source}
}

// resolveEnv mirrors dotenv.ApplyEnv precedence: a non-empty OS-env value wins
// over config.env. The OS-env snapshot must predate the dotenv merge.
func resolveEnv(key string, osEnv, fileEnv map[string]string) resolved {
	if v, ok := osEnv[key]; ok && strings.TrimSpace(v) != "" {
		return resolved{set: true, value: strings.TrimSpace(v), source: sourceEnv}
	}
	if v, ok := fileEnv[key]; ok && strings.TrimSpace(v) != "" {
		return resolved{set: true, value: strings.TrimSpace(v), source: sourceConfig}
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

// disallowedKeys lists keys in config.env that sigil's dotenv loader ignores.
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
