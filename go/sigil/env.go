package sigil

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// canonical SIGIL_* env-var names.
const (
	envEndpoint           = "SIGIL_ENDPOINT"
	envProtocol           = "SIGIL_PROTOCOL"
	envInsecure           = "SIGIL_INSECURE"
	envHeaders            = "SIGIL_HEADERS"
	envAuthMode           = "SIGIL_AUTH_MODE"
	envAuthTenantID       = "SIGIL_AUTH_TENANT_ID"
	envAuthToken          = "SIGIL_AUTH_TOKEN"
	envAgentName          = "SIGIL_AGENT_NAME"
	envAgentVersion       = "SIGIL_AGENT_VERSION"
	envUserID             = "SIGIL_USER_ID"
	envTags               = "SIGIL_TAGS"
	envContentCaptureMode = "SIGIL_CONTENT_CAPTURE_MODE"
	envDebug              = "SIGIL_DEBUG"
)

// envLookup resolves canonical SIGIL_* env vars from os.Environ unless a
// caller-supplied lookup is provided (used by tests).
type envLookup func(string) (string, bool)

func defaultLookup(key string) (string, bool) { return os.LookupEnv(key) }

// ConfigFromEnv returns a Config built from canonical SIGIL_* env vars layered
// on top of DefaultConfig. This is a debugging / advanced helper — most callers
// should construct a Client via NewClient which performs the same resolution
// internally.
func ConfigFromEnv() (Config, error) {
	return resolveFromEnv(defaultLookup, DefaultConfig())
}

// resolveFromEnv applies env overrides onto the supplied baseline. Invalid
// values (bad SIGIL_AUTH_MODE, etc.) are skipped — the base value is kept
// and the per-field error is returned via errors.Join, so one typo cannot
// discard the rest of the env layer.
func resolveFromEnv(lookup envLookup, base Config) (Config, error) {
	if lookup == nil {
		lookup = defaultLookup
	}
	cfg := base
	var errs []error

	if v, ok := envTrimmed(lookup, envEndpoint); ok {
		cfg.GenerationExport.Endpoint = v
	}
	if v, ok := envTrimmed(lookup, envProtocol); ok {
		cfg.GenerationExport.Protocol = GenerationExportProtocol(strings.ToLower(v))
	}
	if v, ok := envTrimmed(lookup, envInsecure); ok {
		b := parseBool(v)
		cfg.GenerationExport.Insecure = &b
	}
	if v, ok := envTrimmed(lookup, envHeaders); ok {
		cfg.GenerationExport.Headers = parseCSVKV(v)
	}

	auth := cfg.GenerationExport.Auth
	if v, ok := envTrimmed(lookup, envAuthMode); ok {
		mode := ExportAuthMode(strings.ToLower(v))
		if !validAuthMode(mode) {
			errs = append(errs, fmt.Errorf("sigil: invalid SIGIL_AUTH_MODE %q", v))
		} else {
			auth.Mode = mode
		}
	}
	if v, ok := envTrimmed(lookup, envAuthTenantID); ok {
		auth.TenantID = v
	}
	if v, ok := envTrimmed(lookup, envAuthToken); ok {
		// Set both fields; resolveHeadersWithAuth uses only the one matching
		// the final mode. Lets env's token fill a caller-supplied mode
		// without env declaring SIGIL_AUTH_MODE.
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

	if v, ok := envTrimmed(lookup, envAgentName); ok {
		cfg.AgentName = v
	}
	if v, ok := envTrimmed(lookup, envAgentVersion); ok {
		cfg.AgentVersion = v
	}
	if v, ok := envTrimmed(lookup, envUserID); ok {
		cfg.UserID = v
	}
	if v, ok := envTrimmed(lookup, envTags); ok {
		cfg.Tags = parseCSVKV(v)
	}

	if v, ok := envTrimmed(lookup, envContentCaptureMode); ok {
		mode, err := parseContentCaptureMode(v)
		if err != nil {
			errs = append(errs, err)
		} else {
			cfg.ContentCapture = mode
		}
	}

	if v, ok := envTrimmed(lookup, envDebug); ok {
		b := parseBool(v)
		cfg.Debug = &b
	}

	return cfg, errors.Join(errs...)
}

func envTrimmed(lookup envLookup, key string) (string, bool) {
	raw, ok := lookup(key)
	if !ok {
		return "", false
	}
	val := strings.TrimSpace(raw)
	if val == "" {
		return "", false
	}
	return val, true
}

func parseBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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

func parseContentCaptureMode(v string) (ContentCaptureMode, error) {
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
		return ContentCaptureModeDefault, fmt.Errorf("sigil: invalid SIGIL_CONTENT_CAPTURE_MODE %q", v)
	}
}

func validAuthMode(m ExportAuthMode) bool {
	switch m {
	case ExportAuthModeNone, ExportAuthModeTenant, ExportAuthModeBearer, ExportAuthModeBasic:
		return true
	}
	return false
}
