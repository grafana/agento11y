package local

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

// clearForwardEnv blanks every variable the forward loader reads so a test's
// config.env file, not the ambient environment, decides the outcome. Without
// this a developer with AGENTO11Y_LOCAL_FORWARD and real credentials exported
// would have the suite POST to their live tenant, and one with
// AGENTO11Y_GUARDS_ENABLED=true would have the hook leg chain to it.
//
// PinAliasEnvBlank covers both spellings of every alias family, which includes
// LOCAL_FORWARD and the three GUARDS_* keys the hook leg reads.
func clearForwardEnv(t *testing.T) {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
	for _, suffix := range []string{"LOCAL_FORWARD", "GUARDS_ENABLED", "GUARDS_FAIL_OPEN", "GUARDS_TIMEOUT_MS"} {
		require.Contains(t, envconfig.AliasSuffixes, suffix, "clearForwardEnv relies on PinAliasEnvBlank covering %s", suffix)
	}
	// The loader also falls back to the bare OTLP variables, which are not part
	// of an alias family.
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_INSECURE"} {
		t.Setenv(k, "")
	}
}

func writeConfigEnvFile(t *testing.T, lines map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.env")
	var b []byte
	for k, v := range lines {
		b = fmt.Appendf(b, "%s=%s\n", k, v)
	}
	require.NoError(t, os.WriteFile(path, b, 0o600))
	return path
}

// guardsOffHookReason is the hook leg's refusal for every case that enables
// forwarding without enabling guards, which is most of the table below.
const guardsOffHookReason = "GUARDS_ENABLED is off"

// contentMarker is in every content-bearing value of contentRichGeneration, so
// one scan of a forwarded payload covers the string, bytes, and metadata fields
// together. Nothing the strip retains contains it.
const contentMarker = "secret"

// legacyConversationTitleKey is the pre-rename spelling of
// metadataKeyConversationTitle, used both as a span attribute and as a
// generation metadata key. An older installed exporter
// (@grafana/agento11y-pi, -opencode, or a sigil-era SDK) can still send it to a
// current daemon, so both spellings have to be dropped.
const legacyConversationTitleKey = "sigil.conversation.title"

// traceContentKeys are the span attribute keys that must not leave the host
// under a reduced content mode. The spellings are the test's own, never
// contentcapture's constants: a test reading the same constant production reads
// would follow a renamed key instead of catching it, and the spelling is what a
// span carries on the wire.
var traceContentKeys = []string{
	"agento11y.conversation.title",
	legacyConversationTitleKey,
	"gen_ai.embeddings.input_texts",
	"gen_ai.tool.description",
	"gen_ai.tool.call.arguments",
	"gen_ai.tool.call.result",
}

func TestForwardLoader_Resolve(t *testing.T) {
	const cloud = "https://cloud.example.test/"
	cases := []struct {
		name             string
		lines            map[string]string
		wantEnabled      bool
		wantStrip        bool
		wantStatusMode   string
		wantStatusReason bool
		wantOTLP         bool
		wantHookURL      string
		wantHookReason   string // substring; "" means the reason must be empty
	}{
		{
			// Credentials the generation export cannot use, but a usable OTLP
			// target: the OTLP leg forwards on its own and the status says so.
			name: "otlp_only_when_generation_creds_missing",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.test",
				"OTEL_EXPORTER_OTLP_HEADERS":            "Authorization=Bearer gateway-token",
			},
			wantEnabled:      true,
			wantStrip:        true,
			wantStatusMode:   forwardModeMetadataOnly,
			wantStatusReason: true,
			wantOTLP:         true,
			wantHookReason:   guardsOffHookReason,
		},
		{
			name:           "disabled_when_toggle_unset",
			lines:          map[string]string{"AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:    false,
			wantStatusMode: forwardModeOff,
		},
		{
			name:           "enabled_defaults_to_metadata_only",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "enabled_full_does_not_strip",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k", "AGENTO11Y_CONTENT_CAPTURE_MODE": "full"},
			wantEnabled:    true,
			wantStrip:      false,
			wantStatusMode: forwardModeFull,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "advanced_capture_mode_still_strips",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k", "AGENTO11Y_CONTENT_CAPTURE_MODE": "no_tool_content"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "legacy_spellings_resolve",
			lines:          map[string]string{"SIGIL_LOCAL_FORWARD": "true", "SIGIL_ENDPOINT": cloud, "SIGIL_AUTH_TENANT_ID": "t", "SIGIL_AUTH_TOKEN": "k", "SIGIL_CONTENT_CAPTURE_MODE": "full"},
			wantEnabled:    true,
			wantStrip:      false,
			wantStatusMode: forwardModeFull,
			wantHookReason: guardsOffHookReason,
		},
		{
			// Both spellings present: the preferred one decides, matching
			// ParseSettings and dotenv.ApplyEnv.
			name: "preferred_spelling_wins",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "SIGIL_LOCAL_FORWARD": "false",
				"AGENTO11Y_ENDPOINT": cloud, "SIGIL_ENDPOINT": "https://other.example.test",
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_CONTENT_CAPTURE_MODE": "full", "SIGIL_CONTENT_CAPTURE_MODE": "metadata_only",
			},
			wantEnabled:    true,
			wantStrip:      false,
			wantStatusMode: forwardModeFull,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "allows_local_endpoint",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://127.0.0.1:4317", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "allows_local_endpoint_with_placeholder_creds",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://127.0.0.1:8080", "AGENTO11Y_AUTH_TENANT_ID": "local", "AGENTO11Y_AUTH_TOKEN": "local"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:           "allows_local_endpoint_without_creds",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://localhost:8080"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: guardsOffHookReason,
		},
		{
			name:             "refuses_placeholder_creds",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "local", "AGENTO11Y_AUTH_TOKEN": "local"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
			wantHookReason:   guardsOffHookReason,
		},
		{
			name:             "refuses_empty_endpoint",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
			wantHookReason:   guardsOffHookReason,
		},
		{
			name:             "refuses_invalid_endpoint",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://%", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
			wantHookReason:   guardsOffHookReason,
		},
		{
			// The hook leg needs both toggles. Forwarding alone leaves guard
			// evaluation local, which is what every existing case above
			// asserts implicitly.
			name: "hooks_refused_when_guards_off",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
			},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: "GUARDS_ENABLED",
		},
		{
			name: "hooks_chain_when_guards_on",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_GUARDS_ENABLED": "true",
			},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookURL:    "https://cloud.example.test/api/v1/hooks:evaluate",
		},
		{
			// A local endpoint is a legitimate generation target but never a
			// hook target: it is either this daemon or another always-allow
			// stub.
			name: "hooks_refused_for_local_endpoint",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://127.0.0.1:8080",
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_GUARDS_ENABLED": "true",
			},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookReason: "is local",
		},
		{
			name: "hooks_refused_without_credentials",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
				"AGENTO11Y_AUTH_TENANT_ID": "t",
				"AGENTO11Y_GUARDS_ENABLED": "true",
			},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
			wantHookReason:   "Cloud credentials are missing",
		},
		{
			name: "hooks_refused_for_placeholder_credentials",
			lines: map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": envconfig.LocalAuthPlaceholder,
				"AGENTO11Y_GUARDS_ENABLED": "true",
			},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
			wantHookReason:   "placeholder",
		},
		{
			name: "hooks_resolve_from_legacy_spellings",
			lines: map[string]string{
				"SIGIL_LOCAL_FORWARD": "true", "SIGIL_ENDPOINT": cloud,
				"SIGIL_AUTH_TENANT_ID": "t", "SIGIL_AUTH_TOKEN": "k",
				"SIGIL_GUARDS_ENABLED": "true",
			},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
			wantHookURL:    "https://cloud.example.test/api/v1/hooks:evaluate",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			l := newForwardLoader(writeConfigEnvFile(t, tc.lines), nil)
			cfg := l.load()
			assert.Equal(t, tc.wantEnabled, cfg.enabled)
			if tc.wantEnabled {
				assert.Equal(t, tc.wantStrip, cfg.strip)
				assert.Equal(t, tc.wantOTLP, cfg.otlpEndpoint != "")
			}
			// The guard policy resolves to its documented defaults on every
			// path, including the ones that refuse every leg, so no consumer
			// can read a half-built config as an explicit "fail closed".
			assert.Equal(t, envconfig.DefaultGuardsTimeoutMs, cfg.timeoutMs)
			assert.True(t, cfg.failOpen)

			assert.Equal(t, tc.wantHookURL, cfg.hookURL)
			switch {
			case tc.wantHookURL != "":
				assert.Empty(t, cfg.hookReason, "a live hook leg has nothing to explain")
			case tc.wantHookReason == "":
				// Forwarding off at all is not a paused state, so the reason
				// is empty there too, same as the generations leg.
				assert.Empty(t, cfg.hookReason)
			default:
				assert.Contains(t, cfg.hookReason, tc.wantHookReason)
			}
			st := l.status()
			assert.Equal(t, tc.wantEnabled, st.Enabled)
			assert.Equal(t, tc.wantStatusMode, st.Mode)
			assert.Equal(t, tc.wantOTLP, st.OTLP)
			assert.Equal(t, cfg.genURL != "", st.Generations)
			assert.Equal(t, tc.wantHookURL != "", st.Hooks)
			assert.Equal(t, cfg.hookReason, st.HookReason)
			if tc.wantStatusReason {
				assert.NotEmpty(t, st.Reason)
			} else {
				assert.Empty(t, st.Reason)
			}
		})
	}
}

