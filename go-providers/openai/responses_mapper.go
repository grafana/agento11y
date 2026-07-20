package openai

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/openai/openai-go/v3/responses"

	"github.com/grafana/agento11y/go/agento11y"
)

// ResponsesStreamSummary captures Responses API stream events and an optional final response.
type ResponsesStreamSummary struct {
	Events        []responses.ResponseStreamEventUnion
	FinalResponse *responses.Response
	FirstChunkAt  time.Time
}

// ResponsesFromRequestResponse maps an OpenAI responses request/response pair to agento11y.Generation.
func ResponsesFromRequestResponse(req responses.ResponseNewParams, resp *responses.Response, opts ...Option) (agento11y.Generation, error) {
	if resp == nil {
		return agento11y.Generation{}, errors.New("response is required")
	}

	options := applyOptions(opts)
	requestPayload := marshalAny(req)
	input, systemPrompt := mapResponsesRequestInput(requestPayload)
	output := mapResponsesOutput(resp.Output)
	tools := mapResponsesTools(requestPayload["tools"])
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapResponsesRequestControls(requestPayload)

	requestModel := req.Model
	responseModel := resp.Model
	if responseModel == "" {
		responseModel = requestModel
	}

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "openai.responses.request", req)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeResponseArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindResponse, "openai.responses.response", resp)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(tools) > 0 {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "openai.responses.tools", tools)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

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
		Tools:             tools,
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             mapResponsesUsage(resp.Usage),
		StopReason:        normalizeResponsesStopReason(resp),
		Tags:              cloneStringMap(options.tags),
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, thinkingBudget),
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

// ResponsesFromStream maps OpenAI responses streaming output to agento11y.Generation.
func ResponsesFromStream(req responses.ResponseNewParams, summary ResponsesStreamSummary, opts ...Option) (agento11y.Generation, error) {
	if summary.FinalResponse != nil {
		generation, err := ResponsesFromRequestResponse(req, summary.FinalResponse, opts...)
		if err != nil {
			return agento11y.Generation{}, err
		}
		return appendResponsesStreamEventsArtifact(generation, summary.Events, opts)
	}

	if len(summary.Events) == 0 {
		return agento11y.Generation{}, errors.New("stream summary has no events and no final response")
	}

	requestPayload := marshalAny(req)
	input, systemPrompt := mapResponsesRequestInput(requestPayload)
	tools := mapResponsesTools(requestPayload["tools"])
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapResponsesRequestControls(requestPayload)
	options := applyOptions(opts)

	responseID := ""
	responseModel := req.Model
	usage := agento11y.TokenUsage{}
	stopReason := ""
	text := strings.Builder{}
	toolCalls := map[string]*responsesStreamToolCall{}
	toolCallOrder := []string{}

	for i := range summary.Events {
		event := summary.Events[i]
		eventType := event.Type

		if event.Response.ID != "" {
			responseID = event.Response.ID
			if model := event.Response.Model; model != "" {
				responseModel = model
			}
			usage = mapResponsesUsage(event.Response.Usage)
			if reason := normalizeResponsesStopReason(&event.Response); reason != "" {
				stopReason = reason
			}
		}

		switch eventType {
		case "response.output_text.delta", "response.refusal.delta":
			if event.Delta != "" {
				text.WriteString(event.Delta)
			}
		case "response.output_text.done":
			if text.Len() == 0 && event.Text != "" {
				text.WriteString(event.Text)
			}
		case "response.refusal.done":
			if text.Len() == 0 && event.Refusal != "" {
				text.WriteString(event.Refusal)
			}
		case "response.output_item.added", "response.output_item.done":
			if event.Item.Type != "function_call" {
				break
			}
			call := ensureResponsesStreamToolCall(toolCalls, &toolCallOrder, event.Item.ID, event.OutputIndex)
			if call.callID == "" {
				call.callID = strings.TrimSpace(event.Item.CallID)
			}
			if call.name == "" {
				call.name = strings.TrimSpace(event.Item.Name)
			}
			if arguments := stringifyResponsesOutputArguments(event.Item.Arguments); arguments != "" {
				call.arguments.Reset()
				call.arguments.WriteString(arguments)
			}
		case "response.function_call_arguments.delta":
			call := ensureResponsesStreamToolCall(toolCalls, &toolCallOrder, event.ItemID, event.OutputIndex)
			if event.Delta != "" {
				call.arguments.WriteString(event.Delta)
			}
		case "response.function_call_arguments.done":
			call := ensureResponsesStreamToolCall(toolCalls, &toolCallOrder, event.ItemID, event.OutputIndex)
			if name := strings.TrimSpace(event.Name); name != "" {
				call.name = name
			}
			if event.Arguments != "" {
				call.arguments.Reset()
				call.arguments.WriteString(event.Arguments)
			}
		case "response.completed":
			if stopReason == "" {
				stopReason = "stop"
			}
		case "response.incomplete":
			if stopReason == "" {
				reason := strings.TrimSpace(event.Response.IncompleteDetails.Reason)
				if reason != "" {
					stopReason = reason
				} else {
					stopReason = "incomplete"
				}
			}
		case "response.failed":
			if stopReason == "" {
				stopReason = "failed"
			}
		case "response.cancelled":
			if stopReason == "" {
				stopReason = "cancelled"
			}
		}
	}

	output := []agento11y.Message{}
	if generated := text.String(); generated != "" {
		output = append(output, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.TextPart(generated)}})
	}
	output = append(output, mapResponsesStreamToolCalls(toolCalls, toolCallOrder)...)

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "openai.responses.request", req)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeToolsArtifact && len(tools) > 0 {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "openai.responses.tools", tools)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeEventsArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "openai.responses.stream_events", summary.Events)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
		ResponseID:        responseID,
		ResponseModel:     responseModel,
		SystemPrompt:      systemPrompt,
		Input:             input,
		Output:            output,
		Tools:             tools,
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
		Usage:             usage,
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, thinkingBudget),
		Artifacts:         artifacts,
	}

	if err := generation.Validate(); err != nil {
		return agento11y.Generation{}, err
	}

	return generation, nil
}

