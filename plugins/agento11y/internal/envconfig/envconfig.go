// Package envconfig collects small helpers for reading the sigil plugin's
// branded environment variables. Every supported variable is an alias family:
// the preferred AGENTO11Y_<suffix> spelling with a SIGIL_<suffix> legacy
// fallback kept during the compatibility period.
package envconfig

import (
	"log"
	"maps"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/grafana/agento11y/go/sigil"
)

// PreferredKey and LegacyKey are the two spellings of one branded variable.
func PreferredKey(suffix string) string { return "AGENTO11Y_" + suffix }
func LegacyKey(suffix string) string    { return "SIGIL_" + suffix }

// AliasSuffixes is the launcher's supported alias families. Dotenv resolution
// materializes exactly these; keys outside this list keep exact-key semantics.
var AliasSuffixes = []string{
	"ENDPOINT",
	"PROTOCOL",
	"INSECURE",
	"HEADERS",
	"AUTH_MODE",
	"AUTH_TENANT_ID",
	"AUTH_TOKEN",
	"AGENT_NAME",
	"AGENT_VERSION",
	"USER_ID",
	"TAGS",
	"CONTENT_CAPTURE_MODE",
	"DEBUG",
	"REDACT_INPUT_MESSAGES",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"OTEL_EXPORTER_OTLP_INSECURE",
	"OTEL_AUTH_TOKEN",
	"GUARDS_ENABLED",
	"GUARDS_FAIL_OPEN",
	"GUARDS_TIMEOUT_MS",
	"AUTO_UPDATE",
	"USER_ID_SOURCE",
	"BIN",
	"COPILOT_HOOK_SURFACE",
}

// LookupEnv resolves a branded variable from the process env: the first
// nonblank of AGENTO11Y_<suffix>, SIGIL_<suffix>. Blank or whitespace-only
// values are treated as unset. The returned key names the spelling the value
// came from so diagnostics can report what the user actually set.
func LookupEnv(suffix string) (value, key string, ok bool) {
	for _, k := range []string{PreferredKey(suffix), LegacyKey(suffix)} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v, k, true
		}
	}
	return "", "", false
}

// Getenv returns the resolved branded value, or "" when neither spelling is
// set.
func Getenv(suffix string) string {
	v, _, _ := LookupEnv(suffix)
	return v
}

// SetBothEnv writes value under both spellings so old and new readers (and
// child processes) observe the same configuration.
func SetBothEnv(suffix, value string) {
	_ = os.Setenv(PreferredKey(suffix), value)
	_ = os.Setenv(LegacyKey(suffix), value)
}

// EnvSetter is the subset of testing.TB PinAliasEnvBlank needs. Declared
// locally so this package does not import testing.
type EnvSetter interface {
	Setenv(key, value string)
}

// PinAliasEnvBlank pins both spellings of every alias family to "" for the
// duration of a test. Materialization helpers (dotenv.ApplyEnv, SetBothEnv)
// write through os.Setenv with no cleanup, so without pinning a value from an
// earlier test in the same process would leak into later ones.
func PinAliasEnvBlank(t EnvSetter) {
	for _, suffix := range AliasSuffixes {
		t.Setenv(PreferredKey(suffix), "")
		t.Setenv(LegacyKey(suffix), "")
	}
}

// ExpandAliases returns updates with every branded key mirrored under its
// other spelling carrying the same value. Managed writers use this to write
// both names at once; an empty value deletes both. Keys already present in
// updates are not overwritten; non-branded keys pass through unchanged.
func ExpandAliases(updates map[string]string) map[string]string {
	out := make(map[string]string, len(updates)*2)
	maps.Copy(out, updates)
	for k, v := range updates {
		var mirror string
		if suffix, ok := strings.CutPrefix(k, "AGENTO11Y_"); ok {
			mirror = LegacyKey(suffix)
		} else if suffix, ok := strings.CutPrefix(k, "SIGIL_"); ok {
			mirror = PreferredKey(suffix)
		} else {
			continue
		}
		if _, exists := out[mirror]; !exists {
			out[mirror] = v
		}
	}
	return out
}

