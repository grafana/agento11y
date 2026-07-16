package sigil

import (
	"maps"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil/sigilmodel"
)

const (
	defaultOperationNameSync   = "generateText"
	defaultOperationNameStream = "streamText"
)

type GenerationMode = sigilmodel.GenerationMode

const (
	GenerationModeSync   = sigilmodel.GenerationModeSync
	GenerationModeStream = sigilmodel.GenerationModeStream
)

// ModelRef identifies the LLM provider and model used for a generation.
type ModelRef = sigilmodel.ModelRef

// ToolDefinition describes a callable tool visible to the model.
type ToolDefinition = sigilmodel.ToolDefinition

// Generation is the normalized, provider-agnostic generation payload.
// It can represent both request/response and streaming outcomes.
type Generation = sigilmodel.Generation

// GenerationStart seeds generation fields before the provider call executes.
// Any zero-valued fields can be filled later by End.
type GenerationStart struct {
	ID                  string
	ConversationID      string
	ConversationTitle   string
	UserID              string
	AgentName           string
	AgentVersion        string
	Mode                GenerationMode
	OperationName       string
	Model               ModelRef
	SystemPrompt        string
	Tools               []ToolDefinition
	MaxTokens           *int64
	Temperature         *float64
	TopP                *float64
	ToolChoice          *string
	ThinkingEnabled     *bool
	ParentGenerationIDs []string
	EffectiveVersion    string
	Tags                map[string]string
	Metadata            map[string]any
	StartedAt           time.Time
	// ContentCapture overrides the client-level ContentCaptureMode for this
	// generation. Default (zero value) inherits from Config.
	ContentCapture ContentCaptureMode
}

func defaultOperationNameForMode(mode GenerationMode) string {
	if mode == GenerationModeStream {
		return defaultOperationNameStream
	}
	return defaultOperationNameSync
}

func cloneGeneration(in Generation) Generation {
	return Generation{
		ID:                  in.ID,
		ConversationID:      in.ConversationID,
		ConversationTitle:   in.ConversationTitle,
		UserID:              in.UserID,
		AgentName:           in.AgentName,
		AgentVersion:        in.AgentVersion,
		Mode:                in.Mode,
		OperationName:       in.OperationName,
		TraceID:             in.TraceID,
		SpanID:              in.SpanID,
		Model:               in.Model,
		ResponseID:          in.ResponseID,
		ResponseModel:       in.ResponseModel,
		SystemPrompt:        in.SystemPrompt,
		Input:               cloneMessages(in.Input),
		Output:              cloneMessages(in.Output),
		Tools:               cloneTools(in.Tools),
		MaxTokens:           cloneInt64Ptr(in.MaxTokens),
		Temperature:         cloneFloat64Ptr(in.Temperature),
		TopP:                cloneFloat64Ptr(in.TopP),
		ToolChoice:          cloneStringPtr(in.ToolChoice),
		ThinkingEnabled:     cloneBoolPtr(in.ThinkingEnabled),
		ParentGenerationIDs: cloneStringSlice(in.ParentGenerationIDs),
		EffectiveVersion:    in.EffectiveVersion,
		Usage:               in.Usage,
		StopReason:          in.StopReason,
		StartedAt:           in.StartedAt,
		CompletedAt:         in.CompletedAt,
		Tags:                cloneTags(in.Tags),
		Metadata:            cloneMetadata(in.Metadata),
		Artifacts:           cloneArtifacts(in.Artifacts),
		CallError:           in.CallError,
	}
}

func cloneGenerationStart(in GenerationStart) GenerationStart {
	return GenerationStart{
		ID:                  in.ID,
		ConversationID:      in.ConversationID,
		ConversationTitle:   in.ConversationTitle,
		UserID:              in.UserID,
		AgentName:           in.AgentName,
		AgentVersion:        in.AgentVersion,
		Mode:                in.Mode,
		OperationName:       in.OperationName,
		Model:               in.Model,
		SystemPrompt:        in.SystemPrompt,
		Tools:               cloneTools(in.Tools),
		MaxTokens:           cloneInt64Ptr(in.MaxTokens),
		Temperature:         cloneFloat64Ptr(in.Temperature),
		TopP:                cloneFloat64Ptr(in.TopP),
		ToolChoice:          cloneStringPtr(in.ToolChoice),
		ThinkingEnabled:     cloneBoolPtr(in.ThinkingEnabled),
		ParentGenerationIDs: cloneStringSlice(in.ParentGenerationIDs),
		EffectiveVersion:    in.EffectiveVersion,
		Tags:                cloneTags(in.Tags),
		Metadata:            cloneMetadata(in.Metadata),
		StartedAt:           in.StartedAt,
		ContentCapture:      in.ContentCapture,
	}
}

func cloneInt64Ptr(in *int64) *int64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneFloat64Ptr(in *float64) *float64 {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneStringPtr(in *string) *string {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneBoolPtr(in *bool) *bool {
	if in == nil {
		return nil
	}
	out := *in
	return &out
}

func cloneMessages(in []Message) []Message {
	if len(in) == 0 {
		return nil
	}

	out := make([]Message, len(in))
	for i := range in {
		out[i] = Message{
			Role:  in[i].Role,
			Name:  in[i].Name,
			Parts: cloneParts(in[i].Parts),
		}
	}

	return out
}

func cloneParts(in []Part) []Part {
	if len(in) == 0 {
		return nil
	}

	out := make([]Part, len(in))
	for i := range in {
		out[i] = Part{
			Kind:     in[i].Kind,
			Text:     in[i].Text,
			Thinking: in[i].Thinking,
			Metadata: in[i].Metadata,
		}

		if in[i].ToolCall != nil {
			call := *in[i].ToolCall
			call.InputJSON = append([]byte(nil), call.InputJSON...)
			out[i].ToolCall = &call
		}

		if in[i].ToolResult != nil {
			result := *in[i].ToolResult
			result.ContentJSON = append([]byte(nil), result.ContentJSON...)
			out[i].ToolResult = &result
		}

		if in[i].Media != nil {
			media := *in[i].Media
			out[i].Media = &media
		}
	}

	return out
}

func cloneTools(in []ToolDefinition) []ToolDefinition {
	if len(in) == 0 {
		return nil
	}

	out := make([]ToolDefinition, len(in))
	copy(out, in)

	for i := range out {
		out[i].InputSchema = append([]byte(nil), out[i].InputSchema...)
	}

	return out
}

func cloneArtifacts(in []Artifact) []Artifact {
	if len(in) == 0 {
		return nil
	}

	out := make([]Artifact, len(in))
	copy(out, in)

	for i := range out {
		out[i].Payload = append([]byte(nil), out[i].Payload...)
	}

	return out
}

func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)

	return out
}

func cloneMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]any, len(in))
	maps.Copy(out, in)

	return out
}
