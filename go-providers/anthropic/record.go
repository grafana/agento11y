package anthropic

import (
	"context"
	"time"

	asdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/grafana/agento11y/go/agento11y"
)

// Message calls the Anthropic messages API and records the generation.
// It mirrors providerClient.Beta.Messages.New but adds agento11y recording.
// The native *asdk.BetaMessage response is returned unchanged.
func Message(
	ctx context.Context,
	client *agento11y.Client,
	provider asdk.Client,
	req asdk.BetaMessageNewParams,
	opts ...Option,
) (*asdk.BetaMessage, error) {
	return message(ctx, client, req, func(ctx context.Context, request asdk.BetaMessageNewParams) (*asdk.BetaMessage, error) {
		return provider.Beta.Messages.New(ctx, request)
	}, opts...)
}

func message(
	ctx context.Context,
	client *agento11y.Client,
	req asdk.BetaMessageNewParams,
	invoke func(context.Context, asdk.BetaMessageNewParams) (*asdk.BetaMessage, error),
	opts ...Option,
) (*asdk.BetaMessage, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartGeneration(ctx, anthropicGenerationStart(options, req))
	defer rec.End()

	resp, err := invoke(ctx, req)
	if resp != nil {
		rec.SetResult(FromRequestResponse(req, resp, opts...))
	}
	if err != nil {
		rec.SetCallError(err)
		return resp, err
	}

	return resp, rec.Err()
}

// MessageStream calls the Anthropic streaming messages API and records the generation.
// It mirrors providerClient.Beta.Messages.NewStreaming but adds agento11y recording.
// All events are collected into StreamSummary; for per-event processing use the
// defer pattern directly with StartStreamingGeneration.
func MessageStream(
	ctx context.Context,
	client *agento11y.Client,
	provider asdk.Client,
	req asdk.BetaMessageNewParams,
	opts ...Option,
) (*asdk.BetaMessage, StreamSummary, error) {
	return messageStream(ctx, client, req, func(ctx context.Context, request asdk.BetaMessageNewParams) betaMessageEventStream {
		return provider.Beta.Messages.NewStreaming(ctx, request)
	}, opts...)
}

func anthropicGenerationStart(options mapperOptions, req asdk.BetaMessageNewParams) agento11y.GenerationStart {
	controls := mapRequestControls(req)
	return agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
		MaxTokens:         controls.maxTokens,
		Temperature:       controls.temperature,
		TopP:              controls.topP,
		TopK:              controls.topK,
		OutputType:        controls.outputType,
		ToolChoice:        controls.toolChoice,
		ThinkingEnabled:   controls.thinkingEnabled,
		Tags:              options.tags,
		Metadata:          mergeThinkingBudgetMetadata(options.metadata, controls.thinkingBudget),
	}
}

type betaMessageEventStream interface {
	Next() bool
	Current() asdk.BetaRawMessageStreamEventUnion
	Err() error
	Close() error
}

func messageStream(
	ctx context.Context,
	client *agento11y.Client,
	req asdk.BetaMessageNewParams,
	invoke func(context.Context, asdk.BetaMessageNewParams) betaMessageEventStream,
	opts ...Option,
) (*asdk.BetaMessage, StreamSummary, error) {
	options := applyOptions(opts)

	ctx, rec := client.StartStreamingGeneration(ctx, anthropicGenerationStart(options, req))
	defer rec.End()

	stream := invoke(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close on stream teardown.
			_ = closeErr
		}
	}()

	summary := StreamSummary{}
	accumulated := &asdk.BetaMessage{}
	canUseAccumulated := true
	for stream.Next() {
		if summary.FirstChunkAt.IsZero() {
			summary.FirstChunkAt = time.Now().UTC()
			rec.SetFirstTokenAt(summary.FirstChunkAt)
		}
		event := stream.Current()
		summary.Events = append(summary.Events, event)
		if canUseAccumulated {
			// Let the provider SDK own reconstruction semantics. Accumulation is
			// observability-only: malformed event ordering must not alter the
			// provider stream's response or error behavior.
			if err := accumulated.Accumulate(event); err != nil {
				canUseAccumulated = false
				summary.FinalMessage = nil
			} else if event.Type == "message_start" || summary.FinalMessage != nil {
				summary.FinalMessage = accumulated
			}
		}
	}

	// Always map what the provider delivered, including an empty stream. This
	// retains partial data after read failures and marks empty/ping-only streams
	// as instrumentation errors instead of exporting successful seed-only data.
	rec.SetResult(FromStream(req, summary, opts...))
	if err := stream.Err(); err != nil {
		rec.SetCallError(err)
		return nil, summary, err
	}

	if summary.FinalMessage != nil {
		return summary.FinalMessage, summary, rec.Err()
	}
	return nil, summary, rec.Err()
}