// ParseBool mirrors the SDK's parseBool whitelist (1/true/yes/on).
func ParseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// ParseBoolDefault parses a boolean config value, returning def when the value
// is empty or unrecognised. It honours the same 1/true/yes/on and
// 0/false/no/off whitelist as resolveGuardsBool, so callers that read SIGIL_*
// booleans from a dotenv map (rather than os.Getenv) get identical semantics.
func ParseBoolDefault(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// EnvOr returns the value of key if non-empty (after trimming), else fallback.
func EnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// IsLocalEndpoint reports whether endpoint points at the local receiver.
// Local URLs never need real Cloud credentials. The check uses URL parsing
// so attacker-controlled hostnames like localhost.attacker.com do not match.
func IsLocalEndpoint(endpoint string) bool {
	e := strings.TrimSpace(endpoint)
	if e == "" {
		return false
	}
	u, err := url.Parse(e)
	if err != nil || u.Scheme != "http" {
		return false
	}
	switch u.Hostname() {
	case "127.0.0.1", "::1", "localhost":
		return true
	default:
		return false
	}
}

// LocalAuthPlaceholders returns non-empty stand-in auth values for local
// endpoints. The local server does not validate auth, but hook/export code
// still expects these variables to be populated before it proceeds.
func LocalAuthPlaceholders(endpoint, tenantID, authToken string) (string, string) {
	if !IsLocalEndpoint(endpoint) {
		return tenantID, authToken
	}
	if strings.TrimSpace(tenantID) == "" {
		tenantID = "local"
	}
	if strings.TrimSpace(authToken) == "" {
		authToken = "local"
	}
	return tenantID, authToken
}

// ApplyLocalAuthPlaceholders writes local endpoint auth placeholders to the
// process environment. Use this after dotenv loading and before credential
// checks or SDK client construction. Any resolved (or placeholder) value is
// materialized under both branded spellings so a consumer that reads only one
// spelling — such as an older SDK — sees the same credential.
func ApplyLocalAuthPlaceholders() {
	endpoint := Getenv("ENDPOINT")
	tenantID, authToken := LocalAuthPlaceholders(endpoint, Getenv("AUTH_TENANT_ID"), Getenv("AUTH_TOKEN"))
	if tenantID != "" {
		SetBothEnv("AUTH_TENANT_ID", tenantID)
	}
	if authToken != "" {
		SetBothEnv("AUTH_TOKEN", authToken)
	}
}

// MissingEnvVars returns the keys of vars whose values are empty, in the
// order they appear in `order`. Keys not present in `vars` are ignored.
func MissingEnvVars(order []string, vars map[string]string) []string {
	var out []string
	for _, k := range order {
		if v, ok := vars[k]; ok && strings.TrimSpace(v) == "" {
			out = append(out, k)
		}
	}
	return out
}

// ParseExtraTags parses a comma-separated "key=value" string into a tag map.
// Malformed entries (empty keys, missing '=', empty values) are silently
// skipped. Empty input returns nil so callers can short-circuit on the
// zero-extras path.
func ParseExtraTags(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := make(map[string]string)
	for pair := range strings.SplitSeq(s, ",") {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" || v == "" {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ResolveContentMode returns the effective ContentCaptureMode from
// AGENTO11Y_CONTENT_CAPTURE_MODE (SIGIL_ fallback). Empty values and the
// Default zero-value enum resolve to metadata_only so the explicit fall-back
// is the same regardless of whether the caller forgot to set the variable or
// set it to an unknown label. An invalid preferred value never falls back to
// the legacy spelling.
//
// Invalid (non-empty, unparseable) values are reported via logger when
// non-nil so a typo doesn't silently downgrade behaviour. Hooks must not
// write to stderr, so callers pass their adapter logger here — the helper
// never touches stderr itself.
func ResolveContentMode(logger *log.Logger) sigil.ContentCaptureMode {
	v, key, ok := LookupEnv("CONTENT_CAPTURE_MODE")
	if !ok {
		return sigil.ContentCaptureModeMetadataOnly
	}
	var mode sigil.ContentCaptureMode
	if err := mode.UnmarshalText([]byte(v)); err != nil {
		if logger != nil {
			logger.Printf("config: unknown %s=%q; using metadata_only", key, v)
		}
		return sigil.ContentCaptureModeMetadataOnly
	}
	if mode == sigil.ContentCaptureModeDefault {
		return sigil.ContentCaptureModeMetadataOnly
	}
	return mode
}

// GuardsConfig is the resolved guard feature flags for a hook handler.
// Mirrors plugins/pi/src/config.ts::GuardsFeatureConfig so a single
// ~/.config/sigil/config.env drives both plugins.
type GuardsConfig struct {
	Enabled   bool
	TimeoutMs int
	FailOpen  bool
}

// DefaultGuardsTimeoutMs is the guard check timeout applied when
// SIGIL_GUARDS_TIMEOUT_MS is unset, non-numeric, or non-positive. Exported so
// callers writing config.env (sigil login, the local Settings page) can treat
// this value as "use the default" and omit the key.
const DefaultGuardsTimeoutMs = 1500

const (
	defaultGuardsEnabled  = false
	defaultGuardsFailOpen = true
)

// ResolveGuards reads the GUARDS_ENABLED / GUARDS_TIMEOUT_MS / GUARDS_FAIL_OPEN
// alias families and returns the effective guard configuration. Unset or empty
// values fall back to the defaults (off / 1500ms / fail-open). Unrecognised
// boolean values and non-numeric, zero, or negative timeout values are
// reported via logger when non-nil and fall back to the default — matching
// pi's resolveGuards behaviour so the shared config.env produces identical
// results across plugins. Hooks must not write to stderr, so callers pass
// their adapter logger here.
func ResolveGuards(logger *log.Logger) GuardsConfig {
	cfg := GuardsConfig{
		Enabled:   resolveGuardsBool(logger, "GUARDS_ENABLED", defaultGuardsEnabled),
		TimeoutMs: DefaultGuardsTimeoutMs,
		FailOpen:  resolveGuardsBool(logger, "GUARDS_FAIL_OPEN", defaultGuardsFailOpen),
	}
	if v, key, ok := LookupEnv("GUARDS_TIMEOUT_MS"); ok {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			if logger != nil {
				logger.Printf("config: invalid %s=%q; using %d", key, v, DefaultGuardsTimeoutMs)
			}
		} else {
			cfg.TimeoutMs = n
		}
	}
	return cfg
}

func resolveGuardsBool(logger *log.Logger, suffix string, def bool) bool {
	raw, key, ok := LookupEnv(suffix)
	if !ok {
		return def
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		if logger != nil {
			logger.Printf("config: invalid %s=%q; using %v", key, raw, def)
		}
		return def
	}
}
