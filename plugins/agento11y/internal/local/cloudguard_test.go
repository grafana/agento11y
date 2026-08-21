package local

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

type cancelReadBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *cancelReadBody) Read(_ []byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (*cancelReadBody) Close() error { return nil }

// hookLoader builds a forward loader for the fake Cloud endpoint.
// Relay-level tests assemble the resolved config directly.
func hookLoader(t *testing.T, f *hookCloud) (*forwardLoader, forwardConfig) {
	t.Helper()
	clearForwardEnv(t)
	l := newForwardLoader(writeConfigEnvFile(t, nil), nil)
	l.client = f.srv.Client()
	cfg := forwardConfig{
		enabled:     true,
		hookURL:     f.srv.URL + hookEvaluatePath,
		hookHeaders: generationForwardHeaders("tenant-1", "token-1"),
		failOpen:    true,
		timeoutMs:   1500,
	}
	return l, cfg
}

// TestEvaluateCloudHook covers the verdicts and failure modes of the relay.
// The caller applies cfg.failOpen; this level only reports whether a verdict
// was obtained.
func TestEvaluateCloudHook(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		respond       string
		delay         time.Duration
		closeCloud    bool
		timeout       time.Duration // defaults to a second
		abortMidCall  bool          // cancel the caller's context while Cloud stalls
		wantAction    agento11y.HookAction
		wantRuleID    string
		wantReason    string
		wantFailure   string // substring of the returned error; "" means a verdict was obtained
		assertMore    func(t *testing.T, resp agento11y.HookEvaluateResponse)
		assertFailure func(t *testing.T, err error, failures []forwardFailure)
	}{
		{
			name:       "allow",
			respond:    `{"action":"allow","evaluations":[]}`,
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "deny_keeps_rule_and_reason",
			respond:    `{"action":"deny","rule_id":"r1","reason":"blocked by policy"}`,
			wantAction: agento11y.HookActionDeny,
			wantRuleID: "r1",
			wantReason: "blocked by policy",
		},
		{
			name:       "missing_action_is_allow",
			respond:    `{}`,
			wantAction: agento11y.HookActionAllow,
		},
		{
			name:       "transform_survives_the_relay",
			respond:    `{"action":"allow","transformed_input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"bash","input_json":{"command":"echo safe"}}}]}]}}`,
			wantAction: agento11y.HookActionAllow,
			assertMore: func(t *testing.T, resp agento11y.HookEvaluateResponse) {
				require.NotNil(t, resp.TransformedInput)
				require.Len(t, resp.TransformedInput.Output, 1)
				parts := resp.TransformedInput.Output[0].Parts
				require.Len(t, parts, 1)
				require.NotNil(t, parts[0].ToolCall)
				assert.JSONEq(t, `{"command":"echo safe"}`, string(parts[0].ToolCall.InputJSON))
			},
		},
		{
			name:       "evaluations_survive_in_order",
			respond:    `{"action":"allow","evaluations":[{"rule_id":"r1","passed":true},{"rule_id":"r2","passed":false}]}`,
			wantAction: agento11y.HookActionAllow,
			assertMore: func(t *testing.T, resp agento11y.HookEvaluateResponse) {
				require.Len(t, resp.Evaluations, 2)
				assert.Equal(t, "r1", resp.Evaluations[0].RuleID)
				assert.Equal(t, "r2", resp.Evaluations[1].RuleID)
			},
		},
		{
			name:        "non_2xx",
			status:      http.StatusServiceUnavailable,
			respond:     `{"error":"upstream down"}`,
			wantFailure: "status 503",
		},
		{
			name:        "bad_json",
			respond:     `{"action":`,
			wantFailure: "decode response",
		},
		{
			// A runaway Cloud response is a failure the caller's fail mode
			// handles, not something the daemon buffers without limit.
			name:        "oversized_response",
			respond:     fmt.Sprintf(`{"action":"allow","reason":%q}`, strings.Repeat("x", maxHookResponseBytes)),
			timeout:     5 * time.Second,
			wantFailure: "larger than",
		},
		{
			// A Cloud call that outlasts the budget is a failure, not a hang,
			// so the caller can apply its fail mode before the agent's own
			// hook deadline fires.
			name:        "timeout",
			respond:     `{"action":"allow"}`,
			delay:       300 * time.Millisecond,
			timeout:     30 * time.Millisecond,
			wantFailure: "context deadline exceeded",
		},
		{
			// An unreachable Cloud lands in the ring the viewer reads, which
			// is the only channel a user without debug logging has.
			name:        "transport_failure",
			closeCloud:  true,
			wantFailure: "POST",
		},
		{
			// The Cloud error detail becomes the deny reason a fail-closed
			// evaluation hands the model, so an HTML error page from a proxy
			// must not arrive as a multi-hundred-kilobyte tool result. The
			// viewer renders the same string.
			name:        "oversized_error_body_is_truncated",
			status:      http.StatusBadGateway,
			respond:     strings.Repeat("<html>proxy is unhappy</html>", 4000),
			wantFailure: "truncated",
			assertFailure: func(t *testing.T, err error, failures []forwardFailure) {
				assert.Less(t, len(err.Error()), maxHookFailureDetail+256)
				assert.Less(t, len(failures[0].Detail), maxHookFailureDetail+256)
			},
		},
		{
			// The one error that must stay out of the ring: the agent gave up
			// (or the user interrupted) before Cloud answered, so nothing was
			// delivered but Cloud saw no problem either. Recording it would
			// dilute the only channel that reports real fail-open events.
			name:         "caller_abort_is_not_a_failure",
			delay:        time.Second,
			timeout:      time.Minute,
			abortMidCall: true,
			wantFailure:  context.Canceled.Error(),
			assertFailure: func(t *testing.T, err error, _ []forwardFailure) {
				require.ErrorIs(t, err, context.Canceled)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newHookCloud(t)
			if tc.status != 0 {
				f.status = tc.status
			}
			if tc.respond != "" {
				f.respond = tc.respond
			}
			f.delay = tc.delay
			l, cfg := hookLoader(t, f)
			if tc.closeCloud {
				f.srv.Close()
			}
			timeout := tc.timeout
			if timeout == 0 {
				timeout = time.Second
			}

			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			if tc.abortMidCall {
				go func() {
					time.Sleep(20 * time.Millisecond)
					cancel()
				}()
			}
			resp, err := l.evaluateCloudHook(ctx, cfg, timeout, []byte(`{"phase":"postflight"}`))

			st := l.status()
			failures := st.Failures
			if tc.wantFailure == "" {
				require.NoError(t, err)
				assert.Equal(t, 1, f.count())
				assert.Empty(t, failures)
				assert.Equal(t, tc.wantAction, resp.Action)
				assert.Equal(t, tc.wantRuleID, resp.RuleID)
				assert.Equal(t, tc.wantReason, resp.Reason)
				assert.NotNil(t, resp.Evaluations, "the response shape stays stable for consumers that index it")
				if tc.assertMore != nil {
					tc.assertMore(t, resp)
				}
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantFailure)
			if tc.abortMidCall {
				assert.Empty(t, failures, "a caller-side abort is not a Cloud delivery failure")
				assert.Nil(t, st.Legs, "an abandoned verdict changes no leg record")
				assert.Zero(t, st.HookFailOpens, "an abandoned verdict is not a fail-open allow")
			} else {
				require.Len(t, failures, 1)
				assert.Equal(t, forwardLabelHooks, failures[0].Label)
				assert.Contains(t, failures[0].Detail, tc.wantFailure)
			}
			if tc.assertFailure != nil {
				tc.assertFailure(t, err, failures)
			}
		})
	}
}

