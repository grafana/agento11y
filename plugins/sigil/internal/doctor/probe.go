package doctor

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/proto/sigil/wire"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/otel"
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
func defaultProbeConversations(ctx context.Context, endpoint, tenant, token string, insecure bool) *ProbeResult {
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
	if tenant != "" {
		req.Header.Set("X-Scope-OrgID", tenant)
	}
	if token != "" {
		req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(tenant+":"+token)))
	}

	res := doProbe(target, req)
	if res.authFailure() {
		res.Message = "endpoint rejected auth — token likely missing sigil:write scope"
	}
	return res
}

// defaultProbeOTLP checks the OTLP metrics and traces endpoints, reusing the
// resolved signal URLs and synthesized auth headers from internal/otel. Each
// signal is POSTed an empty JSON body so the edge auth layer is exercised
// without pushing data; 401/403 indicate the token is missing
// metrics:write/traces:write. The real exporter sends protobuf, so an endpoint
// that validates content-type before auth could answer 400/415 here; against
// Grafana's OTLP gateway auth precedes parsing, so 200/401/403 is what's seen.
func defaultProbeOTLP(ctx context.Context) *AnalyticsProbe {
	metrics, traces, ok := otel.ProbeConfig()
	if !ok {
		return nil
	}
	return &AnalyticsProbe{
		Metrics: probeOTLPSignal(ctx, metrics),
		Traces:  probeOTLPSignal(ctx, traces),
	}
}

func probeOTLPSignal(ctx context.Context, target otel.ProbeTarget) *ProbeResult {
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
	if res.authFailure() {
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
