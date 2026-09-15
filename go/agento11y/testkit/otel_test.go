package testkit_test

import (
	"context"
	"testing"

	"github.com/grafana/agento11y/go/agento11y"
	"github.com/grafana/agento11y/go/agento11y/testkit"
)

func TestNewOTelEnvRecordsGenerationEmbeddingAndToolSpans(t *testing.T) {
	env := testkit.NewOTelEnv(t)

	testkit.RecordGeneration(t, env, agento11y.GenerationStart{
		Model: agento11y.ModelRef{Provider: "openai", Name: "gpt-5"},
	}, agento11y.Generation{
		Input:  []agento11y.Message{agento11y.UserTextMessage("Hello")},
		Output: []agento11y.Message{agento11y.AssistantTextMessage("Hi!")},
	}, nil)

	testkit.RecordEmbedding(t, env, agento11y.EmbeddingStart{
		Model: agento11y.ModelRef{Provider: "openai", Name: "text-embedding-3-small"},
	}, agento11y.EmbeddingResult{InputCount: 1, InputTokens: 2})

	_, tool := env.Client.StartToolExecution(context.Background(), agento11y.ToolExecutionStart{
		ToolName: "weather",
	})
	tool.SetResult(agento11y.ToolExecutionEnd{
		Arguments: map[string]any{"city": "Paris"},
		Result:    map[string]any{"temperature": 18},
	})
	tool.End()
	if err := tool.Err(); err != nil {
		t.Fatalf("record tool execution: %v", err)
	}
	if err := env.Client.Flush(context.Background()); err != nil {
		t.Fatalf("flush OTel spans: %v", err)
	}

	spans := env.Spans.Ended()
	if got := len(spans); got != 3 {
		t.Fatalf("recorded %d spans, want 3", got)
	}
	for _, name := range []string{
		"chat gpt-5",
		"embeddings text-embedding-3-small",
		"execute_tool weather",
	} {
		testkit.FindSpan(t, spans, name)
	}
}
