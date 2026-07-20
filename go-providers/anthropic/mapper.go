package anthropic

import (
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"

	asdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/grafana/agento11y/go/agento11y"
)

const thinkingBudgetMetadataKey = "agento11y.gen_ai.request.thinking.budget_tokens"
const usageServerToolUseWebSearchMetadataKey = "agento11y.gen_ai.usage.server_tool_use.web_search_requests"
const usageServerToolUseWebFetchMetadataKey = "agento11y.gen_ai.usage.server_tool_use.web_fetch_requests"
const usageServerToolUseTotalMetadataKey = "agento11y.gen_ai.usage.server_tool_use.total_requests"
const toolSearchRegexToolUseType = "tool_search_tool_regex"
const toolSearchBM25ToolUseType = "tool_search_tool_bm25"
const toolSearchRegexToolResultType = "tool_search_tool_regex_tool_result"
const toolSearchBM25ToolResultType = "tool_search_tool_bm25_tool_result"

// FromRequestResponse maps an Anthropic request/response pair to agento11y.Generation.
func FromRequestResponse(req asdk.BetaMessageNewParams, resp *asdk.BetaMessage, opts ...Option) (agento11y.Generation, error) {
	if resp == nil {
		return agento11y.Generation{}, errors.New("response is required")
	}

	options := applyOptions(opts)

	input := mapRequestMessages(req.Messages)
	output := mapResponseMessages(resp.Content)

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "anthropic.request", req)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeResponseArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindResponse, "anthropic.response", resp)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(req.Tools) > 0 {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "anthropic.tools", req.Tools)
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
	metadata := mergeThinkingBudgetMetadata(options.metadata, thinkingBudget)
	metadata = mergeServerToolUsageMetadata(metadata, resp.Usage.ServerToolUse)

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: requestModel},
		ResponseID:        resp.ID,
		ResponseModel:     responseModel,
		SystemPrompt:      mapSystemPrompt(req.System),
		Input:             input,
		Output:            output,
		Tools:             mapTools(req.Tools),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             mapUsage(resp.Usage),
		StopReason:        string(resp.StopReason),
		Tags:              cloneStringMap(options.tags),
		Metadata:          metadata,
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

func mapRequestMessages(messages []asdk.BetaMessageParam) []agento11y.Message {
	if len(messages) == 0 {
		return nil
	}

	out := make([]agento11y.Message, 0, len(messages))
	for i := range messages {
		role := mapRequestRole(messages[i].Role)
		normalParts := make([]agento11y.Part, 0, len(messages[i].Content))
		toolParts := make([]agento11y.Part, 0, 1)

		for _, block := range messages[i].Content {
			part, ok := mapRequestBlock(block)
			if !ok {
				continue
			}
			if part.Kind == agento11y.PartKindToolResult {
				toolParts = append(toolParts, part)
				continue
			}
			normalParts = append(normalParts, part)
		}

		if len(normalParts) > 0 {
			out = append(out, agento11y.Message{
				Role:  role,
				Parts: normalParts,
			})
		}
		if len(toolParts) > 0 {
			out = append(out, agento11y.Message{
				Role:  agento11y.RoleTool,
				Parts: toolParts,
			})
		}
	}

	return out
}

func mapResponseMessages(content []asdk.BetaContentBlockUnion) []agento11y.Message {
	if len(content) == 0 {
		return nil
	}

	assistantParts := make([]agento11y.Part, 0, len(content))
	toolParts := make([]agento11y.Part, 0, 1)

	for _, block := range content {
		part, ok := mapResponseBlock(block)
		if !ok {
			continue
		}
		if part.Kind == agento11y.PartKindToolResult {
			toolParts = append(toolParts, part)
			continue
		}
		assistantParts = append(assistantParts, part)
	}

	out := make([]agento11y.Message, 0, 2)
	if len(assistantParts) > 0 {
		out = append(out, agento11y.Message{
			Role:  agento11y.RoleAssistant,
			Parts: assistantParts,
		})
	}
	if len(toolParts) > 0 {
		out = append(out, agento11y.Message{
			Role:  agento11y.RoleTool,
			Parts: toolParts,
		})
	}

	return out
}

