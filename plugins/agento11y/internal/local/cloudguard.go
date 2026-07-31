package local

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/proto/agento11y/wire"
	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/guard"
)

// hookTimeoutHeader carries the calling agent's hook budget to the daemon and
// on to Cloud. It matches the SDK's own header (go/agento11y/hooks.go), which
// is unexported there; this module builds against the released
// github.com/grafana/agento11y/go tag, so the literal is duplicated and kept in
// sync by hand.
const hookTimeoutHeader = "X-Agento11y-Hook-Timeout-Ms"

// legacyHookTimeoutHeader is the pre-rename spelling. Shipped Go hook
// processes still send it: go v0.15.0, which this module is pinned to, has the
// old name baked in. Both spellings are read so a mixed-version machine (new
// daemon, older installed hook binary or plugin) still propagates its deadline.
const legacyHookTimeoutHeader = "X-Sigil-Hook-Timeout-Ms"

// hookTimeoutMargin is shaved off the agent-propagated hook budget before the
// daemon issues its own Cloud call. The agent's HTTP client times out the
// agent->daemon leg at the full budget, so the Cloud call has to return first
// or the agent gives up and never sees the verdict.
const hookTimeoutMargin = 250 * time.Millisecond

// maxHookResponseBytes caps the Cloud hook response the daemon will decode,
// mirroring the SDK's own limit.
const maxHookResponseBytes = 4 << 20

// maxHookFailureDetail caps how much of a Cloud error body ends up in a
// recorded failure. That string is also the deny reason a fail-closed
// evaluation hands the model, so an HTML error page from a proxy must not
// become a multi-hundred-kilobyte tool result. Matches the cap post() applies
// to the same kind of snippet.
const maxHookFailureDetail = 512

// evaluateCloudHook relays one hook request to Cloud and returns the parsed
// verdict. A non-nil error means no verdict was obtained and the caller decides
// what to do with it based on cfg.failOpen.
//
// body is the exact payload the calling agent sent. Relaying it verbatim means
// the daemon cannot drop a field an agent's newer SDK knows about and this one
// does not, and it sidesteps the released SDK client's phase filter: v0.15.0
// defaults HooksConfig.Phases to preflight only, so routing this through the
// SDK client would answer allow without contacting Cloud for exactly the
// postflight tool-call checks this repo's own guard path sends.
//
// Failures are recorded in the forwarding failure ring, so the viewer shows a
// hook leg that looks live but is not delivering.
func (l *forwardLoader) evaluateCloudHook(ctx context.Context, cfg forwardConfig, timeout time.Duration, body []byte) (agento11y.HookEvaluateResponse, error) {
	var out agento11y.HookEvaluateResponse

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.hookURL, bytes.NewReader(body))
	if err != nil {
		return out, l.recordHookFailure("build request for %s: %v", cfg.hookURL, err)
	}
	req.Header.Set("Content-Type", wire.ContentTypeJSON)
	// The marker is what stops a daemon whose ENDPOINT was hand-set to its own
	// address from chaining the relayed copy again.
	req.Header.Set(forwardMarkerHeader, "1")
	req.Header.Set(hookTimeoutHeader, strconv.FormatInt(timeout.Milliseconds(), 10))
	for k, v := range cfg.hookHeaders {
		if v != "" {
			req.Header.Set(k, v)
		}
	}

	// The shared client's 10s timeout is meant for the best-effort background
	// legs; here the caller's budget is the authority, and it can legitimately
	// be longer (a slow llm_judge evaluator). Copying the client keeps the
	// injected transport, which is how tests reach a fake Cloud.
	client := *l.client
	client.Timeout = timeout
	resp, err := client.Do(req)
	if err != nil {
		// A cancelled request context means the agent gave up (or the user
		// interrupted) before Cloud answered. Cloud saw nothing wrong, and the
		// verdict no longer has a reader, so it must not dilute the ring —
		// which is the only place a real fail-open event is reported.
		if isCallerAbort(err) {
			return out, fmt.Errorf("POST %s: %w", cfg.hookURL, err)
		}
		return out, l.recordHookFailure("POST %s: %v", cfg.hookURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxHookResponseBytes+1))
	if err != nil {
		return out, l.recordHookFailure("read response from %s: %v", cfg.hookURL, err)
	}
	if len(respBody) > maxHookResponseBytes {
		return out, l.recordHookFailure("response from %s is larger than %d bytes", cfg.hookURL, maxHookResponseBytes)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return out, l.recordHookFailure("POST %s status %d: %s", cfg.hookURL, resp.StatusCode, hookFailureDetail(respBody, http.StatusText(resp.StatusCode)))
	}
	if err := json.Unmarshal(respBody, &out); err != nil {
		return out, l.recordHookFailure("decode response from %s: %v", cfg.hookURL, err)
	}
	// Mirrors the SDK's own decode: an omitted action is an allow.
	if out.Action == "" {
		out.Action = agento11y.HookActionAllow
	}
	// Cloud omits an empty evaluations list. Answer with the same shape the
	// local verdict uses so the field is an array in every response the daemon
	// serves, whether or not it was chained.
	if out.Evaluations == nil {
		out.Evaluations = []agento11y.HookEvaluation{}
	}

	l.recordSuccess(forwardLabelHooks)
	return out, nil
}

