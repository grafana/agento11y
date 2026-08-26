package local

import (
	"encoding/base64"
	"encoding/json"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/plugins/agento11y/internal/guardeval"
)

// Hook requests arrive in three wire forms: Go snake_case with raw JSON,
// proto-JSON with base64-encoded byte fields, and legacy JS camelCase with JSON
// strings. Accept all three because unknown fields unmarshal without error and
// can hide tool calls from enforcement.
//
// go/agento11y/hooks.go carries wire types of its own that look like these and
// are not. Those spell the one dialect the SDK sends to Cloud, as a client. The
// daemon receives what any host emits, so it decodes a superset. Folding these
// into the SDK would put a server-side decoder in a client library.

func decodeHookEvaluateRequest(body []byte) (agento11y.HookEvaluateRequest, error) {
	var raw wireEvaluateRequest
	if err := json.Unmarshal(body, &raw); err != nil {
		return agento11y.HookEvaluateRequest{}, err
	}
	return agento11y.HookEvaluateRequest{
		Phase:   agento11y.HookPhase(strings.TrimSpace(raw.Phase)),
		Context: raw.Context.decode(),
		Input:   raw.Input.decode(unwrapWireJSON),
	}, nil
}

func decodeHookEvaluateResponse(body []byte) (agento11y.HookEvaluateResponse, error) {
	var raw wireEvaluateResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return agento11y.HookEvaluateResponse{}, err
	}
	out := agento11y.HookEvaluateResponse{
		Action:      raw.Action,
		RuleID:      raw.RuleID,
		Reason:      raw.Reason,
		Evaluations: raw.Evaluations,
	}
	if raw.TransformedInput != nil {
		decoded := raw.TransformedInput.decode(decodeResponseWireJSON)
		out.TransformedInput = &decoded
	}
	return out, nil
}

type wireEvaluateRequest struct {
	Phase   string      `json:"phase"`
	Context wireContext `json:"context"`
	Input   wireInput   `json:"input"`
}

type wireEvaluateResponse struct {
	Action           agento11y.HookAction       `json:"action"`
	RuleID           string                     `json:"rule_id"`
	Reason           string                     `json:"reason"`
	TransformedInput *wireInput                 `json:"transformed_input"`
	Evaluations      []agento11y.HookEvaluation `json:"evaluations"`
}

type wireContext struct {
	AgentName        string            `json:"agent_name"`
	AgentNameCamel   string            `json:"agentName"`
	AgentVersion     string            `json:"agent_version"`
	AgentVersionCaml string            `json:"agentVersion"`
	Model            *wireModel        `json:"model"`
	Tags             map[string]string `json:"tags"`
	ConversationID   string            `json:"conversation_id"`
	ConversationCaml string            `json:"conversationId"`
	TraceID          string            `json:"trace_id"`
	TraceIDCamel     string            `json:"traceId"`
	SpanID           string            `json:"span_id"`
	SpanIDCamel      string            `json:"spanId"`
}

type wireModel struct {
	Provider string `json:"provider"`
	Name     string `json:"name"`
}

func (c wireContext) decode() agento11y.HookContext {
	pick := func(snake, camel string) string {
		if v := strings.TrimSpace(snake); v != "" {
			return v
		}
		if v := strings.TrimSpace(camel); v != "" {
			return v
		}
		return ""
	}
	out := agento11y.HookContext{
		AgentName:      pick(c.AgentName, c.AgentNameCamel),
		AgentVersion:   pick(c.AgentVersion, c.AgentVersionCaml),
		Tags:           c.Tags,
		ConversationID: pick(c.ConversationID, c.ConversationCaml),
		TraceID:        pick(c.TraceID, c.TraceIDCamel),
		SpanID:         pick(c.SpanID, c.SpanIDCamel),
	}
	if c.Model != nil {
		out.Model = &agento11y.HookModel{Provider: c.Model.Provider, Name: c.Model.Name}
	}
	return out
}

type wireInput struct {
	Messages            []wireMessage        `json:"messages"`
	Output              []wireMessage        `json:"output"`
	Tools               []wireToolDefinition `json:"tools"`
	SystemPrompt        string               `json:"system_prompt"`
	SystemPromptCamel   string               `json:"systemPrompt"`
	ConversationPreview string               `json:"conversation_preview"`
	ConversationPrevCml string               `json:"conversationPreview"`
}

type wireJSONDecoder func(json.RawMessage) json.RawMessage

