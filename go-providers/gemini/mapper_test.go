package gemini

import (
	"math"
	"testing"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestFromRequestResponse(t *testing.T) {
	temperature := float32(0.4)
	topP := float32(0.75)
	seed := int32(17)
	thinkingBudget := int32(2048)
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("What is the weather in Paris?", genai.RoleUser),
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromFunctionResponse("weather", map[string]any{
				"temp_c": 18,
			}),
		}, genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("Be concise.", genai.RoleUser),
		MaxOutputTokens:   300,
		Temperature:       &temperature,
		TopP:              &topP,
		CandidateCount:    2,
		Seed:              &seed,
		ResponseMIMEType:  "application/json",
		ToolConfig: &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAny,
			},
		},
		ThinkingConfig: &genai.ThinkingConfig{
			IncludeThoughts: true,
			ThinkingBudget:  &thinkingBudget,
			ThinkingLevel:   genai.ThinkingLevelHigh,
		},
		Tools: []*genai.Tool{
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{
						Name:        "weather",
						Description: "Get weather",
						ParametersJsonSchema: map[string]any{
							"type": "object",
							"properties": map[string]any{
								"city": map[string]any{"type": "string"},
							},
							"required": []string{"city"},
						},
					},
				},
			},
		},
	}

	resp := &genai.GenerateContentResponse{
		ResponseID:   "resp_1",
		ModelVersion: "gemini-2.5-pro-001",
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content: genai.NewContentFromParts([]*genai.Part{
					{
						FunctionCall: &genai.FunctionCall{
							ID:   "call_weather",
							Name: "weather",
							Args: map[string]any{"city": "Paris"},
						},
					},
					genai.NewPartFromText("It is 18C and sunny."),
				}, genai.RoleModel),
			},
		},
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			PromptTokenCount:        120,
			CandidatesTokenCount:    40,
			TotalTokenCount:         160,
			CachedContentTokenCount: 12,
			ThoughtsTokenCount:      10,
			ToolUsePromptTokenCount: 9,
		},
	}

	generation, err := FromRequestResponse(model, contents, config, resp,
		WithConversationID("conv-9b2f"),
		WithConversationTitle("Paris weather"),
		WithAgentName("agent-gemini"),
		WithAgentVersion("v-gemini"),
		WithTag("tenant", "t-123"),
	)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.Model.Provider != "gemini" {
		t.Fatalf("expected provider gemini, got %q", generation.Model.Provider)
	}
	if generation.Model.Name != "gemini-2.5-pro" {
		t.Fatalf("expected model gemini-2.5-pro, got %q", generation.Model.Name)
	}
	if generation.ConversationID != "conv-9b2f" {
		t.Fatalf("expected conv-9b2f, got %q", generation.ConversationID)
	}
	if generation.ConversationTitle != "Paris weather" {
		t.Fatalf("expected conversation title Paris weather, got %q", generation.ConversationTitle)
	}
	if generation.AgentName != "agent-gemini" {
		t.Fatalf("expected agent-gemini, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-gemini" {
		t.Fatalf("expected v-gemini, got %q", generation.AgentVersion)
	}
	if generation.ResponseID != "resp_1" {
		t.Fatalf("expected response id resp_1, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "gemini-2.5-pro-001" {
		t.Fatalf("expected response model gemini-2.5-pro-001, got %q", generation.ResponseModel)
	}
	if generation.SystemPrompt != "Be concise." {
		t.Fatalf("unexpected system prompt: %q", generation.SystemPrompt)
	}
	if generation.StopReason != "STOP" {
		t.Fatalf("expected stop reason STOP, got %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 160 {
		t.Fatalf("expected total tokens 160, got %d", generation.Usage.TotalTokens)
	}
	if generation.Usage.CacheReadInputTokens != 12 {
		t.Fatalf("expected cache read tokens 12, got %d", generation.Usage.CacheReadInputTokens)
	}
	if generation.Usage.ReasoningTokens != 10 {
		t.Fatalf("expected reasoning tokens 10, got %d", generation.Usage.ReasoningTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 300 {
		t.Fatalf("expected max tokens 300, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || math.Abs(*generation.Temperature-0.4) > 1e-6 {
		t.Fatalf("expected temperature 0.4, got %v", generation.Temperature)
	}
	if generation.TopP == nil || math.Abs(*generation.TopP-0.75) > 1e-6 {
		t.Fatalf("expected top_p 0.75, got %v", generation.TopP)
	}
	if generation.ChoiceCount == nil || *generation.ChoiceCount != 2 {
		t.Fatalf("expected choice count 2, got %v", generation.ChoiceCount)
	}
	if generation.Seed == nil || *generation.Seed != 17 {
		t.Fatalf("expected seed 17, got %v", generation.Seed)
	}
	if generation.OutputType == nil || *generation.OutputType != "json" {
		t.Fatalf("expected output type json, got %v", generation.OutputType)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != "any" {
		t.Fatalf("unexpected tool choice %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Metadata == nil {
		t.Fatalf("expected metadata map")
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"] != int64(2048) {
		t.Fatalf("expected thinking budget metadata 2048, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"])
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.level"] != "high" {
		t.Fatalf("expected thinking level metadata high, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.level"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.tool_use_prompt_tokens"] != int64(9) {
		t.Fatalf("expected tool use prompt token metadata 9, got %v", generation.Metadata["agento11y.gen_ai.usage.tool_use_prompt_tokens"])
	}
	if generation.Tags["tenant"] != "t-123" {
		t.Fatalf("expected tenant tag")
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}

	hasToolRole := false
	for _, message := range generation.Input {
		if message.Role == agento11y.RoleTool {
			hasToolRole = true
			if len(message.Parts) != 1 || message.Parts[0].ToolResult == nil {
				t.Fatalf("expected single tool_result part, got %#v", message.Parts)
			}
			if message.Parts[0].ToolResult.ToolCallID != "" {
				t.Fatalf("expected empty Gemini tool_call_id fallback, got %q", message.Parts[0].ToolResult.ToolCallID)
			}
			if message.Parts[0].ToolResult.Name != "weather" {
				t.Fatalf("expected Gemini tool_result name weather, got %q", message.Parts[0].ToolResult.Name)
			}
		}
	}
	if !hasToolRole {
		t.Fatalf("expected tool role message from function response input")
	}
}

func TestFromStream(t *testing.T) {
	temperature := float32(0.2)
	topP := float32(0.6)
	seed := int32(29)
	thinkingBudget := int32(1536)
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("What is the weather in Paris?", genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		MaxOutputTokens:  90,
		Temperature:      &temperature,
		TopP:             &topP,
		CandidateCount:   3,
		Seed:             &seed,
		ResponseMIMEType: "text/plain",
		ToolConfig: &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAuto,
			},
		},
		ThinkingConfig: &genai.ThinkingConfig{
			IncludeThoughts: false,
			ThinkingBudget:  &thinkingBudget,
			ThinkingLevel:   genai.ThinkingLevelMedium,
		},
		Tools: []*genai.Tool{
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "weather"},
				},
			},
		},
	}

	summary := StreamSummary{
		Responses: []*genai.GenerateContentResponse{
			{
				ResponseID:   "resp_stream_1",
				ModelVersion: "gemini-2.5-pro-001",
				Candidates: []*genai.Candidate{
					{
						Content: genai.NewContentFromParts([]*genai.Part{
							{
								FunctionCall: &genai.FunctionCall{
									ID:   "call_weather",
									Name: "weather",
									Args: map[string]any{"city": "Paris"},
								},
							},
						}, genai.RoleModel),
					},
				},
			},
			{
				ResponseID:   "resp_stream_2",
				ModelVersion: "gemini-2.5-pro-001",
				Candidates: []*genai.Candidate{
					{
						FinishReason: genai.FinishReasonStop,
						Content:      genai.NewContentFromText("It is 18C and sunny.", genai.RoleModel),
					},
				},
				UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
					PromptTokenCount:        20,
					CandidatesTokenCount:    6,
					TotalTokenCount:         26,
					ToolUsePromptTokenCount: 5,
				},
			},
		},
	}

	generation, err := FromStream(model, contents, config, summary,
		WithConversationID("conv-stream"),
		WithAgentName("agent-gemini-stream"),
		WithAgentVersion("v-gemini-stream"),
	)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if generation.ConversationID != "conv-stream" {
		t.Fatalf("expected conv-stream, got %q", generation.ConversationID)
	}
	if generation.AgentName != "agent-gemini-stream" {
		t.Fatalf("expected agent-gemini-stream, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-gemini-stream" {
		t.Fatalf("expected v-gemini-stream, got %q", generation.AgentVersion)
	}
	if generation.StopReason != "STOP" {
		t.Fatalf("expected stop reason STOP, got %q", generation.StopReason)
	}
	if generation.ResponseID != "resp_stream_2" {
		t.Fatalf("expected response id resp_stream_2, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "gemini-2.5-pro-001" {
		t.Fatalf("expected response model gemini-2.5-pro-001, got %q", generation.ResponseModel)
	}
	if generation.Usage.TotalTokens != 26 {
		t.Fatalf("expected total tokens 26, got %d", generation.Usage.TotalTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 90 {
		t.Fatalf("expected max tokens 90, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || math.Abs(*generation.Temperature-0.2) > 1e-6 {
		t.Fatalf("expected temperature 0.2, got %v", generation.Temperature)
	}
	if generation.TopP == nil || math.Abs(*generation.TopP-0.6) > 1e-6 {
		t.Fatalf("expected top_p 0.6, got %v", generation.TopP)
	}
	if generation.ChoiceCount == nil || *generation.ChoiceCount != 3 {
		t.Fatalf("expected choice count 3, got %v", generation.ChoiceCount)
	}
	if generation.Seed == nil || *generation.Seed != 29 {
		t.Fatalf("expected seed 29, got %v", generation.Seed)
	}
	if generation.OutputType == nil || *generation.OutputType != "text" {
		t.Fatalf("expected output type text, got %v", generation.OutputType)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != "auto" {
		t.Fatalf("unexpected tool choice %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected positive thinking budget to enable thinking, got %v", generation.ThinkingEnabled)
	}
	if generation.Metadata == nil {
		t.Fatalf("expected metadata map")
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"] != int64(1536) {
		t.Fatalf("expected thinking budget metadata 1536, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.budget_tokens"])
	}
	if generation.Metadata["agento11y.gen_ai.request.thinking.level"] != "medium" {
		t.Fatalf("expected thinking level metadata medium, got %v", generation.Metadata["agento11y.gen_ai.request.thinking.level"])
	}
	if generation.Metadata["agento11y.gen_ai.usage.tool_use_prompt_tokens"] != int64(5) {
		t.Fatalf("expected tool use prompt token metadata 5, got %v", generation.Metadata["agento11y.gen_ai.usage.tool_use_prompt_tokens"])
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}
}

func TestThinkingEnabledTracksThinkingRequestNotThoughtInclusion(t *testing.T) {
	tests := []struct {
		name   string
		config *genai.ThinkingConfig
		want   *bool
	}{
		{
			name:   "thinking level with hidden thoughts",
			config: &genai.ThinkingConfig{ThinkingLevel: genai.ThinkingLevelHigh, IncludeThoughts: false},
			want:   boolTestPtr(true),
		},
		{
			name:   "thought inclusion alone",
			config: &genai.ThinkingConfig{IncludeThoughts: true},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := &genai.GenerateContentConfig{ThinkingConfig: test.config}
			mappers := map[string]func() (agento11y.Generation, error){
				"normal": func() (agento11y.Generation, error) {
					return FromRequestResponse("gemini-2.5-pro", nil, config, &genai.GenerateContentResponse{})
				},
				"streaming": func() (agento11y.Generation, error) {
					return FromStream("gemini-2.5-pro", nil, config, StreamSummary{Responses: []*genai.GenerateContentResponse{{}}})
				},
			}
			for mapperName, mapper := range mappers {
				t.Run(mapperName, func(t *testing.T) {
					generation, err := mapper()
					if err != nil {
						t.Fatalf("map generation: %v", err)
					}
					if test.want == nil {
						if generation.ThinkingEnabled != nil {
							t.Fatalf("thinking enabled = %v, want omitted", *generation.ThinkingEnabled)
						}
						return
					}
					if generation.ThinkingEnabled == nil || *generation.ThinkingEnabled != *test.want {
						t.Fatalf("thinking enabled = %v, want %v", generation.ThinkingEnabled, *test.want)
					}
				})
			}
		})
	}
}

func boolTestPtr(value bool) *bool {
	return &value
}

func TestFromRequestResponseWithRawArtifacts(t *testing.T) {
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("hello", genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		Tools: []*genai.Tool{
			{
				FunctionDeclarations: []*genai.FunctionDeclaration{
					{Name: "weather"},
				},
			},
		},
	}

	resp := &genai.GenerateContentResponse{
		ResponseID:   "resp_1",
		ModelVersion: "gemini-2.5-pro-001",
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content:      genai.NewContentFromText("done", genai.RoleModel),
			},
		},
	}

	generation, err := FromRequestResponse(model, contents, config, resp, WithRawArtifacts())
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Artifacts) != 3 {
		t.Fatalf("expected 3 artifacts with raw artifact opt-in, got %d", len(generation.Artifacts))
	}
}

func TestEmbeddingFromResponse(t *testing.T) {
	model := "gemini-embedding-001"
	dimensions := int32(8)
	contents := []*genai.Content{
		genai.NewContentFromText("first input", genai.RoleUser),
		genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromText("second input"),
		}, genai.RoleUser),
		nil,
	}
	config := &genai.EmbedContentConfig{
		OutputDimensionality: &dimensions,
	}
	resp := &genai.EmbedContentResponse{
		Embeddings: []*genai.ContentEmbedding{
			{
				Values: []float32{0.1, 0.2, 0.3},
				Statistics: &genai.ContentEmbeddingStatistics{
					TokenCount: 5,
				},
			},
			{
				Values: []float32{0.4, 0.5, 0.6},
				Statistics: &genai.ContentEmbeddingStatistics{
					TokenCount: 7,
				},
			},
		},
	}

	result := EmbeddingFromResponse(model, contents, config, resp)

	if result.InputCount != 2 {
		t.Fatalf("expected input count 2, got %d", result.InputCount)
	}
	if result.InputTokens != 12 {
		t.Fatalf("expected input tokens 12, got %d", result.InputTokens)
	}
	if len(result.InputTexts) != 2 || result.InputTexts[0] != "first input" || result.InputTexts[1] != "second input" {
		t.Fatalf("unexpected input texts: %#v", result.InputTexts)
	}
	if result.Dimensions == nil || *result.Dimensions != 3 {
		t.Fatalf("expected dimensions 3, got %v", result.Dimensions)
	}
}

func TestEmbeddingFromResponseFallsBackToRequestedDimensions(t *testing.T) {
	model := "gemini-embedding-001"
	dimensions := int32(12)
	config := &genai.EmbedContentConfig{
		OutputDimensionality: &dimensions,
	}

	result := EmbeddingFromResponse(model, []*genai.Content{
		genai.NewContentFromText("single input", genai.RoleUser),
	}, config, &genai.EmbedContentResponse{})

	if result.InputCount != 1 {
		t.Fatalf("expected input count 1, got %d", result.InputCount)
	}
	if result.InputTokens != 0 {
		t.Fatalf("expected input tokens 0, got %d", result.InputTokens)
	}
	if result.Dimensions == nil || *result.Dimensions != 12 {
		t.Fatalf("expected dimensions 12, got %v", result.Dimensions)
	}
}

func TestFromRequestResponsePreservesWhitespace(t *testing.T) {
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("  user literal \\\\n\\\\n  ", genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromText("  system prompt  ", genai.RoleUser),
	}
	resp := &genai.GenerateContentResponse{
		ResponseID:   "resp_whitespace",
		ModelVersion: "gemini-2.5-pro-001",
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content:      genai.NewContentFromText("\n  assistant output  \n", genai.RoleModel),
			},
		},
	}

	generation, err := FromRequestResponse(model, contents, config, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.SystemPrompt != "  system prompt  " {
		t.Fatalf("unexpected system prompt %q", generation.SystemPrompt)
	}
	if len(generation.Input) != 1 || len(generation.Input[0].Parts) != 1 {
		t.Fatalf("expected single input text part, got %#v", generation.Input)
	}
	if generation.Input[0].Parts[0].Text != "  user literal \\\\n\\\\n  " {
		t.Fatalf("unexpected input text %q", generation.Input[0].Parts[0].Text)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 1 {
		t.Fatalf("expected single output text part, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "\n  assistant output  \n" {
		t.Fatalf("unexpected output text %q", generation.Output[0].Parts[0].Text)
	}
}

func TestFromStreamPreservesWhitespaceOnlyOutput(t *testing.T) {
	model := "gemini-2.5-pro"
	summary := StreamSummary{
		Responses: []*genai.GenerateContentResponse{
			{
				ResponseID:   "resp_stream_whitespace",
				ModelVersion: "gemini-2.5-pro-001",
				Candidates: []*genai.Candidate{
					{
						FinishReason: genai.FinishReasonStop,
						Content:      genai.NewContentFromText("   ", genai.RoleModel),
					},
				},
			},
		},
	}

	generation, err := FromStream(model, nil, nil, summary)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 1 {
		t.Fatalf("expected single output text part, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "   " {
		t.Fatalf("unexpected output text %q", generation.Output[0].Parts[0].Text)
	}
}

func TestFromRequestResponseUsageIncludesThoughtsInOutput(t *testing.T) {
	generation, err := FromRequestResponse("gemini-2.5-pro", nil, nil, &genai.GenerateContentResponse{
		UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
			CandidatesTokenCount: 20,
			ThoughtsTokenCount:   80,
			TotalTokenCount:      100,
		},
	})
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}
	if generation.Usage.OutputTokens != 100 || generation.Usage.ReasoningTokens != 80 || generation.Usage.TotalTokens != 100 {
		t.Fatalf("unexpected inclusive output usage: %#v", generation.Usage)
	}
}

func TestFromRequestResponseMapsIntegralTopK(t *testing.T) {
	want40 := int64(40)
	cases := []struct {
		name  string
		value float32
		want  *int64
	}{
		{name: "integral", value: 40, want: &want40},
		{name: "fractional", value: 40.5},
		{name: "out of range", value: math.MaxFloat32},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			generation, err := FromRequestResponse("gemini-2.5-pro", nil, &genai.GenerateContentConfig{TopK: &tc.value}, &genai.GenerateContentResponse{})
			if err != nil {
				t.Fatalf("from request/response: %v", err)
			}
			if tc.want == nil {
				if generation.TopK != nil {
					t.Fatalf("top_k = %v, want omitted", *generation.TopK)
				}
				return
			}
			if generation.TopK == nil || *generation.TopK != *tc.want {
				t.Fatalf("top_k = %v, want %d", generation.TopK, *tc.want)
			}
		})
	}
}

