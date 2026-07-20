package sigil_test

import (
	"context"
	"strings"

	"github.com/grafana/agento11y/go/sigil"
)

func ExampleClient_StartGeneration() {
	client := sigil.NewClient(sigil.DefaultConfig())

	ctx, recorder := client.StartGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-9b2f",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
	})
	defer recorder.End()

	// Use ctx for the provider request so the request is inside the generation span.
	_ = ctx

	// Keep the provider response in normal local scope.
	responseText := "Hi!"

	recorder.SetResult(sigil.Generation{
		Input:  []sigil.Message{sigil.UserTextMessage("Hello")},
		Output: []sigil.Message{sigil.AssistantTextMessage(responseText)},
		Usage:  sigil.TokenUsage{InputTokens: 120, OutputTokens: 42},
	}, nil)
}

func ExampleClient_StartStreamingGeneration() {
	client := sigil.NewClient(sigil.DefaultConfig())

	ctx, recorder := client.StartStreamingGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-stream",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	defer recorder.End()

	_ = ctx

	chunks := []string{"Hel", "lo", " ", "world"}
	assistantText := strings.Join(chunks, "")

	recorder.SetResult(sigil.Generation{
		Input:  []sigil.Message{sigil.UserTextMessage("Say hello")},
		Output: []sigil.Message{sigil.AssistantTextMessage(assistantText)},
	}, nil)
}

func ExampleClient_StartToolExecution() {
	client := sigil.NewClient(sigil.DefaultConfig())

	ctx, recorder := client.StartToolExecution(context.Background(), sigil.ToolExecutionStart{
		ToolName:        "weather",
		ToolCallID:      "call_weather",
		ToolType:        "function",
		ToolDescription: "Get weather for a city",
		ConversationID:  "conv-tools",
		AgentName:       "assistant-core",
		AgentVersion:    "1.0.0",
		ContentCapture:  sigil.ContentCaptureModeFull,
	})
	defer recorder.End()

	_ = ctx
	result := map[string]any{"temp_c": 18}

	recorder.SetResult(sigil.ToolExecutionEnd{
		Arguments: map[string]any{"city": "Paris"},
		Result:    result,
	})
}

func ExampleClient_StartGeneration_metadataOnly() {
	client := sigil.NewClient(sigil.Config{
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
	})

	ctx, recorder := client.StartGeneration(context.Background(), sigil.GenerationStart{
		ConversationID: "conv-private",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
	})
	defer recorder.End()

	_ = ctx

	recorder.SetResult(sigil.Generation{
		Input:  []sigil.Message{sigil.UserTextMessage("sensitive prompt")},
		Output: []sigil.Message{sigil.AssistantTextMessage("sensitive response")},
		Usage:  sigil.TokenUsage{InputTokens: 120, OutputTokens: 42},
	}, nil)
	// Content is stripped before export; only metadata (usage, timing, model) is sent.
}
