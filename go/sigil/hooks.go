package sigil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// HookPhase enumerates evaluation phases.
type HookPhase string

const (
	// HookPhasePreflight runs evaluators before the LLM call.
	HookPhasePreflight HookPhase = "preflight"
	// HookPhasePostflight runs evaluators after the LLM call. Reserved for v1+.
	HookPhasePostflight HookPhase = "postflight"
)

// HookAction is the server's verdict.
type HookAction string

const (
	HookActionAllow HookAction = "allow"
	HookActionDeny  HookAction = "deny"
)

const (
	hooksEvaluatePath      = "/api/v1/hooks:evaluate"
	hookTimeoutHeader      = "X-Sigil-Hook-Timeout-Ms"
	defaultHookTimeout     = 15 * time.Second
	maxHookEvaluateRespLen = 4 << 20
)

// HooksConfig controls synchronous hook evaluation.
type HooksConfig struct {
	// Enabled gates hook evaluation. When false, EvaluateHook returns
	// HookActionAllow without contacting the server.
	Enabled bool
	// Phases the SDK is allowed to evaluate. Defaults to {HookPhasePreflight}.
	// Requests whose phase isn't listed short-circuit to allow.
	Phases []HookPhase
	// Timeout is the per-request HTTP timeout. Defaults to 15s, capped by the
	// server at 120s. The value is also propagated via the
	// X-Sigil-Hook-Timeout-Ms header so the server can scope its evaluator
	// budget accordingly.
	Timeout time.Duration
	// FailOpen returns HookActionAllow on transport / decode failures when
	// non-nil and *FailOpen == true. Set to a pointer to false to surface
	// ErrHookTransportFailed to the caller. nil falls back to the SDK
	// default (true) so guardrail failures never block production traffic
	// unless explicitly opted in.
	FailOpen *bool
}

// FailOpenEnabled reports the effective FailOpen value, honouring the
// pointer-style tri-state.
func (c HooksConfig) FailOpenEnabled() bool {
	if c.FailOpen == nil {
		return true
	}
	return *c.FailOpen
}

func defaultHooksConfig() HooksConfig {
	failOpen := true
	return HooksConfig{
		Enabled:  false,
		Phases:   []HookPhase{HookPhasePreflight},
		Timeout:  defaultHookTimeout,
		FailOpen: &failOpen,
	}
}

func mergeHooksConfig(base, override HooksConfig) HooksConfig {
	// Override.Enabled wins because zero value is the safe default ("hooks
	// off"). Callers explicitly constructing HooksConfig know whether they
	// want hooks on.
	out := HooksConfig{
		Enabled: override.Enabled,
		Phases:  append([]HookPhase(nil), base.Phases...),
		Timeout: base.Timeout,
	}
	if len(override.Phases) > 0 {
		out.Phases = append([]HookPhase(nil), override.Phases...)
	}
	if override.Timeout > 0 {
		out.Timeout = override.Timeout
	}
	if override.FailOpen != nil {
		v := *override.FailOpen
		out.FailOpen = &v
	} else if base.FailOpen != nil {
		v := *base.FailOpen
		out.FailOpen = &v
	}
	return out
}

// HookModel identifies the upstream model for rule matching.
type HookModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