func (in wireInput) decode(decodeJSON wireJSONDecoder) agento11y.HookInput {
	out := agento11y.HookInput{
		SystemPrompt:        in.SystemPrompt,
		ConversationPreview: in.ConversationPreview,
	}
	if out.SystemPrompt == "" && in.SystemPromptCamel != "" {
		out.SystemPrompt = in.SystemPromptCamel
	}
	if out.ConversationPreview == "" && in.ConversationPrevCml != "" {
		out.ConversationPreview = in.ConversationPrevCml
	}
	out.Messages = decodeWireMessages(in.Messages, decodeJSON)
	out.Output = decodeWireMessages(in.Output, decodeJSON)
	for _, t := range in.Tools {
		out.Tools = append(out.Tools, t.decode(decodeJSON))
	}
	return out
}

type wireToolDefinition struct {
	Name            string          `json:"name"`
	Description     string          `json:"description"`
	Type            string          `json:"type"`
	InputSchema     json.RawMessage `json:"input_schema"`
	InputSchemaJSON json.RawMessage `json:"input_schema_json"`
	InputSchemaCaml json.RawMessage `json:"inputSchemaJSON"`
	Deferred        bool            `json:"deferred"`
}

func (t wireToolDefinition) decode(decodeJSON wireJSONDecoder) agento11y.ToolDefinition {
	out := agento11y.ToolDefinition{Name: t.Name, Description: t.Description, Type: t.Type, Deferred: t.Deferred}
	switch {
	case len(t.InputSchema) > 0:
		out.InputSchema = decodeJSON(t.InputSchema)
	case len(t.InputSchemaJSON) > 0:
		out.InputSchema = decodeJSON(t.InputSchemaJSON)
	case len(t.InputSchemaCaml) > 0:
		out.InputSchema = decodeJSON(t.InputSchemaCaml)
	}
	return out
}

type wireMessage struct {
	Role    string     `json:"role"`
	Name    string     `json:"name"`
	Content string     `json:"content"`
	Parts   []wirePart `json:"parts"`
}

type wirePart struct {
	Kind           string           `json:"kind"`
	Type           string           `json:"type"`
	Text           string           `json:"text"`
	Thinking       string           `json:"thinking"`
	ToolCall       *wireToolCall    `json:"tool_call"`
	ToolCallCamel  *wireToolCall    `json:"toolCall"`
	ToolResult     *wireToolResult  `json:"tool_result"`
	ToolResultCaml *wireToolResult  `json:"toolResult"`
	Media          *wireMedia       `json:"media"`
	Metadata       wirePartMetadata `json:"metadata"`
}

// wireMedia carries a media part through the daemon. No SDK builds one today:
// only Go's model.Part has the field, its hook encoder drops it, and the server
// has no media kind (conformance/hooks/README.md). Decoding it anyway keeps
// transformed_input from deleting a part of a hand-written or proto-JSON body,
// which is a replacement payload the host sends as it stands.
type wireMedia struct {
	Kind          string `json:"kind"`
	URL           string `json:"url"`
	MIMEType      string `json:"mime_type"`
	MIMETypeCamel string `json:"mimeType"`
	Name          string `json:"name"`
}

type wirePartMetadata struct {
	ProviderType      string `json:"provider_type"`
	ProviderTypeCamel string `json:"providerType"`
}

type wireToolCall struct {
	ID             string          `json:"id"`
	Name           string          `json:"name"`
	InputJSON      json.RawMessage `json:"input_json"`
	InputJSONCamel json.RawMessage `json:"inputJSON"`
	InputJSONCaml2 json.RawMessage `json:"inputJson"`
}

type wireToolResult struct {
	ToolCallID       string          `json:"tool_call_id"`
	ToolCallIDCamel  string          `json:"toolCallId"`
	Name             string          `json:"name"`
	IsError          bool            `json:"is_error"`
	IsErrorCamel     bool            `json:"isError"`
	Content          string          `json:"content"`
	ContentJSON      json.RawMessage `json:"content_json"`
	ContentJSONCamel json.RawMessage `json:"contentJSON"`
	ContentJSONCaml2 json.RawMessage `json:"contentJson"`
}

func decodeWireMessages(msgs []wireMessage, decodeJSON wireJSONDecoder) []agento11y.Message {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]agento11y.Message, 0, len(msgs))
	for _, m := range msgs {
		msg := agento11y.Message{Role: agento11y.Role(m.Role), Name: m.Name}
		for _, p := range m.Parts {
			msg.Parts = append(msg.Parts, p.decode(decodeJSON))
		}
		// The `content` shorthand carries the text when no typed parts were
		// sent, so a rule matching message text still sees it.
		if len(msg.Parts) == 0 && m.Content != "" {
			msg.Parts = append(msg.Parts, agento11y.Part{Kind: agento11y.PartKindText, Text: m.Content})
		}
		out = append(out, msg)
	}
	return out
}

