package local

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

func checkRequestOrigin(r *http.Request, allowedHosts []string) error {
	host, port := splitAuthority(r.Host)
	hostAllowed := slices.Contains(allowedHosts, strings.ToLower(host))
	if !loopbackHostname(host) && !hostAllowed {
		// Host keeps the page's hostname after DNS rebinding changes its address
		// to loopback, so this check must run before any handler reads the store.
		return fmt.Errorf("refused: Host %q is not this machine; set AGENTO11Y_LOCAL_ALLOWED_HOSTS to allow it", r.Host)
	}
	if port == "" {
		port = defaultPortForRequest(r, hostAllowed)
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameLocalOrigin(origin, port, allowedHosts) {
		return fmt.Errorf("refused: cross-origin request from %q", origin)
	}
	if !sdkIngestPath(r.URL.Path) {
		return checkFetchMetadata(r)
	}
	return nil
}

func splitAuthority(authority string) (string, string) {
	host, port, err := net.SplitHostPort(authority)
	if err == nil {
		return host, port
	}
	return strings.TrimSuffix(strings.TrimPrefix(authority, "["), "]"), ""
}

func loopbackHostname(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}

func sameLocalOrigin(origin, port string, allowedHosts []string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	host, originPort := splitAuthority(u.Host)
	if !loopbackHostname(host) && !slices.Contains(allowedHosts, strings.ToLower(host)) {
		return false
	}
	defaultPort := defaultPortForScheme(u.Scheme)
	if defaultPort == "" {
		return false
	}
	if originPort == "" {
		originPort = defaultPort
	}
	return originPort == port
}

func defaultPortForRequest(r *http.Request, trustForwardedProto bool) string {
	if trustForwardedProto {
		if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
			// Allowed hosts opt into a local reverse proxy. The proxy must replace
			// this client-controlled header rather than append to it.
			return defaultPortForScheme(proto)
		}
	}
	if r.TLS != nil {
		return "443"
	}
	return "80"
}

func defaultPortForScheme(scheme string) string {
	switch strings.ToLower(scheme) {
	case "http":
		return "80"
	case "https":
		return "443"
	default:
		return ""
	}
}

func checkFetchMetadata(r *http.Request) error {
	site := r.Header.Get("Sec-Fetch-Site")
	switch site {
	case "", "same-origin", "none":
		return nil
	}

	// A linking site cannot read a top-level tab. Frames and subresources keep
	// the response inside the linking site's page and must remain blocked.
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) &&
		r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document" {
		return nil
	}
	return fmt.Errorf("refused: cross-site request (Sec-Fetch-Site: %s)", site)
}

func sdkIngestPath(path string) bool {
	switch path {
	case "/api/v1/generations:export", "/otlp/v1/traces", "/otlp/v1/metrics", hookEvaluatePath:
		return true
	default:
		return false
	}
}

func allowedHostsFromEnv() []string {
	value, _, ok := envconfig.LookupEnv("LOCAL_ALLOWED_HOSTS")
	if !ok {
		return nil
	}

	var hosts []string
	for authority := range strings.SplitSeq(value, ",") {
		host, _ := splitAuthority(strings.TrimSpace(authority))
		if host = strings.ToLower(host); host != "" {
			hosts = append(hosts, host)
		}
	}
	return hosts
}
