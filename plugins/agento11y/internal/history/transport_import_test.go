package history

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
)

func TestOTelImportStopsAfterUnconfirmedBatch(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancelled=%v", cancelled), func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			ledger := testLedger(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			target, err := ResolveTarget(Target{Endpoint: "http://localhost:12345", Protocol: agento11y.GenerationExportProtocolOTel}, nil)
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
					if flushes > 1 {
						return nil
					} // An empty retry would hide the failed batch.
					if cancelled {
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
			if cancelled {
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, failure)
			}
			require.Equal(t, 100, recorded)
			require.Equal(t, 1, flushes)
			require.Zero(t, result.Imported)
			require.Equal(t, 100, result.Failed)
			for i := range 100 {
				id := SourceRef{Agent: AgentClaudeCode, SessionID: "session-1", SourcePath: "/source", TurnID: fmt.Sprint(i)}.Identity()
				entry, ok := ledger.Status(id)
				require.True(t, ok)
				require.Equal(t, StatusFailed, entry.Status)
			}
		})
	}
}

func TestImportExporterSnapshotKeepsLedgerAcrossTransportSwitch(t *testing.T) {
	for _, local := range []bool{true, false} {
		t.Run(fmt.Sprintf("local=%v", local), func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			pinStateHome(t)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			first := Target{Endpoint: "http://localhost:12345", Protocol: agento11y.GenerationExportProtocolHTTP}
			if !local {
				first.Endpoint, first.TenantID, first.AuthToken = "https://api.example.invalid", "tenant", "token"
			}
			second := first
			second.Protocol, second.AuthToken = agento11y.GenerationExportProtocolOTel, "rotated-token"
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
