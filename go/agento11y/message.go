package agento11y

import (
	"encoding/json"

	"github.com/grafana/agento11y/go/agento11y/model"
)

type Role = model.Role

const (
	RoleUser      = model.RoleUser
	RoleAssistant = model.RoleAssistant
	RoleTool      = model.RoleTool
)

type PartKind = model.PartKind

const (
	PartKindText       = model.PartKindText
	PartKindThinking   = model.PartKindThinking
	PartKindToolCall   = model.PartKindToolCall
	PartKindToolResult = model.PartKindToolResult
	PartKindMedia      = model.PartKindMedia
)

type Message = model.Message

type Part = model.Part

// PartMetadata carries provider-specific details while keeping the core shape typed.
type PartMetadata = model.PartMetadata

type ToolCall = model.ToolCall

type ToolResult = model.ToolResult

type Media = model.Media

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

func MediaPart(media Media) Part {
	return Part{
		Kind:  PartKindMedia,
		Media: &media,
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
