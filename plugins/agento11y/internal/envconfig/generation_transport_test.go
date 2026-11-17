package envconfig

import (
	"strings"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestResolveGenerationTransport(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		want    agento11y.GenerationExportProtocol
		wantErr bool
	}{
		{name: "default requires endpoint", wantErr: true},
		{name: "default OTel", env: map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "blank protocol", env: map[string]string{"AGENTO11Y_PROTOCOL": "  ", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "legacy HTTP", env: map[string]string{"SIGIL_PROTOCOL": " HTTP "}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "preferred wins", env: map[string]string{"AGENTO11Y_PROTOCOL": "http", "SIGIL_PROTOCOL": "otel"}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "unknown does not fall back", env: map[string]string{"AGENTO11Y_PROTOCOL": "typo", "SIGIL_PROTOCOL": "http"}, wantErr: true},
		{name: "grpc falls back to HTTP without opt-in", env: map[string]string{"AGENTO11Y_PROTOCOL": "grpc"}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "none falls back to HTTP without opt-in", env: map[string]string{"AGENTO11Y_PROTOCOL": "none"}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "legacy grpc falls back to HTTP", env: map[string]string{"SIGIL_PROTOCOL": " GRPC "}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "legacy none falls back to HTTP", env: map[string]string{"SIGIL_PROTOCOL": " NoNe "}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "preferred grpc overrides legacy OTel", env: map[string]string{"AGENTO11Y_PROTOCOL": "grpc", "SIGIL_PROTOCOL": "otel"}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "preferred none ignores OTel settings", env: map[string]string{"AGENTO11Y_PROTOCOL": "none", "SIGIL_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "false", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "invalid"}, want: agento11y.GenerationExportProtocolHTTP},
		{name: "unknown legacy protocol", env: map[string]string{"SIGIL_PROTOCOL": "unsupported"}, wantErr: true},
		{name: "preferred OTel overrides legacy grpc", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "SIGIL_PROTOCOL": "grpc"}, wantErr: true},
		{name: "explicit OTel without experimental opt-in", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "experimental disabled does not disable OTel", env: map[string]string{"SIGIL_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "false", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "missing OTLP endpoint", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "AGENTO11Y_ENDPOINT": "https://generation.example"}, wantErr: true},
		{name: "signal endpoint is not base config", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "https://traces.example"}, wantErr: true},
		{name: "legacy protocol and endpoint", env: map[string]string{"SIGIL_PROTOCOL": " OtEl ", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": " on ", "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://legacy.example/otlp"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "standard endpoint", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "OTEL_EXPORTER_OTLP_ENDPOINT": "http://localhost:4318"}, want: agento11y.GenerationExportProtocolOTel},
		{name: "invalid preferred endpoint never falls back", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "bad", "SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT": "https://legacy.example", "OTEL_EXPORTER_OTLP_ENDPOINT": "https://standard.example"}, wantErr: true},
		{name: "full with metadata spans is accepted", env: map[string]string{"AGENTO11Y_PROTOCOL": "otel", "AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES": "true", "AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT": "https://collector.example/otlp", "AGENTO11Y_CONTENT_CAPTURE_MODE": "full_with_metadata_spans"}, want: agento11y.GenerationExportProtocolOTel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			PinAliasEnvBlank(t)
			for _, k := range []string{agento11y.EnvEnableExperimentalFeatures, "SIGIL_ENABLE_EXPERIMENTAL_FEATURES", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"} {
				t.Setenv(k, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			for _, resolve := range []func() (agento11y.GenerationExportProtocol, error){
				ResolveGenerationTransport,
				func() (agento11y.GenerationExportProtocol, error) {
					return ResolveGenerationTransportWith(mapLookup(tc.env), func(k string) string { return tc.env[k] })
				},
			} {
				got, err := resolve()
				if got != tc.want || (err != nil) != tc.wantErr {
					t.Fatalf("got (%q, %v), want (%q, error=%v)", got, err, tc.want, tc.wantErr)
				}
			}
		})
	}
}

func TestSelectGenerationTransport(t *testing.T) {
	for _, tc := range []struct {
		protocol string
		want     agento11y.GenerationExportProtocol
		wantErr  bool
	}{
		{want: agento11y.GenerationExportProtocolOTel},
		{protocol: "  ", want: agento11y.GenerationExportProtocolOTel},
		{protocol: " OtEl ", want: agento11y.GenerationExportProtocolOTel},
		{protocol: "http", want: agento11y.GenerationExportProtocolHTTP},
		{protocol: "grpc", want: agento11y.GenerationExportProtocolHTTP},
		{protocol: "none", want: agento11y.GenerationExportProtocolHTTP},
		{protocol: "invalid", wantErr: true},
	} {
		t.Run(tc.protocol, func(t *testing.T) {
			got, err := SelectGenerationTransportWith(mapLookup(map[string]string{"AGENTO11Y_PROTOCOL": tc.protocol}))
			if got != tc.want || (err != nil) != tc.wantErr {
				t.Fatalf("got (%q, %v), want (%q, error=%v)", got, err, tc.want, tc.wantErr)
			}
		})
	}
}

func TestGenerationTransportSnapshotDoesNotReadAmbientEndpoint(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "https://ambient.example")
	_, err := ResolveGenerationTransportWith(mapLookup(nil), func(string) string { return "" })
	if err == nil {
		t.Fatal("snapshot read ambient endpoint")
	}
}

func TestValidateGenerationOTLPEndpoint(t *testing.T) {
	for _, endpoint := range []string{"", " ", "collector:4318", "//collector:4318", "ftp://collector", "https://", "http://:4318", "https://collector:abc", "https://collector:65536", "https://collector:0", "https://user:secret@collector", "https://collector/?secret=x", "https://collector/#secret", "https://collector#", "https://collector/otlp#", "https://collector/otlp%23path#", "https://collector/%zz"} {
		t.Run(endpoint, func(t *testing.T) {
			err := validateGenerationOTLPEndpoint(endpoint)
			if err == nil {
				t.Fatal("accepted invalid endpoint")
			}
			if strings.Contains(err.Error(), "secret") {
				t.Fatal("error leaks endpoint secret")
			}
		})
	}
	for _, endpoint := range []string{"http://localhost:4318", "https://collector.example/otlp/", "http://[::1]:4318/otlp", "https://collector.example/otlp%23path", "https://collector.example/otlp%23", " https://collector.example/v1/traces "} {
		if err := validateGenerationOTLPEndpoint(endpoint); err != nil {
			t.Fatalf("%q: %v", endpoint, err)
		}
	}
}