func TestFromRequestResponsePreservesMixedContentOrder(t *testing.T) {
	contents := []*genai.Content{{
		Role: genai.RoleUser,
		Parts: []*genai.Part{
			genai.NewPartFromText("before"),
			genai.NewPartFromFunctionResponse("weather", map[string]any{"temperature": 18}),
			genai.NewPartFromText("after"),
		},
	}}
	generation, err := FromRequestResponse("gemini-2.5-pro", contents, nil, &genai.GenerateContentResponse{})
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}
	if len(generation.Input) != 3 {
		t.Fatalf("input messages = %#v, want user/tool/user", generation.Input)
	}
	if generation.Input[0].Role != agento11y.RoleUser || generation.Input[0].Parts[0].Text != "before" ||
		generation.Input[1].Role != agento11y.RoleTool || generation.Input[1].Parts[0].ToolResult == nil ||
		generation.Input[2].Role != agento11y.RoleUser || generation.Input[2].Parts[0].Text != "after" {
		t.Fatalf("input messages = %#v, want user/tool/user in provider order", generation.Input)
	}

	generation, err = FromRequestResponse("gemini-2.5-pro", []*genai.Content{
		genai.NewContentFromText("first", genai.RoleUser),
		genai.NewContentFromText("second", genai.RoleUser),
	}, nil, &genai.GenerateContentResponse{})
	if err != nil {
		t.Fatalf("from consecutive request messages: %v", err)
	}
	if len(generation.Input) != 2 || generation.Input[0].Parts[0].Text != "first" || generation.Input[1].Parts[0].Text != "second" {
		t.Fatalf("consecutive provider messages were merged: %#v", generation.Input)
	}
}