type responsesStreamToolCall struct {
	itemID      string
	callID      string
	name        string
	outputIndex int64
	order       int
	arguments   strings.Builder
}

func ensureResponsesStreamToolCall(calls map[string]*responsesStreamToolCall, order *[]string, itemID string, outputIndex int64) *responsesStreamToolCall {
	key := strings.TrimSpace(itemID)
	if key == "" {
		key = fmt.Sprintf("output-%d", outputIndex)
	}
	if existing, ok := calls[key]; ok {
		if existing.outputIndex == 0 && outputIndex != 0 {
			existing.outputIndex = outputIndex
		}
		return existing
	}

	call := &responsesStreamToolCall{
		itemID:      key,
		outputIndex: outputIndex,
		order:       len(*order),
	}
	calls[key] = call
	*order = append(*order, key)
	return call
}

func mapResponsesStreamToolCalls(calls map[string]*responsesStreamToolCall, order []string) []agento11y.Message {
	if len(order) == 0 {
		return nil
	}

	ordered := make([]*responsesStreamToolCall, 0, len(order))
	for _, key := range order {
		call := calls[key]
		if call == nil || strings.TrimSpace(call.name) == "" {
			continue
		}
		ordered = append(ordered, call)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].outputIndex == ordered[j].outputIndex {
			return ordered[i].order < ordered[j].order
		}
		return ordered[i].outputIndex < ordered[j].outputIndex
	})

	out := make([]agento11y.Message, 0, len(ordered))
	for _, call := range ordered {
		callID := strings.TrimSpace(call.callID)
		if callID == "" {
			callID = call.itemID
		}
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        callID,
			Name:      call.name,
			InputJSON: parseJSONOrString(call.arguments.String()),
		})
		part.Metadata.ProviderType = "tool_call"
		out = append(out, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{part}})
	}
	return out
}

func appendResponsesStreamEventsArtifact(generation agento11y.Generation, events []responses.ResponseStreamEventUnion, opts []Option) (agento11y.Generation, error) {
	if len(events) == 0 {
		return generation, nil
	}

	options := applyOptions(opts)
	if !options.includeEventsArtifact {
		return generation, nil
	}

	artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "openai.responses.stream_events", events)
	if err != nil {
		return agento11y.Generation{}, err
	}
	generation.Artifacts = append(generation.Artifacts, artifact)
	return generation, nil
}

