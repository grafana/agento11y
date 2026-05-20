package mapper

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/copilot/internal/fragment"
)

var fixedTime = time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)

func basicFragment() *fragment.Fragment {
	return &fragment.Fragment{
		SessionID:     "sess-1",
		TurnID:        "turn-000001",
		Prompt:        "my token is glc_abcdefghijklmnopqrstuvwxyz",
		InitialPrompt: "fallback",
		Tools: []fragment.ToolRecord{
			{ToolName: "bash", ToolUseID: "tool-1", ToolInput: json.RawMessage(`{"cmd":"echo hi"}`), ToolResponse: json.RawMessage(`{"text_result_for_llm":"ok"}`), Status: "completed"},
		},
		StartedAt:   "2026-05-18T11:59:00Z",
		CompletedAt: "2026-05-18T12:00:00Z",
	}
}

func TestMapFullModeIncludesRedactedPromptAndToolContent(t *testing.T) {
	frag := basicFragment()
	frag.Model = "gpt-5.4"
	frag.AgentVersion = "1.0.48"
	frag.MessageID = "msg-1"
	frag.RequestID = "req-1"
	frag.InteractionID = "int-1"
	frag.NativeTurnID = "4"
	frag.ReasoningEffort = "medium"
	frag.AssistantText = "assistant secret glc_abcdefghijklmnopqrstuvwxyz"
	outTokens := int64(12)
	frag.TokenUsage.OutputTokens = &outTokens
	got := Map(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeFull,
		Now:            fixedTime,
	})
	if len(got.Generation.Input) == 0 {
		t.Fatal("expected input messages")
	}
	userText := got.Generation.Input[0].Parts[0].Text
	if userText == "" || userText == "my token is glc_abcdefghijklmnopqrstuvwxyz" {
		t.Fatalf("user text not redacted: %q", userText)
	}
	if len(got.Generation.Output) == 0 || got.Generation.Output[0].Parts[0].ToolCall == nil {
		t.Fatalf("expected tool call output: %+v", got.Generation.Output)
	}
	if len(got.Generation.Output[0].Parts[0].ToolCall.InputJSON) == 0 {
		t.Fatal("expected tool input in full mode")
	}
	if got.Generation.Model.Provider != "openai" {
		t.Fatalf("Model.Provider = %q", got.Generation.Model.Provider)
	}
	if got.Generation.ResponseModel != "gpt-5.4" {
		t.Fatalf("ResponseModel = %q", got.Generation.ResponseModel)
	}
	if got.Generation.ResponseID != "req-1" {
		t.Fatalf("ResponseID = %q", got.Generation.ResponseID)
	}
	if got.Generation.AgentVersion != "1.0.48" {
		t.Fatalf("AgentVersion = %q", got.Generation.AgentVersion)
	}
	if got.Generation.Usage.OutputTokens != 12 || got.Generation.Usage.TotalTokens != 12 {
		t.Fatalf("Usage = %+v", got.Generation.Usage)
	}
	last := got.Generation.Output[len(got.Generation.Output)-1]
	if len(last.Parts) == 0 || last.Parts[0].Text == "" || last.Parts[0].Text == frag.AssistantText {
		t.Fatalf("assistant text missing or unredacted: %+v", got.Generation.Output)
	}
	if got.Generation.Metadata["copilot.native_turn_id"] != "4" {
		t.Fatalf("native turn id metadata missing: %+v", got.Generation.Metadata)
	}
	if got.Generation.Metadata["copilot.request_id"] != "req-1" {
		t.Fatalf("request id metadata missing: %+v", got.Generation.Metadata)
	}
}

func TestMapMetadataOnlyStripsPromptAndToolResultContent(t *testing.T) {
	got := Map(Inputs{
		Fragment:       basicFragment(),
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	for _, msg := range got.Generation.Input {
		for _, part := range msg.Parts {
			if part.Text != "" {
				t.Fatalf("prompt leaked in metadata_only: %+v", got.Generation.Input)
			}
			if part.ToolResult != nil && (part.ToolResult.Content != "" || len(part.ToolResult.ContentJSON) > 0) {
				t.Fatalf("tool result leaked in metadata_only: %+v", got.Generation.Input)
			}
		}
	}
}

func TestMapErrorPromotesCallError(t *testing.T) {
	frag := basicFragment()
	frag.Errors = []fragment.ErrorRecord{{Context: "model_call", Name: "RateLimit", Message: "429 too many requests"}}
	got := Map(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.CallError == nil {
		t.Fatal("expected call error")
	}
	if got.Generation.StopReason != "error" {
		t.Fatalf("StopReason = %q, want error", got.Generation.StopReason)
	}
	if got.Generation.Metadata["copilot.assistant_text_available"] != false {
		t.Fatalf("assistant_text_available metadata missing: %+v", got.Generation.Metadata)
	}
}

func TestMapUsesHookStopReasonWhenSuccessful(t *testing.T) {
	frag := basicFragment()
	frag.StopReason = "end_turn"
	got := Map(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Generation.StopReason != "end_turn" {
		t.Fatalf("StopReason = %q", got.Generation.StopReason)
	}
}

func TestMapDoesNotSetConversationTitle(t *testing.T) {
	got := Map(Inputs{
		Fragment:       basicFragment(),
		ContentCapture: sigil.ContentCaptureModeFull,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "" {
		t.Fatalf("Start.ConversationTitle = %q", got.Start.ConversationTitle)
	}
	if got.Generation.ConversationTitle != "" {
		t.Fatalf("Generation.ConversationTitle = %q", got.Generation.ConversationTitle)
	}
}
