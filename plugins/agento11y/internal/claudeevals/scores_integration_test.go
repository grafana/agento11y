package claudeevals

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y/experiments"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestExportLifecycleDelayedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protocol  string
		rejection string
		expires   bool
		wantCalls int32
		wantErr   bool
	}{
		{name: "OTel delayed visibility", protocol: "otel", rejection: missingGenerationMessage, wantCalls: 2},
		{name: "OTel other rejection", protocol: "otel", rejection: "trial was not found", wantCalls: 1, wantErr: true},
		{name: "HTTP remains immediate", protocol: "http", rejection: missingGenerationMessage, wantCalls: 1, wantErr: true},
		{name: "caller deadline leaves run open", protocol: "otel", rejection: missingGenerationMessage, expires: true, wantCalls: 1, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			p, err := Parse(bytes.NewReader(fixture(t)), Options{})
			require.NoError(t, err)
			p.IncludeTrajectory = true
			p.Runs = p.Runs[:1]
			p.Runs[0].Trials = p.Runs[0].Trials[:1]
			trial := &p.Runs[0].Trials[0]
			trial.Trajectory = fixtureTrajectory(t)
			require.GreaterOrEqual(t, len(trial.Scores), 2)
			trial.Scores = trial.Scores[:2]
			for i := range trial.Scores {
				trial.Scores[i].GenerationID = trial.Trajectory.Generation.ID
			}
			var generations, scores, updates, finalized atomic.Int32
			var firstPending map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthz" {
					_, _ = io.WriteString(w, `{"capabilities":["otel_generation_ingest_v1"]}`)
					return
				}
				reader := io.Reader(r.Body)
				if r.Header.Get("Content-Encoding") == "gzip" {
					gz, err := gzip.NewReader(reader)
					if !assert.NoError(t, err) {
						return
					}
					defer gz.Close()
					reader = gz
				}
				body, err := io.ReadAll(reader)
				if !assert.NoError(t, err) {
					return
				}
				if strings.HasSuffix(r.URL.Path, "/traces") {
					var request coltracepb.ExportTraceServiceRequest
					if !assert.NoError(t, proto.Unmarshal(body, &request)) {
						return
					}
					for _, resource := range request.ResourceSpans {
						for _, scope := range resource.ScopeSpans {
							for _, span := range scope.Spans {
								for _, attr := range span.Attributes {
									if attr.Key == "agento11y.record" && attr.Value.GetStringValue() == "true" {
										generations.Add(1)
									}
								}
							}
						}
					}
					return
				}
				if strings.HasSuffix(r.URL.Path, "/metrics") {
					return
				}
				w.Header().Set("Content-Type", "application/json")
				var req map[string]any
				if !assert.NoError(t, json.Unmarshal(body, &req)) {
					return
				}
				switch {
				case strings.HasSuffix(r.URL.Path, ":upsert"):
					fmt.Fprintf(w, `{"run":{"experiment_id":%q,"status":"running"}}`, p.Runs[0].Create.RunID)
				case strings.HasSuffix(r.URL.Path, "/generations:export"):
					generations.Add(1)
					fmt.Fprintf(w, `{"results":[{"generation_id":%q,"accepted":true}]}`, trial.Trajectory.Generation.ID)
				case strings.HasSuffix(r.URL.Path, "/scores:export"):
					assert.Equal(t, int32(1), generations.Load(), "trajectory must be confirmed first and never replayed for scores")
					batch := req["scores"].([]any)
					if scores.Add(1) == 1 {
						assert.Len(t, batch, 2)
						firstPending = batch[1].(map[string]any)
						fmt.Fprintf(w, `{"results":[{"score_id":%q,"accepted":true,"status":"accepted"},{"score_id":%q,"accepted":false,"status":"rejected","error":%q}],"accepted":1,"rejected":1}`, trial.Scores[0].ScoreID, trial.Scores[1].ScoreID, tc.rejection)
					} else {
						assert.Len(t, batch, 1)
						assert.Equal(t, firstPending, batch[0])
						fmt.Fprintf(w, `{"results":[{"score_id":%q,"accepted":true,"status":"accepted"}],"accepted":1}`, trial.Scores[1].ScoreID)
					}
				case strings.HasSuffix(r.URL.Path, ":finalize"):
					finalized.Add(1)
					fmt.Fprint(w, `{}`)
				case r.Method == http.MethodPatch:
					updates.Add(1)
					fmt.Fprint(w, `{}`)
				case strings.HasSuffix(r.URL.Path, "/trials"):
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
				}
			}))
			defer server.Close()
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", "true")
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", server.URL)
			t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
			t.Setenv("AGENTO11Y_AUTH_TENANT_ID", "test")
			t.Setenv("AGENTO11Y_AUTH_TOKEN", "test-only")
			telemetry, err := NewTelemetry(t.Context())
			require.NoError(t, err)
			defer func() { _ = telemetry.Shutdown(context.Background()) }()
			client, err := experiments.NewClient(experiments.ClientOptions{Endpoint: server.URL, IngestToken: "test-only", TenantID: "test"})
			require.NoError(t, err)
			defer func() { _ = client.Shutdown(context.Background()) }()
			ctx := t.Context()
			if tc.expires {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond)
				defer cancel()
			}
			err = p.Export(ctx, client, telemetry, io.Discard)
			if tc.wantErr {
				require.Error(t, err)
				assert.Zero(t, finalized.Load())
				assert.Zero(t, updates.Load())
				if tc.expires {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
			} else {
				require.NoError(t, err)
				assert.Equal(t, int32(1), finalized.Load())
				assert.Equal(t, int32(1), updates.Load())
			}
			assert.Equal(t, tc.wantCalls, scores.Load())
			assert.Equal(t, int32(1), generations.Load())
		})
	}
}
