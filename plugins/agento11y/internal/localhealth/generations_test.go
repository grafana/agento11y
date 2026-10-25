package localhealth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsDaemonEndpoint(t *testing.T) {
	for _, tc := range []struct {
		endpoint string
		want     bool
	}{
		{"http://127.0.0.1:8765/otlp", true},
		{"http://LOCALHOST:8765/otlp/", true},
		{"http://[::1]:8765/otlp/v1/traces", true},
		{"http://127.0.0.1:8765/otlp/v1/metrics", true},
		{"http://127.0.0.1:4318", false},
		{"http://localhost:4318/v1/traces", false},
		{"http://127.0.0.1:4318/custom/otlp", false},
		{"http://localhost.attacker.test:8765/otlp", false},
		{"https://127.0.0.1:8765/otlp", false},
		{"http://collector.example/otlp", false},
	} {
		t.Run(tc.endpoint, func(t *testing.T) {
			if got := IsDaemonEndpoint(tc.endpoint); got != tc.want {
				t.Fatalf("IsDaemonEndpoint = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRequireGenerationIngest(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  string
	}{
		{name: "supported", body: `{"status":"ok","capabilities":["otel_generation_ingest_v1"]}`, status: 200},
		{name: "old daemon", body: `{"status":"ok"}`, status: 200, wantError: "restart"},
		{name: "other capability", body: `{"capabilities":["other"]}`, status: 200, wantError: "restart"},
		{name: "invalid health", body: `invalid`, status: 200, wantError: "decode"},
		{name: "unhealthy", body: `{}`, status: 503, wantError: "HTTP 503"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" {
					t.Errorf("path=%q", r.URL.Path)
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("sent authorization")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			err := RequireGenerationIngest(t.Context(), srv.URL+"/v1/traces")
			if tc.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error=%v, want %q", err, tc.wantError)
			}
		})
	}
	if err := RequireGenerationIngest(t.Context(), "https://collector.example/otlp"); err != nil {
		t.Fatal(err)
	}
}