func TestGenerationOperationUsesModalities(t *testing.T) {
	textCandidate := []*genai.Candidate{{Content: genai.NewContentFromText("hello", genai.RoleModel)}}
	imageCandidate := []*genai.Candidate{{Content: genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromBytes([]byte("image"), "image/png"),
	}, genai.RoleModel)}}
	nestedMedia := genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromFunctionResponseWithParts("inspect", map[string]any{"status": "ok"}, []*genai.FunctionResponsePart{
			genai.NewFunctionResponsePartFromURI("gs://bucket/result.png", "image/png"),
		}),
	}, genai.RoleUser)

	tests := []struct {
		name       string
		contents   []*genai.Content
		config     *genai.GenerateContentConfig
		candidates []*genai.Candidate
		want       string
	}{
		{name: "text only", contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}, candidates: textCandidate, want: "chat"},
		{name: "non-text input", contents: []*genai.Content{genai.NewContentFromParts([]*genai.Part{genai.NewPartFromURI("gs://bucket/image.png", "image/png")}, genai.RoleUser)}, want: "generate_content"},
		{name: "configured non-text output", config: &genai.GenerateContentConfig{ResponseModalities: []string{"TEXT", "IMAGE"}}, want: "generate_content"},
		{name: "nested function-response media", contents: []*genai.Content{nestedMedia}, want: "generate_content"},
		{name: "actual non-text output", candidates: imageCandidate, want: "generate_content"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := generationOperation(test.contents, test.config, test.candidates); got != test.want {
				t.Fatalf("operation = %q, want %q", got, test.want)
			}
		})
	}
}

