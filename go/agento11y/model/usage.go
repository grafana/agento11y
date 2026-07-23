package model

type TokenUsage struct {
	// InputTokens is fresh, non-cached input. Cache-inclusive provider adapters
	// subtract CacheReadInputTokens before setting this field.
	InputTokens           int64 `json:"input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	TotalTokens           int64 `json:"total_tokens,omitempty"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
	// ReasoningTokens is an explanatory sub-bucket and may overlap with
	// OutputTokens depending on provider semantics.
	ReasoningTokens int64 `json:"reasoning_tokens,omitempty"`
	// InputIsDisjoint marks that this usage already follows the disjoint
	// contract (InputTokens is fresh, cache buckets additive) because an
	// SDK-owned adapter produced it. Consumers must not re-derive fresh input
	// (e.g. subtract cache reads from input) when this is true. Manual
	// user-supplied usage leaves it false.
	InputIsDisjoint bool `json:"input_is_disjoint,omitempty"`
}

func (u TokenUsage) Normalize() TokenUsage {
	if u.TotalTokens != 0 {
		return u
	}

	u.TotalTokens = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheWriteInputTokens
	return u
}
