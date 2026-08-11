package agento11y

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func mapLookup(env map[string]string) envLookup {
	return func(k string) (string, bool) {
		v, ok := env[k]
		return v, ok
	}
}

// TestResolveFromEnv covers the env-only resolution layer: every supported
// variable under both its AGENTO11Y_* and SIGIL_* spellings, malformed-input
// handling, and the partial-config contract that invalid fields are skipped
// without dropping the valid ones.
func TestResolveFromEnv(t *testing.T) {
	cases := []struct {
		name            string
		env             map[string]string
		base            *Config
		wantErr         bool
		wantErrContains string
		check           func(t *testing.T, cfg Config)
	}{
		{
			name: "endpoint env fills the default API endpoint",
			env:  map[string]string{"AGENTO11Y_ENDPOINT": "https://collector.example.com"},
			check: func(t *testing.T, cfg Config) {
				if cfg.API.Endpoint != "https://collector.example.com" {
					t.Errorf("API.Endpoint=%q want the env endpoint", cfg.API.Endpoint)
				}
			},
		},
		{
			name: "endpoint env fills an empty API endpoint",
			env:  map[string]string{"AGENTO11Y_ENDPOINT": "https://collector.example.com"},
			base: &Config{},
			check: func(t *testing.T, cfg Config) {
				if cfg.API.Endpoint != "https://collector.example.com" {
					t.Errorf("API.Endpoint=%q want the env endpoint", cfg.API.Endpoint)
				}
			},
		},
		{
			name: "explicit API endpoint wins over env",
			env:  map[string]string{"AGENTO11Y_ENDPOINT": "https://environment.example.com"},
			base: &Config{API: APIConfig{Endpoint: "https://caller.example.com"}},
			check: func(t *testing.T, cfg Config) {
				if cfg.API.Endpoint != "https://caller.example.com" {
					t.Errorf("API.Endpoint=%q want the caller endpoint", cfg.API.Endpoint)
				}
			},
		},
		{
			name: "no env uses defaults",
			env:  map[string]string{},
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "localhost:4317" {
					t.Errorf("Endpoint=%q want localhost:4317", cfg.GenerationExport.Endpoint)
				}
				if cfg.GenerationExport.Protocol != GenerationExportProtocolGRPC {
					t.Errorf("Protocol=%q want grpc", cfg.GenerationExport.Protocol)
				}
			},
		},
		{
			name: "transport from env",
			env: map[string]string{
				"SIGIL_ENDPOINT": "https://env:4318",
				"SIGIL_PROTOCOL": "http",
				"SIGIL_INSECURE": "true",
				"SIGIL_HEADERS":  "X-A=1,X-B=two",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "https://env:4318" {
					t.Errorf("Endpoint=%q", cfg.GenerationExport.Endpoint)
				}
				if cfg.GenerationExport.Protocol != GenerationExportProtocolHTTP {
					t.Errorf("Protocol=%q", cfg.GenerationExport.Protocol)
				}
				if cfg.GenerationExport.Insecure == nil || !*cfg.GenerationExport.Insecure {
					t.Errorf("Insecure=%v want true", cfg.GenerationExport.Insecure)
				}
				if cfg.GenerationExport.Headers["X-A"] != "1" || cfg.GenerationExport.Headers["X-B"] != "two" {
					t.Errorf("Headers=%v", cfg.GenerationExport.Headers)
				}
			},
		},
		{
			name: "basic auth from env",
			env: map[string]string{
				"SIGIL_AUTH_MODE":      "basic",
				"SIGIL_AUTH_TENANT_ID": "42",
				"SIGIL_AUTH_TOKEN":     "glc_xxx",
			},
			check: func(t *testing.T, cfg Config) {
				auth := cfg.GenerationExport.Auth
				if auth.Mode != ExportAuthModeBasic {
					t.Errorf("Mode=%q", auth.Mode)
				}
				if auth.TenantID != "42" {
					t.Errorf("TenantID=%q", auth.TenantID)
				}
				if auth.BasicPassword != "glc_xxx" {
					t.Errorf("BasicPassword=%q", auth.BasicPassword)
				}
			},
		},
		{
			name: "bearer auth from env",
			env: map[string]string{
				"SIGIL_AUTH_MODE":  "bearer",
				"SIGIL_AUTH_TOKEN": "tok",
			},
			check: func(t *testing.T, cfg Config) {
				auth := cfg.GenerationExport.Auth
				if auth.Mode != ExportAuthModeBearer {
					t.Errorf("Mode=%q", auth.Mode)
				}
				if auth.BearerToken != "tok" {
					t.Errorf("BearerToken=%q", auth.BearerToken)
				}
			},
		},
		{
			name:    "invalid auth mode returns error",
			env:     map[string]string{"SIGIL_AUTH_MODE": "garbage"},
			wantErr: true,
		},
		{
			name: "invalid auth mode preserves other valid env",
			env: map[string]string{
				"SIGIL_AUTH_MODE":  "Bearrer",
				"SIGIL_ENDPOINT":   "valid.example:4318",
				"SIGIL_AGENT_NAME": "valid-agent",
				"SIGIL_USER_ID":    "alice",
			},
			wantErr: true,
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "valid.example:4318" {
					t.Errorf("Endpoint=%q want valid.example:4318 (preserved despite auth-mode typo)", cfg.GenerationExport.Endpoint)
				}
				if cfg.AgentName != "valid-agent" {
					t.Errorf("AgentName=%q (preserved despite auth-mode typo)", cfg.AgentName)
				}
				if cfg.UserID != "alice" {
					t.Errorf("UserID=%q (preserved despite auth-mode typo)", cfg.UserID)
				}
			},
		},
		{
			// resolveHeadersWithAuth ignores TenantID for mode=none, so the
			// stray var is harmless. See TestNewClient_EnvHandling for the
			// end-to-end "doesn't panic" guarantee.
			name: "stray SIGIL_AUTH_TENANT_ID keeps env mode at none",
			env: map[string]string{
				"SIGIL_AUTH_TENANT_ID": "42",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Auth.Mode != ExportAuthModeNone {
					t.Errorf("Mode=%q want none (env did not override)", cfg.GenerationExport.Auth.Mode)
				}
			},
		},
		{
			name: "agent / user / tags / debug from env",
			env: map[string]string{
				"SIGIL_AGENT_NAME":    "planner",
				"SIGIL_AGENT_VERSION": "1.2.3",
				"SIGIL_USER_ID":       "alice@example.com",
				"SIGIL_TAGS":          "service=orchestrator,env=prod",
				"SIGIL_DEBUG":         "true",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.AgentName != "planner" {
					t.Errorf("AgentName=%q", cfg.AgentName)
				}
				if cfg.AgentVersion != "1.2.3" {
					t.Errorf("AgentVersion=%q", cfg.AgentVersion)
				}
				if cfg.UserID != "alice@example.com" {
					t.Errorf("UserID=%q", cfg.UserID)
				}
				if cfg.Tags["service"] != "orchestrator" || cfg.Tags["env"] != "prod" {
					t.Errorf("Tags=%v", cfg.Tags)
				}
				if cfg.Debug == nil || !*cfg.Debug {
					t.Errorf("Debug=%v want true", cfg.Debug)
				}
			},
		},
		{
			name: "content capture mode from env",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "metadata_only"},
			check: func(t *testing.T, cfg Config) {
				if cfg.ContentCapture != ContentCaptureModeMetadataOnly {
					t.Errorf("ContentCapture=%v", cfg.ContentCapture)
				}
			},
		},
		{
			name: "full_with_metadata_spans content capture mode from env",
			env:  map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "full_with_metadata_spans"},
			check: func(t *testing.T, cfg Config) {
				if cfg.ContentCapture != ContentCaptureModeFullWithMetadataSpans {
					t.Errorf("ContentCapture=%v", cfg.ContentCapture)
				}
			},
		},
		{
			name:    "invalid content capture mode returns error",
			env:     map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "bogus"},
			wantErr: true,
		},
		{
			name: "invalid content capture mode preserves other valid env",
			env: map[string]string{
				"SIGIL_CONTENT_CAPTURE_MODE": "bogus",
				"SIGIL_ENDPOINT":             "valid.example:4318",
				"SIGIL_AGENT_NAME":           "valid-agent",
			},
			wantErr: true,
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "valid.example:4318" {
					t.Errorf("Endpoint=%q (preserved despite content-capture typo)", cfg.GenerationExport.Endpoint)
				}
				if cfg.AgentName != "valid-agent" {
					t.Errorf("AgentName=%q (preserved despite content-capture typo)", cfg.AgentName)
				}
			},
		},
		{
			name: "preferred-only env matches legacy-only resolution",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT":             "https://env:4318",
				"AGENTO11Y_PROTOCOL":             "http",
				"AGENTO11Y_INSECURE":             "true",
				"AGENTO11Y_HEADERS":              "X-A=1,X-B=two",
				"AGENTO11Y_AUTH_MODE":            "basic",
				"AGENTO11Y_AUTH_TENANT_ID":       "42",
				"AGENTO11Y_AUTH_TOKEN":           "glc_xxx",
				"AGENTO11Y_AGENT_NAME":           "planner",
				"AGENTO11Y_AGENT_VERSION":        "1.2.3",
				"AGENTO11Y_USER_ID":              "alice@example.com",
				"AGENTO11Y_TAGS":                 "service=orchestrator,env=prod",
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "metadata_only",
				"AGENTO11Y_DEBUG":                "true",
			},
			check: func(t *testing.T, cfg Config) {
				legacy := map[string]string{
					"SIGIL_ENDPOINT":             "https://env:4318",
					"SIGIL_PROTOCOL":             "http",
					"SIGIL_INSECURE":             "true",
					"SIGIL_HEADERS":              "X-A=1,X-B=two",
					"SIGIL_AUTH_MODE":            "basic",
					"SIGIL_AUTH_TENANT_ID":       "42",
					"SIGIL_AUTH_TOKEN":           "glc_xxx",
					"SIGIL_AGENT_NAME":           "planner",
					"SIGIL_AGENT_VERSION":        "1.2.3",
					"SIGIL_USER_ID":              "alice@example.com",
					"SIGIL_TAGS":                 "service=orchestrator,env=prod",
					"SIGIL_CONTENT_CAPTURE_MODE": "metadata_only",
					"SIGIL_DEBUG":                "true",
				}
				legacyCfg, err := resolveFromEnv(mapLookup(legacy), DefaultConfig())
				if err != nil {
					t.Fatalf("legacy resolve: %v", err)
				}
				if cfg.GenerationExport.Endpoint != legacyCfg.GenerationExport.Endpoint {
					t.Errorf("Endpoint=%q want %q", cfg.GenerationExport.Endpoint, legacyCfg.GenerationExport.Endpoint)
				}
				if cfg.GenerationExport.Protocol != legacyCfg.GenerationExport.Protocol {
					t.Errorf("Protocol=%q want %q", cfg.GenerationExport.Protocol, legacyCfg.GenerationExport.Protocol)
				}
				if *cfg.GenerationExport.Insecure != *legacyCfg.GenerationExport.Insecure {
					t.Errorf("Insecure mismatch")
				}
				if cfg.GenerationExport.Auth != legacyCfg.GenerationExport.Auth {
					t.Errorf("Auth=%+v want %+v", cfg.GenerationExport.Auth, legacyCfg.GenerationExport.Auth)
				}
				if cfg.AgentName != legacyCfg.AgentName || cfg.AgentVersion != legacyCfg.AgentVersion || cfg.UserID != legacyCfg.UserID {
					t.Errorf("identity fields mismatch: %+v vs %+v", cfg, legacyCfg)
				}
				if cfg.Tags["service"] != "orchestrator" || cfg.Tags["env"] != "prod" {
					t.Errorf("Tags=%v", cfg.Tags)
				}
				if cfg.ContentCapture != legacyCfg.ContentCapture {
					t.Errorf("ContentCapture=%v want %v", cfg.ContentCapture, legacyCfg.ContentCapture)
				}
				if *cfg.Debug != *legacyCfg.Debug {
					t.Errorf("Debug mismatch")
				}
			},
		},
		{
			name: "preferred wins over legacy on conflict",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "preferred.example:4318",
				"SIGIL_ENDPOINT":     "legacy.example:4318",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "preferred.example:4318" {
					t.Errorf("Endpoint=%q want preferred.example:4318", cfg.GenerationExport.Endpoint)
				}
			},
		},
		{
			name: "blank preferred falls through to legacy",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "   ",
				"SIGIL_ENDPOINT":     "legacy.example:4318",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Endpoint != "legacy.example:4318" {
					t.Errorf("Endpoint=%q want legacy.example:4318", cfg.GenerationExport.Endpoint)
				}
			},
		},
		{
			name: "invalid preferred capture mode blocks valid legacy fallback",
			env: map[string]string{
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "bogus",
				"SIGIL_CONTENT_CAPTURE_MODE":     "metadata_only",
			},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_CONTENT_CAPTURE_MODE",
			check: func(t *testing.T, cfg Config) {
				if cfg.ContentCapture != ContentCaptureModeDefault {
					t.Errorf("ContentCapture=%v want default (legacy must not resurface)", cfg.ContentCapture)
				}
			},
		},
		{
			name: "invalid preferred auth mode blocks valid legacy fallback",
			env: map[string]string{
				"AGENTO11Y_AUTH_MODE": "garbage",
				"SIGIL_AUTH_MODE":     "bearer",
			},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_AUTH_MODE",
			check: func(t *testing.T, cfg Config) {
				if cfg.GenerationExport.Auth.Mode != ExportAuthModeNone {
					t.Errorf("Mode=%q want none (legacy must not resurface)", cfg.GenerationExport.Auth.Mode)
				}
			},
		},
		{
			name:            "invalid legacy capture mode error names legacy key",
			env:             map[string]string{"SIGIL_CONTENT_CAPTURE_MODE": "bogus"},
			wantErr:         true,
			wantErrContains: "SIGIL_CONTENT_CAPTURE_MODE",
		},
		{
			name: "mixed-prefix auth fields resolve per field",
			env: map[string]string{
				"AGENTO11Y_AUTH_MODE":  "basic",
				"SIGIL_AUTH_TENANT_ID": "42",
				"SIGIL_AUTH_TOKEN":     "glc_xxx",
			},
			check: func(t *testing.T, cfg Config) {
				auth := cfg.GenerationExport.Auth
				if auth.Mode != ExportAuthModeBasic {
					t.Errorf("Mode=%q want basic", auth.Mode)
				}
				if auth.TenantID != "42" {
					t.Errorf("TenantID=%q want 42", auth.TenantID)
				}
				if auth.BasicPassword != "glc_xxx" {
					t.Errorf("BasicPassword=%q want glc_xxx", auth.BasicPassword)
				}
			},
		},
		{
			name: "all four hooks variables from env",
			env: map[string]string{
				"AGENTO11Y_HOOKS_ENABLED":    "true",
				"AGENTO11Y_HOOKS_PHASES":     "preflight,postflight",
				"AGENTO11Y_HOOKS_TIMEOUT_MS": "3000",
				"AGENTO11Y_HOOKS_FAIL_OPEN":  "false",
			},
			check: func(t *testing.T, cfg Config) {
				if !cfg.Hooks.EnabledValue() {
					t.Errorf("Enabled=%v want true", cfg.Hooks.Enabled)
				}
				if !slices.Equal(cfg.Hooks.Phases, []HookPhase{HookPhasePreflight, HookPhasePostflight}) {
					t.Errorf("Phases=%v", cfg.Hooks.Phases)
				}
				if cfg.Hooks.Timeout != 3*time.Second {
					t.Errorf("Timeout=%s want 3s", cfg.Hooks.Timeout)
				}
				if cfg.Hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want false", cfg.Hooks.FailOpen)
				}
			},
		},
		{
			name: "hooks enabled alone leaves the other fields unset",
			env:  map[string]string{"AGENTO11Y_HOOKS_ENABLED": "true"},
			check: func(t *testing.T, cfg Config) {
				if !cfg.Hooks.EnabledValue() {
					t.Errorf("Enabled=%v want true", cfg.Hooks.Enabled)
				}
				if cfg.Hooks.Phases != nil || cfg.Hooks.Timeout != 0 || cfg.Hooks.FailOpen != nil {
					t.Errorf("Hooks=%+v want only Enabled set (NewClient applies the defaults)", cfg.Hooks)
				}
			},
		},
		{
			name: "hook phases are trimmed, lowercased and deduplicated in order",
			env:  map[string]string{"AGENTO11Y_HOOKS_PHASES": " POSTFLIGHT , ,preflight, postflight "},
			check: func(t *testing.T, cfg Config) {
				if !slices.Equal(cfg.Hooks.Phases, []HookPhase{HookPhasePostflight, HookPhasePreflight}) {
					t.Errorf("Phases=%v", cfg.Hooks.Phases)
				}
			},
		},
		{
			name:            "invalid hooks enabled is reported and skipped",
			env:             map[string]string{"AGENTO11Y_HOOKS_ENABLED": "maybe"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_ENABLED",
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Enabled != nil {
					t.Errorf("Enabled=%v want unset", *cfg.Hooks.Enabled)
				}
			},
		},
		{
			// FailOpen defaults to true, so a typo must not read as false.
			name:            "invalid hooks fail-open is reported and skipped",
			env:             map[string]string{"AGENTO11Y_HOOKS_FAIL_OPEN": "ture"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_FAIL_OPEN",
			check: func(t *testing.T, cfg Config) {
				if !cfg.Hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want the true default", cfg.Hooks.FailOpen)
				}
			},
		},
		{
			name:            "unknown hook phase is dropped and the rest applies",
			env:             map[string]string{"AGENTO11Y_HOOKS_PHASES": "preflight,bogus"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_PHASES",
			check: func(t *testing.T, cfg Config) {
				if !slices.Equal(cfg.Hooks.Phases, []HookPhase{HookPhasePreflight}) {
					t.Errorf("Phases=%v want [preflight]", cfg.Hooks.Phases)
				}
			},
		},
		{
			// Rejecting the whole list would fall back to the {preflight}
			// default, starting enforcement on a phase the operator did not ask
			// for and skipping the one they did.
			name:            "a typo beside postflight does not switch the phase to preflight",
			env:             map[string]string{"AGENTO11Y_HOOKS_PHASES": "postflight,bogus"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_PHASES",
			check: func(t *testing.T, cfg Config) {
				if !slices.Equal(cfg.Hooks.Phases, []HookPhase{HookPhasePostflight}) {
					t.Errorf("Phases=%v want [postflight]", cfg.Hooks.Phases)
				}
			},
		},
		{
			name:            "a phase list with no usable entry is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_PHASES": "bogus"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_PHASES",
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Phases != nil {
					t.Errorf("Phases=%v want unset", cfg.Hooks.Phases)
				}
			},
		},
		{
			name:            "zero hook timeout is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "0"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Timeout != 0 {
					t.Errorf("Timeout=%s want unset", cfg.Hooks.Timeout)
				}
			},
		},
		{
			name:            "negative hook timeout is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "-1"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
		},
		{
			name:            "non-integer hook timeout is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "1.5"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
		},
		{
			name:            "unparsable hook timeout is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "not-a-number"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
		},
		{
			// Python's int() reads PEP 515 underscores; Go and JS must not, or
			// the same value would mean different things per language.
			name:            "underscore digit grouping in the hook timeout is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "3_000"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
		},
		{
			name:            "a hook timeout above the server ceiling is rejected",
			env:             map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "120000"},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Timeout != 0 {
					t.Errorf("Timeout=%s want unset", cfg.Hooks.Timeout)
				}
			},
		},
		{
			name: "the largest honoured hook timeout is accepted",
			env:  map[string]string{"AGENTO11Y_HOOKS_TIMEOUT_MS": "119999"},
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Timeout != 119999*time.Millisecond {
					t.Errorf("Timeout=%s want 119999ms", cfg.Hooks.Timeout)
				}
			},
		},
		{
			name: "invalid hook timeout preserves the other hooks variables",
			env: map[string]string{
				"AGENTO11Y_HOOKS_ENABLED":    "true",
				"AGENTO11Y_HOOKS_PHASES":     "preflight,postflight",
				"AGENTO11Y_HOOKS_TIMEOUT_MS": "nope",
				"AGENTO11Y_HOOKS_FAIL_OPEN":  "false",
			},
			wantErr:         true,
			wantErrContains: "AGENTO11Y_HOOKS_TIMEOUT_MS",
			check: func(t *testing.T, cfg Config) {
				if !cfg.Hooks.EnabledValue() {
					t.Errorf("Enabled=%v want true (preserved despite the timeout typo)", cfg.Hooks.Enabled)
				}
				if len(cfg.Hooks.Phases) != 2 {
					t.Errorf("Phases=%v (preserved despite the timeout typo)", cfg.Hooks.Phases)
				}
				if cfg.Hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want false (preserved despite the timeout typo)", cfg.Hooks.FailOpen)
				}
				if cfg.Hooks.Timeout != 0 {
					t.Errorf("Timeout=%s want unset", cfg.Hooks.Timeout)
				}
			},
		},
		{
			name: "SIGIL_HOOKS_ variables are ignored",
			env: map[string]string{
				"SIGIL_HOOKS_ENABLED":    "true",
				"SIGIL_HOOKS_PHASES":     "postflight",
				"SIGIL_HOOKS_TIMEOUT_MS": "3000",
				"SIGIL_HOOKS_FAIL_OPEN":  "false",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.Hooks.Enabled != nil || cfg.Hooks.Phases != nil || cfg.Hooks.Timeout != 0 || cfg.Hooks.FailOpen != nil {
					t.Errorf("Hooks=%+v want untouched by the legacy prefix", cfg.Hooks)
				}
			},
		},
		{
			name: "preferred tags replace legacy tags without merging",
			env: map[string]string{
				"AGENTO11Y_TAGS": "team=ai",
				"SIGIL_TAGS":     "service=orch,env=prod",
			},
			check: func(t *testing.T, cfg Config) {
				if cfg.Tags["team"] != "ai" {
					t.Errorf("Tags[team]=%q want ai", cfg.Tags["team"])
				}
				if _, ok := cfg.Tags["service"]; ok {
					t.Errorf("Tags=%v: legacy tags must not merge into preferred tags", cfg.Tags)
				}
				if len(cfg.Tags) != 1 {
					t.Errorf("Tags=%v want exactly the preferred value", cfg.Tags)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := DefaultConfig()
			if tc.base != nil {
				base = *tc.base
			}
			cfg, err := resolveFromEnv(mapLookup(tc.env), base)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantErrContains != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErrContains)) {
				t.Fatalf("error %v does not mention %q", err, tc.wantErrContains)
			}
			if tc.check != nil {
				tc.check(t, cfg)
			}
		})
	}
}

