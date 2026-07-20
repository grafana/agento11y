package anthropic

import (
	"context"
	"errors"
	"testing"

	asdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/grafana/agento11y/go/agento11y"
)

func TestConformance_MessageErrorMapping(t *testing.T) {
	client := newProviderTestClient(t)
	req := testRequest()

	t.Run("provider errors are preserved", func(t *testing.T) {
		providerErr := errors.New("provider failed")

		response, err := message(
			context.Background(),
			client,
			req,
			func(context.Context, asdk.BetaMessageNewParams) (*asdk.BetaMessage, error) {
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
		expectedResponse := &asdk.BetaMessage{
			Model:      asdk.Model("claude-sonnet-4-5"),
			StopReason: asdk.BetaStopReasonEndTurn,
			Content: []asdk.BetaContentBlockUnion{
				{Type: "text", Text: "hi"},
			},
		}

		response, err := message(
			context.Background(),
			client,
			req,
			func(context.Context, asdk.BetaMessageNewParams) (*asdk.BetaMessage, error) {
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