func mapResponsesRequestInput(payload map[string]any) ([]agento11y.Message, string) {
	input := make([]agento11y.Message, 0, 4)
	systemPrompts := make([]string, 0, 2)

	if instructions, ok := payload["instructions"].(string); ok {
		systemPrompts = append(systemPrompts, instructions)
	}

	rawInput, hasInput := payload["input"]
	if !hasInput {
		return input, strings.Join(systemPrompts, "\n\n")
	}

	switch typed := rawInput.(type) {
	case string:
		if text := typed; text != "" {
			input = append(input, agento11y.Message{Role: agento11y.RoleUser, Parts: []agento11y.Part{agento11y.TextPart(text)}})
		}
	case []any:
		for i := range typed {
			item, ok := typed[i].(map[string]any)
			if !ok {
				continue
			}

			itemType := strings.TrimSpace(fmt.Sprintf("%v", item["type"]))
			role := strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", item["role"])))

			if itemType == "message" && (role == "system" || role == "developer") {
				systemPrompts = append(systemPrompts, extractResponsesText(item["content"]))
				continue
			}

			if itemType == "function_call_output" {
				content := extractResponsesText(item["output"])
				if content == "" {
					content = jsonValueText(item["output"])
				}
				if content == "" {
					continue
				}
				part := agento11y.ToolResultPart(agento11y.ToolResult{
					ToolCallID:  responsesMapString(item, "call_id", "callId"),
					Name:        responsesMapString(item, "name"),
					Content:     content,
					ContentJSON: parseJSONOrString(content),
				})
				part.Metadata.ProviderType = "tool_result"
				input = append(input, agento11y.Message{Role: agento11y.RoleTool, Parts: []agento11y.Part{part}})
				continue
			}

			if itemType == "message" || role != "" {
				content := extractResponsesText(item["content"])
				if content == "" {
					continue
				}

				mappedRole := agento11y.RoleUser
				switch role {
				case "assistant":
					mappedRole = agento11y.RoleAssistant
				case "tool":
					mappedRole = agento11y.RoleTool
				}

				input = append(input, agento11y.Message{Role: mappedRole, Parts: []agento11y.Part{agento11y.TextPart(content)}})
			}
		}
	}

	return input, strings.Join(systemPrompts, "\n\n")
}

func mapResponsesOutput(items []responses.ResponseOutputItemUnion) []agento11y.Message {
	if len(items) == 0 {
		return nil
	}

	out := make([]agento11y.Message, 0, len(items))
	for i := range items {
		item := items[i]
		switch item.Type {
		case "message":
			text := extractResponsesOutputMessageText(item.Content)
			if text == "" {
				continue
			}
			out = append(out, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.TextPart(text)}})
		case "function_call":
			if strings.TrimSpace(item.Name) == "" {
				continue
			}
			part := agento11y.ToolCallPart(agento11y.ToolCall{
				ID:        item.CallID,
				Name:      item.Name,
				InputJSON: parseResponsesOutputArguments(item.Arguments),
			})
			part.Metadata.ProviderType = "tool_call"
			out = append(out, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{part}})
		default:
			fallback := extractResponsesOutputFallback(item)
			if fallback != "" {
				out = append(out, agento11y.Message{Role: agento11y.RoleAssistant, Parts: []agento11y.Part{agento11y.TextPart(fallback)}})
			}
		}
	}

	return out
}

