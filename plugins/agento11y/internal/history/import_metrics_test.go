package history

import (
	"context"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/require"
)

// Metric points are stamped when they are exported, so an imported turn that
// ran weeks ago used to land in gen_ai.client.token.usage on the import day:
// one person's first day read about 20x their bill (grafana/sigil#2044). The
// import still sends the turn as a generation and a backdated span.
func TestImportRecordsNoLiveMetrics(t *testing.T) {
	// The OTel generation transport records gen_ai.client.* through its own
	// meter provider, separate from the client's meter.
	for _, protocol := range []agento11y.GenerationExportProtocol{
		agento11y.GenerationExportProtocolHTTP,
		agento11y.GenerationExportProtocolOTel,
	} {
		t.Run(string(protocol), func(t *testing.T) { importRecordsNoLiveMetrics(t, protocol) })
	}
}

func importRecordsNoLiveMetrics(t *testing.T, protocol agento11y.GenerationExportProtocol) {
	envconfig.PinAliasEnvBlank(t)
	pinStateHome(t)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
	var (
		mu          sync.Mutex
		metricBody  strings.Builder
		traceBodies int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
		case "/v1/metrics":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			metricBody.Write(body)
			mu.Unlock()
		case "/v1/traces":
			mu.Lock()
			traceBodies++
			mu.Unlock()
		default:
			_, _ = io.WriteString(w, acceptAllGenerations(r))
		}
	}))
	defer server.Close()

	target := Target{
		Endpoint:     server.URL,
		OTLPEndpoint: server.URL,
		TenantID:     "tenant",
		AuthToken:    "test-token",
		Protocol:     protocol,
	}
	exporter, cleanup, err := NewTargetExporter(t.Context(), target, nil)
	require.NoError(t, err)

	ranAt := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	past := func(g *HistoricalGeneration) {
		g.Gen.StartedAt = ranAt
		g.Gen.CompletedAt = ranAt.Add(5 * time.Second)
		g.Gen.Usage = agento11y.TokenUsage{InputTokens: 1_200, OutputTokens: 300, CacheReadInputTokens: 9_000}
	}
	result, err := RunImport(t.Context(), ImportOptions{
		Agent:    AgentClaudeCode,
		Sessions: []SessionPreview{sessionAt("/source")},
		Importer: &stubImporter{turns: func(context.Context, SessionPreview) iter.Seq2[HistoricalGeneration, error] {
			return turnSeq([]HistoricalGeneration{turn("1", past)}, nil)
		}},
		Exporter: exporter,
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Imported)
	// Shutdown flushes the metric reader, so anything recorded is sent now.
	require.NoError(t, cleanup(context.Background()))

	mu.Lock()
	defer mu.Unlock()
	require.Positive(t, traceBodies, "the import must still send its backdated span over OTLP")
	for _, name := range []string{
		"gen_ai.client.token.usage",
		"gen_ai.client.operation.duration",
		"gen_ai.client.tool_calls_per_operation",
	} {
		require.Falsef(t, strings.Contains(metricBody.String(), name), "the import recorded %s at import time", name)
	}
}
