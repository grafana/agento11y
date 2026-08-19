package otelhook_test

import (
	"context"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/grafana/agento11y/go/agento11y/contentcapture"
	"github.com/grafana/agento11y/go/agento11y/model"
	"github.com/grafana/agento11y/go/agento11y/otelhook"
	"github.com/grafana/agento11y/go/otelgenai"
)

// TestSpanContentKeysAreClassified drives a full span through otelgenai and the
// hook and fails if an attribute carrying the sentinel is not one
// contentcapture.IsTraceContentAttribute reports. A reducing forwarder deletes
// content by key, so a content field otelgenai grows without a key here leaves
// that content relayed under a reduced capture mode.
func TestSpanContentKeysAreClassified(t *testing.T) {
	const sentinel = "CONTENT-SENTINEL"

	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })

	handler := otelgenai.NewHandler(
		otelgenai.WithTracerProvider(provider),
		otelgenai.WithCaptureMode(otelgenai.CaptureSpanOnly),
		otelgenai.WithEndHook(otelhook.New()),
	)

	inv := &otelgenai.Invocation{
		Provider:           "openai",
		RequestModel:       "gpt-5",
		SystemInstructions: otelgenai.SystemInstructionsFromText(sentinel),
		InputMessages: []otelgenai.Message{{
			Role:  otelgenai.RoleUser,
			Parts: []otelgenai.Part{otelgenai.TextPart(sentinel)},
		}},
		OutputMessages: []otelgenai.Message{{
			Role:  otelgenai.RoleAssistant,
			Parts: []otelgenai.Part{otelgenai.TextPart(sentinel)},
		}},
		ToolDefinitions:    []otelgenai.ToolDefinition{{Name: sentinel, Description: sentinel}},
		ToolCallArguments:  []byte(`{"city":"` + sentinel + `"}`),
		ToolCallResult:     []byte(`{"city":"` + sentinel + `"}`),
		RetrievalQueryText: sentinel,
		RetrievalDocuments: []byte(`[{"id":"` + sentinel + `"}]`),
		Vendor: otelhook.Generation{
			ID:                "gen-1",
			ConversationTitle: sentinel,
			Artifacts:         []otelhook.Artifact{{Kind: model.ArtifactKindRequest, Payload: []byte(sentinel)}},
		},
	}

	ctx := handler.Start(context.Background(), inv)
	handler.End(ctx, inv)

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("recorded %d spans, want 1", len(ended))
	}

	var carriers int
	for _, kv := range ended[0].Attributes() {
		if !strings.Contains(kv.Value.Emit(), sentinel) {
			continue
		}
		carriers++
		if !contentcapture.IsTraceContentAttribute(string(kv.Key)) {
			t.Errorf("%s carries content but IsTraceContentAttribute reports it as metadata", kv.Key)
		}
	}
	// The raw artifacts hold the sentinel base64-encoded, so they never match
	// above. Every other content field does, and a drop to zero would mean the
	// span stopped carrying content rather than that the keys are classified.
	if carriers == 0 {
		t.Fatal("no attribute carries the sentinel, so this test checks nothing")
	}
}
