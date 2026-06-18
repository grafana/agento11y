package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/copilot/fragment"
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

func TestMapResolvesGitBranchFromCwd(t *testing.T) {
	// Fragment cwd points at a temp dir containing a `.git/HEAD` symbolic
	// ref, so the gitbranch resolver finds the branch without shelling
	// out. The second case verifies the session.Cwd fallback when the
	// fragment cwd is empty (copilot's normal resolution).
	cases := []struct {
		name               string
		headRaw            string
		useSessionFallback bool // place root in Session.Cwd instead of Fragment.Cwd
		wantBr             string
	}{
		{name: "frag.Cwd direct", headRaw: "ref: refs/heads/feature/copilot\n", wantBr: "feature/copilot"},
		{name: "session.Cwd fallback", headRaw: "ref: refs/heads/sess-branch\n", useSessionFallback: true, wantBr: "sess-branch"},
		{name: "detached HEAD", headRaw: "abcdef0123456789abcdef0123456789abcdef01\n", wantBr: "abcdef012345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			gitHead := filepath.Join(root, ".git", "HEAD")
			if err := os.MkdirAll(filepath.Dir(gitHead), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
			if err := os.WriteFile(gitHead, []byte(tc.headRaw), 0o644); err != nil {
				t.Fatalf("write head: %v", err)
			}

			frag := basicFragment()
			frag.Model = "gpt-5.4"
			var session *fragment.Session
			if tc.useSessionFallback {
				frag.Cwd = ""
				session = &fragment.Session{Cwd: root}
			} else {
				frag.Cwd = root
			}
			got := Map(Inputs{
				Fragment:       frag,
				Session:        session,
				ContentCapture: sigil.ContentCaptureModeMetadataOnly,
				Now:            fixedTime,
			})
			if got.Generation.Tags["git.branch"] != tc.wantBr {
				t.Fatalf("git.branch = %q, want %q (tags=%+v)", got.Generation.Tags["git.branch"], tc.wantBr, got.Generation.Tags)
			}
			if got.Generation.Tags["cwd"] != root {
				t.Fatalf("cwd = %q, want %q", got.Generation.Tags["cwd"], root)
			}
		})
	}
}

func TestMapOmitsGitBranchWhenNoCheckout(t *testing.T) {
	root := t.TempDir()
	frag := basicFragment()
	frag.Cwd = root
	frag.Model = "gpt-5.4"
	got := Map(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if _, ok := got.Generation.Tags["git.branch"]; ok {
		t.Fatalf("git.branch should be absent when no .git found; got %+v", got.Generation.Tags)
	}
	if got.Generation.Tags["cwd"] != root {
		t.Fatalf("cwd should still be present; got %q", got.Generation.Tags["cwd"])
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
