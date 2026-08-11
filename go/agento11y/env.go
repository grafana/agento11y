package agento11y

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// envPair is one logical config field readable under the preferred
// AGENTO11Y_* name with a SIGIL_* legacy fallback. Selection happens before
// parsing: a nonblank preferred value always wins, even when it later fails
// validation, so stale legacy config cannot silently resurface.
type envPair struct {
	preferred string
	legacy    string
}

func brandedPair(suffix string) envPair {
	return envPair{preferred: "AGENTO11Y_" + suffix, legacy: "SIGIL_" + suffix}
}

// preferredOnlyPair is a variable with no legacy spelling. envTrimmed skips a
// lookup miss, so the empty legacy name never resolves.
func preferredOnlyPair(suffix string) envPair {
	return envPair{preferred: "AGENTO11Y_" + suffix}
}

// canonical env-var names: preferred AGENTO11Y_* with SIGIL_* fallback.
var (
	envEndpoint     = brandedPair("ENDPOINT")
	envProtocol     = brandedPair("PROTOCOL")
	envInsecure     = brandedPair("INSECURE")
	envHeaders      = brandedPair("HEADERS")
	envAuthMode     = brandedPair("AUTH_MODE")
	envAuthTenantID = brandedPair("AUTH_TENANT_ID")
	envAuthToken    = brandedPair("AUTH_TOKEN")
	envAgentName    = brandedPair("AGENT_NAME")
	envAgentVersion = brandedPair("AGENT_VERSION")
	envUserID       = brandedPair("USER_ID")
	// envTags: comma-separated key=value pairs merged into generation export tags
	// and emitted on OTel spans/metrics as agento11y.tag.<key>. The two spellings are
	// never merged; the selected value is used whole.
	envTags                = brandedPair("TAGS")
	envContentCaptureMode  = brandedPair("CONTENT_CAPTURE_MODE")
	envDebug               = brandedPair("DEBUG")
	envRedactInputMessages = brandedPair("REDACT_INPUT_MESSAGES")
	// Hooks configuration is preferred-only: these names have no installed
	// base under SIGIL_*, and the Python SDK already ignores that prefix.
	envHooksEnabled   = preferredOnlyPair("HOOKS_ENABLED")
	envHooksPhases    = preferredOnlyPair("HOOKS_PHASES")
	envHooksTimeoutMS = preferredOnlyPair("HOOKS_TIMEOUT_MS")
	envHooksFailOpen  = preferredOnlyPair("HOOKS_FAIL_OPEN")
)

// envLookup resolves canonical env vars from os.Environ unless a
// caller-supplied lookup is provided (used by tests).
type envLookup func(string) (string, bool)

func defaultLookup(key string) (string, bool) { return os.LookupEnv(key) }

// ConfigFromEnv returns a Config built from canonical AGENTO11Y_* env vars
// (with SIGIL_* fallbacks) layered on top of DefaultConfig. This is a
// debugging / advanced helper — most callers should construct a Client via
// NewClient which performs the same resolution internally.
func ConfigFromEnv() (Config, error) {
	cfg, err := resolveFromEnv(defaultLookup, DefaultConfig())
	// DefaultConfig leaves Hooks zero so NewClient's env layer is not shadowed.
	// Fill the hook defaults here so the returned Config is complete.
	cfg.Hooks = mergeHooksConfig(defaultHooksConfig(), cfg.Hooks)
	return cfg, err
}

