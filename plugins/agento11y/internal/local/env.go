package local

import (
	"os"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/capturemode"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

// LaunchEnv encodes the env-var contract between the agento11y launcher and
// a local-mode agent process. Endpoint and OTLPEndpoint are required;
// the agent reads them via the branded ENDPOINT and
// OTEL_EXPORTER_OTLP_ENDPOINT families (either spelling).
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
		if envconfig.ParseBool(os.Getenv(capturemode.LaunchDisabledEnv)) {
			return envconfig.WithoutCaptureEndpoints(env)
		}
		return env
	}
	return e.Apply(env)
}

// Apply returns env with local-mode overrides applied. The ENDPOINT,
// OTEL_EXPORTER_OTLP_ENDPOINT, and CONTENT_CAPTURE_MODE families are always
// overridden — under both branded spellings, so an inherited AGENTO11Y_* or
// SIGIL_* Cloud value can never leak past local mode: the agent points at
// the local receiver and always captures full content on this machine. The
// configured capture mode is a Cloud-forwarding setting that applies to
// non-local sessions, so any value in config.env is kept on disk but never
// downgrades local capture. The AUTH_TENANT_ID and AUTH_TOKEN families are
// only filled when the user hasn't already configured either spelling, so a
// user with Cloud credentials in their shell doesn't get them clobbered by
// `agento11y <agent> --local`.
//
// Placeholder auth values are injected so existing hook code (which
// short-circuits when the tenant / token families are empty) still exports
// to the local receiver. The local server doesn't validate auth; any
// non-empty value works.
func (e LaunchEnv) Apply(env []string) []string {
	overrides := map[string]string{}
	for suffix, v := range map[string]string{
		"ENDPOINT":                    e.Endpoint,
		"OTEL_EXPORTER_OTLP_ENDPOINT": e.OTLPEndpoint,
		"CONTENT_CAPTURE_MODE":        "full",
	} {
		overrides[envconfig.PreferredKey(suffix)] = v
		overrides[envconfig.LegacyKey(suffix)] = v
	}
	defaultFamilies := []string{"AUTH_TENANT_ID", "AUTH_TOKEN"}
	defaultKeys := map[string]string{}
	for _, suffix := range defaultFamilies {
		defaultKeys[envconfig.PreferredKey(suffix)] = suffix
		defaultKeys[envconfig.LegacyKey(suffix)] = suffix
	}
	keptFamilies := map[string]bool{}
	out := make([]string, 0, len(env)+len(overrides)+len(defaultKeys))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			out = append(out, kv)
			continue
		}
		if _, ok := overrides[key]; ok {
			continue
		}
		if family, ok := defaultKeys[key]; ok {
			if strings.TrimSpace(value) == "" {
				continue
			}
			keptFamilies[family] = true
		}
		out = append(out, kv)
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	for _, suffix := range defaultFamilies {
		if !keptFamilies[suffix] {
			out = append(out, envconfig.PreferredKey(suffix)+"="+envconfig.LocalAuthPlaceholder)
			out = append(out, envconfig.LegacyKey(suffix)+"="+envconfig.LocalAuthPlaceholder)
		}
	}
	return out
}

// hookOverrideSuffixes are the alias families Apply rewrites. ApplyOS
// materializes only these so an in-process hook does not os.Setenv the rest
// of the inherited environment.
var hookOverrideSuffixes = []string{
	"ENDPOINT",
	"OTEL_EXPORTER_OTLP_ENDPOINT",
	"CONTENT_CAPTURE_MODE",
	"AUTH_TENANT_ID",
	"AUTH_TOKEN",
}

// ApplyOS writes the same overrides Apply produces into the current process
// environment. Hook dispatch uses this because hooks run in-process and
// cannot inherit a rewritten child environ the way `agento11y <agent>
// --local` does.
//
// ApplyOS only Setenv keys present in Apply's result. It does not unset an
// inherited empty preferred-spelling AUTH_* var when the family was kept via
// the legacy spelling. LookupEnv trims blanks and falls through, so that
// case still resolves the real credential.
func (e LaunchEnv) ApplyOS() {
	applied := map[string]string{}
	for _, kv := range e.Apply(os.Environ()) {
		if k, v, ok := strings.Cut(kv, "="); ok {
			applied[k] = v
		}
	}
	for _, suffix := range hookOverrideSuffixes {
		for _, key := range []string{envconfig.PreferredKey(suffix), envconfig.LegacyKey(suffix)} {
			if v, ok := applied[key]; ok {
				_ = os.Setenv(key, v)
			}
		}
	}
}
