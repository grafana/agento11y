package entry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
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
			{name: "API-only config needs default setup", credentials: true, wantLogin: 1},
			{name: "first run", wantLogin: 1},
			{name: "default OTLP without API credentials", collector: "http://127.0.0.1:4318"},
			{name: "whitespace default", protocol: "  ", collector: "http://127.0.0.1:4318"},
			{name: "default OTel config file", collector: "http://127.0.0.1:4318", fromFile: true},
			{name: "explicit HTTP", protocol: "http", credentials: true},
			{name: "HTTP ignores unused collector", protocol: "http", credentials: true, collector: "grpc://127.0.0.1:4317"},
			{name: "HTTP still asks for setup", protocol: "http", wantLogin: 1},
			{name: "grpc compatibility", protocol: "grpc", credentials: true},
			{name: "none compatibility", protocol: "none", credentials: true},
			{name: "legacy HTTP protocol", protocol: "http", legacy: true, credentials: true},
			{name: "OTLP only", protocol: "otel", collector: "http://127.0.0.1:4318"},
			{name: "OTel config file", protocol: "otel", collector: "http://127.0.0.1:4318", fromFile: true},
			{name: "legacy protocol", protocol: "otel", legacy: true, collector: "http://127.0.0.1:4318"},
			{name: "invalid opt-in ignored", protocol: "otel", optIn: "maybe", collector: "http://127.0.0.1:4318"},
			{name: "missing collector offers setup", protocol: "otel", wantLogin: 1},
			{name: "invalid default collector", collector: "grpc://127.0.0.1:4317", wantError: "OTLP base endpoint"},
			{name: "invalid collector", protocol: "otel", collector: "grpc://127.0.0.1:4317", wantError: "OTLP base endpoint"},
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
				withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
					logins++
					wantProtocol := agento11y.GenerationExportProtocolOTel
					if agent == "pi" || agent == "opencode" || tt.protocol == "http" {
						wantProtocol = agento11y.GenerationExportProtocolHTTP
					}
					assert.Equal(t, wantProtocol, opts.GenerationProtocol)
					return login.Result{}, login.ErrNotInteractive
				})
				args := []string{agent, "--tag", "transport=test"}
				if tt.localMode {
					args = append(args, "--local")
				}
				var stdout, stderr bytes.Buffer
				code := withExit(t, func() { run(args, bytes.NewReader(nil), &stdout, &stderr) })
				wantError, wantLogin := tt.wantError, tt.wantLogin
				independentDefault := (agent == "pi" || agent == "opencode") && (tt.protocol == "" || tt.protocol == "  ")
				if independentDefault {
					wantError = ""
					wantLogin = 0
					if !tt.credentials {
						wantLogin = 1
					}
				}
				if tt.protocol == "otel" && (agent == "pi" || agent == "opencode") {
					wantLogin = 0
					wantError = agent + " live capture does not support OTel"
					assert.Contains(t, stderr.String(), "AGENTO11Y_PROTOCOL=http")
					assert.Contains(t, stderr.String(), "history imports support OTel")
				}
				if wantError != "" {
					require.NotNil(t, code)
					assert.Equal(t, 1, *code)
					assert.Contains(t, stderr.String(), wantError)
					assert.Zero(t, launches)
					assert.Empty(t, envconfig.Getenv("TAGS"), "invalid transport must fail before tag side effects")
				} else {
					require.Nil(t, code, "stderr=%s", stderr.String())
					assert.Equal(t, 1, launches)
				}
				assert.Equal(t, wantLogin, logins)
				assert.Zero(t, starts)
				if independentDefault {
					assert.Empty(t, envconfig.Getenv("PROTOCOL"), "HTTP exception must not mutate protocol")
				}
			})
		}
	}
}

