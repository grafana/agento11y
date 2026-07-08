// Package sigilcodec converts between the public sigilmodel types and the
// wire-level sigilv1 protobuf messages. It exists so producers can hand
// high-level Generation values to Sigil without re-implementing the SDK's
// field mapping or the effective_version hashing rule.
//
// Only the producer direction (ToProto) is implemented. A FromProto helper is
// not yet provided: today's SDK never decodes Generation values from the wire,
// and the cost of maintaining the reverse mapping (lossy fields, struct
// metadata) is not worth paying speculatively. Add FromProto when an actual
// consumer needs it.
package sigilcodec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	sigilv1 "github.com/grafana/sigil-sdk/go/proto/sigil/v1"
	"github.com/grafana/sigil-sdk/go/sigil/sigilmodel"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProto converts a sigilmodel.Generation into the wire-level proto used by
// the generation ingest service. The effective_version field is hashed with
// the canonical sha256:<hex> rule.
func ToProto(g sigilmodel.Generation) (*sigilv1.Generation, error) {
	metadata, err := metadataToStruct(g.Metadata)
	if err != nil {
		return nil, fmt.Errorf("map metadata: %w", err)
	}

	out := &sigilv1.Generation{
		Id:             g.ID,
		ConversationId: g.ConversationID,
		AgentName:      g.AgentName,
		AgentVersion:   g.AgentVersion,
		OperationName:  g.OperationName,
		Mode:           generationModeToProto(g.Mode),
		TraceId:        g.TraceID,
		SpanId:         g.SpanID,
		Model: &sigilv1.ModelRef{
			Provider: g.Model.Provider,
			Name:     g.Model.Name,
		},
		ResponseId:          g.ResponseID,
		ResponseModel:       g.ResponseModel,
		SystemPrompt:        g.SystemPrompt,
		Input:               messagesToProto(g.Input),
		Output:              messagesToProto(g.Output),
		Tools:               toolsToProto(g.Tools),
		Usage:               usageToProto(g.Usage),
		StopReason:          g.StopReason,
		Tags:                cloneTags(g.Tags),
		Metadata:            metadata,
		RawArtifacts:        artifactsToProto(g.Artifacts),
		CallError:           g.CallError,
		MaxTokens:           cloneInt64Ptr(g.MaxTokens),
		Temperature:         cloneFloat64Ptr(g.Temperature),
		TopP:                cloneFloat64Ptr(g.TopP),
		ToolChoice:          cloneStringPtr(g.ToolChoice),
		ThinkingEnabled:     cloneBoolPtr(g.ThinkingEnabled),
		ParentGenerationIds: cloneStringSlice(g.ParentGenerationIDs),
	}

	if trimmed := strings.TrimSpace(g.EffectiveVersion); trimmed != "" {
		sum := sha256.Sum256([]byte(trimmed))
		out.EffectiveVersion = proto.String("sha256:" + hex.EncodeToString(sum[:]))
	}

	if !g.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(g.StartedAt)
	}
	if !g.CompletedAt.IsZero() {
		out.CompletedAt = timestamppb.New(g.CompletedAt)
	}

	return out, nil
}

// WorkflowStepToProto converts a sigilmodel.WorkflowStep into the wire-level
// proto used by the workflow-step ingest service.
func WorkflowStepToProto(step sigilmodel.WorkflowStep) (*sigilv1.WorkflowStep, error) {
	inputState, err := metadataToStruct(step.InputState)
	if err != nil {
		return nil, fmt.Errorf("map input_state: %w", err)
	}
	outputState, err := metadataToStruct(step.OutputState)
	if err != nil {
		return nil, fmt.Errorf("map output_state: %w", err)
	}
	metadata, err := metadataToStruct(step.Metadata)
	if err != nil {
		return nil, fmt.Errorf("map metadata: %w", err)
	}

	out := &sigilv1.WorkflowStep{
		Id:                  step.ID,
		ConversationId:      step.ConversationID,
		StepName:            step.StepName,
		Framework:           step.Framework,
		InputState:          inputState,
		OutputState:         outputState,
		Error:               step.Error,
		Tags:                cloneTags(step.Tags),
		LinkedGenerationIds: cloneStringSlice(step.LinkedGenerationIDs),
		ParentStepIds:       cloneStringSlice(step.ParentStepIDs),
		AgentName:           step.AgentName,
		AgentVersion:        step.AgentVersion,
		TraceId:             step.TraceID,
		SpanId:              step.SpanID,
		Metadata:            metadata,
	}
	if !step.StartedAt.IsZero() {
		out.StartedAt = timestamppb.New(step.StartedAt)
	}
	if !step.CompletedAt.IsZero() {
		out.CompletedAt = timestamppb.New(step.CompletedAt)
	}
	return out, nil
}

func metadataToStruct(metadata map[string]any) (*structpb.Struct, error) {
	if len(metadata) == 0 {
		return nil, nil
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}

	normalized := map[string]any{}
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return nil, err
	}

	return structpb.NewStruct(normalized)
}

func generationModeToProto(mode sigilmodel.GenerationMode) sigilv1.GenerationMode {
	switch mode {
	case sigilmodel.GenerationModeStream:
		return sigilv1.GenerationMode_GENERATION_MODE_STREAM
	case sigilmodel.GenerationModeSync:
		return sigilv1.GenerationMode_GENERATION_MODE_SYNC
	default:
		return sigilv1.GenerationMode_GENERATION_MODE_UNSPECIFIED
	}
}

