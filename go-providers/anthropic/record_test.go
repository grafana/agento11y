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

func TestMessageStreamUsesNativeAccumulatorOwnership(t *testing.T) {
	client := newProviderTestClient(t)
	stream := &testBetaMessageEventStream{events: hostedToolStreamEvents(t)}

	response, summary, err := messageStream(
		context.Background(),
		client,
		testRequest(),
		func(context.Context, asdk.BetaMessageNewParams) betaMessageEventStream { return stream },
	)
	if err != nil {
		t.Fatalf("message stream: %v", err)
	}
	if response == nil || response != summary.FinalMessage {
		t.Fatalf("response %p and summary final message %p must share native accumulator ownership", response, summary.FinalMessage)
	}
	if response.ID != "msg_ticket_stream" || response.StopReason != asdk.BetaStopReasonEndTurn {
		t.Fatalf("unexpected accumulated identity: %+v", response)
	}
	if len(response.Content) != 3 || response.Content[2].Text != "It is sunny." {
		t.Fatalf("unexpected accumulated content: %+v", response.Content)
	}
	if response.Usage.InputTokens != 20 || response.Usage.CacheReadInputTokens != 1000 ||
		response.Usage.CacheCreationInputTokens != 200 || response.Usage.OutputTokens != 40 {
		t.Fatalf("unexpected accumulated usage: %+v", response.Usage)
	}
	if !stream.closed {
		t.Fatal("stream was not closed")
	}
}

func TestMessageStreamPreservesProviderErrorAndPartialAccumulator(t *testing.T) {
	client := newProviderTestClient(t)
	providerErr := errors.New("stream interrupted")
	events := hostedToolStreamEvents(t)[:8]
	stream := &testBetaMessageEventStream{events: events, err: providerErr}

	response, summary, err := messageStream(
		context.Background(),
		client,
		testRequest(),
		func(context.Context, asdk.BetaMessageNewParams) betaMessageEventStream { return stream },
	)
	if !errors.Is(err, providerErr) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if response != nil {
		t.Fatalf("provider error returned response %+v", response)
	}
	if summary.FinalMessage == nil || summary.FinalMessage.ID != "msg_ticket_stream" {
		t.Fatalf("partial native accumulator lost identity: %+v", summary.FinalMessage)
	}
	if len(summary.FinalMessage.Content) != 3 || summary.FinalMessage.Content[2].Text != "It is sunny." {
		t.Fatalf("partial native accumulator lost content: %+v", summary.FinalMessage.Content)
	}
	if summary.FinalMessage.Usage.InputTokens != 20 || summary.FinalMessage.Usage.OutputTokens != 0 {
		t.Fatalf("partial native accumulator lost known usage: %+v", summary.FinalMessage.Usage)
	}
}

type testBetaMessageEventStream struct {
	events []asdk.BetaRawMessageStreamEventUnion
	err    error
	index  int
	closed bool
}

func (s *testBetaMessageEventStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++
	return true
}

func (s *testBetaMessageEventStream) Current() asdk.BetaRawMessageStreamEventUnion {
	return s.events[s.index-1]
}

func (s *testBetaMessageEventStream) Err() error { return s.err }

func (s *testBetaMessageEventStream) Close() error {
	s.closed = true
	return nil
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
