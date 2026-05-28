package sigil

import (
	"encoding/json"

	"github.com/grafana/sigil-sdk/go/sigil/sigilmodel"
)

type Role = sigilmodel.Role

const (
	RoleUser      = sigilmodel.RoleUser
	RoleAssistant = sigilmodel.RoleAssistant
	RoleTool      = sigilmodel.RoleTool
)

type PartKind = sigilmodel.PartKind

const (
	PartKindText       = sigilmodel.PartKindText
	PartKindThinking   = sigilmodel.PartKindThinking
	PartKindToolCall   = sigilmodel.PartKindToolCall
	PartKindToolResult = sigilmodel.PartKindToolResult
)

type Message = sigilmodel.Message

type Part = sigilmodel.Part

// PartMetadata carries provider-specific details while keeping the core shape typed.
type PartMetadata = sigilmodel.PartMetadata

type ToolCall = sigilmodel.ToolCall

type ToolResult = sigilmodel.ToolResult

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
