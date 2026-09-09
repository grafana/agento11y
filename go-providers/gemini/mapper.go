package gemini

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strings"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y"
)

const thinkingBudgetMetadataKey = "agento11y.gen_ai.request.thinking.budget_tokens"
const thinkingLevelMetadataKey = "agento11y.gen_ai.request.thinking.level"
const usageToolUsePromptTokensMetadataKey = "agento11y.gen_ai.usage.tool_use_prompt_tokens"

// FromRequestResponse maps a Gemini request/response pair to agento11y.Generation.
func FromRequestResponse(
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	resp *genai.GenerateContentResponse,
	opts ...Option,
) (agento11y.Generation, error) {
	if resp == nil {
		return agento11y.Generation{}, errors.New("response is required")
	}
	if strings.TrimSpace(model) == "" {
		return agento11y.Generation{}, errors.New("request model is required")
	}

	options := applyOptions(opts)
	input := mapContents(contents)
	output, stopReason := mapCandidates(resp.Candidates)
	controls := mapRequestControls(config)
	thinkingLevel := extractThinkingLevel(config)

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		requestPayload := map[string]any{
			"model":    model,
			"contents": contents,
			"config":   config,
		}
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "gemini.generate_content.request", requestPayload)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeResponseArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindResponse, "gemini.generate_content.response", resp)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && hasFunctionTools(config) {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "gemini.generate_content.tools", config.Tools)
		if err != nil {
			return agento11y.Generation{}, err
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
	metadata = mergeThinkingBudgetMetadata(metadata, controls.thinkingBudget)
	metadata = mergeThinkingLevelMetadata(metadata, thinkingLevel)
	metadata = mergeGeminiUsageMetadata(metadata, resp.UsageMetadata)

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		OperationName:     generationOperation(contents, config, resp.Candidates),
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
		ResponseID:        resp.ResponseID,
		ResponseModel:     resp.ModelVersion,
		SystemPrompt:      extractSystemPrompt(config),
		Input:             input,
		Output:            output,
		Tools:             mapTools(config),
		MaxTokens:         controls.maxTokens,
		Temperature:       controls.temperature,
		TopP:              controls.topP,
		TopK:              controls.topK,
		ChoiceCount:       controls.choiceCount,
		Seed:              controls.seed,
		OutputType:        controls.outputType,
		ToolChoice:        controls.toolChoice,
		ThinkingEnabled:   controls.thinkingEnabled,
		Usage:             mapUsage(resp.UsageMetadata),
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          metadata,
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

// EmbeddingFromResponse maps a Gemini embed-content request/response pair to agento11y.EmbeddingResult.
func EmbeddingFromResponse(
	model string,
	contents []*genai.Content,
	config *genai.EmbedContentConfig,
	resp *genai.EmbedContentResponse,
) agento11y.EmbeddingResult {
	_ = model

	result := agento11y.EmbeddingResult{
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

func mapContents(contents []*genai.Content) []agento11y.Message {
	if len(contents) == 0 {
		return nil
	}

	out := make([]agento11y.Message, 0, len(contents)+1)
	messageStart := 0
	appendParts := func(role agento11y.Role, parts ...agento11y.Part) {
		if len(parts) == 0 {
			return
		}
		if len(out) > messageStart && out[len(out)-1].Role == role {
			out[len(out)-1].Parts = append(out[len(out)-1].Parts, parts...)
			return
		}
		out = append(out, agento11y.Message{Role: role, Parts: parts})
	}

	for _, content := range contents {
		if content == nil {
			continue
		}

		messageStart = len(out)
		role := mapRole(content.Role)
		for _, part := range content.Parts {
			if part == nil {
				continue
			}

			if media, ok := mapMediaPart(part); ok {
				appendParts(role, media)
			}
			if text := part.Text; text != "" {
				if part.Thought && role == agento11y.RoleAssistant {
					appendParts(role, agento11y.ThinkingPart(text))
				} else {
					appendParts(role, agento11y.TextPart(text))
				}
			}
			if part.FunctionCall != nil && strings.TrimSpace(part.FunctionCall.Name) != "" {
				call := agento11y.ToolCallPart(agento11y.ToolCall{
					ID:        part.FunctionCall.ID,
					Name:      part.FunctionCall.Name,
					InputJSON: marshalAny(part.FunctionCall.Args),
				})
				call.Metadata.ProviderType = "function_call"
				callRole := role
				if callRole != agento11y.RoleAssistant {
					callRole = agento11y.RoleAssistant
				}
				appendParts(callRole, call)
			}
			if part.FunctionResponse != nil {
				result := agento11y.ToolResultPart(agento11y.ToolResult{
					ToolCallID:  part.FunctionResponse.ID,
					Name:        part.FunctionResponse.Name,
					ContentJSON: marshalAny(part.FunctionResponse.Response),
				})
				result.Metadata.ProviderType = "function_response"
				toolParts := []agento11y.Part{result}
				for _, responsePart := range part.FunctionResponse.Parts {
					if media, ok := mapFunctionResponseMediaPart(responsePart); ok {
						toolParts = append(toolParts, media)
					}
				}
				appendParts(agento11y.RoleTool, toolParts...)
			}
		}
	}

	return out
}

func mapMediaPart(part *genai.Part) (agento11y.Part, bool) {
	if part == nil {
		return agento11y.Part{}, false
	}
	if part.InlineData != nil && len(part.InlineData.Data) > 0 {
		mimeType := strings.TrimSpace(part.InlineData.MIMEType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return agento11y.MediaPart(agento11y.Media{
			Kind:     mediaKind(mimeType),
			URL:      "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(part.InlineData.Data),
			MIMEType: mimeType,
			Name:     part.InlineData.DisplayName,
		}), true
	}
	if part.FileData != nil && strings.TrimSpace(part.FileData.FileURI) != "" {
		return agento11y.MediaPart(agento11y.Media{
			Kind:     mediaKind(part.FileData.MIMEType),
			URL:      part.FileData.FileURI,
			MIMEType: part.FileData.MIMEType,
			Name:     part.FileData.DisplayName,
		}), true
	}
	return agento11y.Part{}, false
}

func mapFunctionResponseMediaPart(part *genai.FunctionResponsePart) (agento11y.Part, bool) {
	if part == nil {
		return agento11y.Part{}, false
	}
	if part.InlineData != nil && len(part.InlineData.Data) > 0 {
		mimeType := strings.TrimSpace(part.InlineData.MIMEType)
		if mimeType == "" {
			mimeType = "application/octet-stream"
		}
		return agento11y.MediaPart(agento11y.Media{
			Kind:     mediaKind(mimeType),
			URL:      "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(part.InlineData.Data),
			MIMEType: mimeType,
			Name:     part.InlineData.DisplayName,
		}), true
	}
	if part.FileData != nil && strings.TrimSpace(part.FileData.FileURI) != "" {
		return agento11y.MediaPart(agento11y.Media{
			Kind:     mediaKind(part.FileData.MIMEType),
			URL:      part.FileData.FileURI,
			MIMEType: part.FileData.MIMEType,
			Name:     part.FileData.DisplayName,
		}), true
	}
	return agento11y.Part{}, false
}

func mediaKind(mimeType string) string {
	prefix, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mimeType)), "/")
	switch prefix {
	case "image", "audio", "video":
		return prefix
	default:
		return "file"
	}
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

func mapCandidates(candidates []*genai.Candidate) ([]agento11y.Message, string) {
	if len(candidates) == 0 {
		return nil, ""
	}

	out := make([]agento11y.Message, 0, len(candidates))
	stopReason := ""
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		finishReason := string(candidate.FinishReason)
		if stopReason == "" && finishReason != "" {
			stopReason = finishReason
		}
		parts := mapCandidateParts(candidate.Content)
		if len(parts) == 0 && finishReason == "" {
			continue
		}
		out = append(out, agento11y.Message{
			Role:         agento11y.RoleAssistant,
			Parts:        parts,
			FinishReason: finishReason,
		})
	}

	return out, stopReason
}

func mapCandidateParts(content *genai.Content) []agento11y.Part {
	messages := mapContents([]*genai.Content{content})
	var parts []agento11y.Part
	for _, message := range messages {
		parts = append(parts, message.Parts...)
	}
	return parts
}

func mapTools(config *genai.GenerateContentConfig) []agento11y.ToolDefinition {
	if config == nil || len(config.Tools) == 0 {
		return nil
	}

	out := make([]agento11y.ToolDefinition, 0, len(config.Tools))
	for _, tool := range config.Tools {
		if tool == nil {
			continue
		}
		for _, declaration := range tool.FunctionDeclarations {
			if declaration == nil || strings.TrimSpace(declaration.Name) == "" {
				continue
			}
			definition := agento11y.ToolDefinition{
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

func mapUsage(usage *genai.GenerateContentResponseUsageMetadata) agento11y.TokenUsage {
	if usage == nil {
		return agento11y.TokenUsage{}
	}

	totalTokens := int64(usage.TotalTokenCount)
	toolUsePromptTokens := int64(usage.ToolUsePromptTokenCount)
	reasoningTokens := int64(usage.ThoughtsTokenCount)
	if totalTokens == 0 {
		totalTokens = int64(usage.PromptTokenCount) + int64(usage.CandidatesTokenCount) + toolUsePromptTokens + reasoningTokens
	}

	// PromptTokenCount includes CachedContentTokenCount. CandidatesTokenCount
	// excludes ThoughtsTokenCount, which remains an output-token sub-bucket.
	// ToolUsePromptTokenCount is excluded from input tokens.
	return agento11y.TokenUsage{
		InputTokens:          int64(usage.PromptTokenCount),
		OutputTokens:         int64(usage.CandidatesTokenCount) + reasoningTokens,
		TotalTokens:          totalTokens,
		CacheReadInputTokens: int64(usage.CachedContentTokenCount),
		InputTokensReported:  true,
		OutputTokensReported: true,
		ReasoningTokens:      reasoningTokens,
		InputSemantics:       agento11y.TokenInputSemanticsInclusive,
	}
}

func mapRole(role string) agento11y.Role {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "model", "assistant":
		return agento11y.RoleAssistant
	case "tool":
		return agento11y.RoleTool
	default:
		return agento11y.RoleUser
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

type requestControls struct {
	maxTokens       *int64
	temperature     *float64
	topP            *float64
	topK            *int64
	choiceCount     *int64
	seed            *int64
	outputType      *string
	toolChoice      *string
	thinkingEnabled *bool
	thinkingBudget  *int64
}

func mapRequestControls(config *genai.GenerateContentConfig) requestControls {
	if config == nil {
		return requestControls{}
	}

	controls := requestControls{}
	if config.MaxOutputTokens > 0 {
		value := int64(config.MaxOutputTokens)
		controls.maxTokens = &value
	}

	if config.Temperature != nil {
		value := float64(*config.Temperature)
		controls.temperature = &value
	}

	if config.TopP != nil {
		value := float64(*config.TopP)
		controls.topP = &value
	}

	if config.TopK != nil {
		value := float64(*config.TopK)
		if !math.IsNaN(value) && !math.IsInf(value, 0) && value == math.Trunc(value) &&
			value >= math.MinInt64 && value <= math.MaxInt64 {
			integral := int64(value)
			controls.topK = &integral
		}
	}

	if config.CandidateCount > 0 {
		value := int64(config.CandidateCount)
		controls.choiceCount = &value
	}
	if config.Seed != nil {
		value := int64(*config.Seed)
		controls.seed = &value
	}
	controls.outputType = mapResponseMIMEType(config.ResponseMIMEType)

	if config.ToolConfig != nil && config.ToolConfig.FunctionCallingConfig != nil {
		mode := strings.ToLower(strings.TrimSpace(string(config.ToolConfig.FunctionCallingConfig.Mode)))
		if mode != "" && mode != "mode_unspecified" {
			controls.toolChoice = &mode
		}
	}

	if config.ThinkingConfig != nil {
		if config.ThinkingConfig.ThinkingBudget != nil {
			budget := int64(*config.ThinkingConfig.ThinkingBudget)
			controls.thinkingBudget = &budget
			enabled := budget != 0
			controls.thinkingEnabled = &enabled
		}
		// IncludeThoughts only controls whether thought parts are returned. A
		// thinking level requests model thinking even when those parts are hidden.
		if extractThinkingLevel(config) != nil {
			enabled := true
			controls.thinkingEnabled = &enabled
		}
	}

	return controls
}

func mapResponseMIMEType(mimeType string) *string {
	normalized := strings.ToLower(strings.TrimSpace(mimeType))
	switch normalized {
	case "":
		return nil
	case "application/json":
		normalized = "json"
	case "text/plain":
		normalized = "text"
	}
	return &normalized
}

func generationOperation(contents []*genai.Content, config *genai.GenerateContentConfig, candidates []*genai.Candidate) string {
	if contentsHaveNonTextModality(contents) ||
		(config != nil && (contentHasNonTextModality(config.SystemInstruction) || responseHasNonTextModality(config.ResponseModalities))) {
		return "generate_content"
	}
	for _, candidate := range candidates {
		if candidate != nil && contentHasNonTextModality(candidate.Content) {
			return "generate_content"
		}
	}
	return "chat"
}

func contentsHaveNonTextModality(contents []*genai.Content) bool {
	return slices.ContainsFunc(contents, contentHasNonTextModality)
}

func contentHasNonTextModality(content *genai.Content) bool {
	if content == nil {
		return false
	}
	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		if part.InlineData != nil || part.FileData != nil {
			return true
		}
		if part.FunctionResponse != nil {
			for _, responsePart := range part.FunctionResponse.Parts {
				if responsePart != nil && (responsePart.InlineData != nil || responsePart.FileData != nil) {
					return true
				}
			}
		}
	}
	return false
}

func responseHasNonTextModality(modalities []string) bool {
	for _, modality := range modalities {
		normalized := strings.ToLower(strings.TrimSpace(modality))
		if normalized != "" && normalized != "text" {
			return true
		}
	}
	return false
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