func TestParseCSVKV(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{
			name: "mixed valid and edge cases",
			raw:  "a=1, b = two ,, =skip,c=",
			want: map[string]string{"a": "1", "b": "two", "c": ""},
		},
		{
			name: "empty input",
			raw:  "",
			want: map[string]string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCSVKV(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("got[%q]=%q want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestNewClient_EnvHandling exercises the integration of env resolution with
// caller-supplied Config: precedence rules, malformed-env recovery, and the
// auth mode/credential interaction.
func TestNewClient_EnvHandling(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		cfg   Config
		check func(t *testing.T, c *Client)
	}{
		{
			name: "reads env into empty config",
			env: map[string]string{
				"SIGIL_AGENT_NAME": "from-env",
				"SIGIL_USER_ID":    "alice",
				"SIGIL_TAGS":       "team=ai",
				"SIGIL_PROTOCOL":   "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.AgentName != "from-env" {
					t.Errorf("AgentName=%q", c.config.AgentName)
				}
				if c.config.UserID != "alice" {
					t.Errorf("UserID=%q", c.config.UserID)
				}
				if c.config.Tags["team"] != "ai" {
					t.Errorf("Tags=%v", c.config.Tags)
				}
				if c.config.GenerationExport.Protocol != GenerationExportProtocolNone {
					t.Errorf("Protocol=%v", c.config.GenerationExport.Protocol)
				}
			},
		},
		{
			name: "env endpoint reaches the hooks API endpoint",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "https://collector.example.com",
				"AGENTO11Y_PROTOCOL": "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.API.Endpoint != "https://collector.example.com" {
					t.Errorf("API.Endpoint=%q want the env endpoint", c.config.API.Endpoint)
				}
			},
		},
		{
			// The Go README teaches NewClient(DefaultConfig()); it must not
			// shadow the env endpoint either.
			name: "env endpoint survives an unmodified DefaultConfig",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "https://collector.example.com",
				"AGENTO11Y_PROTOCOL": "none",
			},
			cfg: DefaultConfig(),
			check: func(t *testing.T, c *Client) {
				if c.config.API.Endpoint != "https://collector.example.com" {
					t.Errorf("API.Endpoint=%q want the env endpoint", c.config.API.Endpoint)
				}
			},
		},
		{
			name: "unset API endpoint falls back to localhost",
			env:  map[string]string{"AGENTO11Y_PROTOCOL": "none"},
			check: func(t *testing.T, c *Client) {
				if c.config.API.Endpoint != defaultAPIEndpoint {
					t.Errorf("API.Endpoint=%q want %q", c.config.API.Endpoint, defaultAPIEndpoint)
				}
			},
		},
		{
			// The whole point of comparing against the schema default:
			// DefaultConfig pre-fills the field, so a localhost value in the
			// caller's Config is not a caller choice.
			name: "a caller-supplied schema-default API endpoint does not shadow env",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "https://collector.example.com",
				"AGENTO11Y_PROTOCOL": "none",
			},
			cfg: Config{API: APIConfig{Endpoint: defaultAPIEndpoint}},
			check: func(t *testing.T, c *Client) {
				if c.config.API.Endpoint != "https://collector.example.com" {
					t.Errorf("API.Endpoint=%q want the env endpoint", c.config.API.Endpoint)
				}
			},
		},
		{
			name: "explicit API endpoint wins over env endpoint",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "https://environment.example.com",
				"AGENTO11Y_PROTOCOL": "none",
			},
			cfg: Config{API: APIConfig{Endpoint: "https://caller.example.com"}},
			check: func(t *testing.T, c *Client) {
				if c.config.API.Endpoint != "https://caller.example.com" {
					t.Errorf("API.Endpoint=%q want the caller endpoint", c.config.API.Endpoint)
				}
			},
		},
		{
			name: "explicit caller value wins over env",
			env:  map[string]string{"SIGIL_ENDPOINT": "env-endpoint:4318"},
			cfg: Config{
				GenerationExport: GenerationExportConfig{
					Endpoint: "explicit-endpoint:4318",
					Protocol: GenerationExportProtocolNone,
				},
			},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Endpoint != "explicit-endpoint:4318" {
					t.Errorf("Endpoint=%q", c.config.GenerationExport.Endpoint)
				}
			},
		},
		{
			name: "env Insecure=true survives empty caller config",
			env: map[string]string{
				"SIGIL_INSECURE": "true",
				"SIGIL_PROTOCOL": "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Insecure == nil || !*c.config.GenerationExport.Insecure {
					t.Fatalf("Insecure=%v, want env-resolved true", c.config.GenerationExport.Insecure)
				}
			},
		},
		{
			name: "explicit Insecure=false beats env Insecure=true",
			env: map[string]string{
				"SIGIL_INSECURE": "true",
				"SIGIL_PROTOCOL": "none",
			},
			cfg: Config{GenerationExport: GenerationExportConfig{Insecure: BoolPtr(false)}},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Insecure == nil || *c.config.GenerationExport.Insecure {
					t.Fatalf("Insecure=%v, want explicit false", c.config.GenerationExport.Insecure)
				}
			},
		},
		{
			name: "env Debug=true survives empty caller config",
			env: map[string]string{
				"SIGIL_DEBUG":    "true",
				"SIGIL_PROTOCOL": "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.Debug == nil || !*c.config.Debug {
					t.Fatalf("Debug=%v, want env-resolved true", c.config.Debug)
				}
			},
		},
		{
			name: "explicit Debug=false beats env Debug=true",
			env: map[string]string{
				"SIGIL_DEBUG":    "true",
				"SIGIL_PROTOCOL": "none",
			},
			cfg: Config{Debug: BoolPtr(false)},
			check: func(t *testing.T, c *Client) {
				if c.config.Debug == nil || *c.config.Debug {
					t.Fatalf("Debug=%v, want explicit false", c.config.Debug)
				}
			},
		},
		{
			name: "malformed SIGIL_AUTH_MODE does not panic",
			env: map[string]string{
				"SIGIL_AUTH_MODE": "Bearrer",
				"SIGIL_PROTOCOL":  "none",
			},
		},
		{
			name: "malformed SIGIL_AUTH_MODE preserves valid env siblings",
			env: map[string]string{
				"SIGIL_AUTH_MODE":  "Bearrer",
				"SIGIL_ENDPOINT":   "valid.example:4318",
				"SIGIL_AGENT_NAME": "valid-agent",
				"SIGIL_USER_ID":    "alice",
				"SIGIL_PROTOCOL":   "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Endpoint != "valid.example:4318" {
					t.Errorf("Endpoint=%q want valid.example:4318 (preserved despite typo)", c.config.GenerationExport.Endpoint)
				}
				if c.config.AgentName != "valid-agent" {
					t.Errorf("AgentName=%q (preserved despite typo)", c.config.AgentName)
				}
				if c.config.UserID != "alice" {
					t.Errorf("UserID=%q (preserved despite typo)", c.config.UserID)
				}
			},
		},
		{
			name: "stray SIGIL_AUTH_TENANT_ID does not panic",
			env: map[string]string{
				"SIGIL_AUTH_TENANT_ID": "42",
				"SIGIL_PROTOCOL":       "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Auth.Mode != ExportAuthModeNone {
					t.Errorf("Mode=%q want none", c.config.GenerationExport.Auth.Mode)
				}
			},
		},
		{
			name: "caller bearer mode wins over env basic mode",
			env: map[string]string{
				"SIGIL_AUTH_MODE":      "basic",
				"SIGIL_AUTH_TENANT_ID": "42",
				"SIGIL_AUTH_TOKEN":     "envpass",
				"SIGIL_PROTOCOL":       "none",
			},
			cfg: Config{
				GenerationExport: GenerationExportConfig{
					Auth: AuthConfig{Mode: ExportAuthModeBearer, BearerToken: "callertok"},
				},
			},
			check: func(t *testing.T, c *Client) {
				auth := c.config.GenerationExport.Auth
				if auth.Mode != ExportAuthModeBearer {
					t.Errorf("Mode=%q want bearer (caller wins)", auth.Mode)
				}
				if auth.BearerToken != "callertok" {
					t.Errorf("BearerToken=%q want callertok", auth.BearerToken)
				}
				// Authorization header carries caller's bearer token, not env's password.
				got := c.config.GenerationExport.Headers["Authorization"]
				if got != "Bearer callertok" {
					t.Errorf("Authorization=%q want %q", got, "Bearer callertok")
				}
			},
		},
		{
			// Caller tags merge with env tags as a base layer; caller wins on
			// key collision. Matches JS and Python SDK behavior.
			name: "caller tags merge with env tags",
			env: map[string]string{
				"SIGIL_TAGS":     "service=orch,env=prod",
				"SIGIL_PROTOCOL": "none",
			},
			cfg: Config{
				Tags: map[string]string{"team": "ai", "env": "staging"},
			},
			check: func(t *testing.T, c *Client) {
				if got := c.config.Tags["service"]; got != "orch" {
					t.Errorf("Tags[service]=%q want orch (env-filled)", got)
				}
				if got := c.config.Tags["team"]; got != "ai" {
					t.Errorf("Tags[team]=%q want ai (caller-only)", got)
				}
				if got := c.config.Tags["env"]; got != "staging" {
					t.Errorf("Tags[env]=%q want staging (caller wins on collision)", got)
				}
			},
		},
		{
			name: "env SIGIL_AUTH_TOKEN fills caller-supplied bearer mode",
			env: map[string]string{
				"SIGIL_AUTH_TOKEN": "envtok",
				"SIGIL_PROTOCOL":   "none",
			},
			cfg: Config{
				GenerationExport: GenerationExportConfig{
					Auth: AuthConfig{Mode: ExportAuthModeBearer},
				},
			},
			check: func(t *testing.T, c *Client) {
				auth := c.config.GenerationExport.Auth
				if auth.Mode != ExportAuthModeBearer {
					t.Errorf("Mode=%q want bearer", auth.Mode)
				}
				if auth.BearerToken != "envtok" {
					t.Errorf("BearerToken=%q want envtok (filled from SIGIL_AUTH_TOKEN)", auth.BearerToken)
				}
			},
		},
		{
			name: "reads preferred env into empty config",
			env: map[string]string{
				"AGENTO11Y_AGENT_NAME": "from-env",
				"AGENTO11Y_USER_ID":    "alice",
				"AGENTO11Y_TAGS":       "team=ai",
				"AGENTO11Y_PROTOCOL":   "none",
			},
			check: func(t *testing.T, c *Client) {
				if c.config.AgentName != "from-env" {
					t.Errorf("AgentName=%q", c.config.AgentName)
				}
				if c.config.UserID != "alice" {
					t.Errorf("UserID=%q", c.config.UserID)
				}
				if c.config.Tags["team"] != "ai" {
					t.Errorf("Tags=%v", c.config.Tags)
				}
				if c.config.GenerationExport.Protocol != GenerationExportProtocolNone {
					t.Errorf("Protocol=%v", c.config.GenerationExport.Protocol)
				}
			},
		},
		{
			name: "explicit caller value wins over both branded prefixes",
			env: map[string]string{
				"AGENTO11Y_ENDPOINT": "preferred-endpoint:4318",
				"SIGIL_ENDPOINT":     "legacy-endpoint:4318",
			},
			cfg: Config{
				GenerationExport: GenerationExportConfig{
					Endpoint: "explicit-endpoint:4318",
					Protocol: GenerationExportProtocolNone,
				},
			},
			check: func(t *testing.T, c *Client) {
				if c.config.GenerationExport.Endpoint != "explicit-endpoint:4318" {
					t.Errorf("Endpoint=%q want explicit-endpoint:4318", c.config.GenerationExport.Endpoint)
				}
			},
		},
		{
			name: "hooks resolve from env into a sparse config",
			env: map[string]string{
				"AGENTO11Y_HOOKS_ENABLED":    "true",
				"AGENTO11Y_HOOKS_PHASES":     "preflight,postflight",
				"AGENTO11Y_HOOKS_TIMEOUT_MS": "3000",
				"AGENTO11Y_HOOKS_FAIL_OPEN":  "false",
				"AGENTO11Y_PROTOCOL":         "none",
			},
			check: func(t *testing.T, c *Client) {
				hooks := c.config.Hooks
				if !hooks.EnabledValue() {
					t.Errorf("Enabled=%v want true", hooks.Enabled)
				}
				if !slices.Equal(hooks.Phases, []HookPhase{HookPhasePreflight, HookPhasePostflight}) {
					t.Errorf("Phases=%v", hooks.Phases)
				}
				if hooks.Timeout != 3*time.Second {
					t.Errorf("Timeout=%s want 3s", hooks.Timeout)
				}
				if hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want false", hooks.FailOpen)
				}
			},
		},
		{
			// The Go README teaches NewClient(DefaultConfig()); it must not
			// shadow the env layer.
			name: "env hooks survive an unmodified DefaultConfig",
			env: map[string]string{
				"AGENTO11Y_HOOKS_ENABLED":    "true",
				"AGENTO11Y_HOOKS_TIMEOUT_MS": "3000",
				"AGENTO11Y_PROTOCOL":         "none",
			},
			cfg: DefaultConfig(),
			check: func(t *testing.T, c *Client) {
				if !c.config.Hooks.EnabledValue() {
					t.Errorf("Enabled=%v want true", c.config.Hooks.Enabled)
				}
				if c.config.Hooks.Timeout != 3*time.Second {
					t.Errorf("Timeout=%s want 3s", c.config.Hooks.Timeout)
				}
			},
		},
		{
			name: "caller hooks config wins over env for every field",
			env: map[string]string{
				"AGENTO11Y_HOOKS_ENABLED":    "true",
				"AGENTO11Y_HOOKS_PHASES":     "preflight,postflight",
				"AGENTO11Y_HOOKS_TIMEOUT_MS": "9000",
				"AGENTO11Y_HOOKS_FAIL_OPEN":  "true",
				"AGENTO11Y_PROTOCOL":         "none",
			},
			cfg: Config{Hooks: HooksConfig{
				Enabled:  BoolPtr(false),
				Phases:   []HookPhase{HookPhasePostflight},
				Timeout:  2500 * time.Millisecond,
				FailOpen: BoolPtr(false),
			}},
			check: func(t *testing.T, c *Client) {
				hooks := c.config.Hooks
				if hooks.EnabledValue() {
					t.Errorf("Enabled=%v want the explicit false", hooks.Enabled)
				}
				if !slices.Equal(hooks.Phases, []HookPhase{HookPhasePostflight}) {
					t.Errorf("Phases=%v want the caller list", hooks.Phases)
				}
				if hooks.Timeout != 2500*time.Millisecond {
					t.Errorf("Timeout=%s want 2.5s", hooks.Timeout)
				}
				if hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want the explicit false", hooks.FailOpen)
				}
			},
		},
		{
			name: "unset hooks resolve to the schema defaults",
			env:  map[string]string{"AGENTO11Y_PROTOCOL": "none"},
			check: func(t *testing.T, c *Client) {
				hooks := c.config.Hooks
				if hooks.EnabledValue() {
					t.Errorf("Enabled=%v want false", hooks.Enabled)
				}
				if !slices.Equal(hooks.Phases, []HookPhase{HookPhasePreflight}) {
					t.Errorf("Phases=%v want [preflight]", hooks.Phases)
				}
				if hooks.Timeout != defaultHookTimeout {
					t.Errorf("Timeout=%s want %s", hooks.Timeout, defaultHookTimeout)
				}
				if !hooks.FailOpenEnabled() {
					t.Errorf("FailOpen=%v want true", hooks.FailOpen)
				}
			},
		},
		{
			name: "caller tags merge with preferred env tags",
			env: map[string]string{
				"AGENTO11Y_TAGS":     "service=orch,env=prod",
				"AGENTO11Y_PROTOCOL": "none",
			},
			cfg: Config{
				Tags: map[string]string{"team": "ai", "env": "staging"},
			},
			check: func(t *testing.T, c *Client) {
				if got := c.config.Tags["service"]; got != "orch" {
					t.Errorf("Tags[service]=%q want orch (env-filled)", got)
				}
				if got := c.config.Tags["env"]; got != "staging" {
					t.Errorf("Tags[env]=%q want staging (caller wins on collision)", got)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("NewClient panicked: %v", r)
				}
			}()
			c := NewClient(tc.cfg)
			defer func() { _ = c.Shutdown(context.Background()) }()
			if tc.check != nil {
				tc.check(t, c)
			}
		})
	}
}
