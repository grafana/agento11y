// Package codec converts between the public model types and the wire-level
// agento11y v1 protobuf messages. It exists so producers can hand
// high-level Generation values to Agent Observability without re-implementing the SDK's
// field mapping or the effective_version hashing rule.
//
// Only the producer direction (ToProto) is implemented. A FromProto helper is
// not yet provided: today's SDK never decodes Generation values from the wire,
// and the cost of maintaining the reverse mapping (lossy fields, struct
// metadata) is not worth paying speculatively. Add FromProto when an actual
// consumer needs it.
package codec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/grafana/agento11y/go/agento11y/model"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ToProto converts a model.Generation into the wire-level proto used by
// the generation ingest service. The effective_version field is hashed with
// the canonical sha256:<hex> rule.
func ToProto(g model.Generation) (*agento11yv1.Generation, error) {
	metadata, err := metadataToStruct(g.Metadata)
	if err != nil {
		return nil, fmt.Errorf("map metadata: %w", err)
	}

	out := &agento11yv1.Generation{
		Id:             strings.Clone(g.ID),
		ConversationId: strings.Clone(g.ConversationID),
		AgentName:      strings.Clone(g.AgentName),
		AgentVersion:   strings.Clone(g.AgentVersion),
		OperationName:  strings.Clone(g.OperationName),
		Mode:           generationModeToProto(g.Mode),
		TraceId:        strings.Clone(g.TraceID),
		SpanId:         strings.Clone(g.SpanID),
		Model: &agento11yv1.ModelRef{
			Provider: strings.Clone(g.Model.Provider),
			Name:     strings.Clone(g.Model.Name),
		},
		ResponseId:          strings.Clone(g.ResponseID),
		ResponseModel:       strings.Clone(g.ResponseModel),
		SystemPrompt:        strings.Clone(g.SystemPrompt),
		Input:               messagesToProto(g.Input),
		Output:              messagesToProto(g.Output),
		Tools:               toolsToProto(g.Tools),
		Usage:               usageToProto(g.Usage),
		StopReason:          strings.Clone(g.StopReason),
		Tags:                cloneTags(g.Tags),
		Metadata:            metadata,
		RawArtifacts:        artifactsToProto(g.Artifacts),
		CallError:           strings.Clone(g.CallError),
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

// WorkflowStepToProto converts a model.WorkflowStep into the wire-level
// proto used by the workflow-step ingest service.
func WorkflowStepToProto(step model.WorkflowStep) (*agento11yv1.WorkflowStep, error) {
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

	out := &agento11yv1.WorkflowStep{
		Id:                  strings.Clone(step.ID),
		ConversationId:      strings.Clone(step.ConversationID),
		StepName:            strings.Clone(step.StepName),
		Framework:           strings.Clone(step.Framework),
		InputState:          inputState,
		OutputState:         outputState,
		Error:               strings.Clone(step.Error),
		Tags:                cloneTags(step.Tags),
		LinkedGenerationIds: cloneStringSlice(step.LinkedGenerationIDs),
		ParentStepIds:       cloneStringSlice(step.ParentStepIDs),
		AgentName:           strings.Clone(step.AgentName),
		AgentVersion:        strings.Clone(step.AgentVersion),
		TraceId:             strings.Clone(step.TraceID),
		SpanId:              strings.Clone(step.SpanID),
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

func generationModeToProto(mode model.GenerationMode) agento11yv1.GenerationMode {
	switch mode {
	case model.GenerationModeStream:
		return agento11yv1.GenerationMode_GENERATION_MODE_STREAM
	case model.GenerationModeSync:
		return agento11yv1.GenerationMode_GENERATION_MODE_SYNC
	default:
		return agento11yv1.GenerationMode_GENERATION_MODE_UNSPECIFIED
	}
}

func messagesToProto(messages []model.Message) []*agento11yv1.Message {
	if len(messages) == 0 {
		return nil
	}

	out := make([]*agento11yv1.Message, 0, len(messages))
	for i := range messages {
		out = append(out, &agento11yv1.Message{
			Role:  roleToProto(messages[i].Role),
			Name:  strings.Clone(messages[i].Name),
			Parts: partsToProto(messages[i].Parts),
		})
	}

	return out
}

func roleToProto(role model.Role) agento11yv1.MessageRole {
	switch role {
	case model.RoleUser:
		return agento11yv1.MessageRole_MESSAGE_ROLE_USER
	case model.RoleAssistant:
		return agento11yv1.MessageRole_MESSAGE_ROLE_ASSISTANT
	case model.RoleTool:
		return agento11yv1.MessageRole_MESSAGE_ROLE_TOOL
	default:
		return agento11yv1.MessageRole_MESSAGE_ROLE_UNSPECIFIED
	}
}

func partsToProto(parts []model.Part) []*agento11yv1.Part {
	if len(parts) == 0 {
		return nil
	}

	out := make([]*agento11yv1.Part, 0, len(parts))
	for i := range parts {
		part := &agento11yv1.Part{}
		if providerType := parts[i].Metadata.ProviderType; providerType != "" {
			part.Metadata = &agento11yv1.PartMetadata{ProviderType: strings.Clone(providerType)}
		}

		switch parts[i].Kind {
		case model.PartKindText:
			part.Payload = &agento11yv1.Part_Text{Text: strings.Clone(parts[i].Text)}
		case model.PartKindThinking:
			part.Payload = &agento11yv1.Part_Thinking{Thinking: strings.Clone(parts[i].Thinking)}
		case model.PartKindToolCall:
			if parts[i].ToolCall == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_ToolCall{ToolCall: &agento11yv1.ToolCall{
				Id:        strings.Clone(parts[i].ToolCall.ID),
				Name:      strings.Clone(parts[i].ToolCall.Name),
				InputJson: slices.Clone(parts[i].ToolCall.InputJSON),
			}}
		case model.PartKindToolResult:
			if parts[i].ToolResult == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{
				ToolCallId:  strings.Clone(parts[i].ToolResult.ToolCallID),
				Name:        strings.Clone(parts[i].ToolResult.Name),
				Content:     strings.Clone(parts[i].ToolResult.Content),
				ContentJson: slices.Clone(parts[i].ToolResult.ContentJSON),
				IsError:     parts[i].ToolResult.IsError,
			}}
		case model.PartKindMedia:
			if parts[i].Media == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_Media{Media: &agento11yv1.Media{
				Kind:     strings.Clone(parts[i].Media.Kind),
				Url:      strings.Clone(parts[i].Media.URL),
				MimeType: strings.Clone(parts[i].Media.MIMEType),
				Name:     strings.Clone(parts[i].Media.Name),
			}}
		}

		out = append(out, part)
	}
	return out
}

func toolsToProto(tools []model.ToolDefinition) []*agento11yv1.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}

	out := make([]*agento11yv1.ToolDefinition, 0, len(tools))
	for i := range tools {
		out = append(out, &agento11yv1.ToolDefinition{
			Name:            strings.Clone(tools[i].Name),
			Description:     strings.Clone(tools[i].Description),
			Type:            strings.Clone(tools[i].Type),
			InputSchemaJson: slices.Clone(tools[i].InputSchema),
			Deferred:        tools[i].Deferred,
		})
	}
	return out
}

func usageToProto(usage model.TokenUsage) *agento11yv1.TokenUsage {
	return &agento11yv1.TokenUsage{
		InputTokens:           usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		TotalTokens:           usage.TotalTokens,
		CacheReadInputTokens:  usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheWriteInputTokens,
		ReasoningTokens:       usage.ReasoningTokens,
	}
}

func artifactsToProto(artifacts []model.Artifact) []*agento11yv1.Artifact {
	if len(artifacts) == 0 {
		return nil
	}

	out := make([]*agento11yv1.Artifact, 0, len(artifacts))
	for i := range artifacts {
		out = append(out, &agento11yv1.Artifact{
			Kind:        artifactKindToProto(artifacts[i].Kind),
			Name:        strings.Clone(artifacts[i].Name),
			ContentType: strings.Clone(artifacts[i].ContentType),
			Payload:     slices.Clone(artifacts[i].Payload),
			RecordId:    strings.Clone(artifacts[i].RecordID),
			Uri:         strings.Clone(artifacts[i].URI),
		})
	}
	return out
}

func artifactKindToProto(kind model.ArtifactKind) agento11yv1.ArtifactKind {
	switch kind {
	case model.ArtifactKindRequest:
		return agento11yv1.ArtifactKind_ARTIFACT_KIND_REQUEST
	case model.ArtifactKindResponse:
		return agento11yv1.ArtifactKind_ARTIFACT_KIND_RESPONSE
	case model.ArtifactKindTools:
		return agento11yv1.ArtifactKind_ARTIFACT_KIND_TOOLS
	case model.ArtifactKindProviderEvent:
		return agento11yv1.ArtifactKind_ARTIFACT_KIND_PROVIDER_EVENT
	default:
		return agento11yv1.ArtifactKind_ARTIFACT_KIND_UNSPECIFIED
	}
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

func cloneStringSlice(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	for i := range in {
		out[i] = strings.Clone(in[i])
	}
	return out
}
