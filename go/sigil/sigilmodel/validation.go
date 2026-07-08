package sigilmodel

import (
	"errors"
	"fmt"
	"strings"
)

// MetadataKeyContentCaptureMode is the generation metadata key the SDK uses
// to record which content-capture mode produced the generation. Stripped
// validation reads this to allow blank text/thinking parts.
const MetadataKeyContentCaptureMode = "sigil.sdk.content_capture_mode"

// ContentCaptureModeMetadataOnly is the metadata marker value indicating
// the generation has been stripped down to metadata.
const ContentCaptureModeMetadataOnly = "metadata_only"

// Validate enforces the Sigil generation invariants required by the ingest
// pipeline.
func (g Generation) Validate() error {
	return ValidateGeneration(g)
}

// Validate enforces the workflow-step invariants required by ingest.
func (s WorkflowStep) Validate() error {
	return ValidateWorkflowStep(s)
}

// ValidateGeneration is the package-level form of Generation.Validate.
func ValidateGeneration(g Generation) error {
	contentStripped := isContentStripped(g)
	if g.Mode != "" && g.Mode != GenerationModeSync && g.Mode != GenerationModeStream {
		return errors.New("generation.mode must be one of SYNC|STREAM")
	}

	if strings.TrimSpace(g.Model.Provider) == "" {
		return errors.New("generation.model.provider is required")
	}

	if strings.TrimSpace(g.Model.Name) == "" {
		return errors.New("generation.model.name is required")
	}

	for i := range g.Input {
		if err := validateMessage("generation.input", i, g.Input[i], contentStripped); err != nil {
			return err
		}
	}

	for i := range g.Output {
		if err := validateMessage("generation.output", i, g.Output[i], contentStripped); err != nil {
			return err
		}
	}

	for i := range g.Tools {
		if strings.TrimSpace(g.Tools[i].Name) == "" {
			return fmt.Errorf("generation.tools[%d].name is required", i)
		}
	}

	for i := range g.Artifacts {
		if err := validateArtifact(i, g.Artifacts[i]); err != nil {
			return err
		}
	}

	return nil
}

// ValidateWorkflowStep is the package-level form of WorkflowStep.Validate.
func ValidateWorkflowStep(step WorkflowStep) error {
	if strings.TrimSpace(step.ID) == "" {
		return errors.New("workflow step id is required")
	}
	if strings.TrimSpace(step.ConversationID) == "" {
		return errors.New("workflow step conversation_id is required")
	}
	if strings.TrimSpace(step.StepName) == "" {
		return errors.New("workflow step step_name is required")
	}
	if !step.StartedAt.IsZero() && !step.CompletedAt.IsZero() && step.CompletedAt.Before(step.StartedAt) {
		return errors.New("workflow step completed_at must not be earlier than started_at")
	}
	return nil
}

func isContentStripped(g Generation) bool {
	if g.Metadata == nil {
		return false
	}
	v, _ := g.Metadata[MetadataKeyContentCaptureMode].(string)
	return v == ContentCaptureModeMetadataOnly
}

func validateMessage(path string, index int, message Message, contentStripped bool) error {
	switch message.Role {
	case RoleUser, RoleAssistant, RoleTool:
	default:
		return fmt.Errorf("%s[%d].role must be one of user|assistant|tool", path, index)
	}

	if len(message.Parts) == 0 {
		return fmt.Errorf("%s[%d].parts must not be empty", path, index)
	}

	for i := range message.Parts {
		if err := validatePart(path, index, i, message.Role, message.Parts[i], contentStripped); err != nil {
			return err
		}
	}

	return nil
}

func validatePart(path string, messageIndex, partIndex int, role Role, part Part, contentStripped bool) error {
	switch part.Kind {
	case PartKindText, PartKindThinking, PartKindToolCall, PartKindToolResult:
	default:
		return fmt.Errorf("%s[%d].parts[%d].kind is invalid", path, messageIndex, partIndex)
	}

	fieldCount := 0
	if part.Text != "" {
		fieldCount++
	}
	if part.Thinking != "" {
		fieldCount++
	}
	if part.ToolCall != nil {
		fieldCount++
	}
	if part.ToolResult != nil {
		fieldCount++
	}

	// Stripped text/thinking parts have empty payloads — that's expected.
	// ToolCall/ToolResult keep their struct pointers after stripping so fieldCount is still 1.
	strippedTextOrThinking := contentStripped && (part.Kind == PartKindText || part.Kind == PartKindThinking)
	if fieldCount != 1 && !strippedTextOrThinking {
		return fmt.Errorf("%s[%d].parts[%d] must set exactly one payload field", path, messageIndex, partIndex)
	}

	switch part.Kind {
	case PartKindText:
		if !contentStripped && part.Text == "" {
			return fmt.Errorf("%s[%d].parts[%d].text is required", path, messageIndex, partIndex)
		}
	case PartKindThinking:
		if role != RoleAssistant {
			return fmt.Errorf("%s[%d].parts[%d].thinking only allowed for assistant role", path, messageIndex, partIndex)
		}
		if !contentStripped && part.Thinking == "" {
			return fmt.Errorf("%s[%d].parts[%d].thinking is required", path, messageIndex, partIndex)
		}
	case PartKindToolCall:
		if role != RoleAssistant {
			return fmt.Errorf("%s[%d].parts[%d].tool_call only allowed for assistant role", path, messageIndex, partIndex)
		}
		if part.ToolCall == nil || strings.TrimSpace(part.ToolCall.Name) == "" {
			return fmt.Errorf("%s[%d].parts[%d].tool_call.name is required", path, messageIndex, partIndex)
		}
	case PartKindToolResult:
		if role != RoleTool {
			return fmt.Errorf("%s[%d].parts[%d].tool_result only allowed for tool role", path, messageIndex, partIndex)
		}
		if part.ToolResult == nil {
			return fmt.Errorf("%s[%d].parts[%d].tool_result is required", path, messageIndex, partIndex)
		}
		if strings.TrimSpace(part.ToolResult.ToolCallID) == "" && strings.TrimSpace(part.ToolResult.Name) == "" {
			return fmt.Errorf("%s[%d].parts[%d].tool_result.tool_call_id or name is required", path, messageIndex, partIndex)
		}
	}

	return nil
}

func validateArtifact(index int, artifact Artifact) error {
	switch artifact.Kind {
	case ArtifactKindRequest, ArtifactKindResponse, ArtifactKindTools, ArtifactKindProviderEvent:
	default:
		return fmt.Errorf("generation.artifacts[%d].kind is invalid", index)
	}

	if strings.TrimSpace(artifact.RecordID) == "" && len(artifact.Payload) == 0 {
		return fmt.Errorf("generation.artifacts[%d] must provide payload or record_id", index)
	}

	return nil
}
