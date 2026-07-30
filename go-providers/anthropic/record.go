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

	ctx, rec := client.StartGeneration(ctx, agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
	})
	defer rec.End()

	resp, err := invoke(ctx, req)
	if err != nil {
		rec.SetCallError(err)
		return nil, err
	}

	rec.SetResult(FromRequestResponse(req, resp, opts...))
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
	options := applyOptions(opts)

	ctx, rec := client.StartStreamingGeneration(ctx, agento11y.GenerationStart{
		ConversationID:    options.conversationID,
		ConversationTitle: options.conversationTitle,
		AgentName:         options.agentName,
		AgentVersion:      options.agentVersion,
		Model:             agento11y.ModelRef{Provider: options.providerName, Name: req.Model},
	})
	defer rec.End()

	stream := provider.Beta.Messages.NewStreaming(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close on stream teardown.
			_ = closeErr
		}
	}()

	summary := StreamSummary{}
	for stream.Next() {
		if summary.FirstChunkAt.IsZero() {
			summary.FirstChunkAt = time.Now().UTC()
			rec.SetFirstTokenAt(summary.FirstChunkAt)
		}
		summary.Events = append(summary.Events, stream.Current())
	}
	if err := stream.Err(); err != nil {
		rec.SetCallError(err)
		return nil, summary, err
	}

	rec.SetResult(FromStream(req, summary, opts...))

	if summary.FinalMessage != nil {
		return summary.FinalMessage, summary, rec.Err()
	}
	return nil, summary, rec.Err()
}
