package openai

import (
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"

	osdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/shared"

	"github.com/grafana/agento11y/go/agento11y"
)

const thinkingBudgetMetadataKey = "agento11y.gen_ai.request.thinking.budget_tokens"

// ChatCompletionsFromRequestResponse maps an OpenAI chat-completions request/response pair to agento11y.Generation.
func ChatCompletionsFromRequestResponse(req osdk.ChatCompletionNewParams, resp *osdk.ChatCompletion, opts ...Option) (agento11y.Generation, error) {
	if resp == nil {
		return agento11y.Generation{}, errors.New("response is required")
	}

	options := applyOptions(opts)
	input, systemPrompt := mapRequestMessages(req.Messages)
	output := mapResponseMessages(resp.Choices)

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "openai.chat.request", req)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeResponseArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindResponse, "openai.chat.response", resp)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(req.Tools) > 0 {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "openai.chat.tools", req.Tools)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	requestModel := req.Model
	responseModel := resp.Model
	if responseModel == "" {
		responseModel = requestModel
	}
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapRequestControls(req)

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: requestModel},
		ResponseID:        resp.ID,
		ResponseModel:     responseModel,
		SystemPrompt:      systemPrompt,
		Input:             input,
		Output:            output,
		Tools:             mapTools(req.Tools),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             mapUsage(resp.Usage),
		StopReason:        firstFinishReason(resp.Choices),
		Tags:              cloneStringMap(options.tags),
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, thinkingBudget),
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

// EmbeddingsFromResponse maps an OpenAI embeddings request/response pair to agento11y.EmbeddingResult.
func EmbeddingsFromResponse(req osdk.EmbeddingNewParams, resp *osdk.CreateEmbeddingResponse) agento11y.EmbeddingResult {
	result := agento11y.EmbeddingResult{
		InputCount: embeddingInputCount(req.Input),
		InputTexts: embeddingInputTexts(req.Input),
	}

	if resp == nil {
		return result
	}

	result.InputTokens = resp.Usage.PromptTokens
	result.ResponseModel = resp.Model

	if len(resp.Data) > 0 {
		dimensions := int64(len(resp.Data[0].Embedding))
		if dimensions > 0 {
			result.Dimensions = &dimensions
		}
	}

	return result
}

func mapRequestMessages(messages []osdk.ChatCompletionMessageParamUnion) ([]agento11y.Message, string) {
	if len(messages) == 0 {
		return nil, ""
	}

	out := make([]agento11y.Message, 0, len(messages))
	systemPrompts := make([]string, 0, 2)

	for i := range messages {
		switch {
		case messages[i].OfSystem != nil:
			systemPrompts = append(systemPrompts, extractTextFromSystem(messages[i].OfSystem))
		case messages[i].OfDeveloper != nil:
			systemPrompts = append(systemPrompts, extractTextFromDeveloper(messages[i].OfDeveloper))
		case messages[i].OfUser != nil:
			parts := mapUserParts(messages[i].OfUser)
			if len(parts) > 0 {
				out = append(out, agento11y.Message{Role: agento11y.RoleUser, Parts: parts})
			}
		case messages[i].OfAssistant != nil:
			parts := mapAssistantParamParts(messages[i].OfAssistant)
			if len(parts) > 0 {
				out = append(out, agento11y.Message{Role: agento11y.RoleAssistant, Parts: parts})
			}
		case messages[i].OfTool != nil:
			part := mapToolMessage(messages[i].OfTool)
			if part != nil {
				out = append(out, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{*part}})
			}
		case messages[i].OfFunction != nil:
			part := mapFunctionMessage(messages[i].OfFunction)
			if part != nil {
				out = append(out, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{*part}})
			}
		}
	}

	return out, strings.Join(systemPrompts, "\n\n")
}

func mapResponseMessages(choices []osdk.ChatCompletionChoice) []agento11y.Message {
	if len(choices) == 0 {
		return nil
	}

	message := choices[0].Message
	parts := make([]agento11y.Part, 0, 1+len(message.ToolCalls))

	if text := message.Content; text != "" {
		parts = append(parts, agento11y.TextPart(text))
	}
	if refusal := message.Refusal; refusal != "" {
		parts = append(parts, agento11y.TextPart(refusal))
	}
	for _, call := range message.ToolCalls {
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			InputJSON: parseJSONOrString(call.Function.Arguments),
		})
		part.Metadata.ProviderType = "tool_call"
		parts = append(parts, part)
	}

	if len(parts) == 0 {
		return nil
	}

	return []agento11y.Message{
		{
			Role:  agento11y.RoleAssistant,
			Parts: parts,
		},
	}
}

