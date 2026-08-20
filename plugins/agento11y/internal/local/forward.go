package local

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/dotenv"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// maxInFlightForwards bounds the number of concurrent best-effort Cloud
// forwards. Local capture never blocks on forwarding, so excess payloads are
// dropped (and recorded) rather than queued.
const maxInFlightForwards = 8

// maxRecordedForwardFailures caps the failure ring the loader keeps for
// forwardStatus, per leg. The daemon's logger writes to io.Discard unless debug
// logging is on, so the status endpoint is the only channel a runtime failure
// (rotated token, unreachable Cloud) can reach the user through. The cap is
// per-label because the legs fail at wildly different rates: the hook leg
// records once per tool call, and a global ring would let it evict a
// generation export Cloud has been rejecting all session.
const maxRecordedForwardFailures = 5

// forwardLabelGenerations names the generation-export leg in the failure ring.
// The legs are recorded and cleared independently, so the label a queued
// payload is dropped under must match the one its POST reports.
const forwardLabelGenerations = "generations"

// forwardLabelHooks names the guard-evaluation leg in the failure ring. Unlike
// the other two legs this one is synchronous: a failure here changes the
// verdict the agent acts on, so it is worth surfacing in the same place.
const forwardLabelHooks = "hooks"

// hookEvaluatePath is the Cloud hook endpoint the daemon relays to. There is no
// wire.HooksEvaluateHTTPPath constant to use instead: CI and installed builds
// resolve github.com/grafana/agento11y/go through this module's go.mod pin
// (GOWORK=off), so a constant added to go/proto/agento11y/wire is unusable here
// until the pin moves (see go.mod). A local build resolves the SDK through
// go.work and would compile against a constant CI cannot see.
const hookEvaluatePath = "/api/v1/hooks:evaluate"

// otlpForwardLabel names one OTLP signal leg in the failure ring.
func otlpForwardLabel(signal string) string { return "otlp/" + signal }

// ForwardMarkerHeader is set on every forwarded request. A daemon that
// receives a payload carrying it does not forward again, so two daemons
// pointed at each other (or one pointed at itself through a hand-written
// local ingest endpoint) exchange one copy instead of looping.
//
// It is exported so an in-process producer that exports through the local
// ingest endpoint can mark its own requests and keep them from reaching
// Cloud. A history import that writes months of transcripts needs this.
const ForwardMarkerHeader = "X-Agento11y-Local-Forwarded"

// forwardConfig is the resolved forwarding configuration for one load cycle.
// The two legs resolve independently: generation export needs Cloud
// credentials, while the OTLP relay needs an OTLP endpoint and authenticates
// the way the real exporter does. Either can be live while the other is
// refused, and the matching reason field says why.
type forwardConfig struct {
	// enabled reports whether any leg would forward.
	enabled bool
	// strip reports whether the forwarded copy must be reduced to
	// metadata_only. Full content is forwarded only when the resolved
	// CONTENT_CAPTURE_MODE is exactly "full"; every other mode forwards the
	// reduced copy so opting into Cloud forwarding never widens what leaves
	// the host beyond what the user configured.
	strip bool

	genURL     string            // normalized /api/v1/generations:export URL ("" = not forwarded)
	genHeaders map[string]string // Basic auth + tenant header
	genReason  string            // why generations are not forwarded

	otlpEndpoint string            // base OTLP endpoint ("" = not forwarded)
	otlpHeaders  map[string]string // the exporter's header set for this endpoint
	otlpReason   string            // why OTLP is not forwarded

	hookURL     string            // Cloud /api/v1/hooks:evaluate URL ("" = not chained)
	hookHeaders map[string]string // Basic auth + tenant header, same pair as generations
	hookReason  string            // why hook evaluation is not chained to Cloud

	// failOpen and timeoutMs are the guard policy the chained hook call
	// applies: whether a failed Cloud evaluation allows the tool call, and
	// the budget used when the agent propagates no deadline of its own. They
	// hold their documented defaults (fail open, DefaultGuardsTimeoutMs) on
	// every path that refuses the hook leg, so a partially resolved config
	// never reads as an explicit "fail closed".
	failOpen  bool
	timeoutMs int
}

// disabledReasons joins the per-leg refusal reasons for logging, or "" when
// every leg is live (or forwarding is simply off). One reason shared by all
// legs is a whole-config refusal and drops the per-leg prefixes.
func (c forwardConfig) disabledReasons() string {
	if c.genReason == c.otlpReason && c.genReason == c.hookReason {
		return c.genReason
	}
	parts := make([]string, 0, 3)
	if c.genReason != "" {
		parts = append(parts, "generations: "+c.genReason)
	}
	if c.otlpReason != "" {
		parts = append(parts, "otlp: "+c.otlpReason)
	}
	if c.hookReason != "" {
		parts = append(parts, "hooks: "+c.hookReason)
	}
	return strings.Join(parts, "; ")
}