// HookContext is metadata used to match hook rules against this evaluation.
type HookContext struct {
	AgentName    string            `json:"agent_name,omitempty"`
	AgentVersion string            `json:"agent_version,omitempty"`
	Model        *HookModel        `json:"model,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// HookInput carries the evaluable payload (request for preflight,
// request+response for postflight).
type HookInput struct {
	Messages            []Message        `json:"messages,omitempty"`
	Tools               []ToolDefinition `json:"tools,omitempty"`
	SystemPrompt        string           `json:"system_prompt,omitempty"`
	Output              []Message        `json:"output,omitempty"`
	ConversationPreview string           `json:"conversation_preview,omitempty"`
}

// HookEvaluateRequest is the request body sent to /api/v1/hooks:evaluate.
type HookEvaluateRequest struct {
	Phase   HookPhase   `json:"phase"`
	Context HookContext `json:"context"`
	Input   HookInput   `json:"input"`
}

// HookEvaluation is one rule's outcome.
type HookEvaluation struct {
	RuleID        string `json:"rule_id"`
	EvaluatorID   string `json:"evaluator_id"`
	EvaluatorKind string `json:"evaluator_kind"`
	Passed        bool   `json:"passed"`
	LatencyMs     int64  `json:"latency_ms"`
	Explanation   string `json:"explanation,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// HookEvaluateResponse is the parsed server response.
type HookEvaluateResponse struct {
	Action           HookAction       `json:"action"`
	RuleID           string           `json:"rule_id,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	TransformedInput *HookInput       `json:"transformed_input,omitempty"`
	Evaluations      []HookEvaluation `json:"evaluations"`
}

// EvaluateHook calls POST /api/v1/hooks:evaluate to run synchronous hook
// rules for the given request. The returned response distinguishes allow
// (proceed with the LLM call) from deny (block — typically wrapped into a
// HookDeniedError by callers).
//
// Behavior:
//   - When Hooks.Enabled is false, returns {Action: HookActionAllow} without
//     contacting the server.
//   - When the request phase is not in Hooks.Phases, returns
//     {Action: HookActionAllow} without contacting the server.
//   - When Hooks.FailOpen is true (default), transport/decode errors return
//     {Action: HookActionAllow}. When false, ErrHookTransportFailed is
//     returned wrapped in a descriptive error.
//
// EvaluateHook is safe to call on a nil client (returns allow with no error).
func (c *Client) EvaluateHook(ctx context.Context, req HookEvaluateRequest) (*HookEvaluateResponse, error) {
	if c == nil {
		return allowResponse(), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	hooksCfg := c.config.Hooks
	failOpen := hooksCfg.FailOpenEnabled()
	if !hooksCfg.Enabled {
		return allowResponse(), nil
	}
	if !phaseEnabled(hooksCfg.Phases, req.Phase) {
		return allowResponse(), nil
	}

	timeout := hooksCfg.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}

	baseURL, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: %v", ErrHookTransportFailed, err))
	}
	endpoint := strings.TrimRight(baseURL, "/") + hooksEvaluatePath

	payload, err := json.Marshal(req)
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: marshal hook request: %v", ErrHookTransportFailed, err))
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(hookCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: build hook request: %v", ErrHookTransportFailed, err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(hookTimeoutHeader, strconv.FormatInt(timeout.Milliseconds(), 10))
	resolvedHeaders, err := resolveHeadersWithAuth(c.config.GenerationExport.Headers, c.config.GenerationExport.Auth)
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: resolve auth headers: %v", ErrHookTransportFailed, err))
	}
	for key, value := range resolvedHeaders {
		httpReq.Header.Set(key, value)
	}

	httpClient := &http.Client{Timeout: timeout}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: %v", ErrHookTransportFailed, err))
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, int64(maxHookEvaluateRespLen)+1))
	if err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: read hook response: %v", ErrHookTransportFailed, err))
	}
	if len(body) > maxHookEvaluateRespLen {
		return failOpenOrError(failOpen, fmt.Errorf("%w: hook response too large", ErrHookTransportFailed))
	}

	if httpResp.StatusCode != http.StatusOK {
		bodyText := strings.TrimSpace(string(body))
		if bodyText == "" {
			bodyText = http.StatusText(httpResp.StatusCode)
		}
		return failOpenOrError(failOpen, fmt.Errorf("%w: status %d: %s", ErrHookTransportFailed, httpResp.StatusCode, bodyText))
	}

	var out HookEvaluateResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return failOpenOrError(failOpen, fmt.Errorf("%w: decode hook response: %v", ErrHookTransportFailed, err))
	}
	if out.Action == "" {
		out.Action = HookActionAllow
	}
	return &out, nil
}

// HookDeniedFromResponse converts a denied HookEvaluateResponse into a typed
// error. It is a no-op (returns nil) for allow responses or nil input.
func HookDeniedFromResponse(resp *HookEvaluateResponse) error {
	if resp == nil || resp.Action != HookActionDeny {
		return nil
	}
	evaluations := append([]HookEvaluation(nil), resp.Evaluations...)
	return &HookDeniedError{
		RuleID:      resp.RuleID,
		Reason:      resp.Reason,
		Evaluations: evaluations,
	}
}

func allowResponse() *HookEvaluateResponse {
	return &HookEvaluateResponse{Action: HookActionAllow}
}

func failOpenOrError(failOpen bool, err error) (*HookEvaluateResponse, error) {
	if failOpen {
		return allowResponse(), nil
	}
	return nil, err
}

func phaseEnabled(phases []HookPhase, phase HookPhase) bool {
	if len(phases) == 0 {
		return true
	}
	for _, candidate := range phases {
		if candidate == phase {
			return true
		}
	}
	return false
}
