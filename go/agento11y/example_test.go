package agento11y_test

import (
	"context"
	"os/exec"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func ExampleClient_StartGeneration() {
	client := agento11y.NewClient(agento11y.DefaultConfig())

	ctx, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-9b2f",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
	})
	defer recorder.End()

	// Use ctx for the provider request so the request is inside the generation span.
	_ = ctx

	// Keep the provider response in normal local scope.
	responseText := "Hi!"

	recorder.SetResult(agento11y.Generation{
		Input:  []agento11y.Message{agento11y.UserTextMessage("Hello")},
		Output: []agento11y.Message{agento11y.AssistantTextMessage(responseText)},
		Usage:  agento11y.TokenUsage{InputTokens: 120, OutputTokens: 42},
	}, nil)
}

func ExampleClient_StartStreamingGeneration() {
	client := agento11y.NewClient(agento11y.DefaultConfig())

	ctx, recorder := client.StartStreamingGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-stream",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
	})
	defer recorder.End()

	_ = ctx

	chunks := []string{"Hel", "lo", " ", "world"}
	assistantText := strings.Join(chunks, "")

	recorder.SetResult(agento11y.Generation{
		Input:  []agento11y.Message{agento11y.UserTextMessage("Say hello")},
		Output: []agento11y.Message{agento11y.AssistantTextMessage(assistantText)},
	}, nil)
}

func ExampleClient_StartToolExecution() {
	client := agento11y.NewClient(agento11y.DefaultConfig())

	ctx, recorder := client.StartToolExecution(context.Background(), agento11y.ToolExecutionStart{
		ToolName:        "weather",
		ToolCallID:      "call_weather",
		ToolType:        "function",
		ToolDescription: "Get weather for a city",
		ConversationID:  "conv-tools",
		AgentName:       "assistant-core",
		AgentVersion:    "1.0.0",
		ContentCapture:  agento11y.ContentCaptureModeFull,
	})
	defer recorder.End()

	_ = ctx
	result := map[string]any{"temp_c": 18}

	recorder.SetResult(agento11y.ToolExecutionEnd{
		Arguments: map[string]any{"city": "Paris"},
		Result:    result,
	})
}

func ExampleClient_StartToolExecution_command() {
	// Disable export here; applications supply their configured OTel providers.
	client := agento11y.NewClient(agento11y.Config{
		GenerationExport: agento11y.GenerationExportConfig{Protocol: agento11y.GenerationExportProtocolNone},
		TracerProvider:   tracenoop.NewTracerProvider(),
		MeterProvider:    metricnoop.NewMeterProvider(),
	})
	defer func() { _ = client.Shutdown(context.Background()) }()

	ctx, recorder := client.StartToolExecution(context.Background(), agento11y.ToolExecutionStart{
		ToolName:              "run_skill_script",
		ExecutionKind:         agento11y.ToolExecutionKindCommand,
		SkillName:             "code-review",
		SkillResourceName:     "scripts/check.py",
		ProcessExecutableName: "python3",
	})
	defer recorder.End()

	command := exec.CommandContext(ctx, "python3", "scripts/check.py")
	// Report the runner's error independently of its exit code.
	recorder.SetExecError(command.Run())

	end := agento11y.ToolExecutionEnd{}
	if command.ProcessState != nil && command.ProcessState.Exited() {
		code := int64(command.ProcessState.ExitCode())
		end.ProcessExitCode = &code // Zero is reported success; nil means unknown.
	}
	recorder.SetResult(end)
}

func ExampleClient_StartGeneration_metadataOnly() {
	client := agento11y.NewClient(agento11y.Config{
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
	})

	ctx, recorder := client.StartGeneration(context.Background(), agento11y.GenerationStart{
		ConversationID: "conv-private",
		AgentName:      "assistant-core",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "anthropic", Name: "claude-sonnet-4-5"},
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
	})
	defer recorder.End()

	_ = ctx

	recorder.SetResult(agento11y.Generation{
		Input:  []agento11y.Message{agento11y.UserTextMessage("sensitive prompt")},
		Output: []agento11y.Message{agento11y.AssistantTextMessage("sensitive response")},
		Usage:  agento11y.TokenUsage{InputTokens: 120, OutputTokens: 42},
	}, nil)
	// Content is stripped before export; only metadata (usage, timing, model) is sent.
}