// forwardLoader resolves forwarding configuration from config.env and relays
// captured telemetry to Grafana Cloud. The daemon is a long-lived singleton, so
// it re-reads config.env on size/mtime change instead of freezing config at
// boot, letting a config.env edit take effect without an `agento11y local
// restart`.
type forwardLoader struct {
	path   string
	logger *log.Logger
	client *http.Client

	sem chan struct{}
	wg  sync.WaitGroup

	mu           sync.Mutex
	haveStat     bool
	statMtime    time.Time
	statSize     int64
	cached       forwardConfig
	loggedReason string // last refusal logged, so we log it only once

	// failMu guards delivery state written by forward goroutines and the hook
	// handler and read by the status endpoint.
	failMu    sync.Mutex
	failures  []forwardFailure
	legs      map[string]forwardLeg
	failOpens int
}

func newForwardLoader(path string, logger *log.Logger) *forwardLoader {
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	return &forwardLoader{
		path:   path,
		logger: logger,
		client: &http.Client{Timeout: 10 * time.Second},
		sem:    make(chan struct{}, maxInFlightForwards),
		legs:   make(map[string]forwardLeg),
	}
}

// load returns the current forwarding configuration, re-resolving from
// config.env when the file's size or mtime changes. A missing file is not an
// error: the daemon's process environment (populated from config.env at boot)
// is consulted instead, and a later-created file is picked up on the next call.
// An unreadable file disables forwarding entirely — the file is the only place
// that can express an explicit "off", so failing closed is the safe direction.
func (l *forwardLoader) load() forwardConfig {
	l.mu.Lock()
	defer l.mu.Unlock()

	info, err := os.Stat(l.path)
	switch {
	case err == nil:
		if l.haveStat && info.Size() == l.statSize && info.ModTime().Equal(l.statMtime) {
			return l.cached
		}
		var readErr error
		l.cached, readErr = l.resolve()
		// A read failure is not cached against size/mtime: fixing the file's
		// permissions changes neither, so a cached refusal could outlive the
		// problem until the contents happen to change.
		l.haveStat = readErr == nil
		l.statMtime = info.ModTime()
		l.statSize = info.Size()
	case os.IsNotExist(err):
		l.haveStat = false
		l.cached, _ = l.resolve()
	default:
		// os.Stat's error already names the path.
		l.cached = l.unreadableConfig(err)
		l.haveStat = false
	}
	return l.cached
}

// forwardStatus is a snapshot of how the daemon forwards captured sessions to
// Cloud right now, for the viewer's config and conversations pages. It reads
// the same resolved configuration the forward path uses, so the two cannot
// disagree.
type forwardStatus struct {
	// Enabled reports whether a forwarded copy would actually be sent.
	Enabled bool `json:"enabled"`
	// Mode is one of forwardMode{Off,MetadataOnly,Full}.
	Mode string `json:"mode"`
	// Generations and OTLP report the two legs separately: a configuration
	// can forward generations with no OTLP endpoint, or relay OTLP with
	// credentials the generation export cannot use.
	Generations bool `json:"generations"`
	OTLP        bool `json:"otlp"`
	// Hooks reports whether guard evaluation from a --local session is
	// relayed to Cloud. It is gated harder than the other two legs, so it can
	// be off while both of them deliver.
	Hooks bool `json:"hooks"`
	// Reason explains why generations are not forwarded although the user
	// opted in (empty endpoint, placeholder credentials, unreadable config).
	// Empty when forwarding is simply not enabled.
	Reason string `json:"reason,omitempty"`
	// OTLPReason is the same for the OTLP leg (no endpoint configured, or a
	// local endpoint that would loop back into this daemon).
	OTLPReason string `json:"otlpReason,omitempty"`
	// HookReason is the same for the hook leg (guards off, a local endpoint
	// with no Cloud rules to consult, or unusable credentials).
	HookReason string `json:"hookReason,omitempty"`
	// Failures are the forward attempts that failed since the last success,
	// most recent first. Non-empty means the current posture is not actually
	// delivering, which no other channel would tell the user about: the
	// daemon's logger is discarded unless debug logging is on.
	Failures []forwardFailure `json:"failures,omitempty"`
	// Legs keeps each forwarding leg's last delivery and last failure.
	Legs map[string]forwardLeg `json:"legs,omitempty"`
	// HookFailOpens counts the tool calls allowed since the daemon started
	// because a chained guard evaluation failed and GUARDS_FAIL_OPEN was on.
	// Unlike Failures it is never cleared: the agent cannot tell such an allow
	// from a Cloud allow, so a count that a later success wiped would leave no
	// trace that the guard did not run.
	HookFailOpens int `json:"hookFailOpens,omitempty"`
}

// forwardFailure is one failed forward attempt, kept so the viewer can show
// that a live-looking posture is not delivering.
type forwardFailure struct {
	At     string `json:"at"`
	Label  string `json:"label"`
	Detail string `json:"detail"`
}

