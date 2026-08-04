package agento11y

import (
	"slices"
	"strings"
	"time"

	"github.com/grafana/agento11y/go/agento11y/model"
)

const (
	defaultOperationNameSync   = "generateText"
	defaultOperationNameStream = "streamText"
)

type GenerationMode = model.GenerationMode

const (
	GenerationModeSync   = model.GenerationModeSync
	GenerationModeStream = model.GenerationModeStream
)

// ModelRef identifies the LLM provider and model used for a generation.
type ModelRef = model.ModelRef

// ToolDefinition describes a callable tool visible to the model.
type ToolDefinition = model.ToolDefinition

// Generation is the normalized, provider-agnostic generation payload.
// It can represent both request/response and streaming outcomes.
type Generation = model.Generation

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
		ID:                  strings.Clone(in.ID),
		ConversationID:      strings.Clone(in.ConversationID),
		ConversationTitle:   strings.Clone(in.ConversationTitle),
		UserID:              strings.Clone(in.UserID),
		AgentName:           strings.Clone(in.AgentName),
		AgentVersion:        strings.Clone(in.AgentVersion),
		Mode:                GenerationMode(strings.Clone(string(in.Mode))),
		OperationName:       strings.Clone(in.OperationName),
		TraceID:             strings.Clone(in.TraceID),
		SpanID:              strings.Clone(in.SpanID),
		Model:               cloneModelRef(in.Model),
		ResponseID:          strings.Clone(in.ResponseID),
		ResponseModel:       strings.Clone(in.ResponseModel),
		SystemPrompt:        strings.Clone(in.SystemPrompt),
		Input:               cloneMessages(in.Input),
		Output:              cloneMessages(in.Output),
		Tools:               cloneTools(in.Tools),
		MaxTokens:           cloneInt64Ptr(in.MaxTokens),
		Temperature:         cloneFloat64Ptr(in.Temperature),
		TopP:                cloneFloat64Ptr(in.TopP),
		ToolChoice:          cloneStringPtr(in.ToolChoice),
		ThinkingEnabled:     cloneBoolPtr(in.ThinkingEnabled),
		ParentGenerationIDs: cloneStringSlice(in.ParentGenerationIDs),
		EffectiveVersion:    strings.Clone(in.EffectiveVersion),
		Usage:               in.Usage,
		StopReason:          strings.Clone(in.StopReason),
		StartedAt:           in.StartedAt,
		CompletedAt:         in.CompletedAt,
		Tags:                cloneTags(in.Tags),
		Metadata:            cloneMetadata(in.Metadata),
		Artifacts:           cloneArtifacts(in.Artifacts),
		CallError:           strings.Clone(in.CallError),
	}
}

func cloneGenerationStart(in GenerationStart) GenerationStart {
	return GenerationStart{
		ID:                  strings.Clone(in.ID),
		ConversationID:      strings.Clone(in.ConversationID),
		ConversationTitle:   strings.Clone(in.ConversationTitle),
		UserID:              strings.Clone(in.UserID),
		AgentName:           strings.Clone(in.AgentName),
		AgentVersion:        strings.Clone(in.AgentVersion),
		Mode:                GenerationMode(strings.Clone(string(in.Mode))),
		OperationName:       strings.Clone(in.OperationName),
		Model:               cloneModelRef(in.Model),
		SystemPrompt:        strings.Clone(in.SystemPrompt),
		Tools:               cloneTools(in.Tools),
		MaxTokens:           cloneInt64Ptr(in.MaxTokens),
		Temperature:         cloneFloat64Ptr(in.Temperature),
		TopP:                cloneFloat64Ptr(in.TopP),
		ToolChoice:          cloneStringPtr(in.ToolChoice),
		ThinkingEnabled:     cloneBoolPtr(in.ThinkingEnabled),
		ParentGenerationIDs: cloneStringSlice(in.ParentGenerationIDs),
		EffectiveVersion:    strings.Clone(in.EffectiveVersion),
		Tags:                cloneTags(in.Tags),
		Metadata:            cloneMetadata(in.Metadata),
		StartedAt:           in.StartedAt,
		ContentCapture:      in.ContentCapture,
	}
}

func cloneModelRef(in ModelRef) ModelRef {
	return ModelRef{
		Provider: strings.Clone(in.Provider),
		Name:     strings.Clone(in.Name),
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
	out := strings.Clone(*in)
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
			Role:  Role(strings.Clone(string(in[i].Role))),
			Name:  strings.Clone(in[i].Name),
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
			Kind:     PartKind(strings.Clone(string(in[i].Kind))),
			Text:     strings.Clone(in[i].Text),
			Thinking: strings.Clone(in[i].Thinking),
			Metadata: PartMetadata{ProviderType: strings.Clone(in[i].Metadata.ProviderType)},
		}

		if in[i].ToolCall != nil {
			call := *in[i].ToolCall
			call.ID = strings.Clone(call.ID)
			call.Name = strings.Clone(call.Name)
			call.InputJSON = slices.Clone(call.InputJSON)
			out[i].ToolCall = &call
		}

		if in[i].ToolResult != nil {
			result := *in[i].ToolResult
			result.ToolCallID = strings.Clone(result.ToolCallID)
			result.Name = strings.Clone(result.Name)
			result.Content = strings.Clone(result.Content)
			result.ContentJSON = slices.Clone(result.ContentJSON)
			out[i].ToolResult = &result
		}

		if in[i].Media != nil {
			media := Media{
				Kind:     strings.Clone(in[i].Media.Kind),
				URL:      strings.Clone(in[i].Media.URL),
				MIMEType: strings.Clone(in[i].Media.MIMEType),
				Name:     strings.Clone(in[i].Media.Name),
			}
			out[i].Media = &media
		}
	}

	return out
}

func cloneTools(in []ToolDefinition) []ToolDefinition {
	if len(in) == 0 {
		return nil
	}

	out := slices.Clone(in)

	for i := range out {
		out[i].Name = strings.Clone(out[i].Name)
		out[i].Description = strings.Clone(out[i].Description)
		out[i].Type = strings.Clone(out[i].Type)
		out[i].InputSchema = slices.Clone(out[i].InputSchema)
	}

	return out
}

func cloneArtifacts(in []Artifact) []Artifact {
	if len(in) == 0 {
		return nil
	}

	out := slices.Clone(in)

	for i := range out {
		out[i].Kind = ArtifactKind(strings.Clone(string(out[i].Kind)))
		out[i].Name = strings.Clone(out[i].Name)
		out[i].ContentType = strings.Clone(out[i].ContentType)
		out[i].Payload = slices.Clone(out[i].Payload)
		out[i].RecordID = strings.Clone(out[i].RecordID)
		out[i].URI = strings.Clone(out[i].URI)
	}

	return out
}

func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	for key, value := range in {
		out[strings.Clone(key)] = strings.Clone(value)
	}

	return out
}

func cloneMetadata(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]any, len(in))
	for key, value := range in {
		if text, ok := value.(string); ok {
			value = strings.Clone(text)
		}
		out[strings.Clone(key)] = value
	}

	return out
}
