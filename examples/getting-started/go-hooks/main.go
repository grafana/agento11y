// Guarded Agent Observability getting-started example - Go + OpenAI.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/joho/godotenv"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func main() {
	_ = godotenv.Load()

	ctx := context.Background()
	model := "gpt-4.1-mini"

	res, err := resource.New(ctx, resource.WithAttributes(
		semconv.ServiceName("getting-started-go-hooks"),
	))
	if err != nil {
		log.Fatalf("resource: %v", err)
	}

	traceExp, err := otlptracehttp.New(ctx)
	if err != nil {
		log.Fatalf("trace exporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(ctx) }()

	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		log.Fatalf("metric exporter: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	defer func() { _ = mp.Shutdown(ctx) }()

	cfg := agento11y.DefaultConfig()
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = os.Getenv("AGENTO11Y_ENDPOINT")
	cfg.GenerationExport.Auth = agento11y.AuthConfig{
		Mode:          agento11y.ExportAuthModeBasic,
		TenantID:      os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
		BasicPassword: os.Getenv("AGENTO11Y_AUTH_TOKEN"),
	}
	cfg.API.Endpoint = agento11yAPIEndpoint()
	cfg.Hooks.Enabled = true
	cfg.Hooks.Phases = []agento11y.HookPhase{agento11y.HookPhasePreflight}

	agento11yClient := agento11y.NewClient(cfg)
	defer func() { _ = agento11yClient.Shutdown(ctx) }()

	openaiClient := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	systemPrompt := "You are a helpful assistant. Keep answers concise."
	prompt := "My name is Jane Doe and my email is jane@example.com. Explain LLM guardrails in one sentence."
	inputMessages := []agento11y.Message{agento11y.UserTextMessage(prompt)}

	hookResponse, err := agento11yClient.EvaluateHook(ctx, agento11y.HookEvaluateRequest{
		Phase: agento11y.HookPhasePreflight,
		Context: agento11y.HookContext{
			AgentName:    "getting-started-hooks",
			AgentVersion: "1.0.0",
			Model:        &agento11y.HookModel{Provider: "openai", Name: model},
		},
		Input: agento11y.HookInput{
			Messages:            inputMessages,
			SystemPrompt:        systemPrompt,
			ConversationPreview: prompt,
		},
	})
	if err != nil {
		log.Fatalf("agento11y hook error: %v", err)
	}
	if err := agento11y.HookDeniedFromResponse(hookResponse); err != nil {
		var denied *agento11y.HookDeniedError
		if errors.As(err, &denied) {
			log.Printf("Blocked by guard rule %s: %s", valueOrUnknown(denied.RuleID), denied.Reason)
			return
		}
		log.Fatalf("agento11y hook denied: %v", err)
	}

	if hookResponse.TransformedInput != nil {
		if len(hookResponse.TransformedInput.Messages) > 0 {
			inputMessages = hookResponse.TransformedInput.Messages
		}
		if hookResponse.TransformedInput.SystemPrompt != "" {
			systemPrompt = hookResponse.TransformedInput.SystemPrompt
		}
		log.Println("agento11y hook allowed the call with transformed input.")
	} else {
		log.Println("agento11y hook allowed the call.")
	}

	completion, err := openaiClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:    model,
		Messages: openAIMessages(systemPrompt, inputMessages),
	})
	if err != nil {
		log.Fatalf("OpenAI error: %v", err)
	}

	responseText := completion.Choices[0].Message.Content
	log.Printf("Response: %s", responseText)

	ctx, rec := agento11yClient.StartGeneration(ctx, agento11y.GenerationStart{
		ConversationID: "getting-started-go-hooks",
		AgentName:      "getting-started-hooks",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "openai", Name: model},
		SystemPrompt:   systemPrompt,
	})
	defer rec.End()
	_ = ctx

	rec.SetResult(agento11y.Generation{
		Input:         inputMessages,
		Output:        []agento11y.Message{agento11y.AssistantTextMessage(responseText)},
		ResponseID:    completion.ID,
		ResponseModel: completion.Model,
		StopReason:    completion.Choices[0].FinishReason,
		Usage: agento11y.TokenUsage{
			InputTokens:  completion.Usage.PromptTokens,
			OutputTokens: completion.Usage.CompletionTokens,
		},
	}, nil)

	log.Println("Done - check the Agent Observability plugin in your Grafana Cloud stack.")
}

func agento11yAPIEndpoint() string {
	parsed, err := url.Parse(os.Getenv("AGENTO11Y_ENDPOINT"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return os.Getenv("AGENTO11Y_ENDPOINT")
	}
	return parsed.Scheme + "://" + parsed.Host
}

func openAIMessages(systemPrompt string, messages []agento11y.Message) []openai.ChatCompletionMessageParamUnion {
	out := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt)}
	for _, msg := range messages {
		text := messageText(msg)
		switch msg.Role {
		case agento11y.RoleAssistant:
			out = append(out, openai.AssistantMessage(text))
		default:
			out = append(out, openai.UserMessage(text))
		}
	}
	return out
}

func messageText(msg agento11y.Message) string {
	parts := make([]string, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Kind == agento11y.PartKindText {
			parts = append(parts, part.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func valueOrUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "<unknown>"
	}
	return value
}