// forwardLeg keeps the last delivery and failure after the failure ring clears.
type forwardLeg struct {
	LastSuccessAt     string `json:"lastSuccessAt,omitempty"`
	LastFailureAt     string `json:"lastFailureAt,omitempty"`
	LastFailureDetail string `json:"lastFailureDetail,omitempty"`
}

const (
	forwardModeOff          = "off"
	forwardModeMetadataOnly = "metadata_only"
	forwardModeFull         = "full"
)

// status reports the current forwarding posture. It goes through load() rather
// than re-resolving so the viewer reflects exactly what the forward path does,
// including the cache and the unreadable-config refusal.
func (l *forwardLoader) status() forwardStatus {
	cfg := l.load()
	st := forwardStatus{
		Enabled:       cfg.enabled,
		Mode:          forwardModeOff,
		Generations:   cfg.genURL != "",
		OTLP:          cfg.otlpEndpoint != "",
		Hooks:         cfg.hookURL != "",
		Reason:        cfg.genReason,
		OTLPReason:    cfg.otlpReason,
		HookReason:    cfg.hookReason,
		Failures:      l.recentFailures(),
		Legs:          l.recentLegs(),
		HookFailOpens: l.recentFailOpens(),
	}
	switch {
	case !cfg.enabled:
	case cfg.strip:
		st.Mode = forwardModeMetadataOnly
	default:
		st.Mode = forwardModeFull
	}
	return st
}

// resolve reads config.env plus the process environment and logs any refusal
// once. It returns the read error alongside the configuration so the caller
// knows not to cache a refusal. Must be called with l.mu held.
func (l *forwardLoader) resolve() (forwardConfig, error) {
	get, err := l.reader()
	if err != nil {
		// The file exists but cannot be read, so its explicit "off" (if any)
		// is invisible and the process environment still holds the boot-time
		// values. Fail closed rather than forward on stale config.
		return l.unreadableConfig(err), err
	}
	cfg := resolveForwardConfig(l.logger, get)
	l.logDisabled(cfg.disabledReasons())
	return cfg, nil
}

// unreadableConfig is the fail-closed configuration for a config.env the
// daemon could not read, with the reason logged once. Must be called with
// l.mu held.
func (l *forwardLoader) unreadableConfig(err error) forwardConfig {
	// Both os.Stat's and os.Open's errors already name the path.
	//
	// The guard knobs hold their defaults rather than their zero values: an
	// unreadable file is not an explicit "fail closed", and the hook leg is
	// refused here anyway, so a tool call proceeds. The two telemetry legs fail
	// closed, the enforcement leg effectively fails open.
	cfg := forwardConfig{
		failOpen:  true,
		timeoutMs: envconfig.DefaultGuardsTimeoutMs,
		genReason: "config.env unreadable: " + err.Error(),
	}
	cfg.otlpReason = cfg.genReason
	cfg.hookReason = cfg.genReason
	l.logDisabled(cfg.disabledReasons())
	return cfg
}

// resolveForwardConfig builds the forwarding configuration from a config
// snapshot. It is the single resolution path: both the forward legs and the
// viewer's status read it, so they cannot drift.
func resolveForwardConfig(logger *log.Logger, get envReader) forwardConfig {
	// Refusing every leg still resolves the guard policy to its defaults, so
	// no caller can read a half-built config as an explicit "fail closed".
	off := forwardConfig{failOpen: true, timeoutMs: envconfig.DefaultGuardsTimeoutMs}
	if !boolFamily(logger, get, "LOCAL_FORWARD", false) {
		return off
	}

	endpoint, _ := get.family("ENDPOINT")
	tenant, _ := get.family("AUTH_TENANT_ID")
	token, _ := get.family("AUTH_TOKEN")

	cfg := off
	cfg.strip = contentModeFamily(logger, get) != agento11y.ContentCaptureModeFull

	if reason := forwardDisabledReason(endpoint, tenant, token); reason != "" {
		cfg.genReason = reason
	} else if genURL, err := wire.NormalizeGenerationExportURL(endpoint, false); err != nil {
		cfg.genReason = "generation endpoint: " + err.Error()
	} else {
		cfg.genURL = genURL
		cfg.genHeaders = generationForwardHeaders(tenant, token)
	}

	otlpEndpoint, otlpReason := otlpForwardTarget(get)
	if otlpReason != "" {
		cfg.otlpReason = otlpReason
	} else {
		cfg.otlpEndpoint = otlpEndpoint
		cfg.otlpHeaders = otlpForwardHeaders(get, tenant)
	}

	// The guard knobs are resolved through the same file-first reader as
	// everything else here, not envconfig.ResolveGuards: that helper reads the
	// process environment only, so a config.env edit would not reach the
	// running daemon.
	cfg.failOpen = boolFamily(logger, get, "GUARDS_FAIL_OPEN", true)
	cfg.timeoutMs = intFamily(logger, get, "GUARDS_TIMEOUT_MS", envconfig.DefaultGuardsTimeoutMs)
	if reason := hookForwardDisabledReason(boolFamily(logger, get, "GUARDS_ENABLED", false), endpoint, tenant, token); reason != "" {
		cfg.hookReason = reason
	} else if hookURL := hookEvaluateURL(endpoint); hookURL == "" {
		cfg.hookReason = "hook endpoint: " + endpoint + " has no host"
	} else {
		cfg.hookURL = hookURL
		cfg.hookHeaders = generationForwardHeaders(tenant, token)
	}

	cfg.enabled = cfg.genURL != "" || cfg.otlpEndpoint != "" || cfg.hookURL != ""
	return cfg
}

