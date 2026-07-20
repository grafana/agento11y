package openai

import (
	"testing"

	osdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	oresponses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/grafana/agento11y/go/sigil"
)

func TestFromRequestResponse(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.SystemMessage("You are concise."),
			osdk.UserMessage("What is the weather in Paris?"),
			osdk.ToolMessage(`{"temp_c":18}`, "call_weather"),
		},
		Tools: []osdk.ChatCompletionToolUnionParam{
			osdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name:        "weather",
				Description: osdk.String("Get weather"),
				Parameters: shared.FunctionParameters{
					"type": "object",
					"properties": map[string]any{
						"city": map[string]any{"type": "string"},
					},
					"required": []string{"city"},
				},
			}),
		},
		MaxCompletionTokens: param.NewOpt(int64(128)),
		MaxTokens:           param.NewOpt(int64(256)),
		Temperature:         param.NewOpt(0.7),
		TopP:                param.NewOpt(0.9),
		ToolChoice:          osdk.ToolChoiceOptionFunctionToolChoice(osdk.ChatCompletionNamedToolChoiceFunctionParam{Name: "weather"}),
		ReasoningEffort:     shared.ReasoningEffortLow,
	}

	resp := &osdk.ChatCompletion{
		ID:    "chatcmpl_1",
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{
			{
				FinishReason: "tool_calls",
				Message: osdk.ChatCompletionMessage{
					ToolCalls: []osdk.ChatCompletionMessageToolCallUnion{
						{
							ID:   "call_weather",
							Type: "function",
							Function: osdk.ChatCompletionMessageFunctionToolCallFunction{
								Name:      "weather",
								Arguments: `{"city":"Paris"}`,
							},
						},
					},
				},
			},
		},
		Usage: osdk.CompletionUsage{
			PromptTokens:     120,
			CompletionTokens: 42,
			TotalTokens:      162,
			PromptTokensDetails: osdk.CompletionUsagePromptTokensDetails{
				CachedTokens: 8,
			},
			CompletionTokensDetails: osdk.CompletionUsageCompletionTokensDetails{
				ReasoningTokens: 5,
			},
		},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp,
		WithConversationID("conv-9b2f"),
		WithConversationTitle("Paris weather"),
		WithAgentName("agent-openai"),
		WithAgentVersion("v-openai"),
		WithTag("tenant", "t-123"),
	)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.Model.Provider != "openai" {
		t.Fatalf("expected provider openai, got %q", generation.Model.Provider)
	}
	if generation.Model.Name != "gpt-4o-mini" {
		t.Fatalf("expected model gpt-4o-mini, got %q", generation.Model.Name)
	}
	if generation.ConversationID != "conv-9b2f" {
		t.Fatalf("expected conv-9b2f, got %q", generation.ConversationID)
	}
	if generation.ConversationTitle != "Paris weather" {
		t.Fatalf("expected conversation title Paris weather, got %q", generation.ConversationTitle)
	}
	if generation.AgentName != "agent-openai" {
		t.Fatalf("expected agent-openai, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-openai" {
		t.Fatalf("expected v-openai, got %q", generation.AgentVersion)
	}
	if generation.ResponseID != "chatcmpl_1" {
		t.Fatalf("expected response id chatcmpl_1, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "gpt-4o-mini" {
		t.Fatalf("expected response model gpt-4o-mini, got %q", generation.ResponseModel)
	}
	if generation.SystemPrompt != "You are concise." {
		t.Fatalf("unexpected system prompt: %q", generation.SystemPrompt)
	}
	if generation.StopReason != "tool_calls" {
		t.Fatalf("expected stop reason tool_calls, got %q", generation.StopReason)
	}
	if generation.ResponseModel != "gpt-4o-mini" {
		t.Fatalf("expected response model gpt-4o-mini, got %q", generation.ResponseModel)
	}
	if generation.Usage.TotalTokens != 162 {
		t.Fatalf("expected total tokens 162, got %d", generation.Usage.TotalTokens)
	}
	if generation.Usage.CacheReadInputTokens != 8 {
		t.Fatalf("expected cached tokens 8, got %d", generation.Usage.CacheReadInputTokens)
	}
	if generation.Usage.ReasoningTokens != 5 {
		t.Fatalf("expected reasoning tokens 5, got %d", generation.Usage.ReasoningTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 128 {
		t.Fatalf("expected max tokens 128, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || *generation.Temperature != 0.7 {
		t.Fatalf("expected temperature 0.7, got %v", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != 0.9 {
		t.Fatalf("expected top_p 0.9, got %v", generation.TopP)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != `{"function":{"name":"weather"},"type":"function"}` {
		t.Fatalf("unexpected tool choice: %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Tags["tenant"] != "t-123" {
		t.Fatalf("expected tenant tag")
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}

	hasToolRole := false
	for _, message := range generation.Input {
		if message.Role == sigil.RoleTool {
			hasToolRole = true
			if len(message.Parts) != 1 || message.Parts[0].ToolResult == nil {
				t.Fatalf("expected single tool_result part, got %#v", message.Parts)
			}
			if message.Parts[0].ToolResult.ToolCallID != "call_weather" {
				t.Fatalf("expected tool_result tool_call_id call_weather, got %q", message.Parts[0].ToolResult.ToolCallID)
			}
		}
	}
	if !hasToolRole {
		t.Fatalf("expected tool role message from tool result input")
	}
}

func TestMapFunctionMessageUsesNameFallbackCorrelation(t *testing.T) {
	//nolint:staticcheck // OpenAI still exposes deprecated function messages in the union surface we normalize.
	part := mapFunctionMessage(&osdk.ChatCompletionFunctionMessageParam{
		Name:    "weather",
		Content: param.NewOpt("18C and sunny"),
	})

	if part == nil {
		t.Fatalf("expected tool result part")
	}
	if part.ToolResult == nil {
		t.Fatalf("expected tool result payload, got %#v", part)
	}
	if part.ToolResult.ToolCallID != "" {
		t.Fatalf("expected empty legacy function-result tool_call_id, got %q", part.ToolResult.ToolCallID)
	}
	if part.ToolResult.Name != "weather" {
		t.Fatalf("expected legacy function-result name fallback weather, got %q", part.ToolResult.Name)
	}
}

func TestMapResponsesRequestInputUsesNameFallbackWhenCallIDMissing(t *testing.T) {
	input, systemPrompt := mapResponsesRequestInput(map[string]any{
		"input": []any{
			map[string]any{
				"type":   "function_call_output",
				"name":   "weather",
				"output": map[string]any{"temp_c": 18},
			},
		},
	})

	if systemPrompt != "" {
		t.Fatalf("expected empty system prompt, got %q", systemPrompt)
	}
	if len(input) != 1 {
		t.Fatalf("expected one input message, got %#v", input)
	}
	if input[0].Role != sigil.RoleTool {
		t.Fatalf("expected tool role, got %q", input[0].Role)
	}
	if len(input[0].Parts) != 1 || input[0].Parts[0].ToolResult == nil {
		t.Fatalf("expected single tool_result part, got %#v", input[0].Parts)
	}
	if input[0].Parts[0].ToolResult.ToolCallID != "" {
		t.Fatalf("expected missing call id to stay empty, got %q", input[0].Parts[0].ToolResult.ToolCallID)
	}
	if input[0].Parts[0].ToolResult.Name != "weather" {
		t.Fatalf("expected Responses fallback name weather, got %q", input[0].Parts[0].ToolResult.Name)
	}
}

func TestFromStream(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.SystemMessage("You are concise."),
			osdk.UserMessage("What is the weather in Paris?"),
		},
		Tools: []osdk.ChatCompletionToolUnionParam{
			osdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name: "weather",
				Parameters: shared.FunctionParameters{
					"type": "object",
				},
			}),
		},
		MaxCompletionTokens: param.NewOpt(int64(42)),
		Temperature:         param.NewOpt(0.15),
		TopP:                param.NewOpt(0.4),
		ToolChoice:          osdk.ToolChoiceOptionFunctionToolChoice(osdk.ChatCompletionNamedToolChoiceFunctionParam{Name: "weather"}),
		ReasoningEffort:     shared.ReasoningEffortMedium,
	}

	summary := ChatCompletionsStreamSummary{
		Chunks: []osdk.ChatCompletionChunk{
			{
				ID:    "chatcmpl_stream_1",
				Model: "gpt-4o-mini",
				Choices: []osdk.ChatCompletionChunkChoice{
					{
						Delta: osdk.ChatCompletionChunkChoiceDelta{
							Content: "Calling tool",
							ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{
								{
									Index: 0,
									ID:    "call_weather",
									Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{
										Name:      "weather",
										Arguments: `{"city":"Pa`,
									},
								},
							},
						},
					},
				},
			},
			{
				Choices: []osdk.ChatCompletionChunkChoice{
					{
						Delta: osdk.ChatCompletionChunkChoiceDelta{
							Content: " now.",
							ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{
								{
									Index: 0,
									Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{
										Arguments: `ris"}`,
									},
								},
							},
						},
						FinishReason: "tool_calls",
					},
				},
				Usage: osdk.CompletionUsage{
					PromptTokens:     20,
					CompletionTokens: 5,
					TotalTokens:      25,
				},
			},
		},
	}

	generation, err := ChatCompletionsFromStream(req, summary,
		WithConversationID("conv-stream"),
		WithAgentName("agent-openai-stream"),
		WithAgentVersion("v-openai-stream"),
	)
	if err != nil {
		t.Fatalf("from stream: %v", err)
	}

	if generation.ConversationID != "conv-stream" {
		t.Fatalf("expected conv-stream, got %q", generation.ConversationID)
	}
	if generation.AgentName != "agent-openai-stream" {
		t.Fatalf("expected agent-openai-stream, got %q", generation.AgentName)
	}
	if generation.AgentVersion != "v-openai-stream" {
		t.Fatalf("expected v-openai-stream, got %q", generation.AgentVersion)
	}
	if generation.ResponseID != "chatcmpl_stream_1" {
		t.Fatalf("expected response id chatcmpl_stream_1, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "gpt-4o-mini" {
		t.Fatalf("expected response model gpt-4o-mini, got %q", generation.ResponseModel)
	}
	if generation.StopReason != "tool_calls" {
		t.Fatalf("expected stop reason tool_calls, got %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 25 {
		t.Fatalf("expected total tokens 25, got %d", generation.Usage.TotalTokens)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 42 {
		t.Fatalf("expected max tokens 42, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || *generation.Temperature != 0.15 {
		t.Fatalf("expected temperature 0.15, got %v", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != 0.4 {
		t.Fatalf("expected top_p 0.4, got %v", generation.TopP)
	}
	if generation.ToolChoice == nil || *generation.ToolChoice != `{"function":{"name":"weather"},"type":"function"}` {
		t.Fatalf("unexpected tool choice: %v", generation.ToolChoice)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if len(generation.Artifacts) != 0 {
		t.Fatalf("expected 0 artifacts by default, got %d", len(generation.Artifacts))
	}
}

func TestFromRequestResponseWithRawArtifacts(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.SystemMessage("You are concise."),
			osdk.UserMessage("hello"),
		},
		Tools: []osdk.ChatCompletionToolUnionParam{
			osdk.ChatCompletionFunctionTool(shared.FunctionDefinitionParam{
				Name: "weather",
			}),
		},
	}

	resp := &osdk.ChatCompletion{
		ID:    "chatcmpl_1",
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{
			{
				FinishReason: "stop",
				Message: osdk.ChatCompletionMessage{
					Content: "hi",
				},
			},
		},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp, WithRawArtifacts())
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if len(generation.Artifacts) != 3 {
		t.Fatalf("expected 3 artifacts with raw artifact opt-in, got %d", len(generation.Artifacts))
	}
}

func TestFromRequestResponseLeavesThinkingUnsetWithoutReasoningConfig(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.UserMessage("hello"),
		},
	}
	resp := &osdk.ChatCompletion{
		ID:    "chatcmpl_1",
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{
			{
				FinishReason: "stop",
				Message: osdk.ChatCompletionMessage{
					Content: "hi",
				},
			},
		},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("from request/response: %v", err)
	}

	if generation.ThinkingEnabled != nil {
		t.Fatalf("expected thinking_enabled unset, got %v", generation.ThinkingEnabled)
	}
}

func TestResponsesFromRequestResponse(t *testing.T) {
	req := oresponses.ResponseNewParams{
		Model:           shared.ResponsesModel("gpt-5"),
		Instructions:    param.NewOpt("Be concise."),
		Input:           oresponses.ResponseNewParamsInputUnion{OfString: param.NewOpt("hello")},
		MaxOutputTokens: param.NewOpt(int64(320)),
		Temperature:     param.NewOpt(0.2),
		TopP:            param.NewOpt(0.85),
		Reasoning: shared.ReasoningParam{
			Effort: shared.ReasoningEffortMedium,
		},
	}

	resp := &oresponses.Response{
		ID:     "resp_1",
		Model:  shared.ResponsesModel("gpt-5"),
		Status: oresponses.ResponseStatusCompleted,
		Output: []oresponses.ResponseOutputItemUnion{
			{
				Type: "message",
				Content: []oresponses.ResponseOutputMessageContentUnion{
					{Type: "output_text", Text: "world"},
				},
			},
			{
				Type:      "function_call",
				CallID:    "call_weather",
				Name:      "weather",
				Arguments: oresponses.ResponseOutputItemUnionArguments{OfString: `{"city":"Paris"}`},
			},
		},
		Usage: oresponses.ResponseUsage{
			InputTokens:  80,
			OutputTokens: 20,
			TotalTokens:  100,
			InputTokensDetails: oresponses.ResponseUsageInputTokensDetails{
				CachedTokens: 2,
			},
			OutputTokensDetails: oresponses.ResponseUsageOutputTokensDetails{
				ReasoningTokens: 3,
			},
		},
	}

	generation, err := ResponsesFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("responses from request/response: %v", err)
	}

	if generation.Model.Provider != "openai" {
		t.Fatalf("expected provider openai, got %q", generation.Model.Provider)
	}
	if generation.Model.Name != "gpt-5" {
		t.Fatalf("expected model gpt-5, got %q", generation.Model.Name)
	}
	if generation.ResponseID != "resp_1" {
		t.Fatalf("expected response id resp_1, got %q", generation.ResponseID)
	}
	if generation.ResponseModel != "gpt-5" {
		t.Fatalf("expected response model gpt-5, got %q", generation.ResponseModel)
	}
	if generation.SystemPrompt != "Be concise." {
		t.Fatalf("expected system prompt, got %q", generation.SystemPrompt)
	}
	if generation.StopReason != "stop" {
		t.Fatalf("expected stop reason stop, got %q", generation.StopReason)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 320 {
		t.Fatalf("expected max tokens 320, got %v", generation.MaxTokens)
	}
	if generation.Temperature == nil || *generation.Temperature != 0.2 {
		t.Fatalf("expected temperature 0.2, got %v", generation.Temperature)
	}
	if generation.TopP == nil || *generation.TopP != 0.85 {
		t.Fatalf("expected top_p 0.85, got %v", generation.TopP)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Usage.TotalTokens != 100 {
		t.Fatalf("expected total tokens 100, got %d", generation.Usage.TotalTokens)
	}
	if generation.Usage.CacheReadInputTokens != 2 {
		t.Fatalf("expected cached tokens 2, got %d", generation.Usage.CacheReadInputTokens)
	}
	if generation.Usage.ReasoningTokens != 3 {
		t.Fatalf("expected reasoning tokens 3, got %d", generation.Usage.ReasoningTokens)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("expected two output messages, got %d", len(generation.Output))
	}
}

func TestResponsesFromStream(t *testing.T) {
	req := oresponses.ResponseNewParams{
		Model:           shared.ResponsesModel("gpt-5"),
		Input:           oresponses.ResponseNewParamsInputUnion{OfString: param.NewOpt("hello")},
		MaxOutputTokens: param.NewOpt(int64(128)),
	}

	summary := ResponsesStreamSummary{
		Events: []oresponses.ResponseStreamEventUnion{
			{
				Type:  "response.output_text.delta",
				Delta: "hello",
			},
			{
				Type:  "response.output_text.delta",
				Delta: " world",
			},
			{
				Type: "response.output_item.added",
				Item: oresponses.ResponseOutputItemUnion{
					ID:     "fc_1",
					Type:   "function_call",
					CallID: "call_weather",
					Name:   "weather",
				},
				OutputIndex: 1,
			},
			{
				Type:        "response.function_call_arguments.delta",
				ItemID:      "fc_1",
				OutputIndex: 1,
				Delta:       `{"city":"Pa`,
			},
			{
				Type:        "response.function_call_arguments.done",
				ItemID:      "fc_1",
				OutputIndex: 1,
				Name:        "weather",
				Arguments:   `{"city":"Paris"}`,
			},
			{
				Type: "response.completed",
			},
		},
	}

	generation, err := ResponsesFromStream(req, summary, WithRawArtifacts())
	if err != nil {
		t.Fatalf("responses from stream: %v", err)
	}

	if generation.Model.Provider != "openai" {
		t.Fatalf("expected provider openai, got %q", generation.Model.Provider)
	}
	if generation.ResponseModel != "gpt-5" {
		t.Fatalf("expected response model gpt-5, got %q", generation.ResponseModel)
	}
	if generation.StopReason != "stop" {
		t.Fatalf("expected stop reason stop, got %q", generation.StopReason)
	}
	if generation.MaxTokens == nil || *generation.MaxTokens != 128 {
		t.Fatalf("expected max tokens 128, got %v", generation.MaxTokens)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("expected text and tool-call output messages, got %d", len(generation.Output))
	}
	if generation.Output[0].Parts[0].Text != "hello world" {
		t.Fatalf("expected merged stream output, got %q", generation.Output[0].Parts[0].Text)
	}
	if generation.Output[1].Parts[0].Kind != sigil.PartKindToolCall {
		t.Fatalf("expected tool call output, got %#v", generation.Output[1].Parts[0])
	}
	if generation.Output[1].Parts[0].ToolCall.ID != "call_weather" {
		t.Fatalf("expected tool call id call_weather, got %q", generation.Output[1].Parts[0].ToolCall.ID)
	}
	if generation.Output[1].Parts[0].ToolCall.Name != "weather" {
		t.Fatalf("expected tool call name weather, got %q", generation.Output[1].Parts[0].ToolCall.Name)
	}
	if string(generation.Output[1].Parts[0].ToolCall.InputJSON) != `{"city":"Paris"}` {
		t.Fatalf("expected tool call input JSON, got %q", string(generation.Output[1].Parts[0].ToolCall.InputJSON))
	}
	if len(generation.Artifacts) != 2 {
		t.Fatalf("expected request and provider_event artifacts, got %d", len(generation.Artifacts))
	}
	if generation.Artifacts[0].Kind != sigil.ArtifactKindRequest || generation.Artifacts[1].Kind != sigil.ArtifactKindProviderEvent {
		t.Fatalf("unexpected artifact kinds: %#v", generation.Artifacts)
	}
}

func TestEmbeddingsFromResponse(t *testing.T) {
	req := osdk.EmbeddingNewParams{
		Model: osdk.EmbeddingModel("text-embedding-3-small"),
		Input: osdk.EmbeddingNewParamsInputUnion{
			OfArrayOfStrings: []string{"hello", "world"},
		},
	}

	resp := &osdk.CreateEmbeddingResponse{
		Model: "text-embedding-3-small",
		Data: []osdk.Embedding{
			{
				Embedding: []float64{0.1, 0.2, 0.3},
			},
			{
				Embedding: []float64{0.4, 0.5, 0.6},
			},
		},
		Usage: osdk.CreateEmbeddingResponseUsage{
			PromptTokens: 42,
			TotalTokens:  42,
		},
	}

	result := EmbeddingsFromResponse(req, resp)
	if result.InputCount != 2 {
		t.Fatalf("expected input count 2, got %d", result.InputCount)
	}
	if result.InputTokens != 42 {
		t.Fatalf("expected input tokens 42, got %d", result.InputTokens)
	}
	if result.ResponseModel != "text-embedding-3-small" {
		t.Fatalf("expected response model text-embedding-3-small, got %q", result.ResponseModel)
	}
	if result.Dimensions == nil || *result.Dimensions != 3 {
		t.Fatalf("expected dimensions 3, got %v", result.Dimensions)
	}
	if len(result.InputTexts) != 2 || result.InputTexts[0] != "hello" || result.InputTexts[1] != "world" {
		t.Fatalf("expected input texts [hello world], got %v", result.InputTexts)
	}
}

func TestEmbeddingsFromResponseWithTokenInputDoesNotCaptureTexts(t *testing.T) {
	req := osdk.EmbeddingNewParams{
		Model: osdk.EmbeddingModel("text-embedding-3-small"),
		Input: osdk.EmbeddingNewParamsInputUnion{
			OfArrayOfTokens: []int64{1, 2, 3},
		},
	}

	result := EmbeddingsFromResponse(req, nil)
	if result.InputCount != 1 {
		t.Fatalf("expected input count 1, got %d", result.InputCount)
	}
	if len(result.InputTexts) != 0 {
		t.Fatalf("expected no input texts for tokenized input, got %v", result.InputTexts)
	}
}

func TestChatCompletionsFromRequestResponsePreservesWhitespace(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.SystemMessage("  system prompt  "),
			osdk.UserMessage("  user literal \\\\n\\\\n  "),
		},
	}
	resp := &osdk.ChatCompletion{
		ID:    "chatcmpl_whitespace",
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{
			{
				FinishReason: "stop",
				Message: osdk.ChatCompletionMessage{
					Content: "\n  assistant output  \n",
				},
			},
		},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp)
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

func TestChatCompletionsFromStreamPreservesWhitespaceOnlyOutput(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
	}

	summary := ChatCompletionsStreamSummary{
		Chunks: []osdk.ChatCompletionChunk{
			{
				ID:    "chatcmpl_stream_whitespace",
				Model: "gpt-4o-mini",
				Choices: []osdk.ChatCompletionChunkChoice{
					{
						Delta: osdk.ChatCompletionChunkChoiceDelta{
							Content: "   ",
						},
						FinishReason: "stop",
					},
				},
				Usage: osdk.CompletionUsage{
					PromptTokens:     1,
					CompletionTokens: 1,
					TotalTokens:      2,
				},
			},
		},
	}

	generation, err := ChatCompletionsFromStream(req, summary)
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

func TestResponsesFromRequestResponsePreservesWhitespace(t *testing.T) {
	req := oresponses.ResponseNewParams{
		Model:        shared.ResponsesModel("gpt-5"),
		Instructions: param.NewOpt("  system instructions  "),
		Input:        oresponses.ResponseNewParamsInputUnion{OfString: param.NewOpt("  user literal \\\\n\\\\n  ")},
	}
	resp := &oresponses.Response{
		ID:     "resp_whitespace",
		Model:  shared.ResponsesModel("gpt-5"),
		Status: oresponses.ResponseStatusCompleted,
		Output: []oresponses.ResponseOutputItemUnion{
			{
				Type: "message",
				Content: []oresponses.ResponseOutputMessageContentUnion{
					{Type: "output_text", Text: "\n  assistant output  \n"},
				},
			},
		},
	}

	generation, err := ResponsesFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("responses from request/response: %v", err)
	}
	if generation.SystemPrompt != "  system instructions  " {
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

func TestResponsesFromStreamPreservesWhitespaceOnlyOutput(t *testing.T) {
	req := oresponses.ResponseNewParams{
		Model: shared.ResponsesModel("gpt-5"),
	}
	summary := ResponsesStreamSummary{
		Events: []oresponses.ResponseStreamEventUnion{
			{
				Type:  "response.output_text.delta",
				Delta: "  ",
			},
			{
				Type: "response.completed",
			},
		},
	}

	generation, err := ResponsesFromStream(req, summary)
	if err != nil {
		t.Fatalf("responses from stream: %v", err)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 1 {
		t.Fatalf("expected single output text part, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "  " {
		t.Fatalf("unexpected output text %q", generation.Output[0].Parts[0].Text)
	}
}

func TestMapRequestMessagesPreservesEmptySystemEntries(t *testing.T) {
	system := osdk.ChatCompletionSystemMessageParam{
		Content: osdk.ChatCompletionSystemMessageParamContentUnion{
			OfString: param.NewOpt(""),
		},
	}
	developer := osdk.ChatCompletionDeveloperMessageParam{
		Content: osdk.ChatCompletionDeveloperMessageParamContentUnion{
			OfString: param.NewOpt("developer instruction"),
		},
	}

	input, systemPrompt := mapRequestMessages([]osdk.ChatCompletionMessageParamUnion{
		{OfSystem: &system},
		{OfDeveloper: &developer},
	})

	if len(input) != 0 {
		t.Fatalf("expected no mapped user/assistant/tool input messages, got %#v", input)
	}
	if systemPrompt != "\n\ndeveloper instruction" {
		t.Fatalf("expected preserved empty system entry before developer prompt, got %q", systemPrompt)
	}
}

func TestMapToolMessagePreservesEmptyParts(t *testing.T) {
	part := mapToolMessage(&osdk.ChatCompletionToolMessageParam{
		ToolCallID: "call_1",
		Content: osdk.ChatCompletionToolMessageParamContentUnion{
			OfArrayOfContentParts: []osdk.ChatCompletionContentPartTextParam{
				{Text: ""},
				{Text: ""},
			},
		},
	})

	if part == nil {
		t.Fatalf("expected tool result part for empty text segments")
	}
	if part.ToolResult == nil {
		t.Fatalf("expected tool result payload, got %#v", part)
	}
	if part.ToolResult.Content != "\n" {
		t.Fatalf("expected newline-preserved content, got %q", part.ToolResult.Content)
	}
}

func TestParseJSONOrStringPreservesWhitespace(t *testing.T) {
	if got := string(parseJSONOrString("  {\"city\":\"Paris\"}  ")); got != "  {\"city\":\"Paris\"}  " {
		t.Fatalf("expected JSON bytes to preserve whitespace, got %q", got)
	}
	if got := string(parseJSONOrString("  raw value  ")); got != "\"  raw value  \"" {
		t.Fatalf("expected quoted raw string with whitespace preserved, got %q", got)
	}
	if got := parseJSONOrString(""); got != nil {
		t.Fatalf("expected nil for empty string, got %q", string(got))
	}
}
