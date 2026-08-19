package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/agento11y/go/agento11y"

	"github.com/grafana/agento11y/plugins/agento11y/internal/agents/copilot/fragment"
)

var fixedTime = time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)

// testSecret matches a tier-1 redaction pattern, so any field that carries it
// must come back scrubbed unless the test says otherwise.
const testSecret = "glc_abcdefghijklmnopqrstuvwxyz"

func basicFragment() *fragment.Fragment {
	return &fragment.Fragment{
		SessionID:     "sess-1",
		TurnID:        "turn-000001",
		Prompt:        "my token is " + testSecret,
		InitialPrompt: "fallback",
		Tools: []fragment.ToolRecord{
			{ToolName: "bash", ToolUseID: "tool-1", ToolInput: json.RawMessage(`{"cmd":"echo hi"}`), ToolResponse: json.RawMessage(`{"text_result_for_llm":"ok"}`), Status: "completed"},
		},
		StartedAt:   "2026-05-18T11:59:00Z",
		CompletedAt: "2026-05-18T12:00:00Z",
	}
}

func TestMapFullModeIncludesRedactedPromptAndToolContent(t *testing.T) {
	tests := []struct {
		name                string
		skipPromptRedaction bool
		wantPromptRedacted  bool
	}{
		{name: "redacts the prompt by default", wantPromptRedacted: true},
		{name: "opt-out exports the prompt unredacted", skipPromptRedaction: true, wantPromptRedacted: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frag := basicFragment()
			frag.Model = "gpt-5.4"
			frag.AgentVersion = "1.0.48"
			frag.MessageID = "msg-1"
			frag.RequestID = "req-1"
			frag.InteractionID = "int-1"
			frag.NativeTurnID = "4"
			frag.ReasoningEffort = "medium"
			frag.AssistantText = "assistant secret " + testSecret + " PASSWORD=hunter2"
			outTokens := int64(12)
			frag.TokenUsage.OutputTokens = &outTokens
			got := Map(Inputs{
				Fragment:            frag,
				ContentCapture:      agento11y.ContentCaptureModeFull,
				SkipPromptRedaction: tt.skipPromptRedaction,
				Now:                 fixedTime,
			})
			if len(got.Generation.Input) == 0 {
				t.Fatal("expected input messages")
			}
			userText := got.Generation.Input[0].Parts[0].Text
			if userText == "" {
				t.Fatal("expected user text in full mode")
			}
			if gotRedacted := !strings.Contains(userText, testSecret); gotRedacted != tt.wantPromptRedacted {
				t.Fatalf("prompt redacted = %v, want %v: %q", gotRedacted, tt.wantPromptRedacted, userText)
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
			// The opt-out covers the prompt only: assistant text stays redacted.
			last := got.Generation.Output[len(got.Generation.Output)-1]
			if len(last.Parts) == 0 || last.Parts[0].Text == "" || strings.Contains(last.Parts[0].Text, testSecret) {
				t.Fatalf("assistant text missing or unredacted: %+v", got.Generation.Output)
			}
			// Assistant prose gets tier 1 only, so an env-style pair in a
			// sentence survives. The prompt takes tier 2 as well.
			if !strings.Contains(last.Parts[0].Text, "PASSWORD=hunter2") {
				t.Fatalf("assistant text got tier 2: %q", last.Parts[0].Text)
			}
			if got.Generation.Metadata["copilot.native_turn_id"] != "4" {
				t.Fatalf("native turn id metadata missing: %+v", got.Generation.Metadata)
			}
			if got.Generation.Metadata["copilot.request_id"] != "req-1" {
				t.Fatalf("request id metadata missing: %+v", got.Generation.Metadata)
			}
		})
	}
}

func TestMapFullWithMetadataSpansPreservesStartModeAndFullPayload(t *testing.T) {
	frag := basicFragment()
	frag.Model = "gpt-5.4"
	frag.AssistantText = "done"

	got := Map(Inputs{
		Fragment:       frag,
		ContentCapture: agento11y.ContentCaptureModeFullWithMetadataSpans,
		Now:            fixedTime,
	})
	if got.Start.ContentCapture != agento11y.ContentCaptureModeFullWithMetadataSpans {
		t.Fatalf("Start.ContentCapture = %v; want FullWithMetadataSpans", got.Start.ContentCapture)
	}
	if len(got.Generation.Input) == 0 || got.Generation.Input[0].Parts[0].Text == "" {
		t.Fatalf("full_with_metadata_spans should keep full gRPC input payload: %+v", got.Generation.Input)
	}
	if len(got.Generation.Output) == 0 || got.Generation.Output[0].Parts[0].ToolCall == nil || len(got.Generation.Output[0].Parts[0].ToolCall.InputJSON) == 0 {
		t.Fatalf("full_with_metadata_spans should keep full gRPC tool payload: %+v", got.Generation.Output)
	}
}

func TestMapMetadataOnlyStripsPromptAndToolResultContent(t *testing.T) {
	got := Map(Inputs{
		Fragment:       basicFragment(),
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
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
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
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
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
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
				ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
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
		ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
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
		ContentCapture: agento11y.ContentCaptureModeFull,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "" {
		t.Fatalf("Start.ConversationTitle = %q", got.Start.ConversationTitle)
	}
	if got.Generation.ConversationTitle != "" {
		t.Fatalf("Generation.ConversationTitle = %q", got.Generation.ConversationTitle)
	}
}

// TestMapAgentNameOverride pins the exported identity against Inputs.AgentName
// and holds the model provider fallback still. Copilot reuses the product name
// for both, so a single literal used to decide them together.
func TestMapAgentNameOverride(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		want      string
	}{
		{name: "blank keeps the product name", want: AgentName},
		{name: "override", agentName: "copilot-e2e", want: "copilot-e2e"},
		{name: "override is trimmed", agentName: "  spaced  ", want: "spaced"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frag := basicFragment()
			// No provider reported, so the mapper falls back to the product
			// name. That fallback is what must not follow the override.
			frag.Provider = ""
			frag.Model = ""
			got := Map(Inputs{
				Fragment:       frag,
				AgentName:      tt.agentName,
				ContentCapture: agento11y.ContentCaptureModeMetadataOnly,
				Now:            fixedTime,
			})
			if got.Generation.AgentName != tt.want || got.Start.AgentName != tt.want {
				t.Fatalf("AgentName = %q/%q, want %q", got.Start.AgentName, got.Generation.AgentName, tt.want)
			}
			if got.Generation.Model.Provider != AgentName {
				t.Fatalf("Model.Provider = %q, want %q", got.Generation.Model.Provider, AgentName)
			}
		})
	}
}

func TestInferProvider(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{model: "gemini-2.5-pro", want: "gemini"},
		{model: "Gemini-1.5-Flash", want: "gemini"},
		{model: "gpt-5", want: "openai"},
		{model: "claude-sonnet-4", want: "anthropic"},
		{model: "gemini", want: ""},
		{model: "", want: ""},
	}

	for _, tt := range cases {
		t.Run(tt.model, func(t *testing.T) {
			if got := inferProvider(tt.model); got != tt.want {
				t.Errorf("inferProvider(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}