// hookForwardDisabledReason refuses hook relay when Cloud evaluation cannot
// run. Unlike telemetry forwarding, a local hook target always allows.
func hookForwardDisabledReason(guardsEnabled bool, endpoint, tenant, token string) string {
	switch {
	case !guardsEnabled:
		return envconfig.PreferredKey("GUARDS_ENABLED") + " is off, so no guard check is made at all"
	case endpoint == "":
		return envconfig.PreferredKey("ENDPOINT") + " is empty"
	case envconfig.IsLocalEndpoint(endpoint):
		return "endpoint " + endpoint + " is local, so there are no Cloud rules to consult"
	case tenant == "" || token == "" || tenant == envconfig.LocalAuthPlaceholder || token == envconfig.LocalAuthPlaceholder:
		return "Cloud credentials are missing or are the " + envconfig.LocalAuthPlaceholder + " placeholder"
	default:
		return ""
	}
}

// HookForwardReason reports why `agento11y <agent> --local` would not relay
// guard evaluation to Cloud for the given resolved settings, or "" when it
// would. Exported so `agento11y doctor` reports the gate the daemon actually
// applies instead of reconstructing it from the same inputs.
func HookForwardReason(forwardEnabled, guardsEnabled bool, endpoint, tenant, token string) string {
	if !forwardEnabled {
		return envconfig.PreferredKey("LOCAL_FORWARD") + " is off, so local sessions never contact Cloud"
	}
	return hookForwardDisabledReason(guardsEnabled, strings.TrimSpace(endpoint), strings.TrimSpace(tenant), strings.TrimSpace(token))
}

// hookEvaluateURL builds the Cloud hook URL for an API endpoint: scheme and
// host are kept, any path is dropped, and the hook path is appended. Mirrors
// the SDK's baseURLFromAPIEndpoint (go/agento11y/rating.go), which is
// unexported. Returns "" when the endpoint has no usable host, which the
// caller reports as a refusal rather than POSTing to a relative URL.
func hookEvaluateURL(endpoint string) string {
	u, err := url.Parse(ensureEndpointScheme(endpoint))
	if err != nil || strings.TrimSpace(u.Host) == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host + hookEvaluatePath
}

// generationForwardHeaders builds the auth headers the generation export
// expects: Basic tenant:token plus the tenant header. A local target needs
// neither, in which case both values are blank and post() omits them.
func generationForwardHeaders(tenant, token string) map[string]string {
	value, _ := otel.BasicAuthHeaderValue(tenant, token)
	return map[string]string{
		"Authorization":       value,
		wire.TenantHeaderName: tenant,
	}
}