func TestFromRequestResponseKeepsCandidatesSeparateAndOrdered(t *testing.T) {
	generation, err := FromRequestResponse("gemini-2.5-pro", nil, nil, &genai.GenerateContentResponse{
		Candidates: []*genai.Candidate{
			{Index: 0, FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("first", genai.RoleModel)},
			{Index: 1, FinishReason: genai.FinishReasonMaxTokens, Content: genai.NewContentFromText("second", genai.RoleModel)},
		},
	})
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("expected two candidate messages, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "first" || generation.Output[0].FinishReason != "STOP" {
		t.Fatalf("unexpected first candidate: %#v", generation.Output[0])
	}
	if generation.Output[1].Parts[0].Text != "second" || generation.Output[1].FinishReason != "MAX_TOKENS" {
		t.Fatalf("unexpected second candidate: %#v", generation.Output[1])
	}
}

func TestGeminiPreservesFinishOnlyCandidates(t *testing.T) {
	emptyToolCall := func() *genai.Content {
		return genai.NewContentFromParts([]*genai.Part{{FunctionCall: &genai.FunctionCall{}}}, genai.RoleModel)
	}
	tests := []struct {
		name          string
		mapGeneration func() (agento11y.Generation, error)
	}{
		{
			name: "sync",
			mapGeneration: func() (agento11y.Generation, error) {
				return FromRequestResponse("gemini-2.5-pro", nil, nil, &genai.GenerateContentResponse{Candidates: []*genai.Candidate{
					{Index: 0, FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("answer", genai.RoleModel)},
					{Index: 1, FinishReason: genai.FinishReasonSafety, Content: emptyToolCall()},
				}})
			},
		},
		{
			name: "stream",
			mapGeneration: func() (agento11y.Generation, error) {
				return FromStream("gemini-2.5-pro", nil, nil, StreamSummary{Responses: []*genai.GenerateContentResponse{{Candidates: []*genai.Candidate{
					{Index: 0, FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("answer", genai.RoleModel)},
					{Index: 1, FinishReason: genai.FinishReasonSafety, Content: emptyToolCall()},
				}}}})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generation, err := test.mapGeneration()
			if err != nil {
				t.Fatalf("map generation: %v", err)
			}
			if len(generation.Output) != 2 || len(generation.Output[1].Parts) != 0 || generation.Output[1].FinishReason != "SAFETY" {
				t.Fatalf("output = %#v, want a finish-only safety candidate", generation.Output)
			}
		})
	}
}

