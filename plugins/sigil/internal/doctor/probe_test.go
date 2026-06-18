package doctor

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/proto/sigil/wire"
)

func TestProbeConversations(t *testing.T) {
	tests := []struct {
		name        string
		status      int
		badURL      bool // probe an unparseable endpoint instead of the test server
		wantOK      bool
		wantAuthMsg bool
		wantErr     bool // transport error: status 0 with a message, server never hit
	}{
		{name: "200 ok", status: 200, wantOK: true},
		{name: "401 unauthorized", status: 401, wantAuthMsg: true},
		{name: "403 forbidden", status: 403, wantAuthMsg: true},
		{name: "400 reachable", status: 400, wantOK: false},
		{name: "invalid endpoint", badURL: true, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotTenant, gotAuth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotTenant = r.Header.Get("X-Scope-OrgID")
				gotAuth = r.Header.Get("Authorization")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			url := srv.URL
			if tc.badURL {
				url = "://bad"
			}
			res := defaultProbeConversations(context.Background(), url, "tenant-1", "glc_tok", false)

			if tc.wantErr {
				if res.StatusCode != 0 || res.Message == "" {
					t.Fatalf("expected a transport error result, got %+v", res)
				}
				return
			}
			if res.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.status)
			}
			if res.OK != tc.wantOK {
				t.Fatalf("ok = %v, want %v", res.OK, tc.wantOK)
			}
			if tc.wantAuthMsg && !strings.Contains(res.Message, "sigil:write") {
				t.Fatalf("expected auth message, got %q", res.Message)
			}
			if gotPath != wire.GenerationExportHTTPPath {
				t.Fatalf("probe hit %q, want %q", gotPath, wire.GenerationExportHTTPPath)
			}
			wantAuth := "Basic " + base64.StdEncoding.EncodeToString([]byte("tenant-1:glc_tok"))
			if gotTenant != "tenant-1" || gotAuth != wantAuth {
				t.Fatalf("missing auth headers: tenant=%q auth=%q want auth=%q", gotTenant, gotAuth, wantAuth)
			}
		})
	}
}

func TestProbeConversations_Timeout(t *testing.T) {
	prev := probeTimeout
	t.Cleanup(func() { probeTimeout = prev })
	probeTimeout = 50 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	res := defaultProbeConversations(context.Background(), srv.URL, "t", "tok", false)
	if res.StatusCode != 0 || res.Message == "" {
		t.Fatalf("expected timeout error, got %+v", res)
	}
}

// A scheme-less endpoint with SIGIL_INSECURE resolves to http, matching the
// SDK exporter; without it the probe would hit https and miss a cleartext
// collector that real export reaches.
func TestProbeConversations_InsecureScheme(t *testing.T) {
	secure, err := wire.NormalizeGenerationExportURL("collector.local:4317", false)
	if err != nil {
		t.Fatalf("normalize secure: %v", err)
	}
	if !strings.HasPrefix(secure, "https://") {
		t.Fatalf("secure target = %q, want https scheme", secure)
	}

	var gotScheme string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotScheme = "http"
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// srv.Listener.Addr() is a scheme-less host:port; with insecure the probe
	// must reach it over http.
	res := defaultProbeConversations(context.Background(), srv.Listener.Addr().String(), "t", "tok", true)
	if !res.OK || gotScheme != "http" {
		t.Fatalf("insecure probe = %+v, server scheme %q, want http reach", res, gotScheme)
	}
}

func TestProbeOTLP(t *testing.T) {
	var hitMetrics, hitTraces bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/metrics":
			hitMetrics = true
		case "/v1/traces":
			hitTraces = true
		}
		if r.Header.Get("Authorization") == "" {
			t.Errorf("OTLP probe sent no auth header")
		}
		w.WriteHeader(403)
	}))
	defer srv.Close()

	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("SIGIL_AUTH_TENANT_ID", "tenant-1")
	t.Setenv("SIGIL_AUTH_TOKEN", "glc_tok")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "")

	probe := defaultProbeOTLP(context.Background())
	if probe == nil {
		t.Fatal("expected a probe result")
	}
	if !hitMetrics || !hitTraces {
		t.Fatalf("both signals must be probed: metrics=%v traces=%v", hitMetrics, hitTraces)
	}
	if probe.Metrics.StatusCode != 403 || !probe.Metrics.authFailure() {
		t.Fatalf("metrics probe = %+v", probe.Metrics)
	}
	if !strings.Contains(probe.Metrics.Message, "metrics:write/traces:write") {
		t.Fatalf("metrics message = %q", probe.Metrics.Message)
	}
}

func TestProbeOTLP_NoEndpoint(t *testing.T) {
	t.Setenv("SIGIL_OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	if got := defaultProbeOTLP(context.Background()); got != nil {
		t.Fatalf("expected nil probe when no endpoint configured, got %+v", got)
	}
}