// otlpForwardTarget resolves the base OTLP endpoint to relay to, or a reason
// why the OTLP leg stays off. Scheme and the insecure downgrade are applied
// the way internal/otel builds exporter URLs, and the resulting URL is checked
// against the local-receiver guard last so an insecure downgrade cannot
// reintroduce a loop back into this daemon.
func otlpForwardTarget(get envReader) (endpoint, reason string) {
	raw, _ := get.family("OTEL_EXPORTER_OTLP_ENDPOINT")
	if raw == "" {
		raw = get.key("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if raw == "" {
		return "", "no OTLP endpoint configured, so traces and metrics are not forwarded"
	}

	endpoint = ensureEndpointScheme(raw)
	insecure, _ := get.family("OTEL_EXPORTER_OTLP_INSECURE")
	if insecure == "" {
		insecure = get.key("OTEL_EXPORTER_OTLP_INSECURE")
	}
	if envconfig.ParseBool(insecure) {
		if rest, ok := strings.CutPrefix(endpoint, "https://"); ok {
			endpoint = "http://" + rest
		}
	}
	if envconfig.IsLocalEndpoint(endpoint) {
		return "", "OTLP endpoint " + endpoint + " is local, so relaying it would loop back into this daemon"
	}
	return endpoint, ""
}

// otlpForwardHeaders builds the header set the real OTLP exporter would send
// for this configuration: explicit OTEL_EXPORTER_OTLP_HEADERS win, otherwise
// Basic auth from the tenant plus OTEL_AUTH_TOKEN with AUTH_TOKEN as the
// fall-back. Without this the relay would ignore a dedicated OTLP gateway
// token that `agento11y doctor` tells users to configure.
func otlpForwardHeaders(get envReader, tenant string) map[string]string {
	otelToken, _ := get.family("OTEL_AUTH_TOKEN")
	authToken, _ := get.family("AUTH_TOKEN")
	return otel.ExporterHeaders(get.key("OTEL_EXPORTER_OTLP_HEADERS"), tenant, otelToken, authToken)
}

// envReader resolves configuration values from a config.env snapshot with a
// process-environment fall-back.
//
// The file wins, which inverts dotenv.ApplyEnv (and the precedence
// `agento11y doctor` reports). That is deliberate and is what makes a
// config.env edit take effect without a daemon restart: the daemon's own
// environment was populated from config.env at boot, so an env-first order
// would pin the boot-time values forever.
type envReader struct {
	file map[string]string
}

// reader snapshots config.env for one resolve pass. A missing file is not an
// error; a file that exists but cannot be read is.
func (l *forwardLoader) reader() (envReader, error) {
	file, err := dotenv.ReadDotenv(l.path, l.logger)
	if err != nil {
		return envReader{}, err
	}
	return envReader{file: file}, nil
}

// key resolves a single exact key (for non-branded variables such as the bare
// OTEL_EXPORTER_OTLP_ENDPOINT and OTEL_EXPORTER_OTLP_HEADERS).
func (r envReader) key(k string) string {
	if v := strings.TrimSpace(r.file[k]); v != "" {
		return v
	}
	return strings.TrimSpace(os.Getenv(k))
}

// family resolves a branded alias family preferred-first within each source:
// file AGENTO11Y_*, file SIGIL_*, env AGENTO11Y_*, env SIGIL_*. The returned
// key names the spelling the value came from so diagnostics report what the
// user actually set; it is empty when nothing is set.
func (r envReader) family(suffix string) (value, key string) {
	if v, k, ok := envconfig.LookupMap(r.file, suffix); ok {
		return v, k
	}
	if v, k, ok := envconfig.LookupEnv(suffix); ok {
		return v, k
	}
	return "", ""
}

// boolFamily parses a branded boolean toggle out of an envReader, reporting an
// unrecognised value under the spelling it was set with.
func boolFamily(logger *log.Logger, r envReader, suffix string, def bool) bool {
	raw, key := r.family(suffix)
	if key == "" {
		key = envconfig.PreferredKey(suffix)
	}
	return envconfig.BoolValue(logger, key, raw, def)
}

// intFamily parses a branded integer setting out of an envReader. A
// non-numeric, zero, or negative value is reported under the spelling it was
// set with and falls back to def, as does a value that is not set at all.
// Validation is envconfig.IntValue, shared with envconfig.ResolveGuards, so
// the daemon and the cloud-only hook path agree on what an invalid value means.
// The returned value is always positive when def is.
func intFamily(logger *log.Logger, r envReader, suffix string, def int) int {
	raw, key := r.family(suffix)
	if key == "" {
		key = envconfig.PreferredKey(suffix)
	}
	return envconfig.IntValue(logger, key, raw, def)
}

// contentModeFamily resolves the configured content capture mode out of an
// envReader. This is the mode the forwarded copy is reduced to; the local store
// always keeps full content.
func contentModeFamily(logger *log.Logger, r envReader) agento11y.ContentCaptureMode {
	raw, key := r.family("CONTENT_CAPTURE_MODE")
	return envconfig.ResolveContentModeValue(logger, key, raw)
}

// ensureEndpointScheme prepends https:// to a schemeless endpoint, mirroring
// wire.NormalizeGenerationExportURL so otel.SignalEndpointURL yields an
// absolute URL. A relative URL survives http.NewRequest and fails later in
// client.Do with "unsupported protocol scheme", which would turn a typo into a
// per-payload transport error. An empty endpoint is returned unchanged.
func ensureEndpointScheme(endpoint string) string {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return ""
	}
	lower := strings.ToLower(e)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return e
	}
	return "https://" + e
}

// forwardDisabledReason returns a non-empty reason when generation forwarding
// must be refused for the given Cloud target, guarding against shipping to a
// bogus endpoint with placeholder credentials.
func forwardDisabledReason(endpoint, tenant, token string) string {
	if endpoint == "" {
		return envconfig.PreferredKey("ENDPOINT") + " is empty"
	}
	// A local receiver (http://127.0.0.1, ::1, localhost) is a valid forward
	// target — e.g. a separate daemon — and does not validate auth, so empty
	// or placeholder credentials must not pause forwarding to it. The
	// ForwardMarkerHeader on the outbound request is what stops a daemon
	// pointed at itself from relaying the same payload again.
	if envconfig.IsLocalEndpoint(endpoint) {
		return ""
	}
	switch {
	case tenant == "" || token == "":
		return envconfig.PreferredKey("AUTH_TENANT_ID") + "/" + envconfig.PreferredKey("AUTH_TOKEN") + " are empty"
	case tenant == envconfig.LocalAuthPlaceholder || token == envconfig.LocalAuthPlaceholder:
		return "credentials are the " + envconfig.LocalAuthPlaceholder + " placeholder"
	default:
		return ""
	}
}

