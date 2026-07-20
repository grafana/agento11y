package gemini

import (
	"errors"
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
	maxTokens, temperature, topP, toolChoice, thinkingEnabled, thinkingBudget := mapRequestControls(config)
	thinkingLevel := extractThinkingLevel(config)
	output := make([]agento11y.Message, 0, len(summary.Responses))
	stopReason := ""
	usage := agento11y.TokenUsage{}
	var usageMetadata *genai.GenerateContentResponseUsageMetadata
	responseID := ""
	responseModel := ""

	for _, response := range summary.Responses {
		if response == nil {
			continue
		}

		candidateMessages, candidateStop := mapCandidates(response.Candidates)
		output = append(output, candidateMessages...)
		if candidateStop != "" {
			stopReason = candidateStop
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
	metadata = mergeThinkingBudgetMetadata(metadata, thinkingBudget)
	metadata = mergeThinkingLevelMetadata(metadata, thinkingLevel)
	metadata = mergeGeminiUsageMetadata(metadata, usageMetadata)

	generation := agento11y.Generation{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
		ResponseID:        responseID,
		ResponseModel:     responseModel,
		SystemPrompt:      extractSystemPrompt(config),
		Input:             input,
		Output:            output,
		Tools:             mapTools(config),
		MaxTokens:         maxTokens,
		Temperature:       temperature,
		TopP:              topP,
		ToolChoice:        toolChoice,
		ThinkingEnabled:   thinkingEnabled,
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
