package local

import (
	"os"
	"strings"
)

// LaunchEnv encodes the env-var contract between the sigil launcher and
// a local-mode agent process. Endpoint and OTLPEndpoint are required;
// the agent reads them via SIGIL_ENDPOINT and
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT.
type LaunchEnv struct {
	Endpoint     string
	OTLPEndpoint string
}

// Environ returns os.Environ with local-mode overrides applied when e is
// non-nil. Launchers should use this before exec so normal and local mode
// share one environment path.
func Environ(e *LaunchEnv) []string {
	env := os.Environ()
	if e == nil {
		return env
	}
	return e.Apply(env)
}

// Apply returns env with local-mode overrides applied. SIGIL_ENDPOINT
// and SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT are always overridden so the
// agent points at the local receiver. SIGIL_AUTH_TENANT_ID /
// SIGIL_AUTH_TOKEN / SIGIL_CONTENT_CAPTURE_MODE are only set when the
// user hasn't already configured them — a user with Cloud credentials
// in their shell shouldn't get them clobbered by `sigil <agent>
// --local`.
//
// Placeholder auth values are injected so existing hook code (which
// short-circuits when SIGIL_AUTH_TENANT_ID / SIGIL_AUTH_TOKEN are
// empty) still exports to the local receiver. The local server doesn't
// validate auth; any non-empty value works.
func (e LaunchEnv) Apply(env []string) []string {
	overrides := map[string]string{
		"SIGIL_ENDPOINT":                    e.Endpoint,
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": e.OTLPEndpoint,
	}
	defaults := map[string]string{
		"SIGIL_AUTH_TENANT_ID":       "local",
		"SIGIL_AUTH_TOKEN":           "local",
		"SIGIL_CONTENT_CAPTURE_MODE": "full",
	}
	keptDefaults := map[string]bool{}
	out := make([]string, 0, len(env)+len(overrides)+len(defaults))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if _, ok := overrides[key]; ok {
			continue
		}
		if _, ok := defaults[key]; ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			keptDefaults[key] = true
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	for k, v := range defaults {
		if !keptDefaults[k] {
			out = append(out, k+"="+v)
		}
	}
	return out
}
