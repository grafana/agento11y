package agento11y

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"go.opentelemetry.io/otel/trace"
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
	hookTimeoutHeader      = "X-Agento11y-Hook-Timeout-Ms"
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
	// Timeout is the per-request HTTP timeout, also sent as
	// X-Agento11y-Hook-Timeout-Ms so the server can scope its evaluator budget.
	// Defaults to 15s. The server honours 1..119999 ms and falls back to its own
	// budget for anything else.
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
		Phases:  cloneHookPhases(base.Phases),
		Timeout: base.Timeout,
	}
	if len(override.Phases) > 0 {
		out.Phases = cloneHookPhases(override.Phases)
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

func cloneHookPhases(in []HookPhase) []HookPhase {
	if len(in) == 0 {
		return nil
	}
	out := make([]HookPhase, len(in))
	for i := range in {
		out[i] = HookPhase(strings.Clone(string(in[i])))
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
	AgentName      string            `json:"agent_name,omitempty"`
	AgentVersion   string            `json:"agent_version,omitempty"`
	Model          *HookModel        `json:"model,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	ConversationID string            `json:"conversation_id,omitempty"`
	TraceID        string            `json:"trace_id,omitempty"`
	SpanID         string            `json:"span_id,omitempty"`
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

// The hooks:evaluate endpoint has no generated stubs on either side: the server
// decodes the request with a hand-rolled decoder and encodes its response from
// protobuf structs with encoding/json. The two directions disagree on how JSON
// payloads are carried, and neither convention matches the public SDK types. The
// wire types below own that translation so HookInput, model.Message, and
// model.ToolDefinition stay shared with generation export.
// conformance/hooks/README.md documents the contract. The fixtures under
// conformance/hooks/ pin it.

type hookWireRequest struct {
	Phase   HookPhase            `json:"phase"`
	Context HookContext          `json:"context"`
	Input   hookWireRequestInput `json:"input"`
}

// hookWireRequestInput keeps model.Message for parts, whose snake_case,
// embedded-JSON shape the server already reads, and swaps tool definitions for
// hookWireTool.
type hookWireRequestInput struct {
	Messages            []Message      `json:"messages,omitempty"`
	Tools               []hookWireTool `json:"tools,omitempty"`
	SystemPrompt        string         `json:"system_prompt,omitempty"`
	Output              []Message      `json:"output,omitempty"`
	ConversationPreview string         `json:"conversation_preview,omitempty"`
}

// hookWireTool carries request tool definitions the way the server reads them.
// The server decodes input.tools straight into its protobuf ToolDefinition with
// encoding/json, where the bytes field input_schema_json must be base64.
//
// model.ToolDefinition puts raw JSON under input_schema instead. The server
// ignores that unknown key, so every tool schema disappears from the request.
// Raw JSON under input_schema_json fails the decode, and the server answers 400
// for the whole evaluation.
type hookWireTool struct {
	Name            string `json:"name"`
	Description     string `json:"description,omitempty"`
	Type            string `json:"type,omitempty"`
	InputSchemaJSON []byte `json:"input_schema_json,omitempty"`
	Deferred        bool   `json:"deferred,omitempty"`
}

// hookWireResponseTool decodes response tool definitions. input_schema_json is
// json.RawMessage rather than []byte on purpose. A []byte field rejects anything
// that is not base64, and that error fails json.Unmarshal of the whole response
// body. The caller then loses a deny it has to enforce and falls open to allow.
// Each payload is decoded on its own instead, matching Python and JS.
type hookWireResponseTool struct {
	Name            string          `json:"name"`
	Description     string          `json:"description,omitempty"`
	Type            string          `json:"type,omitempty"`
	InputSchemaJSON json.RawMessage `json:"input_schema_json,omitempty"`
	Deferred        bool            `json:"deferred,omitempty"`
}

type hookWireResponse struct {
	Action           HookAction       `json:"action"`
	RuleID           string           `json:"rule_id,omitempty"`
	Reason           string           `json:"reason,omitempty"`
	TransformedInput *hookWireInput   `json:"transformed_input,omitempty"`
	Evaluations      []HookEvaluation `json:"evaluations"`
}

type hookWireInput struct {
	Messages            []hookWireMessage      `json:"messages,omitempty"`
	Tools               []hookWireResponseTool `json:"tools,omitempty"`
	SystemPrompt        string                 `json:"system_prompt,omitempty"`
	Output              []hookWireMessage      `json:"output,omitempty"`
	ConversationPreview string                 `json:"conversation_preview,omitempty"`
}

type hookWireMessage struct {
	Role  hookWireRole   `json:"role"`
	Name  string         `json:"name,omitempty"`
	Parts []hookWirePart `json:"parts,omitempty"`
}

type hookWirePart struct {
	Kind       PartKind            `json:"kind"`
	Text       string              `json:"text,omitempty"`
	Thinking   string              `json:"thinking,omitempty"`
	ToolCall   *hookWireToolCall   `json:"tool_call,omitempty"`
	ToolResult *hookWireToolResult `json:"tool_result,omitempty"`
}

type hookWireToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	InputJSON json.RawMessage `json:"input_json,omitempty"`
}

type hookWireToolResult struct {
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
	Content     string          `json:"content,omitempty"`
	ContentJSON json.RawMessage `json:"content_json,omitempty"`
}

// hookWireRole accepts both the string roles the hooks API emits and the integer
// enum values an emitter that marshals the generated proto structs with
// encoding/json would send. Python and JS accept the same two forms.
type hookWireRole Role

func (r *hookWireRole) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case string(RoleAssistant):
			*r = hookWireRole(RoleAssistant)
		case string(RoleTool):
			*r = hookWireRole(RoleTool)
		default:
			*r = hookWireRole(RoleUser)
		}
		return nil
	}
	var enum int32
	if err := json.Unmarshal(data, &enum); err != nil {
		return fmt.Errorf("cannot unmarshal hook message role %s", string(data))
	}
	switch enum {
	case int32(agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT):
		*r = hookWireRole(RoleAssistant)
	case int32(agento11yv1.MessageRole_MESSAGE_ROLE_TOOL):
		*r = hookWireRole(RoleTool)
	default:
		*r = hookWireRole(RoleUser)
	}
	return nil
}

func newHookWireRequest(req HookEvaluateRequest) hookWireRequest {
	return hookWireRequest{
		Phase:   req.Phase,
		Context: req.Context,
		Input: hookWireRequestInput{
			Messages:            newHookWireRequestMessages(req.Input.Messages),
			Tools:               newHookWireTools(req.Input.Tools),
			SystemPrompt:        req.Input.SystemPrompt,
			Output:              newHookWireRequestMessages(req.Input.Output),
			ConversationPreview: req.Input.ConversationPreview,
		},
	}
}

// newHookWireRequestMessages copies messages into the subset of model.Message
// the server can read. It makes three adjustments. Python already makes all
// three, and the narrower JS part union produces the same result:
//
//   - It drops a part that has no payload for its kind, and a part whose kind
//     the server does not know. A part with no known kind but with text is the
//     exception: that one travels as a text part. A media part therefore
//     disappears rather than reaching the server's default branch, which
//     recovers text only and decodes it to a part with no payload.
//   - Parts is never nil, so a message with no parts serializes as [] rather
//     than null.
//   - A tool payload that is not valid JSON travels as a JSON string. Streaming
//     providers accumulate input_json_delta without validating it, and
//     json.Marshal of an invalid json.RawMessage fails the whole request. The
//     caller would then see a fail-open allow or a fail-closed error for what is
//     only a payload problem.
func newHookWireRequestMessages(msgs []Message) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		converted := msg
		converted.Parts = make([]Part, 0, len(msg.Parts))
		for _, part := range msg.Parts {
			switch part.Kind {
			case PartKindText:
				if part.Text != "" {
					converted.Parts = append(converted.Parts, part)
				}
			case PartKindThinking:
				if part.Thinking != "" {
					converted.Parts = append(converted.Parts, part)
				}
			case PartKindToolCall:
				if part.ToolCall == nil {
					continue
				}
				call := *part.ToolCall
				call.InputJSON = hookRequestPayloadJSON(call.InputJSON)
				part.ToolCall = &call
				converted.Parts = append(converted.Parts, part)
			case PartKindToolResult:
				if part.ToolResult == nil {
					continue
				}
				result := *part.ToolResult
				result.ContentJSON = hookRequestPayloadJSON(result.ContentJSON)
				part.ToolResult = &result
				converted.Parts = append(converted.Parts, part)
			default:
				if part.Text != "" {
					converted.Parts = append(converted.Parts, Part{
						Kind:     PartKindText,
						Text:     part.Text,
						Metadata: part.Metadata,
					})
				}
			}
		}
		out = append(out, converted)
	}
	return out
}

// hookRequestPayloadJSON keeps a request-side payload valid JSON. Request parts
// carry embedded JSON, so an unparsable payload is sent as a JSON string, which
// leaves its text visible to rules instead of failing the marshal.
func hookRequestPayloadJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	if json.Valid(raw) {
		return raw
	}
	return hookJSONString(string(raw))
}

func newHookWireTools(tools []ToolDefinition) []hookWireTool {
	if len(tools) == 0 {
		return nil
	}
	out := make([]hookWireTool, 0, len(tools))
	for _, tool := range tools {
		out = append(out, hookWireTool{
			Name:            tool.Name,
			Description:     tool.Description,
			Type:            tool.Type,
			InputSchemaJSON: tool.InputSchema,
			Deferred:        tool.Deferred,
		})
	}
	return out
}

func (w hookWireResponse) toResponse() HookEvaluateResponse {
	out := HookEvaluateResponse{
		Action:      w.Action,
		RuleID:      w.RuleID,
		Reason:      w.Reason,
		Evaluations: w.Evaluations,
	}
	if w.TransformedInput != nil {
		out.TransformedInput = w.TransformedInput.toHookInput()
	}
	return out
}

func (w *hookWireInput) toHookInput() *HookInput {
	if w == nil {
		return nil
	}
	out := &HookInput{
		Messages:            hookWireMessagesToMessages(w.Messages),
		Tools:               hookWireToolsToToolDefinitions(w.Tools),
		SystemPrompt:        w.SystemPrompt,
		Output:              hookWireMessagesToMessages(w.Output),
		ConversationPreview: w.ConversationPreview,
	}
	if len(out.Messages) == 0 && len(out.Tools) == 0 && len(out.Output) == 0 &&
		out.SystemPrompt == "" && out.ConversationPreview == "" {
		// An empty transformed_input is not a transform. Returning a non-nil
		// HookInput here would let a caller checking only for nil replace the
		// prompt with nothing. Python and JS return no transform for this body.
		return nil
	}
	return out
}

func hookWireToolsToToolDefinitions(tools []hookWireResponseTool) []ToolDefinition {
	if len(tools) == 0 {
		return nil
	}
	out := make([]ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		converted := ToolDefinition{
			Name:        tool.Name,
			Description: tool.Description,
			Type:        tool.Type,
			Deferred:    tool.Deferred,
		}
		if schema := decodeHookWireJSON(tool.InputSchemaJSON); len(schema) > 0 {
			converted.InputSchema = schema
		}
		out = append(out, converted)
	}
	return out
}

func hookWireMessagesToMessages(msgs []hookWireMessage) []Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]Message, 0, len(msgs))
	for _, msg := range msgs {
		converted := Message{
			Role:  Role(msg.Role),
			Name:  msg.Name,
			Parts: make([]Part, 0, len(msg.Parts)),
		}
		if converted.Role == "" {
			converted.Role = RoleUser
		}
		for _, part := range msg.Parts {
			if parsed, ok := part.toPart(); ok {
				converted.Parts = append(converted.Parts, parsed)
			}
		}
		out = append(out, converted)
	}
	return out
}

// toPart converts one response part, reporting false for a part with nothing to
// recover so it is dropped rather than turned into an empty text part. Python
// and JS drop the same shape.
func (p hookWirePart) toPart() (Part, bool) {
	kind := p.Kind
	if kind == "" {
		// A part without a discriminator is still recoverable from whichever
		// payload field is set. The server always sets kind, so this only covers a
		// hand-written or proto-JSON body.
		switch {
		case p.ToolCall != nil:
			kind = PartKindToolCall
		case p.ToolResult != nil:
			kind = PartKindToolResult
		case p.Thinking != "":
			kind = PartKindThinking
		case p.Text != "":
			kind = PartKindText
		default:
			return Part{}, false
		}
	}
	switch kind {
	case PartKindThinking:
		if p.Thinking == "" {
			return Part{}, false
		}
		return Part{Kind: PartKindThinking, Thinking: p.Thinking}, true
	case PartKindToolCall:
		// A call without a name cannot be routed or re-sent, so there is nothing to
		// recover from it either.
		if p.ToolCall == nil || p.ToolCall.Name == "" {
			return Part{}, false
		}
		return Part{Kind: PartKindToolCall, ToolCall: &ToolCall{
			ID:        p.ToolCall.ID,
			Name:      p.ToolCall.Name,
			InputJSON: decodeHookWireJSON(p.ToolCall.InputJSON),
		}}, true
	case PartKindToolResult:
		if p.ToolResult == nil {
			return Part{}, false
		}
		return Part{Kind: PartKindToolResult, ToolResult: &ToolResult{
			ToolCallID:  p.ToolResult.ToolCallID,
			Name:        p.ToolResult.Name,
			IsError:     p.ToolResult.IsError,
			Content:     p.ToolResult.Content,
			ContentJSON: decodeHookWireJSON(p.ToolResult.ContentJSON),
		}}, true
	default:
		// Text, and any kind this SDK does not know, which the server can only have
		// sent as text. Without the text there is nothing to recover.
		if p.Text == "" {
			return Part{}, false
		}
		return Part{Kind: PartKindText, Text: p.Text}, true
	}
}

// decodeHookWireJSON recovers a response-side JSON payload. The server marshals
// the protobuf bytes fields input_json, content_json, and input_schema_json with
// encoding/json, so they arrive base64-encoded inside a JSON string.
//
// The result is always a valid JSON document, because ToolCall.InputJSON and
// friends are json.RawMessage and invalid bytes there make every later
// json.Marshal of that part fail. Base64 that decodes to something other than
// JSON, and a string that is neither base64 nor JSON, are therefore kept as a
// JSON string so the text survives. Python and JS apply the same rule; see
// conformance/hooks/README.md.
func decodeHookWireJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		// An embedded JSON document rather than the proto encoding.
		if json.Valid(raw) {
			return raw
		}
		return nil
	}
	if text == "" {
		return nil
	}
	if decoded, err := base64.StdEncoding.DecodeString(text); err == nil {
		if json.Valid(decoded) {
			return decoded
		}
		return hookJSONString(string(decoded))
	}
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	return hookJSONString(text)
}

func hookJSONString(text string) json.RawMessage {
	quoted, err := json.Marshal(text)
	if err != nil {
		return nil
	}
	return quoted
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
	enrichHookCorrelation(ctx, &req.Context)

	timeout := hooksCfg.Timeout
	if timeout <= 0 {
		timeout = defaultHookTimeout
	}

	baseURL, err := baseURLFromAPIEndpoint(c.config.API.Endpoint, insecureValue(c.config.GenerationExport.Insecure))
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: %v", ErrHookTransportFailed, err))
	}
	endpoint := strings.TrimRight(baseURL, "/") + hooksEvaluatePath

	payload, err := json.Marshal(newHookWireRequest(req))
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: marshal hook request: %v", ErrHookTransportFailed, err))
	}

	hookCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(hookCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: build hook request: %v", ErrHookTransportFailed, err))
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(hookTimeoutHeader, strconv.FormatInt(timeout.Milliseconds(), 10))
	resolvedHeaders, err := resolveHeadersWithAuth(c.config.GenerationExport.Headers, c.config.GenerationExport.Auth)
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: resolve auth headers: %v", ErrHookTransportFailed, err))
	}
	for key, value := range resolvedHeaders {
		httpReq.Header.Set(key, value)
	}

	httpClient := &http.Client{Timeout: timeout}
	httpResp, err := httpClient.Do(httpReq)
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: %v", ErrHookTransportFailed, err))
	}
	defer func() {
		_ = httpResp.Body.Close()
	}()

	body, err := io.ReadAll(io.LimitReader(httpResp.Body, int64(maxHookEvaluateRespLen)+1))
	if err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: read hook response: %v", ErrHookTransportFailed, err))
	}
	if len(body) > maxHookEvaluateRespLen {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: hook response too large", ErrHookTransportFailed))
	}

	if httpResp.StatusCode != http.StatusOK {
		bodyText := strings.TrimSpace(string(body))
		if bodyText == "" {
			bodyText = http.StatusText(httpResp.StatusCode)
		}
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: status %d: %s", ErrHookTransportFailed, httpResp.StatusCode, bodyText))
	}

	var wire hookWireResponse
	if err := json.Unmarshal(body, &wire); err != nil {
		return c.failOpenOrError(failOpen, fmt.Errorf("%w: decode hook response: %v", ErrHookTransportFailed, err))
	}
	out := wire.toResponse()
	if out.Action == "" {
		out.Action = HookActionAllow
	}
	return &out, nil
}

func enrichHookCorrelation(ctx context.Context, hookCtx *HookContext) {
	if hookCtx == nil {
		return
	}
	if hookCtx.ConversationID == "" {
		if conversationID, ok := ConversationIDFromContext(ctx); ok {
			hookCtx.ConversationID = conversationID
		}
	}
	spanCtx := trace.SpanContextFromContext(ctx)
	if !spanCtx.IsValid() {
		return
	}
	if hookCtx.TraceID == "" {
		hookCtx.TraceID = spanCtx.TraceID().String()
	}
	if hookCtx.SpanID == "" {
		hookCtx.SpanID = spanCtx.SpanID().String()
	}
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

func (c *Client) failOpenOrError(failOpen bool, err error) (*HookEvaluateResponse, error) {
	if failOpen {
		// Fail-open turns an evaluator outage into a silent allow, so record it.
		c.logf("agento11y: hook evaluation failed, allowing request (fail_open): %v", err)
		return allowResponse(), nil
	}
	return nil, err
}

func phaseEnabled(phases []HookPhase, phase HookPhase) bool {
	if len(phases) == 0 {
		return true
	}
	return slices.Contains(phases, phase)
}
