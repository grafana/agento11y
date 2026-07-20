package gemini

import (
	"math"
	"strings"
	"testing"
	"time"

	"google.golang.org/genai"

	sigil "github.com/grafana/agento11y/go/sigil"
	"github.com/grafana/agento11y/go/sigil/sigiltest"
)

const (
	geminiSpanErrorCategory = "error.category"
	geminiSpanInputCount    = "gen_ai.embeddings.input_count"
	geminiSpanDimCount      = "gen_ai.embeddings.dimension.count"
)

func TestConformance_GeminiSyncMapping(t *testing.T) {
	env := sigiltest.NewEnv(t)

	model, contents, config := geminiConformanceRequest()
	resp := &genai.GenerateContentResponse{
		ResponseID:   "resp_gemini_sync",
		ModelVersion: "gemini-2.5-pro-001",
		Candidates: []*genai.Candidate{
			{
				FinishReason: genai.FinishReasonStop,
				Content: genai.NewContentFromParts([]*genai.Part{
					{Text: "need weather tool", Thought: true},
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
	start := sigil.GenerationStart{
		ConversationID:    "conv-gemini-sync",
		ConversationTitle: "Gemini sync",
		AgentName:         "agent-gemini",
		AgentVersion:      "v-gemini",
		Model:             sigil.ModelRef{Provider: "gemini", Name: model},
	}

	generation, err := FromRequestResponse(
		model,
		contents,
		config,
		resp,
		WithConversationID(start.ConversationID),
		WithConversationTitle(start.ConversationTitle),
		WithAgentName(start.AgentName),
		WithAgentVersion(start.AgentVersion),
		WithTag("tenant", "t-gemini"),
	)
	sigiltest.RecordGeneration(t, env, start, generation, err)
	env.Shutdown(t)

	exported := env.SingleGenerationJSON(t)

	if got := sigiltest.StringValue(t, exported, "mode"); got != "GENERATION_MODE_SYNC" {
		t.Fatalf("unexpected mode: got %q want %q\n%s", got, "GENERATION_MODE_SYNC", sigiltest.DebugJSON(exported))
	}
	if got := sigiltest.StringValue(t, exported, "stop_reason"); got != "STOP" {
		t.Fatalf("unexpected stop_reason: got %q want %q", got, "STOP")
	}
	if got := sigiltest.StringValue(t, exported, "output", 0, "parts", 0, "thinking"); got != "need weather tool" {
		t.Fatalf("unexpected thinking part: got %q want %q", got, "need weather tool")
	}
	if got := sigiltest.StringValue(t, exported, "output", 0, "parts", 1, "tool_call", "name"); got != "weather" {
		t.Fatalf("unexpected tool_call.name: got %q want %q", got, "weather")
	}
	if got := sigiltest.StringValue(t, exported, "output", 0, "parts", 2, "text"); got != "It is 18C and sunny." {
		t.Fatalf("unexpected output text: got %q want %q", got, "It is 18C and sunny.")
	}
	if got := sigiltest.StringValue(t, exported, "input", 1, "role"); got != "MESSAGE_ROLE_TOOL" {
		t.Fatalf("unexpected tool input role: got %q want %q", got, "MESSAGE_ROLE_TOOL")
	}
	if got := sigiltest.StringValue(t, exported, "usage", "reasoning_tokens"); got != "10" {
		t.Fatalf("unexpected usage.reasoning_tokens: got %q want %q", got, "10")
	}
	if got := sigiltest.FloatValue(t, exported, "metadata", "sigil.gen_ai.usage.tool_use_prompt_tokens"); got != 9 {
		t.Fatalf("unexpected tool_use_prompt_tokens: got %v want %v", got, float64(9))
	}
}

func TestConformance_GeminiStreamMapping(t *testing.T) {
	env := sigiltest.NewEnv(t)

	model, contents, config := geminiConformanceRequest()
	summary := StreamSummary{
		FirstChunkAt: time.Unix(1_741_780_200, 0).UTC(),
		Responses: []*genai.GenerateContentResponse{
			{
				ResponseID:   "resp_gemini_stream_1",
				ModelVersion: "gemini-2.5-pro-001",
				Candidates: []*genai.Candidate{
					{
						Content: genai.NewContentFromParts([]*genai.Part{
							{Text: "need weather tool", Thought: true},
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
				ResponseID:   "resp_gemini_stream_2",
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
					ThoughtsTokenCount:      4,
					ToolUsePromptTokenCount: 5,
				},
			},
		},
	}
	start := sigil.GenerationStart{
		ConversationID: "conv-gemini-stream",
		AgentName:      "agent-gemini-stream",
		AgentVersion:   "v-gemini-stream",
		Model:          sigil.ModelRef{Provider: "gemini", Name: model},
	}

	generation, err := FromStream(
		model,
		contents,
		config,
		summary,
		WithConversationID(start.ConversationID),
		WithAgentName(start.AgentName),
		WithAgentVersion(start.AgentVersion),
	)
	sigiltest.RecordStreamingGeneration(t, env, start, summary.FirstChunkAt, generation, err)
	env.Shutdown(t)

	exported := env.SingleGenerationJSON(t)

	if got := sigiltest.StringValue(t, exported, "mode"); got != "GENERATION_MODE_STREAM" {
		t.Fatalf("unexpected mode: got %q want %q\n%s", got, "GENERATION_MODE_STREAM", sigiltest.DebugJSON(exported))
	}
	if got := sigiltest.StringValue(t, exported, "response_id"); got != "resp_gemini_stream_2" {
		t.Fatalf("unexpected response_id: got %q want %q", got, "resp_gemini_stream_2")
	}
	if got := sigiltest.StringValue(t, exported, "stop_reason"); got != "STOP" {
		t.Fatalf("unexpected stop_reason: got %q want %q", got, "STOP")
	}
	if got := sigiltest.StringValue(t, exported, "output", 0, "parts", 0, "thinking"); got != "need weather tool" {
		t.Fatalf("unexpected streamed thinking part: got %q want %q", got, "need weather tool")
	}
	if got := sigiltest.StringValue(t, exported, "output", 0, "parts", 1, "tool_call", "name"); got != "weather" {
		t.Fatalf("unexpected streamed tool_call.name: got %q want %q", got, "weather")
	}
	if got := sigiltest.StringValue(t, exported, "output", 1, "parts", 0, "text"); got != "It is 18C and sunny." {
		t.Fatalf("unexpected streamed output text: got %q want %q", got, "It is 18C and sunny.")
	}
	if got := sigiltest.StringValue(t, exported, "usage", "total_tokens"); got != "26" {
		t.Fatalf("unexpected usage.total_tokens: got %q want %q", got, "26")
	}
}

func TestConformance_GeminiErrorMapping(t *testing.T) {
	env := sigiltest.NewEnv(t)

	sigiltest.RecordCallError(t, env, sigil.GenerationStart{
		Model: sigil.ModelRef{Provider: "gemini", Name: "gemini-2.5-pro"},
	}, genai.APIError{Code: 429, Message: "rate limited", Status: "RESOURCE_EXHAUSTED"})

	span := sigiltest.FindSpan(t, env.Spans.Ended(), "generateText gemini-2.5-pro")
	attrs := sigiltest.SpanAttributes(span)
	if got := attrs[geminiSpanErrorCategory].AsString(); got != "rate_limit" {
		t.Fatalf("unexpected error.category: got %q want %q", got, "rate_limit")
	}

	env.Shutdown(t)
	exported := env.SingleGenerationJSON(t)
	callError := sigiltest.StringValue(t, exported, "call_error")
	if !strings.Contains(callError, "429") {
		t.Fatalf("expected call_error to include status code, got %q", callError)
	}
}

func TestConformance_GeminiEmbeddingMapping(t *testing.T) {
	env := sigiltest.NewEnv(t)

	model := "gemini-embedding-001"
	contents := []*genai.Content{
		genai.NewContentFromText("hello", genai.RoleUser),
		genai.NewContentFromText("world", genai.RoleUser),
	}
	dimensions := int32(3)
	config := &genai.EmbedContentConfig{
		OutputDimensionality: &dimensions,
	}
	resp := &genai.EmbedContentResponse{
		Embeddings: []*genai.ContentEmbedding{
			{
				Values: []float32{0.1, 0.2, 0.3},
				Statistics: &genai.ContentEmbeddingStatistics{
					TokenCount: 2,
				},
			},
			{
				Values: []float32{0.4, 0.5, 0.6},
				Statistics: &genai.ContentEmbeddingStatistics{
					TokenCount: 2,
				},
			},
		},
	}
	startDimensions := int64(dimensions)
	sigiltest.RecordEmbedding(t, env, sigil.EmbeddingStart{
		Model:        sigil.ModelRef{Provider: "gemini", Name: model},
		AgentName:    "agent-gemini-embed",
		AgentVersion: "v-gemini-embed",
		Dimensions:   &startDimensions,
	}, EmbeddingFromResponse(model, contents, config, resp))

	span := sigiltest.FindSpan(t, env.Spans.Ended(), "embeddings gemini-embedding-001")
	attrs := sigiltest.SpanAttributes(span)
	if got := attrs[geminiSpanInputCount].AsInt64(); got != 2 {
		t.Fatalf("unexpected gen_ai.embeddings.input_count: got %d want %d", got, 2)
	}
	if got := attrs[geminiSpanDimCount].AsInt64(); got != 3 {
		t.Fatalf("unexpected gen_ai.embeddings.dimension.count: got %d want %d", got, 3)
	}

	env.Shutdown(t)
	sigiltest.RequireRequestCount(t, env, 0)
}

func geminiConformanceRequest() (string, []*genai.Content, *genai.GenerateContentConfig) {
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
	return model, contents, config
}

func TestConformance_GenerateContentSyncNormalization(t *testing.T) {
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
						Text:    "reasoning trace",
						Thought: true,
					},
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
		WithConversationID("conv-gemini-sync"),
		WithConversationTitle("Paris weather"),
		WithAgentName("agent-gemini"),
		WithAgentVersion("v-gemini"),
		WithTag("tenant", "t-123"),
		WithRawArtifacts(),
	)
	if err != nil {
		t.Fatalf("gemini sync mapping: %v", err)
	}

	if generation.Model.Provider != "gemini" || generation.Model.Name != "gemini-2.5-pro" {
		t.Fatalf("unexpected model mapping: %#v", generation.Model)
	}
	if generation.ConversationID != "conv-gemini-sync" || generation.ConversationTitle != "Paris weather" {
		t.Fatalf("unexpected conversation mapping: %#v", generation)
	}
	if generation.AgentName != "agent-gemini" || generation.AgentVersion != "v-gemini" {
		t.Fatalf("unexpected agent mapping: name=%q version=%q", generation.AgentName, generation.AgentVersion)
	}
	if generation.ResponseID != "resp_1" || generation.ResponseModel != "gemini-2.5-pro-001" {
		t.Fatalf("unexpected response mapping: id=%q model=%q", generation.ResponseID, generation.ResponseModel)
	}
	if generation.StopReason != "STOP" {
		t.Fatalf("unexpected stop reason: %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 160 || generation.Usage.CacheReadInputTokens != 12 || generation.Usage.ReasoningTokens != 10 {
		t.Fatalf("unexpected usage mapping: %#v", generation.Usage)
	}
	if generation.ThinkingEnabled == nil || !*generation.ThinkingEnabled {
		t.Fatalf("expected thinking enabled true, got %v", generation.ThinkingEnabled)
	}
	if generation.Temperature == nil || math.Abs(*generation.Temperature-0.4) > 1e-6 {
		t.Fatalf("unexpected temperature: %v", generation.Temperature)
	}
	if generation.TopP == nil || math.Abs(*generation.TopP-0.75) > 1e-6 {
		t.Fatalf("unexpected top_p: %v", generation.TopP)
	}
	if len(generation.Output) != 1 || len(generation.Output[0].Parts) != 3 {
		t.Fatalf("expected thinking + tool call + text output, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Kind != sigil.PartKindThinking || generation.Output[0].Parts[0].Thinking != "reasoning trace" {
		t.Fatalf("unexpected thinking output: %#v", generation.Output[0].Parts[0])
	}
	if generation.Output[0].Parts[1].Kind != sigil.PartKindToolCall {
		t.Fatalf("expected tool call output, got %#v", generation.Output[0].Parts[1])
	}
	if generation.Output[0].Parts[2].Kind != sigil.PartKindText || generation.Output[0].Parts[2].Text != "It is 18C and sunny." {
		t.Fatalf("unexpected text output: %#v", generation.Output[0].Parts[2])
	}
	if generation.Metadata["sigil.gen_ai.request.thinking.level"] != "high" {
		t.Fatalf("unexpected thinking level metadata: %#v", generation.Metadata)
	}
	if generation.Tags["tenant"] != "t-123" {
		t.Fatalf("expected tenant tag")
	}
	requireGeminiArtifactKinds(t, generation.Artifacts,
		sigil.ArtifactKindRequest,
		sigil.ArtifactKindResponse,
		sigil.ArtifactKindTools,
	)
}

func TestConformance_GenerateContentStreamNormalization(t *testing.T) {
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
			IncludeThoughts: true,
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
								Text:    "reasoning trace",
								Thought: true,
							},
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
					ThoughtsTokenCount:      4,
					ToolUsePromptTokenCount: 5,
				},
			},
		},
	}

	generation, err := FromStream(model, contents, config, summary,
		WithConversationID("conv-gemini-stream"),
		WithAgentName("agent-gemini-stream"),
		WithAgentVersion("v-gemini-stream"),
		WithRawArtifacts(),
	)
	if err != nil {
		t.Fatalf("gemini stream mapping: %v", err)
	}

	if generation.ConversationID != "conv-gemini-stream" || generation.AgentName != "agent-gemini-stream" || generation.AgentVersion != "v-gemini-stream" {
		t.Fatalf("unexpected identity mapping: %#v", generation)
	}
	if generation.ResponseID != "resp_stream_2" || generation.ResponseModel != "gemini-2.5-pro-001" {
		t.Fatalf("unexpected response mapping: id=%q model=%q", generation.ResponseID, generation.ResponseModel)
	}
	if generation.StopReason != "STOP" {
		t.Fatalf("unexpected stop reason: %q", generation.StopReason)
	}
	if generation.Usage.TotalTokens != 26 || generation.Usage.ReasoningTokens != 4 {
		t.Fatalf("unexpected usage mapping: %#v", generation.Usage)
	}
	if len(generation.Output) != 2 {
		t.Fatalf("expected streamed thinking/tool output plus final text, got %#v", generation.Output)
	}
	if generation.Output[0].Parts[0].Kind != sigil.PartKindThinking || generation.Output[0].Parts[0].Thinking != "reasoning trace" {
		t.Fatalf("unexpected streamed thinking output: %#v", generation.Output[0].Parts[0])
	}
	if generation.Output[0].Parts[1].Kind != sigil.PartKindToolCall {
		t.Fatalf("expected streamed tool call output, got %#v", generation.Output[0].Parts[1])
	}
	if generation.Output[1].Parts[0].Kind != sigil.PartKindText || generation.Output[1].Parts[0].Text != "It is 18C and sunny." {
		t.Fatalf("unexpected streamed text output: %#v", generation.Output[1].Parts[0])
	}
	requireGeminiArtifactKinds(t, generation.Artifacts,
		sigil.ArtifactKindRequest,
		sigil.ArtifactKindTools,
		sigil.ArtifactKindProviderEvent,
	)
}

func TestConformance_GeminiMapperValidationErrors(t *testing.T) {
	if _, err := FromRequestResponse("", nil, nil, &genai.GenerateContentResponse{}); err == nil || err.Error() != "request model is required" {
		t.Fatalf("expected explicit request model error, got %v", err)
	}
	if _, err := FromRequestResponse("gemini-2.5-pro", nil, nil, nil); err == nil || err.Error() != "response is required" {
		t.Fatalf("expected explicit response error, got %v", err)
	}
	if _, err := FromStream("gemini-2.5-pro", nil, nil, StreamSummary{}); err == nil || err.Error() != "stream summary has no responses" {
		t.Fatalf("expected explicit stream error, got %v", err)
	}

	_, err := FromRequestResponse(
		"gemini-2.5-pro",
		nil,
		nil,
		&genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					Content: genai.NewContentFromText("ok", genai.RoleModel),
				},
			},
		},
		WithProviderName(""),
	)
	if err == nil || err.Error() != "generation.model.provider is required" {
		t.Fatalf("expected explicit validation error for invalid provider mapping, got %v", err)
	}
}

func requireGeminiArtifactKinds(t *testing.T, artifacts []sigil.Artifact, want ...sigil.ArtifactKind) {
	t.Helper()

	if len(artifacts) != len(want) {
		t.Fatalf("expected %d artifacts, got %d", len(want), len(artifacts))
	}
	for i, kind := range want {
		if artifacts[i].Kind != kind {
			t.Fatalf("artifact %d kind mismatch: got %q want %q", i, artifacts[i].Kind, kind)
		}
	}
}
