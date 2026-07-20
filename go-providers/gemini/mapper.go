package gemini

import (
	"encoding/json"
	"errors"
	"maps"
	"strings"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/sigil"
)

const thinkingBudgetMetadataKey = "sigil.gen_ai.request.thinking.budget_tokens"
const thinkingLevelMetadataKey = "sigil.gen_ai.request.thinking.level"
const usageToolUsePromptTokensMetadataKey = "sigil.gen_ai.usage.tool_use_prompt_tokens"

// FromRequestResponse maps a Gemini request/response pair to sigil.Generation.
func FromRequestResponse(
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	resp *genai.GenerateContentResponse,
	opts ...Option,
) (sigil.Generation, error) {
	if resp == nil {
		return sigil.Generation{}, errors.New("response is required")
	}
	if strings.TrimSpace(model) == "" {
		return sigil.Generation{}, errors.New("request model is required")
	}

	options := applyOptions(opts)
	input := mapContents(contents)
	output, stopReason := mapCandidates(resp.Candidates)
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapRequestControls(config)
	thinkingLevel := extractThinkingLevel(config)

	artifacts := make([]sigil.Artifact, 0, 3)
	if options.includeRequestArtifact {
		requestPayload := map[string]any{
			"model":    model,
			"contents": contents,
			"config":   config,
		}
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindRequest, "gemini.generate_content.request", requestPayload)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeResponseArtifact {
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindResponse, "gemini.generate_content.response", resp)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && hasFunctionTools(config) {
		artifact, err := sigil.NewJSONArtifact(sigil.ArtifactKindTools, "gemini.generate_content.tools", config.Tools)
		if err != nil {
			return sigil.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	metadata := cloneAnyMap(options.metadata)
	if resp.ModelVersion != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["model_version"] = resp.ModelVersion
	}
	metadata = mergeThinkingBudgetMetadata(metadata, thinkingBudget)
	metadata = mergeThinkingLevelMetadata(metadata, thinkingLevel)
	metadata = mergeGeminiUsageMetadata(metadata, resp.UsageMetadata)

	generation := sigil.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             sigil.ModelRef{Provider: options.providerName, Name: model},
		ResponseID:        resp.ResponseID,
		ResponseModel:     resp.ModelVersion,
		SystemPrompt:      extractSystemPrompt(config),
		Input:             input,
		Output:            output,
		Tools:             mapTools(config),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             mapUsage(resp.UsageMetadata),
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          metadata,
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return sigil.Generation{}, err
	}

	return generation, nil
}

// EmbeddingFromResponse maps a Gemini embed-content request/response pair to sigil.EmbeddingResult.
func EmbeddingFromResponse(
	model string,
	contents []*genai.Content,
	config *genai.EmbedContentConfig,
	resp *genai.EmbedContentResponse,
) sigil.EmbeddingResult {
	_ = model

	result := sigil.EmbeddingResult{
		InputCount: embeddingInputCount(contents),
		InputTexts: embeddingInputTexts(contents),
	}

	if resp == nil {
		if config != nil && config.OutputDimensionality != nil && *config.OutputDimensionality > 0 {
			dimensions := int64(*config.OutputDimensionality)
			result.Dimensions = &dimensions
		}
		return result
	}

	var inputTokens int64
	for _, embedding := range resp.Embeddings {
		if embedding == nil {
			continue
		}
		if embedding.Statistics != nil && embedding.Statistics.TokenCount > 0 {
			inputTokens += int64(embedding.Statistics.TokenCount)
		}
		if result.Dimensions == nil && len(embedding.Values) > 0 {
			dimensions := int64(len(embedding.Values))
			result.Dimensions = &dimensions
		}
	}
	result.InputTokens = inputTokens

	if result.Dimensions == nil && config != nil && config.OutputDimensionality != nil && *config.OutputDimensionality > 0 {
		dimensions := int64(*config.OutputDimensionality)
		result.Dimensions = &dimensions
	}

	return result
}

