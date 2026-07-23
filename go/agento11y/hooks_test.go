package agento11y

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace/noop"
)

func TestEvaluateHookDisabledShortCircuits(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called when hooks disabled")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{apiEndpoint: server.URL})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{
		Phase:   HookPhasePreflight,
		Context: HookContext{Model: &HookModel{Provider: "openai", Name: "gpt-4o"}},
	})
	if err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}
	if resp == nil || resp.Action != HookActionAllow {
		t.Fatalf("expected allow response, got %#v", resp)
	}
}

func TestEvaluateHookSendsRequestAndParsesAllow(t *testing.T) {
	var capturedPath string
	var capturedHeaders http.Header
	var capturedBody HookEvaluateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		capturedPath = req.URL.Path
		capturedHeaders = req.Header.Clone()
		body, _ := io.ReadAll(req.Body)
		if err := json.Unmarshal(body, &capturedBody); err != nil {
			t.Errorf("decode body: %v", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"action":"allow",
			"evaluations":[
				{"rule_id":"pii","evaluator_id":"ev-pii","evaluator_kind":"regex","passed":true,"latency_ms":12,"explanation":"no match"}
			]
		}`))
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{
		Phase: HookPhasePreflight,
		Context: HookContext{
			AgentName:    "agent-a",
			AgentVersion: "1.0.0",
			Model:        &HookModel{Provider: "openai", Name: "gpt-4o"},
			Tags:         map[string]string{"env": "test"},
		},
		Input: HookInput{
			SystemPrompt: "be helpful",
			Messages: []Message{
				{Role: RoleUser, Parts: []Part{TextPart("hello")}},
			},
		},
	})
	if err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}

	if capturedPath != "/api/v1/hooks:evaluate" {
		t.Fatalf("unexpected path: %s", capturedPath)
	}
	if capturedHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("missing content-type header")
	}
	if capturedHeaders.Get("X-Sigil-Hook-Timeout-Ms") == "" {
		t.Fatalf("missing timeout header")
	}
	if capturedBody.Phase != HookPhasePreflight {
		t.Fatalf("unexpected phase: %q", capturedBody.Phase)
	}
	if capturedBody.Context.AgentName != "agent-a" || capturedBody.Context.Model == nil || capturedBody.Context.Model.Name != "gpt-4o" {
		t.Fatalf("unexpected context: %#v", capturedBody.Context)
	}
	if capturedBody.Input.SystemPrompt != "be helpful" || len(capturedBody.Input.Messages) != 1 {
		t.Fatalf("unexpected input: %#v", capturedBody.Input)
	}

	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
	if len(resp.Evaluations) != 1 || resp.Evaluations[0].RuleID != "pii" {
		t.Fatalf("unexpected evaluations: %#v", resp.Evaluations)
	}
}

func TestEvaluateHookParsesTransformedInput(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"action":"allow",
			"transformed_input":{"conversation_preview":"[REDACTED]"},
			"evaluations":[]
		}`))
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{
		Phase:   HookPhasePreflight,
		Context: HookContext{Model: &HookModel{Provider: "openai", Name: "gpt-4o"}},
	})
	if err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}
	if resp.TransformedInput == nil || resp.TransformedInput.ConversationPreview != "[REDACTED]" {
		t.Fatalf("unexpected transformed input: %#v", resp.TransformedInput)
	}
}

