// Guarded AI Observability getting-started example - Go + OpenAI.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"strings"

	"github.com/grafana/agento11y/go/sigil"
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

	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolHTTP
	cfg.GenerationExport.Endpoint = os.Getenv("AGENTO11Y_ENDPOINT")
	cfg.GenerationExport.Auth = sigil.AuthConfig{
		Mode:          sigil.ExportAuthModeBasic,
		TenantID:      os.Getenv("AGENTO11Y_AUTH_TENANT_ID"),
		BasicPassword: os.Getenv("AGENTO11Y_AUTH_TOKEN"),
	}
	cfg.API.Endpoint = sigilAPIEndpoint()
	cfg.Hooks.Enabled = true
	cfg.Hooks.Phases = []sigil.HookPhase{sigil.HookPhasePreflight}

	sigilClient := sigil.NewClient(cfg)
	defer func() { _ = sigilClient.Shutdown(ctx) }()

	openaiClient := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	systemPrompt := "You are a helpful assistant. Keep answers concise."
	prompt := "My name is Jane Doe and my email is jane@example.com. Explain LLM guardrails in one sentence."
	inputMessages := []sigil.Message{sigil.UserTextMessage(prompt)}

	hookResponse, err := sigilClient.EvaluateHook(ctx, sigil.HookEvaluateRequest{
		Phase: sigil.HookPhasePreflight,
		Context: sigil.HookContext{
			AgentName:    "getting-started-hooks",
			AgentVersion: "1.0.0",
			Model:        &sigil.HookModel{Provider: "openai", Name: model},
		},
		Input: sigil.HookInput{
			Messages:            inputMessages,
			SystemPrompt:        systemPrompt,
			ConversationPreview: prompt,
		},
	})
	if err != nil {
		log.Fatalf("Sigil hook error: %v", err)
	}
	if err := sigil.HookDeniedFromResponse(hookResponse); err != nil {
		var denied *sigil.HookDeniedError
		if errors.As(err, &denied) {
			log.Printf("Blocked by Sigil guard rule %s: %s", valueOrUnknown(denied.RuleID), denied.Reason)
			return
		}
		log.Fatalf("Sigil hook denied: %v", err)
	}

	if hookResponse.TransformedInput != nil {
		if len(hookResponse.TransformedInput.Messages) > 0 {
			inputMessages = hookResponse.TransformedInput.Messages
		}
		if hookResponse.TransformedInput.SystemPrompt != "" {
			systemPrompt = hookResponse.TransformedInput.SystemPrompt
		}
		log.Println("Sigil hook allowed the call with transformed input.")
	} else {
		log.Println("Sigil hook allowed the call.")
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

	ctx, rec := sigilClient.StartGeneration(ctx, sigil.GenerationStart{
		ConversationID: "getting-started-go-hooks",
		AgentName:      "getting-started-hooks",
		AgentVersion:   "1.0.0",
		Model:          sigil.ModelRef{Provider: "openai", Name: model},
		SystemPrompt:   systemPrompt,
	})
	defer rec.End()
	_ = ctx

	rec.SetResult(sigil.Generation{
		Input:         inputMessages,
		Output:        []sigil.Message{sigil.AssistantTextMessage(responseText)},
		ResponseID:    completion.ID,
		ResponseModel: completion.Model,
		StopReason:    completion.Choices[0].FinishReason,
		Usage: sigil.TokenUsage{
			InputTokens:  completion.Usage.PromptTokens,
			OutputTokens: completion.Usage.CompletionTokens,
		},
	}, nil)

	log.Println("Done - check the AI Observability plugin in your Grafana Cloud stack.")
}

func sigilAPIEndpoint() string {
	parsed, err := url.Parse(os.Getenv("AGENTO11Y_ENDPOINT"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return os.Getenv("AGENTO11Y_ENDPOINT")
	}
	return parsed.Scheme + "://" + parsed.Host
}

func openAIMessages(systemPrompt string, messages []sigil.Message) []openai.ChatCompletionMessageParamUnion {
	out := []openai.ChatCompletionMessageParamUnion{openai.SystemMessage(systemPrompt)}
	for _, msg := range messages {
		text := messageText(msg)
		switch msg.Role {
		case sigil.RoleAssistant:
			out = append(out, openai.AssistantMessage(text))
		default:
			out = append(out, openai.UserMessage(text))
		}
	}
	return out
}

func messageText(msg sigil.Message) string {
	parts := make([]string, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if part.Kind == sigil.PartKindText {
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