func TestGeminiMapsInlineAndFileMediaParts(t *testing.T) {
	input := genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromBytes([]byte("png"), "image/png"),
		genai.NewPartFromURI("gs://bucket/clip.mp3", "audio/mpeg"),
	}, genai.RoleUser)
	functionResponse := genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromFunctionResponseWithParts("inspect", map[string]any{"ok": true}, []*genai.FunctionResponsePart{
			genai.NewFunctionResponsePartFromURI("gs://bucket/result.png", "image/png"),
		}),
	}, genai.RoleUser)
	output := genai.NewContentFromParts([]*genai.Part{
		genai.NewPartFromBytes([]byte("video"), "video/mp4"),
	}, genai.RoleModel)
	generation, err := FromRequestResponse("gemini-2.5-pro", []*genai.Content{input, functionResponse}, nil, &genai.GenerateContentResponse{Candidates: []*genai.Candidate{{
		FinishReason: genai.FinishReasonStop,
		Content:      output,
	}}})
	if err != nil {
		t.Fatalf("map media generation: %v", err)
	}
	if generation.OperationName != "generate_content" || len(generation.Input) != 2 || len(generation.Input[0].Parts) != 2 || len(generation.Input[1].Parts) != 2 || len(generation.Output) != 1 || len(generation.Output[0].Parts) != 1 {
		t.Fatalf("media generation = %#v", generation)
	}
	image := generation.Input[0].Parts[0].Media
	audio := generation.Input[0].Parts[1].Media
	nestedImage := generation.Input[1].Parts[1].Media
	video := generation.Output[0].Parts[0].Media
	if image == nil || image.Kind != "image" || image.URL != "data:image/png;base64,cG5n" {
		t.Fatalf("inline image = %#v", image)
	}
	if audio == nil || audio.Kind != "audio" || audio.URL != "gs://bucket/clip.mp3" {
		t.Fatalf("file audio = %#v", audio)
	}
	if nestedImage == nil || nestedImage.Kind != "image" || nestedImage.URL != "gs://bucket/result.png" {
		t.Fatalf("function response image = %#v", nestedImage)
	}
	if video == nil || video.Kind != "video" || video.URL != "data:video/mp4;base64,dmlkZW8=" {
		t.Fatalf("inline video = %#v", video)
	}
}

