package agento11y

import "github.com/grafana/agento11y/go/agento11y/model"

type TokenUsage = model.TokenUsage
type ModalityTokenCounts = model.ModalityTokenCounts

// TokenInputSemantics re-exports the model enum so callers can mark usage
// without importing the model package directly.
type TokenInputSemantics = model.TokenInputSemantics

const (
	TokenInputSemanticsUnspecified = model.TokenInputSemanticsUnspecified
	TokenInputSemanticsInclusive   = model.TokenInputSemanticsInclusive
)

// ApplyModalityJSON maps a positively identified provider's usage details.
// Formats are gemini, interactions and openai; aggregates must already follow
// inclusive semantics. It never infers usage from media or model names.
func ApplyModalityJSON(u TokenUsage, raw []byte, format string) TokenUsage {
	return model.ApplyModalityJSON(u, raw, format)
}
