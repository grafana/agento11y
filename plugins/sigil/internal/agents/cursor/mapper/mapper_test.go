package mapper

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/cursor/fragment"
)

// fixedTime gives every test a deterministic "now".
var fixedTime = time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)

func basicFragment(t *testing.T) *fragment.Fragment {
	t.Helper()
	return &fragment.Fragment{
		ConversationID:  "conv-1",
		GenerationID:    "gen-1",
		UserPrompt:      "hello",
		Assistant:       []fragment.AssistantSegment{{Text: "hi there"}},
		Tools:           []fragment.ToolRecord{{ToolName: "Read", ToolUseID: "t1", ToolInput: json.RawMessage(`{"path":"x"}`), ToolOutput: json.RawMessage(`"contents"`), Status: "completed", Cwd: "/repo"}},
		Model:           "claude-sonnet-4-6",
		Provider:        "anthropic",
		StartedAt:       "2026-04-28T11:59:00Z",
		LastEventAt:     "2026-04-28T12:00:30Z",
		ThinkingPresent: true,
	}
}

// TestMapFragment_BasicFields covers the non-content-capture-dependent
// fields that MapFragment populates from a fragment.
func TestMapFragment_BasicFields(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		ContentCapture: sigil.ContentCaptureModeFull,
		Now:            fixedTime,
	})

	if got.StopStatus != StopStatusCompleted {
		t.Errorf("StopStatus = %v; want completed", got.StopStatus)
	}
	if got.Generation.Model.Provider != "anthropic" || got.Generation.Model.Name != "claude-sonnet-4-6" {
		t.Errorf("Model = %+v; want anthropic/claude-sonnet-4-6", got.Generation.Model)
	}
	if got.Generation.ThinkingEnabled == nil || !*got.Generation.ThinkingEnabled {
		t.Errorf("ThinkingEnabled = %v; want true", got.Generation.ThinkingEnabled)
	}
}

// TestMapFragment_ContentCaptureModes covers what every supported
// ContentCaptureMode includes or strips in the gRPC payload that
// buildMessages produces.
//
//   - Full and FullWithMetadataSpans carry full content; they only diverge
//     in what the OTel span exposes, which is decided elsewhere.
//   - NoToolContent keeps the tool_call/tool_result skeleton but strips
//     argument and result bytes.
//   - MetadataOnly drops user prompts, assistant text, and the tool_result
//     message entirely.
//   - Default is the zero-value enum. envconfig.ResolveContentMode maps it
//     to MetadataOnly, but a caller that bypasses the config layer (or
//     constructs Inputs directly in tests) might pass it through, so
//     buildMessages re-applies the same mapping defensively.
func TestMapFragment_ContentCaptureModes(t *testing.T) {
	cases := []struct {
		name            string
		mode            sigil.ContentCaptureMode
		wantUserPrompt  bool
		wantAssistant   bool
		wantToolInput   bool
		wantToolResult  bool // tool_result message present in input
		wantToolContent bool // tool_result carries ContentJSON or Content
	}{
		{"full", sigil.ContentCaptureModeFull, true, true, true, true, true},
		{"full_with_metadata_spans", sigil.ContentCaptureModeFullWithMetadataSpans, true, true, true, true, true},
		{"no_tool_content", sigil.ContentCaptureModeNoToolContent, true, true, false, true, false},
		{"metadata_only", sigil.ContentCaptureModeMetadataOnly, false, false, false, false, false},
		{"default", sigil.ContentCaptureModeDefault, false, false, false, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MapFragment(Inputs{
				Fragment:       basicFragment(t),
				ContentCapture: tc.mode,
				Now:            fixedTime,
			})

			var gotPrompt, gotAssistant, gotToolInput, gotToolResult, gotToolContent bool
			for _, msg := range got.Generation.Input {
				for _, p := range msg.Parts {
					if p.Text == "hello" {
						gotPrompt = true
					}
				}
				if msg.Role == sigil.RoleTool {
					gotToolResult = true
					for _, p := range msg.Parts {
						if p.ToolResult != nil && (p.ToolResult.Content != "" || len(p.ToolResult.ContentJSON) > 0) {
							gotToolContent = true
						}
					}
				}
			}
			for _, msg := range got.Generation.Output {
				for _, p := range msg.Parts {
					if p.Text == "hi there" {
						gotAssistant = true
					}
					if p.ToolCall != nil && len(p.ToolCall.InputJSON) > 0 {
						gotToolInput = true
					}
				}
			}

			if gotPrompt != tc.wantUserPrompt {
				t.Errorf("user prompt present = %v; want %v", gotPrompt, tc.wantUserPrompt)
			}
			if gotAssistant != tc.wantAssistant {
				t.Errorf("assistant text present = %v; want %v", gotAssistant, tc.wantAssistant)
			}
			if gotToolInput != tc.wantToolInput {
				t.Errorf("tool_call InputJSON present = %v; want %v", gotToolInput, tc.wantToolInput)
			}
			if gotToolResult != tc.wantToolResult {
				t.Errorf("tool_result message present = %v; want %v", gotToolResult, tc.wantToolResult)
			}
			if gotToolContent != tc.wantToolContent {
				t.Errorf("tool_result content present = %v; want %v", gotToolContent, tc.wantToolContent)
			}
		})
	}
}

