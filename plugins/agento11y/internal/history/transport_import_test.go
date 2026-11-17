package history

import (
	"context"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestOTelImportStopsAfterUnconfirmedBatch(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protocol  agento11y.GenerationExportProtocol
		cancelled bool
		confirmed int
	}{
		{name: "implicit rejection"},
		{name: "implicit cancellation", cancelled: true},
		{name: "implicit confirmed then rejection", confirmed: 100},
		{name: "implicit confirmed then cancellation", cancelled: true, confirmed: 100},
		{name: "explicit rejection", protocol: agento11y.GenerationExportProtocolOTel},
		{name: "explicit cancellation", protocol: agento11y.GenerationExportProtocolOTel, cancelled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			ledger := testLedger(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target, err := ResolveTarget(Target{Endpoint: "http://localhost:12345", Protocol: tc.protocol}, nil)
			require.NoError(t, err)
			recorded, flushes := 0, 0
			failure := errors.New("uncertain collector delivery")
			exporter := &Exporter{
				Target: target,
				Record: func(context.Context, agento11y.GenerationStart, agento11y.Generation, error) error {
					recorded++
					return nil
				},
				Confirm: func(context.Context) error {
					flushes++
					if flushes != tc.confirmed/exportBatchSize+1 {
						return nil
					} // An empty retry would hide the failed batch.
					if tc.cancelled {
						cancel()
						return context.Canceled
					}
					return failure
				},
			}
			var gens []HistoricalGeneration
			for i := range 201 {
				gens = append(gens, turn(fmt.Sprint(i)))
			}
			result, err := RunImport(ctx, ImportOptions{
				Agent: AgentClaudeCode, Sessions: []SessionPreview{sessionAt("/source")},
				Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
					return turnSeq(gens, nil)
				}},
				Exporter: exporter, Ledger: ledger,
				Target: Target{Protocol: agento11y.GenerationExportProtocolHTTP}, // Exporter's snapshot wins.
			})
			if tc.cancelled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.Equal(t, tc.confirmed+100, recorded)
			require.Equal(t, tc.confirmed/exportBatchSize+1, flushes)
			require.Equal(t, tc.confirmed, result.Imported)
			require.Equal(t, 100, result.Failed)
			for i := range 201 {
				id := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/source", TurnID: fmt.Sprint(i)}.Identity()
				entry, ok := ledger.Status(id)
				if i >= tc.confirmed+100 {
					require.False(t, ok, "unattempted turns must remain unmarked")
					continue
				}
				require.True(t, ok)
				if i < tc.confirmed {
					require.Equal(t, StatusExported, entry.Status)
				} else {
					require.Equal(t, StatusFailed, entry.Status)
				}
			}
			recorded, flushes = 0, 0
			exporter.Confirm = func(context.Context) error { flushes++; return nil }
			resumed, err := RunImport(t.Context(), ImportOptions{
				Agent: AgentClaudeCode, Sessions: []SessionPreview{sessionAt("/source")},
				Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
					return turnSeq(gens, nil)
				}},
				Exporter: exporter, Ledger: ledger,
				Target: Target{Protocol: agento11y.GenerationExportProtocolHTTP},
			})
			require.NoError(t, err)
			require.Equal(t, 201-tc.confirmed, resumed.Imported)
			require.Equal(t, tc.confirmed, resumed.Skipped)
			require.Zero(t, resumed.Failed)
			require.Equal(t, resumed.Imported, recorded)
			for i := range 201 {
				id := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/source", TurnID: fmt.Sprint(i)}.Identity()
				entry, ok := ledger.Status(id)
				require.True(t, ok)
				require.Equal(t, StatusExported, entry.Status)
			}
		})
	}
}

