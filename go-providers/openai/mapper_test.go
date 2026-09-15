package openai

import (
	"encoding/json"
	"slices"
	"testing"

	osdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	oresponses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/grafana/agento11y/go/agento11y"
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
	if generation.SystemPrompt != "" {
		t.Fatalf("chat history was duplicated into system prompt: %q", generation.SystemPrompt)
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
		if message.Role == agento11y.RoleTool {
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
	if input[0].Role != agento11y.RoleTool {
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
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected one output candidate with two ordered parts, got %#v", generation.Output)
	}
	if generation.Output[0].FinishReason != generation.StopReason {
		t.Fatalf("output finish reason = %q, want stop reason %q", generation.Output[0].FinishReason, generation.StopReason)
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
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 2 {
		t.Fatalf("expected one candidate with text and tool-call parts, got %#v", generation.Output)
	}
	if generation.Output[0].FinishReason != generation.StopReason {
		t.Fatalf("output finish reason = %q, want stop reason %q", generation.Output[0].FinishReason, generation.StopReason)
	}
	if generation.Output[0].Parts[0].Text != "hello world" {
		t.Fatalf("expected merged stream output, got %q", generation.Output[0].Parts[0].Text)
	}
	if generation.Output[0].Parts[1].Kind != agento11y.PartKindToolCall {
		t.Fatalf("expected tool call output, got %#v", generation.Output[0].Parts[1])
	}
	if generation.Output[0].Parts[1].ToolCall.ID != "call_weather" {
		t.Fatalf("expected tool call id call_weather, got %q", generation.Output[0].Parts[1].ToolCall.ID)
	}
	if generation.Output[0].Parts[1].ToolCall.Name != "weather" {
		t.Fatalf("expected tool call name weather, got %q", generation.Output[0].Parts[1].ToolCall.Name)
	}
	if string(generation.Output[0].Parts[1].ToolCall.InputJSON) != `{"city":"Paris"}` {
		t.Fatalf("expected tool call input JSON, got %q", string(generation.Output[0].Parts[1].ToolCall.InputJSON))
	}
	if len(generation.Artifacts) != 2 {
		t.Fatalf("expected request and provider_event artifacts, got %d", len(generation.Artifacts))
	}
	if generation.Artifacts[0].Kind != agento11y.ArtifactKindRequest || generation.Artifacts[1].Kind != agento11y.ArtifactKindProviderEvent {
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

	if generation.SystemPrompt != "" {
		t.Fatalf("chat history was duplicated into system prompt: %q", generation.SystemPrompt)
	}
	if len(generation.Input) != 2 || len(generation.Input[0].Parts) != 1 || len(generation.Input[1].Parts) != 1 {
		t.Fatalf("expected system and user input messages, got %#v", generation.Input)
	}
	if generation.Input[0].Role != agento11y.RoleSystem || generation.Input[0].Parts[0].Text != "  system prompt  " {
		t.Fatalf("unexpected system input %#v", generation.Input[0])
	}
	if generation.Input[1].Role != agento11y.RoleUser || generation.Input[1].Parts[0].Text != "  user literal \\\\n\\\\n  " {
		t.Fatalf("unexpected user input %#v", generation.Input[1])
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

func TestMapRequestMessagesKeepsInstructionsOnlyInHistory(t *testing.T) {
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

	if len(input) != 1 || input[0].Role != agento11y.RoleDeveloper || input[0].Parts[0].Text != "developer instruction" {
		t.Fatalf("unexpected mapped instruction messages: %#v", input)
	}
	if systemPrompt != "" {
		t.Fatalf("chat history was duplicated into system prompt: %q", systemPrompt)
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

func TestChatCompletionsMapsEveryChoiceAndRequestControls(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		N:     param.NewOpt(int64(2)),
		Seed:  param.NewOpt(int64(17)),
		ResponseFormat: osdk.ChatCompletionNewParamsResponseFormatUnion{
			OfJSONObject: &shared.ResponseFormatJSONObjectParam{},
		},
	}
	resp := &osdk.ChatCompletion{
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{
			{Index: 0, FinishReason: "stop", Message: osdk.ChatCompletionMessage{Content: "first"}},
			{Index: 1, FinishReason: "length", Message: osdk.ChatCompletionMessage{Content: "second"}},
		},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("map chat completion: %v", err)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("output candidates = %#v, want two", generation.Output)
	}
	if got := []string{generation.Output[0].Parts[0].Text, generation.Output[1].Parts[0].Text}; !slices.Equal(got, []string{"first", "second"}) {
		t.Fatalf("output text = %v, want [first second]", got)
	}
	if got := []string{generation.Output[0].FinishReason, generation.Output[1].FinishReason}; !slices.Equal(got, []string{"stop", "length"}) {
		t.Fatalf("finish reasons = %v, want [stop length]", got)
	}
	if generation.ChoiceCount == nil || *generation.ChoiceCount != 2 {
		t.Fatalf("choice count = %v, want 2", generation.ChoiceCount)
	}
	if generation.Seed == nil || *generation.Seed != 17 {
		t.Fatalf("seed = %v, want 17", generation.Seed)
	}
	if generation.OutputType == nil || *generation.OutputType != "json" {
		t.Fatalf("output type = %v, want json", generation.OutputType)
	}
}

func TestRequestControlsPreserveDisabledReasoning(t *testing.T) {
	chat := mapRequestControls(osdk.ChatCompletionNewParams{
		Model:           shared.ChatModel("gpt-5"),
		ReasoningEffort: shared.ReasoningEffortNone,
	})
	if chat.thinkingEnabled == nil || *chat.thinkingEnabled {
		t.Fatalf("chat thinking enabled = %v, want false", chat.thinkingEnabled)
	}

	response := mapResponsesRequestControls(marshalAny(oresponses.ResponseNewParams{
		Model:     shared.ResponsesModel("gpt-5"),
		Reasoning: shared.ReasoningParam{Effort: shared.ReasoningEffortNone},
	}))
	if response.thinkingEnabled == nil || *response.thinkingEnabled {
		t.Fatalf("response thinking enabled = %v, want false", response.thinkingEnabled)
	}
}

func TestChatCompletionsPreservesInstructionMessagePositions(t *testing.T) {
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.SystemMessage("system"),
			osdk.UserMessage("question"),
			osdk.DeveloperMessage("developer"),
			osdk.AssistantMessage("prior answer"),
		},
	}
	resp := &osdk.ChatCompletion{
		Model: "gpt-4o-mini",
		Choices: []osdk.ChatCompletionChoice{{
			FinishReason: "stop",
			Message:      osdk.ChatCompletionMessage{Content: "answer"},
		}},
	}

	generation, err := ChatCompletionsFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("map chat completion: %v", err)
	}
	roles := make([]agento11y.Role, len(generation.Input))
	for i := range generation.Input {
		roles[i] = generation.Input[i].Role
	}
	want := []agento11y.Role{agento11y.RoleSystem, agento11y.RoleUser, agento11y.RoleDeveloper, agento11y.RoleAssistant}
	if !slices.Equal(roles, want) {
		t.Fatalf("input roles = %v, want %v", roles, want)
	}
	if generation.SystemPrompt != "" {
		t.Fatalf("chat history was duplicated into system prompt: %q", generation.SystemPrompt)
	}
}

func TestChatCompletionsStreamAccumulatesByChoiceAndToolIndex(t *testing.T) {
	req := osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")}
	summary := ChatCompletionsStreamSummary{Chunks: []osdk.ChatCompletionChunk{
		{Choices: []osdk.ChatCompletionChunkChoice{
			{Index: 1, Delta: osdk.ChatCompletionChunkChoiceDelta{Content: "sec", ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0, ID: "call_second", Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "second_tool", Arguments: `{"value":"se`}}}}},
			{Index: 0, Delta: osdk.ChatCompletionChunkChoiceDelta{Content: "fir", ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0, ID: "call_first", Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Name: "first_tool", Arguments: `{"value":"fi`}}}}},
		}},
		{Choices: []osdk.ChatCompletionChunkChoice{
			{Index: 0, FinishReason: "tool_calls", Delta: osdk.ChatCompletionChunkChoiceDelta{Content: "st", ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0, Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Arguments: `rst"}`}}}}},
			{Index: 1, FinishReason: "length", Delta: osdk.ChatCompletionChunkChoiceDelta{Content: "ond", ToolCalls: []osdk.ChatCompletionChunkChoiceDeltaToolCall{{Index: 0, Function: osdk.ChatCompletionChunkChoiceDeltaToolCallFunction{Arguments: `cond"}`}}}}},
		}},
	}}

	generation, err := ChatCompletionsFromStream(req, summary)
	if err != nil {
		t.Fatalf("map chat stream: %v", err)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("output candidates = %#v, want two", generation.Output)
	}
	for i, want := range []struct {
		text, finish, id, name, arguments string
	}{
		{text: "first", finish: "tool_calls", id: "call_first", name: "first_tool", arguments: `{"value":"first"}`},
		{text: "second", finish: "length", id: "call_second", name: "second_tool", arguments: `{"value":"second"}`},
	} {
		candidate := generation.Output[i]
		if len(candidate.Parts) != 2 || candidate.Parts[0].Text != want.text || candidate.FinishReason != want.finish {
			t.Fatalf("candidate %d = %#v", i, candidate)
		}
		call := candidate.Parts[1].ToolCall
		if call == nil || call.ID != want.id || call.Name != want.name || string(call.InputJSON) != want.arguments {
			t.Fatalf("candidate %d tool call = %#v", i, call)
		}
	}
}

