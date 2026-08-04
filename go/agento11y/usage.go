package agento11y

import "github.com/grafana/agento11y/go/agento11y/model"

type TokenUsage = model.TokenUsage

// TokenInputSemantics re-exports the model enum so callers can mark usage
// without importing the model package directly.
type TokenInputSemantics = model.TokenInputSemantics

const (
	TokenInputSemanticsUnspecified = model.TokenInputSemanticsUnspecified
	TokenInputSemanticsInclusive   = model.TokenInputSemanticsInclusive
)
