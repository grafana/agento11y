package openai

import (
	"errors"
	"strings"
	"time"

	osdk "github.com/openai/openai-go/v3"

	"github.com/grafana/agento11y/go/agento11y"
)

// ChatCompletionsStreamSummary captures chat-completions stream chunks and an optional final response.
type ChatCompletionsStreamSummary struct {
	Chunks        []osdk.ChatCompletionChunk
	FinalResponse *osdk.ChatCompletion
	FirstChunkAt  time.Time
}

type streamToolCall struct {
	id        string
	name      string
	arguments strings.Builder
}

// ChatCompletionsFromStream maps OpenAI chat-completions streaming output to agento11y.Generation.
func ChatCompletionsFromStream(req osdk.ChatCompletionNewParams, summary ChatCompletionsStreamSummary, opts ...Option) (agento11y.Generation, error) {
	if summary.FinalResponse != nil {
		generation, err := ChatCompletionsFromRequestResponse(req, summary.FinalResponse, opts...)
		if err != nil {
			return agento11y.Generation{}, err
		}
		return appendChatCompletionsStreamEventsArtifact(generation, summary.Chunks, opts)
	}

	if len(summary.Chunks) == 0 {
		return agento11y.Generation{}, errors.New("stream summary has no chunks and no final response")
	}

	options := applyOptions(opts)
	input, systemPrompt := mapRequestMessages(req.Messages)
	output := make([]agento11y.Message, 0, 1)
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapRequestControls(req)

	modelName := req.Model
	responseID := ""
	usage := agento11y.TokenUsage{}
	stopReason := ""
	var text strings.Builder

	toolCalls := map[int64]*streamToolCall{}
	order := make([]int64, 0, 2)

	for i := range summary.Chunks {
		chunk := summary.Chunks[i]
		if chunk.ID != "" {
			responseID = chunk.ID
		}
		if chunk.Model != "" {
			modelName = chunk.Model
		}
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			usage = mapUsage(chunk.Usage)
		}

		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				stopReason = choice.FinishReason
			}

			delta := choice.Delta
			if delta.Content != "" {
				text.WriteString(delta.Content)
			}

			for _, toolCall := range delta.ToolCalls {
				call, ok := toolCalls[toolCall.Index]
				if !ok {
					call = &streamToolCall{}
					toolCalls[toolCall.Index] = call
					order = append(order, toolCall.Index)
				}
				if toolCall.ID != "" {
					call.id = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					call.name = toolCall.Function.Name
				}
				if toolCall.Function.Arguments != "" {
					call.arguments.WriteString(toolCall.Function.Arguments)
				}
			}
		}
	}

	assistantParts := make([]agento11y.Part, 0, 1+len(order))
	if generated := text.String(); generated != "" {
		assistantParts = append(assistantParts, agento11y.TextPart(generated))
	}
	for _, index := range order {
		call := toolCalls[index]
		if call == nil || strings.TrimSpace(call.name) == "" {
			continue
		}
		part := agento11y.ToolCallPart(agento11y.ToolCall{
			ID:        call.id,
			Name:      call.name,
			InputJSON: parseJSONOrString(call.arguments.String()),
		})
		part.Metadata.ProviderType = "tool_call"
		assistantParts = append(assistantParts, part)
	}
	if len(assistantParts) > 0 {
		output = append(output, agento11y.Message{
			Role:  agento11y.RoleAssistant,
			Parts: assistantParts,
		})
	}

	artifacts := make([]agento11y.Artifact, 0, 3)
	if options.includeRequestArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindRequest, "openai.chat.request", req)
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
	if options.includeEventsArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "openai.chat.stream_events", summary.Chunks)
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
		ResponseModel:     modelName,
		SystemPrompt:      systemPrompt,
		Input:             input,
		Output:            output,
		Tools:             mapTools(req.Tools),
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

func appendChatCompletionsStreamEventsArtifact(generation agento11y.Generation, chunks []osdk.ChatCompletionChunk, opts []Option) (agento11y.Generation, error) {
	if len(chunks) == 0 {
		return generation, nil
	}

	options := applyOptions(opts)
	if !options.includeEventsArtifact {
		return generation, nil
	}

	artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "openai.chat.stream_events", chunks)
	if err != nil {
		return agento11y.Generation{}, err
	}
	generation.Artifacts = append(generation.Artifacts, artifact)
	return generation, nil
}
