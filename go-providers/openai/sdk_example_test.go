package openai

import (
	"context"
	"os"

	"github.com/grafana/agento11y/go/sigil"
	osdk "github.com/openai/openai-go/v3"
	osdkoption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

// Example_withSigilWrapper shows the one-liner wrapper approach.
func Example_withSigilWrapper() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := osdk.NewClient(osdkoption.WithAPIKey(apiKey))
	req := exampleOpenAIRequest()

	resp, err := ChatCompletionsNew(context.Background(), client, providerClient, req,
		WithConversationID("conv-openai-1"),
		WithAgentName("assistant-openai"),
		WithAgentVersion("1.0.0"),
	)
	if err != nil {
		panic(err)
	}

	_ = resp.Choices[0].Message.Content
}

// Example_withSigilDefer shows the defer pattern for full control.
func Example_withSigilDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := osdk.NewClient(osdkoption.WithAPIKey(apiKey))
	req := exampleOpenAIRequest()

	ctx, rec := client.StartGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-openai-2",
		AgentName:      "assistant-openai",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "openai", Name: req.Model},
	})
	defer rec.End()

	resp, err := providerClient.Chat.Completions.New(ctx, req)
	if err != nil {
		rec.SetCallError(err)
		return
	}

	rec.SetResult(ChatCompletionsFromRequestResponse(req, resp,
		WithConversationID("conv-openai-2"),
		WithAgentName("assistant-openai"),
		WithAgentVersion("1.0.0"),
	))
	_ = resp.Choices[0].Message.Content
}

// Example_withSigilStreamingWrapper shows the streaming wrapper approach.
func Example_withSigilStreamingWrapper() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := osdk.NewClient(osdkoption.WithAPIKey(apiKey))
	req := exampleOpenAIRequest()

	_, _, err := ChatCompletionsNewStreaming(context.Background(), client, providerClient, req,
		WithConversationID("conv-openai-3"),
		WithAgentName("assistant-openai"),
		WithAgentVersion("1.0.0"),
	)
	if err != nil {
		panic(err)
	}
}

// Example_withSigilStreamingDefer shows the defer pattern for streaming with per-chunk processing.
func Example_withSigilStreamingDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := os.Getenv("OPENAI_API_KEY")
	if apiKey == "" {
		return
	}

	client := sigil.NewClient(sigil.DefaultConfig())
	providerClient := osdk.NewClient(osdkoption.WithAPIKey(apiKey))
	req := exampleOpenAIRequest()

	ctx, rec := client.StartStreamingGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-openai-4",
		AgentName:      "assistant-openai",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "openai", Name: req.Model},
	})
	defer rec.End()

	stream := providerClient.Chat.Completions.NewStreaming(ctx, req)
	defer func() {
		if closeErr := stream.Close(); closeErr != nil {
			// Best-effort close in example flow.
			_ = closeErr
		}
	}()

	summary := ChatCompletionsStreamSummary{}
	for stream.Next() {
		chunk := stream.Current()
		summary.Chunks = append(summary.Chunks, chunk)
		// Process each chunk here (e.g., SSE forwarding).
	}
	if err := stream.Err(); err != nil {
		rec.SetCallError(err)
		return
	}

	rec.SetResult(ChatCompletionsFromStream(req, summary,
		WithConversationID("conv-openai-4"),
		WithAgentName("assistant-openai"),
		WithAgentVersion("1.0.0"),
	))
}

func exampleOpenAIRequest() osdk.ChatCompletionNewParams {
	return osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.UserMessage("Hello"),
		},
	}
}
