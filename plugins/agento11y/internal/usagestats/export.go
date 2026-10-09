package usagestats

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// DefaultEndpoint is the usage-stats receiver — the same service that receives
// reports from Grafana, Loki, Mimir, Tempo, and gcx. EnvEndpoint overrides it.
const DefaultEndpoint = "https://stats.grafana.org/agento11y-usage-report"

// exportTimeout caps the whole request. Reporting must never noticeably delay
// CLI exit, so this is deliberately tight: the payload is one small JSON
// document and the receiver answers before doing any work.
const exportTimeout = time.Second

// UserAgent identifies usage-stats traffic. Deliberately not
// useragent.For(...): that builds the generation-export User-Agent for a named
// agent plugin, and reusing it here would file CLI usage reports alongside
// captured session data in any receiver-side breakdown by client.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "agento11y-usage-stats/" + version
}

// Export posts the event to the usage-stats receiver as a flat JSON body. The
// Event json tags are the wire contract, pinned by TestEventFieldInventory and
// by the receiver's own tests; the receiver stamps receipt time itself, so the
// payload carries no timestamp.
//
// Export never reports failure. Reporting is fire-and-forget and must not
// affect the command's outcome: a lost event is fine, a CLI that fails because
// a stats endpoint was unreachable is not. The export is a single attempt with
// no retries, so an unreachable endpoint costs one fast failure rather than
// the full timeout.
//
// Note the asymmetry with gcx, whose client retries and therefore documents
// that rare duplicate rows are possible. This one does not retry, so the
// failure mode is the opposite: rows are dropped, never duplicated. Receiver
// and query-side code should not assume deduplication is needed here.
func Export(event Event, version string) {
	export(event, endpoint(), version)
}

// endpoint returns the configured destination. The override is read from the
// shell only; see EnvEndpoint for why it is not an alias family.
func endpoint() string {
	if override := strings.TrimSpace(os.Getenv(EnvEndpoint)); override != "" {
		return override
	}
	return DefaultEndpoint
}

func export(event Event, url, version string) {
	body, err := json.Marshal(event)
	if err != nil {
		return
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", UserAgent(version))
	// Deliberately a bare client rather than any shared helper with a retry
	// transport: a retrying transport would burn the whole timeout budget on
	// an unreachable endpoint, and this runs synchronously before exit.
	client := &http.Client{Timeout: exportTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	// The status code is ignored on purpose. There is nothing this process can
	// usefully do about a rejected event, and it is about to exit.
	_ = resp.Body.Close()
}