// resolveFromEnv applies env overrides onto the supplied baseline. Invalid
// values (bad AUTH_MODE, etc.) are skipped — the base value is kept
// and the per-field error is returned via errors.Join, so one typo cannot
// discard the rest of the env layer.
func resolveFromEnv(lookup envLookup, base Config) (Config, error) {
	if lookup == nil {
		lookup = defaultLookup
	}
	cfg := base
	var errs []error

	if v, _, ok := envTrimmed(lookup, envEndpoint); ok {
		cfg.GenerationExport.Endpoint = v
		// Hook, experiment and rating calls go to API.Endpoint. One endpoint
		// variable feeds both, so an env-enabled hook reaches the configured
		// server instead of localhost. An endpoint the caller moved off the schema
		// default is kept, here and again in mergeAPIConfig. Matches the Python SDK.
		if cfg.API.Endpoint == "" || cfg.API.Endpoint == defaultAPIEndpoint {
			cfg.API.Endpoint = v
		}
	}
	if v, _, ok := envTrimmed(lookup, envProtocol); ok {
		cfg.GenerationExport.Protocol = GenerationExportProtocol(strings.ToLower(v))
	}
	if v, _, ok := envTrimmed(lookup, envInsecure); ok {
		b := parseBool(v)
		cfg.GenerationExport.Insecure = &b
	}
	if v, _, ok := envTrimmed(lookup, envHeaders); ok {
		cfg.GenerationExport.Headers = parseCSVKV(v)
	}

	auth := cfg.GenerationExport.Auth
	if v, key, ok := envTrimmed(lookup, envAuthMode); ok {
		mode := ExportAuthMode(strings.ToLower(v))
		if !validAuthMode(mode) {
			errs = append(errs, fmt.Errorf("agento11y: invalid %s %q", key, v))
		} else {
			auth.Mode = mode
		}
	}
	if v, _, ok := envTrimmed(lookup, envAuthTenantID); ok {
		auth.TenantID = v
	}
	if v, _, ok := envTrimmed(lookup, envAuthToken); ok {
		// Set both fields; resolveHeadersWithAuth uses only the one matching
		// the final mode. Lets env's token fill a caller-supplied mode
		// without env declaring an AUTH_MODE.
		if auth.BearerToken == "" {
			auth.BearerToken = v
		}
		if auth.BasicPassword == "" {
			auth.BasicPassword = v
		}
	}
	if auth.Mode == ExportAuthModeBasic && auth.BasicUser == "" && auth.TenantID != "" {
		auth.BasicUser = auth.TenantID
	}
	cfg.GenerationExport.Auth = auth

	if v, _, ok := envTrimmed(lookup, envAgentName); ok {
		cfg.AgentName = v
	}
	if v, _, ok := envTrimmed(lookup, envAgentVersion); ok {
		cfg.AgentVersion = v
	}
	if v, _, ok := envTrimmed(lookup, envUserID); ok {
		cfg.UserID = v
	}
	if v, _, ok := envTrimmed(lookup, envTags); ok {
		cfg.Tags = parseCSVKV(v)
	}

	if v, key, ok := envTrimmed(lookup, envContentCaptureMode); ok {
		mode, err := parseContentCaptureMode(key, v)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.ContentCapture = mode
		}
	}

	if v, _, ok := envTrimmed(lookup, envDebug); ok {
		b := parseBool(v)
		cfg.Debug = &b
	}

	// Hooks. One variable per field; NewClient layers caller values on top.
	// The timeout is in milliseconds because that is the unit of the wire
	// header. An unusable value is joined into the returned error and skipped,
	// so one typo cannot discard the other three.
	hooks := cfg.Hooks
	if v, key, ok := envTrimmed(lookup, envHooksEnabled); ok {
		if b, valid := parseStrictBool(v); valid {
			hooks.Enabled = &b
		} else {
			errs = append(errs, fmt.Errorf("agento11y: invalid %s %q", key, v))
		}
	}
	if v, key, ok := envTrimmed(lookup, envHooksPhases); ok {
		phases, err := parseHookPhases(key, v)
		if err != nil {
			errs = append(errs, err)
		}
		// The recognised entries apply even when a sibling entry was rejected.
		if len(phases) > 0 {
			hooks.Phases = phases
		}
	}
	if v, key, ok := envTrimmed(lookup, envHooksTimeoutMS); ok {
		ms, err := parseHookTimeoutMS(key, v)
		if err != nil {
			errs = append(errs, err)
		} else {
			hooks.Timeout = time.Duration(ms) * time.Millisecond
		}
	}
	if v, key, ok := envTrimmed(lookup, envHooksFailOpen); ok {
		if b, valid := parseStrictBool(v); valid {
			hooks.FailOpen = &b
		} else {
			errs = append(errs, fmt.Errorf("agento11y: invalid %s %q", key, v))
		}
	}
	cfg.Hooks = hooks

	return cfg, errors.Join(errs...)
}

