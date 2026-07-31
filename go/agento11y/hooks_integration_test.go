package agento11y

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

// Guard behavior of the Go SDK against a real local HTTP server. The other hook
// tests decode the captured body back into the SDK's own request type, so an
// encoding the server cannot read round-trips cleanly and looks identical to a
// correct one. These cases run the full transport and compare each captured body
// with conformance/hooks/request-preflight.json instead.

// hookServerResponse is what the responder returns for one hooks:evaluate
// request: the per-request responder model of the JS plugins/pi/src/testHttp.ts
// helper, whose status, body, and delay cover allow, deny, transport failure,
// timeout, and malformed-response cases from one server.
type hookServerResponse struct {
	status int
	body   string
	delay  time.Duration
}

type hookServerRequest struct {
	path    string
	headers http.Header
	raw     string
	payload any
}

type hookTestServer struct {
	*httptest.Server
	mu          sync.Mutex
	requests    []hookServerRequest
	inFlight    int
	maxInFlight int
}

func startHookTestServer(t *testing.T, respond func(hookServerRequest) hookServerResponse) *hookTestServer {
	t.Helper()
	server := &hookTestServer{}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		call := hookServerRequest{
			path:    req.URL.Path,
			headers: req.Header.Clone(),
			raw:     string(raw),
		}
		_ = json.Unmarshal(raw, &call.payload)

		server.mu.Lock()
		server.requests = append(server.requests, call)
		server.inFlight++
		if server.inFlight > server.maxInFlight {
			server.maxInFlight = server.inFlight
		}
		server.mu.Unlock()
		defer func() {
			server.mu.Lock()
			server.inFlight--
			server.mu.Unlock()
		}()

		out := respond(call)
		if out.delay > 0 {
			time.Sleep(out.delay)
		}
		status := out.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		// The client aborts on timeout, so a delayed write can fail. That is
		// the case under test.
		_, _ = w.Write([]byte(out.body))
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *hookTestServer) calls() []hookServerRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]hookServerRequest(nil), s.requests...)
}

func (s *hookTestServer) concurrentPeak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFlight
}

func respondWith(status int, body string) func(hookServerRequest) hookServerResponse {
	return func(hookServerRequest) hookServerResponse {
		return hookServerResponse{status: status, body: body}
	}
}

type hookIntegrationOptions struct {
	enabled  bool
	failOpen bool
	phases   []HookPhase
	timeout  time.Duration
	auth     AuthConfig
}

func newHookIntegrationClient(t *testing.T, endpoint string, options hookIntegrationOptions) (*Client, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	phases := options.phases
	if phases == nil {
		phases = []HookPhase{HookPhasePreflight}
	}
	timeout := options.timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	failOpen := options.failOpen
	client := NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-hooks-integration"),
		Logger: log.New(logs, "", 0),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        endpoint + "/api/v1/generations:export",
			Insecure:        BoolPtr(true),
			Auth:            options.auth,
			BatchSize:       1,
			FlushInterval:   time.Hour,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		API: APIConfig{Endpoint: endpoint},
		Hooks: HooksConfig{
			Enabled:  options.enabled,
			Phases:   phases,
			Timeout:  timeout,
			FailOpen: &failOpen,
		},
		testGenerationExporter: newNoopGenerationExporter(nil),
		testDisableWorker:      true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })
	return client, logs
}

func hookFixtureResponseBody(t *testing.T, name string) string {
	t.Helper()
	return string(loadHookResponseFixture(t, name))
}

// assertPreflightHookRequest checks the body the SDK actually put on the wire.
func assertPreflightHookRequest(t *testing.T, server *hookTestServer) {
	t.Helper()
	calls := server.calls()
	if len(calls) != 1 {
		t.Fatalf("expected exactly one hook request, got %d", len(calls))
	}
	call := calls[0]
	if call.path != hooksEvaluatePath {
		t.Errorf("unexpected path %q", call.path)
	}
	if got := call.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("unexpected content type %q", got)
	}
	for _, diff := range diffJSON("", stripEmptyHookMetadata(call.payload), loadHookFixture(t, "request-preflight.json")) {
		t.Errorf("captured request body does not match the shared fixture: %s", diff)
	}
}

func assertFailOpenWarning(t *testing.T, logs *bytes.Buffer, wantSubstring string) {
	t.Helper()
	text := logs.String()
	if !strings.Contains(text, "allowing request (fail_open)") {
		t.Errorf("a swallowed hook failure must be logged, got %q", text)
	}
	if wantSubstring != "" && !strings.Contains(text, wantSubstring) {
		t.Errorf("warning does not mention %q, got %q", wantSubstring, text)
	}
}

