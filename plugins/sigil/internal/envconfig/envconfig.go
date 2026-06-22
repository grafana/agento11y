// Package envconfig collects small helpers for reading the sigil plugin's
// canonical SIGIL_* environment variables.
package envconfig

import (
	"log"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/grafana/sigil-sdk/go/sigil"
)

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
// checks or SDK client construction.
func ApplyLocalAuthPlaceholders() {
	endpoint := os.Getenv("SIGIL_ENDPOINT")
	tenantID, authToken := LocalAuthPlaceholders(endpoint, os.Getenv("SIGIL_AUTH_TENANT_ID"), os.Getenv("SIGIL_AUTH_TOKEN"))
	if tenantID != os.Getenv("SIGIL_AUTH_TENANT_ID") {
		_ = os.Setenv("SIGIL_AUTH_TENANT_ID", tenantID)
	}
	if authToken != os.Getenv("SIGIL_AUTH_TOKEN") {
		_ = os.Setenv("SIGIL_AUTH_TOKEN", authToken)
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
// SIGIL_CONTENT_CAPTURE_MODE. Empty values and the Default zero-value enum
// resolve to metadata_only so the explicit fall-back is the same regardless
// of whether the caller forgot to set the variable or set it to an unknown
// label.
//
// Invalid (non-empty, unparseable) values are reported via logger when
// non-nil so a typo doesn't silently downgrade behaviour. Hooks must not
// write to stderr, so callers pass their adapter logger here — the helper
// never touches stderr itself.
func ResolveContentMode(logger *log.Logger) sigil.ContentCaptureMode {
	v := strings.TrimSpace(os.Getenv("SIGIL_CONTENT_CAPTURE_MODE"))
	if v == "" {
		return sigil.ContentCaptureModeMetadataOnly
	}
	var mode sigil.ContentCaptureMode
	if err := mode.UnmarshalText([]byte(v)); err != nil {
		if logger != nil {
			logger.Printf("config: unknown SIGIL_CONTENT_CAPTURE_MODE=%q; using metadata_only", v)
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

// ResolveGuards reads SIGIL_GUARDS_ENABLED / _TIMEOUT_MS / _FAIL_OPEN and
// returns the effective guard configuration. Unset or empty values fall back
// to the defaults (off / 1500ms / fail-open). Unrecognised boolean values and
// non-numeric, zero, or negative timeout values are reported via logger when
// non-nil and fall back to the default — matching pi's resolveGuards behaviour
// so the shared config.env produces identical results across plugins. Hooks
// must not write to stderr, so callers pass their adapter logger here.
func ResolveGuards(logger *log.Logger) GuardsConfig {
	cfg := GuardsConfig{
		Enabled:   resolveGuardsBool(logger, "SIGIL_GUARDS_ENABLED", defaultGuardsEnabled),
		TimeoutMs: DefaultGuardsTimeoutMs,
		FailOpen:  resolveGuardsBool(logger, "SIGIL_GUARDS_FAIL_OPEN", defaultGuardsFailOpen),
	}
	if v := strings.TrimSpace(os.Getenv("SIGIL_GUARDS_TIMEOUT_MS")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			if logger != nil {
				logger.Printf("config: invalid SIGIL_GUARDS_TIMEOUT_MS=%q; using %d", v, DefaultGuardsTimeoutMs)
			}
		} else {
			cfg.TimeoutMs = n
		}
	}
	return cfg
}

func resolveGuardsBool(logger *log.Logger, key string, def bool) bool {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
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
