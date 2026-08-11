package agento11y

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestEnvEndpointReachesHookServer posts a real hook evaluation for each way a
// caller can build a Config. NewClient(DefaultConfig()) is the pattern
// go/README.md teaches, and it used to shadow AGENTO11Y_ENDPOINT: the request
// went to localhost:8080, the transport failed, and fail-open turned a deny
// into an allow with nothing in the response to say so.
func TestEnvEndpointReachesHookServer(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{name: "empty config", cfg: Config{}},
		{name: "unmodified DefaultConfig", cfg: DefaultConfig()},
		{name: "config carrying the schema-default API endpoint", cfg: Config{API: APIConfig{Endpoint: defaultAPIEndpoint}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var hits atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				hits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"action":"deny","reason":"blocked"}`))
			}))
			defer server.Close()

			t.Setenv("AGENTO11Y_ENDPOINT", server.URL)
			t.Setenv("AGENTO11Y_PROTOCOL", "none")
			t.Setenv("AGENTO11Y_HOOKS_ENABLED", "true")

			client := NewClient(tc.cfg)
			defer func() { _ = client.Shutdown(context.Background()) }()

			res, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{
				Phase: HookPhasePreflight,
				Context: HookContext{
					AgentName: "planner",
					Model:     &HookModel{Provider: "openai", Name: "gpt-5"},
				},
			})
			if err != nil {
				t.Fatalf("EvaluateHook: %v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("server hits = %d, want 1: the request went somewhere else", hits.Load())
			}
			if res.Action != HookActionDeny {
				t.Errorf("Action = %q, want deny", res.Action)
			}
		})
	}
}