// TestForwardLoader_ResolvesFromProcessEnv covers the process-environment
// fall-back: a value exported in the shell that launched the daemon enables
// forwarding even with no config.env entry.
func TestForwardLoader_ResolvesFromProcessEnv(t *testing.T) {
	clearForwardEnv(t)
	t.Setenv(envconfig.PreferredKey("LOCAL_FORWARD"), "true")
	t.Setenv(envconfig.PreferredKey("ENDPOINT"), "https://cloud.example.test/")
	t.Setenv(envconfig.PreferredKey("AUTH_TENANT_ID"), "t")
	t.Setenv(envconfig.PreferredKey("AUTH_TOKEN"), "k")

	l := newForwardLoader(filepath.Join(t.TempDir(), "missing-config.env"), nil)
	assert.True(t, l.load().enabled)
	assert.Equal(t, forwardModeMetadataOnly, l.status().Mode)
}

// TestForwardLoader_ConfigFileBeatsProcessEnv covers the no-restart
// requirement from the other direction: the daemon's process env is populated
// from config.env at boot, so an edited file value must win over the stale
// exported one.
func TestForwardLoader_ConfigFileBeatsProcessEnv(t *testing.T) {
	clearForwardEnv(t)
	t.Setenv(envconfig.PreferredKey("LOCAL_FORWARD"), "false")
	path := writeConfigEnvFile(t, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD":  "true",
		"AGENTO11Y_ENDPOINT":       "https://cloud.example.test/",
		"AGENTO11Y_AUTH_TENANT_ID": "t",
		"AGENTO11Y_AUTH_TOKEN":     "k",
	})
	assert.True(t, newForwardLoader(path, nil).load().enabled)
}

// TestHookEvaluateURL covers the URL join the hook leg uses: any path on the
// configured API endpoint is dropped before the hook path is appended, and a
// hostless endpoint yields "" so the caller refuses instead of POSTing to a
// relative URL.
func TestHookEvaluateURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "appends_path", in: "https://cloud.example.test", want: "https://cloud.example.test/api/v1/hooks:evaluate"},
		{name: "drops_trailing_slash", in: "https://cloud.example.test/", want: "https://cloud.example.test/api/v1/hooks:evaluate"},
		{name: "drops_existing_path", in: "https://cloud.example.test/api/v1/generations:export", want: "https://cloud.example.test/api/v1/hooks:evaluate"},
		{name: "keeps_port", in: "https://cloud.example.test:8443/base", want: "https://cloud.example.test:8443/api/v1/hooks:evaluate"},
		{name: "schemeless_gets_https", in: "cloud.example.test", want: "https://cloud.example.test/api/v1/hooks:evaluate"},
		{name: "keeps_http_scheme", in: "http://cloud.example.test", want: "http://cloud.example.test/api/v1/hooks:evaluate"},
		{name: "empty_endpoint", in: "", want: ""},
		{name: "unparseable_endpoint", in: "http://%", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hookEvaluateURL(tc.in))
		})
	}
}

