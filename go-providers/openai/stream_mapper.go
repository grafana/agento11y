package openai

import (
	"errors"
	"sort"
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

type streamChoice struct {
	index        int64
	text         strings.Builder
	refusal      strings.Builder
	finishReason string
	toolCalls    map[int64]*streamToolCall
	toolOrder    []int64
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
	controls := mapRequestControls(req)

	modelName := req.Model
	responseID := ""
	usage := agento11y.TokenUsage{}
	choices := map[int64]*streamChoice{}
	choiceOrder := make([]int64, 0, 2)

	for i := range summary.Chunks {
		chunk := summary.Chunks[i]
		if chunk.ID != "" {
			responseID = chunk.ID
		}
		if chunk.Model != "" {
			modelName = chunk.Model
		}
		if completionUsagePresent(chunk.Usage) {
			usage = mapUsage(chunk.Usage)
		}

		for _, choice := range chunk.Choices {
			accumulated, ok := choices[choice.Index]
			if !ok {
				accumulated = &streamChoice{
					index:     choice.Index,
					toolCalls: map[int64]*streamToolCall{},
				}
				choices[choice.Index] = accumulated
				choiceOrder = append(choiceOrder, choice.Index)
			}
			if choice.FinishReason != "" {
				accumulated.finishReason = choice.FinishReason
			}

			delta := choice.Delta
			accumulated.text.WriteString(delta.Content)
			accumulated.refusal.WriteString(delta.Refusal)

			for _, toolCall := range delta.ToolCalls {
				call, ok := accumulated.toolCalls[toolCall.Index]
				if !ok {
					call = &streamToolCall{}
					accumulated.toolCalls[toolCall.Index] = call
					accumulated.toolOrder = append(accumulated.toolOrder, toolCall.Index)
				}
				if toolCall.ID != "" {
					call.id = toolCall.ID
				}
				if toolCall.Function.Name != "" {
					call.name = toolCall.Function.Name
				}
				call.arguments.WriteString(toolCall.Function.Arguments)
			}
		}
	}

	orderedChoices := make([]*streamChoice, 0, len(choiceOrder))
	for _, index := range choiceOrder {
		orderedChoices = append(orderedChoices, choices[index])
	}
	sort.SliceStable(orderedChoices, func(i, j int) bool {
		return orderedChoices[i].index < orderedChoices[j].index
	})

	output := make([]agento11y.Message, 0, len(orderedChoices))
	stopReason := ""
	for _, choice := range orderedChoices {
		if stopReason == "" && choice.finishReason != "" {
			stopReason = choice.finishReason
		}
		parts := make([]agento11y.Part, 0, 2+len(choice.toolOrder))
		if generated := choice.text.String(); generated != "" {
			parts = append(parts, agento11y.TextPart(generated))
		}
		if refusal := choice.refusal.String(); refusal != "" {
			parts = append(parts, agento11y.TextPart(refusal))
		}
		sort.SliceStable(choice.toolOrder, func(i, j int) bool { return choice.toolOrder[i] < choice.toolOrder[j] })
		for _, index := range choice.toolOrder {
			call := choice.toolCalls[index]
			if call == nil || strings.TrimSpace(call.name) == "" {
				continue
			}
			part := agento11y.ToolCallPart(agento11y.ToolCall{
				ID:        call.id,
				Name:      call.name,
				InputJSON: parseJSONOrString(call.arguments.String()),
			})
			part.Metadata.ProviderType = "tool_call"
			parts = append(parts, part)
		}
		if len(parts) > 0 || choice.finishReason != "" {
			output = append(output, agento11y.Message{
				Role:         agento11y.RoleAssistant,
				Parts:        parts,
				FinishReason: choice.finishReason,
			})
		}
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
		MaxTokens:         controls.maxTokens,
		Temperature:       controls.temperature,
		TopP:              controls.topP,
		ChoiceCount:       controls.choiceCount,
		Seed:              controls.seed,
		OutputType:        controls.outputType,
		ToolChoice:        controls.toolChoice,
		ThinkingEnabled:   controls.thinkingEnabled,
		Usage:             usage,
		StopReason:        stopReason,
		Tags:              cloneStringMap(options.tags),
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, controls.thinkingBudget),
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
