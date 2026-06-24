package mapper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/grafana/sigil-sdk/go/sigil"

	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/vibe/meta"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/vibe/state"
	"github.com/grafana/sigil-sdk/plugins/sigil/internal/agents/vibe/transcript"
)

func TestMap_GoldenFixture(t *testing.T) {
	tp := filepath.Join("..", "testdata", "messages.jsonl")
	lines, _, err := transcript.Read(tp, 0)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	m, err := meta.Load(tp)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	out := Map(Inputs{
		SessionID:      "session-A",
		CWD:            "/repo/x",
		Lines:          lines,
		Meta:           m,
		PriorState:     state.Session{},
		ContentCapture: sigil.ContentCaptureModeFull,
		Now:            now,
	}, m.Stats.Steps)

	g := out.Generation

	if g.AgentName != "mistral-vibe" {
		t.Errorf("AgentName = %q, want mistral-vibe", g.AgentName)
	}
	if g.ConversationID != "session-A" {
		t.Errorf("ConversationID = %q, want session-A", g.ConversationID)
	}
	if g.Model.Provider != "mistral" {
		t.Errorf("Provider = %q, want mistral", g.Model.Provider)
	}
	if g.Model.Name != "mistral-medium-3.5" {
		t.Errorf("Model.Name = %q (display alias), want mistral-medium-3.5", g.Model.Name)
	}
	if g.ResponseModel != "mistral-vibe-cli-latest" {
		t.Errorf("ResponseModel = %q, want API id mistral-vibe-cli-latest", g.ResponseModel)
	}
	if g.StopReason != "completed" {
		t.Errorf("StopReason = %q, want completed", g.StopReason)
	}
	if !strings.HasPrefix(g.ID, "vibe-") {
		t.Errorf("ID = %q, want vibe- prefix", g.ID)
	}
	if g.SystemPrompt == "" {
		t.Errorf("SystemPrompt is empty in Full mode")
	}

	// The fixture is a mid-session capture (steps=7) exported with no prior
	// state, which means state was lost (or this is the first export after
	// install). Billing the full cumulative session to one turn would
	// over-count, so usage falls back to the last-turn figures.
	if g.Usage.InputTokens != m.Stats.LastTurnPromptTokens {
		t.Errorf("Usage.InputTokens = %d, want last-turn %d", g.Usage.InputTokens, m.Stats.LastTurnPromptTokens)
	}
	if g.Usage.OutputTokens != m.Stats.LastTurnCompletionTokens {
		t.Errorf("Usage.OutputTokens = %d, want last-turn %d", g.Usage.OutputTokens, m.Stats.LastTurnCompletionTokens)
	}

	// Tools come from meta.json's tools_available; verify the first tool
	// flows through with a usable input_schema.
	if len(g.Tools) == 0 {
		t.Fatalf("Tools is empty")
	}
	if g.Tools[0].Type != "function" {
		t.Errorf("Tools[0].Type = %q, want function", g.Tools[0].Type)
	}

	// Messages: the fixture contains user prompts, assistant tool calls,
	// tool results, and assistant text. Check that all three Input
	// classes and at least one assistant tool-call landed.
	var sawUserText, sawToolResult bool
	for _, msg := range g.Input {
		for _, part := range msg.Parts {
			if part.Text != "" && msg.Role == sigil.RoleUser {
				sawUserText = true
			}
			if part.ToolResult != nil {
				sawToolResult = true
			}
		}
	}
	if !sawUserText {
		t.Error("Input missing user text")
	}
	if !sawToolResult {
		t.Error("Input missing tool result")
	}

	var sawToolCall, sawAssistantText bool
	var firstCallArgs string
	for _, msg := range g.Output {
		for _, part := range msg.Parts {
			if part.ToolCall != nil {
				sawToolCall = true
				if firstCallArgs == "" {
					firstCallArgs = string(part.ToolCall.InputJSON)
				}
			}
			if part.Text != "" {
				sawAssistantText = true
			}
		}
	}
	if !sawToolCall {
		t.Error("Output missing assistant tool_call")
	}
	if !sawAssistantText {
		t.Error("Output missing assistant text")
	}
	// vibe encodes arguments as a JSON-encoded string. The mapper must
	// pass those bytes through verbatim so the result parses as JSON.
	if firstCallArgs != "" {
		var v any
		if err := json.Unmarshal([]byte(firstCallArgs), &v); err != nil {
			t.Errorf("InputJSON is not valid JSON: %v (got %q)", err, firstCallArgs)
		}
	}
}

