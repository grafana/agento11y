package localhealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
)

const OTelGenerationCapability = "otel_generation_ingest_v1"

// IsDaemonEndpoint identifies the local daemon's /otlp route. A loopback
// collector on the standard /v1 routes does not use the private health contract.
func IsDaemonEndpoint(endpoint string) bool {
	if !envconfig.IsLocalEndpoint(endpoint) {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	switch strings.TrimRight(u.Path, "/") {
	case "/otlp", "/otlp/v1/traces", "/otlp/v1/metrics":
		return true
	default:
		return false
	}
}

// RequireGenerationIngest checks a known daemon, including managed launches
// and local imports. Callers with an arbitrary collector must identify it first.
func RequireGenerationIngest(ctx context.Context, endpoint string) error {
	if !envconfig.IsLocalEndpoint(endpoint) {
		return nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return err
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/healthz", "", "", ""
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("check local OTel generation support: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("check local OTel generation support: HTTP %d", resp.StatusCode)
	}
	var health struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&health); err != nil {
		return fmt.Errorf("decode local capabilities: %w", err)
	}
	if !slices.Contains(health.Capabilities, OTelGenerationCapability) {
		return errors.New("local daemon does not support OTel generation ingest; restart it with the updated agento11y binary before capturing generations")
	}
	return nil
}
