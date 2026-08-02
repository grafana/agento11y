package doctor

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
)

// probeTimeout bounds each network probe so `--probe` stays responsive even
// against a black-holed endpoint. A var so tests can shrink it.
var probeTimeout = 3 * time.Second

// probeClient is the shared client for probes. Per-request contexts carry the
// real deadline; the client timeout is a backstop.
var probeClient = &http.Client{Timeout: probeTimeout}

// defaultProbeConversations checks the generation-export endpoint with the
// same headers a real export sends: HTTP Basic auth (base64(tenant:token))
// plus the X-Scope-OrgID tenant header, matching the SDK exporter's
// ExportAuthModeBasic that every agent plugin uses. Using Bearer here would
// draw a spurious 401 from Grafana Cloud's gateway even with a valid token. It
// POSTs an empty body so the edge auth layer is exercised (401/403 surface
// before the empty body is rejected) without creating a real generation.
// Connectivity and auth failures are returned as results, never as errors that
// abort the report. insecure mirrors SIGIL_INSECURE so a scheme-less endpoint
// resolves to http here just as the SDK exporter does; otherwise the probe
// would hit https and report a cleartext setup as unreachable.
func defaultProbeConversations(ctx context.Context, endpoint string, tenant envValue, token string, insecure bool) *ProbeResult {
	target, err := wire.NormalizeGenerationExportURL(endpoint, insecure)
	if err != nil {
		return &ProbeResult{Message: "invalid endpoint: " + err.Error()}
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader("{}"))
	if err != nil {
		return &ProbeResult{URL: target, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	if tenant.Value != "" {
		req.Header.Set("X-Scope-OrgID", tenant.Value)
	}
	if token != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(tenant.Value+":"+token)))
	}

	res := doProbe(target, req)
	switch {
	case res.credentialsRejected():
		// Sigil's tenant auth answers 401 for every auth failure on this
		// endpoint, a missing write scope included, so name the scope here
		// too — last, because it is the rarest cause.
		res.Message = credentialsRejectedMessage(tenant) +
			". A token without the sigil:write scope is rejected the same way"
	case res.scopeDenied():
		res.Message = "endpoint rejected auth — token likely missing sigil:write scope"
	}
	return res
}

// credentialsRejectedMessage explains an HTTP 401: the endpoint refused the
// credentials and does not say which one is wrong. Name the token first and
// the tenant id second, with its resolved value, because a wrong tenant id
// reads exactly like a bad token from here. tenant is the tenant id the
// request authenticated with; a zero value means the request did not use one,
// and then the message says nothing about a variable the request never read.
func credentialsRejectedMessage(tenant envValue) string {
	const rejected = "credentials rejected: the token may be invalid or expired"
	if !tenant.Set {
		return rejected
	}
	return fmt.Sprintf("%s, or %s (%s) may be wrong",
		rejected, cmp.Or(tenant.Key, envconfig.PreferredKey("AUTH_TENANT_ID")), tenant.Value)
}

// defaultProbeOTLP checks the OTLP metrics and traces endpoints, reusing the
// resolved signal URLs and synthesized auth headers from internal/otel. Each
// signal is POSTed an empty JSON body so the edge auth layer is exercised
// without pushing data; 401 means the credentials were refused and 403 that
// the token is missing metrics:write/traces:write. The real exporter sends
// protobuf, so an endpoint that validates content-type before auth could
// answer 400/415 here; against Grafana's OTLP gateway auth precedes parsing,
// so 200/401/403 is what's seen. tenant is the tenant id the exporter
// authenticates with, zero when an explicit Authorization header does instead;
// it is only used to name the variable in a 401 message.
func defaultProbeOTLP(ctx context.Context, tenant envValue) *AnalyticsProbe {
	metrics, traces, ok := otel.ProbeConfig()
	if !ok {
		return nil
	}
	return &AnalyticsProbe{
		Metrics: probeOTLPSignal(ctx, metrics, tenant),
		Traces:  probeOTLPSignal(ctx, traces, tenant),
	}
}

func probeOTLPSignal(ctx context.Context, target otel.ProbeTarget, tenant envValue) *ProbeResult {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.URL, strings.NewReader("{}"))
	if err != nil {
		return &ProbeResult{URL: target.URL, Message: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}

	res := doProbe(target.URL, req)
	switch {
	case res.credentialsRejected():
		res.Message = credentialsRejectedMessage(tenant)
	case res.scopeDenied():
		res.Message = "missing metrics:write/traces:write scope"
	}
	return res
}

// doProbe sends req and maps the outcome to a ProbeResult. A transport error
// is reported as no response; any HTTP status below 400 (and 405, since a
// method-restricted route still proves reach + auth) counts as reachable.
func doProbe(target string, req *http.Request) *ProbeResult {
	resp, err := probeClient.Do(req)
	if err != nil {
		return &ProbeResult{URL: target, Message: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()

	return &ProbeResult{
		URL:        target,
		StatusCode: resp.StatusCode,
		OK:         resp.StatusCode < 400 || resp.StatusCode == http.StatusMethodNotAllowed,
	}
}
