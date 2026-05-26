package sigil

import (
	"encoding/json"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type PartKind string

const (
	PartKindText       PartKind = "text"
	PartKindThinking   PartKind = "thinking"
	PartKindToolCall   PartKind = "tool_call"
	PartKindToolResult PartKind = "tool_result"
)

type Message struct {
	Role  Role   `json:"role"`
	Name  string `json:"name,omitempty"`
	Parts []Part `json:"parts"`
}

type Part struct {
	Kind       PartKind    `json:"kind"`
	Text       string      `json:"text,omitempty"`
	Thinking   string      `json:"thinking,omitempty"`
	ToolCall   *ToolCall   `json:"tool_call,omitempty"`
	ToolResult *ToolResult `json:"tool_result,omitempty"`
	// Metadata is a value-type struct where `omitempty` has no effect; the
	// field is always emitted in JSON form.
	Metadata PartMetadata `json:"metadata"`
}

// PartMetadata carries provider-specific details while keeping the core shape typed.
type PartMetadata struct {
	ProviderType string `json:"provider_type,omitempty"`
}

type ToolCall struct {
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name"`
	InputJSON json.RawMessage `json:"input_json,omitempty"`
}

type ToolResult struct {
	ToolCallID  string          `json:"tool_call_id,omitempty"`
	Name        string          `json:"name,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
	Content     string          `json:"content,omitempty"`
	ContentJSON json.RawMessage `json:"content_json,omitempty"`
}

func TextPart(text string) Part {
	return Part{
		Kind: PartKindText,
		Text: text,
	}
}

func ThinkingPart(thinking string) Part {
	return Part{
		Kind:     PartKindThinking,
		Thinking: thinking,
	}
}

func ToolCallPart(call ToolCall) Part {
	return Part{
		Kind:     PartKindToolCall,
		ToolCall: &call,
	}
}

func ToolResultPart(result ToolResult) Part {
	return Part{
		Kind:       PartKindToolResult,
		ToolResult: &result,
	}
}

// ---------------------------------------------------------------------------
// Message-level constructors
// ---------------------------------------------------------------------------

// UserTextMessage creates a user message with a single text part.
func UserTextMessage(text string) Message {
	return Message{
		Role:  RoleUser,
		Parts: []Part{TextPart(text)},
	}
}

// AssistantTextMessage creates an assistant message with a single text part.
func AssistantTextMessage(text string) Message {
	return Message{
		Role:  RoleAssistant,
		Parts: []Part{TextPart(text)},
	}
}

// ToolResultMessage creates a tool message with a single tool-result part.
// content is marshaled to JSON; pass a string, map, or struct.
func ToolResultMessage(callID string, content any) Message {
	var contentJSON json.RawMessage
	if content != nil {
		if data, err := json.Marshal(content); err == nil {
			contentJSON = data
		}
	}
	return Message{
		Role: RoleTool,
		Parts: []Part{ToolResultPart(ToolResult{
			ToolCallID:  callID,
			ContentJSON: contentJSON,
		})},
	}
}
