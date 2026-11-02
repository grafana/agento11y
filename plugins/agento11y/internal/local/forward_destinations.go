package local

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	"github.com/pelletier/go-toml/v2"
	"golang.org/x/net/http/httpguts"
)

const forwardingFile = "forwarding.toml"

// The primary destination keeps one of the four available slots, even when disabled.
const maxExtraForwardDestinations = 3

type forwardDestination struct {
	name   string
	config forwardConfig
}

type forwardDestinationFields struct {
	Name         string `toml:"name"`
	Endpoint     string `toml:"endpoint"`
	TenantID     string `toml:"tenant_id"`
	Token        string `toml:"token"`
	OTLPEndpoint string `toml:"otlp_endpoint"`
	OTLPToken    string `toml:"otlp_token"`
	OTLPHeaders  string `toml:"otlp_headers"`
}

// parseForwardDestinations resolves only file values. It never reads config.env or the process environment.
func parseForwardDestinations(data []byte) ([]forwardDestination, error) {
	var doc struct {
		Destinations []forwardDestinationFields `toml:"destinations"`
	}
	if err := toml.NewDecoder(bytes.NewReader(data)).DisallowUnknownFields().Decode(&doc); err != nil {
		// TOML errors can include source lines containing credentials.
		return nil, errors.New("forwarding.toml must contain valid TOML with only supported destination fields")
	}
	if len(doc.Destinations) > maxExtraForwardDestinations {
		return nil, errors.New("forwarding.toml allows at most three extra destinations")
	}
	out := make([]forwardDestination, 0, len(doc.Destinations))
	names := map[string]bool{"primary": true}
	for _, fields := range doc.Destinations {
		name := strings.TrimSpace(fields.Name)
		if name == "" || hasForwardControl(fields.Name) {
			return nil, errors.New("destination name must be non-empty and contain no control characters")
		}
		if names[name] {
			return nil, errors.New("destination names must be unique and must not be primary")
		}
		names[name] = true
		cfg, err := resolveExtraForwardDestination(fields)
		if err != nil {
			return nil, err
		}
		out = append(out, forwardDestination{name: name, config: cfg})
	}
	if err := validateForwardDestinationRoutes(out); err != nil {
		return nil, err
	}
	return out, nil
}

func resolveExtraForwardDestination(fields forwardDestinationFields) (forwardConfig, error) {
	cfg := forwardConfig{strip: true, failOpen: true, timeoutMs: envconfig.DefaultGuardsTimeoutMs}
	endpoint, err := validateExtraForwardEndpoint(fields.Endpoint)
	if err != nil {
		return forwardConfig{}, err
	}
	otlpEndpoint, err := validateExtraForwardEndpoint(fields.OTLPEndpoint)
	if err != nil {
		return forwardConfig{}, err
	}
	if endpoint == "" && otlpEndpoint == "" {
		return forwardConfig{}, errors.New("destination requires endpoint or otlp_endpoint")
	}
	if slices.ContainsFunc([]string{fields.TenantID, fields.Token, fields.OTLPToken}, hasForwardControl) {
		return forwardConfig{}, errors.New("destination credentials must not contain control characters")
	}
	if err := validateExtraForwardHeaders(fields.OTLPHeaders); err != nil {
		return forwardConfig{}, err
	}
	tenant := strings.TrimSpace(fields.TenantID)
	token := strings.TrimSpace(fields.Token)
	if endpoint != "" {
		if forwardDisabledReason(endpoint, tenant, token) != "" {
			return forwardConfig{}, errors.New("destination endpoint requires non-empty tenant_id and token values other than \"local\"")
		}
		genURL, err := wire.NormalizeGenerationExportURL(endpoint, false)
		if err != nil {
			return forwardConfig{}, errors.New("destination generation endpoint is invalid")
		}
		cfg.genURL = genURL
		cfg.genHeaders = generationForwardHeaders(tenant, token)
	}
	if otlpEndpoint != "" {
		if envconfig.IsLocalEndpoint(otlpEndpoint) {
			return forwardConfig{}, errors.New("destination OTLP endpoint must not use HTTP with localhost, 127.0.0.1, or ::1 (including equivalent IP spellings)")
		}
		cfg.otlpEndpoint = otlpEndpoint
		// Only this destination's token can be the fallback for its OTLP token.
		cfg.otlpHeaders = otel.ExporterHeaders(fields.OTLPHeaders, tenant, fields.OTLPToken, token)
	}
	cfg.enabled = cfg.genURL != "" || cfg.otlpEndpoint != ""
	return cfg, nil
}

