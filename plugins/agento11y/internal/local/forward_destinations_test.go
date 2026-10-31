package local

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func encodeForwardDestinations(t *testing.T, fields ...forwardDestinationFields) []byte {
	t.Helper()
	data, err := toml.Marshal(struct {
		Destinations []forwardDestinationFields `toml:"destinations"`
	}{Destinations: fields})
	require.NoError(t, err)
	return data
}

func TestParseForwardDestinations(t *testing.T) {
	cases := []struct {
		name       string
		fields     forwardDestinationFields
		genURL     string
		genHeaders map[string]string
		otlpURL    string
		headers    map[string]string
	}{
		{
			name:       "HTTP generation URL and auth",
			fields:     forwardDestinationFields{Name: "cloud", Endpoint: "https://cloud.example.test/", TenantID: "tenant", Token: "generation-token"},
			genURL:     "https://cloud.example.test/api/v1/generations:export",
			genHeaders: map[string]string{"Authorization": "Basic dGVuYW50OmdlbmVyYXRpb24tdG9rZW4=", wire.TenantHeaderName: "tenant"},
		},
		{
			name:   "custom generation path",
			fields: forwardDestinationFields{Name: "cloud", Endpoint: "https://cloud.example.test/ingest", TenantID: "tenant", Token: "token"},
			genURL: "https://cloud.example.test/ingest", genHeaders: generationForwardHeaders("tenant", "token"),
		},
		{
			name:    "anonymous OTLP does not need generation credentials",
			fields:  forwardDestinationFields{Name: "collector", OTLPEndpoint: "https://collector.example.test"},
			otlpURL: "https://collector.example.test", headers: map[string]string{},
		},
		{
			name:   "dedicated OTLP token",
			fields: forwardDestinationFields{Name: "both", Endpoint: "https://cloud.example.test", TenantID: "tenant", Token: "generation-token", OTLPEndpoint: "https://collector.example.test/base", OTLPToken: "otlp-token"},
			genURL: "https://cloud.example.test/api/v1/generations:export", genHeaders: generationForwardHeaders("tenant", "generation-token"),
			otlpURL: "https://collector.example.test/base", headers: map[string]string{"Authorization": "Basic dGVuYW50Om90bHAtdG9rZW4="},
		},
		{
			name:    "same destination token fallback",
			fields:  forwardDestinationFields{Name: "collector", TenantID: "tenant", Token: "token", OTLPEndpoint: "https://collector.example.test"},
			otlpURL: "https://collector.example.test", headers: map[string]string{"Authorization": "Basic dGVuYW50OnRva2Vu"},
		},
		{
			name:    "explicit authorization wins",
			fields:  forwardDestinationFields{Name: "collector", TenantID: "tenant", Token: "unused", OTLPToken: "unused-otlp", OTLPEndpoint: "https://collector.example.test", OTLPHeaders: "authorization=Bearer explicit, X-Scope-OrgID=other, x-api-key=key=with=equals"},
			otlpURL: "https://collector.example.test", headers: map[string]string{"authorization": "Bearer explicit", "X-Scope-OrgID": "other", "x-api-key": "key=with=equals"},
		},
		{
			name:    "explicit non-auth headers retain synthesized auth",
			fields:  forwardDestinationFields{Name: "collector", TenantID: "tenant", OTLPToken: "token", OTLPEndpoint: "https://collector.example.test", OTLPHeaders: "X-Route = test"},
			otlpURL: "https://collector.example.test", headers: map[string]string{"Authorization": "Basic dGVuYW50OnRva2Vu", "X-Route": "test"},
		},
		{
			name:   "local HTTP generations remain allowed",
			fields: forwardDestinationFields{Name: "local", Endpoint: "http://127.0.0.1:4318"},
			genURL: "http://127.0.0.1:4318/api/v1/generations:export", genHeaders: generationForwardHeaders("", ""),
		},
		{
			name:    "TLS loopback retains existing OTLP policy",
			fields:  forwardDestinationFields{Name: "collector", OTLPEndpoint: "https://localhost:4318"},
			otlpURL: "https://localhost:4318", headers: map[string]string{},
		},
		{
			name:   "trim file credentials and endpoints",
			fields: forwardDestinationFields{Name: " cloud ", Endpoint: " https://cloud.example.test ", TenantID: " tenant ", Token: " token "},
			genURL: "https://cloud.example.test/api/v1/generations:export", genHeaders: generationForwardHeaders("tenant", "token"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			destinations, err := parseForwardDestinations(encodeForwardDestinations(t, tc.fields))
			require.NoError(t, err)
			require.Len(t, destinations, 1)
			assert.Equal(t, strings.TrimSpace(tc.fields.Name), destinations[0].name)
			cfg := destinations[0].config
			assert.True(t, cfg.enabled)
			assert.True(t, cfg.strip)
			assert.Equal(t, tc.genURL, cfg.genURL)
			assert.Equal(t, tc.genHeaders, cfg.genHeaders)
			assert.Equal(t, tc.otlpURL, cfg.otlpEndpoint)
			assert.Equal(t, tc.headers, cfg.otlpHeaders)
			assert.Empty(t, cfg.hookURL, "extras never chain hooks")
		})
	}
	for _, data := range []string{"", "# still writing\n", "destinations = []"} {
		destinations, err := parseForwardDestinations([]byte(data))
		require.NoError(t, err)
		assert.Empty(t, destinations)
	}
}

func TestParseForwardDestinationsRejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name   string
		fields forwardDestinationFields
		want   string
	}{
		{"missing name", forwardDestinationFields{OTLPEndpoint: "https://collector.test"}, "name must be non-empty"},
		{"blank name", forwardDestinationFields{Name: "  ", OTLPEndpoint: "https://collector.test"}, "name must be non-empty"},
		{"reserved name", forwardDestinationFields{Name: " primary ", OTLPEndpoint: "https://collector.test"}, "must not be primary"},
		{"control in name", forwardDestinationFields{Name: "secret\nname", OTLPEndpoint: "https://collector.test"}, "control characters"},
		{"missing endpoint", forwardDestinationFields{Name: "extra"}, "requires endpoint or otlp_endpoint"},
		{"generation auth missing", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test"}, "requires non-empty"},
		{"generation token missing", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test", TenantID: "tenant"}, "requires non-empty"},
		{"generation tenant missing", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test", Token: "secret-token"}, "requires non-empty"},
		{"placeholder tenant", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test", TenantID: "local", Token: "secret-token"}, "requires non-empty"},
		{"placeholder token", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test", TenantID: "tenant", Token: "local"}, "requires non-empty"},
		{"no OTLP token fallback to generations", forwardDestinationFields{Name: "extra", Endpoint: "https://cloud.test", TenantID: "tenant", OTLPToken: "secret-otlp-token"}, "requires non-empty"},
		{"tenant injection", forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test", TenantID: "tenant\r\nsecret-header"}, "control characters"},
		{"token injection", forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test", Token: "secret-token\n"}, "control characters"},
		{"OTLP token injection", forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test", OTLPToken: "secret-token\x00"}, "control characters"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			destinations, err := parseForwardDestinations(encodeForwardDestinations(t, tc.fields))
			require.ErrorContains(t, err, tc.want)
			assert.Nil(t, destinations)
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestParseForwardDestinationsRejectsHostileEndpoints(t *testing.T) {
	endpoints := []string{
		"collector.test", "//collector.test", "/relative", "ftp://collector.test", "http:", "https:///path",
		"https://:443/path", "https://secret-user:secret-password@collector.test",
		"https://collector.test?token=secret", "https://collector.test?", "https://collector.test/#secret", "https://collector.test/#",
		"https://collector.test/%zz-secret", "https://collector.test:secret-port", "https://collector.test\n",
	}
	for _, endpoint := range endpoints {
		for _, field := range []string{"endpoint", "otlp_endpoint"} {
			t.Run(field+"/"+endpoint, func(t *testing.T) {
				fields := forwardDestinationFields{Name: "extra", TenantID: "tenant", Token: "secret-token"}
				if field == "endpoint" {
					fields.Endpoint = endpoint
				} else {
					fields.OTLPEndpoint = endpoint
				}
				destinations, err := parseForwardDestinations(encodeForwardDestinations(t, fields))
				require.ErrorContains(t, err, "explicit HTTP(S) URLs")
				assert.Nil(t, destinations)
				assert.NotContains(t, err.Error(), "secret")
			})
		}
	}
	for _, host := range []string{"localhost", "LOCALHOST", "127.0.0.1", "[::1]", "[0:0:0:0:0:0:0:1]", "[::ffff:127.0.0.1]"} {
		t.Run("OTLP loopback/"+host, func(t *testing.T) {
			_, err := parseForwardDestinations(encodeForwardDestinations(t, forwardDestinationFields{Name: "extra", OTLPEndpoint: "http://" + host + ":4318"}))
			require.ErrorContains(t, err, "must not use HTTP with localhost, 127.0.0.1, or ::1")
		})
	}
}

func TestParseForwardDestinationsRejectsHostileHeaders(t *testing.T) {
	headers := []string{
		"missing-equals", "=secret", "Authorization=", "Authorization=secret,", ",Authorization=secret",
		"bad name=secret", "bad:name=secret", "X-Key=secret\r\nAuthorization=oops", "X-Key=secret\x00", "X-Key=secret\x7f",
		"Authorization=secret,authorization=other", "X-Key=secret,x-key=other",
	}
	for _, reserved := range []string{ForwardMarkerHeader, "Content-Type", "Content-Encoding", "Host", "Content-Length", "Length"} {
		headers = append(headers, reserved+"=secret", strings.ToUpper(reserved)+"=secret", strings.ToLower(reserved)+"=secret")
	}
	for _, header := range headers {
		t.Run(header, func(t *testing.T) {
			_, err := parseForwardDestinations(encodeForwardDestinations(t, forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test", OTLPHeaders: header}))
			require.ErrorContains(t, err, "otlp_headers")
			assert.NotContains(t, err.Error(), "secret")
		})
	}
}

func TestParseForwardDestinationsStrictTOML(t *testing.T) {
	cases := []string{
		`secret_unknown = "secret-token"`,
		"[[destinations]]\nname = \"extra\"\nsecret_unknown = \"secret-token\"",
		"[[destinations]]\nname = \"extra\"\nendpoint = 123 # secret-token",
		"[[destinations]]\nname = \"extra\"\notlp_headers = { Authorization = \"secret-token\" }",
		"[[destinations]]\nname = \"extra\"\nname = \"secret-token\"",
		"[[destinations]]\nname = \"secret-token",
		"[destinations]\nname = \"secret-token\"",
		"destinations = [\"secret-token\"]",
		"[[destinations]]\nname = \"extra\"\n[destinations.secret_unknown]\ntoken = \"secret-token\"",
		"[[destinations]]\nname = \"extra\"\nenabled = true # secret-token",
	}
	for i, data := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			destinations, err := parseForwardDestinations([]byte(data))
			require.Error(t, err)
			assert.Nil(t, destinations)
			assert.NotContains(t, err.Error(), "secret")
			assert.NotContains(t, err.Error(), data)
		})
	}
}

func TestParseForwardDestinationsLimitsAndNames(t *testing.T) {
	for _, count := range []int{1, 3, 4} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var fields []forwardDestinationFields
			for i := range count {
				fields = append(fields, forwardDestinationFields{Name: fmt.Sprintf("extra-%d", i), OTLPEndpoint: fmt.Sprintf("https://collector-%d.test", i)})
			}
			destinations, err := parseForwardDestinations(encodeForwardDestinations(t, fields...))
			if count > 3 {
				require.ErrorContains(t, err, "at most three")
				assert.Nil(t, destinations)
			} else {
				require.NoError(t, err)
				assert.Len(t, destinations, count)
			}
		})
	}
	_, err := parseForwardDestinations(encodeForwardDestinations(t,
		forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://one.test"},
		forwardDestinationFields{Name: " extra ", OTLPEndpoint: "https://two.test"},
	))
	require.ErrorContains(t, err, "names must be unique")
}

func TestParseForwardDestinationsAuthIsolation(t *testing.T) {
	for _, key := range []string{
		"AGENTO11Y_AUTH_TENANT_ID", "SIGIL_AUTH_TENANT_ID", "AGENTO11Y_AUTH_TOKEN", "SIGIL_AUTH_TOKEN",
		"AGENTO11Y_OTEL_AUTH_TOKEN", "SIGIL_OTEL_AUTH_TOKEN", "OTEL_EXPORTER_OTLP_HEADERS",
		"AGENTO11Y_ENDPOINT", "SIGIL_ENDPOINT", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_INSECURE", "AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE",
		"SIGIL_OTEL_EXPORTER_OTLP_INSECURE", "AGENTO11Y_PROTOCOL", "AGENTO11Y_CONTENT_CAPTURE_MODE",
	} {
		t.Setenv(key, "ambient-secret")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
	cases := []forwardDestinationFields{
		{Name: "anonymous", OTLPEndpoint: "https://collector.test"},
		{Name: "tenant-only", TenantID: "file-tenant", OTLPEndpoint: "https://collector.test"},
		{Name: "token-only", OTLPToken: "file-secret", OTLPEndpoint: "https://collector.test"},
	}
	for _, fields := range cases {
		t.Run(fields.Name, func(t *testing.T) {
			destinations, err := parseForwardDestinations(encodeForwardDestinations(t,
				forwardDestinationFields{Name: "with-auth", TenantID: "other-tenant", Token: "other-secret", OTLPEndpoint: "https://other.test"},
				fields,
			))
			require.NoError(t, err)
			cfg := destinations[1].config
			assert.Equal(t, "https://collector.test", cfg.otlpEndpoint)
			assert.Empty(t, cfg.otlpHeaders)
			assert.Empty(t, cfg.genURL)
		})
	}
	_, err := parseForwardDestinations(encodeForwardDestinations(t, forwardDestinationFields{Name: "no-file-auth", Endpoint: "https://cloud.test"}))
	require.ErrorContains(t, err, "requires non-empty")
	_, err = parseForwardDestinations(encodeForwardDestinations(t, forwardDestinationFields{Name: "no-file-endpoint"}))
	require.ErrorContains(t, err, "requires endpoint")
}

func TestParseForwardDestinationsDuplicateRoutes(t *testing.T) {
	cases := []struct {
		name      string
		first     forwardDestinationFields
		second    forwardDestinationFields
		duplicate bool
	}{
		{"HTTP normalized root", forwardDestinationFields{Endpoint: "https://cloud.test", TenantID: "tenant", Token: "secret"}, forwardDestinationFields{Endpoint: "https://CLOUD.test:443/api/v1/generations:export", TenantID: "tenant", Token: "secret"}, true},
		{"HTTP different tenants", forwardDestinationFields{Endpoint: "https://cloud.test", TenantID: "one", Token: "secret"}, forwardDestinationFields{Endpoint: "https://cloud.test", TenantID: "two", Token: "secret"}, false},
		{"HTTP different tokens", forwardDestinationFields{Endpoint: "https://cloud.test", TenantID: "tenant", Token: "secret"}, forwardDestinationFields{Endpoint: "https://cloud.test", TenantID: "tenant", Token: "different"}, false},
		{"OTLP signal suffixes", forwardDestinationFields{OTLPEndpoint: "https://collector.test/v1/traces"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test/v1/metrics/"}, true},
		{"OTLP different base paths", forwardDestinationFields{OTLPEndpoint: "https://collector.test/one"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test/two"}, false},
		{"OTLP header order and case", forwardDestinationFields{OTLPEndpoint: "https://collector.test", OTLPHeaders: "Authorization=Bearer secret,X-Scope-OrgID=tenant"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test/", OTLPHeaders: "x-scope-orgid=tenant,authorization=Bearer secret"}, true},
		{"OTLP different explicit tenants", forwardDestinationFields{OTLPEndpoint: "https://collector.test", OTLPHeaders: "Authorization=Bearer secret,X-Scope-OrgID=one"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test", OTLPHeaders: "Authorization=Bearer secret,X-Scope-OrgID=two"}, false},
		{"OTLP different basic tenants", forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "one", OTLPToken: "secret"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "two", OTLPToken: "secret"}, false},
		{"OTLP explicit matches synthesized auth", forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "tenant", OTLPToken: "token"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test", OTLPHeaders: "authorization=Basic dGVuYW50OnRva2Vu"}, true},
		{"OTLP shadowed credentials are ignored", forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "one", OTLPToken: "secret", OTLPHeaders: "Authorization=Bearer same"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "two", OTLPToken: "different", OTLPHeaders: "Authorization=Bearer same"}, true},
		{"distinct signals may share URL", forwardDestinationFields{Endpoint: "https://collector.test/v1/traces", TenantID: "tenant", Token: "secret"}, forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "tenant", Token: "secret"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.first.Name, tc.second.Name = "one", "two"
			destinations, err := parseForwardDestinations(encodeForwardDestinations(t, tc.first, tc.second))
			if tc.duplicate {
				require.ErrorContains(t, err, "same effective headers")
				assert.Nil(t, destinations)
				assert.NotContains(t, err.Error(), "secret")
			} else {
				require.NoError(t, err)
				assert.Len(t, destinations, 2)
			}

			clearForwardEnv(t)
			primary, err := resolveExtraForwardDestination(tc.first)
			require.NoError(t, err)
			routes := ForwardPrimaryRoutes{
				GenerationURL: primary.genURL, OTLPEndpoint: primary.otlpEndpoint,
				TenantID: tc.first.TenantID, Token: tc.first.Token,
				OTLPToken: tc.first.OTLPToken, OTLPHeaders: tc.first.OTLPHeaders,
			}
			for _, enabled := range []string{"true", "false"} {
				t.Run("primary forwarding "+enabled, func(t *testing.T) {
					configPath := writeConfigEnvFile(t, map[string]string{
						"AGENTO11Y_LOCAL_FORWARD":               enabled,
						"AGENTO11Y_ENDPOINT":                    tc.first.Endpoint,
						"AGENTO11Y_AUTH_TENANT_ID":              tc.first.TenantID,
						"AGENTO11Y_AUTH_TOKEN":                  tc.first.Token,
						"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": tc.first.OTLPEndpoint,
						"AGENTO11Y_OTEL_AUTH_TOKEN":             tc.first.OTLPToken,
						"OTEL_EXPORTER_OTLP_HEADERS":            tc.first.OTLPHeaders,
					})
					data := encodeForwardDestinations(t, tc.second, forwardDestinationFields{Name: "independent", OTLPEndpoint: "https://independent.test"})
					path := filepath.Join(filepath.Dir(configPath), forwardingFile)
					require.NoError(t, os.WriteFile(path, data, 0o600))
					inspection := InspectForwardingWithPrimary(configPath, routes)
					loaded := newForwardLoader(configPath, nil).load()
					assert.Equal(t, path, inspection.Path)
					if tc.duplicate {
						assert.Equal(t, "forwarding.toml duplicates a primary signal route; all extra destinations are disabled", inspection.Error)
						assert.Empty(t, inspection.Destinations)
						assert.NotEmpty(t, loaded.forwardingError)
						require.Len(t, loaded.destinations, 1)
					} else {
						assert.Empty(t, inspection.Error)
						assert.Len(t, inspection.Destinations, 2)
						assert.Empty(t, loaded.forwardingError)
						require.Len(t, loaded.destinations, 3)
					}
					assert.Equal(t, "primary", loaded.destinations[0].name)
					encoded, err := json.Marshal(inspection)
					require.NoError(t, err)
					for _, secret := range []string{"secret", "Authorization", "Basic ", "Bearer "} {
						assert.NotContains(t, string(encoded), secret)
					}
					for _, headers := range []map[string]string{primary.genHeaders, primary.otlpHeaders} {
						for key, value := range headers {
							if strings.EqualFold(key, "Authorization") && value != "" {
								assert.NotContains(t, string(encoded), value)
							}
						}
					}
					unchanged, err := os.ReadFile(path)
					require.NoError(t, err)
					assert.Equal(t, data, unchanged)
				})
			}
		})
	}
}

func TestValidateForwardDestinationRoutesIncludesPrimary(t *testing.T) {
	cfg, err := resolveExtraForwardDestination(forwardDestinationFields{OTLPEndpoint: "https://collector.test", TenantID: "tenant", OTLPToken: "token"})
	require.NoError(t, err)
	err = validateForwardDestinationRoutes([]forwardDestination{{name: "primary", config: cfg}, {name: "extra", config: cfg}})
	require.ErrorContains(t, err, "same effective headers")
}

func TestInspectForwarding(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"empty", "# no extras", ""},
		{"valid", string(encodeForwardDestinations(t, forwardDestinationFields{Name: "cloud", Endpoint: "https://cloud.test", TenantID: "private-tenant", Token: "private-token", OTLPEndpoint: "https://collector.test", OTLPToken: "private-otlp-token", OTLPHeaders: "X-Api-Key=private-header"})), ""},
		{"syntax source secret", "[[destinations]]\ntoken = \"private-token", "forwarding.toml is invalid"},
		{"unknown field secret", "private_field = \"private-token\"", "forwarding.toml is invalid"},
		{"userinfo secret", string(encodeForwardDestinations(t, forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://private-user:private-token@collector.test"})), "forwarding.toml is invalid"},
		{"query secret", string(encodeForwardDestinations(t, forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test?token=private-token"})), "forwarding.toml is invalid"},
		{"validation secret", string(encodeForwardDestinations(t, forwardDestinationFields{Name: "extra", OTLPEndpoint: "https://collector.test", OTLPHeaders: "Content-Type=private-token"})), "forwarding.toml is invalid"},
		{"partial valid file is not exposed", string(encodeForwardDestinations(t, forwardDestinationFields{Name: "good", OTLPEndpoint: "https://collector.test"}, forwardDestinationFields{Name: "bad", Token: "private-token"})), "forwarding.toml is invalid"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.env")
			path := filepath.Join(filepath.Dir(configPath), "forwarding.toml")
			require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			inspection := InspectForwarding(configPath)
			assert.Equal(t, path, inspection.Path)
			assert.Equal(t, tc.want, inspection.Error)
			if tc.name == "valid" {
				assert.Equal(t, []ForwardDestinationInfo{{Name: "cloud", Endpoint: "https://cloud.test/api/v1/generations:export", OTLPEndpoint: "https://collector.test", HTTPGenerations: true, OTLP: true}}, inspection.Destinations)
			} else {
				assert.Empty(t, inspection.Destinations)
			}
			data, err := json.Marshal(inspection)
			require.NoError(t, err)
			assert.NotContains(t, string(data), "private-")
			basic, _ := otel.BasicAuthHeaderValue("private-tenant", "private-token")
			assert.NotContains(t, string(data), basic)
			assert.NotContains(t, string(data), "Authorization")
		})
	}
}

func TestInspectForwardingPathsAndReadErrors(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.env")
	missing := InspectForwarding(configPath)
	assert.Equal(t, filepath.Join(filepath.Dir(configPath), "forwarding.toml"), missing.Path)
	assert.Empty(t, missing.Error)
	assert.Empty(t, missing.Destinations)

	require.NoError(t, os.Mkdir(missing.Path, 0o700))
	unreadable := InspectForwarding(configPath)
	assert.Equal(t, "forwarding.toml could not be read", unreadable.Error)
	assert.NotContains(t, unreadable.Error, filepath.Dir(configPath))
	assert.Empty(t, unreadable.Destinations)

	empty := InspectForwarding("")
	assert.Empty(t, empty.Path)
	assert.Empty(t, empty.Error)
	assert.Empty(t, empty.Destinations)
	data, err := json.Marshal(empty)
	require.NoError(t, err)
	assert.JSONEq(t, `{"path":"","destinations":[]}`, string(data))
}

func TestForwardDestinationInfoJSON(t *testing.T) {
	data, err := json.Marshal(ForwardDestinationInfo{Name: "extra", Endpoint: "https://cloud.test", OTLPEndpoint: "https://collector.test", HTTPGenerations: true, OTLP: true})
	require.NoError(t, err)
	assert.JSONEq(t, `{"name":"extra","endpoint":"https://cloud.test","otlpEndpoint":"https://collector.test","httpGenerations":true,"otlp":true}`, string(data))
}