// TestForwardLoader_GuardPolicyResolution covers the two guard knobs the
// chained hook call applies. They go through the file-first envReader rather
// than envconfig.ResolveGuards, so a config.env edit reaches a running daemon.
func TestForwardLoader_GuardPolicyResolution(t *testing.T) {
	const cloud = "https://cloud.example.test/"
	base := map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
		"AGENTO11Y_GUARDS_ENABLED": "true",
	}
	cases := []struct {
		name          string
		extra         map[string]string
		processEnv    map[string]string
		wantFailOpen  bool
		wantTimeoutMs int
	}{
		{
			name:          "defaults",
			wantFailOpen:  true,
			wantTimeoutMs: envconfig.DefaultGuardsTimeoutMs,
		},
		{
			name:          "explicit_values",
			extra:         map[string]string{"AGENTO11Y_GUARDS_FAIL_OPEN": "false", "AGENTO11Y_GUARDS_TIMEOUT_MS": "9000"},
			wantFailOpen:  false,
			wantTimeoutMs: 9000,
		},
		{
			name:          "legacy_spellings",
			extra:         map[string]string{"SIGIL_GUARDS_FAIL_OPEN": "no", "SIGIL_GUARDS_TIMEOUT_MS": "2500"},
			wantFailOpen:  false,
			wantTimeoutMs: 2500,
		},
		{
			name:          "invalid_timeout_falls_back",
			extra:         map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "soon"},
			wantFailOpen:  true,
			wantTimeoutMs: envconfig.DefaultGuardsTimeoutMs,
		},
		{
			name:          "non_positive_timeout_falls_back",
			extra:         map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "-1"},
			wantFailOpen:  true,
			wantTimeoutMs: envconfig.DefaultGuardsTimeoutMs,
		},
		{
			// The daemon's own environment holds the boot-time values, so the
			// file has to win or a viewer edit would need a restart.
			name:          "config_file_beats_process_env",
			extra:         map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "4000", "AGENTO11Y_GUARDS_FAIL_OPEN": "false"},
			processEnv:    map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "1000", "AGENTO11Y_GUARDS_FAIL_OPEN": "true"},
			wantFailOpen:  false,
			wantTimeoutMs: 4000,
		},
		{
			name:          "process_env_used_when_file_is_silent",
			processEnv:    map[string]string{"AGENTO11Y_GUARDS_TIMEOUT_MS": "1000", "AGENTO11Y_GUARDS_FAIL_OPEN": "false"},
			wantFailOpen:  false,
			wantTimeoutMs: 1000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			for k, v := range tc.processEnv {
				t.Setenv(k, v)
			}
			lines := map[string]string{}
			maps.Copy(lines, base)
			maps.Copy(lines, tc.extra)
			cfg := newForwardLoader(writeConfigEnvFile(t, lines), nil).load()
			require.NotEmpty(t, cfg.hookURL)
			assert.Equal(t, tc.wantFailOpen, cfg.failOpen)
			assert.Equal(t, tc.wantTimeoutMs, cfg.timeoutMs)
		})
	}
}

// TestHookForwardReason covers the gate `agento11y doctor` shares with the
// daemon, so the report cannot describe a posture the daemon does not apply.
func TestHookForwardReason(t *testing.T) {
	const cloud = "https://cloud.example.test/"
	cases := []struct {
		name       string
		forward    bool
		guards     bool
		endpoint   string
		tenant     string
		token      string
		wantReason string // substring; "" means chaining is live
	}{
		{name: "live", forward: true, guards: true, endpoint: cloud, tenant: "t", token: "k"},
		{name: "forward_off", guards: true, endpoint: cloud, tenant: "t", token: "k", wantReason: "LOCAL_FORWARD"},
		{name: "guards_off", forward: true, endpoint: cloud, tenant: "t", token: "k", wantReason: "GUARDS_ENABLED"},
		{name: "empty_endpoint", forward: true, guards: true, tenant: "t", token: "k", wantReason: "ENDPOINT"},
		{name: "local_endpoint", forward: true, guards: true, endpoint: "http://localhost:8080", tenant: "t", token: "k", wantReason: "is local"},
		{name: "missing_token", forward: true, guards: true, endpoint: cloud, tenant: "t", wantReason: "Cloud credentials"},
		{name: "placeholder_tenant", forward: true, guards: true, endpoint: cloud, tenant: envconfig.LocalAuthPlaceholder, token: "k", wantReason: "placeholder"},
		{name: "whitespace_token", forward: true, guards: true, endpoint: cloud, tenant: "t", token: "  ", wantReason: "Cloud credentials"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := HookForwardReason(tc.forward, tc.guards, tc.endpoint, tc.tenant, tc.token)
			if tc.wantReason == "" {
				assert.Empty(t, got)
			} else {
				assert.Contains(t, got, tc.wantReason)
			}
		})
	}
}

func TestEnsureEndpointScheme(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty_unchanged", in: "", want: ""},
		{name: "schemeless_gets_https", in: "otlp.example.test:4318", want: "https://otlp.example.test:4318"},
		{name: "https_unchanged", in: "https://otlp.example.test", want: "https://otlp.example.test"},
		{name: "http_unchanged", in: "http://otlp.example.test", want: "http://otlp.example.test"},
		{name: "trims_whitespace", in: "  otlp.example.test  ", want: "https://otlp.example.test"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ensureEndpointScheme(tc.in))
		})
	}
}

func TestOTLPSignalFromPath(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{path: "/otlp/v1/traces", want: "traces"},
		{path: "/otlp/v1/metrics", want: "metrics"},
		{path: "/otlp/v1/logs", want: ""},
		{path: "/api/v1/generations:export", want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			assert.Equal(t, tc.want, otlpSignalFromPath(tc.path))
		})
	}
}

// TestForwardLoader_OTLPEndpointResolution covers the OTLP relay target: a
// schemeless endpoint must resolve to an absolute URL (a relative one only
// fails at request time, per payload), the bare OTEL_EXPORTER_OTLP_ENDPOINT is
// the fall-back, and a local receiver is dropped so the daemon never relays
// back to itself.
func TestForwardLoader_OTLPEndpointResolution(t *testing.T) {
	const cloud = "https://cloud.example.test/"
	base := map[string]string{
		"AGENTO11Y_LOCAL_FORWARD":  "true",
		"AGENTO11Y_ENDPOINT":       cloud,
		"AGENTO11Y_AUTH_TENANT_ID": "t",
		"AGENTO11Y_AUTH_TOKEN":     "k",
	}
	cases := []struct {
		name     string
		extra    map[string]string
		bareEnv  string
		wantOTLP string
	}{
		{
			name:     "schemeless_gets_https",
			extra:    map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "otlp.example.test:4318"},
			wantOTLP: "https://otlp.example.test:4318",
		},
		{
			name:     "legacy_spelling_resolves",
			extra:    map[string]string{"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.test"},
			wantOTLP: "https://otlp.example.test",
		},
		{
			name:     "falls_back_to_bare_otel_var",
			bareEnv:  "https://otlp.example.test/otlp",
			wantOTLP: "https://otlp.example.test/otlp",
		},
		{
			name:     "local_receiver_dropped",
			extra:    map[string]string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:8765/otlp"},
			wantOTLP: "",
		},
		{
			name:     "unset_stays_empty",
			wantOTLP: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			if tc.bareEnv != "" {
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", tc.bareEnv)
			}
			lines := map[string]string{}
			maps.Copy(lines, base)
			maps.Copy(lines, tc.extra)
			cfg := newForwardLoader(writeConfigEnvFile(t, lines), nil).load()
			require.True(t, cfg.enabled)
			assert.Equal(t, tc.wantOTLP, cfg.otlpEndpoint)
		})
	}
}