// logDisabled logs a refusal once, suppressing repeats while the reason stays
// the same. An empty reason clears the latch so a later refusal logs again.
// Must be called with l.mu held.
func (l *forwardLoader) logDisabled(reason string) {
	if l.loggedReason == reason {
		return
	}
	l.loggedReason = reason
	if reason != "" {
		l.logger.Printf("local forward: %s", reason)
	}
}

// recordFailuref appends a failed attempt to the ring the status endpoint
// serves, keeping the most recent maxRecordedForwardFailures for that leg. It
// also logs, which only reaches a user who enabled debug logging.
func (l *forwardLoader) recordFailuref(label, format string, args ...any) {
	detail := fmt.Sprintf(format, args...)
	l.logger.Printf("local forward: %s: %s", label, detail)

	l.failMu.Lock()
	defer l.failMu.Unlock()
	at := time.Now().UTC().Format(time.RFC3339Nano)
	l.failures = append(l.failures, forwardFailure{
		At:     at,
		Label:  label,
		Detail: detail,
	})
	leg := l.legs[label]
	leg.LastFailureAt = at
	leg.LastFailureDetail = detail
	l.legs[label] = leg
	// Trim this leg only. The hook leg records once per tool call, so a
	// global trim would silently drop the one entry another leg had.
	// Iterating backwards makes the deletions safe: they only shift entries
	// after i, and every index this loop still has to visit is before it.
	kept := 0
	for i, f := range slices.Backward(l.failures) {
		if f.Label != label {
			continue
		}
		kept++
		if kept > maxRecordedForwardFailures {
			l.failures = slices.Delete(l.failures, i, i+1)
		}
	}
}

// recordFailOpen counts a tool call that was allowed without a Cloud verdict.
// Unlike the failure ring this counter is never cleared: a fail-open allow is
// byte-identical to a Cloud allow in the response the agent acts on, so the
// count is the only durable trace that the guard did not actually run.
func (l *forwardLoader) recordFailOpen() {
	l.failMu.Lock()
	defer l.failMu.Unlock()
	l.failOpens++
}

// recordSuccess clears the recorded failures for one leg, so
// forwardStatus.Failures means "failing since that leg's last delivery" rather
// than "failed at some point". The legs fail independently: a healthy metrics
// relay must not hide a generation export Cloud keeps rejecting.
func (l *forwardLoader) recordSuccess(label string) {
	l.failMu.Lock()
	defer l.failMu.Unlock()
	l.failures = slices.DeleteFunc(l.failures, func(f forwardFailure) bool {
		return f.Label == label
	})
	leg := l.legs[label]
	leg.LastSuccessAt = time.Now().UTC().Format(time.RFC3339Nano)
	l.legs[label] = leg
}

// recentFailOpens returns how many tool calls have been allowed without a
// Cloud verdict since the daemon started.
func (l *forwardLoader) recentFailOpens() int {
	l.failMu.Lock()
	defer l.failMu.Unlock()
	return l.failOpens
}

// recentFailures returns the recorded failures most recent first.
func (l *forwardLoader) recentFailures() []forwardFailure {
	l.failMu.Lock()
	defer l.failMu.Unlock()
	if len(l.failures) == 0 {
		return nil
	}
	out := make([]forwardFailure, 0, len(l.failures))
	for _, f := range slices.Backward(l.failures) {
		out = append(out, f)
	}
	return out
}

// recentLegs returns a copy of the per-leg delivery records.
func (l *forwardLoader) recentLegs() map[string]forwardLeg {
	l.failMu.Lock()
	defer l.failMu.Unlock()
	if len(l.legs) == 0 {
		return nil
	}
	return maps.Clone(l.legs)
}

// enqueue runs fn on a bounded goroutine. When the in-flight limit is reached
// the payload is dropped (best-effort) rather than blocking the caller. The
// drop is recorded under the leg's own label so that leg's next success clears
// it.
func (l *forwardLoader) enqueue(label string, fn func()) {
	l.wg.Add(1)
	select {
	case l.sem <- struct{}{}:
		go func() {
			defer l.wg.Done()
			defer func() { <-l.sem }()
			fn()
		}()
	default:
		l.wg.Done()
		l.recordFailuref(label, "%d forwards in flight, dropping payload", cap(l.sem))
	}
}

// wait blocks until all in-flight forwards complete. Used by tests; the daemon
// never calls it.
func (l *forwardLoader) wait() { l.wg.Wait() }