func mapContents(contents []*genai.Content) []sigil.Message {
	if len(contents) == 0 {
		return nil
	}

	out := make([]sigil.Message, 0, len(contents)+1)
	for _, content := range contents {
		if content == nil {
			continue
		}

		role := mapRole(content.Role)
		roleParts := make([]sigil.Part, 0, len(content.Parts))
		assistantParts := make([]sigil.Part, 0, 1)
		toolParts := make([]sigil.Part, 0, 1)

		for _, part := range content.Parts {
			if part == nil {
				continue
			}

			if text := part.Text; text != "" {
				if part.Thought && role == sigil.RoleAssistant {
					roleParts = append(roleParts, sigil.ThinkingPart(text))
				} else {
					roleParts = append(roleParts, sigil.TextPart(text))
				}
			}

			if part.FunctionCall != nil {
				call := sigil.ToolCallPart(sigil.ToolCall{
					ID:        part.FunctionCall.ID,
					Name:      part.FunctionCall.Name,
					InputJSON: marshalAny(part.FunctionCall.Args),
				})
				call.Metadata.ProviderType = "function_call"
				if role == sigil.RoleAssistant {
					roleParts = append(roleParts, call)
				} else {
					assistantParts = append(assistantParts, call)
				}
			}

			if part.FunctionResponse != nil {
				result := sigil.ToolResultPart(sigil.ToolResult{
					ToolCallID:  part.FunctionResponse.ID,
					Name:        part.FunctionResponse.Name,
					ContentJSON: marshalAny(part.FunctionResponse.Response),
				})
				result.Metadata.ProviderType = "function_response"
				toolParts = append(toolParts, result)
			}
		}

		if len(roleParts) > 0 {
			out = append(out, sigil.Message{Role: role, Parts: roleParts})
		}
		if len(assistantParts) > 0 {
			out = append(out, sigil.Message{Role: sigil.RoleAssistant, Parts: assistantParts})
		}
		if len(toolParts) > 0 {
			out = append(out, sigil.Message{Role: sigil.RoleTool, Parts: toolParts})
		}
	}

	return out
}

func embeddingInputCount(contents []*genai.Content) int {
	count := 0
	for _, content := range contents {
		if content != nil {
			count++
		}
	}
	return count
}

func embeddingInputTexts(contents []*genai.Content) []string {
	if len(contents) == 0 {
		return nil
	}
	out := make([]string, 0, len(contents))
	for _, content := range contents {
		text := embeddingContentText(content)
		if text != "" {
			out = append(out, text)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func embeddingContentText(content *genai.Content) string {
	if content == nil || len(content.Parts) == 0 {
		return ""
	}
	chunks := make([]string, 0, len(content.Parts))
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		if text := part.Text; text != "" {
			chunks = append(chunks, text)
		}
	}
	return strings.Join(chunks, "\n")
}

func mapCandidates(candidates []*genai.Candidate) ([]sigil.Message, string) {
	if len(candidates) == 0 {
		return nil, ""
	}

	out := make([]sigil.Message, 0, len(candidates))
	stopReason := ""
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if stopReason == "" && candidate.FinishReason != "" {
			stopReason = string(candidate.FinishReason)
		}

		contentMessages := mapContents([]*genai.Content{candidate.Content})
		out = append(out, contentMessages...)
	}

	return out, stopReason
}

func mapTools(config *genai.GenerateContentConfig) []sigil.ToolDefinition {
	if config == nil || len(config.Tools) == 0 {
		return nil
	}

	out := make([]sigil.ToolDefinition, 0, len(config.Tools))
	for _, tool := range config.Tools {
		if tool == nil {
			continue
		}
		for _, declaration := range tool.FunctionDeclarations {
			if declaration == nil || strings.TrimSpace(declaration.Name) == "" {
				continue
			}
			definition := sigil.ToolDefinition{
				Name:        declaration.Name,
				Description: declaration.Description,
				Type:        "function",
			}
			if declaration.ParametersJsonSchema != nil {
				definition.InputSchema = marshalAny(declaration.ParametersJsonSchema)
			} else if declaration.Parameters != nil {
				definition.InputSchema = marshalAny(declaration.Parameters)
			}
			out = append(out, definition)
		}
	}

	return out
}

func mapUsage(usage *genai.GenerateContentResponseUsageMetadata) sigil.TokenUsage {
	if usage == nil {
		return sigil.TokenUsage{}
	}

	totalTokens := int64(usage.TotalTokenCount)
	toolUsePromptTokens := int64(usage.ToolUsePromptTokenCount)
	reasoningTokens := int64(usage.ThoughtsTokenCount)
	if totalTokens == 0 {
		totalTokens = int64(usage.PromptTokenCount) + int64(usage.CandidatesTokenCount) + toolUsePromptTokens + reasoningTokens
	}

	return sigil.TokenUsage{
		InputTokens:          int64(usage.PromptTokenCount),
		OutputTokens:         int64(usage.CandidatesTokenCount),
		TotalTokens:          totalTokens,
		CacheReadInputTokens: int64(usage.CachedContentTokenCount),
		ReasoningTokens:      reasoningTokens,
	}
}

func mapRole(role string) sigil.Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "model", "assistant":
		return sigil.RoleAssistant
	case "tool":
		return sigil.RoleTool
	default:
		return sigil.RoleUser
	}
}

