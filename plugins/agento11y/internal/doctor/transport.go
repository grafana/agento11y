package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/local"
	"github.com/grafana/agento11y/plugins/agento11y/internal/localhealth"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

type HTTPAPISection struct {
	Endpoint envValue   `json:"endpoint"`
	TenantID envValue   `json:"tenant_id"`
	Token    tokenValue `json:"token"`
	Health   Health     `json:"status"`
	Messages []string   `json:"messages,omitempty"`
}

func requestedProtocol(osEnv, fileEnv map[string]string) string {
	raw := resolveFamily("PROTOCOL", osEnv, fileEnv).value
	if raw == "" {
		return "http"
	}
	return strings.ToLower(raw)
}

func resolveGenerationTransport(osEnv, fileEnv map[string]string) (agento11y.GenerationExportProtocol, error) {
	return envconfig.ResolveGenerationTransportWith(func(suffix string) (string, string, bool) {
		if suffix == "OTEL_EXPORTER_OTLP_ENDPOINT" {
			r := generationEndpoint(osEnv, fileEnv)
			return r.value, r.key, r.set
		}
		r := resolveFamily(suffix, osEnv, fileEnv)
		return r.value, r.key, r.set
	}, func(key string) string {
		return resolveEnv(key, osEnv, fileEnv).value
	})
}

func otlpEndpoint(osEnv, fileEnv map[string]string) resolved {
	r := resolveFamily("OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)
	if !r.set {
		r = resolveEnv("OTEL_EXPORTER_OTLP_ENDPOINT", osEnv, fileEnv)
	}
	return r
}

func managedLocalOnly(osEnv, fileEnv map[string]string) bool {
	return receiverSupported() && envconfig.ParseBool(resolveFamily("LOCAL", osEnv, fileEnv).value) &&
		!envconfig.ParseBool(daemonFamily("LOCAL_FORWARD", osEnv, fileEnv).value)
}

func generationEndpoint(osEnv, fileEnv map[string]string) resolved {
	if managedLocalOnly(osEnv, fileEnv) {
		endpoint := fmt.Sprintf("http://127.0.0.1:%d", local.DefaultPort)
		if status, err := local.LoadStatus(local.StateDir()); err == nil && status != nil && envconfig.IsLocalEndpoint(status.Endpoint) {
			endpoint = strings.TrimRight(status.Endpoint, "/")
		}
		return resolved{set: true, value: endpoint + "/otlp", source: "local daemon"}
	}
	r := otlpEndpoint(osEnv, fileEnv)
	insecure := resolveFamily("OTEL_EXPORTER_OTLP_INSECURE", osEnv, fileEnv)
	if !insecure.set {
		insecure = resolveEnv("OTEL_EXPORTER_OTLP_INSECURE", osEnv, fileEnv)
	}
	r.value = otel.EffectiveEndpointURL(r.value, envconfig.ParseBool(insecure.value))
	return r
}

func collectOTelConversations(osEnv, fileEnv map[string]string) ConversationsSection {
	token := resolveFamily("OTEL_AUTH_TOKEN", osEnv, fileEnv)
	if !token.set {
		token = resolveFamily("AUTH_TOKEN", osEnv, fileEnv)
	}
	sec := ConversationsSection{
		Protocol: "otel",
		Endpoint: generationEndpoint(osEnv, fileEnv).envValue(),
		TenantID: resolveFamily("AUTH_TENANT_ID", osEnv, fileEnv).envValue(),
		Token:    token.tokenValue(),
		Health:   HealthOK,
		Messages: []string{"experimental OTel generation export; no HTTP fallback"},
	}
	if headersHaveAuthorization(resolveEnv("OTEL_EXPORTER_OTLP_HEADERS", osEnv, fileEnv).value) {
		sec.Messages = append(sec.Messages, "OTLP authentication uses OTEL_EXPORTER_OTLP_HEADERS")
	}
	if !analyticsAuthResolvable(osEnv, fileEnv) {
		sec.Messages = append(sec.Messages, "no OTLP credentials resolved; the probe checks whether the collector accepts unauthenticated requests")
	}
	mode := resolveFamily("CONTENT_CAPTURE_MODE", osEnv, fileEnv)
	if envconfig.ResolveContentModeValue(nil, mode.key, mode.value) == agento11y.ContentCaptureModeFullWithMetadataSpans {
		sec.Messages = append(sec.Messages, "OTel upgrades full_with_metadata_spans to full capture")
	}
	return sec
}

func collectHTTPAPIs(osEnv, fileEnv map[string]string) *HTTPAPISection {
	sec := &HTTPAPISection{
		Endpoint: resolveFamily("ENDPOINT", osEnv, fileEnv).envValue(),
		TenantID: resolveFamily("AUTH_TENANT_ID", osEnv, fileEnv).envValue(),
		Token:    resolveFamily("AUTH_TOKEN", osEnv, fileEnv).tokenValue(),
		Health:   HealthSkipped,
		Messages: []string{"guards, scores, and experiment APIs use HTTP separately from OTel capture; API access was not probed"},
	}
	if !sec.Endpoint.Set || !sec.TenantID.Set || !sec.Token.Set {
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, "HTTP API credentials are incomplete; this does not block Go OTel capture")
	}
	return sec
}

