package agento11y

import (
	"github.com/grafana/agento11y/go/agento11y/codec"
	agento11yv1 "github.com/grafana/agento11y/go/proto/agento11y/v1"
)

// generationToProto translates the SDK's Generation value into the wire-level
// protobuf message. It delegates to codec.ToProto so callers that use
// the codec package directly get the same field mapping and effective_version
// hashing.
func generationToProto(g Generation) (*agento11yv1.Generation, error) {
	return codec.ToProto(g)
}

// workflowStepToProto translates the SDK's WorkflowStep value into the
// wire-level protobuf message.
func workflowStepToProto(step WorkflowStep) (*agento11yv1.WorkflowStep, error) {
	return codec.WorkflowStepToProto(step)
}
