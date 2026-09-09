package agento11y

import (
	"testing"
	"time"
)

func TestTokenUsageNormalize(t *testing.T) {
	tests := []struct {
		name  string
		usage TokenUsage
		want  int64
	}{
		{name: "legacy nonzero counters", usage: TokenUsage{InputTokens: 7, OutputTokens: 3}, want: 10},
		{name: "explicit reported output zero", usage: TokenUsage{InputTokens: 7, OutputTokensReported: true}, want: 7},
		{name: "explicit reported input zero", usage: TokenUsage{InputTokensReported: true, OutputTokens: 3}, want: 3},
		{name: "unknown output does not make a partial total", usage: TokenUsage{InputTokens: 7}, want: 0},
		{name: "unknown input does not make a partial total", usage: TokenUsage{OutputTokens: 3}, want: 0},
		{name: "explicit total is preserved", usage: TokenUsage{InputTokens: 7, TotalTokens: 11}, want: 11},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.usage.Normalize().TotalTokens; got != tt.want {
				t.Errorf("TotalTokens = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGenerationSystemPromptFallback(t *testing.T) {
	tests := []struct {
		name       string
		generation Generation
		want       string
	}{
		{name: "no instructions uses seed", want: "seed"},
		{
			name:       "user history uses seed",
			generation: Generation{Input: []Message{UserTextMessage("question")}},
			want:       "seed",
		},
		{
			name:       "result prompt overrides seed",
			generation: Generation{SystemPrompt: "result"},
			want:       "result",
		},
		{
			name:       "matching system history replaces seed",
			generation: Generation{Input: []Message{{Role: RoleSystem, Parts: []Part{TextPart("seed")}}}},
		},
		{
			name:       "different system history replaces seed",
			generation: Generation{Input: []Message{{Role: RoleSystem, Parts: []Part{TextPart("result")}}}},
		},
		{
			name:       "developer history replaces seed",
			generation: Generation{Input: []Message{{Role: RoleDeveloper, Parts: []Part{TextPart("seed")}}}},
		},
		{
			name: "explicit result prompt and history stay separate",
			generation: Generation{
				SystemPrompt: "result",
				Input:        []Message{{Role: RoleSystem, Parts: []Part{TextPart("history")}}},
			},
			want: "result",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := GenerationRecorder{seed: GenerationStart{SystemPrompt: "seed"}}
			got := rec.normalizeGeneration(tt.generation, time.Time{}, nil, nil)
			if got.SystemPrompt != tt.want {
				t.Errorf("SystemPrompt = %q, want %q", got.SystemPrompt, tt.want)
			}
		})
	}
}

func TestNormalizeGenerationFinishReason(t *testing.T) {
	tests := []struct {
		name       string
		generation Generation
		wantStop   string
		wantFinish string
	}{
		{
			name:       "legacy stop reason fills first output",
			generation: Generation{StopReason: "stop", Output: []Message{{Role: RoleAssistant}}},
			wantStop:   "stop",
			wantFinish: "stop",
		},
		{
			name:       "first output is source of truth",
			generation: Generation{StopReason: "legacy", Output: []Message{{Role: RoleAssistant, FinishReason: "length"}}},
			wantStop:   "length",
			wantFinish: "length",
		},
		{
			name:       "message-only reason fills legacy field",
			generation: Generation{Output: []Message{{Role: RoleAssistant, FinishReason: "tool_calls"}}},
			wantStop:   "tool_calls",
			wantFinish: "tool_calls",
		},
		{
			name: "first non-empty candidate reason fills legacy field",
			generation: Generation{Output: []Message{
				{Role: RoleAssistant},
				{Role: RoleAssistant, FinishReason: "length"},
			}},
			wantStop:   "length",
			wantFinish: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			normalizeGenerationFinishReason(&tt.generation)
			if tt.generation.StopReason != tt.wantStop {
				t.Errorf("StopReason = %q, want %q", tt.generation.StopReason, tt.wantStop)
			}
			if got := tt.generation.Output[0].FinishReason; got != tt.wantFinish {
				t.Errorf("Output[0].FinishReason = %q, want %q", got, tt.wantFinish)
			}
		})
	}
}

func TestCloneGenerationClonesMediaParts(t *testing.T) {
	topK := int64(40)
	choiceCount := int64(2)
	seed := int64(7)
	outputType := "json"
	responseStatus := "completed"
	original := Generation{
		TopK:           &topK,
		ChoiceCount:    &choiceCount,
		Seed:           &seed,
		OutputType:     &outputType,
		ResponseStatus: &responseStatus,
		Input: []Message{
			{
				Role:         RoleUser,
				FinishReason: "stop",
				Parts: []Part{
					MediaPart(Media{
						Kind:     "image",
						URL:      "data:image/png;base64,abc123",
						MIMEType: "image/png",
						Name:     "weather-map.png",
					}),
				},
			},
		},
	}

	cloned := cloneGeneration(original)

	if cloned.Input[0].FinishReason != "stop" {
		t.Fatalf("cloned finish reason = %q, want stop", cloned.Input[0].FinishReason)
	}
	if cloned.TopK == original.TopK || cloned.ChoiceCount == original.ChoiceCount || cloned.Seed == original.Seed || cloned.OutputType == original.OutputType || cloned.ResponseStatus == original.ResponseStatus {
		t.Fatal("cloned generation shares expanded-field pointers with the original")
	}
	if cloned.Input[0].Parts[0].Media == nil {
		t.Fatal("expected cloned media part to keep media payload")
	}
	if got := cloned.Input[0].Parts[0].Media.URL; got != "data:image/png;base64,abc123" {
		t.Fatalf("unexpected cloned media URL: %q", got)
	}

	original.Input[0].Parts[0].Media.URL = "changed"
	if got := cloned.Input[0].Parts[0].Media.URL; got != "data:image/png;base64,abc123" {
		t.Fatalf("cloned media changed after mutating original: %q", got)
	}
}