func extractSystemPrompt(config *genai.GenerateContentConfig) string {
	if config == nil || config.SystemInstruction == nil {
		return ""
	}
	parts := make([]string, 0, len(config.SystemInstruction.Parts))
	for _, part := range config.SystemInstruction.Parts {
		if part == nil {
			continue
		}
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n\n")
}

func hasFunctionTools(config *genai.GenerateContentConfig) bool {
	if config == nil {
		return false
	}
	for _, tool := range config.Tools {
		if tool != nil && len(tool.FunctionDeclarations) > 0 {
			return true
		}
	}
	return false
}

func mapRequestControls(config *genai.GenerateContentConfig) (*int64, *float64, *float64, *string, *bool, *int64) {
	if config == nil {
		return nil, nil, nil, nil, nil, nil
	}

	var maxTokens *int64
	if config.MaxOutputTokens > 0 {
		value := int64(config.MaxOutputTokens)
		maxTokens = &value
	}

	var temperature *float64
	if config.Temperature != nil {
		value := float64(*config.Temperature)
		temperature = &value
	}

	var topP *float64
	if config.TopP != nil {
		value := float64(*config.TopP)
		topP = &value
	}

	var toolChoice *string
	if config.ToolConfig != nil && config.ToolConfig.FunctionCallingConfig != nil {
		mode := strings.ToLower(strings.TrimSpace(string(config.ToolConfig.FunctionCallingConfig.Mode)))
		if mode != "" && mode != "mode_unspecified" {
			toolChoice = &mode
		}
	}

	var thinkingEnabled *bool
	var thinkingBudget *int64
	if config.ThinkingConfig != nil {
		value := config.ThinkingConfig.IncludeThoughts
		thinkingEnabled = &value
		if config.ThinkingConfig.ThinkingBudget != nil {
			budget := int64(*config.ThinkingConfig.ThinkingBudget)
			thinkingBudget = &budget
		}
	}

	return maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget
}

func marshalAny(value any) []byte {
	if value == nil {
		return nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}

	out := make(map[string]any, len(in))
	maps.Copy(out, in)
	return out
}

func mergeThinkingBudgetMetadata(metadata map[string]any, thinkingBudget *int64) map[string]any {
	out := cloneAnyMap(metadata)
	if thinkingBudget == nil {
		return out
	}
	if out == nil {
		out = map[string]any{}
	}
	out[thinkingBudgetMetadataKey] = *thinkingBudget
	return out
}

func mergeThinkingLevelMetadata(metadata map[string]any, thinkingLevel *string) map[string]any {
	out := cloneAnyMap(metadata)
	if thinkingLevel == nil || strings.TrimSpace(*thinkingLevel) == "" {
		return out
	}
	if out == nil {
		out = map[string]any{}
	}
	out[thinkingLevelMetadataKey] = *thinkingLevel
	return out
}

func mergeGeminiUsageMetadata(metadata map[string]any, usage *genai.GenerateContentResponseUsageMetadata) map[string]any {
	out := cloneAnyMap(metadata)
	if usage == nil || usage.ToolUsePromptTokenCount <= 0 {
		return out
	}
	if out == nil {
		out = map[string]any{}
	}
	out[usageToolUsePromptTokensMetadataKey] = int64(usage.ToolUsePromptTokenCount)
	return out
}

func extractThinkingLevel(config *genai.GenerateContentConfig) *string {
	if config == nil || config.ThinkingConfig == nil {
		return nil
	}
	normalized := strings.TrimSpace(strings.ToLower(string(config.ThinkingConfig.ThinkingLevel)))
	switch normalized {
	case "", "thinking_level_unspecified":
		return nil
	case "thinking_level_low":
		value := "low"
		return &value
	case "thinking_level_medium":
		value := "medium"
		return &value
	case "thinking_level_high":
		value := "high"
		return &value
	default:
		return &normalized
	}
}