func TestChatCompletionsPreservesFinishOnlyCandidates(t *testing.T) {
	req := osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")}

	t.Run("sync", func(t *testing.T) {
		response := &osdk.ChatCompletion{Choices: []osdk.ChatCompletionChoice{
			{Index: 0, FinishReason: "stop", Message: osdk.ChatCompletionMessage{Content: "answer"}},
			{
				Index:        1,
				FinishReason: "content_filter",
				Message: osdk.ChatCompletionMessage{ToolCalls: []osdk.ChatCompletionMessageToolCallUnion{{
					Function: osdk.ChatCompletionMessageFunctionToolCallFunction{},
				}}},
			},
		}}
		generation, err := ChatCompletionsFromRequestResponse(req, response)
		if err != nil {
			t.Fatalf("map chat completion: %v", err)
		}
		assertFinishOnlyCandidate(t, generation)
	})

	t.Run("stream", func(t *testing.T) {
		summary := ChatCompletionsStreamSummary{Chunks: []osdk.ChatCompletionChunk{{Choices: []osdk.ChatCompletionChunkChoice{
			{Index: 0, FinishReason: "stop", Delta: osdk.ChatCompletionChunkChoiceDelta{Content: "answer"}},
			{Index: 1, FinishReason: "content_filter"},
		}}}}
		generation, err := ChatCompletionsFromStream(req, summary)
		if err != nil {
			t.Fatalf("map chat stream: %v", err)
		}
		assertFinishOnlyCandidate(t, generation)
	})
}

