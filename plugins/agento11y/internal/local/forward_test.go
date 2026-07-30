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
// would have the suite POST to their live tenant.
func clearForwardEnv(t *testing.T) {
	t.Helper()
	envconfig.PinAliasEnvBlank(t)
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
		},
		{
			name:           "enabled_full_does_not_strip",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k", "AGENTO11Y_CONTENT_CAPTURE_MODE": "full"},
			wantEnabled:    true,
			wantStrip:      false,
			wantStatusMode: forwardModeFull,
		},
		{
			name:           "advanced_capture_mode_still_strips",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k", "AGENTO11Y_CONTENT_CAPTURE_MODE": "no_tool_content"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
		},
		{
			name:           "legacy_spellings_resolve",
			lines:          map[string]string{"SIGIL_LOCAL_FORWARD": "true", "SIGIL_ENDPOINT": cloud, "SIGIL_AUTH_TENANT_ID": "t", "SIGIL_AUTH_TOKEN": "k", "SIGIL_CONTENT_CAPTURE_MODE": "full"},
			wantEnabled:    true,
			wantStrip:      false,
			wantStatusMode: forwardModeFull,
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
		},
		{
			name:           "allows_local_endpoint",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://127.0.0.1:4317", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
		},
		{
			name:           "allows_local_endpoint_with_placeholder_creds",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://127.0.0.1:8080", "AGENTO11Y_AUTH_TENANT_ID": "local", "AGENTO11Y_AUTH_TOKEN": "local"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
		},
		{
			name:           "allows_local_endpoint_without_creds",
			lines:          map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://localhost:8080"},
			wantEnabled:    true,
			wantStrip:      true,
			wantStatusMode: forwardModeMetadataOnly,
		},
		{
			name:             "refuses_placeholder_creds",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": cloud, "AGENTO11Y_AUTH_TENANT_ID": "local", "AGENTO11Y_AUTH_TOKEN": "local"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
		},
		{
			name:             "refuses_empty_endpoint",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
		},
		{
			name:             "refuses_invalid_endpoint",
			lines:            map[string]string{"AGENTO11Y_LOCAL_FORWARD": "true", "AGENTO11Y_ENDPOINT": "http://%", "AGENTO11Y_AUTH_TENANT_ID": "t", "AGENTO11Y_AUTH_TOKEN": "k"},
			wantEnabled:      false,
			wantStatusMode:   forwardModeOff,
			wantStatusReason: true,
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
			st := l.status()
			assert.Equal(t, tc.wantEnabled, st.Enabled)
			assert.Equal(t, tc.wantStatusMode, st.Mode)
			assert.Equal(t, tc.wantOTLP, st.OTLP)
			assert.Equal(t, cfg.genURL != "", st.Generations)
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

// TestStripGenerationProto_ClearsContentKeepsStructure asserts the daemon-side
// proto strip clears the content fields the SDK's stripContent /
// stripMessageContent clear (go/agento11y/content_capture.go) while preserving
// message structure, tool names, usage, tags, and unrelated metadata. The two
// lists are kept in sync by hand, so a new content field in the proto needs a
// case here and in stripGenerationProto.
func TestStripGenerationProto_ClearsContentKeepsStructure(t *testing.T) {
	g := contentRichGeneration(t)
	stripGenerationProto(g)

	assert.Empty(t, g.GetSystemPrompt(), "system prompt cleared")
	assert.Nil(t, g.GetRawArtifacts(), "raw artifacts cleared")
	assert.Equal(t, strippedCallError, g.GetCallError(), "call error replaced")

	// Input user text cleared; part still present (kind preserved).
	require.Len(t, g.GetInput()[0].GetParts(), 2)
	assert.Empty(t, g.GetInput()[0].GetParts()[0].GetText())

	// Output thinking + tool-call arguments cleared; tool-call name kept.
	out := g.GetOutput()[0].GetParts()
	assert.Empty(t, out[0].GetThinking())
	assert.Nil(t, out[1].GetToolCall().GetInputJson())
	assert.Equal(t, "do_thing", out[1].GetToolCall().GetName())

	// Tool-result content cleared; identifiers kept.
	tr := g.GetInput()[1].GetParts()[0].GetToolResult()
	assert.Empty(t, tr.GetContent())
	assert.Nil(t, tr.GetContentJson())
	assert.Equal(t, "t1", tr.GetToolCallId())

	// Media URL cleared (it carries data: URIs for pasted images and files);
	// the part and its descriptive fields stay.
	media := g.GetInput()[0].GetParts()[1].GetMedia()
	assert.Empty(t, media.GetUrl())
	assert.Equal(t, "image", media.GetKind())
	assert.Equal(t, "screenshot.png", media.GetName())

	// Tool description + schema cleared; name kept.
	assert.Empty(t, g.GetTools()[0].GetDescription())
	assert.Nil(t, g.GetTools()[0].GetInputSchemaJson())
	assert.Equal(t, "do_thing", g.GetTools()[0].GetName())

	// Metadata: content mirrors dropped (both the current and the pre-rename
	// title key), marker rewritten, unrelated keys kept.
	fields := g.GetMetadata().GetFields()
	assert.NotContains(t, fields, metadataKeyCallError)
	assert.NotContains(t, fields, metadataKeyConversationTitle)
	assert.NotContains(t, fields, legacyConversationTitleKey)
	assert.Equal(t, "metadata_only", fields[model.MetadataKeyContentCaptureMode].GetStringValue())
	assert.Equal(t, "structural", fields["keep_me"].GetStringValue())

	// Structural top-level fields untouched.
	assert.Equal(t, "gen-1", g.GetId())
	assert.Equal(t, "conv-1", g.GetConversationId())
	assert.Equal(t, int64(10), g.GetUsage().GetInputTokens())
	assert.Equal(t, "test", g.GetTags()["env"])
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
	for _, blocked := range []string{
		"gen_ai.tool.call.arguments", "gen_ai.tool.call.result",
		"gen_ai.tool.description", metadataKeyConversationTitle,
		legacyConversationTitleKey, "gen_ai.embeddings.input_texts",
	} {
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
			InputSchemaJson: []byte(`{"type":"object"}`),
		}},
		RawArtifacts: []*agento11yv1.Artifact{{
			Kind:    agento11yv1.ArtifactKind_ARTIFACT_KIND_REQUEST,
			Payload: []byte("secret raw artifact"),
		}},
		Metadata: mustStruct(t, map[string]any{
			model.MetadataKeyContentCaptureMode: "full",
			metadataKeyConversationTitle:        "secret title",
			legacyConversationTitleKey:          "secret legacy title",
			metadataKeyCallError:                "boom: secret detail",
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
			stringKV(spanAttrErrorCategory, "invalid_request"),
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
	return &coltracepb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}},
		}},
	}
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

func mustStruct(t *testing.T, m map[string]any) *structpb.Struct {
	t.Helper()
	s, err := structpb.NewStruct(m)
	require.NoError(t, err)
	return s
}