func (p wirePart) decode(decodeJSON wireJSONDecoder) agento11y.Part {
	kind := strings.TrimSpace(p.Kind)
	if kind == "" && strings.TrimSpace(p.Type) != "" {
		kind = strings.TrimSpace(p.Type)
	}
	providerType := p.Metadata.ProviderType
	if providerType == "" && p.Metadata.ProviderTypeCamel != "" {
		providerType = p.Metadata.ProviderTypeCamel
	}
	out := agento11y.Part{
		Kind:     agento11y.PartKind(kind),
		Text:     p.Text,
		Thinking: p.Thinking,
		Metadata: agento11y.PartMetadata{ProviderType: providerType},
	}
	tc := p.ToolCall
	if tc == nil && p.ToolCallCamel != nil {
		tc = p.ToolCallCamel
	}
	if tc != nil {
		call := agento11y.ToolCall{ID: tc.ID, Name: tc.Name}
		switch {
		case len(tc.InputJSON) > 0:
			call.InputJSON = decodeJSON(tc.InputJSON)
		case len(tc.InputJSONCamel) > 0:
			call.InputJSON = decodeJSON(tc.InputJSONCamel)
		case len(tc.InputJSONCaml2) > 0:
			call.InputJSON = decodeJSON(tc.InputJSONCaml2)
		}
		out.ToolCall = &call
		if out.Kind == "" {
			out.Kind = agento11y.PartKindToolCall
		}
	}
	tr := p.ToolResult
	if tr == nil && p.ToolResultCaml != nil {
		tr = p.ToolResultCaml
	}
	if tr != nil {
		result := agento11y.ToolResult{
			ToolCallID: tr.ToolCallID,
			Name:       tr.Name,
			IsError:    tr.IsError || tr.IsErrorCamel,
			Content:    tr.Content,
		}
		if result.ToolCallID == "" {
			result.ToolCallID = tr.ToolCallIDCamel
		}
		switch {
		case len(tr.ContentJSON) > 0:
			result.ContentJSON = decodeJSON(tr.ContentJSON)
		case len(tr.ContentJSONCamel) > 0:
			result.ContentJSON = decodeJSON(tr.ContentJSONCamel)
		case len(tr.ContentJSONCaml2) > 0:
			result.ContentJSON = decodeJSON(tr.ContentJSONCaml2)
		}
		out.ToolResult = &result
		if out.Kind == "" {
			out.Kind = agento11y.PartKindToolResult
		}
	}
	if p.Media != nil {
		media := agento11y.Media{Kind: p.Media.Kind, URL: p.Media.URL, MIMEType: p.Media.MIMEType, Name: p.Media.Name}
		if media.MIMEType == "" && p.Media.MIMETypeCamel != "" {
			media.MIMEType = p.Media.MIMETypeCamel
		}
		out.Media = &media
		if out.Kind == "" {
			out.Kind = agento11y.PartKindMedia
		}
	}
	if out.Kind == "" {
		switch {
		case p.Text != "":
			out.Kind = agento11y.PartKindText
		case p.Thinking != "":
			out.Kind = agento11y.PartKindThinking
		}
	}
	return out
}

// unwrapWireJSON accepts raw JSON, a JSON string containing JSON, or a
// proto-JSON base64 bytes value. Invalid string payloads remain unchanged so
// rules can inspect the original value.
func unwrapWireJSON(raw json.RawMessage) json.RawMessage {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return raw
	}
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil && json.Valid(decoded) {
		return decoded
	}
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	return raw
}

// decodeResponseWireJSON returns a valid JSON document from a hook response's
// bytes field. Cloud sends base64. Embedded JSON and malformed strings remain
// accepted so one bad transformed field does not discard a deny.
func decodeResponseWireJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
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
		return jsonString(string(decoded))
	}
	if json.Valid([]byte(text)) {
		return json.RawMessage(text)
	}
	return jsonString(text)
}

func jsonString(text string) json.RawMessage {
	quoted, _ := json.Marshal(text)
	return quoted
}

// encodeHookEvaluateResponse emits the one response shape current SDKs parse:
// snake_case keys and base64-encoded JSON payloads.
func encodeHookEvaluateResponse(resp guardeval.Response) any {
	if resp.TransformedInput == nil {
		return resp
	}
	out := map[string]any{"action": string(resp.Action)}
	if resp.RuleID != "" {
		out["rule_id"] = resp.RuleID
	}
	if resp.Reason != "" {
		out["reason"] = resp.Reason
	}
	if resp.Evaluations != nil {
		out["evaluations"] = resp.Evaluations
	}
	out["transformed_input"] = encodeWireInput(*resp.TransformedInput)
	return out
}

