package sigil

import "github.com/grafana/sigil-sdk/go/sigil/sigilmodel"

// WorkflowStep describes one execution node in an agentic workflow.
type WorkflowStep = sigilmodel.WorkflowStep

func cloneWorkflowStep(in WorkflowStep) WorkflowStep {
	return WorkflowStep{
		ID:                  in.ID,
		ConversationID:      in.ConversationID,
		StepName:            in.StepName,
		Framework:           in.Framework,
		StartedAt:           in.StartedAt,
		CompletedAt:         in.CompletedAt,
		InputState:          cloneMetadata(in.InputState),
		OutputState:         cloneMetadata(in.OutputState),
		Error:               in.Error,
		Tags:                cloneTags(in.Tags),
		LinkedGenerationIDs: cloneStringSlice(in.LinkedGenerationIDs),
		ParentStepIDs:       cloneStringSlice(in.ParentStepIDs),
		AgentName:           in.AgentName,
		AgentVersion:        in.AgentVersion,
		TraceID:             in.TraceID,
		SpanID:              in.SpanID,
		Metadata:            cloneMetadata(in.Metadata),
	}
}
