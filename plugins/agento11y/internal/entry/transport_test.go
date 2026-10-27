package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/localhealth"
	"github.com/grafana/agento11y/plugins/agento11y/internal/login"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRun_LauncherGenerationTransport(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "copilot", "vibe", "kiro", "pi", "opencode"} {
		for _, tt := range []struct {
			name, protocol, optIn, collector, wantError string
			credentials, legacy, localMode, fromFile    bool
			wantLogin                                   int
		}{
			{name: "HTTP default", credentials: true},
			{name: "explicit HTTP", protocol: "http", credentials: true},
			{name: "HTTP still asks for setup", wantLogin: 1},
			{name: "OTLP only", protocol: "otel", optIn: "true", collector: "http://127.0.0.1:4318"},
			{name: "OTel config file", protocol: "otel", optIn: "true", collector: "http://127.0.0.1:4318", fromFile: true},
			{name: "legacy protocol", protocol: "otel", legacy: true, optIn: "true", collector: "http://127.0.0.1:4318"},
			{name: "missing opt-in", protocol: "otel", collector: "http://127.0.0.1:4318", wantError: "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true"},
			{name: "invalid opt-in", protocol: "otel", optIn: "maybe", collector: "http://127.0.0.1:4318", wantError: "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true"},
			{name: "HTTP credentials cannot bypass opt-in", protocol: "otel", credentials: true, collector: "http://127.0.0.1:4318", wantError: "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES=true"},
			{name: "missing collector", protocol: "otel", optIn: "true", wantError: "OTLP base endpoint"},
			{name: "invalid collector", protocol: "otel", optIn: "true", collector: "grpc://127.0.0.1:4317", wantError: "OTLP base endpoint"},
			{name: "unsupported protocol", protocol: "unsupported", credentials: true, wantError: "expected http or otel"},
			{name: "local independent plugin", protocol: "otel", optIn: "true", localMode: true},
		} {
			if tt.localMode && agent != "pi" && agent != "opencode" {
				continue
			}
			t.Run(agent+"/"+tt.name, func(t *testing.T) {
				isolateDotenvHome(t)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", tt.optIn)
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				key := envconfig.PreferredKey("PROTOCOL")
				if tt.legacy {
					key = envconfig.LegacyKey("PROTOCOL")
				}
				t.Setenv(key, tt.protocol)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", tt.collector)
				t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "full_with_metadata_spans")
				if tt.fromFile {
					t.Setenv(key, "")
					t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "")
					t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "")
					writeHistoryConfig(t, "AGENTO11Y_PROTOCOL="+tt.protocol+"\nAGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES="+tt.optIn+"\nAGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT="+tt.collector+"\n")
				}
				if tt.credentials {
					t.Setenv("AGENTO11Y_ENDPOINT", "http://127.0.0.1:8765")
					t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "test-tenant")
					t.Setenv("AGENTO11Y_AUTH_TOKEN", "test-token")
				}
				withStubPreflights(t, nil)
				starts, launches, logins := 0, 0, 0
				t.Cleanup(local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
					starts++
					t.Fatal("invalid transport must not start a daemon")
					return nil, nil
				}))
				withStubLauncher(t, agent, func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
					launches++
					if !tt.credentials {
						assert.Empty(t, envconfig.Getenv("ENDPOINT"))
						assert.Empty(t, envconfig.Getenv("AUTH_TENANT_ID"))
						assert.Empty(t, envconfig.Getenv("AUTH_TOKEN"))
					}
					return nil
				})
				withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
					logins++
					return login.Result{}, login.ErrNotInteractive
				})
				args := []string{agent}
				if tt.localMode {
					args = append(args, "--local")
				}
				var stdout, stderr bytes.Buffer
				code := withExit(t, func() { run(args, bytes.NewReader(nil), &stdout, &stderr) })
				wantError := tt.wantError
				if tt.protocol == "otel" && (agent == "pi" || agent == "opencode") {
					wantError = agent + " live capture does not support OTel"
					assert.Contains(t, stderr.String(), "AGENTO11Y_PROTOCOL=http")
					assert.Contains(t, stderr.String(), "history imports support OTel")
				}
				if wantError != "" {
					require.NotNil(t, code)
					assert.Equal(t, 1, *code)
					assert.Contains(t, stderr.String(), wantError)
					assert.Zero(t, launches)
				} else {
					require.Nil(t, code, "stderr=%s", stderr.String())
					assert.Equal(t, 1, launches)
				}
				assert.Equal(t, tt.wantLogin, logins)
				assert.Zero(t, starts)
			})
		}
	}
}

func TestRun_LocalLauncherRequiresGenerationCapability(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "copilot", "vibe", "kiro"} {
		for _, tt := range []struct {
			name, protocol string
			capable        bool
			wantLaunch     bool
		}{
			{name: "old daemon HTTP unchanged", wantLaunch: true},
			{name: "old daemon rejects OTel", protocol: "otel"},
			{name: "capable daemon", protocol: "otel", capable: true, wantLaunch: true},
		} {
			t.Run(agent+"/"+tt.name, func(t *testing.T) {
				isolateDotenvHome(t)
				t.Setenv("AGENTO11Y_PROTOCOL", tt.protocol)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				var probes atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Empty(t, r.Header.Get("Authorization"))
					if r.URL.Path == "/healthz" {
						probes.Add(1)
						capabilities := []string{}
						if tt.capable {
							capabilities = append(capabilities, localhealth.OTelGenerationCapability)
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": capabilities})
						return
					}
					if r.URL.Path == "/api/v1/config" {
						_, _ = io.WriteString(w, `{}`)
						return
					}
					t.Errorf("unexpected request: %s", r.URL.Path)
				}))
				t.Cleanup(srv.Close)
				require.NoError(t, local.SaveStatus(local.StateDir(), local.Status{PID: os.Getpid(), Endpoint: srv.URL}))
				t.Cleanup(local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
					t.Fatal("a healthy daemon must not be replaced")
					return nil, nil
				}))
				withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
					t.Fatal("local capture must not request HTTP credentials")
					return login.Result{}, nil
				})
				launched := false
				withStubLauncher(t, agent, func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
					launched = true
					require.NotNil(t, env)
					assert.Equal(t, srv.URL, env.Endpoint)
					assert.Equal(t, srv.URL+"/otlp", env.OTLPEndpoint)
					assert.Contains(t, env.Apply(nil), "AGENTO11Y_CONTENT_CAPTURE_MODE=full")
					return nil
				})
				var stdout, stderr bytes.Buffer
				code := withExit(t, func() { run([]string{agent, "--local"}, bytes.NewReader(nil), &stdout, &stderr) })
				assert.Equal(t, tt.wantLaunch, launched)
				if tt.wantLaunch {
					require.Nil(t, code, "stderr=%s", stderr.String())
				} else {
					require.NotNil(t, code)
					assert.Equal(t, 1, *code)
					assert.Contains(t, stderr.String(), "restart")
					assert.Contains(t, stderr.String(), "updated agento11y binary")
				}
				assert.Positive(t, probes.Load())
			})
		}
	}
}