// isCallerAbort reports whether a failed Cloud hook call means the caller went
// away rather than the call going wrong: the agent's context was cancelled
// before Cloud answered. Such an attempt produced no verdict anyone reads, so
// it is kept out of both the failure ring and the fail-open count.
//
// The daemon's own deadline (context.DeadlineExceeded) is not an abort: Cloud
// was too slow, the agent is still waiting, and the resolved fail mode decides
// what it gets.
func isCallerAbort(err error) bool {
	return errors.Is(err, context.Canceled)
}

// hookFailureDetail reduces a Cloud error body to something safe to put in a
// failure record: trimmed, truncated, and replaced by the given fall-back when
// the body is empty.
func hookFailureDetail(body []byte, fallback string) string {
	detail := strings.TrimSpace(string(body))
	if detail == "" {
		return fallback
	}
	if len(detail) > maxHookFailureDetail {
		return detail[:maxHookFailureDetail] + "… (truncated)"
	}
	return detail
}

// recordHookFailure records a failed Cloud hook call in the forwarding failure
// ring and returns it as an error, so one call site both reports the failure to
// the viewer and hands the caller the detail for a fail-closed deny reason.
func (l *forwardLoader) recordHookFailure(format string, args ...any) error {
	l.recordFailuref(forwardLabelHooks, format, args...)
	return fmt.Errorf(format, args...)
}

// hookTimeoutFromHeader resolves the budget for the daemon's Cloud hook call
// from the deadline the calling agent propagated, falling back to the given
// value when no usable header is present. Both header spellings are read, the
// branded one wins, and a margin is shaved off so the Cloud call returns before
// the agent's own hook deadline fires.
//
// fallback is expected to be positive; intFamily guarantees that for the only
// production caller.
func hookTimeoutFromHeader(r *http.Request, fallback time.Duration) time.Duration {
	timeout := fallback
	for _, name := range []string{hookTimeoutHeader, legacyHookTimeoutHeader} {
		ms, err := strconv.Atoi(strings.TrimSpace(r.Header.Get(name)))
		if err == nil && ms > 0 {
			timeout = time.Duration(ms) * time.Millisecond
			break
		}
	}
	// Subtract the margin when the budget can absorb it, otherwise halve it: a
	// tiny budget still has to leave a positive window that returns before the
	// agent->daemon leg expires, and keeping the full value would defeat the
	// margin exactly where it matters most.
	if timeout > hookTimeoutMargin {
		return timeout - hookTimeoutMargin
	}
	return timeout / 2
}

// denyFromCloudError builds the fail-closed deny returned when the Cloud hook
// call failed and GUARDS_FAIL_OPEN is false. It carries
// guard.EvaluationFailureRuleID so every consumer can tell an infrastructure
// failure from a policy decision, and reuses guard.FormatEvalFailure so the
// wording matches the cloud-only path word for word.
func denyFromCloudError(body []byte, err error) agento11y.HookEvaluateResponse {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	return agento11y.HookEvaluateResponse{
		Action:      agento11y.HookActionDeny,
		RuleID:      guard.EvaluationFailureRuleID,
		Reason:      guard.FormatEvalFailure(hookRequestToolName(body), detail),
		Evaluations: []agento11y.HookEvaluation{},
	}
}

// hookRequestToolName returns the name of the first tool call in a hook
// request body (output messages first, then input messages) so the fail-closed
// reason can name the blocked call. The body is decoded leniently and only for
// this: a request the daemon cannot read is still relayed verbatim, and a
// request with no tool call (a pi preflight context event) has no name to give.
func hookRequestToolName(body []byte) string {
	var req hookToolCallProbe
	if err := json.Unmarshal(body, &req); err != nil {
		return unknownToolName
	}
	for _, msgs := range [][]hookProbeMessage{req.Input.Output, req.Input.Messages} {
		for _, m := range msgs {
			for _, p := range m.Parts {
				if name := strings.TrimSpace(p.toolCallName()); name != "" {
					return name
				}
			}
		}
	}
	return unknownToolName
}

// hookToolCallProbe is a lenient view of a hook request: just enough to name a
// tool call. It is deliberately not agento11y.HookEvaluateRequest, which binds
// the Go SDK's `kind` / `tool_call` spelling only. The JS SDK sends
// `type` / `toolCall` (js/src/types.ts), so requests from the pi and opencode
// plugins would decode into zero tool calls and every fail-closed message
// would name the tool "unknown".
type hookToolCallProbe struct {
	Input struct {
		Output   []hookProbeMessage `json:"output"`
		Messages []hookProbeMessage `json:"messages"`
	} `json:"input"`
}

type hookProbeMessage struct {
	Parts []hookProbePart `json:"parts"`
}

// hookProbePart accepts both spellings of the tool-call payload. The two JSON
// tags do not collide: encoding/json's case-insensitive fall-back compares
// "tool_call" and "toolcall", which differ.
type hookProbePart struct {
	ToolCall   *hookProbeToolCall `json:"tool_call"`
	ToolCallJS *hookProbeToolCall `json:"toolCall"`
}

func (p hookProbePart) toolCallName() string {
	if p.ToolCall != nil {
		return p.ToolCall.Name
	}
	if p.ToolCallJS != nil {
		return p.ToolCallJS.Name
	}
	return ""
}

type hookProbeToolCall struct {
	Name string `json:"name"`
}

// unknownToolName stands in when the hook request names no tool call, so the
// fail-closed message reads as a sentence rather than referring to "".
const unknownToolName = "unknown"
