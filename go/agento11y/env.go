package agento11y

import (
	"errors"
	"fmt"
	"os"
	"strings"
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
	return resolveFromEnv(defaultLookup, DefaultConfig())
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