func TestFromStreamAccumulatesCandidatesByIndex(t *testing.T) {
	tests := []struct {
		name      string
		responses []*genai.GenerateContentResponse
	}{
		{
			name: "explicit indexes",
			responses: []*genai.GenerateContentResponse{
				{Candidates: []*genai.Candidate{
					{Index: 1, Content: genai.NewContentFromText("second ", genai.RoleModel)},
					{Index: 0, Content: genai.NewContentFromText("first ", genai.RoleModel)},
				}},
				{Candidates: []*genai.Candidate{
					{Index: 0, FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("candidate", genai.RoleModel)},
					{Index: 1, FinishReason: genai.FinishReasonMaxTokens, Content: genai.NewContentFromText("candidate", genai.RoleModel)},
				}},
			},
		},
		{
			name: "missing indexes use response positions",
			responses: []*genai.GenerateContentResponse{
				{Candidates: []*genai.Candidate{
					{Content: genai.NewContentFromText("first ", genai.RoleModel)},
					{Content: genai.NewContentFromText("second ", genai.RoleModel)},
				}},
				{Candidates: []*genai.Candidate{
					{FinishReason: genai.FinishReasonStop, Content: genai.NewContentFromText("candidate", genai.RoleModel)},
					{FinishReason: genai.FinishReasonMaxTokens, Content: genai.NewContentFromText("candidate", genai.RoleModel)},
				}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			generation, err := FromStream("gemini-2.5-pro", nil, nil, StreamSummary{Responses: test.responses})
			if err != nil {
				t.Fatalf("from stream: %v", err)
			}
			if len(generation.Output) != 2 {
				t.Fatalf("expected two accumulated candidates, got %#v", generation.Output)
			}
			if generation.Output[0].Parts[0].Text != "first candidate" || generation.Output[0].FinishReason != "STOP" {
				t.Fatalf("unexpected index 0 candidate: %#v", generation.Output[0])
			}
			if generation.Output[1].Parts[0].Text != "second candidate" || generation.Output[1].FinishReason != "MAX_TOKENS" {
				t.Fatalf("unexpected index 1 candidate: %#v", generation.Output[1])
			}
		})
	}
}

func TestExtractSystemPromptPreservesEmptySegments(t *testing.T) {
	config := &genai.GenerateContentConfig{
		SystemInstruction: genai.NewContentFromParts([]*genai.Part{
			genai.NewPartFromText(""),
			genai.NewPartFromText("second"),
		}, genai.RoleUser),
	}
	if got := extractSystemPrompt(config); got != "\n\nsecond" {
		t.Fatalf("expected preserved empty segment separator, got %q", got)
	}
}