func TestMap_TurnUsage(t *testing.T) {
	tests := []struct {
		name            string
		stats           meta.Stats
		prior           state.Session
		priorFound      bool
		turnSeq         int
		wantIn, wantOut int64
	}{
		{
			name:    "first turn no prior uses full session totals",
			stats:   meta.Stats{Steps: 1, SessionPromptTokens: 200, SessionCompletionTokens: 50},
			turnSeq: 1,
			wantIn:  200, wantOut: 50,
		},
		{
			name:    "state lost mid-session falls back to last-turn",
			stats:   meta.Stats{Steps: 7, SessionPromptTokens: 104734, SessionCompletionTokens: 1023, LastTurnPromptTokens: 22918, LastTurnCompletionTokens: 21},
			turnSeq: 7,
			wantIn:  22918, wantOut: 21,
		},
		{
			name:       "normal delta against prior snapshot",
			stats:      meta.Stats{SessionPromptTokens: 1000, SessionCompletionTokens: 250},
			prior:      state.Session{SessionPromptTokens: 800, SessionCompletionTokens: 200},
			priorFound: true,
			turnSeq:    3,
			wantIn:     200, wantOut: 50,
		},
		{
			name:       "regressed totals fall back to last-turn",
			stats:      meta.Stats{SessionPromptTokens: 10, SessionCompletionTokens: 5, LastTurnPromptTokens: 4, LastTurnCompletionTokens: 2},
			prior:      state.Session{SessionPromptTokens: 999, SessionCompletionTokens: 999},
			priorFound: true,
			turnSeq:    4,
			wantIn:     4, wantOut: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Map(Inputs{
				SessionID:       "s",
				Meta:            meta.Meta{Stats: tt.stats},
				PriorState:      tt.prior,
				PriorStateFound: tt.priorFound,
				ContentCapture:  sigil.ContentCaptureModeFull,
			}, tt.turnSeq).Generation.Usage
			if got.InputTokens != tt.wantIn || got.OutputTokens != tt.wantOut {
				t.Errorf("usage = %d/%d, want %d/%d", got.InputTokens, got.OutputTokens, tt.wantIn, tt.wantOut)
			}
			if got.TotalTokens != tt.wantIn+tt.wantOut {
				t.Errorf("TotalTokens = %d, want %d", got.TotalTokens, tt.wantIn+tt.wantOut)
			}
		})
	}
}

func TestMap_ContentCaptureModes(t *testing.T) {
	tp := filepath.Join("..", "testdata", "messages.jsonl")
	lines, _, err := transcript.Read(tp, 0)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	m, err := meta.Load(tp)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	tests := []struct {
		name string
		mode sigil.ContentCaptureMode
		// In MetadataOnly we still expect the structural assistant
		// tool-call message (Sigil needs the call sequence to render
		// the conversation), but no text content.
		wantUserText      bool
		wantAssistantText bool
		wantToolArgs      bool
		wantSystemPrompt  bool
	}{
		{name: "full", mode: sigil.ContentCaptureModeFull, wantUserText: true, wantAssistantText: true, wantToolArgs: true, wantSystemPrompt: true},
		{name: "no_tool_content", mode: sigil.ContentCaptureModeNoToolContent, wantUserText: true, wantAssistantText: true, wantToolArgs: false, wantSystemPrompt: true},
		{name: "metadata_only", mode: sigil.ContentCaptureModeMetadataOnly, wantUserText: false, wantAssistantText: false, wantToolArgs: false, wantSystemPrompt: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := Map(Inputs{
				SessionID:      "s",
				Lines:          lines,
				Meta:           m,
				ContentCapture: tt.mode,
			}, m.Stats.Steps).Generation

			var hasUserText, hasAssistantText, hasToolArgs bool
			for _, msg := range g.Input {
				for _, p := range msg.Parts {
					if msg.Role == sigil.RoleUser && p.Text != "" {
						hasUserText = true
					}
				}
			}
			for _, msg := range g.Output {
				for _, p := range msg.Parts {
					if p.Text != "" {
						hasAssistantText = true
					}
					if p.ToolCall != nil && len(p.ToolCall.InputJSON) > 0 {
						hasToolArgs = true
					}
				}
			}
			hasSystemPrompt := g.SystemPrompt != ""

			if hasUserText != tt.wantUserText {
				t.Errorf("user text: got %v want %v", hasUserText, tt.wantUserText)
			}
			if hasAssistantText != tt.wantAssistantText {
				t.Errorf("assistant text: got %v want %v", hasAssistantText, tt.wantAssistantText)
			}
			if hasToolArgs != tt.wantToolArgs {
				t.Errorf("tool args: got %v want %v", hasToolArgs, tt.wantToolArgs)
			}
			if hasSystemPrompt != tt.wantSystemPrompt {
				t.Errorf("system prompt: got %v want %v", hasSystemPrompt, tt.wantSystemPrompt)
			}
		})
	}
}