// envTrimmed selects the pair's first nonblank value (preferred, then legacy)
// and returns it with the env-var name it came from, so validation errors can
// name the key the user actually set.
func envTrimmed(lookup envLookup, pair envPair) (value, key string, ok bool) {
	for _, k := range []string{pair.preferred, pair.legacy} {
		raw, found := lookup(k)
		if !found {
			continue
		}
		val := strings.TrimSpace(raw)
		if val == "" {
			continue
		}
		return val, k, true
	}
	return "", "", false
}

func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// parseStrictBool accepts the same true tokens as parseBool plus the matching
// false tokens, and reports whether the input was recognised. Use this when an
// invalid value must not silently fall through to false.
func parseStrictBool(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func parseCSVKV(raw string) map[string]string {
	out := map[string]string{}
	for part := range strings.SplitSeq(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		idx := strings.IndexByte(part, '=')
		if idx <= 0 {
			continue
		}
		k := strings.TrimSpace(part[:idx])
		v := strings.TrimSpace(part[idx+1:])
		if k != "" {
			out[k] = v
		}
	}
	return out
}

// parseHookTimeoutMS parses AGENTO11Y_HOOKS_TIMEOUT_MS. It rejects
// non-integers, zero and negatives because EvaluateHook reads a non-positive
// timeout as "use the default", and anything above maxHookTimeoutMS because the
// server would not honour it. strconv.Atoi also rejects the underscore digit
// groups Python's int() would accept, which keeps the three SDKs on one set.
func parseHookTimeoutMS(key, raw string) (int, error) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 || value > maxHookTimeoutMS {
		return 0, fmt.Errorf("agento11y: invalid %s %q", key, raw)
	}
	return value, nil
}

// parseHookPhases parses a comma-separated phase list. Entries are trimmed,
// lowercased and deduplicated in first-seen order.
//
// An unknown entry is dropped and reported, and the recognised entries still
// apply. Rejecting the whole list instead would fall back to the default
// {preflight}, so a typo in "postflight,bogus" would start preflight
// enforcement the operator never asked for and skip the phase they did.
func parseHookPhases(key, raw string) ([]HookPhase, error) {
	var out []HookPhase
	var unknown []string
	for part := range strings.SplitSeq(raw, ",") {
		phase := HookPhase(strings.ToLower(strings.TrimSpace(part)))
		if phase == "" {
			continue
		}
		if phase != HookPhasePreflight && phase != HookPhasePostflight {
			unknown = append(unknown, string(phase))
			continue
		}
		if !slices.Contains(out, phase) {
			out = append(out, phase)
		}
	}
	switch {
	case len(unknown) > 0:
		return out, fmt.Errorf("agento11y: ignoring unknown %s entries %q", key, strings.Join(unknown, ","))
	case len(out) == 0:
		return nil, fmt.Errorf("agento11y: invalid %s %q", key, raw)
	}
	return out, nil
}

func parseContentCaptureMode(key, v string) (ContentCaptureMode, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "full":
		return ContentCaptureModeFull, nil
	case "no_tool_content":
		return ContentCaptureModeNoToolContent, nil
	case "metadata_only":
		return ContentCaptureModeMetadataOnly, nil
	case "full_with_metadata_spans":
		return ContentCaptureModeFullWithMetadataSpans, nil
	default:
		return ContentCaptureModeDefault, fmt.Errorf("agento11y: invalid %s %q", key, v)
	}
}

func validAuthMode(m ExportAuthMode) bool {
	switch m {
	case ExportAuthModeNone, ExportAuthModeTenant, ExportAuthModeBearer, ExportAuthModeBasic:
		return true
	}
	return false
}