func mapUserParts(message *osdk.ChatCompletionUserMessageParam) []agento11y.Part {
	parts := make([]agento11y.Part, 0, 2)
	if message.Content.OfString.Valid() {
		if text := message.Content.OfString.Value; text != "" {
			parts = append(parts, agento11y.TextPart(text))
		}
	}
	for _, contentPart := range message.Content.OfArrayOfContentParts {
		text := derefString(contentPart.GetText())
		if text != "" {
			parts = append(parts, agento11y.TextPart(text))
		}
	}
	return parts
}

func mapAssistantParamParts(message *osdk.ChatCompletionAssistantMessageParam) []agento11y.Part {
	parts := make([]agento11y.Part, 0, 2+len(message.ToolCalls))
	if message.Content.OfString.Valid() {
		if text := message.Content.OfString.Value; text != "" {
			parts = append(parts, agento11y.TextPart(text))
		}
	}
	for _, contentPart := range message.Content.OfArrayOfContentParts {
		if text := derefString(contentPart.GetText()); text != "" {
			parts = append(parts, agento11y.TextPart(text))
		}
		if refusal := derefString(contentPart.GetRefusal()); refusal != "" {
			parts = append(parts, agento11y.TextPart(refusal))
		}
	}
	if message.Refusal.Valid() {
		if refusal := message.Refusal.Value; refusal != "" {
			parts = append(parts, agento11y.TextPart(refusal))
		}
	}
	for _, call := range message.ToolCalls {
		function := call.GetFunction()
		if function == nil {
			continue
		}
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        derefString(call.GetID()),
			Name:      function.Name,
			InputJSON: parseJSONOrString(function.Arguments),
		})
		part.Metadata.ProviderType = "tool_call"
		parts = append(parts, part)
	}
	return parts
}

func mapToolMessage(message *osdk.ChatCompletionToolMessageParam) *agento11y.Part {
	content := ""
	if message.Content.OfString.Valid() {
		content = message.Content.OfString.Value
	} else {
		chunks := make([]string, 0, len(message.Content.OfArrayOfContentParts))
		for _, part := range message.Content.OfArrayOfContentParts {
			chunks = append(chunks, part.Text)
		}
		content = strings.Join(chunks, "\n")
	}
	if content == "" {
		return nil
	}

	part := agento11y.ToolResultPart(agento11y.ToolResult{
		ToolCallID: message.ToolCallID,
		Content:    content,
	})
	part.Metadata.ProviderType = "tool_result"
	return &part
}

//nolint:staticcheck // OpenAI API still exposes this deprecated message type in union payloads.
func mapFunctionMessage(message *osdk.ChatCompletionFunctionMessageParam) *agento11y.Part {
	if !message.Content.Valid() {
		return nil
	}
	content := message.Content.Value
	if content == "" {
		return nil
	}

	part := agento11y.ToolResultPart(agento11y.ToolResult{
		Name:        message.Name,
		Content:     content,
		ContentJSON: parseJSONOrString(content),
	})
	part.Metadata.ProviderType = "function_result"
	return &part
}

func mapTools(tools []osdk.ChatCompletionToolUnionParam) []agento11y.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}

	out := make([]agento11y.ToolDefinition, 0, len(tools))
	for i := range tools {
		function := tools[i].GetFunction()
		if function == nil {
			continue
		}
		name := function.Name
		if strings.TrimSpace(name) == "" {
			continue
		}

		definition := agento11y.ToolDefinition{
			Name: name,
		}
		if toolType := tools[i].GetType(); toolType != nil {
			definition.Type = *toolType
		}
		if function.Description.Valid() {
			definition.Description = function.Description.Value
		}
		if schema := marshalFunctionSchema(*function); len(schema) > 0 {
			definition.InputSchema = schema
		}
		out = append(out, definition)
	}

	return out
}

func marshalFunctionSchema(function shared.FunctionDefinitionParam) []byte {
	if function.Parameters == nil {
		return nil
	}
	data, err := json.Marshal(function.Parameters)
	if err != nil {
		return nil
	}
	return data
}

func mapUsage(usage osdk.CompletionUsage) agento11y.TokenUsage {
	return agento11y.TokenUsage{
		InputTokens:          usage.PromptTokens,
		OutputTokens:         usage.CompletionTokens,
		TotalTokens:          usage.TotalTokens,
		CacheReadInputTokens: usage.PromptTokensDetails.CachedTokens,
		ReasoningTokens:      usage.CompletionTokensDetails.ReasoningTokens,
	}
}

func firstFinishReason(choices []osdk.ChatCompletionChoice) string {
	for i := range choices {
		if choices[i].FinishReason != "" {
			return choices[i].FinishReason
		}
	}
	return ""
}