func TestGenerationID_Deterministic(t *testing.T) {
	a := GenerationID("sess", 3)
	b := GenerationID("sess", 3)
	if a != b {
		t.Errorf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "vibe-") {
		t.Errorf("missing prefix: %q", a)
	}
	if GenerationID("sess", 3) == GenerationID("sess", 4) {
		t.Error("turn seq did not influence ID")
	}
}

func TestMap_Reasoning(t *testing.T) {
	tp := filepath.Join("..", "testdata", "messages.jsonl")
	lines, _, err := transcript.Read(tp, 0)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	m, err := meta.Load(tp)
	if err != nil {
		t.Fatalf("load meta: %v", err)
	}

	full := Map(Inputs{SessionID: "s", Lines: lines, Meta: m, ContentCapture: sigil.ContentCaptureModeFull}, 1).Generation
	if full.ThinkingEnabled == nil || !*full.ThinkingEnabled {
		t.Fatal("ThinkingEnabled not set in Full mode despite reasoning_content in the fixture")
	}
	sawThinking := false
	for _, msg := range full.Output {
		for _, p := range msg.Parts {
			if p.Kind == sigil.PartKindThinking && strings.TrimSpace(p.Thinking) != "" {
				sawThinking = true
			}
		}
	}
	if !sawThinking {
		t.Error("Full output missing a thinking part")
	}

	// metadata_only keeps the thinking_enabled signal but drops the text.
	metaOnly := Map(Inputs{SessionID: "s", Lines: lines, Meta: m, ContentCapture: sigil.ContentCaptureModeMetadataOnly}, 1).Generation
	if metaOnly.ThinkingEnabled == nil || !*metaOnly.ThinkingEnabled {
		t.Error("ThinkingEnabled not set in metadata_only mode")
	}
	for _, msg := range metaOnly.Output {
		for _, p := range msg.Parts {
			if p.Kind == sigil.PartKindThinking {
				t.Error("metadata_only must not emit thinking text")
			}
		}
	}
}

func TestMap_CostAndFailureMetadata(t *testing.T) {
	in := Inputs{
		SessionID: "s",
		Meta: meta.Meta{Stats: meta.Stats{
			SessionPromptTokens: 100, SessionCompletionTokens: 10,
			SessionCost:       0.5,
			ToolCallsFailed:   2,
			ToolCallsRejected: 1,
		}},
		PriorState: state.Session{
			SessionPromptTokens: 80, SessionCompletionTokens: 5,
			SessionCost:     0.3,
			ToolCallsFailed: 1,
		},
		PriorStateFound: true,
		ContentCapture:  sigil.ContentCaptureModeFull,
	}
	md := Map(in, 2).Generation.Metadata
	if got, ok := md["vibe.cost_usd"].(float64); !ok || got <= 0.19 || got >= 0.21 {
		t.Errorf("vibe.cost_usd = %v, want ~0.2 (0.5-0.3)", md["vibe.cost_usd"])
	}
	if md["vibe.tool_calls_failed"] != int64(1) {
		t.Errorf("vibe.tool_calls_failed = %v, want 1 (2-1)", md["vibe.tool_calls_failed"])
	}
	if md["vibe.tool_calls_rejected"] != int64(1) {
		t.Errorf("vibe.tool_calls_rejected = %v, want 1 (1-0)", md["vibe.tool_calls_rejected"])
	}
	if _, ok := md["vibe.tool_calls_hook_denied"]; ok {
		t.Error("vibe.tool_calls_hook_denied set despite zero delta")
	}
}

