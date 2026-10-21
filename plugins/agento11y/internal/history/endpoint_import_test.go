package history

import (
	"context"
	"io"
	"iter"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestImportEquivalentEndpointSpellings(t *testing.T) {
	for _, prefix := range []string{"", "/sigil"} {
		for _, fullURL := range []bool{false, true} {
			name := prefix + "/base-url"
			if fullURL {
				name = prefix + "/full-url"
			}
			t.Run(name, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				pinStateHome(t)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
				var generationRequests, traceRequests atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/healthz":
						_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					case "/v1/traces":
						traceRequests.Add(1)
					case "/v1/metrics":
					default:
						assert.Equal(t, prefix+"/api/v1/generations:export", r.URL.Path)
						generationRequests.Add(1)
						_, _ = io.WriteString(w, acceptAllGenerations(r))
					}
				}))
				defer server.Close()

				// Keep Cloud ledger selection while routing every request to this
				// loopback receiver. No DNS lookup or external connection is made.
				previous := http.DefaultTransport
				transport := previous.(*http.Transport).Clone()
				transport.Proxy = nil
				transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
					return new(net.Dialer).DialContext(ctx, network, server.Listener.Addr().String())
				}
				http.DefaultTransport = transport
				defer func() {
					transport.CloseIdleConnections()
					http.DefaultTransport = previous
				}()

				base := "http://api.example.invalid" + prefix
				if fullURL {
					base += "/api/v1/generations:export"
				}
				first, second := base, base+"//"
				var destination string
				run := func(endpoint string, protocol agento11y.GenerationExportProtocol, force bool, imported, skipped int) {
					t.Helper()
					target := Target{Endpoint: endpoint, TenantID: "tenant", AuthToken: "test-token", Protocol: protocol}
					if protocol == agento11y.GenerationExportProtocolOTel {
						target.OTLPEndpoint = server.URL
					}
					exporter, cleanup, err := NewTargetExporter(t.Context(), target, nil)
					require.NoError(t, err)
					defer func() { require.NoError(t, cleanup(context.Background())) }()
					result, err := RunImport(t.Context(), ImportOptions{
						Agent: AgentClaudeCode, Sessions: []SessionPreview{sessionAt("/source")}, Force: force,
						Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
							return turnSeq([]HistoricalGeneration{turn("1")}, nil)
						}},
						Exporter: exporter,
					})
					require.NoError(t, err)
					require.Equal(t, imported, result.Imported)
					require.Equal(t, skipped, result.Skipped)
					require.Zero(t, result.Failed)
					key := exporter.Target.destination()
					require.True(t, strings.HasPrefix(key, "cloud-"))
					if destination == "" {
						destination = key
					}
					require.Equal(t, destination, key)
				}
				run(first, agento11y.GenerationExportProtocolHTTP, false, 1, 0)
				require.EqualValues(t, 1, generationRequests.Load())
				run(second, agento11y.GenerationExportProtocolHTTP, false, 0, 1)
				require.EqualValues(t, 1, generationRequests.Load())
				run(second, agento11y.GenerationExportProtocolOTel, false, 0, 1)
				require.Zero(t, traceRequests.Load())
				require.EqualValues(t, 1, generationRequests.Load())
				run(second, agento11y.GenerationExportProtocolHTTP, true, 1, 0)
				require.EqualValues(t, 2, generationRequests.Load())
			})
		}
	}
}
