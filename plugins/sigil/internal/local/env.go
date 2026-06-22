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

// Apply returns env with local-mode overrides applied. SIGIL_ENDPOINT,
// SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT, and SIGIL_CONTENT_CAPTURE_MODE are
// always overridden: the agent points at the local receiver and always
// captures full content on this machine. The configured capture mode is a
// Cloud-forwarding setting that applies to non-local sessions, so any value
// in config.env is kept on disk but never downgrades local capture.
// SIGIL_AUTH_TENANT_ID and SIGIL_AUTH_TOKEN are only set when the user hasn't
// already configured them, so a user with Cloud credentials in their shell
// doesn't get them clobbered by `sigil <agent> --local`.
//
// Placeholder auth values are injected so existing hook code (which
// short-circuits when SIGIL_AUTH_TENANT_ID / SIGIL_AUTH_TOKEN are
// empty) still exports to the local receiver. The local server doesn't
// validate auth; any non-empty value works.
func (e LaunchEnv) Apply(env []string) []string {
	overrides := map[string]string{
		"SIGIL_ENDPOINT":                    e.Endpoint,
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": e.OTLPEndpoint,
		"SIGIL_CONTENT_CAPTURE_MODE":        "full",
	}
	defaults := map[string]string{
		"SIGIL_AUTH_TENANT_ID": "local",
		"SIGIL_AUTH_TOKEN":     "local",
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