// forwardGenerations relays the given raw proto-JSON generations to Cloud,
// stripping content first when cfg.strip is set.
func (l *forwardLoader) forwardGenerations(cfg forwardConfig, raw []json.RawMessage) {
	if !cfg.enabled || cfg.genURL == "" || len(raw) == 0 {
		return
	}
	payload, err := buildGenerationPayload(raw, cfg.strip)
	if err != nil {
		l.recordFailuref(forwardLabelGenerations, "build payload: %v (dropping payload)", err)
		return
	}
	l.post(cfg.genURL, wire.ContentTypeJSON, "", cfg.genHeaders, payload, forwardLabelGenerations)
}

// buildGenerationPayload wraps the raw generations back into an
// ExportGenerationsRequest envelope. When strip is set it decodes the proto,
// reduces each generation to metadata_only, and re-encodes.
func buildGenerationPayload(raw []json.RawMessage, strip bool) ([]byte, error) {
	envelope, err := json.Marshal(generationsRequest{Generations: raw})
	if err != nil {
		return nil, err
	}
	if !strip {
		return envelope, nil
	}
	// DiscardUnknown, unlike wire.UnmarshalExportGenerationsJSON: an exporter
	// newer than this daemon (the pi and opencode plugins ship on their own
	// npm cadence) would otherwise cost the whole batch, and discarding a
	// field the daemon cannot strip is the safe direction for a reduced copy.
	var req agento11yv1.ExportGenerationsRequest
	if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(envelope, &req); err != nil {
		return nil, err
	}
	for _, gen := range req.GetGenerations() {
		// contentcapture owns which fields carry content, for both the struct
		// the SDK reduces before export and the wire proto a forwarder holds.
		// The empty error category keeps this path's existing fall-back: the
		// daemon holds a call_error string rather than an error, so it cannot
		// classify one.
		contentcapture.StripGeneration(gen, "")
		relabelContentCaptureMode(gen)
	}
	return wire.MarshalExportGenerationsJSON(&req)
}

// relabelContentCaptureMode stamps the forwarded copy as metadata_only.
//
// This is not part of stripping and stays with the daemon. The SDK stamps every
// mode before it reduces, so contentcapture leaves the marker alone; the daemon
// receives generations the launcher forced to "full" (LaunchEnv.Apply, env.go)
// so the local viewer keeps everything, and relaying a reduced copy under that
// marker would have Cloud-side validation reject the now-empty parts.
//
// The container is built when absent: an exporter can send no metadata at all,
// and the strip clears an emptied Struct, so a write through GetFields() would
// assign to a nil map.
func relabelContentCaptureMode(g *agento11yv1.Generation) {
	if g == nil {
		return
	}
	if g.GetMetadata().GetFields() == nil {
		g.Metadata = &structpb.Struct{Fields: map[string]*structpb.Value{}}
	}
	g.Metadata.Fields[model.MetadataKeyContentCaptureMode] = structpb.NewStringValue(model.ContentCaptureModeMetadataOnly)
}

// forwardOTLP relays an OTLP payload to the corresponding Cloud signal URL.
// Metrics and full traces are relayed byte-for-byte with the inbound
// Content-Encoding preserved. Traces under a reduced content mode have content
// attributes, exception events, and error status messages stripped before
// relay; that path decompresses a gzip body so the proto can be parsed and
// relays the re-marshaled payload uncompressed.
func (l *forwardLoader) forwardOTLP(cfg forwardConfig, signal, contentType, contentEncoding string, body []byte) {
	if !cfg.enabled || cfg.otlpEndpoint == "" || len(body) == 0 {
		return
	}
	label := otlpForwardLabel(signal)
	out, ct, enc := body, contentType, contentEncoding
	if signal == "traces" && cfg.strip {
		decoded := body
		if isGzipEncoding(contentEncoding) {
			d, err := gunzip(body)
			if err != nil {
				l.recordFailuref(label, "gunzip: %v (dropping payload)", err)
				return
			}
			decoded = d
		}
		stripped, newCT, err := stripTracePayload(decoded, contentType)
		if err != nil {
			l.recordFailuref(label, "strip content: %v (dropping payload)", err)
			return
		}
		out, ct, enc = stripped, newCT, ""
	}
	if strings.TrimSpace(ct) == "" {
		ct = wire.ContentTypeProto
	}
	l.post(otel.SignalEndpointURL(cfg.otlpEndpoint, signal), ct, enc, cfg.otlpHeaders, out, label)
}

// isGzipEncoding reports whether a Content-Encoding header value is gzip.
func isGzipEncoding(encoding string) bool {
	return strings.EqualFold(strings.TrimSpace(encoding), "gzip")
}

// gunzip decompresses a gzip-encoded body.
func gunzip(body []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer func() { _ = zr.Close() }()
	return io.ReadAll(zr)
}

