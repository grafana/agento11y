// Package codec converts between the public model types and the wire-level
// agento11y v1 protobuf messages. It exists so producers can hand
// high-level Generation values to Sigil without re-implementing the SDK's
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
	"maps"
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
		Id:             g.ID,
		ConversationId: g.ConversationID,
		AgentName:      g.AgentName,
		AgentVersion:   g.AgentVersion,
		OperationName:  g.OperationName,
		Mode:           generationModeToProto(g.Mode),
		TraceId:        g.TraceID,
		SpanId:         g.SpanID,
		Model: &agento11yv1.ModelRef{
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
			Name:  messages[i].Name,
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
			part.Metadata = &agento11yv1.PartMetadata{ProviderType: providerType}
		}

		switch parts[i].Kind {
		case model.PartKindText:
			part.Payload = &agento11yv1.Part_Text{Text: parts[i].Text}
		case model.PartKindThinking:
			part.Payload = &agento11yv1.Part_Thinking{Thinking: parts[i].Thinking}
		case model.PartKindToolCall:
			if parts[i].ToolCall == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_ToolCall{ToolCall: &agento11yv1.ToolCall{
				Id:        parts[i].ToolCall.ID,
				Name:      parts[i].ToolCall.Name,
				InputJson: append([]byte(nil), parts[i].ToolCall.InputJSON...),
			}}
		case model.PartKindToolResult:
			if parts[i].ToolResult == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_ToolResult{ToolResult: &agento11yv1.ToolResult{
				ToolCallId:  parts[i].ToolResult.ToolCallID,
				Name:        parts[i].ToolResult.Name,
				Content:     parts[i].ToolResult.Content,
				ContentJson: append([]byte(nil), parts[i].ToolResult.ContentJSON...),
				IsError:     parts[i].ToolResult.IsError,
			}}
		case model.PartKindMedia:
			if parts[i].Media == nil {
				continue
			}
			part.Payload = &agento11yv1.Part_Media{Media: &agento11yv1.Media{
				Kind:     parts[i].Media.Kind,
				Url:      parts[i].Media.URL,
				MimeType: parts[i].Media.MIMEType,
				Name:     parts[i].Media.Name,
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
			Name:            tools[i].Name,
			Description:     tools[i].Description,
			Type:            tools[i].Type,
			InputSchemaJson: append([]byte(nil), tools[i].InputSchema...),
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
		InputIsDisjoint:       usage.InputIsDisjoint,
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
			Name:        artifacts[i].Name,
			ContentType: artifacts[i].ContentType,
			Payload:     append([]byte(nil), artifacts[i].Payload...),
			RecordId:    artifacts[i].RecordID,
			Uri:         artifacts[i].URI,
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