func TestResolveStopStatus(t *testing.T) {
	cases := []struct {
		in   string
		want StopStatus
	}{
		{"", StopStatusCompleted},
		{"completed", StopStatusCompleted},
		{"success", StopStatusCompleted},
		{"ok", StopStatusCompleted},
		{"aborted", StopStatusAborted},
		{"cancelled", StopStatusAborted},
		{"canceled", StopStatusAborted},
		{"ABORTED", StopStatusAborted},
		{"error", StopStatusError},
		{"failed", StopStatusError},
		{"  ERROR  ", StopStatusError},
		{"unknown_value", StopStatusCompleted},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got := resolveStopStatus(&StopInput{Status: tc.in})
			if got != tc.want {
				t.Errorf("resolveStopStatus(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
	if got := resolveStopStatus(nil); got != StopStatusCompleted {
		t.Errorf("nil StopInput should resolve to completed; got %v", got)
	}
}

func TestInferProviderFromModel(t *testing.T) {
	cases := []struct{ model, want string }{
		{"claude-sonnet-4-6", "anthropic"},
		{"claude-opus", "anthropic"},
		{"gpt-5", "openai"},
		{"o1-preview", "openai"},
		{"o3-mini", "openai"},
		{"o4-fast", "openai"},
		{"gemini-2.5-pro", "google"},
		{"some-random-model", ""},
		{"", ""},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			if got := inferProviderFromModel(tc.model); got != tc.want {
				t.Errorf("inferProviderFromModel(%q) = %q; want %q", tc.model, got, tc.want)
			}
		})
	}
}

func TestResolveUserID(t *testing.T) {
	cases := []struct {
		name         string
		override     string
		payloadEmail string
		want         string
	}{
		{"override wins", "alice@example.com", "bob@example.com", "alice@example.com"},
		{"override trimmed", "  alice@example.com\t", "bob@example.com", "alice@example.com"},
		{"falls back to payload email", "", "bob@example.com", "bob@example.com"},
		{"payload email trimmed", "", "  bob@example.com  ", "bob@example.com"},
		{"whitespace override falls through", "   ", "bob@example.com", "bob@example.com"},
		{"both empty", "", "", ""},
		{"both whitespace-only", "  ", "\t", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveUserID(tc.override, tc.payloadEmail); got != tc.want {
				t.Fatalf("resolveUserID(%q, %q) = %q; want %q",
					tc.override, tc.payloadEmail, got, tc.want)
			}
		})
	}
}

func TestMapFragment_MissingModelAndProvider_FallsBackToCursor(t *testing.T) {
	frag := &fragment.Fragment{
		ConversationID: "conv",
		GenerationID:   "gen",
	}
	got := MapFragment(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Generation.Model.Provider != "cursor" {
		t.Errorf("Provider = %q; want cursor", got.Generation.Model.Provider)
	}
	if got.Generation.Model.Name != "unknown" {
		t.Errorf("Name = %q; want unknown", got.Generation.Model.Name)
	}
}

func TestMapFragment_ConversationTitleFromSession(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment: basicFragment(t),
		Session: &fragment.Session{
			ConversationID:    "conv-1",
			ConversationTitle: "list go files",
		},
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "list go files" {
		t.Errorf("Start.ConversationTitle = %q; want %q", got.Start.ConversationTitle, "list go files")
	}
	if got.Generation.ConversationTitle != "list go files" {
		t.Errorf("Generation.ConversationTitle = %q; want %q", got.Generation.ConversationTitle, "list go files")
	}
}

func TestMapFragment_ConversationTitleEmptyWithoutSession(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Start.ConversationTitle != "" {
		t.Errorf("Start.ConversationTitle = %q; want empty", got.Start.ConversationTitle)
	}
	if got.Generation.ConversationTitle != "" {
		t.Errorf("Generation.ConversationTitle = %q; want empty", got.Generation.ConversationTitle)
	}
}

func TestMapFragment_BuiltinTags(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment: basicFragment(t),
		Session: &fragment.Session{
			ConversationID: "conv-1",
			WorkspaceRoots: []string{"/no-such-dir-without-git"},
		},
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	// No real .git → git.branch absent.
	if _, ok := got.Generation.Tags["git.branch"]; ok {
		t.Errorf("git.branch should be absent when no .git resolves; got %q",
			got.Generation.Tags["git.branch"])
	}
	if got.Generation.Tags["cwd"] != "/repo" {
		t.Errorf("cwd should come from first tool record; got %q", got.Generation.Tags["cwd"])
	}
}

func TestMapFragment_TokenUsage(t *testing.T) {
	in, out := int64(100), int64(50)
	frag := basicFragment(t)
	frag.TokenUsage = &fragment.TokenCounts{InputTokens: &in, OutputTokens: &out}

	got := MapFragment(Inputs{
		Fragment:       frag,
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.Generation.Usage.InputTokens != 100 {
		t.Errorf("InputTokens = %d; want 100", got.Generation.Usage.InputTokens)
	}
	if got.Generation.Usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d; want 50", got.Generation.Usage.OutputTokens)
	}
	if got.Generation.Usage.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d; want 150", got.Generation.Usage.TotalTokens)
	}
}

func TestExtractCallError(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		want string
	}{
		{"nil error", nil, "cursor_stop_error"},
		{"empty bytes", []byte(""), "cursor_stop_error"},
		{"json string", []byte(`"boom"`), "boom"},
		{"json object with message", []byte(`{"message":"timeout","code":"E1"}`), "timeout"},
		{"json object missing message", []byte(`{"code":"E1"}`), "cursor_stop_error"},
		{"unparseable", []byte("garbage"), "cursor_stop_error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractCallError(&StopInput{Error: tc.in})
			if got.Error() != tc.want {
				t.Errorf("got %q; want %q", got.Error(), tc.want)
			}
		})
	}
}