// stripTracePayload decodes an OTLP trace export (protobuf, or JSON when the
// content type says so), strips content from every span, and re-encodes in the
// same format. The returned content type matches the encoding used.
func stripTracePayload(body []byte, contentType string) ([]byte, string, error) {
	isJSON := strings.Contains(strings.ToLower(contentType), "json")
	req, err := decodeTracePayload(body, contentType)
	if err != nil {
		return nil, "", err
	}
	stripTraceContent(req)
	if isJSON {
		out, err := protojson.Marshal(req)
		return out, wire.ContentTypeJSON, err
	}
	out, err := proto.Marshal(req)
	return out, wire.ContentTypeProto, err
}

func decodeTracePayload(body []byte, contentType string) (*coltracepb.ExportTraceServiceRequest, error) {
	req := new(coltracepb.ExportTraceServiceRequest)
	if strings.Contains(strings.ToLower(contentType), "json") {
		if err := protojson.Unmarshal(body, req); err != nil {
			return nil, err
		}
		return req, nil
	}
	if err := proto.Unmarshal(body, req); err != nil {
		return nil, err
	}
	return req, nil
}

func stripTraceContent(req *coltracepb.ExportTraceServiceRequest) {
	for _, rs := range req.GetResourceSpans() {
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				if span == nil {
					continue
				}
				// The status message is read before the attribute filter so
				// the replacement category is still available.
				stripSpanStatus(span)
				span.Attributes = filterContentAttributes(span.GetAttributes())
				span.Events = stripSpanEvents(span.GetEvents())
			}
		}
	}
}

// stripSpanStatus replaces an error span's status message, which the SDK sets
// from the same raw provider error string it puts on the exception event
// (go/agento11y/client.go). Dropping the event and keeping the status would
// leave prompt fragments and tool output echoed by a provider 4xx on the
// forwarded span. The span's own error.category is the replacement when
// present, mirroring what the SDK writes under a reduced mode.
func stripSpanStatus(span *tracepb.Span) {
	status := span.GetStatus()
	if status == nil || status.GetMessage() == "" {
		return
	}
	if status.GetCode() != tracepb.Status_STATUS_CODE_ERROR {
		// A non-error status carries no meaning in the message field.
		status.Message = ""
		return
	}
	if category := stringAttribute(span.GetAttributes(), contentcapture.ErrorCategoryAttributeKey); category != "" {
		status.Message = category
		return
	}
	status.Message = contentcapture.StrippedCallError
}

// stringAttribute returns the string value of one span attribute, or "".
func stringAttribute(attrs []*commonpb.KeyValue, key string) string {
	for _, kv := range attrs {
		if kv.GetKey() == key {
			return kv.GetValue().GetStringValue()
		}
	}
	return ""
}

// filterContentAttributes drops the content-bearing attributes, in place, from
// one span's or one event's attribute list.
func filterContentAttributes(attrs []*commonpb.KeyValue) []*commonpb.KeyValue {
	return slices.DeleteFunc(attrs, func(kv *commonpb.KeyValue) bool {
		return kv == nil || contentcapture.IsTraceContentAttribute(kv.GetKey())
	})
}

func stripSpanEvents(events []*tracepb.Span_Event) []*tracepb.Span_Event {
	out := events[:0]
	for _, e := range events {
		if e == nil {
			continue
		}
		if e.GetName() == contentcapture.ExceptionEventName {
			continue
		}
		e.Attributes = filterContentAttributes(e.GetAttributes())
		out = append(out, e)
	}
	return out
}

// post issues a best-effort POST. Failures are recorded, never returned: a
// Cloud error must not affect the already-completed local store or the child's
// ack. The outcome lands in forwardStatus, which is the only channel that
// reaches a user without debug logging enabled.
func (l *forwardLoader) post(url, contentType, contentEncoding string, headers map[string]string, body []byte, label string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		l.recordFailuref(label, "build request for %s: %v", url, err)
		return
	}
	req.Header.Set("Content-Type", contentType)
	if contentEncoding != "" {
		req.Header.Set("Content-Encoding", contentEncoding)
	}
	req.Header.Set(ForwardMarkerHeader, "1")
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := l.client.Do(req)
	if err != nil {
		l.recordFailuref(label, "POST %s: %v", url, err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		// The body names the offending field when ingest validation rejects a
		// stripped payload, which is the only way a drift between this
		// daemon's field list and the SDK's becomes diagnosable.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		l.recordFailuref(label, "POST %s status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(snippet)))
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	l.recordSuccess(label)
}

// isForwardedRequest reports whether an inbound payload already came from
// another daemon's forwarder, or from an in-process producer that marked it.
// Relaying it again would loop between two daemons pointed at each other, or
// between one daemon and itself when the ingest endpoint is hand-set to this
// daemon's own address.
func isForwardedRequest(r *http.Request) bool {
	return r.Header.Get(ForwardMarkerHeader) != ""
}

// otlpSignalFromPath maps an OTLP receiver path to its signal name.
func otlpSignalFromPath(path string) string {
	switch {
	case strings.HasSuffix(path, "/traces"):
		return "traces"
	case strings.HasSuffix(path, "/metrics"):
		return "metrics"
	default:
		return ""
	}
}