func TestRun_LauncherCollectorAliases(t *testing.T) {
	for _, key := range []string{"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_ENDPOINT"} {
		for _, tt := range []struct {
			name, collector string
			wantErr         bool
		}{
			{name: "valid", collector: "http://127.0.0.1:4318"},
			{name: "encoded fragment in path", collector: "https://example.com/otlp%23path"},
			{name: "encoded fragment at path end", collector: "https://example.com/otlp%23"},
			{name: "fragment", collector: "https://example.com#secret", wantErr: true},
			{name: "bare fragment", collector: "https://example.com#", wantErr: true},
			{name: "bare fragment after path", collector: "https://example.com/otlp#", wantErr: true},
			{name: "bare fragment after encoded fragment", collector: "https://example.com/otlp%23path#", wantErr: true},
		} {
			t.Run(key+"/"+tt.name, func(t *testing.T) {
				isolateDotenvHome(t)
				t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
				t.Setenv(key, tt.collector)
				withStubPreflights(t, nil)
				t.Cleanup(local.SetStartDaemonForTesting(func(context.Context, string, *log.Logger) (*local.Status, error) {
					t.Fatal("collector validation must not start a daemon")
					return nil, nil
				}))
				withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
					t.Fatal("supplied collector must be used or rejected, never repaired by setup")
					return login.Result{}, nil
				})
				launches := 0
				withStubLauncher(t, "claude", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
					launches++
					return nil
				})
				var stdout, stderr bytes.Buffer
				code := withExit(t, func() { run([]string{"claude", "--tag", "transport=test"}, bytes.NewReader(nil), &stdout, &stderr) })
				if !tt.wantErr {
					require.Nil(t, code, "stderr=%s", stderr.String())
					assert.Equal(t, 1, launches)
				} else {
					assert.Zero(t, launches)
					assert.Empty(t, envconfig.Getenv("TAGS"), "invalid collector must fail before tag side effects")
					require.NotNil(t, code)
					assert.Equal(t, 1, *code)
					assert.Contains(t, stderr.String(), "OTLP base endpoint")
					assert.NotContains(t, stderr.String(), "secret")
				}
			})
		}
	}
}

func TestRun_LauncherSetupValidatesFinalTarget(t *testing.T) {
	for _, tt := range []struct {
		name, collector string
		setupErr        error
		wantLaunch      bool
	}{
		{name: "setup provides collector", collector: "http://127.0.0.1:4318", wantLaunch: true},
		{name: "successful setup must provide collector"},
		{name: "successful setup cannot provide malformed collector", collector: "https://user:secret@example.com"}, // trufflehog:ignore
		{name: "successful setup cannot provide bare fragment", collector: "https://example.com/otlp#"},
		{name: "setup preserves encoded fragment in path", collector: "https://example.com/otlp%23", wantLaunch: true},
		{name: "cancelled setup keeps host launch", setupErr: login.ErrAborted, wantLaunch: true},
		{name: "noninteractive setup keeps host launch", setupErr: login.ErrNotInteractive, wantLaunch: true},
		{name: "failed setup keeps host launch", setupErr: errors.New("offline"), wantLaunch: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_ENDPOINT", "https://api.example.com")
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant")
			t.Setenv("AGENTO11Y_AUTH_TOKEN", "token")
			withStubPreflights(t, nil)
			logins, launches := 0, 0
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				logins++
				assert.Equal(t, agento11y.GenerationExportProtocolOTel, opts.GenerationProtocol)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", tt.collector)
				return login.Result{}, tt.setupErr
			})
			withStubLauncher(t, "claude", func(context.Context, []string, *local.LaunchEnv, io.Reader, io.Writer, io.Writer, *log.Logger, string) error {
				launches++
				protocol, err := envconfig.SelectGenerationTransportWith(envconfig.LookupEnv)
				require.NoError(t, err)
				assert.Equal(t, agento11y.GenerationExportProtocolOTel, protocol, "no HTTP fallback")
				return nil
			})
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run([]string{"claude"}, bytes.NewReader(nil), &stdout, &stderr) })
			assert.Equal(t, 1, logins)
			if tt.wantLaunch {
				require.Nil(t, code, "stderr=%s", stderr.String())
				assert.Equal(t, 1, launches)
			} else {
				require.NotNil(t, code)
				assert.Equal(t, 1, *code)
				assert.Zero(t, launches)
				assert.Contains(t, stderr.String(), "OTLP base endpoint")
				assert.NotContains(t, stderr.String(), "secret")
			}
		})
	}
}

func TestRun_LauncherSetupLocalAnswerUsesSelectedTransport(t *testing.T) {
	for _, agent := range []string{"claude", "pi", "opencode"} {
		t.Run(agent, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			_, daemonURL := inProcessDaemon(t)
			withStubPreflights(t, nil)
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				protocol := agento11y.GenerationExportProtocolHTTP
				if agent == "claude" {
					protocol = agento11y.GenerationExportProtocolOTel
				}
				assert.Equal(t, protocol, opts.GenerationProtocol)
				return login.Result{LocalMode: true, UsesLocalDaemon: true}, nil
			})
			launched := false
			withStubLauncher(t, agent, func(_ context.Context, _ []string, env *local.LaunchEnv, _ io.Reader, _, _ io.Writer, _ *log.Logger, _ string) error {
				launched = true
				require.NotNil(t, env)
				assert.Equal(t, daemonURL+"/otlp", env.OTLPEndpoint)
				assert.Empty(t, envconfig.Getenv("PROTOCOL"))
				return nil
			})
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run([]string{agent}, bytes.NewReader(nil), &stdout, &stderr) })
			require.Nil(t, code, "stderr=%s", stderr.String())
			assert.True(t, launched)
		})
	}
}

