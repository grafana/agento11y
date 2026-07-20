package model

type TokenUsage struct {
	InputTokens           int64 `json:"input_tokens,omitempty"`
	OutputTokens          int64 `json:"output_tokens,omitempty"`
	TotalTokens           int64 `json:"total_tokens,omitempty"`
	CacheReadInputTokens  int64 `json:"cache_read_input_tokens,omitempty"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens,omitempty"`
	ReasoningTokens       int64 `json:"reasoning_tokens,omitempty"`
}

func (u TokenUsage) Normalize() TokenUsage {
	if u.TotalTokens != 0 {
		return u
	}

	u.TotalTokens = u.InputTokens + u.OutputTokens
	return u
}