// TestForwardLoader_ReloadsOnConfigChange covers the no-restart requirement:
// flipping the toggle in config.env is observed on the next load once the file
// mtime advances.
func TestForwardLoader_ReloadsOnConfigChange(t *testing.T) {
	clearForwardEnv(t)
	const cloud = "https://cloud.example.test/"
	path := writeConfigEnvFile(t, map[string]string{
		"AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	})
	l := newForwardLoader(path, nil)
	require.False(t, l.load().enabled)

	require.NoError(t, os.WriteFile(path, []byte(
		"AGENTO11Y_LOCAL_FORWARD=true\nAGENTO11Y_ENDPOINT="+cloud+"\nAGENTO11Y_AUTH_TENANT_ID=t\nAGENTO11Y_AUTH_TOKEN=k\n"), 0o600))
	// Force a distinct mtime so the size+mtime cache notices the edit even when
	// the rewrite lands within the filesystem's timestamp granularity.
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(path, future, future))

	assert.True(t, l.load().enabled)
}

// TestBuildGenerationPayload_StripsThroughSharedPolicy covers the daemon's half
// of a reduced forward: buildGenerationPayload runs the shared strip over the
// decoded proto and then relabels the copy. Which fields carry content is
// contentcapture's contract, asserted field by field in that package, so this
// test asserts the call happens on the payload path and that the relabel
// follows it.
func TestBuildGenerationPayload_StripsThroughSharedPolicy(t *testing.T) {
	cases := []struct {
		name   string
		gen    *agento11yv1.Generation
		assert func(t *testing.T, g *agento11yv1.Generation)
	}{
		{
			// Every content value in the fixture is marked, so one scan of the
			// forwarded bytes covers the string, bytes, and metadata fields
			// alike without restating contentcapture's field list here.
			name: "content_and_mirrors_removed",
			gen:  contentRichGeneration(t),
			assert: func(t *testing.T, g *agento11yv1.Generation) {
				raw, err := proto.Marshal(g)
				require.NoError(t, err)
				assert.NotContains(t, strings.ToLower(string(raw)), contentMarker)

				// Structure the reduced copy still has to carry.
				assert.Equal(t, "gen-1", g.GetId())
				assert.Equal(t, "sdk_error", g.GetCallError())
				assert.Equal(t, "do_thing", g.GetTools()[0].GetName())
				assert.Equal(t, "t1", g.GetInput()[1].GetParts()[0].GetToolResult().GetToolCallId())
				assert.Equal(t, int64(10), g.GetUsage().GetInputTokens())
				assert.Equal(t, "structural", g.GetMetadata().GetFields()["keep_me"].GetStringValue())
			},
		},
		{
			// LaunchEnv.Apply forces CONTENT_CAPTURE_MODE=full into the agent so
			// the local viewer keeps everything, so every payload the daemon
			// forwards arrives stamped "full" and the reduced copy has to say
			// otherwise.
			name: "incoming_full_stamp_is_relabeled",
			gen: &agento11yv1.Generation{
				Id:       "gen-1",
				Metadata: mustStruct(t, map[string]any{model.MetadataKeyContentCaptureMode: "full"}),
			},
			assert: func(t *testing.T, g *agento11yv1.Generation) {
				assert.Len(t, g.GetMetadata().GetFields(), 1, "only the rewritten stamp is left")
			},
		},
		{
			// The strip leaves Metadata unset when content keys were all it
			// held, so the relabel has to build the container instead of
			// writing through a nil map.
			name: "stamp_added_when_metadata_ends_up_empty",
			gen: &agento11yv1.Generation{
				Id:       "gen-1",
				Metadata: mustStruct(t, map[string]any{metadataKeyConversationTitle: "secret title"}),
			},
			assert: func(t *testing.T, g *agento11yv1.Generation) {
				assert.NotContains(t, g.GetMetadata().GetFields(), metadataKeyConversationTitle)
				assert.Len(t, g.GetMetadata().GetFields(), 1, "only the stamp is left")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := buildGenerationPayload([]json.RawMessage{generationRawJSON(t, tc.gen)}, true)
			require.NoError(t, err)
			req, err := wire.UnmarshalExportGenerationsJSON(payload)
			require.NoError(t, err)
			require.Len(t, req.GetGenerations(), 1)

			g := req.GetGenerations()[0]
			assert.Equal(t, model.ContentCaptureModeMetadataOnly,
				g.GetMetadata().GetFields()[model.MetadataKeyContentCaptureMode].GetStringValue())
			tc.assert(t, g)
		})
	}
}

func TestForwardLoader_ForwardGenerations(t *testing.T) {
	cases := []struct {
		name       string
		strip      bool
		wantMarker string
		wantSystem string
	}{
		{name: "full_keeps_content", strip: false, wantMarker: "full", wantSystem: "secret system prompt"},
		{name: "metadata_only_strips_content", strip: true, wantMarker: "metadata_only", wantSystem: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			received := make(chan []byte, 1)
			var gotPath, gotAuth, gotTenant string
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotAuth = r.Header.Get("Authorization")
				gotTenant = r.Header.Get(wire.TenantHeaderName)
				b, _ := io.ReadAll(r.Body)
				received <- b
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()

			l := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
			l.client = srv.Client()
			cfg := fakeForwardConfig(t, srv.URL, tc.strip)

			raw := generationRawJSON(t, contentRichGeneration(t))
			l.enqueue(forwardLabelGenerations, func() { l.forwardGenerations(cfg, []json.RawMessage{raw}) })
			l.wait()

			body := <-received
			assert.Equal(t, wire.GenerationExportHTTPPath, gotPath)
			assert.Equal(t, cfg.genHeaders["Authorization"], gotAuth)
			assert.Equal(t, "tenant-x", gotTenant)

			req, err := wire.UnmarshalExportGenerationsJSON(body)
			require.NoError(t, err)
			require.Len(t, req.GetGenerations(), 1)
			g := req.GetGenerations()[0]

			// Structural fields survive regardless of mode.
			assert.Equal(t, "gen-1", g.GetId())
			assert.Equal(t, "do_thing", g.GetTools()[0].GetName())
			assert.Equal(t, int64(10), g.GetUsage().GetInputTokens())

			assert.Equal(t, tc.wantSystem, g.GetSystemPrompt())
			assert.Equal(t, tc.wantMarker, g.GetMetadata().GetFields()[model.MetadataKeyContentCaptureMode].GetStringValue())
			if tc.strip {
				assert.Empty(t, g.GetOutput()[0].GetParts()[0].GetThinking())
				assert.Nil(t, g.GetOutput()[0].GetParts()[1].GetToolCall().GetInputJson())
				assert.NotContains(t, g.GetMetadata().GetFields(), metadataKeyConversationTitle)
			} else {
				assert.Equal(t, "thinking secret", g.GetOutput()[0].GetParts()[0].GetThinking())
				assert.Equal(t, "secret title", g.GetMetadata().GetFields()[metadataKeyConversationTitle].GetStringValue())
			}
		})
	}
}

// TestForwardLoader_ForwardGenerationsSkipsWhenDisabled covers the opt-in
// guarantee at the relay level: a disabled config never issues a request.
func TestForwardLoader_ForwardGenerationsSkipsWhenDisabled(t *testing.T) {
	hits := make(chan struct{}, 1)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
	l.client = srv.Client()
	cfg := fakeForwardConfig(t, srv.URL, true)
	cfg.enabled = false

	raw := generationRawJSON(t, contentRichGeneration(t))
	l.forwardGenerations(cfg, []json.RawMessage{raw})
	l.forwardOTLP(cfg, "traces", "application/x-protobuf", "", []byte("body"))
	l.wait()
	assert.Empty(t, hits)
}

