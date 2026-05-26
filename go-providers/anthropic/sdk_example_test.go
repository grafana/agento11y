package anthropic

import (
	"context"
	"os"

	asdk "github.com/anthropics/anthropic-sdk-go"
	asdkoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/grafana/sigil-sdk/go/sigil"
)

// Example_withSigilWrapper shows the one-liner wrapper approach.
func Example_withSigilWrapper() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	resp, err := Message(context.Background(), client, providerClient, req,
		WithConversationID("conv-anthropic-1"),
		WithAgentName("assistant-anthropic"),
		WithAgentVersion("1.0.0"),
	)
	if err != nil {
		panic(err)
	}

	_ = resp.Content[0].Text
}

// Example_withSigilDefer shows the defer pattern for full control.
func Example_withSigilDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	ctx, rec := client.StartGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-anthropic-2",
		AgentName:      "assistant-anthropic",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "anthropic", Name: req.Model},
	})
	defer rec.End()

	resp, err := providerClient.Beta.Messages.New(ctx, req)
	if err != nil {
		rec.SetCallError(err)
		return
	}

	rec.SetResult(FromRequestResponse(req, resp,
		WithConversationID("conv-anthropic-2"),
		WithAgentName("assistant-anthropic"),
		WithAgentVersion("1.0.0"),
	))
	_ = resp.Content[0].Text
}

// Example_withSigilStreamingWrapper shows the streaming wrapper approach.
func Example_withSigilStreamingWrapper() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	_, _, err := MessageStream(context.Background(), client, providerClient, req,
		WithConversationID("conv-anthropic-3"),
		WithAgentName("assistant-anthropic"),
		WithAgentVersion("1.0.0"),
	)
	if err != nil {
		panic(err)
	}
}

// Example_withSigilStreamingDefer shows the defer pattern for streaming with per-event processing.
func Example_withSigilStreamingDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	ctx, rec := client.StartStreamingGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-anthropic-4",
		AgentName:      "assistant-anthropic",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "anthropic", Name: req.Model},
	})
	defer rec.End()

	stream := providerClient.Beta.Messages.NewStreaming(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close in example flow.
			_ = closeErr
		}
	}()

	summary := StreamSummary{}
	for stream.Next() {
		event := stream.Current()
		summary.Events = append(summary.Events, event)
		// Process each event here (e.g., SSE forwarding).
	}
	if err := stream.Err(); err != nil {
		rec.SetCallError(err)
		return
	}

	rec.SetResult(FromStream(req, summary,
		WithConversationID("conv-anthropic-4"),
		WithAgentName("assistant-anthropic"),
		WithAgentVersion("1.0.0"),
	))
}

func exampleAnthropicRequest() asdk.BetaMessageNewParams {
	return asdk.BetaMessageNewParams{
		Model: asdk.Model("claude-sonnet-4-5"),
		Messages: []asdk.BetaMessageParam{
			{
				Role: asdk.BetaMessageParamRoleUser,
				Content: []asdk.BetaContentBlockParamUnion{
					asdk.NewBetaTextBlock("Hello"),
				},
			},
		},
	}
}