func TestEvaluateHookReturnsDeny(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"action":"deny",
			"rule_id":"rule-block",
			"reason":"matched secret",
			"evaluations":[
				{"rule_id":"rule-block","evaluator_id":"ev-secret","evaluator_kind":"regex","passed":false,"latency_ms":5,"reason":"secret detected"}
			]
		}`))
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{
		Phase: HookPhasePreflight,
	})
	if err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}
	if resp.Action != HookActionDeny {
		t.Fatalf("expected deny, got %q", resp.Action)
	}
	denyErr := HookDeniedFromResponse(resp)
	if denyErr == nil {
		t.Fatalf("expected HookDeniedError, got nil")
	}
	if !errors.Is(denyErr, ErrHookDenied) {
		t.Fatalf("expected ErrHookDenied via Is, got %v", denyErr)
	}
	var typed *HookDeniedError
	if !errors.As(denyErr, &typed) {
		t.Fatalf("expected *HookDeniedError, got %T", denyErr)
	}
	if typed.RuleID != "rule-block" || typed.Reason != "matched secret" {
		t.Fatalf("unexpected fields: %#v", typed)
	}
	if len(typed.Evaluations) != 1 || typed.Evaluations[0].RuleID != "rule-block" {
		t.Fatalf("unexpected evaluations: %#v", typed.Evaluations)
	}
}

func TestEvaluateHookFailsOpenOnHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{Phase: HookPhasePreflight})
	if err != nil {
		t.Fatalf("expected fail-open allow, got error: %v", err)
	}
	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
}

func TestEvaluateHookFailsClosedWhenConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	failClosed := false
	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
		failOpen:     &failClosed,
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	_, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{Phase: HookPhasePreflight})
	if !errors.Is(err, ErrHookTransportFailed) {
		t.Fatalf("expected ErrHookTransportFailed, got %v", err)
	}
	if !strings.Contains(err.Error(), "boom") && !strings.Contains(err.Error(), "500") {
		t.Fatalf("expected error to include status detail, got %v", err)
	}
}

func TestEvaluateHookSkipsPhaseNotConfigured(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Errorf("server should not be called when phase mismatched")
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := newHookTestClient(t, hookTestClientOptions{
		apiEndpoint:  server.URL,
		hooksEnabled: true,
		phases:       []HookPhase{HookPhasePostflight},
	})
	t.Cleanup(func() { _ = client.Shutdown(context.Background()) })

	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{Phase: HookPhasePreflight})
	if err != nil {
		t.Fatalf("evaluate hook: %v", err)
	}
	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
}

func TestEvaluateHookNilClientReturnsAllow(t *testing.T) {
	var client *Client
	resp, err := client.EvaluateHook(context.Background(), HookEvaluateRequest{Phase: HookPhasePreflight})
	if err != nil {
		t.Fatalf("nil client: %v", err)
	}
	if resp.Action != HookActionAllow {
		t.Fatalf("expected allow, got %q", resp.Action)
	}
}

func TestHookDeniedErrorMessageFormat(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  *HookDeniedError
		want string
	}{
		{
			name: "with rule and reason",
			err:  &HookDeniedError{RuleID: "rule-1", Reason: "blocked"},
			want: "agento11y hook denied by rule rule-1: blocked",
		},
		{
			name: "without rule",
			err:  &HookDeniedError{Reason: "blocked"},
			want: "agento11y hook denied: blocked",
		},
		{
			name: "without reason",
			err:  &HookDeniedError{RuleID: "rule-1"},
			want: "agento11y hook denied by rule rule-1: request blocked by Agent Observability hook rule",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.Error(); got != tc.want {
				t.Fatalf("Error() = %q, want %q", got, tc.want)
			}
			if !errors.Is(tc.err, ErrHookDenied) {
				t.Fatalf("expected errors.Is(err, ErrHookDenied) to be true")
			}
		})
	}
}

func TestMergeHooksConfigPreservesDefaults(t *testing.T) {
	merged := mergeHooksConfig(defaultHooksConfig(), HooksConfig{})
	if !merged.FailOpenEnabled() {
		t.Fatalf("expected FailOpen default true, got false")
	}
	if merged.Timeout != defaultHookTimeout {
		t.Fatalf("expected default timeout, got %s", merged.Timeout)
	}
	if len(merged.Phases) != 1 || merged.Phases[0] != HookPhasePreflight {
		t.Fatalf("expected default phases, got %#v", merged.Phases)
	}
	if merged.Enabled {
		t.Fatalf("expected hooks disabled by default")
	}
}

func TestMergeHooksConfigOverridesFields(t *testing.T) {
	failClosed := false
	custom := HooksConfig{
		Enabled:  true,
		Phases:   []HookPhase{HookPhasePostflight},
		Timeout:  3 * time.Second,
		FailOpen: &failClosed,
	}
	merged := mergeHooksConfig(defaultHooksConfig(), custom)
	if !merged.Enabled {
		t.Fatalf("expected Enabled=true after merge")
	}
	if len(merged.Phases) != 1 || merged.Phases[0] != HookPhasePostflight {
		t.Fatalf("phases not overridden: %#v", merged.Phases)
	}
	if merged.Timeout != 3*time.Second {
		t.Fatalf("timeout not overridden: %s", merged.Timeout)
	}
	if merged.FailOpenEnabled() {
		t.Fatalf("expected fail-closed after override")
	}
}

type hookTestClientOptions struct {
	apiEndpoint  string
	hooksEnabled bool
	phases       []HookPhase
	failOpen     *bool
}

func newHookTestClient(t *testing.T, options hookTestClientOptions) *Client {
	t.Helper()
	return NewClient(Config{
		Tracer: noop.NewTracerProvider().Tracer("agento11y-go-hooks-test"),
		GenerationExport: GenerationExportConfig{
			Protocol:        GenerationExportProtocolHTTP,
			Endpoint:        options.apiEndpoint + "/api/v1/generations:export",
			Insecure:        BoolPtr(true),
			BatchSize:       1,
			FlushInterval:   time.Hour,
			QueueSize:       1,
			MaxRetries:      1,
			InitialBackoff:  time.Millisecond,
			MaxBackoff:      time.Millisecond,
			PayloadMaxBytes: 1 << 20,
		},
		API: APIConfig{Endpoint: options.apiEndpoint},
		Hooks: HooksConfig{
			Enabled:  options.hooksEnabled,
			Phases:   options.phases,
			FailOpen: options.failOpen,
		},
		testGenerationExporter: newNoopGenerationExporter(nil),
		testDisableWorker:      true,
	})
}