// TestForwardLoader_ForwardOTLP covers the relay matrix: which signal, whether
// the reduced content mode applies, and the inbound encoding. Metrics and full
// traces must arrive byte-identical with their encoding preserved; reduced
// traces are decompressed, stripped, and relayed uncompressed.
func TestForwardLoader_ForwardOTLP(t *testing.T) {
	type capture struct {
		path            string
		contentType     string
		contentEncoding string
		body            []byte
	}
	received := make(chan capture, 4)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		received <- capture{
			path:            r.URL.Path,
			contentType:     r.Header.Get("Content-Type"),
			contentEncoding: r.Header.Get("Content-Encoding"),
			body:            b,
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := newForwardLoader(filepath.Join(t.TempDir(), "config.env"), nil)
	l.client = srv.Client()

	metricsBody := []byte("\x00\x01\x02metrics-protobuf-bytes")
	tracesBody, err := proto.Marshal(traceRequestWithContent())
	require.NoError(t, err)
	tracesJSON, err := protojson.Marshal(traceRequestWithContent())
	require.NoError(t, err)

	cases := []struct {
		name string
		// signal, strip, and the inbound body/headers are the whole input.
		signal      string
		strip       bool
		contentType string
		gzip        bool
		body        []byte
		// wantPath is the Cloud signal path; wantVerbatim asserts a
		// byte-identical relay (with the encoding preserved), otherwise the
		// body must decode as a stripped trace export.
		wantPath        string
		wantVerbatim    bool
		wantContentType string
	}{
		{
			name: "metrics_relayed_verbatim_even_when_stripping",
			// strip must never touch metrics: they carry no content.
			signal: "metrics", strip: true, contentType: "application/x-protobuf", body: metricsBody,
			wantPath: "/v1/metrics", wantVerbatim: true,
		},
		{
			name:   "gzip_metrics_keeps_encoding",
			signal: "metrics", strip: true, contentType: "application/x-protobuf", gzip: true, body: metricsBody,
			wantPath: "/v1/metrics", wantVerbatim: true,
		},
		{
			name:   "full_traces_relayed_verbatim",
			signal: "traces", strip: false, contentType: "application/x-protobuf", body: tracesBody,
			wantPath: "/v1/traces", wantVerbatim: true,
		},
		{
			name:   "gzip_full_traces_keeps_encoding",
			signal: "traces", strip: false, contentType: "application/x-protobuf", gzip: true, body: tracesBody,
			wantPath: "/v1/traces", wantVerbatim: true,
		},
		{
			name:   "reduced_traces_stripped",
			signal: "traces", strip: true, contentType: "application/x-protobuf", body: tracesBody,
			wantPath: "/v1/traces", wantContentType: wire.ContentTypeProto,
		},
		{
			name:   "gzip_reduced_traces_decompressed_then_stripped",
			signal: "traces", strip: true, contentType: "application/x-protobuf", gzip: true, body: tracesBody,
			wantPath: "/v1/traces", wantContentType: wire.ContentTypeProto,
		},
		{
			// The JSON branch of stripTracePayload: an exporter configured for
			// OTLP/JSON must round-trip through the same strip.
			name:   "reduced_json_traces_stripped",
			signal: "traces", strip: true, contentType: "application/json", body: tracesJSON,
			wantPath: "/v1/traces", wantContentType: wire.ContentTypeJSON,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := fakeForwardConfig(t, srv.URL, tc.strip)
			body, encoding := tc.body, ""
			if tc.gzip {
				body, encoding = gzipBytes(t, tc.body), "gzip"
			}
			l.enqueue(otlpForwardLabel(tc.signal), func() { l.forwardOTLP(cfg, tc.signal, tc.contentType, encoding, body) })
			l.wait()

			c := <-received
			assert.Equal(t, tc.wantPath, c.path)
			if tc.wantVerbatim {
				assert.Equal(t, body, c.body)
				assert.Equal(t, encoding, c.contentEncoding)
				assert.Equal(t, tc.contentType, c.contentType)
				return
			}
			// The stripped payload is re-marshaled uncompressed, so the gzip
			// encoding must be dropped.
			assert.Empty(t, c.contentEncoding)
			assert.Equal(t, tc.wantContentType, c.contentType)
			assertTraceStripped(t, c.body, c.contentType)
		})
	}
}

// TestForwardLoader_OTLPHeaderParity covers the OTLP leg authenticating the way
// the real exporter does: a dedicated OTEL_AUTH_TOKEN wins over AUTH_TOKEN, and
// an explicit Authorization in OTEL_EXPORTER_OTLP_HEADERS wins over both.
func TestForwardLoader_OTLPHeaderParity(t *testing.T) {
	basic := func(tenant, token string) string {
		v, ok := otel.BasicAuthHeaderValue(tenant, token)
		require.True(t, ok)
		return v
	}
	cases := []struct {
		name  string
		extra map[string]string
		want  string
	}{
		{
			name: "falls_back_to_auth_token",
			want: basic("t", "k"),
		},
		{
			name:  "otel_auth_token_wins",
			extra: map[string]string{"AGENTO11Y_OTEL_AUTH_TOKEN": "gateway"},
			want:  basic("t", "gateway"),
		},
		{
			name:  "explicit_authorization_header_wins",
			extra: map[string]string{"OTEL_EXPORTER_OTLP_HEADERS": "Authorization=Bearer explicit"},
			want:  "Bearer explicit",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			lines := map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "https://cloud.example.test/",
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://otlp.example.test",
			}
			maps.Copy(lines, tc.extra)
			cfg := newForwardLoader(writeConfigEnvFile(t, lines), nil).load()
			require.True(t, cfg.enabled)
			assert.Equal(t, tc.want, cfg.otlpHeaders["Authorization"])
			// The generation leg always uses the tenant/token pair.
			assert.Equal(t, basic("t", "k"), cfg.genHeaders["Authorization"])
		})
	}
}

// TestForwardLoader_InsecureOTLPEndpointStaysLocalGuarded covers the insecure
// downgrade: OTEL_EXPORTER_OTLP_INSECURE drops https to http the way the
// exporter does, and the local-receiver guard runs on the resulting URL so the
// downgrade cannot reintroduce a relay loop.
func TestForwardLoader_InsecureOTLPEndpointStaysLocalGuarded(t *testing.T) {
	cases := []struct {
		name     string
		otlp     string
		wantOTLP string
	}{
		{name: "downgrades_remote_to_http", otlp: "otlp.example.test:4318", wantOTLP: "http://otlp.example.test:4318"},
		{name: "downgraded_local_is_dropped", otlp: "127.0.0.1:4318", wantOTLP: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearForwardEnv(t)
			cfg := newForwardLoader(writeConfigEnvFile(t, map[string]string{
				"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "https://cloud.example.test/",
				"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": tc.otlp,
				"OTEL_EXPORTER_OTLP_INSECURE":           "true",
			}), nil).load()
			assert.Equal(t, tc.wantOTLP, cfg.otlpEndpoint)
		})
	}
}

