package model

import (
	"encoding/json"
	"strings"
)

// ModalityPartition preserves provider counts. Completeness is established
// only by a matching reported aggregate, never by a model-name heuristic.
func ModalityPartition(tokens map[string]int64, total int64) *ModalityTokenCounts {
	if tokens == nil {
		return nil
	}
	var sum int64
	for _, n := range tokens {
		sum += n
	}
	return &ModalityTokenCounts{Tokens: tokens, Complete: sum == total}
}

// ApplyModalityJSON augments known provider usage without changing its aggregate
// semantics. format is "gemini", "interactions", or "openai". Provider wrappers
// normalize aggregates first; manual instrumentation may use this helper too.
func ApplyModalityJSON(u TokenUsage, raw []byte, format string) TokenUsage {
	var data map[string]json.RawMessage
	if json.Unmarshal(raw, &data) != nil {
		return u
	}
	details := func(key string, total int64, thinking int64) *ModalityTokenCounts {
		value, ok := data[key]
		if !ok {
			return nil
		}
		tokens := map[string]int64{}
		if format == "openai" {
			var d map[string]json.RawMessage
			if json.Unmarshal(value, &d) != nil {
				return nil
			}
			for _, m := range []string{"text", "image", "audio", "video"} {
				var n int64
				if v, ok := d[m+"_tokens"]; ok && json.Unmarshal(v, &n) == nil {
					tokens[m] = n
				}
			}
		} else {
			var rows []map[string]json.RawMessage
			if json.Unmarshal(value, &rows) != nil {
				return nil
			}
			for _, row := range rows {
				var m string
				var n int64
				_ = json.Unmarshal(row["modality"], &m)
				key := "tokenCount"
				if format == "interactions" {
					key = "tokens"
				}
				if json.Unmarshal(row[key], &n) != nil {
					continue
				}
				tokens[strings.ToLower(m)] += n
			}
		}
		if thinking != 0 {
			tokens["text"] += thinking
		}
		return ModalityPartition(tokens, total)
	}
	switch format {
	case "gemini":
		u.InputByModality = details("promptTokensDetails", u.InputTokens, 0)
		u.OutputByModality = details("candidatesTokensDetails", u.OutputTokens, u.ReasoningTokens)
		u.CacheReadByModality = details("cacheTokensDetails", u.CacheReadInputTokens, 0)
	case "interactions":
		u.InputByModality = details("input_tokens_by_modality", u.InputTokens, 0)
		u.OutputByModality = details("output_tokens_by_modality", u.OutputTokens, u.ReasoningTokens)
		u.CacheReadByModality = details("cached_tokens_by_modality", u.CacheReadInputTokens, 0)
	case "openai":
		u.InputByModality = details("input_tokens_details", u.InputTokens, 0)
		u.OutputByModality = details("output_tokens_details", u.OutputTokens, 0)
		// Only populate cached modalities when the response actually supplies them.
		var input map[string]json.RawMessage
		if json.Unmarshal(data["input_tokens_details"], &input) == nil {
			if cached, ok := input["cached_tokens_details"]; ok {
				data["cached_tokens_details"] = cached
				u.CacheReadByModality = details("cached_tokens_details", u.CacheReadInputTokens, 0)
			}
		}
	}
	return u
}