func TestMap_ParentLinkage(t *testing.T) {
	resolved := Map(Inputs{
		SessionID:          "child",
		ParentSessionID:    "parent",
		ParentGenerationID: "vibe-parentgen",
		Meta:               meta.Meta{Stats: meta.Stats{Steps: 1}},
		ContentCapture:     sigil.ContentCaptureModeFull,
	}, 1).Generation
	if resolved.ConversationID != "parent" {
		t.Errorf("ConversationID = %q, want parent (reparented onto the parent session)", resolved.ConversationID)
	}
	if len(resolved.ParentGenerationIDs) != 1 || resolved.ParentGenerationIDs[0] != "vibe-parentgen" {
		t.Errorf("ParentGenerationIDs = %v, want [vibe-parentgen]", resolved.ParentGenerationIDs)
	}
	if resolved.Tags["vibe.parent_session_id"] != "parent" {
		t.Error("parent session hint tag missing")
	}
	// The child's own session id must survive reparenting so the subagent
	// turn can be tied back to it.
	if resolved.Tags["vibe.child_session_id"] != "child" {
		t.Errorf("child session id tag = %q, want child", resolved.Tags["vibe.child_session_id"])
	}
	if resolved.Metadata["vibe.child_session_id"] != "child" {
		t.Errorf("child session id metadata = %v, want child", resolved.Metadata["vibe.child_session_id"])
	}

	// Without a resolved parent generation, the conversation stays on the
	// child session and only the hint tag/metadata is recorded.
	hintOnly := Map(Inputs{
		SessionID:       "child",
		ParentSessionID: "parent",
		Meta:            meta.Meta{Stats: meta.Stats{Steps: 1}},
		ContentCapture:  sigil.ContentCaptureModeFull,
	}, 1).Generation
	if hintOnly.ConversationID != "child" {
		t.Errorf("ConversationID = %q, want child when no parent gen resolved", hintOnly.ConversationID)
	}
	if len(hintOnly.ParentGenerationIDs) != 0 {
		t.Errorf("ParentGenerationIDs = %v, want empty without a resolved parent gen", hintOnly.ParentGenerationIDs)
	}
	// Not reparented: conversation_id is already the child, so there is no
	// separate child-session tag.
	if _, ok := hintOnly.Tags["vibe.child_session_id"]; ok {
		t.Error("child session id tag set without reparenting")
	}
}

func TestMap_StateLossMidSessionUsesLatestTurnMessages(t *testing.T) {
	// No prior state (priorFound=false) at steps>1 means the handler read
	// the whole transcript from offset 0. The export must carry only the
	// latest turn's messages, matching the last-turn usage fallback, not the
	// full history.
	lines := []transcript.Line{
		{Role: "user", Content: "first prompt"},
		{Role: "assistant", Content: "first answer"},
		{Role: "user", Content: "second prompt"},
		{Role: "assistant", Content: "second answer"},
	}
	g := Map(Inputs{
		SessionID:      "s",
		Lines:          lines,
		Meta:           meta.Meta{Stats: meta.Stats{Steps: 2}},
		ContentCapture: sigil.ContentCaptureModeFull,
	}, 2).Generation

	if got := messageText(g.Input); strings.Contains(got, "first prompt") || !strings.Contains(got, "second prompt") {
		t.Errorf("input text = %q, want only the latest-turn prompt", got)
	}
	if got := messageText(g.Output); strings.Contains(got, "first answer") || !strings.Contains(got, "second answer") {
		t.Errorf("output text = %q, want only the latest-turn response", got)
	}
}

func TestMap_GitBranchTag(t *testing.T) {
	// CWD points at a temp dir; when it holds a `.git/HEAD` the gitbranch
	// resolver finds the branch without shelling out, otherwise the tag is
	// omitted. Mirrors the codex and copilot mapper fixtures.
	tests := []struct {
		name string
		head string // .git/HEAD contents; empty means no checkout
		want string // expected git.branch tag; empty means absent
	}{
		{name: "regular branch", head: "ref: refs/heads/feature/vibe\n", want: "feature/vibe"},
		{name: "detached HEAD", head: "abcdef0123456789abcdef0123456789abcdef01\n", want: "abcdef012345"},
		{name: "no checkout", head: "", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if tc.head != "" {
				gitHead := filepath.Join(root, ".git", "HEAD")
				if err := os.MkdirAll(filepath.Dir(gitHead), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(gitHead, []byte(tc.head), 0o644); err != nil {
					t.Fatalf("write head: %v", err)
				}
			}
			g := Map(Inputs{
				SessionID:      "s",
				CWD:            root,
				ContentCapture: sigil.ContentCaptureModeMetadataOnly,
				Now:            time.Unix(1, 0),
			}, 1).Generation
			if got := g.Tags["git.branch"]; got != tc.want {
				t.Fatalf("git.branch = %q, want %q", got, tc.want)
			}
			if g.Tags["cwd"] != root {
				t.Fatalf("cwd = %q, want %q", g.Tags["cwd"], root)
			}
		})
	}
}

func messageText(messages []sigil.Message) string {
	var b strings.Builder
	for _, msg := range messages {
		for _, part := range msg.Parts {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}
