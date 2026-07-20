package sigil

import (
	"errors"
	"strings"

	"github.com/grafana/agento11y/go/sigil/sigilmodel"
)

// ValidateGeneration enforces the Sigil generation invariants required by the
// ingest pipeline. It delegates to sigilmodel so external callers that work
// with sigilmodel.Generation directly see the same checks.
func ValidateGeneration(g Generation) error {
	return sigilmodel.ValidateGeneration(g)
}

// ValidateWorkflowStep enforces the Sigil workflow-step invariants required by
// the ingest pipeline.
func ValidateWorkflowStep(step WorkflowStep) error {
	return sigilmodel.ValidateWorkflowStep(step)
}

func ValidateEmbeddingStart(start EmbeddingStart) error {
	if strings.TrimSpace(start.Model.Provider) == "" {
		return errors.New("embedding.model.provider is required")
	}
	if strings.TrimSpace(start.Model.Name) == "" {
		return errors.New("embedding.model.name is required")
	}
	if start.Dimensions != nil && *start.Dimensions <= 0 {
		return errors.New("embedding.dimensions must be > 0")
	}
	if start.EncodingFormat != "" && strings.TrimSpace(start.EncodingFormat) == "" {
		return errors.New("embedding.encoding_format must not be blank")
	}
	return nil
}

func ValidateEmbeddingResult(result EmbeddingResult) error {
	if result.InputCount < 0 {
		return errors.New("embedding.input_count must be >= 0")
	}
	if result.InputTokens < 0 {
		return errors.New("embedding.input_tokens must be >= 0")
	}
	if result.Dimensions != nil && *result.Dimensions <= 0 {
		return errors.New("embedding.dimensions must be > 0")
	}
	return nil
}