func TestEvaluateCloudHook_CallerAbortWhileReadingBodyIsNotFailure(t *testing.T) {
	clearForwardEnv(t)
	l := newForwardLoader(writeConfigEnvFile(t, nil), nil)
	started := make(chan struct{})
	l.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        make(http.Header),
			Body:          &cancelReadBody{ctx: r.Context(), started: started},
			ContentLength: -1,
			Request:       r,
		}, nil
	})}
	cfg := forwardConfig{
		enabled:     true,
		hookURL:     "https://cloud.example.test" + hookEvaluatePath,
		hookHeaders: generationForwardHeaders("tenant-1", "token-1"),
		failOpen:    true,
		timeoutMs:   1500,
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, err := l.evaluateCloudHook(ctx, cfg, time.Minute, []byte(`{"phase":"postflight"}`))
	require.ErrorIs(t, err, context.Canceled)
	st := l.status()
	assert.Empty(t, st.Failures)
	assert.Nil(t, st.Legs)
	assert.Zero(t, st.HookFailOpens)
}

// TestEvaluateCloudHook_SuccessClearsFailures covers the ring's semantics for
// the hook leg: a delivered evaluation means the leg is healthy now.
func TestEvaluateCloudHook_SuccessClearsFailures(t *testing.T) {
	f := newHookCloud(t)
	f.status = http.StatusInternalServerError
	l, cfg := hookLoader(t, f)

	_, err := l.evaluateCloudHook(t.Context(), cfg, time.Second, []byte(`{}`))
	require.Error(t, err)
	failed := l.status()
	require.Len(t, failed.Failures, 1)
	failedLeg := failed.Legs[forwardLabelHooks]
	assert.Equal(t, failed.Failures[0].At, failedLeg.LastFailureAt)
	assert.Equal(t, failed.Failures[0].Detail, failedLeg.LastFailureDetail)

	f.status = http.StatusOK
	_, err = l.evaluateCloudHook(t.Context(), cfg, time.Second, []byte(`{}`))
	require.NoError(t, err)
	recovered := l.status()
	assert.Empty(t, recovered.Failures)
	hooksLeg := recovered.Legs[forwardLabelHooks]
	assert.NotEmpty(t, hooksLeg.LastSuccessAt)
	assert.Equal(t, failedLeg.LastFailureAt, hooksLeg.LastFailureAt)
	assert.Equal(t, failedLeg.LastFailureDetail, hooksLeg.LastFailureDetail)
}

