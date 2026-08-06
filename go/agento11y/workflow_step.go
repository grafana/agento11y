package agento11y

import "github.com/grafana/agento11y/go/agento11y/model"

// WorkflowStep describes one execution node in an agentic workflow.
type WorkflowStep = model.WorkflowStep

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