func TestRun_CursorSetupValidatesFinalTarget(t *testing.T) {
	for _, tt := range []struct {
		name, collector string
		setupErr        error
		wantExit        bool
	}{
		{name: "setup supplies default collector", collector: "http://127.0.0.1:4318"},
		{name: "setup leaves collector missing", wantExit: true},
		{name: "setup leaves collector malformed", collector: "grpc://127.0.0.1:4317", wantExit: true},
		{name: "noninteractive preserves installation", setupErr: login.ErrNotInteractive},
		{name: "cancelled preserves installation", setupErr: login.ErrAborted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			installs, logins := 0, 0
			withStubCursorInstall(t, func(io.Writer, io.Writer, *log.Logger) error {
				installs++
				return nil
			})
			withStubLoginRun(t, func(_ context.Context, opts login.RunOpts) (login.Result, error) {
				logins++
				assert.Equal(t, agento11y.GenerationExportProtocolOTel, opts.GenerationProtocol)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", tt.collector)
				return login.Result{}, tt.setupErr
			})
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run([]string{"cursor", "install"}, bytes.NewReader(nil), &stdout, &stderr) })
			assert.Equal(t, 1, installs)
			assert.Equal(t, 1, logins)
			if tt.wantExit {
				require.NotNil(t, code)
				assert.Equal(t, 1, *code)
			} else {
				require.Nil(t, code, "stderr=%s", stderr.String())
			}
			assert.Empty(t, envconfig.Getenv("PROTOCOL"), "setup must not force HTTP")
		})
	}
}

func TestRun_CursorMalformedTransportBeforeInstall(t *testing.T) {
	for _, tt := range []struct{ protocol, collector string }{
		{protocol: "unknown"},
		{collector: "grpc://127.0.0.1:4317"},
		{protocol: "otel", collector: "https://example.com?secret=hidden"},
		{collector: "https://example.com#"},
		{protocol: "otel", collector: "https://example.com/otlp#"},
	} {
		t.Run(tt.protocol+tt.collector, func(t *testing.T) {
			isolateDotenvHome(t)
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
			t.Setenv("AGENTO11Y_PROTOCOL", tt.protocol)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", tt.collector)
			withStubCursorInstall(t, func(io.Writer, io.Writer, *log.Logger) error {
				t.Fatal("malformed transport must not install hooks")
				return nil
			})
			withStubLoginRun(t, func(context.Context, login.RunOpts) (login.Result, error) {
				t.Fatal("malformed transport must not prompt")
				return login.Result{}, nil
			})
			var stdout, stderr bytes.Buffer
			code := withExit(t, func() { run([]string{"cursor", "install"}, bytes.NewReader(nil), &stdout, &stderr) })
			require.NotNil(t, code)
			assert.Equal(t, 1, *code)
			assert.NotContains(t, stderr.String(), "hidden")
		})
	}
}

func TestRun_LocalLauncherRequiresGenerationCapability(t *testing.T) {
	for _, agent := range []string{"claude", "codex", "copilot", "vibe", "kiro", "pi", "opencode"} {
		for _, tt := range []struct {
			name, protocol string
			capable        bool
			wantLaunch     bool
		}{
			{name: "old daemon HTTP unchanged", protocol: "http", wantLaunch: true},
			{name: "old daemon default"},
			{name: "capable default", capable: true, wantLaunch: true},
			{name: "old daemon rejects OTel", protocol: "otel"},
			{name: "capable daemon", protocol: "otel", capable: true, wantLaunch: true},
		} {
			if agent == "pi" || agent == "opencode" {
				if tt.protocol == "otel" {
					continue
				}
				tt.wantLaunch = true
			}
			t.Run(agent+"/"+tt.name, func(t *testing.T) {
				isolateDotenvHome(t)
				t.Setenv("AGENTO11Y_PROTOCOL", tt.protocol)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "")
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