func mapResponsesTools(value any) []agento11y.ToolDefinition {
	tools, ok := value.([]any)
	if !ok || len(tools) == 0 {
		return nil
	}

	out := make([]agento11y.ToolDefinition, 0, len(tools))
	for i := range tools {
		tool, ok := tools[i].(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.TrimSpace(fmt.Sprintf("%v", tool["type"]))
		if toolType == "function" {
			name := fmt.Sprintf("%v", tool["name"])
			if strings.TrimSpace(name) == "" {
				continue
			}
			definition := agento11y.ToolDefinition{
				Name:        name,
				Description: fmt.Sprintf("%v", tool["description"]),
				Type:        "function",
			}
			if parameters, exists := tool["parameters"]; exists {
				definition.InputSchema = jsonValueBytes(parameters)
			}
			out = append(out, definition)
			continue
		}

		name := fmt.Sprintf("%v", tool["name"])
		if toolType != "" && strings.TrimSpace(name) != "" {
			out = append(out, agento11y.ToolDefinition{Name: name, Type: toolType})
		}
	}

	return out
}

func mapResponsesUsage(usage responses.ResponseUsage) agento11y.TokenUsage {
	return agento11y.TokenUsage{
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		TotalTokens:          usage.TotalTokens,
		CacheReadInputTokens: usage.InputTokensDetails.CachedTokens,
		ReasoningTokens:      usage.OutputTokensDetails.ReasoningTokens,
	}
}

func normalizeResponsesStopReason(resp *responses.Response) string {
	if resp == nil {
		return ""
	}

	status := strings.TrimSpace(string(resp.Status))
	statusLower := strings.ToLower(status)
	if statusLower == "incomplete" {
		reason := strings.TrimSpace(resp.IncompleteDetails.Reason)
		if reason != "" {
			return reason
		}
		return "incomplete"
	}
	if statusLower == "completed" {
		return "stop"
	}
	return statusLower
}

func mapResponsesRequestControls(payload map[string]any) (*int64, *float64, *float64, *string, *bool, *int64) {
	maxTokens := readInt64(payload, "max_output_tokens")
	temperature := readFloat64(payload, "temperature")
	topP := readFloat64(payload, "top_p")
	toolChoice := canonicalToolChoice(payload["tool_choice"])

	var thinkingEnabled *bool
	if _, ok := payload["reasoning"]; ok {
		thinkingEnabled = boolPtr(true)
	}
	thinkingBudget := resolveThinkingBudget(payload["reasoning"])

	return maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget
}

func extractResponsesText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for i := range typed {
			if text := extractResponsesText(typed[i]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, ok := typed["text"].(string); ok {
			return text
		}
		if text, ok := typed["content"].(string); ok {
			return text
		}
		if refusal, ok := typed["refusal"].(string); ok {
			return refusal
		}
	}
	return ""
}

func extractResponsesOutputMessageText(content []responses.ResponseOutputMessageContentUnion) string {
	parts := make([]string, 0, len(content))
	for i := range content {
		item := content[i]
		switch item.Type {
		case "output_text":
			if text := item.Text; text != "" {
				parts = append(parts, text)
			}
		case "refusal":
			if refusal := item.Refusal; refusal != "" {
				parts = append(parts, refusal)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func extractResponsesOutputFallback(item responses.ResponseOutputItemUnion) string {
	if item.Input != "" {
		return item.Input
	}
	if item.Result != "" {
		return item.Result
	}
	if item.Error != "" {
		return item.Error
	}
	arguments := stringifyResponsesOutputArguments(item.Arguments)
	if item.Name != "" && arguments != "" {
		return fmt.Sprintf("%s(%s)", item.Name, arguments)
	}
	return ""
}

func parseResponsesOutputArguments(arguments responses.ResponseOutputItemUnionArguments) []byte {
	return parseJSONOrString(stringifyResponsesOutputArguments(arguments))
}

func stringifyResponsesOutputArguments(arguments responses.ResponseOutputItemUnionArguments) string {
	if arguments.OfString != "" {
		return arguments.OfString
	}
	if arguments.OfResponseToolSearchCallArguments == nil {
		return ""
	}
	data, err := json.Marshal(arguments.OfResponseToolSearchCallArguments)
	if err != nil {
		return ""
	}
	return string(data)
}

func marshalAny(value any) map[string]any {
	raw, err := json.Marshal(value)
	if err != nil {
		return map[string]any{}
	}

	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return map[string]any{}
	}
	return payload
}

func jsonValueBytes(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return data
}

func jsonValueText(value any) string {
	data := jsonValueBytes(value)
	if len(data) == 0 {
		return ""
	}
	return string(data)
}

func responsesMapString(item map[string]any, keys ...string) string {
	for _, key := range keys {
		value, ok := item[key]
		if !ok {
			continue
		}
		if text, ok := value.(string); ok {
			return strings.TrimSpace(text)
		}
	}
	return ""
}