func TestHooksIntegrationAllowUnderBothPolicies(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		t.Run(hookPolicyName(failOpen), func(t *testing.T) {
			server := startHookTestServer(t, respondWith(http.StatusOK, hookFixtureResponseBody(t, "allow")))
			client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true, failOpen: failOpen})

			resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
			if err != nil {
				t.Fatalf("evaluate hook: %v", err)
			}
			if resp.Action != HookActionAllow {
				t.Fatalf("expected allow, got %q", resp.Action)
			}
			if HookDeniedFromResponse(resp) != nil {
				t.Error("allow response must not produce a denial")
			}
			if len(resp.Evaluations) != 1 || resp.Evaluations[0].RuleID != "pii-detect" {
				t.Errorf("unexpected evaluations: %#v", resp.Evaluations)
			}
			assertPreflightHookRequest(t, server)
		})
	}
}

func TestHooksIntegrationDenyUnderBothPolicies(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		t.Run(hookPolicyName(failOpen), func(t *testing.T) {
			server := startHookTestServer(t, respondWith(http.StatusOK, hookFixtureResponseBody(t, "deny")))
			client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true, failOpen: failOpen})

			resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
			if err != nil {
				t.Fatalf("evaluate hook: %v", err)
			}
			if resp.Action != HookActionDeny {
				t.Fatalf("expected deny, got %q", resp.Action)
			}
			denied := HookDeniedFromResponse(resp)
			if denied == nil {
				t.Fatal("deny response must produce a denial")
			}
			var typed *HookDeniedError
			if !errors.As(denied, &typed) {
				t.Fatalf("unexpected error type %T", denied)
			}
			if typed.RuleID != "block-destructive-bash" {
				t.Errorf("unexpected rule id %q", typed.RuleID)
			}
			if typed.Reason != "Bash(*rm*) is not allowed in this environment" {
				t.Errorf("unexpected reason %q", typed.Reason)
			}
			assertPreflightHookRequest(t, server)
		})
	}
}

func TestHooksIntegrationHTTPErrorStatuses(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			t.Run("fail-open", func(t *testing.T) {
				server := startHookTestServer(t, respondWith(status, "upstream unavailable"))
				client, logs := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true, failOpen: true})

				resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
				if err != nil {
					t.Fatalf("fail-open must not surface an error: %v", err)
				}
				if resp.Action != HookActionAllow {
					t.Fatalf("expected allow, got %q", resp.Action)
				}
				assertFailOpenWarning(t, logs, fmt.Sprintf("status %d", status))
				assertPreflightHookRequest(t, server)
			})
			t.Run("fail-closed", func(t *testing.T) {
				server := startHookTestServer(t, respondWith(status, "upstream unavailable"))
				client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true})

				_, err := client.EvaluateHook(context.Background(), preflightHookRequest())
				if !errors.Is(err, ErrHookTransportFailed) {
					t.Fatalf("expected a transport error, got %v", err)
				}
				assertPreflightHookRequest(t, server)
			})
		})
	}
}

func TestHooksIntegrationMalformedResponseBody(t *testing.T) {
	const malformed = `{"action": "allow"`

	t.Run("fail-open", func(t *testing.T) {
		server := startHookTestServer(t, respondWith(http.StatusOK, malformed))
		client, logs := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true, failOpen: true})

		resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
		if err != nil {
			t.Fatalf("fail-open must not surface an error: %v", err)
		}
		if resp.Action != HookActionAllow {
			t.Fatalf("expected allow, got %q", resp.Action)
		}
		assertFailOpenWarning(t, logs, "decode hook response")
		assertPreflightHookRequest(t, server)
	})

	t.Run("fail-closed", func(t *testing.T) {
		server := startHookTestServer(t, respondWith(http.StatusOK, malformed))
		client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true})

		_, err := client.EvaluateHook(context.Background(), preflightHookRequest())
		if !errors.Is(err, ErrHookTransportFailed) {
			t.Fatalf("expected a transport error, got %v", err)
		}
		assertPreflightHookRequest(t, server)
	})
}

func TestHooksIntegrationClientTimeout(t *testing.T) {
	const serverDelay = 2 * time.Second
	slow := func(hookServerRequest) hookServerResponse {
		return hookServerResponse{status: http.StatusOK, body: `{"action":"deny","rule_id":"late"}`, delay: serverDelay}
	}

	t.Run("fail-open", func(t *testing.T) {
		server := startHookTestServer(t, slow)
		client, logs := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{
			enabled:  true,
			failOpen: true,
			timeout:  250 * time.Millisecond,
		})

		started := time.Now()
		resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
		elapsed := time.Since(started)
		if err != nil {
			t.Fatalf("fail-open must not surface an error: %v", err)
		}
		if resp.Action != HookActionAllow {
			t.Fatalf("expected allow, got %q", resp.Action)
		}
		if elapsed >= serverDelay {
			t.Errorf("client waited %s, so it did not enforce its own timeout", elapsed)
		}
		assertFailOpenWarning(t, logs, "")
		assertPreflightHookRequest(t, server)
	})

	t.Run("fail-closed", func(t *testing.T) {
		server := startHookTestServer(t, slow)
		client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{
			enabled: true,
			timeout: 250 * time.Millisecond,
		})

		started := time.Now()
		_, err := client.EvaluateHook(context.Background(), preflightHookRequest())
		elapsed := time.Since(started)
		if !errors.Is(err, ErrHookTransportFailed) {
			t.Fatalf("expected a transport error, got %v", err)
		}
		if elapsed >= serverDelay {
			t.Errorf("client waited %s, so it did not enforce its own timeout", elapsed)
		}
		assertPreflightHookRequest(t, server)
	})
}