// TestForwardLoader_StatusReportsFailures covers the only channel a runtime
// forward failure reaches the user through: the daemon's logger is discarded
// unless debug logging is on, so forwardStatus carries the failures since the
// last success.
func TestForwardLoader_StatusReportsFailures(t *testing.T) {
	var status int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":"unknown field parts.media.url"}`))
	}))
	defer srv.Close()

	clearForwardEnv(t)
	l := newForwardLoader(writeConfigEnvFile(t, map[string]string{
		"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": srv.URL,
		"AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k",
	}), nil)
	l.client = srv.Client()
	cfg := l.load()
	require.True(t, cfg.enabled)
	assert.Empty(t, l.status().Failures)

	status = http.StatusBadRequest
	raw := generationRawJSON(t, contentRichGeneration(t))
	l.forwardGenerations(cfg, []json.RawMessage{raw})
	failures := l.status().Failures
	require.Len(t, failures, 1)
	assert.Equal(t, forwardLabelGenerations, failures[0].Label)
	// The response body is the only place the rejected field name appears.
	assert.Contains(t, failures[0].Detail, "status 400")
	assert.Contains(t, failures[0].Detail, "parts.media.url")
	assert.NotEmpty(t, failures[0].At)

	// A success clears the leg, so a non-empty Failures means "failing now".
	status = http.StatusOK
	l.forwardGenerations(cfg, []json.RawMessage{raw})
	assert.Empty(t, l.status().Failures)

	// Only the leg that delivered is cleared. config.env cannot configure the
	// OTLP leg at a 127.0.0.1 test server (otlpForwardTarget refuses a loopback
	// endpoint as a relay loop), so point it at the same server by hand.
	cfg.otlpEndpoint = srv.URL
	status = http.StatusBadRequest
	l.forwardGenerations(cfg, []json.RawMessage{raw})
	require.Len(t, l.status().Failures, 1)

	status = http.StatusOK
	l.forwardOTLP(cfg, "metrics", wire.ContentTypeProto, "", []byte("metrics-payload"))
	failures = l.status().Failures
	require.Len(t, failures, 1, "a metrics success must not clear a generations failure")
	assert.Equal(t, forwardLabelGenerations, failures[0].Label)

	l.forwardGenerations(cfg, []json.RawMessage{raw})
	assert.Empty(t, l.status().Failures)
}

// TestForwardLoader_FailureRingIsPerLeg covers the ring's capacity: the hook
// leg records once per tool call, so a global cap would let a chatty leg evict
// the one entry another leg had, which is the only report that leg gets.
func TestForwardLoader_FailureRingIsPerLeg(t *testing.T) {
	clearForwardEnv(t)
	l := newForwardLoader(writeConfigEnvFile(t, nil), nil)

	l.recordFailuref(forwardLabelGenerations, "generation export rejected")
	l.recordFailuref(otlpForwardLabel("traces"), "traces rejected")
	for i := range maxRecordedForwardFailures * 2 {
		l.recordFailuref(forwardLabelHooks, "hook call %d failed", i)
	}

	byLabel := map[string]int{}
	for _, f := range l.status().Failures {
		byLabel[f.Label]++
	}
	assert.Equal(t, map[string]int{
		forwardLabelGenerations:    1,
		otlpForwardLabel("traces"): 1,
		forwardLabelHooks:          maxRecordedForwardFailures,
	}, byLabel)

	// Within a leg the cap keeps the most recent attempts.
	assert.Contains(t, l.status().Failures[0].Detail, "hook call 9 failed")
}

// TestForwardLoader_StatusReportsUnreadableConfig covers the fail-closed
// branch: an unreadable config.env disables forwarding and says why, rather
// than falling back to the daemon's boot-time environment.
func TestForwardLoader_StatusReportsUnreadableConfig(t *testing.T) {
	clearForwardEnv(t)
	t.Setenv(envconfig.PreferredKey("LOCAL_FORWARD"), "true")
	t.Setenv(envconfig.PreferredKey("ENDPOINT"), "https://cloud.example.test/")
	t.Setenv(envconfig.PreferredKey("AUTH_TENANT_ID"), "t")
	t.Setenv(envconfig.PreferredKey("AUTH_TOKEN"), "k")

	// Two ways to be unreadable: os.Stat itself fails (a path whose parent is
	// a file errors with ENOTDIR rather than IsNotExist), or os.Stat succeeds
	// and the open fails. Both must refuse to forward, since the daemon's
	// process environment still holds the boot-time LOCAL_FORWARD=true.
	t.Run("stat fails", func(t *testing.T) {
		parent := filepath.Join(t.TempDir(), "not-a-dir")
		require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
		assertForwardRefusedAsUnreadable(t, filepath.Join(parent, "config.env"))
	})

	t.Run("open fails", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores file permissions")
		}
		path := filepath.Join(t.TempDir(), "config.env")
		require.NoError(t, os.WriteFile(path, []byte(envconfig.PreferredKey("LOCAL_FORWARD")+"=false\n"), 0o600))
		require.NoError(t, os.Chmod(path, 0o000))

		l := assertForwardRefusedAsUnreadable(t, path)

		// Chmod changes neither size nor mtime, so the refusal must not be
		// cached against them: the file's own "off" has to win once it can be
		// read.
		require.NoError(t, os.Chmod(path, 0o600))
		st := l.status()
		assert.False(t, st.Enabled)
		assert.Empty(t, st.Reason)
	})
}

// assertForwardRefusedAsUnreadable asserts a loader over path forwards nothing
// and reports the config as unreadable, and returns the loader.
func assertForwardRefusedAsUnreadable(t *testing.T, path string) *forwardLoader {
	t.Helper()
	l := newForwardLoader(path, nil)

	assert.False(t, l.load().enabled)
	st := l.status()
	assert.False(t, st.Enabled)
	assert.Equal(t, forwardModeOff, st.Mode)
	assert.Contains(t, st.Reason, "config.env unreadable")
	assert.Contains(t, st.HookReason, "config.env unreadable")
	// The guard knobs keep their documented defaults. An unreadable file is
	// not an explicit "fail closed", and reading it as one would be the
	// opposite of what a user who never set the key asked for.
	cfg := l.load()
	assert.True(t, cfg.failOpen)
	assert.Equal(t, envconfig.DefaultGuardsTimeoutMs, cfg.timeoutMs)
	return l
}

// TestBuildGenerationPayload_DiscardsUnknownFields covers version skew: a
// generation from an exporter newer than this daemon must still forward (minus
// the field the daemon cannot strip) rather than costing the whole batch.
func TestBuildGenerationPayload_DiscardsUnknownFields(t *testing.T) {
	raw := json.RawMessage(`{"id":"gen-1","system_prompt":"secret","not_a_field_yet":{"x":1}}`)
	payload, err := buildGenerationPayload([]json.RawMessage{raw}, true)
	require.NoError(t, err)
	req, err := wire.UnmarshalExportGenerationsJSON(payload)
	require.NoError(t, err)
	require.Len(t, req.GetGenerations(), 1)
	assert.Empty(t, req.GetGenerations()[0].GetSystemPrompt())
	assert.NotContains(t, string(payload), "not_a_field_yet")
}

