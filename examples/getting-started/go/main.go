// Minimal Agent Observability getting-started example — Go + OpenAI.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/joho/godotenv"
	openai "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
	"go.opentelemetry.io/contrib/exporters/autoexport"
	"go.opentelemetry.io/otel"
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
		semconv.ServiceName("getting-started-go"),
	))
	if err != nil {
		log.Fatalf("resource: %v", err)
	}

	traceExp, err := autoexport.NewSpanExporter(ctx)
	if err != nil {
		log.Fatalf("trace exporter: %v", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(traceExp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	defer func() { _ = tp.Shutdown(ctx) }()

	metricExp, err := autoexport.NewMetricReader(ctx)
	if err != nil {
		log.Fatalf("metric reader: %v", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metricExp), sdkmetric.WithResource(res))
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
	// Client tags attach to every generation and become agento11y.tag.<key>
	// attributes on OTel spans and metrics, so keep them low-cardinality
	// (team, env). See docs/concepts/tags-and-metadata.md.
	cfg.Tags = map[string]string{"team": "checkout", "env": "dev"}
	agento11yClient := agento11y.NewClient(cfg)
	defer func() { _ = agento11yClient.Shutdown(ctx) }()

	openaiClient := openai.NewClient(option.WithAPIKey(os.Getenv("OPENAI_API_KEY")))

	prompt := "Explain what LLM observability is in two sentences."

	completion, err := openaiClient.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model: shared.ChatModel(model),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage("You are a helpful assistant."),
			openai.UserMessage(prompt),
		},
	})
	if err != nil {
		log.Fatalf("OpenAI error: %v", err)
	}

	responseText := completion.Choices[0].Message.Content
	fmt.Printf("Response: %s\n\n", responseText)

	ctx, rec := agento11yClient.StartGeneration(ctx, agento11y.GenerationStart{
		ConversationID: "getting-started-go",
		AgentName:      "getting-started",
		AgentVersion:   "1.0.0",
		Model:          agento11y.ModelRef{Provider: "openai", Name: model},
		// user_id sets the user.id span attribute (all SDKs); use it for
		// end-user identity instead of a high-cardinality tag.
		UserID: "demo-user",
		// Per-generation tags and metadata are export-only: searchable on the
		// generation in Agent Observability, never emitted on spans or metrics.
		Tags:     map[string]string{"feature": "summarize"},
		Metadata: map[string]any{"prompt_version": "v2"},
	})
	defer rec.End()
	_ = ctx

	rec.SetResult(agento11y.Generation{
		Input:         []agento11y.Message{agento11y.UserTextMessage(prompt)},
		Output:        []agento11y.Message{agento11y.AssistantTextMessage(responseText)},
		ResponseID:    completion.ID,
		ResponseModel: completion.Model,
		StopReason:    string(completion.Choices[0].FinishReason),
		Usage: agento11y.TokenUsage{
			InputTokens:  completion.Usage.PromptTokens,
			OutputTokens: completion.Usage.CompletionTokens,
		},
	}, nil)

	fmt.Println("Done — check the Agent Observability plugin in your Grafana Cloud stack.")
}