func TestOTelImportRetainsConfirmedProgressAfterCollectorRejection(t *testing.T) {
	for _, protocol := range []agento11y.GenerationExportProtocol{"", agento11y.GenerationExportProtocolOTel} {
		for _, failure := range []string{"rejected", "partial"} {
			t.Run(string(protocol)+"/"+failure, func(t *testing.T) {
				envconfig.PinAliasEnvBlank(t)
				t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
				ledger := testLedger(t)
				var mu sync.Mutex
				requests, native := 0, 0
				seen := map[string]int{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/metrics" {
						return
					}
					if r.URL.Path != "/v1/traces" {
						mu.Lock()
						native++
						mu.Unlock()
						t.Errorf("unexpected request %q", r.URL.Path)
						http.NotFound(w, r)
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					var request coltracepb.ExportTraceServiceRequest
					if err := proto.Unmarshal(body, &request); err != nil {
						t.Error(err)
						return
					}
					mu.Lock()
					requests++
					reject := requests == 2
					for _, rs := range request.ResourceSpans {
						for _, ss := range rs.ScopeSpans {
							for _, span := range ss.Spans {
								for _, attr := range span.Attributes {
									if attr.Key == "agento11y.generation.id" {
										seen[attr.Value.GetStringValue()]++
									}
								}
							}
						}
					}
					mu.Unlock()
					if reject {
						if failure == "rejected" {
							http.Error(w, "denied", http.StatusForbidden)
							return
						}
						body, err := proto.Marshal(&coltracepb.ExportTraceServiceResponse{PartialSuccess: &coltracepb.ExportTracePartialSuccess{RejectedSpans: 1, ErrorMessage: "one generation rejected"}})
						if err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "application/x-protobuf")
						_, _ = w.Write(body)
					}
				}))
				defer server.Close()
				input := Target{Endpoint: "https://api.example.invalid", TenantID: "tenant", Protocol: protocol, OTLPEndpoint: server.URL}
				exporter, cleanup, err := NewTargetExporter(t.Context(), input, nil)
				require.NoError(t, err)
				var gens []HistoricalGeneration
				for i := range 201 {
					gens = append(gens, turn(fmt.Sprint(i)))
				}
				opts := ImportOptions{
					Agent: AgentClaudeCode, Sessions: []SessionPreview{sessionAt("/source")},
					Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
						return turnSeq(gens, nil)
					}},
					Exporter: exporter, Ledger: ledger,
					Target: Target{Protocol: agento11y.GenerationExportProtocolHTTP},
				}
				result, err := RunImport(t.Context(), opts)
				require.ErrorContains(t, err, "delivery unconfirmed")
				require.Equal(t, 100, result.Imported)
				require.Equal(t, 100, result.Failed)
				require.Error(t, exporter.Flush(t.Context()), "empty flush must not hide the rejected batch")
				require.Error(t, cleanup(context.Background()))
				mu.Lock()
				gotRequests, gotNative := requests, native
				mu.Unlock()
				require.Equal(t, 2, gotRequests)
				require.Zero(t, gotNative)

				// A new client has no sticky failure, but reuses the same frozen
				// identity and deterministic generation IDs for unconfirmed turns.
				resumed, cleanup, err := NewTargetExporter(t.Context(), exporter.Target, nil)
				require.NoError(t, err)
				defer func() { require.NoError(t, cleanup(context.Background())) }()
				require.Equal(t, exporter.Target.destination(), resumed.Target.destination())
				opts.Exporter = resumed
				result, err = RunImport(t.Context(), opts)
				require.NoError(t, err)
				require.Equal(t, 100, result.Skipped)
				require.Equal(t, 101, result.Imported)
				require.Zero(t, result.Failed)
				mu.Lock()
				defer mu.Unlock()
				require.Equal(t, 4, requests)
				require.Zero(t, native)
				require.Len(t, seen, 201)
				for i := range 201 {
					source := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/source", TurnID: fmt.Sprint(i)}
					want := 1
					if i >= 100 && i < 200 {
						want = 2 // The whole partially confirmed batch must be replayed.
					}
					require.Equal(t, want, seen[source.GenerationID()])
					entry, ok := ledger.Status(source.Identity())
					require.True(t, ok)
					require.Equal(t, StatusExported, entry.Status)
				}
			})
		}
	}
}

func TestImportExporterSnapshotKeepsLedgerAcrossTransportSwitch(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			pinStateHome(t)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
			first := Target{Endpoint: "http://localhost:12345", Protocol: agento11y.GenerationExportProtocolHTTP}
			if !local {
				first.Endpoint, first.TenantID, first.AuthToken = "https://api.example.invalid", "tenant", "token"
			}
			second := first
			second.Protocol, second.AuthToken = "", "rotated-token"
			second.OTLPEndpoint = "https://other-collector.example.invalid"
			if local {
				second.Endpoint, second.OTLPEndpoint = "http://127.0.0.1:54321", "http://127.0.0.1:54321/otlp"
			}
			for i, input := range []Target{first, second} {
				target, err := ResolveTarget(input, nil)
				require.NoError(t, err)
				// Change the environment after construction. Neither the API
				// endpoint nor the tenant here belongs in this exporter's ledger.
				t.Setenv("AGENTO11Y_ENDPOINT", "https://different.example.invalid")
				t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "different-tenant")
				recorded := 0
				exporter := &Exporter{Target: target, Record: func(context.Context, agento11y.GenerationStart, agento11y.Generation, error) error {
					recorded++
					return nil
				}}
				result, err := RunImport(t.Context(), ImportOptions{
					Agent: AgentClaudeCode, Sessions: []SessionPreview{sessionAt("/source")},
					Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
						return turnSeq([]HistoricalGeneration{turn("1")}, nil)
					}},
					Exporter: exporter,
				})
				require.NoError(t, err)
				if i == 0 {
					require.Equal(t, 1, result.Imported)
					require.Equal(t, 1, recorded)
				} else {
					require.Zero(t, result.Imported)
					require.Equal(t, 1, result.Skipped)
					require.Zero(t, recorded)
				}
			}
		})
	}
}
