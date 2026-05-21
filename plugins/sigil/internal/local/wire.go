package local

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"
)

const metadataKeyConversationTitle = "sigil.conversation.title"

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

func (u storedUsage) toSDK() sigil.TokenUsage {
	return sigil.TokenUsage{
		InputTokens:           u.InputTokens.int64(),
		OutputTokens:          u.OutputTokens.int64(),
		TotalTokens:           u.TotalTokens.int64(),
		CacheReadInputTokens:  u.CacheReadInputTokens.int64(),
		CacheWriteInputTokens: u.CacheWriteInputTokens.int64(),
		ReasoningTokens:       u.ReasoningTokens.int64(),
	}
}

type storedGeneration struct {
	ID                string          `json:"id,omitempty"`
	ConversationID    string          `json:"conversation_id,omitempty"`
	ConversationTitle string          `json:"conversation_title,omitempty"`
	AgentName         string          `json:"agent_name,omitempty"`
	Model             sigil.ModelRef  `json:"model,omitzero"`
	ResponseModel     string          `json:"response_model,omitempty"`
	Input             []storedMessage `json:"input,omitempty"`
	Output            []storedMessage `json:"output,omitempty"`
	Usage             storedUsage     `json:"usage,omitzero"`
	StopReason        string          `json:"stop_reason,omitempty"`
	StartedAt         time.Time       `json:"started_at,omitzero"`
	CompletedAt       time.Time       `json:"completed_at,omitzero"`
	Metadata          map[string]any  `json:"metadata,omitempty"`
	CallError         string          `json:"call_error,omitempty"`
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

func (g storedGeneration) inputMessages() []sigil.Message {
	return storedMessagesToSDK(g.Input)
}

func (g storedGeneration) outputMessages() []sigil.Message {
	return storedMessagesToSDK(g.Output)
}

type storedMessage struct {
	Role  string       `json:"role"`
	Name  string       `json:"name,omitempty"`
	Parts []storedPart `json:"parts"`
}

func storedMessagesToSDK(in []storedMessage) []sigil.Message {
	if len(in) == 0 {
		return nil
	}
	out := make([]sigil.Message, 0, len(in))
	for _, m := range in {
		msg := sigil.Message{Role: storedRoleToSDK(m.Role), Name: m.Name}
		if len(m.Parts) > 0 {
			msg.Parts = make([]sigil.Part, 0, len(m.Parts))
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

func storedRoleToSDK(role string) sigil.Role {
	switch role {
	case string(sigil.RoleUser), "MESSAGE_ROLE_USER":
		return sigil.RoleUser
	case string(sigil.RoleAssistant), "MESSAGE_ROLE_ASSISTANT":
		return sigil.RoleAssistant
	case string(sigil.RoleTool), "MESSAGE_ROLE_TOOL":
		return sigil.RoleTool
	default:
		return ""
	}
}

type storedPart struct {
	Kind       sigil.PartKind     `json:"kind,omitempty"`
	Text       *string            `json:"text,omitempty"`
	Thinking   *string            `json:"thinking,omitempty"`
	ToolCall   *storedToolCall    `json:"tool_call,omitempty"`
	ToolResult *storedToolResult  `json:"tool_result,omitempty"`
	Metadata   sigil.PartMetadata `json:"metadata,omitzero"`
}

func (p storedPart) toSDK() (sigil.Part, bool) {
	part := sigil.Part{Kind: p.Kind, Metadata: p.Metadata}
	switch {
	case p.Kind == sigil.PartKindText || p.Text != nil:
		part.Kind = sigil.PartKindText
		if p.Text != nil {
			part.Text = *p.Text
		}
	case p.Kind == sigil.PartKindThinking || p.Thinking != nil:
		part.Kind = sigil.PartKindThinking
		if p.Thinking != nil {
			part.Thinking = *p.Thinking
		}
	case p.Kind == sigil.PartKindToolCall || p.ToolCall != nil:
		if p.ToolCall == nil {
			return sigil.Part{}, false
		}
		part.Kind = sigil.PartKindToolCall
		part.ToolCall = p.ToolCall.toSDK()
	case p.Kind == sigil.PartKindToolResult || p.ToolResult != nil:
		if p.ToolResult == nil {
			return sigil.Part{}, false
		}
		part.Kind = sigil.PartKindToolResult
		part.ToolResult = p.ToolResult.toSDK()
	default:
		return sigil.Part{}, false
	}
	return part, true
}

type storedToolCall struct {
	ID        string        `json:"id,omitempty"`
	Name      string        `json:"name"`
	InputJSON storedRawJSON `json:"input_json,omitempty"`
}

func (c storedToolCall) toSDK() *sigil.ToolCall {
	return &sigil.ToolCall{ID: c.ID, Name: c.Name, InputJSON: c.InputJSON.raw()}
}

type storedToolResult struct {
	ToolCallID  string        `json:"tool_call_id,omitempty"`
	Name        string        `json:"name,omitempty"`
	IsError     bool          `json:"is_error,omitempty"`
	Content     string        `json:"content,omitempty"`
	ContentJSON storedRawJSON `json:"content_json,omitempty"`
}

func (r storedToolResult) toSDK() *sigil.ToolResult {
	return &sigil.ToolResult{
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