func diagnoseIndependentPlugins(r *Report) {
	const limitation = "live plugin does not support OTel generation export and keeps HTTP; Go history imports support OTel"
	r.Conversations.Messages = append(r.Conversations.Messages,
		"Pi and OpenCode live plugins keep HTTP generation export; their Go history imports support OTel")
	for i := range r.Agents {
		a := &r.Agents[i]
		if a.Name != "pi" && a.Name != "opencode" {
			continue
		}
		if a.Note != "" {
			a.Note += "; "
		}
		a.Note += limitation
		if a.Health == HealthOK {
			a.Health = HealthWarn
		}
	}
}

func generationProbeTarget(osEnv, fileEnv map[string]string) otel.ProbeTarget {
	target := otel.SignalEndpointURL(generationEndpoint(osEnv, fileEnv).value, "traces")
	if managedLocalOnly(osEnv, fileEnv) {
		return otel.ProbeTarget{URL: target}
	}
	return otel.ProbeTarget{URL: target, Headers: otel.ExporterHeaders(
		resolveEnv("OTEL_EXPORTER_OTLP_HEADERS", osEnv, fileEnv).value,
		resolveFamily("AUTH_TENANT_ID", osEnv, fileEnv).value,
		resolveFamily("OTEL_AUTH_TOKEN", osEnv, fileEnv).value,
		resolveFamily("AUTH_TOKEN", osEnv, fileEnv).value,
	)}
}

func runGenerationOTelProbe(ctx context.Context, r *Report, osEnv, fileEnv map[string]string) {
	sec := &r.Conversations
	if sec.Health == HealthError {
		return
	}
	target := generationProbeTarget(osEnv, fileEnv)
	if localhealth.IsDaemonEndpoint(target.URL) {
		// The capability check must not inherit exporter authorization headers.
		err := localhealth.RequireGenerationIngest(ctx, target.URL)
		sec.LocalCapability = &ProbeResult{OK: err == nil, Message: localhealth.OTelGenerationCapability}
		if err != nil {
			sec.Health = HealthError
			sec.LocalCapability.Message = err.Error()
			sec.Messages = append(sec.Messages, err.Error())
			return
		}
	}
	sec.Probe = probeGenerationOTLP(ctx, target, otlpProbeTenant(sec.TenantID, osEnv, fileEnv))
	switch {
	case sec.Probe.OK:
		sec.Messages = append(sec.Messages, "OTLP transport/authentication verified; generation storage is not verified")
	case sec.Probe.AuthFailure(), sec.Probe.redirected(), sec.Probe.unreachable(), sec.Probe.routeMissing():
		sec.Health = HealthError
		sec.Messages = append(sec.Messages, "OTLP generation trace probe failed: "+describeProbe(sec.Probe))
	default:
		sec.Health = HealthWarn
		sec.Messages = append(sec.Messages, "OTLP generation trace probe was not accepted: "+describeProbe(sec.Probe))
	}
}
