package agento11y

import (
	"strings"

	"github.com/grafana/agento11y/go/agento11y/model"
)

// WorkflowStep describes one execution node in an agentic workflow.
type WorkflowStep = model.WorkflowStep

func cloneWorkflowStep(in WorkflowStep) WorkflowStep {
	return WorkflowStep{
		ID:                  strings.Clone(in.ID),
		ConversationID:      strings.Clone(in.ConversationID),
		StepName:            strings.Clone(in.StepName),
		Framework:           strings.Clone(in.Framework),
		StartedAt:           in.StartedAt,
		CompletedAt:         in.CompletedAt,
		InputState:          cloneMetadata(in.InputState),
		OutputState:         cloneMetadata(in.OutputState),
		Error:               strings.Clone(in.Error),
		Tags:                cloneTags(in.Tags),
		LinkedGenerationIDs: cloneStringSlice(in.LinkedGenerationIDs),
		ParentStepIDs:       cloneStringSlice(in.ParentStepIDs),
		AgentName:           strings.Clone(in.AgentName),
		AgentVersion:        strings.Clone(in.AgentVersion),
		TraceID:             strings.Clone(in.TraceID),
		SpanID:              strings.Clone(in.SpanID),
		Metadata:            cloneMetadata(in.Metadata),
	}
}
