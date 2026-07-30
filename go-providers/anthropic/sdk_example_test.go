package anthropic

import (
	"context"
	"os"

	asdk "github.com/anthropics/anthropic-sdk-go"
	asdkoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/grafana/agento11y/go/agento11y"
)

// Example_withAgento11yWrapper shows the one-liner wrapper approach.
func Example_withAgento11yWrapper() {
	if os.Getenv("AGENTO11Y_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
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

// Example_withAgento11yDefer shows the defer pattern for full control.
func Example_withAgento11yDefer() {
	if os.Getenv("AGENTO11Y_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	ctx, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-anthropic-2",
		AgentName:      "assistant-anthropic",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: req.Model},
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

// Example_withAgento11yStreamingWrapper shows the streaming wrapper approach.
func Example_withAgento11yStreamingWrapper() {
	if os.Getenv("AGENTO11Y_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
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

// Example_withAgento11yStreamingDefer shows the defer pattern for streaming with per-event processing.
func Example_withAgento11yStreamingDefer() {
	if os.Getenv("AGENTO11Y_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
	providerClient := asdk.NewClient(asdkoption.WithAPIKey(apiKey))
	req := exampleAnthropicRequest()

	ctx, rec := client.StartStreamingGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-anthropic-4",
		AgentName:      "assistant-anthropic",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: req.Model},
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
