package local

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y"
)

const metadataKeyConversationTitle = "agento11y.conversation.title"

// protoInt64 accepts both proto-JSON int64 strings and ordinary JSON
// numbers. The local store can then read either the HTTP wire shape or
// test fixtures written in the SDK's Go JSON shape.
type protoInt64 int64

func (n *protoInt64) UnmarshalJSON(data []byte) error {
	s := strings.TrimSpace(string(data))
	if s == "" || s == "null" {
		*n = 0
		return nil
	}
	if strings.HasPrefix(s, "\"") {
		var quoted string
		if err := json.Unmarshal(data, &quoted); err != nil {
			return err
		}
		if strings.TrimSpace(quoted) == "" {
			*n = 0
			return nil
		}
		v, err := strconv.ParseInt(quoted, 10, 64)
		if err != nil {
			return err
		}
		*n = protoInt64(v)
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return err
	}
	*n = protoInt64(v)
	return nil
}

func (n protoInt64) int64() int64 { return int64(n) }

type storedUsage struct {
	InputTokens           protoInt64 `json:"input_tokens,omitempty"`
	OutputTokens          protoInt64 `json:"output_tokens,omitempty"`
	TotalTokens           protoInt64 `json:"total_tokens,omitempty"`
	CacheReadInputTokens  protoInt64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens protoInt64 `json:"cache_write_input_tokens,omitempty"`
	ReasoningTokens       protoInt64 `json:"reasoning_tokens,omitempty"`
}

func (u storedUsage) toSDK() agento11y.TokenUsage {
	return agento11y.TokenUsage{
		InputTokens:           u.InputTokens.int64(),
		OutputTokens:          u.OutputTokens.int64(),
		TotalTokens:           u.TotalTokens.int64(),
		CacheReadInputTokens:  u.CacheReadInputTokens.int64(),
		CacheWriteInputTokens: u.CacheWriteInputTokens.int64(),
		ReasoningTokens:       u.ReasoningTokens.int64(),
	}
}

type storedGeneration struct {
	ID                string             `json:"id,omitempty"`
	ConversationID    string             `json:"conversation_id,omitempty"`
	ConversationTitle string             `json:"conversation_title,omitempty"`
	AgentName         string             `json:"agent_name,omitempty"`
	Model             agento11y.ModelRef `json:"model,omitzero"`
	ResponseModel     string             `json:"response_model,omitempty"`
	Input             []storedMessage    `json:"input,omitempty"`
	Output            []storedMessage    `json:"output,omitempty"`
	Usage             storedUsage        `json:"usage,omitzero"`
	StopReason        string             `json:"stop_reason,omitempty"`
	StartedAt         time.Time          `json:"started_at,omitzero"`
	CompletedAt       time.Time          `json:"completed_at,omitzero"`
	Metadata          map[string]any     `json:"metadata,omitempty"`
	CallError         string             `json:"call_error,omitempty"`
}

func (g storedGeneration) title() string {
	if strings.TrimSpace(g.ConversationTitle) != "" {
		return g.ConversationTitle
	}
	if g.Metadata == nil {
		return ""
	}
	if title, ok := g.Metadata[metadataKeyConversationTitle].(string); ok {
		return title
	}
	return ""
}

func (g storedGeneration) modelName() string {
	if g.ResponseModel != "" {
		return g.ResponseModel
	}
	return g.Model.Name
}

func (g storedGeneration) inputMessages() []agento11y.Message {
	return storedMessagesToSDK(g.Input)
}

func (g storedGeneration) outputMessages() []agento11y.Message {
	return storedMessagesToSDK(g.Output)
}

type storedMessage struct {
	Role  string       `json:"role"`
	Name  string       `json:"name,omitempty"`
	Parts []storedPart `json:"parts"`
}

func storedMessagesToSDK(in []storedMessage) []agento11y.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]agento11y.Message, 0, len(in))
	for _, m := range in {
		msg := agento11y.Message{Role: storedRoleToSDK(m.Role), Name: m.Name}
		if len(m.Parts) > 0 {
			msg.Parts = make([]agento11y.Part, 0, len(m.Parts))
			for _, p := range m.Parts {
				part, ok := p.toSDK()
				if ok {
					msg.Parts = append(msg.Parts, part)
				}
			}
		}
		out = append(out, msg)
	}
	return out
}

