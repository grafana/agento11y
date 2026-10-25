package emit

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/envconfig"
	"github.com/grafana/agento11y/plugins/agento11y/internal/otel"
	coltracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

func TestGenerationTransport(t *testing.T) {
	for _, tc := range []struct {
		name, protocol, optIn string
		provider, wantError   bool
		oldDaemon             bool
		collectorStatus       int
		insecure              bool
	}{
		{name: "missing opt-in", protocol: "otel", provider: true, wantError: true},
		{name: "missing provider", protocol: "otel", optIn: "true", wantError: true},
		{name: "invalid protocol", protocol: "unsupported", optIn: "true", provider: true, wantError: true},
		{name: "OTel", protocol: "otel", optIn: "true", provider: true},
		{name: "old local daemon", protocol: "otel", optIn: "true", provider: true, oldDaemon: true, wantError: true},
		{name: "old daemon with effective insecure URL", protocol: "otel", optIn: "true", provider: true, oldDaemon: true, insecure: true, wantError: true},
		{name: "ordinary collector without health route", protocol: "otel", optIn: "true", provider: true, collectorStatus: 404},
		{name: "ordinary collector without private capability", protocol: "otel", optIn: "true", provider: true, collectorStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			envconfig.PinAliasEnvBlank(t)
			t.Setenv("AGENTO11Y_PROTOCOL", tc.protocol)
			t.Setenv("AGENTO11Y_ENABLE_EXPERIMENTAL_FEATURES", tc.optIn)
			var mu sync.Mutex
			var httpGenerations, marked int
			var output, captureMode string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if r.URL.Path == "/healthz" {
					if tc.collectorStatus != 0 {
						w.WriteHeader(tc.collectorStatus)
						_, _ = w.Write([]byte(`{"status":"ok"}`))
						return
					}
					if tc.oldDaemon {
						_, _ = w.Write([]byte(`{"status":"ok"}`))
						return
					}
					_, _ = w.Write([]byte(`{"capabilities":["otel_generation_ingest_v1"]}`))
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/traces") {
					if strings.Contains(r.URL.Path, "generations") {
						httpGenerations++
					}
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					return
				}
				var req coltracepb.ExportTraceServiceRequest
				if err := proto.Unmarshal(body, &req); err != nil {
					t.Error(err)
					return
				}
				for _, rs := range req.ResourceSpans {
					for _, ss := range rs.ScopeSpans {
						for _, s := range ss.Spans {
							for _, a := range s.Attributes {
								switch a.Key {
								case "agento11y.record":
									if a.Value.GetStringValue() == "true" {
										marked++
									}
								case "gen_ai.output.messages":
									output = a.Value.GetStringValue()
								case "agento11y.generation.metadata":
									var metadata map[string]any
									if err := json.Unmarshal([]byte(a.Value.GetStringValue()), &metadata); err != nil {
										t.Error(err)
									}
									captureMode, _ = metadata["agento11y.sdk.content_capture_mode"].(string)
								}
							}
						}
					}
				}
			}))
			defer server.Close()
			t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
			endpoint := server.URL + "/otlp"
			if tc.collectorStatus != 0 {
				endpoint = server.URL
			}
			if tc.insecure {
				endpoint = strings.Replace(endpoint, "http://", "https://", 1)
				t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_INSECURE", "true")
			}
			t.Setenv("AGENTO11Y_OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)
			var providers *otel.Providers
			if tc.provider && tc.optIn == "true" && tc.protocol == "otel" {
				var err error
				providers, err = otel.SetupWithOptions(t.Context(), "test", otel.Options{Headers: map[string]string{}})
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = providers.Shutdown(context.Background()) }()
			}
			client, err := NewClient(ClientOptions{Providers: providers, ContentCapture: agento11y.ContentCaptureModeFullWithMetadataSpans})
			if tc.wantError {
				if err == nil {
					_ = client.Shutdown(t.Context())
					t.Fatal("expected construction error")
				}
				if tc.oldDaemon && !strings.Contains(err.Error(), "restart") {
					t.Fatalf("missing restart instruction: %v", err)
				}
				mu.Lock()
				defer mu.Unlock()
				if marked != 0 || httpGenerations != 0 {
					t.Fatalf("sent generations after construction failure: spans=%d HTTP=%d", marked, httpGenerations)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = client.Shutdown(context.Background()) }()
			if err := Record(t.Context(), client, agento11y.GenerationStart{ID: "gen-otel", ConversationID: "conv-otel", Model: agento11y.ModelRef{Provider: "test", Name: "model"}}, agento11y.Generation{Output: []agento11y.Message{agento11y.AssistantTextMessage("retained content")}, CompletedAt: time.Now()}, nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := client.Flush(t.Context()); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			if marked != 1 || httpGenerations != 0 {
				t.Fatalf("marked=%d HTTP generations=%d", marked, httpGenerations)
			}
			if !strings.Contains(output, "retained content") || captureMode != "full" {
				t.Fatalf("output=%q captureMode=%q", output, captureMode)
			}
		})
	}
}
