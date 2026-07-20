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
	thinkingBudget := int32(1536)
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("What is the weather in Paris?", genai.RoleUser),
	}
	config := &genai.GenerateContentConfig{
		MaxOutputTokens: 90,
		Temperature:     &temperature,
		TopP:            &topP,
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
	if generation.ToolChoice == nil || *generation.ToolChoice != "auto" {
		t.Fatalf("unexpected tool choice %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || *generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled false, got %v", generation.ThinkingEnabled)
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