func assertFinishOnlyCandidate(t *testing.T, generation agento11y.Generation) {
	t.Helper()
	if len(generation.Output) != 2 || len(generation.Output[0].Parts) != 1 || len(generation.Output[1].Parts) != 0 {
		t.Fatalf("output candidates = %#v, want populated and finish-only candidates", generation.Output)
	}
	if generation.Output[0].FinishReason != "stop" || generation.Output[1].FinishReason != "content_filter" {
		t.Fatalf("finish reasons = %q, %q", generation.Output[0].FinishReason, generation.Output[1].FinishReason)
	}
}

func TestOpenAIAbsentUsageRemainsUnknown(t *testing.T) {
	chat, err := ChatCompletionsFromRequestResponse(
		osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")},
		&osdk.ChatCompletion{Choices: []osdk.ChatCompletionChoice{{FinishReason: "stop", Message: osdk.ChatCompletionMessage{Content: "answer"}}}},
	)
	if err != nil {
		t.Fatalf("map chat completion: %v", err)
	}
	if chat.Usage.InputTokensReported || chat.Usage.OutputTokensReported {
		t.Fatalf("chat usage = %+v, want unknown counters", chat.Usage)
	}

	response, err := ResponsesFromRequestResponse(
		oresponses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")},
		&oresponses.Response{Status: oresponses.ResponseStatusFailed},
	)
	if err != nil {
		t.Fatalf("map response: %v", err)
	}
	if response.Usage.InputTokensReported || response.Usage.OutputTokensReported {
		t.Fatalf("responses usage = %+v, want unknown counters", response.Usage)
	}
}