func TestHooksIntegrationUnconfiguredPhaseSendsNoRequest(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		t.Run(hookPolicyName(failOpen), func(t *testing.T) {
			server := startHookTestServer(t, respondWith(http.StatusInternalServerError, "should not be called"))
			client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{
				enabled:  true,
				failOpen: failOpen,
				phases:   []HookPhase{HookPhasePostflight},
			})

			resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
			if err != nil {
				t.Fatalf("evaluate hook: %v", err)
			}
			if resp.Action != HookActionAllow {
				t.Fatalf("expected allow, got %q", resp.Action)
			}
			if calls := server.calls(); len(calls) != 0 {
				t.Errorf("expected no hook request, got %d", len(calls))
			}
		})
	}
}

func TestHooksIntegrationDisabledHooksSendNoRequest(t *testing.T) {
	for _, failOpen := range []bool{true, false} {
		t.Run(hookPolicyName(failOpen), func(t *testing.T) {
			server := startHookTestServer(t, respondWith(http.StatusInternalServerError, "should not be called"))
			client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{failOpen: failOpen})

			resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
			if err != nil {
				t.Fatalf("evaluate hook: %v", err)
			}
			if resp.Action != HookActionAllow {
				t.Fatalf("expected allow, got %q", resp.Action)
			}
			if calls := server.calls(); len(calls) != 0 {
				t.Errorf("expected no hook request, got %d", len(calls))
			}
		})
	}
}

func TestHooksIntegrationConfiguredAuthReachesServer(t *testing.T) {
	server := startHookTestServer(t, respondWith(http.StatusOK, hookFixtureResponseBody(t, "allow")))
	client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{
		enabled: true,
		auth: AuthConfig{
			Mode:          ExportAuthModeBasic,
			BasicUser:     "12345",
			BasicPassword: "glc-token",
			TenantID:      "12345",
		},
	})

	if _, err := client.EvaluateHook(context.Background(), preflightHookRequest()); err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}

	calls := server.calls()
	if len(calls) != 1 {
		t.Fatalf("expected one hook request, got %d", len(calls))
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("12345:glc-token"))
	if got := calls[0].headers.Get("Authorization"); got != want {
		t.Errorf("unexpected authorization header %q", got)
	}
	if got := calls[0].headers.Get("X-Scope-OrgID"); got != "12345" {
		t.Errorf("unexpected tenant header %q", got)
	}
	if got := calls[0].headers.Get(hookTimeoutHeader); got != "5000" {
		t.Errorf("unexpected hook timeout header %q", got)
	}
	assertPreflightHookRequest(t, server)
}

func TestHooksIntegrationConcurrentEvaluations(t *testing.T) {
	// A guard on the request path must not funnel every caller through one
	// connection.
	server := startHookTestServer(t, func(hookServerRequest) hookServerResponse {
		return hookServerResponse{
			status: http.StatusOK,
			body:   hookFixtureResponseBody(t, "allow"),
			delay:  200 * time.Millisecond,
		}
	})
	client, _ := newHookIntegrationClient(t, server.URL, hookIntegrationOptions{enabled: true})

	var wg sync.WaitGroup
	errs := make([]error, 4)
	actions := make([]HookAction, 4)
	for i := range errs {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			resp, err := client.EvaluateHook(context.Background(), preflightHookRequest())
			errs[index] = err
			if resp != nil {
				actions[index] = resp.Action
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("evaluation %d failed: %v", i, err)
		}
		if actions[i] != HookActionAllow {
			t.Errorf("evaluation %d: expected allow, got %q", i, actions[i])
		}
	}
	if peak := server.concurrentPeak(); peak < 2 {
		t.Errorf("evaluations were serialized: peak in-flight %d", peak)
	}
	fixture := loadHookFixture(t, "request-preflight.json")
	for i, call := range server.calls() {
		for _, diff := range diffJSON("", stripEmptyHookMetadata(call.payload), fixture) {
			t.Errorf("request %d does not match the shared fixture: %s", i, diff)
		}
	}
}

func hookPolicyName(failOpen bool) string {
	if failOpen {
		return "fail-open"
	}
	return "fail-closed"
}