// TestStripTraceContent_RemovesContentKeepsStructure covers the daemon's span
// rewrite. The daemon cannot reuse the SDK's span redaction: the SDK redacts at
// emit time, swapping the raw provider message for the category before
// RecordError on a generation span (go/agento11y/client.go) and skipping
// RecordError altogether on tool and embedding spans, while these spans are
// already finished and must be rewritten. contentcapture shares only the keys
// and the two replacement strings, so the cases below pin the wire keys, the
// exception event name, and both replacement status messages.
func TestStripTraceContent_RemovesContentKeepsStructure(t *testing.T) {
	// One case per content key: the key goes, a structural neighbour stays.
	for _, key := range traceContentKeys {
		t.Run("drops_"+key, func(t *testing.T) {
			span := &tracepb.Span{Attributes: []*commonpb.KeyValue{
				stringKV(key, "secret"),
				stringKV("gen_ai.tool.name", "do_thing"),
			}}
			stripTraceContent(traceRequestForSpans(span))
			assert.Equal(t, []string{"gen_ai.tool.name"}, attributeKeys(span.GetAttributes()))
		})
	}

	// A surviving event's own attributes go through the same filter.
	t.Run("event_attributes_filtered", func(t *testing.T) {
		event := &tracepb.Span_Event{Name: "structural-event", Attributes: []*commonpb.KeyValue{
			stringKV("gen_ai.tool.call.result", "secret result"),
			stringKV("gen_ai.tool.name", "do_thing"),
		}}
		stripTraceContent(traceRequestForSpans(&tracepb.Span{Events: []*tracepb.Span_Event{event}}))
		assert.Equal(t, []string{"gen_ai.tool.name"}, attributeKeys(event.GetAttributes()))
	})

	cases := []struct {
		name string
		span *tracepb.Span
		// wantAttrs and wantEvents are the surviving keys and event names in
		// order; wantStatus is the forwarded status message.
		wantAttrs  []string
		wantEvents []string
		wantStatus string
	}{
		{
			// RecordError puts the raw provider text on the exception event's
			// attributes, so the whole event goes rather than being filtered.
			// Every other event stays.
			name: "exception_event_dropped_structural_event_kept",
			span: &tracepb.Span{
				Events: []*tracepb.Span_Event{
					{Name: "exception", Attributes: []*commonpb.KeyValue{stringKV("exception.message", "secret")}},
					{Name: "structural-event"},
				},
			},
			wantAttrs:  []string{},
			wantEvents: []string{"structural-event"},
		},
		{
			// The status message is built from the same raw provider error the
			// exception event carried, so it is replaced by the classified
			// category, which carries no content and stays on the span.
			name: "error_category_replaces_status_message",
			span: &tracepb.Span{
				Status: &tracepb.Status{
					Code:    tracepb.Status_STATUS_CODE_ERROR,
					Message: "secret provider error: messages.1.content secret fragment",
				},
				Attributes: []*commonpb.KeyValue{
					stringKV("error.category", "rate_limit"),
					stringKV("gen_ai.tool.call.arguments", "secret args"),
					stringKV("gen_ai.tool.name", "do_thing"),
				},
				Events: []*tracepb.Span_Event{{Name: "exception"}},
			},
			wantAttrs:  []string{"error.category", "gen_ai.tool.name"},
			wantEvents: []string{},
			wantStatus: "rate_limit",
		},
		{
			// The category is read by a scan over the whole attribute list, so
			// it resolves from the last position as well as the first. The
			// strip's own stripSpanStatus-before-filter ordering is not
			// observable here: error.category is not a content key, so it
			// survives the filter either way.
			name: "error_category_resolves_from_last_attribute",
			span: &tracepb.Span{
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "secret provider error"},
				Attributes: []*commonpb.KeyValue{
					stringKV("gen_ai.tool.call.arguments", "secret args"),
					stringKV("gen_ai.tool.description", "secret description"),
					stringKV("error.category", "timeout"),
				},
			},
			wantAttrs:  []string{"error.category"},
			wantEvents: []string{},
			wantStatus: "timeout",
		},
		{
			name: "missing_error_category_falls_back",
			span: &tracepb.Span{
				Status:     &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "secret provider error"},
				Attributes: []*commonpb.KeyValue{stringKV("gen_ai.tool.name", "do_thing")},
			},
			wantAttrs:  []string{"gen_ai.tool.name"},
			wantEvents: []string{},
			wantStatus: "sdk_error",
		},
		{
			// An empty category is no category: it would leave the forwarded span
			// with no signal that the call failed.
			name: "empty_error_category_falls_back",
			span: &tracepb.Span{
				Status:     &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "secret provider error"},
				Attributes: []*commonpb.KeyValue{stringKV("error.category", "")},
			},
			wantAttrs:  []string{"error.category"},
			wantEvents: []string{},
			wantStatus: "sdk_error",
		},
		{
			// A non-error status carries no meaning in the message field, so
			// there is nothing to replace it with.
			name: "non_error_status_message_cleared",
			span: &tracepb.Span{
				Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_OK, Message: "secret detail"},
			},
			wantAttrs:  []string{},
			wantEvents: []string{},
			wantStatus: "",
		},
		{
			// The in-place filters skip nil entries rather than relaying them.
			name: "nil_attributes_and_events_dropped",
			span: &tracepb.Span{
				Attributes: []*commonpb.KeyValue{nil, stringKV("gen_ai.tool.name", "do_thing"), nil},
				Events:     []*tracepb.Span_Event{nil, {Name: "structural-event"}},
			},
			wantAttrs:  []string{"gen_ai.tool.name"},
			wantEvents: []string{"structural-event"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stripTraceContent(traceRequestForSpans(tc.span))
			assert.Equal(t, tc.wantAttrs, attributeKeys(tc.span.GetAttributes()))
			assert.Equal(t, tc.wantEvents, eventNames(tc.span.GetEvents()))
			assert.Equal(t, tc.wantStatus, tc.span.GetStatus().GetMessage())
		})
	}
}

