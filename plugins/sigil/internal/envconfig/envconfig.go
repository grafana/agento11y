// Package envconfig collects small helpers for reading the sigil plugin's
// canonical SIGIL_* environment variables.
package envconfig

import (
	"log"
	"os"
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

// EnvOr returns the value of key if non-empty (after trimming), else fallback.
func EnvOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
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
	for _, pair := range strings.Split(s, ",") {
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
