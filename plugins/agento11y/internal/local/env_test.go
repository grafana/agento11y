package local

import (
	"os"
	"strings"
	"testing"
)

func envMap(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

func TestLaunchEnvApply(t *testing.T) {
	launch := LaunchEnv{Endpoint: "http://127.0.0.1:4319", OTLPEndpoint: "http://127.0.0.1:4320/otlp"}

	cases := []struct {
		name string
		env  []string
		want map[string]string
	}{
		{
			name: "overrides inherited preferred and legacy endpoints",
			env: []string{
				"AGENTO11Y_ENDPOINT=https://cloud.example",
				"SIGIL_ENDPOINT=https://cloud.example",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp.example",
				"SIGIL_CONTENT_CAPTURE_MODE=metadata_only",
			},
			want: map[string]string{
				"AGENTO11Y_ENDPOINT":                    "http://127.0.0.1:4319",
				"SIGIL_ENDPOINT":                        "http://127.0.0.1:4319",
				"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4320/otlp",
				"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":     "http://127.0.0.1:4320/otlp",
				"AGENTO11Y_CONTENT_CAPTURE_MODE":        "full",
				"SIGIL_CONTENT_CAPTURE_MODE":            "full",
				"AGENTO11Y_AUTH_TENANT_ID":              "local",
				"SIGIL_AUTH_TENANT_ID":                  "local",
				"AGENTO11Y_AUTH_TOKEN":                  "local",
				"SIGIL_AUTH_TOKEN":                      "local",
			},
		},
		{
			name: "keeps configured credentials under either spelling",
			env: []string{
				"SIGIL_AUTH_TENANT_ID=42",
				"SIGIL_AUTH_TOKEN=glc_cloud",
			},
			want: map[string]string{
				"SIGIL_AUTH_TENANT_ID": "42",
				"SIGIL_AUTH_TOKEN":     "glc_cloud",
				"AGENTO11Y_ENDPOINT":   "http://127.0.0.1:4319",
				"SIGIL_ENDPOINT":       "http://127.0.0.1:4319",
			},
		},
		{
			name: "preferred-only credentials block placeholders for the family",
			env: []string{
				"AGENTO11Y_AUTH_TENANT_ID=42",
				"AGENTO11Y_AUTH_TOKEN=glc_cloud",
			},
			want: map[string]string{
				"AGENTO11Y_AUTH_TENANT_ID": "42",
				"AGENTO11Y_AUTH_TOKEN":     "glc_cloud",
			},
		},
		{
			name: "pins http export over an inherited otel protocol",
			env: []string{
				"AGENTO11Y_PROTOCOL=otel",
				"SIGIL_PROTOCOL=otel",
			},
			want: map[string]string{
				"AGENTO11Y_PROTOCOL": "http",
				"SIGIL_PROTOCOL":     "http",
			},
		},
		{
			name: "blank credentials get placeholders under both spellings",
			env:  []string{"SIGIL_AUTH_TENANT_ID=  ", "SIGIL_AUTH_TOKEN="},
			want: map[string]string{
				"AGENTO11Y_AUTH_TENANT_ID": "local",
				"SIGIL_AUTH_TENANT_ID":     "local",
				"AGENTO11Y_AUTH_TOKEN":     "local",
				"SIGIL_AUTH_TOKEN":         "local",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := envMap(launch.Apply(tc.env))
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("%s = %q, want %q", k, got[k], want)
				}
			}
		})
	}
}

func TestLaunchEnvApplyOS(t *testing.T) {
	t.Setenv("AGENTO11Y_ENDPOINT", "https://cloud.example")
	t.Setenv("SIGIL_ENDPOINT", "https://cloud.example")
	t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example")
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "https://otlp.example")
	t.Setenv("AGENTO11Y_CONTENT_CAPTURE_MODE", "metadata_only")
	t.Setenv("SIGIL_CONTENT_CAPTURE_MODE", "metadata_only")
	t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "")
	t.Setenv("SIGIL_AUTH_TENANT_ID", "")
	t.Setenv("AGENTO11Y_AUTH_TOKEN", "")
	t.Setenv("SIGIL_AUTH_TOKEN", "")

	LaunchEnv{Endpoint: "http://127.0.0.1:4319", OTLPEndpoint: "http://127.0.0.1:4320/otlp"}.ApplyOS()

	want := map[string]string{
		"AGENTO11Y_ENDPOINT":                    "http://127.0.0.1:4319",
		"SIGIL_ENDPOINT":                        "http://127.0.0.1:4319",
		"AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "http://127.0.0.1:4320/otlp",
		"SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT":     "http://127.0.0.1:4320/otlp",
		"AGENTO11Y_CONTENT_CAPTURE_MODE":        "full",
		"SIGIL_CONTENT_CAPTURE_MODE":            "full",
		"AGENTO11Y_AUTH_TENANT_ID":              "local",
		"SIGIL_AUTH_TENANT_ID":                  "local",
		"AGENTO11Y_AUTH_TOKEN":                  "local",
		"SIGIL_AUTH_TOKEN":                      "local",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestLaunchEnvApplyPlaceholderNotDuplicatedForKeptFamily(t *testing.T) {
	launch := LaunchEnv{Endpoint: "http://127.0.0.1:4319", OTLPEndpoint: "http://127.0.0.1:4320"}
	out := launch.Apply([]string{"SIGIL_AUTH_TENANT_ID=42"})
	got := envMap(out)
	if got["AGENTO11Y_AUTH_TENANT_ID"] != "" {
		t.Errorf("AGENTO11Y_AUTH_TENANT_ID = %q, want unset (family kept via legacy spelling)", got["AGENTO11Y_AUTH_TENANT_ID"])
	}
	if got["SIGIL_AUTH_TENANT_ID"] != "42" {
		t.Errorf("SIGIL_AUTH_TENANT_ID = %q, want 42", got["SIGIL_AUTH_TENANT_ID"])
	}
}