// encodeRelayBody encodes the decoded request for Cloud after a local rewrite.
// Messages use the request-side raw JSON payloads. Tool schemas use the
// input_schema_json bytes field that the hooks endpoint accepts.
func encodeRelayBody(phase agento11y.HookPhase, ctx agento11y.HookContext, in agento11y.HookInput) ([]byte, error) {
	return json.Marshal(struct {
		Phase   agento11y.HookPhase   `json:"phase"`
		Context agento11y.HookContext `json:"context"`
		Input   relayWireInput        `json:"input"`
	}{
		Phase:   phase,
		Context: ctx,
		Input: relayWireInput{
			Messages:            in.Messages,
			Tools:               encodeWireTools(in.Tools),
			SystemPrompt:        in.SystemPrompt,
			Output:              in.Output,
			ConversationPreview: in.ConversationPreview,
		},
	})
}

type relayWireInput struct {
	Messages            []agento11y.Message `json:"messages,omitempty"`
	Tools               []map[string]any    `json:"tools,omitempty"`
	SystemPrompt        string              `json:"system_prompt,omitempty"`
	Output              []agento11y.Message `json:"output,omitempty"`
	ConversationPreview string              `json:"conversation_preview,omitempty"`
}

func encodeWireInput(in agento11y.HookInput) map[string]any {
	out := map[string]any{}
	if in.SystemPrompt != "" {
		out["system_prompt"] = in.SystemPrompt
	}
	if in.ConversationPreview != "" {
		out["conversation_preview"] = in.ConversationPreview
	}
	if msgs := encodeWireMessages(in.Messages); msgs != nil {
		out["messages"] = msgs
	}
	if msgs := encodeWireMessages(in.Output); msgs != nil {
		out["output"] = msgs
	}
	if len(in.Tools) > 0 {
		out["tools"] = encodeWireTools(in.Tools)
	}
	return out
}

// encodeWireTools spells a tool definition the way the hook API carries one:
// the schema under input_schema_json, base64-encoded.
func encodeWireTools(tools []agento11y.ToolDefinition) []map[string]any {
	if len(tools) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		tool := map[string]any{"name": t.Name}
		if t.Description != "" {
			tool["description"] = t.Description
		}
		if t.Type != "" {
			tool["type"] = t.Type
		}
		if len(t.InputSchema) > 0 {
			tool["input_schema_json"] = base64.StdEncoding.EncodeToString(t.InputSchema)
		}
		if t.Deferred {
			tool["deferred"] = true
		}
		out = append(out, tool)
	}
	return out
}

func encodeWireMessages(msgs []agento11y.Message) []map[string]any {
	if len(msgs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		msg := map[string]any{"role": string(m.Role)}
		if m.Name != "" {
			msg["name"] = m.Name
		}
		parts := make([]map[string]any, 0, len(m.Parts))
		for _, p := range m.Parts {
			parts = append(parts, encodeWirePart(p))
		}
		msg["parts"] = parts
		out = append(out, msg)
	}
	return out
}

func encodeWirePart(p agento11y.Part) map[string]any {
	part := map[string]any{"kind": string(p.Kind)}
	if p.Metadata.ProviderType != "" {
		part["metadata"] = map[string]any{"provider_type": p.Metadata.ProviderType}
	}
	if p.Text != "" {
		part["text"] = p.Text
	}
	if p.Thinking != "" {
		part["thinking"] = p.Thinking
	}
	if p.ToolCall != nil {
		call := map[string]any{"name": p.ToolCall.Name}
		if p.ToolCall.ID != "" {
			call["id"] = p.ToolCall.ID
		}
		if len(p.ToolCall.InputJSON) > 0 {
			call["input_json"] = encodeWireJSON(p.ToolCall.InputJSON)
		}
		part["tool_call"] = call
	}
	if p.ToolResult != nil {
		result := map[string]any{}
		if p.ToolResult.ToolCallID != "" {
			result["tool_call_id"] = p.ToolResult.ToolCallID
		}
		if p.ToolResult.Name != "" {
			result["name"] = p.ToolResult.Name
		}
		if p.ToolResult.IsError {
			result["is_error"] = true
		}
		if p.ToolResult.Content != "" {
			result["content"] = p.ToolResult.Content
		}
		if len(p.ToolResult.ContentJSON) > 0 {
			result["content_json"] = encodeWireJSON(p.ToolResult.ContentJSON)
		}
		part["tool_result"] = result
	}
	if p.Media != nil {
		media := map[string]any{}
		if p.Media.Kind != "" {
			media["kind"] = p.Media.Kind
		}
		if p.Media.URL != "" {
			media["url"] = p.Media.URL
		}
		if p.Media.MIMEType != "" {
			media["mime_type"] = p.Media.MIMEType
		}
		if p.Media.Name != "" {
			media["name"] = p.Media.Name
		}
		part["media"] = media
	}
	return part
}

func encodeWireJSON(raw json.RawMessage) string {
	return base64.StdEncoding.EncodeToString(raw)
}