// assertTraceStripped decodes an uncompressed OTLP trace export and asserts the
// content attributes, exception event, and raw error status message were
// removed while structural attributes and events survive.
func assertTraceStripped(t *testing.T, body []byte, contentType string) {
	t.Helper()
	var got coltracepb.ExportTraceServiceRequest
	if strings.Contains(contentType, "json") {
		require.NoError(t, protojson.Unmarshal(body, &got))
	} else {
		require.NoError(t, proto.Unmarshal(body, &got))
	}
	span := got.GetResourceSpans()[0].GetScopeSpans()[0].GetSpans()[0]
	keys := attributeKeys(span.GetAttributes())
	for _, blocked := range traceContentKeys {
		assert.NotContains(t, keys, blocked)
	}
	assert.Contains(t, keys, "gen_ai.tool.name")
	assert.Contains(t, keys, "gen_ai.usage.input_tokens")

	// Exception event (raw error text) dropped; structural event kept.
	require.Len(t, span.GetEvents(), 1)
	assert.Equal(t, "structural-event", span.GetEvents()[0].GetName())

	// The status message carries the same raw provider error the exception
	// event did, so it is replaced by the classified category.
	assert.Equal(t, "invalid_request", span.GetStatus().GetMessage())
	assert.Equal(t, tracepb.Status_STATUS_CODE_ERROR, span.GetStatus().GetCode())
}

// fakeForwardConfig builds a resolved config that targets the given Cloud URL,
// bypassing the local-endpoint guard so tests can point at a local TLS server.
func fakeForwardConfig(t *testing.T, cloudURL string, strip bool) forwardConfig {
	t.Helper()
	genURL, err := wire.NormalizeGenerationExportURL(cloudURL, false)
	require.NoError(t, err)
	auth, ok := otel.BasicAuthHeaderValue("tenant-x", "token-y")
	require.True(t, ok)
	return forwardConfig{
		enabled:      true,
		strip:        strip,
		genURL:       genURL,
		genHeaders:   map[string]string{"Authorization": auth, wire.TenantHeaderName: "tenant-x"},
		otlpEndpoint: cloudURL,
		otlpHeaders:  map[string]string{"Authorization": auth},
	}
}

// contentRichGeneration builds a generation with every content-bearing field
// populated. Each of those values contains contentMarker; every field the strip
// retains is free of it.
func contentRichGeneration(t *testing.T) *agento11yv1.Generation {
	t.Helper()
	return &agento11yv1.Generation{
		Id:             "gen-1",
		ConversationId: "conv-1",
		Model:          &agento11yv1.ModelRef{Provider: "anthropic", Name: "claude"},
		SystemPrompt:   "secret system prompt",
		CallError:      "boom: secret detail",
		Usage:          &agento11yv1.TokenUsage{InputTokens: 10, OutputTokens: 5},
		Tags:           map[string]string{"env": "test"},
		Input: []*agento11yv1.Message{
			{
				Role: agento11yv1.MessageRole_MESSAGE_ROLE_USER,
				Parts: []*agento11yv1.Part{
					{Payload: &agento11yv1.Part_Text{Text: "user secret"}},
					{Payload: &agento11yv1.Part_Media{Media: &agento11yv1.Media{
						Kind:     "image",
						Url:      "data:image/png;base64,SECRET_IMAGE_BYTES",
						MimeType: "image/png",
						Name:     "screenshot.png",
					}}},
				},
			},
			{
				Role: agento11yv1.MessageRole_MESSAGE_ROLE_TOOL,
				Parts: []*agento11yv1.Part{{Payload: &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{
					ToolCallId:  "t1",
					Name:        "do_thing",
					Content:     "secret result",
					ContentJson: []byte(`{"r":"secret"}`),
				}}}},
			},
		},
		Output: []*agento11yv1.Message{{
			Role: agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT,
			Parts: []*agento11yv1.Part{
				{Payload: &agento11yv1.Part_Thinking{Thinking: "thinking secret"}},
				{Payload: &agento11yv1.Part_ToolCall{ToolCall: &agento11yv1.ToolCall{
					Id:        "t1",
					Name:      "do_thing",
					InputJson: []byte(`{"arg":"secret"}`),
				}}},
			},
		}},
		Tools: []*agento11yv1.ToolDefinition{{
			Name:            "do_thing",
			Description:     "secret tool description",
			InputSchemaJson: []byte(`{"type":"object","description":"secret schema"}`),
		}},
		RawArtifacts: []*agento11yv1.Artifact{{
			Kind:    agento11yv1.ArtifactKind_ARTIFACT_KIND_REQUEST,
			Payload: []byte("secret raw artifact"),
		}},
		Metadata: mustStruct(t, map[string]any{
			model.MetadataKeyContentCaptureMode: "full",
			metadataKeyConversationTitle:        "secret title",
			legacyConversationTitleKey:          "secret legacy title",
			"call_error":                        "boom: secret detail",
			"keep_me":                           "structural",
		}),
	}
}

func generationRawJSON(t *testing.T, g *agento11yv1.Generation) json.RawMessage {
	t.Helper()
	envelope, err := wire.MarshalExportGenerationsJSON(&agento11yv1.ExportGenerationsRequest{
		Generations: []*agento11yv1.Generation{g},
	})
	require.NoError(t, err)
	var req generationsRequest
	require.NoError(t, json.Unmarshal(envelope, &req))
	require.Len(t, req.Generations, 1)
	return req.Generations[0]
}

// traceRequestForSpans wraps spans in the one-resource, one-scope envelope the
// strip walks.
func traceRequestForSpans(spans ...*tracepb.Span) *coltracepb.ExportTraceServiceRequest {
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: spans}},
		}},
	}
}

func traceRequestWithContent() *coltracepb.ExportTraceServiceRequest {
	span := &tracepb.Span{
		Name: "chat claude",
		// The SDK builds the status message from the same raw provider error it
		// records on the exception event, so it can echo prompt fragments.
		Status: &tracepb.Status{
			Code:    tracepb.Status_STATUS_CODE_ERROR,
			Message: "secret provider error: messages.1.content secret prompt fragment",
		},
		Attributes: []*commonpb.KeyValue{
			stringKV("error.category", "invalid_request"),
			stringKV("gen_ai.tool.call.arguments", "secret args"),
			stringKV("gen_ai.tool.call.result", "secret result"),
			stringKV("gen_ai.tool.description", "secret description"),
			stringKV(metadataKeyConversationTitle, "secret title"),
			stringKV(legacyConversationTitleKey, "secret legacy title"),
			stringKV("gen_ai.embeddings.input_texts", "secret texts"),
			stringKV("gen_ai.tool.name", "do_thing"),
			stringKV("gen_ai.usage.input_tokens", "10"),
		},
		Events: []*tracepb.Span_Event{
			{Name: "exception", Attributes: []*commonpb.KeyValue{stringKV("exception.message", "secret error text")}},
			{Name: "structural-event", Attributes: []*commonpb.KeyValue{stringKV("gen_ai.tool.name", "do_thing")}},
		},
	}
	return traceRequestForSpans(span)
}

func stringKV(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}},
	}
}

func gzipBytes(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, err := zw.Write(in)
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func attributeKeys(attrs []*commonpb.KeyValue) []string {
	out := make([]string, 0, len(attrs))
	for _, kv := range attrs {
		out = append(out, kv.GetKey())
	}
	return out
}

func eventNames(events []*tracepb.Span_Event) []string {
	out := make([]string, 0, len(events))
	for _, e := range events {
		out = append(out, e.GetName())
	}
	return out
}

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	require.NoError(t, err)
	return s
}
