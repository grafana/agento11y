package openai

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	osdk "github.com/openai/openai-go/v3"
	oresponses "github.com/openai/openai-go/v3/responses"

	"github.com/grafana/agento11y/go/agento11y"
)

// ChatCompletionsNew calls the OpenAI chat-completions API and records the generation.
// It mirrors providerClient.Chat.Completions.New but adds agento11y recording.
// The native *osdk.ChatCompletion response is returned unchanged.
func ChatCompletionsNew(
	ctx context.Context,
	client *agento11y.Client,
	provider osdk.Client,
	req osdk.ChatCompletionNewParams,
	opts ...Option,
) (*osdk.ChatCompletion, error) {
	return chatCompletionsNew(ctx, client, req, func(ctx context.Context, request osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
		return provider.Chat.Completions.New(ctx, request)
	}, opts...)
}

func chatCompletionsNew(
	ctx context.Context,
	client *agento11y.Client,
	req osdk.ChatCompletionNewParams,
	invoke func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error),
	opts ...Option,
) (*osdk.ChatCompletion, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartGeneration(ctx, openAIGenerationStart(options, req.Model, mapRequestControls(req)))
	defer rec.End()

	resp, err := invoke(ctx, req)
	rec.SetResult(ChatCompletionsFromRequestResponse(req, resp, opts...))
	if err != nil {
		rec.SetCallError(err)
		return resp, err
	}

	return resp, rec.Err()
}

// ChatCompletionsNewStreaming calls the OpenAI streaming chat-completions API and records the generation.
// It mirrors providerClient.Chat.Completions.NewStreaming but adds agento11y recording.
// All chunks are collected into ChatCompletionsStreamSummary; for per-chunk processing use the
// defer pattern directly with StartStreamingGeneration.
func ChatCompletionsNewStreaming(
	ctx context.Context,
	client *agento11y.Client,
	provider osdk.Client,
	req osdk.ChatCompletionNewParams,
	opts ...Option,
) (*osdk.ChatCompletion, ChatCompletionsStreamSummary, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartStreamingGeneration(ctx, openAIGenerationStart(options, req.Model, mapRequestControls(req)))
	defer rec.End()

	stream := provider.Chat.Completions.NewStreaming(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close on stream teardown.
			_ = closeErr
		}
	}()

	summary := ChatCompletionsStreamSummary{}
	for stream.Next() {
		if summary.FirstChunkAt.IsZero() {
			summary.FirstChunkAt = time.Now().UTC()
			rec.SetFirstTokenAt(summary.FirstChunkAt)
		}
		summary.Chunks = append(summary.Chunks, stream.Current())
	}
	if err := stream.Err(); err != nil {
		if len(summary.Chunks) > 0 {
			rec.SetResult(ChatCompletionsFromStream(req, summary, opts...))
		}
		rec.SetCallError(err)
		return nil, summary, err
	}

	rec.SetResult(ChatCompletionsFromStream(req, summary, opts...))

	// If there's a final response with choices, return it.
	if summary.FinalResponse != nil {
		return summary.FinalResponse, summary, rec.Err()
	}
	return nil, summary, rec.Err()
}

// ResponsesNew calls the OpenAI responses API and records the generation.
// It mirrors providerClient.Responses.New but adds agento11y recording.
func ResponsesNew(
	ctx context.Context,
	client *agento11y.Client,
	provider osdk.Client,
	req oresponses.ResponseNewParams,
	opts ...Option,
) (*oresponses.Response, error) {
	return responsesNew(ctx, client, req, func(ctx context.Context, request oresponses.ResponseNewParams) (*oresponses.Response, error) {
		return provider.Responses.New(ctx, request)
	}, opts...)
}

func responsesNew(
	ctx context.Context,
	client *agento11y.Client,
	req oresponses.ResponseNewParams,
	invoke func(context.Context, oresponses.ResponseNewParams) (*oresponses.Response, error),
	opts ...Option,
) (*oresponses.Response, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartGeneration(ctx, openAIGenerationStart(options, req.Model, mapResponsesRequestControls(marshalAny(req))))
	defer rec.End()

	resp, err := invoke(ctx, req)
	rec.SetResult(ResponsesFromRequestResponse(req, resp, opts...))
	if err != nil {
		rec.SetCallError(err)
		return resp, err
	}

	if failedErr := responsesFailedError(resp); failedErr != nil {
		rec.SetCallError(failedErr)
	}
	return resp, rec.Err()
}

