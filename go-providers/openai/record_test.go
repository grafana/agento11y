package openai

import (
	"context"
	"errors"
	"strings"
	"testing"

	osdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	oresponses "github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"github.com/grafana/agento11y/go/sigil"
)

func TestEmbeddingsNewReturnsRecorderValidationErrorAfterEnd(t *testing.T) {
	client := newProviderTestClient(t)

	req := osdk.EmbeddingNewParams{
		Model: osdk.EmbeddingModel("text-embedding-3-small"),
	}
	expectedResponse := &osdk.CreateEmbeddingResponse{
		Model: "text-embedding-3-small",
		Data: []osdk.Embedding{
			{Embedding: []float64{0.1, 0.2}},
		},
		Usage: osdk.CreateEmbeddingResponseUsage{
			PromptTokens: 2,
			TotalTokens:  2,
		},
	}

	response, err := embeddingsNew(
		context.Background(),
		client,
		req,
		func(context.Context, osdk.EmbeddingNewParams) (*osdk.CreateEmbeddingResponse, error) {
			return expectedResponse, nil
		},
		WithProviderName(""),
	)
	if err == nil {
		t.Fatalf("expected recorder validation error")
	}
	if !strings.Contains(err.Error(), "embedding.model.provider is required") {
		t.Fatalf("expected embedding model provider validation error, got %v", err)
	}
	if response != expectedResponse {
		t.Fatalf("expected wrapper to return provider response pointer")
	}
}

func TestEmbeddingsNewPreservesProviderErrors(t *testing.T) {
	client := newProviderTestClient(t)

	req := osdk.EmbeddingNewParams{
		Model: osdk.EmbeddingModel("text-embedding-3-small"),
	}
	providerErr := errors.New("provider failed")

	response, err := embeddingsNew(
		context.Background(),
		client,
		req,
		func(context.Context, osdk.EmbeddingNewParams) (*osdk.CreateEmbeddingResponse, error) {
			return nil, providerErr
		},
		WithProviderName(""),
	)
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if response != nil {
		t.Fatalf("expected nil response on provider error")
	}
}

func TestConformance_ChatCompletionsNewErrorMapping(t *testing.T) {
	client := newProviderTestClient(t)
	req := osdk.ChatCompletionNewParams{
		Model: shared.ChatModel("gpt-4o-mini"),
		Messages: []osdk.ChatCompletionMessageParamUnion{
			osdk.UserMessage("hello"),
		},
	}

	t.Run("provider errors are preserved", func(t *testing.T) {
		providerErr := errors.New("provider failed")

		response, err := chatCompletionsNew(
			context.Background(),
			client,
			req,
			func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
				return nil, providerErr
			},
		)
		if !errors.Is(err, providerErr) {
			t.Fatalf("expected provider error, got %v", err)
		}
		if response != nil {
			t.Fatalf("expected nil response on provider error")
		}
	})

	t.Run("mapping failures do not hide provider responses", func(t *testing.T) {
		expectedResponse := &osdk.ChatCompletion{
			Model: "gpt-4o-mini",
			Choices: []osdk.ChatCompletionChoice{
				{
					FinishReason: "stop",
					Message: osdk.ChatCompletionMessage{
						Content: "hi",
					},
				},
			},
		}

		response, err := chatCompletionsNew(
			context.Background(),
			client,
			req,
			func(context.Context, osdk.ChatCompletionNewParams) (*osdk.ChatCompletion, error) {
				return expectedResponse, nil
			},
			WithProviderName(""),
		)
		if err != nil {
			t.Fatalf("expected nil local error for mapping failure, got %v", err)
		}
		if response != expectedResponse {
			t.Fatalf("expected wrapper to return provider response pointer")
		}
	})
}

func TestConformance_ResponsesNewErrorMapping(t *testing.T) {
	client := newProviderTestClient(t)
	req := oresponses.ResponseNewParams{
		Model: shared.ResponsesModel("gpt-5"),
		Input: oresponses.ResponseNewParamsInputUnion{OfString: param.NewOpt("hello")},
	}

	t.Run("provider errors are preserved", func(t *testing.T) {
		providerErr := errors.New("provider failed")

		response, err := responsesNew(
			context.Background(),
			client,
			req,
			func(context.Context, oresponses.ResponseNewParams) (*oresponses.Response, error) {
				return nil, providerErr
			},
		)
		if !errors.Is(err, providerErr) {
			t.Fatalf("expected provider error, got %v", err)
		}
		if response != nil {
			t.Fatalf("expected nil response on provider error")
		}
	})

	t.Run("mapping failures do not hide provider responses", func(t *testing.T) {
		expectedResponse := &oresponses.Response{
			Model:  shared.ResponsesModel("gpt-5"),
			Status: oresponses.ResponseStatusCompleted,
			Output: []oresponses.ResponseOutputItemUnion{
				{
					Type: "message",
					Content: []oresponses.ResponseOutputMessageContentUnion{
						{Type: "output_text", Text: "hi"},
					},
				},
			},
		}

		response, err := responsesNew(
			context.Background(),
			client,
			req,
			func(context.Context, oresponses.ResponseNewParams) (*oresponses.Response, error) {
				return expectedResponse, nil
			},
			WithProviderName(""),
		)
		if err != nil {
			t.Fatalf("expected nil local error for mapping failure, got %v", err)
		}
		if response != expectedResponse {
			t.Fatalf("expected wrapper to return provider response pointer")
		}
	})
}

func newProviderTestClient(t *testing.T) *sigil.Client {
	t.Helper()

	cfg := sigil.DefaultConfig()
	cfg.GenerationExport.Protocol = sigil.GenerationExportProtocolNone

	client := sigil.NewClient(cfg)
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown sigil client: %v", err)
		}
	})
	return client
}
