package gemini

import (
	"context"
	"time"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y"
)

// GenerateContent calls the Gemini generate-content API and records the generation.
// It mirrors providerClient.Models.GenerateContent but adds Sigil recording.
// The native *genai.GenerateContentResponse is returned unchanged.
func GenerateContent(
	ctx context.Context,
	client *agento11y.Client,
	provider *genai.Client,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	opts ...Option,
) (*genai.GenerateContentResponse, error) {
	return generateContent(ctx, client, model, contents, config, func(
		ctx context.Context,
		model string,
		contents []*genai.Content,
		config *genai.GenerateContentConfig,
	) (*genai.GenerateContentResponse, error) {
		return provider.Models.GenerateContent(ctx, model, contents, config)
	}, opts...)
}

func generateContent(
	ctx context.Context,
	client *agento11y.Client,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	invoke func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error),
	opts ...Option,
) (*genai.GenerateContentResponse, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
	})
	defer rec.End()

	resp, err := invoke(ctx, model, contents, config)
	if err != nil {
		rec.SetCallError(err)
		return nil, err
	}

	rec.SetResult(FromRequestResponse(model, contents, config, resp, opts...))
	return resp, rec.Err()
}

// EmbedContent calls the Gemini embed-content API and records an embeddings span.
// It mirrors providerClient.Models.EmbedContent but adds Sigil recording.
func EmbedContent(
	ctx context.Context,
	client *agento11y.Client,
	provider *genai.Client,
	model string,
	contents []*genai.Content,
	config *genai.EmbedContentConfig,
	opts ...Option,
) (*genai.EmbedContentResponse, error) {
	return embedContent(ctx, client, model, contents, config, func(
		ctx context.Context,
		model string,
		contents []*genai.Content,
		config *genai.EmbedContentConfig,
	) (*genai.EmbedContentResponse, error) {
		return provider.Models.EmbedContent(ctx, model, contents, config)
	}, opts...)
}

func embedContent(
	ctx context.Context,
	client *agento11y.Client,
	model string,
	contents []*genai.Content,
	config *genai.EmbedContentConfig,
	invoke func(context.Context, string, []*genai.Content, *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error),
	opts ...Option,
) (*genai.EmbedContentResponse, error) {
	options := applyOptions(opts)

	start := agento11y.EmbeddingStart{
		AgentName:    options.agentName,
		AgentVersion: options.agentVersion,
		Model:        agento11y.ModelRef{Provider: options.providerName, Name: model},
	}
	if config != nil && config.OutputDimensionality != nil {
		dimensions := int64(*config.OutputDimensionality)
		if dimensions > 0 {
			start.Dimensions = &dimensions
		}
	}

	ctx, rec := client.StartEmbedding(ctx, start)
	defer rec.End()

	resp, err := invoke(ctx, model, contents, config)
	if err != nil {
		rec.SetCallError(err)
		return nil, err
	}

	rec.SetResult(EmbeddingFromResponse(model, contents, config, resp))
	rec.End()
	return resp, rec.Err()
}

// GenerateContentStream calls the Gemini streaming generate-content API and records the generation.
// It mirrors providerClient.Models.GenerateContentStream but adds Sigil recording.
// All responses are collected into StreamSummary; for per-response processing use the
// defer pattern directly with StartStreamingGeneration.
func GenerateContentStream(
	ctx context.Context,
	client *agento11y.Client,
	provider *genai.Client,
	model string,
	contents []*genai.Content,
	config *genai.GenerateContentConfig,
	opts ...Option,
) (StreamSummary, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartStreamingGeneration(ctx, agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
	})
	defer rec.End()

	summary := StreamSummary{}
	for response, err := range provider.Models.GenerateContentStream(ctx, model, contents, config) {
		if err != nil {
			rec.SetCallError(err)
			return summary, err
		}
		if response != nil {
			if summary.FirstChunkAt.IsZero() {
				summary.FirstChunkAt = time.Now().UTC()
				rec.SetFirstTokenAt(summary.FirstChunkAt)
			}
			summary.Responses = append(summary.Responses, response)
		}
	}

	rec.SetResult(FromStream(model, contents, config, summary, opts...))
	return summary, rec.Err()
}