func validateExtraForwardEndpoint(raw string) (string, error) {
	endpoint := strings.TrimSpace(raw)
	if endpoint == "" && !hasForwardControl(raw) {
		return "", nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || hasForwardControl(raw) || u == nil ||
		(u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.Opaque != "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || strings.Contains(endpoint, "#") {
		return "", errors.New("destination endpoints must be explicit HTTP(S) URLs without userinfo, query, or fragment")
	}
	return u.String(), nil
}

func validateExtraForwardHeaders(raw string) error {
	if strings.TrimSpace(raw) == "" && !hasForwardControl(raw) {
		return nil
	}
	seen := map[string]bool{}
	for pair := range strings.SplitSeq(raw, ",") {
		key, value, ok := strings.Cut(pair, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !ok || key == "" || value == "" || hasForwardControl(pair) ||
			!httpguts.ValidHeaderFieldName(key) || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("destination otlp_headers must contain valid comma-separated name=value headers")
		}
		key = strings.ToLower(key)
		switch key {
		case strings.ToLower(ForwardMarkerHeader), "content-type", "content-encoding", "host", "content-length", "length":
			return errors.New("destination otlp_headers must not set reserved headers: X-Agento11y-Local-Forwarded, Content-Type, Content-Encoding, Host, Content-Length, or Length (case-insensitive)")
		}
		if seen[key] {
			return errors.New("destination otlp_headers must not repeat header names")
		}
		seen[key] = true
	}
	return nil
}

func hasForwardControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

func validateForwardDestinationRoutes(destinations []forwardDestination) error {
	type route struct {
		signal   string
		endpoint string
		headers  string
	}
	seen := map[route]bool{}
	add := func(signal, endpoint string, headers map[string]string) bool {
		u, err := url.Parse(endpoint)
		if err != nil || endpoint == "" {
			return true
		}
		u.Host = strings.ToLower(u.Host)
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			u.Host = strings.TrimSuffix(u.Host, ":"+u.Port())
		}
		effective := make(map[string]string, len(headers))
		for key, value := range headers {
			if value != "" {
				effective[strings.ToLower(key)] = value
			}
		}
		// Lowercased header names and JSON's sorted map keys prevent header casing
		// or order from hiding a duplicate route.
		auth, _ := json.Marshal(effective)
		key := route{signal: signal, endpoint: u.String(), headers: string(auth)}
		if seen[key] {
			return false
		}
		seen[key] = true
		return true
	}
	for _, destination := range destinations {
		cfg := destination.config
		if !add("generations", cfg.genURL, cfg.genHeaders) {
			return errors.New("destinations must not repeat a signal endpoint with the same effective headers")
		}
		if cfg.otlpEndpoint != "" {
			for _, signal := range []string{"traces", "metrics"} {
				if !add(signal, otel.SignalEndpointURL(cfg.otlpEndpoint, signal), cfg.otlpHeaders) {
					return errors.New("destinations must not repeat a signal endpoint with the same effective headers")
				}
			}
		}
	}
	return nil
}

type ForwardDestinationInfo struct {
	Name            string `json:"name"`
	Endpoint        string `json:"endpoint"`
	OTLPEndpoint    string `json:"otlpEndpoint"`
	HTTPGenerations bool   `json:"httpGenerations"`
	OTLP            bool   `json:"otlp"`
}

// ForwardingInspection describes only the extra destinations in forwarding.toml.
type ForwardingInspection struct {
	Path         string                   `json:"path"`
	Destinations []ForwardDestinationInfo `json:"destinations"`
	Error        string                   `json:"error,omitempty"`
}

// ForwardPrimaryRoutes contains the primary's resolved, usable signal routes.
// Credentials are used only for duplicate detection and are never returned in an inspection.
type ForwardPrimaryRoutes struct {
	GenerationURL string
	OTLPEndpoint  string
	TenantID      string
	Token         string
	OTLPToken     string
	OTLPHeaders   string
}

// InspectForwarding reads forwarding.toml beside the resolved config.env path. It does not resolve config.env itself.
func InspectForwarding(configPath string) ForwardingInspection {
	return inspectForwarding(configPath, forwardConfig{})
}

// InspectForwardingWithPrimary checks extras against primary before discarding
// credentials. A duplicate disables every extra, as in the daemon loader.
func InspectForwardingWithPrimary(configPath string, primary ForwardPrimaryRoutes) ForwardingInspection {
	return inspectForwarding(configPath, forwardConfig{
		genURL: primary.GenerationURL, genHeaders: generationForwardHeaders(primary.TenantID, primary.Token),
		otlpEndpoint: primary.OTLPEndpoint,
		otlpHeaders:  otel.ExporterHeaders(primary.OTLPHeaders, primary.TenantID, primary.OTLPToken, primary.Token),
	})
}

func inspectForwarding(configPath string, primary forwardConfig) ForwardingInspection {
	inspection := ForwardingInspection{Destinations: []ForwardDestinationInfo{}}
	if configPath == "" {
		return inspection
	}
	inspection.Path = filepath.Join(filepath.Dir(configPath), forwardingFile)
	data, err := os.ReadFile(inspection.Path)
	if err != nil {
		if !os.IsNotExist(err) {
			inspection.Error = "forwarding.toml could not be read"
		}
		return inspection
	}
	destinations, err := parseForwardDestinations(data)
	if err != nil {
		inspection.Error = "forwarding.toml is invalid"
		return inspection
	}
	combined := append([]forwardDestination{{name: "primary", config: primary}}, destinations...)
	if err := validateForwardDestinationRoutes(combined); err != nil {
		inspection.Error = "forwarding.toml duplicates a primary signal route; all extra destinations are disabled"
		return inspection
	}
	for _, destination := range destinations {
		cfg := destination.config
		inspection.Destinations = append(inspection.Destinations, ForwardDestinationInfo{
			Name: destination.name, Endpoint: cfg.genURL, OTLPEndpoint: cfg.otlpEndpoint,
			HTTPGenerations: cfg.genURL != "", OTLP: cfg.otlpEndpoint != "",
		})
	}
	return inspection
}
