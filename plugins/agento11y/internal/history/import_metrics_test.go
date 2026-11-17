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
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// Metric points are stamped when they are exported, so an imported turn that
// ran weeks ago used to land in gen_ai.client.token.usage on the import day:
// one person's first day read about 20x their bill (grafana/sigil#2044). The
// import still sends the turn as a generation and a backdated span.
func TestImportRecordsNoLiveMetrics(t *testing.T) {
	// The OTel generation transport records gen_ai.client.* through its own
	// meter provider, separate from the client's meter.
	for _, protocol := range []agento11y.GenerationExportProtocol{
		"",
		agento11y.GenerationExportProtocolHTTP,
		agento11y.GenerationExportProtocolOTel,
	} {
		t.Run(string(protocol), func(t *testing.T) { importRecordsNoLiveMetrics(t, protocol) })
	}
}

func importRecordsNoLiveMetrics(t *testing.T, protocol agento11y.GenerationExportProtocol) {
	envconfig.PinAliasEnvBlank(t)
	pinStateHome(t)
	t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "false")
	var (
		mu         sync.Mutex
		metricBody strings.Builder
		spans      []*tracepb.Span
		native     int
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
			for _, rs := range request.ResourceSpans {
				for _, ss := range rs.ScopeSpans {
					spans = append(spans, ss.Spans...)
				}
			}
			mu.Unlock()
		case "/api/v1/generations:export":
			mu.Lock()
			native++
			mu.Unlock()
			_, _ = io.WriteString(w, acceptAllGenerations(r))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.NotFound(w, r)
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
	require.Len(t, spans, 1, "the import must still send its backdated span over OTLP")
	require.Equal(t, uint64(ranAt.UnixNano()), spans[0].StartTimeUnixNano)
	require.Equal(t, uint64(ranAt.Add(5*time.Second).UnixNano()), spans[0].EndTimeUnixNano)
	if protocol == agento11y.GenerationExportProtocolHTTP {
		require.Equal(t, 1, native)
	} else {
		require.Zero(t, native, "OTel history must not duplicate generations over HTTP")
	}
	for _, name := range []string{
		"gen_ai.client.token.usage",
		"gen_ai.client.operation.duration",
		"gen_ai.client.tool_calls_per_operation",
	} {
		require.Falsef(t, strings.Contains(metricBody.String(), name), "the import recorded %s at import time", name)
	}
}
