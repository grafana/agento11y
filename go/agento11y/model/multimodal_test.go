package model

import "testing"

func TestModalityProviderUsage(t *testing.T) {
	u := ApplyModalityJSON(TokenUsage{InputTokens: 100, OutputTokens: 12, ReasoningTokens: 2}, []byte(`{"promptTokensDetails":[{"modality":"TEXT","tokenCount":100}],"candidatesTokensDetails":[{"modality":"IMAGE","tokenCount":10}]}`), "gemini")
	if !u.InputByModality.Complete || !u.OutputByModality.Complete || u.OutputByModality.Tokens["text"] != 2 {
		t.Fatalf("lost partition: %+v", u)
	}
	u = ApplyModalityJSON(TokenUsage{InputTokens: 100, OutputTokens: 10, CacheReadInputTokens: 20}, []byte(`{"input_tokens_details":{"text_tokens":40,"image_tokens":60,"cached_tokens":20},"output_tokens_details":{"image_tokens":10}}`), "openai")
	if !u.InputByModality.Complete || u.CacheReadByModality != nil {
		t.Fatalf("must not infer cache partition: %+v", u)
	}
}
