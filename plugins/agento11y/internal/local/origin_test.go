package local

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckRequestOrigin(t *testing.T) {
	tests := []struct {
		name           string
		method         string
		path           string
		host           string
		origin         string
		fetchSite      string
		fetchMode      string
		fetchDest      string
		forwardedProto string
		allowedHosts   []string
		wantErr        string
	}{
		{
			name: "IPv4 loopback host",
			host: "127.0.0.1:8765",
		},
		{
			name: "localhost host",
			host: "localhost:8765",
		},
		{
			name: "uppercase localhost host",
			host: "LOCALHOST:8765",
		},
		{
			name: "IPv6 loopback host",
			host: "[::1]:8765",
		},
		{
			name: "bare IPv4 loopback host",
			host: "127.0.0.1",
		},
		{
			name:    "rebound hostname",
			host:    "example.com:8765",
			wantErr: "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:    "loopback shaped hostname",
			host:    "127.0.0.1.attacker.com:8765",
			wantErr: "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:    "unspecified IPv4 host",
			host:    "0.0.0.0:8765",
			wantErr: "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:    "short IPv4 host",
			host:    "127.1:8765",
			wantErr: "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:    "empty host",
			host:    "",
			wantErr: "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:   "same loopback origin",
			host:   "127.0.0.1:8765",
			origin: "http://127.0.0.1:8765",
		},
		{
			name:   "same localhost origin",
			host:   "localhost:8765",
			origin: "http://localhost:8765",
		},
		{
			name:    "null origin",
			host:    "127.0.0.1:8765",
			origin:  "null",
			wantErr: "cross-origin request",
		},
		{
			name:    "another loopback port",
			host:    "127.0.0.1:8765",
			origin:  "http://127.0.0.1:3000",
			wantErr: "cross-origin request",
		},
		{
			name:    "remote origin",
			host:    "127.0.0.1:8765",
			origin:  "https://evil.com",
			wantErr: "cross-origin request",
		},
		{
			name:         "allowlisted host without origin",
			host:         "space-8765.app.github.dev",
			allowedHosts: []string{"space-8765.app.github.dev"},
		},
		{
			name:           "allowlisted forwarded HTTPS host without port",
			host:           "space-8765.app.github.dev",
			origin:         "https://space-8765.app.github.dev",
			forwardedProto: "https",
			allowedHosts:   []string{"space-8765.app.github.dev"},
		},
		{
			name:           "allowlisted forwarded HTTPS host rejects HTTP origin",
			host:           "space-8765.app.github.dev",
			origin:         "http://space-8765.app.github.dev",
			forwardedProto: "https",
			allowedHosts:   []string{"space-8765.app.github.dev"},
			wantErr:        "cross-origin request",
		},
		{
			name:           "allowlisted forwarded HTTPS host rejects loopback HTTP origin",
			host:           "space-8765.app.github.dev",
			origin:         "http://127.0.0.1",
			forwardedProto: "https",
			allowedHosts:   []string{"space-8765.app.github.dev"},
			wantErr:        "cross-origin request",
		},
		{
			name:         "allowlisted host and implicit HTTPS port",
			host:         "space-8765.app.github.dev:443",
			origin:       "https://space-8765.app.github.dev",
			allowedHosts: []string{"space-8765.app.github.dev"},
		},
		{
			name:         "allowlisted host and implicit HTTP port",
			host:         "space-8765.app.github.dev:80",
			origin:       "http://space-8765.app.github.dev",
			allowedHosts: []string{"space-8765.app.github.dev"},
		},
		{
			name:         "implicit origin port uses scheme default",
			host:         "space-8765.app.github.dev:443",
			origin:       "http://space-8765.app.github.dev",
			allowedHosts: []string{"space-8765.app.github.dev"},
			wantErr:      "cross-origin request",
		},
		{
			name:         "allowlist is case insensitive",
			host:         "SPACE-8765.APP.GITHUB.DEV:443",
			origin:       "https://SPACE-8765.APP.GITHUB.DEV:443",
			allowedHosts: []string{"space-8765.app.github.dev"},
		},
		{
			name:         "allowlist does not include another host",
			host:         "evil.com:8765",
			allowedHosts: []string{"space-8765.app.github.dev"},
			wantErr:      "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:      "same-origin fetch metadata",
			host:      "127.0.0.1:8765",
			fetchSite: "same-origin",
			fetchMode: "cors",
			fetchDest: "empty",
		},
		{
			name:      "direct navigation fetch metadata",
			host:      "127.0.0.1:8765",
			fetchSite: "none",
			fetchMode: "navigate",
			fetchDest: "document",
		},
		{
			name:      "cross-site document navigation",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "document",
		},
		{
			name:      "same-site document navigation",
			host:      "127.0.0.1:8765",
			fetchSite: "same-site",
			fetchMode: "navigate",
			fetchDest: "document",
		},
		{
			name:      "cross-site HEAD document navigation",
			method:    http.MethodHead,
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "document",
		},
		{
			name:      "cross-site fetch",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "cors",
			fetchDest: "empty",
			wantErr:   "cross-site request",
		},
		{
			name:      "same-site fetch",
			host:      "127.0.0.1:8765",
			fetchSite: "same-site",
			fetchMode: "cors",
			fetchDest: "empty",
			wantErr:   "cross-site request",
		},
		{
			name:      "cross-site frame",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "iframe",
			wantErr:   "cross-site request",
		},
		{
			name:      "cross-site object",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "object",
			wantErr:   "cross-site request",
		},
		{
			name:      "cross-site embed",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "embed",
			wantErr:   "cross-site request",
		},
		{
			name:      "cross-site POST document navigation",
			method:    http.MethodPost,
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			fetchMode: "navigate",
			fetchDest: "document",
			wantErr:   "cross-site request",
		},
		{
			name:      "generation ingest skips fetch metadata",
			method:    http.MethodPost,
			path:      "/api/v1/generations:export",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
		},
		{
			name:      "trace ingest skips fetch metadata",
			method:    http.MethodPost,
			path:      "/otlp/v1/traces",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
		},
		{
			name:      "metrics ingest skips fetch metadata",
			method:    http.MethodPost,
			path:      "/otlp/v1/metrics",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
		},
		{
			name:      "hook ingest skips fetch metadata",
			method:    http.MethodPost,
			path:      hookEvaluatePath,
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
		},
		{
			name:      "viewer write checks fetch metadata",
			method:    http.MethodPost,
			path:      "/api/v1/history:import",
			host:      "127.0.0.1:8765",
			fetchSite: "cross-site",
			wantErr:   "cross-site request",
		},
		{
			name:   "Go SDK request",
			method: http.MethodPost,
			path:   "/api/v1/generations:export",
			host:   "127.0.0.1:8765",
		},
		{
			name:      "Node fetch request",
			method:    http.MethodPost,
			path:      "/api/v1/generations:export",
			host:      "127.0.0.1:8765",
			fetchMode: "cors",
		},
		{
			name:      "browser same-origin GET",
			method:    http.MethodGet,
			path:      "/api/v1/conversations",
			host:      "127.0.0.1:8765",
			fetchSite: "same-origin",
			fetchMode: "cors",
			fetchDest: "empty",
		},
		{
			name:      "browser same-origin POST",
			method:    http.MethodPut,
			path:      "/api/v1/config",
			host:      "127.0.0.1:8765",
			origin:    "http://127.0.0.1:8765",
			fetchSite: "same-origin",
			fetchMode: "cors",
			fetchDest: "empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			method := tt.method
			if method == "" {
				method = http.MethodGet
			}
			path := tt.path
			if path == "" {
				path = "/"
			}
			req, err := http.NewRequest(method, "http://127.0.0.1:8765"+path, nil)
			require.NoError(t, err)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			req.Header.Set("Sec-Fetch-Mode", tt.fetchMode)
			req.Header.Set("Sec-Fetch-Dest", tt.fetchDest)
			req.Header.Set("X-Forwarded-Proto", tt.forwardedProto)

			err = checkRequestOrigin(req, tt.allowedHosts)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestAllowedHostsFromEnv(t *testing.T) {
	t.Setenv("AGENTO11Y_LOCAL_ALLOWED_HOSTS", " Space-8765.App.GitHub.Dev:8765, proxy.example.com:443, ,SPACE-8765.APP.GITHUB.DEV ")
	t.Setenv("SIGIL_LOCAL_ALLOWED_HOSTS", "legacy.example.com")

	assert.Equal(t, []string{"space-8765.app.github.dev", "proxy.example.com", "space-8765.app.github.dev"}, allowedHostsFromEnv())
}

func TestServerServeHTTPChecksRequestOrigin(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		path         string
		host         string
		origin       string
		fetchSite    string
		fetchMode    string
		fetchDest    string
		contentType  string
		allowedHosts []string
		wantStatus   int
		wantBody     string
		wantCalls    int
	}{
		{
			name:       "loopback request",
			method:     http.MethodGet,
			path:       "/api/v1/conversations",
			host:       "127.0.0.1:8765",
			wantStatus: http.StatusNoContent,
			wantCalls:  1,
		},
		{
			name:       "rebound hostname",
			method:     http.MethodGet,
			path:       "/api/v1/conversations",
			host:       "evil.com:8765",
			wantStatus: http.StatusForbidden,
			wantBody:   "AGENTO11Y_LOCAL_ALLOWED_HOSTS",
		},
		{
			name:       "remote origin",
			method:     http.MethodPost,
			path:       "/api/v1/history:import",
			host:       "127.0.0.1:8765",
			origin:     "https://evil.com",
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-origin request",
		},
		{
			name:       "another loopback port",
			method:     http.MethodPost,
			path:       "/api/v1/history:import",
			host:       "127.0.0.1:8765",
			origin:     "http://127.0.0.1:3000",
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-origin request",
		},
		{
			name:       "cross-site fetch",
			method:     http.MethodGet,
			path:       "/api/v1/search?q=x",
			host:       "127.0.0.1:8765",
			fetchSite:  "cross-site",
			fetchMode:  "cors",
			fetchDest:  "empty",
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-site request",
		},
		{
			name:       "cross-site document navigation",
			method:     http.MethodGet,
			path:       "/",
			host:       "127.0.0.1:8765",
			fetchSite:  "cross-site",
			fetchMode:  "navigate",
			fetchDest:  "document",
			wantStatus: http.StatusNoContent,
			wantCalls:  1,
		},
		{
			name:       "cross-site frame",
			method:     http.MethodGet,
			path:       "/",
			host:       "127.0.0.1:8765",
			fetchSite:  "cross-site",
			fetchMode:  "navigate",
			fetchDest:  "iframe",
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-site request",
		},
		{
			name:         "allowlisted host and implicit HTTPS port",
			method:       http.MethodGet,
			path:         "/api/v1/conversations",
			host:         "space-8765.app.github.dev:443",
			origin:       "https://space-8765.app.github.dev",
			allowedHosts: []string{"space-8765.app.github.dev"},
			wantStatus:   http.StatusNoContent,
			wantCalls:    1,
		},
		{
			name:        "ingest route skips fetch metadata",
			method:      http.MethodPost,
			path:        "/api/v1/generations:export",
			host:        "127.0.0.1:8765",
			fetchSite:   "cross-site",
			contentType: "application/json",
			wantStatus:  http.StatusNoContent,
			wantCalls:   1,
		},
		{
			name:       "viewer write checks fetch metadata",
			method:     http.MethodPost,
			path:       "/api/v1/history:import",
			host:       "127.0.0.1:8765",
			fetchSite:  "cross-site",
			wantStatus: http.StatusForbidden,
			wantBody:   "cross-site request",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
				calls++
				w.WriteHeader(http.StatusNoContent)
			})
			srv := &Server{allowedHosts: tt.allowedHosts, mux: mux}
			req := newLocalRequest(tt.method, tt.path, nil)
			req.Host = tt.host
			req.Header.Set("Origin", tt.origin)
			req.Header.Set("Sec-Fetch-Site", tt.fetchSite)
			req.Header.Set("Sec-Fetch-Mode", tt.fetchMode)
			req.Header.Set("Sec-Fetch-Dest", tt.fetchDest)
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			rr := httptest.NewRecorder()

			srv.ServeHTTP(rr, req)

			assert.Equal(t, tt.wantStatus, rr.Code)
			assert.Equal(t, tt.wantCalls, calls)
			if tt.wantBody != "" {
				assert.Contains(t, rr.Body.String(), tt.wantBody)
			}
		})
	}
}

func TestNewServerReadsAllowedHostsOnce(t *testing.T) {
	clearForwardEnv(t)
	newServer := func() *Server {
		dir := filepath.Join(t.TempDir(), "local")
		storage, err := NewStorage(dir)
		require.NoError(t, err)
		return NewServer(storage, nil, filepath.Join(dir, "config.env"))
	}

	t.Setenv("AGENTO11Y_LOCAL_ALLOWED_HOSTS", "space-8765.app.github.dev:8765")
	srv := newServer()

	t.Setenv("AGENTO11Y_LOCAL_ALLOWED_HOSTS", "")
	req := newLocalRequest(http.MethodGet, "/healthz", nil)
	req.Host = "space-8765.app.github.dev:8765"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code)

	other := newServer()
	req = newLocalRequest(http.MethodGet, "/healthz", nil)
	req.Host = "space-8765.app.github.dev"
	rr = httptest.NewRecorder()
	other.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code)
}
