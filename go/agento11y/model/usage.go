package model

// TokenInputSemantics declares what InputTokens covers, mirroring
// agento11y.v1.TokenInputSemantics.
type TokenInputSemantics int32

const (
	// TokenInputSemanticsUnspecified means provider-raw or legacy telemetry:
	// InputTokens follows whatever convention the provider used, and
	// consumers may fall back to provider-name heuristics.
	TokenInputSemanticsUnspecified TokenInputSemantics = 0
	// TokenInputSemanticsInclusive means the OTel GenAI contract: InputTokens
	// includes all input token types — both cache buckets are subsets of it,
	// never additions — and fresh input is derived as
	// input - cache_read - cache_write.
	TokenInputSemanticsInclusive TokenInputSemantics = 1
)

type TokenUsage struct {
	// InputTokens is the prompt-side token count. Under
	// TokenInputSemanticsInclusive it includes both cache buckets, per the
	// OTel GenAI semantic conventions.
	InputTokens           int64 `json:"input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	TotalTokens           int64 `json:"total_tokens,omitempty"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
	// ReasoningTokens is an explanatory sub-bucket of OutputTokens when the
	// provider reports it, never an additive bucket.
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// InputSemantics marks which contract InputTokens follows. It is set only
	// by SDK adapters that positively identified the provider payload shape;
	// manual user-supplied usage leaves it unspecified.
	InputSemantics TokenInputSemantics `json:"input_semantics,omitempty"`
}

func (u TokenUsage) Normalize() TokenUsage {
	if u.TotalTokens != 0 {
		return u
	}

	u.TotalTokens = u.InputTokens + u.OutputTokens
	return u
}