// EmbeddingsNew calls the OpenAI embeddings API and records an embeddings span.
// It mirrors providerClient.Embeddings.New but adds agento11y recording.
func EmbeddingsNew(
	ctx context.Context,
	client *agento11y.Client,
	provider osdk.Client,
	req osdk.EmbeddingNewParams,
	opts ...Option,
) (*osdk.CreateEmbeddingResponse, error) {
	return embeddingsNew(ctx, client, req, func(ctx context.Context, request osdk.EmbeddingNewParams) (*osdk.CreateEmbeddingResponse, error) {
		return provider.Embeddings.New(ctx, request)
	}, opts...)
}

func embeddingsNew(
	ctx context.Context,
	client *agento11y.Client,
	req osdk.EmbeddingNewParams,
	invoke func(context.Context, osdk.EmbeddingNewParams) (*osdk.CreateEmbeddingResponse, error),
	opts ...Option,
) (*osdk.CreateEmbeddingResponse, error) {
	options := applyOptions(opts)

	start := agento11y.EmbeddingStart{
		AgentName:    options.agentName,
		AgentVersion: options.agentVersion,
		Model:        agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
	}
	if req.Dimensions.Valid() {
		start.Dimensions = &req.Dimensions.Value
	}
	if req.EncodingFormat != "" {
		start.EncodingFormat = string(req.EncodingFormat)
	}

	ctx, rec := client.StartEmbedding(ctx, start)
	defer rec.End()

	resp, err := invoke(ctx, req)
	if err != nil {
		rec.SetCallError(err)
		return nil, err
	}

	rec.SetResult(EmbeddingsFromResponse(req, resp))
	rec.End()
	return resp, rec.Err()
}

// ResponsesNewStreaming calls the OpenAI streaming responses API and records the generation.
// It mirrors providerClient.Responses.NewStreaming but adds agento11y recording.
func ResponsesNewStreaming(
	ctx context.Context,
	client *agento11y.Client,
	provider osdk.Client,
	req oresponses.ResponseNewParams,
	opts ...Option,
) (*oresponses.Response, ResponsesStreamSummary, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartStreamingGeneration(ctx, openAIGenerationStart(options, req.Model, mapResponsesRequestControls(marshalAny(req))))
	defer rec.End()

	stream := provider.Responses.NewStreaming(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close on stream teardown.
			_ = closeErr
		}
	}()

	summary := ResponsesStreamSummary{}
	for stream.Next() {
		if summary.FirstChunkAt.IsZero() {
			summary.FirstChunkAt = time.Now().UTC()
			rec.SetFirstTokenAt(summary.FirstChunkAt)
		}
		event := stream.Current()
		summary.Events = append(summary.Events, event)
		if isTerminalResponsesEvent(event.Type) && event.Response.ID != "" {
			final := event.Response
			summary.FinalResponse = &final
		}
	}
	if err := stream.Err(); err != nil {
		if len(summary.Events) > 0 {
			rec.SetResult(ResponsesFromStream(req, summary, opts...))
		}
		rec.SetCallError(err)
		return nil, summary, err
	}

	rec.SetResult(ResponsesFromStream(req, summary, opts...))
	if failedErr := responsesStreamFailedError(summary); failedErr != nil {
		rec.SetCallError(failedErr)
	}

	if summary.FinalResponse != nil {
		return summary.FinalResponse, summary, rec.Err()
	}
	return nil, summary, rec.Err()
}

func isTerminalResponsesEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.cancelled", "error":
		return true
	default:
		return false
	}
}

func responsesFailedError(resp *oresponses.Response) error {
	if resp == nil || !strings.EqualFold(strings.TrimSpace(string(resp.Status)), "failed") {
		return nil
	}
	return responseError(strings.TrimSpace(string(resp.Error.Code)), strings.TrimSpace(resp.Error.Message))
}

func responsesStreamFailedError(summary ResponsesStreamSummary) error {
	if err := responsesFailedError(summary.FinalResponse); err != nil {
		return err
	}
	for _, event := range slices.Backward(summary.Events) {
		if event.Type == "error" {
			return responseError(strings.TrimSpace(event.Code), strings.TrimSpace(event.Message))
		}
	}
	return nil
}

func responseError(code, message string) error {
	switch {
	case code != "" && message != "":
		return fmt.Errorf("openai response failed (%s): %s", code, message)
	case message != "":
		return errors.New(message)
	case code != "":
		return fmt.Errorf("openai response failed: %s", code)
	default:
		return errors.New("openai response failed")
	}
}

func openAIGenerationStart(options mapperOptions, model string, controls requestControls) agento11y.GenerationStart {
	return agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: model},
		MaxTokens:         controls.maxTokens,
		Temperature:       controls.temperature,
		TopP:              controls.topP,
		ChoiceCount:       controls.choiceCount,
		Seed:              controls.seed,
		OutputType:        controls.outputType,
		ToolChoice:        controls.toolChoice,
		ThinkingEnabled:   controls.thinkingEnabled,
		Tags:              options.tags,
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, controls.thinkingBudget),
	}
}