func storedRoleToSDK(role string) agento11y.Role {
	switch role {
	case string(agento11y.RoleUser), "MESSAGE_ROLE_USER":
		return agento11y.RoleUser
	case string(agento11y.RoleAssistant), "MESSAGE_ROLE_ASSISTANT":
		return agento11y.RoleAssistant
	case string(agento11y.RoleTool), "MESSAGE_ROLE_TOOL":
		return agento11y.RoleTool
	default:
		return ""
	}
}

type storedPart struct {
	Kind       agento11y.PartKind     `json:"kind,omitempty"`
	Text       *string                `json:"text,omitempty"`
	Thinking   *string                `json:"thinking,omitempty"`
	ToolCall   *storedToolCall        `json:"tool_call,omitempty"`
	ToolResult *storedToolResult      `json:"tool_result,omitempty"`
	Media      *agento11y.Media       `json:"media,omitempty"`
	Metadata   agento11y.PartMetadata `json:"metadata,omitzero"`
}

func (p storedPart) toSDK() (agento11y.Part, bool) {
	part := agento11y.Part{Kind: p.Kind, Metadata: p.Metadata}
	switch {
	case p.Kind == agento11y.PartKindText || p.Text != nil:
		part.Kind = agento11y.PartKindText
		if p.Text != nil {
			part.Text = *p.Text
		}
	case p.Kind == agento11y.PartKindThinking || p.Thinking != nil:
		part.Kind = agento11y.PartKindThinking
		if p.Thinking != nil {
			part.Thinking = *p.Thinking
		}
	case p.Kind == agento11y.PartKindToolCall || p.ToolCall != nil:
		if p.ToolCall == nil {
			return agento11y.Part{}, false
		}
		part.Kind = agento11y.PartKindToolCall
		part.ToolCall = p.ToolCall.toSDK()
	case p.Kind == agento11y.PartKindToolResult || p.ToolResult != nil:
		if p.ToolResult == nil {
			return agento11y.Part{}, false
		}
		part.Kind = agento11y.PartKindToolResult
		part.ToolResult = p.ToolResult.toSDK()
	case p.Kind == agento11y.PartKindMedia || p.Media != nil:
		if p.Media == nil {
			return agento11y.Part{}, false
		}
		part.Kind = agento11y.PartKindMedia
		media := *p.Media
		part.Media = &media
	default:
		return agento11y.Part{}, false
	}
	return part, true
}

type storedToolCall struct {
	ID        string        `json:"id,omitempty"`
	Name      string        `json:"name"`
	InputJSON storedRawJSON `json:"input_json,omitempty"`
}

func (c storedToolCall) toSDK() *agento11y.ToolCall {
	return &agento11y.ToolCall{ID: c.ID, Name: c.Name, InputJSON: c.InputJSON.raw()}
}

type storedToolResult struct {
	ToolCallID  string        `json:"tool_call_id,omitempty"`
	Name        string        `json:"name,omitempty"`
	IsError     bool          `json:"is_error,omitempty"`
	Content     string        `json:"content,omitempty"`
	ContentJSON storedRawJSON `json:"content_json,omitempty"`
}

func (r storedToolResult) toSDK() *agento11y.ToolResult {
	return &agento11y.ToolResult{
		ToolCallID:  r.ToolCallID,
		Name:        r.Name,
		IsError:     r.IsError,
		Content:     r.Content,
		ContentJSON: r.ContentJSON.raw(),
	}
}

type storedRawJSON []byte

func (r *storedRawJSON) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*r = nil
		return nil
	}
	if data[0] == '"' {
		var encoded string
		if err := json.Unmarshal(data, &encoded); err != nil {
			return err
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err == nil && json.Valid(decoded) {
			*r = append((*r)[:0], decoded...)
			return nil
		}
	}
	*r = append((*r)[:0], data...)
	return nil
}

func (r storedRawJSON) raw() json.RawMessage {
	if len(r) == 0 {
		return nil
	}
	return append(json.RawMessage(nil), r...)
}
