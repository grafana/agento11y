package doctor

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
)

// probeTenant is a resolved AUTH_TENANT_ID as runProbes passes it: value plus
// the spelling it came from.
var probeTenant = envValue{Set: true, Value: "tenant-1", Key: "AGENTO11Y_AUTH_TENANT_ID", Source: sourceEnv}

func TestProbeConversations(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		badURL  bool // probe an unparseable endpoint instead of the test server
		wantOK  bool
		wantMsg []string // substrings the probe message must contain
		skipMsg []string // substrings the probe message must not contain
		wantErr bool     // transport error: status 0 with a message, server never hit
	}{
		{name: "200 ok", status: 200, wantOK: true},
		{
			// 401 is what a bad token and a wrong tenant id both
			// return, so the message must not blame the scope.
			name:   "401 blames credentials and tenant",
			status: 401,
			wantMsg: []string{
				"credentials rejected", "invalid or expired",
				"AGENTO11Y_AUTH_TENANT_ID (tenant-1)", "without the sigil:write scope",
			},
			skipMsg: []string{"likely missing sigil:write"},
		},
		{
			name:    "403 blames scope",
			status:  403,
			wantMsg: []string{"missing sigil:write scope"},
			skipMsg: []string{"credentials rejected"},
		},
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
			res := defaultProbeConversations(context.Background(), url, probeTenant, "glc_tok", false)

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
			for _, want := range tc.wantMsg {
				if !strings.Contains(res.Message, want) {
					t.Fatalf("message %q missing %q", res.Message, want)
				}
			}
			for _, skip := range tc.skipMsg {
				if strings.Contains(res.Message, skip) {
					t.Fatalf("message %q must not contain %q", res.Message, skip)
				}
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

	res := defaultProbeConversations(context.Background(), srv.URL, probeTenant, "tok", false)
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
	res := defaultProbeConversations(context.Background(), srv.Listener.Addr().String(), probeTenant, "tok", true)
	if !res.OK || gotScheme != "http" {
		t.Fatalf("insecure probe = %+v, server scheme %q, want http reach", res, gotScheme)
	}
}

func TestProbeOTLP(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		tenant  envValue
		headers string // OTEL_EXPORTER_OTLP_HEADERS
		wantMsg []string
		skipMsg []string
	}{
		{
			name:    "401 blames credentials and tenant",
			status:  401,
			tenant:  probeTenant,
			wantMsg: []string{"credentials rejected", "invalid or expired", "AGENTO11Y_AUTH_TENANT_ID (tenant-1)"},
			skipMsg: []string{"missing metrics:write/traces:write scope"},
		},
		{
			// runProbes passes a zero tenant id when an explicit
			// Authorization header authenticates the export, because the
			// tenant id never reaches the request.
			name:    "401 without a tenant id names only the token",
			status:  401,
			headers: "Authorization=Bearer explicit",
			wantMsg: []string{"credentials rejected", "invalid or expired"},
			skipMsg: []string{"AUTH_TENANT_ID"},
		},
		{
			name:    "403 blames scope",
			status:  403,
			tenant:  probeTenant,
			wantMsg: []string{"missing metrics:write/traces:write scope"},
			skipMsg: []string{"credentials rejected"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			// The probe reads the process env, so clear the host's config and
			// point the preferred spelling at the test server.
			isolateEnv(t)
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "tenant-1")
			t.Setenv("AGENTO11Y_AUTH_TOKEN", "glc_tok")
			t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", tc.headers)

			probe := defaultProbeOTLP(context.Background(), tc.tenant)
			if probe == nil {
				t.Fatal("expected a probe result")
			}
			if !hitMetrics || !hitTraces {
				t.Fatalf("both signals must be probed: metrics=%v traces=%v", hitMetrics, hitTraces)
			}
			if probe.Metrics.StatusCode != tc.status || !probe.Metrics.authFailure() {
				t.Fatalf("metrics probe = %+v", probe.Metrics)
			}
			for _, signal := range []*ProbeResult{probe.Metrics, probe.Traces} {
				for _, want := range tc.wantMsg {
					if !strings.Contains(signal.Message, want) {
						t.Fatalf("message %q missing %q", signal.Message, want)
					}
				}
				for _, skip := range tc.skipMsg {
					if strings.Contains(signal.Message, skip) {
						t.Fatalf("message %q must not contain %q", signal.Message, skip)
					}
				}
			}
		})
	}
}

func TestCredentialsRejectedMessage(t *testing.T) {
	tests := []struct {
		name    string
		tenant  envValue
		wantMsg []string
		skipMsg []string
	}{
		{
			name:    "resolved tenant is named with its value",
			tenant:  probeTenant,
			wantMsg: []string{"the token may be invalid or expired", "AGENTO11Y_AUTH_TENANT_ID (tenant-1) may be wrong"},
		},
		{
			// The legacy spelling is the one the user set, so it is the
			// one to check.
			name:    "legacy spelling is named as set",
			tenant:  envValue{Set: true, Value: "tenant-1", Key: "SIGIL_AUTH_TENANT_ID", Source: sourceEnv},
			wantMsg: []string{"SIGIL_AUTH_TENANT_ID (tenant-1) may be wrong"},
			skipMsg: []string{"AGENTO11Y_AUTH_TENANT_ID"},
		},
		{
			// No tenant id took part in the request, so pointing at one
			// would send the user to the wrong variable.
			name:    "no tenant id names only the token",
			wantMsg: []string{"the token may be invalid or expired"},
			skipMsg: []string{"AUTH_TENANT_ID"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := credentialsRejectedMessage(tc.tenant)
			for _, want := range tc.wantMsg {
				if !strings.Contains(msg, want) {
					t.Fatalf("message %q missing %q", msg, want)
				}
			}
			for _, skip := range tc.skipMsg {
				if strings.Contains(msg, skip) {
					t.Fatalf("message %q must not contain %q", msg, skip)
				}
			}
		})
	}
}

// With no OTLP endpoint configured there is nothing to probe. isolateEnv
// clears every spelling of the endpoint, including the preferred
// AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT, so a configured host shell cannot
// turn this into a live request.
func TestProbeOTLP_NoEndpoint(t *testing.T) {
	isolateEnv(t)
	if got := defaultProbeOTLP(context.Background(), probeTenant); got != nil {
		t.Fatalf("expected nil probe when no endpoint configured, got %+v", got)
	}
}
