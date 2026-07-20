package gemini

import (
	"context"
	"os"

	"github.com/grafana/agento11y/go/agento11y"
	"google.golang.org/genai"
)

// Example_withAgento11yWrapper shows the one-liner wrapper approach.
func Example_withAgento11yWrapper() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := geminiAPIKey()
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
	model, contents, config := exampleGeminiRequest()

	providerClient, err := genai.NewClient(context.Background(), &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		panic(err)
	}

	resp, err := GenerateContent(context.Background(), client, providerClient, model, contents, config,
		WithConversationID("conv-gemini-1"),
		WithAgentName("assistant-gemini"),
		WithAgentVersion("1.0.0"),
	)
	if err != nil {
		panic(err)
	}

	_ = resp.Candidates[0].Content.Parts[0].Text
}

// Example_withAgento11yDefer shows the defer pattern for full control.
func Example_withAgento11yDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := geminiAPIKey()
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
	model, contents, config := exampleGeminiRequest()

	ctx, rec := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-gemini-2",
		AgentName:      "assistant-gemini",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "gemini", Name: model},
	})
	defer rec.End()

	providerClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		rec.SetCallError(err)
		return
	}

	resp, err := providerClient.Models.GenerateContent(ctx, model, contents, config)
	if err != nil {
		rec.SetCallError(err)
		return
	}

	rec.SetResult(FromRequestResponse(model, contents, config, resp,
		WithConversationID("conv-gemini-2"),
		WithAgentName("assistant-gemini"),
		WithAgentVersion("1.0.0"),
	))
	_ = resp.Candidates[0].Content.Parts[0].Text
}

// Example_withAgento11yStreamingDefer shows the defer pattern for streaming with per-response processing.
func Example_withAgento11yStreamingDefer() {
	if os.Getenv("SIGIL_RUN_LIVE_EXAMPLES") != "1" {
		return
	}

	apiKey := geminiAPIKey()
	if apiKey == "" {
		return
	}

	client := agento11y.NewClient(agento11y.DefaultConfig())
	model, contents, config := exampleGeminiRequest()

	ctx, rec := client.StartStreamingGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-gemini-3",
		AgentName:      "assistant-gemini",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "gemini", Name: model},
	})
	defer rec.End()

	providerClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		rec.SetCallError(err)
		return
	}

	summary := StreamSummary{}
	for response, err := range providerClient.Models.GenerateContentStream(ctx, model, contents, config) {
		if err != nil {
			rec.SetCallError(err)
			return
		}
		if response != nil {
			summary.Responses = append(summary.Responses, response)
			// Process each response here (e.g., SSE forwarding).
		}
	}

	rec.SetResult(FromStream(model, contents, config, summary,
		WithConversationID("conv-gemini-3"),
		WithAgentName("assistant-gemini"),
		WithAgentVersion("1.0.0"),
	))
}

func exampleGeminiRequest() (string, []*genai.Content, *genai.GenerateContentConfig) {
	return "gemini-2.5-pro", []*genai.Content{
		genai.NewContentFromText("Hello", genai.RoleUser),
	}, nil
}

func geminiAPIKey() string {
	if key := os.Getenv("GOOGLE_API_KEY"); key != "" {
		return key
	}
	return os.Getenv("GEMINI_API_KEY")
}
