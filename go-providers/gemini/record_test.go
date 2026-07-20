package gemini

import (
	"context"
	"errors"
	"strings"
	"testing"

	"google.golang.org/genai"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestEmbedContentReturnsRecorderValidationErrorAfterEnd(t *testing.T) {
	client := newProviderTestClient(t)

	contents := []*genai.Content{
		genai.NewContentFromText("hello", genai.RoleUser),
	}
	expectedResponse := &genai.EmbedContentResponse{
		Embeddings: []*genai.ContentEmbedding{
			{
				Values: []float32{0.1, 0.2},
				Statistics: &genai.ContentEmbeddingStatistics{
					TokenCount: 2,
				},
			},
		},
	}

	response, err := embedContent(
		context.Background(),
		client,
		"gemini-embedding-001",
		contents,
		nil,
		func(context.Context, string, []*genai.Content, *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
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

func TestEmbedContentPreservesProviderErrors(t *testing.T) {
	client := newProviderTestClient(t)

	providerErr := errors.New("provider failed")

	response, err := embedContent(
		context.Background(),
		client,
		"gemini-embedding-001",
		nil,
		nil,
		func(context.Context, string, []*genai.Content, *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
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

func TestConformance_GenerateContentErrorMapping(t *testing.T) {
	client := newProviderTestClient(t)
	model := "gemini-2.5-pro"
	contents := []*genai.Content{
		genai.NewContentFromText("hello", genai.RoleUser),
	}

	t.Run("provider errors are preserved", func(t *testing.T) {
		providerErr := errors.New("provider failed")

		response, err := generateContent(
			context.Background(),
			client,
			model,
			contents,
			nil,
			func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
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
		expectedResponse := &genai.GenerateContentResponse{
			Candidates: []*genai.Candidate{
				{
					FinishReason: genai.FinishReasonStop,
					Content:      genai.NewContentFromText("hi", genai.RoleModel),
				},
			},
		}

		response, err := generateContent(
			context.Background(),
			client,
			model,
			contents,
			nil,
			func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
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

func newProviderTestClient(t *testing.T) *agento11y.Client {
	t.Helper()

	cfg := agento11y.DefaultConfig()
	cfg.GenerationExport.Protocol = agento11y.GenerationExportProtocolNone

	client := agento11y.NewClient(cfg)
	t.Cleanup(func() {
		if err := client.Shutdown(context.Background()); err != nil {
			t.Errorf("shutdown sigil client: %v", err)
		}
	})
	return client
}
