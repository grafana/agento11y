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

// DefaultEndpoint is the usage-stats receiver, shared with Grafana, Loki,
// Mimir, Tempo, and gcx. EnvEndpoint overrides it.
const DefaultEndpoint = "https://stats.grafana.org/agento11y-usage-report"

// exportTimeout caps the whole request. Tight on purpose: reporting must not
// delay CLI exit, and the payload is one small document.
const exportTimeout = time.Second

// UserAgent identifies usage-stats traffic. Not useragent.For(...), which
// builds the generation-export agent and would conflate the two in any
// receiver-side breakdown by client.
func UserAgent(version string) string {
	if version == "" {
		version = "dev"
	}
	return "agento11y-usage-stats/" + version
}

// Export posts the event as a flat JSON body. The json tags are the wire
// contract; the receiver stamps receipt time, so there is no timestamp.
//
// Never reports failure, and never retries. Unlike gcx, whose client retries
// and so can produce duplicate rows, this drops rows instead — query-side code
// needs no deduplication.
func Export(event Event, version string) {
	export(event, endpoint(), version)
}

// endpoint returns the configured destination.
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
	// Bare client, not a shared helper: a retry transport would burn the whole
	// timeout budget on an unreachable endpoint, synchronously before exit.
	client := &http.Client{Timeout: exportTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return
	}
	// Status ignored: nothing useful to do about it, and we are exiting.
	_ = resp.Body.Close()
}
