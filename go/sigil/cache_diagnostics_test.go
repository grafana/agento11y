package sigil

import (
	"context"
	"testing"
)

func TestSetCacheDiagnostics_NilRecorder(t *testing.T) {
	SetCacheDiagnostics(nil, "system_changed")
}

func TestSetCacheDiagnostics_EmptyReason(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "anthropic", Name: "claude-3-5-sonnet-latest"},
	})
	SetCacheDiagnostics(rec, "   ")
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("ok")}}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("End: %v", err)
	}
	if rec.lastGeneration.Metadata[CacheDiagnosticsMissReasonKey] != nil {
		t.Fatalf("expected no miss_reason in metadata, got %v", rec.lastGeneration.Metadata[CacheDiagnosticsMissReasonKey])
	}
}

func TestSetCacheDiagnostics_StampsMetadata(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "anthropic", Name: "claude-3-5-sonnet-latest"},
	})
	SetCacheDiagnostics(rec, "tools_changed",
		WithMissedInputTokens(42),
		WithPreviousMessageID("msg_prev"),
	)
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("ok")}}, nil)
	rec.End()
	if err := rec.Err(); err != nil {
		t.Fatalf("End: %v", err)
	}
	md := rec.lastGeneration.Metadata
	if got, _ := md[CacheDiagnosticsMissReasonKey].(string); got != "tools_changed" {
		t.Fatalf("miss_reason: want tools_changed, got %v", md[CacheDiagnosticsMissReasonKey])
	}
	if got, _ := md[CacheDiagnosticsMissedInputTokensKey].(string); got != "42" {
		t.Fatalf("missed_input_tokens: want 42, got %v", md[CacheDiagnosticsMissedInputTokensKey])
	}
	if got, _ := md[CacheDiagnosticsPreviousMessageIDKey].(string); got != "msg_prev" {
		t.Fatalf("previous_message_id: want msg_prev, got %v", md[CacheDiagnosticsPreviousMessageIDKey])
	}
}

func TestSetCacheDiagnostics_OverridesPreviousCall(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "anthropic", Name: "claude-3-5-sonnet-latest"},
	})
	SetCacheDiagnostics(rec, "system_changed", WithMissedInputTokens(1))
	SetCacheDiagnostics(rec, "model_changed", WithMissedInputTokens(99))
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("ok")}}, nil)
	rec.End()
	md := rec.lastGeneration.Metadata
	if got, _ := md[CacheDiagnosticsMissReasonKey].(string); got != "model_changed" {
		t.Fatalf("miss_reason: want model_changed, got %v", md[CacheDiagnosticsMissReasonKey])
	}
	if got, _ := md[CacheDiagnosticsMissedInputTokensKey].(string); got != "99" {
		t.Fatalf("missed tokens: want 99, got %v", md[CacheDiagnosticsMissedInputTokensKey])
	}
}

func TestSetCacheDiagnostics_AfterEndNoop(t *testing.T) {
	client, _, _ := newTestClient(t, Config{})
	_, rec := client.StartGeneration(context.Background(), GenerationStart{
		Model: ModelRef{Provider: "anthropic", Name: "claude-3-5-sonnet-latest"},
	})
	rec.SetResult(Generation{Output: []Message{AssistantTextMessage("ok")}}, nil)
	rec.End()

	SetCacheDiagnostics(rec, "system_changed", WithMissedInputTokens(42))

	if rec.lastGeneration.Metadata[CacheDiagnosticsMissReasonKey] != nil {
		t.Fatalf("expected no miss_reason in metadata after End, got %v", rec.lastGeneration.Metadata[CacheDiagnosticsMissReasonKey])
	}
}