// TestHookTimeoutFromHeader covers the budget the daemon gives its Cloud call.
// Two header spellings are in the wild: shipped Go hook processes send the
// legacy one, JS plugins send the branded one.
func TestHookTimeoutFromHeader(t *testing.T) {
	const fallback = 1500 * time.Millisecond
	cases := []struct {
		name    string
		headers map[string]string
		want    time.Duration
	}{
		{name: "branded_header", headers: map[string]string{hookTimeoutHeader: "5000"}, want: 4750 * time.Millisecond},
		{name: "legacy_header", headers: map[string]string{legacyHookTimeoutHeader: "5000"}, want: 4750 * time.Millisecond},
		{
			name:    "branded_wins_over_legacy",
			headers: map[string]string{hookTimeoutHeader: "5000", legacyHookTimeoutHeader: "9000"},
			want:    4750 * time.Millisecond,
		},
		// The margin applies to the fallback too: GUARDS_TIMEOUT_MS is the same
		// knob the agent uses for its own hook deadline, so the daemon still
		// has to return first.
		{name: "no_header_uses_fallback", want: fallback - hookTimeoutMargin},
		{name: "budget_smaller_than_margin_is_halved", headers: map[string]string{hookTimeoutHeader: "100"}, want: 50 * time.Millisecond},
		{name: "budget_equal_to_margin_is_halved", headers: map[string]string{hookTimeoutHeader: "250"}, want: 125 * time.Millisecond},
		{name: "non_numeric_falls_back", headers: map[string]string{hookTimeoutHeader: "soon"}, want: fallback - hookTimeoutMargin},
		{name: "zero_falls_back", headers: map[string]string{hookTimeoutHeader: "0"}, want: fallback - hookTimeoutMargin},
		{name: "negative_falls_back", headers: map[string]string{hookTimeoutHeader: "-100"}, want: fallback - hookTimeoutMargin},
		{
			// An unusable branded value must not shadow a usable legacy one.
			name:    "invalid_branded_falls_through_to_legacy",
			headers: map[string]string{hookTimeoutHeader: "soon", legacyHookTimeoutHeader: "5000"},
			want:    4750 * time.Millisecond,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, hookEvaluatePath, nil)
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			assert.Equal(t, tc.want, hookTimeoutFromHeader(r, fallback))
			assert.Positive(t, hookTimeoutFromHeader(r, fallback), "a non-positive budget would fail the call instantly")
		})
	}
}