// thinkingPart builds a thinking Part, skipping blocks with empty content.
// Adaptive thinking (e.g. Claude Sonnet 5) can emit signature-only thinking
// blocks with no text; a Part without a payload fails generation validation,
// so empty blocks are dropped like empty text blocks.
func thinkingPart(content, providerType string) (agento11y.Part, bool) {
	if content == "" {
		return agento11y.Part{}, false
	}
	part := agento11y.ThinkingPart(content)
	part.Metadata.ProviderType = providerType
	return part, true
}

func mapRequestBlock(block asdk.BetaContentBlockParamUnion) (agento11y.Part, bool) {
	if block.OfText != nil {
		text := block.OfText.Text
		if text == "" {
			return agento11y.Part{}, false
		}
		return agento11y.TextPart(text), true
	}
	if block.OfThinking != nil {
		return thinkingPart(block.OfThinking.Thinking, "thinking")
	}
	if block.OfRedactedThinking != nil {
		return thinkingPart(block.OfRedactedThinking.Data, "redacted_thinking")
	}
	if block.OfImage != nil {
		return imageBlockPart(block.OfImage)
	}
	if block.OfToolUse != nil {
		inputJSON, _ := marshalAny(block.OfToolUse.Input)
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        block.OfToolUse.ID,
			Name:      block.OfToolUse.Name,
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = "tool_use"
		return part, true
	}
	if block.OfServerToolUse != nil {
		inputJSON, _ := marshalAny(block.OfServerToolUse.Input)
		providerType := providerTypeForToolUse("server_tool_use", string(block.OfServerToolUse.Name))
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        block.OfServerToolUse.ID,
			Name:      string(block.OfServerToolUse.Name),
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = providerType
		return part, true
	}
	if block.OfMCPToolUse != nil {
		inputJSON, _ := marshalAny(block.OfMCPToolUse.Input)
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        block.OfMCPToolUse.ID,
			Name:      block.OfMCPToolUse.Name,
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = "mcp_tool_use"
		return part, true
	}
	if block.OfToolResult != nil {
		contentJSON, _ := marshalAny(block.OfToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfToolResult.ToolUseID,
			IsError:     block.OfToolResult.IsError.Value,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "tool_result"
		return part, true
	}
	if block.OfWebSearchToolResult != nil {
		contentJSON, _ := marshalAny(block.OfWebSearchToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfWebSearchToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "web_search_tool_result"
		return part, true
	}
	if block.OfWebFetchToolResult != nil {
		contentJSON, _ := marshalAny(block.OfWebFetchToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfWebFetchToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "web_fetch_tool_result"
		return part, true
	}
	if block.OfCodeExecutionToolResult != nil {
		contentJSON, _ := marshalAny(block.OfCodeExecutionToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfCodeExecutionToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "code_execution_tool_result"
		return part, true
	}
	if block.OfBashCodeExecutionToolResult != nil {
		contentJSON, _ := marshalAny(block.OfBashCodeExecutionToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfBashCodeExecutionToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "bash_code_execution_tool_result"
		return part, true
	}
	if block.OfTextEditorCodeExecutionToolResult != nil {
		contentJSON, _ := marshalAny(block.OfTextEditorCodeExecutionToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfTextEditorCodeExecutionToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "text_editor_code_execution_tool_result"
		return part, true
	}
	if block.OfToolSearchToolResult != nil {
		contentJSON, _ := marshalAny(block.OfToolSearchToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfToolSearchToolResult.ToolUseID,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "tool_search_tool_result"
		return part, true
	}
	if block.OfMCPToolResult != nil {
		contentJSON, _ := marshalAny(block.OfMCPToolResult.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.OfMCPToolResult.ToolUseID,
			IsError:     block.OfMCPToolResult.IsError.Valid() && block.OfMCPToolResult.IsError.Value,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = "mcp_tool_result"
		return part, true
	}

	typ := derefString(block.GetType())
	switch typ {
	case "text":
		text := derefString(block.GetText())
		if text == "" {
			return agento11y.Part{}, false
		}
		return agento11y.TextPart(text), true
	case "thinking":
		return thinkingPart(derefString(block.GetThinking()), typ)
	case "redacted_thinking":
		return thinkingPart(derefString(block.GetData()), typ)
	case "image":
		return imageBlockPart(block.OfImage)
	case "tool_use", "server_tool_use", "mcp_tool_use":
		inputJSON, _ := marshalAny(derefAny(block.GetInput()))
		providerType := providerTypeForToolUse(typ, derefString(block.GetName()))
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        derefString(block.GetID()),
			Name:      derefString(block.GetName()),
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = providerType
		return part, true
	case "tool_result",
		"web_search_tool_result",
		"web_fetch_tool_result",
		"code_execution_tool_result",
		"bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result",
		"tool_search_tool_result",
		toolSearchRegexToolResultType,
		toolSearchBM25ToolResultType,
		"mcp_tool_result":
		contentJSON, _ := marshalAny(block)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  derefString(block.GetToolUseID()),
			IsError:     derefBool(block.GetIsError()),
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = typ
		return part, true
	default:
		return agento11y.Part{}, false
	}
}

func imageBlockPart(block *asdk.BetaImageBlockParam) (agento11y.Part, bool) {
	if block == nil {
		return agento11y.Part{}, false
	}
	return imagePartFromSource(
		derefString(block.Source.GetData()),
		derefString(block.Source.GetMediaType()),
		derefString(block.Source.GetURL()),
	)
}

func imagePartFromSource(base64Data, mediaType, sourceURL string) (agento11y.Part, bool) {
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	url := strings.TrimSpace(sourceURL)
	if mediaType == "" {
		mediaType = mediaTypeFromDataURL(url)
	}
	if url == "" {
		data := strings.TrimSpace(base64Data)
		if mediaType == "" || data == "" {
			return agento11y.Part{}, false
		}
		url = "data:" + mediaType + ";base64," + data
	}

	part := agento11y.MediaPart(agento11y.Media{
		Kind:     "image",
		URL:      url,
		MIMEType: mediaType,
	})
	part.Metadata.ProviderType = "image"
	return part, true
}

func mediaTypeFromDataURL(value string) string {
	trimmed := strings.TrimSpace(value)
	if !strings.HasPrefix(strings.ToLower(trimmed), "data:") {
		return ""
	}

	payload := trimmed[len("data:"):]
	if before, _, ok := strings.Cut(payload, ";"); ok {
		return strings.ToLower(strings.TrimSpace(before))
	}
	if before, _, ok := strings.Cut(payload, ","); ok {
		return strings.ToLower(strings.TrimSpace(before))
	}
	return ""
}

func mapResponseBlock(block asdk.BetaContentBlockUnion) (agento11y.Part, bool) {
	switch block.Type {
	case "text":
		text := block.Text
		if text == "" {
			return agento11y.Part{}, false
		}
		return agento11y.TextPart(text), true
	case "thinking":
		return thinkingPart(block.Thinking, block.Type)
	case "redacted_thinking":
		return thinkingPart(block.Data, block.Type)
	case "tool_use", "server_tool_use", "mcp_tool_use":
		inputJSON, _ := marshalAny(block.Input)
		providerType := providerTypeForToolUse(block.Type, block.Name)
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        block.ID,
			Name:      block.Name,
			InputJSON: inputJSON,
		})
		part.Metadata.ProviderType = providerType
		return part, true
	case "tool_result",
		"web_search_tool_result",
		"web_fetch_tool_result",
		"code_execution_tool_result",
		"bash_code_execution_tool_result",
		"text_editor_code_execution_tool_result",
		"tool_search_tool_result",
		toolSearchRegexToolResultType,
		toolSearchBM25ToolResultType,
		"mcp_tool_result":
		contentJSON, _ := marshalAny(block.Content)
		part := agento11y.ToolResultPart(agento11y.ToolResult{
			ToolCallID:  block.ToolUseID,
			IsError:     block.IsError,
			ContentJSON: contentJSON,
		})
		part.Metadata.ProviderType = block.Type
		return part, true
	default:
		return agento11y.Part{}, false
	}
}

func mapTools(tools []asdk.BetaToolUnionParam) []agento11y.ToolDefinition {
	if len(tools) == 0 {
		return nil
	}

	out := make([]agento11y.ToolDefinition, 0, len(tools))
	for i := range tools {
		name := derefString(tools[i].GetName())
		if strings.TrimSpace(name) == "" {
			continue
		}

		definition := agento11y.ToolDefinition{
			Name:        name,
			Description: derefString(tools[i].GetDescription()),
			Type:        derefString(tools[i].GetType()),
		}
		if deferred := tools[i].GetDeferLoading(); deferred != nil {
			definition.Deferred = *deferred
		}

		if schema := tools[i].GetInputSchema(); schema != nil {
			raw, err := marshalAny(*schema)
			if err == nil {
				definition.InputSchema = raw
			}
		}

		out = append(out, definition)
	}

	return out
}

func providerTypeForToolUse(blockType, toolName string) string {
	if blockType != "server_tool_use" {
		return blockType
	}
	switch toolName {
	case toolSearchRegexToolUseType, toolSearchBM25ToolUseType:
		return toolName
	default:
		return blockType
	}
}

func mapSystemPrompt(system []asdk.BetaTextBlockParam) string {
	if len(system) == 0 {
		return ""
	}

	parts := make([]string, 0, len(system))
	for i := range system {
		parts = append(parts, system[i].Text)
	}

	return strings.Join(parts, "\n\n")
}

func mapUsage(usage asdk.BetaUsage) agento11y.TokenUsage {
	return agento11y.TokenUsage{
		InputTokens:           usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		TotalTokens:           usage.InputTokens + usage.OutputTokens,
		CacheReadInputTokens:  usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheCreationInputTokens,
	}
}

func mapDeltaUsage(usage asdk.BetaMessageDeltaUsage) agento11y.TokenUsage {
	return agento11y.TokenUsage{
		InputTokens:           usage.InputTokens,
		OutputTokens:          usage.OutputTokens,
		TotalTokens:           usage.InputTokens + usage.OutputTokens,
		CacheReadInputTokens:  usage.CacheReadInputTokens,
		CacheWriteInputTokens: usage.CacheCreationInputTokens,
	}
}

func mapRequestRole(role asdk.BetaMessageParamRole) agento11y.Role {
	if role == asdk.BetaMessageParamRoleAssistant {
		return agento11y.RoleAssistant
	}
	return agento11y.RoleUser
}

func mapRequestControls(req asdk.BetaMessageNewParams) (*int64, *float64, *float64, *string, *bool, *int64) {
	payload := marshalRequest(req)
	if payload == nil {
		return nil, nil, nil, nil, nil, nil
	}

	maxTokens := readInt64(payload, "max_tokens")
	temperature := readFloat64(payload, "temperature")
	topP := readFloat64(payload, "top_p")
	toolChoice := canonicalToolChoice(payload["tool_choice"])

	var thinkingEnabled *bool
	var thinkingBudget *int64
	if thinkingValue, ok := payload["thinking"]; ok {
		if resolved, ok := resolveThinkingEnabled(thinkingValue); ok {
			thinkingEnabled = &resolved
		}
		thinkingBudget = resolveThinkingBudget(thinkingValue)
	}

	return maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget
}

func marshalRequest(req asdk.BetaMessageNewParams) map[string]any {
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

func resolveThinkingEnabled(value any) (bool, bool) {
	if value == nil {
		return false, false
	}

	switch typed := value.(type) {
	case string:
		return resolveThinkingType(typed)
	case map[string]any:
		if text, ok := typed["type"].(string); ok {
			return resolveThinkingType(text)
		}
	}

	return false, false
}

func resolveThinkingType(raw string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "enabled", "adaptive":
		return true, true
	case "disabled":
		return false, true
	default:
		return false, false
	}
}

func resolveThinkingBudget(value any) *int64 {
	object, ok := value.(map[string]any)
	if !ok || len(object) == 0 {
		return nil
	}
	return coerceInt64Pointer(object["budget_tokens"])
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

func derefString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func derefBool(value *bool) bool {
	if value == nil {
		return false
	}
	return *value
}

func derefAny(value *any) any {
	if value == nil {
		return nil
	}
	return *value
}

func marshalAny(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
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

func mergeServerToolUsageMetadata(metadata map[string]any, usage asdk.BetaServerToolUsage) map[string]any {
	out := cloneAnyMap(metadata)
	total := usage.WebSearchRequests + usage.WebFetchRequests
	if total == 0 {
		return out
	}
	if out == nil {
		out = map[string]any{}
	}
	if usage.WebSearchRequests > 0 {
		out[usageServerToolUseWebSearchMetadataKey] = usage.WebSearchRequests
	}
	if usage.WebFetchRequests > 0 {
		out[usageServerToolUseWebFetchMetadataKey] = usage.WebFetchRequests
	}
	out[usageServerToolUseTotalMetadataKey] = total
	return out
}
