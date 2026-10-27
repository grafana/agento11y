package history

import (
	"errors"
	"log"
	"maps"
	"os"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

// Target configures one import destination. ResolveTarget freezes environment
// defaults before exporter construction and ledger selection. To change a
// resolved destination, construct a new Target instead of editing its fields.
type Target struct {
	Endpoint                   string // generation API endpoint; also the Cloud ledger identity
	OTLPEndpoint               string
	Protocol                   agento11y.GenerationExportProtocol
	ContentMode                string
	TenantID                   string
	AuthToken                  string
	EnableExperimentalFeatures *bool
	// Headers replaces ambient headers on both transports when non-nil.
	// OTLPHeaders overrides just the collector headers when non-nil.
	Headers     map[string]string
	OTLPHeaders map[string]string

	// The frozen copy is private so callers cannot change a constructed
	// exporter's ledger identity by editing its public Target fields.
	resolved *Target
}

func (t Target) effective() Target {
	if t.resolved != nil {
		return *t.resolved
	}
	return t
}

func cloneTarget(t Target) Target {
	t.Headers = maps.Clone(t.Headers)
	t.OTLPHeaders = maps.Clone(t.OTLPHeaders)
	if t.EnableExperimentalFeatures != nil {
		t.EnableExperimentalFeatures = new(*t.EnableExperimentalFeatures)
	}
	return t
}

// ResolveTarget validates and freezes transport, credentials, content capture,
// and destination identity without changing the process environment. Cloud OTel
// still requires the API endpoint and tenant to preserve existing ledgers; it
// does not require an API token or send generations to that endpoint.
func ResolveTarget(target Target, logger *log.Logger) (Target, error) {
	if target.resolved != nil {
		out := cloneTarget(*target.resolved)
		out.resolved = target.resolved
		return out, nil
	}
	t := cloneTarget(target)
	t.OTLPEndpoint = strings.TrimSpace(t.OTLPEndpoint)
	t.Endpoint = t.endpoint()
	if t.Endpoint == "" {
		return Target{}, errors.New("history: no endpoint configured for import (set AGENTO11Y_ENDPOINT or use --local)")
	}
	local := envconfig.IsLocalEndpoint(t.Endpoint)
	requestedProtocol := string(t.Protocol)
	if requestedProtocol == "" {
		requestedProtocol = envconfig.Getenv("PROTOCOL")
	}
	if t.OTLPEndpoint == "" && (local || strings.EqualFold(strings.TrimSpace(requestedProtocol), "otel")) {
		// HTTP backfills must not add old usage to ambient Cloud metrics.
		// ProbeConfig freezes insecure settings and signal suffixes as provider setup does.
		if _, traces, ok := otel.ProbeConfig(); ok {
			t.OTLPEndpoint = strings.TrimSuffix(traces.URL, "/v1/traces")
		}
	}
	if local && strings.TrimSpace(target.OTLPEndpoint) == "" &&
		(t.OTLPEndpoint != "" || strings.EqualFold(strings.TrimSpace(requestedProtocol), "otel")) {
		// An ambient Cloud collector must never receive a local backfill.
		t.OTLPEndpoint = strings.TrimRight(t.Endpoint, "/") + "/otlp"
	}
	if local && t.OTLPEndpoint != "" && !envconfig.IsLocalEndpoint(t.OTLPEndpoint) {
		return Target{}, errors.New("history: local imports require a loopback OTLP endpoint")
	}
	if t.EnableExperimentalFeatures == nil {
		t.EnableExperimentalFeatures = new(envconfig.ParseBool(os.Getenv(agento11y.EnvEnableExperimentalFeatures)))
	}
	look := func(suffix string) (string, string, bool) {
		switch suffix {
		case "PROTOCOL":
			if t.Protocol != "" {
				return string(t.Protocol), "Target.Protocol", true
			}
		case "OTEL_EXPORTER_OTLP_ENDPOINT":
			return t.OTLPEndpoint, "Target.OTLPEndpoint", t.OTLPEndpoint != ""
		}
		return envconfig.LookupEnv(suffix)
	}
	exact := func(key string) string {
		if key == agento11y.EnvEnableExperimentalFeatures {
			if *t.EnableExperimentalFeatures {
				return "true"
			}
			return "false"
		}
		return ""
	}
	var err error
	t.Protocol, err = envconfig.ResolveGenerationTransportWith(look, exact)
	if err != nil {
		return Target{}, err
	}
	if strings.TrimSpace(t.TenantID) == "" {
		t.TenantID = envconfig.Getenv("AUTH_TENANT_ID")
	}
	if strings.TrimSpace(t.AuthToken) == "" {
		t.AuthToken = envconfig.Getenv("AUTH_TOKEN")
	}
	t.TenantID = strings.TrimSpace(t.TenantID)
	t.AuthToken = strings.TrimSpace(t.AuthToken)
	if !local && (t.TenantID == "" || (t.Protocol == agento11y.GenerationExportProtocolHTTP && strings.TrimSpace(t.AuthToken) == "")) {
		return Target{}, errors.New("history: no credentials configured for import (Cloud requires tenant identity; HTTP also requires an API token; run `agento11y login` or use --local)")
	}
	if t.Headers == nil {
		t.Headers = envconfig.ParseExtraTags(envconfig.Getenv("HEADERS"))
	}
	if t.Headers == nil {
		t.Headers = map[string]string{}
	}
	if t.OTLPHeaders == nil {
		if target.Headers != nil {
			t.OTLPHeaders = maps.Clone(t.Headers)
		} else {
			t.OTLPHeaders = otel.ExporterHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS"), t.TenantID, envconfig.Getenv("OTEL_AUTH_TOKEN"), t.AuthToken)
		}
	}
	mode := envconfig.ResolveContentMode(logger)
	if strings.TrimSpace(t.ContentMode) != "" {
		mode = envconfig.ResolveContentModeValue(logger, "", t.ContentMode)
	}
	if local {
		mode = agento11y.ContentCaptureModeFull
		// Local history never needs Cloud authorization, including credentials
		// supplied explicitly by a caller. Replace ambient headers entirely.
		t.Headers = localHeaders(target.Headers)
		t.OTLPHeaders = localHeaders(target.OTLPHeaders)
		if target.OTLPHeaders == nil {
			t.OTLPHeaders = localHeaders(target.Headers)
		}
		t.TenantID, t.AuthToken = envconfig.LocalAuthPlaceholder, envconfig.LocalAuthPlaceholder
	}
	t.ContentMode = mode.String()
	frozen := cloneTarget(t)
	t.resolved = &frozen
	return t, nil
}

func localHeaders(explicit map[string]string) map[string]string {
	headers := make(map[string]string, len(explicit)+1)
	for k, v := range explicit {
		if !strings.EqualFold(k, "Authorization") && !strings.EqualFold(k, ForwardMarkerHeader) {
			headers[k] = v
		}
	}
	headers[ForwardMarkerHeader] = "1"
	return headers
}