func TestMapFragment_StopStatusError_PopulatesCallError(t *testing.T) {
	got := MapFragment(Inputs{
		Fragment:       basicFragment(t),
		Stop:           &StopInput{Status: "error", Error: []byte(`"network failure"`)},
		ContentCapture: sigil.ContentCaptureModeMetadataOnly,
		Now:            fixedTime,
	})
	if got.StopStatus != StopStatusError {
		t.Errorf("StopStatus = %v; want error", got.StopStatus)
	}
	if got.CallError == nil || got.CallError.Error() != "network failure" {
		t.Errorf("CallError = %v; want 'network failure'", got.CallError)
	}
}

func TestBuildToolDefinitions_DedupAndSort(t *testing.T) {
	tools := []fragment.ToolRecord{
		{ToolName: "Write"},
		{ToolName: "Read"},
		{ToolName: "Read"}, // dup
		{ToolName: ""},     // skipped
		{ToolName: "Bash"},
	}
	got := buildToolDefinitions(tools)
	if len(got) != 3 {
		t.Fatalf("got %d defs; want 3 (got %+v)", len(got), got)
	}
	wantNames := []string{"Bash", "Read", "Write"}
	for i, def := range got {
		if def.Name != wantNames[i] {
			t.Errorf("got[%d].Name = %q; want %q", i, def.Name, wantNames[i])
		}
		if def.Type != "function" {
			t.Errorf("got[%d].Type = %q; want function", i, def.Type)
		}
	}
}

func TestMapFragment_EffectiveVersionStableAcrossToolSubsets(t *testing.T) {
	session := &fragment.Session{CursorVersion: "0.45.2"}

	fragA := basicFragment(t)
	fragA.Tools = []fragment.ToolRecord{{ToolName: "Read", ToolUseID: "t1"}}

	fragB := basicFragment(t)
	fragB.Tools = []fragment.ToolRecord{{ToolName: "Bash", ToolUseID: "t2"}}

	gotA := MapFragment(Inputs{Fragment: fragA, Session: session, ContentCapture: sigil.ContentCaptureModeFull, Now: fixedTime})
	gotB := MapFragment(Inputs{Fragment: fragB, Session: session, ContentCapture: sigil.ContentCaptureModeFull, Now: fixedTime})

	if gotA.Generation.EffectiveVersion == "" {
		t.Fatalf("EffectiveVersion is empty; expected raw cursorVersion")
	}
	if gotA.Generation.EffectiveVersion != gotB.Generation.EffectiveVersion {
		t.Fatalf("EffectiveVersion mismatch across turns: %q vs %q", gotA.Generation.EffectiveVersion, gotB.Generation.EffectiveVersion)
	}
	if gotA.Generation.EffectiveVersion != gotA.Generation.AgentVersion {
		t.Fatalf("EffectiveVersion %q should equal AgentVersion %q", gotA.Generation.EffectiveVersion, gotA.Generation.AgentVersion)
	}
	if gotA.Start.EffectiveVersion != gotA.Generation.EffectiveVersion {
		t.Fatalf("Start.EffectiveVersion %q != Generation.EffectiveVersion %q", gotA.Start.EffectiveVersion, gotA.Generation.EffectiveVersion)
	}
}