func TestOpenAIUsagePresenceIsPerCounter(t *testing.T) {
	var chatUsage osdk.CompletionUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":0}`), &chatUsage); err != nil {
		t.Fatalf("decode chat usage: %v", err)
	}
	mappedChat := mapUsage(chatUsage)
	if !mappedChat.InputTokensReported || mappedChat.OutputTokensReported {
		t.Fatalf("chat usage presence = input %v output %v, want true false", mappedChat.InputTokensReported, mappedChat.OutputTokensReported)
	}

	var responseUsage oresponses.ResponseUsage
	if err := json.Unmarshal([]byte(`{"output_tokens":0}`), &responseUsage); err != nil {
		t.Fatalf("decode response usage: %v", err)
	}
	mappedResponse := mapResponsesUsage(responseUsage)
	if mappedResponse.InputTokensReported || !mappedResponse.OutputTokensReported {
		t.Fatalf("response usage presence = input %v output %v, want false true", mappedResponse.InputTokensReported, mappedResponse.OutputTokensReported)
	}
}

func TestChatCompletionsStreamPreservesReportedZeroUsage(t *testing.T) {
	var chunk osdk.ChatCompletionChunk
	if err := json.Unmarshal([]byte(`{"id":"chat_zero","model":"gpt-4o-mini","choices":[{"index":0,"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`), &chunk); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}

	generation, err := ChatCompletionsFromStream(
		osdk.ChatCompletionNewParams{Model: shared.ChatModel("gpt-4o-mini")},
		ChatCompletionsStreamSummary{Chunks: []osdk.ChatCompletionChunk{chunk}},
	)
	if err != nil {
		t.Fatalf("map chat stream: %v", err)
	}
	if !generation.Usage.InputTokensReported || !generation.Usage.OutputTokensReported {
		t.Fatalf("reported zero usage was lost: %+v", generation.Usage)
	}
}

func TestResponsesStreamGroupsPartsByOutputIndex(t *testing.T) {
	req := oresponses.ResponseNewParams{Model: shared.ResponsesModel("gpt-5")}
	summary := ResponsesStreamSummary{Events: []oresponses.ResponseStreamEventUnion{
		{Type: "response.output_text.delta", ItemID: "message_1", OutputIndex: 1, ContentIndex: 0, Delta: "after tool"},
		{Type: "response.output_item.added", OutputIndex: 0, Item: oresponses.ResponseOutputItemUnion{ID: "fc_1", Type: "function_call", CallID: "call_first", Name: "first"}},
		{Type: "response.function_call_arguments.done", ItemID: "fc_1", OutputIndex: 0, Name: "first", Arguments: `{}`},
		{Type: "response.completed"},
	}}

	generation, err := ResponsesFromStream(req, summary)
	if err != nil {
		t.Fatalf("map response stream: %v", err)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 2 {
		t.Fatalf("output = %#v, want one candidate with two parts", generation.Output)
	}
	if generation.Output[0].Parts[0].ToolCall == nil || generation.Output[0].Parts[0].ToolCall.ID != "call_first" {
		t.Fatalf("first output part = %#v, want tool call", generation.Output[0].Parts[0])
	}
	if generation.Output[0].Parts[1].Text != "after tool" {
		t.Fatalf("second output part = %#v, want text", generation.Output[0].Parts[1])
	}
	if generation.ResponseStatus == nil || *generation.ResponseStatus != "completed" {
		t.Fatalf("response status = %v, want completed", generation.ResponseStatus)
	}
}

func TestResponsesInputContentPreservesPartOrderAndMedia(t *testing.T) {
	input, _ := mapResponsesRequestInput(map[string]any{
		"input": []any{map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "before"},
				map[string]any{"type": "input_image", "image_url": "https://example.test/image.png"},
				map[string]any{"type": "input_text", "text": "after"},
				map[string]any{"type": "input_file", "file_id": "file_123", "filename": "facts.pdf"},
				map[string]any{"type": "input_file", "file_data": "cGRm", "filename": "inline.pdf"},
			},
		}},
	})
	if len(input) != 1 || len(input[0].Parts) != 5 {
		t.Fatalf("input = %#v, want one five-part message", input)
	}
	if input[0].Parts[0].Text != "before" || input[0].Parts[2].Text != "after" {
		t.Fatalf("text part order = %#v", input[0].Parts)
	}
	if image := input[0].Parts[1].Media; image == nil || image.Kind != "image" || image.URL != "https://example.test/image.png" {
		t.Fatalf("image part = %#v", input[0].Parts[1])
	}
	if file := input[0].Parts[3].Media; file == nil || file.Kind != "file" || file.URL != "file_123" || file.Name != "facts.pdf" {
		t.Fatalf("file part = %#v", input[0].Parts[3])
	}
	if file := input[0].Parts[4].Media; file == nil || file.URL != "data:application/octet-stream;base64,cGRm" || file.Name != "inline.pdf" {
		t.Fatalf("inline file part = %#v", input[0].Parts[4])
	}
}