// TestDenyFromCloudError covers the fail-closed response. Its rule ID is the
// marker every consumer checks, and its wording has to say the guard could not
// be evaluated rather than that a policy blocked the call.
func TestDenyFromCloudError(t *testing.T) {
	const detail = "POST https://cloud.example.test/api/v1/hooks:evaluate: connection refused"
	cases := []struct {
		name       string
		body       string
		wantReason string
		wantNoSub  string
	}{
		{
			name:       "tool_call_names_the_tool",
			body:       `{"phase":"postflight","input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"id":"c1","name":"Bash"}}]}]}}`,
			wantReason: guard.FormatEvalFailure("Bash", detail),
		},
		{
			// Prompt errors must not invent a tool call or address the model.
			name:       "preflight_message_gets_the_message_wording",
			body:       `{"phase":"preflight","input":{"messages":[{"role":"user","parts":[{"kind":"text","text":"hello"}]}]}}`,
			wantReason: guard.FormatPromptEvalFailure(detail),
			wantNoSub:  "tool call",
		},
		{
			name:       "undecodable_body_gets_the_message_wording",
			body:       `{"phase":`,
			wantReason: guard.FormatPromptEvalFailure(detail),
			wantNoSub:  "tool call",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := denyFromCloudError([]byte(tc.body), errors.New(detail))

			assert.Equal(t, agento11y.HookActionDeny, resp.Action)
			assert.Equal(t, guard.EvaluationFailureRuleID, resp.RuleID)
			assert.Equal(t, tc.wantReason, resp.Reason)
			assert.Contains(t, resp.Reason, "could not evaluate")
			assert.NotContains(t, resp.Reason, "policy blocked")
			if tc.wantNoSub != "" {
				assert.NotContains(t, resp.Reason, tc.wantNoSub)
			}
			assert.NotNil(t, resp.Evaluations)
		})
	}
}

// TestHookRequestToolName covers naming the blocked call in the fail-closed
// message across the request shapes the daemon receives. The Go and JS SDKs
// serialize a tool-call part differently, and the JS shape is what the pi and
// opencode plugins send.
func TestHookRequestToolName(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "go_sdk_output_tool_call",
			body: `{"input":{"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Bash"}}]}]}}`,
			want: "Bash",
		},
		{
			name: "js_sdk_output_tool_call",
			body: `{"input":{"output":[{"role":"assistant","parts":[{"type":"tool_call","toolCall":{"id":"c1","name":"bash","inputJSON":"{}"}}]}]}}`,
			want: "bash",
		},
		{
			name: "falls_back_to_input_messages",
			body: `{"input":{"messages":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Read"}}]}]}}`,
			want: "Read",
		},
		{
			name: "output_wins_over_messages",
			body: `{"input":{"messages":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Read"}}]}],"output":[{"role":"assistant","parts":[{"kind":"tool_call","tool_call":{"name":"Bash"}}]}]}}`,
			want: "Bash",
		},
		{
			name: "skips_parts_without_a_tool_call",
			body: `{"input":{"output":[{"role":"assistant","parts":[{"type":"text","text":"running it"},{"type":"tool_call","toolCall":{"name":"Edit"}}]}]}}`,
			want: "Edit",
		},
		{name: "preflight_without_tool_call", body: `{"phase":"preflight","input":{"messages":[]}}`},
		{name: "undecodable_body", body: `{"phase":`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, found := hookRequestToolName([]byte(tc.body))
			assert.Equal(t, tc.want != "", found)
			assert.Equal(t, tc.want, name)
		})
	}
}