func mapRequestControls(req osdk.ChatCompletionNewParams) (*int64, *float64, *float64, *string, *bool, *int64) {
	payload := marshalRequest(req)
	if payload == nil {
		return nil, nil, nil, nil, nil, nil
	}

	maxTokens := readInt64(payload, "max_completion_tokens")
	if maxTokens == nil {
		maxTokens = readInt64(payload, "max_tokens")
	}

	temperature := readFloat64(payload, "temperature")
	topP := readFloat64(payload, "top_p")
	toolChoice := canonicalToolChoice(payload["tool_choice"])

	var thinkingEnabled *bool
	if _, ok := payload["reasoning"]; ok {
		thinkingEnabled = boolPtr(true)
	} else if _, ok := payload["reasoning_effort"]; ok {
		thinkingEnabled = boolPtr(true)
	}

	thinkingBudget := resolveThinkingBudget(payload["reasoning"])

	return maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget
}

func marshalRequest(req osdk.ChatCompletionNewParams) map[string]any {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}

	return payload
}

func readInt64(payload map[string]any, key string) *int64 {
	value, ok := payload[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		asInt := int64(typed)
		if typed != float64(asInt) {
			return nil
		}
		return &asInt
	}
	return nil
}

func readFloat64(payload map[string]any, key string) *float64 {
	value, ok := payload[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case float64:
		return &typed
	}
	return nil
}

func canonicalToolChoice(value any) *string {
	if value == nil {
		return nil
	}

	if text, ok := value.(string); ok {
		normalized := strings.ToLower(strings.TrimSpace(text))
		if normalized == "" {
			return nil
		}
		return &normalized
	}

	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 {
		return nil
	}
	normalized := string(raw)
	return &normalized
}

func boolPtr(value bool) *bool {
	return &value
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

func resolveThinkingBudget(value any) *int64 {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return nil
	}
	if resolved := coerceInt64Pointer(object["budget_tokens"]); resolved != nil {
		return resolved
	}
	if resolved := coerceInt64Pointer(object["thinking_budget"]); resolved != nil {
		return resolved
	}
	if resolved := coerceInt64Pointer(object["thinkingBudget"]); resolved != nil {
		return resolved
	}
	if resolved := coerceInt64Pointer(object["max_output_tokens"]); resolved != nil {
		return resolved
	}
	return nil
}

func coerceInt64Pointer(value any) *int64 {
	switch typed := value.(type) {
	case float64:
		asInt := int64(typed)
		if typed != float64(asInt) {
			return nil
		}
		return &asInt
	case int:
		asInt := int64(typed)
		return &asInt
	case int64:
		asInt := typed
		return &asInt
	case json.Number:
		asInt, err := typed.Int64()
		if err != nil {
			return nil
		}
		return &asInt
	case string:
		asInt, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		if err != nil {
			return nil
		}
		return &asInt
	default:
		return nil
	}
}

func extractTextFromSystem(message *osdk.ChatCompletionSystemMessageParam) string {
	if message.Content.OfString.Valid() {
		return message.Content.OfString.Value
	}
	parts := make([]string, 0, len(message.Content.OfArrayOfContentParts))
	for _, part := range message.Content.OfArrayOfContentParts {
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n")
}

func extractTextFromDeveloper(message *osdk.ChatCompletionDeveloperMessageParam) string {
	if message.Content.OfString.Valid() {
		return message.Content.OfString.Value
	}
	parts := make([]string, 0, len(message.Content.OfArrayOfContentParts))
	for _, part := range message.Content.OfArrayOfContentParts {
		parts = append(parts, part.Text)
	}
	return strings.Join(parts, "\n")
}

func parseJSONOrString(value string) []byte {
	if value == "" {
		return nil
	}
	data := []byte(value)
	if json.Valid(data) {
		return data
	}
	quoted, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return quoted
}

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func embeddingInputCount(input osdk.EmbeddingNewParamsInputUnion) int {
	switch {
	case input.OfString.Valid():
		return 1
	case len(input.OfArrayOfStrings) > 0:
		return len(input.OfArrayOfStrings)
	case len(input.OfArrayOfTokenArrays) > 0:
		return len(input.OfArrayOfTokenArrays)
	case len(input.OfArrayOfTokens) > 0:
		return 1
	default:
		return 0
	}
}

func embeddingInputTexts(input osdk.EmbeddingNewParamsInputUnion) []string {
	switch {
	case input.OfString.Valid():
		return []string{input.OfString.Value}
	case len(input.OfArrayOfStrings) > 0:
		return cloneStrings(input.OfArrayOfStrings)
	default:
		return nil
	}
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, len(values))
	copy(out, values)
	return out
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
