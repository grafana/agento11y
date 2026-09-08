package gemini

import (
	"errors"
	"slices"
	"strings"
	"time"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y"
)

// StreamSummary captures Gemini streamed responses.
type StreamSummary struct {
	Responses    []*genai.GenerateContentResponse
	FirstChunkAt time.Time
}

// FromStream maps Gemini streaming output to agento11y.Generation.
func FromStream(
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	summary StreamSummary,
	opts ...Option,
) (agento11y.Generation, error) {
	if strings.TrimSpace(model) == "" {
		return agento11y.Generation{}, errors.New("request model is required")
	}
	if len(summary.Responses) == 0 {
		return agento11y.Generation{}, errors.New("stream summary has no responses")
	}

	options := applyOptions(opts)
	input := mapContents(contents)
	controls := mapRequestControls(config)
	thinkingLevel := extractThinkingLevel(config)
	accumulators := map[int32]*streamCandidate{}
	candidateIndexes := make([]int32, 0, 1)
	usage := agento11y.TokenUsage{}
	var usageMetadata *genai.GenerateContentResponseUsageMetadata
	responseID := ""
	responseModel := ""

	for _, response := range summary.Responses {
		if response == nil {
			continue
		}

		// Missing indexes decode as zero. Use response position when two
		// candidates would otherwise share one index.
		responseIndexes := make(map[int32]struct{}, len(response.Candidates))
		for position, candidate := range response.Candidates {
			if candidate == nil {
				continue
			}
			index := candidate.Index
			if _, duplicate := responseIndexes[index]; duplicate {
				index = int32(position)
				for {
					if _, duplicate = responseIndexes[index]; !duplicate {
						break
					}
					index++
				}
			}
			responseIndexes[index] = struct{}{}
			accumulator, ok := accumulators[index]
			if !ok {
				accumulator = &streamCandidate{}
				accumulators[index] = accumulator
				candidateIndexes = append(candidateIndexes, index)
			}
			if parts := mapCandidateParts(candidate.Content); len(parts) > 0 {
				accumulator.messages = appendStreamMessages(accumulator.messages, []agento11y.Message{{
					Role:  agento11y.RoleAssistant,
					Parts: parts,
				}})
			}
			if candidate.FinishReason != "" {
				accumulator.finishReason = string(candidate.FinishReason)
			}
		}
		if response.UsageMetadata != nil {
			usage = mapUsage(response.UsageMetadata)
			usageMetadata = response.UsageMetadata
		}
		if response.ResponseID != "" {
			responseID = response.ResponseID
		}
		if response.ModelVersion != "" {
			responseModel = response.ModelVersion
		}
	}

	slices.Sort(candidateIndexes)
	output := make([]agento11y.Message, 0, len(candidateIndexes))
	stopReason := ""
	for _, index := range candidateIndexes {
		accumulator := accumulators[index]
		if stopReason == "" {
			stopReason = accumulator.finishReason
		}
		if len(accumulator.messages) == 0 && accumulator.finishReason != "" {
			accumulator.messages = []agento11y.Message{{Role: agento11y.RoleAssistant}}
		}
		for i := range accumulator.messages {
			accumulator.messages[i].FinishReason = accumulator.finishReason
		}
		output = append(output, accumulator.messages...)
	}

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
	if options.includeToolsArtifact && hasFunctionTools(config) {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindTools, "gemini.generate_content.tools", config.Tools)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	if options.includeEventsArtifact {
		artifact, err := agento11y.NewJSONArtifact(agento11y.ArtifactKindProviderEvent, "gemini.generate_content.stream", summary.Responses)
		if err != nil {
			return agento11y.Generation{}, err
		}
		artifacts = append(artifacts, artifact)
	}
	metadata := cloneAnyMap(options.metadata)
	if responseModel != "" {
		if metadata == nil {
			metadata = map[string]any{}
		}
		metadata["model_version"] = responseModel
	}
	metadata = mergeThinkingBudgetMetadata(metadata, controls.thinkingBudget)
	metadata = mergeThinkingLevelMetadata(metadata, thinkingLevel)
	metadata = mergeGeminiUsageMetadata(metadata, usageMetadata)

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		OperationName:     generationOperation(contents, config, streamCandidates(summary.Responses)),
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
		ResponseID:        responseID,
		ResponseModel:     responseModel,
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
		Usage:             usage,
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

type streamCandidate struct {
	messages     []agento11y.Message
	finishReason string
}

func appendStreamMessages(accumulated, delta []agento11y.Message) []agento11y.Message {
	for _, message := range delta {
		if len(accumulated) == 0 || accumulated[len(accumulated)-1].Role != message.Role || accumulated[len(accumulated)-1].Name != message.Name {
			accumulated = append(accumulated, message)
			continue
		}
		last := &accumulated[len(accumulated)-1]
		for _, part := range message.Parts {
			if len(last.Parts) > 0 && mergeStreamPart(&last.Parts[len(last.Parts)-1], part) {
				continue
			}
			last.Parts = append(last.Parts, part)
		}
	}
	return accumulated
}

func mergeStreamPart(accumulated *agento11y.Part, delta agento11y.Part) bool {
	if accumulated.Kind != delta.Kind {
		return false
	}
	switch accumulated.Kind {
	case agento11y.PartKindText:
		accumulated.Text += delta.Text
		return true
	case agento11y.PartKindThinking:
		accumulated.Thinking += delta.Thinking
		return true
	default:
		return false
	}
}

func streamCandidates(responses []*genai.GenerateContentResponse) []*genai.Candidate {
	var candidates []*genai.Candidate
	for _, response := range responses {
		if response != nil {
			candidates = append(candidates, response.Candidates...)
		}
	}
	return candidates
}