func messagesToProto(messages []sigilmodel.Message) []*sigilv1.Message {
	if len(messages) == 0 {
		return nil
	}

	out := make([]*sigilv1.Message, 0, len(messages))
	for i := range messages {
		out = append(out, &sigilv1.Message{
			Role:  roleToProto(messages[i].Role),
			Name:  messages[i].Name,
			Parts: partsToProto(messages[i].Parts),
		})
	}

	return out
}

func roleToProto(role sigilmodel.Role) sigilv1.MessageRole {
	switch role {
	case sigilmodel.RoleUser:
		return sigilv1.MessageRole_MESSAGE_ROLE_USER
	case sigilmodel.RoleAssistant:
		return sigilv1.MessageRole_MESSAGE_ROLE_ASSISTANT
	case sigilmodel.RoleTool:
		return sigilv1.MessageRole_MESSAGE_ROLE_TOOL
	default:
		return sigilv1.MessageRole_MESSAGE_ROLE_UNSPECIFIED
	}
}

func partsToProto(parts []sigilmodel.Part) []*sigilv1.Part {
	if len(parts) == 0 {
		return nil
	}

	out := make([]*sigilv1.Part, 0, len(parts))
	for i := range parts {
		part := &sigilv1.Part{}
		if providerType := parts[i].Metadata.ProviderType; providerType != "" {
			part.Metadata = &sigilv1.PartMetadata{ProviderType: providerType}
		}

		switch parts[i].Kind {
		case sigilmodel.PartKindText:
			part.Payload = &sigilv1.Part_Text{Text: parts[i].Text}
		case sigilmodel.PartKindThinking:
			part.Payload = &sigilv1.Part_Thinking{Thinking: parts[i].Thinking}
		case sigilmodel.PartKindToolCall:
			if parts[i].ToolCall == nil {
				continue
			}
			part.Payload = &sigilv1.Part_ToolCall{ToolCall: &sigilv1.ToolCall{
				Id:        parts[i].ToolCall.ID,
				Name:      parts[i].ToolCall.Name,
				InputJson: append([]byte(nil), parts[i].ToolCall.InputJSON...),
			}}
		case sigilmodel.PartKindToolResult:
			if parts[i].ToolResult == nil {
				continue
			}
			part.Payload = &sigilv1.Part_ToolResult{ToolResult: &sigilv1.ToolResult{
				ToolCallId:  parts[i].ToolResult.ToolCallID,
				Name:        parts[i].ToolResult.Name,
				Content:     parts[i].ToolResult.Content,
				ContentJson: append([]byte(nil), parts[i].ToolResult.ContentJSON...),
				IsError:     parts[i].ToolResult.IsError,
			}}
		}

		out = append(out, part)
	}
	return out
}

func toolsToProto(tools []sigilmodel.ToolDefinition) []*sigilv1.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}

	out := make([]*sigilv1.ToolDefinition, 0, len(tools))
	for i := range tools {
		out = append(out, &sigilv1.ToolDefinition{
			Name:            tools[i].Name,
			Description:     tools[i].Description,
			Type:            tools[i].Type,
			InputSchemaJson: append([]byte(nil), tools[i].InputSchema...),
			Deferred:        tools[i].Deferred,
		})
	}
	return out
}

func usageToProto(usage sigilmodel.TokenUsage) *sigilv1.TokenUsage {
	return &sigilv1.TokenUsage{
		InputTokens:           usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		TotalTokens:           usage.TotalTokens,
		CacheReadInputTokens:  usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens,
		ReasoningTokens:       usage.ReasoningTokens,
	}
}

func artifactsToProto(artifacts []sigilmodel.Artifact) []*sigilv1.Artifact {
	if len(artifacts) == 0 {
		return nil
	}

	out := make([]*sigilv1.Artifact, 0, len(artifacts))
	for i := range artifacts {
		out = append(out, &sigilv1.Artifact{
			Kind:        artifactKindToProto(artifacts[i].Kind),
			Name:        artifacts[i].Name,
			ContentType: artifacts[i].ContentType,
			Payload:     append([]byte(nil), artifacts[i].Payload...),
			RecordId:    artifacts[i].RecordID,
			Uri:         artifacts[i].URI,
		})
	}
	return out
}

func artifactKindToProto(kind sigilmodel.ArtifactKind) sigilv1.ArtifactKind {
	switch kind {
	case sigilmodel.ArtifactKindRequest:
		return sigilv1.ArtifactKind_ARTIFACT_KIND_REQUEST
	case sigilmodel.ArtifactKindResponse:
		return sigilv1.ArtifactKind_ARTIFACT_KIND_RESPONSE
	case sigilmodel.ArtifactKindTools:
		return sigilv1.ArtifactKind_ARTIFACT_KIND_TOOLS
	case sigilmodel.ArtifactKindProviderEvent:
		return sigilv1.ArtifactKind_ARTIFACT_KIND_PROVIDER_EVENT
	default:
		return sigilv1.ArtifactKind_ARTIFACT_KIND_UNSPECIFIED
	}
}

func cloneTags(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
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

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}