func TestResponsesMapsHistoryAndOneOrderedOutputCandidate(t *testing.T) {
	req := oresponses.ResponseNewParams{
		Model:        shared.ResponsesModel("gpt-5"),
		Instructions: param.NewOpt("root instruction"),
		Input: oresponses.ResponseNewParamsInputUnion{OfInputItemList: oresponses.ResponseInputParam{
			{OfMessage: &oresponses.EasyInputMessageParam{Role: oresponses.EasyInputMessageRoleSystem, Content: oresponses.EasyInputMessageContentUnionParam{OfString: param.NewOpt("system in place")}}},
			{OfMessage: &oresponses.EasyInputMessageParam{Role: oresponses.EasyInputMessageRoleUser, Content: oresponses.EasyInputMessageContentUnionParam{OfString: param.NewOpt("question")}}},
			{OfMessage: &oresponses.EasyInputMessageParam{Role: oresponses.EasyInputMessageRoleDeveloper, Content: oresponses.EasyInputMessageContentUnionParam{OfString: param.NewOpt("developer in place")}}},
			{OfFunctionCall: &oresponses.ResponseFunctionToolCallParam{CallID: "call_weather", Name: "weather", Arguments: `{"city":"Paris"}`}},
			{OfFunctionCallOutput: &oresponses.ResponseInputItemFunctionCallOutputParam{CallID: "call_weather", Output: oresponses.ResponseInputItemFunctionCallOutputOutputUnionParam{OfString: param.NewOpt(`{"temp":18}`)}}},
		}},
		Text: oresponses.ResponseTextConfigParam{Format: oresponses.ResponseFormatTextConfigParamOfJSONSchema("answer", map[string]any{"type": "object"})},
	}
	resp := &oresponses.Response{
		Model:  shared.ResponsesModel("gpt-5"),
		Status: oresponses.ResponseStatusCompleted,
		Output: []oresponses.ResponseOutputItemUnion{
			{Type: "message", Content: []oresponses.ResponseOutputMessageContentUnion{{Type: "output_text", Text: "first part"}, {Type: "output_text", Text: "second part"}}},
			{Type: "function_call", CallID: "call_next", Name: "next", Arguments: oresponses.ResponseOutputItemUnionArguments{OfString: `{}`}},
		},
	}

	generation, err := ResponsesFromRequestResponse(req, resp)
	if err != nil {
		t.Fatalf("map response: %v", err)
	}
	if generation.SystemPrompt != "root instruction" {
		t.Fatalf("system prompt = %q, want root instruction", generation.SystemPrompt)
	}
	roles := make([]agento11y.Role, len(generation.Input))
	for i := range generation.Input {
		roles[i] = generation.Input[i].Role
	}
	wantRoles := []agento11y.Role{agento11y.RoleSystem, agento11y.RoleUser, agento11y.RoleDeveloper, agento11y.RoleAssistant, agento11y.RoleTool}
	if !slices.Equal(roles, wantRoles) {
		t.Fatalf("input roles = %v, want %v", roles, wantRoles)
	}
	if generation.Input[3].Parts[0].ToolCall == nil || generation.Input[3].Parts[0].ToolCall.ID != "call_weather" {
		t.Fatalf("replayed function call = %#v", generation.Input[3])
	}
	if generation.Input[4].Parts[0].ToolResult == nil || generation.Input[4].Parts[0].ToolResult.ToolCallID != "call_weather" {
		t.Fatalf("function result = %#v", generation.Input[4])
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 3 {
		t.Fatalf("output = %#v, want one candidate with three parts", generation.Output)
	}
	if generation.Output[0].Parts[0].Text != "first part" || generation.Output[0].Parts[1].Text != "second part" || generation.Output[0].Parts[2].ToolCall == nil {
		t.Fatalf("ordered output parts = %#v", generation.Output[0].Parts)
	}
	if generation.OutputType == nil || *generation.OutputType != "json" {
		t.Fatalf("output type = %v, want json", generation.OutputType)
	}
	if generation.ResponseStatus == nil || *generation.ResponseStatus != "completed" {
		t.Fatalf("response status = %v, want completed", generation.ResponseStatus)
	}
}
